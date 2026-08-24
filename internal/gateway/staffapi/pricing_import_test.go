package gwstaffapi_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
	"github.com/fairlb/fairlb/settings"
)

// A small dataset with fixed numbers, so the assertions below say what they
// mean. The bundled snapshot would move under them at the next refresh.
const testPriceData = `[
  {"id":"acme","api_pattern":"https://api\\.acme\\.test","models":[
    {"id":"acme-large","match":{"equals":"acme-large"},
     "prices":{"input_mtok":3,"output_mtok":15,"cache_read_mtok":0.3,"cache_write_mtok":3.75}},
    {"id":"acme-embed","match":{"equals":"acme-embed"},"prices":{"input_mtok":0.02}}]},
  {"id":"other","api_pattern":"https://api\\.other\\.test","models":[
    {"id":"acme-large","match":{"equals":"acme-large"},
     "prices":{"input_mtok":9,"output_mtok":9,"cache_read_mtok":9,"cache_write_mtok":9}}]},
  {"id":"deepseek","api_pattern":"https://api\\.deepseek\\.com","models":[
    {"id":"acme-large","match":{"equals":"acme-large"},
     "prices":{"input_mtok":7,"output_mtok":7,"cache_read_mtok":7,"cache_write_mtok":7}}]}
]`

const (
	nanoPerUSD = 1_000_000_000
	testDay    = "2026-08-17"
)

func testDataset(t *testing.T) *refdata.Dataset {
	t.Helper()
	d, err := refdata.Parse([]byte(testPriceData), refdata.Snapshot{
		Dataset: "test-prices", SourceURL: "https://example.test/prices",
		SnapshotDate: testDay, SHA256: "0123abc",
	})
	if err != nil {
		t.Fatalf("parse the test dataset: %v", err)
	}
	return d
}

func runImport(t *testing.T, pool *pgxpool.Pool, force bool) *gwstaffapi.ImportReport {
	t.Helper()
	return runImportWith(t, pool, gwstaffapi.ImportOptions{Force: force})
}

// runImportWith fills in the two options every case shares -- the fixed dataset
// and the fixed day -- and leaves the rest to the caller.
func runImportWith(
	t *testing.T, pool *pgxpool.Pool, opts gwstaffapi.ImportOptions,
) *gwstaffapi.ImportReport {
	t.Helper()
	opts.Data = testDataset(t)
	opts.Now = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	rep, err := gwstaffapi.ImportReferencePrices(context.Background(), gwstaffapi.ReferencePriceImportConfig{
		Pool: pool, Options: opts,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return rep
}

// pricingRows returns every stored price row rendered as text, whole row at a
// time.
//
// Whole rows rather than a few chosen columns, because "nothing changed" is a
// claim about the row and a hand-picked subset only ever tests the columns
// somebody thought of. `updated_at`, `provenance` and `updated_by` are all
// written by this path, and all three would be invisible to a comparison of the
// four rates.
func pricingRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT mp::text FROM model_pricing mp ORDER BY mp.model_id`)
	if err != nil {
		t.Fatalf("snapshot the price rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan a price row: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the price rows: %v", err)
	}
	return out
}

func modelID(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM models WHERE slug = $1`, slug).Scan(&id); err != nil {
		t.Fatalf("read the id of %s: %v", slug, err)
	}
	return id
}

// outcomeOf returns what the run decided for one model, so a failure names the
// model rather than an index into a slice.
func outcomeOf(t *testing.T, rep *gwstaffapi.ImportReport, slug string) gwstaffapi.ImportResult {
	t.Helper()
	for _, r := range rep.Results {
		if r.ModelSlug == slug {
			return r
		}
	}
	t.Fatalf("the report says nothing about %s; it covers %d models", slug, len(rep.Results))
	return gwstaffapi.ImportResult{}
}

// seedProviderModelRoute builds the shape the import reads: a provider, a model
// and an enabled route naming the upstream model id.
func seedProviderModelRoute(t *testing.T, pool *pgxpool.Pool, providerSlug, baseURL, modelSlug, upstreamID string) {
	t.Helper()
	seedProviderModelRouteAsVendor(t, pool, catalog.VendorCustom, providerSlug, baseURL, modelSlug, upstreamID)
}

