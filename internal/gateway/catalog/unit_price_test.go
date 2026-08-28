package catalog

import (
	"errors"
	"testing"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

func unitRate(unit, res, audio string, nano int64) gwdb.ModelPriceUnitRate {
	return gwdb.ModelPriceUnitRate{
		Unit: unit, Resolution: res, Audio: audio, ServiceTier: TierStandard, NanoPerUnit: nano,
	}
}

func stdRates() Rates {
	return Rates{ModelMultiplierBps: noDiscountBps, PlanMultiplierBps: noDiscountBps,
		ProcurementMultiplierBps: noDiscountBps, FXRate: "1"}
}

// The property the whole plane rests on: eight seconds at $0.40/second is
// $3.20, exactly, before any upstream has been called.
func TestUnitChargeIsExactAndNotAnEstimate(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		unitRate("second", "720p", "on", 400_000_000), // $0.40/s
	})
	u := Units{Quantities: map[UnitKey]int64{
		{Unit: UnitSecond, Resolution: "720p", Audio: "on"}: 8,
	}}
	q, err := ComputeUnits(pt, pt, u, stdRates())
	if err != nil {
		t.Fatal(err)
	}
	const want = 3_200_000_000 // $3.20 in nano
	if q.ChargedNano != want {
		t.Fatalf("8s at $0.40/s charged %d nano, want %d", q.ChargedNano, want)
	}
	if q.UpstreamUSDNano != want {
		t.Fatalf("cost side %d nano, want %d", q.UpstreamUSDNano, want)
	}
}

// A unit nobody priced must stop the bill, not become a free video.
func TestAnUnpricedUnitIsAnErrorNotAFreeVideo(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		unitRate("second", "720p", "", 100_000_000),
	})
	u := Units{Quantities: map[UnitKey]int64{
		{Unit: UnitSecond, Resolution: "4k"}: 10, // priced for 720p only
	}}
	_, err := ComputeUnits(pt, pt, u, stdRates())
	if !errors.Is(err, ErrUnitPriceMissing) {
		t.Fatalf("an unpriced resolution must refuse to bill, got %v", err)
	}
}

// The single most important negative property of this family: it must never
// borrow a token rate. Nothing links the two tables, so this asserts the
// absence stays an absence.
func TestUnitLookupNeverFallsBackToATokenRate(t *testing.T) {
	// A model with a full token rate card and no unit rates at all.
	tokens := NewPriceTable(Price{
		InNanoPerMTok: 5_000_000_000, OutNanoPerMTok: 15_000_000_000,
	}, nil)
	if got := tokens.Get(BucketIn, TierStandard, VariantNone, 0); got == 0 {
		t.Fatal("fixture is wrong: the token table should resolve its own base rate")
	}
	empty := NewUnitPriceTable(nil)
	if !empty.Empty() {
		t.Fatal("a model with no unit rows must report an empty unit table")
	}
	if rate, ok := empty.Lookup(UnitKey{Unit: UnitSecond}); ok {
		t.Fatalf("an unpriced second resolved to %d nano; a video must never be billed at a text rate", rate)
	}
}

// Widening drops Variant, then Audio, then Resolution -- a flat rate is one row
// with every axis empty. It must not widen across Unit or ServiceTier.
func TestUnitLookupWidensAlongItsAxesOnly(t *testing.T) {
	flat := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("second", "", "", 50_000_000)})
	if rate, ok := flat.Lookup(UnitKey{Unit: UnitSecond, Resolution: "1080p", Audio: "on"}); !ok || rate != 50_000_000 {
		t.Fatalf("a flat per-second rate must match any resolution and audio, got %d ok=%v", rate, ok)
	}
	if _, ok := flat.Lookup(UnitKey{Unit: UnitCall}); ok {
		t.Fatal("a per-second rate answered a per-call lookup")
	}
	if _, ok := flat.Lookup(UnitKey{Unit: UnitSecond, ServiceTier: TierBatch}); ok {
		t.Fatal("a standard rate answered a batch lookup; a discounted tier must be configured, not inferred")
	}

	// The specific rate wins over the flat one.
	mixed := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		unitRate("second", "", "", 50_000_000),
		unitRate("second", "1080p", "on", 90_000_000),
	})
	if rate, _ := mixed.Lookup(UnitKey{Unit: UnitSecond, Resolution: "1080p", Audio: "on"}); rate != 90_000_000 {
		t.Fatalf("the specific rate must win, got %d", rate)
	}
	if rate, _ := mixed.Lookup(UnitKey{Unit: UnitSecond, Resolution: "720p", Audio: "on"}); rate != 50_000_000 {
		t.Fatalf("an unlisted resolution must widen to the flat rate, got %d", rate)
	}
}

