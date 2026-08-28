package refdata

import (
	"slices"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The bundled list has to load, and every entry has to be one a model could
// actually be billed from. A malformed entry here reaches every deployment.
func TestBundledUnitRatesLoad(t *testing.T) {
	d, err := BundledUnitRates()
	if err != nil {
		t.Fatal(err)
	}
	if d.Dataset == "" || d.CheckedOn == "" {
		t.Fatal("the list must name itself and the day it was checked")
	}
	if len(d.Vendors()) == 0 {
		t.Fatal("the list has no entries at all")
	}
}

// The ids these vendors publish carry release dates and deployment suffixes, so
// entries match on a prefix. What matters is that the prefix is anchored: a
// rate card that matched anywhere in the id would price one model from
// another's card.
func TestUnitRatesMatchOnAnAnchoredPrefixAndOnlyWithinTheVendor(t *testing.T) {
	d, err := BundledUnitRates()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Lookup("google", "veo-3.1-generate-preview"); !ok {
		t.Error("a real Veo deployment id did not match its own rate card")
	}
	if _, ok := d.Lookup("google", "VEO-3.1-GENERATE-PREVIEW"); !ok {
		t.Error("the match must not depend on the case the operator typed")
	}
	if _, ok := d.Lookup("kuaishou", "veo-3.1-generate-preview"); ok {
		t.Error("one vendor's model matched another vendor's rate card")
	}
	if _, ok := d.Lookup("google", "my-veo-3.1-proxy"); ok {
		t.Error("the prefix matched in the middle of an id; a rate card must be anchored")
	}
	if _, ok := d.Lookup("google", "gemini-3.7-flash"); ok {
		t.Error("a token-billed model matched a per-unit rate card")
	}
}

// Every rejection below is a file that would import a model which cannot be
// charged for, and a paid per-unit model with no usable rate answers 503 to
// every request against it. Refusing here, where the file is, beats refusing
// there.
func TestUnitRatesRefuseAFileThatCannotBeBilledFrom(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no dataset name", `{"checked_on":"2026-08-26","models":[]}`, "name no list"},
		{"no checked date", `{"dataset":"x","models":[]}`, "no day they were checked"},
		{"no vendor", `{"dataset":"x","checked_on":"d","models":[{"prefixes":["a"],
			"source_name":"s","source_url":"u","rates":[{"unit":"second","usd_per_unit":"1"}]}]}`,
			"names no vendor"},
		{"no prefixes", `{"dataset":"x","checked_on":"d","models":[{"vendor":"v","prefixes":[],
			"source_name":"s","source_url":"u","rates":[{"unit":"second","usd_per_unit":"1"}]}]}`,
			"matches no upstream id"},
		{"no rates", `{"dataset":"x","checked_on":"d","models":[{"vendor":"v","prefixes":["a"],
			"source_name":"s","source_url":"u","rates":[]}]}`,
			"cannot be charged for"},
		{"no source", `{"dataset":"x","checked_on":"d","models":[{"vendor":"v","prefixes":["a"],
			"rates":[{"unit":"second","usd_per_unit":"1"}]}]}`,
			"nobody can ever check"},
		{"rate with no price", `{"dataset":"x","checked_on":"d","models":[{"vendor":"v","prefixes":["a"],
			"source_name":"s","source_url":"u","rates":[{"unit":"second"}]}]}`,
			"missing its unit or its price"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUnitRates([]byte(tc.body))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal must say what is wrong, got %q", err)
			}
		})
	}
}

// Two overlapping prefixes under one vendor make the answer depend on the order
// of the file, and the wrong one of two rate cards is a wrong bill.
func TestUnitRatesRefuseOverlappingPrefixes(t *testing.T) {
	_, err := ParseUnitRates([]byte(`{"dataset":"x","checked_on":"d","models":[
		{"vendor":"v","label":"a","prefixes":["kling-v2"],"source_name":"s","source_url":"u",
		 "rates":[{"unit":"second","usd_per_unit":"1"}]},
		{"vendor":"v","label":"b","prefixes":["kling-v2-master"],"source_name":"s","source_url":"u",
		 "rates":[{"unit":"second","usd_per_unit":"2"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping prefixes must be refused, got %v", err)
	}
	// The same prefix under two different vendors is fine: the vendor is part
	// of the key, and two platforms may well spell a model the same way.
	if _, err := ParseUnitRates([]byte(`{"dataset":"x","checked_on":"d","models":[
		{"vendor":"v1","label":"a","prefixes":["kling-v2"],"source_name":"s","source_url":"u",
		 "rates":[{"unit":"second","usd_per_unit":"1"}]},
		{"vendor":"v2","label":"b","prefixes":["kling-v2"],"source_name":"s","source_url":"u",
		 "rates":[{"unit":"second","usd_per_unit":"2"}]}]}`)); err != nil {
		t.Fatalf("the same id under two vendors must be allowed: %v", err)
	}
}

// The unit is per second, never per million seconds. The token rates beside
// these are quoted per million and divided on the way in; a per-second rate
// that travelled through that conversion would be wrong by a factor of a
// million (ADR-0220). A rate that looks like a per-million figure is the shape
// of that mistake, so the bundled list is checked for it.
func TestBundledUnitRatesAreQuotedPerUnitNotPerMillion(t *testing.T) {
	d, err := BundledUnitRates()
	if err != nil {
		t.Fatal(err)
	}
	for _, vendor := range d.Vendors() {
		for _, m := range d.models {
			if m.Vendor != vendor {
				continue
			}
			for _, r := range m.Rates {
				if strings.HasPrefix(r.USDPerUnit, "0.00000") {
					t.Errorf("%s %s is %s USD per %s, which reads as a per-million figure "+
						"divided by a million", m.Vendor, m.Label, r.USDPerUnit, r.Unit)
				}
			}
		}
	}
}

// Every unit in the bundled file has to be one the schema can store.
//
// The check is here rather than in the parser because the parser cannot see the
// unit registry without this package importing the catalogue, and the catalogue
// reads this one. A gate is enough: the bundled file is the only input there
// is, and a typo in it is introduced while this test is runnable, not while a
// deployment is serving. What it prevents is a rate card that parses, looks
// like it exists, and fails at an insert a long way from the line that caused
// it -- `"images"` for `"image"` would do exactly that.
func TestBundledUnitRatesUseKnownUnits(t *testing.T) {
	d, err := BundledUnitRates()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range d.models {
		for _, r := range m.Rates {
			if !slices.Contains(catalog.KnownUnits(), catalog.Unit(r.Unit)) {
				t.Errorf("%s (%s): %q is not a billing unit", m.Vendor, m.Label, r.Unit)
			}
		}
	}
}
