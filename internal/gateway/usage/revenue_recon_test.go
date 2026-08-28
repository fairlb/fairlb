package usage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/usage"
)

// Reverse reconciliation.
//
// The nightly ledger checks ask whether the accounting is internally
// consistent; these ask whether everything served was charged for. Both
// directions need someone watching. The lesson behind this file: with only the
// first, a request that was never settled leaves the accounting perfectly
// consistent and is never found.

type reconFixture struct {
	pool *pgxpool.Pool
	org  pgtype.UUID
	key  pgtype.UUID
}

// Successful requests charged nothing must be found: that is the shared symptom
// of an unpriced model and of a missing cache-write rate.
func TestRevenueReconFindsZeroCharged(t *testing.T) {
	f := newReconFixture(t)
	f.usageLog(t, "gw_zero_1", "openai/unpriced", "ok", 0, false)
	f.usageLog(t, "gw_paid_1", "openai/priced", "ok", 12_600_000, false)

	rep := f.run(t)
	if len(rep.ZeroCharged) != 1 {
		t.Fatalf("expected 1 zero-charged combination, got %d: %+v", len(rep.ZeroCharged), rep.ZeroCharged)
	}
	if rep.ZeroCharged[0].ModelSlug != "openai/unpriced" {
		t.Errorf("wrong model reported: %s", rep.ZeroCharged[0].ModelSlug)
	}
	if rep.Clean() {
		t.Error("must not report clean while zero-charged requests exist")
	}
}

// Explicitly free is not a violation: that is this request's own declaration,
// not a misconfiguration.
//
// The criterion reads the billing mode from the request's pricing snapshot
// rather than from the model row. That is not merely a different column:
// explaining an old charge with the model's current state rewrites history
// after any paid/free switch. The snapshot is the fact of that moment, and it
// is the only thing that counts.
func TestRevenueReconIgnoresRequestsPricedAsFree(t *testing.T) {
	f := newReconFixture(t)
	f.model(t, "openai/gift")
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO usage_logs (org_id, api_key_id, request_id, surface, model_slug,
		 status, http_status, charged_nano, charged_currency, usage_estimated, pricing_snapshot)
		 VALUES ($1, $2, 'gw_free_1', 'chat_completions', 'openai/gift',
		 'ok', 200, 0, 'USD', false, '{"billing_mode":"free"}'::jsonb)`,
		f.org, f.key); err != nil {
		t.Fatal(err)
	}
	if rep := f.run(t); len(rep.ZeroCharged) != 0 {
		t.Errorf("a zero charge settled as free is not a violation: %+v", rep.ZeroCharged)
	}
}

// Old rows whose snapshot has no billing mode must surface; they may not be let
// through in silence.
//
// Falling back to "treat as free and skip" is the wrong direction: this is a
// reconciliation query, and a row we cannot classify should be visible to a
// human. Classifying them as free would make defects that silently drive a
// charge to zero disappear from the result -- which is precisely what this
// query exists to catch.
func TestRevenueReconReportsRowsWithNoBillingModeInSnapshot(t *testing.T) {
	f := newReconFixture(t)
	f.model(t, "openai/ancient")
	// usageLog writes an empty snapshot `{}` -- exactly the shape of the older
	// rows.
	f.usageLog(t, "gw_ancient_1", "openai/ancient", "ok", 0, false)

	rep := f.run(t)
	if len(rep.ZeroCharged) != 1 {
		t.Fatalf("a zero-charged row whose snapshot cannot state its billing mode must be reported: %+v", rep.ZeroCharged)
	}
	if rep.ZeroCharged[0].ModelSlug != "openai/ancient" {
		t.Errorf("the reported row is not the expected one: %+v", rep.ZeroCharged[0])
	}
}

// Failed requests are charged nothing by design (a failure before the first
// byte is free), so they must not count as missed revenue.
func TestRevenueReconIgnoresFailedRequests(t *testing.T) {
	f := newReconFixture(t)
	f.usageLog(t, "gw_fail_1", "openai/x", "upstream_error", 0, false)
	f.usageLog(t, "gw_fail_2", "openai/x", "client_error", 0, false)

	if rep := f.run(t); len(rep.ZeroCharged) != 0 {
		t.Errorf("a failed request is not missed revenue: %+v", rep.ZeroCharged)
	}
}

// Any abandoned charge at all is a violation: that money is not coming back and
// somebody has to see it.
func TestRevenueReconFlagsAbandonedUnsettled(t *testing.T) {
	f := newReconFixture(t)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO gateway_unsettled (request_id, org_id, charged_nano, currency, reason, payload, abandoned_at)
		 VALUES ('gw_dead', $1, 9_000_000, 'USD', 'retries exhausted', '{}'::jsonb, now())`,
		f.org); err != nil {
		t.Fatal(err)
	}
	rep := f.run(t)
	if rep.AbandonedUnsettled != 1 || rep.AbandonedNano != 9_000_000 {
		t.Errorf("wrong abandoned count/amount: %d / %d", rep.AbandonedUnsettled, rep.AbandonedNano)
	}
	if rep.Clean() {
		t.Error("must not report clean while an abandoned charge exists")
	}
}

