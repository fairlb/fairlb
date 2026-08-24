package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// Principal is the request subject produced by an authentication domain.
// Browser planes get theirs from a session resolver, the data plane from an API
// key resolver.
type Principal struct {
	Scope   string // authentication domain: console | admin | dataplane | webhooks
	Subject string // subject id (internal uuid string); empty when anonymous
	// Role is the subject's role on the admin plane (superadmin | operator),
	// filled in by that plane's authenticator and empty elsewhere.
	Role string
	// Impersonator, when non-empty, marks a staff impersonation session; the
	// value is the staff user's internal uuid string.
	Impersonator string
	// ImpersonatedOrgID is the org this impersonation session was pinned to when
	// it was issued; empty for ordinary sessions. It deliberately does not reuse
	// OrgID: a non-empty OrgID is the test for "this is a management key", so
	// sharing the field would make an impersonation session look like credential
	// authentication.
	ImpersonatedOrgID string
	// Scopes is the permission set carried by the credential itself (the
	// management:* scopes of a management key). Empty for session subjects — a
	// session's permissions come from org membership, not from the credential.
	Scopes []string
	// OrgID non-empty means the credential is pinned to exactly this org (a
	// management key). Empty for sessions: one user can belong to several orgs,
	// so the credential itself is not org-bound.
	OrgID string
}

// HasScope reports whether the subject holds a given scope.
func (p Principal) HasScope(want string) bool {
	for _, s := range p.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// IsCredential reports whether the subject authenticated with a credential (a
// management key) rather than a session. The test is OrgID rather than "Scopes
// is non-empty": a credential with no scopes is still a credential, and
// mistaking it for a session would let a scopeless key skip scope checks
// entirely.
func (p Principal) IsCredential() bool { return p.OrgID != "" }

// Authenticator is the extension point each plane implements to resolve its own
// credentials.
type Authenticator interface {
	// Authenticate resolves the request subject; an error means the request is
	// rejected with a 401 problem document.
	Authenticate(r *http.Request) (Principal, error)
}

// AnonymousAuthenticator admits every request and only stamps the plane's scope.
type AnonymousAuthenticator struct{ Scope string }

func (a AnonymousAuthenticator) Authenticate(*http.Request) (Principal, error) {
	return Principal{Scope: a.Scope}, nil
}

// AuthDomain puts the authenticator's principal into the request context;
// a resolution failure renders a 401.
func AuthDomain(auth Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := auth.Authenticate(r)
			if err != nil {
				// The underlying error goes to the log only: the reason an
				// authentication attempt failed must not leak to the caller,
				// since it enables enumeration and exposes internals.
				slog.WarnContext(r.Context(), "authentication failed", "error", err)
				Error(w, r, errcode.CommonUnauthenticated, "")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, p)))
		})
	}
}

// RequireAuthenticated rejects anonymous subjects before request binding.
//
// Generated strict middleware only runs after path parameters, required headers
// and the body have all been parsed. Authenticating only there means an
// anonymous request gets a 400 for a malformed UUID or If-Match value before it
// ever gets a 401 — which leaks the shape of the endpoint and makes the 401
// semantics of a protected namespace inconsistent. Install this after
// AuthDomain; combine it with ForPathPrefix to protect only the private subtree
// of a shared router.
func RequireAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if PrincipalFrom(r.Context()).Subject == "" {
			Error(w, r, errcode.CommonUnauthenticated, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ExceptPaths applies mw to every request *except* the listed ones; keys are
// "METHOD /full/path".
//
// This is the opposite direction from ForPathPrefix ("protect only the named
// subtree"): here the whole plane is protected and only the named few are let
// through. Defaulting to deny is deliberate — paired with RequireAuthenticated,
// a newly added endpoint is protected without anyone remembering to say so, and
// forgetting to register an exemption costs a 401 rather than an open door. The
// opposite default fails silently: an entire namespace can ship unauthenticated
// while every automated check stays green.
//
// The exemption list matches static paths only, so an operation with a path
// parameter can never match and therefore can never be exempted. Same
// fail-closed direction.
func ExceptPaths(exempt map[string]bool, mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt[r.Method+" "+r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

// WithPrincipal puts a subject into the context. In production the AuthDomain
// middleware does this; the function is exported so tests can build an
// authenticated context without standing up the whole middleware stack.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// PrincipalFrom returns the current subject; outside an authentication domain
// it returns the zero value.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKeyPrincipal).(Principal)
	return p
}
