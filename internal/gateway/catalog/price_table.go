package catalog

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Pricing dimensions.
//
// Four scalars cannot hold what both protocols already report today:
//
//	service tier     both have it; standard, priority and batch can differ
//	                 in price by a factor of two
//	cache-write TTL  Anthropic splits cache creation into 5m and 1h, priced
//	                 differently
//	audio tokens     OpenAI reports them on both the input and output side,
//	                 priced differently from text
//	context length   several models charge more once the prompt passes a
//	                 threshold, on the output side as well as the input side
//
// Not modelling any of these silently produces a wrong price: a batch request
// billed at the standard rate overcharges, a priority request costed at the
// standard rate inflates the margin, a 1h cache write billed at the 5m rate
// undercharges, and a 250k-token prompt billed at the short-prompt rate
// undercharges on all four buckets at once.
//
// The design here has one principle: with no overrides configured, lookup must
// fall back to the model's four base rates. Nothing changes for a model that
// declares no dimensions -- that is guaranteed by the structure, not by
// backfilling data.

// The pricing buckets. The first four map one to one onto the base rates; the
// two audio buckets are the added dimension.
const (
	BucketIn         = "in"
	BucketOut        = "out"
	BucketCacheRead  = "cache_read"
	BucketCacheWrite = "cache_write"
	BucketAudioIn    = "audio_in"
	BucketAudioOut   = "audio_out"
)

// Service tiers. Both protocols agree on these three values.
const (
	TierStandard = "standard"
	TierPriority = "priority"
	TierBatch    = "batch"
)

// The cache-write TTL tiers, matching the two keys Anthropic reports. The empty
// string means undifferentiated, which is the default.
const (
	VariantNone    = ""
	VariantCache5m = "5m"
	VariantCache1h = "1h"
)

type priceKey struct{ bucket, tier, variant string }

// contextBand is one rate of a priceKey, valid from MinInputTokens upwards. The
// bound is inclusive, so the bands of one key tile the axis as
// [min, next min) -- no gap, no overlap, and a band at 0 covers every request
// including one with no input at all.
//
// That inclusiveness is why an upstream rule phrased as "prompts above 200K" is
// configured as 200001. Configured as 200000 it would move the request that
// lands exactly on the boundary into the dearer band, which is the same class
// of error as undercharging, only in the other direction.
type contextBand struct {
	minInputTokens int64
	nanoPerMTok    int64
}

type DimensionRateSnapshot struct {
	Bucket      string `json:"bucket"`
	ServiceTier string `json:"service_tier"`
	Variant     string `json:"variant,omitempty"`
	// MinInputTokens is the lower bound of this rate's context-length band.
	// Omitted when 0, which is the band every model without long-context
	// pricing has.
	MinInputTokens int64 `json:"min_input_tokens,omitempty"`
	NanoPerMTok    int64 `json:"nano_per_mtok"`
}

type PriceTableSnapshot struct {
	Base           Price                   `json:"base_nano_per_mtok"`
	DimensionRates []DimensionRateSnapshot `json:"dimension_rates"`
	ToolRates      map[string]int64        `json:"tool_rates_nano_per_call"`
}

// PriceTable addresses a rate by (bucket, tier, variant) and, within that, by
// context length.
type PriceTable struct {
	base Price
	// over holds each key's context-length bands, sorted by lower bound
	// descending so that resolving a length is a walk to the first band that
	// qualifies.
	over  map[priceKey][]contextBand
	tools map[string]int64 // tool name to nano per call
	// billingFree marks the customer side as free without losing the
	// information about which overrides exist. The cost side keeps using the
	// unswitched table, so "free" never zeroes out the real cost.
	billingFree bool
}

// NewPriceTable builds a table from the model's base rates plus its dimension
// override rows.
func NewPriceTable(base Price, rows []gwdb.ModelPriceDimensionRate) PriceTable {
	t := PriceTable{base: base}
	if len(rows) == 0 {
		return t
	}
	t.over = make(map[priceKey][]contextBand, len(rows))
	for _, r := range rows {
		key := priceKey{r.Bucket, r.ServiceTier, r.Variant}
		t.over[key] = append(t.over[key], contextBand{
			minInputTokens: int64(r.MinInputTokens), nanoPerMTok: r.NanoPerMtok,
		})
	}
	// Descending, so the first qualifying band is also the dearest applicable
	// one. The rows arrive ordered by the query, but sorting here means the
	// resolution does not depend on a caller having read them from that query.
	for _, bands := range t.over {
		sort.Slice(bands, func(i, j int) bool {
			return bands[i].minInputTokens > bands[j].minInputTokens
		})
	}
	return t
}

// WithToolPrices attaches the per-call tool rates. It is a separate step rather
// than another constructor argument because most models configure neither, and
// separating them makes "only one of the two is configured" fall out
// naturally.
func (t PriceTable) WithToolPrices(rows []gwdb.ModelPriceToolRate) PriceTable {
	if len(rows) == 0 {
		return t
	}
	t.tools = make(map[string]int64, len(rows))
	for _, r := range rows {
		t.tools[r.Tool] = r.NanoPerCall
	}
	return t
}

