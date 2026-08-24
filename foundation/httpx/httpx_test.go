package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
)

func ok200(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func TestRequestID(t *testing.T) {
	var seen string
	h := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestIDFrom(r.Context())
		ok200(w, r)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	got := rec.Header().Get(httpx.HeaderRequestID)
	if got == "" || got != seen || !strings.HasPrefix(got, "req_") {
		t.Errorf("the response header and the context should carry the same req_ id: header=%q ctx=%q", got, seen)
	}
}

func TestErrorRendersProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, errcode.CommonNotFound, "some detail")
	})).ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Status != 404 || p.Code != errcode.CommonNotFound || p.Detail != "some detail" || p.RequestID == "" {
		t.Errorf("problem fields are wrong: %+v", p)
	}
	if rec.Code != 404 {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	h := httpx.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), errcode.CommonInternal) {
		t.Errorf("a panic should render a 500 problem: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

type denyAuth struct{}

func (denyAuth) Authenticate(*http.Request) (httpx.Principal, error) {
	return httpx.Principal{}, errors.New("no valid session")
}

func TestAuthDomain(t *testing.T) {
	var p httpx.Principal
	h := httpx.AuthDomain(httpx.AnonymousAuthenticator{Scope: "console"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p = httpx.PrincipalFrom(r.Context())
			ok200(w, r)
		}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if p.Scope != "console" {
		t.Errorf("scope = %q", p.Scope)
	}

	rec := httptest.NewRecorder()
	httpx.AuthDomain(denyAuth{})(http.HandlerFunc(ok200)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), errcode.CommonUnauthenticated) {
		t.Errorf("a refused authentication should be a 401 problem: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no valid session") {
		t.Errorf("the authentication error detail must not leak: %s", rec.Body.String())
	}
}

func TestRequireAuthenticatedRunsBeforeHandler(t *testing.T) {
	called := false
	h := httpx.RequireAuthenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		ok200(w, r)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/private", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("anonymous = %d called=%v, want 401 before handler", rec.Code, called)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/private", nil)
	req = req.WithContext(httpx.WithPrincipal(req.Context(), httpx.Principal{
		Scope: "admin", Subject: "00000000-0000-7000-8000-000000000001",
	}))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("authenticated = %d called=%v, want 200", rec.Code, called)
	}
}

// The generated server's error exits and 405 all go through the problem
// renderer.
func TestOAPIErrorHandlersRenderProblem(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)

	rec := httptest.NewRecorder()
	httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.OAPIRequestError(w, r, errors.New("binding: internal detail"))
	})).ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Header().Get("Content-Type"), "problem+json") {
		t.Errorf("a request error should be a 400 problem: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "internal detail") {
		t.Errorf("the binding error detail must not leak: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	httpx.OAPIResponseError(rec, req, errors.New("boom"))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), errcode.CommonInternal) {
		t.Errorf("a response error should be a 500 problem: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	httpx.MethodNotAllowedHandler()(rec, req)
	if rec.Code != 405 || !strings.Contains(rec.Body.String(), errcode.CommonMethodNotAllowed) {
		t.Errorf("405 should carry its own error code: %d %s", rec.Code, rec.Body.String())
	}
}

type captureHook struct {
	evs     []httpx.AuditEvent
	audited []bool // value of WasAudited at each Record call (dedup semantics)
}

func (c *captureHook) Record(ctx context.Context, ev httpx.AuditEvent) {
	c.evs = append(c.evs, ev)
	c.audited = append(c.audited, httpx.WasAudited(ctx))
}

func TestAuditOnlyMutations(t *testing.T) {
	hook := &captureHook{}
	h := httpx.Audit(hook)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/b", nil))

	if len(hook.evs) != 1 {
		t.Fatalf("only write operations should be audited, got %d", len(hook.evs))
	}
	if ev := hook.evs[0]; ev.Method != "POST" || ev.Path != "/b" || ev.Status != 201 {
		t.Errorf("the audit event is wrong: %+v", ev)
	}
}

// A write request whose handler panics still produces an audit record (status
// 500), and the panic is then re-raised for the recovery middleware.
func TestAuditPanicStillRecorded(t *testing.T) {
	hook := &captureHook{}
	// Same wiring as production: Recover outside, Audit inside, so Recover
	// catches the panic Audit re-raises and renders the 500.
	h := httpx.Recover(httpx.Audit(hook)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))

	if len(hook.evs) != 1 {
		t.Fatalf("a panicking write request should produce 1 audit record, got %d", len(hook.evs))
	}
	if hook.evs[0].Status != http.StatusInternalServerError {
		t.Errorf("the audit status for a panic should be 500, got %d", hook.evs[0].Status)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Recover should render a 500, got %d", rec.Code)
	}
}

// Dedup flag: after MarkAudited, WasAudited is true, which is how the database
// hook knows to skip the fallback row.
func TestAuditMarkDedup(t *testing.T) {
	hook := &captureHook{}
	h := httpx.Audit(hook)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.MarkAudited(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/c", nil))

	if len(hook.audited) != 1 || !hook.audited[0] {
		t.Fatalf("WasAudited should be true after MarkAudited, got %v", hook.audited)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	h := httpx.RateLimit(ratelimit.NewMemory(), httpx.IPKey, 2, time.Minute)(http.HandlerFunc(ok200))
	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		return r
	}

	for i := range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req())
		if rec.Code != 200 {
			t.Fatalf("request %d should be allowed: %d", i+1, rec.Code)
		}
		if rec.Header().Get("X-RateLimit-Limit") != "2" || rec.Header().Get("X-RateLimit-Reset") == "" {
			t.Errorf("an allowed response should carry the X-RateLimit-* headers: %v", rec.Header())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req())
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("over the limit should be 429 with Retry-After: %d %v", rec.Code, rec.Header())
	}
	if !strings.Contains(rec.Body.String(), errcode.CommonRateLimited) {
		t.Errorf("a 429 should have a problem body: %s", rec.Body.String())
	}

	// A different IP is unaffected.
	other := httptest.NewRequest("GET", "/", nil)
	other.RemoteAddr = "10.0.0.2:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, other)
	if rec.Code != 200 {
		t.Errorf("a different address should be counted independently: %d", rec.Code)
	}
}

func TestRealIP(t *testing.T) {
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr; ok200(w, r) })

	newReq := func(xff string) *http.Request {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:9"
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}

	// One trusted hop: the last entry is the real client, written by the proxy.
	httpx.RealIP(true, 1)(inner).ServeHTTP(httptest.NewRecorder(), newReq("203.0.113.7"))
	if !strings.HasPrefix(got, "203.0.113.7") {
		t.Errorf("a trusted proxy should recover the client address: %q", got)
	}

	// The client forged the first entry: the last one (appended by the proxy)
	// must win, or rate limiting can be bypassed with a header.
	httpx.RealIP(true, 1)(inner).ServeHTTP(httptest.NewRecorder(), newReq("8.8.8.8, 203.0.113.50"))
	if !strings.HasPrefix(got, "203.0.113.50") {
		t.Errorf("a forged first entry must not win; the last one should: %q", got)
	}

	httpx.RealIP(false, 1)(inner).ServeHTTP(httptest.NewRecorder(), newReq("8.8.8.8"))
	if !strings.HasPrefix(got, "127.0.0.1") {
		t.Errorf("an untrusted proxy must not be believed: %q", got)
	}

	// ── hops=2: a CDN in front of the local reverse proxy ───────────────────
	// Chain shape: the CDN appends the real client after whatever the client
	// sent itself, then the local proxy appends the CDN edge. The second entry
	// from the right is the real client; taking the last one collapses every
	// user onto a handful of edge addresses.
	httpx.RealIP(true, 2)(inner).ServeHTTP(httptest.NewRecorder(), newReq("8.8.8.8, 203.0.113.9, 172.68.1.2"))
	if !strings.HasPrefix(got, "203.0.113.9") {
		t.Errorf("with hops=2 the second entry from the right is the real client: %q", got)
	}

	// Negative probe for forged headers: however many entries the client
	// invents, the second from the right is still the address the CDN wrote.
	// The forged entries all sit further left, so the rate-limit key is
	// unmoved.
	httpx.RealIP(true, 2)(inner).ServeHTTP(httptest.NewRecorder(), newReq("1.1.1.1, 2.2.2.2, 3.3.3.3, 203.0.113.9, 172.68.1.2"))
	if !strings.HasPrefix(got, "203.0.113.9") {
		t.Errorf("with hops=2, forged entries must not move the rate-limit key: %q", got)
	}

	// Fewer entries than hops: take the leftmost one, the real peer as seen by
	// the first trusted proxy. That is what a request bypassing the CDN and
	// hitting the origin directly looks like; the defense for that shape is
	// network-level. What is asserted here is only "no out-of-range index, no
	// empty value".
	httpx.RealIP(true, 2)(inner).ServeHTTP(httptest.NewRecorder(), newReq("203.0.113.31"))
	if !strings.HasPrefix(got, "203.0.113.31") {
		t.Errorf("with fewer entries than hops, the leftmost one should be used: %q", got)
	}
}

