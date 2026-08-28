package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// VideoJobs is everything that can be done to a video job once it exists,
// expressed in terms of an organization rather than of a request.
//
// It exists because two transports need exactly these operations and must not
// have two implementations of them. The data plane reaches them with an API key
// and answers in the OpenAI error shape; the organization console reaches them
// with a session and answers in problem+json. Those are the only differences,
// and they belong to the handlers.
//
// Cancelling is the case that settles the argument. What "cancelled" means here
// is a chain of judgements -- the model's declared cancel mode, the upstream's
// own verdict deciding rather than the call completing, the hold voided however
// it ends -- and a second copy of that chain is how the two surfaces come to
// disagree about whether a customer was charged.
type VideoJobs struct{ p *Pipeline }

// VideoJobs returns the job surface. A method rather than a field so that the
// value cannot be assembled without the pipeline it needs.
func (p *Pipeline) VideoJobs() *VideoJobs { return &VideoJobs{p: p} }

// VideoJobFilter narrows a listing. Zero values mean "no filter"; the cursor is
// the id of the last row of the previous page.
type VideoJobFilter struct {
	Status    string
	ModelSlug string
	From      time.Time
	To        time.Time
	After     string
	Limit     int
}

// Get reads one job, scoped to the organization.
//
// A job belonging to another organization answers not-found, never forbidden: a
// job id from elsewhere has to be indistinguishable from one that never
// existed.
func (j *VideoJobs) Get(ctx context.Context, orgID pgtype.UUID, rawID string) (gwdb.GatewayAsyncJob, *Error) {
	jobID, ok := parseVideoJobID(rawID)
	if !ok {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayResourceNotFound, "Video job not found")
	}
	job, err := j.p.gw.GetVideoJob(ctx, gwdb.GetVideoJobParams{ID: jobID, OrgID: orgID})
	if err != nil {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayResourceNotFound, "Video job not found")
	}
	return job, nil
}

