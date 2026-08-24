package catalog_test

import (
	"errors"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

var dimBase = catalog.Price{
	InNanoPerMTok:         3_000_000_000,
	OutNanoPerMTok:        15_000_000_000,
	CacheReadNanoPerMTok:  300_000_000,
	CacheWriteNanoPerMTok: 3_750_000_000,
}

// With no overrides, the table is bit-for-bit the four base rates. That is what
// makes adding dimensions a no-op for every model that does not use them.
func TestPriceTableFallsBackToBaseColumns(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, nil)
	for _, c := range []struct {
		bucket string
		want   int64
	}{
		{catalog.BucketIn, dimBase.InNanoPerMTok},
		{catalog.BucketOut, dimBase.OutNanoPerMTok},
		{catalog.BucketCacheRead, dimBase.CacheReadNanoPerMTok},
		{catalog.BucketCacheWrite, dimBase.CacheWriteNanoPerMTok},
		// With no audio override, both audio buckets fall back to their text
		// counterparts, which is what already happens: audio tokens are
		// counted inside prompt and completion tokens and billed at the text
		// rate. Falling back to 0 would give them away, which is far worse
		// than billing them as text.
		{catalog.BucketAudioIn, dimBase.InNanoPerMTok},
		{catalog.BucketAudioOut, dimBase.OutNanoPerMTok},
	} {
		if got := pt.Get(c.bucket, catalog.TierStandard, catalog.VariantNone, 0); got != c.want {
			t.Errorf("%s should fall back to %d, got %d", c.bucket, c.want, got)
		}
	}
	if pt.HasOverrides() {
		t.Error("zero rows must not report an override")
	}
}

// The three-step fallback: exact, then same tier without a variant, then the
// default tier.
func TestPriceTableFallbackChain(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierBatch,
			Variant: catalog.VariantNone, NanoPerMtok: 1_500_000_000,
		},
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierStandard,
			Variant: catalog.VariantCache1h, NanoPerMtok: 6_000_000_000,
		},
	})

	// An exact hit.
	if got := pt.Get(catalog.BucketIn, catalog.TierBatch, catalog.VariantNone, 0); got != 1_500_000_000 {
		t.Errorf("the batch input price should hit the override: %d", got)
	}
	if got := pt.Get(catalog.BucketCacheWrite, catalog.TierStandard, catalog.VariantCache1h, 0); got != 6_000_000_000 {
		t.Errorf("the 1h cache-write price should hit the override: %d", got)
	}
	// Same bucket and tier with no variant: 5m is not configured, so it falls
	// back to standard-without-variant and then to the base rate.
	if got := pt.Get(catalog.BucketCacheWrite, catalog.TierStandard, catalog.VariantCache5m, 0); got != dimBase.CacheWriteNanoPerMTok {
		t.Errorf("an unconfigured 5m bucket should fall back to the default column %d, got %d", dimBase.CacheWriteNanoPerMTok, got)
	}
	// A batch input rate is configured but no batch output rate, so output
	// falls back to the base rate.
	if got := pt.Get(catalog.BucketOut, catalog.TierBatch, catalog.VariantNone, 0); got != dimBase.OutNanoPerMTok {
		t.Errorf("an unconfigured batch output should fall back to the default column %d, got %d", dimBase.OutNanoPerMTok, got)
	}
}

