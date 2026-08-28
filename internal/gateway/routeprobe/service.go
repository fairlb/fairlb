package routeprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/jobs"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Service owns the stored side of probing: seeding rows, enqueueing jobs,
// resetting verdicts, writing the operator's overrides, and reading them back.
type Service struct {
	pool  *pgxpool.Pool
	q     *gwdb.Queries
	river *river.Client[pgx.Tx]
}

func NewService(pool *pgxpool.Pool, jobs *river.Client[pgx.Tx]) *Service {
	return &Service{pool: pool, q: gwdb.New(pool), river: jobs}
}

// Args is the probe job for one route.
//
// Endpoints narrows the job to the endpoints named; empty means every endpoint
// the route is automatically probed on. A re-probe requested by live traffic
// or by the sweeper names the endpoint in question, so that a verdict already
// green is not re-bought every time a red one ages.
type Args struct {
	RouteID   uuid.UUID `json:"route_id"`
	Endpoints []string  `json:"endpoints,omitempty"`
}

func (Args) Kind() string { return "gateway_route_probe" }

// Status values stored in model_route_probes.status. The two thresholds they
// serve are described on the table: the data plane excludes only
// StatusUnsupported, the catalog publishes only StatusOK.
const (
	StatusUnverified  = "unverified"
	StatusOK          = "ok"
	StatusUnsupported = "unsupported"
	StatusFailed      = "failed"

	// The other source, "operator", has no constant here: every operator verdict
	// is written by catalog.sql, which cannot reference a Go identifier anyway.
	SourceProbe = "probe"
)

// Seed writes the unverified rows a route has on a provider that speaks the
// given protocols: one per probeable endpoint of each protocol. Existing rows
// are left alone, so widening a provider's protocol set adds rows without
// disturbing verdicts already reached.
func Seed(ctx context.Context, q *gwdb.Queries, routeID pgtype.UUID, providerProtocols []string) error {
	for _, ep := range Probeable(catalog.EndpointsForProtocols(providerProtocols)) {
		protocol, _ := catalog.ProtocolForEndpoint(ep)
		mode, _ := catalog.ProbeModeForEndpoint(ep)
		if err := q.SeedRouteProbe(ctx, gwdb.SeedRouteProbeParams{
			RouteID: routeID, Endpoint: ep, Protocol: protocol, ProbeMode: string(mode),
		}); err != nil {
			return fmt.Errorf("routeprobe: seed the initial probe row: %w", err)
		}
	}
	return nil
}

// Enqueue seeds the initial rows and enqueues the job inside the caller's
// transaction.
//
// The two have to share a transaction. A route written without its job
// enqueued sits at "unverified" forever with nothing coming along to fix it
// but the sweeper. The reverse -- job enqueued, route never written -- is
// harmless: the worker does not find the route and skips it as deleted. One is
// a silent dead configuration, the other is a no-op.
func Enqueue(
	ctx context.Context, q *gwdb.Queries, client *river.Client[pgx.Tx],
	tx pgx.Tx, routeID pgtype.UUID, providerProtocols []string,
) error {
	if err := Seed(ctx, q, routeID, providerProtocols); err != nil {
		return err
	}
	if client == nil {
		// No job queue injected (tests, or a degraded assembly): the initial
		// rows are seeded and probing is left to a manual trigger. Not an
		// error -- probe results are extra information, and missing them must
		// not fail creating a route.
		return nil
	}
	if len(AutoProbeable(catalog.EndpointsForProtocols(providerProtocols))) == 0 {
		return nil // nothing this route could be probed on without spending money
	}
	if _, err := client.InsertTx(ctx, tx, Args{RouteID: uuid.UUID(routeID.Bytes)}, nil); err != nil {
		return fmt.Errorf("routeprobe: enqueue probe job: %w", err)
	}
	return markEnqueued(ctx, q, routeID, nil)
}

