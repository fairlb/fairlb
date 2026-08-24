package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// TestCSRFGuard covers the cross-site write defense on cookie planes: Origin
// first, Referer's origin as a fallback, deny when both are absent. The
// comparison includes scheme and port.
func TestCSRFGuard(t *testing.T) {
	h := httpx.CSRFGuard("https://console.example.com")(http.HandlerFunc(ok200))
	cases := []struct {
		name, method, origin, referer string
		want                          int
	}{
		{"safe_method_cross_origin", "GET", "https://evil.example", "", 200},
		{"same_origin", "POST", "https://console.example.com", "", 200},
		{"same_origin_explicit_default_port", "POST", "https://console.example.com:443", "", 200},
		{"cross_origin", "POST", "https://evil.example", "", 403},
		{"scheme_downgrade", "POST", "http://console.example.com", "", 403},
		{"opaque_origin", "POST", "null", "", 403},
		{"referer_fallback_same_origin", "POST", "", "https://console.example.com/settings/security", 200},
		{"referer_cross_origin", "POST", "", "https://evil.example/page", 403},
		{"both_missing", "POST", "", "", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "https://console.example.com/api/v1/x", nil)
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.referer != "" {
				req.Header.Set("Referer", c.referer)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("want %d, got %d", c.want, rec.Code)
			}
		})
	}

	// An empty allowedOrigin means single-host mode: the guard is disabled.
	off := httpx.CSRFGuard("")(http.HandlerFunc(ok200))
	req := httptest.NewRequest("POST", "https://console.example.com/api/v1/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("an empty configuration should pass through: %d", rec.Code)
	}

	// An invalid allowedOrigin panics at assembly time: a misconfigured
	// security boundary must fail fast, not silently admit everything.
	defer func() {
		if recover() == nil {
			t.Fatal("an invalid allowedOrigin should panic")
		}
	}()
	httpx.CSRFGuard("not a url")(http.HandlerFunc(ok200))
}

// Three states of the SPA CSP. The zero-value extra must produce the baseline
// policy byte for byte — a regression pin on "no frame-src means fall back to
// default-src" — and the widget directives must appear only when supplied.
func TestSPASecurityHeaders(t *testing.T) {
	get := func(extra httpx.CSPExtra) string {
		h := httpx.SPASecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), extra)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Header().Get("Content-Security-Policy")
	}

	const baseline = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	if got := get(httpx.CSPExtra{}); got != baseline {
		t.Fatalf("the CSP for a zero-value extra deviates from the baseline:\n got  %q\n want %q", got, baseline)
	}

	if got := get(httpx.CSPExtra{ConnectSrc: []string{"https://api.example.com", ""}}); !strings.Contains(got, "connect-src 'self' https://api.example.com;") {
		t.Fatalf("the connect-src addition did not apply, or the empty string was not filtered: %q", got)
	}

	const ts = "https://challenges.cloudflare.com"
	got := get(httpx.CSPExtra{ScriptSrc: []string{ts}, FrameSrc: []string{ts}})
	if !strings.Contains(got, "script-src 'self' "+ts+";") {
		t.Fatalf("script-src did not allow the widget origin: %q", got)
	}
	if !strings.Contains(got, "frame-src 'self' "+ts+";") {
		t.Fatalf("frame-src did not allow the widget origin, or is missing 'self'; an explicit frame-src replaces the default-src fallback: %q", got)
	}
}

// The single-host CSRF test compares hostnames only, so the common shape where
// TLS terminates at a reverse proxy (browser sends https, the app sees
// host:8080) must be admitted while a different hostname must be rejected.
func TestCSRFGuardSameHost(t *testing.T) {
	h := httpx.CSRFGuardSameHost()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, c := range []struct {
		name    string
		method  string
		host    string
		headers map[string]string
		want    int
	}{
		{"GET passes through", http.MethodGet, "gw.example.com", nil, http.StatusNoContent},
		{
			"same hostname in Origin", http.MethodPost, "gw.example.com",
			map[string]string{"Origin": "https://gw.example.com"},
			http.StatusNoContent,
		},
		{
			"TLS terminated at a proxy: scheme and port differ, hostname matches", http.MethodPost, "gw.example.com:8080",
			map[string]string{"Origin": "https://gw.example.com"},
			http.StatusNoContent,
		},
		{
			"a different hostname", http.MethodPost, "gw.example.com",
			map[string]string{"Origin": "https://evil.example.com"},
			http.StatusForbidden,
		},
		{
			"no Origin, falling back to Referer", http.MethodPost, "gw.example.com",
			map[string]string{"Referer": "https://gw.example.com/keys"},
			http.StatusNoContent,
		},
		{
			"Referer is someone else too", http.MethodPost, "gw.example.com",
			map[string]string{"Referer": "https://evil.example.com/x"},
			http.StatusForbidden,
		},
		{"neither header: denied by default", http.MethodPost, "gw.example.com", nil, http.StatusForbidden},
		{
			"bearer passes through: browsers never attach one automatically", http.MethodPost, "gw.example.com",
			map[string]string{"Authorization": "Bearer sk-x"},
			http.StatusNoContent,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "http://"+c.host+"/api/v1/x", nil)
			r.Host = c.host
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("want %d, got %d", c.want, w.Code)
			}
		})
	}
}
