// Shutdown is configurable, so these check that the configuration is what
// decides — not that stopping works at all.
//
// The failure this guards against does not look like a failure. Before the
// settings were wired up, the window was a hard-coded 15 seconds: an operator
// who had read the documentation, seen that image requests run to about 300
// seconds, and set SHUTDOWN_TIMEOUT_SECONDS accordingly still had every
// in-flight request cut off on every restart, and nothing anywhere said so.
package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/httpx"
	communityconfig "github.com/fairlb/fairlb/internal/community/config"
)

func shutdownCfg(drain, window time.Duration) communityconfig.Config {
	return communityconfig.Config{
		Config: config.Config{DrainGrace: drain, ShutdownTimeout: window},
	}
}

func runGracefulShutdown(cfg communityconfig.Config, health *httpx.Health, servers ...*http.Server) error {
	return httpx.GracefulShutdown(httpx.ShutdownConfig{
		Health:      health,
		DrainGrace:  cfg.DrainGrace,
		Timeout:     cfg.ShutdownTimeout,
		HTTPServers: servers,
	})
}

// noKeepAlive avoids answering "is it still listening" from a pooled connection.
func noKeepAlive() *http.Client {
	return &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

func getCode(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noKeepAlive().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// slowServer serves one request that takes dur to complete, and reports both
// when the handler was entered and whether it ran to the end.
func slowServer(t *testing.T, dur time.Duration) (srv *http.Server, entered <-chan struct{}, finished *atomic.Bool) {
	t.Helper()
	in := make(chan struct{})
	var done atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(in)
		time.Sleep(dur)
		done.Store(true)
		_, _ = w.Write([]byte("done"))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://"+ln.Addr().String()+"/slow", http.NoBody)
		if rerr != nil {
			return
		}
		if resp, derr := noKeepAlive().Do(req); derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	return srv, in, &done
}

// The window is the operator's number, and a request inside it survives.
//
// Note what is asserted: whether shutdown *waited*, not whether the request
// finished. http.Server.Shutdown never interrupts a handler that is already
// running, so the request always runs to the end and "it finished" would be
// true no matter what the window said. The truncation happens after
// gracefulShutdown returns, when the process exits and the connection drops.
func TestGracefulShutdownWaitsOutTheConfiguredWindow(t *testing.T) {
	srv, entered, finished := slowServer(t, 400*time.Millisecond)
	<-entered

	if err := runGracefulShutdown(shutdownCfg(0, 5*time.Second), httpx.NewHealth(nil), srv); err != nil {
		t.Fatalf("stopping should not error: %v", err)
	}
	if !finished.Load() {
		t.Fatal("returned while the request was still running — the process exits next, " +
			"which cuts that request off")
	}
}

// A window shorter than the request gives up, and says so rather than leaving
// the operator to wonder why clients see truncated responses on every restart.
func TestGracefulShutdownGivesUpWhenTheWindowIsTooSmall(t *testing.T) {
	srv, entered, finished := slowServer(t, 3*time.Second)
	<-entered
	t.Cleanup(func() { _ = srv.Close() })

	start := time.Now()
	err := runGracefulShutdown(shutdownCfg(0, 100*time.Millisecond), httpx.NewHealth(nil), srv)
	if err != nil {
		t.Fatalf("running out of time is not a start-up failure: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("should have given up at the deadline, took %v", elapsed)
	}
	if finished.Load() {
		t.Fatal("the request finished inside the window — this case is not testing what it claims")
	}
}

// The drain window comes first and the servers keep serving through it. Both
// halves matter: draining after closing would be a pause with nothing left to
// observe it, and not draining at all would hand the proxy a refused
// connection as its first notice that anything was happening.
func TestGracefulShutdownDrainsBeforeItStopsServing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()

	health := httpx.NewHealth(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health.Up)
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	if code := getCode(t, base+"/healthz"); code != http.StatusOK {
		t.Fatalf("before shutdown /healthz should be 200, got %d", code)
	}

	// Generous, because the assertions below have to land inside this window
	// and this runs alongside the rest of the suite.
	const grace = 2 * time.Second
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- runGracefulShutdown(shutdownCfg(grace, 5*time.Second), health, srv) }()

	// Partway into the drain window: the probe already says "send no more",
	// and requests that arrive anyway are still answered.
	time.Sleep(grace / 10)
	if left := grace - time.Since(start); left < grace/2 {
		t.Fatalf("only %v of the drain window left before the assertions ran — this "+
			"machine stalled, so a failure below would not mean anything", left)
	}
	if code := getCode(t, base+"/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("while draining /healthz should be 503, got %d — a proxy polling it "+
			"has no way to know it should stop, and the drain window buys nothing", code)
	}
	if code := getCode(t, base+"/ping"); code != http.StatusOK {
		t.Fatalf("while draining the server should still answer, got %d", code)
	}

	if err := <-done; err != nil {
		t.Fatalf("stopping should not error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("returned after %v, which is less than the %v drain window — "+
			"DRAIN_GRACE_SECONDS was not waited out", elapsed, grace)
	}
	// And once it returns, the listener really is closed.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/ping", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp, derr := noKeepAlive().Do(req); derr == nil {
		_ = resp.Body.Close()
		t.Fatal("still accepting connections after shutdown returned")
	}
}

// Every server shares one budget rather than getting one each, so that the
// configured window is the total and not a per-server multiplier.
func TestGracefulShutdownSharesOneBudgetAcrossServers(t *testing.T) {
	a, enteredA, _ := slowServer(t, 3*time.Second)
	b, enteredB, _ := slowServer(t, 3*time.Second)
	<-enteredA
	<-enteredB
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	start := time.Now()
	if err := runGracefulShutdown(shutdownCfg(0, 300*time.Millisecond), httpx.NewHealth(nil), a, b); err != nil {
		t.Fatalf("running out of time is not a start-up failure: %v", err)
	}
	// Two servers each waiting out the full window would take twice as long.
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("took %v for a 300ms window across two servers — the budget is being "+
			"spent per server, so the configured total is not the total", elapsed)
	}
}
