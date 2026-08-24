package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/alerttest"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/settle/settletest"
	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
	"github.com/fairlb/fairlb/settings"
)

type pipeFixture struct {
	*authFixture
	gw       *gwdb.Queries
	pipeline *proxy.Pipeline
	upstream *httptest.Server
	// lastHeaders records what the upstream actually received, for the
	// anonymity assertions.
	lastHeaders http.Header
	lastBody    []byte
	box         *crypto.Box
	cat         *catalog.Service
	providers   []providerRef
	// settler records the funds actions this layer issued. A fake rather than
	// the real billing implementation: this layer's job is to call the right
	// funds action at the right moment, while the correctness of wallets and
	// the ledger is billing's own job, covered by its coverage gate and by the
	// end-to-end conservation-of-funds tests at the assembly layer.
	settler *settletest.Fake
	// probeRequests records what the data plane asked the probe worker to
	// look at after an upstream said there was nothing at an endpoint. The
	// data plane never writes a verdict itself; this is the whole of its
	// reaction, and a test about a 404 asserts on it.
	probeRequests []probeRequest
}

type probeRequest struct {
	routeID  pgtype.UUID
	endpoint string
}

// newPipeFixture builds the full-pipeline fixture: a real PostgreSQL and a
// local test upstream.
func newPipeFixture(
	t *testing.T, upstreamHandler http.HandlerFunc,
) *pipeFixture {
	return newPipeFixtureWithPolicy(t, upstreamHandler, nil)
}

func newPipeFixtureWithPolicy(
	t *testing.T, upstreamHandler http.HandlerFunc, alerter proxy.Alerter,
) *pipeFixture {
	t.Helper()
	af := newAuthFixture(t)
	f := &pipeFixture{authFixture: af, gw: gwdb.New(af.pool)}

	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastHeaders = r.Header.Clone()
		f.lastBody, _ = readAll(r)
		upstreamHandler(w, r)
	}))
	t.Cleanup(f.upstream.Close)

	key := make([]byte, 32)
	box, err := crypto.NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	f.box = box

	set := settings.New(af.pool, nil, settings.NewRegistry(), nil)
	cat := catalog.NewService(f.gw, nil, set)
	f.cat = cat
	f.settler = &settletest.Fake{}
	// One limiter for both, which is what the assembly point injects: the
	// customer's ceilings and the upstream accounts' allowances are separate
	// counters but the same driver, so a fixture with two of them would be
	// testing a shape no deployment has.
	limiter := ratelimit.NewMemory()
	f.pipeline = proxy.NewPipeline(proxy.PipelineConfig{
		Pool: af.pool, Gateway: f.gw, Catalog: cat,
		Authenticator: proxy.NewAuthenticator(af.keyStore, af.orgStore, f.gw, nil),
		Guard:         proxy.NewGuard(af.keyStore, limiter),
		RateLimit:     limiter,
		Settlement:    f.settler,
		Cipher:        box,
		HTTPClient:    f.upstream.Client(),
		Alerter:       alerter,
		ProbeRequester: func(_ context.Context, routeID pgtype.UUID, endpoint string) {
			f.probeRequests = append(f.probeRequests, probeRequest{routeID: routeID, endpoint: endpoint})
		},
	})
	return f
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// seedCatalog creates a provider, a credential, a model and a route, all
// pointing at the local test upstream.
//
// The provider belongs to the platform whose dialect it speaks -- "openai" the
// vendor for the openai dialect -- because organization credentials are matched by
// vendor, and a fixture whose provider belonged to nobody in particular could
// never exercise that match. seedCatalogAsVendor states a different one.
//
// endpoints are recorded as verified on the route. That shapes what the
// catalog lists, not what is callable: the data plane tries any endpoint not
// found unsupported. The route id comes back so a test can write a verdict.
func (f *pipeFixture) seedCatalog(t *testing.T, protocol, modelSlug, upstreamModel string, endpoints []string) pgtype.UUID {
	t.Helper()
	return f.seedCatalogAsVendor(t, protocol, protocol, modelSlug, upstreamModel, endpoints)
}

func (f *pipeFixture) seedCatalogAsVendor(
	t *testing.T, vendor, protocol, modelSlug, upstreamModel string, endpoints []string,
) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var provID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, $4, ARRAY[$2], $3) RETURNING id`,
		"p-"+protocol, protocol, f.upstream.URL, vendor).Scan(&provID); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.box.Seal([]byte("sk-upstream-secret"), provID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	// The credential's associated data is its own row id, so the row is
	// inserted first and the ciphertext written back afterwards.
	var keyID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', $2) RETURNING id`,
		provID, sealed).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	resealed, err := f.box.Seal([]byte("sk-upstream-secret"), keyID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE provider_keys SET secret_enc = $2 WHERE id = $1`, keyID, resealed); err != nil {
		t.Fatal(err)
	}

	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ($1, 4096) RETURNING id`,
		modelSlug).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			multiplier_bps, source_name)
		-- 12000 is a 20% markup. The sales multiplier lives directly in this
		-- column rather than being composed from a global setting and a
		-- per-model override.
		VALUES ($1, 'paid', 3000000000, 15000000000, 0, 0, 12000, 'test-fixture')
		ON CONFLICT (model_id) DO NOTHING`, modelID); err != nil {
		t.Fatal(err)
	}
	return catalogtest.SeedRoute(t, f.pool, modelID, provID, upstreamModel, endpoints...)
}

// topup declares "this org has enough balance".
//
// With a fake settler the balance is no longer real state, so this function is
// a declaration of intent: it guarantees no hold failure is injected. The call
// sites are kept rather than deleted from twenty-odd tests because "this test
// assumes sufficient balance" is useful information in itself -- without it a
// reader cannot tell whether a test exercises the sufficient path or the
// insufficient one.
func (f *pipeFixture) topup(t *testing.T, _ pgtype.UUID, _ int64) {
	t.Helper()
	f.settler.HoldErr = nil
}

func (f *pipeFixture) seedProviderProcurementBaseline(t *testing.T, modelSlug string) {
	t.Helper()
	ctx := context.Background()
	var providerID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		SELECT r.provider_id FROM model_routes r
		JOIN models m ON m.id = r.model_id
		WHERE m.slug = $1 LIMIT 1`, modelSlug).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE providers SET cost_multiplier_bps = 10000 WHERE id = $1`, providerID); err != nil {
		t.Fatal(err)
	}
}

// seedBYOK gives an org a organization credential at a vendor. The name carries the
// vendor so one org can hold several, which is the case the per-vendor gate is
// about.
func (f *pipeFixture) seedBYOK(t *testing.T, org pgtype.UUID, vendor string) {
	t.Helper()
	f.seedBYOKSecret(t, org, vendor, "sk-organization-byok", false)
}

// seedBYOKSecret is seedBYOK with the credential and the fallback switch stated,
// for the tests that have to tell one organization credential from another.
func (f *pipeFixture) seedBYOKSecret(t *testing.T, org pgtype.UUID, vendor, secret string, fallback bool) {
	t.Helper()
	ctx := context.Background()
	var keyID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO org_provider_keys (org_id, vendor, name, secret_enc, allow_fallback)
		VALUES ($1, $2, 'byok-'||$2, '\x00', $3) RETURNING id`,
		org, vendor, fallback).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.box.Seal([]byte(secret), keyID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE org_provider_keys SET secret_enc=$2 WHERE id=$1`, keyID, sealed); err != nil {
		t.Fatal(err)
	}
}

// seedCatalogAt creates a provider pointing at a given URL, for the multi-
// provider failover tests.
func (f *pipeFixture) seedCatalogAt(t *testing.T, url, protocol, slug string, priority int32) {
	t.Helper()
	f.seedCatalogAtAsVendor(t, url, "custom", protocol, slug, priority)
}

// seedCatalogAtAsVendor is seedCatalogAt with the platform stated, for the tests
// where which company an endpoint belongs to is the thing under test.
func (f *pipeFixture) seedCatalogAtAsVendor(
	t *testing.T, url, vendor, protocol, slug string, priority int32,
) {
	t.Helper()
	ctx := context.Background()
	var provID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, $4, ARRAY[$2], $3) RETURNING id`,
		slug, protocol, url, vendor).Scan(&provID); err != nil {
		t.Fatal(err)
	}
	var keyID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', '\x00') RETURNING id`,
		provID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.box.Seal([]byte("sk-"+slug), keyID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE provider_keys SET secret_enc = $2 WHERE id = $1`, keyID, sealed); err != nil {
		t.Fatal(err)
	}
	f.providers = append(f.providers, providerRef{id: provID, slug: slug, priority: priority})
}

// seedModelWithRoutes creates a model and attaches it to several already
// seeded providers.
func (f *pipeFixture) seedModelWithRoutes(t *testing.T, slug, protocol string, providerSlugs []string) {
	t.Helper()
	ctx := context.Background()
	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ($1, 4096) RETURNING id`,
		slug).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			multiplier_bps, source_name)
		VALUES ($1, 'paid', 3000000000, 15000000000, 0, 0, 12000, 'test-fixture')
		ON CONFLICT (model_id) DO NOTHING`, modelID); err != nil {
		t.Fatal(err)
	}
	for _, ps := range providerSlugs {
		for _, p := range f.providers {
			if p.slug != ps {
				continue
			}
			routeID := catalogtest.SeedRoute(t, f.pool, modelID, p.id, "up-"+ps, "chat")
			if _, err := f.pool.Exec(ctx, `UPDATE model_routes SET priority = $2 WHERE id = $1`,
				routeID, p.priority); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// setProviderCapacity declares what an already-seeded provider's upstream
// account will take. A zero means "not declared", which is what NULL says.
func (f *pipeFixture) setProviderCapacity(t *testing.T, slug string, rpm, tpm int) {
	t.Helper()
	toNull := func(n int) any {
		if n <= 0 {
			return nil
		}
		return n
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE providers SET rate_limit_rpm = $2, rate_limit_tpm = $3 WHERE slug = $1`,
		slug, toNull(rpm), toNull(tpm)); err != nil {
		t.Fatal(err)
	}
}