// seedProviderModelRouteAsVendor is the same seed with the provider's vendor
// stated, for the cases where the vendor is what decides the answer.
func seedProviderModelRouteAsVendor(
	t *testing.T, pool *pgxpool.Pool, vendor, providerSlug, baseURL, modelSlug, upstreamID string,
) {
	t.Helper()
	ctx := context.Background()
	var providerID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, $3, ARRAY['openai'], $2)
		ON CONFLICT (slug) DO UPDATE SET base_url = EXCLUDED.base_url
		RETURNING id`, providerSlug, baseURL, vendor).Scan(&providerID); err != nil {
		t.Fatalf("seed provider %s: %v", providerSlug, err)
	}
	var modelID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO models (slug) VALUES ($1)
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`, modelSlug).Scan(&modelID); err != nil {
		t.Fatalf("seed model %s: %v", modelSlug, err)
	}
	catalogtest.SeedRoute(t, pool, modelID, providerID, upstreamID, "chat")
}

func storedPricing(t *testing.T, pool *pgxpool.Pool, modelSlug string) gwdb.ModelPricing {
	t.Helper()
	var row gwdb.ModelPricing
	err := pool.QueryRow(context.Background(), `
		SELECT mp.model_id, mp.billing_mode,
		       mp.upstream_in_nano_per_mtok, mp.upstream_out_nano_per_mtok,
		       mp.upstream_cache_read_nano_per_mtok, mp.upstream_cache_write_nano_per_mtok,
		       mp.source_name, mp.source_url, mp.verified_at, mp.provenance,
		       mp.reason, mp.updated_by, mp.multiplier_bps
		FROM model_pricing mp JOIN models m ON m.id = mp.model_id
		WHERE m.slug = $1`, modelSlug).Scan(
		&row.ModelID, &row.BillingMode,
		&row.UpstreamInNanoPerMtok, &row.UpstreamOutNanoPerMtok,
		&row.UpstreamCacheReadNanoPerMtok, &row.UpstreamCacheWriteNanoPerMtok,
		&row.SourceName, &row.SourceUrl, &row.VerifiedAt, &row.Provenance,
		&row.Reason, &row.UpdatedBy, &row.MultiplierBps)
	if err != nil {
		t.Fatalf("read the stored price for %s: %v", modelSlug, err)
	}
	return row
}

// The default run fills empty prices in, and running it again writes nothing.
// Idempotence is the property that makes this safe to put in a runbook.
func TestImportFillsEmptyPricesAndTheSecondRunWritesNothing(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")

	first := runImport(t, pool, false)
	if got := first.Count(gwstaffapi.ImportPriced); got != 1 {
		t.Fatalf("first run priced %d models, want 1 (%+v)", got, first.Results)
	}
	row := storedPricing(t, pool, "acme/large")
	if row.UpstreamInNanoPerMtok.Int64 != 3*nanoPerUSD || row.UpstreamOutNanoPerMtok.Int64 != 15*nanoPerUSD {
		t.Errorf("stored rates in=%d out=%d, want 3 and 15 USD/M in nano",
			row.UpstreamInNanoPerMtok.Int64, row.UpstreamOutNanoPerMtok.Int64)
	}

	second := runImport(t, pool, false)
	if got := second.Count(gwstaffapi.ImportPriced); got != 0 {
		t.Errorf("the second run priced %d models, want 0", got)
	}
	if got := second.Count(gwstaffapi.ImportKept); got != 1 {
		t.Errorf("the second run kept %d models, want 1", got)
	}
	if res := outcomeOf(t, second, "acme/large"); !strings.Contains(res.Detail, "already priced") {
		t.Errorf("the second run does not say why it left the price alone: %q", res.Detail)
	}
}

