package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/settings"
)

type fixture struct {
	pool *pgxpool.Pool
	svc  *catalog.Service
	set  *settings.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testpg.Start(t)
	set := settings.New(pool, nil, settings.NewRegistry(), nil)
	return &fixture{pool: pool, svc: catalog.NewService(gwdb.New(pool), nil, set), set: set}
}

func TestProviderTransportHealthAndLastKnownGood(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "transport-lkg", "openai")
	model := f.model(t, "openai/transport-lkg")
	f.route(t, model, provider, "upstream-model", []string{"chat"})
	if _, err := f.pool.Exec(ctx,
		`UPDATE providers SET transport = '{"auth":"header:api-key","connect_timeout_ms":3000}' WHERE id = $1`,
		provider); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.TransportHealth(ctx); err != nil {
		t.Fatalf("valid initial catalog must be ready: %v", err)
	}

	if _, err := f.pool.Exec(ctx,
		`UPDATE providers SET transport = '{"retries":3}' WHERE id = $1`, provider); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.TransportHealth(ctx); err == nil {
		t.Fatal("invalid hot catalog must fail readiness")
	}
	resolved, err := f.svc.Resolve(ctx, "openai/transport-lkg", catalog.SurfaceChat, pgtype.UUID{})
	if err != nil {
		t.Fatalf("hot load must retain the last-known-good transport: %v", err)
	}
	if got := resolved.Routes[0].Transport.Auth; got != "header:api-key" {
		t.Fatalf("route used transport auth %q, want last-known-good profile", got)
	}

	fresh := catalog.NewService(gwdb.New(f.pool), nil, f.set)
	if _, err := fresh.Resolve(ctx, "openai/transport-lkg", catalog.SurfaceChat, pgtype.UUID{}); err == nil {
		t.Fatal("a fresh process must fail closed when no valid transport was ever loaded")
	}
}

func (f *fixture) provider(t *testing.T, slug, protocol string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, 'custom', ARRAY[$2], 'https://up.test') RETURNING id`,
		slug, protocol).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) model(t *testing.T, slug string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO models (slug, max_output_tokens) VALUES ($1, 4096) RETURNING id`,
		slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// A model's single current price lives in model_pricing.
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok, source_name)
		VALUES ($1, 'paid', 3000000000, 15000000000, 0, 0, 'test-fixture')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// route wires a model to a provider and records the given endpoints as
// verified (`ok`), which is what the catalog publishes. A route declares
// nothing itself; the data plane tries any endpoint not found unsupported, so
// the verified set here only shapes what is listed, not what is callable.
func (f *fixture) route(t *testing.T, model, provider, upstream string, verified []string) string {
	t.Helper()
	return uuid.UUID(catalogtest.SeedRoute(t, f.pool, model, provider, upstream, verified...).Bytes).String()
}

// verdict writes what is known about one endpoint of a route, the way the
// probe worker would.
func (f *fixture) verdict(t *testing.T, route, endpoint, status string) {
	t.Helper()
	catalogtest.SeedVerdict(t, f.pool, route, endpoint, status)
}

