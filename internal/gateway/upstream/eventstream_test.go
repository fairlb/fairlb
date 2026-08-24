package upstream_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/fairlb/fairlb/internal/gateway/upstream"
)

// The frames are built with the protocol's own encoder rather than by writing
// bytes out by hand. Hand-built frames would have to carry hand-computed
// checksums, and a test whose fixtures are wrong in the same way the code is
// wrong proves nothing.
func chunkFrame(inner string) eventstream.Message {
	payload, err := json.Marshal(map[string]any{"bytes": []byte(inner)})
	if err != nil {
		panic(err)
	}
	return eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue("chunk")},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		},
		Payload: payload,
	}
}

func exceptionFrame(kind, message string) eventstream.Message {
	return eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("exception")},
			{Name: ":exception-type", Value: eventstream.StringValue(kind)},
		},
		Payload: []byte(`{"message":` + quote(message) + `}`),
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func encodeFrames(t *testing.T, msgs ...eventstream.Message) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, m := range msgs {
		if err := enc.Encode(&buf, m); err != nil {
			t.Fatalf("encoding a fixture frame: %v", err)
		}
	}
	return buf.Bytes()
}

// A whole stream, re-framed. The output has to be SSE a client can parse: an
// event name taken from the payload's own type field, one data line, and a
// blank line ending each event.
func TestEventStreamBecomesSSE(t *testing.T) {
	raw := encodeFrames(t,
		chunkFrame(`{"type":"message_start","message":{"usage":{"input_tokens":11}}}`),
		chunkFrame(`{"type":"content_block_delta","delta":{"text":"hi"}}`),
		chunkFrame(`{"type":"message_stop"}`),
	)
	got := readAll(t, upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw))))

	want := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	if got != want {
		t.Fatalf("re-framed stream:\n got=%q\nwant=%q", got, want)
	}
}

// The inner bytes come out byte-identical.
//
// Asserted separately from the shape above because it is the property that
// makes this a re-framing rather than a translation: every byte of every data
// line was a byte of the payload, unparsed and unreordered. A re-encode would
// still produce valid SSE, still pass a shape assertion, and would silently
// reorder fields -- which a client comparing bytes would see and nothing here
// would.
func TestEventStreamDoesNotReencodeThePayload(t *testing.T) {
	// Deliberately not in the order a Go map would marshal, with spacing a
	// re-encode would normalise away.
	inner := `{"delta":{"text":"x"}, "type":"content_block_delta", "index":0}`
	raw := encodeFrames(t, chunkFrame(inner))
	got := readAll(t, upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw))))
	if !strings.Contains(got, "data: "+inner+"\n\n") {
		t.Fatalf("the payload was not passed through verbatim:\n%q", got)
	}
}

// Frame boundaries do not line up with read boundaries, and the decoder has to
// carry state across them. A reader that yields one byte at a time is the
// harshest version of the real thing: over a network a frame arrives in
// whatever pieces the path happens to produce.
func TestEventStreamSurvivesReadsThatSplitFrames(t *testing.T) {
	raw := encodeFrames(t,
		chunkFrame(`{"type":"a"}`),
		chunkFrame(`{"type":"b"}`),
	)
	whole := readAll(t, upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw))))
	split := readAll(t, upstream.SSEFromEventStream(
		io.NopCloser(iotest.OneByteReader(bytes.NewReader(raw)))))
	if whole != split {
		t.Fatalf("splitting the reads changed the output:\n whole=%q\n split=%q", whole, split)
	}
}

