package gwconsoleapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// Storing organization-supplied upstream credentials, and testing them.
//
// The two properties that matter most: the plaintext must not be readable back
// from any endpoint, and one organization must not see another's. These are somebody
// else's upstream billing credentials -- leaking one means someone else pays
// for your traffic.

const byokSecret = "sk-organization-super-secret-value-1234"

// The plaintext appears once, in the create request, and no endpoint returns it
// afterwards.
func TestBYOKSecretNeverReadable(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()
	seedVendorProvider(t, f, "openai", "https://api.openai.test")

	created, err := s.CreateOrgProviderKey(ctx, gwconsoleapi.CreateOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgA),
		Body: &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
			Vendor: "openai", Name: "prod", Secret: byokSecret,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := gwconsoleapi.OrgProviderKey(created.(gwconsoleapi.CreateOrgProviderKey201JSONResponse))

	// The create response itself must not carry the plaintext.
	assertNoPlaintext(t, "create response", key)
	if key.SecretHint == "" || strings.Contains(key.SecretHint, byokSecret) {
		t.Errorf("the mask should exist and contain no plaintext: %q", key.SecretHint)
	}

	// Nor may the list.
	listed, err := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{
		OrgId: orgParam(f.orgA),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoPlaintext(t, "list response", listed)

	// What is stored must be ciphertext.
	var raw []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT secret_enc FROM org_provider_keys WHERE org_id = $1`, f.orgA).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), byokSecret) {
		t.Error("the plaintext was stored -- secret_enc must hold ciphertext")
	}
}

// Invisible across organizations: even with the authorizer bypassed, row-level
// security has to stop it.
func TestBYOKIsolatedAcrossOrgs(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()
	seedVendorProvider(t, f, "openai", "https://api.openai.test")

	if _, err := s.CreateOrgProviderKey(ctx, gwconsoleapi.CreateOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgB),
		Body: &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
			Vendor: "openai", Name: "b-key", Secret: byokSecret,
		},
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{
		OrgId: orgParam(f.orgA),
	})
	if err != nil {
		t.Fatal(err)
	}
	items := listed.(gwconsoleapi.ListOrgProviderKeys200JSONResponse).Items
	if len(items) != 0 {
		t.Fatalf("A must not see B's credential, got %+v -- cross-organization leak", items)
	}
}

// Holding someone else's credential id lets you neither delete nor test it.
func TestBYOKCrossOrgWriteRejected(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()
	seedVendorProvider(t, f, "openai", "https://api.openai.test")

	created, err := s.CreateOrgProviderKey(ctx, gwconsoleapi.CreateOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgB),
		Body: &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
			Vendor: "openai", Name: "b-key", Secret: byokSecret,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bKeyID := gwconsoleapi.OrgProviderKey(created.(gwconsoleapi.CreateOrgProviderKey201JSONResponse)).Id

	// Both assertions check the error *code*, not merely that an error
	// occurred: with isolation working, these two endpoints also fail for
	// other reasons (the connectivity test runs into "no default upstream
	// endpoint for this protocol"). A bare err != nil cannot tell those apart
	// and would stay green if isolation broke. See the ErrWriteDenied comment
	// for the reverse probe that established this.
	var ce *httpx.CodeError

	// A tries to delete B's credential by its id.
	if _, err := s.DeleteOrgProviderKey(ctx, gwconsoleapi.DeleteOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgA), KeyId: bKeyID,
	}); !errors.As(err, &ce) || ce.Code != errcode.CommonNotFound {
		t.Errorf("A deleting B's credential should give %s -- not 403, which would reveal that the id exists -- got %v",
			errcode.CommonNotFound, err)
	}
	// A tries to test B's credential, which would send B's secret upstream.
	_, err = s.TestOrgProviderKey(ctx, gwconsoleapi.TestOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgA), KeyId: bKeyID,
		Body: &gwconsoleapi.TestOrgProviderKeyJSONRequestBody{UpstreamModel: "m"},
	})
	if !errors.As(err, &ce) || ce.Code != errcode.CommonNotFound {
		t.Errorf("A testing B's credential should give %s, since B's credential does not exist for A, got %v",
			errcode.CommonNotFound, err)
	}
	// B's credential is still there.
	listed, _ := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{
		OrgId: orgParam(f.orgB),
	})
	if n := len(listed.(gwconsoleapi.ListOrgProviderKeys200JSONResponse).Items); n != 1 {
		t.Errorf("B's credential should be intact, got %d rows", n)
	}
}

func TestMemberCannotListBYOKCredentials(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, memberAuthz{})
	_, err := s.ListOrgProviderKeys(context.Background(), gwconsoleapi.ListOrgProviderKeysRequestObject{
		OrgId: orgParam(f.orgA),
	})
	assertCode(t, err, errcode.CommonForbidden)
}

// A successful connectivity test sets the status to active and stamps the time.
func TestBYOKTestSuccessMarksActive(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer up.Close()

	f := newFixture(t)
	s := byokServer(t, f)
	id := seedBYOK(t, f, s, "openai", up.URL)

	res := runBYOKTest(t, s, f, id, "gpt-x")
	if !res.Ok {
		t.Fatalf("an upstream 200 should be reported as success: %+v", res)
	}
	if got := byokStatus(t, f, "openai"); got != "active" {
		t.Errorf("after success the status should be active, got %s", got)
	}
	var verified *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT last_verified_at::text FROM org_provider_keys WHERE org_id=$1`, f.orgA).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified == nil {
		t.Error("last_verified_at should be recorded after success")
	}
}

