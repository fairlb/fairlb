package catalog_test

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// The Sonnet 4/4.5 shape: $3/$15 up to a 200K prompt, $6/$22.50 above it. The
// upstream rule is phrased "above 200K", and with an inclusive lower bound that
// is configured as 200001.
const (
	sonnetLongIn  = 6_000_000_000
	sonnetLongOut = 22_500_000_000
	aboveTwoHK    = 200_001
)

func sonnetBands() []gwdb.ModelPriceDimensionRate {
	return []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: aboveTwoHK, NanoPerMtok: sonnetLongIn,
		},
		{
			Bucket: catalog.BucketOut, ServiceTier: catalog.TierStandard,
			MinInputTokens: aboveTwoHK, NanoPerMtok: sonnetLongOut,
		},
	}
}

// A long prompt is charged at the long-prompt rate, and so is its output.
//
// Wiring the length axis to the input buckets alone is the mistake this guards:
// Anthropic raises the output price above the threshold too, so a
// long-prompt request whose output was billed at the short rate undercharges on
// a bucket nobody was looking at. The per-bucket contributions are asserted
// individually for exactly that reason -- a total can come out right with two
// buckets wrong in opposite directions.
func TestContextBandsPriceLongPromptsHigherOnEveryBucket(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, sonnetBands())
	rates := catalog.Rates{FXRate: "1"}

	short, err := catalog.Compute(pt, pt, catalog.Tokens{In: 150_000, Out: 1_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	long, err := catalog.Compute(pt, pt, catalog.Tokens{In: 250_000, Out: 1_000}, rates)
	if err != nil {
		t.Fatal(err)
	}

	// Computed independently. Short: 150000x3/1e6 + 1000x15/1e6 USD, i.e.
	// 450000000 + 15000000 nano. Long: 250000x6/1e6 + 1000x22.5/1e6, i.e.
	// 1500000000 + 22500000 nano.
	if short.UpstreamUSDNano != 465_000_000 {
		t.Errorf("a 150k prompt should stay on the base band: %d want 465000000", short.UpstreamUSDNano)
	}
	if long.UpstreamUSDNano != 1_522_500_000 {
		t.Errorf("a 250k prompt should move to the long band: %d want 1522500000", long.UpstreamUSDNano)
	}

	shortParts, err := catalog.ComputeExactContributions(pt, pt,
		catalog.Tokens{In: 150_000, Out: 1_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	longParts, err := catalog.ComputeExactContributions(pt, pt,
		catalog.Tokens{In: 250_000, Out: 1_000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if shortParts.ChargedNano.Input != "450000000" || shortParts.ChargedNano.Output != "15000000" {
		t.Errorf("the short-band buckets are wrong: %+v", shortParts.ChargedNano)
	}
	if longParts.ChargedNano.Input != "1500000000" {
		t.Errorf("the long-band input bucket is wrong: %+v", longParts.ChargedNano)
	}
	if longParts.ChargedNano.Output != "22500000" {
		t.Errorf(
			"the output bucket stayed on the short rate (%s, want 22500000) -- "+
				"the threshold has to reach every bucket, not only the input ones",
			longParts.ChargedNano.Output)
	}
}

// Where the boundary falls, decided once: the bound is inclusive, so a prompt
// exactly on it is inside the band.
func TestContextBandLowerBoundIsInclusive(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{{
		Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
		MinInputTokens: 200_000, NanoPerMtok: sonnetLongIn,
	}})
	for _, c := range []struct {
		name    string
		context int64
		want    int64
	}{
		{"one below", 199_999, dimBase.InNanoPerMTok},
		{"exactly on the bound", 200_000, sonnetLongIn},
		{"one above", 200_001, sonnetLongIn},
	} {
		got := pt.Get(catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, c.context)
		if got != c.want {
			t.Errorf("%s: rate = %d, want %d", c.name, got, c.want)
		}
	}

	// The consequence for configuration, asserted rather than only documented:
	// an upstream rule phrased "above 200K" has to be entered as 200001, and
	// then a prompt of exactly 200000 stays on the base rate.
	strict := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{{
		Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
		MinInputTokens: aboveTwoHK, NanoPerMtok: sonnetLongIn,
	}})
	if got := strict.Get(
		catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, 200_000,
	); got != dimBase.InNanoPerMTok {
		t.Errorf("200000 tokens against a 200001 band should stay on the base rate, got %d", got)
	}
}

// The band is selected against the whole prompt, and the whole prompt is
// exactly In + CachedRead + CacheWrite.
func TestContextBandSelectorCountsCacheButNotAudioTwice(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{{
		Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
		MinInputTokens: 200_000, NanoPerMtok: sonnetLongIn,
	}})

	// 100k fresh input plus 60k read from cache plus 50k written to cache is a
	// 210k prompt as far as the upstream is concerned. Selecting on In alone
	// would read 100k and bill the whole thing at the short rate.
	cached := catalog.Tokens{In: 100_000, CachedRead: 60_000, CacheWrite: 50_000}
	if got := cached.ContextTokens(); got != 210_000 {
		t.Fatalf("the selector should be the whole prompt: %d want 210000", got)
	}
	if got := pt.Get(
		catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, cached.ContextTokens(),
	); got != sonnetLongIn {
		t.Errorf("cache buckets belong to the prompt, so 210k should reach the band: %d", got)
	}

	// Audio input is a subset of In, already counted. Adding it would turn a
	// 150k prompt into an apparent 250k one and move it across a threshold it
	// never crossed.
	audio := catalog.Tokens{In: 150_000, AudioIn: 100_000}
	if got := audio.ContextTokens(); got != 150_000 {
		t.Fatalf("audio input is a subset of In and must not be counted twice: %d want 150000", got)
	}
	if got := pt.Get(
		catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, audio.ContextTokens(),
	); got != dimBase.InNanoPerMTok {
		t.Errorf("a 150k prompt with audio should stay on the base rate: %d", got)
	}
}

// The length is resolved inside each fallback step, not ahead of them. Order the
// other way round and a long batch request would pick up the standard tier's
// long-context rate -- pricing batch usage at the standard rate, which is what
// the tier axis exists to prevent.
func TestContextBandResolvesWithinEachFallbackStep(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierBatch,
			MinInputTokens: 0, NanoPerMtok: 1_500_000_000,
		},
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: 200_000, NanoPerMtok: sonnetLongIn,
		},
	})

	if got := pt.Get(
		catalog.BucketIn, catalog.TierBatch, catalog.VariantNone, 250_000,
	); got != 1_500_000_000 {
		t.Errorf(
			"a long batch prompt should keep the batch rate (%d, want 1500000000) -- "+
				"the length axis must not override the tier axis", got)
	}
	if got := pt.Get(
		catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, 250_000,
	); got != sonnetLongIn {
		t.Errorf("a long standard prompt should take the long band: %d", got)
	}
}

