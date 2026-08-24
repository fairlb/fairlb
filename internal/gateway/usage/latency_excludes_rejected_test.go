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

// Refused requests must not become latency samples.
//
// Requests refused at a gate are still written to usage_logs, and their
// duration is a millisecond or two -- they never reached upstream. Counted as
// samples, one client hammering into a rate limit injects thousands of
// near-zero samples and drags p50 and p95 down to milliseconds while real
// requests take seconds.
//
// What this checks: however many refused rows are added, the quantiles and the
// histogram do not move at all.
func TestLatencyHistogramExcludesRejected(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	gw := gwdb.New(pool)

	var org, key pgtype.UUID
	org = orgtest.Create(t, pool, orgtest.Seed{Slug: "rej", Name: "R"})
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,'k','sk-flb-v1-r','hr',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	insert := func(n int, ms int, status, errCode string) {
		for range n {
			if _, err := pool.Exec(ctx, `
				INSERT INTO usage_logs (org_id,api_key_id,created_at,request_id,surface,model_slug,
				                        status,error_code,http_status,charged_nano,duration_ms)
				VALUES ($1,$2,$3,'r'||gen_random_uuid()::text,'chat_completions','m',$4,$5,200,0,$6)`,
				org, key, at, status, errCode, ms); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Real traffic: 4 successful requests at 800ms.
	insert(4, 800, "ok", "")
	// 200 rate-limited refusals at 2ms each -- exactly the kind that would
	// crush the quantiles.
	insert(200, 2, "client_error", "gateway.rate_limited")

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

	// Only the 4 successful requests count as samples. The assertion is against
	// an independently derived expected value (4), not one aggregate column
	// compared against another -- that would only prove they share a source.
	if row.Samples != 4 {
		t.Fatalf("latency sample count = %d, want 4 (the 200 refused requests must not be samples)", row.Samples)
	}
	// Four requests at 800ms: le_100 must be 0. If the refusals leaked in it
	// would be 200.
	if row.Le100 != 0 {
		t.Fatalf("le_100 = %d, want 0 -- the refused requests' 2ms leaked into the histogram and would drag p50 down to milliseconds", row.Le100)
	}
	if row.Le1000 != 4 {
		t.Errorf("le_1000 = %d, want 4", row.Le1000)
	}

	// The request count still counts all of them: they really were requests,
	// they just do not carry latency. This also pins down that "samples is not
	// requests" is deliberate rather than an undercount.
	var requests int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(requests),0) FROM gateway_usage_rollups WHERE org_id = $1`, org).
		Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 204 {
		t.Errorf("requests = %d, want 204 (4 successful plus 200 refused all count as requests)", requests)
	}
}