// A multi-dialect provider can serve models from both protocols at run time.
//
// The promise made on the configuration side -- one provider record instead of
// two -- only holds if candidate filtering really tests membership. This
// creates the provider by writing the protocols array in raw SQL, bypassing the
// admin API, so what is exercised is the query itself.
func TestResolveAcceptsMultiDialectProvider(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var agg string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('p-agg', 'custom', ARRAY['anthropic','openai'], 'https://agg.test') RETURNING id`).
		Scan(&agg); err != nil {
		t.Fatal(err)
	}

	oa := f.model(t, "openai/agg-oa")
	an := f.model(t, "anthropic/agg-an")
	f.route(t, oa, agg, "up-oa", []string{"chat", "responses"})
	f.route(t, an, agg, "up-an", []string{"messages"})

	// One provider record; models from either protocol resolve to candidates.
	for _, tc := range []struct {
		slug    string
		surface catalog.Surface
		want    string
	}{
		{"openai/agg-oa", catalog.SurfaceChat, "up-oa"},
		{"openai/agg-oa", catalog.SurfaceResponses, "up-oa"},
		{"anthropic/agg-an", catalog.SurfaceMessages, "up-an"},
	} {
		res, err := f.svc.Resolve(ctx, tc.slug, tc.surface, pgtype.UUID{})
		if err != nil {
			t.Fatalf("%s/%s should resolve: %v", tc.slug, tc.surface, err)
		}
		if len(res.Routes) != 1 || res.Routes[0].ProviderModelID != tc.want {
			t.Fatalf("%s/%s candidates do not match: %+v", tc.slug, tc.surface, res.Routes)
		}
		// Route.Protocol is the dialect used for this request, filled in by the
		// query, not a column on the provider -- a multi-dialect provider has
		// no single value there. The outbound auth header and the
		// same-protocol check both read this.
		famOf := map[catalog.Surface]string{
			catalog.SurfaceChat: "openai", catalog.SurfaceResponses: "openai",
			catalog.SurfaceMessages: "anthropic",
		}
		if got := res.Routes[0].Protocol; got != famOf[tc.surface] {
			t.Errorf("%s: Route.Protocol = %q, want the request-side %q", tc.surface, got, famOf[tc.surface])
		}
	}

	// A model owns no protocol. The same slug is a candidate on every
	// protocol its provider speaks: the model wired for chat above is also
	// reachable on /v1/messages through the same record, with the auth header
	// following the request side. Nothing translated -- the request arrived
	// on anthropic and leaves on anthropic.
	res, err := f.svc.Resolve(ctx, "openai/agg-oa", catalog.SurfaceMessages, pgtype.UUID{})
	if err != nil {
		t.Fatalf("a model on a provider that speaks anthropic should resolve on the messages surface: %v", err)
	}
	if got := res.Routes[0].Protocol; got != "anthropic" {
		t.Errorf("Route.Protocol = %q, want the request-side anthropic", got)
	}

	// The filter was not relaxed to accept everything: the provider's protocol
	// set still applies. A provider that speaks only openai is no candidate on
	// the messages surface, however the model is wired.
	only := f.provider(t, "p-only-openai", "openai")
	solo := f.model(t, "openai/solo")
	f.route(t, solo, only, "up-solo", []string{"chat"})
	if _, err := f.svc.Resolve(ctx, "openai/solo", catalog.SurfaceMessages, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("a model whose only provider speaks openai should be unavailable on the messages surface: %v", err)
	}
	// ... and becomes one the moment a route on an anthropic-speaking provider
	// is added -- with no change to the model itself.
	f.route(t, solo, agg, "up-solo", []string{"messages"})
	if _, err := f.svc.Resolve(ctx, "openai/solo", catalog.SurfaceMessages, pgtype.UUID{}); err != nil {
		t.Fatalf("adding an anthropic-speaking route should make the surface available: %v", err)
	}
}

// A route is a candidate for every automatically probed endpoint of its
// provider's protocols unless a probe has found the endpoint unsupported on
// it. The two other states -- no verdict at all, and an inconclusive failure
// -- let the request through: the upstream is the authority, and a live
// request is how it is asked. Manual endpoints and callers with their own
// credential are the two exceptions, and both are here.
func TestResolveSkipsOnlyEndpointsFoundUnsupported(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provChat := f.provider(t, "p-chat", "openai")
	provImg := f.provider(t, "p-img", "openai")
	m := f.model(t, "openai/gpt-5.4")
	chatRoute := f.route(t, m, provChat, "gpt-5.4", []string{"chat"})

	// Only chat verified: chat resolves, and so does embeddings -- nothing has
	// said the route cannot serve it, so the request is allowed to find out.
	res, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceChat, pgtype.UUID{})
	if err != nil {
		t.Fatalf("chat should resolve: %v", err)
	}
	if len(res.Routes) != 1 || res.Routes[0].ProviderModelID != "gpt-5.4" {
		t.Fatalf("candidates do not match: %+v", res.Routes)
	}
	if _, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceEmbeddings, pgtype.UUID{}); err != nil {
		t.Fatalf("an automatically probed endpoint nobody has verified is still a candidate: %v", err)
	}
	// images is the other way round: the gateway refuses to probe it on its
	// own, so nothing would ever converge if live traffic were the probe. An
	// unobservable endpoint is opt-in -- not a candidate until a verdict says
	// ok -- rather than tried upstream on every request forever.
	if _, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceImages, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("a manual endpoint with no verdict must not be a candidate: %v", err)
	}

	// A definitive verdict removes the candidate for that endpoint only.
	f.verdict(t, chatRoute, "responses", "unsupported")
	if _, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceResponses, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("an endpoint found unsupported on the only route should be unavailable: %v", err)
	}
	if _, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceChat, pgtype.UUID{}); err != nil {
		t.Fatalf("the verdict on responses must not touch chat: %v", err)
	}
	// ... unless the caller brings its own credential for that vendor: the
	// verdict was the shared key's view, and an upstream's 404 also means
	// "your project has no access", which the organization's own key may have.
	if _, err := f.svc.ResolveFor(ctx, "openai/gpt-5.4", catalog.SurfaceResponses, pgtype.UUID{}, []string{"custom"}); err != nil {
		t.Fatalf("a shared-key verdict must not exclude a route for an organization with its own credential at that vendor: %v", err)
	}
	if _, err := f.svc.ResolveFor(ctx, "openai/gpt-5.4", catalog.SurfaceResponses, pgtype.UUID{}, []string{"openai"}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("a credential for another vendor changes nothing: %v", err)
	}

	// An inconclusive verdict does not: a 5xx or a body the upstream would not
	// take is the provider's or the probe's problem, not a statement about
	// the endpoint.
	f.verdict(t, chatRoute, "embeddings", "failed")
	if _, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceEmbeddings, pgtype.UUID{}); err != nil {
		t.Fatalf("a failed probe must not take the route out of rotation: %v", err)
	}

	// Adding a second route on which images is verified routes images only
	// there: the first route has no verdict for it, which for a manual
	// endpoint is not enough.
	f.route(t, m, provImg, "gpt-image-2", []string{"images"})
	res, err = f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceImages, pgtype.UUID{})
	if err != nil {
		t.Fatalf("images should be available once a route is verified for it: %v", err)
	}
	if len(res.Routes) != 1 || res.Routes[0].ProviderModelID != "gpt-image-2" {
		t.Fatalf("images should route only to the route verified for it: %+v", res.Routes)
	}

	// No leaking across protocols: neither provider speaks anthropic, so the
	// messages surface has no candidate. Nothing translates between protocols.
	if _, err := f.svc.Resolve(ctx, "openai/gpt-5.4", catalog.SurfaceMessages, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("a surface no provider speaks should be unavailable: %v", err)
	}
}

// Disabling a provider or a model removes it from the candidates -- the
// provider-level and model-level kill switches.
func TestResolveRespectsDisabled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prov := f.provider(t, "p1", "openai")
	m := f.model(t, "openai/m1")
	f.route(t, m, prov, "m1", []string{"chat"})

	if _, err := f.svc.Resolve(ctx, "openai/m1", catalog.SurfaceChat, pgtype.UUID{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE providers SET enabled = false WHERE id = $1`, prov); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Resolve(ctx, "openai/m1", catalog.SurfaceChat, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("there should be no candidates once the provider is disabled: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE providers SET enabled = true WHERE id = $1`, prov); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE models SET enabled = false WHERE id = $1`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Resolve(ctx, "openai/m1", catalog.SurfaceChat, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("a disabled model should be unavailable: %v", err)
	}

	// A nonexistent model and a disabled one produce the same outward result,
	// so existence is not disclosed.
	if _, err := f.svc.Resolve(ctx, "openai/ghost", catalog.SurfaceChat, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Fatalf("a nonexistent model should be unavailable the same way: %v", err)
	}
}