// No failure of this test disables the credential -- not even a 401.
//
// The probe borrows the transport profile of whichever provider at this vendor
// sorts first by slug, so a deployment holding two of them (an EU and a US Azure
// resource, a mainland and an international endpoint) can address the wrong one
// and be rejected for a credential that is perfectly good. Writing "invalid"
// there takes a working key out of routing and silently moves that organization to
// full price, which is far worse than a red result on a page they are looking
// at. The rejection that does disable a key is the data plane's, where the
// profile is the candidate's own -- see markBYOKInvalid.
//
// Success still promotes the row, which is the one transition this endpoint owns.
func TestBYOKTestStatusTransitions(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantStatus string
	}{
		{"credential rejected", http.StatusUnauthorized, "active"},
		{"forbidden", http.StatusForbidden, "active"},
		{"wrong model name", http.StatusNotFound, "active"},
		{"upstream failure", http.StatusBadGateway, "active"},
		{"rate limited", http.StatusTooManyRequests, "active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"error":"x"}`))
			}))
			defer up.Close()

			f := newFixture(t)
			s := byokServer(t, f)
			id := seedBYOK(t, f, s, "openai", up.URL)

			res := runBYOKTest(t, s, f, id, "m")
			if res.Ok {
				t.Fatalf("an upstream %d must not be reported as success", c.status)
			}
			if got := byokStatus(t, f, "openai"); got != c.wantStatus {
				t.Errorf("after an upstream %d the status should be %s, got %s", c.status, c.wantStatus, got)
			}
			if res.Message == nil || *res.Message == "" {
				t.Error("a failure should carry the upstream's own text, which is what distinguishes a bad credential from a wrong model name from an exhausted quota")
			}
		})
	}
}

// An empty base_url falls back to the deployment's default endpoint for that
// vendor; with no endpoint to fall back to, it errors rather than guessing.
func TestBYOKBaseURLFallback(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ('plat','openai',ARRAY['openai'],$1)`, up.URL); err != nil {
		t.Fatal(err)
	}

	// With no base_url of its own, the credential is tested against the
	// deployment's provider for that vendor.
	id := seedBYOK(t, f, s, "openai", "")
	if res := runBYOKTest(t, s, f, id, "m"); !res.Ok {
		t.Errorf("it should fall back to the default endpoint and succeed: %+v", res)
	}

	// Take every provider for that vendor out of service -- which is how a
	// credential ends up with nothing to resolve against, since one cannot be
	// created for a vendor the deployment does not route to. There is then no
	// address to guess, and guessing one would report "cannot connect" for what
	// is actually missing configuration.
	if _, err := f.pool.Exec(ctx, `UPDATE providers SET enabled = false WHERE vendor = 'openai'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TestOrgProviderKey(ctx, gwconsoleapi.TestOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgA), KeyId: id,
		Body: &gwconsoleapi.TestOrgProviderKeyJSONRequestBody{UpstreamModel: "m"},
	}); err == nil {
		t.Error("with no endpoint for the vendor and no base_url on the credential it should error, not guess an address")
	}
}