// The multiplier chain is shared with the token path through quoteFrom. If the
// two ever diverge, the same negotiated discount means two different things
// depending on the modality.
func TestUnitChargeRunsTheSameMultiplierChainAsTokens(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("second", "", "", 1_000_000_000)})
	u := Units{Quantities: map[UnitKey]int64{{Unit: UnitSecond}: 10}} // $10 list

	r := stdRates()
	r.ModelMultiplierBps = 12000 // 1.2x markup
	r.PlanMultiplierBps = 5000   // 0.5x customer discount
	r.ProcurementMultiplierBps = 8000
	r.FXRate = "2"

	q, err := ComputeUnits(pt, pt, u, r)
	if err != nil {
		t.Fatal(err)
	}
	// list 10 * 1.2 * 0.5 * fx 2 = 12
	if want := int64(12_000_000_000); q.ChargedNano != want {
		t.Fatalf("charged %d nano, want %d", q.ChargedNano, want)
	}
	// cost 10 * 0.8, and the exchange rate never touches the cost side
	if want := int64(8_000_000_000); q.UpstreamUSDNano != want {
		t.Fatalf("upstream cost %d nano, want %d", q.UpstreamUSDNano, want)
	}
	if q.PlanMultiplierBps != 5000 || q.ModelMultiplierBps != 12000 {
		t.Fatalf("the snapshot lost a multiplier: %+v", q)
	}
}

// Free must not zero the cost side, or margin reporting silently loses the row.
func TestFreeBillingKeepsTheRealCost(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("second", "", "", 700_000_000)})
	u := Units{Quantities: map[UnitKey]int64{{Unit: UnitSecond}: 5}}

	q, err := ComputeUnits(pt.ForBilling(true), pt, u, stdRates())
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 0 {
		t.Fatalf("a free model charged %d nano", q.ChargedNano)
	}
	if want := int64(3_500_000_000); q.UpstreamUSDNano != want {
		t.Fatalf("a free model must still record what it cost us: got %d, want %d", q.UpstreamUSDNano, want)
	}
}

func TestNegativeQuantityIsRefused(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("second", "", "", 1)})
	u := Units{Quantities: map[UnitKey]int64{{Unit: UnitSecond}: -1}}
	if _, err := ComputeUnits(pt, pt, u, stdRates()); err == nil {
		t.Fatal("a negative quantity was billed")
	}
}

// The unit has to come from the rate card. Fixing it at the call site is how a
// model priced per generation gets looked up per second, misses every row, and
// answers "unpriced" on every request while its rates sit right there.
func TestBillingUnitComesFromTheRateCard(t *testing.T) {
	perCall := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("call", "", "", 250_000_000)})
	unit, err := perCall.BillingUnit()
	if err != nil || unit != UnitCall {
		t.Fatalf("a per-call model must report the call unit, got %q %v", unit, err)
	}

	perSecond := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("second", "720p", "on", 400_000_000)})
	if unit, err := perSecond.BillingUnit(); err != nil || unit != UnitSecond {
		t.Fatalf("a per-second model must report the second unit, got %q %v", unit, err)
	}

	if _, err := NewUnitPriceTable(nil).BillingUnit(); !errors.Is(err, ErrUnitPriceMissing) {
		t.Fatalf("an empty rate card has no unit, got %v", err)
	}

	// Two units is a configuration error with no right answer, and guessing one
	// would charge some requests twice over.
	mixed := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		unitRate("second", "", "", 1), unitRate("call", "", "", 2),
	})
	if _, err := mixed.BillingUnit(); !errors.Is(err, ErrUnitAmbiguous) {
		t.Fatalf("a model priced in two units must fail closed, got %v", err)
	}
}

