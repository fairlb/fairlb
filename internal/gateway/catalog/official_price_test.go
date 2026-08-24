package catalog_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// official_price is the competitive anchor: a paid model publishes the
// upstream's own rate so a client can show "official $3, here $2.55", while a
// free model omits it so the retained official rate is not disclosed. Both
// catalog endpoints share WriteModelList, so testing one covers both.
func TestCatalogRendersOfficialPriceAnchor(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.svc.WriteModelList(context.Background(), rec, []catalog.PublicModel{
		{
			Slug: "openai/paid-anchor", Protocols: []string{"openai"}, Currency: "USD",
			PriceIn: 3_000_000_000, PriceOut: 15_000_000_000,
			ModelMultiplierBps: 8500, // sold at 85% of the official rate
		},
		{
			Slug: "openai/free-anchor", Protocols: []string{"openai"}, Currency: "USD", IsFree: true,
			PriceIn: 9_000_000_000, // official rate retained, never disclosed
		},
	}, catalog.Rates{FXRate: "1"})

	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Input    string `json:"input_per_mtok"`
				Official *struct {
					Input  string `json:"input_per_mtok"`
					Output string `json:"output_per_mtok"`
				} `json:"official_price"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Data) != 2 {
		t.Fatalf("malformed catalog response: %s err=%v", rec.Body.String(), err)
	}
	paid := body.Data[0]
	if paid.Pricing.Official == nil || paid.Pricing.Official.Input != "3" ||
		paid.Pricing.Official.Output != "15" {
		t.Fatalf("a paid model should expose the official anchor price: %+v", paid.Pricing)
	}
	// 2.55 sold against 3 official: both numbers the comparison needs are
	// present.
	if paid.Pricing.Input != "2.55" {
		t.Fatalf("a 0.85 multiplier should sell at 2.55: %s", paid.Pricing.Input)
	}
	free := body.Data[1]
	if free.Pricing.Official != nil {
		t.Fatalf("a free model must not leak the retained official price: %+v", free.Pricing.Official)
	}
}
