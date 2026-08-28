package gwstaffapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// These tests exercise the path an operator actually walks to configure the
// gateway: provider, credential, model, route. Without them, the only way the
// catalog gets configured is a test fixture writing SQL directly, which proves
// nothing about whether a deployed instance can be made to serve a request.

func TestProviderKeyLifecycle(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	prov := mustProvider(t, s, "openai", "https://api.example.com")

	const plaintext = "sk-test-abcdef123456"
	key := mustCreateKey(t, s, prov, "primary", plaintext)

	if key.SecretHint == "" {
		t.Error("no mask was produced: the operator page cannot tell several keys apart")
	}
	if strings.Contains(key.SecretHint, "abcdef123456") {
		t.Errorf("the mask leaks the key body: %q", key.SecretHint)
	}

	// The list endpoint must return neither the plaintext nor the ciphertext:
	// a credential goes in and never comes back out.
	listed, err := s.ListGatewayProviderKeys(ctx,
		gwstaffapi.ListGatewayProviderKeysRequestObject{ProviderId: prov})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(listed)
	for _, leak := range []string{plaintext, "secret_enc", "SecretEnc"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the key listing leaks %q: %s", leak, raw)
		}
	}

	del, err := s.DeleteGatewayProviderKey(ctx, gwstaffapi.DeleteGatewayProviderKeyRequestObject{
		ProviderId: prov, KeyId: key.Id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := del.(gwstaffapi.DeleteGatewayProviderKey204Response); !ok {
		t.Fatalf("delete should return 204: %T", del)
	}
}

// A cross-provider delete has to fail: path parameters can be tampered with.
func TestDeleteKeyRejectsWrongProvider(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	a := mustProvider(t, s, "openai", "https://a.example.com")
	b := mustProvider(t, s, "anthropic", "https://b.example.com")

	key := mustCreateKey(t, s, a, "k", "sk-a-0001")

	if _, err := s.DeleteGatewayProviderKey(ctx, gwstaffapi.DeleteGatewayProviderKeyRequestObject{
		ProviderId: b, KeyId: key.Id,
	}); err == nil {
		t.Fatal("a cross-provider delete should fail")
	}

	listed, _ := s.ListGatewayProviderKeys(ctx,
		gwstaffapi.ListGatewayProviderKeysRequestObject{ProviderId: a})
	got := listed.(gwstaffapi.ListGatewayProviderKeys200JSONResponse)
	if len(got.Items) != 1 {
		t.Error("a key was deleted across providers")
	}
}

// Once provider, credential, model and route are configured, a model's
// advertised capabilities are the union of what has been verified on its
// enabled routes. Nothing is declared: a route freshly created advertises
// nothing until a verdict says otherwise.
func TestCatalogConfigurableEndToEnd(t *testing.T) {
	s, _, _ := newServer(t)

	prov := mustProvider(t, s, "openai", "https://api.example.com")
	mustCreateKey(t, s, prov, "k", "sk-upstream-key")
	model := mustModel(t, s, "openai/gpt-test")

	// A route with nothing verified yet advertises nothing.
	mustRoute(t, s, model.Id, prov, "gpt-4o", nil)
	if got := endpointsOfModel(t, s, model.Id); len(got) != 0 {
		t.Fatalf("exposed capabilities = %v, nothing has been verified so nothing should show", got)
	}

	// chat verified on it.
	routes, _ := s.ListGatewayRoutes(context.Background(), gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model.Id})
	first := routes.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items[0]
	mustVerdict(t, s, model.Id, first.Id, "chat", "ok")
	if got := endpointsOfModel(t, s, model.Id); !equalSet(got, []string{"chat"}) {
		t.Fatalf("exposed capabilities = %v, only chat is verified so only chat should show", got)
	}

	// A second route with images verified widens the union to chat+images.
	mustRoute(t, s, model.Id, prov, "gpt-image-2", []string{"images"})
	if got := endpointsOfModel(t, s, model.Id); !equalSet(got, []string{"chat", "images"}) {
		t.Errorf("exposed capabilities = %v, the union of the two routes should be chat+images", got)
	}
}

