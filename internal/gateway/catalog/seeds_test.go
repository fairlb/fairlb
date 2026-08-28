package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
)

func TestSeedSlugsSatisfyTheDatabaseConstraint(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range SeedModels() {
		if !ValidModelSlug(m.Slug) {
			t.Errorf("%s: does not satisfy models_slug_shape, so creating it would fail", m.Slug)
		}
		if seen[m.Slug] {
			t.Errorf("%s: listed twice; the slug is unique in the database", m.Slug)
		}
		seen[m.Slug] = true
		if strings.TrimSpace(m.UpstreamID) == "" {
			t.Errorf("%s: no upstream id, so a route created from it would name nothing", m.Slug)
		}
		if strings.TrimSpace(m.DisplayName) == "" {
			t.Errorf("%s: no display name", m.Slug)
		}
		if len(m.Vendors) == 0 {
			t.Errorf("%s: no vendors, so nothing can ever match it", m.Slug)
		}
	}
}

// The creator segment of a seed's slug has to be the creator its vendor
// declares. Both are written by hand in this package, and the whole point of
// the seed is that an operator does not have to check them -- so something
// else must.
func TestSeedCreatorSegmentMatchesItsVendor(t *testing.T) {
	for _, m := range SeedModels() {
		creator, _, _ := strings.Cut(m.Slug, "/")
		for _, slug := range m.Vendors {
			v, ok := LookupVendor(slug)
			if !ok {
				t.Errorf("%s: vendor %q is not in the registry", m.Slug, slug)
				continue
			}
			if v.Creator == "" {
				t.Errorf("%s: vendor %q declares no creator, so it cannot be the first-party "+
					"source of a seeded model", m.Slug, slug)
				continue
			}
			if v.Creator != creator {
				t.Errorf("%s: slug says the creator is %q, vendor %q says %q",
					m.Slug, creator, slug, v.Creator)
			}
		}
	}
}

// Every seed must resolve to exactly one reference price, under the scope its
// own vendor resolves to.
//
// This is the guard that keeps the seed honest without anybody watching it. A
// mistyped upstream id still creates a catalog entry that looks right, and the
// only symptom is that the "price this from the reference dataset" step
// quietly does nothing for that one model -- which is the exact step the seed
// exists to make reliable. Here it is a failing test instead.
func TestSeedUpstreamIDsAreAllPriceable(t *testing.T) {
	data, err := refdata.Bundled()
	if err != nil {
		t.Fatalf("load the bundled reference dataset: %v", err)
	}
	// A fixed instant, not time.Now(): the dataset carries announced price
	// changes ahead of the day they take effect, and a test that reads the
	// clock would start failing on the morning one of them lands.
	on := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	for _, m := range SeedModels() {
		for _, slug := range m.Vendors {
			v, ok := LookupVendor(slug)
			if !ok {
				continue // reported by TestSeedCreatorSegmentMatchesItsVendor
			}
			if v.RefdataProvider == "" {
				t.Errorf("%s: vendor %q has no reference-price scope, so the one-click "+
					"pricing this table exists for cannot work", m.Slug, slug)
				continue
			}
			res := data.Lookup(v.RefdataProvider, m.UpstreamID, on)
			if res.Outcome != refdata.Matched {
				t.Errorf("%s: upstream id %q under scope %q: %s (%s)",
					m.Slug, m.UpstreamID, v.RefdataProvider, res.Outcome, res.Detail)
			}
		}
	}
}

func TestLookupSeedNeedsBothHalves(t *testing.T) {
	if _, ok := LookupSeed("openai", "gpt-5.6-sol"); !ok {
		t.Error("openai/gpt-5.6-sol should be found under its own vendor")
	}
	// The same bare name under a vendor that does not spell it that way must
	// not match: an upstream name is only meaningful together with who is
	// being asked.
	if _, ok := LookupSeed("aws-bedrock", "claude-opus-5"); ok {
		t.Error("Bedrock spells Claude differently; matching it here would hand out a wrong id")
	}
	if _, ok := LookupSeed("", "gpt-5.6-sol"); ok {
		t.Error("an empty vendor must not match")
	}
	if _, ok := LookupSeed("openai", ""); ok {
		t.Error("an empty upstream id must not match")
	}
}
