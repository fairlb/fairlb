// Package orgscope runs a piece of work inside one organization's row-level
// security scope, having first resolved what the caller may read.
//
// The order is the point, and it is a connection-pool safety boundary rather
// than one query saved: the injected authorizer uses the same pool as the work
// it authorizes. Calling it from inside the transaction callback means the
// transaction already holds one connection and the authorizer acquires a
// second; add a long-lived LISTEN connection in production and a small pool
// deadlocks against itself.
//
// It is its own package because it is neither transport nor domain: the handler
// above it deals in DTOs, the readers below it take a querier, and this is the
// piece that turns "who is asking about which organization" into "a transaction
// scoped to that organization".
package orgscope

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Authorizer answers what a caller may do with one organization.
type Authorizer interface {
	// ResolveOrgReadAccess: basic member read plus the finance and key-metadata
	// dimensions, all from one snapshot. No org-status gate.
	ResolveOrgReadAccess(ctx context.Context, orgID pgtype.UUID) (finance bool, keyMetadata bool, err error)
	// AuthorizeOrgAdminRead: the sensitive configuration surface an owner or
	// admin may read. No org-status gate.
	AuthorizeOrgAdminRead(ctx context.Context, orgID pgtype.UUID) error
	// AuthorizeOrgWrite: admin or above, and the org's status permits writes
	// (suspended gives 403, being deleted gives 409).
	AuthorizeOrgWrite(ctx context.Context, orgID pgtype.UUID) error
}

// Access is which sensitive dimensions this caller may see.
type Access struct {
	Finance     bool
	KeyMetadata bool
}

// Requirements is which of them the work about to run needs.
type Requirements struct {
	Finance     bool
	KeyMetadata bool
}

// Runner holds the pool and the authorizer.
type Runner struct {
	pool  *pgxpool.Pool
	q     *gwdb.Queries
	authz Authorizer
}

func New(pool *pgxpool.Pool, authz Authorizer) *Runner {
	return &Runner{pool: pool, q: gwdb.New(pool), authz: authz}
}

// Read resolves basic access and the sensitive dimensions, then runs fn inside
// the organization's transaction.
func (r *Runner) Read(
	ctx context.Context, orgPublicID string, require Requirements,
	fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, access Access) error,
) error {
	org, access, err := r.Access(ctx, orgPublicID)
	if err != nil {
		return err
	}
	if (require.Finance && !access.Finance) || (require.KeyMetadata && !access.KeyMetadata) {
		return httpx.ErrCode(errcode.CommonForbidden)
	}
	return db.WithOrgTx(ctx, r.pool, publicid.UUIDString(org), func(tx pgx.Tx) error {
		return fn(ctx, r.q.WithTx(tx), org, access)
	})
}

// Access resolves the organization id and the caller's dimensions without
// opening a transaction.
func (r *Runner) Access(ctx context.Context, orgPublicID string) (pgtype.UUID, Access, error) {
	org, err := publicid.Parse(publicid.Org, orgPublicID)
	if err != nil {
		return pgtype.UUID{}, Access{}, httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid org_id")
	}
	finance, keyMetadata, err := r.authz.ResolveOrgReadAccess(ctx, org)
	if err != nil {
		return pgtype.UUID{}, Access{}, err
	}
	return org, Access{Finance: finance, KeyMetadata: keyMetadata}, nil
}

// AdminRead serves the read-only view of sensitive configuration such as BYOK:
// admin or above, but a suspended org can still read.
func (r *Runner) AdminRead(
	ctx context.Context, orgPublicID string,
	fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error,
) error {
	return r.scoped(ctx, orgPublicID, r.authz.AuthorizeOrgAdminRead, fn)
}

// Write is the write path: admin or above, and the org's status permits writes.
func (r *Runner) Write(
	ctx context.Context, orgPublicID string,
	fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error,
) error {
	return r.scoped(ctx, orgPublicID, r.authz.AuthorizeOrgWrite, fn)
}

func (r *Runner) scoped(
	ctx context.Context, orgPublicID string,
	authorize func(context.Context, pgtype.UUID) error,
	fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error,
) error {
	org, err := publicid.Parse(publicid.Org, orgPublicID)
	if err != nil {
		return httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid org_id")
	}
	if err := authorize(ctx, org); err != nil {
		return err
	}
	return db.WithOrgTx(ctx, r.pool, publicid.UUIDString(org), func(tx pgx.Tx) error {
		return fn(ctx, r.q.WithTx(tx), org)
	})
}