// A credential for a platform this deployment does not route to is refused.
//
// It could never take effect: routing only ever reaches configured providers, so
// no candidate would be served by it, and the key would sit in the organization's list
// looking configured. The list of what may be supplied travels with the list of
// what is, so the page and the form cannot disagree.
func TestBYOKRefusesAVendorTheDeploymentDoesNotRouteTo(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()
	seedVendorProvider(t, f, "openai", "https://api.openai.test")

	_, err := s.CreateOrgProviderKey(ctx, gwconsoleapi.CreateOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgA),
		Body: &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
			Vendor: "anthropic", Name: "unroutable", Secret: byokSecret,
		},
	})
	if err == nil {
		t.Fatal("a credential for a vendor with no provider should be refused")
	}
	if !strings.Contains(err.Error(), "Anthropic") {
		t.Errorf("the refusal should name the platform, got %q", err)
	}

	listed, lErr := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{OrgId: orgParam(f.orgA)})
	if lErr != nil {
		t.Fatal(lErr)
	}
	vendors := listed.(gwconsoleapi.ListOrgProviderKeys200JSONResponse).Vendors
	if len(vendors) != 1 || vendors[0].Vendor != "openai" {
		t.Fatalf("the offered vendors should be exactly the routed ones, got %+v", vendors)
	}
	if vendors[0].Label != "OpenAI" {
		t.Errorf("the vendor should carry its display name, got %q", vendors[0].Label)
	}
	if vendors[0].BaseUrlHint == nil || *vendors[0].BaseUrlHint != "https://api.openai.test" {
		t.Errorf("the hint should be the endpoint an empty base_url resolves to, got %v", vendors[0].BaseUrlHint)
	}
}

