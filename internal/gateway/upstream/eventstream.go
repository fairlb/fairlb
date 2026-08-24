package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// Binary event-stream frames, re-framed as server-sent events.
//
// One hosted platform returns its streamed answers as a length-prefixed binary
// frame protocol instead of SSE. Each frame carries a set of headers and a JSON
// payload whose single field holds the base64 of one SSE event's data -- the
// same event, with the same field names and the same values, that the direct
// endpoint would have written straight onto the wire.
//
// *Re-framing is not translation, and the distinction is worth being exact
// about because it is the boundary this whole layer is built on.* Translation
// is when two sides describe the world differently and something has to decide
// how one maps onto the other: a field that exists on one side and not the
// other, a count that means a subset here and an addition there. Every such
// decision is a guess, and a wrong guess about a token count is a wrong bill.
//
// There is no decision here. One container is unwrapped and another is wrapped
// around the identical bytes: base64 out, `data:` in, and the event name copied
// from a field the payload already carries. Nothing is dropped because nothing
// fails to correspond; nothing is invented because every output byte came from
// an input byte. The check is mechanical -- if this file ever needs an `if` on
// a field's *meaning*, it has stopped re-framing and started translating.
//
// What is genuinely lost is stated rather than hidden: the platform's own
// per-frame metadata headers, which name the frame type and content type, are
// consumed rather than forwarded. They describe the container being removed.

// Frame headers. The names are the platform's, quoted here because they are
// what arrives on the wire.
const (
	messageTypeHeader  = ":message-type"
	exceptionHeader    = ":exception-type"
	errorCodeHeader    = ":error-code"
	errorMessageHeader = ":error-message"

	messageTypeException = "exception"
	messageTypeError     = "error"
)

// EventStreamContentType is what an upstream sends back when its body is these
// frames rather than SSE.
const EventStreamContentType = "application/vnd.amazon.eventstream"

// maxFramePayload bounds one frame's payload. The protocol's own prelude
// permits far more than any single streamed delta needs, and a hostile or
// broken upstream should not be able to make the gateway allocate without
// bound one frame at a time.
const maxFramePayload = 4 << 20

// ErrStreamException is returned when the upstream reports a failure inside the
// stream rather than as an HTTP status.
//
// It surfaces as a read error rather than as a forwarded event on purpose. The
// pipeline already distinguishes "the upstream failed before any byte reached
// the client" -- where another candidate may still serve the request -- from
// "it failed part way through", where the request settles against what was
// produced. Emitting an error event instead would make every failure look like
// the second case, including the ones that arrive as the very first frame.
var ErrStreamException = errors.New("upstream: the upstream reported an error inside the stream")

// SSEFromEventStream re-frames a binary event-stream body as SSE.
//
// The result is an io.ReadCloser so it can stand in for a response body
// unchanged; closing it closes the underlying body.
func SSEFromEventStream(body io.ReadCloser) io.ReadCloser {
	return &eventStreamReader{
		src:     body,
		decoder: eventstream.NewDecoder(),
		scratch: make([]byte, 0, 16<<10),
	}
}

type eventStreamReader struct {
	src     io.ReadCloser
	decoder *eventstream.Decoder
	out     bytes.Buffer
	scratch []byte
	err     error
}

func (r *eventStreamReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 {
		if r.err != nil {
			return 0, r.err
		}
		r.fill()
	}
	// The pending error is deliberately not returned alongside buffered bytes:
	// the reader above splits on event boundaries and would drop whatever is
	// still buffered. It is returned on the next call, once the buffer is
	// empty.
	return r.out.Read(p)
}

func (r *eventStreamReader) Close() error { return r.src.Close() }

// fill decodes one frame and appends whatever it produces to the buffer.
func (r *eventStreamReader) fill() {
	msg, err := r.decoder.Decode(r.src, r.scratch[:0])
	if err != nil {
		r.err = err
		return
	}
	if len(msg.Payload) > maxFramePayload {
		r.err = fmt.Errorf("upstream: a stream frame exceeded %d bytes", maxFramePayload)
		return
	}

	switch headerString(msg.Headers, messageTypeHeader) {
	case messageTypeException:
		r.err = fmt.Errorf("%w: %s%s", ErrStreamException,
			headerString(msg.Headers, exceptionHeader), quotedMessage(msg.Payload))
		return
	case messageTypeError:
		r.err = fmt.Errorf("%w: %s %s", ErrStreamException,
			headerString(msg.Headers, errorCodeHeader),
			headerString(msg.Headers, errorMessageHeader))
		return
	}

	// Anything that is not a data-bearing frame is skipped rather than
	// refused. This is the read direction, where the sender may be a newer
	// version of the platform: a frame type this build does not know is not a
	// reason to break a stream that is otherwise arriving correctly.
	var wrapper struct {
		Bytes []byte `json:"bytes"`
	}
	if err := json.Unmarshal(msg.Payload, &wrapper); err != nil || len(wrapper.Bytes) == 0 {
		return
	}
	r.writeEvent(wrapper.Bytes)
}

// writeEvent writes one SSE event carrying the unwrapped data.
func (r *eventStreamReader) writeEvent(data []byte) {
	// The event name comes from the payload's own type field, which is where
	// the direct endpoint takes it from as well. A payload without one still
	// goes out: a data-only event is valid SSE, and dropping the frame would
	// lose content over a missing label.
	var named struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &named)
	if named.Type != "" {
		r.out.WriteString("event: ")
		r.out.WriteString(named.Type)
		r.out.WriteByte('\n')
	}

	// A newline inside the data would end the data line and split one event
	// into two, so the payload is compacted first. In practice it arrives
	// compact already, and then this leaves the bytes untouched -- which is
	// what keeps the pass-through byte-for-byte for every real frame.
	if bytes.ContainsAny(data, "\r\n") {
		var flat bytes.Buffer
		if err := json.Compact(&flat, data); err == nil {
			data = flat.Bytes()
		} else {
			data = bytes.ReplaceAll(bytes.ReplaceAll(data, []byte("\r\n"), []byte(" ")), []byte("\n"), []byte(" "))
		}
	}
	r.out.WriteString("data: ")
	r.out.Write(data)
	r.out.WriteString("\n\n")
}

func headerString(hs eventstream.Headers, name string) string {
	v := hs.Get(name)
	if v == nil {
		return ""
	}
	return v.String()
}

// quotedMessage renders the message an exception frame carries, if it carries
// one. The upstream's own words are worth more than the exception class alone.
func quotedMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Message == "" {
		return ""
	}
	return ": " + body.Message
}
