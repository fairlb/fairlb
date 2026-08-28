// End to end over the administrator plane: a real router, real HTTP requests.
//
// # Why this has to go through the router
//
// The package-level cases call handler methods directly and verify the
// handlers' own behaviour. But a whole layer exists only inside buildRouter:
// deny-by-default for anonymous callers, the two tiers of rate limiting,
// idempotency, and audit logging. Every one of those has the same failure
// shape -- forget to mount it and everything still works. Creating a key
// succeeds, logging in succeeds, the tests stay green; there is simply no audit
// trail and anyone can walk in.
//
// This file is that layer's only witness.
package main

import (
	"context"
	"encoding/json"
	"github.com/fairlb/fairlb/foundation/testutil/testx"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/drivers"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/jobs"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/foundation/testutil/testredis"
	"github.com/fairlb/fairlb/gateway"
	communitybootstrap "github.com/fairlb/fairlb/internal/community/bootstrap"
	communityconfig "github.com/fairlb/fairlb/internal/community/config"
	communityorgauthz "github.com/fairlb/fairlb/internal/community/orgauthz"
	communitysettle "github.com/fairlb/fairlb/internal/community/settle"
	communitystaffapi "github.com/fairlb/fairlb/internal/community/staffapi"
	communitystaffauth "github.com/fairlb/fairlb/internal/community/staffauth"
	"github.com/fairlb/fairlb/settings"
)

func newCERouter(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	return newCERouterWithSetupToken(t, "")
}

// newCERouterWithSetupToken builds the same router with a setup token
// configured, which is the only difference the first-run endpoints care about.
func newCERouterWithSetupToken(t *testing.T, setupToken string) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	r, pool, _ := newCERouterWithHealth(t, setupToken)
	return r, pool
}

