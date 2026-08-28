package pricing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// ErrInvalid is a write the domain refuses: a blocker risk, an unacknowledged
// warning, or a value the rules do not allow.
var ErrInvalid = errors.New("pricing: invalid write")

// bucketToDB is built from KnownBuckets rather than written out beside it.
//
// A bucket missing here is not a silently unpriced dimension -- it is a save
// refused as "unknown pricing bucket", which is how image input came to be
// unwritable while the column accepted it. Deriving the map removes the way
// that happened: there is no second place to forget.
//
// The two vocabularies are identical by construction, which is what the map was
// there to guarantee in the first place; it survives as a map because the
// lookup is on a hot-ish write path and because a missing key is the error
// signal both readers already handle.
var bucketToDB = func() map[Bucket]string {
	out := make(map[Bucket]string, len(KnownBuckets()))
	for _, b := range KnownBuckets() {
		out[b] = string(b)
	}
	return out
}()

func rateColumn(r RateInput) pgtype.Int8 {
	return pgtype.Int8{Int64: r.Nano, Valid: r.Set}
}

// tokenRateColumn is rateColumn with the unit family's exception.
//
// NULL means "unknown" throughout this schema and fails closed, which is right
// for a token model whose rate nobody has filled in. A unit-priced model's text
// rate is not unknown -- it does not exist -- and zero is the honest spelling
// of that. Writing NULL instead would trip the completeness constraint and make
// a per-second model unrepresentable, which is the state this family was added
// to fix.
func tokenRateColumn(family PricingFamily, r RateInput) pgtype.Int8 {
	if family == FamilyUnits && !r.Set {
		return pgtype.Int8{Int64: 0, Valid: true}
	}
	return rateColumn(r)
}

