package gwstaffapi

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/pricing"
)

// The write path for usage pricing. Saving is publishing: what used to be four
// steps -- store a draft, preview it, cut a release, activate it -- is one, and
// the version state machine went with them.
//
// Three things did not go, because they are ledger correctness rather than
// release machinery:
//
//   - NULL is not 0. A paid model must supply all four rates, and a model that
//     is free as a whole is expressed only through billing_mode.
//   - Risks are still computed. A blocker refuses; a warning has to be
//     acknowledged. Selling below cost is a legitimate business decision, but
//     it has to be an informed one.
//   - A reason is mandatory and the change is audited. The release record used
//     to double as the answer to "who changed this, when, and why".

func checkExpected(got, expected string) error {
	if expected != "" && got != expected {
		return ErrPricingConflict
	}
	return nil
}

func valueOr[T any](v *T, fallback T) T {
	if v == nil {
		return fallback
	}
	return *v
}

// The API and the database name the pricing buckets differently, and the
// difference is not cosmetic in either direction:
//
//   - the contract says input/output/audio_input/audio_output, matching the
//     four field names of official_rates in the same document;
//   - the column says in/out/audio_in/audio_out, matching the catalog.Bucket*
//     constants and the literals already written into every pricing_snapshot.
//
// What was missing was the translation between them. Without it a saved
// dimension rate for the input bucket fails the column's CHECK outright, and
// cache_read/cache_write only worked because those two names happen to coincide
// -- so the advanced pricing editor could store two of its six buckets. Both
// maps are total over their enum, and the round trip is asserted in the tests.
var (
	dimensionBucketToDB = map[ModelPriceDimensionRateBucket]string{
		Input:       "in",
		Output:      "out",
		CacheRead:   "cache_read",
		CacheWrite:  "cache_write",
		AudioInput:  "audio_in",
		AudioOutput: "audio_out",
	}
	dimensionBucketFromDB = map[string]ModelPriceDimensionRateBucket{
		"in":          Input,
		"out":         Output,
		"cache_read":  CacheRead,
		"cache_write": CacheWrite,
		"audio_in":    AudioInput,
		"audio_out":   AudioOutput,
	}
)

// SaveModelPricing writes the price, which takes effect on save.
//
// The precondition is compared here, for the same reason as the organization
// assignment: the validator is a hash, so an incoming `If-Match` cannot be
// turned back into a version. The transport renders the current state's
// validator, compares, and hands the domain the version it read; the domain
// checks that version again inside its own transaction.
func (s *pgPricingAdminService) SaveModelPricing(
	ctx context.Context, modelID uuid.UUID, in ModelPricingInput, expected string, actor pgtype.UUID,
) (ModelPricingResource, string, error) {
	write, err := modelPricingWriteFromDTO(in)
	if err != nil {
		return ModelPricingResource{}, "", err
	}
	write.Actor = actor
	// The editor's contract makes checked_at mandatory: filling this form in is
	// the act of a person checking the vendor's price.
	write.VerifiedAt = pgtype.Timestamptz{Time: in.CheckedAt, Valid: true}
	write.Provenance = []byte(`{"maintenance":"manual"}`)
	if expected != "" {
		current, rErr := s.reader.ModelPricing(ctx, modelID)
		if rErr != nil {
			return ModelPricingResource{}, "", mapPricingDomainError(rErr)
		}
		if err := checkExpected(modelPricingETag(current), expected); err != nil {
			return ModelPricingResource{}, "", err
		}
		version := current.Version
		write.Expected = &version
	}
	return s.saveModelPricing(ctx, modelID, in, write)
}

// saveModelPricing validates the contract-level rules and hands the write to
// the domain. `in` is still needed for the validation that is about this
// contract's shape rather than about pricing.
func (s *pgPricingAdminService) saveModelPricing(
	ctx context.Context, modelID uuid.UUID, in ModelPricingInput, write pricing.ModelPricingWrite,
) (ModelPricingResource, string, error) {
	if err := validateModelPricingInput(&in); err != nil {
		return ModelPricingResource{}, "", err
	}
	saved, err := s.writer.SaveModelPricing(ctx, modelID, write)
	if err != nil {
		return ModelPricingResource{}, "", mapPricingDomainError(err)
	}
	if write.DryRun {
		return ModelPricingResource{}, "", nil
	}
	return modelPricingDTO(saved), modelPricingETag(saved), nil
}

// The plan CRUD below is transport: it validates what this contract says about
// shape, turns the strong ETag into a version, and maps the result back to DTOs.
// The rules live in internal/gateway/pricing (ADR-0174).

