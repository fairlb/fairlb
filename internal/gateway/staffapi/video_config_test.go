package gwstaffapi_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The two configuration surfaces a video model needs, over HTTP.
//
// They existed in the domain layer well before they existed here, and the gap
// was invisible: the domain tests passed, the write path worked, and nothing
// outside a Go test could reach either -- so an operator could not create a
// per-second model at all. These are the tests that would have said so.

// A per-second model, priced and read back, through the contract's own DTOs.
func TestOperatorPricesAPerSecondModelOverHTTP(t *testing.T) {
	_, pool, _ := newServer(t)
	ctx := context.Background()
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})
	modelID := videoModelRow(t, pool, "google/veo-3.1")

	family := gwstaffapi.PricingFamilyUnits
	saved, _, err := svc.SaveModelPricing(ctx, modelID, gwstaffapi.ModelPricingInput{
		BillingMode:   gwstaffapi.ModelPricingInputBillingModePaid,
		PricingFamily: &family,
		// Deliberately no official_rates: a per-second model has no token
		// price, and requiring four zeros here is what this contract change
		// removed.
		Adjustment: gwstaffapi.PricingAdjustment{MultiplierBps: 12000},
		SourceName: "vendor price list",
		CheckedAt:  time.Now().UTC(),
		Reason:     "initial",
		UnitRates: &[]gwstaffapi.ModelPriceUnitRate{
			{Unit: gwstaffapi.UnitSecond, RateUsdPerUnit: "0.40"},
		},
		AcknowledgedRisks: &[]gwstaffapi.PricingRiskCode{gwstaffapi.UnknownProcurementCost},
	}, "", pgtype.UUID{})
	if err != nil {
		t.Fatalf("a per-second model must be priceable over the API: %v", err)
	}
	if saved.PricingFamily == nil || *saved.PricingFamily != gwstaffapi.PricingFamilyUnits {
		t.Fatalf("pricing_family read back as %v; without it admission refuses the model as unpriced",
			saved.PricingFamily)
	}
	if saved.UnitRates == nil || len(*saved.UnitRates) != 1 {
		t.Fatalf("unit rates read back as %v; an editor that cannot see them offers to save half a card",
			saved.UnitRates)
	}
	if (*saved.UnitRates)[0].RateUsdPerUnit != "0.4" {
		t.Fatalf("rate read back as %q, want 0.4 -- a per-unit rate must not travel through the "+
			"per-million conversion, which would divide it by a million",
			(*saved.UnitRates)[0].RateUsdPerUnit)
	}
	// The four token columns are stored as explicit zeros, and are not rendered:
	// showing them would state a token price for a model that has none.
	if saved.OfficialRates != nil {
		t.Errorf("official_rates rendered for a per-unit model: %+v", *saved.OfficialRates)
	}
}

// Sending token rates for a per-unit model is refused rather than ignored. Two
// statements of what charges a model, one of them false, is exactly the shape
// this contract is arranged to prevent.
func TestPerUnitPricingRefusesTokenRates(t *testing.T) {
	_, pool, _ := newServer(t)
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})
	modelID := videoModelRow(t, pool, "google/veo-3.1-b")
	family := gwstaffapi.PricingFamilyUnits
	zero := "0"
	_, _, err := svc.SaveModelPricing(context.Background(), modelID, gwstaffapi.ModelPricingInput{
		BillingMode:   gwstaffapi.ModelPricingInputBillingModePaid,
		PricingFamily: &family,
		OfficialRates: &gwstaffapi.DraftTokenRatesUSDPerM{
			Input: &zero, Output: &zero, CacheRead: &zero, CacheWrite: &zero,
		},
		Adjustment: gwstaffapi.PricingAdjustment{MultiplierBps: 10000},
		SourceName: "vendor price list",
		CheckedAt:  time.Now().UTC(),
		Reason:     "initial",
		UnitRates:  &[]gwstaffapi.ModelPriceUnitRate{{Unit: gwstaffapi.UnitSecond, RateUsdPerUnit: "0.4"}},
	}, "", pgtype.UUID{})
	if err == nil {
		t.Fatal("official_rates on a per-unit model must be refused")
	}
	if !strings.Contains(err.Error(), "official_rates") {
		t.Errorf("the refusal must name the offending field: %v", err)
	}
}