func TestHostGuard(t *testing.T) {
	h := httpx.HostGuard("console.example.com")(http.HandlerFunc(ok200))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "console.example.com:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("a matching host should be allowed: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("a mismatched host should be hidden behind a 404: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	httpx.HostGuard("")(http.HandlerFunc(ok200)).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with no host configured it should pass through: %d", rec.Code)
	}
}

func TestHealthDrain(t *testing.T) {
	h := httpx.NewHealth(map[string]func(context.Context) error{
		"always_ok": func(context.Context) error { return nil },
	})

	rec := httptest.NewRecorder()
	h.Up(rec, httptest.NewRequest("GET", "/up", nil))
	if rec.Code != 200 {
		t.Errorf("/up = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"always_ok":"ok"`) {
		t.Errorf("/healthz = %d %s", rec.Code, rec.Body.String())
	}

	h.SetDraining()
	rec = httptest.NewRecorder()
	h.Up(rec, httptest.NewRequest("GET", "/up", nil))
	if rec.Code != 503 {
		t.Errorf("/up should answer 503 while draining: %d", rec.Code)
	}
}

func TestHealthzDegraded(t *testing.T) {
	h := httpx.NewHealth(map[string]func(context.Context) error{
		"db": func(context.Context) error { return errors.New("connection failed") },
	})
	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "degraded") {
		t.Errorf("a failing dependency should be 503 degraded: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHealthStartupThenLivenessWaitsForInitialReadinessThenLatches(t *testing.T) {
	ready := false
	h := httpx.NewHealth(map[string]func(context.Context) error{
		"catalog": func(context.Context) error {
			if !ready {
				return errors.New("invalid provider transport")
			}
			return nil
		},
	})

	rec := httptest.NewRecorder()
	h.StartupThenLiveness(rec, httptest.NewRequest("GET", "/up", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "not ready" {
		t.Fatalf("initial /up = %d %q, want 503 not ready", rec.Code, rec.Body.String())
	}

	ready = true
	rec = httptest.NewRecorder()
	h.StartupThenLiveness(rec, httptest.NewRequest("GET", "/up", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready /up = %d, want 200", rec.Code)
	}

	// Runtime dependency failures belong on /healthz. Once this process has
	// entered rotation, /up remains a liveness signal and last-known-good state
	// can continue serving traffic.
	ready = false
	rec = httptest.NewRecorder()
	h.StartupThenLiveness(rec, httptest.NewRequest("GET", "/up", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("latched /up = %d, want liveness 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime /healthz = %d, want 503", rec.Code)
	}
}

func TestSPA(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":      {Data: []byte("<html>app</html>")},
		"assets/x.123.js": {Data: []byte("js")},
	}
	h := httpx.SPA(dist, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("the root path should serve the index: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/orgs/123/settings", nil))
	if !strings.Contains(rec.Body.String(), "app") {
		t.Errorf("a client route should fall back to the index: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/x.123.js", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("a fingerprinted asset should be cached for a long time: %q", cc)
	}

	rec = httptest.NewRecorder()
	httpx.SPA(nil, "placeholder").ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "placeholder") {
		t.Errorf("with no build it should serve the placeholder: %s", rec.Body.String())
	}
}

// An unmatched API path must 404 and must not fall back to index.html — the
// classic soft 404.
//
// How it shows up: a retired route prefix answers 200 + text/html, which reads
// as "the old routing is still live and the cutover did not take effect". The
// giveaway is that a completely invented path returns the same HTML.
//
// Three consequences: the caller gets HTML instead of error JSON, monitoring
// never records a 4xx, and triage is actively misled.
func TestSPAUnmatchedAPIPathIs404NotIndex(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":      {Data: []byte("<html>app</html>")},
		"assets/x.123.js": {Data: []byte("js")},
	}
	h := httpx.SPA(dist, "")

	// One of each shape: a retired prefix, a wholly invented path, and /api.
	for _, p := range []string{"/api/admin/v1/meta", "/api/nonexistent/v1/zzz", "/api"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s should be 404: %d", p, rec.Code)
		}
		// Asserting the status alone does not pin the mechanism down: a soft
		// 404 *is* 200 + index. All three assertions together do — status, no
		// index in the body, not text/html.
		if strings.Contains(rec.Body.String(), "app") {
			t.Errorf("%s must not fall back to the index: %s", p, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Errorf("%s must not answer with text/html: %q", p, ct)
		}
	}
}

// Negative control: client-side routes must still fall back. The previous test
// draws the "no fallback" boundary at /api/; this one pins that the boundary was
// not drawn too wide. Without it, deleting the fallback entirely would still
// leave the previous test green.
func TestSPAClientRouteStillFallsBack(t *testing.T) {
	dist := fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}}
	h := httpx.SPA(dist, "")

	// Includes a path that starts with "api" but is *not* in the API namespace
	// (/apikeys): the test is "api/", not "api", and this pins that the slash
	// cannot be dropped.
	for _, p := range []string{"/organizations/org_x", "/audit-logs", "/apikeys"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
			t.Errorf("%s is a client route and should fall back to the index: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// After a deploy, a chunk referenced by the previous app shell is no longer part
// of the build. Falling back to index.html answers it with 200 + text/html: the
// browser reports a failure to fetch a dynamically imported module, and
// monitoring never records a 4xx. There are no client routes under assets/, so a
// miss there must be a 404.
func TestSPAMissingAssetIs404NotIndex(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":      {Data: []byte("<html>app</html>")},
		"assets/x.123.js": {Data: []byte("js")},
	}
	h := httpx.SPA(dist, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/security-old.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing fingerprinted asset should be 404: %d", rec.Code)
	}
	// The status alone is not enough: the fallback branch answers exactly
	// 200 + index, so "the body is not the index" is the assertion that pins it.
	if strings.Contains(rec.Body.String(), "app") {
		t.Errorf("a missing fingerprinted asset must not fall back to the index: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("a missing fingerprinted asset must not answer with text/html: %q", ct)
	}

	// The directory itself: a file server lists any directory without an
	// index.html, which hands out the entire chunk manifest for free.
	for _, p := range []string{"/assets/", "/assets"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s must not be listable: %d", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "x.123.js") {
			t.Errorf("%s leaked the chunk manifest: %s", p, rec.Body.String())
		}
	}

	// An asset that does exist, under the same path shape, must still be
	// served — otherwise the 404s above could just be "everything 404s".
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/x.123.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "js" {
		t.Errorf("an asset that exists should still be served: %d %q", rec.Code, rec.Body.String())
	}
}

func TestSPAMissingRootStaticFileIs404NotIndex(t *testing.T) {
	dist := fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}}
	h := httpx.SPA(dist, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/favicon.ico", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing root static file should be 404: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "app") {
		t.Errorf("a missing root static file must not fall back to the index: %s", rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); strings.Contains(contentType, "text/html") {
		t.Errorf("a missing root static file must not answer with text/html: %q", contentType)
	}
}

// index.html changes on every deploy, and an embedded FS reports a zero
// ModTime, so the standard file server sends neither Last-Modified nor ETag.
// Without an explicit validator, "a reload always fetches the new shell" is not
// true — and any stale-shell auto-reload in the frontend rests entirely on it.
func TestSPAIndexRevalidates(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":      {Data: []byte("<html>app</html>")},
		"assets/x.123.js": {Data: []byte("js")},
	}
	h := httpx.SPA(dist, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("the shell must revalidate every time: %q", cc)
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 8 {
		t.Fatalf("the shell should carry a quoted strong ETag: %q", etag)
	}

	// The fallback also serves the shell, so the cache semantics must match;
	// otherwise anyone arriving through a deep link still gets an index with no
	// validator and the whole problem survives.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/orgs/123/settings", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("the shell served by the fallback must revalidate too: %q", cc)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Errorf("the same shell should have the same ETag: %q vs %q", got, etag)
	}

	// A conditional request must actually save a transfer, or no-cache means
	// resending the whole shell every single time.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("a matching ETag should answer 304: %d", rec.Code)
	}

	// The ETag has to follow the content, or after a deploy a conditional
	// request would declare the old shell still fresh.
	other := httpx.SPA(fstest.MapFS{"index.html": {Data: []byte("<html>app v2</html>")}}, "")
	rec = httptest.NewRecorder()
	other.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("ETag"); got == etag {
		t.Errorf("different content should have a different ETag: %q", got)
	}
}

// Precondition for streaming responses to pass through: every wrapping
// ResponseWriter must implement Unwrap, or http.ResponseController cannot reach
// the underlying flusher and the response is buffered until the handler returns
// — which is the same as not streaming at all.
func TestResponseControllerFlushThroughWrappers(t *testing.T) {
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte(": processing\n\n")); err != nil {
			t.Errorf("write heartbeat: %v", err)
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush should reach through the wrappers: %v", err)
			return
		}
		<-release // keep the handler from returning, proving the bytes were flushed
		_, _ = w.Write([]byte("data: done\n\n"))
	})

	// Same wrapper chain as the data plane: audit outside, handler inside.
	srv := httptest.NewServer(httpx.Audit(nil)(handler))
	// Cleanup is LIFO: release the handler first, then close the server. The
	// other order makes Close block on a handler that has not returned.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, len(": processing\n\n"))
	done := make(chan error, 1)
	go func() {
		_, rerr := io.ReadFull(resp.Body, buf)
		done <- rerr
	}()
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("read the first chunk: %v", rerr)
		}
		if string(buf) != ": processing\n\n" {
			t.Fatalf("the first chunk has the wrong content: %q", buf)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the flushed first chunk never arrived before the handler returned: a wrapper blocked the flusher")
	}
}