// The enable gate: a model with no price and no route must not be turned on.
func TestModelEnableRequiresChecklist(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "openai/versioned-only")
	enabled := true
	if _, err := s.UpdateGatewayModel(ctx, gwstaffapi.UpdateGatewayModelRequestObject{
		ModelId: model.Id, Body: &gwstaffapi.GatewayModelInput{Enabled: &enabled},
	}); err == nil {
		t.Fatal("a model with neither pricing nor a route must not be enabled")
	}

	provider := mustProvider(t, s, "openai", "https://ready.example.com")
	mustRoute(t, s, model.Id, provider, "ready-upstream", []string{"chat"})
	if _, err := s.UpdateGatewayModel(ctx, gwstaffapi.UpdateGatewayModelRequestObject{
		ModelId: model.Id, Body: &gwstaffapi.GatewayModelInput{Enabled: &enabled},
	}); err == nil {
		t.Fatal("a route without published pricing must still not be enabled")
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO model_pricing (
  model_id, billing_mode,
  upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
  upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
  multiplier_bps, source_name, verified_at
) VALUES ($1, 'paid', 1000000000, 2000000000, 0, 0, 10000, 'test fixture', now())`, model.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateGatewayModel(ctx, gwstaffapi.UpdateGatewayModelRequestObject{
		ModelId: model.Id, Body: &gwstaffapi.GatewayModelInput{Enabled: &enabled},
	}); err != nil {
		t.Fatalf("with pricing and a route in place it should be enablable: %v", err)
	}
}

// A partial update to a route has to actually reach the database.
//
// This is the regression anchor for an entire untested update path: the SQL set
// updated_at while the table had no such column, so every route edit from the
// operator page failed outright. Nobody noticed, because with an empty catalog
// nobody had ever edited a route.
func TestRouteUpdatePersists(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	prov := mustProvider(t, s, "openai", "https://api.example.com")
	model := mustModel(t, s, "openai/route-upd")

	upstream := "gpt-4o"
	created, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
		ModelId: model.Id,
		Body:    &gwstaffapi.GatewayRouteInput{ProviderId: &prov, ProviderModelId: &upstream},
	})
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	route := gwstaffapi.GatewayRoute(created.(gwstaffapi.CreateGatewayRoute201JSONResponse))

	// Change priority, upstream model name and enabled; every field left out
	// must stay as it was.
	newPriority := 10
	newUpstream := "gpt-4o-2024"
	disabled := false
	updated, err := s.UpdateGatewayRoute(ctx, gwstaffapi.UpdateGatewayRouteRequestObject{
		ModelId: model.Id, RouteId: route.Id,
		Body: &gwstaffapi.GatewayRouteInput{
			Priority: &newPriority, ProviderModelId: &newUpstream, Enabled: &disabled,
		},
	})
	if err != nil {
		t.Fatalf("update route: %v", err)
	}
	got := gwstaffapi.GatewayRoute(updated.(gwstaffapi.UpdateGatewayRoute200JSONResponse))

	if got.Priority != newPriority {
		t.Errorf("priority = %d, want %d", got.Priority, newPriority)
	}
	if got.ProviderModelId != newUpstream {
		t.Errorf("upstream model name = %q, want %q", got.ProviderModelId, newUpstream)
	}
	if got.Enabled {
		t.Error("enabled should have been set to false")
	}
	// Fields absent from the request must not be changed along the way; that
	// is what partial-update semantics mean.
	if got.Weight != route.Weight {
		t.Errorf("weight was not supplied yet changed: %d -> %d", route.Weight, got.Weight)
	}

	// updated_at is not part of the wire type, so read it from the database
	// directly. Only its existing and advancing shows that the column added
	// is more than a dummy that keeps the SQL from erroring.
	var advanced bool
	if err := pool.QueryRow(ctx,
		`SELECT updated_at > created_at FROM model_routes WHERE id = $1`, route.Id).
		Scan(&advanced); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if !advanced {
		t.Error("updated_at did not move forward with the update")
	}
}

// The connectivity probe answers 200 whether it passed or failed, with the
// verdict in the body, so no client has to tell "the probe ran and did not
// pass" from "the probe endpoint is broken".
func TestProviderConnectivityTest(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	status := http.StatusOK
	body := `{"choices":[{"message":{"content":"hi"}}]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-good")

	res := mustTest(t, s, prov, "gpt-4o")
	if !res.Ok {
		t.Fatalf("a healthy upstream should test as reachable: %+v", res)
	}

	// A 401 from upstream: still HTTP 200, but ok=false with the upstream's
	// own message carried back.
	status, body = http.StatusUnauthorized, `{"error":{"message":"bad key"}}`
	res = mustTest(t, s, prov, "gpt-4o")
	if res.Ok {
		t.Error("invalid credentials must not report as reachable")
	}
	if !strings.Contains(res.Message, "bad key") {
		t.Errorf("the upstream's own text did not come back, so the operator cannot tell a bad credential from a bad model name: %q", res.Message)
	}

	// The outcome is recorded, so the list page can show why it failed without
	// anyone paying for another probe.
	listed, _ := s.ListGatewayProviderKeys(ctx,
		gwstaffapi.ListGatewayProviderKeysRequestObject{ProviderId: prov})
	keys := listed.(gwstaffapi.ListGatewayProviderKeys200JSONResponse).Items
	if len(keys) == 0 || keys[0].LastVerifiedAt == nil {
		t.Error("the connectivity test left no audit trail")
	}
}

// A provider with no credential yet is a normal intermediate configuration
// state, so the probe should return a verdict rather than an error.
func TestProviderTestWithoutKey(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProvider(t, s, "openai", "https://api.example.com")
	if res := mustTest(t, s, prov, "gpt-4o"); res.Ok {
		t.Error("with no key it must not report as reachable")
	}
}

// ===== Fixtures =====

// A route can be hard-deleted: the cost audit trail lives in the per-request
// snapshot on each usage log row and no longer depends on any history kept on
// the route, so there is nothing left for a "has cost history, cannot delete"
// restriction to protect.
func TestDeleteRouteAllowedAfterProcurementDeversioning(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	provider := mustProvider(t, s, "openai", "https://history.example.com")
	model := mustModel(t, s, "openai/route-history")
	mustRoute(t, s, model.Id, provider, "route-history", []string{"chat"})
	var routeID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM model_routes WHERE model_id=$1`, model.Id).
		Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteGatewayRoute(ctx, gwstaffapi.DeleteGatewayRouteRequestObject{
		ModelId: model.Id, RouteId: routeID,
	}); err != nil {
		t.Fatalf("a route should be deletable (cost auditing no longer depends on route history): %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_routes WHERE id=$1)`, routeID).
		Scan(&exists); err != nil || exists {
		t.Fatalf("the route should be gone: exists=%v err=%v", exists, err)
	}
}

// responses is a legitimate surface in the openai protocol, and verifying it
// publishes the stored-resource operations with it: those cannot be probed
// (nothing to retrieve until a request has stored something) and are only ever
// reached pinned to the route that created the resource, so they ride on
// responses rather than needing a verdict of their own.
func TestRouteAcceptsResponsesEndpoint(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProvider(t, s, "openai", "https://oa-resp.example.com")
	mustCreateKey(t, s, prov, "k", "sk-resp") // with a credential, unverified stays unlisted
	model := mustModel(t, s, "openai/resp-surface")

	mustRoute(t, s, model.Id, prov, "gpt-5.4", []string{"responses"})

	if got := endpointsOfModel(t, s, model.Id); !equalSet(got, []string{"responses", "responses_resources"}) {
		t.Fatalf("exposed capabilities = %v, want responses plus the stored-resource operations that ride on it", got)
	}

	// It coexists with chat on one model: an upstream serving both endpoints
	// is a normal shape.
	mustRoute(t, s, model.Id, prov, "gpt-5.4-chat", []string{"chat"})
	if got := endpointsOfModel(t, s, model.Id); !equalSet(got, []string{"chat", "responses", "responses_resources"}) {
		t.Errorf("exposed capabilities = %v, want the union of the two routes", got)
	}
}

// The caller chooses which endpoint the probe hits, instead of everything going
// to chat/completions.
//
// The prompt for this was a real upstream that serves only responses: probing
// nine of its models as chat returned 400 every time. The same shape shows up
// on providers that only do embeddings or images. Probing those as chat fails
// by construction, and what failed is the probe, not the provider -- so the
// operator sees red, goes to check the credential, and finds nothing wrong.
func TestConnectivityTestPicksEndpoint(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-good")

	probe := func(ep *gwstaffapi.TestGatewayProviderJSONBodyEndpoint) (string, error) {
		gotPath = ""
		_, err := s.TestGatewayProvider(ctx, gwstaffapi.TestGatewayProviderRequestObject{
			ProviderId: prov,
			Body: &gwstaffapi.TestGatewayProviderJSONRequestBody{
				UpstreamModel: "m", Endpoint: ep,
			},
		})
		return gotPath, err
	}

	// Omitting it means the protocol's canonical endpoint. This assertion is the
	// baseline default for the request contract.
	if p, err := probe(nil); err != nil || p != "/v1/chat/completions" {
		t.Errorf("the default should hit chat/completions: path=%q err=%v", p, err)
	}

	emb := gwstaffapi.TestGatewayProviderJSONBodyEndpointEmbeddings
	if p, err := probe(&emb); err != nil || p != "/v1/embeddings" {
		t.Errorf("choosing embeddings should hit /v1/embeddings: path=%q err=%v", p, err)
	}

	img := gwstaffapi.TestGatewayProviderJSONBodyEndpointImages
	if p, err := probe(&img); err != nil || p != "/v1/images/generations" {
		t.Errorf("choosing images should hit /v1/images/generations: path=%q err=%v", p, err)
	}

	// A cross-protocol endpoint is refused before any request goes out --
	// otherwise real money buys a failure that was certain in advance.
	msg := gwstaffapi.TestGatewayProviderJSONBodyEndpointMessages
	p, err := probe(&msg)
	if err == nil {
		t.Error("messages on an openai provider should be refused")
	}
	if p != "" {
		t.Errorf("a refused probe must not actually be sent (it costs money and is bound to fail): path=%q", p)
	}
}