// The mirror image of the test above, and the one that is easy to miss: the
// length axis must not move a request across the tier boundary in the fallback
// direction either.
//
// A tier this model prices is a tier this request stays in. Configure a batch
// rate that only covers long prompts and the short batch prompts still belong to
// batch -- they fall to the model's base rate, not to whatever the standard tier
// charges. Letting them drop into the standard tier bills batch usage at the
// standard rate, which is the mistake the tier axis exists to prevent, arrived at
// from the other side.
//
// The numbers below are chosen so every reading is distinct: the wrong answer
// cannot coincide with the right one.
func TestContextBandMissStaysInsideTheServiceTier(t *testing.T) {
	// A batch rate that only covers long prompts, alongside a fully banded
	// standard tier.
	batchLongOnly := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierBatch,
			MinInputTokens: aboveTwoHK, NanoPerMtok: 3_000_000_000,
		},
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: 0, NanoPerMtok: 5_000_000_000,
		},
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: aboveTwoHK, NanoPerMtok: 9_000_000_000,
		},
	})
	if got := batchLongOnly.Get(
		catalog.BucketIn, catalog.TierBatch, catalog.VariantNone, 150_000,
	); got != dimBase.InNanoPerMTok {
		t.Errorf(
			"a short batch prompt resolved to %d, want the base rate %d -- "+
				"5000000000 is the standard tier's short band, which is neither the "+
				"batch price nor the base price",
			got, dimBase.InNanoPerMTok)
	}
	// The other direction still holds: a long batch prompt keeps the batch rate.
	if got := batchLongOnly.Get(
		catalog.BucketIn, catalog.TierBatch, catalog.VariantNone, 250_000,
	); got != 3_000_000_000 {
		t.Errorf("a long batch prompt should keep the batch band: %d want 3000000000", got)
	}

	// The same on the priority tier, where the standard row is deliberately
	// dearer than the base rate so that "fell into standard" and "fell to base"
	// read differently.
	priorityLongOnly := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketOut, ServiceTier: catalog.TierPriority,
			MinInputTokens: aboveTwoHK, NanoPerMtok: 45_000_000_000,
		},
		{
			Bucket: catalog.BucketOut, ServiceTier: catalog.TierStandard,
			MinInputTokens: 0, NanoPerMtok: 20_000_000_000,
		},
	})
	if got := priorityLongOnly.Get(
		catalog.BucketOut, catalog.TierPriority, catalog.VariantNone, 10_000,
	); got != dimBase.OutNanoPerMTok {
		t.Errorf(
			"a short priority prompt resolved to %d, want the base rate %d (20000000000 "+
				"is the standard tier's rate)", got, dimBase.OutNanoPerMTok)
	}

	// A variant miss inside a priced tier is the same case: the batch tier is
	// priced for this bucket, so the request stays in it.
	batchVariantLongOnly := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierBatch,
			Variant: catalog.VariantCache1h, MinInputTokens: aboveTwoHK, NanoPerMtok: 8_000_000_000,
		},
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierStandard,
			MinInputTokens: 0, NanoPerMtok: 6_000_000_000,
		},
	})
	if got := batchVariantLongOnly.Get(
		catalog.BucketCacheWrite, catalog.TierBatch, catalog.VariantCache1h, 150_000,
	); got != dimBase.CacheWriteNanoPerMTok {
		t.Errorf(
			"a short 1h batch cache write resolved to %d, want the base rate %d "+
				"(6000000000 is the standard tier's rate)", got, dimBase.CacheWriteNanoPerMTok)
	}
}

