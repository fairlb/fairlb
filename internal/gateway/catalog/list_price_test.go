package catalog_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// renderInputPrice runs the catalog rendering the two endpoints share and
// returns the first model's input rate and the plan id it disclosed.
func renderInputPrice(t *testing.T, f *fixture, models []catalog.PublicModel) (string, string) {
	t.Helper()
	if len(models) != 1 {
		t.Fatalf("expected exactly one catalog entry, got %d", len(models))
	}
	rec := httptest.NewRecorder()
	f.svc.WriteModelList(context.Background(), rec, models, catalog.Rates{FXRate: "1"})
	var body struct {
		Data []struct {
			Pricing struct {
				Input    string `json:"input_per_mtok"`
				Official *struct {
					Input      string `json:"input_per_mtok"`
					SourceName string `json:"source_name"`
					VerifiedAt string `json:"verified_at"`
				} `json:"official_price"`
			} `json:"pricing"`
			Meta struct {
				PricingPlanID string `json:"pricing_plan_id"`
			} `json:"fairlb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("malformed catalog response: %s err=%v", rec.Body.String(), err)
	}
	return body.Data[0].Pricing.Input, body.Data[0].Meta.PricingPlanID
}

// The anonymous catalog publishes the *list* price, and list price is defined
// as the default plan's: what a reader who registers right now will be charged.
//
// Before this was locked down the public endpoint applied no plan multiplier at
// all. That happened to agree with the default plan, whose multiplier is 1x out
// of the box, so no test could tell the two definitions apart -- and the moment
// an operator discounted or marked up the default plan they diverged silently.
// The mark-up direction is the dangerous one: the site would advertise a price
// lower than the one the gateway charges.
//
// So the criterion is not "the handler reads the default plan". It is the
// consequence: the anonymous number and the number an organization with no plan
// assignment is charged are the same number.
func TestPublicCatalogPriceEqualsWhatAnUnassignedOrgPays(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "p-list-price", "openai")
	model := f.model(t, "openai/list-price")
	f.route(t, model, provider, "list-price", []string{"chat"})
	// $10 official, sold at official (the model's own multiplier is 1x).
	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing
		   SET upstream_in_nano_per_mtok = 10000000000, multiplier_bps = 10000
		 WHERE model_id = $1`, model); err != nil {
		t.Fatal(err)
	}
	// An organization that has never been assigned a plan: the default applies.
	orgID := orgtest.Create(t, f.pool, orgtest.Seed{Slug: "list-price-org", Name: "List Price"})

	for _, tc := range []struct {
		name           string
		defaultBps     int
		wantPerMillion string
	}{
		{"untouched default plan", 10000, "10"},
		{"platform-wide discount", 9000, "9"},
		{"platform-wide mark-up", 11000, "11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.pool.Exec(ctx,
				`UPDATE pricing_plans SET default_multiplier_bps = $1 WHERE is_default`,
				tc.defaultBps); err != nil {
				t.Fatal(err)
			}

			public, err := f.svc.PublicModels(ctx)
			if err != nil {
				t.Fatal(err)
			}
			publicPrice, publicPlanID := renderInputPrice(t, f, public)

			charged, err := f.svc.ModelsForOrg(ctx, pgtype.UUID{}, orgID)
			if err != nil {
				t.Fatal(err)
			}
			chargedPrice, _ := renderInputPrice(t, f, charged)

			if publicPrice != chargedPrice {
				t.Fatalf("the published price must be the price charged: public=%s charged=%s",
					publicPrice, chargedPrice)
			}
			if publicPrice != tc.wantPerMillion {
				t.Fatalf("wrong list price: got %s, want %s", publicPrice, tc.wantPerMillion)
			}
			// The default plan prices the anonymous catalog, but there is no
			// organization here whose plan it is, so its id stays internal.
			if publicPlanID != "" {
				t.Fatalf("the anonymous catalog disclosed a pricing plan id: %s", publicPlanID)
			}
		})
	}
}

