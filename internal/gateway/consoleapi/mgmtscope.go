package gwconsoleapi

import (
	"github.com/fairlb/fairlb/foundation/httpx"
)

// managementScopes is the allowlist of console API endpoints a management key
// can reach on the gateway's half of that API. Structurally the same as the
// allowlist for the other half; only the ownership of these endpoints differs.
// TestManagementScopesMatchSpec pins it to the annotations in the console spec.
//
// The scope literals are not imported from the package that defines them: these
// two strings are protocol constants, and repeating one is better than an
// import that points the wrong way across a layer boundary.
var managementScopes = map[string]string{
	"getUsage":            "management:usage:read",
	"exportUsageCSV":      "management:usage:read",
	"listRequestLogs":     "management:usage:read",
	"getRequestLog":       "management:usage:read",
	"exportLogsCSV":       "management:usage:read",
	"listAvailableModels": "management:usage:read",
	"listVideoJobs":       "management:usage:read",
	"getVideoJob":         "management:usage:read",
	"getVideoJobContent":  "management:usage:read",
	// cancelVideoJob and deleteVideoJob are deliberately absent: they are
	// session-only. A management key holds one of three scopes and none of them
	// is a write over usage, and minting a fourth to fit two buttons would
	// widen an externally reachable authentication surface as a side effect of
	// adding a screen. Neither operation is out of reach -- a person cancels
	// from the console, a program cancels through the data plane.
}

// RequireManagementScope is the authorization gate for management keys, applied
// to every endpoint in this package.
func RequireManagementScope(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return httpx.RequireManagementScope(managementScopes, f, operationID)
}
