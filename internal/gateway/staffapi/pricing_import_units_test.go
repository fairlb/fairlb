package gwstaffapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The per-unit list is this repository's own, with its own provenance, and it
// is the only source that can price a video model: the vendored token dataset
// carries none at all. This fixture is a miniature of it.
const testUnitRateData = `{
  "dataset": "test-unit-rates",
  "checked_on": "2026-08-26",
  "models": [
    {"vendor": "kuaishou", "label": "Test Kling",
     "prefixes": ["kling-v2"],
     "source_name": "Test source", "source_url": "https://example.test/kling",
     "rates": [
       {"unit": "second", "resolution": "720p", "audio": "off", "usd_per_unit": "0.28"},
       {"unit": "second", "resolution": "1080p", "audio": "off", "usd_per_unit": "0.56"}
     ]}
  ]
}`

func testUnitDataset(t *testing.T) *refdata.UnitDataset {
	t.Helper()
	d, err := refdata.ParseUnitRates([]byte(testUnitRateData))
	if err != nil {
		t.Fatalf("parse the test per-unit rates: %v", err)
	}
	return d
}

// A model billed by the second is priced from the per-unit list, and the row it
// produces has to be one admission can actually charge against: the per-unit
// family, a rate card with a row per resolution, and four explicit token zeroes
// rather than NULLs.
//
// The zeroes are the part worth asserting. NULL means "not known" everywhere in
// this schema and fails closed at request time; for a model billed by the
// second the token rates are not unknown, they do not exist. Getting that
// backwards would take a perfectly priced video model and refuse every request
// against it.
func TestImportPricesAPerUnitModelFromTheUnitList(t *testing.T) {
	pool := testpg.Start(t)
	seedVideoProviderModelRoute(t, pool, "kuaishou", "kling", "https://api.kling.test",
		"kuaishou/kling-v2", "kling-v2-master")

	rep := runUnitImport(t, pool, gwstaffapi.ImportOptions{})
	if got := outcomeFor(rep, "kuaishou/kling-v2"); got != gwstaffapi.ImportPriced {
		t.Fatalf("outcome %q, want priced; report: %+v", got, rep.Results)
	}

	ctx := context.Background()
	var family, mode string
	var in, out, cacheRead, cacheWrite *int64
	var verifiedAt *time.Time
	var sourceName string
	if err := pool.QueryRow(ctx, `
		SELECT mp.pricing_family, mp.billing_mode,
		       mp.upstream_in_nano_per_mtok, mp.upstream_out_nano_per_mtok,
		       mp.upstream_cache_read_nano_per_mtok, mp.upstream_cache_write_nano_per_mtok,
		       mp.verified_at, mp.source_name
		  FROM model_pricing mp JOIN models m ON m.id = mp.model_id
		 WHERE m.slug = $1`, "kuaishou/kling-v2").
		Scan(&family, &mode, &in, &out, &cacheRead, &cacheWrite, &verifiedAt, &sourceName); err != nil {
		t.Fatalf("read the stored price: %v", err)
	}
	if family != "units" {
		t.Errorf("pricing_family is %q; a model billed by the second is not on the token family", family)
	}
	if mode != "paid" {
		t.Errorf("billing_mode is %q", mode)
	}
	for name, v := range map[string]*int64{
		"in": in, "out": out, "cache_read": cacheRead, "cache_write": cacheWrite,
	} {
		if v == nil {
			t.Errorf("%s is NULL; NULL means \"not known\" here and fails closed at request time, "+
				"while these rates are known not to exist", name)
		} else if *v != 0 {
			t.Errorf("%s is %d; a per-unit model has no token rate", name, *v)
		}
	}
	if verifiedAt != nil {
		t.Error("the import filled in a verification date; pressing a button is not the act " +
			"that field records (ADR-0128)")
	}
	if sourceName != "Test source" {
		t.Errorf("source_name is %q; it must name where the rate came from", sourceName)
	}

	var rows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM model_price_unit_rates r JOIN models m ON m.id = r.model_id
		 WHERE m.slug = $1 AND r.unit = 'second'`, "kuaishou/kling-v2").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("stored %d per-second rates, want one per resolution", rows)
	}
	var nano int64
	if err := pool.QueryRow(ctx, `
		SELECT r.nano_per_unit FROM model_price_unit_rates r JOIN models m ON m.id = r.model_id
		 WHERE m.slug = $1 AND r.resolution = '1080p'`, "kuaishou/kling-v2").Scan(&nano); err != nil {
		t.Fatal(err)
	}
	// 0.56 USD per second, per *unit* and not per million of them. A rate that
	// travelled through the token conversion would be wrong by a factor of a
	// million, which is the one arithmetic mistake this family can make.
	if want := int64(560_000_000); nano != want {
		t.Errorf("1080p stored as %d nano per second, want %d", nano, want)
	}
}

// A price a person has checked outranks the list, exactly as it does for token
// rates. The two sources share that rule because they share the reason for it.
func TestImportLeavesAVerifiedPerUnitPriceAlone(t *testing.T) {
	pool := testpg.Start(t)
	seedVideoProviderModelRoute(t, pool, "kuaishou", "kling", "https://api.kling.test",
		"kuaishou/kling-v2", "kling-v2-master")
	runUnitImport(t, pool, gwstaffapi.ImportOptions{})

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		UPDATE model_pricing SET verified_at = now()
		 WHERE model_id = (SELECT id FROM models WHERE slug = $1)`, "kuaishou/kling-v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE model_price_unit_rates SET nano_per_unit = 1
		 WHERE model_id = (SELECT id FROM models WHERE slug = $1)`, "kuaishou/kling-v2"); err != nil {
		t.Fatal(err)
	}

	rep := runUnitImport(t, pool, gwstaffapi.ImportOptions{Force: true})
	if got := outcomeFor(rep, "kuaishou/kling-v2"); got != gwstaffapi.ImportVerified {
		t.Fatalf("outcome %q, want verified; --force overwrites prices nobody has checked, not these", got)
	}
	var nano int64
	if err := pool.QueryRow(ctx, `
		SELECT min(r.nano_per_unit) FROM model_price_unit_rates r JOIN models m ON m.id = r.model_id
		 WHERE m.slug = $1`, "kuaishou/kling-v2").Scan(&nano); err != nil {
		t.Fatal(err)
	}
	if nano != 1 {
		t.Errorf("the checked rate was overwritten, now %d nano", nano)
	}
}

// A model the per-unit list has no opinion about must fall through to the token
// dataset unchanged. The two sources are consulted in one order, and the
// per-unit one being asked first must not cost the other any coverage.
func TestImportStillPricesTokenModelsWhenTheUnitListIsPresent(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")

	rep := runUnitImport(t, pool, gwstaffapi.ImportOptions{})
	if got := outcomeFor(rep, "acme/large"); got != gwstaffapi.ImportPriced {
		t.Fatalf("outcome %q, want priced from the token dataset", got)
	}
	var family string
	if err := pool.QueryRow(context.Background(), `
		SELECT mp.pricing_family FROM model_pricing mp JOIN models m ON m.id = mp.model_id
		 WHERE m.slug = $1`, "acme/large").Scan(&family); err != nil {
		t.Fatal(err)
	}
	if family != "tokens" {
		t.Errorf("pricing_family is %q; this model is billed in tokens", family)
	}
}

func runUnitImport(
	t *testing.T, pool *pgxpool.Pool, opts gwstaffapi.ImportOptions,
) *gwstaffapi.ImportReport {
	t.Helper()
	opts.UnitData = testUnitDataset(t)
	return runImportWith(t, pool, opts)
}

func outcomeFor(rep *gwstaffapi.ImportReport, slug string) gwstaffapi.ImportOutcome {
	for _, r := range rep.Results {
		if r.ModelSlug == slug {
			return r.Outcome
		}
	}
	return ""
}

// seedVideoProviderModelRoute is the video-plane twin of the helper above: the
// provider declares the video protocol and the route is probed on the video
// endpoint, because a route on the wrong endpoint is not a usable one.
func seedVideoProviderModelRoute(
	t *testing.T, pool *pgxpool.Pool, vendor, providerSlug, baseURL, modelSlug, upstreamID string,
) {
	t.Helper()
	ctx := context.Background()
	var providerID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, $3, ARRAY['video'], $2)
		ON CONFLICT (slug) DO UPDATE SET base_url = EXCLUDED.base_url
		RETURNING id`, providerSlug, baseURL, vendor).Scan(&providerID); err != nil {
		t.Fatalf("seed provider %s: %v", providerSlug, err)
	}
	var modelID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO models (slug, output_modalities) VALUES ($1, ARRAY['video'])
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`, modelSlug).Scan(&modelID); err != nil {
		t.Fatalf("seed model %s: %v", modelSlug, err)
	}
	catalogtest.SeedRoute(t, pool, modelID, providerID, upstreamID, "video")
}
