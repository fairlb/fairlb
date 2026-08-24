package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the cross-instance limiter. It is a token bucket, the same algorithm
// the in-process implementation uses, because the two must be interchangeable.
//
// It was a fixed window (INCR plus EXPIRE) at first, and the obvious upgrade
// looked like a sliding window. It became a token bucket instead, for the reason
// the contract suite exists: the in-process side is a token bucket, and if the
// two disagree then changing the driver setting silently changes the rate
// limiting behavior. A sliding window is more accurate than a fixed one, but it
// still differs observably from a token bucket in burst shape and recovery
// curve, so it would not have lined up either.
//
// What was concretely wrong with the fixed window: at a window boundary it can
// admit twice the limit — the last limit requests of one window plus the first
// limit of the next — whereas a token bucket's burst ceiling is always limit.
//
// The decision and the state update happen atomically inside the script. Split
// across round trips, concurrent requests each read a stale token count and the
// overshoot grows with concurrency.
type Redis struct {
	rdb  *redis.Client
	take *redis.Script
}

// takeScript takes tokens from the bucket.
//
//	KEYS[1] = bucket key
//	ARGV[1] = limit (bucket capacity)
//	ARGV[2] = window in milliseconds (time to refill a whole bucket)
//	ARGV[3] = n (tokens this request needs)
//
// Returns {allowed, remaining, retry_after_ms}.
//
// The time comes from the server's own clock, not from a timestamp the caller
// passes in.
//
// Using the caller's clock is tempting, and older guidance discouraged
// non-deterministic commands in scripts because of how replication worked. That
// restriction is long gone — effects are replicated, not scripts — and the cost
// of the caller's clock is real: with several instances, clock skew miscomputes
// the quota. The bucket's timestamp is written unconditionally as the caller's
// now, so once an instance whose clock runs a minute fast writes a future
// timestamp, every other instance computes an elapsed time of zero and no tokens
// refill at all — for a full minute, for everyone. A single-instance deployment
// never shows it; the day a second instance appears, it does.
//
// RetryAfter stays a duration rather than an instant, so the caller converts it
// against its own clock and is unaffected either way.
//
// There is deliberately no local regression test for this. The two behaviors
// differ only when the caller's clock is itself skewed, and a test process and a
// local server share the host clock, so they produce identical results. A test
// was written and it did not fail against the broken version, so it was deleted:
// a test that can never go red is worse than no test, because it manufactures
// confidence.
const takeScript = `
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local n      = tonumber(ARGV[3])
-- Server clock: TIME returns {seconds, microseconds}
local t      = redis.call('TIME')
local now    = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local rate   = limit / window            -- tokens per millisecond

local st     = redis.call('HMGET', KEYS[1], 'tokens', 'ts', 'limit', 'window')
local tokens = tonumber(st[1])
local ts     = tonumber(st[2])
-- A parameter change rebuilds a full bucket, matching the in-process side,
-- which replaces its limiter on a change and starts it full. Carrying the old
-- token count across would break "raising the limit takes effect now": a key
-- that was just refused would still be refused.
if tokens == nil or tonumber(st[3]) ~= limit or tonumber(st[4]) ~= window then
  tokens = limit
  ts = now
end

-- Refill by elapsed time, capped at the bucket capacity
local elapsed = math.max(0, now - ts)
tokens = math.min(limit, tokens + elapsed * rate)

local allowed = 0
local retry = 0
if tokens >= n then
  tokens = tokens - n
  allowed = 1
else
  -- The suggested wait is how long the shortfall takes to refill; nothing is
  -- deducted, because it is all or nothing
  retry = math.ceil((n - tokens) / rate)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now, 'limit', limit, 'window', window)
-- The TTL is one window, exactly how long an empty bucket takes to refill, so
-- past that point "no key (a full bucket)" and the stored state are the same
-- thing and expiring early never loosens the limit
redis.call('PEXPIRE', KEYS[1], window)

return {allowed, math.floor(tokens), retry}
`

func NewRedis(rdb *redis.Client) *Redis {
	return &Redis{rdb: rdb, take: redis.NewScript(takeScript)}
}

func (r *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	return r.AllowN(ctx, key, 1, limit, window)
}

func (r *Redis) AllowN(ctx context.Context, key string, n, limit int, window time.Duration) (Result, error) {
	if n <= 0 {
		n = 1
	}
	now := time.Now()
	res := Result{Limit: limit}

	// An n above the limit can never pass, because the bucket's capacity is
	// the limit. Refuse it here without touching the server: otherwise a
	// permanently impossible request would keep refreshing the bucket's
	// timestamp for nothing.
	if n > limit {
		res.RetryAfter = time.Duration(n) * window / time.Duration(limit)
		res.ResetAt = now.Add(res.RetryAfter)
		return res, nil
	}

	windowMs := window.Milliseconds()
	if windowMs <= 0 {
		windowMs = 1 // a sub-millisecond window becomes 1ms, so the script cannot divide by zero
	}
	out, err := r.take.Run(ctx, r.rdb, []string{"flb:rl:" + key},
		limit, windowMs, n).Slice()
	if err != nil {
		return Result{}, err
	}
	if len(out) != 3 {
		return Result{}, errScriptResult
	}
	allowed, _ := out[0].(int64)
	remaining, _ := out[1].(int64)
	retryMs, _ := out[2].(int64)

	if allowed == 1 {
		res.Allowed = true
		res.Remaining = int(max(remaining, 0))
		res.ResetAt = now.Add(window)
		return res, nil
	}
	res.RetryAfter = time.Duration(retryMs) * time.Millisecond
	if res.RetryAfter <= 0 {
		// A zero from the script can only come from rounding. A refusal must
		// carry a usable wait, or the caller retries immediately and spins.
		res.RetryAfter = time.Millisecond
	}
	res.ResetAt = now.Add(res.RetryAfter)
	return res, nil
}

var errScriptResult = errors.New("ratelimit: unexpected return shape from the rate-limit script")

var _ Limiter = (*Redis)(nil)
