// Package orgauthz decides organisation-level authorization.
//
// # What it stands in for
//
// The gateway's console endpoints delegate "may this caller read or write this
// organisation" to an injected decision. A multi-tenant deployment answers it
// from organisation membership roles. There is no membership table here: a
// single instance, a single administrator identity, and however many teams that
// administrator has made -- the operator is the user, and every team is theirs.
//
// # What is always permitted is the privilege ladder, not authentication
//
// Authentication happens earlier: the whole console plane sits behind the
// administrator session middleware, and nothing reaches this package without a
// session. What this decides is the role level and the organisation membership
// that follow a successful login, and there is no ladder to climb here.
//
// # Existence is still checked
//
// Always permitting is not the same as ignoring the parameter. An organisation
// in the URL that this instance does not have gets a 404 -- identical code and
// identical meaning to "not a member" elsewhere. Without that check, a
// fabricated organisation id would travel all the way to row-level security,
// and that layer returns an empty set rather than an error: "no data found" and
// "you may not look" have the same response shape, and the first reads as "no
// usage yet".
//
// The check is against the table rather than against one remembered id. Teams
// are created while the process runs, so a remembered id would refuse every
// team made after start-up -- and the symptom would be a page that renders
// empty for a team the operator can see in the team list.
package orgauthz

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/access/orgstatus"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	communitydb "github.com/fairlb/fairlb/internal/community/db"
)

// Authorizer implements the console API's OrgAuthorizer.
type Authorizer struct {
	orgs      *organizations.Store
	community *communitydb.Queries
}

func New(pool *pgxpool.Pool) *Authorizer {
	return &Authorizer{orgs: organizations.New(pool), community: communitydb.New(pool)}
}

// belongs reports whether the organisation in the URL is one this instance has.
// If not, the answer is 404, which hides existence.
//
// A lookup failure is also 404 rather than 500. The distinction the caller
// could draw from a 500 -- "this id exists but something broke" -- is exactly
// the one that must not be drawable, and every other refusal on this path is
// already 404.
func (a *Authorizer) belongs(ctx context.Context, orgID pgtype.UUID) error {
	if _, err := a.community.GetTeam(ctx, orgID); err != nil {
		return httpx.ErrCode(errcode.CommonNotFound)
	}
	return nil
}

// ResolveOrgReadAccess: the operator here sees every dimension, both costs and
// key metadata.
func (a *Authorizer) ResolveOrgReadAccess(ctx context.Context, orgID pgtype.UUID) (bool, bool, error) {
	if err := a.belongs(ctx, orgID); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// AuthorizeOrgAdminRead: the read-only view of sensitive configuration. The
// single identity here already holds the highest privilege.
func (a *Authorizer) AuthorizeOrgAdminRead(ctx context.Context, orgID pgtype.UUID) error {
	return a.belongs(ctx, orgID)
}

// AuthorizeOrgWrite: the write path. The organisation status gate still
// applies, with the same fail-closed semantics used everywhere else -- an
// unknown status is refused.
//
// "The organisation here is always active, so this can just permit" is an
// assumption with an expiry date: the column is writable, and a path that
// writes it may well appear later, such as an operator suspending it by hand.
// The check is cheap; skipping it saves one indexed lookup and buys a hole
// nobody notices until the day it matters.
func (a *Authorizer) AuthorizeOrgWrite(ctx context.Context, orgID pgtype.UUID) error {
	if err := a.belongs(ctx, orgID); err != nil {
		return err
	}
	status, err := a.orgs.Status(ctx, orgID)
	if err != nil {
		return err
	}
	return orgstatus.RequireWritable(status)
}
