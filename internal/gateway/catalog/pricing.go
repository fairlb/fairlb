package catalog

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Pricing and billing.
//
// Three rules, settled once:
//
//   - Integers and rationals throughout, never floating point. A float64
//     mantissa cannot hold nano-scale amounts, and the error from chaining an
//     exchange rate with a markup lands directly on somebody's bill.
//   - Always round up in the customer's direction. The rounding difference is
//     absorbed here, and nothing is ever charged as zero because it rounded
//     down.
//   - Intermediate arithmetic goes through exact rationals, converting to
//     int64 once at the end with an overflow check: the intermediate product
//     of tokens times nano-per-million approaches the int64 limit on extreme
//     input.

// Price is a model's rate, in nano USD per million tokens.
type Price struct {
	InNanoPerMTok         int64 `json:"input"`
	OutNanoPerMTok        int64 `json:"output"`
	CacheReadNanoPerMTok  int64 `json:"cache_read"`
	CacheWriteNanoPerMTok int64 `json:"cache_write"`
}

// Tokens is one request's usage, with the four buckets mutually disjoint.
//
// Normalizing them is the job of each upstream protocol's response parser, not of
// this layer: OpenAI's prompt_tokens includes cached_tokens while Anthropic's
// input_tokens excludes both cache figures, so putting the raw fields straight
// into this struct double-charges one protocol and undercharges the other. By the
// time values arrive here they must already be disjoint.
type Tokens struct {
	In         int64 // non-cached input
	Out        int64
	CachedRead int64 // input served from cache
	CacheWrite int64 // input written to cache, priced separately upstream

	// Below: the pricing dimensions. All-zero behaves exactly as if they did
	// not exist.

	// AudioIn and AudioOut are audio tokens. In OpenAI's usage report they are
	// a subset of prompt/completion tokens, so they are subtracted out of
	// In/Out and reported separately -- without that they are billed at the
	// text rate, and with it a separate audio rate can be configured.
	AudioIn  int64
	AudioOut int64
	// ImageIn is image input tokens, a subset of In for the same reason and
	// handled the same way: subtracted out of In so it can carry its own rate,
	// and billed at the text input rate when no image rate is configured.
	ImageIn int64
	// ImageOut is image *output* tokens: what a model that generates images
	// reports for the pixels it produced. A subset of Out, subtracted out of it
	// the same way. It exists because both upstreams that generate images price
	// this well above text output -- OpenAI lists its own image output token
	// rate, Google charges a generated image as a fixed block of tokens -- so
	// folding it into Out undercharges every image generated.
	ImageOut int64
	// CacheWrite5m and CacheWrite1h are Anthropic's two cache-write TTL tiers,
	// priced differently. Their sum is at most CacheWrite; when the upstream
	// does not break it down both are 0 and the whole amount is billed at the
	// CacheWrite rate.
	CacheWrite5m int64
	CacheWrite1h int64
	// ServiceTier is the tier the upstream actually reported (standard,
	// priority or batch -- both protocols have them, and the price can differ
	// by a factor of two). Empty means it was not reported and standard is
	// assumed.
	ServiceTier string
	// ToolCalls is usage counted in requests rather than tokens. It is a
	// different unit, so its rates live in their own table.
	ToolCalls map[string]int64
}

// ContextTokens is the whole prompt: non-cached input plus both cache buckets.
// It is the quantity the context-length price bands are selected against, and
// it is deliberately the sum of exactly those three.
//
//   - The three are mutually disjoint by construction (see the type comment),
//     so their sum is the prompt size the upstream itself bands on.
//   - AudioIn and ImageIn are not added: each is a subset of In, and adding
//     either would count those tokens twice, pushing a request over a
//     threshold it never crossed.
//   - Reasoning tokens play no part: they are on the output side, and the
//     question here is how long the prompt was.
func (t Tokens) ContextTokens() int64 { return t.In + t.CachedRead + t.CacheWrite }

