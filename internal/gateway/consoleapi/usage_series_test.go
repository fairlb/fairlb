package gwconsoleapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/publicid"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// Continuity of the usage series.
//
// What the defect looked like: pick "last 7 days" in the console and the trend
// chart draws a straight line climbing evenly from 0, with an x axis spanning
// roughly 24 hours. The cause was a plain GROUP BY over buckets -- days with no
// rollup rows were simply absent from the result, so the chart's time axis
// interpolated between the only two points it had. The reader saw "spend is
// growing steadily" where the truth was "five days with no traffic, then two
// bursts".
//
// The same series also feeds the breakdown table below the chart and the CSV
// export, so all three were wrong together.

// rollupAt inserts a rollup into the hour bucket at a given instant.
//
// Kept separate from the fixture in server_test.go, which always writes into the
// current hour: what this file needs to construct is precisely "some buckets in
// the window have data and some do not".
func (f *fixture) rollupAt(t *testing.T, org pgtype.UUID, at time.Time, model string, nano int64) {
	t.Helper()
	ctx := context.Background()
	var key pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,$2,'sk-flb-v1-1',$3,ARRAY['inference']) RETURNING id`,
		org, "k-"+model+at.Format(time.RFC3339Nano),
		publicid.UUIDString(org)+":"+model+at.Format(time.RFC3339Nano)).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO gateway_usage_rollups
		   (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,
		    requests,tokens_in,tokens_out,charged_nano)
		 VALUES ($1,date_trunc('hour',$2::timestamptz),'hour',$3,$4,gen_random_uuid(),1,10,20,$5)`,
		org, at, key, model, nano); err != nil {
		t.Fatal(err)
	}
}

func usageSeries(
	t *testing.T, f *fixture, gran gwconsoleapi.GetUsageParamsGranularity, from, to time.Time,
) []gwconsoleapi.UsagePoint {
	t.Helper()
	s := newConsoleServer(f.pool, allowAll{})
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{From: from, To: to, Granularity: &gran},
	})
	if err != nil {
		t.Fatal(err)
	}
	return gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse)).Series
}

// TestUsageSeriesHasNoGaps is the reverse probe for that fix.
//
// The criterion is that the gap between adjacent buckets always equals one
// granularity, not that the bucket count equals N. The latter has to be derived
// alongside the rounding of from and to, and getting that derivation wrong just
// means asserting my own arithmetic -- while the thing actually worth
// preventing, missing buckets in the middle, goes unexpressed.
func TestUsageSeriesHasNoGaps(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	// Only two days in the window carry traffic, with a gap between them --
	// exactly the shape of the 7-day window that exposed the defect.
	f.rollupAt(t, f.orgA, now.AddDate(0, 0, -6), "openai/a", 1_000_000_000)
	f.rollupAt(t, f.orgA, now.AddDate(0, 0, -1), "openai/a", 2_000_000_000)

	series := usageSeries(t, f, gwconsoleapi.GetUsageParamsGranularityDay,
		now.AddDate(0, 0, -7), now.Add(time.Hour))

	if len(series) < 7 {
		t.Fatalf("a 7-day window returned only %d daily buckets -- the empty "+
			"days were dropped, and the line chart interpolates the few "+
			"remaining points into a straight line", len(series))
	}
	for i := 1; i < len(series); i++ {
		gap := series[i].BucketStart.Sub(series[i-1].BucketStart)
		if gap != 24*time.Hour {
			t.Errorf("buckets %d and %d are %v apart, want 24h -- buckets are missing in between", i-1, i, gap)
		}
	}
	// The zero-filling has to actually happen: if every bucket were non-zero,
	// the "no gaps" check above might hold merely because this data happens to
	// cover every day, and the criterion would not be testing what it claims.
	zeros := 0
	for _, p := range series {
		if p.ChargedNano == 0 {
			zeros++
		}
	}
	if zeros == 0 {
		t.Error("there is no zero bucket at all -- this case only creates two days of traffic, so either zero-filling is not working or the fixture is broken")
	}

	// The total must not change: what is filled in is zero, not data.
	var sum int64
	for _, p := range series {
		sum += p.ChargedNano
	}
	if sum != 3_000_000_000 {
		t.Errorf("the series sums to %d, want 3e9 -- zero-filling must not change the total", sum)
	}
}

