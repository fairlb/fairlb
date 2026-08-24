package catalog_test

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Rounding up happens once, after everything is summed, never per bucket.
//
// A single-bucket rounding test cannot prove this: with one bucket, rounding
// per bucket and rounding after summing give the same answer. Only several
// buckets separate them -- when each bucket is worth less than one nano,
// rounding per bucket charges once per bucket, and the more buckets the larger
// the overcharge.
func TestChargeCeilsOnceAfterSummation(t *testing.T) {
	// One nano per million in each bucket and one token in each: 1e-6 nano per
	// bucket, 4e-6 nano in total. Rounding up once after summing gives 1;
	// rounding each bucket and adding gives 4.
	p := catalog.Price{
		InNanoPerMTok:         1,
		OutNanoPerMTok:        1,
		CacheReadNanoPerMTok:  1,
		CacheWriteNanoPerMTok: 1,
	}
	q, err := catalog.Compute(
		catalog.Flat(p), catalog.Flat(p),
		catalog.Tokens{In: 1, Out: 1, CachedRead: 1, CacheWrite: 1},
		catalog.Rates{
			ModelMultiplierBps: 10000, PlanMultiplierBps: 10000,
			ProcurementMultiplierBps: 10000, FXRate: "1",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if q.ChargedNano != 1 {
		t.Fatalf("with 1e-6 nano in each of the four buckets, one ceil over the sum should give 1, got %d"+
			"(a 4 means it rounds each bucket, so the more buckets the more it overcharges)", q.ChargedNano)
	}
}