// tier normalizes: empty or unrecognized means standard. An unfamiliar tier
// must never refuse to bill -- that turns a request that was already served
// into a 500, with the money spent and nothing recorded.
func (t Tokens) tier() string {
	switch t.ServiceTier {
	case TierPriority, TierBatch:
		return t.ServiceTier
	default:
		return TierStandard
	}
}

// Quote is the outcome of billing one request; every field is written straight
// to the usage log.
type Quote struct {
	// UpstreamUSDNano is the cost in USD nano, excluding any markup. It is
	// what the margin is measured against.
	UpstreamUSDNano int64
	// ChargedNano is what the organization is charged, in their wallet currency,
	// with the multipliers and the exchange rate already applied.
	ChargedNano int64
	// The three multipliers are the price snapshot for this request. 10000 is
	// 1x. The model and plan multipliers can express both discount and markup;
	// the procurement multiplier affects cost only, never the customer price.
	ModelMultiplierBps       int64
	PlanMultiplierBps        int64
	ProcurementMultiplierBps int64
	FXRate                   string // the exchange rate snapshot for this request
}

// Rates holds the three factors applied to this request.
//
// It is a struct rather than three positional parameters because all three are
// int64 with different meanings, and getting the order wrong produces no
// compile error at all. This path multiplies straight into a bill, so that
// mistake should not be possible to make.
type Rates struct {
	// ModelMultiplierBps takes the upstream's official rate to the published
	// rate; PlanMultiplierBps takes the published rate to this customer's
	// final rate; ProcurementMultiplierBps takes the official rate to the cost
	// of the route serving this request. A zero in any of them normalizes to
	// 1x, so one missed assignment cannot multiply an amount to zero.
	ModelMultiplierBps       int64
	PlanMultiplierBps        int64
	ProcurementMultiplierBps int64
	FXRate                   string
}

// ErrAdvancedPriceMissing means the upstream reported real usage on an advanced
// dimension for which the pinned price has no rate. That is not a charge of
// zero: the caller must keep the hold and route the request to the unsettled
// queue, to be handled once the price is filled in.
var ErrAdvancedPriceMissing = errors.New("catalog: an advanced billing dimension has no price")

// noDiscountBps is the basis-point value for "no adjustment", i.e. 1x.
const noDiscountBps = 10000

// maxMultiplierBps is 10x. All three multipliers share this upper bound, and it
// matches the CHECK constraint in the database.
const maxMultiplierBps = 100000

// normalize fills in zero values and validates. A zero in any multiplier
// normalizes to 1x.
func (r Rates) normalize() (Rates, error) {
	if r.ModelMultiplierBps == 0 {
		r.ModelMultiplierBps = noDiscountBps
	}
	if r.PlanMultiplierBps == 0 {
		r.PlanMultiplierBps = noDiscountBps
	}
	if r.ProcurementMultiplierBps == 0 {
		r.ProcurementMultiplierBps = noDiscountBps
	}
	if r.ModelMultiplierBps < 1 || r.ModelMultiplierBps > maxMultiplierBps {
		return r, fmt.Errorf("catalog: model multiplier %d bps is outside 1..%d", r.ModelMultiplierBps, maxMultiplierBps)
	}
	if r.PlanMultiplierBps < 1 || r.PlanMultiplierBps > maxMultiplierBps {
		return r, fmt.Errorf("catalog: plan multiplier %d bps is outside 1..%d", r.PlanMultiplierBps, maxMultiplierBps)
	}
	if r.ProcurementMultiplierBps < 1 || r.ProcurementMultiplierBps > maxMultiplierBps {
		return r, fmt.Errorf("catalog: procurement multiplier %d bps is outside 1..%d", r.ProcurementMultiplierBps, maxMultiplierBps)
	}
	if r.FXRate == "" {
		return r, fmt.Errorf("catalog: FX rate is not configured, refusing to bill")
	}
	return r, nil
}

