package httpx

import (
	"context"
	"github.com/fairlb/fairlb/foundation/publicid"
	"net/http"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// Authorization gate for management keys.
//
// It is table-driven and denies by default rather than being a check each
// handler remembers to call. Per-handler checks fail in the one direction that
// cannot be noticed: a whole group of endpoints ships without an authorization
// call and every response still looks correct. With a table, an endpoint that is
// not listed rejects credential subjects outright, so "forgot to annotate it"
// costs a 403 instead of an open door.
//
// Each API package installs its own thin middleware — the generated strict
// handler type is package-local, so there is no single place to wrap — but the
// decision logic lives only here.

// SpecOperationID folds the generated Go method name back to the spec's
// operationId.
//
// The code generator hands middleware the PascalCase Go name (ListApiKeys),
// while the scope tables are written with the spec's camelCase name
// (listApiKeys). The spec name is the one in the published contract and the one
// that can be reconciled against the spec file directly. The two differ only in
// the first letter, so folding beats maintaining a second mapping table.
// TestManagementScopesMatchSpec covers this conversion: if the generator ever
// changes its naming style, that test goes red.
func SpecOperationID(goName string) string {
	if goName == "" {
		return ""
	}
	r := []rune(goName)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// CheckManagementScope decides whether a credential subject may call an
// operation.
//
// Session subjects pass straight through: their authorization comes from org
// membership and is decided in the services, not here. scopes is an allowlist
// mapping operationId to the required scope; anything not in the table is
// denied. orgParam is the name of the org path parameter, used to pin the
// credential to its own org; an empty string means the endpoint has no org
// dimension. When there is one, the org binding is checked *before* the
// operation allowlist and the scope, so a 403 never confirms to an out-of-scope
// credential that some other org exists.
func CheckManagementScope(
	ctx context.Context, r *http.Request,
	scopes map[string]string, operationID, orgParam string,
	sameOrg func(principalOrg, urlOrg string) bool,
) error {
	p := PrincipalFrom(ctx)
	if !p.IsCredential() {
		return nil
	}
	var urlOrg string
	if orgParam != "" {
		urlOrg = chi.URLParam(r, orgParam)
		// Same status as "no such org": never confirm an org's existence to
		// a key that has no business with it.
		if urlOrg != "" && !sameOrg(p.OrgID, urlOrg) {
			return ErrCode(errcode.CommonNotFound)
		}
	}
	want, ok := scopes[SpecOperationID(operationID)]
	if !ok {
		return ErrCodeDetail(errcode.CommonForbidden, "This endpoint is not available to management keys")
	}
	if !p.HasScope(want) {
		return ErrCodeDetail(errcode.CommonForbidden, "The management key lacks the "+want+" scope")
	}
	if orgParam != "" && urlOrg == "" {
		return ErrCode(errcode.CommonNotFound)
	}
	return nil
}

// CheckImpersonatedOrg pins an impersonation session to the org that was chosen
// when it was issued. Ordinary sessions and management credentials are
// unaffected. Operations without an org path parameter are denied by default for
// impersonation, and only the endpoints an API package explicitly lists are
// allowed. An org mismatch is reported as 404 so the caller is never told
// whether another org exists.
func CheckImpersonatedOrg(
	ctx context.Context,
	r *http.Request,
	orgParam string,
	sameOrg func(principalOrg, urlOrg string) bool,
) error {
	return CheckImpersonatedOrgOperation(ctx, r, "", nil, orgParam, sameOrg)
}

// CheckImpersonatedOrgOperation adds an operation allowlist to that boundary.
// The keys of allowedUnscoped are spec operationIds; the generated Go method
// name is folded back through SpecOperationID. Each API package owns its own
// list, so the shared layer never has to guess which org-less endpoints are safe
// to expose during impersonation.
func CheckImpersonatedOrgOperation(
	ctx context.Context,
	r *http.Request,
	operationID string,
	allowedUnscoped map[string]struct{},
	orgParam string,
	sameOrg func(principalOrg, urlOrg string) bool,
) error {
	p := PrincipalFrom(ctx)
	if p.Impersonator == "" {
		return nil
	}
	if orgParam == "" {
		return ErrCode(errcode.CommonForbidden)
	}
	urlOrg := chi.URLParam(r, orgParam)
	// The strict middleware wraps the whole API, so an empty value means this
	// operation simply has no such parameter — not that an org-scoped route
	// slipped past the binding. Org-less operations must be allowlisted one by
	// one; otherwise every newly added account-level endpoint silently widens
	// what an impersonation session can reach.
	if urlOrg == "" {
		if _, ok := allowedUnscoped[SpecOperationID(operationID)]; ok {
			return nil
		}
		return ErrCode(errcode.CommonForbidden)
	}
	if p.ImpersonatedOrgID == "" || !sameOrg(p.ImpersonatedOrgID, urlOrg) {
		return ErrCode(errcode.CommonNotFound)
	}
	return nil
}

// StrictHandler is the shape every oapi-codegen strict handler has. Each
// generated package declares its own named type for it, which is why the gate
// below is generic: the same body, returned as the caller's type.
type StrictHandler = func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error)

// RequireManagementScope wraps f so that a management-key principal must hold
// the scope `scopes` assigns to operationID, and must address its own
// organization (`org_id` path parameter, compared through publicid.OrgMatches).
// Session principals pass through untouched; see CheckManagementScope.
func RequireManagementScope[F ~StrictHandler](scopes map[string]string, f F, operationID string) F {
	return F(func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if err := CheckManagementScope(ctx, r, scopes, operationID, "org_id", publicid.OrgMatches); err != nil {
			return nil, err
		}
		return f(ctx, w, r, request)
	})
}
