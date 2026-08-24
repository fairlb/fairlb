package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
	publicusage "github.com/fairlb/fairlb/usage"
)

// End to end: write rows with known durations, run the aggregation, and check
// the histogram against a hand-computed distribution.
// This checks that the aggregator really folds durations into buckets; the unit
// tests only cover the interpolation algorithm itself.
func TestAggregatorFillsLatencyHistogram(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	gw := gwdb.New(pool)

	var org, key pgtype.UUID
	org = orgtest.Create(t, pool, orgtest.Seed{Slug: "lat", Name: "L"})
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,'k','sk-flb-v1-l','hl',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}

	// Known distribution: five at 50ms, three at 800ms, two at 30s.
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	insert := func(n int, ms int) {
		for range n {
			if _, err := pool.Exec(ctx, `
				INSERT INTO usage_logs (org_id,api_key_id,created_at,request_id,surface,model_slug,
				                        status,http_status,charged_nano,duration_ms)
				VALUES ($1,$2,$3,'r'||gen_random_uuid()::text,'chat_completions','m','ok',200,1,$4)`,
				org, key, at, ms); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(5, 50)
	insert(3, 800)
	insert(2, 30000)

	agg := gwusage.NewAggregator(pool, publicusage.NewPostingStore(pool), gw)
	if err := gwusage.NewAggregateWorker(agg).Work(ctx, &river.Job[gwusage.AggregateArgs]{}); err != nil {
		t.Fatal(err)
	}

	row, err := gw.UsageLatencyHistogram(ctx, gwdb.UsageLatencyHistogramParams{
		OrgID:  org,
		FromTs: pgtype.Timestamptz{Time: at.Add(-time.Hour), Valid: true},
		ToTs:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// By hand: le_100=5 (only the 50ms ones), le_1000=8 (plus the three at
	// 800ms), le_10000=8, total=10.
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"samples", row.Samples, 10},
		{"le_100", row.Le100, 5},
		{"le_250", row.Le250, 5},
		{"le_1000", row.Le1000, 8},
		{"le_10000", row.Le10000, 8},
		// The two 30s requests have a bucket of their own.
		{"le_30000", row.Le30000, 10},
		{"le_120000", row.Le120000, 10},
		{"duration_sum", row.DurationMsSum, 5*50 + 3*800 + 2*30000},
	} {
		if c.got != c.want {
			t.Errorf("%s should be %d, got %d", c.name, c.want, c.got)
		}
	}

	full := gwusage.LatencyHistogram{
		Bounds: gwusage.LatencyBounds(),
		Cumulative: []int64{
			row.Le100, row.Le250, row.Le500, row.Le1000, row.Le2500,
			row.Le5000, row.Le10000, row.Le30000, row.Le60000, row.Le120000,
		},
		Total: row.Samples, DurationMs: row.DurationMsSum,
	}
	st := full.Stats()
	if !st.HasSamples {
		t.Error("expected samples")
	}
	// By hand: target=ceil(9.5)=10; le_10000=8 < 10 and le_30000=10 >= 10,
	//          so it lands in (10000, 30000] with 2 in the bucket and pos=2
	//          -> 10000 + 20000*2/2 = 30000.
	if st.P95Unbounded {
		t.Error("30s is now within the bounds and must no longer be marked unbounded -- that is what the extra buckets bought")
	}
	if st.P95 != 30000 {
		t.Errorf("p95 should be 30000, got %d", st.P95)
	}
}

// Failed requests have no upstream latency sample and must not enter the
// quantile's denominator.
func TestFailedRequestsExcludedFromLatencyDenominator(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	gw := gwdb.New(pool)

	var org, key pgtype.UUID
	org = orgtest.Create(t, pool, orgtest.Seed{Slug: "lat-failed", Name: "L"})
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,'k','sk-flb-v1-g','hg',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)

	// 1. A row with samples: 10 requests, all <= 100ms.
	if _, err := pool.Exec(ctx, `
		INSERT INTO gateway_usage_rollups (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,
		  requests,lat_count,lat_le_100,lat_le_250,lat_le_500,lat_le_1000,lat_le_2500,
		  lat_le_5000,lat_le_10000,lat_le_30000,lat_le_60000,lat_le_120000,duration_ms_sum)
		VALUES ($1,$2,'hour',$3,'m-a',gen_random_uuid(),10,10,10,10,10,10,10,10,10,10,10,10,500)`, org, at, key); err != nil {
		t.Fatal(err)
	}
	// 2. Failed requests increment requests but carry no latency samples.
	if _, err := pool.Exec(ctx, `
		INSERT INTO gateway_usage_rollups (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,
		  requests,lat_count)
		VALUES ($1,$2,'hour',$3,'m-b',gen_random_uuid(),90,0)`, org, at, key); err != nil {
		t.Fatal(err)
	}

	row, err := gw.UsageLatencyHistogram(ctx, gwdb.UsageLatencyHistogramParams{
		OrgID:  org,
		FromTs: pgtype.Timestamptz{Time: at.Add(-time.Hour), Valid: true},
		ToTs:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.Samples != 10 {
		t.Fatalf("the denominator should count only the 10 rows with samples, got %d -- the sampleless rows leaked in", row.Samples)
	}
	st := gwusage.LatencyHistogram{
		Bounds: gwusage.LatencyBounds(),
		Cumulative: []int64{
			row.Le100, row.Le250, row.Le500, row.Le1000, row.Le2500,
			row.Le5000, row.Le10000, row.Le30000, row.Le60000, row.Le120000,
		},
		Total: row.Samples, DurationMs: row.DurationMsSum,
	}.Stats()
	// By hand: all 10 samples are in (0,100], p50 target=5 -> 0 + 100*5/10 = 50.
	if st.P50 != 50 {
		t.Errorf("p50 computed by hand is 50ms, got %d -- the denominator was polluted by sampleless rows", st.P50)
	}
	if st.P95Unbounded {
		t.Error("every sample is in the first bucket, so p95 must not be unbounded")
	}
}
