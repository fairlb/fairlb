package gwstaffapi

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/money"
)

const (
	pricingMultiplierMin = 1
	pricingMultiplierMax = 100_000
	// A public/customer rate composes at most a 10x model multiplier and a 10x
	// pricing-plan multiplier. Keeping the base at MaxInt64/100 makes every
	// configured preview exactly representable.
	maxConfigurableRateNano int64 = 92_233_720_368_547_758
)

// parseUSDPerMToNano converts the contract's exact decimal USD/M into the
// int64 nano USD/M stored in the database. It never goes through float64, so
// "0.000000001", very large values and trailing zeros all survive without
// rounding drift.
func parseUSDPerMToNano(value string) (int64, error) {
	if !money.IsPlainDecimal(value, 9) {
		return 0, fmt.Errorf("price %q must be a non-negative decimal string with at most 9 fractional digits", value)
	}
	whole, fraction, _ := strings.Cut(value, ".")
	fraction += strings.Repeat("0", 9-len(fraction))
	n := new(big.Int)
	if _, ok := n.SetString(whole, 10); !ok {
		return 0, fmt.Errorf("price %q is invalid", value)
	}
	n.Mul(n, big.NewInt(money.NanoPerUnit))
	if fraction != "" {
		f := new(big.Int)
		if _, ok := f.SetString(fraction, 10); !ok {
			return 0, fmt.Errorf("price %q is invalid", value)
		}
		n.Add(n, f)
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("price %q is outside the storable range", value)
	}
	return n.Int64(), nil
}

func parseConfigurableUSDPerMToNano(value string) (int64, error) {
	nano, err := parseUSDPerMToNano(value)
	if err != nil {
		return 0, err
	}
	if nano > maxConfigurableRateNano {
		return 0, fmt.Errorf("price %q is outside the configurable range (max 92233720.368547758 USD/M)", value)
	}
	return nano, nil
}

// FormatNanoUSDPerM is the canonical inverse of parseUSDPerMToNano: zero
// renders as "0" and everything else drops meaningless trailing zeros, so the
// storage adapter can assemble the public contract from it.
func FormatNanoUSDPerM(nano int64) string {
	if nano < 0 {
		panic("negative USD/M rate")
	}
	return money.FormatNanoExact(nano)
}

func validateAdjustment(v PricingAdjustment) error {
	if v.MultiplierBps < pricingMultiplierMin || v.MultiplierBps > pricingMultiplierMax {
		return httpx.ErrCodeDetail(errcode.CommonUnprocessable,
			fmt.Sprintf("multiplier_bps must be between %d and %d",
				pricingMultiplierMin, pricingMultiplierMax))
	}
	return nil
}

func validateDraftRates(r DraftTokenRatesUSDPerM) error {
	for name, value := range map[string]*string{
		"input": r.Input, "output": r.Output,
		"cache_read": r.CacheRead, "cache_write": r.CacheWrite,
	} {
		if value == nil {
			// A missing rate is allowed at this stage; the completeness check
			// is what refuses a paid model with a missing price.
			continue
		}
		if _, err := parseConfigurableUSDPerMToNano(*value); err != nil {
			return httpx.ErrCodeDetail(errcode.CommonValidation, name+": "+err.Error())
		}
	}
	return nil
}

// validateModelPricingInput validates one save.
//
// NULL is not 0. A paid model must supply all four rates, and "0" is a valid
// value meaning "this component is free", so a missing one is always refused.
// A model that is free as a whole is expressed only through billing_mode, never
// by setting all four rates to zero -- that would merge "deliberately free"
// back into the same state as "the price was never filled in".
func validateModelPricingInput(in *ModelPricingInput) error {
	if in == nil {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return httpx.ErrCodeDetail(errcode.CommonValidation,
			"reason is required: once published, the audit log is the only "+
				"record of who changed the price, when, and why")
	}
	if err := validateAdjustment(in.Adjustment); err != nil {
		return err
	}
	if err := validateDraftRates(in.OfficialRates); err != nil {
		return err
	}
	if in.BillingMode == ModelPricingInputBillingModePaid {
		for name, v := range map[string]*string{
			"input": in.OfficialRates.Input, "output": in.OfficialRates.Output,
			"cache_read": in.OfficialRates.CacheRead, "cache_write": in.OfficialRates.CacheWrite,
		} {
			if v == nil {
				return httpx.ErrCodeDetail(errcode.CommonValidation,
					"A paid model needs all four list prices; missing: "+name+
						" (use \"0\" to say something is free — leaving it out is not the same)")
			}
		}
	}
	if strings.TrimSpace(in.SourceName) == "" {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "source_name is required: every price must record where it came from")
	}
	return nil
}

func validatePricingReason(reason *string) error {
	if reason == nil {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "reason is required")
	}
	*reason = strings.TrimSpace(*reason)
	if *reason == "" {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "reason is required")
	}
	// OpenAPI maxLength counts Unicode code points. Keep a byte ceiling as well so
	// a future decoder/change cannot turn this audit field into an oversized log
	// payload (valid UTF-8 is at most four bytes per code point).
	if utf8.RuneCountInString(*reason) > 500 || len(*reason) > 2_000 {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "reason must be at most 500 characters")
	}
	return nil
}

func validateIfMatch(value string) error {
	if !strongETag(value) {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "If-Match must use the strong ETag from the most recent response")
	}
	return nil
}

func strongETag(value string) bool {
	return len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' &&
		!strings.Contains(value[1:len(value)-1], `"`) && value != `""`
}

func etagHeader(value string) *string {
	if !strongETag(value) {
		value = strconv.Quote(value)
	}
	return &value
}

func duplicateModelOverride(items []PricingPlanModelOverrideInput) bool {
	seen := make(map[uuid.UUID]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.ModelId]; ok {
			return true
		}
		seen[item.ModelId] = struct{}{}
	}
	return false
}