// Work billed to the organization's own upstream account is charged a service
// fee, not the list rate. Without this path the unit plane charged full price
// for a clip the organization had already paid the vendor for.
func TestBYOKUnitPricingChargesTheFeeNotTheListRate(t *testing.T) {
	table := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{unitRate("second", "", "", 400_000_000)})
	units := Units{Quantities: map[UnitKey]int64{{Unit: UnitSecond}: 8}}

	list, err := ComputeUnits(table, table, units, stdRates())
	if err != nil {
		t.Fatal(err)
	}
	byok, err := ComputeUnitsBYOK(table, units, 500, stdRates()) // 5% service fee
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(3_200_000_000); list.ChargedNano != want {
		t.Fatalf("platform-served charge %d, want %d", list.ChargedNano, want)
	}
	if want := int64(160_000_000); byok.ChargedNano != want {
		t.Fatalf("BYOK charge %d, want the 5%% fee %d -- charging the list rate bills the "+
			"organization twice for one clip", byok.ChargedNano, want)
	}
	// Those units were consumed on the organization's account, so recording our
	// cost would turn margin into "fee minus the whole upstream bill".
	if byok.UpstreamUSDNano != 0 {
		t.Fatalf("a BYOK job recorded %d as our cost", byok.UpstreamUSDNano)
	}
}

// imageRate builds a per-image row. Image rates vary on resolution and
// variant -- the quality tier the upstream sells -- and leave the audio axis
// empty, which is a video axis.
func imageRate(res, variant string, nano int64) gwdb.ModelPriceUnitRate {
	return gwdb.ModelPriceUnitRate{
		Unit: string(UnitImage), Resolution: res, Variant: variant,
		ServiceTier: TierStandard, NanoPerUnit: nano,
	}
}

// Four images at $0.04 each is $0.16, known before the upstream is called.
//
// The synchronous half of ADR-0227: a per-image charge is as exactly knowable
// as a per-second one, so the hold is the amount rather than an estimate.
func TestPerImageChargeCountsEveryImage(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		imageRate("1024x1024", "", 40_000_000), // $0.04 per image
	})
	u := Units{Quantities: map[UnitKey]int64{
		{Unit: UnitImage, Resolution: "1024x1024"}: 4,
	}}
	q, err := ComputeUnits(pt, pt, u, stdRates())
	if err != nil {
		t.Fatal(err)
	}
	const want = 160_000_000 // $0.16
	if q.ChargedNano != want {
		t.Fatalf("4 images at $0.04 charged %d nano, want %d", q.ChargedNano, want)
	}
}

// The quality axis has to narrow the lookup, or a model selling two tiers bills
// both at whichever row happens to be found.
func TestPerImageRateVariesOnTheQualityTier(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		imageRate("1024x1024", "low", 10_000_000),
		imageRate("1024x1024", "high", 80_000_000),
	})
	for _, tc := range []struct {
		variant string
		want    int64
	}{{"low", 10_000_000}, {"high", 80_000_000}} {
		u := Units{Quantities: map[UnitKey]int64{
			{Unit: UnitImage, Resolution: "1024x1024", Variant: tc.variant}: 1,
		}}
		q, err := ComputeUnits(pt, pt, u, stdRates())
		if err != nil {
			t.Fatalf("%s: %v", tc.variant, err)
		}
		if q.ChargedNano != tc.want {
			t.Fatalf("one %s image charged %d nano, want %d", tc.variant, q.ChargedNano, tc.want)
		}
	}
}

// A size the card does not price refuses, exactly as an unpriced resolution
// does on the video plane. Serving it would hand the caller a 4K image at the
// 1K price, or at no price at all.
func TestAnUnpricedImageSizeRefusesToBill(t *testing.T) {
	pt := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{imageRate("1024x1024", "", 40_000_000)})
	u := Units{Quantities: map[UnitKey]int64{
		{Unit: UnitImage, Resolution: "4096x4096"}: 1,
	}}
	if _, err := ComputeUnits(pt, pt, u, stdRates()); !errors.Is(err, ErrUnitPriceMissing) {
		t.Fatalf("an unpriced image size must refuse to bill, got %v", err)
	}
}

// `image` and `call` count different things, and the card decides which. A
// model priced per generation looked up per image misses every row -- which is
// why the unit comes from BillingUnit() and never from the call site.
func TestPerImageAndPerCallAreDifferentUnits(t *testing.T) {
	perCall := NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		{Unit: string(UnitCall), ServiceTier: TierStandard, NanoPerUnit: 40_000_000},
	})
	unit, err := perCall.BillingUnit()
	if err != nil {
		t.Fatal(err)
	}
	if unit != UnitCall {
		t.Fatalf("BillingUnit() = %q, want %q", unit, UnitCall)
	}
	asImages := Units{Quantities: map[UnitKey]int64{{Unit: UnitImage}: 4}}
	if _, err := ComputeUnits(perCall, perCall, asImages, stdRates()); !errors.Is(err, ErrUnitPriceMissing) {
		t.Fatalf("a per-generation card asked for images must refuse, got %v", err)
	}
}
