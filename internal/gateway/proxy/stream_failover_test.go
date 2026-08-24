package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// A streamed request whose first candidate fails before sending anything is
// served by the next one, and the caller cannot tell.
//
// This is the case the streaming path could not handle at all: it picked one
// usable candidate and stayed with it, so an upstream that answered 503
// instantly failed the request outright while the identical failure on the
// identical route was recovered from for a non-streamed one. The gateway's own
// front page promises automatic failover; this is the half of it that was
// missing.
func TestStreamFailsOverBeforeFirstByte(t *testing.T) {
	var firstHits, secondHits int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer first.Close()

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		secondHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	// The failing provider gets the lower priority number, so it is tried
	// first: priority is a hard constraint, which makes the ordering
	// deterministic rather than a matter of which way the weighted shuffle
	// happened to fall.
	f.seedCatalogAt(t, first.URL, "openai", "p-down", 10)
	f.seedCatalogAt(t, f.upstream.URL, "openai", "p-up", 20)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"p-down", "p-up"})

	rec := httptest.NewRecorder()
	gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI)
	if gerr != nil {
		t.Fatalf("the healthy candidate should have served this: %v", gerr)
	}
	if firstHits != 1 || secondHits != 1 {
		t.Errorf("each candidate should be tried once: down=%d up=%d", firstHits, secondHits)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("the client should see one clean 200: %d", rec.Code)
	}
	// The body must be the healthy upstream's stream, whole. A client that
	// received the first candidate's error frame *and then* the second's
	// content would be worse off than one that simply failed.
	if body := rec.Body.String(); !strings.Contains(body, `"content":"hi"`) || strings.Contains(body, "down") {
		t.Errorf("the client should see only the healthy stream: %q", body)
	}

	// The usage row names the candidate that served it and carries the one
	// that did not.
	var attempts int32
	var trail []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT route_attempts, attempts FROM usage_logs WHERE org_id = $1`, org).
		Scan(&attempts, &trail); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("two candidates were tried: %d", attempts)
	}
	var hops []map[string]any
	if err := json.Unmarshal(trail, &hops); err != nil {
		t.Fatalf("the trail should be a JSON array: %v (%s)", err, trail)
	}
	if len(hops) != 1 {
		t.Fatalf("only the failed hop belongs in the trail, the winner is the row itself: %s", trail)
	}
	if got := hops[0]["http_status"]; got != float64(503) {
		t.Errorf("the failed hop should record the status that caused it: %v", got)
	}
	if got := hops[0]["class"]; got != "provider" {
		t.Errorf("a 503 is a provider-class failure: %v", got)
	}
}

// Once a byte has gone out, a stream is not retried -- the client already has
// content that a second attempt would not reproduce.
func TestStreamDoesNotFailOverAfterFirstByte(t *testing.T) {
	var firstHits, secondHits int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits++
		w.Header().Set("Content-Type", "text/event-stream")
		// One good frame, then the connection dies.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server should support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer first.Close()

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		secondHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalogAt(t, first.URL, "openai", "p-dies", 10)
	f.seedCatalogAt(t, f.upstream.URL, "openai", "p-up", 20)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"p-dies", "p-up"})

	rec := httptest.NewRecorder()
	_ = f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI)

	if firstHits != 1 {
		t.Errorf("the first candidate should be tried once: %d", firstHits)
	}
	if secondHits != 0 {
		t.Errorf("no rotation is allowed once bytes are out: the second provider was hit %d times", secondHits)
	}
	// What was produced is still billed: part of the service was delivered.
	if _, ok := f.settler.LastSettle(); !ok {
		t.Error("an interrupted stream settles against what it produced")
	}
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM usage_logs WHERE org_id = $1`, org).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "upstream_error" {
		t.Errorf("an interrupted stream is an upstream error: %q", status)
	}
}

// A client-class failure is not retried on the streaming path either: a
// malformed request is malformed for every candidate, and trying the next one
// only spends somebody's quota to get the same answer.
func TestStreamDoesNotRotateOnClientError(t *testing.T) {
	var firstHits, secondHits int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"max_tokens is too large"}}`))
	}))
	defer first.Close()

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		secondHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalogAt(t, first.URL, "openai", "p-picky", 10)
	f.seedCatalogAt(t, f.upstream.URL, "openai", "p-up", 20)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"p-picky", "p-up"})

	rec := httptest.NewRecorder()
	gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI)

	if gerr == nil {
		t.Fatal("a 400 should reach the caller rather than being retried away")
	}
	if firstHits != 1 || secondHits != 0 {
		t.Errorf("a client-class failure must not rotate: picky=%d up=%d", firstHits, secondHits)
	}
	// The upstream's own words survive: they are what locates the bad
	// parameter.
	if gerr.UpstreamMessage == "" {
		t.Error("a passed-through 400 must carry the upstream's own text")
	}
}
