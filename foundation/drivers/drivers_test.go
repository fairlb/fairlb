package drivers_test

import (
	"context"
	"testing"

	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/drivers"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
)

func TestNewMemorySelection(t *testing.T) {
	pool := testpg.Start(t)
	// cancel has to run before the pool is torn down (cleanups run LIFO, so
	// this is registered later), or pool.Close deadlocks waiting for the
	// connection the Listen goroutine holds.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := config.Config{Drivers: config.Drivers{
		Cache: config.DriverMemory, RateLimit: config.DriverMemory,
		Breaker: config.DriverMemory, Lock: config.DriverMemory,
	}}
	d, err := drivers.New(ctx, cfg, pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Cache == nil || d.RateLimit == nil || d.Breaker == nil || d.Lock == nil {
		t.Fatalf("all four in-process drivers should be present: %+v", d)
	}
}

// Assembly against a real shared store puts all four drivers in place.
//
// This test used to point at an address with nothing listening, because
// assembly did not check reachability and "an object was constructed" counted
// as passing. Once assembly started pinging, that address fails fast — so the
// test now runs against a real instance, and the assertion is no longer "the
// object is not nil" but "it assembles against a live store with all four
// drivers present".
func TestNewRedisSelection(t *testing.T) {
	cfg := config.Config{
		RedisURL: "redis://" + testredis.Addr(t),
		Drivers: config.Drivers{
			Cache: config.DriverRedis, RateLimit: config.DriverRedis,
			Breaker: config.DriverRedis, Lock: config.DriverRedis,
		},
	}
	d, err := drivers.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Cache == nil || d.RateLimit == nil || d.Breaker == nil || d.Lock == nil {
		t.Fatalf("all four shared-store drivers should be present: %+v", d)
	}
}