// SaveModelPricing writes a model's price. Saving is publishing: there is no
// draft, and no version state machine.
//
// Three things survive from the version machinery it replaced, because they are
// ledger correctness rather than release workflow: NULL is not 0, risks are
// still assessed, and a reason is mandatory and audited.
func (w *Writer) SaveModelPricing(
	ctx context.Context, modelID uuid.UUID, in ModelPricingWrite,
) (ModelPricing, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return ModelPricing{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.q.WithTx(tx)

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM models WHERE id=$1)`, modelID).Scan(&exists); err != nil {
		return ModelPricing{}, err
	}
	if !exists {
		return ModelPricing{}, ErrNotFound
	}

	// Read the current state inside the transaction, then check the caller's
	// precondition against it. The caller may already have compared a rendered
	// validator outside; this is the check that closes the window between that
	// read and this write.
	current, err := w.reader.WithQueries(q).ModelPricing(ctx, modelID)
	if err != nil {
		return ModelPricing{}, err
	}
	if in.Expected != nil && !in.Expected.Equal(current.Version) {
		return ModelPricing{}, ErrConflict
	}

	risks, err := w.assessRisks(ctx, modelID, in, current)
	if err != nil {
		return ModelPricing{}, err
	}
	if err := requireAcknowledged(risks, in.AcknowledgedRisks); err != nil {
		return ModelPricing{}, err
	}

	if _, err := q.UpsertModelPricing(ctx, gwdb.UpsertModelPricingParams{
		ModelID:            pgID(modelID),
		BillingMode:        string(in.BillingMode),
		PricingFamily:      familyOrTokens(in.Family),
		UpstreamIn:         tokenRateColumn(in.Family, in.Official.Input),
		UpstreamOut:        tokenRateColumn(in.Family, in.Official.Output),
		UpstreamCacheRead:  tokenRateColumn(in.Family, in.Official.CacheRead),
		UpstreamCacheWrite: tokenRateColumn(in.Family, in.Official.CacheWrite),
		MultiplierBps:      in.MultiplierBps,
		SourceName:         in.SourceName,
		SourceUrl:          in.SourceURL,
		VerifiedAt:         in.VerifiedAt,
		Provenance:         in.Provenance,
		Reason:             in.Reason,
		UpdatedBy:          in.Actor,
	}); err != nil {
		return ModelPricing{}, err
	}

	if in.UnitRates != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM model_price_unit_rates WHERE model_id=$1`, modelID); err != nil {
			return ModelPricing{}, err
		}
		for _, u := range *in.UnitRates {
			if _, err := q.UpsertModelPriceUnitRate(ctx, gwdb.UpsertModelPriceUnitRateParams{
				ModelID: pgID(modelID), Unit: u.Unit,
				Resolution: u.Resolution, Audio: u.Audio, Variant: u.Variant,
				ServiceTier: tierOrStandard(u.ServiceTier), NanoPerUnit: u.NanoPerUnit,
			}); err != nil {
				return ModelPricing{}, err
			}
		}
	}

	var replaced ReplacedRates
	if in.DimensionRates != nil {
		tag, err := tx.Exec(ctx, `DELETE FROM model_price_dimension_rates WHERE model_id=$1`, modelID)
		if err != nil {
			return ModelPricing{}, err
		}
		replaced.Dimensions = tag.RowsAffected()
		for _, d := range *in.DimensionRates {
			bucket, ok := bucketToDB[d.Bucket]
			if !ok {
				// Refusing here rather than passing the value through keeps the
				// answer a validation error naming the field. Passed through it
				// would fail the column's CHECK and surface as a database error.
				return ModelPricing{}, fmt.Errorf("%w: unknown pricing bucket %q", ErrInvalid, d.Bucket)
			}
			if d.MinInputTokens < 0 {
				return ModelPricing{}, fmt.Errorf(
					"%w: min_input_tokens must not be negative, got %d", ErrInvalid, d.MinInputTokens)
			}
			if _, err := q.UpsertModelPriceDimensionRate(ctx, gwdb.UpsertModelPriceDimensionRateParams{
				ModelID: pgID(modelID), Bucket: bucket,
				ServiceTier: d.ServiceTier, Variant: d.Variant,
				MinInputTokens: d.MinInputTokens, NanoPerMtok: d.NanoPerMTok,
			}); err != nil {
				return ModelPricing{}, err
			}
		}
	}
	if in.ToolRates != nil {
		tag, err := tx.Exec(ctx, `DELETE FROM model_price_tool_rates WHERE model_id=$1`, modelID)
		if err != nil {
			return ModelPricing{}, err
		}
		replaced.Tools = tag.RowsAffected()
		for _, t := range *in.ToolRates {
			if _, err := q.UpsertModelPriceToolRate(ctx, gwdb.UpsertModelPriceToolRateParams{
				ModelID: pgID(modelID), Tool: t.Tool, NanoPerCall: t.NanoPerCall,
			}); err != nil {
				return ModelPricing{}, err
			}
		}
	}
	if in.AfterReplace != nil {
		if err := in.AfterReplace(ctx, tx, replaced); err != nil {
			return ModelPricing{}, err
		}
	}
	// The dry run stops here, after everything above has really run -- and
	// after AfterReplace, not instead of it: the point of doing the work and
	// discarding it is that the preview exercises the same path the real write
	// does, and a hook skipped only in preview is a path the preview cannot
	// speak for.
	if in.DryRun {
		// The deferred rollback discards everything, so nothing is stored and
		// nothing is invalidated. No state is returned either: there is no
		// stored row to describe, and reading the old one back would hand the
		// caller a value that looks like a result of this call.
		return ModelPricing{}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return ModelPricing{}, err
	}
	if w.modelInvalidator != nil {
		// Exactly this model, rather than bumping a global counter and calling
		// everything stale.
		w.modelInvalidator.InvalidateModel(ctx, modelID)
	}
	return w.reader.ModelPricing(ctx, modelID)
}