// addProviderKey adds one more credential to an already-seeded provider. The
// plaintext differs per key so that the upstream can tell which one was used.
func (f *pipeFixture) addProviderKey(t *testing.T, providerSlug, name, secret string) {
	t.Helper()
	ctx := context.Background()
	var keyID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc)
		 SELECT id, $2, '\x00' FROM providers WHERE slug = $1 RETURNING id`,
		providerSlug, name).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	// The ciphertext's associated data is the row's own id, so it can only be
	// sealed once the row exists.
	sealed, err := f.box.Seal([]byte(secret), keyID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE provider_keys SET secret_enc = $2 WHERE id = $1`, keyID, sealed); err != nil {
		t.Fatal(err)
	}
}

type providerRef struct {
	id       pgtype.UUID
	slug     string
	priority int32
}

const openAIResponse = `{"id":"c1","object":"chat.completion","choices":[{"message":{"content":"hi"}}],
	"usage":{"prompt_tokens":1000,"completion_tokens":500}}`

// The pipeline end to end: hold, forward, extract usage, settle and record --
// with the amount taken and the usage row agreeing exactly.
func TestPipelineChatCompletion(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "gpt-5.4-upstream", []string{"chat"})

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	})
	if gerr != nil {
		t.Fatalf("the pipeline should succeed: %v", gerr)
	}
	if res.Status != 200 {
		t.Fatalf("status code: %d", res.Status)
	}

	// The upstream receives the mapped model name.
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(f.lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "gpt-5.4-upstream" {
		t.Errorf("the model name should be mapped to the upstream name: %q", sent.Model)
	}
	// The upstream gets the provider credential, not the organization's key.
	if got := f.lastHeaders.Get("Authorization"); got != "Bearer sk-upstream-secret" {
		t.Errorf("the upstream auth header should carry the provider credential: %q", got)
	}

	// Billing: 1000 in at $3/Mtok plus 500 out at $15/Mtok is 10.5e6 nano,
	// and with the 20% default markup 12.6e6.
	const wantCharged = 12_600_000
	st, ok := f.settler.LastSettle()
	if !ok {
		t.Fatal("a settlement should have been issued")
	}
	if st.ActualNano != wantCharged {
		t.Errorf("the settled amount should be %d, got %d", wantCharged, st.ActualNano)
	}
	if _, voids, _ := f.settler.Counts(); voids != 0 {
		t.Errorf("the success path must not void the hold, it voided %d times", voids)
	}

	// The usage row agrees with the accounting.
	var logged struct {
		charged   int64
		tokensIn  int32
		tokensOut int32
		estimated bool
		status    string
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT charged_nano, tokens_in, tokens_out, usage_estimated, status FROM usage_logs
		 WHERE org_id = $1`, org).
		Scan(&logged.charged, &logged.tokensIn, &logged.tokensOut, &logged.estimated, &logged.status); err != nil {
		t.Fatalf("one usage row should have been written: %v", err)
	}
	if logged.charged != wantCharged || logged.tokensIn != 1000 || logged.tokensOut != 500 {
		t.Errorf("the usage row disagrees with the accounting: %+v", logged)
	}
	if logged.estimated {
		t.Error("the upstream returned usage, so this must not be marked estimated")
	}
	if logged.status != "ok" {
		t.Errorf("status = %s", logged.status)
	}

	// The response body carries the gateway's usage extension.
	var out struct {
		Usage struct {
			Fairlb struct {
				Estimated bool   `json:"estimated"`
				Cost      string `json:"cost"`
				Currency  string `json:"currency"`
			} `json:"fairlb"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Usage.Fairlb.Cost != "0.0126" || out.Usage.Fairlb.Currency != "USD" {
		t.Errorf("the usage extension does not match: %+v", out.Usage.Fairlb)
	}
}

func TestPipelineLocksVersionedPricingAndWritesReproducibleSnapshot(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/versioned-usage", "versioned-upstream", []string{"chat"})

	var modelID, providerID, routeID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM models WHERE slug = 'openai/versioned-usage'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM providers WHERE slug = 'p-openai'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM model_routes WHERE model_id = $1`, modelID).Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET
			upstream_in_nano_per_mtok = 4000000000, upstream_out_nano_per_mtok = 10000000000,
			upstream_cache_read_nano_per_mtok = 1000000000,
			upstream_cache_write_nano_per_mtok = 2000000000, multiplier_bps = 15000
		WHERE model_id = $1`, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE providers SET cost_multiplier_bps = 8000 WHERE id = $1`, providerID); err != nil {
		t.Fatal(err)
	}
	_ = routeID

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/versioned-usage","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
		RequestID:    "versioned-pricing-snapshot",
	})
	if gerr != nil {
		t.Fatalf("the request under locked pricing should succeed: %v", gerr)
	}

	var charged, upstreamCost int64
	var snapshot []byte
	if err := f.pool.QueryRow(ctx, `
		SELECT charged_nano, upstream_cost_usd_nano, pricing_snapshot
		FROM usage_logs WHERE request_id = 'versioned-pricing-snapshot'`).Scan(
		&charged, &upstreamCost, &snapshot,
	); err != nil {
		t.Fatal(err)
	}
	// (1000x4 + 500x10) / 1M is $0.009. The customer pays x1.5 = $0.0135 and
	// procurement costs x0.8 = $0.0072.
	if charged != 13_500_000 || upstreamCost != 7_200_000 {
		t.Fatalf("the sales and procurement chains are not separated: charged=%d cost=%d", charged, upstreamCost)
	}
	var snap struct {
		SchemaVersion            int    `json:"schema_version"`
		PricingPlanID            string `json:"pricing_plan_id"`
		FXRate                   string `json:"fx_rate"`
		FXVersion                string `json:"fx_version"`
		ModelMultiplierBps       int64  `json:"model_multiplier_bps"`
		PlanMultiplierBps        int64  `json:"plan_multiplier_bps"`
		ProcurementMultiplierBps int64  `json:"procurement_multiplier_bps"`
		Official                 struct {
			Base catalog.Price `json:"base_nano_per_mtok"`
		} `json:"official_price_table"`
		Procurement   catalog.PriceTableSnapshot `json:"procurement_price_table"`
		OfficialRates struct {
			Input string `json:"input"`
		} `json:"official_rates_usd_per_m"`
		PublicRates struct {
			Input string `json:"input"`
		} `json:"public_rates_usd_per_m"`
		CustomerRates struct {
			Input string `json:"input"`
		} `json:"customer_rates_usd_per_m"`
		ProcurementRates struct {
			Input string `json:"input"`
		} `json:"procurement_rates_usd_per_m"`
	}
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatalf("the pricing snapshot is malformed: %v (%s)", err, snapshot)
	}
	if snap.SchemaVersion != 1 || snap.PricingPlanID == "" ||
		snap.FXRate != "1" || snap.FXVersion == "" ||
		snap.ModelMultiplierBps != 15000 || snap.PlanMultiplierBps != 10000 ||
		snap.ProcurementMultiplierBps != 8000 || snap.Official.Base.InNanoPerMTok != 4_000_000_000 {
		t.Fatalf("the pricing snapshot cannot be recomputed on its own: %+v", snap)
	}
	if snap.OfficialRates.Input != "4" || snap.PublicRates.Input != "6" ||
		snap.CustomerRates.Input != "6" || snap.ProcurementRates.Input != "3.2" {
		t.Fatalf("the four groups of effective USD-per-million prices are wrong: %+v", snap)
	}
}

func TestPipelineKeepsHoldAndQueuesUsageWhenAdvancedPriceIsMissing(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"c-tool","choices":[{"message":{"content":"hi"}}],
			"usage":{"prompt_tokens":1000,"completion_tokens":1},
			"tool_usage":{"web_search":{"num_requests":2}}
		}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/missing-tool-price", "missing-tool-price", []string{"chat"})

	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM models WHERE slug = 'openai/missing-tool-price'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET
			upstream_cache_read_nano_per_mtok = 1000000000,
			upstream_cache_write_nano_per_mtok = 2000000000
		WHERE model_id = $1`, modelID); err != nil {
		t.Fatal(err)
	}
	f.seedProviderProcurementBaseline(t, "openai/missing-tool-price")

	const requestID = "missing-advanced-price"
	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/missing-tool-price","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext, RequestID: requestID,
	})
	if gerr != nil || res.Status != http.StatusOK {
		t.Fatalf("a request the upstream already served must not lose its response because settlement is pending: res=%+v err=%v", res, gerr)
	}

	var payload []byte
	// The missing-price path neither settles nor voids; it asks the funds side
	// to stop the sweeper from expiring this hold. What "protect" means
	// concretely differs by implementation -- see settle.Settler.
	if !f.settler.Protected(requestID) {
		t.Fatal("a missing advanced price must protect the hold, or the sweeper releases the reserved funds before an operator can add the price")
	}
	if _, voids, settles := f.settler.Counts(); voids != 0 || settles != 0 {
		t.Fatalf("with a price missing there must be neither a settlement nor a void: voids=%d settles=%d", voids, settles)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT payload FROM gateway_pricing_unsettled WHERE request_id = $1`, requestID).
		Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var usageCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM usage_logs WHERE request_id = $1`, requestID).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 0 {
		t.Fatal("no usage row that looks settled may be inserted while the price is undetermined")
	}
	pending, err := f.gw.ListUnsettledPending(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatal("a pricing_missing entry must not reach the generic worker, which would charge the reserved estimate automatically")
	}

	params, err := gwusage.DecodeUsageReplayPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	var toolCalls map[string]int64
	if err := json.Unmarshal(params.ToolCalls, &toolCalls); err != nil || toolCalls["web_search"] != 2 {
		t.Fatalf("the unsettled payload did not preserve the advanced usage: %s", params.ToolCalls)
	}
	var snap struct {
		SettlementStatus string `json:"settlement_status"`
		PricingIssue     string `json:"pricing_issue"`
	}
	if err := json.Unmarshal(params.PricingSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.SettlementStatus != "pricing_missing" || snap.PricingIssue == "" {
		t.Fatalf("the unsettled snapshot did not record why the price was missing: %+v", snap)
	}
}