// Batch usage is billed at the batch rate: charging the standard rate for batch
// overcharges the organization.
func TestComputeUsesServiceTierPrice(t *testing.T) {
	half := []gwdb.ModelPriceDimensionRate{
		{Bucket: catalog.BucketIn, ServiceTier: catalog.TierBatch, NanoPerMtok: 1_500_000_000},
		{Bucket: catalog.BucketOut, ServiceTier: catalog.TierBatch, NanoPerMtok: 7_500_000_000},
	}
	pt := catalog.NewPriceTable(dimBase, half)
	tok := catalog.Tokens{In: 1_000_000, Out: 1_000_000}
	rates := catalog.Rates{FXRate: "1"}

	std, err := catalog.Compute(pt, pt, tok, rates)
	if err != nil {
		t.Fatal(err)
	}
	tok.ServiceTier = catalog.TierBatch
	batch, err := catalog.Compute(pt, pt, tok, rates)
	if err != nil {
		t.Fatal(err)
	}

	// Expected values computed independently: standard 3+15 = 18 USD, batch
	// 1.5+7.5 = 9 USD.
	if std.UpstreamUSDNano != 18_000_000_000 {
		t.Errorf("standard bucket cost = %d, computed independently as 18000000000", std.UpstreamUSDNano)
	}
	if batch.UpstreamUSDNano != 9_000_000_000 {
		t.Errorf("batch bucket cost = %d, computed independently as 9000000000", batch.UpstreamUSDNano)
	}
	if batch.ChargedNano >= std.ChargedNano {
		t.Error("batch must be cheaper for the organization too -- charging list price for batch is overcharging")
	}
}

