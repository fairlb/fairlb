package lock_test

import (
	"context"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers/lock"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
)

// testLockerContract is the contract suite both implementations must satisfy.
func testLockerContract(t *testing.T, l lock.Locker) {
	t.Helper()
	ctx := context.Background()

	rel1, ok, err := l.TryAcquire(ctx, "job:sweep", time.Minute)
	if err != nil || !ok {
		t.Fatalf("the first acquire should succeed: ok=%v err=%v", ok, err)
	}

	if _, ok, err := l.TryAcquire(ctx, "job:sweep", time.Minute); err != nil || ok {
		t.Fatalf("acquiring again while held should fail: ok=%v err=%v", ok, err)
	}

	// Different names do not interfere.
	relOther, ok, err := l.TryAcquire(ctx, "job:other", time.Minute)
	if err != nil || !ok {
		t.Fatalf("a different lock name should be acquirable: ok=%v err=%v", ok, err)
	}
	if err := relOther(ctx); err != nil {
		t.Fatalf("release the other lock: %v", err)
	}

	if err := rel1(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	rel2, ok, err := l.TryAcquire(ctx, "job:sweep", time.Minute)
	if err != nil || !ok {
		t.Fatalf("it should be acquirable again after release: ok=%v err=%v", ok, err)
	}
	if err := rel2(ctx); err != nil {
		t.Fatalf("release again: %v", err)
	}
}

func TestPostgresContract(t *testing.T) {
	pool := testpg.Start(t)
	testLockerContract(t, lock.NewPostgres(pool))
}

// Releasing must still succeed when the caller's context is already cancelled —
// the unlock detaches from it — and the lock must be acquirable again.
func TestPostgresReleaseWithCanceledContext(t *testing.T) {
	pool := testpg.Start(t)
	l := lock.NewPostgres(pool)

	rel, ok, err := l.TryAcquire(context.Background(), "job:canceled-release", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rel(canceled); err != nil {
		t.Fatalf("release should succeed with a cancelled context: %v", err)
	}
	rel2, ok, err := l.TryAcquire(context.Background(), "job:canceled-release", time.Minute)
	if err != nil || !ok {
		t.Fatalf("it should be acquirable again after release: ok=%v err=%v", ok, err)
	}
	if err := rel2(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRedisContract(t *testing.T) {
	testLockerContract(t, lock.NewRedis(testredis.Start(t)))
}

// Only the holder may release. The release has to check the holder token, or A's
// release unlocks B's lock: A's lock times out, B acquires it, then A's late
// release deletes it — and two workers run at once.
func TestRedisReleaseIsOwnerScoped(t *testing.T) {
	c := testredis.Start(t)
	l := lock.NewRedis(c)
	ctx := context.Background()

	// A holds the lock with a very short TTL so it expires on its own, which is
	// what "A still believes it holds the lock" looks like.
	relA, ok, err := l.TryAcquire(ctx, "job:owner", 150*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("A acquires: ok=%v err=%v", ok, err)
	}
	time.Sleep(300 * time.Millisecond)

	// B acquires the same lock.
	relB, ok, err := l.TryAcquire(ctx, "job:owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("B should acquire once A's lock has expired: ok=%v err=%v", ok, err)
	}
	// A's late release must not unlock B's lock.
	_ = relA(ctx)
	if _, ok, _ := l.TryAcquire(ctx, "job:owner", time.Minute); ok {
		t.Error("A's late release unlocked B's lock, which would let two workers run at once")
	}
	if err := relB(ctx); err != nil {
		t.Fatal(err)
	}
}