func TestPipelineEstimatesWhenUsageMissing(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"hello world"}}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "up-m", []string{"chat"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatal(gerr)
	}

	var estimated bool
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT usage_estimated, charged_nano FROM usage_logs WHERE org_id = $1`, org).
		Scan(&estimated, &charged); err != nil {
		t.Fatal(err)
	}
	if !estimated {
		t.Error("with no usage from the upstream the row must be marked estimated")
	}
	if charged <= 0 {
		t.Error("the estimation fallback must still charge")
	}
}

// A failure before the first byte releases the hold and charges nothing.
func TestPipelineVoidsHoldOnUpstreamFailure(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "up-m", []string{"chat"})

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[]}`),
		Credential:   plaintext,
	})
	if gerr == nil {
		t.Fatal("an upstream 5xx should produce an error")
	}
	if gerr.Code != errcode.GatewayAllProvidersFailed {
		t.Errorf("error code: %s", gerr.Code)
	}

	_, voids, settles := f.settler.Counts()
	if settles != 0 {
		t.Errorf("a failure before the first byte must charge nothing, but %d settlements were issued", settles)
	}
	if voids == 0 {
		t.Error("a failure before the first byte must release the hold, but no void was issued")
	}

	// The failure row is still written -- support investigations and error-rate
	// statistics depend on it -- with the billing columns at zero.
	var charged int64
	var status, code string
	if err := f.pool.QueryRow(ctx,
		`SELECT charged_nano, status, error_code FROM usage_logs WHERE org_id = $1`, org).
		Scan(&charged, &status, &code); err != nil {
		t.Fatalf("a failure should get a usage row too: %v", err)
	}
	if charged != 0 || status != "upstream_error" {
		t.Errorf("failure row: charged=%d status=%s code=%s", charged, status, code)
	}
}

