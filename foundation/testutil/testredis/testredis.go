// Package testredis provides a real Redis test fixture backed by containers.
// Tests that need a container are skipped under `go test -short`.
//
// Same structure as the PostgreSQL fixture: one container for the whole test
// process, with tests isolated from each other by taking their own logical
// database index. Each test gets its own and flushes it at the end.
//
// Isolating by index rather than by key prefix is deliberate: the contract suite
// uses fixed keys like "k1", and prefix isolation would mean changing the tests
// themselves. How isolation is achieved should not leak into the code under
// test.
package testredis

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// redisDatabases is the default number of logical databases. Beyond that count
// the indices would start being reused, so the fixture fails loudly instead of
// wrapping around silently: two tests sharing one database delete each other's
// keys, which surfaces as random failures — the hardest kind to track down.
const redisDatabases = 16

var (
	bootOnce sync.Once
	shared   *base
	bootErr  error
	dbSeq    atomic.Int64
)

type base struct {
	addr string
}

// Start returns a client bound to its own logical database, flushed and closed
// when the test ends.
//
// gate-honesty: same reasoning as the PostgreSQL fixture — a skip under -short
// does not appear in the default `go test` output, but the merge criterion runs
// without -short, where the container tests necessarily do run.
func Start(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container test in short mode")
	}
	b := boot(t)

	n := dbSeq.Add(1)
	if n >= redisDatabases {
		// Reusing an index makes tests delete each other's keys and fail
		// randomly. Better to fail explicitly and ask for the usage to change.
		t.Fatalf("testredis: out of logical databases (%d); this process has more redis tests than it can isolate", redisDatabases)
	}

	c := redis.NewClient(&redis.Options{Addr: b.addr, DB: int(n)})
	ctx := context.Background()
	if err := c.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("testredis: flush database %d: %v", n, err)
	}
	t.Cleanup(func() {
		_ = c.FlushDB(context.Background()).Err()
		_ = c.Close()
	})
	return c
}

// Addr returns the container's address, for tests that need to build their own
// client.
func Addr(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container test in short mode")
	}
	return boot(t).addr
}

func boot(t *testing.T) *base {
	t.Helper()
	bootOnce.Do(func() {
		ctx := context.Background()
		// The image is pinned by digest; a tag can move and is not an immutable
		// input.
		ctr, err := tcredis.Run(ctx,
			"redis:8-alpine@sha256:8096655e437712b07503796fb64d81359256cfcff0ab29d95a7da72863786efb")
		if err != nil {
			bootErr = err
			return
		}
		addr, err := ctr.ConnectionString(ctx)
		if err != nil {
			bootErr = err
			return
		}
		// ConnectionString returns a redis:// URL while the client's Addr option
		// wants a bare host:port.
		opts, err := redis.ParseURL(addr)
		if err != nil {
			bootErr = err
			return
		}
		shared = &base{addr: opts.Addr}
	})
	if bootErr != nil {
		t.Fatalf("testredis: start container: %v", bootErr)
	}
	return shared
}

// Keeps the linter aware that the container library is used indirectly. Its
// per-test cleanup helper does not apply to a shared container, which the reaper
// removes when the process exits.
var _ = testcontainers.ContainerRequest{}
