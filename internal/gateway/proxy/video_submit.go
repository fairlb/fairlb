package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// Submitting a video job: the first point on this plane where money moves.
//
// The order is the whole of ADR-0220 and is worth keeping in view:
//
//  1. everything that can refuse has already refused (handler);
//  2. the hold is taken for the *exact* charge, not an estimate;
//  3. the row is written before the upstream is called, because the row is the
//     record that a hold exists -- the reverse order leaves a submitted job
//     that nothing points at, and a hold nothing will ever release;
//  4. the upstream is called;
//  5. the row learns the upstream's id.
//
// A crash between any two of those leaves a state the reconciler finishes: a
// row with no upstream id is swept and voided, and a row with one is polled.

// holdSlack is how much longer a hold lives than the job it covers. Three times
// the model's own ceiling, so an upstream running late does not lose its
// reservation while it is still working.
const holdSlack = 3

const (
	minVideoHoldTTL = 30 * time.Minute
	maxVideoHoldTTL = 6 * time.Hour
	// firstPollDelay is deliberately short: a queued job that fails validation
	// upstream usually says so within seconds, and voiding the hold quickly is
	// worth one extra call.
	firstPollDelay = 5 * time.Second
)

// videoJobResponse is the caller-facing shape of a job.
//
// The upstream's own identifier never appears here: it names the vendor, and
// upstream anonymity is an explicit property of this gateway (ADR-0222). The id
// a caller sees is ours.
type videoJobResponse struct {
	ID              string         `json:"id"`
	Object          string         `json:"object"`
	Model           string         `json:"model"`
	Status          string         `json:"status"`
	Progress        int            `json:"progress"`
	DurationSeconds int            `json:"duration_seconds,omitempty"`
	Resolution      string         `json:"resolution,omitempty"`
	CreatedAt       int64          `json:"created_at"`
	ExpiresAt       int64          `json:"expires_at"`
	Error           *videoJobError `json:"error,omitempty"`
}

type videoJobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func videoJobPayload(job gwdb.GatewayAsyncJob) videoJobResponse {
	out := videoJobResponse{
		ID: VideoJobID(job), Object: "video", Model: job.ModelSlug,
		Status: job.Status, Progress: int(job.Progress),
		CreatedAt: job.CreatedAt.Time.Unix(), ExpiresAt: job.ExpiresAt.Time.Unix(),
	}
	var params video.Request
	if err := json.Unmarshal(job.Params, &params); err == nil {
		out.DurationSeconds = params.DurationSeconds
		out.Resolution = params.Resolution
	}
	if job.ErrorCode != "" || job.ErrorMessage != "" {
		out.Error = &videoJobError{Code: job.ErrorCode, Message: job.ErrorMessage}
	}
	return out
}

// submitVideoJob takes the hold, writes the row, and calls the upstream.
func (p *Pipeline) submitVideoJob(
	ctx context.Context, prep prepared, in Request, req video.Request, audioOn bool,
	units catalog.Units, routes []catalog.Route,
	idempotencyKey, fingerprint string, maxJobSeconds int,
) (gwdb.GatewayAsyncJob, *Error) {
	// Rotation is allowed right up to the moment a job id exists and never
	// after -- the same rule streaming applies at the first byte. A submit is a
	// fast call that only enqueues work upstream, so a candidate that refuses
	// it costs nothing to move past (ADR-0220).
	var lastErr *Error
	for _, route := range routes {
		job, gerr := p.submitToRoute(ctx, prep, in, req, audioOn, units, route,
			idempotencyKey, fingerprint, maxJobSeconds)
		if gerr == nil {
			return job, nil
		}
		lastErr = gerr
		if !retryableVideoSubmit(gerr) {
			break
		}
	}
	if lastErr == nil {
		lastErr = NewError(errcode.GatewayAllProvidersFailed, "No upstream is able to serve this model")
	}
	return gwdb.GatewayAsyncJob{}, lastErr
}

// retryableVideoSubmit says whether the next candidate is worth trying.
//
// A refusal that is about the request itself will be refused identically
// everywhere, and trying again only spends another upstream call to learn the
// same thing.
func retryableVideoSubmit(e *Error) bool {
	switch e.Code {
	case errcode.GatewayInvalidRequest, errcode.GatewayVideoParamsUnsupported,
		errcode.GatewayInsufficientCredits, errcode.GatewayInternal:
		return false
	default:
		return true
	}
}