// tokensPerMTok is the denominator of a rate: rates are quoted per million
// tokens.
const tokensPerMTok = 1_000_000

const (
	exactInput = iota
	exactOutput
	exactCacheRead
	exactCacheWrite
	exactTools
	exactBucketCount
)

type exactBuckets [exactBucketCount]*big.Rat

func newExactBuckets() exactBuckets {
	var out exactBuckets
	for i := range out {
		out[i] = new(big.Rat)
	}
	return out
}

func (b exactBuckets) total() *big.Rat {
	out := new(big.Rat)
	for _, amount := range b {
		out.Add(out, amount)
	}
	return out
}

// upstreamCostBuckets sums the contribution buckets, in USD nano, as exact
// rationals with no rounding.
//
// It goes through a PriceTable rather than four scalars because the same bucket
// can carry a different rate per service tier, per cache TTL and per context
// length. With no overrides configured, the lookup falls back to the four base
// rates and the result is bit-for-bit identical.
//
// The context length is read once and applies to all four buckets, output
// included. Several upstreams raise the output price too once the prompt passes
// their threshold, so wiring the length axis to the input buckets alone would
// undercharge the output of exactly the requests this axis exists for.
func upstreamCostBuckets(pt PriceTable, t Tokens) (exactBuckets, error) {
	tier := t.tier()
	contextTokens := t.ContextTokens()
	parts := newExactBuckets()
	add := func(tokens int64, bucket, variant string, contributionBucket int) {
		if tokens == 0 {
			return
		}
		rate := pt.lookup(bucket, tier, variant, contextTokens)
		amount := new(big.Rat).SetFrac(
			new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate)),
			big.NewInt(tokensPerMTok),
		)
		parts[contributionBucket].Add(parts[contributionBucket], amount)
	}

	// The dimension rate differences still land in the input and output
	// contributions; there is no invented fifth token bucket.
	//
	// Audio and image input are each a subset of In and disjoint from each
	// other -- one upstream report never counts the same token as both -- so
	// both are subtracted before the remainder is billed at the text rate. Audio
	// and image *output* are the same arrangement on the other side.
	add(nonNeg(t.In-t.AudioIn-t.ImageIn), BucketIn, VariantNone, exactInput)
	add(nonNeg(t.Out-t.AudioOut-t.ImageOut), BucketOut, VariantNone, exactOutput)
	add(t.AudioIn, BucketAudioIn, VariantNone, exactInput)
	add(t.ImageIn, BucketImageIn, VariantNone, exactInput)
	add(t.AudioOut, BucketAudioOut, VariantNone, exactOutput)
	add(t.ImageOut, BucketImageOut, VariantNone, exactOutput)
	add(t.CachedRead, BucketCacheRead, VariantNone, exactCacheRead)

	// The per-TTL rate difference still lands in the cache-write
	// contribution.
	if t.CacheWrite5m > 0 || t.CacheWrite1h > 0 {
		add(t.CacheWrite5m, BucketCacheWrite, VariantCache5m, exactCacheWrite)
		add(t.CacheWrite1h, BucketCacheWrite, VariantCache1h, exactCacheWrite)
		add(nonNeg(t.CacheWrite-t.CacheWrite5m-t.CacheWrite1h),
			BucketCacheWrite, VariantNone, exactCacheWrite)
	} else {
		add(t.CacheWrite, BucketCacheWrite, VariantNone, exactCacheWrite)
	}

	// Tools are a different unit from tokens and always stay their own line in
	// the exact contributions.
	for tool, calls := range t.ToolCalls {
		if calls <= 0 {
			continue
		}
		per, ok := pt.lookupToolPrice(tool)
		if !ok {
			return exactBuckets{}, fmt.Errorf("%w: tool %q", ErrAdvancedPriceMissing, tool)
		}
		parts[exactTools].Add(parts[exactTools], new(big.Rat).SetInt(
			new(big.Int).Mul(big.NewInt(calls), big.NewInt(per))))
	}
	return parts, nil
}

