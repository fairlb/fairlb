package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
)

// ErrAlreadyConfigured means an administrator already exists, so whatever was
// being attempted (the setup wizard, an environment-driven first user) has no
// work left to do.
var ErrAlreadyConfigured = errors.New("bootstrap: an administrator already exists")

// MinPasswordLength is the shortest password an administrator may have.
//
// The API description says 12 and so does the hosted build's spec, but a
// declared minLength is not enforcement: nothing in this project runs an
// OpenAPI request validator, so until this check existed a five-character
// password was accepted with 204. A constraint that only exists in the document
// is a constraint the operator believes they have.
//
// It lives here rather than in the handler so the command line is held to it
// too — that path creates exactly the same account.
const MinPasswordLength = 12

// ErrPasswordTooShort lets callers answer 400 rather than 500 for what is a
// request problem, not a server problem.
var ErrPasswordTooShort = fmt.Errorf(
	"bootstrap: password must be at least %d characters", MinPasswordLength)

// FirstAdminLockKey serialises "create the first administrator" across
// processes.
//
// The check and the insert have to be one atomic step. Under READ COMMITTED,
// two transactions that begin together both take their snapshot before either
// commits, so both see an empty table, both pass WHERE NOT EXISTS, and both
// insert — different addresses, so no unique index saves us. The result is two
// administrators where the product promises one.
//
// Exported so that a test can hold the lock and prove this function waits for
// it. That is not decoration: a test that merely starts N goroutines and counts
// the survivors passes with the lock removed, because the password hashing in
// front of the transaction spreads the callers far enough apart that the window
// never opens.
const FirstAdminLockKey = 0x6661_6972_0001

// CreateFirstAdmin creates the initial administrator, and only if there is none.
//
// Returns ErrAlreadyConfigured when an account already exists. Callers decide
// what that means: start-up treats it as "nothing to do", the setup endpoint
// turns it into 409 — because by then it means someone else got there first,
// and quietly succeeding would tell the loser of that race that they now own an
// instance they do not.
func CreateFirstAdmin(ctx context.Context, pool *pgxpool.Pool, email, password, name string) error {
	if email == "" || password == "" {
		return errors.New("bootstrap: email and password are both required")
	}
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if name == "" {
		name = DefaultAdminName
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return fmt.Errorf("bootstrap: hashing the password: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Transaction-scoped: released on commit or rollback, including a crash,
	// so a process dying mid-setup cannot leave the wizard wedged.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", FirstAdminLockKey); err != nil {
		return fmt.Errorf("bootstrap: taking the first-administrator lock: %w", err)
	}

	var created bool
	err = tx.QueryRow(ctx, `
		INSERT INTO staff_users (email, password_hash, name, role)
		SELECT $1, $2, $3, 'superadmin'
		WHERE NOT EXISTS (SELECT 1 FROM staff_users)
		RETURNING true`, email, hash, name).Scan(&created)
	if db.IsNoRows(err) {
		return ErrAlreadyConfigured
	}
	if err != nil {
		return fmt.Errorf("bootstrap: creating the administrator: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bootstrap: commit: %w", err)
	}
	return nil
}

// SetPassword replaces an existing administrator's password.
//
// This is the way back in after a lost password. Without it the only remedy is
// creating a second account under a different address and abandoning the first,
// which is how installs end up with dead accounts nobody can remove.
func SetPassword(ctx context.Context, pool *pgxpool.Pool, email, password string) error {
	if email == "" || password == "" {
		return errors.New("bootstrap: email and password are both required")
	}
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return fmt.Errorf("bootstrap: hashing the password: %w", err)
	}
	tag, err := pool.Exec(ctx,
		`UPDATE staff_users SET password_hash = $2 WHERE email = $1`, email, hash)
	if err != nil {
		return fmt.Errorf("bootstrap: updating the password: %w", err)
	}
	// Reporting "no such account" is deliberate: this runs on a terminal the
	// operator already owns, and a silent no-op here reads as "password
	// changed" right up until the next failed sign-in.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("bootstrap: no administrator with the address %q", email)
	}
	return nil
}
