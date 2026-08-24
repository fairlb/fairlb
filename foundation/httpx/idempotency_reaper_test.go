package httpx_test

import (
	"context"
	"testing"

	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

// The reaper removes keys past their replay window and leaves live ones alone.
//
// This assertion used to live in the hosted build's compliance sweep, which is
// precisely why the self-hosted build had no reaper at all: the coverage was
// attached to one product's worker rather than to the behaviour. It belongs
// next to the middleware that writes these rows, where both products inherit
// it.
func TestIdempotencyReaperRemovesOnlyExpiredKeys(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, expires_at)
		VALUES ('console', 'expired', 'h', now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, expires_at)
		VALUES ('console', 'live', 'h', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}

	w := httpx.NewIdempotencyReapWorker(pool)
	if err := w.Work(ctx, &river.Job[httpx.IdempotencyReapArgs]{}); err != nil {
		t.Fatalf("reap: %v", err)
	}

	count := func(key string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM idempotency_keys WHERE idempotency_key = $1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count("expired"); got != 0 {
		t.Errorf("the expired key should have been removed, %d left", got)
	}
	// The other half matters as much: a reaper that deletes everything would
	// pass an "is it gone" assertion while breaking every in-flight retry.
	if got := count("live"); got != 1 {
		t.Errorf("a key still inside its replay window must survive, found %d", got)
	}
}
