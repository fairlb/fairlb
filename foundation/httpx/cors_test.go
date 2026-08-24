package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// TestCORS covers the data plane's single-origin cross-origin allowance.
// The middleware only does two things: short-circuit a preflight with 204, and
// add headers to a real request. Everything else passes through untouched, so
// non-browser clients and non-matching origins cannot tell it is installed.
func TestCORS(t *testing.T) {
	const want = "https://console.example.com"

	newHandler := func(allowed string) (http.Handler, *bool) {
		reached := false
		h := httpx.CORS(allowed)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(200)
		}))
		return h, &reached
	}

	t.Run("empty_config_is_noop", func(t *testing.T) {
		h, reached := newHandler("")
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("Origin", want)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !*reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("single-host mode must pass through and write no CORS headers: reached=%v ACAO=%q",
				*reached, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	})

	t.Run("preflight_shortcircuits_with_full_header_set", func(t *testing.T) {
		h, reached := newHandler(want)
		req := httptest.NewRequest("OPTIONS", "/v1/messages", nil)
		req.Header.Set("Origin", want)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if *reached {
			t.Error("a preflight must short-circuit with 204 and never reach the rest of the chain")
		}
		if rec.Code != 204 {
			t.Errorf("a preflight should be 204: %d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("Allow-Origin should echo %q exactly: %q", want, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") || !strings.Contains(got, "GET") {
			t.Errorf("Allow-Methods should include GET and POST: %q", got)
		}
		// The three headers a browser client has to send, plus the equivalent
		// authentication header, asserted one by one (case-insensitively).
		allowHeaders := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
		for _, name := range []string{"authorization", "content-type", "anthropic-version", "x-api-key"} {
			if !strings.Contains(allowHeaders, name) {
				t.Errorf("Allow-Headers is missing %q: %q", name, allowHeaders)
			}
		}
		if rec.Header().Get("Access-Control-Max-Age") == "" {
			t.Error("a preflight should carry Max-Age: an authorization header plus JSON preflights every request, and without caching that doubles them")
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("a response that varies by origin must say Vary: Origin: %q", got)
		}
	})

	t.Run("actual_request_passes_through_with_headers", func(t *testing.T) {
		h, reached := newHandler(want)
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("Origin", want)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if !*reached {
			t.Error("a real request must pass through; authentication, metering and rate limiting still apply behind it")
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("Allow-Origin should echo the origin exactly: %q", got)
		}
		// Correlating a response with its usage record reads X-Request-Id;
		// without an expose list the browser reads it as null.
		if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.EqualFold(got, "X-Request-Id") {
			t.Errorf("Expose-Headers should be X-Request-Id: %q", got)
		}
	})

	t.Run("mismatched_origin_gets_no_cors_headers", func(t *testing.T) {
		h, reached := newHandler(want)
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !*reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("a non-matching origin must pass through untouched with no CORS headers: reached=%v ACAO=%q",
				*reached, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	})

	t.Run("options_without_request_method_is_not_preflight", func(t *testing.T) {
		// A bare OPTIONS with no Access-Control-Request-Method is not a
		// preflight and must pass through; short-circuiting it would swallow a
		// whole class of legitimate requests into a 204.
		h, reached := newHandler(want)
		req := httptest.NewRequest("OPTIONS", "/v1/messages", nil)
		req.Header.Set("Origin", want)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !*reached {
			t.Error("a non-preflight OPTIONS should pass through to the rest of the chain")
		}
	})

	t.Run("explicit_default_port_matches", func(t *testing.T) {
		// Browsers do not send :443, but an intermediary may rewrite the
		// header; normalization must still make it match.
		h, _ := newHandler(want)
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("Origin", "https://console.example.com:443")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") == "" {
			t.Error("the same origin with an explicit default port should match")
		}
	})
}