// List pages an organization's jobs, newest first. `more` says whether another
// page follows, answered by reading one row past the page rather than by
// counting: a count is a second query over the same predicate and disagrees
// with the page whenever a job is created between the two.
func (j *VideoJobs) List(
	ctx context.Context, orgID pgtype.UUID, f VideoJobFilter,
) (jobs []gwdb.GatewayAsyncJob, more bool, gerr *Error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, maxVideoJobPage)
	params := gwdb.ListVideoJobsForOrgFilteredParams{
		OrgID: orgID, Lim: int32(limit) + 1,
		Status: strings.TrimSpace(f.Status), ModelSlug: strings.TrimSpace(f.ModelSlug),
	}
	if !f.From.IsZero() {
		params.HasFrom, params.FromTs = true, pgtype.Timestamptz{Time: f.From, Valid: true}
	}
	if !f.To.IsZero() {
		params.HasTo, params.ToTs = true, pgtype.Timestamptz{Time: f.To, Valid: true}
	}
	// The cursor is resolved by reading the row it names rather than by
	// decoding a timestamp out of it. The row is the only thing that can say
	// what its own sort key was, and an id that no longer resolves simply
	// starts from the top instead of paging past a silently wrong instant.
	if after, ok := parseVideoJobID(f.After); ok {
		if prev, err := j.p.gw.GetVideoJob(ctx, gwdb.GetVideoJobParams{ID: after, OrgID: orgID}); err == nil {
			params.HasCursor = true
			params.CursorCreatedAt = prev.CreatedAt
			params.CursorID = prev.ID
		}
	}
	rows, err := j.p.gw.ListVideoJobsForOrgFiltered(ctx, params)
	if err != nil {
		return nil, false, NewError(errcode.GatewayInternal, "The job list could not be read")
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// Cancel stops a running job. Nothing is charged for it, however it ends.
func (j *VideoJobs) Cancel(
	ctx context.Context, orgID pgtype.UUID, rawID string,
) (gwdb.GatewayAsyncJob, *Error) {
	job, gerr := j.Get(ctx, orgID, rawID)
	if gerr != nil {
		return gwdb.GatewayAsyncJob{}, gerr
	}
	if job.Status != "queued" && job.Status != "in_progress" {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayJobNotCancelable,
			"This job has already finished")
	}
	route, cred, mapper, ok := j.p.videoRouteFor(ctx, job)
	if !ok {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayStateRouteUnavailable,
			"The upstream that created this job is no longer configured")
	}
	// The route's declared envelope decides, not the vendor mapper's own
	// default. The catalogue publishes cancel from the envelope
	// (`GET /v1/videos/models`), so reading it from the mapper here made the
	// two disagree in both directions: a route declaring `anytime` was refused
	// while the catalogue advertised it, and a route declaring `never` was
	// cancelled anyway while the catalogue said it could not be. One declared
	// fact, one reader (ADR-0221 decision six).
	switch VideoCancelMode(route.VideoEnvelope) {
	case video.CancelNever:
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayJobNotCancelable,
			"This model cannot cancel a job once it has been submitted")
	case video.CancelQueuedOnly:
		if job.Status != "queued" {
			return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayJobNotCancelable,
				"This model can only cancel a job that has not started generating")
		}
	}
	// The mapper still has a veto, and it is a different one: the envelope says
	// what an operator promised, this says whether the vendor published a cancel
	// at all. A declaration the upstream cannot carry out is refused here rather
	// than sent and silently ignored.
	out, err := mapper.Cancel(job.UpstreamID)
	if err != nil {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayJobNotCancelable, err.Error())
	}
	status, _, gerr := j.p.doVideoCall(ctx, route, cred, out, true)
	if gerr != nil {
		return gwdb.GatewayAsyncJob{}, gerr
	}
	// The upstream's own verdict decides, not the fact that the call completed.
	// A vendor that refuses because generation has already started answers 4xx
	// over a perfectly healthy connection, and marking the job cancelled anyway
	// would void the hold while the clip is still being made and billed to us.
	if status < 200 || status >= 300 {
		slog.WarnContext(ctx, "dataplane: the upstream refused a video cancel",
			"job", job.ID, "status", status)
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayJobNotCancelable,
			"The upstream refused to cancel this job; it is already generating")
	}
	j.p.terminateVideoJob(ctx, job, video.Poll{
		Status: video.StatusCanceled, UpstreamStatus: "canceled",
	}, string(video.StatusCanceled))
	// Re-read rather than patch the fields we happen to remember. Termination
	// moves the money as well as the status -- settlement_state, charged_nano,
	// terminal_at -- and the response now carries a capability derived from
	// settlement (`deletable`), so a hand-patched `Status` alone would answer
	// "you cannot delete this" about a job the very next request will delete.
	// If the re-read fails the caller still gets a correct-enough row: the
	// cancel itself succeeded, and that is what the status says.
	//
	// Untested end-to-end, deliberately: every vendor mapper the test fixtures
	// cover declares CancelNever, so reaching this line from HTTP would mean
	// building a cancel-capable upstream fixture for a four-line re-read.
	if fresh, gerr := j.Get(ctx, orgID, rawID); gerr == nil {
		return fresh, nil
	}
	job.Status = string(video.StatusCanceled)
	return job, nil
}

// videoDeleteRefusal is the one statement of when a job row may not be dropped,
// and it returns the refusal rather than a boolean so the two reasons keep their
// own messages.
//
// One function because there are three readers -- the endpoint that enforces it,
// the DTO that tells the interface whether to offer the control, and the
// retention sweep that must not undo it -- and three copies of a money rule
// drift in three directions.
func videoDeleteRefusal(job gwdb.GatewayAsyncJob) *Error {
	// Still running: dropping the row would strand the reservation with nothing
	// pointing at it while the upstream carries on generating a clip we are
	// billed for. Cancel it first, or wait for it to finish.
	switch job.Status {
	case "queued", "in_progress":
		return NewError(errcode.GatewayJobNotReady,
			"This job is still running; cancel it or wait for it to finish before deleting it")
	}
	// Terminal, but the money has not moved. This row is the only thing that
	// points at an outstanding hold: `held` waits for the generic sweeper to
	// void it, so a delivered video would go uncharged, and `protected` is never
	// swept at all -- ProtectHold pushes its expiry to infinity precisely so it
	// cannot be -- so the balance would stay reserved with nothing left able to
	// release it. It is also the row the operator's repair queue reads.
	if job.SettlementState == "held" || job.SettlementState == "protected" {
		return NewError(errcode.GatewayJobNotReady,
			"This job is still holding a reservation; its charge has to be settled or voided before it can be deleted")
	}
	return nil
}

