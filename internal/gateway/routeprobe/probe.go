// Package routeprobe finds out what a route really serves, and keeps the
// answer in model_route_probes -- the only record of a route's capability.
//
// A route declares nothing about which endpoints it serves. Every endpoint of
// a protocol its provider speaks is a question, and the probe answers it: the
// data plane tries any endpoint not found unsupported, and the catalog
// publishes any endpoint found working. The package exists on its own because
// the same probe is reached from five places that must not disagree: the
// operator's "test connectivity" button, the asynchronous worker that runs
// after a route is created, the re-probe that follows an upstream-model
// rename, the re-probe a live 404 asks for, and the sweeper that re-checks
// aged verdicts. Two implementations of "does this route work" is two greens
// that can contradict each other on one screen.
//
// Running it asynchronously is a requirement, not an optimization: one probe
// takes up to Timeout, and a single "discover, then adopt in bulk" pass is
// dozens of endpoints. Done synchronously, one click on save would hang for
// minutes, and any single timeout along the way would turn the whole save into
// a failure.
package routeprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/upstreamprobe"
)

// Timeout bounds one probe. Shared with the operator-facing connectivity test,
// so a verdict means the same thing wherever it came from.
const Timeout = 20 * time.Second

// Target is the provider a probe is sent to.
type Target struct {
	BaseURL string
	// Vendor is which platform this provider is. It is inert for every dialect
	// endpoint, and load-bearing for exactly one: the video plane's request
	// shape comes from the vendor's mapper, so a probe that did not know the
	// vendor could not build one (ADR-0219).
	Vendor string
	// Protocols is carried so the callee can check the provider really declares
	// the dialect the endpoint implies; which dialect to speak is derived from
	// the endpoint itself.
	Protocols []string
	// Headers is the provider's extra header map, as stored.
	Headers json.RawMessage
	// Transport is the provider's transport profile, as stored. Applied here too, because
	// without it the probe would build a different request from the one the
	// data plane builds and then report its own failure as the provider's -- a
	// diagnostic that disagrees with the thing it is diagnosing is worse than
	// no diagnostic.
	Transport []byte
}

// Result is one live probe's outcome. It is not what gets stored: Verdict is.
type Result struct {
	CheckedAt  time.Time
	KeyID      uuid.UUID
	OK         bool
	LatencyMs  *int
	StatusCode *int
	Message    string
	Trace      *upstreamprobe.Trace
}

// Verdict is a stored probe result for one endpoint of one route.
type Verdict struct {
	Endpoint string
	// ProbeMode says whether the worker probes this endpoint on its own or
	// only when asked; the page tells the operator which is which from this,
	// not from the endpoint's name.
	ProbeMode string
	Status    string
	// Source says who wrote the verdict: the probe worker, or the operator
	// overriding it.
	Source    string
	CheckedAt time.Time
	// EnqueuedAt is set while a probe of this endpoint is in flight and cleared
	// the moment a verdict lands. Separate from Status because Status is the
	// verdict: a re-probe leaves the standing one in place, and it has to --
	// the catalogue publishes what was found `ok`, so a pending value living in
	// Status would take the route out of the catalogue for the length of the
	// re-probe (ADR-0224).
	EnqueuedAt time.Time
	LatencyMs  *int
	StatusCode *int
	Error      string
}

// probeable reports whether an endpoint can be probed at all: every mode but
// the derived one, which has nothing to send.
func probeable(endpoint string) bool {
	mode, ok := catalog.ProbeModeForEndpoint(endpoint)
	return ok && mode != catalog.ProbeDerived
}

// autoProbeable reports whether an endpoint is probed without anyone asking.
//
// Image endpoints are not: one image probe costs one to two orders of
// magnitude more than a text one, and the UI already promises the operator that
// this runs once per click and never on its own, because it spends real money.
// Probing images automatically would overturn a promise already made, and would
// make the cost of a bulk adoption unbounded. Those endpoints stay unverified
// -- and, being unobservable, are not candidates until a verdict says `ok`.
func autoProbeable(endpoint string) bool {
	mode, ok := catalog.ProbeModeForEndpoint(endpoint)
	return ok && mode == catalog.ProbeAuto
}

// Probeable keeps the endpoints that can be probed at all.
func Probeable(all []string) []string {
	return slices.DeleteFunc(slices.Clone(all), func(ep string) bool { return !probeable(ep) })
}

// AutoProbeable keeps the endpoints eligible for automatic probing.
func AutoProbeable(all []string) []string {
	return slices.DeleteFunc(slices.Clone(all), func(ep string) bool { return !autoProbeable(ep) })
}

