package gwstaffapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/pricing"
)

type pgPricingAdminService struct {
	pool        *pgxpool.Pool
	q           *gwdb.Queries
	reader      *pricing.Reader
	writer      *pricing.Writer
	catalog     *catalog.Service
	invalidator pricingInvalidator
}

// pricingInvalidator narrows "a price was written" down to the exact model or
// plan affected, rather than bumping a global generation counter and calling
// everything stale.
type pricingInvalidator interface {
	InvalidateModel(ctx context.Context, modelID uuid.UUID)
	InvalidatePlan(ctx context.Context, planID uuid.UUID)
}

type PGPricingAdminConfig struct {
	Pool    *pgxpool.Pool
	Catalog *catalog.Service
}

// NewPGPricingAdminService returns the one Postgres-backed implementation, in
// which saving is publishing: no versions and no post-construction mutation.
func NewPGPricingAdminService(cfg PGPricingAdminConfig) *pgPricingAdminService {
	svc := &pgPricingAdminService{
		pool: cfg.Pool, q: gwdb.New(cfg.Pool),
		reader: pricing.NewReader(cfg.Pool), catalog: cfg.Catalog,
	}
	// Both invalidators stay nil interfaces when there is no catalog. Assigning
	// `cfg.Catalog` into an interface unconditionally would make it non-nil
	// while holding a nil pointer, and the domain's `!= nil` guard would then
	// call a method on it.
	if cfg.Catalog != nil {
		svc.invalidator = catalogInvalidator{cat: cfg.Catalog, pool: cfg.Pool}
		svc.writer = pricing.NewWriter(cfg.Pool, cfg.Catalog, svc.invalidator, svc.invalidator)
	} else {
		svc.writer = pricing.NewWriter(cfg.Pool, nil, nil, nil)
	}
	return svc
}

// catalogInvalidator invalidates precisely, by model or by plan.
type catalogInvalidator struct {
	cat  *catalog.Service
	pool *pgxpool.Pool
}

func (c catalogInvalidator) InvalidateModel(ctx context.Context, modelID uuid.UUID) {
	var slug string
	// Failing to read the slug must not turn a successful write into a
	// failure: the database commit is the authority and the TTL is the
	// backstop.
	_ = c.pool.QueryRow(ctx, `SELECT slug FROM models WHERE id=$1`, modelID).Scan(&slug)
	if err := c.cat.InvalidateModelPricing(ctx, pgid(modelID), slug); err != nil {
		slog.WarnContext(ctx, "model price cache invalidation failed (the DB commit went through; the TTL is the fallback)",
			"model_id", modelID, "error", err)
	}
}

