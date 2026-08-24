package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AppRole is the least-privilege application role that row-level security
// policies are written against. The core migration creates it.
const AppRole = "fairlb_app"

// WithOrgTx runs fn inside one transaction scoped to a single org: SET LOCAL
// ROLE puts the session under the row-level security policies, and SET LOCAL
// app.org_id supplies the key those policies compare against.
//
// Everything is SET LOCAL, so it unwinds when the transaction ends and the
// pattern stays correct behind a connection pooler running in transaction mode.
// Every read and write of org-scoped data must go through here; the policies
// raise an error when app.org_id is unset, so the failure mode is a refusal
// rather than a leak.
func WithOrgTx(ctx context.Context, pool *pgxpool.Pool, orgID string, fn func(pgx.Tx) error) error {
	var u pgtype.UUID
	if err := u.Scan(orgID); err != nil {
		return fmt.Errorf("db: invalid org id: %w", err)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// SET ROLE takes no bind parameters. The role name is a constant in this
		// package, never user input.
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+AppRole); err != nil {
			return fmt.Errorf("db: switch to application role: %w", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.org_id', $1, true)", orgID); err != nil {
			return fmt.Errorf("db: set org scope: %w", err)
		}
		return fn(tx)
	})
}

// WithSystemTx runs a transaction as the connecting role — the table owner,
// which bypasses row-level security unless a policy is FORCE'd — for system and
// operations paths. The name states the intent: org-scoped data must never be
// reached through here.
func WithSystemTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, fn)
}