// The only route has been found not to serve this endpoint, so 404, and no
// hold is taken: model resolution happens before the hold.
//
// A route declares nothing; the verdict is what excludes it. Without the
// verdict the request would go through -- that case is TestUpstream404RequestsAProbeNotAVerdict.
func TestPipelineRejectsUnsupportedEndpoint(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the upstream must not be reached")
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	route := f.seedCatalog(t, "openai", "openai/chat-only", "up", []string{"chat"})
	catalogtest.SeedVerdict(t, f.pool, route, "embeddings", "unsupported")

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceEmbeddings, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/embeddings",
		Body:         []byte(`{"model":"openai/chat-only","input":"x"}`),
		Credential:   plaintext,
	})
	if gerr == nil || gerr.Code != errcode.GatewayModelNotFound {
		t.Fatalf("no provider for this surface should give 404: %v", gerr)
	}
	if holds, _, _ := f.settler.Counts(); holds != 0 {
		t.Errorf("a resolution failure must take no hold: %d", holds)
	}
}

// An endpoint nobody has verified is still tried, and when the upstream
// answers 404 the data plane asks the probe worker to look rather than
// deciding anything itself: the route stays a candidate, the verdict table is
// untouched, and exactly one probe request names the route and the endpoint.
//
// One live 404 cannot tell "unsupported" from "wrong upstream name" or "being
// rolled"; a verdict written here would take a working route out of rotation
// on the strength of a blip. The worker, with the same request builder and
// the shared credential, is the single source of verdicts.
func TestUpstream404RequestsAProbeNotAVerdict(t *testing.T) {
	hits := 0
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no such model for embeddings"}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	route := f.seedCatalog(t, "openai", "openai/chat-only", "up", []string{"chat"})

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceEmbeddings, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/embeddings",
		Body:         []byte(`{"model":"openai/chat-only","input":"x"}`),
		Credential:   plaintext,
	})
	if gerr == nil || hits != 1 {
		t.Fatalf("an unverified endpoint is tried upstream, and its 404 reaches the caller when no other candidate exists: hits=%d err=%v", hits, gerr)
	}
	if len(f.probeRequests) != 1 || f.probeRequests[0].routeID != route || f.probeRequests[0].endpoint != "embeddings" {
		t.Fatalf("a 404 must ask for exactly one probe of (route, embeddings): %+v", f.probeRequests)
	}
	var status string
	err := f.pool.QueryRow(ctx,
		`SELECT status FROM model_route_probes WHERE route_id = $1 AND endpoint = 'embeddings'`, route).Scan(&status)
	if err == nil && status == "unsupported" {
		t.Fatal("the data plane wrote a verdict; only the probe worker may")
	}
	// And the route is still a candidate: a second request goes upstream
	// again rather than being refused at resolution. The caller sees the
	// same 404 either way -- what differs is who answered it.
	_, _ = f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceEmbeddings, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/embeddings",
		Body:         []byte(`{"model":"openai/chat-only","input":"x"}`),
		Credential:   plaintext,
	})
	if hits != 2 {
		t.Fatalf("without a verdict the route stays a candidate and the upstream is asked again: hits=%d", hits)
	}
}

// A 404 on the organization's own credential says nothing about the shared
// route, so it asks for nothing -- on the buffered path as on the streaming
// one, which carry the credential's provenance through the failure alike.
// And a route found unsupported with the shared credential is still a
// candidate for an organization bringing its own at that vendor: the shared
// key's 404 may have been "your project has no access".
func TestBYOK404NeitherRequestsAProbeNorIsExcludedByOne(t *testing.T) {
	hits := 0
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no access to this model"}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	// The provider belongs to the openai vendor, which is what the BYOK key is
	// matched on.
	route := f.seedCatalog(t, "openai", "openai/gated", "gated", []string{"chat"})
	f.seedBYOK(t, org, "openai")

	req := proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gated","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}
	if _, gerr := f.pipeline.Run(ctx, req); gerr == nil || hits != 1 {
		t.Fatalf("the upstream's 404 on the organization's key reaches the caller: hits=%d err=%v", hits, gerr)
	}
	if len(f.probeRequests) != 0 {
		t.Fatalf("a 404 on the organization's own credential must ask for no probe of the shared route: %+v", f.probeRequests)
	}

	// The shared credential's verdict does not bind this organization.
	catalogtest.SeedVerdict(t, f.pool, route, "chat", "unsupported")
	if _, _ = f.pipeline.Run(ctx, req); hits != 2 {
		t.Fatalf("a shared-key verdict must not exclude the route for an organization with its own credential at that vendor: hits=%d", hits)
	}
}

// Insufficient balance gives 402 and never reaches the upstream.
func TestPipelineInsufficientCredits(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("insufficient balance must not reach the upstream")
	})
	ctx := context.Background()
	plaintext, _, _ := f.seedKey(t, apikeys.CreateInput{})
	f.seedCatalog(t, "openai", "openai/m", "up-m", []string{"chat"})

	// The funds side refuses the hold. This used to be triggered by simply not
	// funding the org and letting the real billing refuse; with a fake settler
	// the balance is no longer real state, so it is injected explicitly. The
	// error code must be the generic one verbatim, because what is under test
	// is precisely mapHoldError translating it into the dataplane's 402.
	f.settler.HoldErr = httpx.ErrCode(errcode.CommonInsufficientCredits)

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[]}`),
		Credential:   plaintext,
	})
	if gerr == nil || gerr.Code != errcode.GatewayInsufficientCredits {
		t.Fatalf("should be 402: %v", gerr)
	}
}

// The upstream connects and answers 200 but produces not one byte.
//
// At that point our own 200 and SSE headers are *not yet committed* -- they are
// withheld until the first upstream chunk -- so RunStream returns a real error
// and the caller can still write a real error status. The hold is released in
// full and the failure leaves a trace.
func TestPipelineStreamZeroOutputReturnsRealError(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200) // the upstream really did connect, and then sent nothing
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up-model", []string{"chat"})

	rec := httptest.NewRecorder()
	gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI)
	if gerr == nil || gerr.Code != errcode.GatewayUpstreamTimeout {
		t.Fatalf("zero output should return a real error (GatewayUpstreamTimeout), got: %v", gerr)
	}
	// Our own 200 must not have been committed: the body is empty and the SSE
	// headers are unset, so an error status can still be written. (The
	// recorder's Code defaults to 200 when no header was written, so it cannot
	// serve as the criterion -- the assertion is anchored to what is
	// observable.)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatalf("with zero output the 200 and SSE headers must not be committed: body=%q ct=%q",
			rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	// The hold is released in full and nothing is charged.
	if _, voids, settles := f.settler.Counts(); settles != 0 || voids == 0 {
		t.Fatalf("zero output must not be charged and must release the hold: settles=%d voids=%d", settles, voids)
	}
	// The failure leaves a trace, so an investigation can find this request.
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM usage_logs WHERE org_id = $1`, org).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "ok" {
		t.Fatalf("the trace left by zero output must not be ok: %s", status)
	}
}

