package gwstaffapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// Each of the four states holds on its own. This is where the feature's value
// is: a list of strings is useless, whereas "which can be wired in one click,
// which still need a price, which do not exist locally at all" is what the
// operator came for.
func TestDiscoverClassifiesFourStates(t *testing.T) {
	s, pool, _ := newServer(t)

	body := `{"object":"list","data":[
		{"id":"gpt-routed"},{"id":"gpt-mappable"},{"id":"gpt-unpriced"},{"id":"gpt-unknown"}]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet {
			t.Errorf("it should GET /v1/models, got %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	// routed: a local model exists and a route already serves that upstream
	// name.
	routed := mustPricedModel(t, s, pool, "openai/gpt-routed")
	mustRoute(t, s, routed.Id, prov, "gpt-routed", []string{"chat"})
	// mappable: a priced local model whose slug ends in /<upstream name>.
	mustPricedModel(t, s, pool, "openai/gpt-mappable")
	// unpriced: a local model with no price and not marked free.
	mustModel(t, s, "openai/gpt-unpriced")
	// unknown: nothing local at all, which is gpt-unknown.

	res := mustDiscover(t, s, prov)
	if !res.Ok {
		t.Fatalf("the fetch should succeed: %+v", res.Message)
	}
	got := map[string]string{}
	for _, m := range res.Models {
		got[m.UpstreamModel] = string(m.State)
	}
	want := map[string]string{
		"gpt-routed":   "routed",
		"gpt-mappable": "mappable",
		"gpt-unpriced": "unpriced",
		"gpt-unknown":  "unknown",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s should be %s, got %q", k, v, got[k])
		}
	}

	// A mappable entry must carry the local model id, or the client has
	// nothing to build the one-click wiring from.
	for _, m := range res.Models {
		if m.UpstreamModel == "gpt-mappable" {
			if m.ModelId == nil {
				t.Error("a mappable entry must carry the local model_id")
			}
			if m.ModelSlug == nil || *m.ModelSlug != "openai/gpt-mappable" {
				t.Errorf("a mappable entry should carry the local slug: %v", m.ModelSlug)
			}
		}
		if m.UpstreamModel == "gpt-unknown" && m.ModelId != nil {
			t.Error("an unknown entry must not carry a model_id")
		}
	}
}

// Discovery is read-only: no model and no route may appear in the database as a
// result of running it. Creating models automatically would manufacture dead
// unpriced ones that cannot be routed to -- invisible in the catalog and
// failing with a 503 only once a request reaches them.
func TestDiscoverWritesNothing(t *testing.T) {
	s, pool, _ := newServer(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"brand-new-model"},{"id":"another-new"}]}`))
	}))
	defer up.Close()
	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	before := countRows(t, pool, `SELECT count(*) FROM models`)
	beforeRoutes := countRows(t, pool, `SELECT count(*) FROM model_routes`)

	res := mustDiscover(t, s, prov)
	if !res.Ok || len(res.Models) != 2 {
		t.Fatalf("two unknown models should be reported: %+v", res)
	}
	for _, m := range res.Models {
		if m.State != "unknown" {
			t.Errorf("%s should be unknown: %s", m.UpstreamModel, m.State)
		}
	}
	if got := countRows(t, pool, `SELECT count(*) FROM models`); got != before {
		t.Errorf("discovery must not create models: %d -> %d", before, got)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM model_routes`); got != beforeRoutes {
		t.Errorf("discovery must not create routes: %d -> %d", beforeRoutes, got)
	}
}

// Cursor pagination has to be followed to the end. Anthropic's GET /v1/models
// pages with has_more and last_id, so reading only the first page reports every
// model from page two onward as "upstream does not have it" -- the opposite of
// what this feature is for, and silently so: the upstream says has_more=true
// and the endpoint answers Ok, one page, no message at all.
func TestDiscoverFollowsPagination(t *testing.T) {
	s, _, _ := newServer(t)

	var gotCursors []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCursors = append(gotCursors, r.URL.Query().Get("after_id"))
		switch r.URL.Query().Get("after_id") {
		case "":
			_, _ = w.Write([]byte(
				`{"data":[{"id":"p1-a"},{"id":"p1-b"}],"has_more":true,"last_id":"p1-b"}`))
		case "p1-b":
			_, _ = w.Write([]byte(
				`{"data":[{"id":"p2-a"},{"id":"p2-b"}],"has_more":true,"last_id":"p2-b"}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"p3-a"}],"has_more":false}`))
		}
	}))
	defer up.Close()
	prov := mustProvider(t, s, "anthropic", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	res := mustDiscover(t, s, prov)
	if !res.Ok {
		t.Fatalf("it should report success: %+v", res.Message)
	}
	// The first request must hit the plain endpoint with no query parameters:
	// supporting pagination must not change what an OpenAI-protocol provider
	// sees on the wire.
	if len(gotCursors) != 3 || gotCursors[0] != "" {
		t.Fatalf("it should walk three pages with no cursor on the first, got %v", gotCursors)
	}
	if len(res.Models) != 5 {
		t.Errorf("all five models should arrive, got %d", len(res.Models))
	}
	// Followed to the end means complete; there should be no partial notice.
	if res.Message != nil {
		t.Errorf("having followed every page it must not report incomplete: %q", *res.Message)
	}
	// The positive control for complete: without it, an implementation that
	// always returns false would satisfy the stalled-cursor case below.
	if !res.Complete {
		t.Error("every page was followed, so complete should be true")
	}
}

// The upstream says there is more but the cursor does not advance: stop, and
// say so. Silent truncation is the one unacceptable outcome here -- "upstream
// only has these" and "these are all I saw" are completely different
// conclusions, and reading the second as the first makes the missing models
// look unsupported.
func TestDiscoverReportsIncompleteWhenCursorStalls(t *testing.T) {
	s, _, _ := newServer(t)

	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		// Always has_more, always the same page: a broken upstream or a
		// compatibility layer that got the protocol wrong.
		_, _ = w.Write([]byte(`{"data":[{"id":"stuck"}],"has_more":true,"last_id":"stuck"}`))
	}))
	defer up.Close()
	prov := mustProvider(t, s, "anthropic", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	res := mustDiscover(t, s, prov)
	if !res.Ok || len(res.Models) != 1 {
		t.Fatalf("what was read so far is still a valid verdict: %+v", res)
	}
	if res.Message == nil {
		t.Fatal("it must be marked incomplete -- a silent truncation makes the operator read a missing model as unsupported upstream")
	}
	if !strings.Contains(*res.Message, "more models") {
		t.Errorf("the note should say the upstream has more: %q", *res.Message)
	}
	// This one line is the entire value of the complete field: ok is still
	// true, because what was read is valid, but the enumeration is partial, so
	// nothing in the UI may conclude "upstream no longer offers this". A
	// client that looks only at ok mislabels every route it did not see.
	if res.Complete {
		t.Error("the enumeration was truncated, so complete must be false -- otherwise the frontend marks models gone from half a reading")
	}
	// Paging must not run forever: a cursor that does not advance stops it
	// immediately rather than grinding on until the timeout.
	if hits > 2 {
		t.Errorf("a cursor that does not advance should stop immediately, but it called %d times", hits)
	}
}

// An upstream failure is the result of the probe, not a fault in the endpoint:
// always 200 with the verdict in the body, for the same reason the connectivity
// probe does it -- so no client has to tell "it ran and did not pass" from "the
// endpoint itself is broken".
func TestDiscoverUpstreamFailuresAre200(t *testing.T) {
	s, _, _ := newServer(t)

	var status int
	var payload string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()
	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	cases := []struct {
		name      string
		status    int
		body      string
		wantInMsg string
	}{
		{"invalid credentials", http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, "bad key"},
		{"endpoint does not exist", http.StatusNotFound, `not found`, "404"},
		{"wrong response shape", http.StatusOK, `["gpt-4o"]`, "shape"},
		// The empty-catalog case lives in its own test, because a 200 with
		// data:[] is successfully enumerating zero entries rather than a
		// failure. It was moved out rather than flipped in place: a behaviour
		// change deserves its own test, and leaving it in a table of failures
		// would tell the next reader it is still one. Note that
		// `{"data":[{"id":""}]}` -- entries present, ids all blank -- remains
		// a failure, because that response is malformed.
		{"every id is an empty string", http.StatusOK, `{"data":[{"id":""},{"id":"  "}]}`, "non-empty id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, payload = c.status, c.body
			res := mustDiscover(t, s, prov) // mustDiscover asserts HTTP 200 itself
			if res.Ok {
				t.Errorf("it must not report success: %+v", res)
			}
			// With no verdict at all, the complete bit is meaningless and must
			// be false -- otherwise a client marks routes gone from an empty
			// reading.
			if res.Complete {
				t.Errorf("complete must be false when ok=false: %+v", res)
			}
			got := ""
			if res.Message != nil {
				got = *res.Message
			}
			if !strings.Contains(got, c.wantInMsg) {
				t.Errorf("the failure note should contain %q: %q", c.wantInMsg, got)
			}
		})
	}
}

// The upstream answers normally with no models at all: that is successfully
// enumerating zero entries.
//
// Classifying it as a failure leaves the client with no upstream names, so
// nothing gets flagged as "configured here, no longer offered upstream" -- and
// this is precisely the case that most needs raising, because it is what a
// fully withdrawn upstream looks like, with every local route now pointing at
// nothing.
func TestDiscoverEmptyCatalogIsASuccessfulEnumeration(t *testing.T) {
	s, _, _ := newServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer up.Close()
	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	res := mustDiscover(t, s, prov)

	// All four assertions are needed: asserting only Ok would also pass for
	// "ok but incomplete", and in that state a client still must not mark
	// anything gone.
	if !res.Ok {
		t.Errorf("a normal upstream answer is a verdict, so it should be a success: %+v", res)
	}
	if !res.Complete {
		t.Errorf("every page was followed (one page, no has_more), so it should be complete: %+v", res)
	}
	if len(res.Models) != 0 {
		t.Errorf("the upstream reported no models, so models should be empty: %+v", res.Models)
	}
	// Success still needs something to say, or the screen is simply blank.
	if res.Message == nil || !strings.Contains(*res.Message, "no models at all") {
		t.Errorf("the note should say the upstream catalog is empty: %v", res.Message)
	}
}

// A provider with no credential yet is a normal intermediate configuration
// state, so this returns a verdict rather than an error.
func TestDiscoverWithoutKey(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProvider(t, s, "openai", "https://api.example.com")
	res := mustDiscover(t, s, prov)
	if res.Ok {
		t.Error("with no key it must not report success")
	}
	if res.Message == nil || !strings.Contains(*res.Message, "credential") {
		t.Errorf("the note should say the key is missing: %v", res.Message)
	}
}

// ===== Fixtures =====

func mustDiscover(t *testing.T, s *gwstaffapi.Server, prov uuid.UUID) gwstaffapi.DiscoverModelsResult {
	t.Helper()
	res, err := s.DiscoverProviderModels(context.Background(),
		gwstaffapi.DiscoverProviderModelsRequestObject{ProviderId: prov})
	if err != nil {
		t.Fatalf("the discovery endpoint itself must not fail (an upstream failure is a verdict in the body): %v", err)
	}
	return gwstaffapi.DiscoverModelsResult(res.(gwstaffapi.DiscoverProviderModels200JSONResponse))
}

// mustPricedModel creates a priced model: the model through the API, then one
// price row.
//
// Pricing has exactly one storage location: the model's current price row.
func mustPricedModel(t *testing.T, s *gwstaffapi.Server, pool *pgxpool.Pool, slug string) gwstaffapi.GatewayModel {
	t.Helper()
	model := mustModel(t, s, slug)
	mustPrice(t, pool, model.Id, "paid")
	return model
}

// mustPrice writes one current price row for a model.
//
// Every field here is forced to be right by the table's own CHECK, which
// requires all four rates and, for a paid model, that they are not all zero. It
// is not possible to write a shape that sidesteps the rule, which is the whole
// point of doing it this way.
func mustPrice(t *testing.T, pool *pgxpool.Pool, modelID uuid.UUID, mode string) {
	t.Helper()
	// All four rates at zero is legitimate for a free model -- the constraint
	// permits zeros there. "Deliberately free" and "no price" are told apart by
	// billing_mode, not by the size of the numbers.
	price := int64(3_000_000_000)
	if mode == "free" {
		price = 0
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO model_pricing (
    model_id, billing_mode,
    upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
    upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
    source_name, verified_at, provenance
) VALUES ($1, $2::text, $3::bigint, $3::bigint, $3::bigint, $3::bigint,
          'test-fixture', now(), '{"maintenance":"manual"}'::jsonb)`,
		modelID, mode, price)
	if err != nil {
		t.Fatalf("write price (%s): %v", mode, err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The value range of "priced": which shapes count as having a price.
//
// With only the four-states test, hardcoding priced to true would still be
// green. The two edges that remain: free is a kind of pricing rather than a
// missing price -- billing_mode is what tells them apart -- and only the
// absence of a price row is unpriced.
func TestDiscoverPricedMatchesPricingRow(t *testing.T) {
	cases := []struct {
		name      string
		seed      func(t *testing.T, pool *pgxpool.Pool, modelID uuid.UUID)
		wantState string
	}{
		{"paid", func(t *testing.T, p *pgxpool.Pool, id uuid.UUID) {
			mustPrice(t, p, id, "paid")
		}, "mappable"},
		// Free is a kind of pricing, not a missing price: billing_mode is the
		// distinction, not the magnitude of the numbers.
		{"free", func(t *testing.T, p *pgxpool.Pool, id uuid.UUID) {
			mustPrice(t, p, id, "free")
		}, "mappable"},
		{"no price row at all", func(_ *testing.T, _ *pgxpool.Pool, _ uuid.UUID) {}, "unpriced"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A fresh database per case: model_pricing is keyed by model_id,
			// so two shapes cannot coexist in one.
			s, pool, _ := newServer(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"probe-model"}]}`))
			}))
			defer up.Close()
			prov := mustProvider(t, s, "openai", up.URL)
			mustCreateKey(t, s, prov, "k", "sk-x")

			model := mustModel(t, s, "openai/probe-model")
			c.seed(t, pool, model.Id)

			res := mustDiscover(t, s, prov)
			if !res.Ok || len(res.Models) != 1 {
				t.Fatalf("exactly one model should be fetched: %+v", res)
			}
			if got := string(res.Models[0].State); got != c.wantState {
				t.Errorf("want %s, got %q", c.wantState, got)
			}
		})
	}
}

// Reconciling two copies of the same rule: discovery reports mappable if and
// only if that model can be enabled once it also has a route.
//
// "Is it priced" is written twice in SQL -- once in the classification query,
// once in the enable gate. Not extracting a shared view has a cost, and this
// test is it: when it goes red, somebody changed one side only.
//
// The order matters. Discovery runs first, while there is still no route, so
// the state priority lands on priced and reports mappable. Creating the route
// first would make discovery answer routed instead, hiding what is being
// checked.
func TestDiscoverPricedAgreesWithEnableGate(t *testing.T) {
	cases := []struct {
		name       string
		seed       func(t *testing.T, pool *pgxpool.Pool, modelID uuid.UUID)
		wantPriced bool
	}{
		{"paid", func(t *testing.T, p *pgxpool.Pool, id uuid.UUID) {
			mustPrice(t, p, id, "paid")
		}, true},
		{"free", func(t *testing.T, p *pgxpool.Pool, id uuid.UUID) {
			mustPrice(t, p, id, "free")
		}, true},
		{"no price row at all", func(_ *testing.T, _ *pgxpool.Pool, _ uuid.UUID) {}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, pool, _ := newServer(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"agree-model"}]}`))
			}))
			defer up.Close()
			prov := mustProvider(t, s, "openai", up.URL)
			mustCreateKey(t, s, prov, "k", "sk-x")

			model := mustModel(t, s, "openai/agree-model")
			c.seed(t, pool, model.Id)

			// What discovery says.
			res := mustDiscover(t, s, prov)
			if !res.Ok || len(res.Models) != 1 {
				t.Fatalf("exactly one model should be fetched: %+v", res)
			}
			discoverSaysPriced := res.Models[0].State == "mappable"

			// Satisfy the other precondition (a usable provider), then ask the
			// enable gate.
			mustRoute(t, s, model.Id, prov, "agree-model", []string{"chat"})
			enabled := true
			_, enableErr := s.UpdateGatewayModel(context.Background(),
				gwstaffapi.UpdateGatewayModelRequestObject{
					ModelId: model.Id,
					Body:    &gwstaffapi.GatewayModelInput{Enabled: &enabled},
				})
			gateSaysPriced := enableErr == nil

			if discoverSaysPriced != gateSaysPriced {
				t.Errorf("the two criteria disagree: discover says priced=%v, the enablement gate says priced=%v (%v)"+
					" -- the two SQL statements have drifted apart",
					discoverSaysPriced, gateSaysPriced, enableErr)
			}
			if discoverSaysPriced != c.wantPriced {
				t.Errorf("want priced=%v, got %v", c.wantPriced, discoverSaysPriced)
			}
		})
	}
}

