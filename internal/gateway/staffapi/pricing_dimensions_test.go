package gwstaffapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// Every bucket the contract offers has to survive a save and come back, and the
// context-length band has to survive with it.
//
// The buckets are the reason this test exists at all. The contract names them
// input/output/audio_input/audio_output while the column names them
// in/out/audio_in/audio_out, and nothing translated between the two -- so four
// of the six buckets could not be saved at all, failing the column's CHECK,
// while cache_read and cache_write worked because those two names happen to
// coincide. Nothing covered this path before, which is why it stayed that way.
//
// The stored values are asserted in SQL as well as through the resource,
// because a round trip alone would also pass if both ends were changed to the
// wrong vocabulary together: the column's spelling is what pricing_snapshot and
// the catalog.Bucket* constants already agree on.
func TestModelPricingDimensionRatesRoundTripEveryBucketAndBand(t *testing.T) {
	_, pool, _ := newServer(t)
	ctx := context.Background()
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})

	var modelID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ('openai/dimension-round-trip', 4096) RETURNING id`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}

	want := []gwstaffapi.ModelPriceDimensionRate{
		{Bucket: gwstaffapi.Input, RateUsdPerM: "3", MinInputTokens: new(int32(0))},
		{Bucket: gwstaffapi.Input, RateUsdPerM: "6", MinInputTokens: new(int32(200001))},
		{Bucket: gwstaffapi.Output, RateUsdPerM: "22.5", MinInputTokens: new(int32(200001))},
		{Bucket: gwstaffapi.CacheRead, RateUsdPerM: "0.3"},
		{Bucket: gwstaffapi.CacheWrite, RateUsdPerM: "3.75", Variant: new("1h")},
		{Bucket: gwstaffapi.AudioInput, RateUsdPerM: "30"},
		{Bucket: gwstaffapi.AudioOutput, RateUsdPerM: "60"},
	}
	if _, _, err := svc.SaveModelPricing(ctx, modelID, gwstaffapi.ModelPricingInput{
		AcknowledgedRisks: &[]gwstaffapi.PricingRiskCode{gwstaffapi.UnknownProcurementCost},
		BillingMode:       gwstaffapi.ModelPricingInputBillingModePaid,
		OfficialRates: &gwstaffapi.DraftTokenRatesUSDPerM{
			Input: new("3"), Output: new("15"), CacheRead: new("0.3"), CacheWrite: new("3.75"),
		},
		Adjustment:     gwstaffapi.PricingAdjustment{MultiplierBps: 10000},
		SourceName:     "test-fixture",
		Reason:         "cover every dimension bucket and one long-context band",
		CheckedAt:      time.Now().UTC(),
		DimensionRates: &want,
	}, "", pgtype.UUID{}); err != nil {
		t.Fatalf("saving a rate on every bucket should work: %v", err)
	}

	got, _, err := svc.GetModelPricing(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DimensionRates == nil {
		t.Fatal("the saved dimension rates did not come back at all")
	}
	type key struct {
		bucket   gwstaffapi.ModelPriceDimensionRateBucket
		variant  string
		minInput int32
	}
	back := map[key]string{}
	for _, d := range *got.DimensionRates {
		var minInput int32
		if d.MinInputTokens != nil {
			minInput = *d.MinInputTokens
		}
		var variant string
		if d.Variant != nil {
			variant = *d.Variant
		}
		back[key{d.Bucket, variant, minInput}] = d.RateUsdPerM
	}
	for _, w := range want {
		var minInput int32
		if w.MinInputTokens != nil {
			minInput = *w.MinInputTokens
		}
		var variant string
		if w.Variant != nil {
			variant = *w.Variant
		}
		if rate := back[key{w.Bucket, variant, minInput}]; rate != w.RateUsdPerM {
			t.Errorf("bucket %q variant %q band %d came back as %q, saved as %q",
				w.Bucket, variant, minInput, rate, w.RateUsdPerM)
		}
	}
	if len(back) != len(want) {
		t.Errorf("saved %d rates, read back %d", len(want), len(back))
	}

	// The column's own vocabulary, checked directly. This is what the dataplane
	// looks up by and what every pricing_snapshot already contains.
	rows, err := pool.Query(ctx,
		`SELECT bucket, min_input_tokens FROM model_price_dimension_rates
		 WHERE model_id = $1 ORDER BY bucket, min_input_tokens`, modelID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	stored := map[string][]int32{}
	for rows.Next() {
		var bucket string
		var minInput int32
		if err := rows.Scan(&bucket, &minInput); err != nil {
			t.Fatal(err)
		}
		stored[bucket] = append(stored[bucket], minInput)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for bucket, bands := range map[string][]int32{
		"in":          {0, 200_001},
		"out":         {200_001},
		"cache_read":  {0},
		"cache_write": {0},
		"audio_in":    {0},
		"audio_out":   {0},
	} {
		if len(stored[bucket]) != len(bands) {
			t.Errorf("column bucket %q holds bands %v, want %v", bucket, stored[bucket], bands)
			continue
		}
		for i, band := range bands {
			if stored[bucket][i] != band {
				t.Errorf("column bucket %q band %d is %d, want %d", bucket, i, stored[bucket][i], band)
			}
		}
	}
}

// Two bands of one bucket are two rows, not one overwriting the other. The
// conflict target of the upsert has to name min_input_tokens for that to hold,
// and leaving it out is a silent collapse rather than an error.
func TestModelPricingKeepsEveryContextBandOfOneBucket(t *testing.T) {
	_, pool, _ := newServer(t)
	ctx := context.Background()
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})

	var modelID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ('openai/banded', 4096) RETURNING id`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	bands := []gwstaffapi.ModelPriceDimensionRate{
		{Bucket: gwstaffapi.Input, RateUsdPerM: "3", MinInputTokens: new(int32(0))},
		{Bucket: gwstaffapi.Input, RateUsdPerM: "6", MinInputTokens: new(int32(200001))},
		{Bucket: gwstaffapi.Input, RateUsdPerM: "12", MinInputTokens: new(int32(1000000))},
	}
	if _, _, err := svc.SaveModelPricing(ctx, modelID, gwstaffapi.ModelPricingInput{
		AcknowledgedRisks: &[]gwstaffapi.PricingRiskCode{gwstaffapi.UnknownProcurementCost},
		BillingMode:       gwstaffapi.ModelPricingInputBillingModePaid,
		OfficialRates: &gwstaffapi.DraftTokenRatesUSDPerM{
			Input: new("3"), Output: new("15"), CacheRead: new("0"), CacheWrite: new("0"),
		},
		Adjustment:     gwstaffapi.PricingAdjustment{MultiplierBps: 10000},
		SourceName:     "test-fixture",
		Reason:         "three bands on one bucket",
		CheckedAt:      time.Now().UTC(),
		DimensionRates: &bands,
	}, "", pgtype.UUID{}); err != nil {
		t.Fatalf("saving three bands of one bucket should work: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_dimension_rates WHERE model_id = $1 AND bucket = 'in'`,
		modelID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("the input bucket kept %d bands, want 3 -- bands of one bucket must not "+
			"collapse onto a single row", count)
	}
}