// Streaming end to end: SSE passed through, settled from the final chunk's
// usage, with include_usage force-injected.
func TestPipelineStreamEndToEnd(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, frame := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(frame))
			if fl != nil {
				fl.Flush()
			}
		}
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up-model", []string{"chat"})

	rec := httptest.NewRecorder()
	gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI)
	if gerr != nil {
		t.Fatalf("streaming should succeed: %v", gerr)
	}

	// The SSE bytes reach the client unchanged.
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("the SSE did not pass through: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	// The request the upstream receives must carry include_usage, or the final
	// chunk has no usage and settlement has nothing actual to charge against.
	var sent struct {
		Stream        bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(f.lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.Stream || sent.StreamOptions == nil || !sent.StreamOptions.IncludeUsage {
		t.Fatalf("streaming must force-inject include_usage: %s", f.lastBody)
	}

	// Billed the same way as non-streaming: 1000 in and 500 out with the 20%
	// markup is 12.6e6.
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != 12_600_000 {
		t.Errorf("streaming should bill the same way as non-streaming: settlement %+v (want 12600000)", st)
	}

	var stream bool
	var status string
	var tokensOut int32
	if err := f.pool.QueryRow(ctx,
		`SELECT stream, status, tokens_out FROM usage_logs WHERE org_id = $1`, org).
		Scan(&stream, &status, &tokensOut); err != nil {
		t.Fatal(err)
	}
	if !stream || status != "ok" || tokensOut != 500 {
		t.Errorf("streaming usage row: stream=%v status=%s out=%d", stream, status, tokensOut)
	}
}

func TestPipelineStreamKeepsRequestStartPriceAcrossPublication(t *testing.T) {
	firstByte := make(chan struct{})
	finish := make(chan struct{})
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"locked\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		close(firstByte)
		<-finish
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":0}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/stream-price-lock", "stream-price-lock", []string{"chat"})

	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM models WHERE slug = 'openai/stream-price-lock'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET
			upstream_in_nano_per_mtok = 1000000000, upstream_out_nano_per_mtok = 2000000000,
			upstream_cache_read_nano_per_mtok = 100000000,
			upstream_cache_write_nano_per_mtok = 200000000, multiplier_bps = 10000
		WHERE model_id = $1`, modelID); err != nil {
		t.Fatal(err)
	}
	f.seedProviderProcurementBaseline(t, "openai/stream-price-lock")

	rec := httptest.NewRecorder()
	done := make(chan *proxy.Error, 1)
	go func() {
		done <- f.pipeline.RunStream(ctx, rec, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/stream-price-lock","stream":true,"messages":[]}`),
			Credential:   plaintext, RequestID: "stream-price-version-lock",
		}, proxy.SurfaceOpenAI)
	}()
	<-firstByte
	// Raise the price a hundredfold mid-stream. *This request must still settle
	// at the price read when it began* -- the lock is the pipeline's pricing
	// snapshot transaction, where reading once inside the transaction is itself
	// the lock, not a version id.
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET
			upstream_in_nano_per_mtok = 100000000000, upstream_out_nano_per_mtok = 200000000000,
			upstream_cache_read_nano_per_mtok = 10000000000,
			upstream_cache_write_nano_per_mtok = 20000000000
		WHERE model_id = $1`, modelID); err != nil {
		t.Fatalf("changing the price mid-stream failed: %v", err)
	}
	close(finish)
	if gerr := <-done; gerr != nil {
		t.Fatal(gerr)
	}

	// The criterion is the *amount* rather than a version id: with no versions,
	// the only way to falsify "settled at the starting price" is this number --
	// drifting to the new price would make it a hundred times larger.
	var charged int64
	if err := f.pool.QueryRow(ctx, `
		SELECT charged_nano FROM usage_logs
		WHERE request_id = 'stream-price-version-lock'`).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if charged != 1_000_000 {
		t.Fatalf("a streaming request must not drift to the new price mid-flight: charged=%d (drifting makes it a hundred times larger)", charged)
	}
}

// The upstream stream sent no usage, so it is estimated from what was
// forwarded and the row is marked estimated.
func TestPipelineStreamEstimatesWhenUsageMissing(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"some text here\"}}]}\n\ndata: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "up-m", []string{"chat"})

	rec := httptest.NewRecorder()
	if gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","stream":true,"messages":[]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI); gerr != nil {
		t.Fatal(gerr)
	}

	var estimated bool
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT usage_estimated, charged_nano FROM usage_logs WHERE org_id = $1`, org).
		Scan(&estimated, &charged); err != nil {
		t.Fatal(err)
	}
	if !estimated {
		t.Error("the upstream stream sent no usage, so the row must be marked estimated")
	}
	if charged <= 0 {
		t.Error("the estimation fallback must still charge")
	}
}

// The upstream returns an error status before the stream begins: the same as a
// failure before the first byte, so the hold is voided and nothing charged.
func TestPipelineStreamUpstreamErrorBeforeFirstByte(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "up-m", []string{"chat"})

	rec := httptest.NewRecorder()
	gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","stream":true,"messages":[]}`),
		Credential:   plaintext,
	}, proxy.SurfaceOpenAI)
	if gerr == nil {
		t.Fatal("an upstream 5xx should error before the first byte")
	}

	_, voids, settles := f.settler.Counts()
	if settles != 0 {
		t.Errorf("a failure before the first byte must charge nothing, but %d settlements were issued", settles)
	}
	if voids == 0 {
		t.Error("a failure before the first byte must release the hold, but no void was issued")
	}
}

// Automatic failover away from a broken provider: the first answers 5xx and
// the request should land on the second and succeed.
func TestPipelineFailsOverToHealthyProvider(t *testing.T) {
	var badHits, goodHits int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		goodHits++
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	// The broken provider has the higher priority and is tried first; the
	// healthy one is the fallback.
	f.seedCatalogAt(t, bad.URL, "openai", "bad", 10)
	f.seedCatalogAt(t, f.upstream.URL, "openai", "good", 20)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"bad", "good"})

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[]}`),
		Credential:   plaintext,
	})
	if gerr != nil {
		t.Fatalf("it should fail over to the healthy provider: %v", gerr)
	}
	if res.Status != 200 {
		t.Fatalf("status code: %d", res.Status)
	}
	if badHits == 0 || goodHits == 0 {
		t.Fatalf("it should try the broken provider first and then the healthy one: bad=%d good=%d", badHits, goodHits)
	}

	// After failing over it bills as usual, and the usage row records how many
	// routes were tried.
	var attempts int32
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT route_attempts, charged_nano FROM usage_logs WHERE org_id = $1`, org).
		Scan(&attempts, &charged); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("two route attempts should have been recorded: %d", attempts)
	}
	if charged != 12_600_000 {
		t.Errorf("billing is unchanged after failing over: %d", charged)
	}
}

