package gwdb_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/foundation/testutil/testx"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// A route is one deployment of a model on one provider. It declares nothing
// about which endpoints it serves: model_route_probes does, and two readers
// use that table with two different thresholds. The data plane tries a route
// for an endpoint unless a probe has found it unsupported; the catalog lists
// an endpoint only when a probe has found it working. This test runs the two
// generated queries side by side, because the gap between "callable" and
// "listed" is the design, not an accident.
func TestVerifiedEndpointsArePublishedAndOnlyUnsupportedOnesAreSkipped(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	q := gwdb.New(pool)

	provA := seedProvider(t, pool, "prov-a", "openai")
	provB := seedProvider(t, pool, "prov-b", "openai")
	model := seedModel(t, pool, "openai/gpt-5.4")

	// Route A has chat verified and nothing said about images.
	routeA := catalogtest.SeedRoute(t, pool, model, provA, "gpt-5.4", "chat")

	if got := publishedEndpoints(t, q, model); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("with only chat verified the catalog lists chat alone: %v", got)
	}
	if n := candidateCount(t, q, model, "openai", "images"); n != 1 {
		t.Fatalf("an endpoint nobody has ruled out is still a candidate: %d", n)
	}

	// A verdict of unsupported removes the candidate; a failed one does not.
	catalogtest.SeedVerdict(t, pool, routeA, "images", "unsupported")
	catalogtest.SeedVerdict(t, pool, routeA, "embeddings", "failed")
	if n := candidateCount(t, q, model, "openai", "images"); n != 0 {
		t.Fatalf("an endpoint found unsupported has no candidate: %d", n)
	}
	if n := candidateCount(t, q, model, "openai", "embeddings"); n != 1 {
		t.Fatalf("an inconclusive verdict must not remove the candidate: %d", n)
	}
	if got := publishedEndpoints(t, q, model); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("neither verdict adds to the published set: %v", got)
	}

	// Route B has images verified: the published set widens, and images
	// routes to B alone because A's verdict still stands.
	catalogtest.SeedRoute(t, pool, model, provB, "gpt-image-2", "images")
	if got := publishedEndpoints(t, q, model); !slices.Equal(got, []string{"chat", "images"}) {
		t.Fatalf("the published set should hold both chat and images: %v", got)
	}
	if n := candidateCount(t, q, model, "openai", "images"); n != 1 {
		t.Fatalf("images should route to route B alone: %d", n)
	}

	// Disabling B leaves images unpublished and with no candidate: both
	// readers track the enabled flag.
	if _, err := pool.Exec(ctx, `UPDATE model_routes SET enabled = false WHERE provider_id = $1`, provB); err != nil {
		t.Fatal(err)
	}
	if got := publishedEndpoints(t, q, model); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("a disabled route publishes nothing: %v", got)
	}
	if n := candidateCount(t, q, model, "openai", "images"); n != 0 {
		t.Fatalf("after disabling B, images should have no candidate: %d", n)
	}

	// Protocols do not cross: neither provider speaks anthropic, so the
	// messages surface has no candidate. Nothing here translates.
	if n := candidateCount(t, q, model, "anthropic", "messages"); n != 0 {
		t.Fatalf("a surface must filter candidates by the provider's protocols: %d", n)
	}
	// A verdict for a protocol the provider no longer speaks is invisible to
	// the catalog, whatever it says.
	catalogtest.SeedVerdict(t, pool, routeA, "messages", "ok")
	if got := publishedEndpoints(t, q, model); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("a verdict on a protocol the provider does not speak must not be published: %v", got)
	}
}

