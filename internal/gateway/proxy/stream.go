package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/upstream"
)

// Byte-for-byte SSE pass-through.
//
// *Single writer*: upstream bytes are written out by this loop and nothing
// else. Two goroutines writing to one ResponseWriter tear SSE frames apart and
// the client parses half an event. That kind of corruption only appears under
// particular timings and is nearly impossible to test for, so it is excluded by
// design instead.
//
// *Headers are withheld until the first byte*: this function can still fail
// before that first byte, and then the caller has to be able to send a real
// HTTP error status.
//
// The billing boundary: a failure before the first byte reaches the client can
// still void; after it, the request settles against what was produced --
// because part of the service really was delivered.

const (
	// firstByteTimeout is the deadline for the upstream's first SSE event on
	// every surface but images, which waits far longer for a reason of its own
	// -- see firstByteBudgetFor. The stream client's ResponseHeaderTimeout is
	// set to the widest of those budgets and is only a backstop: each attempt
	// arms a context cancel at its own remaining budget, and that is what
	// actually bounds the wait here.
	firstByteTimeout = 60 * time.Second
	// idleTimeout bounds a gap between events inside a stream.
	idleTimeout = 30 * time.Second
)

// upstreamStreamBody adapts an upstream's streamed body to the SSE this
// package reads.
//
// For almost every upstream it is the body unchanged, and that is the important
// half: the pass-through stays byte-for-byte and nothing here can perturb it.
// One hosted platform answers in a binary frame protocol instead, and its frames
// are unwrapped into the SSE events they already contain -- see the re-framing
// itself for why that is not a translation.
//
// Closing stays with the caller, who holds the response body: this returns a
// reader precisely so that ownership does not move.
func upstreamStreamBody(tp catalog.Transport, body io.ReadCloser) io.Reader {
	if tp.BodyEnvelope() == catalog.EnvelopeBedrock {
		return upstream.SSEFromEventStream(body)
	}
	return body
}

// StreamOutcome is the result of one streamed forward, for settlement to use.
type StreamOutcome struct {
	Usage Usage
	// ResourceID is shadow-parsed from Responses/Interactions events so a
	// streamed creation gains the same route affinity as a buffered one.
	ResourceID string
	// FirstByteSent decides whether a failure may void: once sent, the request
	// must settle against what was produced.
	FirstByteSent bool
	// Canceled means the client hung up.
	Canceled bool
	// Interrupted means the upstream stopped after the first byte.
	Interrupted bool
	// TimedOut means a deadline fired rather than the upstream failing.
	//
	// It exists to separate the two ways a stream can end with no byte sent,
	// which are otherwise the same shape and differ only in the wording of the
	// returned error. They call for opposite decisions: an upstream that closed
	// the connection immediately is a fast failure and another candidate is
	// worth trying, whereas a first-byte deadline means the budget for this
	// request is *already spent* -- rotating there would hand the next candidate
	// no time at all and push the total past what the proxies in front of us
	// will wait. Distinguishing them by matching on the error string would work
	// until somebody rewords the message.
	TimedOut bool
	// Text is the forwarded text content, the input to estimation when the
	// upstream reported no usage.
	Text string
	TTFB time.Duration
}

// streamClock abstracts the timer so tests can inject a deterministic clock.
// Streaming tests that depend on real time are inevitably flaky.
type streamClock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Streamer performs the SSE pass-through.
type Streamer struct {
	clock     streamClock
	firstByte time.Duration
	idle      time.Duration
}

func NewStreamer() *Streamer {
	return &Streamer{clock: realClock{}, firstByte: firstByteTimeout, idle: idleTimeout}
}

// WithFirstByteBudget overrides the first-byte deadline with the *remaining*
// budget.
//
// Waiting for the upstream's response headers and waiting for its first SSE
// event are two serial waits. Giving each its own 60s means the worst case
// takes 120s to produce any result, which is already past the point where
// intermediaries give up on a silent response. So the caller subtracts what has
// been spent and the two waits share a single 60s budget.
//
// A non-positive budget is treated as an immediate timeout: the caller has
// already used it all up.
func (s *Streamer) WithFirstByteBudget(d time.Duration) *Streamer {
	if d <= 0 {
		d = time.Nanosecond
	}
	s.firstByte = d
	return s
}