// A client-class failure does not rotate: a bad parameter is bad for every
// candidate, and retrying only wastes upstream quota.
func TestPipelineClientErrorDoesNotFailover(t *testing.T) {
	var secondHits int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits++
		_, _ = w.Write([]byte(openAIResponse))
	}))
	t.Cleanup(second.Close)

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad temperature"}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalogAt(t, f.upstream.URL, "openai", "first", 10)
	f.seedCatalogAt(t, second.URL, "openai", "second", 20)
	f.seedModelWithRoutes(t, "openai/m", "openai", []string{"first", "second"})

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[]}`),
		Credential:   plaintext,
	})
	if gerr == nil || gerr.Code != errcode.GatewayInvalidRequest {
		t.Fatalf("a 400 should pass through rather than be retried: %v", gerr)
	}
	if secondHits != 0 {
		t.Errorf("a client-class failure must not rotate: the second provider was hit %d times", secondHits)
	}
	// The upstream's own text must pass through.
	if gerr.UpstreamMessage == "" {
		t.Error("a 400 must carry the upstream's own text")
	}
}

// A multipart image edit bills correctly, and the multipart body reaches the
// upstream intact, with the boundary and the binary content unchanged.
func TestPipelineImageEditBillsCorrectly(t *testing.T) {
	// The fixture reads the body before the handler runs, so what arrived is
	// inspected through the fixture's recorded body and headers.
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"abc"}],
			"usage":{"input_tokens":100,"output_tokens":1000}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-image-2", "gpt-image-2", []string{"images"})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "openai/gpt-image-2")
	_ = mw.WriteField("prompt", "make it blue")
	part, _ := mw.CreateFormFile("image", "a.png")
	_, _ = part.Write(bytes.Repeat([]byte{0xFF}, 8<<10))
	_ = mw.Close()
	original := buf.Bytes()

	res, gerr := f.pipeline.RunImageEdit(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/edits",
		Credential:   plaintext,
	}, mw.FormDataContentType(), bytes.NewReader(original))
	if gerr != nil {
		t.Fatalf("the image edit should succeed: %v", gerr)
	}
	if res.Status != 200 {
		t.Fatalf("status code: %d", res.Status)
	}

	// The multipart body must arrive unchanged: same boundary, identical
	// bytes.
	if ct := f.lastHeaders.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("the upstream should receive multipart: %q", ct)
	}
	if !bytes.Equal(f.lastBody, original) {
		t.Errorf("the body the upstream received must be byte-identical to the original: %d vs %d bytes", len(f.lastBody), len(original))
	}

	// Billing: 100 in at $3/Mtok plus 1000 out at $15/Mtok is 0.3e6 + 15e6 =
	// 15.3e6, and with the 20% default markup 18.36e6.
	const wantCharged = 18_360_000
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != wantCharged {
		t.Errorf("the image charge should be %d: settlement %+v", wantCharged, st)
	}
	if _, voids, _ := f.settler.Counts(); voids != 0 {
		t.Errorf("the success path must not void the hold: %d", voids)
	}

	var surface string
	var tokensOut int32
	if err := f.pool.QueryRow(ctx,
		`SELECT surface, tokens_out FROM usage_logs WHERE org_id = $1`, org).
		Scan(&surface, &tokensOut); err != nil {
		t.Fatal(err)
	}
	if surface != "images" || tokensOut != 1000 {
		t.Errorf("image usage row: surface=%s out=%d", surface, tokensOut)
	}
}

// Billing embeddings on the positive path. Only the negative branch -- 404 when
// no provider serves the surface -- used to be covered, so nobody had proved the
// money came out right on the positive one.
//
// An embeddings response reports only prompt and total tokens, with no
// completion tokens: the output side is always zero and the whole cost is on the
// input side.
func TestPipelineEmbeddingsBilling(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"up-embed",
		  "data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
		  "usage":{"prompt_tokens":8000,"total_tokens":8000}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/embed-4", "up-embed", []string{"embeddings"})

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceEmbeddings, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/embeddings",
		Body:         []byte(`{"model":"openai/embed-4","input":"hello world"}`),
		Credential:   plaintext,
	})
	if gerr != nil {
		t.Fatalf("embeddings should succeed: %v", gerr)
	}
	if res.Status != 200 {
		t.Fatalf("status code: %d", res.Status)
	}

	// The fixture's pricing: $3/Mtok input, $15/Mtok output, 20% default
	// markup.
	// 8000 in at 3000 nano/token is 24e6, output is 0, times 1.2 is 28.8e6.
	const wantCharged = 28_800_000
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != wantCharged {
		t.Errorf("the amount taken should be %d: settlement %+v", wantCharged, st)
	}
	if _, voids, _ := f.settler.Counts(); voids != 0 {
		t.Errorf("the hold should have been settled, not voided: voids=%d", voids)
	}

	// The usage row and the accounting must agree: this is reconciliation at
	// the granularity of a single request.
	var loggedIn, loggedOut int32
	var logged int64
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tokens_in, tokens_out, charged_nano, usage_estimated
		   FROM usage_logs WHERE org_id = $1`, org).
		Scan(&loggedIn, &loggedOut, &logged, &estimated); err != nil {
		t.Fatal(err)
	}
	if loggedIn != 8000 || loggedOut != 0 {
		t.Errorf("usage row token counts: in=%d out=%d, want 8000/0", loggedIn, loggedOut)
	}
	if logged != wantCharged {
		t.Errorf("the usage row amount %d differs from what was actually charged %d", logged, wantCharged)
	}
	if estimated {
		t.Error("the upstream supplied usage, so this must not be marked estimated")
	}
}

// The upstream response body must be bounded: a broken or compromised upstream
// can exhaust memory with a huge response, and the per-provider concurrency cap
// is 64. Exceeding the bound must *error rather than silently truncate* --
// truncated JSON yields no usage and would quietly degrade into estimated
// billing instead of failing where it can be seen.
func TestPipelineRejectsOversizedUpstreamBody(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), 65<<20) // just past the 64 MiB bound
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(huge)
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up", []string{"chat"})

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	})
	if gerr == nil {
		t.Fatal("an oversized response should be refused rather than buffered")
	}
	// A failure before the first byte is not charged.
	if _, _, settles := f.settler.Counts(); settles != 0 {
		t.Errorf("the size failure happens before the first byte and must not be charged: %d settlements were issued", settles)
	}
}

// An unpriced model: refuse to serve, alert, and take no hold at all.
//
// The alert is half of this: a missing price is an operator problem, and an
// operator who does not go looking at the logs will never find out. But a model
// missing prices keeps being requested, and alerting on every one of those
// drowns the channel, so the alerts are suppressed.
func TestPipelineRejectsUnpricedModel(t *testing.T) {
	rec := &alerttest.Recorder{}
	f := newPipeFixtureWithPolicy(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unpriced model must not produce an upstream request")
	}, rec)
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	// Create a model with all four buckets at zero that is not marked free.
	var modelID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens) VALUES
		 ('openai/unpriced', 4096) RETURNING id`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	var provID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ('p-unpriced','custom',ARRAY['openai'],$1)
		 RETURNING id`, f.upstream.URL).Scan(&provID); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, f.pool, modelID, provID, "up", "chat")

	req := proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/unpriced","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}
	_, gerr := f.pipeline.Run(ctx, req)
	if gerr == nil || gerr.Code != errcode.GatewayModelUnpriced {
		t.Fatalf("an unpriced model should be refused: %v", gerr)
	}

	// A separate code from model_not_found: this one tells the operator to go
	// and add a price, the other needs nothing done.
	if holds, _, _ := f.settler.Counts(); holds != 0 {
		t.Errorf("an unpriced model must take no hold, got %d", holds)
	}
	if rec.Count() != 1 {
		t.Fatalf("it should alert once, alerted %d times", rec.Count())
	}

	// Repeat requests inside the suppression window do not alert again.
	for range 5 {
		_, _ = f.pipeline.Run(ctx, req)
	}
	if rec.Count() != 1 {
		t.Errorf("inside the suppression window it should still be 1 alert, got %d -- alerting on every request drowns the channel", rec.Count())
	}
}