// Run sends one minimal request and reports what came back.
//
// wantTrace is the caller's decision. The operator-facing probe gates it on the
// deployment; the asynchronous worker always passes false -- it has nobody to
// show a trace to, so producing one would only put the plaintext credential in
// one more place in memory.
func Run(
	ctx context.Context, hc *http.Client, target Target,
	apiKey, upstreamModel, endpoint string, keyID uuid.UUID, wantTrace bool,
) Result {
	out := Result{CheckedAt: time.Now().UTC(), KeyID: keyID}

	spec, ok := upstreamprobe.SpecForEndpoint(endpoint, upstreamModel, target.Vendor)
	if !ok {
		out.Message = "Unknown probe endpoint: " + endpoint
		return out
	}
	var headers map[string]string
	if err := json.Unmarshal(target.Headers, &headers); err != nil {
		headers = nil
	}
	transport, err := catalog.ParseTransport(target.Transport)
	if err != nil {
		out.Message = "The provider's transport profile could not be read: " + err.Error()
		return out
	}
	res := upstreamprobe.Run(ctx, upstreamprobe.Input{
		Client: hc, Spec: spec, BaseURL: target.BaseURL, APIKey: apiKey,
		Model: upstreamModel, Headers: headers, Transport: transport,
		Timeout: Timeout, CaptureTrace: wantTrace,
	})
	out.CheckedAt, out.OK = res.CheckedAt, res.OK
	out.LatencyMs, out.StatusCode, out.Message = res.LatencyMs, res.StatusCode, res.Message
	out.Trace = res.Trace
	return out
}

func intPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func groupVerdicts(rows []gwdb.ListRouteProbesRow) map[uuid.UUID][]Verdict {
	out := map[uuid.UUID][]Verdict{}
	for _, r := range rows {
		id := uuid.UUID(r.RouteID.Bytes)
		v := Verdict{
			Endpoint: r.Endpoint, ProbeMode: r.ProbeMode, Status: r.Status, Source: r.Source,
			LatencyMs: intPtr(r.LatencyMs), StatusCode: intPtr(r.StatusCode),
			Error: r.Error,
		}
		if r.CheckedAt.Valid {
			v.CheckedAt = r.CheckedAt.Time.UTC()
		}
		if r.ProbeEnqueuedAt.Valid {
			v.EnqueuedAt = r.ProbeEnqueuedAt.Time.UTC()
		}
		out[id] = append(out[id], v)
	}
	return out
}

// VerdictsForModel reads the stored verdicts for every route under one model.
//
// A read failure degrades to "no verdicts" rather than taking the list down:
// probe results are supplementary display, and a page that refuses to render
// because it could not decorate itself is worse than one that renders plain.
func (s *Service) VerdictsForModel(ctx context.Context, modelID pgtype.UUID) map[uuid.UUID][]Verdict {
	rows, err := s.q.ListRouteProbes(ctx, modelID)
	if err != nil {
		slog.WarnContext(ctx, "failed to read the probe verdicts, showing them as unverified", "error", err)
		return nil
	}
	return groupVerdicts(rows)
}

// Verdict reads one stored row. A row that does not exist is a real answer
// here -- the operator asked about an endpoint nothing has been recorded for
// -- and comes back as unverified rather than as an error.
func (s *Service) Verdict(ctx context.Context, routeID pgtype.UUID, endpoint string) (Verdict, error) {
	r, err := s.q.GetRouteProbe(ctx, gwdb.GetRouteProbeParams{RouteID: routeID, Endpoint: endpoint})
	if err != nil {
		if db.IsNoRows(err) {
			mode, _ := catalog.ProbeModeForEndpoint(endpoint)
			return Verdict{Endpoint: endpoint, ProbeMode: string(mode), Status: StatusUnverified, Source: SourceProbe}, nil
		}
		return Verdict{}, fmt.Errorf("routeprobe: read the verdict: %w", err)
	}
	return groupVerdicts([]gwdb.ListRouteProbesRow{gwdb.ListRouteProbesRow(r)})[uuid.UUID(routeID.Bytes)][0], nil
}

// VerdictsForProvider is the other axis, degrading the same way.
func (s *Service) VerdictsForProvider(ctx context.Context, providerID pgtype.UUID) map[uuid.UUID][]Verdict {
	rows, err := s.q.ListRouteProbesForProvider(ctx, providerID)
	if err != nil {
		slog.WarnContext(ctx, "failed to read the probe verdicts by provider, showing them as unverified", "error", err)
		return nil
	}
	// The two queries select identical columns, so one row type converts
	// straight into the other: that makes "somebody changed the columns on one
	// side only" a compile error instead of letting two copies of the grouping
	// logic drift apart.
	out := make([]gwdb.ListRouteProbesRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, gwdb.ListRouteProbesRow(r))
	}
	return groupVerdicts(out)
}