// --force is for prices nobody has checked. A price a person confirmed is not
// one of those, and no flag makes it one.
func TestImportForceReplacesUnverifiedAndReportsWhatItSkipped(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")

	// Two stored prices that both differ from the reference: one nobody has
	// looked at, one somebody confirmed.
	for _, seed := range []struct {
		slug     string
		verified bool
	}{{"acme/large", false}, {"acme/embed", true}} {
		var verifiedAt any
		if seed.verified {
			verifiedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO model_pricing (model_id, billing_mode,
				upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
				upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
				source_name, verified_at)
			SELECT id, 'paid', 111, 222, 333, 444, 'typed in by hand', $2
			FROM models WHERE slug = $1`, seed.slug, verifiedAt); err != nil {
			t.Fatalf("seed a stored price for %s: %v", seed.slug, err)
		}
	}

	rep := runImport(t, pool, true)
	if got := rep.Count(gwstaffapi.ImportUpdated); got != 1 {
		t.Errorf("--force replaced %d prices, want 1 (%+v)", got, rep.Results)
	}
	if got := rep.Count(gwstaffapi.ImportVerified); got != 1 {
		t.Errorf("--force reported %d verified prices, want 1", got)
	}
	skipped := outcomeOf(t, rep, "acme/embed")
	if skipped.Outcome != gwstaffapi.ImportVerified {
		t.Errorf("a price checked by a person came back as %s", skipped.Outcome)
	}
	if !strings.Contains(skipped.Detail, "human") {
		t.Errorf("the report does not say why it was left alone: %q", skipped.Detail)
	}

	if got := storedPricing(t, pool, "acme/large").UpstreamInNanoPerMtok.Int64; got != 3*nanoPerUSD {
		t.Errorf("the unverified price is still %d, so --force did not replace it", got)
	}
	kept := storedPricing(t, pool, "acme/embed")
	if kept.UpstreamInNanoPerMtok.Int64 != 111 || kept.SourceName != "typed in by hand" {
		t.Errorf("the verified price was overwritten: %+v", kept)
	}

	// Running --force again changes nothing: the rates now equal the reference.
	again := runImport(t, pool, true)
	if got := again.Count(gwstaffapi.ImportUpdated); got != 0 {
		t.Errorf("a second --force run replaced %d prices, want 0", got)
	}
	if got := again.Count(gwstaffapi.ImportUnchanged); got != 1 {
		t.Errorf("a second --force run reported %d unchanged, want 1", got)
	}
}

// Replacing a base rate replaces the rates that were set against it.
//
// A per-dimension rate is entered in proportion to the base rate beside it --
// twice the base above 200k, half the base for batch -- so a base rate that
// moves and a band that does not leaves the model multiplying by a number that
// no longer exists. Measured: a hand-entered base of 99 USD/M with a 198 USD/M
// long-context band, replaced by a reference base of 3 USD/M, used to leave the
// band at 198 -- a step of sixty-six times rather than two, with nothing in the
// report or on the row saying so.
//
// The counts are asserted in three places because each answers a different
// question later: the rows are gone, the operator was told, and the row itself
// records it.
func TestImportForceAlsoReplacesTheRatesSetAgainstTheOldBase(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")

	var modelID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM models WHERE slug='acme/large'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	const handEntered = 99 * nanoPerUSD
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			source_name, reason)
		VALUES ($1,'paid',$2,$2,$2,$2,'typed in by hand','')`, modelID, int64(handEntered)); err != nil {
		t.Fatal(err)
	}
	// Twice the hand-entered base, above 200k, on both the input and output
	// buckets -- the shape the long-context axis exists for.
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_price_dimension_rates
			(model_id, bucket, service_tier, variant, min_input_tokens, nano_per_mtok)
		VALUES ($1,'in','standard','',200001,$2), ($1,'out','standard','',200001,$2)`,
		modelID, int64(2*handEntered)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_price_tool_rates (model_id, tool, nano_per_call)
		VALUES ($1,'web_search',$2)`, modelID, int64(50*nanoPerUSD)); err != nil {
		t.Fatal(err)
	}

	rep := runImport(t, pool, true)
	res := outcomeOf(t, rep, "acme/large")
	if res.Outcome != gwstaffapi.ImportUpdated {
		t.Fatalf("outcome %s, want %s", res.Outcome, gwstaffapi.ImportUpdated)
	}
	row := storedPricing(t, pool, "acme/large")
	if row.UpstreamInNanoPerMtok.Int64 != 3*nanoPerUSD {
		t.Fatalf("the base rate is %d, so --force did not replace it", row.UpstreamInNanoPerMtok.Int64)
	}

	var bands, tools int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_dimension_rates WHERE model_id=$1`, modelID).Scan(&bands); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_tool_rates WHERE model_id=$1`, modelID).Scan(&tools); err != nil {
		t.Fatal(err)
	}
	if bands != 0 || tools != 0 {
		t.Errorf(
			"%d dimension and %d tool rates survived the replacement; a 198 USD/M band "+
				"against a 3 USD/M base is a sixty-six-times step nobody chose", bands, tools)
	}

	// Told, not merely done. A replacement an operator cannot see is how a
	// hand-entered rate disappears without anybody noticing it was ever there.
	if !strings.Contains(res.Detail, "2 advanced rates") ||
		!strings.Contains(res.Detail, "1 tool rate") {
		t.Errorf("the report does not say what was dropped: %q", res.Detail)
	}
	// And recorded, because the report lasts as long as the scrollback.
	var prov map[string]any
	if err := json.Unmarshal(row.Provenance, &prov); err != nil {
		t.Fatalf("provenance is not readable: %v", err)
	}
	if prov["dropped_dimension_rates"] != float64(2) || prov["dropped_tool_rates"] != float64(1) {
		t.Errorf("provenance does not record the replaced rates: %v", prov)
	}
	if !strings.Contains(row.Reason, "dropped") {
		t.Errorf("the stored reason does not mention the replacement: %q", row.Reason)
	}
}