// markEnqueued records that a probe of these endpoints is in flight, so the
// interface can say so and stop offering to buy a second one.
//
// A nil endpoint list means the job will probe every automatically probed
// endpoint, which is the worker's own rule and is expressed in SQL through the
// stored `probe_mode` column rather than restated here.
//
// Failing to mark is a warning, never an error: the job is already enqueued,
// and refusing the write that enqueued it because a display field could not be
// set would trade a real outcome for a cosmetic one.
func markEnqueued(ctx context.Context, q *gwdb.Queries, routeID pgtype.UUID, endpoints []string) error {
	if endpoints == nil {
		endpoints = []string{}
	}
	if err := q.MarkRouteProbesEnqueued(ctx, gwdb.MarkRouteProbesEnqueuedParams{
		RouteID: routeID, Endpoints: endpoints,
	}); err != nil {
		slog.WarnContext(ctx, "routeprobe: failed to mark the probe as in flight; it still runs",
			"route_id", uuid.UUID(routeID.Bytes), "error", err)
	}
	return nil
}

// Reprobe re-probes after a change, in its own transaction rather than the one
// that performed the update.
//
// The update is already committed, and failing to enqueue a re-probe must not
// roll it back: invalidating the old verdict is mandatory, while re-probing is
// only "get a new verdict soon". They do not carry the same weight -- which is
// why every failure here is a warning and none is returned.
func (s *Service) Reprobe(ctx context.Context, routeID pgtype.UUID) {
	r, err := s.q.RouteForProbe(ctx, routeID)
	if err != nil {
		slog.WarnContext(ctx, "failed to read the route to re-probe, the verdict stays at its old value", "error", err)
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to begin the re-probe transaction, the verdict stays at its old value", "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Reset the old verdicts to unverified first: once the upstream model name
	// changed, that green no longer means anything.
	if err := s.q.WithTx(tx).ResetRouteProbes(ctx, routeID); err != nil {
		slog.WarnContext(ctx, "failed to reset the probe verdict", "error", err)
		return
	}
	if err := Enqueue(ctx, s.q.WithTx(tx), s.river, tx, routeID, r.Protocols); err != nil {
		slog.WarnContext(ctx, "failed to enqueue the re-probe", "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.WarnContext(ctx, "failed to commit the re-probe", "error", err)
	}
}

// requestWindow is how long one (route, endpoint) re-probe request stays unique
// in the queue. Live traffic can hit a misconfigured route hundreds of times a
// minute; every one of those asks the same question, and one probe answers it.
const requestWindow = 10 * time.Minute

// requestTimeout bounds the queue insert made on behalf of a live request.
// Cancellation is stripped so the client hanging up does not lose the ask,
// but a stalled database must not hold the ask -- or the request -- open.
const requestTimeout = 2 * time.Second

// dedupedWithinWindow is the insert policy for asks that are expected to
// repeat: many identical requests in one window collapse into one job. River
// counts a completed job in the window as a duplicate too, so an ask that must
// not be swallowed by an earlier identical one (the operator's) does not use
// it.
func dedupedWithinWindow() *river.InsertOpts {
	return &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: requestWindow}}
}

// Request asks for a probe of one endpoint on one route, on behalf of live
// traffic that just saw the upstream answer 404 or 405 there.
//
// Traffic never writes a verdict itself. A 404 on a live request does not say
// "this endpoint is unsupported": it says the same thing when the upstream
// model name is wrong, when a deployment is being rolled, or when the upstream
// withdrew the model for an hour. Writing `unsupported` from one such response
// would take a working route out of rotation on the strength of a blip. The
// worker, with the provider's shared credential and the same request builder
// the data plane uses, is the one source of verdicts -- traffic only points it
// at the question.
//
// The request path must not wait on this: the insert runs off the request's
// goroutine, detached from its cancellation but under its own short deadline,
// deduplicated by River, and a failure is logged, not returned. Endpoints that
// are never probed automatically (images) are not requested either -- that
// promise to the operator stands, and such an endpoint is not a candidate
// until an operator says so, so a 404 there is a verdict already being
// overridden rather than a question.
func (s *Service) Request(ctx context.Context, routeID pgtype.UUID, endpoint string) {
	if s.river == nil || !autoProbeable(endpoint) {
		return
	}
	detached := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(detached, requestTimeout)
		defer cancel()
		_, err := s.river.Insert(ctx, Args{RouteID: uuid.UUID(routeID.Bytes), Endpoints: []string{endpoint}},
			dedupedWithinWindow())
		if err != nil {
			slog.WarnContext(ctx, "routeprobe: failed to request a probe after an upstream 404",
				"route_id", uuid.UUID(routeID.Bytes), "endpoint", endpoint, "error", err)
			return
		}
		_ = markEnqueued(ctx, s.q, routeID, []string{endpoint})
	}()
}

