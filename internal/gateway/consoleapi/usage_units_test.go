package gwconsoleapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/publicid"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// Seconds of video and prepaid generations are not added together.
//
// They were, and the sum looked like an answer: an organization using a
// per-second model and a per-generation one saw a single "billed units" figure
// that was neither, with no unit beside it. The page's own comment already said
// why that is wrong -- seconds are a different dimension from tokens, and
// adding them produces a number that means nothing -- and the same sentence
// applies between the two units just as well.
func TestUsageKeepsSecondsAndGenerationsApart(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, allowAll{})
	now := time.Now()

	unitRollup(t, f, f.orgA, now, "google/veo-3.1", 96, 0)
	unitRollup(t, f, f.orgA, now, "kuaishou/kling-v2", 0, 3)

	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{From: dayAgo(1), To: now.Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	totals := res.(gwconsoleapi.GetUsage200JSONResponse).Totals
	if totals.BilledSeconds != 96 {
		t.Errorf("billed_seconds = %d, want 96", totals.BilledSeconds)
	}
	if totals.BilledCalls != 3 {
		t.Errorf("billed_calls = %d, want 3", totals.BilledCalls)
	}
	// The number that used to be reported: 99, of nothing.
	if totals.BilledSeconds == 99 || totals.BilledCalls == 99 {
		t.Error("the two units were summed into one figure again")
	}
}

// unitRollup writes one rollup hour carrying unit-billed quantities.
func unitRollup(
	t *testing.T, f *fixture, org pgtype.UUID, at time.Time, model string, seconds, calls int64,
) {
	t.Helper()
	ctx := context.Background()
	var key pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,$2,'sk-flb-v1-1',$3,ARRAY['inference']) RETURNING id`,
		org, "unit-"+model, publicid.UUIDString(org)+":unit:"+model).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO gateway_usage_rollups
		   (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,
		    requests,billed_seconds,billed_calls,charged_nano)
		 VALUES ($1,date_trunc('hour',$2::timestamptz),'hour',$3,$4,gen_random_uuid(),1,$5,$6,1000)`,
		org, at, key, model, seconds, calls); err != nil {
		t.Fatal(err)
	}
}
