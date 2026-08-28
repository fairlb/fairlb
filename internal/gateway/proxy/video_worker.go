package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/jobs"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/settle"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// The reconciler: the worker ADR-0220 exists to build, and the one
// resource_handlers.go has been waiting for.
//
// A video job outlives the request that created it, so something other than
// that request has to observe its end and move the money. Two shapes, matching
// the pair the route prober already uses:
//
//   - a scan that claims due jobs and polls them. Claiming and rescheduling are
//     one statement, so a second replica takes a different batch rather than
//     the same one.
//   - a sweep that finishes what polling cannot: holds taken for jobs never
//     submitted, jobs that never reach a terminal state, and rows past their
//     retention.
//
// Nothing here is driven by the caller. A GET of a job is a pure read: letting
// it drive the poll would leave a hold outstanding whenever a caller walks
// away, and turn a caller polling in a loop into an amplifier against the
// upstream.

const (
	videoScanInterval  = 15 * time.Second
	videoSweepInterval = 5 * time.Minute
	videoClaimBatch    = 50
	// pollLease is how long a claimed job is left alone. It bounds how quickly
	// a crashed poller's job is retried, and it is longer than any single poll.
	pollLease = 90 * time.Second

	// The poll ladder. Video generation takes tens of seconds to minutes, and
	// several vendors rate-limit their status endpoint, so a flat five-second
	// poll is both wasteful and self-defeating.
	pollBaseQueued     = 5 * time.Second
	pollBaseInProgress = 10 * time.Second
	pollMax            = 2 * time.Minute

	// notFoundStrikes is how many consecutive 404s make a job gone. One cannot
	// tell "forgotten" from "being rolled between replicas" -- the same
	// reasoning the probe table gives for a single 404.
	notFoundStrikes = 2
)

// VideoScanArgs claims due jobs and polls them.
type VideoScanArgs struct{}

func (VideoScanArgs) Kind() string { return "gateway_video_job_scan" }

// VideoSweepArgs finishes what polling cannot.
type VideoSweepArgs struct{}

func (VideoSweepArgs) Kind() string { return "gateway_video_job_sweep" }

// VideoScanPeriodicJob polls in-flight video jobs.
func VideoScanPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(videoScanInterval,
		func() (river.JobArgs, *river.InsertOpts) { return VideoScanArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true})
}

// VideoSweepPeriodicJob reclaims orphans and expired artifacts.
func VideoSweepPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(videoSweepInterval,
		func() (river.JobArgs, *river.InsertOpts) { return VideoSweepArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true})
}

// VideoScanWorker polls every job whose next poll is due.
type VideoScanWorker struct {
	river.WorkerDefaults[VideoScanArgs]
	p *Pipeline
}

// NewVideoScanWorker builds the poller.
func NewVideoScanWorker(p *Pipeline) *VideoScanWorker { return &VideoScanWorker{p: p} }

func (w *VideoScanWorker) Work(ctx context.Context, _ *river.Job[VideoScanArgs]) error {
	due, err := w.p.gw.ClaimDueVideoJobs(ctx, gwdb.ClaimDueVideoJobsParams{
		Lease: intervalOf(pollLease), Lim: videoClaimBatch,
	})
	if err != nil {
		return err
	}
	for _, job := range due {
		w.p.pollVideoJob(ctx, job)
	}
	return nil
}

// VideoSweepWorker reclaims what polling alone cannot finish.
type VideoSweepWorker struct {
	river.WorkerDefaults[VideoSweepArgs]
	p *Pipeline
}

// NewVideoSweepWorker builds the sweeper.
func NewVideoSweepWorker(p *Pipeline) *VideoSweepWorker { return &VideoSweepWorker{p: p} }