// The counterpart: a run that writes no new base rate must not touch the rates
// that hang off the one already there.
//
// Both halves matter. `kept` and `unchanged` leave the stored base alone, so
// the bands beside it are still anchored to the number they were entered
// against; clearing them there would delete a rate the operator entered and
// replace nothing.
func TestImportLeavesRatesAloneWhenItWritesNoBaseRate(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")

	// Exactly the reference rates, so --force reports `unchanged` rather than
	// replacing anything.
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			source_name, reason)
		SELECT id, 'paid', $1, $2, $3, $4, 'typed in by hand', '' FROM models WHERE slug='acme/large'`,
		int64(3*nanoPerUSD), int64(15*nanoPerUSD), int64(0.3*nanoPerUSD), int64(3.75*nanoPerUSD),
	); err != nil {
		t.Fatal(err)
	}
	var modelID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM models WHERE slug='acme/large'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_price_dimension_rates
			(model_id, bucket, service_tier, variant, min_input_tokens, nano_per_mtok)
		VALUES ($1,'in','standard','',200001,$2)`, modelID, int64(6*nanoPerUSD)); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		rep := runImport(t, pool, force)
		res := outcomeOf(t, rep, "acme/large")
		var bands int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM model_price_dimension_rates WHERE model_id=$1`,
			modelID).Scan(&bands); err != nil {
			t.Fatal(err)
		}
		if bands != 1 {
			t.Errorf(
				"--force=%v reported %s and left %d bands; a run that writes no base rate "+
					"has replaced nothing for them to be stale against", force, res.Outcome, bands)
		}
		if strings.Contains(res.Detail, "dropped") {
			t.Errorf("--force=%v claims it dropped something: %q", force, res.Detail)
		}
	}
}

// A zero written for a bucket the dataset does not price is a price, and the
// price is free.
//
// It is right far more often than not -- an embedding model really has no
// output tokens -- but not always: the dataset prices cache reads for one entry
// of a model and not for another, and cached input is subtracted out of the
// ordinary input count before billing, so a zero there is not "charged as
// input", it is charged nothing. The report and the row have to say that in
// those terms, because "zero written for cache_read" reads as bookkeeping and
// this is a bill.
func TestImportSaysDefaultedBucketsAreChargedNothing(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")

	rep := runImport(t, pool, false)
	res := outcomeOf(t, rep, "acme/embed")
	if !strings.Contains(res.Detail, "charged nothing") {
		t.Errorf(
			"the report says %q -- it names the buckets but not what the zero costs", res.Detail)
	}

	row := storedPricing(t, pool, "acme/embed")
	var prov map[string]any
	if err := json.Unmarshal(row.Provenance, &prov); err != nil {
		t.Fatalf("provenance is not readable: %v", err)
	}
	effect, _ := prov["defaulted_buckets_effect"].(string)
	if !strings.Contains(effect, "charged nothing") {
		t.Errorf("provenance does not state what the defaulted zero costs: %v", prov)
	}
	for _, bucket := range []string{"cache_read", "cache_write", "output"} {
		if !strings.Contains(effect, bucket) {
			t.Errorf("provenance does not name %s among the zero-rated buckets: %q", bucket, effect)
		}
	}

	// A model with every bucket priced says nothing about defaults, so the
	// sentence above only appears where it is true.
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	full := outcomeOf(t, runImport(t, pool, false), "acme/large")
	if strings.Contains(full.Detail, "charged nothing") {
		t.Errorf("a fully priced model should not mention defaulted buckets: %q", full.Detail)
	}
	var fullProv map[string]any
	if err := json.Unmarshal(storedPricing(t, pool, "acme/large").Provenance, &fullProv); err != nil {
		t.Fatal(err)
	}
	if _, ok := fullProv["defaulted_buckets_effect"]; ok {
		t.Errorf("a fully priced model carries a defaulted-bucket note: %v", fullProv)
	}
}

// Every model the import declined to price is named with a reason. A silent
// omission here recreates the exact situation the command exists to end.
func TestImportNamesEveryModelItDidNotPrice(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/unknown", "acme-not-in-the-dataset")

	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO models (slug) VALUES ('acme/routeless')`); err != nil {
		t.Fatal(err)
	}

	rep := runImport(t, pool, false)
	if got := rep.Count(gwstaffapi.ImportSkipped); got != 2 {
		t.Fatalf("skipped %d models, want 2 (%+v)", got, rep.Results)
	}
	unknown := outcomeOf(t, rep, "acme/unknown")
	if !strings.Contains(unknown.Detail, "acme-not-in-the-dataset") {
		t.Errorf("the reason does not name the upstream id that was looked up: %q", unknown.Detail)
	}
	routeless := outcomeOf(t, rep, "acme/routeless")
	if !strings.Contains(routeless.Detail, "no enabled route") {
		t.Errorf("a model nothing serves should say so: %q", routeless.Detail)
	}
}

