package gwconsoleapi

import (
	"context"
	"github.com/fairlb/fairlb/foundation/publicid"
	"net/http"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
)

var impersonationBlockedOperations = map[string]struct{}{
	"exportUsageCSV": {},
	"exportLogsCSV":  {},
}

// RequireImpersonatedOrg enforces the same-org boundary for the gateway's
// console endpoints. Usage, logs, models and BYOK all run it before reaching a
// handler, so no individual implementation can forget to apply it.
func RequireImpersonatedOrg(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if err := httpx.CheckImpersonatedOrg(ctx, r, "org_id", publicid.OrgMatches); err != nil {
			return nil, err
		}
		if httpx.PrincipalFrom(ctx).Impersonator != "" {
			if _, blocked := impersonationBlockedOperations[httpx.SpecOperationID(operationID)]; blocked {
				return nil, httpx.ErrCode(errcode.CommonForbidden)
			}
		}
		return f(ctx, w, r, request)
	}
}