// VideoJobDeletable reports whether Delete would accept this row.
//
// Exported so the console contract can carry the answer instead of the caller
// guessing at it from status alone -- the settlement half of the rule is not
// visible in the DTO, deliberately, so it cannot be re-derived client-side.
func VideoJobDeletable(job gwdb.GatewayAsyncJob) bool { return videoDeleteRefusal(job) == nil }

// Delete removes the job row and the artifact this deployment holds. It does
// not promise the upstream also forgets.
func (j *VideoJobs) Delete(ctx context.Context, orgID pgtype.UUID, rawID string) (gwdb.GatewayAsyncJob, *Error) {
	job, gerr := j.Get(ctx, orgID, rawID)
	if gerr != nil {
		return gwdb.GatewayAsyncJob{}, gerr
	}
	if refusal := videoDeleteRefusal(job); refusal != nil {
		return gwdb.GatewayAsyncJob{}, refusal
	}
	if job.ArtifactKey != "" {
		if err := j.p.artifacts.Delete(ctx, job.ArtifactKey); err != nil {
			slog.ErrorContext(ctx, "dataplane: deleting a video artifact failed",
				"error", err, "job", job.ID)
			return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayInternal, "The artifact could not be deleted")
		}
	}
	if err := j.p.gw.DeleteVideoJob(ctx, job.ID); err != nil {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayInternal, "The job could not be deleted")
	}
	return job, nil
}

// OpenContent hands back the film's bytes.
//
// The caller closes the reader. Where custody was taken the bytes come from the
// store; where it was not, they are fetched from the upstream on read, which
// works for as long as the upstream still has them. Either way they leave
// through this deployment: the upstream URL names the vendor, often carries its
// credential, and is one this deployment can neither renew nor revoke.
func (j *VideoJobs) OpenContent(
	ctx context.Context, orgID pgtype.UUID, rawID string,
) (io.ReadCloser, ArtifactInfo, *Error) {
	job, gerr := j.Get(ctx, orgID, rawID)
	if gerr != nil {
		return nil, ArtifactInfo{}, gerr
	}
	if job.Status != string(video.StatusCompleted) {
		return nil, ArtifactInfo{}, NewError(errcode.GatewayJobNotReady, "This job has not produced a video")
	}
	if job.ArtifactKey != "" {
		body, info, err := j.p.artifacts.Open(ctx, job.ArtifactKey)
		if err == nil {
			return body, info, nil
		}
		if !errors.Is(err, ErrArtifactGone) {
			return nil, ArtifactInfo{}, NewError(errcode.GatewayInternal, "The video could not be read")
		}
	}
	return j.p.openUpstreamArtifact(ctx, job)
}

// openUpstreamArtifact is the no-custody deployment's whole content path, and
// the fallback for a stored artifact that has been swept.
func (p *Pipeline) openUpstreamArtifact(
	ctx context.Context, job gwdb.GatewayAsyncJob,
) (io.ReadCloser, ArtifactInfo, *Error) {
	gone := NewError(errcode.GatewayArtifactGone, "This video is no longer available")
	if job.UpstreamArtifactRef == "" {
		return nil, ArtifactInfo{}, NewError(errcode.GatewayArtifactGone, "This video is no longer stored")
	}
	route, cred, mapper, ok := p.videoRouteFor(ctx, job)
	if !ok {
		return nil, ArtifactInfo{}, gone
	}
	// The same fetch the reconciler runs, including the second hop one vendor
	// needs. Reading it here from a private copy is how this path came to hand
	// back the *document* naming where the bytes are, with a video content
	// type on it, instead of the bytes.
	body, art, gerr := p.fetchVideoArtifact(ctx, route, cred, mapper, video.Poll{
		ArtifactRef: job.UpstreamArtifactRef, ContentType: job.ArtifactContentType,
	})
	if gerr != nil {
		// The upstream's own retention window has closed. That is a normal
		// outcome on a deployment that takes no custody, so it is stated as one
		// rather than rendered as a server fault.
		return nil, ArtifactInfo{}, gone
	}
	return io.NopCloser(bytes.NewReader(body)),
		ArtifactInfo{SizeBytes: int64(len(body)), ContentType: art.ContentType}, nil
}