// The connectivity probe sends the request the data plane would send, envelope
// and all.
//
// Without this the probe is a second, simpler request builder, and the two
// disagree exactly where it matters most: on a provider whose profile re-cuts
// the body, the probe would send a shape the upstream refuses and report a
// working configuration as broken. A diagnostic that disagrees with the thing
// it diagnoses is worse than no diagnostic, because it is believed.
func TestConnectivityProbeSendsTheEnvelopedBody(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	var gotPath string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer up.Close()

	profile := map[string]any{
		"envelope": "vertex",
		"path_overrides": map[string]any{
			"/v1/messages": "/v1/projects/p/locations/l/publishers/anthropic/models/{model}:rawPredict",
		},
	}
	slug := "vertex-" + uuid.NewString()[:8]
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
			Slug:      &slug,
			Protocols: &[]gwstaffapi.GatewayProviderInputProtocols{"anthropic"},
			BaseUrl:   new(up.URL),
			Transport: &profile,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prov := created.(gwstaffapi.CreateGatewayProvider201JSONResponse).Id
	mustCreateKey(t, s, prov, "k", "sk-good")

	if _, err := s.TestGatewayProvider(ctx, gwstaffapi.TestGatewayProviderRequestObject{
		ProviderId: prov,
		Body:       &gwstaffapi.TestGatewayProviderJSONRequestBody{UpstreamModel: "claude-x"},
	}); err != nil {
		t.Fatal(err)
	}

	if want := "/v1/projects/p/locations/l/publishers/anthropic/models/claude-x:rawPredict"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotBody == nil {
		t.Fatal("the upstream received no readable body")
	}
	if _, has := gotBody["model"]; has {
		t.Errorf("the probe left the model in the body, which this endpoint refuses: %v", gotBody)
	}
	if gotBody["anthropic_version"] != "vertex-2023-10-16" {
		t.Errorf("the probe omitted the version this endpoint requires: %v", gotBody)
	}
}

// A route is accepted on any provider, whatever protocols it speaks: a model
// owns no protocol, so there is nothing on its side to mismatch. The same
// slug wired to an openai-only provider and to an anthropic-only one is
// reachable on both /v1/chat/completions and /v1/messages, each request
// passing through on the protocol it arrived on.
//
// What the old configuration-time refusal protected against -- a route that
// could never be selected -- can no longer be configured, because the route
// no longer declares the thing that was being checked.
func TestCrossProtocolRouteIsAccepted(t *testing.T) {
	s, _, _ := newServer(t)
	model := mustModel(t, s, "anthropic/claude-x")
	anthropicProv := mustProvider(t, s, "anthropic", "https://an.example.com")
	openaiProv := mustProvider(t, s, "openai", "https://oa.example.com")
	mustCreateKey(t, s, anthropicProv, "k", "sk-an")
	mustCreateKey(t, s, openaiProv, "k", "sk-oa")

	anRoute := mustRoute(t, s, model.Id, anthropicProv, "claude-x", []string{"messages"})
	oaRoute := mustRoute(t, s, model.Id, openaiProv, "claude-x", []string{"chat"})
	if anRoute == oaRoute {
		t.Fatal("two routes were expected")
	}

	// The model is listed as configured on both protocols, and advertises the
	// verified endpoint of each.
	res, err := s.GetGatewayModel(context.Background(), gwstaffapi.GetGatewayModelRequestObject{ModelId: model.Id})
	if err != nil {
		t.Fatal(err)
	}
	got := gwstaffapi.GatewayModel(res.(gwstaffapi.GetGatewayModel200JSONResponse))
	if !equalSet(got.Protocols, []string{"anthropic", "openai"}) {
		t.Errorf("protocols = %v, want both: the model is reachable on every protocol its providers speak", got.Protocols)
	}
	if !equalSet(got.Endpoints, []string{"chat", "messages"}) {
		t.Errorf("endpoints = %v, want the verified endpoint of each route", got.Endpoints)
	}
}

// The operator's verdict is bounded by the provider: an endpoint of a protocol
// the route's provider does not speak can never be asked of that route, so a
// verdict for it is refused rather than stored where no reader would see it.
func TestRouteVerdictMustBelongToAProviderProtocol(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "openai/x-verdict")
	prov := mustProvider(t, s, "openai", "https://oa2.example.com")
	route := mustRoute(t, s, model.Id, prov, "up-a", []string{"chat"})

	if _, err := s.SetGatewayRouteProbe(ctx, gwstaffapi.SetGatewayRouteProbeRequestObject{
		ModelId: model.Id, RouteId: route, Endpoint: "messages",
		Body: &gwstaffapi.GatewayRouteProbeOverride{Status: gwstaffapi.GatewayRouteProbeOverrideStatusOk},
	}); err == nil {
		t.Error("a verdict on messages for a route whose provider speaks only openai should be refused")
	} else if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "openai") {
		t.Errorf("the error should name both protocols, or the operator cannot tell which side to change: %v", err)
	}
	if _, err := s.SetGatewayRouteProbe(ctx, gwstaffapi.SetGatewayRouteProbeRequestObject{
		ModelId: model.Id, RouteId: route, Endpoint: "video",
		Body: &gwstaffapi.GatewayRouteProbeOverride{Status: gwstaffapi.GatewayRouteProbeOverrideStatusOk},
	}); err == nil {
		t.Error("an unknown endpoint should be refused")
	}
	// A legitimate verdict must still go through. Without this anchor, a
	// check that refuses everything would satisfy both assertions above.
	if v := mustVerdict(t, s, model.Id, route, "images", "ok"); v.Source != "operator" || v.Status != "ok" {
		t.Fatalf("the operator's verdict should come back as theirs: %+v", v)
	}
}

// Renaming the upstream model invalidates every verdict on the route, the
// operator's override included: the override was about the old name, and a
// stale green for a name nobody has looked at is the listed-but-404 the
// catalogue exists to prevent.
func TestRenamingTheUpstreamResetsTheOperatorsVerdictToo(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "openai/x-rename")
	prov := mustProvider(t, s, "openai", "https://oa3.example.com")
	mustCreateKey(t, s, prov, "k", "sk-img")
	route := mustRoute(t, s, model.Id, prov, "gpt-image-2", nil)
	if v := mustVerdict(t, s, model.Id, route, "images", "ok"); v.Source != "operator" {
		t.Fatalf("precondition: the operator's verdict should be theirs: %+v", v)
	}

	renamed := "gpt-image-3-preview"
	if _, err := s.UpdateGatewayRoute(ctx, gwstaffapi.UpdateGatewayRouteRequestObject{
		ModelId: model.Id, RouteId: route,
		Body: &gwstaffapi.GatewayRouteInput{ProviderModelId: &renamed},
	}); err != nil {
		t.Fatal(err)
	}
	var status, source string
	if err := pool.QueryRow(ctx,
		`SELECT status, source FROM model_route_probes WHERE route_id = $1 AND endpoint = 'images'`, route).
		Scan(&status, &source); err != nil {
		t.Fatal(err)
	}
	if status != "unverified" || source != "probe" {
		t.Fatalf("after a rename the operator's row must be handed back unverified: %s/%s", status, source)
	}
	if got := endpointsOfModel(t, s, model.Id); len(got) != 0 {
		t.Fatalf("nothing is published for a name nobody has looked at: %v", got)
	}
}

