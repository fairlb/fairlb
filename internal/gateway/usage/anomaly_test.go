package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Per-org spend anomaly detection.
//
// The two easiest ways to get a statistical criterion like this wrong are
// alerting on normal growth (noise) and going blind to a real anomaly after one
// spike has poisoned the baseline. Hence a median rather than a mean, and both
// a relative and an absolute condition.

type anomalyFixture struct {
	pool *pgxpool.Pool
	q    *gwdb.Queries
	org  pgtype.UUID
	key  pgtype.UUID
}

// A spike that also clears the absolute floor is a hit.
func TestAnomalyDetectsSpike(t *testing.T) {
	f := newAnomalyFixture(t)
	// Past 7 days: 1e8 (ten cents) per hour.
	f.history(t, 24, 100_000_000)
	// This hour: 5e9 (five dollars), 50 times the median.
	f.currentHour(t, 5_000_000_000)

	rows := f.detect(t, 10, 1_000_000_000)
	if len(rows) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(rows))
	}
	if rows[0].BaselineSpend != 100_000_000 {
		t.Errorf("baseline should be the median 1e8, got %d", rows[0].BaselineSpend)
	}
}

// Too small in absolute terms: no alert. A new org's baseline is zero, so a
// pure multiple would alert on their very first spend.
func TestAnomalyRespectsFloor(t *testing.T) {
	f := newAnomalyFixture(t)
	f.currentHour(t, 500_000_000) // 50 cents, below the one-dollar floor

	if rows := f.detect(t, 10, 1_000_000_000); len(rows) != 0 {
		t.Errorf("below the absolute floor there should be no alert: %+v", rows)
	}
}

// A new org with no history should still be a hit once it clears the floor --
// it may well be the one that needs attention.
func TestAnomalyCatchesNewOrgAboveFloor(t *testing.T) {
	f := newAnomalyFixture(t)
	f.currentHour(t, 5_000_000_000)

	rows := f.detect(t, 10, 1_000_000_000)
	if len(rows) != 1 {
		t.Fatalf("an org with no history but a sizeable amount should be a hit, got %d rows", len(rows))
	}
	if rows[0].BaselineSpend != 0 {
		t.Errorf("with no history the baseline should be 0, got %d", rows[0].BaselineSpend)
	}
}

// Normal growth does not alert. Otherwise the alert drowns in noise and nobody
// looks at the real anomaly.
func TestAnomalyIgnoresNormalGrowth(t *testing.T) {
	f := newAnomalyFixture(t)
	f.history(t, 24, 1_000_000_000)
	f.currentHour(t, 3_000_000_000) // 3x, short of the 10x threshold

	if rows := f.detect(t, 10, 1_000_000_000); len(rows) != 0 {
		t.Errorf("3x growth must not alert at a 10x threshold: %+v", rows)
	}
}

// The baseline is a median, not a mean: one historical spike must not raise it
// far enough to make the detector blind to a real anomaly.
func TestAnomalyBaselineResistsHistoricalSpike(t *testing.T) {
	f := newAnomalyFixture(t)
	f.history(t, 23, 100_000_000)      // 23 hours of low spend
	f.historyAt(t, 24, 50_000_000_000) // one historical spike (a mean would be dragged to ~2e9)
	f.currentHour(t, 5_000_000_000)

	rows := f.detect(t, 10, 1_000_000_000)
	if len(rows) != 1 {
		t.Fatalf("the median must not be polluted by one spike and should still be a hit, got %d rows", len(rows))
	}
	if rows[0].BaselineSpend != 100_000_000 {
		t.Errorf("baseline should be the median 1e8; a mean would be about 2e9, got %d", rows[0].BaselineSpend)
	}
}

// ===== Fixtures =====

func (f *anomalyFixture) detect(t *testing.T, multiplier, floor int64) []gwdb.DetectSpendAnomaliesRow {
	t.Helper()
	rows, err := f.q.DetectSpendAnomalies(context.Background(), gwdb.DetectSpendAnomaliesParams{
		Multiplier: multiplier, FloorNano: floor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// history writes one bucket of the same amount for each of the past n hours.
func (f *anomalyFixture) history(t *testing.T, hours int, nano int64) {
	t.Helper()
	for i := 1; i <= hours; i++ {
		f.historyAt(t, i, nano)
	}
}

// historyAt writes a single bucket n hours ago.
func (f *anomalyFixture) historyAt(t *testing.T, hoursAgo int, nano int64) {
	t.Helper()
	f.bucket(t, time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(hoursAgo)*time.Hour), nano)
}

func (f *anomalyFixture) currentHour(t *testing.T, nano int64) {
	t.Helper()
	f.bucket(t, time.Now().UTC().Truncate(time.Hour), nano)
}

func (f *anomalyFixture) bucket(t *testing.T, at time.Time, nano int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO gateway_usage_rollups
		   (org_id, bucket_start, granularity, api_key_id, model_slug, provider_id, requests, charged_nano)
		 VALUES ($1, $2, 'hour', $3, 'openai/x', gen_random_uuid(), 1, $4)`,
		f.org, at, f.key, nano); err != nil {
		t.Fatal(err)
	}
}

func newAnomalyFixture(t *testing.T) *anomalyFixture {
	t.Helper()
	pool := testpg.Start(t)
	ctx := context.Background()

	org := orgtest.Create(t, pool, orgtest.Seed{Slug: "a-org", Name: "A"})
	var key pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id, name, prefix, key_hash, scopes)
		 VALUES ($1,'k','sk-flb-v1-1','h',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return &anomalyFixture{pool: pool, q: gwdb.New(pool), org: org, key: key}
}
