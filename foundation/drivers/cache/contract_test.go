package cache_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
)

// testStoreContract is the contract suite both implementations must satisfy.
func testStoreContract(t *testing.T, store cache.Store) {
	t.Helper()
	ctx := context.Background()

	if _, ok, err := store.Get(ctx, "absent"); err != nil || ok {
		t.Fatalf("a miss should return ok=false: ok=%v err=%v", ok, err)
	}

	if err := store.Set(ctx, "k", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := store.Get(ctx, "k")
	if err != nil || !ok || !bytes.Equal(v, []byte("v1")) {
		t.Fatalf("roundtrip: v=%q ok=%v err=%v", v, ok, err)
	}

	if err := store.Set(ctx, "ttl", []byte("x"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set ttl: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok, _ := store.Get(ctx, "ttl"); ok {
		t.Error("it should miss once the TTL has expired")
	}

	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := store.Get(ctx, "k"); ok {
		t.Error("it should miss after Delete")
	}
}

func TestMemoryContract(t *testing.T) {
	pool := testpg.Start(t)
	mem, err := cache.NewMemory(pool, 128)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	testStoreContract(t, mem)
}

// Invalidation broadcast: after instance A deletes a key, instance B's local
// copy is dropped via LISTEN/NOTIFY.
func TestMemoryInvalidationBroadcast(t *testing.T) {
	pool := testpg.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, err := cache.NewMemory(pool, 128)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cache.NewMemory(pool, 128)
	if err != nil {
		t.Fatal(err)
	}
	go b.Listen(ctx)

	if err := b.Set(ctx, "shared", []byte("stale"), time.Minute); err != nil {
		t.Fatal(err)
	}

	// There is no way to observe when the listening connection is ready, so
	// resend the delete on a poll until B's copy disappears.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := a.Delete(ctx, "shared"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		if _, ok, _ := b.Get(ctx, "shared"); !ok {
			return // the broadcast took effect
		}
	}
	t.Fatal("the invalidation broadcast did not reach the second instance within 5s")
}

func TestRedisContract(t *testing.T) {
	testStoreContract(t, cache.NewRedis(testredis.Start(t)))
}

// Defensive copying: both implementations must behave the same, so the caller's
// slice and the cached copy are independent. If they shared a backing array, one
// byte written by the caller would poison every later reader.
func TestMemoryDefensiveCopy(t *testing.T) {
	pool := testpg.Start(t)
	mem, err := cache.NewMemory(pool, 8)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	ctx := context.Background()

	// Mutating the source slice after Set must not change what is cached.
	src := []byte("v1")
	if err := mem.Set(ctx, "k", src, time.Minute); err != nil {
		t.Fatal(err)
	}
	src[0] = 'X'
	got, ok, err := mem.Get(ctx, "k")
	if err != nil || !ok || !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("mutating the source slice after Set must not affect the cache: %q", got)
	}

	// Mutating a returned value must not affect later readers.
	got[0] = 'Y'
	again, _, _ := mem.Get(ctx, "k")
	if !bytes.Equal(again, []byte("v1")) {
		t.Fatalf("mutating a value returned by Get must not affect the cache: %q", again)
	}
}

// Reading an expired entry while another writer stores a new one: the expiry
// branch must not wipe the concurrent write.
func TestMemoryExpiryDoesNotDropConcurrentSet(t *testing.T) {
	pool := testpg.Start(t)
	mem, err := cache.NewMemory(pool, 64)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	ctx := context.Background()

	for range 200 {
		if err := mem.Set(ctx, "race", []byte("old"), time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // make sure it has expired
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _, _ = mem.Get(ctx, "race") }()
		go func() { defer wg.Done(); _ = mem.Set(ctx, "race", []byte("new"), time.Minute) }()
		wg.Wait()
		v, ok, _ := mem.Get(ctx, "race")
		if !ok || !bytes.Equal(v, []byte("new")) {
			t.Fatalf("the expiry branch wiped a concurrently written value: ok=%v v=%q", ok, v)
		}
	}
}
