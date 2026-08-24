package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

// Behavioral contract for row-level security: an org-scoped transaction sees
// only its own org, cross-org reads and writes are both refused, the system path
// (the connecting role) is not subject to the policies, and an application-role
// query with no org context set errors out instead of returning an empty set.
//
// This file covers the core tables only. Tables owned by another layer carry the
// same kind of contract in their own package, next to the schema they describe.

// Isolation of the core tables: an org-scoped transaction sees only its own org,
// key, daily spend and audit rows, and a cross-org write is refused by the
// policy's WITH CHECK clause.
//
// This is the only test standing behind data isolation. A single-org deployment
// never exercises it, so if the policies silently stop applying nothing looks
// wrong — until the day a second org exists.
func TestRLSCoreTables(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	orgA := publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Slug: "core-a", Name: "A"}))
	orgB := publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Slug: "core-b", Name: "B"}))
	for _, org := range []string{orgA, orgB} {
		err := db.WithSystemTx(ctx, pool, func(tx pgx.Tx) error {
			var keyID string
			if err := tx.QueryRow(ctx, `INSERT INTO api_keys (org_id, name, prefix, key_hash)
				VALUES ($1, 'k', 'sk-flb-v1-xx', $2) RETURNING id::text`, org, org+"-corehash").Scan(&keyID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO api_key_daily_spend (api_key_id, day, spent_nano)
				VALUES ($1, current_date, 1)`, keyID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO audit_logs (org_id, actor_type, action, target_type)
				VALUES ($1, 'system', 'key.create', 'api_key')`, org)
			return err
		})
		if err != nil {
			t.Fatalf("seed core data: %v", err)
		}
	}

	// Scoped to org A: each of the four tables shows exactly its own one row.
	err := db.WithOrgTx(ctx, pool, orgA, func(tx pgx.Tx) error {
		for _, table := range []string{"orgs", "api_keys", "api_key_daily_spend", "audit_logs"} {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("%s should show exactly its own 1 row, got %d", table, n)
			}
		}
		// Asking for B by name does not reveal it either.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE org_id = $1`, orgB).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("naming another org explicitly should return nothing, got %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scoped to the first org: %v", err)
	}

	// Write side: creating a key for B while scoped to A must be refused by
	// the policy's WITH CHECK clause.
	err = db.WithOrgTx(ctx, pool, orgA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO api_keys (org_id, name, prefix, key_hash)
			VALUES ($1, 'cross', 'sk-flb-v1-yy', 'cross-hash')`, orgB)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("a cross-org INSERT should be refused by the policy WITH CHECK: %v", err)
	}

	// System path: both orgs are visible, which is what the operations role is
	// for.
	err = db.WithSystemTx(ctx, pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("the system path should see every org, got %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("system path: %v", err)
	}
}

// Application role with no org context: the query errors out. Fail closed, not
// a silent empty result — an empty result would look like "no data" and hide
// the missing scope.
func TestRLSFailClosedWithoutOrgContext(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+db.AppRole); err != nil {
			return err
		}
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&n)
	})
	if err == nil {
		t.Fatal("an application-role query with no app.org_id set should error")
	}
}

func TestWithOrgTxRejectsInvalidID(t *testing.T) {
	pool := testpg.Start(t)
	err := db.WithOrgTx(context.Background(), pool, "not-a-uuid", func(pgx.Tx) error { return nil })
	if err == nil {
		t.Fatal("an invalid org id should be refused before the transaction starts")
	}
}
