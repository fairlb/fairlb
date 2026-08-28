package proxy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// A pipeline built without a client of its own must still bound how long it
// will spend opening a connection.
//
// The two assembly points both pass nil, so this default is what production
// runs on. Before it existed the platform default of 30s applied, which is not
// a wrong number so much as an unowned one: nobody chose it, and it is six
// times the value the documentation stated.
//
// The assertion is on how the transport is configured, not on a live dial to
// an unreachable address. That was the first version of this test and it is
// worth recording why it was withdrawn: it dialled 192.0.2.1, the TEST-NET-1
// range reserved for documentation, and *the connection succeeded* -- some
// networks answer for addresses nobody should be answering for. A blackhole
// address is therefore not an instrument that can be relied on, and a test
// built on one fails in a way that says nothing about the code.
func TestDefaultPipelineHasAConnectTimeout(t *testing.T) {
	p := proxy.NewPipeline(proxy.PipelineConfig{})

	tr, ok := proxy.ExportUpstreamTransport(p).(*http.Transport)
	if !ok {
		t.Fatalf("the default upstream transport should be an *http.Transport, got %T",
			proxy.ExportUpstreamTransport(p))
	}
	if tr.DialContext == nil {
		t.Fatal("the default transport must install its own dialer; without one the platform default applies")
	}

	// Cloning the process-wide default rather than building one from scratch is
	// load-bearing: a hand-rolled Transport silently drops proxy support and
	// HTTP/2, and neither loss produces an error -- requests simply stop going
	// through the proxy the operator configured.
	if tr.Proxy == nil {
		t.Error("the default transport must keep proxy support; it is cloned from the platform default for exactly this reason")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("the default transport must keep HTTP/2")
	}

	// The published number and the wired number are the same one. This is the
	// half a configuration assertion can carry on its own: it cannot prove the
	// dialer honours the bound -- that is the standard library's contract --
	// but it does catch the value drifting away from what the operator was told.
	if got := proxy.ExportConnectTimeout(); got != 5*time.Second {
		t.Errorf("the documented connect timeout is 5s: %s", got)
	}

	// The default client must not be left with an unbounded overall timeout
	// either; the two bounds answer different questions and both are needed.
	if proxy.ExportClientTimeout(p) <= 0 {
		t.Error("the default client must keep its overall timeout")
	}
}

// A request refused before any upstream was contacted records zero attempts,
// and one that burned through every candidate records how many it burned.
//
// These two are asserted together because the value that used to be written
// was the same for both -- a hardcoded 1 -- which made the column unable to
// answer the only question it exists to answer.
func TestFailureRowRecordsAttemptsAndProvider(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream is down"}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "m-up", []string{"chat"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}); gerr == nil {
		t.Fatal("an upstream answering 503 should fail the request")
	}

	var attempts int32
	var providerID, providerKeyID *string
	if err := f.pool.QueryRow(ctx,
		`SELECT route_attempts, provider_id::text, provider_key_id::text
		   FROM usage_logs WHERE org_id = $1`, org).
		Scan(&attempts, &providerID, &providerKeyID); err != nil {
		t.Fatal(err)
	}
	// One candidate exists, so one attempt was made against it.
	if attempts != 1 {
		t.Errorf("route_attempts should be 1: %d", attempts)
	}
	// The provider and the credential that failed both have to be on the row.
	// A failure row that names neither is the row an investigation starts
	// from, and it used to name neither once every candidate had been tried.
	if providerID == nil {
		t.Error("the failure row must name the provider that was tried")
	}
	if providerKeyID == nil {
		t.Error("the failure row must name the credential that was used")
	}
}

// A request refused by a gate never reaches an upstream, and says so.
func TestRejectionRowRecordsZeroAttempts(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the upstream must not be contacted at all")
		w.WriteHeader(http.StatusOK)
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	// No catalog is seeded, so the model does not resolve and the request is
	// refused before any candidate is chosen.

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}); gerr == nil {
		t.Fatal("an unknown model should be refused")
	}

	var attempts int32
	if err := f.pool.QueryRow(ctx,
		`SELECT route_attempts FROM usage_logs WHERE org_id = $1`, org).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	// Zero, not one. "We refused this" and "an upstream failed" are different
	// events, and reporting the first as the second makes an upstream look
	// responsible for a request it never saw.
	if attempts != 0 {
		t.Errorf("a request refused before any upstream was contacted should record 0 attempts: %d", attempts)
	}
}
