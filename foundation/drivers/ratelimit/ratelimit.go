// Package ratelimit is the rate-limiting driver: per-key request rates and
// per-IP limits on authentication endpoints.
package ratelimit

import (
	"context"
	"time"
)

// Result carries the decision plus everything the X-RateLimit-* and Retry-After
// response headers need.
type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAt    time.Time     // when the quota refills
	RetryAfter time.Duration // suggested wait when refused; zero when allowed
}

// Limiter decides whether a request under this key is allowed, given limit per
// window. Implementations must be safe for concurrent use, and a change to a
// key's parameters takes effect immediately.
type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error)
	// AllowN consumes n units at once, for limits measured by volume rather
	// than by request count — for example a token-per-minute limit, where one
	// request costs its estimated token count. An n of zero or less counts as
	// one.
	//
	// When the quota is insufficient nothing is consumed: it is all or nothing.
	// Partial consumption would let one large request eat half the quota and
	// still be refused, leaving the remaining allowance inexplicable to the
	// small requests that follow.
	//
	// A request with n greater than limit can never pass; the caller should
	// refuse it earlier with a clearer error.
	AllowN(ctx context.Context, key string, n, limit int, window time.Duration) (Result, error)
}
