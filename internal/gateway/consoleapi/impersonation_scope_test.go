package gwconsoleapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

func TestImpersonationScopeBlocksExportsAfterOrgBinding(t *testing.T) {
	f := newFixture(t)
	bound := publicid.UUIDString(f.orgA)
	ctx := httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "console", Subject: "user", Impersonator: "staff", ImpersonatedOrgID: bound,
	})

	request := func(org string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("org_id", org)
		return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	}
	next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
		return "called", nil
	}

	for _, operation := range []string{"ExportUsageCSV", "ExportLogsCSV"} {
		_, err := gwconsoleapi.RequireImpersonatedOrg(next, operation)(
			ctx, httptest.NewRecorder(), request(string(orgParam(f.orgA))), nil,
		)
		assertCode(t, err, errcode.CommonForbidden)
	}

	got, err := gwconsoleapi.RequireImpersonatedOrg(next, "GetUsage")(
		ctx, httptest.NewRecorder(), request(string(orgParam(f.orgA))), nil,
	)
	if err != nil || got != "called" {
		t.Fatalf("a plain read within the same organisation should reach the handler, got=%v err=%v", got, err)
	}

	_, err = gwconsoleapi.RequireImpersonatedOrg(next, "ExportUsageCSV")(
		ctx, httptest.NewRecorder(), request(string(orgParam(f.orgB))), nil,
	)
	assertCode(t, err, errcode.CommonNotFound)
}