// submitToRoute is one candidate's attempt: pin a credential, write the row,
// call the upstream.
func (p *Pipeline) submitToRoute(
	ctx context.Context, prep prepared, in Request, req video.Request, audioOn bool,
	units catalog.Units, route catalog.Route,
	idempotencyKey, fingerprint string, maxJobSeconds int,
) (gwdb.GatewayAsyncJob, *Error) {
	mapper, ok := video.MapperFor(route.ProviderVendor)
	if !ok {
		slog.ErrorContext(ctx, "dataplane: no video mapper for vendor",
			"vendor", route.ProviderVendor, "model", prep.modelSlug)
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayAllProvidersFailed,
			"No upstream is able to serve this model")
	}

	// Resolved once, before the row is written, because the row has to record
	// which account the upstream job will belong to. Every later call on this
	// job -- poll, cancel, artifact fetch -- reads it back rather than picking
	// again.
	cred, gerr := p.pinVideoCredential(ctx, route, prep.byok)
	if gerr != nil {
		return gwdb.GatewayAsyncJob{}, gerr
	}

	// Priced after the credential is known, because whose account this runs on
	// changes what it costs: work billed to the organization's own upstream is
	// charged a service fee, not the list rate. Quoting before the route was
	// chosen made every BYOK video job pay full price for a clip the
	// organization had already paid the vendor for.
	quote, gerr := p.quoteVideoForCredential(ctx, prep, units, cred)
	if gerr != nil {
		return gwdb.GatewayAsyncJob{}, gerr
	}

	// Each attempt owns its own reservation. A candidate that refuses releases
	// it before the next one is tried, so a rotation never stacks holds.
	ttl := time.Duration(maxJobSeconds*holdSlack) * time.Second
	ttl = min(max(ttl, minVideoHoldTTL), maxVideoHoldTTL)
	holdID, gerr := p.settlementRecorder.ReserveFor(ctx, prep.id, in.RequestID, quote.ChargedNano, ttl)
	if gerr != nil {
		p.settlementRecorder.RecordHoldRejection(ctx, in, gerr, time.Now())
		return gwdb.GatewayAsyncJob{}, gerr
	}

	job, existing, gerr := p.createVideoJobRow(ctx, prep, in, req, quote, units, route,
		cred, holdID, ttl, idempotencyKey, fingerprint, maxJobSeconds)
	if gerr != nil {
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, in.RequestID)
		return gwdb.GatewayAsyncJob{}, gerr
	}
	if existing {
		// A concurrent retry of the same key won the insert. That job is
		// already being submitted by the attempt that created it, so this one
		// must not call the upstream again -- a second generation would be
		// billed by the vendor and would overwrite the winner's upstream id.
		// Its own reservation is released here rather than left for the
		// timeout sweeper.
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, in.RequestID)
		return job, nil
	}

	// From here a failure leaves a recoverable row rather than a lost hold, so
	// the upstream call may fail without the caller being charged.
	upstreamID, gerr := p.callVideoUpstream(ctx, mapper, route, cred, req, audioOn)
	if gerr != nil {
		// failVideoJob voids this attempt's hold, so the next candidate starts
		// from nothing reserved.
		p.failVideoJob(ctx, job, "gateway.all_providers_failed", gerr.Message)
		return gwdb.GatewayAsyncJob{}, gerr
	}
	if err := p.gw.MarkVideoJobSubmitted(ctx, gwdb.MarkVideoJobSubmittedParams{
		ID: job.ID, UpstreamID: upstreamID, UpstreamStatus: "submitted",
		Status: "in_progress", PollAfter: intervalOf(firstPollDelay),
	}); err != nil {
		slog.ErrorContext(ctx, "dataplane: recording the upstream job id failed",
			"error", err, "job", job.ID)
		p.failVideoJob(ctx, job, "gateway.internal", "the job could not be recorded")
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayInternal, "The job could not be recorded")
	}
	job.UpstreamID = upstreamID
	job.Status = "in_progress"
	return job, nil
}

// pinVideoCredential chooses the account this job will live on.
func (p *Pipeline) pinVideoCredential(
	ctx context.Context, route catalog.Route, byok byokChoices,
) (videoCredential, *Error) {
	keyID, apiKey, baseURL, byokKeyID, gerr := p.credentialFor(ctx, route, byok)
	if gerr != nil {
		return videoCredential{}, gerr
	}
	return videoCredential{
		APIKey: apiKey, BaseURL: baseURL,
		ProviderKeyID: keyID, OrgProviderKeyID: byokKeyID,
	}, nil
}

