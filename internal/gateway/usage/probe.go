package usage

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/jobs"

	"github.com/fairlb/fairlb/foundation/alert"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Scheduled liveness probing, complementary to the passive circuit breaker.
//
// The breaker only notices a failure once real traffic hits it. Probing finds
// the failure during quiet periods, and re-enables a provider once it recovers.
// Without it, a provider that dies at 3am stays in rotation until the first
// unlucky user runs into it.

// ProbeArgs is the provider liveness job.
type ProbeArgs struct{}

func (ProbeArgs) Kind() string { return "gateway_provider_probe" }

// ProbeInterval is how often providers are probed.
const ProbeInterval = 5 * time.Minute

// probeTimeout bounds one probe: it only has to establish liveness, so there is
// no reason to wait out a long response.
const probeTimeout = 10 * time.Second

// Alerter is where operational alerts go; the notifier is injected at the
// assembly point (ADR-0190: one declaration, in foundation).
type Alerter = alert.Sink

// ProbeWorker periodically checks each provider's reachability, auto-disabling
// on failure and auto-enabling on recovery.
type ProbeWorker struct {
	river.WorkerDefaults[ProbeArgs]
	pool    *pgxpool.Pool
	q       *gwdb.Queries
	client  *http.Client
	alerter Alerter
}

func NewProbeWorker(pool *pgxpool.Pool, q *gwdb.Queries, alerter Alerter) *ProbeWorker {
	return &ProbeWorker{
		pool: pool, q: q, alerter: alerter,
		client: &http.Client{Timeout: probeTimeout, Transport: httpx.UpstreamTransport()},
	}
}

func (w *ProbeWorker) Work(ctx context.Context, _ *river.Job[ProbeArgs]) error {
	rows, err := w.q.ListProvidersForProbe(ctx)
	if err != nil {
		return fmt.Errorf("gateway: list providers to probe: %w", err)
	}
	for _, p := range rows {
		healthy := w.probeOne(ctx, p.BaseUrl, p.Transport)
		w.applyResult(ctx, p, healthy)
	}
	return nil
}

// probeOne establishes liveness with a lightweight, dialect-independent
// endpoint: free to call and with no side effects.
//
// It does not branch by protocol. GET /v1/models is a shared endpoint every
// protocol supports, and a multi-dialect provider has no single dialect to branch
// on anyway. The question here is "is this upstream alive", not "does this one
// dialect work" -- the latter belongs to the per-endpoint route probes.
//
// The provider's transport profile decides the address. Probing the standard
// path on an upstream that keeps its catalog elsewhere, or that requires a
// query parameter, produces a 404 on a perfectly healthy machine -- and this
// worker acts on its verdict by disabling the provider, so a wrong address here
// takes a working upstream out of rotation on a schedule.
//
// No credential is sent, so the profile's auth mode does not apply: 401 and 403
// already count as alive.
func (w *ProbeWorker) probeOne(ctx context.Context, baseURL string, rawTransport []byte) bool {
	transport, err := catalog.ParseTransport(rawTransport)
	if err != nil {
		slog.ErrorContext(ctx, "transport profile is not valid json, probing the default address", "error", err)
	}
	// Never streamed: this is a GET at the catalog address, so the streaming
	// override set -- which exists for upstreams that put streaming somewhere
	// else -- is not the one to read.
	target := baseURL + transport.PathFor(catalog.PathModels, "", false)
	if len(transport.Query) > 0 {
		u, perr := url.Parse(target)
		if perr != nil {
			return false
		}
		q := u.Query()
		for k, v := range transport.Query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		target = u.String()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	// A 401 or 403 means the service is alive and the probe simply carried no
	// credentials -- that still counts as healthy. Only a connection failure or
	// a 5xx is treated as unavailable.
	return resp.StatusCode < 500
}

// applyResult auto-disables or auto-recovers a provider from a probe result.
func (w *ProbeWorker) applyResult(ctx context.Context, p gwdb.ListProvidersForProbeRow, healthy bool) {
	switch {
	case !healthy && p.Enabled:
		if err := w.q.SetProviderAutoDisabled(ctx, p.ID); err != nil {
			slog.ErrorContext(ctx, "auto-disabling the provider after a failed probe did not work", "provider", p.Slug, "error", err)
			return
		}
		slog.WarnContext(ctx, "probe failed, provider auto-disabled", "provider", p.Slug)
		w.alert(ctx, "Provider auto-disabled", fmt.Sprintf("Provider %s failed its liveness probe and has been disabled automatically.", p.Slug))

	case healthy && !p.Enabled && p.AutoDisabled:
		// Only providers the probe itself disabled are recovered; a manually
		// disabled one is left alone. Otherwise an operator takes a provider
		// out of service and the probe puts it back five minutes later.
		// The WHERE auto_disabled clause in the SQL is the second line of
		// defence, for a manual disable that lands concurrently.
		if err := w.q.SetProviderAutoEnabled(ctx, p.ID); err != nil {
			slog.ErrorContext(ctx, "auto-enabling the recovered provider did not work", "provider", p.Slug, "error", err)
			return
		}
		slog.InfoContext(ctx, "probe recovered, provider auto-enabled", "provider", p.Slug)
		w.alert(ctx, "Provider auto-recovered", fmt.Sprintf("Provider %s passed its liveness probe again and has been enabled automatically.", p.Slug))
	}
}

func (w *ProbeWorker) alert(ctx context.Context, subject, detail string) {
	if w.alerter != nil {
		w.alerter.Alert(ctx, subject, detail)
	}
}

// ProbePeriodicJob builds the periodic probe job, registered at the assembly
// point.
func ProbePeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		ProbeInterval,
		func() (river.JobArgs, *river.InsertOpts) { return ProbeArgs{}, nil },
		// Probing on start would slow down a cold boot.
		&river.PeriodicJobOpts{RunOnStart: false},
	)
}