func (w *VideoSweepWorker) Work(ctx context.Context, _ *river.Job[VideoSweepArgs]) error {
	p := w.p

	// 1. A hold was taken and nothing was ever submitted: the process died
	//    between the two. The caller has nothing, so they pay nothing.
	unsubmitted, err := p.gw.ListUnsubmittedVideoJobs(ctx, gwdb.ListUnsubmittedVideoJobsParams{
		OlderThan: intervalOf(2 * time.Minute), Lim: videoClaimBatch,
	})
	if err != nil {
		return err
	}
	for _, job := range unsubmitted {
		slog.WarnContext(ctx, "dataplane: voiding a video job that was never submitted", "job", job.ID)
		p.failVideoJob(ctx, job, "gateway.internal", "the job was never submitted upstream")
	}

	// 2. Never terminal within twice its own ceiling. Bounded by the model's
	//    declared maximum rather than one global constant, because a short clip
	//    and a long render are an order of magnitude apart.
	// Bounded per job by the model's own ceiling, which the row carries.
	stale, err := p.gw.ListStaleVideoJobs(ctx, videoClaimBatch)
	if err != nil {
		return err
	}
	for _, job := range stale {
		slog.WarnContext(ctx, "dataplane: expiring a video job that never finished", "job", job.ID)
		p.terminateVideoJob(ctx, job, video.Poll{
			Status: "expired", UpstreamStatus: job.UpstreamStatus,
			ErrorCode: "gateway.upstream_timeout", ErrorMessage: "the upstream never finished this job",
		}, "expired")
	}

	// 3. Retention. The artifact goes first: a row without an object is a 404,
	//    which is correct, while an object without a row is an unreferenced
	//    blob nothing will ever delete.
	//
	//    The row only follows when the money has moved. Retention is about the
	//    media; a row whose charge is still held or protected is the only thing
	//    pointing at that reservation, and dropping it here would undo on a
	//    timer exactly what Delete refuses to do by hand -- and empty the
	//    operator's repair queue by destroying its contents rather than by
	//    anyone repairing them. Such a row keeps its place in the queue with its
	//    artifact already gone, which is the honest state: the clip's retention
	//    expired, the charge has not.
	expired, err := p.gw.ListExpiredVideoJobs(ctx, videoClaimBatch)
	if err != nil {
		return err
	}
	for _, job := range expired {
		if job.ArtifactKey != "" && p.artifacts != nil {
			if err := p.artifacts.Delete(ctx, job.ArtifactKey); err != nil {
				slog.ErrorContext(ctx, "dataplane: deleting an expired video artifact failed",
					"error", err, "job", job.ID)
				continue // retry next sweep rather than orphan the object
			}
		}
		if refusal := videoDeleteRefusal(job); refusal != nil {
			// Clear the key so the next pass does not re-delete an object that
			// is already gone; the row stays for whoever settles or voids it.
			if err := p.gw.ClearVideoJobArtifact(ctx, job.ID); err != nil {
				slog.ErrorContext(ctx, "dataplane: clearing an expired video artifact reference failed",
					"error", err, "job", job.ID)
				continue
			}
			slog.WarnContext(ctx, "dataplane: an expired video job is kept because its charge never settled",
				"job", job.ID, "settlement_state", job.SettlementState, "status", job.Status)
			continue
		}
		if err := p.gw.DeleteVideoJob(ctx, job.ID); err != nil {
			slog.ErrorContext(ctx, "dataplane: deleting an expired video job row failed", "error", err, "job", job.ID)
		}
	}
	return nil
}

// pollVideoJob asks one upstream where a job has got to.
func (p *Pipeline) pollVideoJob(ctx context.Context, job gwdb.GatewayAsyncJob) {
	route, cred, mapper, ok := p.videoRouteFor(ctx, job)
	if !ok {
		// The route that created this job is gone or unusable. The job cannot
		// be reached on any other -- an upstream id means nothing on a
		// different account -- so it ends here and the hold is released.
		p.failVideoJob(ctx, job, "gateway.state_route_unavailable",
			"the upstream that created this job is no longer configured")
		return
	}
	out, err := mapper.Poll(job.UpstreamID)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: shaping a video poll failed", "error", err, "job", job.ID)
		return
	}
	status, body, gerr := p.doVideoCall(ctx, route, cred, out, true)
	if gerr != nil {
		// A transport failure is not a verdict. The job keeps its lease and is
		// polled again; only the sweeper's ceiling ends it.
		return
	}
	poll, err := mapper.PollResult(status, body)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: reading a video poll failed", "error", err, "job", job.ID)
		return
	}

	if poll.NotFound {
		count, err := p.gw.MarkVideoJobNotFound(ctx, gwdb.MarkVideoJobNotFoundParams{
			ID: job.ID, PollAfter: intervalOf(pollBaseInProgress),
		})
		if err != nil {
			return
		}
		if count >= notFoundStrikes {
			p.terminateVideoJob(ctx, job, video.Poll{
				Status: video.StatusFailed, UpstreamStatus: "NOT_FOUND",
				ErrorCode: "gateway.resource_not_found", ErrorMessage: "the upstream no longer has this job",
			}, "expired")
		}
		return
	}

	if !poll.Status.Terminal() {
		p.keepVideoHoldAlive(ctx, job)
		if err := p.gw.MarkVideoJobProgress(ctx, gwdb.MarkVideoJobProgressParams{
			ID: job.ID, Status: string(poll.Status), UpstreamStatus: poll.UpstreamStatus,
			Progress: int16(poll.Progress), PollAfter: intervalOf(nextPollDelay(poll.Status, int(job.PollAttempts))),
		}); err != nil {
			slog.ErrorContext(ctx, "dataplane: recording video job progress failed", "error", err, "job", job.ID)
		}
		return
	}
	p.terminateVideoJob(ctx, job, poll, string(poll.Status))

	// Custody is taken as soon as the job completes, not when a caller first
	// asks: several upstreams keep a generated video for a day or two, and a
	// lazy fetch would usually find the window closed (ADR-0222).
	if poll.Status == video.StatusCompleted {
		p.captureVideoArtifact(ctx, job, route, cred, mapper, poll)
	}
}