// A malformed dimension row is answered with a 4xx that names the field, not a
// 500 about the database.
//
// The spec's `minimum: 0` on min_input_tokens looks like it covers this and
// does not: nothing in this repository validates requests against the spec at
// runtime, so an unguarded value travels to the column's CHECK and comes back
// as a database error. The bucket field next to it was guarded when it was
// added; min_input_tokens was not, and two new fields in one function with only
// one of them guarded is exactly how a 500 reaches an operator who typed a
// negative number.
func TestModelPricingRefusesMalformedDimensionRows(t *testing.T) {
	_, pool, _ := newServer(t)
	ctx := context.Background()
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})

	var modelID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ('openai/dimension-refusals', 4096) RETURNING id`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}

	save := func(rates []gwstaffapi.ModelPriceDimensionRate) error {
		_, _, err := svc.SaveModelPricing(ctx, modelID, gwstaffapi.ModelPricingInput{
			AcknowledgedRisks: &[]gwstaffapi.PricingRiskCode{gwstaffapi.UnknownProcurementCost},
			BillingMode:       gwstaffapi.ModelPricingInputBillingModePaid,
			OfficialRates: &gwstaffapi.DraftTokenRatesUSDPerM{
				Input: new("3"), Output: new("15"), CacheRead: new("0.3"), CacheWrite: new("3.75"),
			},
			Adjustment: gwstaffapi.PricingAdjustment{MultiplierBps: 10000},
			SourceName: "test-fixture", Reason: "refusal cases", CheckedAt: time.Now().UTC(),
			DimensionRates: &rates,
		}, "", pgtype.UUID{})
		return err
	}

	for _, tc := range []struct {
		name  string
		rates []gwstaffapi.ModelPriceDimensionRate
	}{
		{"negative band bound", []gwstaffapi.ModelPriceDimensionRate{
			{Bucket: gwstaffapi.Input, RateUsdPerM: "3", MinInputTokens: new(int32(-1))},
		}},
		{"very negative band bound", []gwstaffapi.ModelPriceDimensionRate{
			{Bucket: gwstaffapi.Input, RateUsdPerM: "3", MinInputTokens: new(int32(-2147483648))},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := save(tc.rates)
			if err == nil {
				t.Fatal("a negative band bound should be refused")
			}
			// The distinction that matters: ErrPricingInvalid renders as a 4xx
			// naming the field, anything else renders as a 500.
			if !errors.Is(err, gwstaffapi.ErrPricingInvalid) {
				t.Errorf("should be refused as invalid input, got %v", err)
			}
		})
	}

	// The positive control: a well-formed row on the same path still saves. A
	// guard that refused everything would satisfy every assertion above.
	if err := save([]gwstaffapi.ModelPriceDimensionRate{
		{Bucket: gwstaffapi.Input, RateUsdPerM: "6", MinInputTokens: new(int32(200001))},
	}); err != nil {
		t.Errorf("a well-formed band should still save: %v", err)
	}
}
