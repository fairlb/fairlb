package gwstaffapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

func mustStoredDiscovery(
	t *testing.T, s *gwstaffapi.Server, prov [16]byte,
) gwstaffapi.DiscoverModelsResult {
	t.Helper()
	res, err := s.GetProviderDiscoveredModels(context.Background(),
		gwstaffapi.GetProviderDiscoveredModelsRequestObject{ProviderId: prov})
	if err != nil {
		t.Fatalf("read the stored catalogue: %v", err)
	}
	return gwstaffapi.DiscoverModelsResult(
		res.(gwstaffapi.GetProviderDiscoveredModels200JSONResponse))
}

// Asking an upstream what it serves costs real money. The answer used to live
// only in the state of the screen showing it, so reloading the page meant
// paying again to learn the same thing.
func TestTheDiscoveredCatalogueSurvivesTheScreenThatFetchedIt(t *testing.T) {
	s, pool, _ := newServer(t)

	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4"},{"id":"gpt-5.6-sol"}]}`))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")

	// Never asked is not the same fact as asked-and-empty, so it must not read
	// like a successful fetch that found nothing.
	before := mustStoredDiscovery(t, s, prov)
	if before.Ok {
		t.Error("a provider nobody has asked must not report a successful fetch")
	}
	if !before.CheckedAt.IsZero() {
		t.Errorf("nothing has been checked, so checked_at should be zero: %v", before.CheckedAt)
	}

	live := mustDiscover(t, s, prov)
	if !live.Ok || len(live.Models) != 2 {
		t.Fatalf("the fetch should have found two models: %+v", live)
	}

	stored := mustStoredDiscovery(t, s, prov)
	if calls != 1 {
		t.Errorf("reading the stored answer must not call upstream again: %d calls", calls)
	}
	if !stored.Ok || len(stored.Models) != 2 {
		t.Fatalf("the stored answer should hold both models: %+v", stored)
	}
	if stored.CheckedAt.IsZero() {
		t.Error("the stored answer must say when it was obtained; without that it is not readable")
	}
	for i, m := range stored.Models {
		if m.UpstreamModel != live.Models[i].UpstreamModel {
			t.Errorf("model %d: stored %q, fetched %q", i, m.UpstreamModel, live.Models[i].UpstreamModel)
		}
	}

	// The classification is recomputed on read, not stored: what exists locally
	// changes without the upstream saying anything, so a stored verdict would
	// go wrong on its own.
	if stored.Models[0].State != "unknown" {
		t.Fatalf("nothing is in the catalog yet, so this should be unknown: %+v", stored.Models[0])
	}
	mustPricedModel(t, s, pool, "openai/gpt-5.4")
	again := mustStoredDiscovery(t, s, prov)
	if again.Models[0].State != "mappable" {
		t.Errorf("after the model was created the same stored answer should reclassify "+
			"it as mappable, got %q", again.Models[0].State)
	}
}

// A new address or platform means the stored catalogue is an answer from
// somewhere else -- worse than no answer, because it still reads as one.
func TestTheStoredCatalogueIsForgottenWhenTheUpstreamMoves(t *testing.T) {
	s, _, _ := newServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4"}]}`))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")
	if res := mustDiscover(t, s, prov); !res.Ok {
		t.Fatalf("the fetch should succeed: %+v", res.Message)
	}
	if got := mustStoredDiscovery(t, s, prov); len(got.Models) != 1 {
		t.Fatalf("the answer should be stored first: %+v", got)
	}

	moved := "https://api.example.invalid"
	if _, err := s.UpdateGatewayProvider(context.Background(),
		gwstaffapi.UpdateGatewayProviderRequestObject{
			ProviderId: prov, Body: &gwstaffapi.GatewayProviderInput{BaseUrl: &moved},
		}); err != nil {
		t.Fatalf("move the provider: %v", err)
	}

	after := mustStoredDiscovery(t, s, prov)
	if after.Ok || len(after.Models) != 0 {
		t.Errorf("the catalogue of the previous address must not be reported for the "+
			"new one: %+v", after)
	}

	// A change that leaves the record pointing at the same upstream keeps it.
	name := "renamed"
	if _, err := s.UpdateGatewayProvider(context.Background(),
		gwstaffapi.UpdateGatewayProviderRequestObject{
			ProviderId: prov, Body: &gwstaffapi.GatewayProviderInput{Name: &name},
		}); err != nil {
		t.Fatalf("rename the provider: %v", err)
	}
	if _, err := s.DiscoverProviderModels(context.Background(),
		gwstaffapi.DiscoverProviderModelsRequestObject{ProviderId: prov}); err != nil {
		t.Fatal(err)
	}
}