// Two providers pricing the same model differently is not a tie to be broken.
func TestImportReportsRoutesThatDisagreeInsteadOfPicking(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	seedProviderModelRoute(t, pool, "other", "https://api.other.test", "acme/large", "acme-large")

	rep := runImport(t, pool, false)
	res := outcomeOf(t, rep, "acme/large")
	if res.Outcome != gwstaffapi.ImportSkipped {
		t.Fatalf("outcome %s, want %s", res.Outcome, gwstaffapi.ImportSkipped)
	}
	if !strings.Contains(res.Detail, "disagree") {
		t.Errorf("the reason does not say the routes disagree: %q", res.Detail)
	}
	var priced bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM model_pricing)`).Scan(&priced); err != nil {
		t.Fatal(err)
	}
	if priced {
		t.Error("a price was written despite the disagreement")
	}
}

// What an imported row has to be able to say for itself afterwards.
func TestImportedRowIsUnverifiedUnattributedAndTraceable(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")
	runImport(t, pool, false)

	row := storedPricing(t, pool, "acme/embed")
	if row.VerifiedAt.Valid {
		t.Error("verified_at was filled in; that field means a person checked the number")
	}
	if row.UpdatedBy.Valid {
		t.Error("updated_by names somebody, but nobody signed in to run this")
	}
	if row.SourceName != "test-prices" || row.SourceUrl != "https://example.test/prices" {
		t.Errorf("source %q / %q does not identify the dataset", row.SourceName, row.SourceUrl)
	}
	if row.Reason == "" {
		t.Error("reason is empty, so nothing on the row says how it got there")
	}
	var prov map[string]any
	if err := json.Unmarshal(row.Provenance, &prov); err != nil {
		t.Fatalf("provenance is not readable: %v", err)
	}
	for _, key := range []string{"maintenance", "dataset", "snapshot_date", "model_key", "matched_by"} {
		if prov[key] == nil || prov[key] == "" {
			t.Errorf("provenance has no %s: %v", key, prov)
		}
	}
	if prov["matched_by"] != "equals" || prov["model_key"] != "acme-embed" {
		t.Errorf("provenance does not identify the entry that matched: %v", prov)
	}
	// The dataset prices only this model's input, and the other three buckets
	// have to be written as an explicit zero -- recorded, so that "the dataset
	// says free" stays distinguishable from "the dataset says nothing".
	if row.UpstreamOutNanoPerMtok.Int64 != 0 || !row.UpstreamOutNanoPerMtok.Valid {
		t.Errorf("output rate %+v, want an explicit zero", row.UpstreamOutNanoPerMtok)
	}
	defaulted, _ := prov["defaulted_buckets"].([]any)
	if len(defaulted) != 3 {
		t.Errorf("provenance records %v as defaulted, want the three unpriced buckets", prov["defaulted_buckets"])
	}
}

// The acceptance run, in the order an operator actually does it: a provider,
// then a route, then the import -- and only then does the model appear in the
// catalogue. Done the other way round the last step proves nothing, because the
// catalogue lists a model only when it has both a price and a usable route.
func TestImportMakesAModelReachableInTheCatalogue(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	cat := catalog.NewService(gwdb.New(pool), nil, settings.New(pool, nil, settings.NewRegistry(), nil))

	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")

	before, err := cat.PublicModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("the catalogue already lists %d models before any price exists", len(before))
	}

	rep := runImport(t, pool, false)
	if got := rep.Count(gwstaffapi.ImportPriced); got != 1 {
		t.Fatalf("priced %d models, want 1 (%+v)", got, rep.Results)
	}

	after, err := cat.PublicModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Slug != "acme/large" {
		t.Fatalf("the catalogue lists %+v, want just acme/large", after)
	}
	if after[0].PriceIn != 3*nanoPerUSD {
		t.Errorf("the listed input rate is %d, want 3 USD/M in nano", after[0].PriceIn)
	}
}

// Without an active default plan every priced model still fails closed at
// request time, so the import says so instead of writing rows that cannot be
// charged against -- and it says so before writing any of them.
func TestImportRefusesWithoutAnActiveDefaultPlan(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	// A CHECK forbids disabling the default plan in place, which is the
	// mechanism working; deleting it reaches the same state -- no active
	// default -- which is what the import has to notice.
	if _, err := pool.Exec(ctx, `DELETE FROM pricing_plans WHERE is_default`); err != nil {
		t.Fatal(err)
	}

	_, err := gwstaffapi.ImportReferencePrices(ctx, gwstaffapi.ReferencePriceImportConfig{
		Pool: pool, Options: gwstaffapi.ImportOptions{Data: testDataset(t)},
	})
	if err == nil {
		t.Fatal("the import ran without an active default pricing plan")
	}
	if !strings.Contains(err.Error(), "default pricing plan") {
		t.Errorf("the failure does not say what is missing: %v", err)
	}
	var priced bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_pricing)`).Scan(&priced); err != nil {
		t.Fatal(err)
	}
	if priced {
		t.Error("rows were written before the check refused the run")
	}
}