func upstreamCost(pt PriceTable, t Tokens) (*big.Rat, error) {
	parts, err := upstreamCostBuckets(pt, t)
	if err != nil {
		return nil, err
	}
	return parts.total(), nil
}

// procurementCost is the summed official rates times the procurement
// multiplier.
func procurementCost(pt PriceTable, t Tokens, multiplierBps int64) (*big.Rat, error) {
	total, err := upstreamCost(pt, t)
	if err != nil {
		return nil, err
	}
	return total.Mul(total, new(big.Rat).SetFrac(
		big.NewInt(multiplierBps), big.NewInt(noDiscountBps))), nil
}

// nonNeg clamps to 0. Subtracting a subset can go negative when the upstream
// contradicts itself, and a negative token count would shrink the cost and
// inflate the margin out of nothing. Subtracting too little is preferable to
// producing a negative quantity.
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// Compute bills one request. The list price decides the bill; the cost decides
// the margin.
//
// The two PriceTable parameters are not redundant:
//
//   - list is the model's published rate, from a single source, and has nothing
//     to do with routing. The charge derives from it rather than from cost,
//     because otherwise routing the same model to a cheaper provider would move
//     the organization's bill -- and which provider serves a request is an internal
//     decision that should not show up there.
//   - cost is the rate table of whichever provider actually served this
//     request: the official rate times the procurement multiplier. Margin is
//     charge minus cost and therefore varies by provider, which is the entire
//     economic reason for routing across several. Sharing one number between
//     the two chains makes "margin broken down by provider" an identity.
//
// The model multiplier and the plan multiplier are two layers that multiply,
// not two that override each other: one is "how much margin this model carries"
// and the other is "what discount this customer negotiated" -- orthogonal
// facts. Collapsing them into a single priority chain means configuring a
// markup override on one model silently erases every customer's discount on
// that model.
//
// An empty exchange rate means none is configured, which is an error rather
// than an implicit 1: treating it as 1 would issue a bill in one currency using
// amounts denominated in another.
func Compute(list, cost PriceTable, t Tokens, r Rates) (Quote, error) {
	if err := ValidateTokens(t); err != nil {
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

	upstream, err := procurementCost(cost, t, r.ProcurementMultiplierBps)
	if err != nil {
		return Quote{}, err
	}
	// The charge derives from the list price, computed independently of the
	// cost above.
	charged, err := upstreamCost(list, t)
	if err != nil {
		return Quote{}, err
	}
	return quoteFrom(upstream, charged, fx, r)
}

// quoteFrom applies the customer-side multipliers and the exchange rate to an
// already-summed pair of amounts, rounds each up exactly once, and assembles
// the snapshot.
//
// It is shared by the token path above and the per-unit path in
// unit_price.go. Only "how many of what, at which rate" differs between the
// two families; the multiplier chain, the single rounding point and the
// overflow check must not, because the day they diverge is the day the same
// discount means two things depending on the modality.
func quoteFrom(upstream, charged *big.Rat, fx *big.Rat, r Rates) (Quote, error) {
	upstreamNano, err := ceilToInt64(upstream)
	if err != nil {
		return Quote{}, fmt.Errorf("catalog: upstream cost overflow: %w", err)
	}
	charged = new(big.Rat).Set(charged)
	charged.Mul(charged, new(big.Rat).SetFrac(
		big.NewInt(r.ModelMultiplierBps), big.NewInt(noDiscountBps)))
	charged.Mul(charged, new(big.Rat).SetFrac(
		big.NewInt(r.PlanMultiplierBps), big.NewInt(noDiscountBps)))
	charged.Mul(charged, fx)
	chargedNano, err := ceilToInt64(charged)
	if err != nil {
		return Quote{}, fmt.Errorf("catalog: billed amount overflow: %w", err)
	}
	return Quote{
		UpstreamUSDNano:          upstreamNano,
		ChargedNano:              chargedNano,
		ModelMultiplierBps:       r.ModelMultiplierBps,
		PlanMultiplierBps:        r.PlanMultiplierBps,
		ProcurementMultiplierBps: r.ProcurementMultiplierBps,
		FXRate:                   r.FXRate,
	}, nil
}

// ComputeBYOK bills a request that used the organization's own upstream credential.
//
// Two fundamental differences from Compute:
//
//   - The formula is upstream cost times the service fee rate times the
//     exchange rate, not cost times a markup. The organization's token consumption is
//     billed directly to their own upstream account; only a service fee is
//     charged here.
//   - UpstreamUSDNano is always 0. That upstream cost is what the organization paid
//     upstream, not a cost incurred here. Recording it truthfully would turn
//     margin -- the sum of charges minus the sum of costs -- into "service fee
//     minus the full upstream bill", deeply negative, contaminating the whole
//     margin report. The field is not visible to the organization either, so
//     recording 0 loses nothing.
//
// The upstream cost is still computed from the published rates, because it is
// the base the service fee is charged on. What the organization actually pays
// upstream may differ from those rates, since they have their own discount, but
// the base of a fee has to be a number both sides can recompute.
//
//   - The plan multiplier applies to the service fee as well: a discounted
//     customer should have a discounted fee. Applying it only on the
//     platform-credential path means the same customer quietly loses their
//     discount by switching to their own credential.
func ComputeBYOK(p PriceTable, t Tokens, feeBps int64, r Rates) (Quote, error) {
	if err := ValidateTokens(t); err != nil {
		return Quote{}, err
	}
	if feeBps < 0 {
		return Quote{}, fmt.Errorf("catalog: BYOK service fee must not be negative")
	}
	// This path has no model multiplier -- the service fee is itself the rate
	// -- so it borrows only the plan multiplier and the exchange-rate
	// validation from Rates.
	r.ModelMultiplierBps = noDiscountBps
	r, err := r.normalize()
	if err != nil {
		return Quote{}, err
	}
	fx, ok := new(big.Rat).SetString(r.FXRate)
	if !ok || fx.Sign() <= 0 {
		return Quote{}, fmt.Errorf("catalog: FX rate %q is invalid", r.FXRate)
	}

	charged, err := upstreamCost(p, t)
	if err != nil {
		return Quote{}, err
	}
	charged.Mul(charged, new(big.Rat).SetFrac(big.NewInt(feeBps), big.NewInt(10000)))
	charged.Mul(charged, new(big.Rat).SetFrac(big.NewInt(r.PlanMultiplierBps), big.NewInt(noDiscountBps)))
	charged.Mul(charged, fx)
	chargedNano, err := ceilToInt64(charged)
	if err != nil {
		return Quote{}, fmt.Errorf("catalog: BYOK service fee overflow: %w", err)
	}
	return Quote{
		UpstreamUSDNano:          0, // see the doc comment
		ChargedNano:              chargedNano,
		ModelMultiplierBps:       noDiscountBps,
		PlanMultiplierBps:        r.PlanMultiplierBps,
		ProcurementMultiplierBps: noDiscountBps,
		FXRate:                   r.FXRate,
	}, nil
}

// ExactBucketAmounts holds the unrounded per-bucket contributions, in nano.
// Each value is the canonical rational string: an integer is written as a
// number, anything else as numerator/denominator. The four buckets exclude
// tools; audio folds into input and output, TTL tiers fold into cache write,
// and tools stay their own line.
type ExactBucketAmounts struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read"`
	CacheWrite string `json:"cache_write"`
	Tools      string `json:"tools"`
}