// Pump passes the upstream SSE stream through to the client while parsing usage
// off to the side.
//
// upstream is the upstream response body and w is the client connection. The
// returned outcome is usable for settlement whether or not this succeeded;
// FirstByteSent decides whether anything is charged.
func (s *Streamer) Pump(
	ctx context.Context, w http.ResponseWriter, upstream io.Reader, surface catalog.Surface,
) (StreamOutcome, error) {
	var out StreamOutcome
	started := time.Now()

	// Capability detection cannot delegate to rc.Flush(): that would commit a
	// 200 early, and withholding the header is exactly the property being
	// preserved below. Without a Flusher the response is buffered until the
	// handler returns and streaming stops working outright, so this stays
	// fatal.
	if !flushable(w) {
		return out, fmt.Errorf("proxy: streaming is unavailable: the response writer cannot flush")
	}
	rc := http.NewResponseController(w)

	// The reader goroutine has to be stoppable. Pump has three return paths
	// that do not drain chunks -- the client hanging up, a write failure, and
	// an idle timeout inside the stream -- and the producer would block on its
	// send forever. Closing upstream does not help: the blocking point is the
	// channel send, not the read. Hence an explicit context to shut it down.
	readCtx, stopReader := context.WithCancel(ctx)
	defer stopReader()

	chunks := make(chan []byte, 16)
	readErr := make(chan error, 1)
	go readUpstream(readCtx, upstream, chunks, readErr)

	var textBuf bytes.Buffer
	var usageAcc usageAccumulator

	for {
		select {
		case <-ctx.Done():
			// The client hung up: stop forwarding, but settle what was
			// produced.
			out.Canceled = true
			out.Usage = usageAcc.result()
			out.ResourceID = usageAcc.resourceID
			out.Text = textBuf.String()
			return out, nil

		case chunk, ok := <-chunks:
			if !ok {
				// A closed channel means the upstream read finished. *Only* the
				// close counts; readErr is deliberately not selected alongside
				// it. When both are ready Go picks a branch at random, which
				// would return before the remaining frames are consumed and
				// lose the final chunk -- the one carrying usage. The error is
				// already sitting in a buffered channel by then and can simply
				// be taken.
				out.Usage = usageAcc.result()
				out.ResourceID = usageAcc.resourceID
				out.Text = textBuf.String()
				var err error
				select {
				case err = <-readErr:
				default:
				}
				if err != nil && !errors.Is(err, io.EOF) {
					out.Interrupted = out.FirstByteSent
					return out, err
				}
				return out, nil
			}
			if !out.FirstByteSent {
				// The header is committed only now: every failure before this
				// point has to be able to return a real error status.
				h := w.Header()
				h.Set("Content-Type", "text/event-stream")
				h.Set("Cache-Control", "no-cache")
				h.Set("Connection", "keep-alive")
				w.WriteHeader(http.StatusOK)
				out.FirstByteSent = true
				out.TTFB = time.Since(started)
			}
			// Byte-for-byte pass-through: written out as received, never
			// re-encoded. Re-encoding would change frame boundaries and field
			// order, and a client checking bytes would stop matching.
			if _, err := w.Write(chunk); err != nil {
				out.Usage = usageAcc.result()
				out.ResourceID = usageAcc.resourceID
				out.Text = textBuf.String()
				out.Canceled = true // a failed write means the client is gone
				return out, nil
			}
			_ = rc.Flush()
			// Shadow parsing: a parse failure must never disturb the
			// pass-through, and missing usage falls back to an estimate.
			usageAcc.consume(surface, chunk, &textBuf)

		case <-s.clock.After(s.deadlineFor(out.FirstByteSent)):
			out.TimedOut = true
			if out.FirstByteSent {
				// Idle too long inside the stream: bytes have gone out, so
				// settle what was produced and mark it interrupted.
				out.Interrupted = true
				out.Usage = usageAcc.result()
				out.ResourceID = usageAcc.resourceID
				out.Text = textBuf.String()
				return out, fmt.Errorf("proxy: the stream was idle for more than %s", s.idle)
			}
			// First-byte deadline: not one byte has been written, so the caller
			// can void and return a real error status. No heartbeat is injected
			// here -- that would disguise "the upstream is wedged" as "still
			// generating" and leave the client waiting with no upper bound.
			return out, fmt.Errorf("proxy: the upstream took more than %s to send its first byte", s.firstByte)
		}
	}
}

// deadlineFor picks the deadline for the current phase: the first-byte budget
// before the first byte, the in-stream idle bound after it.
func (s *Streamer) deadlineFor(firstByteSent bool) time.Duration {
	if firstByteSent {
		return s.idle
	}
	return s.firstByte
}

// flushable reports whether w, or some layer on its Unwrap chain, supports
// Flush.
//
// It walks the same Unwrap chain as http.ResponseController.Flush but only
// probes and never writes: calling Flush implicitly emits a 200, and this
// package requires the header to be withheld until the first byte.
func flushable(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

// readUpstream reads in chunks split on SSE event boundaries, which are blank
// lines. Splitting per event rather than per fixed buffer guarantees every
// chunk written out is a complete frame, so the client never sees half an
// event.
//
// It exits when ctx is cancelled: once the consumer stops taking data, waiting
// forever on a send is a goroutine leak -- and "the user stopped generation
// half way" is ordinary streaming behaviour rather than an anomaly, so those
// leaks would pile up without bound.
func readUpstream(ctx context.Context, r io.Reader, chunks chan<- []byte, errCh chan<- error) {
	defer close(chunks)
	br := bufio.NewReaderSize(r, 64<<10)
	var frame bytes.Buffer
	send := func(b []byte) bool {
		select {
		case chunks <- b:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			frame.Write(line)
			// A blank line ends the event.
			if isBlankLine(line) {
				buf := make([]byte, frame.Len())
				copy(buf, frame.Bytes())
				if !send(buf) {
					return
				}
				frame.Reset()
			}
		}
		if err != nil {
			if frame.Len() > 0 { // a trailing partial frame goes out too
				buf := make([]byte, frame.Len())
				copy(buf, frame.Bytes())
				if !send(buf) {
					return
				}
			}
			errCh <- err
			return
		}
	}
}

func isBlankLine(line []byte) bool {
	t := bytes.TrimRight(line, "\r\n")
	return len(t) == 0
}