// VideoJobID renders a job's caller-facing id, prefix included. Exported so
// that the two surfaces spell it the same way -- the prefix is part of the id a
// caller passes back, and two spellings of it are two ids.
func VideoJobID(job gwdb.GatewayAsyncJob) string { return "vid_" + uuidString(job.ID) }

// VideoCancelMode reads how far a job on this route can be stopped.
//
// One function so that admission, the catalogue and every interface reach the
// same answer from the same column. Unset reads as `never`: an operator who has
// not said a model can be stopped has not promised that it can.
func VideoCancelMode(envelope []byte) video.CancelMode {
	e, err := video.ParseEnvelope(envelope)
	if err != nil {
		return video.CancelNever
	}
	return e.CancelModeOrDefault()
}

// VideoJobDetail is what a job row says once its two JSON columns are read.
//
// Decoded here rather than by each renderer: `params` and `billing_units` are
// the row's own encoding of what was asked for and what is being charged, and a
// second decoder of either is a second opinion about what a customer ordered
// and what they owe.
type VideoJobDetail struct {
	Params      video.Request
	BilledUnits int64
	// BilledUnit is empty until the job reaches a terminal state.
	BilledUnit string
	// Cancel is how far this job can still be stopped -- the model's declared
	// capability, so that an interface can decide whether to offer a stop
	// control instead of offering one that refuses. Not a claim about this
	// instant: a queued-only job that has begun generating still refuses.
	Cancel video.CancelMode
}

// Details reads a page of jobs, resolving every route's declared cancel mode in
// one query rather than one per job.
//
// The per-job shape was an N+1: a hundred-row page issued a hundred and one
// queries, and all but one of them served a field the interface reads only on
// the rows that are still running. The result is positional, one entry per job.
func (j *VideoJobs) Details(ctx context.Context, jobs []gwdb.GatewayAsyncJob) []VideoJobDetail {
	cancels := j.cancelModes(ctx, jobs)
	out := make([]VideoJobDetail, 0, len(jobs))
	for _, job := range jobs {
		d := VideoJobDetail{Cancel: video.CancelNever}
		if err := json.Unmarshal(job.Params, &d.Params); err != nil {
			slog.WarnContext(ctx, "dataplane: a video job's parameters could not be read",
				"job", job.ID, "error", err)
		}
		for _, r := range unitRowsOf(job.BillingUnits) {
			d.BilledUnits += r.Quantity
			if d.BilledUnit == "" {
				d.BilledUnit = r.Unit
			}
		}
		if job.RouteID.Valid {
			if mode, ok := cancels[job.RouteID]; ok {
				d.Cancel = mode
			}
		}
		out = append(out, d)
	}
	return out
}

// Detail is Details for one job.
func (j *VideoJobs) Detail(ctx context.Context, job gwdb.GatewayAsyncJob) VideoJobDetail {
	return j.Details(ctx, []gwdb.GatewayAsyncJob{job})[0]
}

// cancelModes resolves the declared cancel mode of every route a page of jobs
// is pinned to.
//
// A read failure degrades to an empty map rather than taking the listing down:
// the jobs then render as not stoppable, which is the conservative reading and
// the one an unset envelope produces anyway.
func (j *VideoJobs) cancelModes(
	ctx context.Context, jobs []gwdb.GatewayAsyncJob,
) map[pgtype.UUID]video.CancelMode {
	ids := make([]pgtype.UUID, 0, len(jobs))
	seen := make(map[pgtype.UUID]bool, len(jobs))
	for _, job := range jobs {
		if job.RouteID.Valid && !seen[job.RouteID] {
			seen[job.RouteID] = true
			ids = append(ids, job.RouteID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := j.p.gw.ListVideoRouteEnvelopes(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "dataplane: the routes of a video job page could not be read",
			"error", err)
		return nil
	}
	out := make(map[pgtype.UUID]video.CancelMode, len(rows))
	for _, r := range rows {
		out[r.ID] = VideoCancelMode(r.VideoEnvelope)
	}
	return out
}