// nextPollDelay is the backoff ladder.
func nextPollDelay(status video.Status, attempts int) time.Duration {
	base, growth := pollBaseInProgress, 1.4
	if status == video.StatusQueued {
		base, growth = pollBaseQueued, 1.5
	}
	d := float64(base)
	for range min(attempts, 20) {
		d *= growth
		if time.Duration(d) >= pollMax {
			return pollMax
		}
	}
	return time.Duration(d)
}

// keepVideoHoldAlive stops the reservation sweeper reclaiming a job that is
// still running.
//
// ProtectHold's stated purpose is the window where the service is delivered but
// the charge cannot yet be computed; here it is read one notch earlier, while
// the service is still being delivered. When the reservation is already gone,
// the job is marked orphaned and settled late through the unsettled queue --
// the one path that is allowed to debit an expired reservation.
func (p *Pipeline) keepVideoHoldAlive(ctx context.Context, job gwdb.GatewayAsyncJob) {
	if job.SettlementState != "held" || !job.HoldExpiresAt.Valid {
		return
	}
	if time.Until(job.HoldExpiresAt.Time) > 15*time.Minute {
		return
	}
	kept, err := p.billing.ProtectHold(ctx, job.OrgID, job.RequestID)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: protecting a video hold failed", "error", err, "job", job.ID)
		return
	}
	if kept {
		if err := p.gw.ProtectVideoJobHold(ctx, job.ID); err != nil {
			slog.ErrorContext(ctx, "dataplane: recording a protected video hold failed", "error", err, "job", job.ID)
		}
		return
	}
	slog.WarnContext(ctx, "dataplane: a video job's reservation was already reclaimed", "job", job.ID)
	if err := p.gw.OrphanVideoJobHold(ctx, job.ID); err != nil {
		slog.ErrorContext(ctx, "dataplane: recording an orphaned video hold failed", "error", err, "job", job.ID)
	}
}