// GET /v1/models lists only models that are public, enabled and served,
// advertises capabilities as the union of routes, and prices from the
// organization-facing side.
func TestModelsHandler(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prov := f.provider(t, "p1", "openai")
	m := f.model(t, "openai/gpt-5.4")
	f.route(t, m, prov, "gpt-5.4", []string{"chat"})

	// A model nothing serves is not listed.
	f.model(t, "openai/orphan")
	// A hidden model is not listed.
	hidden := f.model(t, "openai/hidden")
	f.route(t, hidden, prov, "hidden", []string{"chat"})
	if _, err := f.pool.Exec(ctx, `UPDATE models SET visibility = 'hidden' WHERE id = $1`, hidden); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	f.svc.PublicModelsHandler()(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code: %d", rec.Code)
	}

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Pricing struct {
				Currency     string `json:"currency"`
				InputPerMTok string `json:"input_per_mtok"`
			} `json:"pricing"`
			Meta struct {
				BillingMode string   `json:"billing_mode"`
				Endpoints   []string `json:"endpoints"`
			} `json:"fairlb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse response: %v (body=%s)", err, rec.Body.String())
	}
	if got.Object != "list" || len(got.Data) != 1 {
		t.Fatalf("exactly 1 model should be listed: %+v", got)
	}
	entry := got.Data[0]
	if entry.ID != "openai/gpt-5.4" {
		t.Fatalf("model id: %s", entry.ID)
	}
	if len(entry.Meta.Endpoints) != 1 || entry.Meta.Endpoints[0] != "chat" {
		t.Fatalf("capabilities should be exposed as the union over the routes: %v", entry.Meta.Endpoints)
	}
	if entry.Meta.BillingMode != "paid" {
		t.Fatalf("a paid model must state billing_mode=paid: %+v", entry.Meta)
	}
	// $3 per million at a multiplier of 1.0 gives 3. There is no global markup
	// setting: the multiplier lives on the price row rather than being
	// assembled from a global value plus a per-model override.
	if entry.Pricing.InputPerMTok != "3" {
		t.Fatalf("catalog pricing should be the official price x the model multiplier: %s", entry.Pricing.InputPerMTok)
	}

	// The model multiplier is that one number: $3 x 1.5 = $4.50 per
	// million.
	if _, err := f.pool.Exec(ctx,
		`UPDATE model_pricing SET multiplier_bps = 15000 WHERE model_id = $1`, m,
	); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	f.svc.PublicModelsHandler()(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data[0].Pricing.InputPerMTok != "4.5" {
		t.Fatalf("the model multiplier should apply directly to the official price: %s", got.Data[0].Pricing.InputPerMTok)
	}

	// Free is its own billing mode: the original rates are retained for later
	// restoration and for cost analysis, while all four customer-facing rates
	// are explicitly 0.
	if _, err := f.pool.Exec(ctx,
		`UPDATE model_pricing SET billing_mode = 'free' WHERE model_id = $1`, m); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	f.svc.PublicModelsHandler()(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data[0].Pricing.InputPerMTok != "0" || got.Data[0].Meta.BillingMode != "free" {
		t.Fatalf("free mode must return a zero price and state billing_mode=free: %+v", got.Data[0])
	}

	// Adding an images route widens the capability union.
	provImg := f.provider(t, "p-img", "openai")
	f.route(t, m, provImg, "gpt-image-2", []string{"images"})
	rec = httptest.NewRecorder()
	f.svc.PublicModelsHandler()(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data[0].Meta.Endpoints) != 2 {
		t.Fatalf("the union should widen to 2: %v", got.Data[0].Meta.Endpoints)
	}
}

func TestModelsHandlerExcludesPaidUnpricedButKeepsExplicitFree(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "p-unpriced-catalog", "openai")
	var unpriced, free string
	// Unpriced means there is no price row, not four zero rates. "Nobody set a
	// price" and "deliberately free" are two different shapes of data.
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO models (slug, enabled, visibility)
		VALUES ('openai/unpriced-catalog', true, 'public')
		RETURNING id`).Scan(&unpriced); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO models (slug, enabled, visibility)
		VALUES ('openai/free-catalog', true, 'public')
		RETURNING id`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	// Only the free one gets a price row, with billing_mode stating it.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok, source_name)
		VALUES ($1, 'free', 0, 0, 0, 0, 'test-fixture')`, free); err != nil {
		t.Fatal(err)
	}
	f.route(t, unpriced, provider, "unpriced", []string{"chat"})
	f.route(t, free, provider, "free", []string{"chat"})

	models, err := f.svc.PublicModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Slug != "openai/free-catalog" || !models[0].IsFree {
		t.Fatalf("an unpriced paid model should be hidden and an explicit free one kept: %+v", models)
	}
}