// newCERouterWithHealth also hands back the health tracker, for the one caller
// that has to put the router into the draining state.
func newCERouterWithHealth(
	t *testing.T, setupToken string,
) (http.Handler, *pgxpool.Pool, *httpx.Health) {
	t.Helper()
	pool := testpg.Start(t)
	ctx := context.Background()
	base := config.Config{
		RedisURL: "redis://" + testredis.Addr(t),
		Drivers: config.Drivers{
			Cache: config.DriverRedis, RateLimit: config.DriverRedis,
			Breaker: config.DriverRedis, Lock: config.DriverRedis,
		},
		RateLimitPerIPRPM:     1000,
		AuthRateLimitPerIPRPM: 1000,
	}
	// A plaintext local address: Secure is false, so the session cookie uses
	// the name without the `__Host-` prefix. That matches running the container
	// and hitting localhost, which is the shape these cases exist to cover.
	cfg := communityconfig.Config{
		Config:     base,
		PublicURL:  &url.URL{Scheme: "http", Host: "127.0.0.1:8080"},
		Secure:     false,
		SetupToken: setupToken,
	}
	drv, err := drivers.New(ctx, base, pool)
	if err != nil {
		t.Fatal(err)
	}
	box, err := crypto.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	orgID, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	set := settings.New(pool, drv.Cache, settings.NewRegistry(), nil)
	workers := jobs.NewWorkers()
	jobClient, err := jobs.NewWorkerClient(pool, jobs.WorkerConfig{
		Workers: workers, PeriodicJobs: gateway.PeriodicJobs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	module, err := gateway.NewModule(gateway.Dependencies{
		Database: pool, Settlement: communitysettle.New(pool),
		OrganizationAccess: communityorgauthz.New(pool),
		AlertSink:          gateway.AlertFunc(func(context.Context, string, string) {}),
		OrgNotifier:        gateway.OrgNotifierFunc(func(context.Context, pgtype.UUID, pgtype.UUID, int) error { return nil }),
		Settings:           set, Cipher: box, Cache: drv.Cache, RateLimit: drv.RateLimit,
		Breaker: drv.Breaker, Jobs: jobClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := module.RegisterWorkers(workers); err != nil {
		t.Fatal(err)
	}
	keys := apikeys.NewService(apikeys.ServiceConfig{
		Database: pool,
		Admin:    communitystaffapi.AllowKeyAdmin,
		Invalidator: func(ctx context.Context, keyHash string) {
			if err := gateway.NewKeyInvalidator(drv.Cache)(ctx, keyHash); err != nil {
				t.Errorf("invalidate key cache: %v", err)
			}
		},
	})
	health := httpx.NewHealth(map[string]func(context.Context) error{"db": pool.Ping})
	r := buildRouter(cfg, pool, module, communitystaffauth.New(pool), drv, orgID, keys, health, set, nil)
	return r, pool, health
}

// Log in, create a key, list keys, revoke -- all over HTTP, with every step
// leaving an audit record.
func TestCERouterKeyFlowIsAuthenticatedAndAudited(t *testing.T) {
	r, pool := newCERouter(t)
	ctx := context.Background()
	if err := communitybootstrap.CreateAdmin(ctx, pool, "ce@example.com", "correct-horse", "CE"); err != nil {
		t.Fatal(err)
	}

	// Every write carries a same-host Origin: the CSRF guard refuses a write
	// with no origin header at all (no header, no trust). That is not a burden
	// the tests carry, it is the guard's criterion.
	do := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "gw.test"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://gw.test")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	countAudit := func(action string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const keyWrite = "POST /api/staff/v1/keys"

	// 1. Creating a key while logged out must be 401 -- not 400, not 404.
	//    The deny-anonymous middleware has to run before request binding,
	//    otherwise an anonymous request hits body validation's 400 first and
	//    the shape of the API leaks.
	if w := do(http.MethodPost, "/api/staff/v1/keys", `{"name":"x"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("creating a key anonymously should be 401, got %d: %s", w.Code, w.Body.String())
	}
	// A refused attempt is audited too: half the value of an audit log is "who
	// changed what", the other half is "who tried and did not get in". Nothing
	// held this down before; it turned up while writing the case above, when
	// the key-creation path produced two rows instead of one.
	if n := countAudit(keyWrite); n != 1 {
		t.Fatalf("a refused anonymous write should be audited too, want 1 row, got %d", n)
	}

	// 2. Log in.
	w := do(http.MethodPost, "/api/staff/v1/auth/login",
		`{"email":"ce@example.com","password":"correct-horse"}`, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login should be 204, got %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("exactly one cookie should be issued, got %d", len(cookies))
	}
	session := cookies[0]

	// 3. Create a key.
	w = do(http.MethodPost, "/api/staff/v1/keys", `{"name":"prod"}`, session)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a key should be 201, got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Key    string `json:"key"`
		APIKey struct {
			ID string `json:"id"`
		} `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" || created.APIKey.ID == "" {
		t.Fatalf("the response is missing fields: %s", w.Body.String())
	}

	// 4. Audit: this particular key creation must add a row of its own.
	//    This is the audit middleware's only witness -- without it mounted,
	//    the three steps above stay green word for word.
	//
	// The criterion is the delta, not a total, and it is anchored on an exact
	// action:
	//   - no substring match and no fallback to "any audit row at all" --
	//     logging in is also a POST and is also audited, so any criterion loose
	//     enough for login to satisfy stays green when key writes are not
	//     audited at all;
	//   - not a total either: the refused anonymous request above is audited
	//     too, so a total drifts with the steps of the case.
	// The audit middleware records `METHOD path` (the target type is filled in
	// by the handler and is empty here), so the literal is written in exactly
	// that shape: if it ever changes, this turns red immediately, which is what
	// should happen.
	if n := countAudit(keyWrite); n != 2 {
		t.Fatalf("a successful key creation should add another audit row, 2 in total with the refused one above, got %d", n)
	}

	// 5. Revoke.
	w = do(http.MethodDelete, "/api/staff/v1/keys/"+created.APIKey.ID, "", session)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revocation should be 204, got %d: %s", w.Code, w.Body.String())
	}

	// 6. A cross-site write is refused: a different Origin fails even with the
	//    same session cookie.
	crossReq := httptest.NewRequest(http.MethodPost, "/api/staff/v1/keys", strings.NewReader(`{"name":"x"}`))
	crossReq.Host = "gw.test"
	crossReq.Header.Set("Content-Type", "application/json")
	crossReq.Header.Set("Origin", "https://evil.test")
	crossReq.AddCookie(session)
	crossW := httptest.NewRecorder()
	r.ServeHTTP(crossW, crossReq)
	if crossW.Code != http.StatusForbidden {
		t.Fatalf("a cross-site write should be 403, got %d: %s", crossW.Code, crossW.Body.String())
	}

	// 7. After revocation the data plane no longer accepts the key.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key calling the data plane should be 401, got %d: %s", w.Code, w.Body.String())
	}
}

// The console plane: the same generated strict handler used everywhere, with an
// injected authorizer degrading the organisation dimension to the single
// bootstrapped one.
//
// Three criteria, none of which can be dropped:
//
//  1. anonymous callers always get 401 -- this plane has no login endpoint and
//     its allowlist is empty, so forgetting the guard makes usage world-readable
//  2. an administrator session can read this organisation's usage, which is
//     where "one session serves both planes" actually lands
//  3. any other organisation id gets 404, not an empty set -- row-level
//     security returns an empty result, and "no data found" reads as "no usage
//     yet", which shows nothing about an unauthorized access being stopped
func TestCEConsolePlaneScopedToDefaultOrg(t *testing.T) {
	r, pool := newCERouter(t)
	ctx := context.Background()
	if err := communitybootstrap.CreateAdmin(ctx, pool, "con@example.com", "correct-horse", "CE"); err != nil {
		t.Fatal(err)
	}
	org, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	orgPublic := publicid.Format(publicid.Org, org)

	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "gw.test"
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	usage := "/api/v1/orgs/" + orgPublic + "/usage?from=2026-08-01T00:00:00Z&to=2026-08-31T00:00:00Z"

	// 1. Anonymous callers get 401.
	if w := get(usage, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("reading usage anonymously should be 401, got %d: %s", w.Code, w.Body.String())
	}

	// Log in to obtain a session.
	login := httptest.NewRequest(http.MethodPost, "/api/staff/v1/auth/login",
		strings.NewReader(`{"email":"con@example.com","password":"correct-horse"}`))
	login.Host = "gw.test"
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "https://gw.test")
	lw := httptest.NewRecorder()
	r.ServeHTTP(lw, login)
	if lw.Code != http.StatusNoContent {
		t.Fatalf("login should be 204, got %d: %s", lw.Code, lw.Body.String())
	}
	session := lw.Result().Cookies()[0]

	// 2. The same administrator session can read the console plane.
	if w := get(usage, session); w.Code != http.StatusOK {
		t.Fatalf("an administrator session reading usage should be 200, got %d: %s", w.Code, w.Body.String())
	}

	// 3. Another organisation gives 404, not a 200 with an empty set.
	other := publicid.Format(publicid.Org, testx.MustUUID(t, "00000000-0000-7000-8000-0000000000ff"))
	otherUsage := "/api/v1/orgs/" + other + "/usage?from=2026-08-01T00:00:00Z&to=2026-08-31T00:00:00Z"
	w := get(otherUsage, session)
	if w.Code != http.StatusNotFound {
		t.Fatalf("an out-of-scope organisation should be 404, got %d: %s", w.Code, w.Body.String())
	}
}

// Organization-supplied upstream credentials are not served here: all four operations
// return 404.
//
// # Why this is a criterion and not "the UI does not link to it anyway"
//
// Whether the UI links to something decides whether anyone can click it, not
// whether the endpoint answers. These four were live, writable endpoints:
// reachable with curl after logging in, and organization credentials take priority
// over provider credentials during routing. On a single instance that gives two
// write paths to "which upstream credential is in effect" with no defensible
// precedence between them.
//
// # Enumerated one by one, not sampled
//
// The full set comes from the unserved-operations list and every one is called.
// Testing only one would let anything missing from that list keep serving
// silently -- and something missing from the list is exactly the likely failure
// (the specification gains a new endpoint and nobody remembers this list
// exists).
func TestCEDoesNotServeBYOK(t *testing.T) {
	r, pool := newCERouter(t)
	ctx := context.Background()
	if err := communitybootstrap.CreateAdmin(ctx, pool, "byok@example.com", "correct-horse", "CE"); err != nil {
		t.Fatal(err)
	}
	org, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/orgs/" + publicid.Format(publicid.Org, org) + "/provider-keys"

	login := httptest.NewRequest(http.MethodPost, "/api/staff/v1/auth/login",
		strings.NewReader(`{"email":"byok@example.com","password":"correct-horse"}`))
	login.Host = "gw.test"
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "https://gw.test")
	lw := httptest.NewRecorder()
	r.ServeHTTP(lw, login)
	session := lw.Result().Cookies()[0]

	// The criterion is checked against the specification as it stands: the
	// paths carrying this segment are exactly the organization-credential endpoints,
	// no more and no fewer. What is blocked is a path segment, and only the
	// specification can say whether that segment captures precisely what it
	// should -- a hard-coded list of operations would silently go stale the
	// moment an endpoint is added.
	byok, other := specPathsWithSegment(t, communityorgauthz.UnservedSegment)
	if len(byok) != 3 {
		t.Fatalf("the console spec should have 3 paths carrying the %q segment, got %d: %v",
			communityorgauthz.UnservedSegment, len(byok), byok)
	}
	for _, p := range other {
		if strings.Contains(p, communityorgauthz.UnservedSegment) {
			t.Fatalf("path %q carries that segment without being one of the blocked endpoints -- the block would catch it by mistake", p)
		}
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPost, base, `{"vendor":"openai","name":"x","secret":"sk-x"}`},
		{http.MethodDelete, base + "/pk_00000000000000000000000000", ""},
		{http.MethodPost, base + "/pk_00000000000000000000000000/test", ""},
		// A malformed request must be 404 as well. This is the reason the
		// check moved from the generated handler's middleware down to the
		// routing layer: mounted there it answered 400, and the difference
		// between 400 and 404 tells the caller the endpoint exists.
		{http.MethodPost, base, `{ not json at all`},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			req.Host = "gw.test"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "https://gw.test")
			req.AddCookie(session)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
			}
		})
	}

	// The control: the endpoints on this plane that should be served still
	// work. Otherwise "everything 404s" would satisfy the checks above, and
	// that would mean the whole mount is broken rather than four endpoints
	// being subtracted.
	usage := "/api/v1/orgs/" + publicid.Format(publicid.Org, org) +
		"/usage?from=2026-08-01T00:00:00Z&to=2026-08-31T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, usage, nil)
	req.Host = "gw.test"
	req.AddCookie(session)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("the usage endpoint on the same plane should still be 200, got %d: %s", w.Code, w.Body.String())
	}
}

