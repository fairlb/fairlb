package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/settings"
)

// The authenticated /v1/models must apply the same three-way narrowing the
// dataplane does: globally available, intersected with the admission tier,
// intersected with the key's allowlist. Drop any one and a organization sees a model
// in the catalogue that is certain to 404 when called -- an inconsistency that
// turns straight into a support ticket.
func TestModelsHandlerThreeLayerIntersection(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	cat := catalog.NewService(gwdb.New(f.pool), nil, settings.New(f.pool, nil, settings.NewRegistry(), nil))
	h := proxy.ModelsHandler(
		proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil),
		proxy.NewGuard(f.keyStore, nil), cat,
	)

	prov := seedProv(t, f.pool, "p-cat", "openai")
	inTier := seedPricedModel(t, f.pool, "openai/in-tier", prov)
	seedPricedModel(t, f.pool, "openai/out-tier", prov)

	// No tier configured falls back to the default, which admits everything, so
	// both models appear.
	plaintext, row, orgID := f.seedKey(t, apikeys.CreateInput{})
	if got := slugsFrom(t, h, plaintext); !hasAll(got, "openai/in-tier", "openai/out-tier") {
		t.Fatalf("the default tier admits everything, so it should list every model: %v", got)
	}

	// Create a tier holding only the in-tier model and point the org at it, so
	// the other one disappears.
	var tier string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model_tiers (slug, allow_all_models) VALUES ('cat-tier', false) RETURNING id`).Scan(&tier); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model_tier_models (tier_id, model_id) VALUES ($1::uuid, $2::uuid)`,
		tier, inTier); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO org_gateway_settings (org_id, tier_id) VALUES ($1, $2::uuid)`,
		orgID, tier); err != nil {
		t.Fatal(err)
	}
	got := slugsFrom(t, h, plaintext)
	if hasAll(got, "openai/out-tier") {
		t.Errorf("a model outside the tier must not appear in the catalogue: %v", got)
	}
	if !hasAll(got, "openai/in-tier") {
		t.Errorf("a model inside the tier should appear: %v", got)
	}

	// Then layer on the key allowlist, the innermost narrowing: whatever is off
	// the list must leave the catalogue too.
	if _, err := f.pool.Exec(ctx,
		`UPDATE api_keys SET allow_all_models = false, allowed_models = ARRAY['openai/nothing'] WHERE id = $1`,
		row.ID); err != nil {
		t.Fatal(err)
	}
	if got := slugsFrom(t, h, plaintext); len(got) != 0 {
		t.Errorf("with everything excluded by the key allowlist the catalogue should be empty: %v", got)
	}

	// An allowlist emptied altogether narrows to nothing too. It is the same
	// answer as above but a different configuration -- and under the old shape
	// it was the opposite answer, because an empty list meant "unrestricted".
	if _, err := f.pool.Exec(ctx,
		`UPDATE api_keys SET allow_all_models = false, allowed_models = ARRAY[]::text[] WHERE id = $1`,
		row.ID); err != nil {
		t.Fatal(err)
	}
	if got := slugsFrom(t, h, plaintext); len(got) != 0 {
		t.Errorf("a key restricted to no models should see an empty catalogue: %v", got)
	}
}

func TestModelsHandlerReturnsExplicitBillingMode(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	cat := catalog.NewService(gwdb.New(f.pool), nil, settings.New(f.pool, nil, settings.NewRegistry(), nil))
	h := proxy.ModelsHandler(
		proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil),
		proxy.NewGuard(f.keyStore, nil), cat,
	)
	providerID := seedProv(t, f.pool, "p-billing-mode", "openai")
	seedPricedModel(t, f.pool, "openai/paid-mode", providerID)
	var freeID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO models (slug)
		VALUES ('openai/free-mode') RETURNING id`).Scan(&freeID); err != nil {
		t.Fatal(err)
	}
	// Free is expressed by the billing mode, never guessed from "all four
	// prices are zero".
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok, source_name)
		VALUES ($1, 'free', 0, 0, 0, 0, 'test-fixture')`, freeID); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, f.pool, freeID, providerID, "free-upstream", "chat")
	plaintext, _, _ := f.seedKey(t, apikeys.CreateInput{})
	rec := call(h, plaintext)
	if rec.Code != http.StatusOK {
		t.Fatalf("the catalogue should answer 200, got %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Input string `json:"input_per_mtok"`
			} `json:"pricing"`
			Meta struct {
				BillingMode string `json:"billing_mode"`
			} `json:"fairlb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]struct{ mode, input string }, len(body.Data))
	for _, model := range body.Data {
		got[model.ID] = struct{ mode, input string }{model.Meta.BillingMode, model.Pricing.Input}
	}
	if got["openai/paid-mode"].mode != "paid" {
		t.Fatalf("the authenticated catalogue does not state paid: %+v", got)
	}
	if free := got["openai/free-mode"]; free.mode != "free" || free.input != "0" {
		t.Fatalf("the authenticated catalogue does not state free, or still exposes a leftover price: %+v", got)
	}
}