func TestVersionedCatalogUsesPublishedPriceAndPlanOverride(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "p-versioned-catalog", "openai")
	model := f.model(t, "openai/versioned-catalog")
	f.route(t, model, provider, "versioned-catalog", []string{"chat"})
	// The current price is $5 with a 50% markup.
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET
			upstream_in_nano_per_mtok = 5000000000, upstream_out_nano_per_mtok = 9000000000,
			upstream_cache_read_nano_per_mtok = 1000000000,
			upstream_cache_write_nano_per_mtok = 2000000000,
			multiplier_bps = 15000
		WHERE model_id = $1`, model); err != nil {
		t.Fatal(err)
	}

	var orgID pgtype.UUID
	orgID = orgtest.Create(t, f.pool, orgtest.Seed{Slug: "versioned-catalog-org", Name: "Versioned Catalog"})
	var planID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO pricing_plans (slug, name) VALUES ('catalog-vip', 'Catalog VIP') RETURNING id`).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE pricing_plans SET default_multiplier_bps = 9000 WHERE id = $1`, planID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO pricing_plan_model_overrides (pricing_plan_id, model_id, multiplier_bps)
		VALUES ($1, $2, 8000)`, planID, model); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO org_pricing_plan_assignments (org_id, pricing_plan_id, reason)
		VALUES ($1, $2, 'catalog test')`, orgID, planID); err != nil {
		t.Fatal(err)
	}

	resolved, err := f.svc.Resolve(ctx, "openai/versioned-catalog", catalog.SurfaceChat, pgtype.UUID{})
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.PriceOf(resolved.ModelPricing).InNanoPerMTok; got != 5_000_000_000 {
		t.Fatalf("the data plane should use the current price, got %d", got)
	}
	if !resolved.ModelPricing.Priced || resolved.ModelPricing.MultiplierBps != 15000 {
		t.Fatalf("the model price snapshot was not locked: %+v", resolved.ModelPricing)
	}

	models, err := f.svc.ModelsForOrg(ctx, pgtype.UUID{}, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].PlanMultiplierBps != 8000 {
		t.Fatalf("a model override should replace the plan default of 9000: %+v", models)
	}
	rec := httptest.NewRecorder()
	f.svc.WriteModelList(ctx, rec, models, catalog.Rates{})
	var body struct {
		Data []struct {
			Pricing struct {
				Input string `json:"input_per_mtok"`
			} `json:"pricing"`
			Meta struct {
				PricingPlanID string `json:"pricing_plan_id"`
			} `json:"fairlb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// $5 × 1.50 × 0.80 = $6
	if body.Data[0].Pricing.Input != "6" {
		t.Fatalf("wrong organization catalog price: %s", body.Data[0].Pricing.Input)
	}
	if body.Data[0].Meta.PricingPlanID == "" {
		t.Fatalf("the catalog returned no pricing metadata: %+v", body.Data[0].Meta)
	}
}