// No default plan must fail closed rather than quietly falling back to 1x.
// Publishing list price while the operator believes a discount is in force is
// the same fault on the anonymous endpoint as on the authenticated one, and
// before the plan was resolved here at all the anonymous side could not even
// notice.
//
// Disabling the default is not the way in -- pricing_plans_default_active_ck
// already rejects that, and pricing_plans_one_default_uk keeps there from being
// two. What no constraint covers is a deployment left with *none*: clearing the
// flag, or deleting the row while nothing references it.
func TestPublicCatalogFailsClosedWithoutAnActiveDefaultPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "p-no-default-plan", "openai")
	model := f.model(t, "openai/no-default-plan")
	f.route(t, model, provider, "no-default-plan", []string{"chat"})
	if _, err := f.pool.Exec(ctx,
		`UPDATE pricing_plans SET is_default = false WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublicModels(ctx); err == nil {
		t.Fatal("a catalog with no default plan must fail rather than publish list price")
	}
}

// official_price carries the evidence for the rate, not just the number: where
// it came from and whether a person confirmed it. An unverified rate is the
// bundled dataset's suggestion, and a client that cannot tell the two apart
// will present the weaker one as the stronger.
func TestOfficialPriceCarriesItsProvenance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "p-provenance", "openai")
	model := f.model(t, "openai/provenance")
	f.route(t, model, provider, "provenance", []string{"chat"})

	assertProvenance := func(t *testing.T, wantSource, wantVerified string) {
		t.Helper()
		models, err := f.svc.PublicModels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		f.svc.WriteModelList(ctx, rec, models, catalog.Rates{FXRate: "1"})
		var body struct {
			Data []struct {
				Pricing struct {
					Official *struct {
						SourceName string `json:"source_name"`
						SourceURL  string `json:"source_url"`
						VerifiedAt string `json:"verified_at"`
					} `json:"official_price"`
				} `json:"pricing"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("malformed catalog response: %s err=%v", rec.Body.String(), err)
		}
		got := body.Data[0].Pricing.Official
		if got == nil {
			t.Fatal("a paid model must publish official_price")
		}
		if got.SourceName != wantSource {
			t.Fatalf("source_name: got %q, want %q", got.SourceName, wantSource)
		}
		switch wantVerified {
		case "":
			if got.VerifiedAt != "" {
				t.Fatalf("an unconfirmed rate must not carry verified_at: %q", got.VerifiedAt)
			}
		default:
			if got.VerifiedAt == "" {
				t.Fatal("a confirmed rate must carry verified_at")
			}
		}
	}

	// The fixture's row is what an import leaves behind: a source, no
	// confirmation.
	assertProvenance(t, "test-fixture", "")

	if _, err := f.pool.Exec(ctx, `
		UPDATE model_pricing
		   SET source_name = 'OpenAI pricing page',
		       source_url = 'https://openai.com/api/pricing/',
		       verified_at = now()
		 WHERE model_id = $1`, model); err != nil {
		t.Fatal(err)
	}
	assertProvenance(t, "OpenAI pricing page", "set")
}

// The BYOK fee travels with the price list and is quoted at the same plan.
//
// Both halves matter. A page that shows "$1.00 per million, and 5% on top of
// your own key" is answering one question, so the two numbers have to describe
// the same customer: quoting the fee at list while the rates carry a discount
// would understate one of them, and which one depends on the sign.
//
// It rides on the list rather than on a second endpoint because two fetches are
// two chances to disagree about which moment they describe.
func TestBYOKFeeTravelsWithThePriceListAtTheSamePlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	provider := f.provider(t, "p-byok-fee", "openai")
	model := f.model(t, "openai/byok-fee")
	f.route(t, model, provider, "byok-fee", []string{"chat"})

	feeOf := func(t *testing.T) int64 {
		t.Helper()
		models, err := f.svc.PublicModels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		f.svc.WriteModelList(ctx, rec, models, catalog.Rates{FXRate: "1"})
		var body struct {
			Meta struct {
				BYOKFeeBps int64 `json:"byok_fee_bps"`
			} `json:"fairlb"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("malformed catalog response: %s err=%v", rec.Body.String(), err)
		}
		return body.Meta.BYOKFeeBps
	}

	if got := feeOf(t); got != 500 {
		t.Fatalf("the registered default service fee should be published: got %d", got)
	}

	// A platform-wide discount moves the rates, so it has to move the fee with
	// them -- the reader is being quoted one deal, not two.
	if _, err := f.pool.Exec(ctx,
		`UPDATE pricing_plans SET default_multiplier_bps = 9000 WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	if got := feeOf(t); got != 450 {
		t.Fatalf("the fee must follow the plan the prices are quoted at: got %d, want 450", got)
	}

	// A per-model override changes that model's rate, not which plan the reader
	// is on. Letting it move the deployment-level fee would make the published
	// figure depend on whichever model happened to sort first.
	var planID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM pricing_plans WHERE is_default`).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO pricing_plan_model_overrides (pricing_plan_id, model_id, multiplier_bps)
		VALUES ($1, $2, 5000)`, planID, model); err != nil {
		t.Fatal(err)
	}
	if got := feeOf(t); got != 450 {
		t.Fatalf("a per-model override must not move the deployment fee: got %d, want 450", got)
	}
}