// When cache writes are split by TTL they are billed per tier. 1h costs more,
// so billing the whole amount at the 5m rate undercharges.
func TestComputeSplitsCacheWriteByTTL(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierStandard,
			Variant: catalog.VariantCache1h, NanoPerMtok: 6_000_000_000,
		},
	})
	rates := catalog.Rates{FXRate: "1"}

	// A million writes, all in the 1h tier.
	split, err := catalog.Compute(pt, pt,
		catalog.Tokens{CacheWrite: 1_000_000, CacheWrite1h: 1_000_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if split.UpstreamUSDNano != 6_000_000_000 {
		t.Errorf("the 1h bucket should be charged at 6 USD, got %d", split.UpstreamUSDNano)
	}

	// Undifferentiated: the whole amount at the base cache-write rate, 3.75
	// USD.
	flat, err := catalog.Compute(pt, pt, catalog.Tokens{CacheWrite: 1_000_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if flat.UpstreamUSDNano != 3_750_000_000 {
		t.Errorf("an unbucketed row should be charged at the default column's 3.75 USD, got %d", flat.UpstreamUSDNano)
	}
	if split.UpstreamUSDNano <= flat.UpstreamUSDNano {
		t.Error("the 1h bucket is dearer than the default, so bucketing must raise the cost -- otherwise it undercharges as if it were 5m")
	}
}

// Audio tokens are subtracted out of the text buckets and reported separately,
// with the total conserved.
func TestComputeSplitsAudioFromText(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketAudioIn, ServiceTier: catalog.TierStandard,
			NanoPerMtok: 30_000_000_000,
		}, // audio costs ten times as much as text
	})
	rates := catalog.Rates{FXRate: "1"}

	// Of a million input tokens, 200k are audio: 800k x 3 + 200k x 30 = 2.4 +
	// 6 = 8.4 USD.
	q, err := catalog.Compute(pt, pt,
		catalog.Tokens{In: 1_000_000, AudioIn: 200_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if q.UpstreamUSDNano != 8_400_000_000 {
		t.Errorf("cost = %d, computed independently as 8400000000 (0.8x3 + 0.2x30)", q.UpstreamUSDNano)
	}

	// With no audio rate configured it falls back to the text rate, which is
	// identical to treating everything as text.
	plain := catalog.NewPriceTable(dimBase, nil)
	same, err := catalog.Compute(plain, plain,
		catalog.Tokens{In: 1_000_000, AudioIn: 200_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if same.UpstreamUSDNano != 3_000_000_000 {
		t.Errorf("with no audio price configured it should behave as all-text (3 USD), got %d", same.UpstreamUSDNano)
	}
}

// An unrecognized service tier must not fail billing: the request was already
// served and the money was already spent.
func TestComputeToleratesUnknownServiceTier(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, nil)
	q, err := catalog.Compute(pt, pt,
		catalog.Tokens{In: 1_000_000, ServiceTier: "flex-2026"}, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatalf("an unknown bucket must not make billing error out: %v", err)
	}
	if q.UpstreamUSDNano != 3_000_000_000 {
		t.Errorf("an unknown bucket should be charged as standard (3 USD), got %d", q.UpstreamUSDNano)
	}
}

// Cost is the official rate times the provider's scalar, and that scalar
// applies uniformly across the dimensions -- tier, audio, TTL -- and the tool
// rates. The customer price is unaffected by the cost side.
func TestProcurementScalarAppliesAcrossDimensions(t *testing.T) {
	official := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{Bucket: catalog.BucketIn, ServiceTier: catalog.TierBatch, NanoPerMtok: 1_500_000_000},
		{Bucket: catalog.BucketAudioIn, ServiceTier: catalog.TierStandard, NanoPerMtok: 30_000_000_000},
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierStandard,
			Variant: catalog.VariantCache1h, NanoPerMtok: 6_000_000_000,
		},
	}).WithToolPrices([]gwdb.ModelPriceToolRate{
		{Tool: "web_search", NanoPerCall: 10_000_000},
	})
	tokens := catalog.Tokens{
		In: 1_000_000, Out: 1_000_000, AudioIn: 200_000,
		CacheWrite: 1_000_000, CacheWrite1h: 1_000_000,
		ServiceTier: catalog.TierBatch,
		ToolCalls:   map[string]int64{"web_search": 5},
	}
	quote, err := catalog.Compute(official, official, tokens,
		catalog.Rates{ProcurementMultiplierBps: 8000, FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	// Customer price by hand: 0.8x$1.5 + 0.2x$30 + $6 + $15 + 5x$0.01 =
	// $28.25. Cost is the same official basis times 0.8 = $22.60, the scalar
	// applying to every line alike.
	if quote.ChargedNano != 28_250_000_000 {
		t.Fatalf("the customer price must not be affected by the cost multiplier: %d", quote.ChargedNano)
	}
	if quote.UpstreamUSDNano != 22_600_000_000 {
		t.Fatalf("multiplied cost = %d, want 22600000000", quote.UpstreamUSDNano)
	}
	contributions, err := catalog.ComputeExactContributions(official, official, tokens,
		catalog.Rates{ProcurementMultiplierBps: 8000, FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if contributions.UpstreamCostUSDNano.Input != "5760000000" ||
		contributions.UpstreamCostUSDNano.Output != "12000000000" ||
		contributions.UpstreamCostUSDNano.CacheWrite != "4800000000" ||
		contributions.UpstreamCostUSDNano.Tools != "40000000" ||
		contributions.ChargedNano.Input != "7200000000" ||
		contributions.ChargedNano.Output != "15000000000" ||
		contributions.ChargedNano.CacheWrite != "6000000000" ||
		contributions.ChargedNano.Tools != "50000000" {
		t.Fatalf("the multiplied cost was not bucketed by exact contribution: %+v", contributions)
	}
}

// Tool usage is counted in calls, a different unit from tokens.
func TestComputeChargesToolCallsPerCall(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, nil).
		WithToolPrices([]gwdb.ModelPriceToolRate{
			{Tool: "web_search", NanoPerCall: 10_000_000}, // 0.01 USD per call
		})
	rates := catalog.Rates{FXRate: "1"}

	q, err := catalog.Compute(pt, pt, catalog.Tokens{
		In:        1_000_000, // 3 USD
		ToolCalls: map[string]int64{"web_search": 5},
	}, rates)
	if err != nil {
		t.Fatal(err)
	}
	// Computed independently: 3 USD + 5 x 0.01 USD = 3.05 USD.
	if q.UpstreamUSDNano != 3_050_000_000 {
		t.Errorf("cost = %d, computed independently as 3050000000 (3 + 5x0.01)", q.UpstreamUSDNano)
	}

	// Tool calls have no base rate to inherit. When the upstream reports real
	// usage for which no rate exists, the request goes to the unsettled queue;
	// a missing rate must never be silently read as free.
	_, err = catalog.Compute(pt, pt, catalog.Tokens{
		In:        1_000_000,
		ToolCalls: map[string]int64{"code_interpreter": 99},
	}, rates)
	if !errors.Is(err, catalog.ErrAdvancedPriceMissing) {
		t.Fatalf("an unconfigured tool price should explicitly refuse settlement, got %v", err)
	}
}
