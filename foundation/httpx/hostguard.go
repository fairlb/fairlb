package httpx

import (
	"net"
	"net/http"

	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/errcode"
)

// HostGuard enforces that a plane is only reachable under its own hostname. It
// is the application-layer backstop for plane isolation; in a multi-host
// deployment the edge should reject the mismatch first, but the backstop must
// exist because edge rules live outside this repository and can be forgotten.
// An empty want (single-host mode) makes it a pass-through.
func HostGuard(want string) func(http.Handler) http.Handler {
	want = config.NormalizeHost(want)
	return func(next http.Handler) http.Handler {
		if want == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if RequestHost(r) != want {
				Error(w, r, errcode.CommonNotFound, "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestHost returns the normalized request host — port stripped, lowercased,
// trailing dot removed — so that every routing decision compares the same form.
func RequestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return config.NormalizeHost(host)
}
