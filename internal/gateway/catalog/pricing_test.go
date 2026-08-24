package catalog_test

import (
	"math"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// An absurd token count reported by an upstream is refused at the billing
// entrance -- validation is built into Compute -- so there is no path that
// inflates a charge. It is refused rather than silently truncated: truncating
// undercharges, which is equally wrong.
func TestComputeRejectsInsaneUpstreamTokenCounts(t *testing.T) {
	p := catalog.Price{InNanoPerMTok: 3_000_000_000, OutNanoPerMTok: 15_000_000_000}
	if _, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p),
		catalog.Tokens{In: 1000, Out: 1 << 40},
		catalog.Rates{FXRate: "1"}); err == nil {
		t.Fatal("tokens_out = 2^40 must be refused at the billing entry point, with no quote produced")
	}
}

// A discount and a model-level markup both take effect when both are present.
//
// The expected values are computed inside the test and reuse none of the code
// under test: three paths consuming the same number and agreeing is not
// evidence that the number is right.
func TestDiscountMultipliesWithMarkupOverride(t *testing.T) {
	// $3 per million input; 1000 tokens gives an upstream cost of 3e9 x 1000 /
	// 1e6 = 3_000_000 nano.
	p := catalog.Price{InNanoPerMTok: 3_000_000_000}
	tok := catalog.Tokens{In: 1000}
	const upstream = 3_000_000 // 3e9 nano/Mtok × 1000 / 1e6

	// A model-level markup of 4000 basis points (+40%) and a organization discount
	// of 8000 basis points (a 20% reduction).
	q, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), tok, catalog.Rates{
		ModelMultiplierBps: 14000, PlanMultiplierBps: 8000, FXRate: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.UpstreamUSDNano != upstream {
		t.Fatalf("upstream cost base = %d, want %d (every hand calculation below builds on it)", q.UpstreamUSDNano, upstream)
	}
	// By hand: 3_000_000 x 1.4 x 0.8 = 3_360_000.
	const want = 3_360_000
	if q.ChargedNano != want {
		t.Fatalf("charged = %d, want %d (markup and discount must multiply, not override each other)", q.ChargedNano, want)
	}
	// The same markup with no discount must cost more, and by exactly 1/0.8.
	full, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), tok, catalog.Rates{ModelMultiplierBps: 14000, FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if full.ChargedNano != 4_200_000 {
		t.Fatalf("charged without a discount = %d, want 4200000", full.ChargedNano)
	}
	if q.ChargedNano >= full.ChargedNano {
		t.Error("the discounted amount must be lower -- otherwise the discount never entered the formula")
	}
}

// The customer price and the cost are two independent chains derived from the
// same official rate. A procurement discount on a route may move the cost and
// must never move the charge.
func TestVersionedPricingSeparatesSalesAndProcurement(t *testing.T) {
	official := catalog.Price{InNanoPerMTok: 3_000_000_000}
	tokens := catalog.Tokens{In: 1_000_000}
	rates := catalog.Rates{
		ModelMultiplierBps:       12000, // published rate $3 x 1.2 = $3.60
		PlanMultiplierBps:        8000,  // customer rate $3.60 x 0.8 = $2.88
		ProcurementMultiplierBps: 8500,  // cost $3 x 0.85 = $2.55
		FXRate:                   "1",
	}

	q, err := catalog.Compute(catalog.Flat(official), catalog.Flat(official), tokens, rates)
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 2_880_000_000 {
		t.Fatalf("customer price = %d, want 2880000000", q.ChargedNano)
	}
	if q.UpstreamUSDNano != 2_550_000_000 {
		t.Fatalf("procurement cost = %d, want 2550000000", q.UpstreamUSDNano)
	}
	if q.ModelMultiplierBps != 12000 || q.PlanMultiplierBps != 8000 ||
		q.ProcurementMultiplierBps != 8500 {
		t.Fatalf("the three multiplier layers were not fully snapshotted: %+v", q)
	}

	// Switching to a cheaper provider moves the cost only; the customer price
	// must be identical.
	rates.ProcurementMultiplierBps = 7000
	cheaper, err := catalog.Compute(catalog.Flat(official), catalog.Flat(official), tokens, rates)
	if err != nil {
		t.Fatal(err)
	}
	if cheaper.ChargedNano != q.ChargedNano {
		t.Fatalf("the route changed the customer price: before=%d after=%d", q.ChargedNano, cheaper.ChargedNano)
	}
	if cheaper.UpstreamUSDNano != 2_100_000_000 {
		t.Fatalf("cheap provider cost = %d, want 2100000000", cheaper.UpstreamUSDNano)
	}
}