// ReseedProvider realigns every route of a provider with its new protocol set,
// inside the caller's transaction: rows for protocols it no longer speaks are
// deleted, rows for protocols it now speaks are seeded, and each route is
// enqueued so the new rows get a verdict.
func (s *Service) ReseedProvider(
	ctx context.Context, tx pgx.Tx, providerID pgtype.UUID, protocols []string,
) error {
	q := s.q.WithTx(tx)
	if err := q.DeleteRouteProbesOutsideProtocols(ctx, gwdb.DeleteRouteProbesOutsideProtocolsParams{
		ProviderID: providerID, Protocols: protocols,
	}); err != nil {
		return fmt.Errorf("routeprobe: drop probe rows for protocols no longer spoken: %w", err)
	}
	routes, err := q.ListRouteIDsForProvider(ctx, providerID)
	if err != nil {
		return fmt.Errorf("routeprobe: list the provider's routes: %w", err)
	}
	for _, id := range routes {
		if err := Enqueue(ctx, q, s.river, tx, id, protocols); err != nil {
			return err
		}
	}
	return nil
}

// EnqueueProviderRoutes asks for a probe of every route on a provider. The
// caller runs it when the provider gains its first credential: routes created
// before that were deferred by the worker, and nothing else would come for
// them. A later credential changes nothing about what the routes serve, so it
// does not re-buy every verdict.
//
// Best effort, like Reprobe: the credential is already saved, and a queue
// hiccup must not turn that into an error.
func (s *Service) EnqueueProviderRoutes(ctx context.Context, providerID pgtype.UUID) {
	if s.river == nil {
		return
	}
	routes, err := s.q.ListRouteIDsForProvider(ctx, providerID)
	if err != nil {
		slog.WarnContext(ctx, "routeprobe: failed to list routes to probe after a key was added", "error", err)
		return
	}
	for _, id := range routes {
		if _, err := s.river.Insert(ctx, Args{RouteID: uuid.UUID(id.Bytes)}, nil); err != nil {
			slog.WarnContext(ctx, "routeprobe: failed to enqueue a probe after a key was added",
				"route_id", uuid.UUID(id.Bytes), "error", err)
			continue
		}
		_ = markEnqueued(ctx, s.q, id, nil)
	}
}

// Override records the operator's verdict for one endpoint of one route. The
// row is marked as theirs and the worker leaves it alone from then on. The
// caller has checked that the endpoint belongs to a protocol the provider
// speaks.
func (s *Service) Override(ctx context.Context, routeID pgtype.UUID, endpoint, status string) error {
	protocol, ok := catalog.ProtocolForEndpoint(endpoint)
	if !ok {
		return fmt.Errorf("routeprobe: unknown endpoint %q", endpoint)
	}
	mode, _ := catalog.ProbeModeForEndpoint(endpoint)
	if err := s.q.SetRouteProbeOverride(ctx, gwdb.SetRouteProbeOverrideParams{
		RouteID: routeID, Endpoint: endpoint, Protocol: protocol, ProbeMode: string(mode), Status: status,
	}); err != nil {
		return fmt.Errorf("routeprobe: write the operator's verdict: %w", err)
	}
	return nil
}