// Echoing a provider's current protocol set is not a change: the update must
// not realign the probe rows of every route on the provider, which would
// re-seed and re-probe the lot for a cosmetic edit. Only a set that moves does.
func TestEchoingTheProtocolSetDoesNotReseed(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	both := mustProviderWith(t, s, []string{"openai", "anthropic"}, "https://agg3.example.com")
	model := mustModel(t, s, "anthropic/echo")
	route := mustRoute(t, s, model.Id, both, "up", []string{"messages"})
	// A verdict the reseed would visibly disturb if it ran: rows for a
	// protocol it "drops and re-adds" come back unverified.
	before := func() string {
		t.Helper()
		var st string
		if err := pool.QueryRow(ctx, `SELECT status FROM model_route_probes WHERE route_id = $1 AND endpoint = 'messages'`, route).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if before() != "ok" {
		t.Fatal("precondition")
	}
	same := []gwstaffapi.GatewayProviderInputProtocols{
		gwstaffapi.GatewayProviderInputProtocolsAnthropic, gwstaffapi.GatewayProviderInputProtocolsOpenai,
	}
	name := "renamed"
	if _, err := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: both, Body: &gwstaffapi.GatewayProviderInput{Name: &name, Protocols: &same},
	}); err != nil {
		t.Fatal(err)
	}
	if before() != "ok" {
		t.Fatal("an unchanged protocol set, in any order, must leave the verdicts alone")
	}
}

// A multi-dialect provider carries any model on every protocol it speaks: one
// record, probed on the endpoints of both protocols.
//
// The promise behind it: an aggregator that serves both chat/completions and
// messages is one provider record, not two copies each carrying its own
// credential, breaker state and margin accounting -- and the same Claude model
// is reachable through it on either surface.
func TestRouteOnMultiDialectProviderIsProbedOnBothProtocols(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	both := mustProviderWith(t, s, []string{"openai", "anthropic"}, "https://agg.example.com")
	model := mustModel(t, s, "anthropic/agg-claude")
	mustRoute(t, s, model.Id, both, "claude-agg", nil)

	res, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model.Id})
	if err != nil {
		t.Fatal(err)
	}
	routes := res.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items
	if len(routes) != 1 || routes[0].Probes == nil {
		t.Fatalf("the route should carry probe rows: %+v", routes)
	}
	if !equalSet(routes[0].ProviderProtocols, []string{"openai", "anthropic"}) {
		t.Errorf("the row should carry the provider's protocols: %v", routes[0].ProviderProtocols)
	}
	seen := map[string]bool{}
	for _, p := range *routes[0].Probes {
		seen[string(p.Endpoint)] = true
	}
	for _, want := range []string{"chat", "responses", "embeddings", "images", "messages", "messages_count_tokens"} {
		if !seen[want] {
			t.Errorf("the route should be probed on %s, an endpoint of a protocol its provider speaks; rows: %v", want, seen)
		}
	}
	if seen["generate_content"] {
		t.Errorf("gemini is not a protocol this provider speaks, so its endpoints must not be seeded: %v", seen)
	}
}

