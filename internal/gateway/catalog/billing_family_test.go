package catalog

import (
	"slices"
	"testing"
)

// One surface, two billing families, both resolvable.
//
// This is the whole of ADR-0227 stated as a test. The images surface carries
// gpt-image, billed per token, and Seedream, billed per produced image. Before
// the family became a set, a correctly configured per-image model answered 404
// to every request: `checkPriced` compared the price row's family against a
// single value pinned to the endpoint, and one of the two was always wrong.
func TestImagesSurfaceServesBothBillingFamilies(t *testing.T) {
	for _, family := range []BillingFamily{FamilyTokens, FamilyUnits} {
		if !SurfaceImages.ServesFamily(family) {
			t.Errorf("the images surface refuses the %s family; a model priced that way "+
				"cannot be reached on the endpoint it is actually served on", family)
		}
	}
}

// The set is not "everything, everywhere". A token-billed model on the video
// plane, or a per-second model on chat, is still unavailable -- that check is
// what stops an all-zero rate card from billing nothing at all.
func TestTokenOnlyAndUnitOnlySurfacesStayNarrow(t *testing.T) {
	if SurfaceChat.ServesFamily(FamilyUnits) {
		t.Error("chat serves the unit family; a per-second model reached there would " +
			"resolve with four zero token rates and bill nothing")
	}
	if SurfaceVideo.ServesFamily(FamilyTokens) {
		t.Error("the video plane serves the token family; a job's charge is a function " +
			"of its parameters and has no token counts to bill from")
	}
}

// checkPriced answers on the price row's family, not the surface's name.
func TestCheckPricedFollowsThePriceRow(t *testing.T) {
	unitPriced := ModelPricingSnapshot{Priced: true, BillingMode: "paid", Family: FamilyUnits}
	if err := checkPriced(unitPriced, SurfaceImages); err != nil {
		t.Fatalf("a per-image model on the images surface was refused: %v", err)
	}
	if err := checkPriced(unitPriced, SurfaceChat); err == nil {
		t.Fatal("a per-image model was accepted on chat, where nothing can bill it")
	}

	tokenPriced := ModelPricingSnapshot{
		Priced: true, BillingMode: "paid", Family: FamilyTokens,
		Upstream: Price{InNanoPerMTok: 1},
	}
	if err := checkPriced(tokenPriced, SurfaceImages); err != nil {
		t.Fatalf("gpt-image, billed per token, was refused on the images surface: %v", err)
	}
}

// Every surface has an arm in every table.
//
// Enumerated from AllSurfaces rather than restated, which is the reason that
// function exists: a surface added without a billing family would default to
// tokens silently, and a per-unit one would then bill from four zero rates.
func TestEverySurfaceDeclaresItsBillingFamilies(t *testing.T) {
	for _, s := range AllSurfaces() {
		families := s.BillingFamilies()
		if len(families) == 0 {
			t.Errorf("%s declares no billing family", s)
			continue
		}
		for _, f := range families {
			if f != FamilyTokens && f != FamilyUnits {
				t.Errorf("%s declares unknown billing family %q", s, f)
			}
		}
		if _, ok := s.protocol(); !ok {
			t.Errorf("%s belongs to no protocol", s)
		}
		if _, ok := s.endpoint(); !ok {
			t.Errorf("%s names no endpoint", s)
		}
	}
}

// The modality vocabulary is closed, and the values are the ones the column
// accepts. A value here that the CHECK refuses is a model the operator can
// select and cannot save.
func TestKnownModalitiesMatchTheColumn(t *testing.T) {
	want := []Modality{ModalityText, ModalityImage, ModalityVideo}
	if !slices.Equal(KnownModalities(), want) {
		t.Fatalf("KnownModalities() = %v, want %v", KnownModalities(), want)
	}
	for _, m := range want {
		if !ValidModality(string(m)) {
			t.Errorf("%q is in the list but ValidModality refuses it", m)
		}
	}
	if ValidModality("audio") {
		t.Error("ValidModality accepts a modality the column does not; saving it would " +
			"fail as a constraint violation rather than as a message about the field")
	}
}
