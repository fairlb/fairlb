package gwstaffapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
	"github.com/fairlb/fairlb/settings"
)

func newServer(t *testing.T) (*gwstaffapi.Server, *pgxpool.Pool, breaker.Store) {
	t.Helper()
	pool := testpg.Start(t)
	brk := breaker.NewMemory()
	return serverForPool(t, pool, brk, nil), pool, brk
}

func serverForPool(
	t *testing.T,
	pool *pgxpool.Pool,
	brk breaker.Store,
	configure func(*gwstaffapi.ServerConfig),
) *gwstaffapi.Server {
	t.Helper()
	q := gwdb.New(pool)
	cat := catalog.NewService(q, nil, settings.New(pool, nil, settings.NewRegistry(), nil))
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := gwstaffapi.ServerConfig{
		Pool: pool, Catalog: cat, Breaker: brk, Cipher: box,
	}
	if configure != nil {
		configure(&cfg)
	}
	return gwstaffapi.NewServer(cfg)
}

// vendorCustom is what a provider fixture declares unless the test is about the
// vendor itself: these tests are about everything else, and the custom vendor is
// the one that constrains nothing.
var vendorCustom = catalog.VendorCustom

func TestProviderCostMultiplierTerminalBounds(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	protocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	slug, baseURL, maximum := "provider-cost-bound", "https://up.test", 100_000
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
			Slug: &slug, Protocols: &protocols, BaseUrl: &baseURL, CostMultiplierBps: &maximum,
		},
	})
	if err != nil {
		t.Fatalf("100000 must be accepted: %v", err)
	}
	provider := created.(gwstaffapi.CreateGatewayProvider201JSONResponse)
	if provider.CostMultiplierBps != maximum {
		t.Fatalf("stored multiplier = %d, want %d", provider.CostMultiplierBps, maximum)
	}

	tooHigh := 100_001
	badSlug := "provider-cost-too-high"
	_, createErr := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
			Slug: &badSlug, Protocols: &protocols, BaseUrl: &baseURL, CostMultiplierBps: &tooHigh,
		},
	})
	assertUnprocessable(t, createErr)
	_, updateErr := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: provider.Id,
		Body:       &gwstaffapi.GatewayProviderInput{CostMultiplierBps: &tooHigh},
	})
	assertUnprocessable(t, updateErr)
}

func assertUnprocessable(t *testing.T, err error) {
	t.Helper()
	var coded *httpx.CodeError
	if !errors.As(err, &coded) || coded.Code != errcode.CommonUnprocessable {
		t.Fatalf("want %s, got %v", errcode.CommonUnprocessable, err)
	}
	recorder := httptest.NewRecorder()
	httpx.OAPIResponseError(recorder, httptest.NewRequest("POST", "/", nil), err)
	if recorder.Code != 422 {
		t.Fatalf("out-of-range write status = %d, want 422", recorder.Code)
	}
}

