package proxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// A /v1/responses request through the whole pipeline has to actually record a
// usage row and actually take the money.
//
// The CHECK constraint on the usage row's surface column once omitted
// 'responses'. That insert shares a transaction with settlement, so violating
// the CHECK rolled the whole transaction back: the client still received a 200
// and the upstream cost was still incurred, but no usage row was written,
// nothing was charged, and a hold was left held forever.
//
// It survived because the responses surface had *only unit tests* -- usage
// parsing and request rewriting -- and not one of them ran the full pipeline as
// far as the database. This test covers that stretch.
func TestResponsesSurfaceSettlesAndLogs(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","output":[{"content":[{"type":"output_text","text":"hi"}]}],
			"usage":{"input_tokens":10,"output_tokens":5}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	const topup = 1_000_000_000
	f.topup(t, org, topup)
	f.seedCatalog(t, "openai", "openai/resp-settle", "up-resp", []string{"responses"})

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface:      catalog.SurfaceResponses,
		Protocol:     proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/responses",
		Body:         []byte(`{"model":"openai/resp-settle","input":"hi"}`),
		Credential:   plaintext,
	})
	if gerr != nil {
		t.Fatalf("the request should succeed: %v", gerr)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}

	// 1. The usage row is written; it is the only evidence an investigation or
	// a reconciliation has.
	var surface, status string
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT surface, status, charged_nano FROM usage_logs WHERE org_id = $1`, org).
		Scan(&surface, &status, &charged); err != nil {
		t.Fatalf("no usage row was written for responses: %v", err)
	}
	if surface != "responses" || status != "ok" {
		t.Fatalf("usage row fields do not match: surface=%q status=%q", surface, status)
	}

	// 2. Money really moved. The assertion is against an independent
	// expectation rather than comparing the charged amount with itself, which
	// would only prove the two readings share a source.
	if charged <= 0 {
		t.Fatalf("charged_nano should be positive: %d", charged)
	}
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano <= 0 {
		t.Fatalf("the service was delivered but nothing was charged: settlement %+v (present=%v)", st, ok)
	}

	// 3. Every hold ends somewhere: settled or voided. A dangling hold ties up
	// the customer's balance indefinitely, and that is exactly the symptom of a
	// rolled-back transaction.
	holds, voids, settles := f.settler.Counts()
	if settles+voids != holds {
		t.Fatalf("some holds were never resolved: hold=%d settle=%d void=%d", holds, settles, voids)
	}
}

// The produced text for the estimation test and its expected token count.
// Repeat is used rather than a hand-written long string because the length has
// to be countable at a glance, or the "independently computed expectation"
// degrades into a copy of the actual output.
//
// 200 ASCII characters: 200/4 = 50 by bytes, 200/2 = 100 by runes, and
// EstimateTokens takes the larger, so 100. The assertion is anchored to that
// 100, computed here without reusing the code under test.
var (
	probeOutputText         = strings.Repeat("ab", 100)
	probeOutputTokens int32 = 100
)

// When the upstream reports no usage, the Responses surface still has to
// estimate output tokens.
//
// The estimation fallback reads the response text, and Responses puts that text
// in `output[].content[].text`. Branch on protocol instead of surface -- chat and
// responses share the openai protocol -- and this always yields an empty string,
// which estimates as zero output tokens, meaning *the service was delivered and
// the output side was never charged for*. That is not a theoretical risk:
// relays and self-hosted inference servers routinely report no usage, and every
// bit of coverage the responses surface had was at the unit level, with nothing
// reaching this point.
func TestResponsesEstimatesOutputWhenUpstreamOmitsUsage(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately no usage; that is the entire point of this test.
		_, _ = w.Write([]byte(`{"id":"resp_1","output":[{"type":"message","content":[
			{"type":"output_text","text":"` + probeOutputText + `"}]}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/resp-estimate", "up-resp", []string{"responses"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface:      catalog.SurfaceResponses,
		Protocol:     proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/responses",
		Body:         []byte(`{"model":"openai/resp-estimate","input":"hi"}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("the request should succeed: %v", gerr)
	}

	var tokensOut int32
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tokens_out, usage_estimated FROM usage_logs WHERE org_id = $1`, org).
		Scan(&tokensOut, &estimated); err != nil {
		t.Fatalf("no usage row was written: %v", err)
	}
	if !estimated {
		t.Fatal("the upstream reported no usage, so this row should be marked estimated")
	}
	if tokensOut != probeOutputTokens {
		t.Errorf("output-side estimate = %d, should be the independently computed %d; "+
			"a 0 means the response text was not extracted, which is what branching "+
			"on protocol does to Responses, and the service then goes unbilled on the output side",
			tokensOut, probeOutputTokens)
	}
}