// terminateVideoJob writes the terminal state and moves the money, in one
// transaction, guarded so that a duplicate poll settles nothing.
func (p *Pipeline) terminateVideoJob(
	ctx context.Context, job gwdb.GatewayAsyncJob, poll video.Poll, status string,
) {
	// Settlement outlives the request that triggered it.
	ctx = context.WithoutCancel(ctx)
	charged := job.HoldNano
	settling := status == string(video.StatusCompleted)
	if !settling {
		// Failed, cancelled or expired: nothing was produced, so nothing is
		// charged. Void first because Void is idempotent -- a crash between the
		// two is repaired by the next sweep, whereas recording first would
		// leave a job that looks resolved with money still held.
		p.settlementRecorder.VoidHold(ctx, job.OrgID, job.RequestID)
		charged = 0
	}

	err := fdb.WithSystemTx(ctx, p.pool, func(tx pgx.Tx) error {
		q := p.gw.WithTx(tx)
		moved, err := q.MarkVideoJobTerminal(ctx, gwdb.MarkVideoJobTerminalParams{
			ID: job.ID, Status: status, UpstreamStatus: poll.UpstreamStatus,
			ErrorCode: poll.ErrorCode, ErrorMessage: poll.ErrorMessage,
			UpstreamArtifactRef:       poll.ArtifactRef,
			UpstreamArtifactExpiresAt: timestamptzOf(poll.ArtifactExpiry),
		})
		if err != nil {
			return err
		}
		if moved == 0 {
			// Another poller got there first.
			return errVideoAlreadyTerminal
		}
		if settling {
			claimed, err := q.SettleVideoJob(ctx, gwdb.SettleVideoJobParams{ID: job.ID, ChargedNano: charged})
			if err != nil {
				return err
			}
			if claimed == 0 {
				return errVideoAlreadyTerminal
			}
			if job.SettlementState == "orphaned" {
				// An expired reservation may only be debited late, through the
				// queue that exists for exactly that. The terminal state and
				// the usage row are still written here; only the ledger entry
				// is deferred.
				if err := p.insertVideoUsageLog(ctx, q, job, poll, status, charged); err != nil {
					return err
				}
				return p.recordVideoUnsettled(ctx, tx, job, charged)
			}
			if err := p.billing.SettleTx(ctx, tx, settle.SettleInput{
				OrgID: job.OrgID, RequestID: job.RequestID,
				ActualNano: charged, APIKeyID: job.ApiKeyID,
			}); err != nil {
				return err
			}
		} else if _, err := q.VoidVideoJob(ctx, job.ID); err != nil {
			return err
		}
		return p.insertVideoUsageLog(ctx, q, job, poll, status, charged)
	})
	switch {
	case err == nil:
	case errors.Is(err, errVideoAlreadyTerminal):
		// Not a fault: two pollers raced and one of them lost.
	default:
		slog.ErrorContext(ctx, "dataplane: settling a video job failed; it will be retried",
			"error", err, "job", job.ID, "status", status)
	}
}

var errVideoAlreadyTerminal = errors.New("proxy: this video job was already finished by another poller")

// videoRouteFor loads the exact route and credential a job was pinned to.
//
// It reads the route by id and does not re-run admission. Polling needs two
// things -- where to send the request, and which account created the upstream
// job -- and neither is a question about whether the model is still admissible
// today. Re-resolving made an ordinary catalog edit (hiding a model, renaming
// an upstream model, which resets every probe verdict) mark every in-flight job
// failed while the upstream carried on generating clips we had been billed for.
func (p *Pipeline) videoRouteFor(
	ctx context.Context, job gwdb.GatewayAsyncJob,
) (catalog.Route, videoCredential, video.Mapper, bool) {
	if !job.RouteID.Valid {
		return catalog.Route{}, videoCredential{}, nil, false
	}
	row, err := p.gw.GetVideoJobRoute(ctx, job.RouteID)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: the route a video job was pinned to is gone",
			"job", job.ID, "route", job.RouteID)
		return catalog.Route{}, videoCredential{}, nil, false
	}
	mapper, ok := video.MapperFor(row.Vendor)
	if !ok {
		return catalog.Route{}, videoCredential{}, nil, false
	}
	transport, err := catalog.ParseTransport(row.ProviderTransport)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: the pinned route's transport profile is unreadable",
			"job", job.ID, "error", err)
		return catalog.Route{}, videoCredential{}, nil, false
	}
	route := catalog.Route{
		ID: row.ID, ProviderID: row.ProviderID, ProviderSlug: row.ProviderSlug,
		ProviderVendor: row.Vendor, Protocol: catalog.ProtocolVideo, BaseURL: row.BaseUrl,
		ProviderModelID: row.ProviderModelID, Transport: transport,
		ProviderHeaders: decodeHeaderMap(row.ProviderHeaders),
		RouteHeaders:    decodeHeaderMap(row.RouteHeaders),
		// Carried so that cancelling reads the declared envelope rather than
		// the vendor mapper's default -- the catalogue publishes it from here.
		VideoEnvelope: row.VideoEnvelope,
	}
	cred, ok := p.credentialOfJob(ctx, job, route)
	if !ok {
		return catalog.Route{}, videoCredential{}, nil, false
	}
	return route, cred, mapper, true
}

