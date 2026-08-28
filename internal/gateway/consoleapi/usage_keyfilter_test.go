package gwconsoleapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// api_key_id is a filter the contract spells out -- "usage of this key only" --
// and the group-by path once simply did not take the parameter: the series and
// the totals were filtered, the groups were not.
//
// A caller that passed it, including any API consumer holding a management key,
// got filtered totals alongside unfiltered groups: summing the groups did not
// match the total. The parameter appeared nowhere in the older tests, so this
// path had no coverage at all.

// rollupForKey inserts a rollup under a given key and returns that key's id.
func (f *fixture) rollupForKey(
	t *testing.T, org pgtype.UUID, keyName, model string, nano int64,
) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var key pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,$2,'sk-flb-v1-1',$3,ARRAY['inference']) RETURNING id`,
		org, keyName, publicid.UUIDString(org)+":"+keyName).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO gateway_usage_rollups
		   (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,
		    requests,tokens_in,tokens_out,charged_nano)
		 VALUES ($1,date_trunc('hour',now()),'hour',$2,$3,gen_random_uuid(),1,10,20,$4)`,
		org, key, model, nano); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestUsageKeyFilterAppliesToGroupsToo pins down that the three blocks of
// numbers add up to each other.
//
// The criterion is deliberately not "there is exactly one group" but "the
// groups sum to the totals". The first holds only for this particular shape of
// two keys and two models; the second is a relationship that ought to hold
// between these three blocks whatever the data looks like.
func TestUsageKeyFilterAppliesToGroupsToo(t *testing.T) {
	f := newFixture(t)
	keyA := f.rollupForKey(t, f.orgA, "ka", "openai/x", 1_000_000_000)
	f.rollupForKey(t, f.orgA, "kb", "openai/y", 7_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	group := gwconsoleapi.Model
	keyID := publicid.Format(publicid.Key, keyA)
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(1), To: time.Now().Add(time.Hour),
			GroupBy: &group, ApiKeyId: &keyID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))

	if rep.Totals.ChargedNano != 1_000_000_000 {
		t.Fatalf("the totals should contain only keyA's 1e9, got %d", rep.Totals.ChargedNano)
	}
	if rep.Groups == nil {
		t.Fatal("expected groups")
	}
	var groupSum int64
	for _, g := range *rep.Groups {
		groupSum += g.ChargedNano
	}
	if groupSum != rep.Totals.ChargedNano {
		t.Errorf("the groups sum to %d but the total is %d -- api_key_id "+
			"filtered the totals and not the groups, so the two blocks of "+
			"numbers on the page disagree", groupSum, rep.Totals.ChargedNano)
	}
	// The series belongs to the same key.
	var seriesSum int64
	for _, p := range rep.Series {
		seriesSum += p.ChargedNano
	}
	if seriesSum != rep.Totals.ChargedNano {
		t.Errorf("the series sums to %d but the total is %d", seriesSum, rep.Totals.ChargedNano)
	}
}

// Grouping by key is filtered by key too; otherwise the per-key leaderboard
// would list the very keys that were filtered out.
func TestUsageKeyFilterAppliesToGroupByKey(t *testing.T) {
	f := newFixture(t)
	keyA := f.rollupForKey(t, f.orgA, "ga", "openai/x", 1_000_000_000)
	f.rollupForKey(t, f.orgA, "gb", "openai/y", 7_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	group := gwconsoleapi.ApiKey
	keyID := publicid.Format(publicid.Key, keyA)
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(1), To: time.Now().Add(time.Hour),
			GroupBy: &group, ApiKeyId: &keyID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))
	if rep.Groups == nil || len(*rep.Groups) != 1 {
		t.Fatalf("grouping by key while filtered to one key should give exactly 1 group, got %+v", rep.Groups)
	}
	if (*rep.Groups)[0].ChargedNano != 1_000_000_000 {
		t.Errorf("that group's amount should be 1e9, got %d", (*rep.Groups)[0].ChargedNano)
	}
}

// Behaviour is unchanged when api_key_id is absent. This guards the fix above
// from having filtered the unfiltered case as well.
func TestUsageWithoutKeyFilterStillSeesEverything(t *testing.T) {
	f := newFixture(t)
	f.rollupForKey(t, f.orgA, "na", "openai/x", 1_000_000_000)
	f.rollupForKey(t, f.orgA, "nb", "openai/y", 7_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	group := gwconsoleapi.Model
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(1), To: time.Now().Add(time.Hour), GroupBy: &group,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))
	if rep.Totals.ChargedNano != 8_000_000_000 {
		t.Errorf("unfiltered this should be 8e9, got %d", rep.Totals.ChargedNano)
	}
	if rep.Groups == nil || len(*rep.Groups) != 2 {
		t.Fatalf("unfiltered there should be 2 model groups, got %+v", rep.Groups)
	}
}

// An unrecognised group_by is a 400, not a 500.
//
// The spec declares `enum: [model, api_key]`, but oapi-codegen binds the raw
// string and never calls the generated `Valid()`, and no request-validator
// middleware is mounted — so the value really does reach the handler. This was
// a 500 for exactly as long as the read model's own switch was doing the
// rejecting; the test pins which layer answers.
func TestUnknownGroupByIsAValidationError(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, allowAll{})
	bogus := gwconsoleapi.GetUsageParamsGroupBy("banana")
	_, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(1), To: time.Now().Add(time.Hour), GroupBy: &bogus,
		},
	})
	var coded *httpx.CodeError
	if !errors.As(err, &coded) || coded.Code != errcode.CommonValidation {
		t.Fatalf("group_by=banana = %v, want a validation error", err)
	}
}
