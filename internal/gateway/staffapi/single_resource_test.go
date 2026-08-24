package gwstaffapi_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// Detail views read a single row.
//
// Fetching the whole list and picking the entry out client-side breaks once
// those endpoints are capped: open a record past the cap and the provider page
// reports "provider not found" -- an outage that does not exist -- while the
// model page spins forever, because it has no not-found branch at all.
//
// The point of a single-row endpoint is not saving bandwidth; it is that the
// detail view's correctness no longer depends on where a record sorts. Hence
// the two properties asserted here:
//
//  1. The object returned for one id is field-for-field equal to the entry with
//     that id in the list. The moment the two diverge, the detail view stops
//     rendering something, and no gate can see that.
//  2. An unknown id returns a not-found error rather than an empty object or a
//     200.
//
// The first uses reflect.DeepEqual rather than comparing field by field:
// spelling the fields out would hardcode "which fields count" in a third place,
// and it would stay green when a field is added.

func TestSingleProviderReadMatchesTheListEntry(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (slug, vendor, protocols, base_url, name, cost_multiplier_bps)
		VALUES ('single-read/p1', 'custom', ARRAY['openai'], 'https://u.test/', 'P1', 10500)
		RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// The count columns need something to count, or "both sides return 0"
	// would satisfy the equality assertion.
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_keys (provider_id, name, secret_enc, secret_hint)
		VALUES ($1, 'k1', '\x00'::bytea, 'sk-…1234')`, id); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListGatewayProviders(ctx, gwstaffapi.ListGatewayProvidersRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	var fromList *gwstaffapi.GatewayProvider
	for _, p := range listed.(gwstaffapi.ListGatewayProviders200JSONResponse).Items {
		if p.Id == id {
			entry := p
			fromList = &entry
		}
	}
	if fromList == nil {
		t.Fatal("the provider just created is not in the listing -- this case's premise does not hold")
	}
	if fromList.KeyCount == nil || *fromList.KeyCount != 1 {
		t.Fatal("the listing row's key_count is not 1: the count column is not actually exercised, so the first criterion degrades to comparing nothing with nothing")
	}

	got, err := s.GetGatewayProvider(ctx, gwstaffapi.GetGatewayProviderRequestObject{
		ProviderId: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	single := gwstaffapi.GatewayProvider(got.(gwstaffapi.GetGatewayProvider200JSONResponse))
	if !reflect.DeepEqual(single, *fromList) {
		t.Errorf("the single resource and the listing row disagree:\nsingle = %+v\nlisting = %+v\n"+
			"the detail page renders from this, so a divergence means the detail page is quietly missing something", single, *fromList)
	}
}

func TestSingleModelReadMatchesTheListEntry(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO models (slug, display_name, enabled, visibility,
		                    context_window, max_output_tokens)
		VALUES ('single-read/m1', 'M1', true, 'public', 128000, 4096)
		RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListGatewayModels(ctx, gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	var fromList *gwstaffapi.GatewayModel
	for _, m := range listed.(gwstaffapi.ListGatewayModels200JSONResponse).Items {
		if m.Id == id {
			entry := m
			fromList = &entry
		}
	}
	if fromList == nil {
		t.Fatal("the model just created is not in the listing -- this case's premise does not hold")
	}
	// Pricing enrichment is the part of modelOut most likely to diverge
	// between the two paths, so confirm the list side really produced it.
	if fromList.PricingStatus == nil {
		t.Fatal("the listing row has no pricing_status: the pricing enrichment did not run, so the first criterion does not cover it")
	}

	got, err := s.GetGatewayModel(ctx, gwstaffapi.GetGatewayModelRequestObject{ModelId: id})
	if err != nil {
		t.Fatal(err)
	}
	single := gwstaffapi.GatewayModel(got.(gwstaffapi.GetGatewayModel200JSONResponse))
	if !reflect.DeepEqual(single, *fromList) {
		t.Errorf("the single resource and the listing row disagree:\nsingle = %+v\nlisting = %+v", single, *fromList)
	}
}

// A disabled model must still be readable: the operator page has to open it in
// order to enable it again. The data-plane lookup requires enabled, so reusing
// it as the detail query would make disabling a model lose access to it.
func TestSingleModelReadReturnsDisabledModels(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO models (slug, display_name, enabled, visibility)
		VALUES ('single-read/off', 'Off', false, 'public')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetGatewayModel(ctx, gwstaffapi.GetGatewayModelRequestObject{ModelId: id})
	if err != nil {
		t.Fatalf("a disabled model can no longer be read: %v -- one disable would then lock this page away for good", err)
	}
	if gwstaffapi.GatewayModel(got.(gwstaffapi.GetGatewayModel200JSONResponse)).Enabled {
		t.Error("the enabled read back should be false")
	}
}

func TestSingleResourceReadsRejectUnknownIds(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	missing := uuid.MustParse("00000000-0000-7000-8000-0000000000ff")

	if _, err := s.GetGatewayProvider(ctx, gwstaffapi.GetGatewayProviderRequestObject{
		ProviderId: missing,
	}); err == nil {
		t.Error("a nonexistent provider id should error -- a 200 hands the detail page an empty shell to render")
	}
	if _, err := s.GetGatewayModel(ctx, gwstaffapi.GetGatewayModelRequestObject{
		ModelId: missing,
	}); err == nil {
		t.Error("a nonexistent model id should error")
	}
}
