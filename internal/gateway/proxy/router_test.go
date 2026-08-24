package proxy_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

func route(priority, weight int32, slug string) catalog.Route {
	var id pgtype.UUID
	copy(id.Bytes[:], slug)
	id.Valid = true
	return catalog.Route{ID: id, ProviderSlug: slug, Priority: priority, Weight: weight}
}

// Priority is a hard constraint: every candidate in a lower-priority group must
// come after every candidate in a higher-priority one. Otherwise an expensive
// standby provider gets chosen at random while the primary is healthy.
func TestPriorityIsHardConstraint(t *testing.T) {
	s := proxy.NewPriorityWeighted()
	in := []catalog.Route{
		route(10, 1, "p10-a"), route(10, 1, "p10-b"),
		route(20, 100, "p20-heavy"), // no weight is high enough to jump priority
	}
	for range 20 {
		out := s.Order(context.Background(), in)
		if len(out) != 3 {
			t.Fatalf("the candidate count must not change: %d", len(out))
		}
		if out[2].ProviderSlug != "p20-heavy" {
			t.Fatalf("the lower priority must come last: %v", slugs(out))
		}
	}
}

// Within a group the order is weighted-random: a higher weight comes first more
// often, but a low weight is never simply excluded. This is weighted
// allocation, not primary-and-standby failover.
func TestWeightedShuffleDistribution(t *testing.T) {
	s := proxy.NewPriorityWeighted()
	in := []catalog.Route{route(10, 9, "heavy"), route(10, 1, "light")}

	heavyFirst := 0
	const runs = 400
	for range runs {
		if s.Order(context.Background(), in)[0].ProviderSlug == "heavy" {
			heavyFirst++
		}
	}
	// Around 90% is expected; the margin here is generous on purpose. What
	// matters is that neither end is zero.
	if heavyFirst < runs/2 {
		t.Errorf("the heavier weight should come first more often: %d/%d", heavyFirst, runs)
	}
	if heavyFirst == runs {
		t.Error("a low weight must not be excluded outright -- that would be standby failover, not weighted allocation")
	}
}

func slugs(rs []catalog.Route) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ProviderSlug
	}
	return out
}

// The retry budget: no limiting until there are enough samples, then refusal
// past the ratio, which is what keeps a retry storm from forming.
func TestRetryBudget(t *testing.T) {
	b := proxy.NewRetryBudget()

	// Below the sample threshold everything is allowed: a ratio over few
	// samples means nothing.
	for range 50 {
		b.StartRequest()
		if !b.AllowRetry() {
			t.Fatal("retries must not be limited before there are enough samples")
		}
	}

	// With enough samples, a retry rate above 10% is refused.
	for range 200 {
		b.StartRequest()
	}
	allowed, denied := 0, 0
	for range 100 {
		if b.AllowRetry() {
			allowed++
		} else {
			denied++
		}
	}
	if denied == 0 {
		t.Error("past the budget, retries should start being refused")
	}
	reqs, retries := b.Stats()
	if float64(retries) > float64(reqs)*0.15 {
		t.Errorf("the retry rate must not run well past the cap: %d/%d", retries, reqs)
	}
}

// Backpressure: at capacity, refuse rather than queue. Queueing only pushes the
// latency onto the client, which already has a timeout of its own and may have
// given up by the time its turn comes.
func TestSemaphoreBackpressure(t *testing.T) {
	sem := proxy.NewSemaphore(2)
	// Written as a loop on purpose: with || the second call would be
	// short-circuited away on the first success, and "take both slots" would
	// never actually be exercised.
	for i := range 2 {
		if !sem.TryAcquire() {
			t.Fatalf("acquisition %d should succeed while within capacity", i+1)
		}
	}
	if sem.TryAcquire() {
		t.Fatal("at capacity it should refuse immediately")
	}
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("after a release it should be acquirable again")
	}

	// n <= 0 means unlimited concurrency.
	unlimited := proxy.NewSemaphore(0)
	for range 100 {
		if !unlimited.TryAcquire() {
			t.Fatal("unlimited concurrency must never refuse")
		}
	}
}