// ClearOverride hands one endpoint back to the worker, unverified, and asks
// for a probe so that it does not stay that way. The ask is the operator's and
// is not deduplicated against whatever traffic asked for in the same window:
// an identical job that already ran would otherwise swallow it, and the
// contract says the probe follows. Manual endpoints are not probed for free
// by clearing; the operator runs them.
func (s *Service) ClearOverride(ctx context.Context, routeID pgtype.UUID, endpoint string) error {
	if err := s.q.ClearRouteProbeOverride(ctx, gwdb.ClearRouteProbeOverrideParams{
		RouteID: routeID, Endpoint: endpoint,
	}); err != nil {
		return fmt.Errorf("routeprobe: clear the operator's verdict: %w", err)
	}
	if s.river == nil || !autoProbeable(endpoint) {
		return nil
	}
	if _, err := s.river.Insert(ctx, Args{RouteID: uuid.UUID(routeID.Bytes), Endpoints: []string{endpoint}}, nil); err != nil {
		return fmt.Errorf("routeprobe: enqueue the probe after clearing the override: %w", err)
	}
	return markEnqueued(ctx, s.q, routeID, []string{endpoint})
}

// Probe asks for a probe of one route on the endpoints named (all of the
// automatically probeable ones when empty), on the operator's request.
func (s *Service) Probe(ctx context.Context, routeID pgtype.UUID, endpoints []string) error {
	if s.river == nil {
		return fmt.Errorf("routeprobe: no job queue to run the probe on")
	}
	if _, err := s.river.Insert(ctx, Args{RouteID: uuid.UUID(routeID.Bytes), Endpoints: endpoints}, nil); err != nil {
		return fmt.Errorf("routeprobe: enqueue the probe: %w", err)
	}
	return markEnqueued(ctx, s.q, routeID, endpoints)
}

// Worker sends one minimal request per endpoint a route is probed on.
type Worker struct {
	river.WorkerDefaults[Args]
	q   *gwdb.Queries
	box *crypto.Box
	// river is for the one job the worker books itself: the confirming second
	// look at a verified endpoint that answered 404. Nil in a degraded
	// assembly, in which case the sweeper's daily pass is the second look.
	river *river.Client[pgx.Tx]
	// hc is the client the probe sends on. The worker deliberately holds no
	// handler: workers are assembled before the routes are (the job queue has
	// to start first), so none exists yet.
	hc *http.Client
}

func NewWorker(pool *pgxpool.Pool, box *crypto.Box, jobs *river.Client[pgx.Tx]) *Worker {
	return &Worker{
		q: gwdb.New(pool), box: box, river: jobs,
		hc: &http.Client{Timeout: Timeout, Transport: httpx.UpstreamTransport()},
	}
}