// End to end: the discount really reaches the invoice, and all three rate
// factors land in the usage row.
//
// The expected value is computed *by hand* in the test and reuses none of the
// production billing path. When three readings consume the same charged amount
// they are necessarily equal, and that equality is not evidence of
// correctness.
func TestPipelineAppliesOrgDiscount(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "gpt-5.4-upstream", []string{"chat"})

	// This org gets 20% off. The carrier is a pricing plan, never a bare
	// multiplier on the organization: a negotiated price is expressed as a named plan
	// rather than a number typed into the organization's page.
	var planID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO pricing_plans (slug, name, default_multiplier_bps)
		VALUES ('vip-8', 'VIP 20% off', 8000) RETURNING id`).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO org_pricing_plan_assignments (org_id, pricing_plan_id, reason)
		VALUES ($1, $2, 'test')`, org, planID); err != nil {
		t.Fatal(err)
	}

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("the pipeline should succeed: %v", gerr)
	}

	// By hand: 1000 in at $3/Mtok plus 500 out at $15/Mtok is 10_500_000 nano
	// of upstream cost; x1.20 for the default markup is 12_600_000; x0.80 for
	// the pricing plan is 10_080_000.
	const wantCharged = 10_080_000
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != wantCharged {
		t.Errorf("after the discount it should charge %d, settlement was %+v", wantCharged, st)
	}

	// The row must be recomputable from itself alone: upstream cost times the
	// model multiplier times the plan multiplier, with the factors in the
	// pricing snapshot.
	var charged, upstream int64
	var fx string
	var rawSnapshot []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT charged_nano, upstream_cost_usd_nano, fx_rate::text, pricing_snapshot
		 FROM usage_logs WHERE org_id = $1`, org).
		Scan(&charged, &upstream, &fx, &rawSnapshot); err != nil {
		t.Fatal(err)
	}
	var snap struct {
		ModelMultiplierBps int64 `json:"model_multiplier_bps"`
		PlanMultiplierBps  int64 `json:"plan_multiplier_bps"`
	}
	if err := json.Unmarshal(rawSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ModelMultiplierBps != 12000 || snap.PlanMultiplierBps != 8000 {
		t.Errorf("the rates were not snapshotted: model=%d plan=%d (missing either factor makes this row impossible to recompute alone)",
			snap.ModelMultiplierBps, snap.PlanMultiplierBps)
	}
	if charged != wantCharged {
		t.Errorf("usage row charged_nano = %d, want %d", charged, wantCharged)
	}
	// Recompute the charge from the upstream cost using the snapshot's factors.
	// What this proves is that those columns are *sufficient* to recompute --
	// the gap being that without the factors on the row, reconciliation
	// degrades into self-confirmation. One division at the end rather than
	// dividing step by step, which would compound the truncation error twice.
	//
	// Note that it does *not* prove the rounding rule: production stays in
	// exact rationals throughout and rounds up exactly once at the end, while
	// this line truncates in integers. These particular numbers divide evenly
	// at every step so the two agree; a set that did not would differ by one.
	// The rounding is anchored by the hand-computed constant above, so changing
	// the rounding rule means changing that constant, not this line.
	recomputed := upstream * snap.ModelMultiplierBps * snap.PlanMultiplierBps / (10000 * 10000)
	if recomputed != charged {
		t.Errorf("recomputing from the snapshot gives %d, which differs from charged_nano %d", recomputed, charged)
	}
}

// With both a discount and a per-model markup override in play, both take
// effect. A per-model override used to swallow the discount entirely.
func TestPipelineDiscountSurvivesModelMarkupOverride(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "gpt-5.4-upstream", []string{"chat"})

	// The operator sets this one model's margin to +40%. The carrier is the
	// model's own multiplier column, not a global markup composed with a
	// per-model override.
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing SET multiplier_bps = 14000
		WHERE model_id = (SELECT id FROM models WHERE slug = 'openai/gpt-5.4')`); err != nil {
		t.Fatal(err)
	}
	// The customer discount goes through a pricing plan; the organization page
	// offers no bare multiplier.
	var planID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO pricing_plans (slug, name, default_multiplier_bps)
		VALUES ('vip-8b', 'VIP 20% off', 8000) RETURNING id`).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO org_pricing_plan_assignments (org_id, pricing_plan_id, reason)
		VALUES ($1, $2, 'test')`, org, planID); err != nil {
		t.Fatal(err)
	}

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("the pipeline should succeed: %v", gerr)
	}

	// By hand: 10_500_000 x 1.40 x 0.80 = 11_760_000. The old behaviour, where
	// the model multiplier overrode the plan, gave 14_700_000 -- the discount
	// vanished entirely.
	const wantCharged = 11_760_000
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != wantCharged {
		t.Errorf("a +40%% model override with a 20%% discount should charge %d, settlement was %+v"+
			" (14700000 would mean the model override swallowed the discount again)", wantCharged, st)
	}
}

// A request on a organization-supplied credential settles as a *service fee*, not at
// the token list price.
//
// *The fee's base is the model's list price, not our purchase price*; byok.go
// records the reason -- we do not know what the organization pays their upstream,
// they have their own discount, and using the route cost as the base would make
// the same call cost different amounts depending on which provider it routed
// to. So this request's upstream cost is 0: *we bought nothing from an
// upstream*. That is asserted here too, because recording a cost would skew the
// margin reporting.
func TestPipelineBYOKChargesServiceFeeNotListPrice(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/byok-model", "byok-upstream", []string{"chat"})
	f.seedBYOK(t, org, "openai")

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/byok-model","messages":[]}`),
		Credential:   plaintext, RequestID: "byok-fee",
	}); gerr != nil {
		t.Fatal(gerr)
	}

	var charged, cost, feeBps int64
	var byok bool
	if err := f.pool.QueryRow(ctx, `
		SELECT charged_nano, upstream_cost_usd_nano, byok,
		       COALESCE((pricing_snapshot->>'byok_fee_bps')::bigint, -1)
		  FROM usage_logs WHERE request_id = 'byok-fee'`).
		Scan(&charged, &cost, &byok, &feeBps); err != nil {
		t.Fatal(err)
	}
	// The positive control: this hop really did use the organization's credential.
	// Without it, the assertions below could hold by coincidence on an
	// implementation that never used one at all.
	if !byok {
		t.Fatal("this hop should be recorded as using a organization credential")
	}
	if feeBps != 500 {
		t.Errorf("the snapshot should carry the service-fee rate in effect (500 bps by default): %d", feeBps)
	}
	// An independent expected value rather than a comparison against "the list
	// price": comparing two readings from one source hides an error that moved
	// both. 1000 in at $3/Mtok is 3e6 nano, 500 out at $15/Mtok is 7.5e6 nano,
	// 10.5e6 together. The sales multiplier does *not* apply -- the service fee
	// is itself the rate -- so 5% is 525_000.
	if charged != 525_000 {
		t.Errorf("a organization credential should be charged only the 5%% service fee on the list price: charged=%d want=525000", charged)
	}
	// On a organization credential the upstream is billed to the organization, so there is
	// no purchase price to record.
	if cost != 0 {
		t.Errorf("a organization credential must record no upstream cost for this deployment: %d", cost)
	}
}