// The probe table is the only record of what a route serves, so its CHECKs
// are the last line of defence on the vocabulary: an unknown endpoint, an
// unknown status, an unknown source or an unknown protocol leaves both readers
// with nothing to stand on.
func TestRouteProbeConstraints(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	prov := seedProvider(t, pool, "prov-c", "openai")
	model := seedModel(t, pool, "openai/gpt-5.5")
	route := catalogtest.SeedRoute(t, pool, model, prov, "x")

	// The control: a well-formed row inserts, so the rejections below are
	// the CHECKs' doing and not a missing column.
	if _, err := pool.Exec(ctx, `INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode, status, source)
		VALUES ($1, 'responses', 'openai', 'auto', 'ok', 'probe')`, route); err != nil {
		t.Fatalf("a well-formed probe row should insert: %v", err)
	}
	for _, bad := range []struct{ endpoint, protocol, mode, status, source string }{
		{"bogus", "openai", "auto", "ok", "probe"},
		{"chat", "openai", "auto", "declared", "probe"},
		{"chat", "openai", "auto", "ok", "traffic"},
		{"chat", "http", "auto", "ok", "probe"},
		{"chat", "openai", "derived", "ok", "probe"},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode, status, source)
			VALUES ($1, $2, $3, $4, $5, $6)`, route, bad.endpoint, bad.protocol, bad.mode, bad.status, bad.source)
		if err == nil {
			t.Fatalf("an invalid probe row should be rejected: %+v", bad)
		}
	}
	// The route itself carries no endpoint declaration any more.
	if _, err := pool.Exec(ctx, `INSERT INTO model_routes (model_id, provider_id, provider_model_id, endpoints)
		VALUES ($1, $2, 'y', ARRAY['chat'])`, model, prov); err == nil {
		t.Fatal("model_routes.endpoints no longer exists, so a write using it must fail, but it inserted")
	}
}

// The constraint on providers.protocols.
//
// The only guard against an empty set is the providers_protocols_check
// CHECK constraint. Measured, not assumed: disabling the trigger inside a
// transaction and inserting an empty array reports that constraint, not a
// not-null violation. It is not redundant with anything.
func TestProviderProtocolsConstraint(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	// One provider row declaring two dialects is the shape this schema is for.
	var protocols []string
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('dual', 'custom', ARRAY['openai','anthropic'], 'https://u') RETURNING protocols`).
		Scan(&protocols); err != nil {
		t.Fatalf("a provider with several dialects should insert: %v", err)
	}
	if len(protocols) != 2 {
		t.Errorf("protocols should keep both dialects: %v", protocols)
	}

	// Only protocols belongs to the baseline schema. Writes naming any retired
	// predecessor must fail instead of being silently accepted.
	//
	// Both names are checked because both existed: `family` was the single-value
	// column, `families` the array that replaced it and that `protocols` replaced
	// in turn. Naming a column that never existed here would make the assertion
	// unfalsifiable -- the INSERT would fail for the wrong reason and this test
	// would guard nothing.
	for _, column := range []string{"family", "families"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO providers (slug, vendor, `+column+`, base_url) VALUES ('obsolete-column', 'custom', 'openai', 'https://u')`,
		); err == nil {
			t.Errorf("providers.%s no longer exists, so a write using it must fail, but it inserted", column)
		}
	}

	// Both an empty set and an unknown dialect must be rejected. The CHECK is
	// the only guard here.
	// gemini is a protocol this gateway serves, so it belongs on the accepted
	// side; the rejected set is an empty declaration and a name nothing speaks.
	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('gem', 'google', ARRAY['gemini'], 'https://u')`); err != nil {
		t.Errorf("a Gemini-protocol provider should insert: %v", err)
	}
	for _, bad := range [][]string{{}, {"openai", "bogus"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO providers (slug, vendor, protocols, base_url)
			 VALUES ('p-'||substr(md5(random()::text),1,8), 'custom', $1, 'https://u')`, bad); err == nil {
			t.Errorf("an invalid protocols value should be rejected: %v", bad)
		}
	}
}

// providers.vendor: the column constrains the shape of a slug and nothing else.
// Which slugs exist is decided by the registry in the code, because that list
// ships with the binary -- a database enumeration would need a migration per
// platform, and would let a deployment hold a vendor its own code cannot
// resolve.
func TestProviderVendorConstraint(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	for _, ok := range []string{"openai", "google-vertex", "aws-bedrock", "custom"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO providers (slug, vendor, protocols, base_url)
			 VALUES ('p-'||substr(md5(random()::text),1,8), $1, ARRAY['openai'], 'https://u')`,
			ok); err != nil {
			t.Errorf("vendor %q should be accepted: %v", ok, err)
		}
	}

	// Upper case, spaces, leading or doubled separators: all of them would make
	// the value that identifies a platform depend on how it was typed, and the
	// registry lookup is exact.
	for _, bad := range []string{"OpenAI", "open ai", "-openai", "openai-", "openai--x", "",
		strings.Repeat("x", 41)} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO providers (slug, vendor, protocols, base_url)
			 VALUES ('p-'||substr(md5(random()::text),1,8), $1, ARRAY['openai'], 'https://u')`,
			bad); err == nil {
			t.Errorf("vendor %q should be rejected", bad)
		}
	}

	// No default: a row that never states its vendor would be a provider whose
	// organization credentials, price scope and discovery behaviour were decided by
	// whatever the column happened to fall back to.
	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (slug, protocols, base_url)
		 VALUES ('no-vendor', ARRAY['openai'], 'https://u')`); err == nil {
		t.Error("a provider without a vendor should be rejected, but it inserted")
	}
}

