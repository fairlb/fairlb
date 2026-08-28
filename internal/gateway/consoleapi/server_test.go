package gwconsoleapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/orgscope"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/settings"
)

// The console usage API.
//
// The property that matters most is that one organization cannot read another's data.
// The org comes from a URL parameter, i.e. user-controlled input, which is
// exactly the side of the line that requires an org-scoped transaction.

// allowAll permits every authorization decision, so that row-level security can
// be tested on its own: with both the authorizer and RLS in force, a blocked
// cross-organization read does not say which of the two blocked it.
type allowAll struct{}

func (allowAll) ResolveOrgReadAccess(context.Context, pgtype.UUID) (bool, bool, error) {
	return true, true, nil
}

func (allowAll) AuthorizeOrgAdminRead(context.Context, pgtype.UUID) error {
	return nil
}
func (allowAll) AuthorizeOrgWrite(context.Context, pgtype.UUID) error { return nil }

type denyAll struct{}

func (denyAll) ResolveOrgReadAccess(context.Context, pgtype.UUID) (bool, bool, error) {
	return false, false, httpx.ErrCode(errcode.CommonNotFound)
}

func (denyAll) AuthorizeOrgAdminRead(context.Context, pgtype.UUID) error {
	return errors.New("access denied")
}

func (denyAll) AuthorizeOrgWrite(context.Context, pgtype.UUID) error {
	return errors.New("access denied")
}

// readOnlyAuthz permits reads and refuses writes, pinning down which gate each
// endpoint goes through.
//
// This stub is a regression probe. There used to be a single read gate here, so
// the three BYOK write endpoints (create, delete, connectivity test) both
// escaped the role check -- a plain member could enter upstream credentials --
// and were wrongly caught by the org-status gate. If anyone reattaches a write
// endpoint to the read path, or a read endpoint to the write path, this stub
// turns red immediately.
type readOnlyAuthz struct{}

// ErrWriteDenied is a sentinel rather than a plain error: a write endpoint
// failing is not the same as the write gate stopping it. A reverse probe
// established that -- putting the connectivity test back behind the read gate
// still produced an error (it could not resolve the protocol's default upstream
// endpoint), so asserting only err != nil would pass for the wrong reason.
var ErrWriteDenied = errors.New("write gate refused")

func (readOnlyAuthz) ResolveOrgReadAccess(context.Context, pgtype.UUID) (bool, bool, error) {
	return true, true, nil
}

func (readOnlyAuthz) AuthorizeOrgAdminRead(context.Context, pgtype.UUID) error { return nil }

func (readOnlyAuthz) AuthorizeOrgWrite(context.Context, pgtype.UUID) error {
	return ErrWriteDenied
}

type memberAuthz struct{ allowAll }

func (memberAuthz) AuthorizeOrgAdminRead(context.Context, pgtype.UUID) error {
	return httpx.ErrCode(errcode.CommonForbidden)
}

func (memberAuthz) ResolveOrgReadAccess(context.Context, pgtype.UUID) (bool, bool, error) {
	return false, false, nil
}

type financeOnlyAuthz struct{ allowAll }

func (financeOnlyAuthz) ResolveOrgReadAccess(context.Context, pgtype.UUID) (bool, bool, error) {
	return true, false, nil
}

type keyOnlyAuthz struct{ allowAll }

func (keyOnlyAuthz) ResolveOrgReadAccess(context.Context, pgtype.UUID) (bool, bool, error) {
	return false, true, nil
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *httpx.CodeError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

type fixture struct {
	pool *pgxpool.Pool
	orgA pgtype.UUID
	orgB pgtype.UUID
}

// A failed authorization returns no data at all.
func TestUsageRejectsUnauthorized(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, denyAll{})

	_, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{From: dayAgo(7), To: time.Now()},
	})
	if err == nil {
		t.Fatal("a failed authorization must return no data")
	}
}

func TestUsageChecksOrgRelationshipBeforeSensitiveCapability(t *testing.T) {
	f := newFixture(t)
	group := gwconsoleapi.ApiKey
	_, err := newConsoleServer(f.pool, denyAll{}).GetUsage(
		context.Background(), gwconsoleapi.GetUsageRequestObject{
			OrgId: orgParam(f.orgA), Params: gwconsoleapi.GetUsageParams{
				From: dayAgo(1), To: time.Now(), GroupBy: &group,
			},
		},
	)
	assertCode(t, err, errcode.CommonNotFound)
}

