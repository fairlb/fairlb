package drivers_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/drivers"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
)

// Driver lifecycle: ping at assembly, register the dependency with the health
// check, and close at shutdown.

// An unreachable shared store must fail at startup. Constructing the client does
// not dial, so without this check a process with a wrong address starts
// normally, goes behind the proxy and takes traffic until the first rate-limit
// decision blows up. That reads as scattered 500s in production rather than "the
// deployment failed".
func TestRedisUnreachableFailsStartup(t *testing.T) {
	pool := testpg.Start(t)
	cfg := config.Config{
		RedisURL: "redis://127.0.0.1:1/", // nothing listens on port 1
		Drivers: config.Drivers{
			Cache: config.DriverRedis, RateLimit: config.DriverRedis,
			Breaker: config.DriverRedis, Lock: config.DriverRedis,
		},
	}
	_, err := drivers.New(context.Background(), cfg, pool)
	if err == nil {
		t.Fatal("assembly must fail when the shared store is unreachable, or a broken connection reaches production")
	}
	if !strings.Contains(err.Error(), "redis unreachable") {
		t.Errorf("the error should say the store is unreachable, got: %v", err)
	}
}

// When it is reachable, assembly succeeds and both Ping and Close work.
func TestRedisLifecycle(t *testing.T) {
	pool := testpg.Start(t)
	addr := testredis.Addr(t)
	cfg := config.Config{
		RedisURL: "redis://" + addr,
		Drivers: config.Drivers{
			Cache: config.DriverRedis, RateLimit: config.DriverRedis,
			Breaker: config.DriverRedis, Lock: config.DriverRedis,
		},
	}
	d, err := drivers.New(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("assembly should succeed when the store is reachable: %v", err)
	}
	if err := d.Ping(context.Background()); err != nil {
		t.Errorf("Ping should succeed: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close should succeed: %v", err)
	}
	// After Close, Ping must fail. That is what proves Close actually released
	// the pool rather than being an empty method.
	if err := d.Ping(context.Background()); err == nil {
		t.Error("Ping still succeeds after Close, so the pool was not really closed")
	}
}

// With all four drivers in-process there is no shared-store dependency at all:
// Ping and Close are no-ops, and a missing REDIS_URL must not turn the health
// check red.
func TestMemoryOnlyHasNoRedisDependency(t *testing.T) {
	pool := testpg.Start(t)
	cfg := config.Config{
		Drivers: config.Drivers{
			Cache: config.DriverMemory, RateLimit: config.DriverMemory,
			Breaker: config.DriverMemory, Lock: config.DriverMemory,
		},
	}
	if cfg.UsesRedis() {
		t.Fatal("UsesRedis should be false when all four drivers are in-process")
	}
	d, err := drivers.New(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("assembly should succeed: %v", err)
	}
	if err := d.Ping(context.Background()); err != nil {
		t.Errorf("Ping should be a no-op with no shared store: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close should be a no-op with no shared store: %v", err)
	}
}

// Close must stop the in-process cache's LISTEN goroutine, not just the shared
// store.
//
// That goroutine holds a database connection open. Without stopping it, the
// connection pool's Close waits forever for a connection that never returns —
// which hangs the whole package until the test run times out, and only under
// the race detector, because everything else finishes fast enough to miss it.
// The assertion here is the direct inverse of that hang: after Close, the pool
// can be closed.
func TestCloseReleasesListenConnection(t *testing.T) {
	pool := testpg.Start(t)
	cfg := config.Config{
		Drivers: config.Drivers{
			Cache: config.DriverMemory, RateLimit: config.DriverMemory,
			Breaker: config.DriverMemory, Lock: config.DriverMemory,
		},
	}
	// Deliberately pass a context that is never cancelled: assembly points
	// often do, and the driver cannot rely on the caller cleaning up for it.
	d, err := drivers.New(context.Background(), cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// After Close the pool should shut quickly. Hanging means a connection is
	// still checked out.
	done := make(chan struct{})
	go func() { pool.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pool still will not close after Close: the LISTEN goroutine is holding a connection")
	}
}
