package usage_test

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/usage"
)

// Latency quantiles. Every assertion compares against a hand-computed expected
// value, never against a second implementation: when both sides come from the
// same source, equality proves nothing about correctness.

var bounds = []int64{100, 250, 500, 1000, 2500, 5000, 10000}

func hist(cum []int64, total, durSum int64) usage.LatencyHistogram {
	return usage.LatencyHistogram{Bounds: bounds, Cumulative: cum, Total: total, DurationMs: durSum}
}

// With every sample in one bucket, interpolate uniformly inside it.
//
// The histogram only tells us "all of them were <= 100ms"; the distribution
// inside the bucket is unknown. Taking the upper bound (100) is the
// conservative estimate, interpolating (p50 -> 50, p95 -> 95) is the unbiased
// one. The latter is what Prometheus's histogram_quantile does and what is
// chosen here: someone reading p50 wants to know how fast a typical request is,
// and always reporting the bound makes every period look equally slow.
func TestQuantileSingleBucket(t *testing.T) {
	// 100 requests, all <= 100ms, 5000ms of total duration.
	h := hist([]int64{100, 100, 100, 100, 100, 100, 100}, 100, 100*50)
	s := h.Stats()
	if !s.HasSamples {
		t.Fatal("expected samples")
	}
	// By hand: p50 target=50 -> 0 + (100-0)*50/100 = 50; p95 target=95 -> 95.
	if s.P50 != 50 || s.P95 != 95 {
		t.Errorf("uniform in-bucket interpolation should give p50=50 p95=95, got p50=%d p95=%d", s.P50, s.P95)
	}
	if s.Mean != 50 {
		t.Errorf("mean should be 5000/100 = 50, got %d", s.Mean)
	}
	if s.P95Unbounded {
		t.Error("p95 is within the bounds and must not be marked unbounded")
	}
}

// Interpolation across buckets. By hand:
//
//	le_100=10, le_250=10, le_500=60, ... total=100
//	p50 -> target = ceil(0.5*100) = 50, landing in (250,500] (cumulative 60)
//	that bucket holds 60-10 = 50 samples, and we want its 50-10 = 40th
//	-> 250 + (500-250) * 40/50 = 250 + 200 = 450
func TestQuantileInterpolates(t *testing.T) {
	h := hist([]int64{10, 10, 60, 90, 100, 100, 100}, 100, 0)
	s := h.Stats()
	if s.P50 != 450 {
		t.Errorf("p50 computed by hand is 450, got %d", s.P50)
	}
	// p95 -> target = 95, landing in (1000,2500] (cumulative 100)
	// that bucket holds 100-90 = 10 samples, and we want its 95-90 = 5th
	// -> 1000 + 1500 * 5/10 = 1750
	if s.P95 != 1750 {
		t.Errorf("p95 computed by hand is 1750, got %d", s.P95)
	}
}

// A quantile beyond the largest bound must be flagged, never reported as an
// invented number.
func TestQuantileUnbounded(t *testing.T) {
	// Of 100 requests, 90 are <= 10s and 10 are slower, so p95 is in +Inf.
	h := hist([]int64{0, 0, 0, 0, 50, 80, 90}, 100, 0)
	s := h.Stats()
	if !s.P95Unbounded {
		t.Error("a p95 beyond the largest bound must be flagged, or the reader takes the bound for the real value")
	}
	if s.P95 != 10000 {
		t.Errorf("an unbounded result should return the largest bound 10000, got %d", s.P95)
	}
	if s.P50 == 0 {
		t.Error("p50 should still be computable")
	}
}

// Rows written before latency was recorded: a request count with an all-zero
// histogram. That must report "no samples", not 0ms.
func TestNoSamplesIsNotZeroLatency(t *testing.T) {
	h := hist([]int64{0, 0, 0, 0, 0, 0, 0}, 500, 0)
	s := h.Stats()
	if s.HasSamples {
		t.Error("an all-zero histogram means no latency samples and must not read as data")
	}
	if s.P50 != 0 || s.P95 != 0 {
		t.Errorf("no quantile should be reported without samples, got p50=%d p95=%d", s.P50, s.P95)
	}
}

// No requests at all.
func TestEmptyHistogram(t *testing.T) {
	if s := hist([]int64{0, 0, 0, 0, 0, 0, 0}, 0, 0).Stats(); s.HasSamples {
		t.Error("zero requests must not report having samples")
	}
}

// A single sample: both p50 and p95 land in its bucket.
func TestSingleSample(t *testing.T) {
	// One request, with a duration in (500,1000].
	h := hist([]int64{0, 0, 0, 1, 1, 1, 1}, 1, 800)
	s := h.Stats()
	if s.P50 != 1000 || s.P95 != 1000 {
		t.Errorf("with a single sample the quantile is its bucket bound 1000, got p50=%d p95=%d", s.P50, s.P95)
	}
	if s.Mean != 800 {
		t.Errorf("mean should be 800, got %d", s.Mean)
	}
}

// Cumulative counts must be non-decreasing -- that is the histogram's
// invariant, and a violation means the aggregation wrote something wrong.
// What this checks is that the algorithm does not produce absurd output on
// malformed input, not that malformed input is acceptable.
func TestNonMonotonicDoesNotPanic(t *testing.T) {
	h := hist([]int64{50, 10, 60, 90, 100, 100, 100}, 100, 0)
	if s := h.Stats(); s.P50 < 0 {
		t.Errorf("a quantile must not go negative on malformed input, got %d", s.P50)
	}
}

// ===== Full terminal bucket set =====

var wideBounds = []int64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000}

func wideHist(cum []int64, total, durSum int64) usage.LatencyHistogram {
	return usage.LatencyHistogram{Bounds: wideBounds, Cumulative: cum, Total: total, DurationMs: durSum}
}

func TestFullResolutionUsesUpperBuckets(t *testing.T) {
	// 100 samples: 60 are <= 10s, 90 are <= 30s, 100 are <= 60s.
	cum := []int64{0, 0, 0, 0, 30, 45, 60, 90, 100, 100}
	s := wideHist(cum, 100, 100*8000).Stats()
	if s.P95Unbounded {
		t.Error("every sample is within the bounds and must not be marked unbounded")
	}
	// By hand: target=95 lands in (30000, 60000], the bucket holds 10, pos=5
	//          -> 30000 + 30000*5/10 = 45000
	if s.P95 != 45000 {
		t.Errorf("p95 should be 45000, got %d", s.P95)
	}
}
