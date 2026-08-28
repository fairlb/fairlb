package pricing_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/pricing"
)

// Before the unit family existed an operator could not create a per-second
// model at all: the completeness constraint required four token rates, and a
// video model has none to give. This is the write path for that.
func TestOperatorCanPriceAPerSecondModel(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	w, modelID := newPricingFixture(t, pool, "google/veo-3.1")

	_, err := w.SaveModelPricing(ctx, modelID, pricing.ModelPricingWrite{
		BillingMode:   pricing.BillingPaid,
		Family:        pricing.FamilyUnits,
		MultiplierBps: 12000,
		SourceName:    "vendor price list",
		Reason:        "initial",
		// The fixture has no route, so margin cannot be computed. That warning
		// is the writer working, not the test working around it.
		AcknowledgedRisks: []string{"unknown_procurement_cost"},
		Provenance:        []byte(`{}`),
		UnitRates: &[]pricing.UnitRateInput{
			{Unit: "second", Resolution: "720p", Audio: "on", NanoPerUnit: 400_000_000},
			{Unit: "second", Resolution: "1080p", Audio: "on", NanoPerUnit: 750_000_000},
		},
	})
	if err != nil {
		t.Fatalf("a per-second model must be priceable: %v", err)
	}

	var family string
	if err := pool.QueryRow(ctx,
		`SELECT pricing_family FROM model_pricing WHERE model_id = $1`, modelID).Scan(&family); err != nil {
		t.Fatal(err)
	}
	if family != "units" {
		t.Fatalf("pricing_family = %q; without it the model is refused as unpriced at admission", family)
	}
	var rates int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_unit_rates WHERE model_id = $1`, modelID).Scan(&rates); err != nil {
		t.Fatal(err)
	}
	if rates != 2 {
		t.Fatalf("%d unit rates stored, want 2", rates)
	}
}

// nil leaves the set alone, which is the same rule the dimension and tool rates
// follow -- "do not touch" and "remove them all" are different instructions.
//
// And clearing every rate on a paid unit-priced model is refused, because that
// is a model that cannot be charged for: admission would answer 503 to every
// request against it. Better to refuse the save than to publish a model that
// silently stops working.
func TestUnitRatesAreLeftAloneByNilAndCannotBeClearedWhilePaid(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	w, modelID := newPricingFixture(t, pool, "google/veo-rules")

	base := pricing.ModelPricingWrite{
		BillingMode: pricing.BillingPaid, Family: pricing.FamilyUnits,
		MultiplierBps: 10000, SourceName: "s", Reason: "r",
		AcknowledgedRisks: []string{"unknown_procurement_cost"},
		Provenance:        []byte(`{}`),
		UnitRates:         &[]pricing.UnitRateInput{{Unit: "second", NanoPerUnit: 100}},
	}
	if _, err := w.SaveModelPricing(ctx, modelID, base); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM model_price_unit_rates WHERE model_id = $1`, modelID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	untouched := base
	untouched.UnitRates = nil
	if _, err := w.SaveModelPricing(ctx, modelID, untouched); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("a nil set removed the rates; nil means leave them alone")
	}

	cleared := base
	empty := []pricing.UnitRateInput{}
	cleared.UnitRates = &empty
	if _, err := w.SaveModelPricing(ctx, modelID, cleared); err == nil {
		t.Fatal("clearing every rate on a paid unit model was accepted; " +
			"every request against it would then be refused as unpriced")
	}
	if count() != 1 {
		t.Fatalf("a refused save still removed rates: %d left", count())
	}
}

// newPricingFixture creates a model and a writer bound to it.
func newPricingFixture(t *testing.T, pool *pgxpool.Pool, slug string) (*pricing.Writer, uuid.UUID) {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO models (slug) VALUES ($1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return pricing.NewWriter(pool, nil, nil, nil), id
}
