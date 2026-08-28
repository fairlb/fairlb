package gwdb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// videoFixture builds the smallest catalog a video job can point at, and
// returns the two orgs used to prove the row is org-scoped.
type videoFixture struct {
	orgA, orgB             pgtype.UUID
	provider, model, route pgtype.UUID
	providerKey            pgtype.UUID
}

func seedVideoFixture(t *testing.T, pool *pgxpool.Pool, suffix string) videoFixture {
	t.Helper()
	ctx := context.Background()
	f := videoFixture{
		orgA: orgtest.Create(t, pool, orgtest.Seed{Slug: "vj-a-" + suffix}),
		orgB: orgtest.Create(t, pool, orgtest.Seed{Slug: "vj-b-" + suffix}),
	}
	// `video` in providers.protocols is itself part of what this pins: a
	// provider that speaks the job plane must be representable.
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('vj-provider-'||$1, 'custom', ARRAY['video'], 'https://upstream.test')
		 RETURNING id`, suffix,
	).Scan(&f.provider); err != nil {
		t.Fatalf("a provider speaking the video plane must be insertable: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug) VALUES ('google/veo-'||$1) RETURNING id`, suffix,
	).Scan(&f.model); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO model_routes (model_id, provider_id, provider_model_id, video_envelope)
		 VALUES ($1, $2, 'veo-3.1', '{"durations_seconds":[4,6,8],"cancel":"never"}'::jsonb)
		 RETURNING id`, f.model, f.provider,
	).Scan(&f.route); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc)
		 VALUES ($1, 'primary', '\x01') RETURNING id`, f.provider,
	).Scan(&f.providerKey); err != nil {
		t.Fatal(err)
	}
	return f
}

func insertJob(
	ctx context.Context, pool *pgxpool.Pool, f videoFixture, requestID, idempotencyKey string,
) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO gateway_async_jobs
		   (org_id, kind, request_id, idempotency_key, request_fingerprint,
		    model_id, model_slug, route_id, provider_id,
		    provider_key_id, status, params, hold_nano, max_job_seconds, expires_at)
		 VALUES ($1, 'video', $2, $3, 'fp', $4, 'google/veo-test', $5, $6, $7, 'queued',
		         '{"duration_seconds":8}'::jsonb, 4000000000, 900, now() + interval '7 days')
		 RETURNING id`,
		f.orgA, requestID, idempotencyKey, f.model, f.route, f.provider, f.providerKey,
	).Scan(&id)
	return id, err
}

// request_id is unique, but that is bookkeeping rather than a retry guard: it
// is minted per HTTP attempt, so two retries of one submit carry different
// values and both would insert. Naming it as the thing that stops a duplicate
// would claim a protection this column cannot give.
func TestAsyncJobRequestIDIsUnique(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	f := seedVideoFixture(t, pool, "dup")

	if _, err := insertJob(ctx, pool, f, "req-duplicate", "idem-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertJob(ctx, pool, f, "req-duplicate", "idem-b"); err == nil {
		t.Fatal("two jobs shared one request id, so a usage row could not be traced back to one job")
	}
}

// This is the guard that actually stops a retried submit from becoming a second
// paid job: the caller's own key, unique per organization, enforced by the
// database rather than by a check-then-insert two concurrent retries would both
// pass.
func TestARetriedSubmitCannotBecomeASecondPaidJob(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	f := seedVideoFixture(t, pool, "idem")

	if _, err := insertJob(ctx, pool, f, "req-first", "caller-key-1"); err != nil {
		t.Fatal(err)
	}
	// The same key on a fresh attempt -- a client retrying after a timeout.
	if _, err := insertJob(ctx, pool, f, "req-retry", "caller-key-1"); err == nil {
		t.Fatal("a retry under the same idempotency key created a second job; it would be billed twice")
	}
	// A different key is a different job, not a duplicate.
	if _, err := insertJob(ctx, pool, f, "req-other", "caller-key-2"); err != nil {
		t.Fatalf("a distinct key must create a distinct job: %v", err)
	}
}

// status and settlement_state are two columns precisely so that the settle
// path can guard on the second one. This is the shape of that guard: the
// winner updates one row, every loser updates none.
func TestAsyncJobSettlementGuardAdmitsExactlyOneWinner(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	f := seedVideoFixture(t, pool, "settle")

	id, err := insertJob(ctx, pool, f, "req-settle", "idem-settle")
	if err != nil {
		t.Fatal(err)
	}
	settle := func() int64 {
		tag, err := pool.Exec(ctx,
			`UPDATE gateway_async_jobs
			    SET settlement_state = 'settled', status = 'completed',
			        charged_nano = hold_nano, terminal_at = now()
			  WHERE id = $1 AND settlement_state = 'held'`, id)
		if err != nil {
			t.Fatal(err)
		}
		return tag.RowsAffected()
	}
	if n := settle(); n != 1 {
		t.Fatalf("first settlement must claim the job, affected %d rows", n)
	}
	if n := settle(); n != 0 {
		t.Fatalf("a duplicate poll, a racing replica or a job retry must settle nothing, affected %d rows", n)
	}
}

// A job id means nothing on a different upstream account, and it must mean
// nothing to a different organization either.
func TestAsyncJobIsOrgScoped(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	f := seedVideoFixture(t, pool, "rls")

	if _, err := insertJob(ctx, pool, f, "req-rls", "idem-rls"); err != nil {
		t.Fatal(err)
	}
	count := func(org pgtype.UUID) int {
		var n int
		orgID, _ := org.Value()
		if err := fdb.WithOrgTx(ctx, pool, orgID.(string), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM gateway_async_jobs`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(f.orgA); got != 1 {
		t.Fatalf("the owning org sees %d of its own jobs", got)
	}
	if got := count(f.orgB); got != 0 {
		t.Fatalf("another org sees %d jobs that are not its own", got)
	}
}

