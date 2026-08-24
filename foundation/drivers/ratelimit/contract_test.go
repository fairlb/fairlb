package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
)

// testLimiterContract is the contract suite both implementations must satisfy.
func testLimiterContract(t *testing.T, lim ratelimit.Limiter) {
	t.Helper()
	ctx := context.Background()

	// Within the burst capacity requests pass; beyond it they are refused with a
	// retry hint.
	for i := range 3 {
		res, err := lim.Allow(ctx, "k1", 3, time.Second)
		if err != nil || !res.Allowed {
			t.Fatalf("request %d should be allowed: %+v err=%v", i+1, res, err)
		}
		if res.Limit != 3 {
			t.Errorf("Limit = %d, want 3", res.Limit)
		}
	}
	res, err := lim.Allow(ctx, "k1", 3, time.Second)
	if err != nil || res.Allowed {
		t.Fatalf("the fourth request should be refused: %+v err=%v", res, err)
	}
	if res.RetryAfter <= 0 || res.Remaining != 0 {
		t.Errorf("a refusal should carry RetryAfter and Remaining=0: %+v", res)
	}

	// The quota refills once the window advances.
	if r, _ := lim.Allow(ctx, "refill", 1, 300*time.Millisecond); !r.Allowed {
		t.Fatal("the first request should be allowed")
	}
	if r, _ := lim.Allow(ctx, "refill", 1, 300*time.Millisecond); r.Allowed {
		t.Fatal("an exhausted quota should refuse")
	}
	time.Sleep(400 * time.Millisecond)
	if r, _ := lim.Allow(ctx, "refill", 1, 300*time.Millisecond); !r.Allowed {
		t.Fatal("it should allow again once the window has advanced")
	}

	// Keys are counted independently.
	if r, _ := lim.Allow(ctx, "other", 1, time.Second); !r.Allowed {
		t.Fatal("different keys should count independently")
	}

	// AllowN consumes by volume rather than by request count.
	if r, _ := lim.AllowN(ctx, "n1", 4, 10, time.Second); !r.Allowed {
		t.Fatal("4 of 10 should be allowed")
	}
	if r, _ := lim.AllowN(ctx, "n1", 4, 10, time.Second); !r.Allowed {
		t.Fatal("8 of 10 cumulatively should be allowed")
	}
	// All or nothing: a remaining allowance of 2 cannot serve a request for 4,
	// and must not be partially consumed by it.
	if r, _ := lim.AllowN(ctx, "n1", 4, 10, time.Second); r.Allowed || r.RetryAfter <= 0 {
		t.Fatalf("an insufficient allowance should refuse the whole request and suggest a retry: %+v", r)
	}
	// A refused large request must not eat into the allowance: a smaller one
	// after it still passes.
	if r, _ := lim.AllowN(ctx, "n1", 2, 10, time.Second); !r.Allowed {
		t.Fatalf("a refused request must not consume quota; the remaining 2 should be allowed: %+v", r)
	}

	// An n larger than the limit itself can never pass; the caller should refuse
	// it earlier with a clearer error.
	if r, _ := lim.AllowN(ctx, "n2", 11, 10, time.Second); r.Allowed {
		t.Fatal("an n above the limit must not be allowed")
	}
	// ...and must not pollute that key's counter.
	if r, _ := lim.AllowN(ctx, "n2", 10, 10, time.Second); !r.Allowed {
		t.Fatalf("an oversized request must not pollute the counter: %+v", r)
	}

	// An n of zero or less counts as one.
	if r, _ := lim.AllowN(ctx, "n3", 0, 1, time.Second); !r.Allowed {
		t.Fatal("n=0 should count as 1 and be allowed")
	}
	if r, _ := lim.AllowN(ctx, "n3", 1, 1, time.Second); r.Allowed {
		t.Fatal("n=0 consumed one unit, so the quota should now be exhausted")
	}
}

func TestMemoryContract(t *testing.T) {
	testLimiterContract(t, ratelimit.NewMemory())
}

// A parameter change takes effect immediately: the same key is judged by the
// new configuration.
func TestMemoryParamChange(t *testing.T) {
	ctx := context.Background()
	lim := ratelimit.NewMemory()
	if r, _ := lim.Allow(ctx, "k", 1, time.Second); !r.Allowed {
		t.Fatal("the first request should be allowed")
	}
	if r, _ := lim.Allow(ctx, "k", 1, time.Second); r.Allowed {
		t.Fatal("an exhausted quota should refuse")
	}
	if r, _ := lim.Allow(ctx, "k", 5, time.Second); !r.Allowed {
		t.Fatal("raising the limit should allow it")
	}
}

func TestRedisContract(t *testing.T) {
	testLimiterContract(t, ratelimit.NewRedis(testredis.Start(t)))
}

// The same holds for the shared implementation. Across a parameter change the
// token count is clamped to the new capacity, so shrinking the limit leaves no
// allowance above it.
func TestRedisParamChange(t *testing.T) {
	ctx := context.Background()
	lim := ratelimit.NewRedis(testredis.Start(t))
	if r, _ := lim.Allow(ctx, "k", 1, time.Second); !r.Allowed {
		t.Fatal("the first request should be allowed")
	}
	if r, _ := lim.Allow(ctx, "k", 1, time.Second); r.Allowed {
		t.Fatal("an exhausted quota should refuse")
	}
	if r, _ := lim.Allow(ctx, "k", 5, time.Second); !r.Allowed {
		t.Fatal("raising the limit should allow it")
	}
}
