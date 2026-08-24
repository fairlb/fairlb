package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// An upstream account's declared capacity is a filter on the candidates, in the
// same position and with the same meaning as health.
//
// The number to watch is not "did it refuse" -- it is *which upstream served the
// second request*. A capacity that merely slowed a provider down, or that
// refused the whole request instead of moving on, would both leave the caller
// with something other than a normal answer from the fallback.
func TestProviderCapacityMovesTrafficToTheNextCandidate(t *testing.T) {
	var smallHits, bigHits int
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		smallHits++
		_, _ = w.Write([]byte(openAIResponse))
	}))
	t.Cleanup(small.Close)

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		bigHits++
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	// The rationed provider is tried first; the other one is the fallback.
	f.seedCatalogAt(t, small.URL, "openai", "rationed", 10)
	f.seedCatalogAt(t, f.upstream.URL, "openai", "roomy", 20)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"rationed", "roomy"})
	f.setProviderCapacity(t, "rationed", 1, 0)

	call := func() *proxy.Error {
		_, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/m","messages":[]}`),
			Credential:   plaintext,
		})
		return gerr
	}

	if gerr := call(); gerr != nil {
		t.Fatalf("the first request is within the rationed provider's allowance: %v", gerr)
	}
	if smallHits != 1 || bigHits != 0 {
		t.Fatalf("the first request belongs to the higher-priority provider: rationed=%d roomy=%d", smallHits, bigHits)
	}

	if gerr := call(); gerr != nil {
		t.Fatalf("the second request should be served by the fallback, not refused: %v", gerr)
	}
	if smallHits != 1 {
		t.Errorf("the rationed provider had nothing left and must not have been called again: %d", smallHits)
	}
	if bigHits != 1 {
		t.Errorf("the fallback should have served the second request: %d", bigHits)
	}

	// Being skipped is not an attempt: nothing was sent, so nothing was tried.
	// Counting it would let one busy provider spend a request's whole failover
	// allowance without a single upstream call having been made.
	var attempts int32
	if err := f.pool.QueryRow(ctx,
		`SELECT route_attempts FROM usage_logs WHERE org_id = $1 ORDER BY created_at DESC LIMIT 1`, org).
		Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("the skipped candidate must not count as an attempt, got %d", attempts)
	}
}

// With every candidate out of allowance the request is refused, and with the
// code that says so: saturated, not "every provider failed". They are different
// facts -- one is a rate the operator configured, the other is an outage.
func TestProviderCapacityExhaustedIsSaturated(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	f.seedCatalogAt(t, f.upstream.URL, "openai", "only", 10)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"only"})
	f.setProviderCapacity(t, "only", 1, 0)

	call := func() *proxy.Error {
		_, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/m","messages":[]}`),
			Credential:   plaintext,
		})
		return gerr
	}
	if gerr := call(); gerr != nil {
		t.Fatalf("the first request is within the allowance: %v", gerr)
	}
	gerr := call()
	if gerr == nil {
		t.Fatal("with the only candidate out of allowance the request has nowhere to go")
	}
	if gerr.Code != errcode.GatewaySaturated {
		t.Errorf("code = %s, want %s", gerr.Code, errcode.GatewaySaturated)
	}
	// Retry-After is what makes the refusal actionable rather than a shrug.
	if gerr.RetryAfter <= 0 {
		t.Error("a saturation refusal must say when to come back")
	}
}

// An undeclared allowance measures nothing. Reading a missing limit as zero
// would refuse every request on every provider that has not been configured --
// which is all of them, on an installation that never touches this.
func TestProviderWithoutDeclaredCapacityIsNotRationed(t *testing.T) {
	var hits int
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	f.seedCatalogAt(t, f.upstream.URL, "openai", "unmetered", 10)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"unmetered"})

	for i := range 3 {
		if _, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/m","messages":[]}`),
			Credential:   plaintext,
		}); gerr != nil {
			t.Fatalf("request %d should be served: %v", i+1, gerr)
		}
	}
	if hits != 3 {
		t.Errorf("every request should have reached the upstream, got %d", hits)
	}
}

// Several credentials on one provider are a pool, not a primary and its
// standbys: consecutive requests use different ones.
//
// The reason they mostly exist is to share one account's quota, and a scheme
// that always picks the same key gives that account one key's worth of
// throughput while the others sit idle -- and lets a credential revoked
// upstream go unnoticed until the first one finally trips.
func TestProviderCredentialsAreUsedInTurn(t *testing.T) {
	var seen []string
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	f.seedCatalogAt(t, f.upstream.URL, "openai", "pooled", 10)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"pooled"})
	// seedCatalogAt already left one credential behind; this is the second.
	f.addProviderKey(t, "pooled", "second", "sk-second")

	for i := range 4 {
		if _, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/m","messages":[]}`),
			Credential:   plaintext,
		}); gerr != nil {
			t.Fatalf("request %d: %v", i+1, gerr)
		}
	}

	distinct := map[string]int{}
	for _, h := range seen {
		distinct[h]++
	}
	if len(distinct) != 2 {
		t.Fatalf("both credentials should have been used across four requests, saw %v", distinct)
	}
	// Two keys and four requests: an even split is what round-robin produces,
	// and asserting it rules out "used the second one once by accident".
	for h, n := range distinct {
		if n != 2 {
			t.Errorf("credential %s served %d of 4 requests; the pool should be shared evenly", h, n)
		}
	}
}