// Two hosted platforms publish no catalogue at all, and the registry knows
// which. Asking them anyway returns either a failed fetch or -- worse, where
// the address answers with something -- an empty list, which renders as "this
// provider serves no models": a conclusion rather than an error, so nothing
// sends anyone looking.
func TestDiscoverSaysSoWhenTheVendorHasNoCatalogue(t *testing.T) {
	s, _, _ := newServer(t)

	called := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer up.Close()

	prov := mustProviderForVendor(t, s, "aws-bedrock", []string{"anthropic"}, up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	res := mustDiscover(t, s, prov)
	if res.Ok {
		t.Error("there is nothing to discover here, so this is not a successful fetch")
	}
	if res.Message == nil || !strings.Contains(*res.Message, "no model catalogue") {
		t.Fatalf("the answer should say the vendor publishes no catalogue, got %v", res.Message)
	}
	if !strings.Contains(*res.Message, "by hand") {
		t.Errorf("it should say what to do instead, got %q", *res.Message)
	}
	if called {
		t.Error("no request should go out: the answer does not depend on the upstream")
	}
}

// The Gemini protocol keeps its catalogue at its own address, in its own shape,
// behind its own cursor. None of the three is derivable from the other
// protocols, and getting any of them wrong produces an empty list — which reads
// as "this upstream serves no models" rather than as an error.
func TestDiscoverReadsTheGeminiCatalogue(t *testing.T) {
	s, pool, _ := newServer(t)

	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-flash"}],"nextPageToken":"tok"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro"}]}`))
	}))
	defer up.Close()

	prov := mustProviderForVendor(t, s, "google", []string{"gemini"}, up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")
	// A local model for one of them, so the classification is exercised rather
	// than only the fetch.
	mustPricedModel(t, s, pool, "google/gemini-2.5-flash")

	res := mustDiscover(t, s, prov)
	if !res.Ok {
		t.Fatalf("the fetch should succeed: %v", res.Message)
	}
	if len(paths) != 2 {
		t.Fatalf("the cursor should have been followed once: %v", paths)
	}
	if !strings.HasPrefix(paths[0], "/v1beta/models") {
		t.Errorf("the catalogue was requested at %q, not this protocol's own address", paths[0])
	}
	if !strings.Contains(paths[1], "pageToken=tok") {
		t.Errorf("the second page did not carry the opaque cursor: %q", paths[1])
	}

	got := map[string]string{}
	for _, m := range res.Models {
		got[m.UpstreamModel] = string(m.State)
	}
	// The prefix is stripped: a route names the model the way a request does,
	// and "models/gemini-2.5-flash" would match nothing local.
	if got["gemini-2.5-flash"] != "mappable" {
		t.Errorf("gemini-2.5-flash classified as %q, want mappable: %v", got["gemini-2.5-flash"], got)
	}
	if got["gemini-2.5-pro"] != "unknown" {
		t.Errorf("gemini-2.5-pro classified as %q, want unknown", got["gemini-2.5-pro"])
	}
}
