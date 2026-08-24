package gwstaffapi_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The latency column on the health dashboard, and the two honesty bits it has
// to carry.
//
// Showing the numbers is not enough. Quantiles are interpolated from a
// cumulative histogram, and there are two situations in which a
// respectable-looking number is a lie, which is mostly what these cases guard:
//
//  1. No samples is not 0ms. A rollup row can have requests > 0 while its
//     latency count is 0, and reporting 0 makes that window look
//     unbelievably fast.
//  2. When p95 falls beyond the largest trustworthy bound, all that can be
//     said is "at least this much". When a window mixes rows recorded under
//     different upper bounds, the merged data no longer distinguishes them
//     and only the smallest bound can be trusted -- inventing a precise
//     number is worse than stating a lower bound.
func TestProviderHealthCarriesLatencyAndItsHonestyBits(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	var quick, blind, mixed string
	for _, p := range []*struct {
		slug string
		id   *string
	}{{"lat-quick", &quick}, {"lat-blind", &blind}, {"lat-mixed", &mixed}} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO providers (slug, vendor, protocols, base_url)
			 VALUES ($1, 'custom', ARRAY['openai'], 'https://up.test') RETURNING id`,
			p.slug).Scan(p.id); err != nil {
			t.Fatal(err)
		}
	}
	org := publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Name: "Lat"}))

	// The rollup's api_key_id is NOT NULL, so a key has to exist first.
	var key string
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id, name, prefix, key_hash, scopes)
		 VALUES ($1, 'k', 'sk-flb-v1-l', 'l`+uuid.NewString()+`', ARRAY['inference'])
		 RETURNING id`, org).Scan(&key); err != nil {
		t.Fatal(err)
	}

	// A cumulative histogram: each le_* column is "requests at or below this
	// bound", so the columns are monotonically non-decreasing.
	insert := func(providerID string, requests, latCount int64, cum [10]int64, durSum int64) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO gateway_usage_rollups
			   (org_id, bucket_start, granularity, api_key_id, model_slug, provider_id, requests, errors,
			    lat_le_100, lat_le_250, lat_le_500, lat_le_1000, lat_le_2500,
			    lat_le_5000, lat_le_10000, lat_le_30000, lat_le_60000, lat_le_120000,
			    lat_count, duration_ms_sum)
			 VALUES ($1, date_trunc('hour', now()), 'hour', $2, $3, $4, $5, 0,
			         $6,$7,$8,$9,$10,$11,$12,$13,$14,$15, $16, $17)`,
			org, key, "m/"+uuid.NewString(), providerID, requests,
			cum[0], cum[1], cum[2], cum[3], cum[4], cum[5], cum[6], cum[7], cum[8], cum[9],
			latCount, durSum); err != nil {
			t.Fatal(err)
		}
	}

	// quick: 10 samples all at or below 100ms.
	insert(quick, 10, 10, [10]int64{10, 10, 10, 10, 10, 10, 10, 10, 10, 10}, 500)
	// blind: requests recorded but no latency samples at all.
	insert(blind, 10, 0, [10]int64{}, 0)
	// mixed: of 10 samples, 6 are at or below 100ms and 4 land in (10s, 30s].
	insert(mixed, 10, 10, [10]int64{6, 6, 6, 6, 6, 6, 6, 10, 10, 10}, 200000)

	res, err := s.GetGatewayHealth(ctx, gwstaffapi.GetGatewayHealthRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]gwstaffapi.GatewayProviderHealth{}
	for _, h := range res.(gwstaffapi.GetGatewayHealth200JSONResponse).Providers {
		bySlug[h.Slug] = h
	}

	q := bySlug["lat-quick"].Latency1h
	if q == nil || !q.HasSamples {
		t.Fatal("a provider with samples reported no latency -- the dimension the spec summary promises is still empty")
	}
	if q.P50Ms == nil || *q.P50Ms > 100 {
		t.Errorf("p50 should land in the first bucket (<=100ms), got %v", q.P50Ms)
	}
	if q.P95Unbounded == nil || *q.P95Unbounded {
		t.Error("every sample is inside a bucket, so p95 must not be marked unbounded")
	}

	// No samples must say so, rather than reporting 0ms.
	b := bySlug["lat-blind"].Latency1h
	if b == nil {
		t.Fatal("a provider with no samples should still carry a latency object, just with has_samples=false")
	}
	if b.HasSamples {
		t.Error("lat_count=0 yet it reports having samples -- existing rows would be drawn as a fake 0ms line")
	}
	if b.P50Ms != nil || b.P95Ms != nil {
		t.Errorf("with no samples it must not state percentile numbers, got p50=%v p95=%v", b.P50Ms, b.P95Ms)
	}

	// The upper buckets preserve resolution for long completions.
	m := bySlug["lat-mixed"].Latency1h
	if m == nil || !m.HasSamples {
		t.Fatal("the mixed provider should have samples")
	}
	if m.P95Unbounded == nil || *m.P95Unbounded {
		t.Error("every sample is within 30s, so p95 must be bounded")
	}
	if m.P95Ms == nil || *m.P95Ms != 30000 {
		t.Errorf("p95 should land at 30000ms, got %v", m.P95Ms)
	}
}