// The connectivity test has to address the upstream exactly the way the request
// path does, profile included.
//
// This is the failure the test exists to prevent, inverted: an upstream that
// keeps its chat endpoint behind a deployment path answers the standard one
// with a 404, so a test that ignores the profile tells the organization their
// credential is broken while inference through the gateway works. A diagnostic
// that disagrees with the thing it diagnoses is worse than no diagnostic --
// it sends someone to re-issue a credential that was never the problem.
//
// Note on scope: this deployment shape is the one that serves these endpoints.
// A community deployment refuses the whole BYOK segment with a 404 before
// routing, so nobody there reaches this code -- but the code is shared, the
// server method is called directly here, and the guard is what keeps the two
// request builders from drifting apart wherever both do run.
func TestBYOKTestAddressesUpstreamLikeTheDataPlane(t *testing.T) {
	// An Azure-shaped profile: all three axes at once -- a path override with a
	// model placeholder, a required query parameter, and the credential in a
	// header of the upstream's choosing.
	const azure = `{"auth":"header:api-key","query":{"api-version":"2024-10-21"},` +
		`"path_overrides":{"/v1/chat/completions":"/openai/deployments/{model}/chat/completions"}}`

	var got *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url, transport)
		 VALUES ('plat','openai',ARRAY['openai'],$1,$2::jsonb)`, up.URL, azure); err != nil {
		t.Fatal(err)
	}

	// base_url empty, so the endpoint and the profile both come from the
	// provider above -- the case where the deployment, not the organization, decides
	// how this upstream is addressed.
	id := seedBYOK(t, f, s, "openai", "")
	if res := runBYOKTest(t, s, f, id, "gpt-x"); !res.Ok {
		t.Fatalf("the upstream answered 200, so the test should report success: %+v", res)
	}
	if got == nil {
		t.Fatal("the upstream was never called")
	}

	// Anchored by hand rather than by asking the request builder what it would
	// do. Both sides of the comparison below run through BuildRequest, so that
	// comparison can only see the two drifting apart -- not both being wrong
	// together. These literals are the independent statement of the address the
	// profile describes.
	if want := "/openai/deployments/gpt-x/chat/completions"; got.URL.Path != want {
		t.Errorf("path = %q, want %q -- the override and its {model} placeholder", got.URL.Path, want)
	}
	if want := "api-version=2024-10-21"; got.URL.RawQuery != want {
		t.Errorf("query = %q, want %q", got.URL.RawQuery, want)
	}
	if v := got.Header.Get("Api-Key"); v != byokSecret {
		t.Errorf("the credential should ride in Api-Key, got %q", v)
	}
	if v := got.Header.Get("Authorization"); v != "" {
		t.Errorf("the profile moved the credential, so Authorization must be absent, got %q", v)
	}

	// And the whole outbound header set has to match the data plane's, not just
	// the parts named above: an extra header here is a fingerprint the request
	// path does not send.
	tp, err := catalog.ParseTransport([]byte(azure))
	if err != nil {
		t.Fatal(err)
	}
	want, err := proxy.BuildRequest(ctx, proxy.Target{
		Protocol: proxy.Protocol("openai"), BaseURL: up.URL, APIKey: byokSecret,
		Path: catalog.PathChat, Transport: tp, UpstreamModel: "gpt-x",
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// The allowlist is derived from the same Target the data plane builds, so
	// the two sides are asked about one configuration rather than two that
	// happen to be written the same way.
	for _, name := range proxy.OutboundAllowlist(proxy.Target{
		Protocol: proxy.Protocol("openai"), BaseURL: up.URL, APIKey: byokSecret,
		Path: catalog.PathChat, Transport: tp, UpstreamModel: "gpt-x",
	}) {
		if got.Header.Get(name) != want.Header.Get(name) {
			t.Errorf("header %s = %q, the data plane sends %q",
				name, got.Header.Get(name), want.Header.Get(name))
		}
	}
}

// With no profile configured, each dialect still gets its own endpoint and its
// own credential header. This is the case the path constants stand for, and it
// is the one a regression in the override logic would take down silently.
func TestBYOKTestUsesTheDialectDefaultsWithoutAProfile(t *testing.T) {
	cases := []struct {
		vendor     string
		wantPath   string
		wantHeader string
	}{
		{"openai", catalog.PathChat, "Authorization"},
		{"anthropic", catalog.PathMessages, "X-Api-Key"},
	}
	for _, c := range cases {
		t.Run(c.vendor, func(t *testing.T) {
			var got *http.Request
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Clone(r.Context())
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer up.Close()

			f := newFixture(t)
			s := byokServer(t, f)
			id := seedBYOK(t, f, s, c.vendor, up.URL)
			if res := runBYOKTest(t, s, f, id, "m"); !res.Ok {
				t.Fatalf("the upstream answered 200: %+v", res)
			}
			if got == nil {
				t.Fatal("the upstream was never called")
			}
			if got.URL.Path != c.wantPath {
				t.Errorf("path = %q, want %q", got.URL.Path, c.wantPath)
			}
			if got.Header.Get(c.wantHeader) == "" {
				t.Errorf("the credential should ride in %s, headers were %v", c.wantHeader, got.Header)
			}
		})
	}
}

// ===== Fixtures =====

func byokServer(t *testing.T, f *fixture) *gwconsoleapi.Server {
	t.Helper()
	return gwconsoleapi.NewServer(gwconsoleapi.ServerConfig{
		Pool: f.pool, OrganizationAccess: allowAll{}, Catalog: testCatalog(f.pool), Cipher: byokBox(t),
	})
}

func byokBox(t *testing.T) *crypto.Box {
	t.Helper()
	box, err := crypto.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// seedBYOK adds a credential for a vendor, seeding a provider at that vendor
// first when the fixture has none: a credential for a platform the deployment
// does not route to is refused, because nothing could ever be served by it.
func seedBYOK(t *testing.T, f *fixture, s *gwconsoleapi.Server, vendor, baseURL string) string {
	t.Helper()
	seedVendorProvider(t, f, vendor, "https://unused.test")
	body := &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
		Vendor: vendor,
		Name:   "k", Secret: byokSecret,
	}
	if baseURL != "" {
		body.BaseUrl = &baseURL
	}
	created, err := s.CreateOrgProviderKey(context.Background(),
		gwconsoleapi.CreateOrgProviderKeyRequestObject{OrgId: orgParam(f.orgA), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return gwconsoleapi.OrgProviderKey(created.(gwconsoleapi.CreateOrgProviderKey201JSONResponse)).Id
}

func runBYOKTest(t *testing.T, s *gwconsoleapi.Server, f *fixture, id, model string) gwconsoleapi.OrgProviderKeyTestResult {
	t.Helper()
	res, err := s.TestOrgProviderKey(context.Background(), gwconsoleapi.TestOrgProviderKeyRequestObject{
		OrgId: orgParam(f.orgA), KeyId: id,
		Body: &gwconsoleapi.TestOrgProviderKeyJSONRequestBody{UpstreamModel: model},
	})
	if err != nil {
		t.Fatal(err)
	}
	return gwconsoleapi.OrgProviderKeyTestResult(res.(gwconsoleapi.TestOrgProviderKey200JSONResponse))
}

// seedVendorProvider makes the deployment route to a vendor. Idempotent, so a
// test that also configures its own provider is unaffected.
func seedVendorProvider(t *testing.T, f *fixture, vendor, baseURL string) {
	t.Helper()
	protocol := catalog.ProtocolOpenAI
	if v, ok := catalog.LookupVendor(vendor); ok && len(v.DefaultProtocols) > 0 {
		protocol = v.DefaultProtocols[0]
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('seed-'||$1, $1, ARRAY[$2::text], $3) ON CONFLICT (slug) DO NOTHING`,
		vendor, protocol, baseURL); err != nil {
		t.Fatal(err)
	}
}