// ExactQuoteContributions is the exact per-bucket record. The quote itself
// still rounds up exactly once, after everything is summed; nothing here is
// rounded early.
//
// **This is a test oracle, not a production path.** The comment used to say it
// was "used for shadow reconciliation", and there is no shadow reconciliation
// anywhere in this repository -- the only callers of ComputeExactContributions
// are image_tokens_test, price_table_test, pricing_test and context_bands_test.
// It stays because that is a real job: recomputing the same price without
// rounding is what lets those suites catch "input over-counted, output
// under-counted, total happens to match", which comparing totals cannot. It is
// the same kind of thing as proxy.OutboundAllowlist.
//
// It is not in a _test.go file because the callers are four suites in this
// package plus the unexported helpers below; if a fifth package ever needs it,
// that is the moment to move it to a catalogtest-style package rather than to
// widen this one.
type ExactQuoteContributions struct {
	UpstreamCostUSDNano ExactBucketAmounts `json:"upstream_cost_usd_nano"`
	ChargedNano         ExactBucketAmounts `json:"charged_nano"`
}

// ComputeExactContributions applies exactly the same rate fallbacks and
// multipliers as Compute but returns the unrounded per-bucket contributions.
// That is what catches "input over-counted, output under-counted, total happens
// to match".
func ComputeExactContributions(
	list, cost PriceTable, t Tokens, r Rates,
) (ExactQuoteContributions, error) {
	if err := ValidateTokens(t); err != nil {
		return ExactQuoteContributions{}, err
	}
	r, err := r.normalize()
	if err != nil {
		return ExactQuoteContributions{}, err
	}
	fx, ok := new(big.Rat).SetString(r.FXRate)
	if !ok || fx.Sign() <= 0 {
		return ExactQuoteContributions{}, fmt.Errorf("catalog: FX rate %q is invalid", r.FXRate)
	}
	costParts, err := upstreamCostBuckets(cost, t)
	if err != nil {
		return ExactQuoteContributions{}, err
	}
	listParts, err := upstreamCostBuckets(list, t)
	if err != nil {
		return ExactQuoteContributions{}, err
	}
	procurement := scaleExactBuckets(costParts,
		new(big.Rat).SetFrac(big.NewInt(r.ProcurementMultiplierBps), big.NewInt(noDiscountBps)),
	)
	charged := scaleExactBuckets(listParts,
		new(big.Rat).SetFrac(big.NewInt(r.ModelMultiplierBps), big.NewInt(noDiscountBps)),
		new(big.Rat).SetFrac(big.NewInt(r.PlanMultiplierBps), big.NewInt(noDiscountBps)),
		fx,
	)
	return ExactQuoteContributions{
		UpstreamCostUSDNano: snapshotExactBuckets(procurement),
		ChargedNano:         snapshotExactBuckets(charged),
	}, nil
}