// The usage row records which reservation funded it.
//
// That cross-reference is written down as a decision -- a usage row and the
// accounting entries behind it are supposed to be reachable from each other --
// and it had never worked: the column existed, the decision existed, and
// nothing ever put a value there, because the settlement seam returned no id
// for the gateway to write.
//
// The fixture stands in for the commercial accounting implementation, which is
// the only one that has reservations at all. The other configuration is
// asserted separately; a green here says nothing about it.
func TestUsageRowNamesTheHoldThatFundedIt(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	held := pgtype.UUID{Bytes: [16]byte{0xab, 0xcd, 0xef}, Valid: true}
	f.settler.HoldID = held

	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "m-up", []string{"chat"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[]}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("the request should succeed: %v", gerr)
	}

	var got pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT hold_id FROM usage_logs WHERE org_id = $1`, org).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid {
		t.Fatal("hold_id is NULL: the row cannot be traced back to the reservation that funded it")
	}
	if got.Bytes != held.Bytes {
		t.Errorf("hold_id should be the id the reservation returned: %x want %x", got.Bytes, held.Bytes)
	}
}

// The other configuration: no reservations exist, so the column stays NULL --
// and that has to be a clean NULL rather than an error, because a deployment
// without an accounting subsystem is a supported configuration, not a
// degraded one.
func TestUsageRowLeavesHoldNullWhereThereAreNoReservations(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	// The zero id is what the community settler returns.
	f.settler.HoldID = pgtype.UUID{}

	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/m", "m-up", []string{"chat"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"openai/m","messages":[]}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("a deployment without reservations must still serve: %v", gerr)
	}

	var got pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT hold_id FROM usage_logs WHERE org_id = $1`, org).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Errorf("hold_id should be NULL where nothing was reserved: %x", got.Bytes)
	}
}

// A organization credential is offered only to the platform it belongs to.
//
// This is the defect the vendor column exists to close. Credentials used to be
// keyed by protocol dialect, and dozens of companies speak the OpenAI dialect,
// so a organization's OpenAI key was handed to whichever of them routing happened to
// reach -- sent, in clear, to another company's endpoint, on a request the
// organization was then billed a service fee for.
//
// Both directions are asserted from what the upstreams actually received,
// because the two failure modes look identical in the usage row: a request that
// used no organization credential and a request that sent one to the wrong place both
// record byok=false if the recording is what broke.
func TestBYOKAppliesOnlyToTheVendorItBelongsTo(t *testing.T) {
	var openaiAuth, otherAuth string
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	}))
	defer openaiUp.Close()
	otherUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	}))
	defer otherUp.Close()

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	// Two candidates for one model, both speaking the OpenAI dialect and
	// belonging to different companies. This is the ordinary shape of a
	// deployment that resells a model available from several platforms.
	f.seedCatalogAtAsVendor(t, otherUp.URL, "deepseek", "openai", "p-deepseek", 1)
	f.seedCatalogAtAsVendor(t, openaiUp.URL, "openai", "openai", "p-openai-vendor", 2)
	f.seedModelWithRoutes(t, "shared/model", "openai", []string{"p-deepseek", "p-openai-vendor"})
	f.seedBYOK(t, org, "openai")

	run := func(requestID string) {
		t.Helper()
		if _, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"shared/model","messages":[]}`),
			Credential:   plaintext, RequestID: requestID,
		}); gerr != nil {
			t.Fatal(gerr)
		}
	}

	// Priority 1 is the other company. The organization has no account there, so the
	// deployment's own credential is used and the request is billed in full.
	run("byok-vendor-other")
	if otherAuth == "" {
		t.Fatal("the lower-priority candidate should have been called")
	}
	if strings.Contains(otherAuth, "sk-organization-byok") {
		t.Fatalf("the organization's OpenAI credential was sent to another company's endpoint: %q", otherAuth)
	}
	if got := usageBYOK(t, f, "byok-vendor-other"); got {
		t.Error("a request served by a platform the organization has no account with is not a organization-credential request")
	}

	// Take that candidate out of rotation so the same model routes to the
	// platform the credential does belong to.
	if _, err := f.pool.Exec(ctx, `UPDATE providers SET enabled = false WHERE slug = 'p-deepseek'`); err != nil {
		t.Fatal(err)
	}
	f.cat.InvalidateAll(ctx)

	run("byok-vendor-own")
	if !strings.Contains(openaiAuth, "sk-organization-byok") {
		t.Fatalf("at its own platform the organization's credential should be used, got %q", openaiAuth)
	}
	if got := usageBYOK(t, f, "byok-vendor-own"); !got {
		t.Error("the request served on the organization's own credential should be recorded as one")
	}
}

func usageBYOK(t *testing.T, f *pipeFixture, requestID string) bool {
	t.Helper()
	var byok bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT byok FROM usage_logs WHERE request_id = $1`, requestID).Scan(&byok); err != nil {
		t.Fatal(err)
	}
	return byok
}

// A rejected credential is dropped for its own platform only.
//
// Falling back is a per-credential decision the organization makes, and the thing
// being fallen back from is one account at one platform. Clearing the whole
// request's organization credentials on a single rejection would bill every later
// candidate at full price over an account that was never involved -- and
// silently, because the usage row would simply say byok=false.
func TestBYOKFallbackDropsOnlyTheRejectedVendor(t *testing.T) {
	var secondAuth string
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer rejecting.Close()
	accepting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	}))
	defer accepting.Close()

	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)

	f.seedCatalogAtAsVendor(t, rejecting.URL, "openai", "openai", "p-first", 1)
	f.seedCatalogAtAsVendor(t, accepting.URL, "deepseek", "openai", "p-second", 2)
	f.seedModelWithRoutes(t, "two-vendors/model", "openai", []string{"p-first", "p-second"})
	// Two accounts at two platforms. The first allows falling back; the second
	// is a separate account that this request must keep using.
	f.seedBYOKSecret(t, org, "openai", "sk-openai-organization", true)
	f.seedBYOKSecret(t, org, "deepseek", "sk-deepseek-organization", false)

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"two-vendors/model","messages":[]}`),
		Credential:   plaintext, RequestID: "byok-fallback-scope",
	}); gerr != nil {
		t.Fatal(gerr)
	}

	if !strings.Contains(secondAuth, "sk-deepseek-organization") {
		t.Fatalf("the second platform's own organization credential should still have been used, got %q", secondAuth)
	}
	if !usageBYOK(t, f, "byok-fallback-scope") {
		t.Error("the hop that served used a organization credential, so the usage row must say so")
	}
	// The rejected one is marked invalid; the other is untouched.
	var openaiStatus, deepseekStatus string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM org_provider_keys WHERE org_id=$1 AND vendor='openai'`, org).
		Scan(&openaiStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM org_provider_keys WHERE org_id=$1 AND vendor='deepseek'`, org).
		Scan(&deepseekStatus); err != nil {
		t.Fatal(err)
	}
	if openaiStatus != "invalid" {
		t.Errorf("the rejected credential should be marked invalid, got %q", openaiStatus)
	}
	if deepseekStatus != "active" {
		t.Errorf("another platform's credential must not be touched by this rejection, got %q", deepseekStatus)
	}
}
