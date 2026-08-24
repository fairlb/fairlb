package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

// The middleware fallback row: with no detailed row from a domain, a write
// request produces one HTTP-level audit row carrying the status and the address.
func TestHookWritesGenericRow(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	h := httpx.Audit(NewHook(pool))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest("POST", "/api/staff/v1/things", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	h.ServeHTTP(httptest.NewRecorder(), req) // Record stores synchronously from the deferred call

	rows, err := readAll(ctx, pool)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 fallback row, got %d", len(rows))
	}
	r := rows[0]
	if r.ActorType != "system" || r.Action != "POST /api/staff/v1/things" {
		t.Errorf("fallback row fields are wrong: type=%s action=%s", r.ActorType, r.Action)
	}
	if r.Ip == nil || r.Ip.String() != "203.0.113.7" {
		t.Errorf("the fallback row should record the source address, got %v", r.Ip)
	}
}

// Deduplication: once a domain has committed its detailed row and called
// MarkAudited, the Hook skips the fallback — exactly one row in total.
func TestHookSkipsWhenAudited(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	h := httpx.Audit(NewHook(pool))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = db.WithSystemTx(r.Context(), pool, func(tx pgx.Tx) error {
			return InsertTx(r.Context(), tx, Entry{
				ActorType: "staff", Action: "thing.do",
				TargetType: "thing", TargetID: "t1",
				Meta: map[string]any{"reason": "manual adjustment"},
			})
		})
		httpx.MarkAudited(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/staff/v1/things", nil))

	rows, err := readAll(ctx, pool)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "thing.do" {
		t.Fatalf("want exactly one detailed thing.do row and no fallback, got %d rows %+v", len(rows), rows)
	}
}

// The fallback row is best effort: a failed audit write never fails the request.
// Closing the pool stands in for an unreachable store.
func TestHookNeverFailsRequest(t *testing.T) {
	pool := testpg.Start(t)
	pool.Close() // the store is now unreachable
	rec := httptest.NewRecorder()
	httpx.Audit(NewHook(pool))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("an unreachable audit store must not affect the request, got %d", rec.Code)
	}
}

// Creating partitions is idempotent, and the catch-all default partition absorbs
// any month, so an insert never fails for want of a partition.
func TestPartitionEnsureAndDefault(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	m := time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := ensurePartition(ctx, pool, m); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	if err := ensurePartition(ctx, pool, m); err != nil {
		t.Fatalf("creating them again should be idempotent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_logs (actor_type, action, created_at) VALUES ('system','test.future',$1)`,
		m.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("insert into a newly created partition: %v", err)
	}
	// A far-future month with no partition still inserts, landing in the
	// default one.
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_logs (actor_type, action, created_at) VALUES ('system','test.default',$1)`,
		time.Date(2099, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("the default partition should absorb any month: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action LIKE 'test.%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("both rows should be readable, got %d", n)
	}
}

// The periodic job is idempotent on a re-run.
func TestPartitionWorkerIdempotent(t *testing.T) {
	pool := testpg.Start(t)
	w := NewPartitionWorker(pool)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("a re-run should be idempotent: %v", err)
	}
}

// Keyset cursor encoding round trip.

// readAll reads every audit row directly, in write order, so that a test of the
// write side asserts on something the write side can see.
//
// It deliberately does not call the read-side service. That service lives in a
// layer this package must not depend on, and using it here would also couple the
// two unnecessarily: changing the read side's pagination or filters would turn
// this test red in a direction that does not point at the actual change. Both
// go through the same stable read DTO, so nothing is lost.
func readAll(ctx context.Context, pool *pgxpool.Pool) ([]Log, error) {
	return NewStore(pool).List(ctx, Filter{Limit: 100})
}