func (c catalogInvalidator) InvalidatePlan(ctx context.Context, planID uuid.UUID) {
	// A change to a plan affects the catalog view of every org bound to it, so
	// each of their cached identities has to be invalidated.
	rows, err := c.pool.Query(ctx, `SELECT org_id FROM org_pricing_plan_assignments
		WHERE pricing_plan_id=$1 AND NOT inherit_default`, planID)
	if err != nil {
		slog.WarnContext(ctx,
			"pricing plan cache invalidation failed (the DB commit went through; "+
				"the TTL is the fallback)",
			"plan_id", planID, "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var org pgtype.UUID
		if rows.Scan(&org) == nil {
			c.cat.InvalidateAll(ctx)
			return
		}
	}
}

func pgid(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func textPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func parseStrongUUIDETag(value string) (pgtype.UUID, error) {
	if !strongETag(value) {
		return pgtype.UUID{}, ErrPricingConflict
	}
	id, err := uuid.Parse(strings.Trim(value, `"`))
	if err != nil {
		return pgtype.UUID{}, ErrPricingConflict
	}
	return pgid(id), nil
}

func compositeETag(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf(`"%x"`, h[:16])
}

// The read side now lives in internal/gateway/pricing (ADR-0173). What remains
// here are thin wrappers: the write path reads its own result through them, and
// they exist so those call sites keep returning DTOs without each one repeating
// the mapping.

func (s *pgPricingAdminService) modelPricingResource(
	ctx context.Context, q *gwdb.Queries, modelID uuid.UUID,
) (ModelPricingResource, string, error) {
	p, err := s.reader.WithQueries(q).ModelPricing(ctx, modelID)
	if err != nil {
		return ModelPricingResource{}, "", mapPricingDomainError(err)
	}
	return modelPricingDTO(p), modelPricingETag(p), nil
}

func (s *pgPricingAdminService) GetModelPricing(
	ctx context.Context, modelID uuid.UUID,
) (ModelPricingResource, string, error) {
	return s.modelPricingResource(ctx, s.q, modelID)
}

func (s *pgPricingAdminService) ListPricingPlans(
	ctx context.Context, page cursorpage.KeyPage, search string,
) ([]PricingPlan, string, error) {
	plans, err := s.reader.Plans(ctx, page, search)
	if err != nil {
		return nil, "", mapPricingDomainError(err)
	}
	// The cursor is minted here, from the domain rows, rather than rebuilt in the
	// handler from the DTOs: the key is (is_default, slug), and a transport that
	// reassembles it is a second place that has to agree with the ORDER BY.
	kept, more := cursorpage.Trim(plans, int(page.Limit))
	out := make([]PricingPlan, 0, len(kept))
	for _, p := range kept {
		out = append(out, planDTO(p))
	}
	next := ""
	if more {
		next = pricing.PlanCursor(kept[len(kept)-1])
	}
	return out, next, nil
}

func (s *pgPricingAdminService) GetPricingPlan(
	ctx context.Context, id uuid.UUID,
) (PricingPlan, string, error) {
	plan, err := s.reader.Plan(ctx, id)
	if err != nil {
		return PricingPlan{}, "", mapPricingDomainError(err)
	}
	return planDTO(plan), planETag(plan.Version), nil
}

func (s *pgPricingAdminService) ListPricingPlanModelOverrides(
	ctx context.Context, id uuid.UUID,
) ([]PricingPlanModelOverride, string, error) {
	items, version, err := s.reader.PlanModelOverrides(ctx, id)
	if err != nil {
		return nil, "", mapPricingDomainError(err)
	}
	return planOverridesDTO(items), planETag(version), nil
}

func (s *pgPricingAdminService) GetOrgPricingPlan(
	ctx context.Context, orgPublicID string,
) (OrgPricingPlanAssignment, string, error) {
	orgID, err := publicid.Parse(publicid.Org, orgPublicID)
	if err != nil {
		return OrgPricingPlanAssignment{}, "", ErrPricingNotFound
	}
	plan, err := s.reader.OrgPlan(ctx, orgID)
	if err != nil {
		return OrgPricingPlanAssignment{}, "", mapPricingDomainError(err)
	}
	return orgPlanDTO(orgPublicID, plan), orgPlanETag(orgPublicID, plan), nil
}

// AssignOrgPricingPlan moves an organization onto a plan.
//
// The precondition is checked here, not in the domain, because the validator is
// a **hash** of the organization id and the assigned plan -- there is no way to
// turn an incoming `If-Match` back into a version. So the transport renders the
// current state's ETag, compares, and hands the domain the version it read.
// The domain checks that version again before writing, which is what closes the
// window between this read and that write.
func (s *pgPricingAdminService) AssignOrgPricingPlan(
	ctx context.Context, orgPublicID string, in OrgPricingPlanAssignmentInput,
	expected string, actor pgtype.UUID,
) (OrgPricingPlanAssignment, string, error) {
	orgID, err := publicid.Parse(publicid.Org, orgPublicID)
	if err != nil {
		return OrgPricingPlanAssignment{}, "", ErrPricingNotFound
	}
	current, err := s.reader.OrgPlan(ctx, orgID)
	if err != nil {
		return OrgPricingPlanAssignment{}, "", mapPricingDomainError(err)
	}
	if err := checkExpected(orgPlanETag(orgPublicID, current), expected); err != nil {
		return OrgPricingPlanAssignment{}, "", err
	}
	assigned, err := s.writer.AssignOrgPlan(
		ctx, orgID, in.PricingPlanId, in.Reason, actor, current.Version)
	if err != nil {
		return OrgPricingPlanAssignment{}, "", mapPricingDomainError(err)
	}
	return orgPlanDTO(orgPublicID, assigned), orgPlanETag(orgPublicID, assigned), nil
}
