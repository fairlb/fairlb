// Package pricing owns what a model costs and which plan an organization is on.
//
// # Why these types exist rather than the generated DTOs
//
// The pricing service used to live inside the staff handler package and return
// `api.gen.go`'s types directly. Moving it out needed types the service could
// return without importing the handler package back (ADR-0160). Regenerating
// the DTOs into this package was not an option -- `make generate` rebuilds them
// where the spec says -- so the domain gets its own read models and the handler
// maps.
//
// The mapping is not busywork. Three things are decided here that the DTO left
// ambiguous, and each was a field-by-field judgement:
//
//   - **Rates are integers, not decimal strings.** The wire format is
//     `"1.50"`; nano is what the ledger charges against. Formatting is the
//     transport's job, and doing it here is how a rounding rule ends up applied
//     twice.
//   - **Absent is spelled once.** The DTO marks every optional rate `*string`,
//     because the columns are nullable. `model_pricing_complete_ck` says a row
//     that exists has all four -- so on this side "no price at all" is one
//     boolean, and the four rates are plain integers. The nil-per-field state
//     the DTO can express is not reachable.
//   - **Version is a value, not an ETag.** The service compares versions; the
//     handler renders and parses the header. An ETag string crossing this
//     boundary would put HTTP caching semantics inside the pricing rules.
package pricing

import (
	"time"

	"github.com/google/uuid"
)

// BillingMode is what the model charges: paid at its rates, or free.
// The column's CHECK is the source of this vocabulary.
type BillingMode string

const (
	BillingPaid BillingMode = "paid"
	BillingFree BillingMode = "free"
)

// TokenRates are the four per-million-token buckets, in nano USD.
//
// Always complete when it appears on a priced model: `model_pricing_complete_ck`
// refuses a row with any of the four missing.
type TokenRates struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Bucket names a rate dimension. The values match the column's CHECK, and that
// they still do is asserted rather than assumed: this list stood at six while
// the column allowed seven, so a saved image-input rate -- including every one
// the reference-price import produced -- was refused as an unknown bucket by
// the map below, on a path no test covered.
type Bucket string

const (
	BucketIn         Bucket = "in"
	BucketOut        Bucket = "out"
	BucketCacheRead  Bucket = "cache_read"
	BucketCacheWrite Bucket = "cache_write"
	BucketAudioIn    Bucket = "audio_in"
	BucketAudioOut   Bucket = "audio_out"
	BucketImageIn    Bucket = "image_in"
	BucketImageOut   Bucket = "image_out"
)

// KnownBuckets is every dimension bucket that may be written, in a stable
// order.
//
// It is the single list: the two round-trip maps are built from it rather than
// written out beside it, so they cannot be missing an entry, and a test holds
// it against the column's CHECK in the migration. That arrangement is the whole
// fix -- this list stood at six while the column allowed seven, and the maps
// stood at six too, so nothing in Go disagreed with anything else in Go and the
// only symptom was a save refused as "unknown pricing bucket".
func KnownBuckets() []Bucket {
	return []Bucket{
		BucketIn, BucketOut, BucketCacheRead, BucketCacheWrite,
		BucketAudioIn, BucketAudioOut, BucketImageIn, BucketImageOut,
	}
}

// DimensionRate is one advanced rate row: a bucket, optionally narrowed by
// service tier, variant, or a minimum input size.
type DimensionRate struct {
	Bucket         Bucket
	ServiceTier    string
	Variant        string
	MinInputTokens int32
	NanoPerMTok    int64
}

// ToolRate is a per-call price for a built-in tool.
type ToolRate struct {
	Tool        string
	NanoPerCall int64
}

// UnitRate is one price in the per-unit family: a second of produced output, or
// a generation.
//
// Resolution and Audio are the axes such a rate varies on, and an empty value
// there means "does not vary on that axis" and matches anything -- the opposite
// direction from the token rates, which walk down to a base rate. A model with
// one flat per-second price is a single row with both empty.
type UnitRate struct {
	Unit        string
	Resolution  string
	Audio       string
	Variant     string
	ServiceTier string
	NanoPerUnit int64
}

// ModelPricing is the whole of what this domain knows about one model's price.
type ModelPricing struct {
	ModelID uuid.UUID

	// Priced is false when the model exists but has no price row. That is a
	// legitimate state, not an error: a model stays disabled until priced.
	// Every field below is zero when it is false.
	Priced bool

	BillingMode BillingMode
	// Official is what the vendor charges.
	Official TokenRates
	// MultiplierBps is the markup in basis points; 10000 is cost.
	MultiplierBps int32
	// Published is Official times the multiplier, rounded up per bucket -- and
	// all zeros on a free model, which does not disclose the official rate.
	// Derived here rather than in the handler: it is a pricing rule, and a
	// second implementation of it is a second answer to "what do we charge".
	Published TokenRates

	SourceName string
	SourceURL  string
	// CheckedAt is when a person last verified the rate against the vendor.
	// Zero means never.
	CheckedAt time.Time
	UpdatedAt time.Time
	Reason    string

	// Family decides which of the rates below actually charge this model.
	// Empty reads as tokens, so a row written before the column existed keeps
	// its meaning.
	Family PricingFamily

	DimensionRates []DimensionRate
	ToolRates      []ToolRate
	// UnitRates is the whole rate card of the per-unit family. It never falls
	// back to a token rate: a lookup that finds nothing here fails, because
	// falling back would charge a clip at the price of a paragraph.
	UnitRates []UnitRate

	// Version changes on every save (a trigger moves updated_at). The handler
	// turns it into an ETag; the service compares it against a caller's
	// precondition.
	Version time.Time
}

// Plan is a pricing plan: a named default markup plus per-model overrides.
type Plan struct {
	ID                   uuid.UUID
	Slug                 string
	Name                 string
	Description          string
	IsDefault            bool
	Status               string
	OrgCount             int64
	DefaultMultiplierBps int32
	Reason               string
	CreatedAt            time.Time
	UpdatedAt            time.Time

	// Version is the row's etag column: a uuid the database rotates on write.
	// Unlike ModelPricing's timestamp this is an opaque identity, which is why
	// the two render as different kinds of ETag.
	Version uuid.UUID
}

// PlanModelOverride is one model's markup inside a plan.
type PlanModelOverride struct {
	ModelID       uuid.UUID
	ModelSlug     string
	MultiplierBps int32
}

// OrgPlan is the plan an organization is actually on.
type OrgPlan struct {
	OrgID uuid.UUID
	// PlanID is always set, including when the organization inherits: the
	// assignment records which default was in force at the time.
	PlanID   uuid.UUID
	PlanSlug string
	PlanName string
	// InheritedDefault is true when the organization follows the default plan
	// rather than naming one.
	InheritedDefault bool

	// Version is the assigned plan: the assignment has nothing else that
	// changes, so the plan's identity is what a precondition can be about.
	Version uuid.UUID
}