// ComputeBYOKExactContributions matches ComputeBYOK: the cost buckets are 0 and
// the customer contribution is the pinned official rates times the service fee
// rate, the plan multiplier and the exchange rate.
func ComputeBYOKExactContributions(
	list PriceTable, t Tokens, feeBps int64, r Rates,
) (ExactQuoteContributions, error) {
	if err := ValidateTokens(t); err != nil {
		return ExactQuoteContributions{}, err
	}
	if feeBps < 0 {
		return ExactQuoteContributions{}, fmt.Errorf("catalog: BYOK service fee must not be negative")
	}
	r.ModelMultiplierBps = noDiscountBps
	r, err := r.normalize()
	if err != nil {
		return ExactQuoteContributions{}, err
	}
	fx, ok := new(big.Rat).SetString(r.FXRate)
	if !ok || fx.Sign() <= 0 {
		return ExactQuoteContributions{}, fmt.Errorf("catalog: FX rate %q is invalid", r.FXRate)
	}
	listParts, err := upstreamCostBuckets(list, t)
	if err != nil {
		return ExactQuoteContributions{}, err
	}
	charged := scaleExactBuckets(listParts,
		new(big.Rat).SetFrac(big.NewInt(feeBps), big.NewInt(noDiscountBps)),
		new(big.Rat).SetFrac(big.NewInt(r.PlanMultiplierBps), big.NewInt(noDiscountBps)),
		fx,
	)
	return ExactQuoteContributions{
		UpstreamCostUSDNano: snapshotExactBuckets(newExactBuckets()),
		ChargedNano:         snapshotExactBuckets(charged),
	}, nil
}

