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
	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
	publicusage "github.com/fairlb/fairlb/usage"
)

func TestAggregatorBoundsBacklogAndReplayConverges(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	var org, key pgtype.UUID
	org = orgtest.Create(t, pool, orgtest.Seed{Slug: "meter-batch", Name: "Meter"})
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,'k','sk-flb-v1-m','meter-hash',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}
	nowHour := time.Now().UTC().Truncate(time.Hour)
	first := nowHour.Add(-20*time.Hour + 10*time.Minute)
	second := nowHour.Add(-2*time.Hour + 10*time.Minute)
	for i, at := range []time.Time{first, second} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO usage_logs (
			  org_id,api_key_id,created_at,request_id,surface,model_slug,status,http_status,
			  tokens_in,tokens_out,tokens_cached_read,tokens_cache_write,tokens_reasoning,
			  tokens_audio_in,tokens_audio_out,tokens_cache_write_5m,tokens_cache_write_1h,
			  charged_nano,upstream_cost_usd_nano,duration_ms)
			VALUES ($1,$2,$3,$4,'chat_completions','model-x','ok',200,
			        10,20,3,4,5,6,7,8,9,100,40,250)`, org, key, at, string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	aggregator := gwusage.NewAggregator(pool, publicusage.NewPostingStore(pool), gwdb.New(pool))
	if n, err := aggregator.Run(ctx); err != nil || n != 1 {
		t.Fatalf("first batch = %d, %v; want one old bucket", n, err)
	}
	var watermark time.Time
	if err := pool.QueryRow(ctx,
		`SELECT watermark FROM posting_watermarks WHERE key='metering:gateway_usage'`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if want := first.Truncate(time.Hour).Add(6 * time.Hour); !watermark.Equal(want) {
		t.Fatalf("watermark = %s, want six-hour bound %s", watermark, want)
	}
	if n, err := aggregator.Run(ctx); err != nil || n != 1 {
		t.Fatalf("second batch = %d, %v; want pending recent bucket", n, err)
	}

	assertMeteringTotals(t, ctx, pool, 2)
	if _, err := pool.Exec(ctx,
		`UPDATE posting_watermarks SET watermark=to_timestamp(0) WHERE key='metering:gateway_usage'`); err != nil {
		t.Fatal(err)
	}
	if _, err := aggregator.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := aggregator.Run(ctx); err != nil {
		t.Fatal(err)
	}
	assertMeteringTotals(t, ctx, pool, 2)
}

func TestAggregatorSkipsEmptyHistoryToDatabaseCursor(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	aggregator := gwusage.NewAggregator(pool, publicusage.NewPostingStore(pool), gwdb.New(pool))
	if n, err := aggregator.Run(ctx); err != nil || n != 0 {
		t.Fatalf("empty aggregation = %d, %v", n, err)
	}
	var watermark time.Time
	if err := pool.QueryRow(ctx,
		`SELECT watermark FROM posting_watermarks WHERE key='metering:gateway_usage'`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if want := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Hour); !watermark.Equal(want) {
		t.Fatalf("empty history stopped at %s, want database cursor %s", watermark, want)
	}
}

func TestAggregatorRollsBackRollupsWhenWatermarkAdvanceFails(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	var org, key pgtype.UUID
	org = orgtest.Create(t, pool, orgtest.Seed{Slug: "meter-rollback", Name: "Meter"})
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,'k','sk-flb-v1-r','meter-rollback-hash',ARRAY['inference']) RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO usage_logs (
		  org_id,api_key_id,created_at,request_id,surface,model_slug,status,http_status,
		  tokens_in,tokens_out,charged_nano,upstream_cost_usd_nano,duration_ms)
		VALUES ($1,$2,$3,'rollback-event','chat_completions','model-x','ok',200,10,20,100,40,250)`,
		org, key, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_meter_watermark() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'watermark write rejected'; END $$;
		CREATE TRIGGER reject_meter_watermark
		BEFORE UPDATE OF watermark ON posting_watermarks
		FOR EACH ROW WHEN (NEW.key = 'metering:gateway_usage')
		EXECUTE FUNCTION reject_meter_watermark()`); err != nil {
		t.Fatal(err)
	}

	aggregator := gwusage.NewAggregator(pool, publicusage.NewPostingStore(pool), gwdb.New(pool))
	if _, err := aggregator.Run(ctx); err == nil {
		t.Fatal("aggregation succeeded despite rejected watermark update")
	}
	var rollups, watermarks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway_usage_rollups`).Scan(&rollups); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM posting_watermarks WHERE key='metering:gateway_usage'`).Scan(&watermarks); err != nil {
		t.Fatal(err)
	}
	if rollups != 0 || watermarks != 0 {
		t.Fatalf("failed transaction left rollups=%d watermarks=%d", rollups, watermarks)
	}
}

func assertMeteringTotals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, requests int64) {
	t.Helper()
	var got struct {
		requests, in, out, cached, write, reasoning, audioIn, audioOut, write5m, write1h int64
	}
	err := pool.QueryRow(ctx, `SELECT
		COALESCE(sum(requests),0), COALESCE(sum(tokens_in),0), COALESCE(sum(tokens_out),0),
		COALESCE(sum(tokens_cached_read),0), COALESCE(sum(tokens_cache_write),0),
		COALESCE(sum(tokens_reasoning),0), COALESCE(sum(tokens_audio_in),0),
		COALESCE(sum(tokens_audio_out),0), COALESCE(sum(tokens_cache_write_5m),0),
		COALESCE(sum(tokens_cache_write_1h),0) FROM gateway_usage_rollups`).Scan(
		&got.requests, &got.in, &got.out, &got.cached, &got.write, &got.reasoning,
		&got.audioIn, &got.audioOut, &got.write5m, &got.write1h,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{requests, 20, 40, 6, 8, 10, 12, 14, 16, 18}
	have := []int64{got.requests, got.in, got.out, got.cached, got.write, got.reasoning, got.audioIn, got.audioOut, got.write5m, got.write1h}
	for i := range want {
		if have[i] != want[i] {
			t.Fatalf("metering totals[%d] = %d, want %d (all=%v)", i, have[i], want[i], have)
		}
	}
}
