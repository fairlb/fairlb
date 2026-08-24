package httpx

import (
	"net/http"
	"strings"
)

// CORS grants the data plane a cross-origin allowance for exactly one origin:
// allowedOrigin is echoed back, never `*`, and credentials are never enabled
// (the data plane authenticates with a bearer token or x-api-key, not cookies).
//
// It exists for split-host deployments where a browser app calls the data plane
// directly, so that the request the browser sends has the same host and the same
// shape as the one a customer sends with curl.
//
// allowedOrigin looks like "https://console.example.com"; the empty string means
// single-host mode and turns the whole middleware into a pass-through, the same
// switch shape CSRFGuard and HostGuard use.
//
// The 204 preflight short-circuit happens here, which is why this middleware
// must be installed *before* rate limiting: a rate-limited preflight shows up in
// the browser as an undiagnosable CORS failure, and answering one preflight
// costs the server about as much as answering a 404.
//
// A missing or non-matching Origin is passed through untouched. That branch is a
// bare call to next, byte-for-byte equivalent to this middleware not being
// installed at all (an OPTIONS request falls back to router method matching:
// 405 on a registered path, 404 otherwise), so non-browser clients cannot tell
// it is there.
//
// CORS does not weaken authentication. It only decides whether the browser lets
// a page *read* the response; key validation, metering and rate limiting apply
// to the actual request exactly as before. That is why the real-request branch
// only adds headers and never blocks.
func CORS(allowedOrigin string) func(http.Handler) http.Handler {
	want, wantOK := normalizeOrigin(allowedOrigin)
	return func(next http.Handler) http.Handler {
		if allowedOrigin == "" {
			return next
		}
		if !wantOK {
			panic("httpx: invalid CORS allowedOrigin: " + allowedOrigin)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			got, ok := normalizeOrigin(origin)
			if origin == "" || !ok || got != want {
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			// A response that varies by Origin must say so, even while the
			// current Cache-Control is no-store: the day the caching policy
			// changes, this must not be the line someone has to remember.
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Origin", origin)

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST")
				// authorization, content-type and anthropic-version are what a
				// browser-side client has to send; x-api-key is the equivalent
				// authentication header some upstream SDKs use, so it is
				// allowed alongside.
				h.Set("Access-Control-Allow-Headers",
					strings.Join([]string{"authorization", "content-type", "anthropic-version", "x-api-key"}, ", "))
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			// Correlating a response back to its usage record needs this
			// header; without an expose list the browser reads it as null.
			h.Set("Access-Control-Expose-Headers", HeaderRequestID)
			next.ServeHTTP(w, r)
		})
	}
}