// Provider CRUD, and what it means for an operator to take over a provider that
// health checks disabled.
func TestProviderCRUDAndTakeover(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	protocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	slug, baseURL := "p-admin", "https://up.test"
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom, Slug: &slug, Protocols: &protocols, BaseUrl: &baseURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := created.(gwstaffapi.CreateGatewayProvider201JSONResponse)
	if !ok {
		t.Fatalf("it should return 201: %T", created)
	}
	if !p.Enabled || p.AutoDisabled {
		t.Fatalf("a newly created provider should be enabled and not auto-disabled: %+v", p)
	}

	// Simulate a health check disabling it automatically.
	if _, err := pool.Exec(ctx,
		`UPDATE providers SET enabled = false, auto_disabled = true WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}

	// Enabling by hand must clear auto_disabled: otherwise the next failed
	// health check disables it again, and a background job keeps overwriting
	// a human decision.
	enabled := true
	updated, err := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: p.Id,
		Body:       &gwstaffapi.GatewayProviderInput{Enabled: &enabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := updated.(gwstaffapi.UpdateGatewayProvider200JSONResponse)
	if !up.Enabled || up.AutoDisabled {
		t.Fatalf("a manual enable should take the provider over and clear the auto-disabled flag: %+v", up)
	}
}

// Header map round-trip: what goes in comes back out, including the ${api_key}
// placeholder and the empty-string-means-remove semantics.
func TestProviderHeadersRoundTrip(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	protocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	slug, baseURL := "p-hdr", "https://up.test"
	headers := map[string]string{"api-key": "${api_key}", "Authorization": "", "X-Title": "flb"}
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
			Slug: &slug, Protocols: &protocols, BaseUrl: &baseURL, Headers: &headers,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := created.(gwstaffapi.CreateGatewayProvider201JSONResponse)
	if p.Headers == nil {
		t.Fatal("the header mapping should survive the round trip")
	}
	got := *p.Headers
	if got["api-key"] != "${api_key}" || got["X-Title"] != "flb" {
		t.Fatalf("the header mapping content does not match: %v", got)
	}
	if _, has := got["Authorization"]; !has {
		t.Error("an empty-string value (delete semantics) must be kept too -- it is meaningful configuration, not an absent one")
	}
}

// The model list carries the union of route capabilities, defined the same way
// /v1/models defines it.
func TestModelListShowsEndpointUnion(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var provID, modelID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ('p1','custom',ARRAY['openai'],'https://u') RETURNING id`).
		Scan(&provID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug) VALUES ('openai/m') RETURNING id`).
		Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, pool, modelID, provID, "up-a", "chat")
	catalogtest.SeedRoute(t, pool, modelID, provID, "up-b", "images")

	resp, err := s.ListGatewayModels(ctx, gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	list := resp.(gwstaffapi.ListGatewayModels200JSONResponse)
	if len(list.Items) != 1 {
		t.Fatalf("there should be 1 model: %d", len(list.Items))
	}
	if len(list.Items[0].Endpoints) != 2 {
		t.Fatalf("capabilities should be the union of the two routes: %v", list.Items[0].Endpoints)
	}
	if list.Items[0].RouteCount == nil || *list.Items[0].RouteCount != 2 {
		t.Errorf("there should be 2 routes: %v", list.Items[0].RouteCount)
	}
}

// A free model must be recognizable in the list.
//
// Being free is the one exemption from "no price means unpriced means refuse
// service". If the list queries that fact and does not return it, the operator
// page labels a correctly configured free model "unpriced, refusing traffic" --
// an outage that does not exist.
func TestModelListCarriesIsFree(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	// Whether a model is free lives in model_pricing.billing_mode. The fixture
	// builds data the way the real write path does, rather than stuffing a
	// value into a column that no longer exists: a fixture writing dead
	// columns in raw SQL is a fossil of the defect it was meant to catch,
	// because it can manufacture states production cannot reach.
	if _, err := pool.Exec(ctx, `
		WITH m AS (
		  INSERT INTO models (slug) VALUES ('openai/free'),('openai/paid')
		  RETURNING id, slug
		)
		INSERT INTO model_pricing (
		  model_id, billing_mode,
		  upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
		  upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
		  multiplier_bps, source_name
		)
		SELECT m.id,
		       CASE WHEN m.slug = 'openai/free' THEN 'free' ELSE 'paid' END,
		       1000000000, 2000000000, 0, 0, 10000, 'test-fixture'
		  FROM m`); err != nil {
		t.Fatal(err)
	}

	resp, err := s.ListGatewayModels(ctx, gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]gwstaffapi.GatewayModel{}
	for _, m := range resp.(gwstaffapi.ListGatewayModels200JSONResponse).Items {
		bySlug[m.Slug] = m
	}

	// Assert both sides: checking only the free model would pass for an
	// implementation that hardcodes the field to true.
	free, ok := bySlug["openai/free"]
	if !ok {
		t.Fatal("the free model is missing from the listing")
	}
	if free.IsFree == nil || !*free.IsFree {
		t.Errorf("an explicitly free model should carry is_free=true: %v", free.IsFree)
	}
	paid, ok := bySlug["openai/paid"]
	if !ok {
		t.Fatal("the paid model is missing from the listing")
	}
	if paid.IsFree == nil || *paid.IsFree {
		t.Errorf("a non-free model should carry is_free=false: %v", paid.IsFree)
	}
}

// Whether a person has checked a price has to reach the list.
//
// A bulk import writes several hundred rows nobody has checked, and without
// this field the list cannot tell those from a rate somebody agreed to charge
// against. The model's own page can -- through an empty checked-on date -- but
// one model at a time, and only if somebody goes and looks.
//
// Three states, and the third is the one worth being careful about: absent has
// to keep meaning "there is no price row here". Sent as an omission for an
// unverified price, every unpriced model would read as unverified too, which is
// a different sentence about a different problem.
func TestModelListCarriesWhetherThePriceWasVerified(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		WITH m AS (
		  INSERT INTO models (slug)
		  VALUES ('openai/checked'),('openai/imported'),('openai/unpriced')
		  RETURNING id, slug
		)
		INSERT INTO model_pricing (
		  model_id, billing_mode,
		  upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
		  upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
		  multiplier_bps, source_name, verified_at
		)
		SELECT m.id, 'paid', 1000000000, 2000000000, 0, 0, 10000, 'test-fixture',
		       CASE WHEN m.slug = 'openai/checked'
		            THEN timestamptz '2026-01-02 00:00:00+00' END
		  FROM m WHERE m.slug <> 'openai/unpriced'`); err != nil {
		t.Fatal(err)
	}

	resp, err := s.ListGatewayModels(ctx, gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]gwstaffapi.GatewayModel{}
	for _, m := range resp.(gwstaffapi.ListGatewayModels200JSONResponse).Items {
		bySlug[m.Slug] = m
	}
	for _, want := range []struct {
		slug     string
		verified *bool
	}{
		{"openai/checked", ptrTo(true)},
		{"openai/imported", ptrTo(false)},
		{"openai/unpriced", nil},
	} {
		got, ok := bySlug[want.slug]
		if !ok {
			t.Fatalf("%s is missing from the listing", want.slug)
		}
		switch {
		case want.verified == nil && got.PriceVerified != nil:
			t.Errorf("%s has no price row, so price_verified must be absent, not %v",
				want.slug, *got.PriceVerified)
		case want.verified != nil && got.PriceVerified == nil:
			t.Errorf("%s has a price row but price_verified is absent", want.slug)
		case want.verified != nil && *got.PriceVerified != *want.verified:
			t.Errorf("%s: price_verified is %v, want %v",
				want.slug, *got.PriceVerified, *want.verified)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }

// The health dashboard reads breaker state as an in-memory snapshot taken at
// query time.
func TestHealthReportsBreakerSnapshot(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ('p-health','custom',ARRAY['openai'],'https://u')`); err != nil {
		t.Fatal(err)
	}
	resp, err := s.GetGatewayHealth(ctx, gwstaffapi.GetGatewayHealthRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	h := resp.(gwstaffapi.GetGatewayHealth200JSONResponse)
	if len(h.Providers) != 1 {
		t.Fatalf("there should be 1 provider: %d", len(h.Providers))
	}
	// Use literals rather than the generated bare constants: names like
	// Closed and Open carry no prefix and collide easily.
	if h.Providers[0].BreakerStatus != "closed" {
		t.Errorf("with the breaker untripped it should be closed: %s", h.Providers[0].BreakerStatus)
	}
}

// The vendor is required at creation and has to be one this build knows.
//
// Both halves matter for different reasons. Missing means a provider whose
// organization credentials, price scope and discovery behaviour have no answer;
// unknown means one whose answer this binary cannot look up, which would then
// degrade silently at every one of those three points rather than at the write
// that caused it.
func TestProviderCreateRequiresAKnownVendor(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	protocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	baseURL := "https://up.test"

	slug := "p-no-vendor"
	_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Slug: &slug, Protocols: &protocols, BaseUrl: &baseURL},
	})
	assertValidation(t, err, "vendor")

	slug, unknown := "p-unknown-vendor", "acme-intelligence"
	_, err = s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &unknown, Protocols: &protocols, BaseUrl: &baseURL,
		},
	})
	assertValidation(t, err, unknown)
}

// A provider may only declare dialects its vendor publishes. Declared but
// unspoken, a dialect produces routes that save without complaint, never become
// candidates, and read as configured -- the failure this check exists to move
// forward to the form.
func TestProviderProtocolsMustBeOnesTheVendorPublishes(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	baseURL := "https://up.test"
	openai := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	both := []gwstaffapi.GatewayProviderInputProtocols{
		gwstaffapi.GatewayProviderInputProtocolsOpenai,
		gwstaffapi.GatewayProviderInputProtocolsAnthropic,
	}

	slug, anthropicVendor := "p-anthropic-openai", "anthropic"
	_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &anthropicVendor, Protocols: &openai, BaseUrl: &baseURL,
		},
	})
	assertValidation(t, err, "anthropic")

	// DeepSeek publishes both, and one record serving both is the shape its
	// preset is for.
	slug, deepseek := "p-deepseek", "deepseek"
	if _, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &deepseek, Protocols: &both, BaseUrl: &baseURL,
		},
	}); err != nil {
		t.Fatalf("DeepSeek speaks both of its own dialects: %v", err)
	}

	// The custom vendor constrains nothing: it is the entry for upstreams this
	// build has no knowledge of.
	slug = "p-custom-both"
	if _, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &vendorCustom, Protocols: &both, BaseUrl: &baseURL,
		},
	}); err != nil {
		t.Fatalf("custom should accept any known dialect: %v", err)
	}
}

// The pair constrains itself, so a partial update of either side is checked
// against the stored value of the other. Without this, moving a provider to a
// vendor that does not speak its current dialects arrives by a request that
// mentions neither of the two fields the rule is about.
func TestProviderVendorChangeIsCheckedAgainstStoredProtocols(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	baseURL := "https://up.test"
	openai := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	slug := "p-move-vendor"
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &vendorCustom, Protocols: &openai, BaseUrl: &baseURL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := created.(gwstaffapi.CreateGatewayProvider201JSONResponse).Id

	anthropicVendor := "anthropic"
	_, err = s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: id,
		Body:       &gwstaffapi.GatewayProviderInput{Vendor: &anthropicVendor},
	})
	assertValidation(t, err, "anthropic")

	// Moving to a vendor that does speak the stored dialect is ordinary.
	openaiVendor := "openai"
	updated, err := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: id,
		Body:       &gwstaffapi.GatewayProviderInput{Vendor: &openaiVendor},
	})
	if err != nil {
		t.Fatalf("openai speaks the openai dialect: %v", err)
	}
	if got := updated.(gwstaffapi.UpdateGatewayProvider200JSONResponse).Vendor; got != openaiVendor {
		t.Errorf("stored vendor = %q, want %q", got, openaiVendor)
	}
}

// A base URL copied from a preset without replacing its placeholder resolves to
// nothing: the request fails as a DNS error, which reads like an upstream
// outage rather than like a form nobody finished.
func TestProviderRejectsAnUnfinishedPresetBaseURL(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	protocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	slug, azure := "p-azure-template", "azure-openai"
	unfinished := "https://{resource}.openai.azure.com/openai"
	_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &azure, Protocols: &protocols, BaseUrl: &unfinished,
		},
	})
	assertValidation(t, err, "placeholder")
}

// The same rule for the profile, which is where the hosted-platform presets
// actually keep their placeholders: a project and a region only the operator can
// supply. Guarding the base URL and not this left the ones that really ship
// unguarded -- and an unreplaced one there is worse, because the address
// resolves, answers 404 on every request and opens the provider's circuit.
func TestProviderRejectsAnUnfinishedPresetProfile(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	protocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsAnthropic}
	slug, vertex := "p-vertex-template", "google-vertex"
	baseURL := "https://us-east5-aiplatform.googleapis.com"
	profile := map[string]any{
		"auth":     "gcp_service_account",
		"envelope": "vertex",
		"path_overrides": map[string]any{
			"/v1/messages": "/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:rawPredict",
		},
	}
	_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slug, Vendor: &vertex, Protocols: &protocols, BaseUrl: &baseURL,
			Transport: &profile,
		},
	})
	assertValidation(t, err, "placeholder")

	// {model} and {resource} are the placeholders the gateway substitutes per
	// request; this profile uses the model form.
	finished := map[string]any{
		"auth":     "gcp_service_account",
		"envelope": "vertex",
		"path_overrides": map[string]any{
			"/v1/messages": "/v1/projects/acme/locations/us-east5/publishers/anthropic/models/{model}:rawPredict",
		},
	}
	slugOK := "p-vertex-done"
	if _, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &slugOK, Vendor: &vertex, Protocols: &protocols, BaseUrl: &baseURL,
			Transport: &finished,
		},
	}); err != nil {
		t.Fatalf("a finished profile must save: %v", err)
	}

	resourceProtocols := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	resourceProfile := map[string]any{
		"path_overrides": map[string]any{
			"/v1/responses/{resource}": "/stored-responses/{resource}",
		},
	}
	resourceSlug, custom := "p-resource-placeholder", "custom"
	resourceBaseURL := "https://relay.example"
	if _, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug: &resourceSlug, Vendor: &custom, Protocols: &resourceProtocols,
			BaseUrl: &resourceBaseURL, Transport: &resourceProfile,
		},
	}); err != nil {
		t.Fatalf("the runtime-substituted resource placeholder must save: %v", err)
	}
}

// An envelope re-cuts an Anthropic Messages request, so declaring one on a
// provider that speaks another dialect saves a record whose every request has
// its model deleted and an anthropic_version added -- the upstream answers 400
// and the blame lands on it. The registry's own presets were held to this rule
// by a test; the path where an operator types a profile was not.
func TestProviderRejectsAnEnvelopeOnAnotherProtocol(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	slug, custom := "p-envelope-mismatch", "custom"
	baseURL := "https://relay.example"
	profile := map[string]any{"envelope": "bedrock"}
	for _, protocol := range []gwstaffapi.GatewayProviderInputProtocols{
		gwstaffapi.GatewayProviderInputProtocolsOpenai,
		gwstaffapi.GatewayProviderInputProtocolsGemini,
	} {
		protocols := []gwstaffapi.GatewayProviderInputProtocols{protocol}
		_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
			Body: &gwstaffapi.GatewayProviderInput{
				Slug: &slug, Vendor: &custom, Protocols: &protocols, BaseUrl: &baseURL,
				Transport: &profile,
			},
		})
		assertValidation(t, err, "envelope")
	}
}

// The registry endpoint reports the registry, whole. A filter here would make
// the create form's options depend on what is already configured, which is
// backwards for the form whose job is to configure the first one.
func TestListGatewayVendorsMirrorsTheRegistry(t *testing.T) {
	s, _, _ := newServer(t)
	// The endpoint is staff-only like every other read on this plane, so the
	// call needs a subject; anonymous is covered by the plane's own middleware
	// tests rather than repeated here.
	ctx := httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "admin", Subject: uuid.NewString(), Role: "operator",
	})
	resp, err := s.ListGatewayVendors(ctx, gwstaffapi.ListGatewayVendorsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.(gwstaffapi.ListGatewayVendors200JSONResponse).Items
	want := catalog.Vendors()
	if len(got) != len(want) {
		t.Fatalf("endpoint returned %d vendors, the registry has %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Slug != want[i].Slug || got[i].Label != want[i].Label {
			t.Errorf("entry %d = %s/%s, registry has %s/%s",
				i, got[i].Slug, got[i].Label, want[i].Slug, want[i].Label)
		}
		if len(got[i].Protocols) != len(want[i].Protocols) {
			t.Errorf("%s: protocols %v, registry has %v", want[i].Slug, got[i].Protocols, want[i].Protocols)
		}
		if len(got[i].BaseUrls) != len(want[i].BaseURLs) {
			t.Errorf("%s: %d base URLs, registry has %d",
				want[i].Slug, len(got[i].BaseUrls), len(want[i].BaseURLs))
		}
		hasPreset := len(want[i].Transport.PathOverrides) > 0 || want[i].Transport.Auth != "" ||
			want[i].Transport.Envelope != ""
		if hasPreset && got[i].Transport == nil {
			t.Errorf("%s has a transport preset that the endpoint dropped", want[i].Slug)
		}
	}
}

func assertValidation(t *testing.T, err error, mustMention string) {
	t.Helper()
	var coded *httpx.CodeError
	if !errors.As(err, &coded) || coded.Code != errcode.CommonValidation {
		t.Fatalf("want %s, got %v", errcode.CommonValidation, err)
	}
	if !strings.Contains(coded.Error(), mustMention) {
		t.Errorf("the refusal should name %q, got %q", mustMention, coded.Error())
	}
}