// LockedPriceTable reads this model's dimension rates.
//
// The price is pinned by the caller's snapshot transaction, not by an immutable
// version id: reading once inside the transaction is itself the lock, so a new
// price taking effect afterwards cannot make this request drift.
func (s *Service) LockedPriceTable(
	ctx context.Context, modelID pgtype.UUID, pricing ModelPricingSnapshot,
) (PriceTable, error) {
	dims, err := s.q.ListModelPriceDimensionRates(ctx, modelID)
	if err != nil {
		return PriceTable{}, fmt.Errorf("catalog: read model dimension rates: %w", err)
	}
	tools, err := s.q.ListModelPriceToolRates(ctx, modelID)
	if err != nil {
		return PriceTable{}, fmt.Errorf("catalog: read model tool rates: %w", err)
	}
	return NewPriceTable(pricing.Upstream, dims).WithToolPrices(tools), nil
}

// Snapshot returns the complete table written alongside the request in the
// usage log, so nothing later has to join against configuration. The sorting is
// only to keep the JSON stable and diffs readable; billing does not depend on
// order.
func (t PriceTable) Snapshot() PriceTableSnapshot {
	dims := make([]DimensionRateSnapshot, 0, len(t.over))
	for key, bands := range t.over {
		for _, band := range bands {
			dims = append(dims, DimensionRateSnapshot{
				Bucket: key.bucket, ServiceTier: key.tier, Variant: key.variant,
				MinInputTokens: band.minInputTokens, NanoPerMTok: band.nanoPerMTok,
			})
		}
	}
	sort.Slice(dims, func(i, j int) bool {
		if dims[i].Bucket != dims[j].Bucket {
			return dims[i].Bucket < dims[j].Bucket
		}
		if dims[i].ServiceTier != dims[j].ServiceTier {
			return dims[i].ServiceTier < dims[j].ServiceTier
		}
		if dims[i].Variant != dims[j].Variant {
			return dims[i].Variant < dims[j].Variant
		}
		return dims[i].MinInputTokens < dims[j].MinInputTokens
	})
	tools := make(map[string]int64, len(t.tools))
	for name, rate := range t.tools {
		tools[name] = rate
	}
	return PriceTableSnapshot{Base: t.base, DimensionRates: dims, ToolRates: tools}
}

// ForBilling switches the table to the customer-billing view. A model declared
// free keeps its official and procurement rates so cost can still be computed,
// while every customer-facing token, dimension and tool rate goes to zero at
// once.
func (t PriceTable) ForBilling(free bool) PriceTable {
	if !free {
		return t
	}
	t.billingFree = true
	return t
}

// Get returns a rate, falling back in three steps:
//
//  1. an exact match on (bucket, tier, variant)
//  2. the same bucket and tier with no variant -- "a batch cache-write rate
//     configured without splitting by TTL" is a normal configuration
//  3. the default tier, i.e. the model's four base rates
//
// contextTokens selects the context-length band *within* each of those steps:
// the step yields the dearest band whose lower bound the request reaches.
//
// The order matters and is not interchangeable. Tier and variant say which kind
// of token is being priced; the band says which stretch of that same kind.
// Resolving the length first would let a long batch request pick up a
// standard-tier long-context rate -- pricing batch usage at the standard rate,
// which is precisely what the tier axis exists to prevent.
//
// Falling through on a band miss obeys the same boundary, and it has to be
// stated separately because it is the mirror image of the same mistake. A tier
// this model prices at all is a tier this request stays in: if the requested
// tier has rates for this bucket but none that reach this prompt length, the
// answer is the model's base rate, not whatever the standard tier charges. Let
// it drop into the standard tier there and a short batch request lands on the
// standard band -- again batch usage at the standard rate, reached from the
// other direction. A tier the model does not price for this bucket at all is a
// different case: that fallback predates the length axis and is unchanged.
//
// With no override configured, the two audio buckets fall back to their text
// counterparts, which is what already happens: audio tokens are counted inside
// prompt and completion tokens and billed at the text rate. Falling back to 0
// would give them away, and giving them away is far worse than billing them at
// the text rate.
func (t PriceTable) Get(bucket, tier, variant string, contextTokens int64) int64 {
	return t.lookup(bucket, tier, variant, contextTokens)
}

// lookup resolves a rate from the dimension overrides down to the base rates;
// the free view always returns 0.
func (t PriceTable) lookup(bucket, tier, variant string, contextTokens int64) int64 {
	if t.billingFree {
		return 0
	}
	if v, ok := lookupDimensionRate(t.over, bucket, tier, variant, contextTokens); ok {
		return v
	}
	return t.baseOf(bucket)
}

