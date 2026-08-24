package organizations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

func TestDeletePendingRechecksLifecycleState(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	store := organizations.New(pool)

	var restored, pending pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO orgs (slug, name, kind, status)
		VALUES ('restored-before-purge', 'Restored', 'team', 'pending_delete')
		RETURNING id`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO orgs (slug, name, kind, status)
		VALUES ('still-pending-purge', 'Pending', 'team', 'pending_delete')
		RETURNING id`).Scan(&pending); err != nil {
		t.Fatal(err)
	}

	// Simulate a cleanup worker holding a stale candidate ID after restore.
	if _, err := store.SetStatus(ctx, restored, "active"); err != nil {
		t.Fatal(err)
	}
	if n, err := store.DeletePending(ctx, restored); err != nil || n != 0 {
		t.Fatalf("restored org delete = %d, %v; want no-op", n, err)
	}
	if n, err := store.DeletePending(ctx, pending); err != nil || n != 1 {
		t.Fatalf("pending org delete = %d, %v; want one row", n, err)
	}

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1)`, restored).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("restored organization was deleted from a stale purge candidate")
	}
}
