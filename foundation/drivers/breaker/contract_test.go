package breaker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
)

// testStoreContract is the contract suite both implementations must satisfy.
func testStoreContract(t *testing.T, store breaker.Store) {
	t.Helper()
	ctx := context.Background()

	if _, ok, err := store.Get(ctx, "absent"); err != nil || ok {
		t.Fatalf("a miss should return ok=false: ok=%v err=%v", ok, err)
	}

	until := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	// Opens and Failures are two independent counters and each must survive a
	// round trip. Dropping Opens silently flattens the backoff ladder whenever
	// the shared driver is in use.
	want := breaker.State{Status: breaker.StatusOpen, Failures: 5, Opens: 3, Until: until}
	if err := store.Set(ctx, "up1", want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := store.Get(ctx, "up1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != want.Status || got.Failures != want.Failures ||
		got.Opens != want.Opens || !got.Until.Equal(want.Until) {
		t.Errorf("the round trip changed the value: got=%+v want=%+v", got, want)
	}

	// Overwrite.
	if err := store.Set(ctx, "up1", breaker.State{Status: breaker.StatusHalfOpen}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Get(ctx, "up1"); got.Status != breaker.StatusHalfOpen {
		t.Errorf("the overwrite did not take effect: %+v", got)
	}

	// TTL expiry.
	if err := store.Set(ctx, "ttl", breaker.State{Status: breaker.StatusOpen}, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok, _ := store.Get(ctx, "ttl"); ok {
		t.Error("it should miss once the TTL has expired")
	}

	if err := store.Delete(ctx, "up1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get(ctx, "up1"); ok {
		t.Error("it should miss after Delete")
	}
}

func TestMemoryContract(t *testing.T) {
	testStoreContract(t, breaker.NewMemory())
}

func TestRedisContract(t *testing.T) {
	testStoreContract(t, breaker.NewRedis(testredis.Start(t)))
}

// Reading an expired entry while another writer stores a new one: expiry cleanup
// compares before deleting and must not wipe the concurrent write. Wiping it
// means a breaker that just tripped is swallowed by the expiry branch, and the
// breaker never opens.
func TestMemoryExpiryDoesNotDropConcurrentSet(t *testing.T) {
	store := breaker.NewMemory()
	ctx := context.Background()

	for range 300 {
		if err := store.Set(ctx, "race", breaker.State{Status: breaker.StatusClosed}, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // make sure it has expired
		var wg sync.WaitGroup
		wg.Go(func() { _, _, _ = store.Get(ctx, "race") })
		wg.Go(func() {
			_ = store.Set(ctx, "race", breaker.State{Status: breaker.StatusOpen, Failures: 5}, time.Minute)
		})
		wg.Wait()
		got, ok, _ := store.Get(ctx, "race")
		if !ok || got.Status != breaker.StatusOpen {
			t.Fatalf("the expiry branch wiped a concurrently written breaker state: ok=%v got=%+v", ok, got)
		}
	}
}