func lookupDimensionRate(
	rates map[priceKey][]contextBand, bucket, tier, variant string, contextTokens int64,
) (int64, bool) {
	if rates == nil {
		return 0, false
	}
	// band walks one key's bands, which are sorted by lower bound descending,
	// and returns the first one the request reaches. Not reaching any of them
	// is reported as a miss rather than as a rate of 0: a model that configures
	// only a 200k band still charges its base rate below 200k, and answering 0
	// there would give those requests away.
	band := func(key priceKey) (int64, bool) {
		for _, b := range rates[key] {
			if b.minInputTokens <= contextTokens {
				return b.nanoPerMTok, true
			}
		}
		return 0, false
	}
	// withinTier resolves one service tier: the requested variant first, then
	// the undifferentiated one, which is a normal way to configure a rate that
	// does not split by cache TTL.
	//
	// It returns two different negative answers, and keeping them apart is the
	// whole point. "priced" says this tier has a rate for this bucket at some
	// length; a caller may not treat that as an unconfigured tier and go looking
	// in another one.
	withinTier := func(t string) (rate int64, found, priced bool) {
		exact := priceKey{bucket, t, variant}
		priced = len(rates[exact]) > 0
		if v, ok := band(exact); ok {
			return v, true, true
		}
		if variant != VariantNone {
			plain := priceKey{bucket, t, VariantNone}
			priced = priced || len(rates[plain]) > 0
			if v, ok := band(plain); ok {
				return v, true, true
			}
		}
		return 0, false, priced
	}

	rate, found, priced := withinTier(tier)
	if found {
		return rate, true
	}
	if tier == TierStandard || priced {
		// Either there is no other tier to try, or this tier is priced and
		// simply not at this length -- which the base rate answers. Continuing
		// into the standard tier here would bill a short batch prompt at the
		// standard rate.
		return 0, false
	}
	if rate, found, _ = withinTier(TierStandard); found {
		return rate, true
	}
	return 0, false
}

// LongContextCeiling is the dearest rate each of the four base buckets can
// reach through the context-length axis, and zero for a bucket that has no such
// band. It is what a pre-authorization holds against; see Estimate.
//
// Two exclusions, both deliberate:
//
//   - The tier and variant axes are not folded in. The hold has always been
//     computed on the flat base rates, ignoring every dimension override, and
//     folding them in now would change the hold of models that configure no
//     context bands at all -- the one thing this axis must not do.
//   - The audio buckets are not folded in either. A hold prices In and Out
//     only, so an audio band would raise the gate using a rate that takes no
//     part in the computation.
func (t PriceTable) LongContextCeiling() Price {
	var out Price
	if t.billingFree {
		// Every rate reads 0 in the free view, so there is nothing to raise.
		return out
	}
	raise := func(dst *int64, v int64) {
		if v > *dst {
			*dst = v
		}
	}
	for key, bands := range t.over {
		for _, b := range bands {
			if b.minInputTokens <= 0 {
				continue
			}
			switch key.bucket {
			case BucketIn:
				raise(&out.InNanoPerMTok, b.nanoPerMTok)
			case BucketOut:
				raise(&out.OutNanoPerMTok, b.nanoPerMTok)
			case BucketCacheRead:
				raise(&out.CacheReadNanoPerMTok, b.nanoPerMTok)
			case BucketCacheWrite:
				raise(&out.CacheWriteNanoPerMTok, b.nanoPerMTok)
			}
		}
	}
	return out
}

func (t PriceTable) baseOf(bucket string) int64 {
	switch bucket {
	case BucketIn, BucketAudioIn:
		return t.base.InNanoPerMTok
	case BucketOut, BucketAudioOut:
		return t.base.OutNanoPerMTok
	case BucketCacheRead:
		return t.base.CacheReadNanoPerMTok
	case BucketCacheWrite:
		return t.base.CacheWriteNanoPerMTok
	}
	return 0
}

func (t PriceTable) lookupToolPrice(tool string) (rate int64, ok bool) {
	if t.billingFree {
		// Free is an explicit customer-side mode. The same billing pass first
		// checks the official tool rate against the cost table; this only
		// zeroes the already-validated customer charge.
		return 0, true
	}
	v, found := t.tools[tool]
	return v, found
}

// HasOverrides reports whether any non-default rate is configured. It answers
// "should a missing dimension raise an alert": for a model that never
// configured dimensions, an upstream reporting batch is not an anomaly --
// nobody intended to price by tier there.
func (t PriceTable) HasOverrides() bool { return len(t.over) > 0 }

// Flat wraps a single set of rates as a table with no dimension overrides.
//
// It serves two kinds of caller: paths that are dimension-independent by nature,
// such as catalog display and pre-authorization, and tests. With no overrides,
// Get always falls back to the four base rates.
func Flat(p Price) PriceTable { return PriceTable{base: p} }

// Base returns the four default-tier rates, which is the base the service fee
// is computed on.
func (t PriceTable) Base() Price { return t.base }
