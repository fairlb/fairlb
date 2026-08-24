// Package staffapi implements the administrator-facing endpoints specific to
// this deployment shape.
//
// # Only two groups live here
//
// Identity (log in, log out, who am I) and API key management. The provider,
// model and route configuration endpoints come from the shared administrator
// specification and its generated handler, and are not repeated here.
//
// These two are specific for different reasons. Identity differs in mechanism:
// a multi-tenant operations desk gates login on TOTP and an IP allowlist, which
// a single self-hosted instance does not need. Key management differs in
// dimension: elsewhere it lives on a per-organisation self-service surface,
// and there is no organization layer here.
//
// # How the cookie is issued
//
// A strict handler's method signature carries only a context, so it cannot see
// the ResponseWriter -- but the response object can. So two custom response
// types are defined here and set the cookie inside their Visit method. That is
// cleaner than bypassing the generated handler and hand-writing routes: the
// endpoints stay bound to the specification, and parameter validation, error
// rendering and the middleware stack all keep working.
package staffapi

import (
	"context"
	"github.com/fairlb/fairlb/settings"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/access/staff"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	communitydb "github.com/fairlb/fairlb/internal/community/db"
	"github.com/fairlb/fairlb/internal/community/staffauth"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// Server implements the administrator endpoints specific to this deployment.
type Server struct {
	svc   *staffauth.Service
	pool  *pgxpool.Pool
	staff *staff.Store
	// community is this deployment's own query set: the team lifecycle, which a
	// hosted deployment owns entirely differently and must not share.
	community *communitydb.Queries
	keys      *apikeys.Service
	// settings is the registry this deployment assembled (the gateway layer):
	// the settings endpoints list and write exactly what it holds (ADR-0198).
	settings *settings.Store
	// orgID is the team created at first start: where a key lands when the
	// request names none, and the one team that cannot be suspended.
	orgID pgtype.UUID
	// keyCache is the data plane's identity cache. Suspending a team has to
	// reach it immediately; nil means no cache is configured, so reads go to
	// the database and there is no stale copy to clear.
	keyCache cache.Store
	// secure reports whether session cookies may carry the Secure attribute;
	// it follows the instance's public URL scheme.
	secure bool
	setup  SetupConfig
}

func NewServer(
	pool *pgxpool.Pool, svc *staffauth.Service, keys *apikeys.Service,
	orgID pgtype.UUID, keyCache cache.Store, secure bool, setup SetupConfig, set *settings.Store,
) *Server {
	return &Server{
		svc: svc, pool: pool, staff: staff.New(pool), community: communitydb.New(pool), keys: keys,
		orgID: orgID, keyCache: keyCache, secure: secure, setup: setup, settings: set,
	}
}

// invalidateTeamKeyCache drops the data-plane snapshot of every active key in a
// team.
//
// Suspending a team is a refusal, and a refusal that takes effect "in a little
// while" is not one. The cache is keyed by key hash, so invalidating by team
// means looking those hashes up first -- the same shape, and the same reason,
// as the gateway's own tier writes.
//
// A failure logs rather than failing the request: the status is already
// committed, so returning an error would only make the operator try again and
// store the same value, while the cache that needed clearing still would not be
// cleared. The honest outcome is to say so and let the TTL close the gap.
func (s *Server) invalidateTeamKeyCache(ctx context.Context, teamID pgtype.UUID) {
	if s.keyCache == nil {
		return
	}
	hashes, err := s.community.ListActiveKeyHashesForTeam(ctx, teamID)
	if err != nil {
		slog.ErrorContext(ctx, "could not look up the team's key hashes; the data plane "+
			"keeps serving them until their cache entries expire", "error", err)
		return
	}
	for _, h := range hashes {
		if err := s.keyCache.Delete(ctx, proxy.KeyCacheKey(h)); err != nil {
			slog.ErrorContext(ctx, "data-plane key cache invalidation failed; this key "+
				"keeps working until its entry expires", "error", err)
		}
	}
}

// AllowKeyAdmin implements the key-administration check: being logged in is
// enough to manage keys.
//
// Always permitting is not the absence of authorization. Authentication happens
// earlier: these endpoints sit behind the session middleware and nothing
// reaches them without a session. What this decides is the privilege level
// after login, and a single instance with a single administrator identity has
// no levels to distinguish.
func AllowKeyAdmin(context.Context, pgtype.UUID, pgtype.UUID) error { return nil }

var _ StrictServerInterface = (*Server)(nil)

// sessionCookie is the response issued on a successful login: 204 with a
// Set-Cookie header.
//
// It is a custom response type rather than the generated 204 response, whose
// Visit only writes the status code. The cookie attributes (HttpOnly, Secure,
// SameSite, and the __Host- prefix contract) are a security boundary and have
// to be guaranteed by code, not left to whoever calls it to remember.
type sessionCookie struct {
	token  string
	secure bool
}

func (r sessionCookie) VisitCommunityLoginResponse(w http.ResponseWriter) error {
	http.SetCookie(w, &http.Cookie{
		Name: staffauth.SessionCookieName(r.secure), Value: r.token, Path: "/",
		HttpOnly: true, Secure: r.secure, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type clearCookie struct{ secure bool }

func (r clearCookie) VisitCommunityLogoutResponse(w http.ResponseWriter) error {
	http.SetCookie(w, &http.Cookie{
		Name: staffauth.SessionCookieName(r.secure), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.secure, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) CommunityLogin(ctx context.Context, req CommunityLoginRequestObject) (CommunityLoginResponseObject, error) {
	if req.Body == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "request body is required")
	}
	token, err := s.svc.Login(ctx, string(req.Body.Email), req.Body.Password)
	if err != nil {
		return nil, err // wrong password and unknown account share one 401
	}
	return sessionCookie{token: token, secure: s.secure}, nil
}

func (s *Server) CommunityLogout(ctx context.Context, _ CommunityLogoutRequestObject) (CommunityLogoutResponseObject, error) {
	// Idempotent: 204 even with no session. A strict handler is not given the
	// original request, so logout revokes by the principal the middleware
	// already resolved -- no principal means no session to revoke.
	if p := httpx.PrincipalFrom(ctx); p.Subject != "" {
		if err := s.svc.LogoutBySubject(ctx, p.Subject); err != nil {
			return nil, err
		}
	}
	return clearCookie{secure: s.secure}, nil
}

func (s *Server) CommunityMe(ctx context.Context, _ CommunityMeRequestObject) (CommunityMeResponseObject, error) {
	p := httpx.PrincipalFrom(ctx)
	if p.Subject == "" {
		return nil, httpx.ErrCode(errcode.CommonUnauthenticated)
	}
	var id pgtype.UUID
	if err := id.Scan(p.Subject); err != nil {
		return nil, httpx.ErrCode(errcode.CommonUnauthenticated)
	}
	u, err := s.staff.ByID(ctx, id)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrCode(errcode.CommonUnauthenticated)
		}
		return nil, err
	}
	var oid openapi_types.UUID
	if err := oid.UnmarshalText([]byte(p.Subject)); err != nil {
		return nil, httpx.ErrCode(errcode.CommonUnauthenticated)
	}
	return CommunityMe200JSONResponse{
		Id: oid, Email: openapi_types.Email(u.Email), Name: u.Name, Role: Superadmin,
		OrgId: publicid.Format(publicid.Org, s.orgID),
	}, nil
}

// WithHTTPRequest is the strict middleware that puts the raw request into the
// context, for the handlers that record who did something from where.
func WithHTTPRequest(f StrictHandlerFunc, _ string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		return f(httpx.WithRequest(ctx, r), w, r, request)
	}
}
