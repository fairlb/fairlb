package proxy

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Candidate selection and the retry budget.

// PriorityWeighted groups candidates by priority and shuffles within a group by
// weight. It is the only candidate ordering; lowest-cost and latency-aware
// orderings would need latency samples this deployment does not collect, and an
// interface with one implementation and no injection point is a sketch, not a
// seam (ADR-0155).
//
// Grouping is what makes priority a hard constraint and weight a soft
// allocation: no candidate at a higher priority number is touched until every
// candidate at a lower one is unusable -- and the higher-numbered ones are
// typically the expensive standby providers.
type PriorityWeighted struct {
	rnd *rand.Rand
}

func NewPriorityWeighted() *PriorityWeighted {
	//nolint:gosec // load distribution only; cryptographic randomness is not needed
	return &PriorityWeighted{rnd: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))}
}

func (s *PriorityWeighted) Order(_ context.Context, candidates []catalog.Route) []catalog.Route {
	if len(candidates) <= 1 {
		return candidates
	}
	// Group by priority; the catalog query already returns them in ascending
	// priority order.
	var out []catalog.Route
	i := 0
	for i < len(candidates) {
		j := i
		for j < len(candidates) && candidates[j].Priority == candidates[i].Priority {
			j++
		}
		out = append(out, s.weightedShuffle(candidates[i:j])...)
		i = j
	}
	return out
}

// weightedShuffle orders a group at random, weighted: a higher weight is more
// likely to come first, but a low-weight provider still gets its turn. This is
// weighted allocation, not primary-and-standby failover.
func (s *PriorityWeighted) weightedShuffle(group []catalog.Route) []catalog.Route {
	if len(group) <= 1 {
		return group
	}
	pool := make([]catalog.Route, len(group))
	copy(pool, group)
	out := make([]catalog.Route, 0, len(pool))

	for len(pool) > 0 {
		total := 0
		for _, r := range pool {
			w := int(r.Weight)
			if w < 1 {
				w = 1 // a weight of 0 does not mean never chosen, only lowest
			}
			total += w
		}
		pick := s.rnd.IntN(total)
		idx := 0
		for i, r := range pool {
			w := int(r.Weight)
			if w < 1 {
				w = 1
			}
			if pick < w {
				idx = i
				break
			}
			pick -= w
		}
		out = append(out, pool[idx])
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}

// maxRouteAttempts caps how many routes one request may try.
const maxRouteAttempts = 3

// RetryBudget caps the global retry rate so a retry storm cannot form. If every
// request retried three times through an upstream wobble, the recovering
// upstream would be knocked over again by triple the traffic.
type RetryBudget struct {
	requests atomic.Int64
	retries  atomic.Int64
	// maxRatio bounds retries as a share of all requests; 0.1 is 10%.
	maxRatio float64
	// Below minSamples nothing is limited: a ratio over too few samples means
	// nothing.
	minSamples int64
}

func NewRetryBudget() *RetryBudget {
	return &RetryBudget{maxRatio: 0.1, minSamples: 100}
}

// StartRequest counts one request, retried or not.
func (b *RetryBudget) StartRequest() { b.requests.Add(1) }

// AllowRetry reports whether any retry budget is left.
func (b *RetryBudget) AllowRetry() bool {
	reqs := b.requests.Load()
	if reqs < b.minSamples {
		return true
	}
	if float64(b.retries.Load()+1) > float64(reqs)*b.maxRatio {
		return false
	}
	b.retries.Add(1)
	return true
}

// Stats exposes current budget usage for the health dashboard.
func (b *RetryBudget) Stats() (requests, retries int64) {
	return b.requests.Load(), b.retries.Load()
}

// Semaphore is the per-provider concurrency limit that provides backpressure.
// At capacity it refuses immediately rather than queueing: queueing only pushes
// the latency onto the client, which already has a timeout of its own and may
// well have given up by the time its turn comes.
type Semaphore struct {
	slots chan struct{}
	// capacity is kept so a change to the provider's configured limit can be
	// noticed: a channel cannot be resized, so the semaphore is replaced.
	capacity int
}

func NewSemaphore(n int) *Semaphore {
	if n <= 0 {
		return nil // unlimited
	}
	return &Semaphore{slots: make(chan struct{}, n), capacity: n}
}

// Capacity is how many slots this semaphore was built with; zero for the nil
// (unlimited) semaphore.
func (s *Semaphore) Capacity() int {
	if s == nil {
		return 0
	}
	return s.capacity
}

// TryAcquire takes a slot without blocking.
func (s *Semaphore) TryAcquire() bool {
	if s == nil {
		return true
	}
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Semaphore) Release() {
	if s == nil {
		return
	}
	select {
	case <-s.slots:
	default:
	}
}