// Even with the authorizer bypassed, row-level security must stop a
// cross-organization read. That is the entire point of having a third line of
// defence.
func TestUsageIsolatedAcrossOrgs(t *testing.T) {
	f := newFixture(t)
	f.rollup(t, f.orgA, "openai/a", 1_000_000_000)
	f.rollup(t, f.orgB, "openai/b", 9_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{From: dayAgo(7), To: time.Now().Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))
	if rep.Totals.ChargedNano != 1_000_000_000 {
		t.Errorf("A's total should be 1e9, its own rows only, got %d -- cross-organization leak",
			rep.Totals.ChargedNano)
	}
}

func TestMemberUsageRedactsFinanceAndRejectsExport(t *testing.T) {
	f := newFixture(t)
	key := f.rollup(t, f.orgA, "openai/member", 1_250_000_000)
	s := newConsoleServer(f.pool, memberAuthz{})
	from, to := dayAgo(7), time.Now().Add(time.Hour)
	group := gwconsoleapi.Model

	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: from, To: to, GroupBy: &group,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))
	if rep.Totals.Requests == 0 || rep.Totals.TokensIn == 0 {
		t.Fatalf("a member should still see the operational facts: %+v", rep.Totals)
	}
	if rep.Totals.ChargedNano != 0 || rep.Totals.Currency != "" {
		t.Fatalf("a member must not see the financial totals: %+v", rep.Totals)
	}
	for _, p := range rep.Series {
		if p.ChargedNano != 0 {
			t.Fatalf("amounts in the series were not redacted for a member: %+v", p)
		}
	}
	if rep.Groups == nil || len(*rep.Groups) == 0 {
		t.Fatal("a member should still see the non-financial groups")
	}
	for _, g := range *rep.Groups {
		if g.ChargedNano != 0 {
			t.Fatalf("amounts in the groups were not redacted for a member: %+v", g)
		}
	}

	_, err = s.ExportUsageCSV(context.Background(), gwconsoleapi.ExportUsageCSVRequestObject{
		OrgId: orgParam(f.orgA), Params: gwconsoleapi.ExportUsageCSVParams{From: from, To: to},
	})
	assertCode(t, err, errcode.CommonForbidden)

	keyID := publicid.Format(publicid.Key, key)
	keyGroup := gwconsoleapi.ApiKey
	_, err = s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{From: from, To: to, GroupBy: &keyGroup},
	})
	assertCode(t, err, errcode.CommonForbidden)
	_, err = s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{From: from, To: to, ApiKeyId: &keyID},
	})
	assertCode(t, err, errcode.CommonForbidden)
}

