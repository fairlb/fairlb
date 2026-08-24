package pricing

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The write model.
//
// It is not the read model with different field names, and the difference is
// the reason both exist. On the way out, `model_pricing_complete_ck` guarantees
// a priced row has all four rates, so ModelPricing carries plain integers. On
// the way **in** there is no such guarantee, and absent is not zero:
//
//   - absent stores NULL, which the constraint then refuses for a paid model --
//     that is how "the price was never filled in" stays a distinguishable state;
//   - zero stores 0, which means this component is free.
//
// Collapsing the two would make a half-filled form look like a free model.

// RateInput is one editable rate. Set distinguishes absent from zero.
type RateInput struct {
	Nano int64
	Set  bool
}

// TokenRatesInput is the four editable rates as submitted.
type TokenRatesInput struct {
	Input      RateInput
	Output     RateInput
	CacheRead  RateInput
	CacheWrite RateInput
}

// AllZero reports whether every supplied rate is zero. An absent rate is not
// zero, so a rate that was not supplied does not make this true on its own --
// but the constraint refuses those anyway.
func (r TokenRatesInput) AllZero() bool {
	return r.Input.Nano == 0 && r.Output.Nano == 0 &&
		r.CacheRead.Nano == 0 && r.CacheWrite.Nano == 0
}

// DimensionRateInput is one advanced rate row as submitted.
type DimensionRateInput struct {
	Bucket         Bucket
	ServiceTier    string
	Variant        string
	MinInputTokens int32
	NanoPerMTok    int64
}

// ToolRateInput is one per-call tool price as submitted.
type ToolRateInput struct {
	Tool        string
	NanoPerCall int64
}

// ReplacedRates is how many advanced rate rows a write removed.
type ReplacedRates struct {
	Dimensions int64
	Tools      int64
}

// RiskSeverity is how a risk is treated: a blocker refuses, a warning has to be
// acknowledged.
type RiskSeverity string

const (
	SeverityBlocker RiskSeverity = "blocker"
	SeverityWarning RiskSeverity = "warning"
)

// Risk is one thing about a write worth stopping for.
type Risk struct {
	Severity RiskSeverity
	Code     string
	Message  string
}

// ModelPricingWrite is one write to a model's price.
//
// Provenance, verification time and actor are parameters rather than constants
// because they are exactly what differs between the two ways a price gets
// written -- an operator editing it in the console, and an import of a bundled
// reference dataset:
//
//   - VerifiedAt: an edit means a human read the vendor's price list; an import
//     means a dataset said so. Storing the import's run time here would erase
//     that distinction permanently, and it is what the "unverified" badge rests
//     on.
//   - Provenance: where the number came from, the only thing that can answer
//     "why is this model priced like that" six months later.
//   - Actor: an edit has a signed-in identity; an import run from a container
//     does not have one to record, and the column is nullable precisely so it
//     can say so rather than name somebody who was not there.
type ModelPricingWrite struct {
	BillingMode   BillingMode
	Official      TokenRatesInput
	MultiplierBps int32
	SourceName    string
	SourceURL     string
	Reason        string

	// DimensionRates and ToolRates: non-nil replaces that whole set, nil leaves
	// the existing set alone. An empty non-nil slice therefore means "remove
	// them all", which is a different instruction from "do not touch them".
	DimensionRates *[]DimensionRateInput
	ToolRates      *[]ToolRateInput

	// AcknowledgedRisks are the warning codes the caller has seen and accepted.
	// Blockers cannot be acknowledged.
	AcknowledgedRisks []string

	// Expected is the version the caller believes it is replacing. nil means no
	// precondition, which is what a bulk import has: it holds no earlier read
	// to be stale against, and its own overwrite rules decide what it may
	// touch. A pointer rather than a zero-value check because the zero time is
	// itself a meaningful version -- it is what an unpriced model has.
	Expected *time.Time

	VerifiedAt pgtype.Timestamptz
	Provenance []byte
	Actor      pgtype.UUID

	// AfterReplace, when set, runs inside this write's transaction once the old
	// dimension and tool rates have been removed, and is told how many went.
	//
	// A hook rather than a second return value because the count is only useful
	// to a caller that then writes it down, and it has to be written down here:
	// counted before the call it would describe whatever happened to be there a
	// moment earlier, and stamped after the commit it could be lost between the
	// two. A record naming a number nobody can reproduce is worse than one that
	// says nothing.
	AfterReplace func(context.Context, pgx.Tx, ReplacedRates) error

	// DryRun does the whole write and then rolls it back: the input is
	// validated, the risks are assessed, the row is upserted and every column
	// CHECK really runs, inside a transaction that is discarded.
	//
	// That way round rather than predicting the outcome separately, because a
	// preview computed by a second code path answers a slightly different
	// question every time the two drift -- and a preview that says "this would
	// be priced" where the real run refuses is worse than no preview at all.
	DryRun bool
}