// TestUsageSeriesDayBucketsFollowRequestedTZ pins day boundaries to the
// requested timezone.
//
// What the defect looked like: day buckets were truncated in UTC while the
// console rendered axis labels in the browser's local zone. At UTC+8, UTC
// midnight is 08:00 local -- which is where the 08:00 ticks on the x axis came
// from -- and the date label could be off by a full day.
func TestUsageSeriesDayBucketsFollowRequestedTZ(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	f.rollupAt(t, f.orgA, now.AddDate(0, 0, -2), "openai/a", 1_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	gran := gwconsoleapi.GetUsageParamsGranularityDay
	ask := func(tz string) []gwconsoleapi.UsagePoint {
		t.Helper()
		params := gwconsoleapi.GetUsageParams{
			From: now.AddDate(0, 0, -4), To: now.Add(time.Hour), Granularity: &gran,
		}
		if tz != "" {
			params.Tz = &tz
		}
		res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
			OrgId: orgParam(f.orgA), Params: params,
		})
		if err != nil {
			t.Fatal(err)
		}
		return gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse)).Series
	}

	utc := ask("")
	sh := ask("Asia/Shanghai")
	if len(utc) == 0 || len(sh) == 0 {
		t.Fatal("both sets should have buckets")
	}
	// The UTC day boundary is 00:00Z; Asia/Shanghai's is local 00:00, which is
	// 16:00Z the previous day.
	if h := utc[0].BucketStart.UTC().Hour(); h != 0 {
		t.Errorf("the default UTC day boundary should fall at 00:00Z, got %02d:00Z", h)
	}
	if h := sh[0].BucketStart.UTC().Hour(); h != 16 {
		t.Errorf("the Asia/Shanghai day boundary should fall at 16:00Z, which "+
			"is local 00:00, got %02d:00Z -- the boundary did not follow the "+
			"requested timezone and each bar spans two local calendar days", h)
	}
	// However the buckets are cut, the total is unchanged: cutting moves
	// boundaries, not data.
	sum := func(ps []gwconsoleapi.UsagePoint) int64 {
		var n int64
		for _, p := range ps {
			n += p.ChargedNano
		}
		return n
	}
	if sum(utc) != sum(sh) || sum(utc) != 1_000_000_000 {
		t.Errorf("the totals should be identical at 1e9 in both timezones, got UTC=%d SH=%d", sum(utc), sum(sh))
	}
}

// An unrecognised timezone name falls back to UTC rather than turning the whole
// page into a 500 -- the name comes from the browser and is user-controlled
// input.
func TestUsageSeriesRejectsUnknownTZGracefully(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	f.rollupAt(t, f.orgA, now.AddDate(0, 0, -1), "openai/a", 1_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	gran := gwconsoleapi.GetUsageParamsGranularityDay
	bogus := "Not/AZone"
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: now.AddDate(0, 0, -3), To: now.Add(time.Hour), Granularity: &gran, Tz: &bogus,
		},
	})
	if err != nil {
		t.Fatalf("an invalid timezone name should fall back to UTC rather than error: %v", err)
	}
	series := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse)).Series
	if len(series) == 0 {
		t.Fatal("there should still be buckets after the fallback")
	}
	if h := series[0].BucketStart.UTC().Hour(); h != 0 {
		t.Errorf("the fallback should behave as UTC with a 00:00Z boundary, got %02d:00Z", h)
	}
}

// Hourly granularity behaves the same way: empty hours in a 24-hour window must
// still be present.
func TestUsageSeriesHasNoGapsHourly(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	f.rollupAt(t, f.orgA, now.Add(-20*time.Hour), "openai/a", 1_000_000_000)
	f.rollupAt(t, f.orgA, now.Add(-2*time.Hour), "openai/a", 2_000_000_000)

	series := usageSeries(t, f, gwconsoleapi.GetUsageParamsGranularityHour,
		now.Add(-24*time.Hour), now.Add(time.Minute))

	if len(series) < 20 {
		t.Fatalf("a 24-hour window returned only %d hourly buckets -- the empty ones were dropped", len(series))
	}
	for i := 1; i < len(series); i++ {
		if gap := series[i].BucketStart.Sub(series[i-1].BucketStart); gap != time.Hour {
			t.Errorf("buckets %d and %d are %v apart, want 1h", i-1, i, gap)
		}
	}
}
