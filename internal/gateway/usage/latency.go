package usage

// Latency quantiles for the dashboard.
//
// The data source is the cumulative histogram in the rollups (the lat_le_*
// columns). Storing a p50/p95 per bucket and averaging them is not an option:
// the result is not the period's quantile, it is a number with no statistical
// meaning. Summing the histogram columns and interpolating afterwards is
// correct everywhere.

// LatencyHistogram is the merged cumulative count over a period.
//
// Cumulative[i] is the number of requests that took no longer than Bounds[i];
// Total is the number of requests overall, i.e. the +Inf bucket. So
// Total - Cumulative[last] is what fell beyond the largest bound.
type LatencyHistogram struct {
	Bounds     []int64
	Cumulative []int64
	Total      int64
	DurationMs int64 // total duration, for the mean
}

// LatencyStats is the set of numbers the console displays.
type LatencyStats struct {
	P50, P95 int64
	Mean     int64
	// P95Unbounded means p95 fell beyond the largest bound. P95 then holds that
	// bound and only supports the claim "at least this much". Without the flag,
	// a reader takes the bound for the real value.
	P95Unbounded bool
	// HasSamples false means the period has no successfully served requests.
	// That is different from "latency was zero" and must not draw a spurious
	// 0ms line on the chart.
	HasSamples bool
}

// Stats computes p50, p95 and the mean.
func (h LatencyHistogram) Stats() LatencyStats {
	// An all-zero histogram means the window has no served-request sample.
	// Report "no data" rather than 0ms.
	if h.Total == 0 || len(h.Cumulative) == 0 || h.Cumulative[len(h.Cumulative)-1] == 0 {
		return LatencyStats{}
	}
	s := LatencyStats{HasSamples: true}
	s.P50, _ = h.quantile(0.50)
	var within bool
	s.P95, within = h.quantile(0.95)
	s.P95Unbounded = !within
	s.Mean = h.DurationMs / h.Total
	return s
}

// quantile interpolates linearly over the cumulative counts. within false means
// the quantile fell beyond the largest bound.
//
// It targets the bucket holding the ceil(p*N)-th sample. ceil rather than round
// puts p50 on the "at least half the samples are no larger than this" side for
// small sample counts, matching the definition of a quantile.
func (h LatencyHistogram) quantile(p float64) (value int64, within bool) {
	target := int64(float64(h.Total)*p + 0.999999) // ceil, avoiding a float edge
	if target < 1 {
		target = 1
	}
	// Walk only as far as both slices have values. Bounds and Cumulative are
	// supposed to be the same length, but a mismatch must not panic the read
	// path: this feeds a dashboard, and an index out of range replaces the whole
	// page with an error when all it really means is "a few buckets are
	// missing". Extending the bucket set actually triggered this once, with ten
	// bounds and callers still passing seven cumulative values.
	n := min(len(h.Bounds), len(h.Cumulative))
	var prevCount, prevBound int64
	for i := range n {
		bound := h.Bounds[i]
		count := h.Cumulative[i]
		if count >= target {
			// The target lands in (prevBound, bound]; interpolate linearly
			// over the samples inside that bucket. With a single sample in
			// the bucket the interpolation degenerates to the upper bound,
			// which is the correct conservative estimate.
			inBucket := count - prevCount
			if inBucket <= 0 {
				return bound, true
			}
			pos := target - prevCount
			span := bound - prevBound
			return prevBound + span*pos/inBucket, true
		}
		prevCount, prevBound = count, bound
	}
	// It fell in the +Inf bucket: all we know is "greater than the largest
	// bound". Return that bound and mark it as not converged -- reporting an
	// invented specific number is worse than reporting "at least 10s".
	// The bound returned is the last one actually walked, not the last entry in
	// Bounds: on a length mismatch that final bound has no count behind it, and
	// using it would claim one bucket more than we have.
	if n == 0 {
		return 0, false
	}
	return h.Bounds[n-1], false
}

// LatencyBounds exposes the bucket bounds so the query side can build a
// histogram. It returns a copy, so callers cannot mutate the definition -- it
// must correspond strictly to the rollup's lat_le_* columns.
func LatencyBounds() []int64 {
	out := make([]int64, len(latencyBounds))
	copy(out, latencyBounds)
	return out
}