func TestMemberUsageRanksByRequestsThenKey(t *testing.T) {
	f := newFixture(t)
	f.rollup(t, f.orgA, "openai/a", 9_000_000_000)
	f.rollup(t, f.orgA, "openai/b", 8_000_000_000)
	f.rollup(t, f.orgA, "openai/z", 1_000_000_000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_usage_rollups SET requests = CASE model_slug
		 WHEN 'openai/z' THEN 5 ELSE 2 END WHERE org_id=$1`, f.orgA); err != nil {
		t.Fatal(err)
	}

	group := gwconsoleapi.Model
	res, err := newConsoleServer(f.pool, memberAuthz{}).GetUsage(
		context.Background(), gwconsoleapi.GetUsageRequestObject{
			OrgId: orgParam(f.orgA),
			Params: gwconsoleapi.GetUsageParams{
				From: dayAgo(1), To: time.Now().Add(time.Hour), GroupBy: &group,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	groups := *gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse)).Groups
	if len(groups) != 3 || groups[0].Key != "openai/z" || groups[1].Key != "openai/a" || groups[2].Key != "openai/b" {
		t.Fatalf("the non-financial ranking must sort by requests descending then key ascending, got %+v", groups)
	}
	for _, item := range groups {
		if item.ChargedNano != 0 {
			t.Fatalf("sorting must not bring the amounts back: %+v", item)
		}
	}
}

func TestUsageCSVRequiresKeyMetadataOnlyForKeyFilter(t *testing.T) {
	f := newFixture(t)
	key := f.rollup(t, f.orgA, "openai/export", 1_000_000_000)
	s := newConsoleServer(f.pool, financeOnlyAuthz{})
	from, to := dayAgo(1), time.Now().Add(time.Hour)
	if _, err := s.ExportUsageCSV(context.Background(), gwconsoleapi.ExportUsageCSVRequestObject{
		OrgId: orgParam(f.orgA), Params: gwconsoleapi.ExportUsageCSVParams{From: from, To: to},
	}); err != nil {
		t.Fatalf("a usage CSV without a key drill-down needs only financial read access: %v", err)
	}
	keyID := publicid.Format(publicid.Key, key)
	_, err := s.ExportUsageCSV(context.Background(), gwconsoleapi.ExportUsageCSVRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.ExportUsageCSVParams{From: from, To: to, ApiKeyId: &keyID},
	})
	assertCode(t, err, errcode.CommonForbidden)
}

func TestKeyMetadataWithoutFinanceUsesRequestRanking(t *testing.T) {
	f := newFixture(t)
	expensive := f.rollup(t, f.orgA, "openai/expensive", 9_000_000_000)
	busy := f.rollup(t, f.orgA, "openai/busy", 1_000_000_000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_usage_rollups SET requests = CASE WHEN api_key_id=$2 THEN 5 ELSE 1 END
		 WHERE org_id=$1`, f.orgA, busy); err != nil {
		t.Fatal(err)
	}
	group := gwconsoleapi.ApiKey
	res, err := newConsoleServer(f.pool, keyOnlyAuthz{}).GetUsage(
		context.Background(), gwconsoleapi.GetUsageRequestObject{
			OrgId: orgParam(f.orgA), Params: gwconsoleapi.GetUsageParams{
				From: dayAgo(1), To: time.Now().Add(time.Hour), GroupBy: &group,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	groups := *gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse)).Groups
	if len(groups) != 2 || groups[0].Key != publicid.Format(publicid.Key, busy) || groups[0].Requests != 5 {
		t.Fatalf("with key metadata but no finance access it should rank public IDs by request count, got %+v", groups)
	}
	for _, item := range groups {
		if item.ChargedNano != 0 || item.Key == publicid.UUIDString(expensive) {
			t.Fatalf("this capability combination must leak neither amounts nor database UUIDs: %+v", item)
		}
	}
}

// Grouping by model aggregates per model and returns only this org's models.
func TestUsageGroupByModel(t *testing.T) {
	f := newFixture(t)
	f.rollup(t, f.orgA, "openai/x", 1_000_000_000)
	f.rollup(t, f.orgA, "openai/y", 2_000_000_000)
	f.rollup(t, f.orgB, "openai/z", 9_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	group := gwconsoleapi.Model
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(7), To: time.Now().Add(time.Hour), GroupBy: &group,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))
	if rep.Groups == nil || len(*rep.Groups) != 2 {
		t.Fatalf("expected 2 model groups, excluding B's, got %+v", rep.Groups)
	}
	// Sorted by amount descending, so the console can take the first few and
	// have the top models.
	if (*rep.Groups)[0].ChargedNano < (*rep.Groups)[1].ChargedNano {
		t.Error("groups should be sorted by amount descending")
	}
}