// A header mapping is a flat key-to-string table. The values have to be
// strings: anything else either breaks the injection layer or makes it drop
// the header silently, and a dropped header shows up as an upstream auth
// failure that is hard to trace back.
func TestHeaderMappingShape(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	ok := []string{`{}`, `{"api-key":"${api_key}"}`, `{"Authorization":""}`, `{"X-Title":"fairlb","HTTP-Referer":"https://x"}`}
	for _, h := range ok {
		if _, err := pool.Exec(ctx, `INSERT INTO providers (slug, vendor, protocols, base_url, headers)
			VALUES ('p-'||substr(md5(random()::text),1,8), 'custom', ARRAY['openai'], 'https://u', $1)`, h); err != nil {
			t.Fatalf("a valid header mapping should be accepted %s: %v", h, err)
		}
	}

	bad := []string{`{"x":1}`, `{"x":null}`, `{"x":{"y":"z"}}`, `{"x":["a"]}`, `[]`, `"s"`}
	for _, h := range bad {
		_, err := pool.Exec(ctx, `INSERT INTO providers (slug, vendor, protocols, base_url, headers)
			VALUES ('p-'||substr(md5(random()::text),1,8), 'custom', ARRAY['openai'], 'https://u', $1)`, h)
		if err == nil {
			t.Fatalf("an invalid header mapping should be rejected: %s", h)
		}
	}
}