func byokStatus(t *testing.T, f *fixture, vendor string) string {
	t.Helper()
	var st string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM org_provider_keys WHERE org_id=$1 AND vendor=$2`,
		f.orgA, vendor).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// assertNoPlaintext serialises the whole response and searches it for the
// plaintext. That is more reliable than asserting field by field: when a field
// is added to the response later, this check covers it automatically.
func assertNoPlaintext(t *testing.T, what string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), byokSecret) {
		t.Errorf("%s leaked the secret in plaintext: %s", what, raw)
	}
}

// Paging the credential list, on a key that is not time.
//
// This list is ordered by (vendor, name) because it is a configuration screen:
// an organization holding several accounts at one platform reads them together.
// So the cursor has to follow that key. A cursor on created_at would have to
// reorder the list to serve itself, and the reader would lose the grouping the
// screen exists to provide.
//
// The names below are chosen so that creation order and (vendor, name) order
// disagree: seeded z→a within each vendor, and the vendors interleaved. A cursor
// that secretly paged by time — or by id, which is uuidv7 and therefore also
// time-ordered — would still return every row, just in the wrong order and with
// the wrong rows at each boundary. Asserting the *sequence* is what catches
// that; asserting the set would not.
func TestBYOKPagingFollowsTheDisplayOrder(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	ctx := context.Background()
	seedVendorProvider(t, f, "openai", "https://api.openai.test")
	seedVendorProvider(t, f, "anthropic", "https://api.anthropic.test")

	for _, k := range []struct{ vendor, name string }{
		{"openai", "zeta"}, {"anthropic", "yankee"}, {"openai", "alpha"},
		{"anthropic", "bravo"}, {"openai", "mike"},
	} {
		if _, err := s.CreateOrgProviderKey(ctx, gwconsoleapi.CreateOrgProviderKeyRequestObject{
			OrgId: orgParam(f.orgA),
			Body: &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
				Vendor: k.vendor, Name: k.name, Secret: byokSecret,
			},
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", k.vendor, k.name, err)
		}
	}
	want := []string{
		"anthropic/bravo", "anthropic/yankee",
		"openai/alpha", "openai/mike", "openai/zeta",
	}

	limit := 2
	var got []string
	var cursor *string
	for round := 0; ; round++ {
		if round > 10 {
			t.Fatal("paging did not terminate: the cursor is not advancing")
		}
		listed, err := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{
			OrgId:  orgParam(f.orgA),
			Params: gwconsoleapi.ListOrgProviderKeysParams{Cursor: cursor, Limit: &limit},
		})
		if err != nil {
			t.Fatalf("page %d: %v", round, err)
		}
		body := listed.(gwconsoleapi.ListOrgProviderKeys200JSONResponse)
		if len(body.Items) > limit {
			t.Fatalf("page %d returned %d rows for a limit of %d — the probe row leaked out",
				round, len(body.Items), limit)
		}
		// The vendor list describes the deployment, not the page. A form that
		// only works on page one would be a worse bug than an unpaginated list.
		if len(body.Vendors) == 0 {
			t.Fatalf("page %d dropped the vendor list", round)
		}
		for _, k := range body.Items {
			got = append(got, k.Vendor+"/"+k.Name)
		}
		if body.NextCursor == nil {
			break
		}
		cursor = body.NextCursor
	}

	if len(got) != len(want) {
		t.Fatalf("paged %d rows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q\n full sequence: %v", i, got[i], want[i], got)
		}
	}
}

// A cursor minted for a different key shape is refused, not misread.
func TestBYOKRefusesACursorFromAnotherEndpoint(t *testing.T) {
	f := newFixture(t)
	s := byokServer(t, f)
	// Three components where this endpoint packs two: the shape another list
	// with a deeper key would produce.
	foreign := cursorpage.EncodeKey("a", "b", "c")
	_, err := s.ListOrgProviderKeys(context.Background(), gwconsoleapi.ListOrgProviderKeysRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.ListOrgProviderKeysParams{Cursor: &foreign},
	})
	var coded *httpx.CodeError
	if !errors.As(err, &coded) || coded.Code != errcode.CommonValidation {
		t.Fatalf("foreign cursor = %v, want a validation error", err)
	}
}