func (p *Pipeline) createVideoJobRow(
	ctx context.Context, prep prepared, in Request, req video.Request,
	quote catalog.Quote, units catalog.Units, route catalog.Route,
	cred videoCredential, holdID pgtype.UUID, ttl time.Duration,
	idempotencyKey, fingerprint string, maxJobSeconds int,
) (job gwdb.GatewayAsyncJob, existing bool, gerr *Error) {
	params, _ := json.Marshal(req)
	unitsJSON, _ := json.Marshal(unitsSnapshot(units))
	snapshot, _ := json.Marshal(map[string]any{
		// The same schema version the token path stamps. The unsettled-replay
		// decoder accepts exactly one shape, and a snapshot without it cannot
		// be replayed -- which is the path an orphaned hold settles through.
		"schema_version":             usagePricingSchemaVersion,
		"charged_nano":               quote.ChargedNano,
		"upstream_usd_nano":          quote.UpstreamUSDNano,
		"model_multiplier_bps":       quote.ModelMultiplierBps,
		"plan_multiplier_bps":        quote.PlanMultiplierBps,
		"procurement_multiplier_bps": quote.ProcurementMultiplierBps,
		"fx_rate":                    quote.FXRate,
		"unit_rates":                 prep.unitPriceTable.Snapshot(),
	})

	row, err := p.gw.CreateVideoJob(ctx, gwdb.CreateVideoJobParams{
		OrgID: prep.id.OrgID, ApiKeyID: prep.id.KeyID,
		RequestID: in.RequestID, IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint,
		ModelID: prep.res.Model.ID, ModelSlug: prep.modelSlug,
		RouteID: route.ID, ProviderID: route.ProviderID,
		// The credential is part of the pin: an upstream job id means nothing
		// on another account, so every later call reads this back rather than
		// choosing again.
		ProviderKeyID: cred.ProviderKeyID, OrgProviderKeyID: cred.OrgProviderKeyID,
		Byok:   cred.BYOK(),
		Params: params, BillingUnits: unitsJSON,
		MaxJobSeconds: int32(maxJobSeconds),
		HoldID:        holdID, HoldNano: quote.ChargedNano,
		HoldExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true},
		ChargedCurrency: prep.id.WalletCurrency,
		PricingSnapshot: snapshot, EndUserID: in.EndUserID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(p.catalog.Settings().VideoRetention(ctx)), Valid: true},
	})
	if err != nil {
		// A unique violation on the idempotency index is a retry that raced the
		// first attempt, not a fault. The caller gets the job that won, and the
		// `existing` flag tells the caller not to submit it a second time.
		if won, ok := p.videoJobForKey(ctx, prep.id.OrgID, idempotencyKey); ok {
			return won, true, nil
		}
		slog.ErrorContext(ctx, "dataplane: creating the video job row failed", "error", err)
		return gwdb.GatewayAsyncJob{}, false, NewError(errcode.GatewayInternal, "The job could not be created")
	}
	return row, false, nil
}

func (p *Pipeline) videoJobForKey(ctx context.Context, orgID pgtype.UUID, key string) (gwdb.GatewayAsyncJob, bool) {
	job, err := p.gw.GetVideoJobByIdempotencyKey(ctx, gwdb.GetVideoJobByIdempotencyKeyParams{
		OrgID: orgID, IdempotencyKey: key,
	})
	if err != nil {
		return gwdb.GatewayAsyncJob{}, false
	}
	return job, true
}

// callVideoUpstream shapes the request through the vendor's mapper and delivers
// it through the same outbound machinery the inference plane uses.
func (p *Pipeline) callVideoUpstream(
	ctx context.Context, mapper video.Mapper, route catalog.Route,
	cred videoCredential, req video.Request, audioOn bool,
) (string, *Error) {
	out, err := mapper.Submit(req, route.ProviderModelID, audioOn)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: shaping the video submit failed", "error", err)
		return "", NewError(errcode.GatewayInvalidRequest, err.Error())
	}
	status, body, gerr := p.doVideoCall(ctx, route, cred, out, true)
	if gerr != nil {
		return "", gerr
	}
	upstreamID, err := mapper.SubmitResult(req, status, body)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: the upstream refused the video submit",
			"error", err, "status", status, "provider", route.ProviderSlug)
		return "", NewError(errcode.GatewayAllProvidersFailed, "The upstream refused the request")
	}
	return upstreamID, nil
}

// videoCredential is the exact credential one job was created with.
//
// Carried explicitly rather than re-derived per call, because an upstream job
// id only means something on the account that created it: re-picking would
// round-robin onto a second shared key, or fall back from an organization's own
// key to a platform one, and either way the poll asks the wrong account about a
// job it has never heard of.
type videoCredential struct {
	APIKey  string
	BaseURL string
	// Exactly one of these is set, and it is what the job row records.
	ProviderKeyID    pgtype.UUID
	OrgProviderKeyID pgtype.UUID
}

// BYOK reports whether this job is served on the organization's own account.
func (c videoCredential) BYOK() bool { return c.OrgProviderKeyID.Valid }

