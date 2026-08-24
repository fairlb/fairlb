package gwconsoleapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// The request log endpoints. The two properties that matter most: one organization
// cannot read another's, as with usage, and pagination does not drop rows.

// Pagination must not drop rows -- that is the entire reason the cursor is
// composite.
//
// Five records are created with identical created_at values and paged through
// two at a time; all five must come back. With a bare timestamp cursor only two
// survive: the second page starts from created_at < cursor, skipping the other
// three at that same instant. Under concurrent gateway writes, several rows per
// microsecond is normal.
func TestLogsPaginationDoesNotSkipSameTimestamp(t *testing.T) {
	f := newFixture(t)
	ts := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	for i := range 5 {
		f.log(t, f.orgA, logRow{requestID: fmt.Sprintf("req-%d", i), at: ts})
	}

	s := newConsoleServer(f.pool, allowAll{})
	seen := map[string]bool{}
	var cursor *string
	for page := range 10 {
		limit := 2
		res, err := s.ListRequestLogs(context.Background(), gwconsoleapi.ListRequestLogsRequestObject{
			OrgId: orgParam(f.orgA),
			Params: gwconsoleapi.ListRequestLogsParams{
				From: &ts, To: ptrTime(ts.Add(time.Minute)), Limit: &limit, Cursor: cursor,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := res.(gwconsoleapi.ListRequestLogs200JSONResponse)
		for _, it := range got.Items {
			if seen[it.RequestId] {
				t.Errorf("page %d returned %s again", page, it.RequestId)
			}
			seen[it.RequestId] = true
		}
		if got.NextCursor == nil {
			break
		}
		cursor = got.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("all 5 records at the same instant should be paged through, got %d: %v -- the cursor skipped rows", len(seen), seen)
	}
}

// No cross-organization reads: even with the authorizer bypassed, row-level security
// has to stop it.
func TestLogsIsolatedAcrossOrgs(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-time.Minute)
	f.log(t, f.orgA, logRow{requestID: "mine", at: at})
	f.log(t, f.orgB, logRow{requestID: "theirs", at: at})

	s := newConsoleServer(f.pool, allowAll{})
	res, err := s.ListRequestLogs(context.Background(), gwconsoleapi.ListRequestLogsRequestObject{
		OrgId:  orgParam(f.orgA),
		Params: gwconsoleapi.ListRequestLogsParams{},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := res.(gwconsoleapi.ListRequestLogs200JSONResponse).Items
	if len(items) != 1 || items[0].RequestId != "mine" {
		t.Fatalf("only this org's own records should be visible, got %+v -- cross-organization leak", items)
	}

	// The single-record query must not leak either: knowing the other org's
	// request_id still reads nothing.
	if _, err := s.GetRequestLog(context.Background(), gwconsoleapi.GetRequestLogRequestObject{
		OrgId: orgParam(f.orgA), RequestId: "theirs",
	}); err == nil {
		t.Error("fetching another org's record by request_id should fail")
	}
}

// Each filter dimension takes effect independently.
func TestLogsFilters(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-time.Minute)
	f.log(t, f.orgA, logRow{requestID: "a", at: at, model: "openai/gpt", status: "ok", endUser: "u1"})
	f.log(t, f.orgA, logRow{requestID: "b", at: at, model: "anthropic/c", status: "upstream_error", endUser: "u2"})

	s := newConsoleServer(f.pool, allowAll{})
	list := func(p gwconsoleapi.ListRequestLogsParams) []gwconsoleapi.RequestLog {
		t.Helper()
		res, err := s.ListRequestLogs(context.Background(), gwconsoleapi.ListRequestLogsRequestObject{
			OrgId: orgParam(f.orgA), Params: p,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.(gwconsoleapi.ListRequestLogs200JSONResponse).Items
	}

	errStatus := gwconsoleapi.ListRequestLogsParamsStatusUpstreamError
	cases := map[string]struct {
		params gwconsoleapi.ListRequestLogsParams
		want   string
	}{
		"by model":    {gwconsoleapi.ListRequestLogsParams{Model: ptrStr("openai/gpt")}, "a"},
		"by status":   {gwconsoleapi.ListRequestLogsParams{Status: &errStatus}, "b"},
		"by end user": {gwconsoleapi.ListRequestLogsParams{EndUserId: ptrStr("u2")}, "b"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := list(c.params)
			if len(got) != 1 || got[0].RequestId != c.want {
				t.Errorf("only %s should remain, got %+v", c.want, got)
			}
		})
	}

	// An empty string means "no filter", not "filter on the empty value" --
	// that is exactly what the frontend sends when a select is cleared.
	if got := list(gwconsoleapi.ListRequestLogsParams{Model: ptrStr(""), EndUserId: ptrStr("")}); len(got) != 2 {
		t.Errorf("an empty string should mean no filter, got %d rows", len(got))
	}
}

// The detail view returns the chain of routing attempts and the cache buckets:
// the log page's side panel uses those fields to explain why a request was slow
// or expensive.
func TestLogDetailCarriesRouteChain(t *testing.T) {
	f := newFixture(t)
	f.log(t, f.orgA, logRow{
		requestID: "detail", at: time.Now().Add(-time.Minute),
		attempts: 3, cachedRead: 128, cacheWrite: 64,
	})

	s := newConsoleServer(f.pool, allowAll{})
	res, err := s.GetRequestLog(context.Background(), gwconsoleapi.GetRequestLogRequestObject{
		OrgId: orgParam(f.orgA), RequestId: "detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	d := gwconsoleapi.RequestLogDetail(res.(gwconsoleapi.GetRequestLog200JSONResponse))
	if d.RouteAttempts == nil || *d.RouteAttempts != 3 {
		t.Errorf("the attempt count should be 3, meaning two failovers, got %v", d.RouteAttempts)
	}
	if d.TokensCachedRead == nil || *d.TokensCachedRead != 128 {
		t.Errorf("cache-read tokens should be 128, got %v", d.TokensCachedRead)
	}
	if d.TokensCacheWrite == nil || *d.TokensCacheWrite != 64 {
		t.Errorf("cache-write tokens should be 64, got %v", d.TokensCacheWrite)
	}
}

// An unknown request_id returns 404, not an empty result and not a 500.
func TestLogDetailNotFound(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, allowAll{})
	if _, err := s.GetRequestLog(context.Background(), gwconsoleapi.GetRequestLogRequestObject{
		OrgId: orgParam(f.orgA), RequestId: "nope",
	}); err == nil {
		t.Error("an unknown request_id should error")
	}
}

func TestMemberLogsRedactFinanceAndRejectExport(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-time.Minute)
	key := f.rollup(t, f.orgA, "openai/member-log", 1)
	keyID := publicid.Format(publicid.Key, key)
	f.log(t, f.orgA, logRow{requestID: "member-log", at: at, charged: 987_654_321, apiKeyID: key})
	s := newConsoleServer(f.pool, memberAuthz{})

	listRes, err := s.ListRequestLogs(context.Background(), gwconsoleapi.ListRequestLogsRequestObject{
		OrgId: orgParam(f.orgA), Params: gwconsoleapi.ListRequestLogsParams{},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := listRes.(gwconsoleapi.ListRequestLogs200JSONResponse).Items
	if len(items) != 1 || items[0].TokensIn == nil || items[0].ChargedNano != 0 || items[0].ApiKeyId != nil {
		t.Fatalf("a member's logs should keep the operational facts and hide the amounts: %+v", items)
	}

	detailRes, err := s.GetRequestLog(context.Background(), gwconsoleapi.GetRequestLogRequestObject{
		OrgId: orgParam(f.orgA), RequestId: "member-log",
	})
	if err != nil {
		t.Fatal(err)
	}
	detail := gwconsoleapi.RequestLogDetail(detailRes.(gwconsoleapi.GetRequestLog200JSONResponse))
	if detail.ChargedNano != 0 || detail.ChargedCurrency != nil || detail.ApiKeyId != nil {
		t.Fatalf("amounts in the log detail were not redacted for a member: %+v", detail)
	}
	_, err = s.ListRequestLogs(context.Background(), gwconsoleapi.ListRequestLogsRequestObject{
		OrgId: orgParam(f.orgA), Params: gwconsoleapi.ListRequestLogsParams{ApiKeyId: &keyID},
	})
	assertCode(t, err, errcode.CommonForbidden)

	_, err = s.ExportLogsCSV(context.Background(), gwconsoleapi.ExportLogsCSVRequestObject{
		OrgId: orgParam(f.orgA),
	})
	assertCode(t, err, errcode.CommonForbidden)

	ownerRes, err := newConsoleServer(f.pool, allowAll{}).ListRequestLogs(
		context.Background(), gwconsoleapi.ListRequestLogsRequestObject{
			OrgId: orgParam(f.orgA), Params: gwconsoleapi.ListRequestLogsParams{ApiKeyId: &keyID},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ownerItems := ownerRes.(gwconsoleapi.ListRequestLogs200JSONResponse).Items
	if len(ownerItems) != 1 || ownerItems[0].ApiKeyId == nil || *ownerItems[0].ApiKeyId != keyID {
		t.Fatalf("a caller with key-metadata access should receive the public ID: %+v", ownerItems)
	}

	_, err = newConsoleServer(f.pool, financeOnlyAuthz{}).ExportLogsCSV(
		context.Background(), gwconsoleapi.ExportLogsCSVRequestObject{OrgId: orgParam(f.orgA)},
	)
	assertCode(t, err, errcode.CommonForbidden)
}

// ===== Fixtures =====

type logRow struct {
	requestID  string
	at         time.Time
	model      string
	status     string
	endUser    string
	attempts   int
	cachedRead int
	cacheWrite int
	charged    int64
	apiKeyID   pgtype.UUID
}

func (f *fixture) log(t *testing.T, org pgtype.UUID, r logRow) {
	t.Helper()
	if r.model == "" {
		r.model = "openai/gpt"
	}
	if r.status == "" {
		r.status = "ok"
	}
	if r.attempts == 0 {
		r.attempts = 1
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO usage_logs
		   (org_id, created_at, request_id, surface, model_slug, status, http_status,
		    route_attempts, tokens_in, tokens_out, tokens_cached_read, tokens_cache_write,
		    charged_nano, end_user_id, duration_ms, api_key_id)
		 VALUES ($1,$2,$3,'chat_completions',$4,$5,200,$6,10,20,$7,$8,$9,$10,42,$11)`,
		org, r.at, r.requestID, r.model, r.status, r.attempts,
		r.cachedRead, r.cacheWrite, r.charged, r.endUser, r.apiKeyID); err != nil {
		t.Fatal(err)
	}
}

func ptrStr(s string) *string        { return &s }
func ptrTime(t time.Time) *time.Time { return &t }