// assessRisks names what is worth stopping for before a price takes effect.
func (w *Writer) assessRisks(
	ctx context.Context, modelID uuid.UUID, in ModelPricingWrite, prev ModelPricing,
) ([]Risk, error) {
	var risks []Risk
	add := func(sev RiskSeverity, code, msg string) {
		risks = append(risks, Risk{Severity: sev, Code: code, Message: msg})
	}

	// Blocker: there must always be an active default plan, or requests that
	// cannot resolve one fail closed at run time.
	var hasDefault bool
	if err := w.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pricing_plans WHERE is_default AND status='active')`).
		Scan(&hasDefault); err != nil {
		return nil, err
	}
	if !hasDefault {
		add(SeverityBlocker, "missing_default_plan",
			"there is no active default pricing plan; after saving, requests will fail closed")
	}
	// Blocker: a paid model with all four rates at zero would merge
	// "deliberately free" and "price never filled in" into one state.
	//
	// Not for a unit-priced model: its four token rates are legitimately zero
	// because it has no token price to give, and its own pricedness is decided
	// by its unit rates below. This is the third place that rule lives -- the
	// column constraint, the admission check and here -- and each of the three
	// has to know about the family or a per-second model is unrepresentable at
	// that layer.
	if in.BillingMode == BillingPaid && in.Family != FamilyUnits && in.Official.AllZero() {
		add(SeverityBlocker, "paid_all_zero",
			"a paid model with all four rates at zero: use billing_mode=free to make the whole model free")
	}
	// The unit family's own version of the same rule: a paid model billed by
	// unit with no unit rate cannot be charged for, and admission would refuse
	// every request against it.
	//
	// Only an explicit clear is blocked. A nil set means "leave the existing
	// rates alone", so treating it as "no rates" would refuse every save that
	// touches anything else about a unit-priced model. The remaining case -- a
	// model that has never had a rate and is saved without one -- is caught at
	// admission, which refuses it as unpriced and tells the operator.
	if in.BillingMode == BillingPaid && in.Family == FamilyUnits &&
		in.UnitRates != nil && len(*in.UnitRates) == 0 {
		add(SeverityBlocker, "units_without_rates",
			"a unit-priced model needs at least one per-unit rate; without one every request is refused as unpriced")
	}
	// Warning: switching to free changes what customers are charged.
	if in.BillingMode == BillingFree && prev.Priced && prev.BillingMode == BillingPaid {
		add(SeverityWarning, "switch_to_free", "this model changes from paid to free")
	}
	// Warning: pricing below cost. Estimated conservatively against the most
	// expensive provider, since that is where the margin goes negative first.
	var maxCost pgtype.Int4
	if err := w.pool.QueryRow(ctx, `
SELECT max(p.cost_multiplier_bps) FROM model_routes r
JOIN providers p ON p.id = r.provider_id
WHERE r.model_id = $1 AND r.enabled AND p.enabled`, modelID).Scan(&maxCost); err != nil {
		return nil, err
	}
	if !maxCost.Valid {
		// No usable route, or the cost is unknown: the margin cannot be stated,
		// and pretending otherwise would be worse than saying so.
		add(SeverityWarning, "unknown_procurement_cost",
			"this model has no usable provider, so the margin cannot be computed")
	} else if int64(in.MultiplierBps) < int64(maxCost.Int32) {
		add(SeverityWarning, "negative_margin",
			"the sales multiplier is below the most expensive provider's procurement multiplier; on that provider this model has a negative margin")
	}
	// Warning: the customer-facing price drops by 20% or more, estimated
	// against the default plan; per-model overrides are assessed on the plan.
	if prev.Priced && prev.MultiplierBps > 0 {
		drop := float64(prev.MultiplierBps-in.MultiplierBps) / float64(prev.MultiplierBps)
		if drop >= 0.2 {
			add(SeverityWarning, "customer_price_drop", "the customer-facing price drops by more than 20%")
		}
	}
	return risks, nil
}

// requireAcknowledged turns the assessed risks into a decision to proceed or refuse.
//
// A blocker cannot be acknowledged: it means the ledger does not add up, and
// acknowledging that would be signing off on a wrong number.
func requireAcknowledged(risks []Risk, acked []string) error {
	ackSet := make(map[string]bool, len(acked))
	for _, c := range acked {
		ackSet[c] = true
	}
	var blockers, unacked []string
	for _, r := range risks {
		if r.Severity == SeverityBlocker {
			blockers = append(blockers, r.Message)
			continue
		}
		if !ackSet[r.Code] {
			unacked = append(unacked, r.Code+" ("+r.Message+")")
		}
	}
	if len(blockers) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(blockers, "; "))
	}
	if len(unacked) > 0 {
		return fmt.Errorf("%w: these risks must be confirmed in acknowledged_risks before saving: %s",
			ErrInvalid, strings.Join(unacked, "; "))
	}
	return nil
}

// familyOrTokens defaults an unset family to tokens, so every caller that
// predates the unit family keeps writing what it always wrote.
func familyOrTokens(f PricingFamily) string {
	if f == FamilyUnits {
		return string(FamilyUnits)
	}
	return string(FamilyTokens)
}

// tierOrStandard fills in the service tier the way the column's default does.
func tierOrStandard(tier string) string {
	if tier == "" {
		return "standard"
	}
	return tier
}