// credentialOfJob reads back the exact credential the submit pinned.
//
// Never re-picked: pickKey round-robins across a provider's keys, so a second
// call would ask key B about a job that only exists under key A, and a job
// created on an organization's own credential would be polled with a platform
// one. Both look identical from here -- a 404 -- and end with a finished job
// marked expired and its charge voided.
func (p *Pipeline) credentialOfJob(
	ctx context.Context, job gwdb.GatewayAsyncJob, route catalog.Route,
) (videoCredential, bool) {
	switch {
	case job.OrgProviderKeyID.Valid:
		row, err := p.gw.GetOrgProviderKeySecret(ctx, gwdb.GetOrgProviderKeySecretParams{
			ID: job.OrgProviderKeyID, OrgID: job.OrgID,
		})
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: the organization credential a video job used is gone",
				"job", job.ID)
			return videoCredential{}, false
		}
		plain, err := p.box.Open(row.SecretEnc, row.ID.Bytes[:])
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: decrypting the organization credential failed",
				"job", job.ID, "error", err)
			return videoCredential{}, false
		}
		baseURL := route.BaseURL
		if row.BaseUrl.Valid && row.BaseUrl.String != "" {
			baseURL = row.BaseUrl.String
		}
		return videoCredential{
			APIKey: string(plain), BaseURL: baseURL, OrgProviderKeyID: job.OrgProviderKeyID,
		}, true
	case job.ProviderKeyID.Valid:
		row, err := p.gw.GetVideoJobProviderKey(ctx, job.ProviderKeyID)
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: the shared credential a video job used is gone", "job", job.ID)
			return videoCredential{}, false
		}
		plain, err := p.box.Open(row.SecretEnc, row.ID.Bytes[:])
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: decrypting the shared credential failed",
				"job", job.ID, "error", err)
			return videoCredential{}, false
		}
		return videoCredential{
			APIKey: string(plain), BaseURL: route.BaseURL, ProviderKeyID: job.ProviderKeyID,
		}, true
	default:
		slog.ErrorContext(ctx, "dataplane: a video job records no credential", "job", job.ID)
		return videoCredential{}, false
	}
}

