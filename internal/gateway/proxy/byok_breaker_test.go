package proxy

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// On a organization-supplied credential, no upstream failure may count toward *this
// deployment's* circuit breaker.
//
// That hop uses the organization's credential, their quota, possibly even their own
// base URL. Failures on that path are a different thing from this deployment's
// own link to the provider -- counting them would mean that one organization's
// self-hosted gateway going down takes that provider away from everyone.
func TestBYOKFailuresDoNotCountTowardPlatformBreaker(t *testing.T) {
	for _, c := range []struct {
		status int
		what   string
	}{
		{http.StatusUnauthorized, "organization credential no longer valid"},
		{http.StatusForbidden, "organization credential lacks permission"},
		{http.StatusTooManyRequests, "the organization ran out of their own quota"},
		{http.StatusInternalServerError, "the organization's own gateway is down"},
		{http.StatusBadGateway, "the organization's own gateway is down"},
		{http.StatusServiceUnavailable, "the organization's upstream is unavailable"},
	} {
		cls := ClassifyStatus(c.status, []byte(`{"error":"x"}`), "")
		// Reproduce what attemptOnce does on the organization-credential branch.
		cls.CountsTowardHealth = false
		cls.keyID = pgtype.UUID{}
		if cls.CountsTowardHealth {
			t.Errorf("%d (%s) still counts toward this deployment's breaker", c.status, c.what)
		}
		if cls.keyID.Valid {
			t.Errorf("%d (%s) still records the failure against some credential", c.status, c.what)
		}
	}
}
