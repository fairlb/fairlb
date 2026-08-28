package proxy

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// A free model stops the charge, never the cost.
//
// This asserts the helper the handler actually calls, not the arithmetic in
// isolation: the defect it guards was in the plumbing, where `prepared` kept
// only the ForBilling(free) view of the rate card and quoteVideo then handed
// that same zeroed table to both sides. The arithmetic was right the whole
// time, which is why a test that built the two tables by hand stayed green.
func TestPreparedKeepsTheCostViewOfAFreeUnitRateCard(t *testing.T) {
	table := catalog.NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		{Unit: "second", ServiceTier: catalog.TierStandard, NanoPerUnit: 700_000_000},
	})
	prep := prepared{
		unitPriceTable: table,
		res: catalog.Resolution{
			ModelPricing: catalog.ModelPricingSnapshot{Priced: true, BillingMode: "free"},
		},
	}
	list, cost := prep.billingUnitPrices()
	units := catalog.Units{Quantities: map[catalog.UnitKey]int64{{Unit: catalog.UnitSecond}: 5}}

	q, err := catalog.ComputeUnits(list, cost, units, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 0 {
		t.Fatalf("a free model charged %d nano", q.ChargedNano)
	}
	if want := int64(3_500_000_000); q.UpstreamUSDNano != want {
		t.Fatalf("a free model recorded %d as its cost, want %d; margin reporting silently "+
			"loses every free request when the cost side is zeroed too", q.UpstreamUSDNano, want)
	}
}

// The paid case must be unaffected: both views bill and cost the same.
func TestPreparedPaidUnitRateCardIsUnchangedByTheSplit(t *testing.T) {
	table := catalog.NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		{Unit: "second", ServiceTier: catalog.TierStandard, NanoPerUnit: 400_000_000},
	})
	prep := prepared{
		unitPriceTable: table,
		res: catalog.Resolution{
			ModelPricing: catalog.ModelPricingSnapshot{Priced: true, BillingMode: "paid"},
		},
	}
	list, cost := prep.billingUnitPrices()
	units := catalog.Units{Quantities: map[catalog.UnitKey]int64{{Unit: catalog.UnitSecond}: 8}}
	q, err := catalog.ComputeUnits(list, cost, units, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	const want = 3_200_000_000
	if q.ChargedNano != want || q.UpstreamUSDNano != want {
		t.Fatalf("paid: charged=%d cost=%d, both want %d", q.ChargedNano, q.UpstreamUSDNano, want)
	}
}