func scaleExactBuckets(in exactBuckets, multipliers ...*big.Rat) exactBuckets {
	out := newExactBuckets()
	for i := range out {
		out[i].Set(in[i])
		for _, multiplier := range multipliers {
			out[i].Mul(out[i], multiplier)
		}
	}
	return out
}

func snapshotExactBuckets(in exactBuckets) ExactBucketAmounts {
	return ExactBucketAmounts{
		Input: in[exactInput].RatString(), Output: in[exactOutput].RatString(),
		CacheRead: in[exactCacheRead].RatString(), CacheWrite: in[exactCacheWrite].RatString(),
		Tools: in[exactTools].RatString(),
	}
}

// ceilToInt64 rounds up and checks the int64 upper bound.
func ceilToInt64(r *big.Rat) (int64, error) {
	num, denom := r.Num(), r.Denom()
	q, rem := new(big.Int).QuoRem(num, denom, new(big.Int))
	if rem.Sign() > 0 { // only a positive remainder rounds up; truncation toward zero already suffices for negatives
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("value is outside the int64 range (%s)", q.String())
	}
	return q.Int64(), nil
}

// EstimateInput is the input to the pre-authorization estimate: the estimated
// input plus the output cap, times the rate.
type EstimateInput struct {
	Price Price
	// LongContext raises Price, bucket by bucket, to the dearest context-length
	// band the model configures -- PriceTable.LongContextCeiling produces it.
	//
	// The zero value has to mean the behaviour that existed before this axis
	// did, and here it does so structurally: taking the larger of a rate and
	// zero returns the rate. A model with no long-context pricing therefore
	// holds exactly what it always held, and no call site that leaves this
	// field unset can silently change how holds are computed.
	LongContext   Price
	InputTokens   int64 // estimated input tokens
	MaxOutput     int64 // max_tokens as stated in the request
	DefaultMaxCap int64 // output cap: the larger of the model's value and the candidates' overrides
	// IgnoreRequestCap reports that at least one candidate upstream does not
	// honour the output limit in the request. When it does, max_tokens is not
	// a cost ceiling and the estimate has to fall back to DefaultMaxCap.
	//
	// Phrasing it as "ignore" rather than "trust" is deliberate: the zero
	// value has to mean the existing behaviour. Written the other way round,
	// every call site that does not set the field explicitly would silently
	// change how holds are computed -- the first draft did exactly that, and
	// the existing tests caught it immediately.
	IgnoreRequestCap bool
	// Rates uses the same factors as settlement, so the hold is estimated on
	// the discounted amount. Otherwise a discounted customer has list price
	// held against their balance -- a gate stricter than their actual bill,
	// with nothing on screen to explain it.
	Rates Rates
}