// The catalogue publishes what has been verified, and only that: the
// protocols are those of the verified endpoints, not the providers' declared
// sets, and an endpoint with no verdict -- callable, since nothing has found
// it unsupported -- is not listed. Listing what the caller cannot act on is
// the failure the catalogue exists to prevent; omitting what they could is
// free. The one derivation is the stored-resource operations, which ride on
// `responses` because they can only ever be reached pinned to it.
func TestModelsHandlerPublishesOnlyVerifiedOperations(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	cat := catalog.NewService(gwdb.New(f.pool), nil, settings.New(f.pool, nil, settings.NewRegistry(), nil))
	h := proxy.ModelsHandler(
		proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil),
		proxy.NewGuard(f.keyStore, nil), cat,
	)
	// The provider declares anthropic as well; with nothing verified on that
	// protocol, the catalogue must not repeat the declaration. It holds a
	// shared credential, so the worker can look: an unverified endpoint here
	// is "not yet", not "nobody can know" (that case publishes, and is
	// covered on the view itself).
	var providerID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('p-operation-catalog', 'custom', ARRAY['openai','anthropic'], 'https://u.test') RETURNING id`).
		Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1::uuid, 'k', '\x00')`, providerID); err != nil {
		t.Fatal(err)
	}
	modelID := seedPricedModel(t, f.pool, "openai/operation-catalog", providerID)
	var routeID string
	if err := f.pool.QueryRow(ctx, `SELECT id FROM model_routes WHERE model_id = $1`, modelID).Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedVerdict(t, f.pool, routeID, "responses", "ok")
	catalogtest.SeedVerdict(t, f.pool, routeID, "embeddings", "failed")
	catalogtest.SeedVerdict(t, f.pool, routeID, "images", "unsupported")
	// messages is seeded but unverified: callable, unlisted.
	catalogtest.SeedVerdict(t, f.pool, routeID, "messages", "unverified")
	plaintext, _, _ := f.seedKey(t, apikeys.CreateInput{})

	rec := call(h, plaintext)
	if rec.Code != http.StatusOK {
		t.Fatalf("the catalogue should answer 200, got %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				Protocols           []string `json:"protocols"`
				SupportedOperations []string `json:"supported_operations"`
			} `json:"fairlb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, model := range body.Data {
		if model.ID != "openai/operation-catalog" {
			continue
		}
		if !slices.Equal(model.Meta.Protocols, []string{"openai"}) {
			t.Fatalf("protocols must be those of the verified endpoints, not the provider's declaration: %+v", model.Meta)
		}
		if !slices.Equal(model.Meta.SupportedOperations, []string{"chat", "responses", "responses_resources"}) {
			t.Fatalf("operations must be the verified ones plus the stored-resource operations that ride on responses: %+v", model.Meta)
		}
		return
	}
	t.Fatal("seeded model was missing from the catalogue")
}

// A disabled tier answers 403 rather than an empty list. An empty list reads as
// "this deployment has no models", when what is really true is that this
// account's admission configuration is not in effect, and the two call for
// entirely different responses.
func TestModelsHandlerTierDisabledIs403(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	cat := catalog.NewService(gwdb.New(f.pool), nil, settings.New(f.pool, nil, settings.NewRegistry(), nil))
	h := proxy.ModelsHandler(
		proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil),
		proxy.NewGuard(f.keyStore, nil), cat,
	)
	plaintext, _, orgID := f.seedKey(t, apikeys.CreateInput{})

	var tier string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model_tiers (slug, status) VALUES ('off', 'disabled') RETURNING id`).
		Scan(&tier); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO org_gateway_settings (org_id, tier_id) VALUES ($1, $2::uuid)`,
		orgID, tier); err != nil {
		t.Fatal(err)
	}

	rec := call(h, plaintext)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a disabled tier should answer 403, got %d: %s", rec.Code, rec.Body)
	}
}

// No credential means 401, rendered in OpenAI's native error shape rather than
// the management plane's problem+json. This endpoint used to be unauthenticated,
// so this behaviour is part of its outward contract.
func TestModelsHandlerRequiresAuth(t *testing.T) {
	f := newAuthFixture(t)
	cat := catalog.NewService(gwdb.New(f.pool), nil, settings.New(f.pool, nil, settings.NewRegistry(), nil))
	h := proxy.ModelsHandler(
		proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil),
		proxy.NewGuard(f.keyStore, nil), cat,
	)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential should answer 401, got %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct{ Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error body should be OpenAI's native shape: %v (%s)", err, rec.Body)
	}
	if body.Error.Type == "" {
		t.Errorf("error.type is missing: %s", rec.Body)
	}
}

// /v1/models is a shared endpoint with no protocol to consult, which is precisely
// why the credential headers are not split per protocol. The Anthropic SDK's model
// listing sends x-api-key too.
func TestModelsAcceptsXAPIKey(t *testing.T) {
	f := newAuthFixture(t)
	plaintext, _, _ := f.seedKey(t, apikeys.CreateInput{})
	cat := catalog.NewService(gwdb.New(f.pool), nil, settings.New(f.pool, nil, settings.NewRegistry(), nil))
	h := proxy.ModelsHandler(
		proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil),
		proxy.NewGuard(f.keyStore, nil), cat,
	)

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("x-api-key", plaintext)
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key should be accepted, got %d: %s", rec.Code, rec.Body)
	}
}

// ===== Fixtures =====

func call(h http.HandlerFunc, plaintext string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func slugsFrom(t *testing.T, h http.HandlerFunc, plaintext string) []string {
	t.Helper()
	rec := call(h, plaintext)
	if rec.Code != http.StatusOK {
		t.Fatalf("the catalogue should answer 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	slugs := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		slugs = append(slugs, d.ID)
	}
	return slugs
}

func hasAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func seedProv(t *testing.T, pool *pgxpool.Pool, slug, protocol string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, 'custom', ARRAY[$2], 'https://u.test') RETURNING id`,
		slug, protocol).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedPricedModel creates a model that both has a price and has a usable route.
// The catalogue query requires both; missing either, the model does not appear
// and the test fails somewhere that has nothing to do with admission.
func seedPricedModel(t *testing.T, pool *pgxpool.Pool, slug, provider string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug)
		 VALUES ($1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok, source_name)
		VALUES ($1, 'paid', 3000000000, 15000000000, 0, 0, 'test-fixture')`, id); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, pool, id, provider, "up", "chat")
	return id
}