// api_key_id must apply to the series, the totals and the groups alike.
//
// Only the first two once honoured it: passing api_key_id together with
// group_by returned "this key's totals plus the whole org's model
// distribution", two figures in one response with no error. The assertion is
// anchored on the grouped amounts summing to the total, not merely on there
// being one group -- the latter would pass for the wrong reason with a fixture
// that happens to have a single model.
func TestUsageKeyFilterAppliesToGroups(t *testing.T) {
	f := newFixture(t)
	mine := f.rollup(t, f.orgA, "openai/x", 1_000_000_000)
	f.rollup(t, f.orgA, "openai/y", 2_000_000_000) // another key in the same org; must not appear
	f.rollup(t, f.orgB, "openai/z", 9_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	group := gwconsoleapi.Model
	keyID := publicid.Format(publicid.Key, mine)
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(7), To: time.Now().Add(time.Hour),
			GroupBy: &group, ApiKeyId: &keyID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))

	if rep.Totals.ChargedNano != 1_000_000_000 {
		t.Fatalf("the total should contain only the filtered key: %d", rep.Totals.ChargedNano)
	}
	if rep.Groups == nil {
		t.Fatal("asking for group_by should return groups")
	}
	var sum int64
	for _, g := range *rep.Groups {
		sum += g.ChargedNano
		if g.Key == "openai/y" {
			t.Errorf("another key's model %q appeared in the groups -- the filter did not reach them", g.Key)
		}
	}
	if sum != rep.Totals.ChargedNano {
		t.Errorf("the groups sum to %d while the total is %d: two different bases in one response",
			sum, rep.Totals.ChargedNano)
	}
}

// Grouping by key must honour api_key_id too: filtered to one key, only that
// key may appear in the groups.
func TestUsageKeyFilterAppliesToKeyGroups(t *testing.T) {
	f := newFixture(t)
	mine := f.rollup(t, f.orgA, "openai/x", 1_000_000_000)
	f.rollup(t, f.orgA, "openai/y", 2_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	group := gwconsoleapi.ApiKey
	keyID := publicid.Format(publicid.Key, mine)
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(7), To: time.Now().Add(time.Hour),
			GroupBy: &group, ApiKeyId: &keyID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))
	if rep.Groups == nil || len(*rep.Groups) != 1 {
		t.Fatalf("filtering by key should leave exactly one group, got %+v", rep.Groups)
	}
	if got := (*rep.Groups)[0].ChargedNano; got != 1_000_000_000 {
		t.Errorf("the group amount should be the filtered key's 1e9, got %d", got)
	}
	if got := (*rep.Groups)[0].Key; got != keyID {
		t.Errorf("grouping by key must return the public ID %q, got %q", keyID, got)
	}
}

// Range validation: without a cap, one "show me everything" scans every
// partition.
func TestUsageRejectsBadRange(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, allowAll{})

	cases := map[string]gwconsoleapi.GetUsageParams{
		"to before from": {From: time.Now(), To: dayAgo(1)},
		"range too long": {From: dayAgo(500), To: time.Now()},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
				OrgId: orgParam(f.orgA), Params: params,
			}); err == nil {
				t.Error("should be refused")
			}
		})
	}
}

