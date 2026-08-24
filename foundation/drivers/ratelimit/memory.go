package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Memory is the in-process token bucket: it refills at limit per window and
// bursts up to limit. Exact within one process.
type Memory struct {
	mu      sync.Mutex
	buckets map[string]*memBucket
	calls   int
}

type memBucket struct {
	lim      *rate.Limiter
	limit    int
	window   time.Duration
	lastUsed time.Time
}

// Idle bucket sweep: every sweepEvery calls, reclaim buckets untouched for
// longer than idleTTL.
const (
	sweepEvery = 4096
	idleTTL    = time.Hour
)

func NewMemory() *Memory {
	return &Memory{buckets: make(map[string]*memBucket)}
}

func (m *Memory) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	return m.AllowN(ctx, key, 1, limit, window)
}

func (m *Memory) AllowN(_ context.Context, key string, n, limit int, window time.Duration) (Result, error) {
	if n <= 0 {
		n = 1
	}
	now := time.Now()

	m.mu.Lock()
	b, ok := m.buckets[key]
	if !ok || b.limit != limit || b.window != window {
		b = &memBucket{
			lim:    rate.NewLimiter(rate.Limit(float64(limit)/window.Seconds()), limit),
			limit:  limit,
			window: window,
		}
		m.buckets[key] = b
	}
	b.lastUsed = now
	m.calls++
	if m.calls%sweepEvery == 0 {
		for k, v := range m.buckets {
			if now.Sub(v.lastUsed) > idleTTL {
				delete(m.buckets, k)
			}
		}
	}
	m.mu.Unlock()

	res := Result{Limit: limit}
	// All or nothing. The bucket's capacity is limit, so an n above it can
	// never pass.
	if n <= limit && b.lim.AllowN(now, n) {
		res.Allowed = true
		res.Remaining = max(int(b.lim.Tokens()), 0)
		res.ResetAt = now.Add(window)
		return res, nil
	}
	// The suggested wait is how long n tokens take to refill.
	res.RetryAfter = time.Duration(n) * window / time.Duration(limit)
	res.ResetAt = now.Add(res.RetryAfter)
	return res, nil
}

var _ Limiter = (*Memory)(nil)
