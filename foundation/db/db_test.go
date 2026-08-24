package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/migrations"
)

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Start(t) // Start has already migrated once

	if err := db.Migrate(ctx, pool, migrations.Community); err != nil {
		t.Fatalf("migrating again should be harmless: %v", err)
	}

	// The core objects exist: the schema's own tables plus the job queue's.
	for _, table := range []string{"idempotency_keys", "river_job"} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Errorf("table %s should exist: %v", table, err)
		}
	}
}

// With a pool limited to one connection the migration must not deadlock against
// itself: the migration lock is held on a separate direct connection.
func TestMigrateWithSingleConnPool(t *testing.T) {
	pool := testpg.Start(t) // already migrated once; this asserts the idempotent re-run does not hang on a tiny pool

	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()

	if err := db.Migrate(ctx, small, migrations.Community); err != nil {
		t.Fatalf("migrating on a single-connection pool should finish rather than deadlock: %v", err)
	}
}

func TestIdempotencyKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Start(t)

	expires := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	var claimedStatus string
	err := pool.QueryRow(ctx, `
		INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, expires_at)
		VALUES ('console', 'k1', 'h1', $1)
		ON CONFLICT (scope, idempotency_key) DO NOTHING
		RETURNING status`, expires).Scan(&claimedStatus)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if claimedStatus != "in_flight" {
		t.Errorf("Status = %q, want in_flight", claimedStatus)
	}

	// A second claim on the same key conflicts and returns no rows; the caller
	// then reads the first attempt's result with Get.
	var duplicateID pgtype.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, expires_at)
		VALUES ('console', 'k1', 'h1', $1)
		ON CONFLICT (scope, idempotency_key) DO NOTHING RETURNING id`, expires).Scan(&duplicateID)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a conflicting claim should return ErrNoRows, got %v", err)
	}

	_, err = pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_status = 201,
		    response_headers = $1, response_body = $2
		WHERE scope = 'console' AND idempotency_key = 'k1'
		  AND status = 'in_flight' AND request_hash = 'h1'`,
		[]byte(`{"Content-Type":["application/json"]}`), []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	var gotStatus string
	var gotResponseStatus pgtype.Int4
	var gotCreatedAt, gotUpdatedAt pgtype.Timestamptz
	err = pool.QueryRow(ctx, `
		SELECT status, response_status, created_at, updated_at FROM idempotency_keys
		WHERE scope = 'console' AND idempotency_key = 'k1'`).
		Scan(&gotStatus, &gotResponseStatus, &gotCreatedAt, &gotUpdatedAt)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotStatus != "completed" || gotResponseStatus.Int32 != 201 {
		t.Errorf("the completed state was not stored: status=%q response_status=%v", gotStatus, gotResponseStatus)
	}
	if !gotUpdatedAt.Time.After(gotCreatedAt.Time) {
		t.Errorf("the updated_at trigger did not fire: created=%v updated=%v", gotCreatedAt.Time, gotUpdatedAt.Time)
	}

	// The expiry sweep deletes only expired rows.
	_, err = pool.Exec(ctx, `
		INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, expires_at)
		VALUES ('console', 'k-expired', 'h', $1)`,
		pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true})
	if err != nil {
		t.Fatalf("claim the expired sample: %v", err)
	}
	tag, err := pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Errorf("the sweep should delete exactly 1 row, got n=%d err=%v", tag.RowsAffected(), err)
	}
}
