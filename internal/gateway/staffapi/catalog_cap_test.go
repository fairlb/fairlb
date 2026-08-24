package gwstaffapi_test

import (
	"context"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
	"github.com/google/uuid"
)

// The catalog list is capped, and the kill-switch counts do not come from the
// capped page.
//
// The model catalog is exempt from pagination because five different pickers
// reuse this list and paging it would break them; the exemption is paid for
// with a server-side cap instead.
//
// Adding that cap immediately creates a second problem. The "N total, M
// disabled" on the health page used to be counted client-side from this same
// list. Once the list is capped, what it counts becomes "how many are disabled
// on the first page" -- silently wrong, with no outward sign. So the two belong
// in one change, proved by one test: the list is truncated and the counts still
// state the real totals.
func TestModelCatalogCapsAndSwitchCountsStaySourceTruthful(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	// Past the cap of 500: 501 rows, 7 of them disabled. Inserting one at a
	// time is too slow, so generate_series loads them in one statement.
	const total, disabled = 501, 7
	if _, err := pool.Exec(ctx, `
		INSERT INTO models (slug, display_name, enabled, visibility)
		SELECT 'capped/m' || lpad(i::text, 4, '0'), 'M' || i,
		       i > $2,            -- the first $2 are disabled, the rest enabled
		       'public'
		FROM generate_series(1, $1) AS i`, total, disabled); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListGatewayModels(ctx, gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	rows := listed.(gwstaffapi.ListGatewayModels200JSONResponse).Items
	if len(rows) != 500 {
		t.Fatalf("the catalog should be capped server-side at 500 rows, got %d -- "+
			"without the cap this endpoint grows without bound as the upstream ships new models", len(rows))
	}

	res, err := s.GetGatewayHealth(ctx, gwstaffapi.GetGatewayHealthRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	counts := res.(gwstaffapi.GetGatewayHealth200JSONResponse).SwitchCounts
	if counts == nil {
		t.Fatal("the health dashboard carries no switch_counts: the kill-switch card falls back to 'count unavailable'")
	}

	// These two assertions are the point: the counts have to be over the whole
	// table, not over the 500 rows above. Counting the page yields 500 and 7 --
	// both plausible-looking, both wrong.
	if counts.ModelsTotal != total {
		t.Errorf("models_total = %d, want %d -- the count was taken from the capped page, "+
			"rather than from the thing it claims to count", counts.ModelsTotal, total)
	}
	if counts.ModelsDisabled != disabled {
		t.Errorf("models_disabled = %d, want %d", counts.ModelsDisabled, disabled)
	}
	// The reverse must hold too: the truncated length must never pass itself
	// off as the total.
	if counts.ModelsTotal == int64(len(rows)) {
		t.Error("models_total exactly equals the listing length -- the count is probably being derived from the listing again")
	}
}

// The provider list pages on slug, and its search runs in the database.
//
// Both halves are load-bearing and they had to land together. The list page's
// search box used to filter the 200 rows it had already fetched; once the list
// is paginated that box would only ever see the first page, and an operator
// searching for a provider past it would be told it does not exist.
//
// Slug alone is the whole key because providers.slug is UNIQUE — the assertion
// below is on the *sequence*, which is what catches a cursor built from a
// different column than the ORDER BY sorts by.
func TestProviderListPagesOnSlugAndSearchesServerSide(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (slug, vendor, protocols, base_url, name)
		VALUES ('alpha',  'custom', ARRAY['openai'], 'https://a.test',       'Alpha Co'),
		       ('bravo',  'custom', ARRAY['openai'], 'https://b.test',       'Bravo Co'),
		       ('delta',  'custom', ARRAY['openai'], 'https://api.acme.test', 'Delta Co'),
		       ('charlie','custom', ARRAY['openai'], 'https://c.test',       'Charlie Co'),
		       ('echo',   'custom', ARRAY['openai'], 'https://e.test',       'Echo Co')`); err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "bravo", "charlie", "delta", "echo"}

	limit := 2
	var got []string
	var cursor *string
	for round := 0; ; round++ {
		if round > 10 {
			t.Fatal("paging did not terminate: the cursor is not advancing")
		}
		listed, err := s.ListGatewayProviders(ctx, gwstaffapi.ListGatewayProvidersRequestObject{
			Params: gwstaffapi.ListGatewayProvidersParams{Cursor: cursor, Limit: &limit},
		})
		if err != nil {
			t.Fatalf("page %d: %v", round, err)
		}
		body := listed.(gwstaffapi.ListGatewayProviders200JSONResponse)
		if len(body.Items) > limit {
			t.Fatalf("page %d returned %d rows for a limit of %d — the probe row leaked out",
				round, len(body.Items), limit)
		}
		for _, p := range body.Items {
			got = append(got, p.Slug)
		}
		if body.NextCursor == nil {
			break
		}
		cursor = body.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("paged %d providers, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q\n full sequence: %v", i, got[i], want[i], got)
		}
	}

	// Search covers the three fields the list page's own box has always covered.
	// base_url is the one most easily dropped in a move like this, and it is the
	// one that answers "which provider points at api.acme.test".
	for _, probe := range []struct{ q, want string }{
		{"charl", "charlie"},  // slug
		{"Bravo Co", "bravo"}, // display name
		{"api.acme", "delta"}, // base URL
		{"ALPHA", "alpha"},    // case-insensitive
	} {
		listed, err := s.ListGatewayProviders(ctx, gwstaffapi.ListGatewayProvidersRequestObject{
			Params: gwstaffapi.ListGatewayProvidersParams{Q: &probe.q},
		})
		if err != nil {
			t.Fatalf("search %q: %v", probe.q, err)
		}
		items := listed.(gwstaffapi.ListGatewayProviders200JSONResponse).Items
		if len(items) != 1 || items[0].Slug != probe.want {
			names := make([]string, 0, len(items))
			for _, p := range items {
				names = append(names, p.Slug)
			}
			t.Fatalf("search %q matched %v, want exactly [%s]", probe.q, names, probe.want)
		}
	}

	// A cursor and a filter compose: the search result is itself pageable.
	q, one := "o", 1
	listed, err := s.ListGatewayProviders(ctx, gwstaffapi.ListGatewayProvidersRequestObject{
		Params: gwstaffapi.ListGatewayProvidersParams{Q: &q, Limit: &one},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := listed.(gwstaffapi.ListGatewayProviders200JSONResponse)
	if len(first.Items) != 1 || first.NextCursor == nil {
		t.Fatalf("a filtered list must page too: %d rows, next=%v", len(first.Items), first.NextCursor)
	}
	listed, err = s.ListGatewayProviders(ctx, gwstaffapi.ListGatewayProvidersRequestObject{
		Params: gwstaffapi.ListGatewayProvidersParams{Q: &q, Limit: &one, Cursor: first.NextCursor},
	})
	if err != nil {
		t.Fatal(err)
	}
	second := listed.(gwstaffapi.ListGatewayProviders200JSONResponse)
	if len(second.Items) != 1 || second.Items[0].Slug == first.Items[0].Slug {
		t.Fatalf("the second filtered page repeated the first: %v then %v",
			first.Items[0].Slug, second.Items[0].Slug)
	}
}

// The verified-key count is over the whole set, not over the page.
//
// The readiness checklist asks "has any credential verified". It used to answer
// by scanning the rows it happened to hold, which was fine while the list came
// back whole. Paginated, a verified key sitting past the first page reads as
// "none verified" — a checklist step shown incomplete while the thing it checks
// is done, and nothing on screen says why.
//
// The fixture puts the verified key *last* on purpose: with a page size of one
// it lands on the second page, so a count derived from the page would be zero.
func TestVerifiedKeyCountSpansThePagesNotThePage(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var providerID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (slug, vendor, protocols, base_url)
		VALUES ('keyed', 'custom', ARRAY['openai'], 'https://k.test') RETURNING id`).
		Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	// Three keys, oldest first; only the newest has ever verified.
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_keys (provider_id, name, secret_enc, secret_hint, created_at, last_verified_at)
		VALUES ($1, 'old',    '\x00', 'sk-…aa', now() - interval '2 hours', NULL),
		       ($1, 'middle', '\x00', 'sk-…bb', now() - interval '1 hour',  NULL),
		       ($1, 'newest', '\x00', 'sk-…cc', now(),                      now())`,
		providerID); err != nil {
		t.Fatal(err)
	}

	one := 1
	listed, err := s.ListGatewayProviderKeys(ctx, gwstaffapi.ListGatewayProviderKeysRequestObject{
		ProviderId: uuid.MustParse(providerID),
		Params:     gwstaffapi.ListGatewayProviderKeysParams{Limit: &one},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := listed.(gwstaffapi.ListGatewayProviderKeys200JSONResponse)

	name := ""
	if len(body.Items) == 1 && body.Items[0].Name != nil {
		name = *body.Items[0].Name
	}
	if name != "old" {
		t.Fatalf("first page = %+v, want exactly the oldest key — the connectivity "+
			"test picks the first row and must keep picking the same one", body.Items)
	}
	if body.NextCursor == nil {
		t.Fatal("three keys at a page size of one must offer a next page")
	}
	// The assertion this test exists for.
	if body.VerifiedCount != 1 {
		t.Fatalf("verified_count = %d, want 1 — counted over the page instead of the "+
			"whole set, so the readiness checklist reports a finished step as pending",
			body.VerifiedCount)
	}
}

// A route carries the names it is rendered with.
//
// Four screens used to resolve them by id against the two catalogues — the
// wiring dialogs on both sides, the provider's model panel, the model's provider
// panel. That was safe while both catalogues came back whole. Once providers
// paginated (ADR-0187) each lookup started missing, and every one of them had a
// fallback that says something false: an empty slug, a `—`, or the id prefix
// that is meant for a *deleted* provider.
//
// So the row carries them, and the contract marks them required — both queries
// read through inner joins, so they are always there. This case is what stops
// them being quietly dropped again: it asserts the values, not their presence.
func TestRouteCarriesTheNamesItIsRenderedWith(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var modelID, providerID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO models (slug, display_name, enabled, visibility)
		VALUES ('acme/big-model', 'Big', true, 'public') RETURNING id`).
		Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (slug, vendor, protocols, base_url)
		VALUES ('acme-eu', 'custom', ARRAY['anthropic'], 'https://eu.acme.test') RETURNING id`).
		Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, pool, modelID, providerID, "big-1", "messages")

	// Read from the model side.
	listed, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{
		ModelId: uuid.MustParse(modelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	byModel := listed.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items
	if len(byModel) != 1 {
		t.Fatalf("want one route, got %d", len(byModel))
	}
	if byModel[0].ModelSlug != "acme/big-model" || byModel[0].ProviderSlug != "acme-eu" {
		t.Fatalf("model side: model_slug=%q provider_slug=%q — a screen resolving these "+
			"by id would render a live entry as deleted",
			byModel[0].ModelSlug, byModel[0].ProviderSlug)
	}
	if len(byModel[0].ProviderProtocols) == 0 {
		t.Fatalf("model side: provider_protocols is empty — the row carries which endpoints " +
			"this route can be asked about, and the probe panel is keyed on it")
	}

	// And from the provider side: identical columns, different axis.
	fromProvider, err := s.ListGatewayProviderRoutes(ctx,
		gwstaffapi.ListGatewayProviderRoutesRequestObject{ProviderId: uuid.MustParse(providerID)})
	if err != nil {
		t.Fatal(err)
	}
	byProvider := fromProvider.(gwstaffapi.ListGatewayProviderRoutes200JSONResponse).Items
	if len(byProvider) != 1 || byProvider[0].ModelSlug != "acme/big-model" {
		t.Fatalf("provider side: %+v — the two axes must agree on what the row says", byProvider)
	}
}