func TestProviderCostScalarLockedPerRequest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	providerID := f.provider(t, "p-cost-scalar", "openai")
	modelID := f.model(t, "openai/cost-scalar")
	f.route(t, modelID, providerID, "cost-scalar", []string{"chat"})
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET multiplier_bps = 10000 WHERE model_id = $1`, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE providers SET cost_multiplier_bps = 6000 WHERE id = $1`, providerID); err != nil {
		t.Fatal(err)
	}

	locked := func() (catalog.Route, catalog.PriceTable) {
		t.Helper()
		res, err := f.svc.Resolve(ctx, "openai/cost-scalar", catalog.SurfaceChat, pgtype.UUID{})
		if err != nil {
			t.Fatal(err)
		}
		priceTable, err := f.svc.LockedPriceTable(ctx, res.Model.ID, res.ModelPricing)
		if err != nil {
			t.Fatal(err)
		}
		return res.Routes[0], priceTable
	}
	quote := func(route catalog.Route, priceTable catalog.PriceTable) catalog.Quote {
		t.Helper()
		got, err := catalog.Compute(
			priceTable, priceTable,
			catalog.Tokens{In: 1_000_000, Out: 1_000_000},
			catalog.Rates{
				ModelMultiplierBps: 8500, PlanMultiplierBps: 10000,
				ProcurementMultiplierBps: route.Procurement.MultiplierBps, FXRate: "1",
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	oldRoute, oldTable := locked()
	if oldRoute.Procurement.MultiplierBps != 6000 {
		t.Fatalf("the 6000 multiplier should be locked: %+v", oldRoute.Procurement)
	}
	oldQuote := quote(oldRoute, oldTable)
	// By hand: official (3+15) = 18 USD per million over 1M tokens; cost x0.6
	// = 10.8e9 nano; customer x0.85 = 15.3e9 nano -- sold below the official
	// rate and above the purchase cost.
	if oldQuote.UpstreamUSDNano != 10_800_000_000 || oldQuote.ChargedNano != 15_300_000_000 {
		t.Fatalf("v=6000 quote=%+v", oldQuote)
	}

	if _, err := f.pool.Exec(ctx,
		`UPDATE providers SET cost_multiplier_bps = 5000 WHERE id = $1`, providerID); err != nil {
		t.Fatal(err)
	}
	newRoute, newTable := locked()
	newQuote := quote(newRoute, newTable)
	if newQuote.UpstreamUSDNano != 9_000_000_000 || newQuote.ChargedNano != oldQuote.ChargedNano {
		t.Fatalf("changing the multiplier should change only the cost: old=%+v new=%+v", oldQuote, newQuote)
	}
	// Changing the scalar mid-stream: a snapshot already held must still
	// recompute the old cost.
	if got := quote(oldRoute, oldTable); got != oldQuote {
		t.Fatalf("the locked snapshot drifted: before=%+v after=%+v", oldQuote, got)
	}
}

// Reading settings: an unconfigured exchange rate, and which way the kill
// switch fails.
func TestSettingsReads(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.svc.Settings()

	// No exchange rate is configured by default: USD is always 1, and another
	// currency returns the empty string for the caller to reject.
	if got := s.FXRate(ctx, "USD"); got != "1" {
		t.Fatalf("the USD FX rate is always 1: %q", got)
	}
	if got := s.FXRate(ctx, "CNY"); got != "" {
		t.Fatalf("an unconfigured CNY rate should come back as an empty string: %q", got)
	}
	// A kill switch that cannot be read counts as not pulled: it is an
	// availability switch, so it does not fail closed.
	if s.KillSwitch(ctx) {
		t.Fatal("the kill switch should not be pulled by default")
	}

	// The per-group markup key is retired: discounts moved onto the organization's
	// settings row and became multiplicative. This asserts it is absent from
	// the registry, because the gate that refuses unregistered keys sits on
	// the write endpoint rather than on the store -- so the registry is what
	// to inspect, not a write attempt.
	specs := settings.NewRegistry(catalog.Specs())
	if _, ok := specs.Lookup("gateway.group_markup_bps"); ok {
		t.Error("a retired group markup key is still in the registry -- leaving it there keeps it visible on the settings page")
	}
	// The global markup key is retired the same way: the only source of a
	// model's multiplier is its price row. The constant went with it, hence
	// the literal here -- the guard deliberately outlives the constant,
	// because what it protects against is exactly the key being registered
	// again, which would give pricing a second entrance.
	if _, ok := specs.Lookup("gateway.markup_bps"); ok {
		t.Error("a global markup key is present -- pricing would have a second entry point")
	}

	// It takes effect as soon as it is stored.
	if err := f.set.Set(ctx, catalog.KeyFXUSDCNY, json.RawMessage(`"7.15"`), "test"); err != nil {
		t.Fatal(err)
	}
	if got := s.FXRate(ctx, "CNY"); got != "7.15" {
		t.Fatalf("FX rate: %q", got)
	}
	if err := f.set.Set(ctx, catalog.KeyKillSwitch, json.RawMessage(`true`), "test"); err != nil {
		t.Fatal(err)
	}
	if !s.KillSwitch(ctx) {
		t.Fatal("the kill switch should be pulled")
	}
}

// An unpriced model cannot be routed to.
//
// When "nobody set a price" and "deliberately free" produce identical data and
// identical behaviour, requests route normally and charge zero, so one missed
// price is an ongoing giveaway with no signal at all. Once the intent is
// explicit, the two cases have to end differently.
func TestResolveRejectsUnpricedModel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prov := f.provider(t, "p-price", "openai")

	// The price goes into the price row: free is expressed by billing_mode,
	// and unpriced means the row does not exist. The two are different shapes
	// of data, not the same set of zeros.
	seed := func(t *testing.T, slug string, isFree bool, priceIn int64) {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO models (slug, max_output_tokens)
			 VALUES ($1, 4096) RETURNING id`, slug).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if isFree || priceIn > 0 {
			mode := "paid"
			if isFree {
				mode = "free"
			}
			if _, err := f.pool.Exec(ctx, `
				INSERT INTO model_pricing (model_id, billing_mode,
					upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
					upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
					source_name)
				VALUES ($1, $2, $3, 0, 0, 0, 'test-fixture')`, id, mode, priceIn); err != nil {
				t.Fatal(err)
			}
		}
		f.route(t, id, prov, "up", []string{"chat"})
	}

	t.Run("all four buckets zero and not marked free is refused", func(t *testing.T) {
		seed(t, "openai/forgot-price", false, 0)
		_, err := f.svc.Resolve(ctx, "openai/forgot-price", catalog.SurfaceChat, pgtype.UUID{})
		if !errors.Is(err, catalog.ErrModelUnpriced) {
			t.Fatalf("an unpriced model should be refused, got %v", err)
		}
	})

	t.Run("explicitly free is allowed", func(t *testing.T) {
		seed(t, "openai/really-free", true, 0)
		if _, err := f.svc.Resolve(ctx, "openai/really-free", catalog.SurfaceChat, pgtype.UUID{}); err != nil {
			t.Fatalf("an explicitly free model should be allowed: %v", err)
		}
	})

	t.Run("any one bucket priced is allowed", func(t *testing.T) {
		// The rule is "all four buckets zero", not "the input rate is zero":
		// embeddings have no output, an image model's cost is on the output
		// side, and a cache-oriented entry may carry only cache rates. Judging
		// on one bucket would reject those normal configurations.
		seed(t, "openai/priced", false, 3_000_000_000)
		if _, err := f.svc.Resolve(ctx, "openai/priced", catalog.SurfaceChat, pgtype.UUID{}); err != nil {
			t.Fatalf("a priced model should be allowed: %v", err)
		}
	})

	t.Run("unpriced and unavailable are two different errors", func(t *testing.T) {
		// Merging them makes the most fixable problem the hardest to locate:
		// one means "nothing serves this", the other means "enter a price".
		_, err := f.svc.Resolve(ctx, "openai/nonexistent", catalog.SurfaceChat, pgtype.UUID{})
		if errors.Is(err, catalog.ErrModelUnpriced) {
			t.Error("a nonexistent model must not report as unpriced")
		}
	})
}