// doVideoCall performs one mapper-shaped call against a route, with a
// credential the caller has already pinned.
//
// withCredential is honoured rather than assumed: an artifact served from a
// presigned CDN link carries its own authorisation, and attaching ours would
// hand the upstream API key to a third party.
func (p *Pipeline) doVideoCall(
	ctx context.Context, route catalog.Route, cred videoCredential, out video.Outbound, withCredential bool,
) (int, []byte, *Error) {
	target := Target{
		Protocol: ProtocolVideo, BaseURL: cred.BaseURL,
		Path: out.Path, Method: out.Method, Transport: route.Transport,
		UpstreamModel: route.ProviderModelID, ExtraQuery: out.Query,
		Headers: mergeThree(route.ProviderHeaders, route.RouteHeaders, out.Headers),
	}
	if withCredential {
		target.APIKey = cred.APIKey
	} else {
		target.OmitCredential = true
	}
	// An absolute URL replaces the provider address entirely. Artifacts live
	// wherever the upstream put them -- a vendor CDN, an object store -- and
	// joining that onto the provider's base URL produces a path that exists
	// nowhere.
	if out.URL != "" {
		target.AbsoluteURL = out.URL
	}
	req, err := BuildRequest(ctx, target, out.Body)
	if err != nil {
		return 0, nil, NewError(errcode.GatewayInternal, "The upstream request could not be built")
	}
	resp, err := p.clientFor(catalog.SurfaceVideo).Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: video upstream call failed", "error", err, "provider", route.ProviderSlug)
		return 0, nil, NewError(errcode.GatewayAllProvidersFailed, "The upstream could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBoundedBody(resp.Body)
	if err != nil {
		return 0, nil, NewError(errcode.GatewayInternal, "The upstream response could not be read")
	}
	return resp.StatusCode, body, nil
}

// failVideoJob marks a job terminal and releases its hold. Void first, because
// Void is idempotent and a crash between the two is repaired by the sweeper,
// whereas recording first would leave a job that looks resolved with money
// still held.
func (p *Pipeline) failVideoJob(ctx context.Context, job gwdb.GatewayAsyncJob, code, message string) {
	detached := context.WithoutCancel(ctx)
	p.settlementRecorder.VoidHold(detached, job.OrgID, job.RequestID)
	if err := fdb.WithSystemTx(detached, p.pool, func(tx pgx.Tx) error {
		q := p.gw.WithTx(tx)
		if _, err := q.MarkVideoJobTerminal(detached, gwdb.MarkVideoJobTerminalParams{
			ID: job.ID, Status: "failed", UpstreamStatus: "", ErrorCode: code, ErrorMessage: message,
		}); err != nil {
			return err
		}
		if _, err := q.VoidVideoJob(detached, job.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		slog.ErrorContext(detached, "dataplane: marking a video job failed did not persist",
			"error", err, "job", job.ID)
	}
}

// unitsSnapshot renders the billable quantity vector as an object, because the
// column is constrained to one -- an array here would be refused at insert, and
// wrapping keeps room for a total or a unit alongside the rows later.
func unitsSnapshot(u catalog.Units) map[string]any {
	rows := make([]map[string]any, 0, len(u.Quantities))
	for k, q := range u.Quantities {
		rows = append(rows, map[string]any{
			"unit": string(k.Unit), "resolution": k.Resolution,
			"audio": k.Audio, "service_tier": k.ServiceTier, "quantity": q,
		})
	}
	return map[string]any{"units": rows}
}

func intervalOf(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func uuidString(u pgtype.UUID) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// readBoundedBody reads an upstream response under the shared cap. A truncated
// body would parse as an unfinished job and quietly reset the poll ladder, so
// exceeding the bound is an error rather than a trim -- the same rule the
// inference path applies.
func readBoundedBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxUpstreamBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxUpstreamBody {
		return nil, fmt.Errorf("proxy: upstream response exceeded %d bytes", maxUpstreamBody)
	}
	return body, nil
}

// mergeThree layers the mapper's own headers over the two configured levels.
func mergeThree(providerLevel, routeLevel, mapperLevel map[string]string) map[string]string {
	merged := MergeHeaders(providerLevel, routeLevel)
	if len(mapperLevel) == 0 {
		return merged
	}
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range mapperLevel {
		merged[k] = v
	}
	return merged
}

// quoteVideoForCredential prices an admitted request against the account that
// will actually serve it.
func (p *Pipeline) quoteVideoForCredential(
	ctx context.Context, prep prepared, units catalog.Units, cred videoCredential,
) (catalog.Quote, *Error) {
	list, cost := prep.billingUnitPrices()
	return p.quoteOrRefuse(ctx, prep, func() (catalog.Quote, error) {
		if cred.BYOK() {
			return catalog.ComputeUnitsBYOK(list, units, prep.pricing.byokFeeBps, prep.pricing.rates)
		}
		return catalog.ComputeUnits(list, cost, units, prep.pricing.rates)
	})
}