func TestVersionedPricingMultiplierValidation(t *testing.T) {
	p := catalog.Price{InNanoPerMTok: 1_000_000_000}
	for name, rates := range map[string]catalog.Rates{
		"model zero is normalized": {ModelMultiplierBps: 0, FXRate: "1"},
		"model negative":           {ModelMultiplierBps: -1, FXRate: "1"},
		"plan negative":            {PlanMultiplierBps: -1, FXRate: "1"},
		"procurement negative":     {ProcurementMultiplierBps: -1, FXRate: "1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1}, rates)
			if name == "model zero is normalized" {
				if err != nil {
					t.Fatalf("a zero value should normalize to the base price: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("an invalid multiplier should be refused")
			}
		})
	}
}

// Free is a billing mode, not a display label: the retained rates still charge
// the customer 0, while the cost stays visible.
func TestFreeModelZeroesRevenueButKeepsProcurementCost(t *testing.T) {
	// Both the free flag and the four rates come from the price row; there is
	// no second source.
	m := catalog.ModelPricingSnapshot{
		Priced:      true,
		BillingMode: "free",
		Upstream:    catalog.Price{InNanoPerMTok: 3_000_000_000},
	}
	quote, err := catalog.Compute(
		catalog.Flat(catalog.BillablePriceOf(m)),
		catalog.Flat(catalog.PriceOf(m)),
		catalog.Tokens{In: 1_000_000},
		catalog.Rates{FXRate: "1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if quote.ChargedNano != 0 {
		t.Fatalf("a free model still charged the customer: %d", quote.ChargedNano)
	}
	if quote.UpstreamUSDNano != 3_000_000_000 {
		t.Fatalf("a free model lost the platform's procurement cost: %d", quote.UpstreamUSDNano)
	}
}

// A zero multiplier means "no adjustment"; out-of-range values are refused.
//
// Zero has to be equivalent to 10000 rather than a literal multiplier of 0: one
// missed assignment read as x0 makes the entire bill free, and nothing about
// that raises an error.
func TestDiscountZeroMeansNoDiscount(t *testing.T) {
	p := catalog.Price{InNanoPerMTok: 3_000_000_000}
	tok := catalog.Tokens{In: 1000}

	zero, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), tok, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), tok, catalog.Rates{PlanMultiplierBps: 10000, FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if zero.ChargedNano != explicit.ChargedNano || zero.ChargedNano != 3_000_000 {
		t.Fatalf("a zero discount should behave the same as no discount: zero=%d explicit=%d", zero.ChargedNano, explicit.ChargedNano)
	}
	// The out-of-range samples changed with the definition: a discount-only
	// field caps at 10000, while a plan multiplier expresses both discount and
	// markup and its constraint runs to 100000, so 10001 and 20000 are now
	// legitimate values. Keeping them as failures would demand that the
	// current rule obey a constraint that no longer exists.
	for _, bad := range []int64{-1, 100001} {
		if _, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), tok, catalog.Rates{PlanMultiplierBps: bad, FXRate: "1"}); err == nil {
			t.Errorf("an out-of-range plan multiplier %d should be refused", bad)
		}
	}
	// The markup direction has to work.
	if _, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), tok,
		catalog.Rates{PlanMultiplierBps: 12000, FXRate: "1"}); err != nil {
		t.Errorf("a plan multiplier of 12000 (a 20%% markup) should be accepted: %v", err)
	}
}