// What the boundary above must NOT do: the descent inside one tier is
// untouched, and so is the tier fallback for a tier this model does not price
// at all.
//
// Both of those predate the length axis, and tightening one boundary is a
// standing invitation to tighten the neighbouring one by accident. Here that
// would overcharge in exactly the way the neighbouring rule is meant to prevent
// -- a batch request pushed onto the base rate, which is the standard-tier
// price by construction.
func TestContextBandMissStillFallsBackWithinATierAndAcrossAnUnpricedOne(t *testing.T) {
	// A batch tier priced twice: a 1h cache-write band for long prompts, and an
	// undifferentiated batch rate covering everything. The short 1h request
	// belongs to the undifferentiated batch rate -- it is in the same tier.
	pricedTwice := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierBatch,
			Variant: catalog.VariantCache1h, MinInputTokens: aboveTwoHK, NanoPerMtok: 4_000_000_000,
		},
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierBatch,
			MinInputTokens: 0, NanoPerMtok: 1_500_000_000,
		},
	})
	if got := pricedTwice.Get(
		catalog.BucketCacheWrite, catalog.TierBatch, catalog.VariantCache1h, 150_000,
	); got != 1_500_000_000 {
		t.Errorf(
			"a short 1h batch cache write resolved to %d, want the undifferentiated batch "+
				"rate 1500000000 -- the base rate %d is the standard price, so answering it "+
				"here would overcharge batch by 2.5x", got, dimBase.CacheWriteNanoPerMTok)
	}

	// A tier with no rate at all for this bucket still falls back to the
	// standard tier, exactly as it did before the length axis existed.
	unpricedTier := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{{
		Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
		MinInputTokens: 0, NanoPerMtok: 5_000_000_000,
	}})
	if got := unpricedTier.Get(
		catalog.BucketIn, catalog.TierBatch, catalog.VariantNone, 150_000,
	); got != 5_000_000_000 {
		t.Errorf(
			"a batch prompt on a model with no batch rate should still read the standard "+
				"rate 5000000000, got %d", got)
	}
}

// A step whose bands all sit above this prompt is a miss, not a rate of zero:
// it falls through, and a model that configures only a long band still charges
// its base rate below the threshold.
func TestContextBandStepWithNoQualifyingBandFallsThrough(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{{
		Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
		MinInputTokens: 200_000, NanoPerMtok: sonnetLongIn,
	}})
	if got := pt.Get(
		catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, 150_000,
	); got != dimBase.InNanoPerMTok {
		t.Errorf(
			"a prompt below every configured band should fall through to the base rate "+
				"(%d, want %d) -- answering 0 would give those requests away",
			got, dimBase.InNanoPerMTok)
	}
}

