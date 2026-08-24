package catalog_test

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// byokPrice: $3 per million input, $15 per million output, in nano.
var byokPrice = catalog.Price{
	InNanoPerMTok:         3_000_000_000,
	OutNanoPerMTok:        15_000_000_000,
	CacheReadNanoPerMTok:  300_000_000,
	CacheWriteNanoPerMTok: 3_750_000_000,
}

// The hand-computed base shared by every case below:
//
//	in  1000 tok × 3e9  / 1e6 =  3_000_000 nano
//	out  500 tok × 15e9 / 1e6 =  7_500_000 nano
//	                    total = 10_500_000 nano ($0.0105)
const byokBaseCost = 10_500_000

var byokTokens = catalog.Tokens{In: 1000, Out: 500}

// With the organization's own credential, the charge is the upstream cost times the
// fee rate times the exchange rate, and the recorded upstream cost is 0.
func TestComputeBYOK(t *testing.T) {
	tests := []struct {
		name        string
		feeBps      int64
		fx          string
		wantCharged int64 // computed by hand, not derived from the formula
	}{
		// 10_500_000 × 500/10000 = 525_000
		{"default 500bps (5%)", 500, "1", 525_000},
		// 10_500_000 × 1000/10000 = 1_050_000
		{"1000bps (10%)", 1000, "1", 1_050_000},
		// 10_500_000 × 500/10000 × 7.2 = 3_780_000
		{"500bps + a CNY rate of 7.2", 500, "7.2", 3_780_000},
		// A fee of 0 makes it entirely free, which is a configuration an
		// operator is allowed to choose.
		{"0bps", 0, "1", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := catalog.ComputeBYOK(catalog.Flat(byokPrice), byokTokens, tc.feeBps, catalog.Rates{FXRate: tc.fx})
			if err != nil {
				t.Fatalf("ComputeBYOK: %v", err)
			}
			if q.ChargedNano != tc.wantCharged {
				t.Errorf("charged = %d, want %d", q.ChargedNano, tc.wantCharged)
			}
			// The recorded upstream cost is always 0: that money went from
			// the organization to the upstream and was never a cost here.
			// Recording it truthfully would turn margin -- charges minus
			// costs -- into "service fee minus the full upstream bill",
			// deeply negative.
			if q.UpstreamUSDNano != 0 {
				t.Errorf("upstream_cost must be 0 for BYOK, got %d", q.UpstreamUSDNano)
			}
			if q.FXRate != tc.fx {
				t.Errorf("FXRate = %q, want %q", q.FXRate, tc.fx)
			}
		})
	}
}

// For identical usage, the organization-credential path charges only the service fee
// while the platform-credential path charges the full rate plus markup. The two
// must never collapse into the same number.
func TestBYOKChargesLessThanPlatform(t *testing.T) {
	byokQ, err := catalog.ComputeBYOK(catalog.Flat(byokPrice), byokTokens, 500, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	// Platform path at 2000 basis points: 10_500_000 x 1.2 = 12_600_000, by
	// hand.
	platQ, err := catalog.Compute(catalog.Flat(byokPrice), catalog.Flat(byokPrice), byokTokens, catalog.Rates{ModelMultiplierBps: 12000, FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if platQ.ChargedNano != 12_600_000 {
		t.Fatalf("the hand-computed platform baseline does not match: %d, want 12600000", platQ.ChargedNano)
	}
	if byokQ.ChargedNano != 525_000 {
		t.Fatalf("the hand-computed BYOK baseline does not match: %d, want 525000", byokQ.ChargedNano)
	}
	if platQ.UpstreamUSDNano != byokBaseCost {
		t.Errorf("the platform tier should record the upstream cost as %d, got %d", byokBaseCost, platQ.UpstreamUSDNano)
	}
}

// Reconciliation: with a non-zero fee, the charge must be greater than zero, or
// the reverse reconciliation reports it as "succeeded and charged nothing" --
// a configuration incident.
//
// This case pins the behaviour when the fee is non-zero. A fee configured as 0
// is an explicit operator choice, and having reconciliation flag that is
// correct: somebody should know the service is being given away.
func TestBYOKNonZeroChargeAvoidsReconAlert(t *testing.T) {
	// The smallest possible usage: one input token. An implementation that
	// truncates after applying the fee collapses to zero here first.
	tiny := catalog.Tokens{In: 1}
	q, err := catalog.ComputeBYOK(catalog.Flat(byokPrice), tiny, 500, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	// 1 tok × 3e9 / 1e6 = 3000 nano; × 5% = 150 nano
	if q.ChargedNano != 150 {
		t.Fatalf("charged = %d, want 150", q.ChargedNano)
	}
	if q.ChargedNano <= 0 {
		t.Fatal("a BYOK request charged down to zero would be misreported by reverse reconciliation as a configuration incident")
	}

	// More extreme: when the exact value is below one nano it must round up to
	// 1, never truncate to 0.
	q2, err := catalog.ComputeBYOK(catalog.Flat(catalog.Price{InNanoPerMTok: 1}), catalog.Tokens{In: 1}, 1, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	// 1 x 1/1e6 = 1e-6 nano, times 1/10000: far below 1, so rounding up gives
	// 1.
	if q2.ChargedNano != 1 {
		t.Errorf("a BYOK service fee below 1 nano should round up to 1 (the same rule Compute uses), got %d", q2.ChargedNano)
	}
}

// Parameter validation matches Compute: a missing exchange rate is always an
// error, never an implicit 1.
func TestComputeBYOKRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		tok    catalog.Tokens
		feeBps int64
		fx     string
	}{
		{"empty FX rate", byokTokens, 500, ""},
		{"zero FX rate", byokTokens, 500, "0"},
		{"invalid FX rate", byokTokens, 500, "abc"},
		{"negative fee rate", byokTokens, -1, "1"},
		{"negative token count", catalog.Tokens{In: -1}, 500, "1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := catalog.ComputeBYOK(catalog.Flat(byokPrice), tc.tok, tc.feeBps, catalog.Rates{FXRate: tc.fx}); err == nil {
				t.Error("an error was expected, got nil")
			}
		})
	}
}