// CreatePricingPlan creates a pricing plan. The default adjustment lives on the
// plan itself.
func (s *pgPricingAdminService) CreatePricingPlan(
	ctx context.Context, in PricingPlanCreateInput, _ pgtype.UUID,
) (PricingPlan, string, error) {
	if err := validateAdjustment(in.DefaultAdjustment); err != nil {
		return PricingPlan{}, "", err
	}
	plan, err := s.writer.CreatePlan(ctx, pricing.PlanCreate{
		Slug: in.Slug, Name: in.Name, Description: valueOr(in.Description, ""),
		DefaultMultiplierBps: int32(in.DefaultAdjustment.MultiplierBps),
	})
	if err != nil {
		return PricingPlan{}, "", mapPricingDomainError(err)
	}
	return planDTO(plan), planETag(plan.Version), nil
}

func (s *pgPricingAdminService) UpdatePricingPlan(
	ctx context.Context, id uuid.UUID, in PricingPlanPatchInput, expected string, actor pgtype.UUID,
) (PricingPlan, string, error) {
	if in.DefaultAdjustment != nil {
		if err := validateAdjustment(*in.DefaultAdjustment); err != nil {
			return PricingPlan{}, "", err
		}
	}
	version, err := expectedPlanVersion(expected)
	if err != nil {
		return PricingPlan{}, "", err
	}
	patch := pricing.PlanPatch{Name: in.Name, Description: in.Description, Reason: valueOr(in.Reason, "")}
	if in.Status != nil {
		status := string(*in.Status)
		patch.Status = &status
	}
	if in.DefaultAdjustment != nil {
		bps := int32(in.DefaultAdjustment.MultiplierBps)
		patch.DefaultMultiplierBps = &bps
	}
	plan, err := s.writer.UpdatePlan(ctx, id, patch, version, actor)
	if err != nil {
		return PricingPlan{}, "", mapPricingDomainError(err)
	}
	return planDTO(plan), planETag(plan.Version), nil
}

func (s *pgPricingAdminService) DeletePricingPlan(
	ctx context.Context, id uuid.UUID, expected string, _ pgtype.UUID,
) error {
	// Delete requires a precondition rather than accepting an absent one: it is
	// the one operation with nothing to undo.
	version, err := parseStrongUUIDETag(expected)
	if err != nil {
		return err
	}
	return mapPricingDomainError(s.writer.DeletePlan(ctx, id, uuid.UUID(version.Bytes)))
}

// CopyPricingPlan duplicates a pricing plan, model overrides included.
func (s *pgPricingAdminService) CopyPricingPlan(
	ctx context.Context, sourceID uuid.UUID, in PricingPlanCopyInput, _ pgtype.UUID,
) (PricingPlan, string, error) {
	plan, err := s.writer.CopyPlan(ctx, sourceID, pricing.PlanCreate{
		Slug: in.Slug, Name: in.Name, Description: valueOr(in.Description, ""),
	})
	if err != nil {
		return PricingPlan{}, "", mapPricingDomainError(err)
	}
	return planDTO(plan), planETag(plan.Version), nil
}

// ReplacePricingPlanModelOverrides replaces the whole set of model overrides.
func (s *pgPricingAdminService) ReplacePricingPlanModelOverrides(
	ctx context.Context, id uuid.UUID, inputs []PricingPlanModelOverrideInput,
	expected string, _ pgtype.UUID,
) ([]PricingPlanModelOverride, string, error) {
	for _, o := range inputs {
		if err := validateAdjustment(o.Adjustment); err != nil {
			return nil, "", err
		}
	}
	version, err := expectedPlanVersion(expected)
	if err != nil {
		return nil, "", err
	}
	items := make([]pricing.PlanOverrideInput, 0, len(inputs))
	for _, o := range inputs {
		items = append(items, pricing.PlanOverrideInput{
			ModelID: o.ModelId, MultiplierBps: int32(o.Adjustment.MultiplierBps),
		})
	}
	saved, planVersion, err := s.writer.ReplacePlanOverrides(ctx, id, items, version)
	if err != nil {
		return nil, "", mapPricingDomainError(err)
	}
	return planOverridesDTO(saved), planETag(planVersion), nil
}

// expectedPlanVersion turns an optional If-Match into a version. Empty means no
// precondition, which is the zero uuid on the other side.
func expectedPlanVersion(expected string) (uuid.UUID, error) {
	if expected == "" {
		return uuid.UUID{}, nil
	}
	parsed, err := parseStrongUUIDETag(expected)
	if err != nil {
		return uuid.UUID{}, err
	}
	return uuid.UUID(parsed.Bytes), nil
}
