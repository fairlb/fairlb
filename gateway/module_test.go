package gateway

import (
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/jobs"
)

// The schedule is a product invariant, not a Cloud-only assembly detail. This
// test prevents a Community build from silently losing aggregation, partition
// creation or settlement recovery again.
func TestPeriodicJobsAreACompleteGatewaySet(t *testing.T) {
	if got := len(PeriodicJobs()); got != 8 {
		t.Fatalf("periodic gateway job count = %d, want 8", got)
	}
	want := map[string]time.Duration{
		"gateway_partition_maintain":   24 * time.Hour,
		"gateway_provider_probe":       5 * time.Minute,
		"gateway_usage_aggregate":      15 * time.Minute,
		"gateway_unsettled_retry":      2 * time.Minute,
		"gateway_revenue_recon":        24 * time.Hour,
		"gateway_spend_anomaly":        15 * time.Minute,
		"gateway_resource_affinity_gc": 24 * time.Hour,
		"gateway_route_probe_sweep":    time.Hour,
	}
	got := jobs.Schedules()
	for kind, interval := range want {
		if got[kind] != interval {
			t.Errorf("schedule %s = %s, want %s", kind, got[kind], interval)
		}
	}
}
