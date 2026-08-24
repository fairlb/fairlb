package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The timing of deadlines and cancellation cannot be verified against real
// time: such a test is either unacceptably slow or flaky under load. So an
// injected clock and a synchronising writer drive every step explicitly from
// the test.

// manualClock fires its timeouts only when the test says so.
type manualClock struct{ fire chan time.Time }

func (c manualClock) After(time.Duration) <-chan time.Time { return c.fire }

// syncRecorder is a concurrency-safe ResponseWriter: the pump writes from its
// own goroutine while the test reads the progress, and the standard recorder is
// flagged as a race under the detector when used that way.
type syncRecorder struct {
	mu   sync.Mutex
	buf  strings.Builder
	hdr  http.Header
	code int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{hdr: make(http.Header)}
}

func (r *syncRecorder) Header() http.Header { return r.hdr }

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *syncRecorder) WriteHeader(code int) { r.code = code }

// Flush is what lets http.ResponseController find a Flusher, which streaming
// requires.
func (r *syncRecorder) Flush() {}

func (r *syncRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// The first-byte deadline: give up if the upstream is slow to send its first
// event, and *at that moment nothing may have been written yet* -- that is
// exactly what lets the caller return a real HTTP error status. The older
// behaviour injected a `: processing` heartbeat every 15s and waited forever.
func TestPumpFirstByteDeadline(t *testing.T) {
	clock := manualClock{fire: make(chan time.Time)}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	rec := newSyncRecorder()
	s := &Streamer{clock: clock, firstByte: time.Hour, idle: time.Hour}

	done := make(chan StreamOutcome, 1)
	errCh := make(chan error, 1)
	go func() {
		out, err := s.Pump(context.Background(), rec, pr, catalog.SurfaceChat)
		errCh <- err
		done <- out
	}()

	clock.fire <- time.Now() // the first-byte deadline expires

	out := <-done
	if err := <-errCh; err == nil {
		t.Fatal("the first-byte deadline should be reported as an error")
	}
	if out.FirstByteSent {
		t.Error("not one byte arrived, so FirstByteSent must stay false -- otherwise this would be billed as produced")
	}
	if rec.code != 0 {
		t.Errorf("no status may be committed before the first byte, got %d -- once committed, no error status can be returned", rec.code)
	}
	if body := rec.body(); body != "" {
		t.Errorf("nothing may be written to the client before the first byte: %q", body)
	}
}

// The 200 and the SSE headers are committed exactly when the first byte
// arrives, neither earlier nor later.
func TestPumpWritesHeadersOnFirstByte(t *testing.T) {
	clock := manualClock{fire: make(chan time.Time)}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	rec := newSyncRecorder()
	s := &Streamer{clock: clock, firstByte: time.Hour, idle: time.Hour}

	done := make(chan StreamOutcome, 1)
	go func() {
		out, _ := s.Pump(context.Background(), rec, pr, catalog.SurfaceChat)
		done <- out
	}()

	_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
	_ = pw.Close()
	out := <-done

	if rec.code != http.StatusOK {
		t.Fatalf("the status should be 200 once the first byte arrives, got %d", rec.code)
	}
	if got := rec.hdr.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.hdr.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
	if !strings.Contains(rec.body(), `"content":"x"`) {
		t.Errorf("the data must arrive unchanged: %q", rec.body())
	}
	if !out.FirstByteSent {
		t.Error("once data has arrived the first byte should be marked as sent")
	}
}

// After the first byte it switches to the idle timeout: on expiry it settles
// against what was produced and marks the stream interrupted, rather than
// continuing to send heartbeats, which would keep a wedged stream alive
// forever.
func TestPumpIdleTimeoutAfterFirstByte(t *testing.T) {
	clock := manualClock{fire: make(chan time.Time)}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	rec := newSyncRecorder()
	s := &Streamer{clock: clock, firstByte: time.Hour, idle: time.Hour}

	done := make(chan StreamOutcome, 1)
	errCh := make(chan error, 1)
	go func() {
		out, err := s.Pump(context.Background(), rec, pr, catalog.SurfaceChat)
		errCh <- err
		done <- out
	}()

	// Send one chunk to establish that the first byte is out, then fire the
	// timeout.
	_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
	waitFor(t, func() bool { return strings.Contains(rec.body(), `"a"`) })
	clock.fire <- time.Now()

	out := <-done
	if err := <-errCh; err == nil {
		t.Fatal("the idle timeout should be reported as an error")
	}
	if !out.Interrupted {
		t.Error("an idle timeout must mark the stream interrupted, so it settles against what was produced")
	}
	if !out.FirstByteSent {
		t.Error("the first byte had been sent")
	}
	if out.Text != "a" {
		t.Errorf("the produced text must be kept for settlement: %q", out.Text)
	}
	// No SSE comment frame may appear at any point: client parsers vary in how
	// well they tolerate them.
	if strings.Contains(rec.body(), ": processing") {
		t.Error("no heartbeat comment should appear")
	}
}

// The client hangs up: stop forwarding, settle what was produced as usual,
// and mark it canceled.
func TestPumpClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	rec := newSyncRecorder()
	s := &Streamer{clock: manualClock{fire: make(chan time.Time)}, firstByte: time.Hour, idle: time.Hour}

	done := make(chan StreamOutcome, 1)
	go func() {
		out, _ := s.Pump(ctx, rec, pr, catalog.SurfaceChat)
		done <- out
	}()

	_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	waitFor(t, func() bool { return strings.Contains(rec.body(), "partial") })
	cancel()

	out := <-done
	if !out.Canceled {
		t.Fatalf("a client hanging up must be marked canceled: %+v", out)
	}
	if out.Text != "partial" {
		t.Errorf("what was produced must be kept for settlement: %q", out.Text)
	}
	if !out.FirstByteSent {
		t.Error("bytes have gone out, so the billing boundary is \"settle against what was produced\"")
	}
}

// waitFor polls for a condition on a millisecond scale, so no fixed sleep is
// introduced.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the condition")
}

// When the consumer leaves early the reader goroutine must exit. All three
// early-return paths -- the client hanging up, a failed write, an idle timeout
// inside the stream -- leave the chunk channel undrained, so a producer blocked
// forever on its send is a leak. Stopping generation half way is ordinary
// streaming behaviour, so those leaks would pile up without bound.
func TestPumpReaderExitsWhenConsumerLeaves(t *testing.T) {
	var sb strings.Builder
	for i := range 500 { // far more than the channel buffer, so the producer really blocks on a send
		fmt.Fprintf(&sb, "data: {\"n\":%d}\n\n", i)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Streamer{clock: realClock{}, firstByte: time.Hour, idle: time.Hour}
	_, _ = s.Pump(ctx, newSyncRecorder(),
		&cancelOnFirstRead{r: strings.NewReader(sb.String()), cancel: cancel}, catalog.SurfaceChat)

	for range 50 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the reader goroutine did not exit: before=%d after=%d", before, runtime.NumGoroutine())
}

// cancelOnFirstRead cancels the context after the first read, standing in for
// a user pressing stop.
type cancelOnFirstRead struct {
	r      io.Reader
	cancel func()
	fired  bool
}

func (c *cancelOnFirstRead) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if !c.fired {
		c.fired = true
		c.cancel()
	}
	return n, err
}
