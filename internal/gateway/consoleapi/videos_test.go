package gwconsoleapi_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// The organization's own video jobs, and only its own.
//
// Isolation is asserted rather than assumed: the job statements carry `org_id`
// in their own predicate rather than relying on the session setting, precisely
// so that the answer does not depend on which connection asks.
func TestVideoJobsAreScopedToTheOrganization(t *testing.T) {
	f := newFixture(t)
	s := newConsoleServer(f.pool, allowAll{})
	mine := seedVideoJob(t, f, f.orgA, "google/veo-3.1", "completed", 8, 400_000_000)
	seedVideoJob(t, f, f.orgB, "google/veo-3.1", "completed", 8, 400_000_000)

	res, err := s.ListVideoJobs(context.Background(), gwconsoleapi.ListVideoJobsRequestObject{
		OrgId: orgParam(f.orgA),
	})
	if err != nil {
		t.Fatal(err)
	}
	items := res.(gwconsoleapi.ListVideoJobs200JSONResponse).Items
	if len(items) != 1 {
		t.Fatalf("%d jobs returned, want only this organization's one", len(items))
	}
	if items[0].Id != "vid_"+mine {
		t.Fatalf("job id = %q, want the one belonging to this organization", items[0].Id)
	}

	// Another organization's job id has to be indistinguishable from one that
	// never existed, so this is a not-found rather than a forbidden.
	_, err = s.GetVideoJob(context.Background(), gwconsoleapi.GetVideoJobRequestObject{
		OrgId: orgParam(f.orgB), VideoId: "vid_" + mine,
	})
	assertCode(t, err, errcode.GatewayResourceNotFound)
}

// A member without the financial capability sees the job and not the charge.
// The same rule the request log applies, for the same reason: operational facts
// are everyone's, money is not.
func TestVideoJobsRedactTheChargeWithoutFinance(t *testing.T) {
	f := newFixture(t)
	seedVideoJob(t, f, f.orgA, "google/veo-3.1", "completed", 8, 400_000_000)

	res, err := newConsoleServer(f.pool, memberAuthz{}).ListVideoJobs(
		context.Background(), gwconsoleapi.ListVideoJobsRequestObject{OrgId: orgParam(f.orgA)})
	if err != nil {
		t.Fatal(err)
	}
	items := res.(gwconsoleapi.ListVideoJobs200JSONResponse).Items
	if len(items) != 1 {
		t.Fatalf("%d jobs returned; the operational facts are every member's", len(items))
	}
	if items[0].ChargedNano != nil || items[0].ChargedCurrency != nil {
		t.Fatalf("the charge reached a caller without the financial capability: %+v", items[0])
	}
	if items[0].Status != "completed" {
		t.Fatalf("status = %q; redacting money must not redact the job", items[0].Status)
	}
}