// The dearest qualifying band wins when several apply, which is what makes the
// bands a partition rather than an unordered set.
func TestContextBandsPickTheDearestQualifyingBand(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: 1_000_000, NanoPerMtok: 12_000_000_000,
		},
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: 200_000, NanoPerMtok: sonnetLongIn,
		},
		{
			Bucket: catalog.BucketIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: 0, NanoPerMtok: 3_000_000_000,
		},
	})
	for _, c := range []struct {
		context, want int64
	}{
		{0, 3_000_000_000},
		{199_999, 3_000_000_000},
		{200_000, sonnetLongIn},
		{999_999, sonnetLongIn},
		{1_000_000, 12_000_000_000},
		{5_000_000, 12_000_000_000},
	} {
		if got := pt.Get(
			catalog.BucketIn, catalog.TierStandard, catalog.VariantNone, c.context,
		); got != c.want {
			t.Errorf("a %d-token prompt resolved to %d, want %d", c.context, got, c.want)
		}
	}
}

// The regression that matters most: with no band configured, the four buckets
// come out exactly as they did before this axis existed.
//
// The expected values are worked out by hand rather than taken from a second
// call through the same code, and they are asserted per bucket -- a total can
// come out right with two buckets wrong in opposite directions.
func TestNoContextBandsLeavesEveryBucketUnchanged(t *testing.T) {
	rates := catalog.Rates{FXRate: "1"}
	plain := catalog.NewPriceTable(dimBase, nil)
	// Dimension rows that use every other axis and none of the length axis. If
	// the length ever reached these lookups, this table is where it would show.
	dimensioned := catalog.NewPriceTable(dimBase, []gwdb.ModelPriceDimensionRate{
		{Bucket: catalog.BucketIn, ServiceTier: catalog.TierBatch, NanoPerMtok: 1_500_000_000},
		{Bucket: catalog.BucketAudioIn, ServiceTier: catalog.TierStandard, NanoPerMtok: 30_000_000_000},
		{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierStandard,
			Variant: catalog.VariantCache1h, NanoPerMtok: 6_000_000_000,
		},
	})

	for _, c := range []struct {
		name                                 string
		table                                catalog.PriceTable
		tokens                               catalog.Tokens
		input, output, cacheRead, cacheWrite string
	}{
		{
			name: "base rates only", table: plain,
			tokens: catalog.Tokens{In: 1_000_000, Out: 1_000_000},
			// 1M x $3 and 1M x $15.
			input: "3000000000", output: "15000000000", cacheRead: "0", cacheWrite: "0",
		},
		{
			name: "every bucket, base rates only", table: plain,
			tokens: catalog.Tokens{
				In: 1_000_000, Out: 2_000_000, CachedRead: 500_000, CacheWrite: 400_000,
			},
			// 1M x $3, 2M x $15, 0.5M x $0.30, 0.4M x $3.75.
			input: "3000000000", output: "30000000000",
			cacheRead: "150000000", cacheWrite: "1500000000",
		},
		{
			name: "tier, audio and TTL overrides", table: dimensioned,
			tokens: catalog.Tokens{
				In: 1_000_000, Out: 1_000_000, AudioIn: 200_000,
				CacheWrite: 1_000_000, CacheWrite1h: 1_000_000,
				ServiceTier: catalog.TierBatch,
			},
			// 0.8M x $1.50 + 0.2M x $30 on the input side, 1M x $15 out (no
			// batch output rate is configured), 1M x $6 for the 1h cache write.
			input: "7200000000", output: "15000000000",
			cacheRead: "0", cacheWrite: "6000000000",
		},
	} {
		parts, err := catalog.ComputeExactContributions(c.table, c.table, c.tokens, rates)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if parts.ChargedNano.Input != c.input || parts.ChargedNano.Output != c.output ||
			parts.ChargedNano.CacheRead != c.cacheRead ||
			parts.ChargedNano.CacheWrite != c.cacheWrite {
			t.Errorf(
				"%s: in/out/cacheRead/cacheWrite = %s/%s/%s/%s, computed independently as %s/%s/%s/%s",
				c.name,
				parts.ChargedNano.Input, parts.ChargedNano.Output,
				parts.ChargedNano.CacheRead, parts.ChargedNano.CacheWrite,
				c.input, c.output, c.cacheRead, c.cacheWrite)
		}
	}
}