func (w *Worker) Work(ctx context.Context, job *river.Job[Args]) error {
	routeID := pgtype.UUID{Bytes: job.Args.RouteID, Valid: true}
	// However this pass ends, nothing on this route is in flight afterwards.
	// Each verdict already clears its own row as it lands -- that is what makes
	// the badges stop one at a time -- and this catches what is left: an
	// operator's row the upsert refused to touch, an endpoint skipped because
	// the provider has no credential yet, a route deleted under the job.
	//
	// Detached from the job's context so that a cancelled or timed-out pass
	// still clears: a marker that outlives its job is one an operator can never
	// clear themselves, and the interface would refuse to let them ask again.
	defer func() {
		clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
		defer cancel()
		if err := w.q.ClearRouteProbesEnqueued(clearCtx, routeID); err != nil {
			slog.WarnContext(clearCtx, "routeprobe: failed to clear the in-flight marker; the sweeper will",
				"route_id", job.Args.RouteID, "error", err)
		}
	}()
	r, err := w.q.RouteForProbe(ctx, routeID)
	if err != nil {
		// A deleted route means the job is stale, not failed. Returning an
		// error would make the queue retry a job that can never succeed.
		slog.InfoContext(ctx, "the probe job's route no longer exists, skipping", "route_id", job.Args.RouteID)
		return nil
	}
	keys, err := w.q.GetProviderKeysForProvider(ctx, r.ProviderID)
	if err != nil || len(keys) == 0 {
		// Having no credential yet is an intermediate configuration state --
		// creating the route before adding the key is a normal order -- and
		// must not be recorded as a failed probe. That would show a screen full
		// of red and send someone off to investigate an outage that does not
		// exist. Adding the key enqueues the probe again.
		slog.InfoContext(ctx, "the provider has no key yet, deferring the probe", "route_id", job.Args.RouteID)
		return nil
	}
	plain, err := w.box.Open(keys[0].SecretEnc, keys[0].ID.Bytes[:])
	if err != nil {
		return fmt.Errorf("routeprobe: decrypt the key for probing: %w", err)
	}
	target := Target{
		BaseURL: r.BaseUrl, Protocols: r.Protocols, Vendor: r.Vendor,
		Headers: r.ProviderHeaders, Transport: r.ProviderTransport,
	}
	// The endpoint set is derived from the provider's protocols, the same
	// table the data plane routes with; a job that names endpoints is kept to
	// those, but never to one the provider does not speak.
	spoken := catalog.EndpointsForProtocols(r.Protocols)
	endpoints := AutoProbeable(spoken)
	if len(job.Args.Endpoints) > 0 {
		endpoints = slices.DeleteFunc(Probeable(job.Args.Endpoints), func(ep string) bool {
			return !slices.Contains(spoken, ep)
		})
	}
	var confirm []string
	for _, ep := range endpoints {
		// The asynchronous probe never captures a trace: its result is written
		// to the database, and the trace contains the credential in clear text,
		// so capturing it would put that credential into a table operators can
		// query -- exactly what this feature refuses to do.
		res := Run(ctx, w.hc, target, string(plain), r.ProviderModelID, ep,
			uuid.UUID(keys[0].ID.Bytes), false)
		protocol, _ := catalog.ProtocolForEndpoint(ep)
		mode, _ := catalog.ProbeModeForEndpoint(ep)
		verdict := VerdictOf(res)
		stored, err := w.q.SaveRouteProbe(ctx, gwdb.SaveRouteProbeParams{
			RouteID: routeID, Endpoint: ep, Protocol: protocol, ProbeMode: string(mode), Status: verdict,
			LatencyMs:  int4FromPtr(res.LatencyMs),
			StatusCode: int4FromPtr(res.StatusCode),
			Error:      res.Message,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // the operator's row: theirs, not overwritten
			}
			return fmt.Errorf("routeprobe: save probe verdict: %w", err)
		}
		// A verified endpoint answered 404 once: the upsert kept the verdict
		// and recorded the answer. One sample is not a conclusion, so the
		// worker books the second look itself rather than waiting a day for
		// the sweeper -- and the same answer then is.
		if verdict == StatusUnsupported && stored == StatusOK {
			confirm = append(confirm, ep)
		}
	}
	if len(confirm) > 0 && w.river != nil {
		if _, err := w.river.Insert(ctx, Args{RouteID: job.Args.RouteID, Endpoints: confirm},
			&river.InsertOpts{ScheduledAt: time.Now().Add(confirmAfter)}); err != nil {
			return fmt.Errorf("routeprobe: schedule the confirming probe: %w", err)
		}
	}
	return nil
}

// confirmAfter is how long the worker waits before the second look at a
// verified endpoint that just answered 404. Long enough for a deployment roll
// to finish, short enough that a real withdrawal is acted on within the hour
// the upsert's confirmation window allows.
const confirmAfter = 5 * time.Minute

// VerdictOf turns one probe's outcome into the stored status.
//
// Only a 404 or 405 is definitive: the upstream was reached, took the
// credential, and said there is nothing at that address for that model. A 400
// or 422 is not -- the probe's minimal body is a guess at what every upstream
// will accept, and a relay that rejects it has said nothing about whether a
// real request would work. Everything else that is not a success (5xx,
// timeouts, quota, credentials) is the provider's or the key's problem, which
// the circuit breakers already own. Those all stay inconclusive: shown red,
// never used to take a candidate out of rotation.
func VerdictOf(res Result) string {
	if res.OK {
		return StatusOK
	}
	if res.StatusCode != nil && (*res.StatusCode == http.StatusNotFound || *res.StatusCode == http.StatusMethodNotAllowed) {
		return StatusUnsupported
	}
	return StatusFailed
}

