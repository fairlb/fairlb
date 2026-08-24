package apikeys

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/keyfmt"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// Management keys: the provisioning API.
//
// A organization can take a key carrying management:* scopes, call the console API
// with it as a bearer token, embed the gateway in their own product, and mint
// keys for their end users programmatically.
//
// This is a new externally reachable authentication surface, so all three of its
// constraints are structural rather than conventions each handler follows. A
// convention gets forgotten a whole group of endpoints at a time, and nothing
// reports it:
//   - the endpoint allowlist denies by default;
//   - a key is pinned to its own org (see Principal.OrgID and AuthorizeOrg);
//   - the scope is checked per endpoint, driven by the generated spec rather
//     than by each handler remembering.

const (
	// ScopeKeysRW allows reading and writing this org's API keys.
	ScopeKeysRW = "management:keys:rw"
	// ScopeUsageRead allows reading this org's usage and request logs.
	ScopeUsageRead = "management:usage:read"
	// ScopeInference is the default data-plane scope. It grants no management
	// permissions whatsoever.
	ScopeInference = "inference"
)

// ManagementAuthenticator accepts a bearer management key in addition to
// session authentication.
//
// The order is deliberate: session first, bearer second. A browser request
// carries a cookie and no Authorization header, a server-to-server request the
// reverse, so the two do not interfere. When both are present the session wins,
// because that means a person is driving and their permissions should be decided
// by the finer-grained membership role rather than a key's coarse scopes.
type ManagementAuthenticator struct {
	session httpx.Authenticator
	store   *Store
}

func NewManagementAuthenticator(session httpx.Authenticator, pool *pgxpool.Pool) *ManagementAuthenticator {
	return &ManagementAuthenticator{session: session, store: NewStore(pool)}
}

func (a *ManagementAuthenticator) Authenticate(r *http.Request) (httpx.Principal, error) {
	p, err := a.session.Authenticate(r)
	if err != nil {
		return p, err
	}
	if p.Subject != "" {
		return p, nil // the session wins
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return p, nil // anonymous; RequireUser turns this into a 401
	}
	return a.fromKey(r.Context(), p.Scope, token)
}

// fromKey resolves a management key into a subject.
//
// Every failure falls back to anonymous rather than returning an error: why an
// authentication attempt failed must not leak, since it feeds enumeration. The
// downstream RequireUser turns anonymity into a uniform 401.
func (a *ManagementAuthenticator) fromKey(ctx context.Context, plane, plaintext string) (httpx.Principal, error) {
	anon := httpx.Principal{Scope: plane}
	// A local format check (prefix plus checksum) that does not pass falls back
	// to anonymous, giving away no more than "no such key" does. Typos and
	// forged strings stop here without costing a database round trip.
	if !keyfmt.Valid(plaintext) {
		return anon, nil
	}
	row, err := a.store.KeyByHash(ctx, crypto.HashToken(plaintext))
	if db.IsNoRows(err) {
		return anon, nil
	}
	if err != nil {
		// An infrastructure failure fails closed to anonymous and is logged,
		// the same trade-off the session authenticator makes.
		slog.ErrorContext(ctx, "management key lookup failed", "error", err)
		return anon, nil
	}
	if row.Status != "active" {
		return anon, nil
	}
	if row.ExpiresAt.Valid && row.ExpiresAt.Time.Before(time.Now()) {
		return anon, nil
	}
	// Only keys carrying a management scope are accepted. A pure inference key
	// arriving here means the caller pointed a data-plane key at the management
	// plane — not insufficient permission but the wrong surface — and it is
	// treated as anonymous all the same.
	mgmt := make([]string, 0, len(row.Scopes))
	for _, s := range row.Scopes {
		if strings.HasPrefix(s, "management:") {
			mgmt = append(mgmt, s)
		}
	}
	if len(mgmt) == 0 {
		return anon, nil
	}
	return httpx.Principal{
		Scope: plane,
		// The subject is the key's own id. A key is not a user and must not
		// impersonate one; what shows up in the audit trail is this key.
		Subject: publicid.UUIDString(row.ID),
		Scopes:  mgmt,
		OrgID:   publicid.UUIDString(row.OrgID),
	}, nil
}

// AuthorizeOrg checks that a credential subject may act on this org and holds
// the required scope.
//
// Session subjects pass straight through: their authorization is decided from
// the membership role inside each service, not here.
func AuthorizeOrg(ctx context.Context, orgID pgtype.UUID, wantScope string) error {
	p := httpx.PrincipalFrom(ctx)
	if !p.IsCredential() {
		return nil
	}
	if p.OrgID != publicid.UUIDString(orgID) {
		// Same status as "no such org": never confirm an org's existence to a
		// key that has no business with it.
		return httpx.ErrCode(errcode.CommonNotFound)
	}
	if !p.HasScope(wantScope) {
		return httpx.ErrCodeDetail(errcode.CommonForbidden, "The management key lacks the "+wantScope+" scope")
	}
	return nil
}

// bearerToken extracts the value of a Bearer Authorization header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}