// A provider's protocol set can be narrowed freely -- a model owns no
// protocol, so nothing is orphaned -- and the probe rows follow it: rows for
// a protocol dropped go with it, rows for a protocol added are seeded. The
// read side filters by the same set, so the two never disagree.
func TestRouteProbesFollowProviderProtocols(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	both := mustProviderWith(t, s, []string{"openai", "anthropic"}, "https://agg2.example.com")
	model := mustModel(t, s, "anthropic/follow")
	route := mustRoute(t, s, model.Id, both, "up-an", []string{"messages", "chat"})

	probes := func() map[string]string {
		t.Helper()
		res, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model.Id})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, p := range *res.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items[0].Probes {
			out[string(p.Endpoint)] = string(p.Status)
		}
		return out
	}
	if got := probes(); got["messages"] != "ok" || got["chat"] != "ok" {
		t.Fatalf("both verdicts should show before narrowing: %v", got)
	}

	onlyOpenAI := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
	if _, err := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: both, Body: &gwstaffapi.GatewayProviderInput{Protocols: &onlyOpenAI},
	}); err != nil {
		t.Fatalf("narrowing a provider's protocols is allowed; nothing is orphaned: %v", err)
	}
	if got := probes(); got["chat"] != "ok" || got["messages"] != "" {
		t.Errorf("after narrowing, the anthropic rows should be gone and the openai ones untouched: %v", got)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_route_probes WHERE route_id = $1 AND protocol = 'anthropic'`, route).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the anthropic rows should be deleted, not merely hidden: %d left", n)
	}

	// Widening seeds the rows again, unverified, and asks for a probe.
	bothAgain := []gwstaffapi.GatewayProviderInputProtocols{
		gwstaffapi.GatewayProviderInputProtocolsOpenai, gwstaffapi.GatewayProviderInputProtocolsAnthropic,
	}
	if _, err := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: both, Body: &gwstaffapi.GatewayProviderInput{Protocols: &bothAgain},
	}); err != nil {
		t.Fatal(err)
	}
	if got := probes(); got["messages"] != "unverified" || got["chat"] != "ok" {
		t.Errorf("after widening, the anthropic rows should be back as unverified and the openai verdicts kept: %v", got)
	}
}

func mustProvider(t *testing.T, s *gwstaffapi.Server, protocol, baseURL string) uuid.UUID {
	t.Helper()
	return mustProviderWith(t, s, []string{protocol}, baseURL)
}

// mustProviderWith creates a provider declaring an arbitrary set of dialects.
// A transport profile the gateway cannot act on is refused where the person who
// typed it is standing, with a message naming what was wrong.
//
// The alternative behaviours are both worse and both were available: storing it
// and ignoring it makes the setting look like it works, and letting it through
// to the data plane turns a configuration mistake into an upstream failure
// attributed to the provider.
func TestProviderTransportProfileRefusedOnSave(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	bad := map[string]map[string]any{
		"an unknown key":       {"retires": 3},
		"an unknown auth mode": {"auth": "basic"},
		// Signing with nowhere to sign for. There is no default that could be
		// right, and a signature computed for the wrong region fails as an
		// authentication error -- which sends the reader to look at the key.
		"signing with no region": {"auth": "aws_sigv4"},
		// The mirror image: a signing region on a provider that never signs is
		// a setting that saves and does nothing.
		"a signing region without signing": {"sigv4": map[string]any{"region": "us-east-1"}},
		"an unknown envelope":              {"envelope": "vertexai"},
		"an override the gateway never sends": {
			"path_overrides": map[string]any{"/v1/completions": "/x"},
		},
		"a streaming override the gateway never sends": {
			"stream_path_overrides": map[string]any{"/v1/completions": "/x"},
		},
		"a connect bound outside the range": {"connect_timeout_ms": 900000},
	}
	for name, profile := range bad {
		t.Run(name, func(t *testing.T) {
			slug := "bad-" + uuid.NewString()[:8]
			_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
				Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
					Slug:      &slug,
					Protocols: &[]gwstaffapi.GatewayProviderInputProtocols{"openai"},
					BaseUrl:   new("https://up.test"),
					Transport: &profile,
				},
			})
			if err == nil {
				t.Fatal("an unusable transport profile should be refused")
			}
			// A validation refusal, not a 500. The distinction matters to the
			// caller: one says "fix what you sent", the other says "this is
			// broken, try again later", and only one of those is true.
			var ce *httpx.CodeError
			if !errors.As(err, &ce) || ce.Code != errcode.CommonValidation {
				t.Fatalf("want a validation refusal, got %v", err)
			}
			if ce.Detail == "" {
				t.Error("the refusal must say what was wrong; an empty detail leaves the operator guessing")
			}
		})
	}

	// A valid profile survives the round trip. Refusing everything would satisfy
	// the assertions above on its own, so this is the anchor that stops that.
	good := map[string]any{
		"auth":               "header:api-key",
		"query":              map[string]any{"api-version": "2024-10-21"},
		"path_overrides":     map[string]any{"/v1/chat/completions": "/openai/deployments/{model}/chat/completions"},
		"connect_timeout_ms": 3000,
	}
	slug := "azure-" + uuid.NewString()[:8]
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
			Slug:      &slug,
			Protocols: &[]gwstaffapi.GatewayProviderInputProtocols{"openai"},
			BaseUrl:   new("https://res.openai.azure.test"),
			Transport: &good,
		},
	})
	if err != nil {
		t.Fatalf("a valid transport profile should be accepted: %v", err)
	}
	id := created.(gwstaffapi.CreateGatewayProvider201JSONResponse).Id

	// Read it back through the detail view rather than trusting the create
	// response. "Stored" and "rendered on the page the operator returns to" are
	// different claims, and it is the second one that decides whether the
	// setting can be edited again.
	got, err := s.GetGatewayProvider(ctx, gwstaffapi.GetGatewayProviderRequestObject{ProviderId: id})
	if err != nil {
		t.Fatal(err)
	}
	back := gwstaffapi.GatewayProvider(got.(gwstaffapi.GetGatewayProvider200JSONResponse))
	if back.Transport == nil {
		t.Fatal("the detail view must render the stored transport profile, or it cannot be edited")
	}
	if (*back.Transport)["auth"] != "header:api-key" {
		t.Errorf("auth did not round trip: %v", *back.Transport)
	}

	// A complete signing profile is accepted and comes back intact, including
	// the nested object. Nesting is where a strict validator usually starts
	// dropping things it did not expect to have to carry.
	signing := map[string]any{
		"auth":     "aws_sigv4",
		"envelope": "bedrock",
		"sigv4":    map[string]any{"region": "us-east-1"},
		"path_overrides": map[string]any{
			"/v1/messages": "/model/{model}/invoke",
		},
		"stream_path_overrides": map[string]any{
			"/v1/messages": "/model/{model}/invoke-with-response-stream",
		},
	}
	bedrockSlug := "bedrock-" + uuid.NewString()[:8]
	bedrock, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom,
			Slug:      &bedrockSlug,
			Protocols: &[]gwstaffapi.GatewayProviderInputProtocols{"anthropic"},
			BaseUrl:   new("https://bedrock-runtime.us-east-1.amazonaws.test"),
			Transport: &signing,
		},
	})
	if err != nil {
		t.Fatalf("a complete signing profile should be accepted: %v", err)
	}
	shown, err := s.GetGatewayProvider(ctx, gwstaffapi.GetGatewayProviderRequestObject{
		ProviderId: bedrock.(gwstaffapi.CreateGatewayProvider201JSONResponse).Id,
	})
	if err != nil {
		t.Fatal(err)
	}
	storedSigning := gwstaffapi.GatewayProvider(shown.(gwstaffapi.GetGatewayProvider200JSONResponse)).Transport
	if storedSigning == nil {
		t.Fatal("the signing profile did not come back")
	}
	nested, ok := (*storedSigning)["sigv4"].(map[string]any)
	if !ok || nested["region"] != "us-east-1" {
		t.Errorf("the nested signing target did not round trip: %v", (*storedSigning)["sigv4"])
	}
	if (*storedSigning)["envelope"] != "bedrock" {
		t.Errorf("the envelope did not round trip: %v", *storedSigning)
	}
	// The profile is returned to the browser, so it must never be a place a
	// credential can be stored. Nothing that arrives here is secret; this
	// asserts the shape stays that way.
	for _, forbidden := range []string{"access_key_id", "secret_access_key", "session_token", "private_key"} {
		if _, has := (*storedSigning)[forbidden]; has {
			t.Errorf("the transport profile carries %q, which is a credential field", forbidden)
		}
	}

	// And a bad profile on update is refused the same way, with the stored one
	// left alone.
	worse := map[string]any{"auth": "aws_sigv4", "sigv4": map[string]any{"regoin": "us-east-1"}}
	if _, err := s.UpdateGatewayProvider(ctx, gwstaffapi.UpdateGatewayProviderRequestObject{
		ProviderId: id,
		Body:       &gwstaffapi.GatewayProviderInput{Transport: &worse},
	}); err == nil {
		t.Fatal("a misspelled key inside the nested object should be refused on update too")
	}
	after, err := s.GetGatewayProvider(ctx, gwstaffapi.GetGatewayProviderRequestObject{ProviderId: id})
	if err != nil {
		t.Fatal(err)
	}
	kept := gwstaffapi.GatewayProvider(after.(gwstaffapi.GetGatewayProvider200JSONResponse))
	if kept.Transport == nil || (*kept.Transport)["auth"] != "header:api-key" {
		t.Errorf("a refused update must leave the stored profile alone: %v", kept.Transport)
	}
}

func mustProviderWith(t *testing.T, s *gwstaffapi.Server, protocols []string, baseURL string) uuid.UUID {
	t.Helper()
	return mustProviderForVendor(t, s, catalog.VendorCustom, protocols, baseURL)
}

// mustProviderForVendor creates a provider belonging to a stated vendor, for the
// cases where the vendor is what the test is about.
func mustProviderForVendor(
	t *testing.T, s *gwstaffapi.Server, vendor string, protocols []string, baseURL string,
) uuid.UUID {
	t.Helper()
	fs := make([]gwstaffapi.GatewayProviderInputProtocols, 0, len(protocols))
	for _, f := range protocols {
		fs = append(fs, gwstaffapi.GatewayProviderInputProtocols(f))
	}
	slug := strings.Join(protocols, "-") + "-" + uuid.NewString()[:8]
	res, err := s.CreateGatewayProvider(context.Background(),
		gwstaffapi.CreateGatewayProviderRequestObject{
			Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendor, Slug: &slug, Protocols: &fs, BaseUrl: &baseURL},
		})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return res.(gwstaffapi.CreateGatewayProvider201JSONResponse).Id
}

func mustCreateKey(t *testing.T, s *gwstaffapi.Server, prov uuid.UUID, name, secret string) gwstaffapi.GatewayProviderKey {
	t.Helper()
	res, err := s.CreateGatewayProviderKey(context.Background(),
		gwstaffapi.CreateGatewayProviderKeyRequestObject{
			ProviderId: prov,
			Body: &gwstaffapi.CreateGatewayProviderKeyJSONRequestBody{
				Name: &name, Secret: secret,
			},
		})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return gwstaffapi.GatewayProviderKey(res.(gwstaffapi.CreateGatewayProviderKey201JSONResponse))
}

func mustModel(t *testing.T, s *gwstaffapi.Server, slug string) gwstaffapi.GatewayModel {
	t.Helper()
	return mustModelWith(t, s, slug, &gwstaffapi.GatewayModelInput{})
}

func mustModelWith(t *testing.T, s *gwstaffapi.Server, slug string, in *gwstaffapi.GatewayModelInput) gwstaffapi.GatewayModel {
	t.Helper()
	in.Slug = &slug
	res, err := s.CreateGatewayModel(context.Background(),
		gwstaffapi.CreateGatewayModelRequestObject{Body: in})
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	return gwstaffapi.GatewayModel(res.(gwstaffapi.CreateGatewayModel201JSONResponse))
}

// mustRoute wires a model to a provider and then records each of verified as
// `ok` through the operator override -- the route itself declares nothing, and
// the worker is not running in these tests. The verified set shapes what the
// catalogue lists; candidacy is wider, and a test about that writes
// `unsupported` with mustVerdict.
func mustRoute(t *testing.T, s *gwstaffapi.Server, model, prov uuid.UUID, upstream string, verified []string) uuid.UUID {
	t.Helper()
	res, err := s.CreateGatewayRoute(context.Background(),
		gwstaffapi.CreateGatewayRouteRequestObject{
			ModelId: model,
			Body:    &gwstaffapi.GatewayRouteInput{ProviderId: &prov, ProviderModelId: &upstream},
		})
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	route := gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))
	for _, ep := range verified {
		mustVerdict(t, s, model, route.Id, ep, "ok")
	}
	return route.Id
}

// mustVerdict writes the operator's verdict for one endpoint of a route.
func mustVerdict(t *testing.T, s *gwstaffapi.Server, model, route uuid.UUID, endpoint, status string) gwstaffapi.GatewayRouteProbe {
	t.Helper()
	res, err := s.SetGatewayRouteProbe(context.Background(), gwstaffapi.SetGatewayRouteProbeRequestObject{
		ModelId: model, RouteId: route, Endpoint: endpoint,
		Body: &gwstaffapi.GatewayRouteProbeOverride{Status: gwstaffapi.GatewayRouteProbeOverrideStatus(status)},
	})
	if err != nil {
		t.Fatalf("set verdict %s=%s: %v", endpoint, status, err)
	}
	return gwstaffapi.GatewayRouteProbe(res.(gwstaffapi.SetGatewayRouteProbe200JSONResponse))
}

func mustTest(t *testing.T, s *gwstaffapi.Server, prov uuid.UUID, upstreamModel string) gwstaffapi.GatewayProviderTestResult {
	t.Helper()
	res, err := s.TestGatewayProvider(context.Background(),
		gwstaffapi.TestGatewayProviderRequestObject{
			ProviderId: prov,
			Body:       &gwstaffapi.TestGatewayProviderJSONRequestBody{UpstreamModel: upstreamModel},
		})
	if err != nil {
		t.Fatalf("connectivity test: %v", err)
	}
	return gwstaffapi.GatewayProviderTestResult(res.(gwstaffapi.TestGatewayProvider200JSONResponse))
}

// endpointsOfModel reads the advertised capability union from the model list.
func endpointsOfModel(t *testing.T, s *gwstaffapi.Server, id uuid.UUID) []string {
	t.Helper()
	res, err := s.ListGatewayModels(context.Background(), gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.(gwstaffapi.ListGatewayModels200JSONResponse).Items {
		if m.Id == id {
			return m.Endpoints
		}
	}
	t.Fatalf("model %s is not in the listing", id)
	return nil
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// The health dashboard joins each provider straight to its rollups instead of
// apportioning them through the model slug.
//
// Going providers -> routes -> models -> rollups(model_slug) means:
//
//	one model on two providers  -> each shows that model's full request and
//	                               error counts
//	two routes on one provider  -> the same data counted twice
//
// The "error rate for this provider" it displays is then neither that
// provider's nor any real number at all.
func TestProviderHealthDoesNotSmearAcrossProviders(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	pa := mustProvider(t, s, "openai", "https://a.example.com")
	pb := mustProvider(t, s, "openai", "https://b.example.com")
	model := mustModel(t, s, "openai/shared")
	// One model on two providers, with two routes on A -- the apportioning
	// version double-counts that.
	mustRoute(t, s, model.Id, pa, "up-a1", []string{"chat"})
	mustRoute(t, s, model.Id, pa, "up-a2", []string{"embeddings"})
	mustRoute(t, s, model.Id, pb, "up-b", []string{"chat"})

	var key string
	org := publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Name: "H"}))
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,'k','sk-flb-v1-h','h`+uuid.NewString()+`',ARRAY['inference']) RETURNING id`,
		org).Scan(&key); err != nil {
		t.Fatal(err)
	}
	// Only provider A ever served anything: 10 requests, 2 errors.
	if _, err := pool.Exec(ctx,
		`INSERT INTO gateway_usage_rollups
		   (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,requests,errors)
		 VALUES ($1,date_trunc('hour',now()),'hour',$2,'openai/shared',$3,10,2)`,
		org, key, pa); err != nil {
		t.Fatal(err)
	}

	res, err := s.GetGatewayHealth(ctx, gwstaffapi.GetGatewayHealthRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]gwstaffapi.GatewayProviderHealth{}
	for _, h := range res.(gwstaffapi.GetGatewayHealth200JSONResponse).Providers {
		byID[h.ProviderId.String()] = h
	}

	if got := byID[pa.String()]; got.Requests1h != 10 || got.Errors1h != 2 {
		t.Errorf("provider A should read 10/2 (two routes must not double it), got %d/%d",
			got.Requests1h, got.Errors1h)
	}
	if got := byID[pb.String()]; got.Requests1h != 0 || got.Errors1h != 0 {
		t.Errorf("provider B never served anything, so it should read 0/0, got %d/%d -- "+
			"a non-zero means it is still being apportioned by model_slug", got.Requests1h, got.Errors1h)
	}
}

