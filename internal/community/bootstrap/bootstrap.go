// Package bootstrap handles first-start setup: making sure the default
// organisation row exists, and providing the entry point for creating an
// administrator from the command line.
//
// # Why there is an organisation at all
//
// A single self-hosted instance has no concept of "organisation", but every
// table in the gateway schema carries org_id and row-level security isolates by
// it. Dropping the column would fork every query those tables share. So one
// fixed organisation row is created here, the mechanism works unchanged, and
// the word never appears in the UI.
//
// # Why account creation uses plain SQL rather than a generated query
//
// The generated staff-creation query writes TOTP columns that this schema does
// not have. Splitting the whole staff query file -- which mixes the shared
// account basics with TOTP operations -- for the sake of one one-off CLI action
// is not worth it, so this talks to the database directly. It is not on any hot
// path.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
)

const (
	// DefaultOrgSlug is the slug of the single organisation row. It is a fixed
	// value: org_id has to point at the same row after a restart, otherwise
	// historical usage and every API key become orphans.
	DefaultOrgSlug = "default"
	// DefaultOrgName exists only in the database; the UI never shows it.
	DefaultOrgName = "Default"
	// DefaultAdminName is the display name used when an account is created
	// without one.
	DefaultAdminName = "Admin"
)

// EnsureDefaultOrg idempotently creates the default organisation and returns
// its id.
//
// kind is 'personal': the CHECK constraint accepts only personal or team, and a
// single self-hosted instance is closer to the former. The choice affects no
// behaviour; it just keeps the constraint from refusing the insert.
func EnsureDefaultOrg(ctx context.Context, pool *pgxpool.Pool) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO orgs (slug, name, kind) VALUES ($1, $2, 'personal')
		ON CONFLICT (slug) DO UPDATE SET slug = excluded.slug
		RETURNING id`, DefaultOrgSlug, DefaultOrgName).Scan(&id)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("bootstrap: ensure the default organisation: %w", err)
	}
	return id, nil
}

// CreateAdmin creates an administrator account.
//
// The role is always superadmin -- there is no privilege ladder here. It is
// written into the row rather than hard-coded in the code path: the gateway's
// superadmin check reads the principal's role, and the principal's role comes
// from the row. Storing it is what lets one copy of the decision logic serve
// every deployment shape.
func CreateAdmin(ctx context.Context, pool *pgxpool.Pool, email, password, name string) error {
	// Same rule as the other two entry points. Applying it to the wizard but
	// not to the command line would just move the weak password to whichever
	// path is not checked.
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return fmt.Errorf("bootstrap: hash the password: %w", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO staff_users (email, password_hash, name, role)
		VALUES ($1, $2, $3, 'superadmin')`, email, hash, name)
	if err != nil {
		return fmt.Errorf("bootstrap: create the administrator: %w", err)
	}
	return nil
}

// AdminExists reports whether any administrator exists; the first-start wizard
// uses it to decide whether to walk the user through creating one.
func AdminExists(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM staff_users)`).Scan(&ok)
	if err != nil && !db.IsNoRows(err) {
		return false, fmt.Errorf("bootstrap: check whether an administrator exists: %w", err)
	}
	return ok, nil
}
