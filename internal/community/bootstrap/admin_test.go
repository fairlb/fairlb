package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/community/bootstrap"
)

func TestCreateFirstAdminRefusesOnceOneExists(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	if err := bootstrap.CreateFirstAdmin(ctx, pool, "first@example.com", "hunter2hunter2", ""); err != nil {
		t.Fatal(err)
	}
	err := bootstrap.CreateFirstAdmin(ctx, pool, "second@example.com", "hunter2hunter2", "")
	if !errors.Is(err, bootstrap.ErrAlreadyConfigured) {
		t.Fatalf("second call returned %v, want ErrAlreadyConfigured", err)
	}
	if n := countAdmins(t, pool); n != 1 {
		t.Fatalf("%d administrators exist, want 1", n)
	}
}

// Proves the serialisation directly: hold the lock, then show that a caller
// waits for it rather than proceeding on a stale view of the table.
//
// This is the test that fails when the lock is removed. The N-goroutine test
// below does not — measured, not assumed — because the password hashing in
// front of the transaction staggers the callers enough that the two snapshots
// never overlap. A concurrency test whose trigger is a timing coincidence
// reports "passed" for both a correct and a broken implementation.
func TestCreateFirstAdminWaitsForTheLock(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1)", bootstrap.FirstAdminLockKey); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- bootstrap.CreateFirstAdmin(ctx, pool, "waiter@example.com", "hunter2hunter2", "")
	}()

	select {
	case err := <-done:
		t.Fatalf("proceeded while the lock was held (err=%v) — the check and the "+
			"insert are not serialised", err)
	case <-time.After(750 * time.Millisecond):
		// Still waiting, which is the point.
	}

	// Releasing the lock must let it through: otherwise this test would also
	// pass for a function that simply hangs forever.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still blocked after the lock was released")
	}
	if n := countAdmins(t, pool); n != 1 {
		t.Fatalf("%d administrators exist, want 1", n)
	}
}

// Outcome check under load. It cannot by itself prove the locking (see above),
// but it does catch gross breakage of the "exactly one" invariant.
func TestConcurrentFirstAdminLeavesExactlyOneWinner(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			<-start
			// Distinct addresses on purpose: with one shared address the
			// unique index on email would serialise the racers, and this test
			// would pass even with the advisory lock removed — it would be
			// testing Postgres rather than the code under test.
			results[i] = bootstrap.CreateFirstAdmin(ctx, pool,
				fmt.Sprintf("racer%d@example.com", i), "hunter2hunter2", "")
		})
	}
	close(start)
	wg.Wait()

	var won, refused int
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, bootstrap.ErrAlreadyConfigured):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || refused != racers-1 {
		t.Fatalf("%d succeeded and %d were refused; want exactly 1 and %d", won, refused, racers-1)
	}
	if n := countAdmins(t, pool); n != 1 {
		t.Fatalf("%d administrators exist, want 1", n)
	}
}

func TestCreateFirstAdminStoresAUsablePasswordAndSuperadminRole(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	if err := bootstrap.CreateFirstAdmin(ctx, pool, "who@example.com", "hunter2hunter2", "Ada"); err != nil {
		t.Fatal(err)
	}
	var hash, role, name string
	if err := pool.QueryRow(ctx,
		`SELECT password_hash, role, name FROM staff_users WHERE email = $1`,
		"who@example.com").Scan(&hash, &role, &name); err != nil {
		t.Fatal(err)
	}
	if role != "superadmin" {
		t.Errorf("role is %q, want superadmin", role)
	}
	if name != "Ada" {
		t.Errorf("name is %q, want Ada", name)
	}
	// The stored value has to verify, and the wrong password has to not.
	// Asserting only the first would pass for an implementation that accepts
	// anything.
	ok, err := crypto.VerifyPassword("hunter2hunter2", hash)
	if err != nil || !ok {
		t.Fatalf("the stored password does not verify (ok=%v err=%v)", ok, err)
	}
	if ok, _ := crypto.VerifyPassword("wrong-password", hash); ok {
		t.Error("a wrong password verified against the stored hash")
	}
}

func TestCreateFirstAdminDefaultsTheDisplayName(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	if err := bootstrap.CreateFirstAdmin(ctx, pool, "noname@example.com", "hunter2hunter2", ""); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT name FROM staff_users WHERE email = $1`, "noname@example.com").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != bootstrap.DefaultAdminName {
		t.Fatalf("name is %q, want %q", name, bootstrap.DefaultAdminName)
	}
}

func TestSetPasswordChangesTheStoredHash(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	if err := bootstrap.CreateFirstAdmin(ctx, pool, "reset@example.com", "old-password-x", ""); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.SetPassword(ctx, pool, "reset@example.com", "new-password-y"); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := pool.QueryRow(ctx,
		`SELECT password_hash FROM staff_users WHERE email = $1`, "reset@example.com").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := crypto.VerifyPassword("new-password-y", hash); !ok {
		t.Error("the new password does not verify")
	}
	if ok, _ := crypto.VerifyPassword("old-password-x", hash); ok {
		t.Error("the old password still verifies after a reset")
	}
}

// A typo in the address must not look like success — the operator would go on
// trying to sign in with a password that was never stored anywhere.
func TestSetPasswordReportsAnUnknownAddress(t *testing.T) {
	pool := testpg.Start(t)
	err := bootstrap.SetPassword(context.Background(), pool, "nobody@example.com", "whatever-pass")
	if err == nil {
		t.Fatal("resetting the password of a non-existent account succeeded")
	}
	if !strings.Contains(err.Error(), "nobody@example.com") {
		t.Errorf("the error does not name the address: %v", err)
	}
}

func countAdmins(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM staff_users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