// A provider whose name is not in the dataset is still recognised by its
// endpoint, which is what makes the import useful to anyone who named their
// provider after themselves rather than after the vendor.
func TestImportRecognisesAProviderByItsEndpoint(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "my-relay", "https://api.acme.test/v1", "acme/large", "acme-large")

	rep := runImport(t, pool, false)
	if res := outcomeOf(t, rep, "acme/large"); res.Outcome != gwstaffapi.ImportPriced {
		t.Fatalf("outcome %s (%s)", res.Outcome, res.Detail)
	}
}

// The vendor identifies the platform when neither the provider's name nor its
// endpoint can.
//
// Both existing signals are properties of how this deployment was set up: a
// provider called "internal-relay" behind a private hostname resolves against
// nothing, and the import then reports every one of its models as unmatched.
// The vendor is the operator saying which platform it is, which is exactly the
// question the price dataset is indexed by.
func TestImportScopesByVendorWhenTheNameAndEndpointCannot(t *testing.T) {
	pool := testpg.Start(t)
	// A name the dataset has never heard of, behind a hostname no api_pattern
	// claims. Only the vendor says this is DeepSeek.
	seedProviderModelRouteAsVendor(t, pool, "deepseek",
		"internal-relay", "https://relay.internal", "acme/large", "acme-large")

	rep := runImport(t, pool, false)
	res := outcomeOf(t, rep, "acme/large")
	if res.Outcome != gwstaffapi.ImportPriced {
		t.Fatalf("outcome %s (%s)", res.Outcome, res.Detail)
	}
	// The price has to come from the vendor's own entry, not from whichever
	// other entry happens to carry a model of the same name: pricing against
	// the wrong platform is a wrong number that looks entirely plausible.
	var input int64
	if err := pool.QueryRow(context.Background(),
		`SELECT upstream_in_nano_per_mtok FROM model_pricing mp
		 JOIN models m ON m.id = mp.model_id WHERE m.slug = 'acme/large'`).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if want := int64(7 * nanoPerUSD); input != want {
		t.Errorf("input price = %d, want %d (DeepSeek's entry, not another platform's)", input, want)
	}
}