func TestRevenueReconFlagsPricingMissingWithoutTreatingReserveAsCharge(t *testing.T) {
	f := newReconFixture(t)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO gateway_pricing_unsettled
		 (request_id, org_id, reserved_nano, currency, reason, payload)
		 VALUES ('gw_pricing_missing', $1, 12_000_000, 'USD', 'tool price missing', '{}'::jsonb)`,
		f.org); err != nil {
		t.Fatal(err)
	}
	rep := f.run(t)
	if rep.PricingMissing != 1 || rep.PricingReservedNano != 12_000_000 {
		t.Fatalf("wrong missing-price count/reserved amount: %d / %d", rep.PricingMissing, rep.PricingReservedNano)
	}
	if rep.Clean() {
		t.Fatal("must not report clean while a request is awaiting a price")
	}
}

// The estimated-billing share. The plan has always been "adopt a real tokenizer
// once this crosses a threshold"; this turns that never-measured threshold into
// an observable number.
func TestRevenueReconEstimatedShare(t *testing.T) {
	f := newReconFixture(t)
	// 3e8 estimated against 7e8 measured is a 30% share, past the 15%
	// threshold.
	f.usageLog(t, "gw_est_1", "openai/x", "ok", 300_000_000, true)
	f.usageLog(t, "gw_real_1", "openai/x", "ok", 700_000_000, false)

	rep := f.run(t)
	if got := rep.EstimatedShareBps(); got != 3000 {
		t.Errorf("estimated share = %d bps, want 3000", got)
	}
	if rep.Clean() {
		t.Error("must not report clean while the estimated share is over the threshold")
	}
}

// Everything normal reports clean. Otherwise the alert gets ignored as routine
// noise.
// A terminal job whose reservation never moved is the fourth way revenue goes
// missing, and the only one with no retry path behind it: a `protected` hold is
// deliberately exempt from the timeout sweep, so nothing clears it without a
// person. It therefore has to reach the alert rather than only a dashboard.
func TestRevenueReconFlagsJobsWhoseMoneyNeverMoved(t *testing.T) {
	for _, state := range []string{"held", "protected"} {
		t.Run(state, func(t *testing.T) {
			f := newReconFixture(t)
			f.seedStuckJob(t, state, "completed")

			rep := f.run(t)
			if rep.StuckJobs != 1 {
				t.Errorf("want 1 stuck job, got %d", rep.StuckJobs)
			}
			if rep.StuckOldest == nil {
				t.Error("a stuck job must carry how long it has been waiting")
			}
			if rep.Clean() {
				t.Error("must not report clean while a delivered job holds an unmoved reservation")
			}
		})
	}
}

// The states where the money did move are not findings; counting them would
// make the alert cry wolf on every healthy deployment.
func TestRevenueReconIgnoresJobsWhoseMoneyMoved(t *testing.T) {
	f := newReconFixture(t)
	for _, state := range []string{"settled", "voided", "orphaned"} {
		f.seedStuckJob(t, state, "completed")
	}
	// Still running: the reservation is in flight, not stranded.
	f.seedStuckJob(t, "held", "in_progress")

	rep := f.run(t)
	if rep.StuckJobs != 0 {
		t.Errorf("settled, voided, orphaned and in-flight jobs must not count as stuck, got %d", rep.StuckJobs)
	}
}

func TestRevenueReconCleanWhenHealthy(t *testing.T) {
	f := newReconFixture(t)
	f.usageLog(t, "gw_ok_1", "openai/x", "ok", 12_600_000, false)

	rep := f.run(t)
	if !rep.Clean() {
		t.Errorf("a normal state should report clean: %+v", rep)
	}
}

// ===== Fixtures =====

func (f *reconFixture) run(t *testing.T) usage.RevenueReconReport {
	t.Helper()
	rep, err := usage.NewRevenueReconWorker(f.pool, nil).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// model creates one catalog row. It deliberately takes no "free" flag: whether
// a request was free is decided by that request's own snapshot, and the
// reconciliation query does not join this table at all.
func (f *reconFixture) model(t *testing.T, slug string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ($1, 4096)`, slug); err != nil {
		t.Fatal(err)
	}
}

func (f *reconFixture) usageLog(t *testing.T, requestID, model, status string, charged int64, estimated bool) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO usage_logs (org_id, api_key_id, request_id, surface, model_slug,
		                         status, http_status, charged_nano, charged_currency, usage_estimated)
		 VALUES ($1, $2, $3, 'chat_completions', $4, $5, 200, $6, 'USD', $7)`,
		f.org, f.key, requestID, model, status, charged, estimated); err != nil {
		t.Fatal(err)
	}
}

func newReconFixture(t *testing.T) *reconFixture {
	t.Helper()
	pool := testpg.Start(t)
	ctx := context.Background()

	org := orgtest.Create(t, pool, orgtest.Seed{Slug: "r-org", Name: "R"})
	var key pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id, name, prefix, key_hash, scopes)
		 VALUES ($1,'k','sk-flb-v1-1','h',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return &reconFixture{pool: pool, org: org, key: key}
}

// seedStuckJob inserts one asynchronous job in a given status/settlement pair.
func (f *reconFixture) seedStuckJob(t *testing.T, settlement, status string) {
	t.Helper()
	ctx := context.Background()
	var model pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, output_modalities) VALUES ($1, ARRAY['video']) RETURNING id`,
		"acme/vid-"+settlement+"-"+status).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO gateway_async_jobs
		   (org_id, kind, request_id, idempotency_key, request_fingerprint, model_id, model_slug,
		    status, settlement_state, params, hold_nano, max_job_seconds, terminal_at, expires_at)
		 VALUES ($1, 'video', $2, $3, 'fp', $4, 'acme/vid', $5, $6, '{}', 1000, 600,
		         CASE WHEN $5 IN ('queued','in_progress') THEN NULL ELSE now() - interval '2 days' END,
		         now() + interval '1 day')`,
		f.org, "req-"+settlement+"-"+status, "idem-"+settlement+"-"+status, model, status, settlement); err != nil {
		t.Fatal(err)
	}
}