// A failed job is listed, its reason is carried, and its charge is zero.
//
// All three matter together. A content refusal is the ordinary failure on this
// plane; hiding the row leaves "why did my video fail" unanswerable, hiding the
// message leaves it unanswered, and hiding the zero leaves the customer
// wondering whether they paid for it.
func TestAFailedVideoJobCarriesItsReasonAndCostsNothing(t *testing.T) {
	f := newFixture(t)
	id := seedVideoJob(t, f, f.orgA, "google/veo-3.1", "failed", 8, 0)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_async_jobs SET error_code = $2, error_message = $3 WHERE id = $1`,
		id, "gateway.video_content_rejected", "the prompt was refused"); err != nil {
		t.Fatal(err)
	}

	res, err := newConsoleServer(f.pool, allowAll{}).ListVideoJobs(
		context.Background(), gwconsoleapi.ListVideoJobsRequestObject{OrgId: orgParam(f.orgA)})
	if err != nil {
		t.Fatal(err)
	}
	job := res.(gwconsoleapi.ListVideoJobs200JSONResponse).Items[0]
	if job.Error == nil || job.Error.Code != "gateway.video_content_rejected" {
		t.Fatalf("the failure reason did not travel: %+v", job.Error)
	}
	if job.Error.Message == nil || *job.Error.Message == "" {
		t.Error("the upstream's own words were dropped; a bare code is a support ticket")
	}
	if job.ChargedNano == nil || *job.ChargedNano != 0 {
		t.Errorf("charged = %v; a job that produced no film is charged nothing, and the zero "+
			"has to be visible", job.ChargedNano)
	}
}

// Filtering by status narrows the list rather than being ignored.
func TestVideoJobsFilterByStatus(t *testing.T) {
	f := newFixture(t)
	seedVideoJob(t, f, f.orgA, "google/veo-3.1", "completed", 8, 1)
	seedVideoJob(t, f, f.orgA, "google/veo-3.1", "failed", 8, 0)

	failed := gwconsoleapi.ListVideoJobsParamsStatus("failed")
	res, err := newConsoleServer(f.pool, allowAll{}).ListVideoJobs(
		context.Background(), gwconsoleapi.ListVideoJobsRequestObject{
			OrgId:  orgParam(f.orgA),
			Params: gwconsoleapi.ListVideoJobsParams{Status: &failed},
		})
	if err != nil {
		t.Fatal(err)
	}
	items := res.(gwconsoleapi.ListVideoJobs200JSONResponse).Items
	if len(items) != 1 || items[0].Status != "failed" {
		t.Fatalf("the status filter did not narrow the list: %+v", items)
	}
}

// Cancelling and deleting go through the write gate, not the read one.
//
// They are the only two operations on this plane that change anything, and the
// distinction matters here more than usual: cancelling settles money. A read
// gate would have let anyone who can look at the list stop somebody else's job.
func TestVideoJobWritesGoThroughTheWriteGate(t *testing.T) {
	f := newFixture(t)
	id := seedVideoJob(t, f, f.orgA, "google/veo-3.1", "completed", 8, 1)
	s := newConsoleServer(f.pool, readOnlyAuthz{})

	if _, err := s.CancelVideoJob(context.Background(), gwconsoleapi.CancelVideoJobRequestObject{
		OrgId: orgParam(f.orgA), VideoId: "vid_" + id,
	}); !errors.Is(err, ErrWriteDenied) {
		t.Fatalf("cancel got past the write gate: %v", err)
	}
	if _, err := s.DeleteVideoJob(context.Background(), gwconsoleapi.DeleteVideoJobRequestObject{
		OrgId: orgParam(f.orgA), VideoId: "vid_" + id,
	}); !errors.Is(err, ErrWriteDenied) {
		t.Fatalf("delete got past the write gate: %v", err)
	}
}

// How far a job can be stopped comes from the route's declared envelope, the
// same column the catalogue publishes it from.
//
// It used to come from the vendor mapper, and the two disagreed in both
// directions: a route declaring `anytime` was refused while
// `GET /v1/videos/models` advertised it, and a route declaring `never` was
// cancelled anyway while the catalogue said it could not be. The Kling mapper
// says `queued_only`, so a route declaring `anytime` is the case that tells the
// two sources apart.
func TestCancelModeComesFromTheRouteEnvelope(t *testing.T) {
	f := newFixture(t)
	id := seedVideoJob(t, f, f.orgA, "kuaishou/kling-v2-master", "in_progress", 5, 0)
	routeWithEnvelope(t, f, id, "kuaishou", `{"cancel":"anytime","durations_seconds":[5]}`)

	res, err := newConsoleServer(f.pool, allowAll{}).GetVideoJob(
		context.Background(), gwconsoleapi.GetVideoJobRequestObject{
			OrgId: orgParam(f.orgA), VideoId: "vid_" + id,
		})
	if err != nil {
		t.Fatal(err)
	}
	job := gwconsoleapi.VideoJob(res.(gwconsoleapi.GetVideoJob200JSONResponse))
	if job.Cancel == nil || *job.Cancel != "anytime" {
		t.Fatalf("cancel = %v, want anytime -- the operator declared it and the catalogue "+
			"publishes it, so the console has to read the same column", job.Cancel)
	}
}

// An unset envelope reads as never, not as whatever the vendor happens to
// support: saying nothing is not a promise that a job can be stopped.
func TestAnUndeclaredEnvelopeCannotBeStopped(t *testing.T) {
	f := newFixture(t)
	id := seedVideoJob(t, f, f.orgA, "kuaishou/kling-v2-silent", "in_progress", 5, 0)
	routeWithEnvelope(t, f, id, "kuaishou", `{}`)

	res, err := newConsoleServer(f.pool, allowAll{}).GetVideoJob(
		context.Background(), gwconsoleapi.GetVideoJobRequestObject{
			OrgId: orgParam(f.orgA), VideoId: "vid_" + id,
		})
	if err != nil {
		t.Fatal(err)
	}
	job := gwconsoleapi.VideoJob(res.(gwconsoleapi.GetVideoJob200JSONResponse))
	if job.Cancel == nil || *job.Cancel != "never" {
		t.Fatalf("cancel = %v, want never", job.Cancel)
	}
}

// routeWithEnvelope pins a job to a route carrying the given envelope.
func routeWithEnvelope(t *testing.T, f *fixture, jobID, vendor, envelope string) {
	t.Helper()
	ctx := context.Background()
	var provider, route pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ($1,$2,ARRAY['video'],'https://api.example.test') RETURNING id`,
		"p-"+jobID[:8], vendor).Scan(&provider); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model_routes (model_id, provider_id, provider_model_id, video_envelope)
		 SELECT model_id, $2, 'upstream', $3::jsonb FROM gateway_async_jobs WHERE id = $1
		 RETURNING id`,
		jobID, provider, envelope).Scan(&route); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE gateway_async_jobs SET route_id = $2, provider_id = $3 WHERE id = $1`,
		jobID, route, provider); err != nil {
		t.Fatal(err)
	}
}

// seedVideoJob writes a terminal job row directly. The submit path needs an
// upstream; what these tests are about is the read model over the row it leaves.
func seedVideoJob(
	t *testing.T, f *fixture, org pgtype.UUID, model, status string, seconds, chargedNano int64,
) string {
	t.Helper()
	ctx := context.Background()
	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, max_output_tokens) VALUES ($1, 4096)
		 ON CONFLICT (slug) DO UPDATE SET slug = excluded.slug RETURNING id`,
		model).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	unique := strconv.FormatInt(time.Now().UnixNano(), 10)
	var id string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO gateway_async_jobs
		  (org_id, kind, request_id, idempotency_key, request_fingerprint,
		   model_id, model_slug, status, settlement_state, params, billing_units,
		   hold_nano, charged_nano, charged_currency, max_job_seconds, expires_at)
		VALUES ($1,'video',$2,$3,'fp',$4,$5,$6,'settled',
		        jsonb_build_object('prompt','a cat asleep','duration_seconds',$7::int),
		        jsonb_build_object('units', jsonb_build_array(
		            jsonb_build_object('unit','second','quantity',$7::int))),
		        0,$8,'USD',600, now() + interval '7 days')
		RETURNING id::text`,
		org, "req-"+unique, unique, modelID, model, status, seconds, chargedNano,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
