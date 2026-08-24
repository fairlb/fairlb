package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/usage"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testpg.Start(t)
}

// Partition pre-creation: the migrations create the current and next month, and
// the worker keeps three months ahead and is safe to run repeatedly.
func TestPartitionMaintain(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	w := usage.NewPartitionWorker(pool)

	if err := w.Work(ctx, &river.Job[usage.PartitionArgs]{}); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a repeated run raises no error.
	if err := w.Work(ctx, &river.Job[usage.PartitionArgs]{}); err != nil {
		t.Fatalf("repeated pre-creation should be idempotent: %v", err)
	}

	// Truncate to the month start before adding months, matching what the
	// maintainer under test does. Adding months to "today" breaks on the 31st:
	// Go's AddDate normalises a nonexistent date forwards, so 2026-07-31 plus
	// two months becomes September 31st and then October 1st -- the test would
	// look for 2026_10 while the maintainer, counting from July 1st, created
	// 2026_09. It only reproduces on a 31st whose target month is shorter,
	// which happens a handful of times a year.
	now := time.Now().UTC()
	for _, table := range []string{"usage_logs", "gateway_usage_rollups"} {
		m0 := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		for i := range 3 {
			name := table + "_" + m0.AddDate(0, i, 0).Format("2006_01")
			var exists bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Errorf("partition should have been pre-created: %s", name)
			}
		}
	}
}

// A default partition must exist. A missing partition fails the INSERT, and
// that INSERT shares a transaction with settlement -- so the whole dataplane
// would be unavailable. Availability wins over archival purity here.
func TestDefaultPartitionCatchesUnpreparedMonth(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// A far-future timestamp, well past the pre-creation window, must still be
	// writable and land in the default partition.
	far := time.Now().UTC().AddDate(3, 0, 0)
	org := orgtest.CreateID(t, pool, orgtest.Seed{Name: "gw"})
	if _, err := pool.Exec(ctx, `INSERT INTO usage_logs
		(created_at, org_id, request_id, surface, model_slug, status)
		VALUES ($1, $2, 'far-future', 'chat_completions', 'openai/gpt-5.4', 'ok')`, far, org); err != nil {
		t.Fatalf("a write into a month with no partition must not fail; it belongs in the default partition: %v", err)
	}

	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_logs_default`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("that row should be in the default partition: %d", n)
	}

	// A non-empty default must not fail the worker: it warns, it does not
	// block pre-creation.
	if err := usage.NewPartitionWorker(pool).Work(ctx, &river.Job[usage.PartitionArgs]{}); err != nil {
		t.Fatalf("a non-empty default partition must not fail the pre-creation job: %v", err)
	}
}

// Usage rows carry no foreign key, so the billing record survives an org being
// physically deleted -- the archive has to be self-contained. With a foreign
// key this would be either a cascading delete or a constraint error, and the
// billing record would disappear along with the org.
func TestUsageLogsSurviveOrgDeletion(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	org := orgtest.CreateID(t, pool, orgtest.Seed{Name: "gw"})

	if _, err := pool.Exec(ctx, `INSERT INTO usage_logs
		(org_id, request_id, surface, model_slug, status, charged_nano)
		VALUES ($1, 'r-keep', 'chat_completions', 'openai/gpt-5.4', 'ok', 123)`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, org); err != nil {
		t.Fatalf("the org should be physically deletable: %v", err)
	}

	var charged int64
	if err := pool.QueryRow(ctx,
		`SELECT charged_nano FROM usage_logs WHERE request_id = 'r-keep'`).Scan(&charged); err != nil {
		t.Fatalf("the usage rows must survive the org being deleted: %v", err)
	}
	if charged != 123 {
		t.Fatalf("the charged amount should be intact: %d", charged)
	}
}