// Creating a route seeds one probe row per probeable endpoint of every
// protocol the provider speaks, initially unverified.
//
// The rows are seeded in the same transaction that creates the route, not once
// a worker gets around to it -- otherwise the operator sees a blank panel right
// after saving. Image endpoints get a row but are never probed automatically,
// since they cost one to two orders of magnitude more than text ones and the UI
// promises they run on click only. The stored-resource operations get no row:
// they cannot be probed and are published by derivation.
func TestRouteProbesSeededOnCreate(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	prov := mustProvider(t, s, "openai", "https://probe.example.com")
	model := mustModel(t, s, "openai/probe-seed")
	mustRoute(t, s, model.Id, prov, "up-x", nil)

	res, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model.Id})
	if err != nil {
		t.Fatal(err)
	}
	routes := res.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items
	if len(routes) != 1 || routes[0].Probes == nil {
		t.Fatalf("the route should carry a probe verdict: %+v", routes)
	}
	got := map[string]string{}
	for _, p := range *routes[0].Probes {
		got[string(p.Endpoint)] = string(p.Status)
		if p.Source != "probe" {
			t.Errorf("%s: a seeded row belongs to the worker, got source %s", p.Endpoint, p.Source)
		}
	}
	want := routeprobe.Probeable(catalog.EndpointsForProtocols([]string{"openai"}))
	if len(got) != len(want) {
		t.Fatalf("every probeable openai endpoint should have its own row: got %v, want %v", got, want)
	}
	for _, ep := range want {
		if got[ep] != "unverified" {
			t.Errorf("%s should start as unverified (the worker has not run yet), got %q", ep, got[ep])
		}
	}
	if _, has := got["responses_resources"]; has {
		t.Error("the stored-resource operations cannot be probed and must not be seeded")
	}
}