// A dry run decides everything and keeps nothing.
//
// The comparison is the whole row rendered as text, not a chosen handful of
// columns: "the database is untouched" is a claim about every column, and a dry
// run that quietly refreshed updated_at or provenance would pass a comparison of
// the four rates while being false.
//
// The second half is what makes the first half mean anything. A test that only
// asserts "nothing changed" also passes when the import does nothing at all --
// against an empty catalogue, a broken lookup, a dataset that failed to parse.
// So the same options are then run for real and the snapshot has to move.
func TestImportDryRunDecidesEverythingAndKeepsNothing(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")

	// One model with no price at all, one with an unverified price that differs
	// from the reference: between them they cover both writing outcomes.
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			source_name)
		SELECT id, 'paid', 111, 222, 333, 444, 'typed in by hand'
		FROM models WHERE slug = 'acme/embed'`); err != nil {
		t.Fatalf("seed a stored price: %v", err)
	}

	before := pricingRows(t, pool)
	if len(before) != 1 {
		t.Fatalf("the fixture has %d price rows, want 1", len(before))
	}

	dry := runImportWith(t, pool, gwstaffapi.ImportOptions{Force: true, DryRun: true})
	if !dry.DryRun {
		t.Error("the report does not say it was a dry run, so a reader cannot tell")
	}
	if got := outcomeOf(t, dry, "acme/large").Outcome; got != gwstaffapi.ImportPriced {
		t.Errorf("acme/large came back as %s, want %s", got, gwstaffapi.ImportPriced)
	}
	if got := outcomeOf(t, dry, "acme/embed").Outcome; got != gwstaffapi.ImportUpdated {
		t.Errorf("acme/embed came back as %s, want %s", got, gwstaffapi.ImportUpdated)
	}
	if after := pricingRows(t, pool); !slices.Equal(after, before) {
		t.Errorf("the dry run changed the stored rows.\nbefore: %v\nafter:  %v", before, after)
	}

	// The reverse probe: the same run, allowed to keep what it decided.
	applied := runImportWith(t, pool, gwstaffapi.ImportOptions{Force: true})
	if got := applied.Count(gwstaffapi.ImportPriced) + applied.Count(gwstaffapi.ImportUpdated); got != 2 {
		t.Fatalf("the real run wrote %d prices, want 2 (%+v)", got, applied.Results)
	}
	stored := pricingRows(t, pool)
	if len(stored) != 2 {
		t.Fatalf("after the real run there are %d price rows, want 2", len(stored))
	}
	if slices.Equal(stored, before) {
		t.Error("the real run left the rows byte for byte identical, so the dry-run " +
			"comparison above was never able to fail")
	}
}

// A dry run reaches the same verdict the real one does, refusals included.
//
// This is what decides whether a preview is worth having, and it holds because
// the dry run really performs the write and rolls it back: the completeness
// rule, the risk assessment and every column CHECK run against it. A preview
// that predicted the outcome by other means would agree here and drift later,
// and the direction of that drift is always "the preview looked fine".
func TestImportDryRunAgreesWithTheRealRunOutcomeForOutcome(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")
	// A model the dataset does not know and a model nothing serves: both are
	// refusals, and a preview that hid them would be the very failure this
	// command exists to remove.
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/mystery", "not-in-the-dataset")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO models (slug) VALUES ('acme/routeless')`); err != nil {
		t.Fatal(err)
	}

	dry := runImportWith(t, pool, gwstaffapi.ImportOptions{DryRun: true})
	real := runImportWith(t, pool, gwstaffapi.ImportOptions{})
	if len(dry.Results) != len(real.Results) {
		t.Fatalf("the dry run covered %d models and the real one %d",
			len(dry.Results), len(real.Results))
	}
	for i, want := range real.Results {
		if got := dry.Results[i]; got != want {
			t.Errorf("model %d: the dry run said %+v, the real run did %+v", i, got, want)
		}
	}
}