// One credential, not two -- the same rule resource_affinities carries, for the
// same reason: settlement has to be able to say which key actually paid.
func TestAsyncJobRefusesTwoCredentials(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	f := seedVideoFixture(t, pool, "cred")

	var orgKey pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO org_provider_keys (org_id, vendor, name, secret_enc)
		 VALUES ($1, 'custom', 'byok', '\x02') RETURNING id`, f.orgA,
	).Scan(&orgKey); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO gateway_async_jobs
		   (org_id, kind, request_id, idempotency_key, request_fingerprint,
		    model_id, model_slug, route_id, provider_id,
		    provider_key_id, org_provider_key_id, status, params, hold_nano, max_job_seconds, expires_at)
		 VALUES ($1, 'video', 'req-two-creds', 'idem-two-creds', 'fp', $2, 'google/veo-test', $3, $4, $5, $6,
		         'queued', '{}'::jsonb, 1, 900, now() + interval '1 day')`,
		f.orgA, f.model, f.route, f.provider, f.providerKey, orgKey)
	if err == nil {
		t.Fatal("a job naming both a shared and a BYOK credential was accepted")
	}
	if !strings.Contains(err.Error(), "one_credential") {
		t.Fatalf("expected the one-credential constraint to refuse it, got: %v", err)
	}
}