// int4FromPtr converts a nullable int to pgtype.Int4. Both latency and status
// are absent when the connection itself failed.
func int4FromPtr(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

// ===== The sweeper =====

// Verdicts age. An upstream that withdrew a model and brought it back, a relay
// that added an endpoint, a route created before its provider had a key: none
// of these produce an event the gateway can see, so the only way to find out
// is to ask again. The sweeper re-probes aged verdicts on a schedule, on the
// automatically probeable endpoints only, and leaves the operator's overrides
// alone.

const (
	// sweepInterval is how often the sweeper looks for aged verdicts.
	sweepInterval = time.Hour
	// unverifiedGrace is how long an unverified row is left for the route's
	// own job before the sweeper picks it up.
	unverifiedGrace = 10 * time.Minute
	// verdictTTL is how long a red verdict stands before it is re-checked.
	verdictTTL = 24 * time.Hour
	// sweepBatch bounds one sweep; what it does not reach this hour it reaches
	// the next.
	sweepBatch = 500
	// inFlightTTL is how long an in-flight marker may stand before the sweeper
	// declares the job that set it dead.
	//
	// The worker clears its own marker however its pass ends, so a row still
	// marked after this long means the process holding the job went away
	// between marking and answering. Generously longer than any probe: this is
	// a backstop, and clearing early would re-offer a probe that is still
	// running -- on the endpoints that cost a real generation, that is the
	// second charge the marker exists to prevent.
	inFlightTTL = 2 * time.Hour
)

type SweepArgs struct{}

func (SweepArgs) Kind() string { return "gateway_route_probe_sweep" }

type SweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	q     *gwdb.Queries
	river *river.Client[pgx.Tx]
}

func NewSweepWorker(pool *pgxpool.Pool, jobs *river.Client[pgx.Tx]) *SweepWorker {
	return &SweepWorker{q: gwdb.New(pool), river: jobs}
}

func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	rows, err := w.q.ListRouteProbesDueForReprobe(ctx, gwdb.ListRouteProbesDueForReprobeParams{
		UnverifiedAfter: interval(unverifiedGrace),
		VerdictAfter:    interval(verdictTTL),
		MaxRows:         sweepBatch,
	})
	if err != nil {
		return fmt.Errorf("routeprobe: list aged verdicts: %w", err)
	}
	// The query already returns only rows the worker can advance; here they
	// are only grouped per route so one job carries every aged endpoint of it.
	byRoute := map[uuid.UUID][]string{}
	var order []uuid.UUID
	for _, r := range rows {
		id := uuid.UUID(r.RouteID.Bytes)
		if _, seen := byRoute[id]; !seen {
			order = append(order, id)
		}
		byRoute[id] = append(byRoute[id], r.Endpoint)
	}
	enqueued := 0
	for _, id := range order {
		if _, err := w.river.Insert(ctx, Args{RouteID: id, Endpoints: byRoute[id]}, dedupedWithinWindow()); err != nil {
			return fmt.Errorf("routeprobe: enqueue a sweep probe: %w", err)
		}
		enqueued++
	}
	if enqueued > 0 {
		slog.InfoContext(ctx, "route probe sweep enqueued re-probes", "routes", enqueued, "verdicts", len(rows))
	}
	// Markers left by a worker that died. Without this they stand forever, and
	// forever is the one duration an operator cannot wait out: the interface
	// would never let them ask for that probe again.
	stale, err := w.q.SweepStaleProbeEnqueued(ctx, pgtype.Timestamptz{
		Time: time.Now().Add(-inFlightTTL), Valid: true,
	})
	if err != nil {
		return fmt.Errorf("routeprobe: clear stale in-flight markers: %w", err)
	}
	if stale > 0 {
		slog.WarnContext(ctx, "route probe sweep cleared in-flight markers left by a worker that never answered",
			"rows", stale)
	}
	return nil
}

func SweepPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		sweepInterval,
		func() (river.JobArgs, *river.InsertOpts) { return SweepArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
