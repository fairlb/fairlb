package gwconsoleapi_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// The organization catalog in the console must show the final unit price as this
// organization sees it, through the same chain as the dataplane's model listing.
//
// Before the fix it read price columns off the model row and applied no plan
// multiplier at all. Nothing had written those columns for a long time -- model
// creation hard-coded zero -- so a model priced through the normal flow showed
// all four prices as zero in the console catalog. Same mechanism as a defect in
// the model discovery classifier, one page over: a value has as many places to
// be read wrong as it has places to be stored.
//
// Both catalogs are specified to read the same pricing chain, so this was a
// contradiction with the design rather than a missing nicety.
func TestAvailableModelsUsesPublishedPriceAndPlanMultiplier(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A model priced through the normal flow: the price columns on the model
	// row are all zero, as they are for anything newly created, and the price
	// lives only in the current pricing row. List price for input is
	// 10 USD per million tokens.
	const officialIn = int64(10_000_000_000) // 10 USD/M in nano
	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO models (slug, display_name, context_window, max_output_tokens,
		                    visibility, enabled)
		VALUES ('openai/priced','Priced',128000,4096,'public',true)
		RETURNING id`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	// One enabled route on an enabled provider -- the catalog only lists models
	// somebody can actually serve.
	var provID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO providers (slug, vendor, protocols, name, base_url, enabled)
		VALUES ('p-priced', 'custom', ARRAY['openai'], 'P', 'https://u.test', true) RETURNING id`).
		Scan(&provID); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, f.pool, modelID, provID, "priced", "chat")
	// The current price: list price 10 USD per million tokens, model
	// multiplier 10000, i.e. no markup.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (
			model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			multiplier_bps, source_name, verified_at, provenance)
		VALUES ($1, 'paid', $2, $2, 0, 0, 10000, 'manual', now(),
		        '{"maintenance":"manual"}'::jsonb)`,
		modelID, officialIn); err != nil {
		t.Fatal(err)
	}
	// A customer pricing plan at 80% of list, with this organization assigned to it.
	// The console catalog once did not read this layer at all -- and it is
	// exactly where "group A at list price, group B at a discount" lives.
	// A default plan already exists from the migrations, so this creates a
	// non-default plan plus an explicit assignment.
	var planID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO pricing_plans (slug, name, is_default, status, default_multiplier_bps)
		VALUES ('b-group','Group B', false, 'active', 8000) RETURNING id`).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO org_pricing_plan_assignments (org_id, pricing_plan_id, reason)
		VALUES ($1, $2, 'test')`, f.orgA, planID); err != nil {
		t.Fatal(err)
	}

	s := newConsoleServer(f.pool, allowAll{})
	res, err := s.ListAvailableModels(ctx,
		gwconsoleapi.ListAvailableModelsRequestObject{OrgId: orgParam(f.orgA)})
	if err != nil {
		t.Fatal(err)
	}
	body := res.(gwconsoleapi.ListAvailableModels200JSONResponse)
	var got *gwconsoleapi.AvailableModel
	for i := range body.Body.Items {
		if body.Body.Items[i].Slug == "openai/priced" {
			got = &body.Body.Items[i]
		}
	}
	if got == nil {
		t.Fatalf("openai/priced should appear in the catalog: %+v", body.Body.Items)
	}
	if got.PriceInNanoPerMtok == nil {
		t.Fatal("all four unit prices must reach the organization surface")
	}
	// List price 10 x model multiplier 1.0 x plan multiplier 0.8 = 8 USD per
	// million tokens.
	// Before the fix this was 0, because it read a column nothing writes -- and
	// 0 is visually identical to "free" in the UI.
	const wantIn = int64(8_000_000_000)
	if *got.PriceInNanoPerMtok != wantIn {
		t.Errorf("the input unit price should be %d (list price x model multiplier x group discount), got %d",
			wantIn, *got.PriceInNanoPerMtok)
	}
	if *got.PriceOutNanoPerMtok != wantIn {
		t.Errorf("the output unit price should be %d, got %d", wantIn, *got.PriceOutNanoPerMtok)
	}
}
