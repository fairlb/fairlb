package proxy

import (
	"math/big"

	"github.com/fairlb/fairlb/foundation/money"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// usageTokenRatesUSDPerM holds the effective USD price per million units as a
// decimal string. Multipliers can make nano-per-million land on a fractional
// nano, so it must not be squeezed back into an int64 or rounded up per bucket.
// The final charge is still rounded up exactly once, by catalog.Compute, after
// every bucket has been summed and the multipliers and exchange rate applied.
type usageTokenRatesUSDPerM struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read"`
	CacheWrite string `json:"cache_write"`
}

type usageEffectiveRateGroups struct {
	Official    usageTokenRatesUSDPerM
	Public      usageTokenRatesUSDPerM
	Customer    usageTokenRatesUSDPerM
	Procurement usageTokenRatesUSDPerM
}

func versionedEffectiveRates(a settleArgs) usageEffectiveRateGroups {
	return effectiveRateGroups(
		a.priceTable.Base(),
		map[bool]string{true: "free", false: "paid"}[a.modelPricing.IsFree()],
		a.pricing.rates.ModelMultiplierBps,
		a.pricing.rates.PlanMultiplierBps,
		a.pricing.ratesForRoute(a.route).ProcurementMultiplierBps,
		a.byok,
		a.pricing.byokFeeBps,
	)
}

func effectiveRateGroups(
	official catalog.Price,
	billingMode string,
	modelMultiplierBps, planMultiplierBps, procurementMultiplierBps int64,
	byok bool,
	byokFeeBps int64,
) usageEffectiveRateGroups {
	modelMultiplierBps = multiplierOrOne(modelMultiplierBps)
	planMultiplierBps = multiplierOrOne(planMultiplierBps)
	procurementMultiplierBps = multiplierOrOne(procurementMultiplierBps)

	out := usageEffectiveRateGroups{Official: ratesOf(official)}
	if billingMode != "free" {
		out.Public = multipliedRatesOf(official, modelMultiplierBps)
		if byok {
			out.Customer = multipliedRatesOf(official, byokFeeBps, planMultiplierBps)
		} else {
			out.Customer = multipliedRatesOf(official, modelMultiplierBps, planMultiplierBps)
		}
	} else {
		out.Public = zeroUsageTokenRates()
		out.Customer = zeroUsageTokenRates()
	}

	if byok {
		// The organization pays the upstream bill directly, so this deployment's
		// procurement cost is always zero.
		out.Procurement = zeroUsageTokenRates()
		return out
	}
	out.Procurement = procurementRatesOf(official, procurementMultiplierBps)
	return out
}

func ratesOf(p catalog.Price) usageTokenRatesUSDPerM {
	return usageTokenRatesUSDPerM{
		Input:      exactUSDPerM(p.InNanoPerMTok),
		Output:     exactUSDPerM(p.OutNanoPerMTok),
		CacheRead:  exactUSDPerM(p.CacheReadNanoPerMTok),
		CacheWrite: exactUSDPerM(p.CacheWriteNanoPerMTok),
	}
}

func multipliedRatesOf(p catalog.Price, multipliers ...int64) usageTokenRatesUSDPerM {
	return usageTokenRatesUSDPerM{
		Input:      exactUSDPerM(p.InNanoPerMTok, multipliers...),
		Output:     exactUSDPerM(p.OutNanoPerMTok, multipliers...),
		CacheRead:  exactUSDPerM(p.CacheReadNanoPerMTok, multipliers...),
		CacheWrite: exactUSDPerM(p.CacheWriteNanoPerMTok, multipliers...),
	}
}

func procurementRatesOf(official catalog.Price, multiplierBps int64) usageTokenRatesUSDPerM {
	return usageTokenRatesUSDPerM{
		Input:      exactUSDPerM(official.InNanoPerMTok, multiplierBps),
		Output:     exactUSDPerM(official.OutNanoPerMTok, multiplierBps),
		CacheRead:  exactUSDPerM(official.CacheReadNanoPerMTok, multiplierBps),
		CacheWrite: exactUSDPerM(official.CacheWriteNanoPerMTok, multiplierBps),
	}
}

func zeroUsageTokenRates() usageTokenRatesUSDPerM {
	return usageTokenRatesUSDPerM{Input: "0", Output: "0", CacheRead: "0", CacheWrite: "0"}
}

func multiplierOrOne(v int64) int64 {
	if v == 0 {
		return 10000
	}
	return v
}

// exactUSDPerM renders nano USD per million, with any number of basis-point
// multipliers applied, as a decimal string with no rounding. The denominator is
// always 10^(9+4N), so the result is always a terminating decimal; big.Int
// keeps a product of several multipliers from overflowing.
func exactUSDPerM(nano int64, multipliers ...int64) string {
	n := big.NewInt(nano)
	for _, multiplier := range multipliers {
		n.Mul(n, big.NewInt(multiplier))
	}
	return money.FormatScaled(n, 9+4*len(multipliers))
}