// Estimate computes the amount to hold. On the output side it takes the smaller
// of the request's limit and the model's default cap: a hold is a gate, not a
// bill, so overestimating needlessly ties up the customer's balance, and
// settlement always charges what was actually used.
//
// But the request's limit is only a limit if the upstream honours it. One relay
// was measured ignoring max_output_tokens entirely -- 16 requested, 94 returned
// -- and holding for 16 there means the budget guard does not work. Settlement
// still bills correctly, but the point of a hold is to reserve what might be
// spent. When IgnoreRequestCap is set, the model's or route's cap is used
// instead: better to hold too much, which settlement returns, than too
// little.
//
// The same asymmetry decides the context-length axis, on the input side this
// time. The input figure here is a character-count heuristic, so a request
// estimated just under a threshold can settle just above it. The hold therefore
// prices at the dearest band the model has rather than at the band the estimate
// lands in: holding too much is returned at settlement, while holding too
// little lets through a request the balance cannot pay for.
func Estimate(in EstimateInput) (int64, error) {
	out := in.MaxOutput
	if in.IgnoreRequestCap {
		out = 0 // fall through to the DefaultMaxCap branch below
	}
	if out <= 0 {
		out = in.DefaultMaxCap
	}
	if in.DefaultMaxCap > 0 && out > in.DefaultMaxCap {
		out = in.DefaultMaxCap
	}
	if in.InputTokens < 0 || out < 0 {
		return 0, fmt.Errorf("catalog: estimated token count must not be negative")
	}
	price := dearerOf(in.Price, in.LongContext)
	// The hold estimates what the organization will be charged, which depends only
	// on the list price; upstream cost plays no part in the gate.
	q, err := Compute(Flat(price), Flat(price), Tokens{In: in.InputTokens, Out: out}, in.Rates)
	if err != nil {
		return 0, err
	}
	if q.ChargedNano <= 0 {
		// A model priced at zero still holds a minimum amount: a hold of 0 is
		// rejected by the billing layer's positive-amount check.
		return 1, nil
	}
	return q.ChargedNano, nil
}

// dearerOf takes the larger of each bucket. A zero ceiling leaves the rate
// untouched, which is what makes an unconfigured axis a no-op.
func dearerOf(base, ceiling Price) Price {
	larger := func(a, b int64) int64 {
		if b > a {
			return b
		}
		return a
	}
	return Price{
		InNanoPerMTok:         larger(base.InNanoPerMTok, ceiling.InNanoPerMTok),
		OutNanoPerMTok:        larger(base.OutNanoPerMTok, ceiling.OutNanoPerMTok),
		CacheReadNanoPerMTok:  larger(base.CacheReadNanoPerMTok, ceiling.CacheReadNanoPerMTok),
		CacheWriteNanoPerMTok: larger(base.CacheWriteNanoPerMTok, ceiling.CacheWriteNanoPerMTok),
	}
}

// maxSaneTokens is a defensive upper bound on the token count in one request:
// if an estimate runs away, fail early rather than computing an astronomical
// hold that empties the customer's balance.
const maxSaneTokens = int64(math.MaxInt32)

// ValidateTokens checks the base buckets, the advanced sub-buckets and the tool
// counts. The advanced fields come from the upstream's response, so validating
// only the four base buckets is not enough: a negative audio, image or TTL
// count produces a negative amount as soon as those rates differ.
func ValidateTokens(t Tokens) error {
	for _, v := range []int64{
		t.In, t.Out, t.CachedRead, t.CacheWrite,
		t.AudioIn, t.AudioOut, t.ImageIn, t.CacheWrite5m, t.CacheWrite1h,
	} {
		if v < 0 || v > maxSaneTokens {
			return fmt.Errorf("catalog: token count %d is outside the plausible range", v)
		}
	}
	if t.AudioIn > t.In || t.AudioOut > t.Out {
		return fmt.Errorf("catalog: audio tokens must be a subset of the input/output tokens")
	}
	if t.ImageIn > t.In {
		return fmt.Errorf("catalog: image tokens must be a subset of the input tokens")
	}
	if t.CacheWrite5m+t.CacheWrite1h > t.CacheWrite {
		return fmt.Errorf("catalog: the cache-write TTL buckets must not sum above the cache-write tokens")
	}
	for tool, calls := range t.ToolCalls {
		if calls < 0 || calls > maxSaneTokens {
			return fmt.Errorf("catalog: tool %q call count %d is outside the plausible range", tool, calls)
		}
	}
	return nil
}