func seedProvider(t *testing.T, pool *pgxpool.Pool, slug, protocol string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1, 'custom', ARRAY[$2], 'https://upstream.test') RETURNING id`,
		slug, protocol).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedModel(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO models (slug) VALUES ($1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// publishedEndpoints is what the admin listing -- and, by the same subquery,
// the public catalog -- says the model serves. The generated query is used
// rather than a copy of its SQL, so that this test cannot drift from it.
func publishedEndpoints(t *testing.T, q *gwdb.Queries, model string) []string {
	t.Helper()
	row, err := q.GetModelForAdmin(context.Background(), testx.MustUUID(t, model))
	if err != nil {
		t.Fatal(err)
	}
	return row.Endpoints
}

// candidateCount is the request path's own candidate query.
func candidateCount(t *testing.T, q *gwdb.Queries, model, protocol, endpoint string) int {
	t.Helper()
	rows, err := q.ListRoutesForModel(context.Background(), gwdb.ListRoutesForModelParams{
		ModelID: testx.MustUUID(t, model), Protocol: protocol, Endpoint: endpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// The worker's upsert carries the verdict rules, so every writer obeys them.
// Three of them are tested against the generated query rather than against
// the worker, because the worker is one writer of several.
func TestSaveRouteProbeVerdictRules(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	q := gwdb.New(pool)
	prov := seedProvider(t, pool, "prov-v", "openai")
	model := seedModel(t, pool, "openai/verdicts")
	route := catalogtest.SeedRoute(t, pool, model, prov, "x", "chat")
	routeID := testx.MustUUID(t, uuid.UUID(route.Bytes).String())

	save := func(status string, code int) string {
		t.Helper()
		got, err := q.SaveRouteProbe(ctx, gwdb.SaveRouteProbeParams{
			RouteID: routeID, Endpoint: "chat", Protocol: "openai", ProbeMode: "auto", Status: status,
			StatusCode: pgtype.Int4{Int32: int32(code), Valid: true}, Error: "probe said " + status,
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	read := func() (status string, code int, errText string) {
		t.Helper()
		var c pgtype.Int4
		if err := pool.QueryRow(ctx, `SELECT status, status_code, error FROM model_route_probes WHERE route_id = $1 AND endpoint = 'chat'`,
			route).Scan(&status, &c, &errText); err != nil {
			t.Fatal(err)
		}
		return status, int(c.Int32), errText
	}

	// An inconclusive answer never downgrades a verified endpoint, but what
	// happened is on the row: the admin page can say "verified, but the last
	// probe got a 401".
	if got := save("failed", 401); got != "ok" {
		t.Fatalf("failed must not downgrade ok, stored %q", got)
	}
	if st, code, e := read(); st != "ok" || code != 401 || e == "" {
		t.Fatalf("the failure's reading must be recorded on the ok row: %s %d %q", st, code, e)
	}
	// A single 404 is not a conclusion: the verdict stands, the 404 is noted.
	if got := save("unsupported", 404); got != "ok" {
		t.Fatalf("the first 404 against a verified endpoint must keep ok, stored %q", got)
	}
	// The confirming second sample within the window is.
	if got := save("unsupported", 404); got != "unsupported" {
		t.Fatalf("a second 404 within the hour must flip the verdict, stored %q", got)
	}
	// An endpoint that was never verified needs no second look.
	catalogtest.SeedVerdict(t, pool, route, "embeddings", "unverified")
	got, err := q.SaveRouteProbe(ctx, gwdb.SaveRouteProbeParams{
		RouteID: routeID, Endpoint: "embeddings", Protocol: "openai", ProbeMode: "auto", Status: "unsupported",
		StatusCode: pgtype.Int4{Int32: 404, Valid: true},
	})
	if err != nil || got != "unsupported" {
		t.Fatalf("an unverified endpoint is unsupported on the first 404: %q %v", got, err)
	}
	// The operator's row is not the worker's to write: the upsert touches
	// nothing and says so by returning no row.
	if err := q.SetRouteProbeOverride(ctx, gwdb.SetRouteProbeOverrideParams{
		RouteID: routeID, Endpoint: "chat", Protocol: "openai", ProbeMode: "auto", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SaveRouteProbe(ctx, gwdb.SaveRouteProbeParams{
		RouteID: routeID, Endpoint: "chat", Protocol: "openai", ProbeMode: "auto", Status: "unsupported",
		StatusCode: pgtype.Int4{Int32: 404, Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the worker must not overwrite the operator's row: %v", err)
	}
	if st, _, _ := read(); st != "ok" {
		t.Fatalf("the operator's verdict must stand: %s", st)
	}
	// A change of upstream name resets every row, the operator's included.
	if err := q.ResetRouteProbes(ctx, routeID); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT status, source FROM model_route_probes WHERE route_id = $1 AND endpoint = 'chat'`,
		route).Scan(new(string), &source); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := read(); st != "unverified" || source != "probe" {
		t.Fatalf("a rename must hand the operator's row back to the worker: %s/%s", st, source)
	}
}