// decodeHeaderMap reads a stored header map, treating an unreadable one as
// empty rather than failing the call it belongs to.
func decodeHeaderMap(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func timestamptzOf(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// insertVideoUsageLog writes the billing record for a finished job.
//
// A failed or cancelled job gets a row too, with a charge of zero. Without it
// "why did my video fail" has no answer anywhere the organization can see, and
// content-policy refusals -- the common failure on this plane -- would be
// invisible.
func (p *Pipeline) insertVideoUsageLog(
	ctx context.Context, q *gwdb.Queries, job gwdb.GatewayAsyncJob,
	poll video.Poll, status string, charged int64,
) error {
	return q.InsertUsageLog(ctx, videoUsageLogParams(job, poll, status, charged))
}

// videoUsageLogParams maps a job onto the shared usage row.
//
// Every token column stays zero: this plane is billed by unit, and the
// quantities live in the price snapshot. Any dashboard reading tokens per model
// must therefore exclude this surface -- that is a property of the dashboard,
// not a reason for a second ledger. The charge has to land in this table
// because per-key spend accumulates from it, and a key with no spend record has
// no enforceable budget (ADR-0220).
func videoUsageLogParams(
	job gwdb.GatewayAsyncJob, poll video.Poll, status string, charged int64,
) gwdb.InsertUsageLogParams {
	outcome := "ok"
	switch status {
	case string(video.StatusFailed):
		outcome = "upstream_error"
		if poll.ErrorCode == "gateway.video_content_rejected" {
			// A refusal by the upstream's own policy is the caller's request
			// being declined, not the upstream breaking.
			outcome = "client_error"
		}
	case string(video.StatusCanceled):
		outcome = "canceled"
	case "expired":
		outcome = "upstream_error"
	}
	var durationMs int32
	if job.SubmittedAt.Valid {
		durationMs = int32(time.Since(job.SubmittedAt.Time).Milliseconds())
	}
	return gwdb.InsertUsageLogParams{
		OrgID: job.OrgID, ApiKeyID: job.ApiKeyID, RequestID: job.RequestID,
		Surface: string(catalog.SurfaceVideo), ModelSlug: job.ModelSlug,
		ProviderID: job.ProviderID, ProviderKeyID: job.ProviderKeyID, RouteID: job.RouteID,
		Status: outcome, HttpStatus: 200, Byok: job.Byok, RouteAttempts: 1,
		ChargedNano: charged, ChargedCurrency: job.ChargedCurrency,
		UpstreamCostUsdNano: upstreamCostFrom(job.PricingSnapshot),
		PricingSnapshot:     job.PricingSnapshot, HoldID: job.HoldID,
		OrgProviderKeyID: job.OrgProviderKeyID,
		// One attempt: route rotation happens before a job id exists and never
		// after, so there is never a second attempt to describe.
		Attempts:  []byte(`[]`),
		ToolCalls: []byte(`{}`),
		// The billable quantity, so "how many seconds did this organization
		// generate" is a column that can be summed rather than a document that
		// has to be parsed.
		BilledUnits: billedUnitsOf(job.BillingUnits),
		BilledUnit:  billedUnitOf(job.BillingUnits),
		// Valid-but-zero rather than NULL, for the same reason a unit-priced
		// model records explicit zeros for its four token rates: NULL in this
		// schema means "not reported", and these are known to be absent. It is
		// also load-bearing -- the unsettled-replay encoder refuses a row whose
		// token dimensions are missing, and the orphaned-hold path settles
		// video through exactly that queue.
		TokensAudioIn:      pgtype.Int4{Valid: true},
		TokensAudioOut:     pgtype.Int4{Valid: true},
		TokensImageIn:      pgtype.Int4{Valid: true},
		TokensCacheWrite5m: pgtype.Int4{Valid: true},
		TokensCacheWrite1h: pgtype.Int4{Valid: true},
		// Valid-but-empty rather than NULL, matching the token path: NULL in
		// this schema means "unknown", and these are known to be absent.
		ServiceTier: pgtype.Text{Valid: true},
		FxRate:      numericFromString(fxRateFrom(job.PricingSnapshot)),
		// Deliberately not set on this plane: a video job runs for minutes, and
		// letting that into the request-latency histogram destroys it. Job
		// duration is reported separately (ADR-0220).
		DurationMs: durationMs,
		EndUserID:  job.EndUserID,
		ErrorCode:  poll.ErrorCode,
	}
}

// recordVideoUnsettled routes a settlement whose reservation is already gone to
// the queue that exists for exactly that: an expired reservation may only be
// debited late, and that path already knows how.
func (p *Pipeline) recordVideoUnsettled(
	ctx context.Context, _ pgx.Tx, job gwdb.GatewayAsyncJob, charged int64,
) error {
	params := videoUsageLogParams(job, video.Poll{}, string(video.StatusCompleted), charged)
	p.settlementRecorder.RecordUnsettled(ctx, job.RequestID,
		Identity{OrgID: job.OrgID, KeyID: job.ApiKeyID},
		catalog.Quote{ChargedNano: charged}, params,
		errors.New("the reservation was reclaimed before the job finished"))
	return nil
}

// fetchVideoArtifact gets a finished job's bytes, following the second hop
// where the vendor needs one.
//
// One implementation, reached from both places that fetch: the reconciler
// taking custody, and the no-custody read path serving a caller directly. Two
// copies is how one of them ends up handing back the *document* that names
// where the bytes are instead of the bytes -- which is exactly what happened
// while this lived only in the reconciler.
//
// Exactly one further hop is followed. A mapper asking for a second would be
// saying the artifact shape is wrong again, and looping on it would turn that
// into an unbounded fetch rather than a bug report.
func (p *Pipeline) fetchVideoArtifact(
	ctx context.Context, route catalog.Route, cred videoCredential,
	mapper video.Mapper, poll video.Poll,
) ([]byte, video.Artifact, *Error) {
	art, err := mapper.Artifact(poll)
	if err != nil {
		return nil, video.Artifact{}, NewError(errcode.GatewayArtifactGone,
			"This job has no artifact to fetch")
	}
	status, body, gerr := p.doVideoCall(ctx, route, cred, art.Request, art.NeedsUpstreamCredential)
	if gerr != nil {
		return nil, video.Artifact{}, gerr
	}
	if status < 200 || status >= 300 {
		return nil, video.Artifact{}, NewError(errcode.GatewayArtifactGone,
			"This video is no longer available from the upstream")
	}
	if !art.Indirect {
		return body, art, nil
	}
	// One vendor answers the poll with a file id rather than a URL, so the
	// first call returns a document naming where the bytes are.
	resolved, err := mapper.ResolveArtifact(status, body)
	if err != nil || resolved.Indirect {
		return nil, video.Artifact{}, NewError(errcode.GatewayArtifactGone,
			"This video's location could not be resolved")
	}
	status, body, gerr = p.doVideoCall(ctx, route, cred, resolved.Request, resolved.NeedsUpstreamCredential)
	if gerr != nil {
		return nil, video.Artifact{}, gerr
	}
	if status < 200 || status >= 300 {
		return nil, video.Artifact{}, NewError(errcode.GatewayArtifactGone,
			"This video is no longer available from the upstream")
	}
	return body, resolved, nil
}

// captureVideoArtifact takes the bytes into custody.
//
// Run on the completion transition rather than on first read: several upstreams
// keep a generated video for a day or two, so a lazy fetch would usually find
// the window closed. A deployment that takes no custody skips it entirely
// rather than spending a download it would throw away.
func (p *Pipeline) captureVideoArtifact(
	ctx context.Context, job gwdb.GatewayAsyncJob, route catalog.Route,
	cred videoCredential, mapper video.Mapper, poll video.Poll,
) {
	if !p.artifacts.Takes() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	body, art, gerr := p.fetchVideoArtifact(ctx, route, cred, mapper, poll)
	if gerr != nil {
		// Left for the next sweep: the row still carries the upstream
		// reference, and the caller can still be served by proxying on read in
		// the meantime.
		slog.ErrorContext(ctx, "dataplane: fetching a video artifact failed",
			"job", job.ID, "error", gerr.Message)
		return
	}
	key, err := p.artifacts.Put(ctx, ArtifactRef{
		OrgID: uuidString(job.OrgID), JobID: uuidString(job.ID), ContentType: art.ContentType,
	}, bytesReader(body), int64(len(body)))
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: storing a video artifact failed", "error", err, "job", job.ID)
		return
	}
	if err := p.gw.RecordVideoJobArtifact(ctx, gwdb.RecordVideoJobArtifactParams{
		ID: job.ID, ArtifactKey: key, ArtifactBytes: int64(len(body)),
		ArtifactContentType: art.ContentType,
	}); err != nil {
		slog.ErrorContext(ctx, "dataplane: recording a stored video artifact failed", "error", err, "job", job.ID)
	}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// upstreamCostFrom reads the cost basis back out of the snapshot the job row
// stored at admission.
//
// The charge was final then; this number was not, because the route that would
// serve the job had not been chosen. It is recorded as the best figure
// available rather than left at zero, and the snapshot carries the multipliers
// that produced it, so a margin report can be recomputed from the row alone.
// fxRateFrom reads back the exchange rate the charge was computed with, so the
// row stays recomputable on its own.
func fxRateFrom(snapshot []byte) string {
	var s struct {
		FXRate string `json:"fx_rate"`
	}
	if err := json.Unmarshal(snapshot, &s); err != nil || s.FXRate == "" {
		return "1"
	}
	return s.FXRate
}

// billedUnitsOf and billedUnitOf read the quantity vector stored on the job.
//
// Absent rather than zero when the vector cannot be read: NULL means "this
// request is not billed by unit", and a zero would claim a unit-billed request
// that consumed nothing.
func billedUnitsOf(raw []byte) pgtype.Int4 {
	rows := unitRowsOf(raw)
	if len(rows) == 0 {
		return pgtype.Int4{}
	}
	var total int64
	for _, r := range rows {
		total += r.Quantity
	}
	return pgtype.Int4{Int32: int32(min(total, math.MaxInt32)), Valid: true}
}

func billedUnitOf(raw []byte) string {
	rows := unitRowsOf(raw)
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Unit
}

type unitRow struct {
	Unit     string `json:"unit"`
	Quantity int64  `json:"quantity"`
}

func unitRowsOf(raw []byte) []unitRow {
	var doc struct {
		Units []unitRow `json:"units"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc.Units
}

func upstreamCostFrom(snapshot []byte) int64 {
	var s struct {
		UpstreamUSDNano int64 `json:"upstream_usd_nano"`
	}
	if err := json.Unmarshal(snapshot, &s); err != nil {
		return 0
	}
	return s.UpstreamUSDNano
}