// specPathsWithSegment reads the paths out of the console specification and
// splits them by whether they contain the segment.
//
// Reading the yaml rather than maintaining a list of operations: a list goes
// stale, and it goes stale in the direction of "the specification gained a
// similar endpoint and this file does not know". Extracting it live makes that
// situation turn the count assertion above red immediately.
func specPathsWithSegment(t *testing.T, segment string) (with, without []string) {
	t.Helper()
	raw, err := os.ReadFile("../../api/gateway-console.yaml")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^  (/\S+):`)
	ms := re.FindAllStringSubmatch(string(raw), -1)
	if len(ms) < 5 {
		t.Fatalf("only %d paths were extracted from the spec -- the pattern no longer matches and the criterion is dead", len(ms))
	}
	for _, m := range ms {
		if slices.Contains(strings.Split(m[1], "/"), segment) {
			with = append(with, m[1])
		} else {
			without = append(without, m[1])
		}
	}
	return with, without
}

// The admin UI is really served, and the session cookie's name can honour its
// own contract.
//
// Both criteria came from running the documented container instructions for
// real, and neither had any test holding it down. They share one shape:
// the response codes were right and the feature was not there.
//
//   - `/` returned 404: the image was built with the embed tag and the built
//     assets were copied in, but the embed only covered other applications and
//     no single-page-application route was mounted at all. The product was
//     supposed to ship with a complete admin UI, and the artifact had only an
//     API.
//   - login returned 204 but the session could not be stored: the cookie was
//     named with the `__Host-` prefix without Secure, and that prefix requires
//     Secure by specification, so browsers and curl alike discard it. The
//     documented quick start sets no TLS, so login failed under the default
//     configuration every time.
func TestCEServesAdminUIAndUsableSessionCookie(t *testing.T) {
	r, pool := newCERouter(t)
	ctx := context.Background()
	if err := communitybootstrap.CreateAdmin(ctx, pool, "ui@example.com", "correct-horse", "CE"); err != nil {
		t.Fatal(err)
	}

	// 1. `/` has to be caught by the single-page application. This build has
	//    no embedded assets, so what comes back is the placeholder -- and the
	//    criterion is "not a 404": both the placeholder and a real build mean
	//    something serves this path, while 404 means the application has no
	//    frontend at all.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "gw.test"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Fatal("`/` returned 404 -- the admin UI is not mounted, and building with the embed tag achieves nothing")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("`/` should be 200, either the real build or the placeholder, got %d", w.Code)
	}
	// The single-page application's security baseline has to be there (content
	// security policy, frame options); the API header set does not apply.
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Error("the single-page application response has no Content-Security-Policy")
	}
	// Images stay same-origin, byte for byte. This is pinned rather than left
	// implicit because the pressure to widen it is real and recurring: an avatar
	// taken from an identity provider is served from that provider's CDN, and
	// rendering one means admitting a third-party origin here. ADR-0146 settled
	// that the other way -- the avatar is derived locally -- so this directive
	// has no reason to grow, and a diff on this line is the question being
	// reopened rather than a detail.
	//
	// `data:` is in the baseline for the enrolment QR code, which is generated
	// in the browser as a data URI.
	const imgSrc = "img-src 'self' data:;"
	if !strings.Contains(csp, imgSrc) {
		t.Errorf("the image policy is no longer %q: %q", imgSrc, csp)
	}

	// 2. The API prefixes must not be swallowed by the catch-all -- taking
	//    over the API is the most likely thing to go wrong when mounting `/*`.
	for _, p := range []string{"/healthz", "/v1/models", "/api/staff/v1/auth/me"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Host = "gw.test"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Header().Get("Content-Security-Policy") != "" {
			t.Errorf("%s was swallowed by the catch-all: it carries the single-page application's CSP header", p)
		}
	}

	// 3. The cookie's name has to match whether it carries Secure. This build
	//    has secure=false, so the name must not use the `__Host-` prefix:
	//    using it would be a lie, and the recipient discards such a cookie as
	//    the specification requires.
	login := httptest.NewRequest(http.MethodPost, "/api/staff/v1/auth/login",
		strings.NewReader(`{"email":"ui@example.com","password":"correct-horse"}`))
	login.Host = "gw.test"
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "https://gw.test")
	lw := httptest.NewRecorder()
	r.ServeHTTP(lw, login)
	if lw.Code != http.StatusNoContent {
		t.Fatalf("login should be 204, got %d", lw.Code)
	}
	c := lw.Result().Cookies()[0]
	if strings.HasPrefix(c.Name, "__Host-") && !c.Secure {
		t.Fatalf("the cookie is named %q but carries no Secure attribute. The "+
			"`__Host-` prefix requires it, browsers and curl alike discard "+
			"such a cookie, and the symptom is a login that returns 204 and "+
			"then immediately has no session", c.Name)
	}
	if c.Secure && !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("Secure is set but the `__Host-` prefix is not used -- a contract that could be honoured was not: %q", c.Name)
	}

	// 4. That cookie really resolves to an identity -- the right name with a
	//    read side that never learned it is just as useless.
	me := httptest.NewRequest(http.MethodGet, "/api/staff/v1/auth/me", nil)
	me.Host = "gw.test"
	me.AddCookie(c)
	mw := httptest.NewRecorder()
	r.ServeHTTP(mw, me)
	if mw.Code != http.StatusOK {
		t.Fatalf("the cookie just issued should resolve to an identity, got %d: %s", mw.Code, mw.Body.String())
	}
}

// The public /healthz is the one a proxy in front of this instance can reach,
// and the deployment guide tells operators it turns 503 once shutdown starts.
// It used to be a hard-coded 200, which made that sentence false and left
// DRAIN_GRACE_SECONDS with nothing observing it.
func TestCEPublicHealthzReportsDraining(t *testing.T) {
	r, _, health := newCERouterWithHealth(t, "")

	probe := func() int {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = "gw.test"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := probe(); code != http.StatusOK {
		t.Fatalf("serving normally, /healthz should be 200, got %d", code)
	}
	health.SetDraining()
	if code := probe(); code != http.StatusServiceUnavailable {
		t.Fatalf("draining, /healthz should be 503, got %d — a proxy polling the public "+
			"port keeps sending traffic into an instance that is going away", code)
	}
}

// Gemini has one native version plane. Mounting it under /v1 as well makes the
// OpenAI/Anthropic and Gemini contracts overlap and creates an undocumented
// alias that SDK configuration can accidentally depend on.
func TestCEServesGeminiOnlyUnderV1Beta(t *testing.T) {
	r, _ := newCERouter(t)

	path := "/models/google/gemini-2.5-flash:generateContent"
	for prefix, want := range map[string]int{"/v1": http.StatusNotFound, "/v1beta": http.StatusUnauthorized} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, prefix+path, strings.NewReader(`{"contents":[]}`))
		r.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s status = %d, want %d (%s)", prefix, rec.Code, want, rec.Body.String())
		}
	}
}

// The /v1beta prefix carries Gemini-shaped endpoints and nothing else.
//
// It used to be the whole data plane mounted a second time, which put the
// OpenAI-shaped catalogue on GET /v1beta/models -- the address a Gemini client
// asks for its model list. That answered 200 with a body carrying `data` where
// the SDK looks for `models`, so it reported an empty catalogue: a conclusion
// rather than an error, with nothing in any log. The native catalogue now
// reaches authentication while the other protocols remain absent.
func TestCEV1BetaCarriesOnlyTheGeminiEndpoint(t *testing.T) {
	r, _ := newCERouter(t)

	for path, want := range map[string]int{
		"/v1beta/models":           http.StatusUnauthorized,
		"/v1beta/chat/completions": http.StatusNotFound,
		"/v1beta/messages":         http.StatusNotFound,
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			method := http.MethodGet
			if path != "/v1beta/models" {
				method = http.MethodPost
			}
			r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
			if rec.Code != want {
				t.Errorf("status = %d, want %d for %s (body: %s)", rec.Code, want, path, rec.Body.String())
			}
		})
	}

	// And the same addresses are still served under /v1.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code == http.StatusNotFound {
		t.Error("GET /v1/models must still be served")
	}
}

// Two ways of addressing this endpoint are refused before the request is even
// read, because neither can be served correctly:
//
//   - an unknown method is a statement about the address, so it answers 404
//     rather than 400 about a body that was fine;
//   - a streamed request without alt=sse asks for the JSON array form. Serving
//     it as SSE would hand the client a well-formed stream of the wrong framing,
//     and forwarding the array would leave this gateway unable to read the usage
//     it bills from.
func TestCEGeminiRefusesAddressesItCannotServe(t *testing.T) {
	r, _ := newCERouter(t)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"unknown method", "/v1beta/models/x:tuneModel", http.StatusNotFound},
		{"streaming without the SSE selector", "/v1beta/models/x:streamGenerateContent", http.StatusBadRequest},
		{"no method at all", "/v1beta/models/x", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{}`)))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}

	// With the selector, the address is served and the request gets as far as
	// authentication.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1beta/models/x:streamGenerateContent?alt=sse", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 once the address is acceptable", rec.Code)
	}
}
