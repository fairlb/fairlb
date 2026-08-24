package pricing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/db"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// ErrReferenced is a delete refused because something still points at the row.
var ErrReferenced = errors.New("pricing: still referenced")

// PlanInvalidator narrows "a plan changed" to the plan it was.
type PlanInvalidator interface {
	InvalidatePlan(ctx context.Context, planID uuid.UUID)
}

// PlanCreate is a new plan.
type PlanCreate struct {
	Slug                 string
	Name                 string
	Description          string
	DefaultMultiplierBps int32
}

// PlanPatch is a partial update: a nil field is left as it is.
//
// Pointers here and plain values in PlanCreate, because the two carry different
// questions. On create every field has an answer; on patch "not supplied" and
// "supplied as empty" are different instructions, and collapsing them would
// make clearing a description impossible to express.
type PlanPatch struct {
	Name                 *string
	Description          *string
	Status               *string
	DefaultMultiplierBps *int32
	Reason               string
}

// PlanOverrideInput is one model's markup inside a plan.
type PlanOverrideInput struct {
	ModelID       uuid.UUID
	MultiplierBps int32
}

func planNotFound(err error) error {
	if db.IsNoRows(err) {
		return ErrNotFound
	}
	return err
}

func conflictOnNoRows(err error) error {
	if db.IsNoRows(err) {
		return ErrConflict
	}
	return err
}

// CreatePlan creates a pricing plan. The default markup lives on the plan.
func (w *Writer) CreatePlan(ctx context.Context, in PlanCreate) (Plan, error) {
	row, err := w.q.CreatePricingPlan(ctx, gwdb.CreatePricingPlanParams{
		Slug: in.Slug, Name: in.Name, Description: in.Description,
		DefaultMultiplierBps: in.DefaultMultiplierBps,
	})
	if err != nil {
		return Plan{}, err
	}
	return w.reader.Plan(ctx, uuid.UUID(row.ID.Bytes))
}

// UpdatePlan applies a partial update under a row lock.
//
// expected is the version the caller believes it is replacing; the zero uuid
// means no precondition. Unlike a model's price, a plan's version is an opaque
// identity the database rotates, so a caller can hand it straight back and this
// needs no read-then-compare on the transport side.
func (w *Writer) UpdatePlan(
	ctx context.Context, id uuid.UUID, in PlanPatch, expected uuid.UUID, actor pgtype.UUID,
) (Plan, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.q.WithTx(tx)

	var locked pgtype.UUID
	if err := tx.QueryRow(ctx,
		`SELECT etag FROM pricing_plans WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		return Plan{}, planNotFound(err)
	}
	row, err := q.GetPricingPlan(ctx, pgID(id))
	if err != nil {
		return Plan{}, planNotFound(err)
	}
	if expected != (uuid.UUID{}) && expected != uuid.UUID(row.Etag.Bytes) {
		return Plan{}, ErrConflict
	}

	if in.Status != nil && *in.Status == "disabled" {
		// There is always exactly one active default plan. Disabling it makes
		// every request that cannot resolve a plan fail closed, and what the
		// operator then sees is "the gateway is broken", not "I disabled the
		// wrong thing".
		if row.IsDefault {
			return Plan{}, fmt.Errorf("%w: the default pricing plan must stay active", ErrInvalid)
		}
		var referenced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM org_pricing_plan_assignments
			WHERE pricing_plan_id=$1 AND NOT inherit_default)`, id).Scan(&referenced); err != nil {
			return Plan{}, err
		}
		if referenced {
			return Plan{}, fmt.Errorf(
				"%w: organizations are still assigned to this pricing plan, so it cannot be disabled",
				ErrInvalid)
		}
	}

	name, description, status := row.Name, row.Description, row.Status
	multiplier := row.DefaultMultiplierBps
	if in.Name != nil {
		name = *in.Name
	}
	if in.Description != nil {
		description = *in.Description
	}
	if in.Status != nil {
		status = *in.Status
	}
	if in.DefaultMultiplierBps != nil {
		multiplier = *in.DefaultMultiplierBps
	}
	updated, err := q.UpdatePricingPlan(ctx, gwdb.UpdatePricingPlanParams{
		Name: name, Description: description, Status: status,
		DefaultMultiplierBps: multiplier,
		Reason:               in.Reason, UpdatedBy: actor,
		ID: row.ID, Etag: row.Etag,
	})
	if err != nil {
		return Plan{}, conflictOnNoRows(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Plan{}, err
	}
	if w.planInvalidator != nil {
		w.planInvalidator.InvalidatePlan(ctx, id)
	}
	return w.reader.Plan(ctx, uuid.UUID(updated.ID.Bytes))
}