// The full trace (include_trace) is a diagnostic that only a trusted
// deployment may enable: the request headers it returns carry the decrypted
// credential. These cases guard that gate, and the first one matters most.
func TestConnectivityTestTrace(t *testing.T) {
	s, pool, brk := newServer(t)
	// Capturing a trace writes an audit entry, and no audit entry means no
	// trace, so this path needs a staff identity. That is not scaffolding
	// added to make the test pass: it is a precondition of the feature.
	staffID := uuid.New()
	ctx := httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "admin", Subject: staffID.String(), Role: "superadmin",
	})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Marker", "hello")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL)
	const secret = "sk-super-secret-value"
	mustCreateKey(t, s, prov, "k", secret)

	yes := true
	probe := func(includeTrace *bool) *gwstaffapi.GatewayProbeTrace {
		t.Helper()
		resp, err := s.TestGatewayProvider(ctx, gwstaffapi.TestGatewayProviderRequestObject{
			ProviderId: prov,
			Body: &gwstaffapi.TestGatewayProviderJSONRequestBody{
				UpstreamModel: "m", IncludeTrace: includeTrace,
			},
		})
		if err != nil {
			t.Fatalf("probe failed: %v", err)
		}
		return gwstaffapi.GatewayProviderTestResult(resp.(gwstaffapi.TestGatewayProvider200JSONResponse)).Trace
	}

	// Gate closed (traceEnabled at its zero value of false): asking explicitly
	// still gets nothing. This is the security premise of the whole feature --
	// if it goes red, a plaintext credential can be pulled out of a live
	// deployment's API.
	if tr := probe(&yes); tr != nil {
		t.Fatalf("with the environment gate off no trace may be returned, yet one arrived: %+v", tr)
	}

	// Gate open but not asked for: still nothing. A few kilobytes of payload
	// do not ride along on every probe by default.
	s = serverForPool(t, pool, brk, func(cfg *gwstaffapi.ServerConfig) {
		cfg.ProbeTrace = true
	})
	if tr := probe(nil); tr != nil {
		t.Errorf("without an explicit include_trace no trace should be returned: %+v", tr)
	}

	// Gate open and asked for: the full trace, and it really does contain the
	// credential in clear text. Not redacting is the premise of the feature --
	// "is the key I stored the same one that actually goes out" can only be
	// answered by seeing it -- so asserting its presence forces any future
	// "let me just add redaction here" to change this line first.
	tr := probe(&yes)
	if tr == nil {
		t.Fatal("with the gate open and the trace explicitly requested it should be returned")
	}
	if !strings.Contains(tr.Request, secret) {
		t.Errorf("the raw request should contain the plaintext key (this feature does not redact), got: %s", tr.Request)
	}
	if !strings.Contains(tr.Request, "Authorization: Bearer") {
		t.Errorf("the openai protocol should use the Authorization header, got: %s", tr.Request)
	}
	// DumpRequestOut runs the transport's own logic, so it sees a User-Agent
	// that is not in req.Header. Assembling req.Header by hand would miss it,
	// which is exactly why the code does not do that.
	if !strings.Contains(tr.Request, "User-Agent:") {
		t.Errorf("the raw request should contain the User-Agent the transport adds, got: %s", tr.Request)
	}
	if tr.Url == "" || !strings.HasPrefix(tr.Url, up.URL) {
		t.Errorf("url should be the resolved upstream address, got: %q", tr.Url)
	}
	if tr.Response == nil || !strings.Contains(*tr.Response, "X-Upstream-Marker: hello") {
		t.Errorf("the raw response should contain the upstream response headers, got: %v", tr.Response)
	}
	if tr.Response == nil || !strings.Contains(*tr.Response, `{"ok":true}`) {
		t.Errorf("the raw response should contain the body, got: %v", tr.Response)
	}
	if tr.ResponseStatus == "" {
		t.Error("the status line should come back")
	}

	// Capturing a trace must leave an audit entry: this request handed out a
	// credential in clear text and has to be attributable afterwards. The
	// assertion that Meta contains no trace content matters too -- the trace
	// is deliberately never stored, since writing it to the audit table would
	// leave a plaintext credential in a table that can be queried, exported,
	// and is retained for years, which is far worse than returning it once.
	var actorID, meta string
	if err := pool.QueryRow(ctx,
		`SELECT actor_id::text, meta::text FROM audit_logs
		 WHERE action = 'gateway.provider.trace_captured'
		 ORDER BY created_at DESC LIMIT 1`,
	).Scan(&actorID, &meta); err != nil {
		t.Fatalf("fetching a trace should leave an audit row: %v", err)
	}
	if strings.Contains(meta, secret) {
		t.Errorf("the audit meta must not contain the plaintext key, got: %s", meta)
	}
	if !strings.Contains(meta, "endpoint") {
		t.Errorf("the audit meta should carry the locating dimensions, got: %s", meta)
	}
	// Attribution only works if the right person was recorded.
	if actorID != staffID.String() {
		t.Errorf("the audit actor should be the staff who started it: want %s got %s", staffID, actorID)
	}
}

// All four catalog creation paths answer 409 on a unique-constraint collision.
//
// Left alone they all land on a generic 500, so the create dialog says
// "internal error" when the truth is "you already made one with that slug" --
// rendering the most self-fixable kind of failure as the least diagnosable one.
func TestCatalogCreateConflictsAre409(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	t.Run("provider slug", func(t *testing.T) {
		slug, base := "dup-provider-"+uuid.NewString()[:8], "https://dup.example.com"
		fams := []gwstaffapi.GatewayProviderInputProtocols{gwstaffapi.GatewayProviderInputProtocolsOpenai}
		create := func() error {
			_, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
				Body: &gwstaffapi.GatewayProviderInput{Vendor: &vendorCustom, Slug: &slug, Protocols: &fams, BaseUrl: &base},
			})
			return err
		}
		if err := create(); err != nil {
			t.Fatalf("first create provider: %v", err)
		}
		assertConflict(t, create())
	})

	t.Run("model slug", func(t *testing.T) {
		slug := "openai/dup-" + uuid.NewString()[:8]
		create := func() error {
			_, err := s.CreateGatewayModel(ctx, gwstaffapi.CreateGatewayModelRequestObject{
				Body: &gwstaffapi.GatewayModelInput{Slug: &slug},
			})
			return err
		}
		if err := create(); err != nil {
			t.Fatalf("first create model: %v", err)
		}
		assertConflict(t, create())
	})

	// Names are unique per provider, and two unnamed keys collide the same way
	// as two identically named ones. The first key created through the
	// provider dialog never hits this; adding a second one from the detail
	// page does.
	t.Run("same key name on the same provider", func(t *testing.T) {
		prov := mustProvider(t, s, "openai", "https://keys.example.com")
		empty := ""
		create := func() error {
			_, err := s.CreateGatewayProviderKey(ctx, gwstaffapi.CreateGatewayProviderKeyRequestObject{
				ProviderId: prov,
				Body: &gwstaffapi.CreateGatewayProviderKeyJSONRequestBody{
					Name: &empty, Secret: "sk-dup-000000000001",
				},
			})
			return err
		}
		if err := create(); err != nil {
			t.Fatalf("first create key: %v", err)
		}
		assertConflict(t, create())
	})

	t.Run("same model, same provider, same upstream name", func(t *testing.T) {
		prov := mustProvider(t, s, "openai", "https://routes.example.com")
		model := mustModel(t, s, "openai/dup-route-"+uuid.NewString()[:8])
		upstream := "gpt-4o-mini"
		create := func() error {
			_, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
				ModelId: model.Id,
				Body: &gwstaffapi.GatewayRouteInput{
					ProviderId: &prov, ProviderModelId: &upstream,
				},
			})
			return err
		}
		if err := create(); err != nil {
			t.Fatalf("first create route: %v", err)
		}
		assertConflict(t, create())
	})
}