// The model catalog returns only models that have a usable route: something
// listed with nobody to serve it should not appear.
func TestAvailableModelsRequireRoute(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var routed, orphan string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ('openai/routed',4096) RETURNING id::text`).Scan(&routed); err != nil {
		t.Fatal(err)
	}
	seedPrice(t, f, routed)
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens)
		 VALUES ('openai/orphan',4096) RETURNING id::text`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	seedPrice(t, f, orphan)
	var prov string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ('p','custom',ARRAY['openai'],'https://u.test')
		 RETURNING id::text`).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, f.pool, routed, prov, "up", "chat")

	s := newConsoleServer(f.pool, allowAll{})
	res, err := s.ListAvailableModels(ctx, gwconsoleapi.ListAvailableModelsRequestObject{
		OrgId: orgParam(f.orgA),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := res.(gwconsoleapi.ListAvailableModels200JSONResponse).Body.Items
	if len(data) != 1 || data[0].Slug != "openai/routed" {
		t.Fatalf("only models with a route should be listed, got %+v", data)
	}
	if len(data[0].Endpoints) != 1 || data[0].Endpoints[0] != "chat" {
		t.Errorf("capability should be the union of the enabled routes: %v", data[0].Endpoints)
	}
	memberRes, err := newConsoleServer(f.pool, memberAuthz{}).ListAvailableModels(
		ctx, gwconsoleapi.ListAvailableModelsRequestObject{OrgId: orgParam(f.orgA)},
	)
	if err != nil {
		t.Fatal(err)
	}
	memberData := memberRes.(gwconsoleapi.ListAvailableModels200JSONResponse).Body.Items
	if len(memberData) != 1 || memberData[0].PriceInNanoPerMtok != nil || memberData[0].PriceOutNanoPerMtok != nil ||
		memberData[0].PriceCacheReadNanoPerMtok != nil || memberData[0].PriceCacheWriteNanoPerMtok != nil ||
		memberData[0].IsFree != nil {
		t.Fatalf("a member should see model capabilities but not prices: %+v", memberData)
	}
	if data[0].IsFree == nil {
		t.Fatal("a caller with financial read access should receive the free flag")
	}
}

// ===== Fixtures =====

func orgParam(org pgtype.UUID) gwconsoleapi.OrgID {
	return gwconsoleapi.OrgID(publicid.Format(publicid.Org, org))
}

func dayAgo(n int) time.Time { return time.Now().AddDate(0, 0, -n) }

// rollup creates a key, records one rollup row against it, and returns the
// key's id, which the key-filter cases need to build api_key_id.
func (f *fixture) rollup(t *testing.T, org pgtype.UUID, model string, nano int64) pgtype.UUID {
	t.Helper()
	var key pgtype.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
		 VALUES ($1,$2,'sk-flb-v1-1',$3,ARRAY['inference']) RETURNING id`,
		org, "k-"+model, publicid.UUIDString(org)+":"+model).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO gateway_usage_rollups
		   (org_id,bucket_start,granularity,api_key_id,model_slug,provider_id,requests,tokens_in,tokens_out,charged_nano)
		 VALUES ($1,date_trunc('hour',now()),'hour',$2,$3,gen_random_uuid(),1,10,20,$4)`,
		org, key, model, nano); err != nil {
		t.Fatal(err)
	}
	return key
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testpg.Start(t)
	mk := func(slug string) pgtype.UUID {
		return orgtest.Create(t, pool, orgtest.Seed{Slug: slug, Name: "T"})
	}
	return &fixture{pool: pool, orgA: mk("c-a"), orgB: mk("c-b")}
}

// The series must zero-fill empty buckets.
//
// Returning only the buckets that have rollup rows lets the frontend draw a
// straight line between two isolated points, which reads as "steady linear
// growth" when in fact there were days with no calls at all. That is where a
// suspiciously straight spend curve on the overview page comes from.
func TestUsageSeriesFillsEmptyBuckets(t *testing.T) {
	f := newFixture(t)
	// One rollup row in the current hour only; the preceding 7 days are empty.
	f.rollup(t, f.orgA, "openai/x", 1_000_000_000)

	s := newConsoleServer(f.pool, allowAll{})
	day := gwconsoleapi.GetUsageParamsGranularityDay
	res, err := s.GetUsage(context.Background(), gwconsoleapi.GetUsageRequestObject{
		OrgId: orgParam(f.orgA),
		Params: gwconsoleapi.GetUsageParams{
			From: dayAgo(7), To: time.Now().Add(time.Hour), Granularity: &day,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := gwconsoleapi.UsageReport(res.(gwconsoleapi.GetUsage200JSONResponse))

	// A 7-day window cut into days has a partial bucket at each end when the
	// boundaries do not align, hence 8.
	if len(rep.Series) < 8 {
		t.Fatalf("a 7-day window should fill in 8 daily buckets, got %d -- without zero-filling the chart joins isolated points into a straight line",
			len(rep.Series))
	}
	// Buckets must be strictly increasing and evenly spaced; otherwise
	// "filled in" just means a few extra rows happened to come back.
	for i := 1; i < len(rep.Series); i++ {
		gap := rep.Series[i].BucketStart.Sub(rep.Series[i-1].BucketStart)
		if gap != 24*time.Hour {
			t.Fatalf("bucket %d is %v after the previous one, want 24h", i, gap)
		}
	}
	// The filled buckets really are zero, not copies of the one with data.
	var nonZero int
	var sum int64
	for _, p := range rep.Series {
		sum += p.ChargedNano
		if p.ChargedNano != 0 {
			nonZero++
		}
	}
	if nonZero != 1 {
		t.Errorf("exactly 1 bucket should carry spend, got %d", nonZero)
	}
	if sum != rep.Totals.ChargedNano {
		t.Errorf("the series sums to %d while the total is %d -- zero-filling changed the total", sum, rep.Totals.ChargedNano)
	}
}

// The catalog supports conditional requests: a matching ETag gets a 304, and a
// changed catalog no longer does.
func TestAvailableModelsConditionalRequest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mkModel := func(slug string) {
		var id, prov string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO models (slug, max_output_tokens)
			 VALUES ($1,4096) RETURNING id::text`, slug).Scan(&id); err != nil {
			t.Fatal(err)
		}
		seedPrice(t, f, id)
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO providers (slug, vendor, protocols, base_url) VALUES ($1,'custom',ARRAY['openai'],'https://u.test')
			 RETURNING id::text`, "p-"+slug).Scan(&prov); err != nil {
			t.Fatal(err)
		}
		catalogtest.SeedRoute(t, f.pool, id, prov, "up", "chat")
	}
	mkModel("openai/one")

	s := newConsoleServer(f.pool, allowAll{})
	call := func(inm *string) gwconsoleapi.ListAvailableModelsResponseObject {
		res, err := s.ListAvailableModels(ctx, gwconsoleapi.ListAvailableModelsRequestObject{
			OrgId:  orgParam(f.orgA),
			Params: gwconsoleapi.ListAvailableModelsParams{IfNoneMatch: inm},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	first, ok := call(nil).(gwconsoleapi.ListAvailableModels200JSONResponse)
	if !ok {
		t.Fatal("the first request should return 200")
	}
	if first.Headers.ETag == nil || *first.Headers.ETag == "" {
		t.Fatal("a 200 must carry an ETag, or there is no conditional request to speak of")
	}
	etag := *first.Headers.ETag

	// Same ETag, unchanged catalog: 304.
	if _, ok := call(&etag).(gwconsoleapi.ListAvailableModels304Response); !ok {
		t.Errorf("an unchanged catalog should return 304, got %T", call(&etag))
	}

	// A changed catalog must return 200. This is the dangerous direction for
	// an ETag: a wrongly computed tag quietly hides the change behind a 304,
	// and the symptom is "an operator changed it and organizations cannot see it".
	mkModel("openai/two")
	second, ok := call(&etag).(gwconsoleapi.ListAvailableModels200JSONResponse)
	if !ok {
		t.Fatalf("a changed catalog should return 200, got %T", call(&etag))
	}
	if len(second.Body.Items) != 2 {
		t.Errorf("expected 2 models, got %d", len(second.Body.Items))
	}
	if second.Headers.ETag == nil || *second.Headers.ETag == etag {
		t.Error("a changed catalog must change the ETag too")
	}
}

// testCatalog builds a catalog service backed only by the database: nil cache
// so reads go straight through, and settings on the same pool -- the organization
// catalog has to read the markup and the engine mode.
func testCatalog(pool *pgxpool.Pool) *catalog.Service {
	return catalog.NewService(gwdb.New(pool), nil, settings.New(pool, nil, settings.NewRegistry(), nil))
}

func newConsoleServer(pool *pgxpool.Pool, authz orgscope.Authorizer) *gwconsoleapi.Server {
	// The video plane comes from the pipeline's own job surface, as it does at
	// the assembly point: the console and the data plane must reach the same
	// answers about a job, and two implementations of "what does cancelling
	// mean" is how two surfaces come to disagree about whether a customer was
	// charged. Only the pool and the query set are wired here -- reading a job
	// list needs nothing else, and a test that stood up an upstream to read a
	// table would be testing the fixture.
	video := proxy.NewPipeline(proxy.PipelineConfig{Pool: pool, Gateway: gwdb.New(pool)})
	return gwconsoleapi.NewServer(gwconsoleapi.ServerConfig{
		Pool: pool, OrganizationAccess: authz, Catalog: testCatalog(pool),
		VideoJobs: video.VideoJobs(),
	})
}

// seedPrice writes one current-price row for a model. Price lives only in
// model_pricing: no read path touches price columns on the model row any more,
// so a fixture that writes them would construct a model that cannot exist in
// production.
func seedPrice(t *testing.T, f *fixture, modelID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO model_pricing (model_id, billing_mode,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok, source_name)
		VALUES ($1, 'paid', 1000000000, 1000000000, 0, 0, 'test-fixture')`, modelID); err != nil {
		t.Fatal(err)
	}
}
