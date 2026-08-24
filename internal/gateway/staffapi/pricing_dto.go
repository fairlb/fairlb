package gwstaffapi

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/internal/gateway/pricing"
)

// Translation between the pricing domain and this contract's DTOs.
//
// This file is the whole reason the pricing service could move out of the
// handler package (ADR-0160/0173). Everything the DTO expresses that the domain
// does not -- optional-as-pointer, rates as decimal strings, versions as ETag
// headers -- is decided here, once.

func nanoRatesDTO(r pricing.TokenRates) *TokenRatesUSDPerM {
	return &TokenRatesUSDPerM{
		Input:      FormatNanoUSDPerM(r.Input),
		Output:     FormatNanoUSDPerM(r.Output),
		CacheRead:  FormatNanoUSDPerM(r.CacheRead),
		CacheWrite: FormatNanoUSDPerM(r.CacheWrite),
	}
}

func draftRatesDTO(r pricing.TokenRates) *DraftTokenRatesUSDPerM {
	input, output := FormatNanoUSDPerM(r.Input), FormatNanoUSDPerM(r.Output)
	read, write := FormatNanoUSDPerM(r.CacheRead), FormatNanoUSDPerM(r.CacheWrite)
	return &DraftTokenRatesUSDPerM{
		Input: &input, Output: &output, CacheRead: &read, CacheWrite: &write,
	}
}

func timePtr(v time.Time) *time.Time {
	if v.IsZero() {
		return nil
	}
	return &v
}

// modelPricingDTO renders a model's price as the API resource.
func modelPricingDTO(p pricing.ModelPricing) ModelPricingResource {
	out := ModelPricingResource{ModelId: p.ModelID, Priced: p.Priced}
	if !p.Priced {
		return out
	}
	mode := ModelPricingResourceBillingMode(p.BillingMode)
	out.BillingMode = &mode
	out.OfficialRates = draftRatesDTO(p.Official)
	out.Adjustment = &PricingAdjustment{MultiplierBps: int(p.MultiplierBps)}
	out.PublicRates = nanoRatesDTO(p.Published)
	out.SourceName, out.SourceUrl = textPtr(p.SourceName), textPtr(p.SourceURL)
	out.CheckedAt, out.UpdatedAt = timePtr(p.CheckedAt), timePtr(p.UpdatedAt)
	out.Reason = textPtr(p.Reason)

	if len(p.DimensionRates) > 0 {
		items := make([]ModelPriceDimensionRate, 0, len(p.DimensionRates))
		for _, d := range p.DimensionRates {
			tier := ModelPriceDimensionRateServiceTier(d.ServiceTier)
			variant, minInput := d.Variant, d.MinInputTokens
			items = append(items, ModelPriceDimensionRate{
				Bucket:      dimensionBucketFromDB[string(d.Bucket)],
				ServiceTier: &tier, Variant: &variant, MinInputTokens: &minInput,
				RateUsdPerM: FormatNanoUSDPerM(d.NanoPerMTok),
			})
		}
		out.DimensionRates = &items
	}
	if len(p.ToolRates) > 0 {
		items := make([]ModelPriceToolRate, 0, len(p.ToolRates))
		for _, t := range p.ToolRates {
			items = append(items, ModelPriceToolRate{
				Tool: t.Tool, RateUsdPerCall: FormatNanoUSDPerM(t.NanoPerCall),
			})
		}
		out.ToolRates = &items
	}
	return out
}

// modelPricingETag renders the version as a validator.
//
// Two shapes, because the two versions are different kinds of thing: an
// unpriced model has no row and therefore no timestamp, and hashing the literal
// "unpriced" keeps its validator stable across requests while still changing
// the moment a price appears.
func modelPricingETag(p pricing.ModelPricing) string {
	if !p.Priced {
		return compositeETag(p.ModelID.String(), "unpriced")
	}
	return compositeETag(p.ModelID.String(), p.Version.String())
}