// The billing rules: exact rationals, rounding up in the customer's direction,
// and the exchange rate as part of the same chain.
func TestComputeCharge(t *testing.T) {
	// $3 per million input, $15 per million output, as 3e9 and 15e9 nano.
	p := catalog.Price{InNanoPerMTok: 3_000_000_000, OutNanoPerMTok: 15_000_000_000}

	// 1000 input plus 500 output, no markup, in USD: 3e9*1000/1e6 +
	// 15e9*500/1e6 = 3e6 + 7.5e6.
	q, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1000, Out: 500}, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if q.UpstreamUSDNano != 10_500_000 || q.ChargedNano != 10_500_000 {
		t.Fatalf("with no markup the charge should equal the upstream cost: %+v", q)
	}

	// A 20% markup.
	q, err = catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1000, Out: 500}, catalog.Rates{ModelMultiplierBps: 12000, FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 12_600_000 {
		t.Fatalf("after a 20%% markup it should be 12600000: %d", q.ChargedNano)
	}
	if q.UpstreamUSDNano != 10_500_000 {
		t.Fatalf("the upstream cost must not include the markup: %d", q.UpstreamUSDNano)
	}

	// The exchange rate joins in at x7.15, exactly; floating point loses
	// precision here.
	q, err = catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1000, Out: 500}, catalog.Rates{ModelMultiplierBps: 12000, FXRate: "7.15"})
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 90_090_000 {
		t.Fatalf("with the FX rate applied the charge should be 90090000: %d", q.ChargedNano)
	}
	if q.FXRate != "7.15" {
		t.Fatalf("the FX rate must be snapshotted into the record: %q", q.FXRate)
	}
}

// The rounding direction is settled once: up, in the customer's direction, so
// usage is never billed as zero.
func TestChargeRoundsUp(t *testing.T) {
	// One nano per million and one token gives 1e-6 nano, below 1 before
	// rounding.
	p := catalog.Price{InNanoPerMTok: 1}
	q, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1}, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 1 {
		t.Fatalf("below 1 nano it still charges 1 (rounded up): %d", q.ChargedNano)
	}

	// Only genuinely zero usage is zero.
	q, err = catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{}, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 0 {
		t.Fatalf("zero usage should charge 0: %d", q.ChargedNano)
	}
}

// An unconfigured exchange rate must be an error rather than an implicit 1, or
// a bill in one currency is issued with amounts denominated in another.
func TestComputeRejectsMissingFX(t *testing.T) {
	p := catalog.Price{InNanoPerMTok: 3_000_000_000}
	for _, fx := range []string{"", "0", "-1", "abc"} {
		if _, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 100}, catalog.Rates{FXRate: fx}); err == nil {
			t.Fatalf("the FX rate %q should be refused", fx)
		}
	}
}

// Overflow: when an extreme token count times an extreme rate exceeds int64,
// that must be an error, never a wrap-around into a small or negative amount.
func TestComputeOverflowIsRejected(t *testing.T) {
	p := catalog.Price{InNanoPerMTok: math.MaxInt64}
	_, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: math.MaxInt32}, catalog.Rates{ModelMultiplierBps: 20000, FXRate: "7.15"})
	if err == nil {
		t.Fatal("an overflow should error rather than wrap around")
	}
}