// A provider's route_count counts only enabled routes. A disabled route carries
// no traffic, so counting it as "a model is served here" would show the
// provider's readiness checklist green while nothing actually uses it.
func TestProviderRouteCountCountsEnabledOnly(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	prov := mustProvider(t, s, "openai", "https://count.example.com")
	model := mustModel(t, s, "openai/counted-"+uuid.NewString()[:8])

	if got := providerRouteCount(t, s, prov); got != 0 {
		t.Fatalf("a new provider has route_count = %d, want 0", got)
	}

	upstream := "gpt-4o-mini"
	res, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
		ModelId: model.Id,
		Body: &gwstaffapi.GatewayRouteInput{
			ProviderId: &prov, ProviderModelId: &upstream,
		},
	})
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	route := gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))

	if got := providerRouteCount(t, s, prov); got != 1 {
		t.Fatalf("after creating one enabled route, route_count = %d, want 1", got)
	}

	off := false
	if _, err := s.UpdateGatewayRoute(ctx, gwstaffapi.UpdateGatewayRouteRequestObject{
		ModelId: model.Id, RouteId: route.Id,
		Body: &gwstaffapi.GatewayRouteInput{Enabled: &off},
	}); err != nil {
		t.Fatalf("disable route: %v", err)
	}
	if got := providerRouteCount(t, s, prov); got != 0 {
		t.Fatalf("after disabling the route, route_count = %d, want 0", got)
	}
}

// Listing routes by provider. Three independent properties get an assertion
// each, because none of them follows from the others:
//
//   - Only this provider's routes come back. This is the reverse probe: a third
//     route belongs to another provider and must not appear. Without it, a
//     WHERE clause written on model_id would satisfy the other two.
//   - Disabled routes are included. That differs on purpose from route_count,
//     which counts only enabled ones: the count answers "is it carrying
//     traffic", this view answers "what is configured".
//   - Ordering is by model slug, because priorities on different models of the
//     same provider are not comparable.
func TestListProviderRoutesIsProviderScoped(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	mine := mustProvider(t, s, "openai", "https://mine.example.com")
	other := mustProvider(t, s, "openai", "https://other.example.com")
	// A fixed prefix plus one shared random suffix: the relative order of the
	// slugs is then independent of the suffix, so the ordering assertion has a
	// deterministic expected value.
	sfx := uuid.NewString()[:8]
	first := mustModel(t, s, "openai/aaa-"+sfx)
	second := mustModel(t, s, "openai/zzz-"+sfx)

	newRoute := func(modelID uuid.UUID, providerID uuid.UUID, upstream string) gwstaffapi.GatewayRoute {
		t.Helper()
		res, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
			ModelId: modelID,
			Body: &gwstaffapi.GatewayRouteInput{
				ProviderId: &providerID, ProviderModelId: &upstream,
			},
		})
		if err != nil {
			t.Fatalf("create route: %v", err)
		}
		return gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))
	}

	// Create the zzz route first, so the ordering assertion tests ordering
	// rather than insertion order.
	newRoute(second.Id, mine, "gpt-4o")
	disabled := newRoute(first.Id, mine, "gpt-4o-mini")
	newRoute(first.Id, other, "gpt-4o-mini")

	off := false
	if _, err := s.UpdateGatewayRoute(ctx, gwstaffapi.UpdateGatewayRouteRequestObject{
		ModelId: first.Id, RouteId: disabled.Id,
		Body: &gwstaffapi.GatewayRouteInput{Enabled: &off},
	}); err != nil {
		t.Fatalf("disable route: %v", err)
	}

	res, err := s.ListGatewayProviderRoutes(ctx, gwstaffapi.ListGatewayProviderRoutesRequestObject{
		ProviderId: mine,
	})
	if err != nil {
		t.Fatalf("list routes by provider: %v", err)
	}
	got := res.(gwstaffapi.ListGatewayProviderRoutes200JSONResponse).Items

	if len(got) != 2 {
		t.Fatalf("the by-provider lookup returned %d rows, want 2 (the other provider's row must not appear)", len(got))
	}
	for _, r := range got {
		if r.ProviderId != mine {
			t.Errorf("a route belonging to provider %s leaked into the result -- the WHERE is on the wrong axis", r.ProviderId)
		}
	}
	// Ordering: aaa comes before zzz.
	if got[0].ModelId != first.Id || got[1].ModelId != second.Id {
		t.Errorf("ordering = [%s %s], want ordering by model slug (aaa first)", got[0].ModelId, got[1].ModelId)
	}
	// The disabled route has to be present.
	var sawDisabled bool
	for _, r := range got {
		if r.Id == disabled.Id {
			sawDisabled = true
			if r.Enabled {
				t.Error("a disabled route reads back as enabled")
			}
		}
	}
	if !sawDisabled {
		t.Error("the disabled route is missing -- those are exactly the ones that most need to be seen and turned back on")
	}
}

func assertConflict(t *testing.T, err error) {
	t.Helper()
	var coded *httpx.CodeError
	if !errors.As(err, &coded) {
		t.Fatalf("the duplicate-create error = %v, want *httpx.CodeError (anything else renders as a 500)", err)
	}
	if coded.Code != errcode.CommonConflict {
		t.Fatalf("the duplicate-create error code = %q, want %q", coded.Code, errcode.CommonConflict)
	}
	if coded.Detail == "" {
		t.Error("the 409 carries no detail: the operator would be shown an empty statement")
	}
}

func providerRouteCount(t *testing.T, s *gwstaffapi.Server, prov uuid.UUID) int {
	t.Helper()
	res, err := s.ListGatewayProviders(context.Background(), gwstaffapi.ListGatewayProvidersRequestObject{})
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	for _, p := range res.(gwstaffapi.ListGatewayProviders200JSONResponse).Items {
		if p.Id != prov {
			continue
		}
		if p.RouteCount == nil {
			t.Fatal("the provider response has no route_count: the third step of the readiness checklist has no data source")
		}
		return *p.RouteCount
	}
	t.Fatalf("provider %s is not in the listing", prov)
	return 0
}