// An unknown row carries what it would create, and says how much is known
// behind it.
func TestDiscoveryOffersASeededEntryForAModelItRecognises(t *testing.T) {
	s, _, _ := newServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-5.6-sol"},{"id":"some-fine-tune"}]}`))
	}))
	defer up.Close()

	// The vendor is what this test is about: the creator segment comes from the
	// registry entry, and a custom upstream deliberately has none.
	prov := mustProviderForVendor(t, s, "openai", []string{"openai"}, up.URL)
	mustCreateKey(t, s, prov, "k", "sk-x")
	res := mustDiscover(t, s, prov)

	byName := map[string]gwstaffapi.DiscoveredModel{}
	for _, m := range res.Models {
		byName[m.UpstreamModel] = m
	}

	// Seeded: the slug, the display name and both windows come from the
	// vendor's own documentation, so the row is complete on arrival.
	seeded := byName["gpt-5.6-sol"].Suggestion
	if seeded == nil {
		t.Fatal("a seeded model should arrive with the entry it would create")
	}
	if seeded.Slug != "openai/gpt-5.6-sol" || seeded.Source != "seed" {
		t.Errorf("got %q from %q, want openai/gpt-5.6-sol from seed", seeded.Slug, seeded.Source)
	}
	if seeded.DisplayName == nil || *seeded.DisplayName == "" {
		t.Error("a seeded entry knows its display name")
	}
	if seeded.ContextWindow == nil || *seeded.ContextWindow == 0 {
		t.Error("a seeded entry knows its context window; a zero one degrades the estimate")
	}

	// Not seeded: correctly shaped, and honest that this is all it is.
	guessed := byName["some-fine-tune"].Suggestion
	if guessed == nil {
		t.Fatal("a first-party vendor can still supply the creator segment")
	}
	if guessed.Slug != "openai/some-fine-tune" || guessed.Source != "vendor" {
		t.Errorf("got %q from %q, want openai/some-fine-tune from vendor",
			guessed.Slug, guessed.Source)
	}
	if guessed.DisplayName != nil && *guessed.DisplayName != "" {
		t.Errorf("nothing here knows this model's display name, so it must not "+
			"present one: %q", *guessed.DisplayName)
	}
}

// A custom upstream answers for nothing, and says so by offering nothing.
//
// Its model names are whatever the relay operator chose, so a prefix put in
// front of one would be invention dressed as knowledge -- for a slug that can
// never be changed afterwards.
func TestDiscoveryOffersNothingForAnUpstreamThatCannotNameItsCreator(t *testing.T) {
	s, _, _ := newServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.6-sol"}]}`))
	}))
	defer up.Close()

	prov := mustProvider(t, s, "openai", up.URL) // a custom vendor
	mustCreateKey(t, s, prov, "k", "sk-x")
	res := mustDiscover(t, s, prov)

	if len(res.Models) != 1 {
		t.Fatalf("expected the one model: %+v", res.Models)
	}
	if sg := res.Models[0].Suggestion; sg != nil {
		t.Errorf("a custom upstream cannot say who made its models, so nothing "+
			"should be offered; got %q from %q", sg.Slug, sg.Source)
	}
}