// The constraint this change relaxed, pinned in both directions.
//
// Before ADR-0220 a per-second model was not representable at all: four NOT
// NULL token buckets plus "not all four may be zero" is the shape of "every
// model is billed per token", and a video model has no token price to give.
// Relaxing that is easy to overshoot -- the same edit could quietly let a
// *token* model be priced at nothing, which is the state the original
// constraint existed to forbid. So both arms are asserted here.
func TestUnitPricedModelIsRepresentableAndTokenPricedZeroStillIsNot(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	newModel := func(slug string) pgtype.UUID {
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO models (slug) VALUES ($1) RETURNING id`, slug).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	price := func(model pgtype.UUID, family string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO model_pricing
			   (model_id, billing_mode, pricing_family,
			    upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			    upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok)
			 VALUES ($1, 'paid', $2, 0, 0, 0, 0)`, model, family)
		return err
	}

	video := newModel("google/veo-3.1")
	if err := price(video, "units"); err != nil {
		t.Fatalf("a per-second model must be priceable with no token rates: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_price_unit_rates (model_id, unit, resolution, audio, nano_per_unit)
		 VALUES ($1, 'second', '720p', 'on', 400000000)`, video); err != nil {
		t.Fatalf("a per-second rate must be storable: %v", err)
	}

	text := newModel("openai/gpt-zero")
	err := price(text, "tokens")
	if err == nil {
		t.Fatal("a token-priced model with four zero buckets was accepted; " +
			"'deliberately free' and 'price never filled in' are the same state again")
	}
	if !strings.Contains(err.Error(), "model_pricing_complete_ck") {
		t.Fatalf("expected the completeness constraint to refuse it, got: %v", err)
	}
}

// A video unit rate has no token fallback by design: falling back to the input
// rate would bill a generated video at a text price. Nothing in the schema
// links the two families, and that is the property worth holding still.
func TestUnitRatesAreASeparateFamilyFromTokenBuckets(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	var model pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO models (slug) VALUES ('kuaishou/kling-v3') RETURNING id`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_pricing
		   (model_id, billing_mode, pricing_family,
		    upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
		    upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok)
		 VALUES ($1, 'paid', 'units', 0, 0, 0, 0)`, model); err != nil {
		t.Fatal(err)
	}
	// A per-call unit, for upstreams selling generation packs rather than time.
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_price_unit_rates (model_id, unit, nano_per_unit)
		 VALUES ($1, 'call', 250000000)`, model); err != nil {
		t.Fatalf("a per-call rate must be storable: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_price_dimension_rates WHERE model_id = $1`, model).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pricing a unit must not populate the token dimension table, found %d rows", n)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_price_unit_rates (model_id, unit, nano_per_unit) VALUES ($1, 'mtok', 1)`, model,
	); err == nil {
		t.Fatal("a token unit was accepted into the per-unit family; the two must not converge")
	}
}

// The sweeper bounds a job by its own model's ceiling. One global interval
// either expires a long render that was about to succeed, or leaves a dead
// short job holding a reservation for hours.
func TestStaleSweepUsesEachJobsOwnCeiling(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	f := seedVideoFixture(t, pool, "stale")
	q := gwdb.New(pool)

	insert := func(requestID string, maxJobSeconds int, ageMinutes int) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO gateway_async_jobs
			   (org_id, kind, request_id, idempotency_key, request_fingerprint,
			    model_id, model_slug, route_id, provider_id, provider_key_id,
			    status, params, hold_nano, max_job_seconds, created_at, expires_at)
			 VALUES ($1, 'video', $2, $2, 'fp', $3, 'google/veo-test', $4, $5, $6,
			         'in_progress', '{}'::jsonb, 1, $7,
			         now() - make_interval(mins => $8), now() + interval '7 days')`,
			f.orgA, requestID, f.model, f.route, f.provider, f.providerKey,
			maxJobSeconds, ageMinutes); err != nil {
			t.Fatal(err)
		}
	}
	// A short-ceiling job well past twice its own limit.
	insert("short-overdue", 300, 30) // 5 min ceiling, 30 min old
	// A long-ceiling job that is older than any single global bound would allow
	// but still inside twice its own.
	insert("long-running", 7200, 150) // 2 h ceiling, 2.5 h old

	stale, err := q.ListStaleVideoJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range stale {
		got[row.RequestID] = true
	}
	if !got["short-overdue"] {
		t.Error("a job 30 minutes past a 5-minute ceiling was not swept; its reservation " +
			"stays outstanding until the billing timeout")
	}
	if got["long-running"] {
		t.Error("a long render still inside twice its own ceiling was expired; the caller " +
			"loses work the upstream was about to deliver")
	}
}