// A paid per-unit model with its rates explicitly emptied cannot be charged, and
// admission answers 503 to every request against it. It is a blocker, and the
// code for it is now in the contract so the interface can name it.
func TestPerUnitPricingRefusesAnEmptyRateCard(t *testing.T) {
	_, pool, _ := newServer(t)
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})
	modelID := videoModelRow(t, pool, "google/veo-3.1-c")
	family := gwstaffapi.PricingFamilyUnits
	_, _, err := svc.SaveModelPricing(context.Background(), modelID, gwstaffapi.ModelPricingInput{
		BillingMode:   gwstaffapi.ModelPricingInputBillingModePaid,
		PricingFamily: &family,
		Adjustment:    gwstaffapi.PricingAdjustment{MultiplierBps: 10000},
		SourceName:    "vendor price list",
		CheckedAt:     time.Now().UTC(),
		Reason:        "clearing the card",
		UnitRates:     &[]gwstaffapi.ModelPriceUnitRate{},
	}, "", pgtype.UUID{})
	if err == nil {
		t.Fatal("a paid per-unit model with no rates must be refused: it cannot be charged")
	}
}

// Two rates covering the same case would both match one request and one would
// silently win. Refused here so the answer names the two rows rather than a
// database constraint.
func TestUnitRatesRefuseOverlappingRows(t *testing.T) {
	_, pool, _ := newServer(t)
	svc := gwstaffapi.NewPGPricingAdminService(gwstaffapi.PGPricingAdminConfig{Pool: pool})
	modelID := videoModelRow(t, pool, "google/veo-3.1-d")
	family := gwstaffapi.PricingFamilyUnits
	_, _, err := svc.SaveModelPricing(context.Background(), modelID, gwstaffapi.ModelPricingInput{
		BillingMode:   gwstaffapi.ModelPricingInputBillingModePaid,
		PricingFamily: &family,
		Adjustment:    gwstaffapi.PricingAdjustment{MultiplierBps: 10000},
		SourceName:    "vendor price list",
		CheckedAt:     time.Now().UTC(),
		Reason:        "initial",
		UnitRates: &[]gwstaffapi.ModelPriceUnitRate{
			{Unit: gwstaffapi.UnitSecond, RateUsdPerUnit: "0.40"},
			{Unit: gwstaffapi.UnitSecond, RateUsdPerUnit: "0.75"},
		},
	}, "", pgtype.UUID{})
	if err == nil {
		t.Fatal("two rates covering the same case must be refused")
	}
}

// The envelope goes in and comes back. It is what admission refuses against, so
// a route that cannot carry one serves nothing at all.
func TestRouteVideoEnvelopeRoundTrips(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "kuaishou/kling-v2-master")
	prov := mustProviderForVendor(t, s, "kuaishou", []string{catalog.ProtocolVideo}, "https://api.example.com")

	upstream := "kling-v2-master"
	audio := gwstaffapi.VideoEnvelopeAudio("optional")
	cancel := gwstaffapi.VideoEnvelopeCancel("queued_only")
	res, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
		ModelId: model.Id,
		Body: &gwstaffapi.GatewayRouteInput{
			ProviderId: &prov, ProviderModelId: &upstream,
			VideoEnvelope: &gwstaffapi.VideoEnvelope{
				DurationsSeconds: &[]int32{5, 10},
				Resolutions:      &[]string{"720p", "1080p"},
				AspectRatios:     &[]string{"16:9", "9:16"},
				Audio:            &audio,
				Cancel:           &cancel,
			},
		},
	})
	if err != nil {
		t.Fatalf("create route with an envelope: %v", err)
	}
	route := gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))
	if route.VideoEnvelope == nil {
		t.Fatal("the envelope did not come back; a write-only field cannot be edited")
	}
	if got := route.VideoEnvelope.DurationsSeconds; got == nil || len(*got) != 2 {
		t.Fatalf("durations read back as %v, want two", got)
	}
	if got := route.VideoEnvelope.Cancel; got == nil || *got != "queued_only" {
		t.Fatalf("cancel read back as %v; unset would silently read as never", got)
	}

	// Reading the route again must give the same answer as creating it did. The
	// two used to be different code paths and only one of them carried the
	// field.
	listed, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model.Id})
	if err != nil {
		t.Fatal(err)
	}
	items := listed.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items
	if len(items) != 1 || items[0].VideoEnvelope == nil {
		t.Fatalf("the listing dropped the envelope: %+v", items)
	}
}