// The same regression stated as the property it really is: without a band, cost
// is linear in the prompt length, so nothing happens at any particular size. A
// length axis that leaked into an unconfigured model would break linearity at
// whichever size it leaked at, and the sizes here bracket the one every
// upstream bands on.
func TestNoContextBandsMakesPromptLengthLinear(t *testing.T) {
	plain := catalog.NewPriceTable(dimBase, nil)
	rates := catalog.Rates{FXRate: "1"}
	for _, in := range []int64{0, 1, 199_999, 200_000, 200_001, 1_000_000, 10_000_000} {
		q, err := catalog.Compute(plain, plain, catalog.Tokens{In: in}, rates)
		if err != nil {
			t.Fatalf("%d input tokens: %v", in, err)
		}
		// Independent formula: tokens x nano-per-million / one million. The
		// base rate is a whole multiple of a million, so this is exact.
		want := in * dimBase.InNanoPerMTok / 1_000_000
		if q.UpstreamUSDNano != want {
			t.Errorf("%d input tokens cost %d, computed independently as %d", in, q.UpstreamUSDNano, want)
		}
	}
}

// The hold is a gate: the input figure is a character-count heuristic, so a
// request estimated just under a threshold can settle just above it. It
// therefore holds at the dearest band rather than at the band the estimate
// lands in.
func TestEstimateHoldsAtTheDearestContextBand(t *testing.T) {
	pt := catalog.NewPriceTable(dimBase, sonnetBands())
	ceiling := pt.LongContextCeiling()
	if ceiling.InNanoPerMTok != sonnetLongIn || ceiling.OutNanoPerMTok != sonnetLongOut {
		t.Fatalf("the ceiling should be the dearest band on each bucket: %+v", ceiling)
	}

	held, err := catalog.Estimate(catalog.EstimateInput{
		Price: dimBase, LongContext: ceiling,
		InputTokens: 1_000, DefaultMaxCap: 4_096, Rates: catalog.Rates{FXRate: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Computed independently at the long band: 1000x$6/1e6 + 4096x$22.50/1e6 =
	// 6000000 + 92160000 nano.
	if held != 98_160_000 {
		t.Fatalf("the hold should price at the long band: %d want 98160000", held)
	}

	// A short estimate must not lower the hold below the ceiling: that is the
	// direction that lets through a request the balance cannot pay for.
	atBase, err := catalog.Estimate(catalog.EstimateInput{
		Price:       dimBase,
		InputTokens: 1_000, DefaultMaxCap: 4_096, Rates: catalog.Rates{FXRate: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if atBase >= held {
		t.Fatalf("holding at the base band (%d) must be cheaper than at the ceiling (%d)", atBase, held)
	}
}

// The ceiling is raised by the length axis alone. Folding the other axes in
// would change the hold of models that configure no bands at all, which is the
// one thing this change must not do; and a free model's ceiling is zero.
func TestLongContextCeilingIgnoresEveryOtherAxis(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []gwdb.ModelPriceDimensionRate
	}{
		{"no rows at all", nil},
		{"a dearer batch tier", []gwdb.ModelPriceDimensionRate{
			{Bucket: catalog.BucketIn, ServiceTier: catalog.TierPriority, NanoPerMtok: 90_000_000_000},
		}},
		{"a dearer cache-write TTL", []gwdb.ModelPriceDimensionRate{{
			Bucket: catalog.BucketCacheWrite, ServiceTier: catalog.TierStandard,
			Variant: catalog.VariantCache1h, NanoPerMtok: 60_000_000_000,
		}}},
		{"a dearer audio rate", []gwdb.ModelPriceDimensionRate{
			{Bucket: catalog.BucketAudioIn, ServiceTier: catalog.TierStandard, NanoPerMtok: 30_000_000_000},
		}},
		{"an audio rate that is also a long band", []gwdb.ModelPriceDimensionRate{{
			Bucket: catalog.BucketAudioIn, ServiceTier: catalog.TierStandard,
			MinInputTokens: 200_000, NanoPerMtok: 30_000_000_000,
		}}},
	} {
		if got := catalog.NewPriceTable(dimBase, c.rows).LongContextCeiling(); got != (catalog.Price{}) {
			t.Errorf("%s must leave the hold ceiling at zero, got %+v", c.name, got)
		}
	}

	// A free model charges nothing, bands included, so its ceiling is zero and
	// its hold is what it always was.
	free := catalog.NewPriceTable(dimBase, sonnetBands()).ForBilling(true)
	if got := free.LongContextCeiling(); got != (catalog.Price{}) {
		t.Errorf("a free model must not hold against a band it will never charge: %+v", got)
	}
}