// Narrowing the run to named models is what lets an interface offer "price the
// models I just wired up" without that button also repricing the catalogue.
func TestImportNarrowedToNamedModelsLeavesTheRestAlone(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")

	// An unnarrowed run prices both, which is what makes the narrowed run below
	// a statement about the narrowing rather than about the lookup.
	wide := runImportWith(t, pool, gwstaffapi.ImportOptions{DryRun: true})
	if got := wide.Count(gwstaffapi.ImportPriced); got != 2 {
		t.Fatalf("an unnarrowed run would price %d models, want 2 (%+v)", got, wide.Results)
	}

	rep := runImportWith(t, pool, gwstaffapi.ImportOptions{
		Models: []uuid.UUID{modelID(t, pool, "acme/large")},
	})
	if len(rep.Results) != 1 {
		t.Fatalf("the narrowed run covered %d models, want 1 (%+v)", len(rep.Results), rep.Results)
	}
	if got := outcomeOf(t, rep, "acme/large").Outcome; got != gwstaffapi.ImportPriced {
		t.Errorf("the named model came back as %s", got)
	}
	if got := pricedSlugs(t, pool); !slices.Equal(got, []string{"acme/large"}) {
		t.Errorf("prices exist for %v, want only the model that was named", got)
	}
}

// An id naming no model is reported rather than dropped. A caller's list goes
// stale in the ordinary course of things -- a model deleted, or one created
// earlier in the same request whose creation failed -- and a run that silently
// considers less than it was asked to leaves the caller believing otherwise.
func TestImportNamesTheIdsItCouldNotFind(t *testing.T) {
	pool := testpg.Start(t)
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	ghost := uuid.MustParse("00000000-0000-7000-8000-0000000000ff")

	rep := runImportWith(t, pool, gwstaffapi.ImportOptions{
		Models: []uuid.UUID{modelID(t, pool, "acme/large"), ghost},
	})
	if len(rep.Results) != 2 {
		t.Fatalf("the run reported %d results, want 2 (%+v)", len(rep.Results), rep.Results)
	}
	res := outcomeOf(t, rep, ghost.String())
	if res.Outcome != gwstaffapi.ImportSkipped {
		t.Errorf("an id matching no model came back as %s", res.Outcome)
	}
	if !strings.Contains(res.Detail, "no model with this id") {
		t.Errorf("the reason does not say what was wrong with it: %q", res.Detail)
	}
}

// A run somebody signed in for records who they were; one nobody signed in for
// records the blank. Neither ever records a verification date -- that field
// means a person compared the number against the vendor's list, which is not
// what either of them did.
func TestImportRecordsTheActorWhenThereIsOneAndNeverAVerificationDate(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/large", "acme-large")
	seedProviderModelRoute(t, pool, "acme", "https://api.acme.test", "acme/embed", "acme-embed")

	var actor pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO staff_users (email, password_hash, name, role)
		VALUES ('importer@example.test', 'x', 'Importer', 'superadmin') RETURNING id`).
		Scan(&actor); err != nil {
		t.Fatalf("seed a staff identity: %v", err)
	}

	runImportWith(t, pool, gwstaffapi.ImportOptions{
		Models: []uuid.UUID{modelID(t, pool, "acme/large")}, Actor: actor,
	})
	runImportWith(t, pool, gwstaffapi.ImportOptions{
		Models: []uuid.UUID{modelID(t, pool, "acme/embed")},
	})

	signed := storedPricing(t, pool, "acme/large")
	if !signed.UpdatedBy.Valid || signed.UpdatedBy != actor {
		t.Errorf("updated_by is %+v, want the identity that asked for the run", signed.UpdatedBy)
	}
	if signed.VerifiedAt.Valid {
		t.Error("a signed-in import filled in verified_at; pressing a button is not checking a rate")
	}
	if unsigned := storedPricing(t, pool, "acme/embed"); unsigned.UpdatedBy.Valid {
		t.Error("updated_by names somebody for a run nobody signed in to")
	}
}

// pricedSlugs lists the models that have a stored price, in slug order.
func pricedSlugs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT m.slug FROM model_pricing mp JOIN models m ON m.id = mp.model_id ORDER BY m.slug`)
	if err != nil {
		t.Fatalf("list the priced models: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan a slug: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the priced models: %v", err)
	}
	return out
}