// An error arriving as the very first frame must surface as a read error with
// nothing written, so the caller can still choose another candidate and answer
// with a real HTTP status. Emitting it as an event instead would commit a 200
// to the client before anything was actually delivered.
func TestEventStreamExceptionOnTheFirstFrameYieldsNoBytes(t *testing.T) {
	raw := encodeFrames(t, exceptionFrame("ValidationException", "malformed input"))
	r := upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw)))

	buf := make([]byte, 512)
	n, err := r.Read(buf)
	if n != 0 {
		t.Fatalf("an error frame must not produce output, got %q", buf[:n])
	}
	if !errors.Is(err, upstream.ErrStreamException) {
		t.Fatalf("err = %v, want an in-stream exception", err)
	}
	// The upstream's own words survive: the class alone does not say what was
	// malformed, and this message is the only description anyone will get.
	for _, want := range []string{"ValidationException", "malformed input"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// An error part way through must not swallow the frames that already arrived.
// Those bytes were produced, and on this path they are also what the request
// settles against.
func TestEventStreamExceptionAfterDataKeepsTheData(t *testing.T) {
	raw := encodeFrames(t,
		chunkFrame(`{"type":"content_block_delta","delta":{"text":"partial"}}`),
		exceptionFrame("ModelStreamErrorException", "the model stopped"),
	)
	r := upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw)))

	var out bytes.Buffer
	buf := make([]byte, 512)
	var readErr error
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			readErr = err
			break
		}
	}
	if !strings.Contains(out.String(), `"partial"`) {
		t.Errorf("the frames before the error were lost:\n%q", out.String())
	}
	if !errors.Is(readErr, upstream.ErrStreamException) {
		t.Errorf("err = %v, want an in-stream exception", readErr)
	}
}

// A frame this build does not recognise is skipped, not fatal. This is the read
// direction: the sender may be a newer version of the platform, and a stream
// that is otherwise arriving correctly should not be broken by one frame nobody
// asked about.
func TestEventStreamSkipsFramesItCannotRead(t *testing.T) {
	unknown := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue("something-new")},
		},
		Payload: []byte(`{"not-bytes":1}`),
	}
	raw := encodeFrames(t, unknown, chunkFrame(`{"type":"message_stop"}`))
	got := readAll(t, upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw))))
	if want := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"; got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

// A payload with no type field still goes out. A data-only event is valid SSE,
// and dropping content because it lacks a label loses tokens the caller is
// being billed for.
func TestEventStreamEmitsDataWithoutAnEventName(t *testing.T) {
	raw := encodeFrames(t, chunkFrame(`{"nothing":"here"}`))
	got := readAll(t, upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw))))
	if want := "data: {\"nothing\":\"here\"}\n\n"; got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

// A newline inside the payload would end the data line and split one event into
// two, so it is compacted first. The positive control matters as much as the
// assertion: the compaction must not run on payloads that do not need it, which
// is what the byte-for-byte test above pins.
func TestEventStreamKeepsOneEventOnOneDataLine(t *testing.T) {
	raw := encodeFrames(t, chunkFrame("{\n  \"type\": \"ping\"\n}"))
	got := readAll(t, upstream.SSEFromEventStream(io.NopCloser(bytes.NewReader(raw))))
	if strings.Count(got, "data: ") != 1 {
		t.Fatalf("a payload with newlines produced more than one data line:\n%q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("the event is not terminated:\n%q", got)
	}
	if strings.Count(strings.TrimSuffix(got, "\n\n"), "\n") != 1 { // just the event: line
		t.Fatalf("the data spans more than one line:\n%q", got)
	}
}

// Closing the re-framer closes the body underneath it. A leaked upstream
// connection per streamed request is not a slow leak, it is one per request.
func TestEventStreamCloseReachesTheBody(t *testing.T) {
	c := &countingCloser{Reader: bytes.NewReader(nil)}
	if err := upstream.SSEFromEventStream(c).Close(); err != nil {
		t.Fatal(err)
	}
	if c.closed != 1 {
		t.Fatalf("underlying body closed %d times, want 1", c.closed)
	}
}

type countingCloser struct {
	io.Reader
	closed int
}

func (c *countingCloser) Close() error { c.closed++; return nil }

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading the re-framed stream: %v", err)
	}
	return string(b)
}

// Guard on the fixture helper itself: the wrapper field really is base64, which
// is what makes the decode direction meaningful rather than a no-op.
func TestFixtureWrapsPayloadAsBase64(t *testing.T) {
	var probe struct {
		Bytes string `json:"bytes"`
	}
	if err := json.Unmarshal(chunkFrame(`{"type":"x"}`).Payload, &probe); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(probe.Bytes)
	if err != nil {
		t.Fatalf("the fixture is not base64: %v", err)
	}
	if string(decoded) != `{"type":"x"}` {
		t.Fatalf("the fixture does not carry the payload: %q", decoded)
	}
}
