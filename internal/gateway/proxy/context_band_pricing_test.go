package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// End to end: a model whose price rises above a prompt-length threshold is
// billed at the band the request actually lands in, on the output bucket as
// well as the input one, and the usage row records enough to replay that
// resolution later.
//
// The fixture's model is priced at $3 in and $15 out with a 1.2x sales
// multiplier; the bands added here are the Sonnet 4/4.5 shape, $6 and $22.50
// above a 200K prompt. The upstream rule is phrased "above 200K", so the
// configured bound is 200001.
func TestPipelineBillsLongPromptsAtTheContextBand(t *testing.T) {
	var promptTokens atomic.Int64
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"c1","object":"chat.completion",
			"choices":[{"message":{"content":"hi"}}],
			"usage":{"prompt_tokens":%d,"completion_tokens":1000}}`, promptTokens.Load())
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/long-context", "long-context-upstream", []string{"chat"})
	f.seedProviderProcurementBaseline(t, "openai/long-context")

	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM models WHERE slug = 'openai/long-context'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	for _, band := range []struct {
		bucket string
		nano   int64
	}{
		{"in", 6_000_000_000},
		{"out", 22_500_000_000},
	} {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO model_price_dimension_rates
				(model_id, bucket, service_tier, variant, min_input_tokens, nano_per_mtok)
			VALUES ($1, $2, 'standard', '', 200001, $3)`,
			modelID, band.bucket, band.nano); err != nil {
			t.Fatal(err)
		}
	}

	run := func(t *testing.T, requestID string, prompt int64) (charged, upstream int64, snapshot []byte) {
		t.Helper()
		promptTokens.Store(prompt)
		if _, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/long-context","messages":[{"role":"user","content":"hi"}]}`),
			Credential:   plaintext,
			RequestID:    requestID,
		}); gerr != nil {
			t.Fatalf("the request should succeed: %v", gerr)
		}
		if err := f.pool.QueryRow(ctx, `
			SELECT charged_nano, upstream_cost_usd_nano, pricing_snapshot
			FROM usage_logs WHERE request_id = $1`, requestID).Scan(
			&charged, &upstream, &snapshot,
		); err != nil {
			t.Fatal(err)
		}
		return charged, upstream, snapshot
	}

	// Computed independently. Short: 150000x$3/1e6 + 1000x$15/1e6 = 465000000
	// nano of cost, x1.2 = 558000000 charged. Long: 250000x$6/1e6 +
	// 1000x$22.50/1e6 = 1522500000 of cost, x1.2 = 1827000000 charged.
	shortCharged, shortCost, shortSnap := run(t, "context-band-short", 150_000)
	if shortCost != 465_000_000 || shortCharged != 558_000_000 {
		t.Fatalf("a 150k prompt should stay on the base band: cost=%d charged=%d, "+
			"computed independently as 465000000 / 558000000", shortCost, shortCharged)
	}
	longCharged, longCost, longSnap := run(t, "context-band-long", 250_000)
	if longCost != 1_522_500_000 || longCharged != 1_827_000_000 {
		t.Fatalf("a 250k prompt should move to the long band on both buckets: cost=%d charged=%d, "+
			"computed independently as 1522500000 / 1827000000", longCost, longCharged)
	}

	// The snapshot has to answer "which band did this bill use" without joining
	// anything, or a historical bill cannot be recomputed. It records the
	// selector rather than one resolved band, because different buckets can
	// land on different bands.
	for _, c := range []struct {
		name        string
		raw         []byte
		wantContext int64
	}{
		{"short", shortSnap, 150_000},
		{"long", longSnap, 250_000},
	} {
		var snap struct {
			SchemaVersion int   `json:"schema_version"`
			ContextTokens int64 `json:"context_tokens"`
			Official      struct {
				DimensionRates []catalog.DimensionRateSnapshot `json:"dimension_rates"`
			} `json:"official_price_table"`
		}
		if err := json.Unmarshal(c.raw, &snap); err != nil {
			t.Fatalf("%s: the pricing snapshot is malformed: %v (%s)", c.name, err, c.raw)
		}
		if snap.SchemaVersion != 1 {
			t.Errorf("%s: schema_version = %d, want 1",
				c.name, snap.SchemaVersion)
		}
		if snap.ContextTokens != c.wantContext {
			t.Errorf("%s: context_tokens = %d, want %d", c.name, snap.ContextTokens, c.wantContext)
		}
		bands := map[string]int64{}
		for _, d := range snap.Official.DimensionRates {
			if d.MinInputTokens > 0 {
				bands[d.Bucket] = d.MinInputTokens
			}
		}
		if bands["in"] != 200_001 || bands["out"] != 200_001 {
			t.Errorf("%s: the snapshot does not carry the configured bands (%v) -- "+
				"without them the bill cannot be recomputed from the row alone", c.name, bands)
		}
	}
}