// `source` is read-only, and an input that sets it is ignored rather than
// honoured. The rule it protects is that the interface must never present
// somebody's typed-in envelope as a vendor's own answer.
func TestRouteEnvelopeSourceIsAlwaysDeclared(t *testing.T) {
	s, _, _ := newServer(t)
	model := mustModel(t, s, "kuaishou/kling-v2-pro")
	prov := mustProviderForVendor(t, s, "kuaishou", []string{catalog.ProtocolVideo}, "https://api.example.com")
	upstream := "kling-v2-pro"

	res, err := s.CreateGatewayRoute(context.Background(), gwstaffapi.CreateGatewayRouteRequestObject{
		ModelId: model.Id,
		Body: &gwstaffapi.GatewayRouteInput{
			ProviderId: &prov, ProviderModelId: &upstream,
			VideoEnvelope: &gwstaffapi.VideoEnvelope{
				DurationsSeconds: &[]int32{5},
				Resolutions:      &[]string{"720p"},
				AspectRatios:     &[]string{"16:9"},
				Source:           ptrSource("observed"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	route := gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))
	if got := route.VideoEnvelope.Source; got == nil || *got != "declared" {
		t.Fatalf("source stored as %v; nothing in this build observes an envelope, so claiming "+
			"one was observed is a check nobody performed", got)
	}
}

// A blank envelope clears the route rather than leaving the old one standing.
//
// The contract has no null variant on purpose -- it would have decoded to the
// same nil pointer an omitted field does, so "clear it" would have quietly left
// the previous envelope admitting requests. An empty object is the reachable
// spelling, and it has to land as an empty object: a lone `source` stamp is a
// provenance claim about a declaration that is not there.
func TestABlankEnvelopeUnconfiguresTheRoute(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "kuaishou/kling-clearable")
	prov := mustProviderForVendor(t, s, "kuaishou", []string{catalog.ProtocolVideo}, "https://api.example.com")
	upstream := "kling-v2-master"

	res, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
		ModelId: model.Id,
		Body: &gwstaffapi.GatewayRouteInput{
			ProviderId: &prov, ProviderModelId: &upstream,
			VideoEnvelope: &gwstaffapi.VideoEnvelope{
				DurationsSeconds: &[]int32{5, 10},
				Resolutions:      &[]string{"720p"},
				AspectRatios:     &[]string{"16:9"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	route := gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))

	if _, err := s.UpdateGatewayRoute(ctx, gwstaffapi.UpdateGatewayRouteRequestObject{
		ModelId: model.Id, RouteId: route.Id,
		Body: &gwstaffapi.GatewayRouteInput{VideoEnvelope: &gwstaffapi.VideoEnvelope{}},
	}); err != nil {
		t.Fatalf("saving a blank envelope: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT video_envelope::text FROM model_routes WHERE id = $1`, route.Id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "{}" {
		t.Fatalf("stored envelope = %s, want {} -- anything else is a declaration that is "+
			"not there, and the route goes on admitting requests", stored)
	}
}

// Omitting the field leaves the stored envelope alone. It is the other half of
// the rule above: if both spellings cleared, every route edit that did not
// mention video would silently take a video route out of service.
func TestOmittingTheEnvelopeKeepsIt(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "kuaishou/kling-keeps-envelope")
	prov := mustProviderForVendor(t, s, "kuaishou", []string{catalog.ProtocolVideo}, "https://api.example.com")
	upstream := "kling-v2-master"
	weight := 3

	res, err := s.CreateGatewayRoute(ctx, gwstaffapi.CreateGatewayRouteRequestObject{
		ModelId: model.Id,
		Body: &gwstaffapi.GatewayRouteInput{
			ProviderId: &prov, ProviderModelId: &upstream,
			VideoEnvelope: &gwstaffapi.VideoEnvelope{
				DurationsSeconds: &[]int32{5, 10},
				Resolutions:      &[]string{"720p"},
				AspectRatios:     &[]string{"16:9"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	route := gwstaffapi.GatewayRoute(res.(gwstaffapi.CreateGatewayRoute201JSONResponse))

	updated, err := s.UpdateGatewayRoute(ctx, gwstaffapi.UpdateGatewayRouteRequestObject{
		ModelId: model.Id, RouteId: route.Id,
		Body: &gwstaffapi.GatewayRouteInput{Weight: &weight},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := gwstaffapi.GatewayRoute(updated.(gwstaffapi.UpdateGatewayRoute200JSONResponse))
	if got.VideoEnvelope == nil || got.VideoEnvelope.DurationsSeconds == nil ||
		len(*got.VideoEnvelope.DurationsSeconds) != 2 {
		t.Fatalf("changing the weight dropped the envelope: %+v", got.VideoEnvelope)
	}
}

// The prefill endpoint answers for a vendor and an upstream model, and marks its
// answer `declared`: these numbers were read out of a vendor's contract when the
// mapper was written, not observed from the vendor now.
func TestVendorVideoEnvelopePrefillIsDeclared(t *testing.T) {
	s, _, _ := newServer(t)
	res, err := s.GetGatewayVendorVideoEnvelope(staffCtx(),
		gwstaffapi.GetGatewayVendorVideoEnvelopeRequestObject{
			Vendor: "kuaishou",
			Params: gwstaffapi.GetGatewayVendorVideoEnvelopeParams{UpstreamModel: "kling-v2-master"},
		})
	if err != nil {
		t.Fatalf("the prefill must answer for a vendor this build maps: %v", err)
	}
	env := res.(gwstaffapi.GetGatewayVendorVideoEnvelope200JSONResponse).Envelope
	if env.DurationsSeconds == nil || len(*env.DurationsSeconds) == 0 {
		t.Fatal("the prefill produced no durations, so it prefills nothing")
	}
	if env.Source == nil || *env.Source != "declared" {
		t.Fatalf("the prefill is marked %v; it is what this build recorded, not what a vendor "+
			"answered just now", env.Source)
	}
}

func TestVendorVideoEnvelopeRefusesAnUnmappedVendor(t *testing.T) {
	s, _, _ := newServer(t)
	_, err := s.GetGatewayVendorVideoEnvelope(staffCtx(),
		gwstaffapi.GetGatewayVendorVideoEnvelopeRequestObject{
			Vendor: "openai",
			Params: gwstaffapi.GetGatewayVendorVideoEnvelopeParams{UpstreamModel: "gpt-5"},
		})
	if err == nil {
		t.Fatal("a vendor with no video mapper must be refused, not answered with an empty envelope")
	}
}

// videoModelRow inserts a bare model to price. The pricing service is reached
// directly rather than through the HTTP wrapper, which needs an ETag from a
// prior read; what is under test here is the contract's own DTO translation.
func videoModelRow(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO models (slug, max_output_tokens) VALUES ($1, 4096) RETURNING id`,
		slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func ptrSource(v gwstaffapi.VideoEnvelopeSource) *gwstaffapi.VideoEnvelopeSource { return &v }

// staffCtx carries the subject every read on this plane needs. Anonymous access
// is covered by the plane's own middleware tests rather than repeated here.
func staffCtx() context.Context {
	return httpx.WithPrincipal(context.Background(), httpx.Principal{
		Scope: "admin", Subject: uuid.NewString(), Role: "operator",
	})
}
