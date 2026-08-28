package catalog

import (
	"cmp"
	"errors"
	"fmt"
	"math/big"
	"slices"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// A per-unit price is what a model costs when it is not billed per token.
//
// # Why this is a separate family and not another bucket
//
// model_price_dimension_rates already carries every token bucket, and adding
// `video_second` to it looks like the smaller change. It is not, for two
// reasons that are both about being wrong rather than being ugly:
//
//   - that table's rate column is nano *per million tokens*, and every reader
//     divides by a million. A per-second rate stored there has to be
//     pre-multiplied by 1e6, and the first person to read the row without
//     knowing that convention is off by a factor of a million. A unit belongs
//     in the schema, not in a convention somebody has to have been told.
//   - a token bucket that has no rate falls back to one of the model's four
//     base rates, which is exactly right for a dimension bucket (an image input
//     token really is an input token). A video second has no such parent. Left
//     to fall back it would bill a generated clip at the model's text input
//     rate. This family therefore fails closed: a unit with no rate is
//     ErrUnitPriceMissing, never zero and never somebody else's number.
//
// The two families share everything downstream of "how many of what, at which
// rate" -- the multiplier chain, the single rounding point, the overflow check
// -- through quoteFrom in pricing.go.

// Unit is what is being counted.
type Unit string

const (
	// UnitSecond is one second of produced output.
	UnitSecond Unit = "second"
	// UnitCall is one generation, for upstreams that sell generation packs
	// rather than time.
	UnitCall Unit = "call"
	// UnitImage is one produced image.
	//
	// Kept apart from UnitCall because the two count different things: a
	// request for four images is four units here and one there. Collapsing them
	// would quarter or quadruple a bill depending on which way the collapse
	// went, and neither error announces itself.
	UnitImage Unit = "image"
)

// KnownUnits is every unit that may be priced, matching the CHECK constraint on
// model_price_unit_rates.
func KnownUnits() []Unit { return []Unit{UnitSecond, UnitCall, UnitImage} }

// UnitKey addresses one rate.
//
// Resolution, Audio and Variant are the axes a rate varies on. An empty field
// means "this rate does not vary on that axis" and matches any request. That is
// the opposite direction from the token table, which walks *down* to a base
// rate: here a model with one flat per-second price is a single row with every
// axis empty, and lookup widens toward it.
//
// Which axes carry meaning depends on the unit: a video rate varies on
// Resolution and Audio, an image rate on Resolution (the requested size) and
// Variant (the quality tier the upstream sells), leaving Audio empty.
type UnitKey struct {
	Unit        Unit
	Resolution  string
	Audio       string // "on", "off", or "" for a rate that does not vary on audio
	Variant     string
	ServiceTier string // "" normalises to standard
}

func (k UnitKey) normalize() UnitKey {
	if k.ServiceTier == "" {
		k.ServiceTier = TierStandard
	}
	return k
}

// Units is one job's billable quantity vector: the exact input the charge is a
// function of.
//
// It is stored on the job row so the charge can be recomputed later without
// re-running the vendor mapper -- the same reason usage_logs carries a price
// snapshot rather than a reference to the price table.
type Units struct {
	Quantities map[UnitKey]int64
}

// ValidateUnits refuses a quantity vector that cannot be billed.
//
// A negative quantity would shrink a bill; an unknown unit would be priced by
// nothing at all. Both are refusals rather than clamps: unlike an upstream's
// self-contradictory token report, these numbers are derived from a request
// this gateway already admitted, so a bad one is a defect here, not upstream.
func ValidateUnits(u Units) error {
	for k, q := range u.Quantities {
		if q < 0 {
			return fmt.Errorf("catalog: unit %q has a negative quantity %d", k.Unit, q)
		}
		if !slices.Contains(KnownUnits(), k.Unit) {
			return fmt.Errorf("catalog: %q is not a billable unit", k.Unit)
		}
	}
	return nil
}

// ErrUnitPriceMissing means a job's quantity vector names a unit the pinned
// price table has no rate for.
//
// Deliberately an error and not a zero. The token side has ErrAdvancedPriceMissing
// for the same situation and for the same reason: a rate nobody filled in is
// unknown, and billing an unknown as free is a decision nobody made.
var ErrUnitPriceMissing = errors.New("catalog: a billable unit has no price")

// UnitPriceTable addresses a per-unit rate.
type UnitPriceTable struct {
	rates map[UnitKey]int64
	// billingFree marks the customer side as free without losing which rates
	// exist, so the cost side keeps using the unswitched table. Same shape as
	// PriceTable.billingFree, and for the same reason: "free" must never zero
	// out the real cost, or margin reporting silently loses those requests.
	billingFree bool
}

// NewUnitPriceTable builds a table from a model's per-unit rate rows.
func NewUnitPriceTable(rows []gwdb.ModelPriceUnitRate) UnitPriceTable {
	if len(rows) == 0 {
		return UnitPriceTable{}
	}
	t := UnitPriceTable{rates: make(map[UnitKey]int64, len(rows))}
	for _, r := range rows {
		k := UnitKey{
			Unit:        Unit(r.Unit),
			Resolution:  r.Resolution,
			Audio:       r.Audio,
			Variant:     r.Variant,
			ServiceTier: r.ServiceTier,
		}.normalize()
		t.rates[k] = r.NanoPerUnit
	}
	return t
}

// ForBilling returns the table to charge the customer with.
func (t UnitPriceTable) ForBilling(free bool) UnitPriceTable {
	t.billingFree = free
	return t
}

// Empty reports whether the model has any per-unit rate at all. A `units`
// model with an empty table is unpriced, which admission refuses.
func (t UnitPriceTable) Empty() bool { return len(t.rates) == 0 }

// ErrUnitAmbiguous means a model prices rows in more than one unit, so there is
// no answer to "what is this model billed by".
var ErrUnitAmbiguous = errors.New("catalog: a model must be priced in exactly one unit")

// BillingUnit reports which unit this model is billed in.
//
// It is read from the rate card rather than chosen by the caller. Fixing the
// unit at the call site is how a model priced per generation ends up looked up
// per second, missing every row, and answering "unpriced" on every request
// while its rates sit right there in the table.
//
// Exactly one unit per model: a model billed both per second and per
// generation has no single answer, and guessing one would charge some requests
// twice over. That is a configuration error, and it fails closed.
func (t UnitPriceTable) BillingUnit() (Unit, error) {
	var found Unit
	for k := range t.rates {
		if found == "" {
			found = k.Unit
			continue
		}
		if k.Unit != found {
			return "", ErrUnitAmbiguous
		}
	}
	if found == "" {
		return "", ErrUnitPriceMissing
	}
	return found, nil
}

// Lookup resolves a rate, widening from the most specific key toward the least.
//
// The order drops Variant, then Audio, then Resolution: a rate that does not
// name an axis applies to every value of it. Nothing widens across Unit or
// ServiceTier -- a per-second rate is not a per-call rate, and a batch rate is
// not a standard one, so those are lookups that must miss rather than borrow.
func (t UnitPriceTable) Lookup(k UnitKey) (int64, bool) {
	if t.billingFree {
		return 0, true
	}
	k = k.normalize()
	for _, candidate := range []UnitKey{
		k,
		{Unit: k.Unit, Resolution: k.Resolution, Audio: k.Audio, ServiceTier: k.ServiceTier},
		{Unit: k.Unit, Resolution: k.Resolution, ServiceTier: k.ServiceTier},
		{Unit: k.Unit, ServiceTier: k.ServiceTier},
	} {
		if rate, ok := t.rates[candidate]; ok {
			return rate, true
		}
	}
	return 0, false
}

// UnitRateSnapshot is one rate as recorded on a job and its usage row.
type UnitRateSnapshot struct {
	Unit        string `json:"unit"`
	Resolution  string `json:"resolution,omitempty"`
	Audio       string `json:"audio,omitempty"`
	Variant     string `json:"variant,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`
	NanoPerUnit int64  `json:"nano_per_unit"`
}

// Snapshot renders the whole rate card in a stable order, so a settled row
// stays recomputable without joining against configuration -- the same reason
// PriceTable carries one.
func (t UnitPriceTable) Snapshot() []UnitRateSnapshot {
	out := make([]UnitRateSnapshot, 0, len(t.rates))
	for k, rate := range t.rates {
		out = append(out, UnitRateSnapshot{
			Unit: string(k.Unit), Resolution: k.Resolution, Audio: k.Audio,
			Variant: k.Variant, ServiceTier: k.ServiceTier, NanoPerUnit: rate,
		})
	}
	slices.SortFunc(out, func(a, b UnitRateSnapshot) int {
		return cmp.Or(
			cmp.Compare(a.Unit, b.Unit),
			cmp.Compare(a.Resolution, b.Resolution),
			cmp.Compare(a.Audio, b.Audio),
			cmp.Compare(a.ServiceTier, b.ServiceTier),
			cmp.Compare(a.Variant, b.Variant),
		)
	})
	return out
}

// unitCost sums a quantity vector against a table, in USD nano as an exact
// rational. A unit with no rate is an error, never a zero contribution.
func unitCost(t UnitPriceTable, u Units) (*big.Rat, error) {
	total := new(big.Rat)
	// Iterating the map is fine: addition is commutative and every term is
	// exact, so the sum does not depend on the order. That is only true because
	// these are big.Rat and not floats.
	for k, quantity := range u.Quantities {
		if quantity == 0 {
			continue
		}
		rate, ok := t.Lookup(k)
		if !ok {
			return nil, fmt.Errorf("%w: unit=%s resolution=%q audio=%q tier=%q",
				ErrUnitPriceMissing, k.Unit, k.Resolution, k.Audio, k.normalize().ServiceTier)
		}
		total.Add(total, new(big.Rat).SetFrac(
			new(big.Int).Mul(big.NewInt(quantity), big.NewInt(rate)),
			big.NewInt(1),
		))
	}
	return total, nil
}

// ComputeUnits bills one job priced per unit.
//
// The signature mirrors Compute deliberately, including the two tables: `list`
// is the model's published rate and decides the bill, `cost` is the serving
// provider's rate times the procurement multiplier and decides the margin.
// Routing a job to a cheaper upstream must not move the customer's bill on this
// plane either.
//
// Unlike Compute, this is not an estimate at any point. A job's quantity vector
// is derived from parameters the caller wrote and this gateway admitted, so the
// amount computed here before the upstream is called is the amount that will be
// settled (ADR-0220).
func ComputeUnits(list, cost UnitPriceTable, u Units, r Rates) (Quote, error) {
	if err := ValidateUnits(u); err != nil {
		return Quote{}, err
	}
	r, err := r.normalize()
	if err != nil {
		return Quote{}, err
	}
	fx, ok := new(big.Rat).SetString(r.FXRate)
	if !ok || fx.Sign() <= 0 {
		return Quote{}, fmt.Errorf("catalog: FX rate %q is invalid", r.FXRate)
	}

	upstream, err := unitCost(cost, u)
	if err != nil {
		return Quote{}, err
	}
	upstream.Mul(upstream, new(big.Rat).SetFrac(
		big.NewInt(r.ProcurementMultiplierBps), big.NewInt(noDiscountBps)))

	charged, err := unitCost(list, u)
	if err != nil {
		return Quote{}, err
	}
	return quoteFrom(upstream, charged, fx, r)
}

// ComputeUnitsBYOK bills a unit-priced job that ran on the organization's own
// upstream credential.
//
// The same two differences from ComputeUnits that ComputeBYOK has from Compute:
// the formula is upstream cost times the service fee rather than cost times a
// markup, and UpstreamUSDNano is zero because those units were consumed on the
// organization's account, not ours. Recording the real figure there would turn
// margin -- charges minus costs -- into "service fee minus the whole upstream
// bill", deeply negative, for every BYOK job.
//
// It exists because the unit plane otherwise had no BYOK path at all, and a
// missing path does not fail loudly: it charges the organization the full list
// price for a clip billed to their own account.
func ComputeUnitsBYOK(list UnitPriceTable, u Units, feeBps int64, r Rates) (Quote, error) {
	if err := ValidateUnits(u); err != nil {
		return Quote{}, err
	}
	if feeBps < 0 {
		return Quote{}, fmt.Errorf("catalog: BYOK service fee must not be negative")
	}
	// No model multiplier on this path: the service fee is itself the rate.
	r.ModelMultiplierBps = noDiscountBps
	r, err := r.normalize()
	if err != nil {
		return Quote{}, err
	}
	fx, ok := new(big.Rat).SetString(r.FXRate)
	if !ok || fx.Sign() <= 0 {
		return Quote{}, fmt.Errorf("catalog: FX rate %q is invalid", r.FXRate)
	}
	charged, err := unitCost(list, u)
	if err != nil {
		return Quote{}, err
	}
	charged.Mul(charged, new(big.Rat).SetFrac(big.NewInt(feeBps), big.NewInt(10000)))
	charged.Mul(charged, new(big.Rat).SetFrac(
		big.NewInt(r.PlanMultiplierBps), big.NewInt(noDiscountBps)))
	charged.Mul(charged, fx)
	chargedNano, err := ceilToInt64(charged)
	if err != nil {
		return Quote{}, fmt.Errorf("catalog: BYOK service fee overflow: %w", err)
	}
	return Quote{
		UpstreamUSDNano:          0,
		ChargedNano:              chargedNano,
		ModelMultiplierBps:       noDiscountBps,
		PlanMultiplierBps:        r.PlanMultiplierBps,
		ProcurementMultiplierBps: noDiscountBps,
		FXRate:                   r.FXRate,
	}, nil
}
