package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
)

func TestCheckManagementScopeHidesCrossOrgBeforeMissingScope(t *testing.T) {
	const requiredScope = "management:usage:read"
	scopes := map[string]string{"getUsage": requiredScope}

	tests := []struct {
		name      string
		principal httpx.Principal
		urlOrg    string
		wantCode  string
	}{
		{
			name: "wrong org and missing scope stays hidden",
			principal: httpx.Principal{
				OrgID: "org-a",
			},
			urlOrg:   "org-b",
			wantCode: errcode.CommonNotFound,
		},
		{
			name: "same org and missing scope is forbidden",
			principal: httpx.Principal{
				OrgID: "org-a",
			},
			urlOrg:   "org-a",
			wantCode: errcode.CommonForbidden,
		},
		{
			name: "same org and matching scope passes",
			principal: httpx.Principal{
				OrgID:  "org-a",
				Scopes: []string{requiredScope},
			},
			urlOrg: "org-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := requestWithRouteParam(t, "org_id", tt.urlOrg)
			ctx := httpx.WithPrincipal(context.Background(), tt.principal)
			err := httpx.CheckManagementScope(
				ctx, r, scopes, "GetUsage", "org_id", func(bound, requested string) bool {
					return bound == requested
				},
			)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("expected to be allowed, got %v", err)
				}
				return
			}
			var coded *httpx.CodeError
			if !errors.As(err, &coded) || coded.Code != tt.wantCode {
				t.Fatalf("want error code %s, got %v", tt.wantCode, err)
			}
		})
	}
}

func TestCheckManagementScopeKeepsUnscopedOperationsDefaultDeny(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := httpx.WithPrincipal(context.Background(), httpx.Principal{OrgID: "org-a"})
	err := httpx.CheckManagementScope(
		ctx, r, map[string]string{"getUsage": "management:usage:read"},
		"GetMe", "org_id", func(bound, requested string) bool { return bound == requested },
	)
	var coded *httpx.CodeError
	if !errors.As(err, &coded) || coded.Code != errcode.CommonForbidden {
		t.Fatalf("an org-less endpoint that is not allowlisted should be denied by default, got %v", err)
	}
}

func requestWithRouteParam(t *testing.T, name, value string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(name, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}