// The sweeper's work list contains only rows a probe can advance: not the
// operator's, not manual endpoints, not routes on a provider the worker has
// no credential for -- and oldest first. A row that can never be advanced
// would otherwise be due forever and fill the bounded batch.
func TestSweepWorkListHoldsOnlyWhatAProbeCanAdvance(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	q := gwdb.New(pool)
	keyed := seedProvider(t, pool, "prov-keyed", "openai")
	keyless := seedProvider(t, pool, "prov-keyless", "openai")
	if _, err := pool.Exec(ctx, `INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', '\x00')`, keyed); err != nil {
		t.Fatal(err)
	}
	model := seedModel(t, pool, "openai/sweep")
	onKeyed := catalogtest.SeedRoute(t, pool, model, keyed, "a")
	onKeyless := catalogtest.SeedRoute(t, pool, model, keyless, "b")
	for _, r := range []pgtype.UUID{onKeyed, onKeyless} {
		catalogtest.SeedVerdict(t, pool, r, "chat", "unsupported")
		catalogtest.SeedVerdict(t, pool, r, "images", "unverified")
		catalogtest.SeedVerdict(t, pool, r, "embeddings", "failed")
	}
	catalogtest.SeedVerdict(t, pool, onKeyed, "responses", "ok")
	if _, err := pool.Exec(ctx, `UPDATE model_route_probes SET checked_at = now() - interval '2 days', updated_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_route_probes SET source = 'operator' WHERE route_id = $1 AND endpoint = 'embeddings'`, onKeyed); err != nil {
		t.Fatal(err)
	}

	rows, err := q.ListRouteProbesDueForReprobe(ctx, gwdb.ListRouteProbesDueForReprobeParams{
		UnverifiedAfter: pgtype.Interval{Microseconds: int64(10 * time.Minute / time.Microsecond), Valid: true},
		VerdictAfter:    pgtype.Interval{Microseconds: int64(24 * time.Hour / time.Microsecond), Valid: true},
		MaxRows:         500,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[uuid.UUID(r.RouteID.Bytes).String()+"/"+r.Endpoint] = true
	}
	keyedID := uuid.UUID(onKeyed.Bytes).String()
	if !got[keyedID+"/chat"] {
		t.Errorf("an aged unsupported verdict on a keyed provider is due: %v", got)
	}
	if got[keyedID+"/images"] {
		t.Errorf("a manual endpoint is never the sweeper's to probe: %v", got)
	}
	if got[keyedID+"/embeddings"] {
		t.Errorf("the operator's row is not the sweeper's business: %v", got)
	}
	if got[keyedID+"/responses"] {
		t.Errorf("a green verdict is not re-bought: %v", got)
	}
	for ep := range map[string]bool{"chat": true, "images": true, "embeddings": true} {
		if got[uuid.UUID(onKeyless.Bytes).String()+"/"+ep] {
			t.Errorf("a provider with no credential gives the worker nothing to probe with; its rows must not fill the batch: %v", got)
		}
	}
}

// What the catalog publishes is one rule, in the model_published_endpoints
// view: verified endpoints, plus -- on a provider the platform holds no
// credential for -- automatically probed endpoints that are merely unverified.
func TestPublishedEndpointsView(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	q := gwdb.New(pool)
	keyed := seedProvider(t, pool, "prov-keyed", "anthropic")
	byokOnly := seedProvider(t, pool, "prov-byok-only", "openai")
	if _, err := pool.Exec(ctx, `INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', '\x00')`, keyed); err != nil {
		t.Fatal(err)
	}
	model := seedModel(t, pool, "anthropic/claude-view")
	onKeyed := catalogtest.SeedRoute(t, pool, model, keyed, "claude")
	onByok := catalogtest.SeedRoute(t, pool, model, byokOnly, "claude")
	catalogtest.SeedVerdict(t, pool, onKeyed, "messages", "ok")
	catalogtest.SeedVerdict(t, pool, onKeyed, "messages_count_tokens", "unverified")
	catalogtest.SeedVerdict(t, pool, onByok, "chat", "unverified")
	catalogtest.SeedVerdict(t, pool, onByok, "images", "unverified")
	catalogtest.SeedVerdict(t, pool, onByok, "embeddings", "unsupported")

	got := publishedEndpoints(t, q, model)
	want := []string{"chat", "messages"}
	if !slices.Equal(got, want) {
		t.Fatalf("published = %v, want %v: verified on the keyed provider, unverified-but-auto on the one nobody can probe, never manual or unsupported", got, want)
	}
	// The BYOK-only provider gaining a credential turns its unverified
	// endpoints back into "not yet": the worker can look now.
	if _, err := pool.Exec(ctx, `INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', '\x00')`, byokOnly); err != nil {
		t.Fatal(err)
	}
	if got := publishedEndpoints(t, q, model); !slices.Equal(got, []string{"messages"}) {
		t.Fatalf("once a credential exists, unverified is unlisted again: %v", got)
	}
}
