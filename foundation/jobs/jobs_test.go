package jobs_test

import (
	"context"
	"testing"

	"github.com/fairlb/fairlb/foundation/jobs"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

type testArgs struct{}

func (testArgs) Kind() string { return "test_job" }

// The core constraint: enqueueing shares the business transaction, so a rollback
// leaves no job and a commit makes it visible.
func TestInsertTxFollowsTransaction(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Start(t)

	client, err := jobs.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}

	countJobs := func() int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'test_job'").Scan(&n); err != nil {
			t.Fatalf("query the job table: %v", err)
		}
		return n
	}

	// Rollback path.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := client.InsertTx(ctx, tx, testArgs{}, nil); err != nil {
		t.Fatalf("InsertTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := countJobs(); n != 0 {
		t.Fatalf("after a rollback the job should not exist, got %d", n)
	}

	// Commit path.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := client.InsertTx(ctx, tx, testArgs{}, nil); err != nil {
		t.Fatalf("InsertTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := countJobs(); n != 1 {
		t.Fatalf("after a commit there should be exactly 1 job, got %d", n)
	}
}