// The pre-authorization estimate takes the smaller of the request's limit and
// the model's default cap on the output side; the hold is only a gate.
func TestEstimateOutputCap(t *testing.T) {
	p := catalog.Price{InNanoPerMTok: 1_000_000_000, OutNanoPerMTok: 1_000_000_000}

	// Unspecified in the request: use the default cap of 4096.
	got, err := catalog.Estimate(catalog.EstimateInput{
		Price: p, InputTokens: 1000, DefaultMaxCap: 4096, Rates: catalog.Rates{FXRate: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1000, Out: 4096}, catalog.Rates{FXRate: "1"})
	if got != want.ChargedNano {
		t.Fatalf("unspecified should use the default cap: %d want %d", got, want.ChargedNano)
	}

	// A smaller value in the request: use the request's.
	got, err = catalog.Estimate(catalog.EstimateInput{
		Price: p, InputTokens: 1000, MaxOutput: 100, DefaultMaxCap: 4096, Rates: catalog.Rates{FXRate: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want, _ = catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1000, Out: 100}, catalog.Rates{FXRate: "1"})
	if got != want.ChargedNano {
		t.Fatalf("the request's own limit should be used: %d want %d", got, want.ChargedNano)
	}

	// Above the default cap: capped, so a client cannot pry loose an
	// oversized hold with a huge max_tokens.
	got, err = catalog.Estimate(catalog.EstimateInput{
		Price: p, InputTokens: 1000, MaxOutput: 999999, DefaultMaxCap: 4096, Rates: catalog.Rates{FXRate: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want.ChargedNano && got == 0 {
		t.Fatal("the estimate must not be 0")
	}
	capped, _ := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{In: 1000, Out: 4096}, catalog.Rates{FXRate: "1"})
	if got != capped.ChargedNano {
		t.Fatalf("above the default cap it should be capped: %d want %d", got, capped.ChargedNano)
	}
}

// A model priced at zero still takes a minimum hold: a hold of 0 is rejected by
// the billing layer's positive-amount check.
func TestEstimateFreeModelKeepsMinimalHold(t *testing.T) {
	got, err := catalog.Estimate(catalog.EstimateInput{
		Price: catalog.Price{}, InputTokens: 1000, DefaultMaxCap: 100, Rates: catalog.Rates{FXRate: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("a free tier should hold 1 nano rather than 0: %d", got)
	}
}

func TestValidateTokens(t *testing.T) {
	if err := catalog.ValidateTokens(catalog.Tokens{In: 100, Out: 200}); err != nil {
		t.Fatalf("a normal value should not error: %v", err)
	}
	if err := catalog.ValidateTokens(catalog.Tokens{In: -1}); err == nil {
		t.Fatal("a negative value should be refused")
	}
	if err := catalog.ValidateTokens(catalog.Tokens{Out: math.MaxInt64}); err == nil {
		t.Fatal("a value outside the plausible range should be refused")
	}
	for name, tokens := range map[string]catalog.Tokens{
		"negative audio":   {In: 10, AudioIn: -1},
		"audio over input": {In: 10, AudioIn: 11},
		"ttl over cache":   {CacheWrite: 10, CacheWrite5m: 6, CacheWrite1h: 5},
		"negative tool":    {ToolCalls: map[string]int64{"web_search": -1}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := catalog.ValidateTokens(tokens); err == nil {
				t.Fatal("invalid advanced usage should be refused")
			}
		})
	}
}

func TestComputeEntrypointsRejectNegativeAdvancedUsage(t *testing.T) {
	price := catalog.Flat(catalog.Price{InNanoPerMTok: 1_000_000_000})
	tokens := catalog.Tokens{In: 10, AudioIn: -1}
	rates := catalog.Rates{FXRate: "1"}
	checks := []func() error{
		func() error {
			_, err := catalog.Compute(price, price, tokens, rates)
			return err
		},
		func() error {
			_, err := catalog.ComputeBYOK(price, tokens, 1000, rates)
			return err
		},
		func() error {
			_, err := catalog.ComputeExactContributions(price, price, tokens, rates)
			return err
		},
		func() error {
			_, err := catalog.ComputeBYOKExactContributions(price, tokens, 1000, rates)
			return err
		},
	}
	for i, check := range checks {
		if err := check(); err == nil {
			t.Fatalf("billing entry point %d accepted negative advanced usage", i)
		}
	}
}

// The list price decides the bill; the cost decides the margin.
//
// This is what makes "margin broken down by provider" mean anything. Sharing
// one number between the two chains makes cost identically equal to the charge
// divided by the markup, so switching to a cheaper provider is invisible in the
// report -- and that switch is the entire economic reason for routing across
// several.
func TestComputeSplitsListPriceFromProcurementCost(t *testing.T) {
	list := catalog.Price{InNanoPerMTok: 3_000_000_000, OutNanoPerMTok: 15_000_000_000}
	tok := catalog.Tokens{In: 1_000_000, Out: 1_000_000}
	rates := catalog.Rates{ModelMultiplierBps: 12000, FXRate: "1"}

	// Two routes for the same model: one expensive, one cheap.
	pricey := catalog.Price{InNanoPerMTok: 3_000_000_000, OutNanoPerMTok: 15_000_000_000}
	cheap := catalog.Price{InNanoPerMTok: 1_000_000_000, OutNanoPerMTok: 5_000_000_000}

	qa, err := catalog.Compute(catalog.Flat(list), catalog.Flat(pricey), tok, rates)
	if err != nil {
		t.Fatal(err)
	}
	qb, err := catalog.Compute(catalog.Flat(list), catalog.Flat(cheap), tok, rates)
	if err != nil {
		t.Fatal(err)
	}

	// The bill follows the list price alone: which provider served the request
	// is an internal decision and must not show up on the organization's bill.
	if qa.ChargedNano != qb.ChargedNano {
		t.Errorf("switching providers must not change the organization's bill: %d vs %d", qa.ChargedNano, qb.ChargedNano)
	}
	// Expected values computed independently of the code under test:
	// (3 + 15) USD × 1.2 = 21.6 USD = 21_600_000_000 nano
	if qa.ChargedNano != 21_600_000_000 {
		t.Errorf("charged = %d, computed independently as 21600000000", qa.ChargedNano)
	}

	// The cost follows whichever provider actually served it.
	if qa.UpstreamUSDNano != 18_000_000_000 {
		t.Errorf("expensive provider cost = %d, want 18000000000 (3 + 15)", qa.UpstreamUSDNano)
	}
	if qb.UpstreamUSDNano != 6_000_000_000 {
		t.Errorf("cheap provider cost = %d, want 6000000000 (1 + 5)", qb.UpstreamUSDNano)
	}
	// Which is what makes the margin vary by provider.
	if qa.ChargedNano-qa.UpstreamUSDNano >= qb.ChargedNano-qb.UpstreamUSDNano {
		t.Error("the cheap provider should have the higher margin, otherwise the provider dimension is still an identity")
	}
}

// When an upstream does not honour the requested output limit, the hold must
// not treat it as a cost ceiling.
//
// One relay was measured returning 94 tokens for a requested 16. Holding for 16
// there means the budget guard does not work. Settlement still bills correctly,
// but the point of a hold is to reserve what might be spent.
func TestEstimateIgnoresUntrustedRequestCap(t *testing.T) {
	base := catalog.EstimateInput{
		Price:         catalog.Price{OutNanoPerMTok: 1_000_000_000},
		InputTokens:   0,
		MaxOutput:     16,
		DefaultMaxCap: 4096,
		Rates:         catalog.Rates{FXRate: "1"},
	}

	// The zero value trusts the requested cap, which is the existing
	// behaviour. This assertion also pins "adding the field changes nothing
	// for callers that do not set it".
	trusted, err := catalog.Estimate(base)
	if err != nil {
		t.Fatal(err)
	}
	if trusted != 16_000 { // 16 tok × 1e9 nano/Mtok ÷ 1e6 tok/Mtok
		t.Errorf("when the request cap is trusted the estimate should be 16: %d", trusted)
	}

	// Set: fall back to the cap.
	base.IgnoreRequestCap = true
	untrusted, err := catalog.Estimate(base)
	if err != nil {
		t.Fatal(err)
	}
	if untrusted != 4_096_000 {
		t.Errorf("when the request cap is not trusted it should fall back to the 4096 cap: %d", untrusted)
	}
	if untrusted <= trusted {
		t.Error("the hold must be more conservative when the cap is not trusted, otherwise the gate is decorative")
	}
}

// HoldCap takes the most conservative candidate, because the hold is computed
// before a route is chosen.
func TestHoldCapTakesMostConservativeCandidate(t *testing.T) {
	m := gwdb.Model{MaxOutputTokens: 4096}

	// No candidate override: use the model's value and trust the requested
	// cap.
	if cap, ignore := catalog.HoldCap(m, nil); cap != 4096 || ignore {
		t.Errorf("with no candidates it should be (4096, false), got (%d, %v)", cap, ignore)
	}

	// The limit is the maximum across candidates: which one will serve is
	// unknown, and holding too little defeats the guard.
	routes := []catalog.Route{{MaxOutputTokens: 8192}, {MaxOutputTokens: 2048}}
	if cap, _ := catalog.HoldCap(m, routes); cap != 8192 {
		t.Errorf("the limit should be the candidates' maximum of 8192, got %d", cap)
	}

	// If any single candidate ignores the requested cap, none of them are
	// trusted with it.
	routes = []catalog.Route{{MaxOutputTokens: 2048}, {IgnoresMaxOutputTokens: true}}
	if _, ignore := catalog.HoldCap(m, routes); !ignore {
		t.Error("if one candidate does not honour the request cap then none may be trusted -- the request could route to exactly that one")
	}
}