func planDTO(p pricing.Plan) PricingPlan {
	return PricingPlan{
		Id: p.ID, Slug: p.Slug, Name: p.Name,
		Description: textPtr(p.Description), IsDefault: p.IsDefault,
		Status: PricingPlanStatus(p.Status), OrgCount: p.OrgCount,
		DefaultAdjustment: &PricingAdjustment{MultiplierBps: int(p.DefaultMultiplierBps)},
		Reason:            textPtr(p.Reason),
		CreatedAt:         p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func planETag(version uuid.UUID) string {
	if version == (uuid.UUID{}) {
		return `"missing"`
	}
	return `"` + version.String() + `"`
}

func planOverridesDTO(items []pricing.PlanModelOverride) []PricingPlanModelOverride {
	out := make([]PricingPlanModelOverride, 0, len(items))
	for _, o := range items {
		out = append(out, PricingPlanModelOverride{
			ModelId: o.ModelID, ModelSlug: o.ModelSlug,
			Adjustment: PricingAdjustment{MultiplierBps: int(o.MultiplierBps)},
		})
	}
	return out
}

func orgPlanDTO(orgPublicID string, p pricing.OrgPlan) OrgPricingPlanAssignment {
	return OrgPricingPlanAssignment{
		OrgId: orgPublicID, PricingPlanId: p.PlanID,
		PricingPlanSlug: p.PlanSlug, PricingPlanName: p.PlanName,
		InheritedDefault: p.InheritedDefault,
	}
}

func orgPlanETag(orgPublicID string, p pricing.OrgPlan) string {
	return compositeETag(orgPublicID, p.Version.String())
}

// parseRateInput turns one editable rate from the wire into the domain's form.
//
// nil and "0" are different and stay different: nil means the field was not
// supplied and stores NULL, "0" means this component is free.
func parseRateInput(v *string) (pricing.RateInput, error) {
	if v == nil {
		return pricing.RateInput{}, nil
	}
	nano, err := parseConfigurableUSDPerMToNano(*v)
	if err != nil {
		return pricing.RateInput{}, fmt.Errorf("%w: %v", ErrPricingInvalid, err)
	}
	return pricing.RateInput{Nano: nano, Set: true}, nil
}

// modelPricingWriteFromDTO is the whole translation from this contract's input
// to the pricing domain's: decimal strings become nano, the contract's bucket
// names become the domain's, and the optional-with-default fields get their
// defaults here rather than inside the pricing rules.
func modelPricingWriteFromDTO(in ModelPricingInput) (pricing.ModelPricingWrite, error) {
	out := pricing.ModelPricingWrite{
		BillingMode:   pricing.BillingMode(in.BillingMode),
		MultiplierBps: int32(in.Adjustment.MultiplierBps),
		SourceName:    in.SourceName,
		SourceURL:     valueOr(in.SourceUrl, ""),
		Reason:        in.Reason,
	}
	var err error
	if out.Official.Input, err = parseRateInput(in.OfficialRates.Input); err != nil {
		return pricing.ModelPricingWrite{}, err
	}
	if out.Official.Output, err = parseRateInput(in.OfficialRates.Output); err != nil {
		return pricing.ModelPricingWrite{}, err
	}
	if out.Official.CacheRead, err = parseRateInput(in.OfficialRates.CacheRead); err != nil {
		return pricing.ModelPricingWrite{}, err
	}
	if out.Official.CacheWrite, err = parseRateInput(in.OfficialRates.CacheWrite); err != nil {
		return pricing.ModelPricingWrite{}, err
	}
	if in.AcknowledgedRisks != nil {
		for _, c := range *in.AcknowledgedRisks {
			out.AcknowledgedRisks = append(out.AcknowledgedRisks, string(c))
		}
	}
	if in.DimensionRates != nil {
		items := make([]pricing.DimensionRateInput, 0, len(*in.DimensionRates))
		for _, d := range *in.DimensionRates {
			nano, pErr := parseConfigurableUSDPerMToNano(d.RateUsdPerM)
			if pErr != nil {
				return pricing.ModelPricingWrite{}, fmt.Errorf("%w: %v", ErrPricingInvalid, pErr)
			}
			bucket, ok := dimensionBucketToDB[d.Bucket]
			if !ok {
				return pricing.ModelPricingWrite{}, fmt.Errorf(
					"%w: unknown pricing bucket %q", ErrPricingInvalid, d.Bucket)
			}
			items = append(items, pricing.DimensionRateInput{
				Bucket:         pricing.Bucket(bucket),
				ServiceTier:    string(valueOr(d.ServiceTier, Standard)),
				Variant:        valueOr(d.Variant, ""),
				MinInputTokens: valueOr(d.MinInputTokens, 0),
				NanoPerMTok:    nano,
			})
		}
		out.DimensionRates = &items
	}
	if in.ToolRates != nil {
		items := make([]pricing.ToolRateInput, 0, len(*in.ToolRates))
		for _, r := range *in.ToolRates {
			nano, pErr := parseConfigurableUSDPerMToNano(r.RateUsdPerCall)
			if pErr != nil {
				return pricing.ModelPricingWrite{}, fmt.Errorf("%w: %v", ErrPricingInvalid, pErr)
			}
			items = append(items, pricing.ToolRateInput{Tool: r.Tool, NanoPerCall: nano})
		}
		out.ToolRates = &items
	}
	return out, nil
}