// DeletePlan removes a plan that nothing points at.
func (w *Writer) DeletePlan(ctx context.Context, id uuid.UUID, expected uuid.UUID) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.q.WithTx(tx)

	var etag pgtype.UUID
	var isDefault, hasAssignments bool
	err = tx.QueryRow(ctx, `
SELECT p.etag, p.is_default,
       EXISTS(SELECT 1 FROM org_pricing_plan_assignments a WHERE a.pricing_plan_id=p.id)
FROM pricing_plans p WHERE p.id=$1 FOR UPDATE`, id).Scan(&etag, &isDefault, &hasAssignments)
	if err != nil {
		return planNotFound(err)
	}
	if uuid.UUID(etag.Bytes) != expected {
		return ErrConflict
	}
	if isDefault || hasAssignments {
		return fmt.Errorf(
			"%w: a default or still-referenced pricing plan cannot be deleted; disable it instead",
			ErrReferenced)
	}
	n, err := q.DeletePricingPlan(ctx, gwdb.DeletePricingPlanParams{ID: pgID(id), Etag: etag})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

// CopyPlan duplicates a plan, model overrides included.
func (w *Writer) CopyPlan(ctx context.Context, sourceID uuid.UUID, in PlanCreate) (Plan, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.q.WithTx(tx)

	source, err := q.GetPricingPlan(ctx, pgID(sourceID))
	if err != nil {
		return Plan{}, planNotFound(err)
	}
	// The copy takes the source's default markup, not the caller's: copying a
	// plan means "the same pricing under another name", and letting the request
	// set it here would make copy quietly different from create-plus-copy.
	plan, err := q.CreatePricingPlan(ctx, gwdb.CreatePricingPlanParams{
		Slug: in.Slug, Name: in.Name, Description: in.Description,
		DefaultMultiplierBps: source.DefaultMultiplierBps,
	})
	if err != nil {
		return Plan{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO pricing_plan_model_overrides (pricing_plan_id, model_id, multiplier_bps)
SELECT $1, model_id, multiplier_bps FROM pricing_plan_model_overrides WHERE pricing_plan_id = $2`,
		plan.ID, source.ID); err != nil {
		return Plan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Plan{}, err
	}
	return w.reader.Plan(ctx, uuid.UUID(plan.ID.Bytes))
}

// ReplacePlanOverrides replaces the whole set of a plan's model overrides.
func (w *Writer) ReplacePlanOverrides(
	ctx context.Context, id uuid.UUID, inputs []PlanOverrideInput, expected uuid.UUID,
) ([]PlanModelOverride, uuid.UUID, error) {
	seen := make(map[uuid.UUID]bool, len(inputs))
	for _, o := range inputs {
		if seen[o.ModelID] {
			return nil, uuid.UUID{}, fmt.Errorf("%w: a model can have at most one override", ErrInvalid)
		}
		seen[o.ModelID] = true
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, uuid.UUID{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := w.q.WithTx(tx)

	row, err := q.GetPricingPlan(ctx, pgID(id))
	if err != nil {
		return nil, uuid.UUID{}, planNotFound(err)
	}
	if expected != (uuid.UUID{}) && expected != uuid.UUID(row.Etag.Bytes) {
		return nil, uuid.UUID{}, ErrConflict
	}
	if _, err := q.ClearPricingPlanModelOverrides(ctx, pgID(id)); err != nil {
		return nil, uuid.UUID{}, err
	}
	for _, o := range inputs {
		if _, err := q.UpsertPricingPlanModelOverride(ctx, gwdb.UpsertPricingPlanModelOverrideParams{
			PricingPlanID: pgID(id), ModelID: pgID(o.ModelID),
			MultiplierBps: o.MultiplierBps,
		}); err != nil {
			return nil, uuid.UUID{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, uuid.UUID{}, err
	}
	if w.planInvalidator != nil {
		w.planInvalidator.InvalidatePlan(ctx, id)
	}
	return w.reader.PlanModelOverrides(ctx, id)
}
