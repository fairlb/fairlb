package pricing

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// ErrConflict is "someone else changed this since you read it".
var ErrConflict = errors.New("pricing: revision conflict")

// Invalidator is told which cached pricing became stale.
//
// Declared here and implemented at the assembly point: the catalog cache is
// downstream of a price change, and a domain that imported its cache to notify
// it would have the arrow pointing the wrong way.
type Invalidator interface {
	// InvalidateAll is for a change whose blast radius is every organization --
	// today, moving an organization between plans, because a plan carries
	// per-model overrides and the effective price of any model may have moved.
	InvalidateAll(ctx context.Context)
}

// ModelInvalidator narrows "a price was written" to the one model it touched.
//
// Separate from Invalidator because the two are satisfied by different things
// at the assembly point, and because writing one interface with both methods
// would force every caller to supply a whole-world invalidation it does not
// have a use for.
type ModelInvalidator interface {
	InvalidateModel(ctx context.Context, modelID uuid.UUID)
}

// Writer performs pricing writes.
type Writer struct {
	pool             *pgxpool.Pool
	q                *gwdb.Queries
	reader           *Reader
	invalidator      Invalidator
	modelInvalidator ModelInvalidator
	planInvalidator  PlanInvalidator
}

func NewWriter(
	pool *pgxpool.Pool, invalidator Invalidator,
	models ModelInvalidator, plans PlanInvalidator,
) *Writer {
	return &Writer{
		pool: pool, q: gwdb.New(pool), reader: NewReader(pool),
		invalidator: invalidator, modelInvalidator: models, planInvalidator: plans,
	}
}

// AssignOrgPlan moves an organization onto a plan, or back to inheriting the
// default.
//
// planID nil means inherit. The row still records a plan id -- the default in
// force at the time -- so the audit trail shows what was inherited rather than
// just that inheriting was chosen.
//
// expected is the version the caller believes it is replacing; the zero value
// means "no precondition". Comparing versions is the domain's job because it is
// the domain that knows what a version is; rendering one as an ETag is not.
func (w *Writer) AssignOrgPlan(
	ctx context.Context, orgID pgtype.UUID, planID *uuid.UUID,
	reason string, actor pgtype.UUID, expected uuid.UUID,
) (OrgPlan, error) {
	current, err := w.reader.OrgPlan(ctx, orgID)
	if err != nil {
		return OrgPlan{}, err
	}
	if expected != (uuid.UUID{}) && expected != current.Version {
		return OrgPlan{}, ErrConflict
	}

	inherit := planID == nil
	target := pgtype.UUID{}
	if planID != nil {
		target = pgID(*planID)
	}
	if !target.Valid {
		def, dErr := w.q.GetEffectivePricingPlanForOrg(ctx, orgID)
		if dErr != nil {
			return OrgPlan{}, dErr
		}
		target = def.PricingPlanID
	}
	if _, err := w.q.UpsertOrgPricingPlanAssignment(ctx, gwdb.UpsertOrgPricingPlanAssignmentParams{
		OrgID: orgID, PricingPlanID: target,
		InheritDefault: inherit, Reason: reason, UpdatedBy: actor,
	}); err != nil {
		return OrgPlan{}, err
	}
	if w.invalidator != nil {
		w.invalidator.InvalidateAll(ctx)
	}
	return w.reader.OrgPlan(ctx, orgID)
}
