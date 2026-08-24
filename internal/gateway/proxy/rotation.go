package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Candidate rotation, shared by the streaming and non-streaming paths.
//
// The two paths differ in what one attempt *is* -- the non-streaming one reads
// a whole response into memory and then judges it, while the streaming one
// hands the body to the pump and judges the first frame -- but they agree on
// everything around it: the order candidates are tried in, how many are
// allowed, when a failure is worth another try, which circuit to advance, and
// what to record about the ones that failed.
//
// They used to agree on none of that, because only the non-streaming path had a
// loop. The streaming path was a copy with the loop taken out, and the
// difference had grown into a behaviour: a streamed request whose upstream
// refused the connection failed outright, while the same failure on the same
// route succeeded for a non-streamed one. Sharing the surrounding machinery is
// what keeps the two from drifting again.

// attemptOutcome is what one attempt reports back to the rotation.
type attemptOutcome struct {
	// cls classifies the failure. A zero Class with a nil err means success.
	cls Classification
	// err is the client-facing error for this attempt.
	err *Error
	// keyID is the credential this attempt used, recorded whether or not it
	// worked: an investigation into a rejecting upstream starts here.
	keyID pgtype.UUID
	// byok records whether this attempt used the organization's own credential.
	byok bool
	// committed means this attempt has already sent bytes to the client, so
	// rotation must stop no matter how it ended. Only the streaming path can
	// set it.
	committed bool
	// budgetSpent means the attempt failed by running out of time rather than
	// by failing fast. Rotating after one buys the next candidate nothing,
	// because the budget it would run under is the same one that just expired.
	budgetSpent bool
}

// failedHop is one failed attempt, as recorded on the request's usage row.
type failedHop struct {
	RouteID       string `json:"route_id,omitempty"`
	ProviderID    string `json:"provider_id,omitempty"`
	ProviderKeyID string `json:"provider_key_id,omitempty"`
	Class         string `json:"class"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	LatencyMs     int64  `json:"latency_ms"`
	Priority      int32  `json:"priority"`
	Weight        int32  `json:"weight"`
}

// rotationResult is what the rotation reports to its caller.
type rotationResult struct {
	route    catalog.Route
	keyID    pgtype.UUID
	byok     bool
	attempts int
	// trail is the hops that failed before the one in route, oldest first. The
	// successful hop is deliberately absent: it is already named by route.
	trail []failedHop
	err   *Error
}

// trailJSON serialises the trail for the usage row. An empty trail serialises
// as an empty array rather than null, so a reader never has to distinguish
// "no failures" from "not recorded".
func (r rotationResult) trailJSON() []byte {
	if len(r.trail) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(r.trail)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// routeID gives the route's id for the usage row, empty when no candidate was
// ever reached.
func (r rotationResult) routeID() pgtype.UUID { return r.route.ID }

// rotate walks the candidates, calling attempt for each one until it succeeds
// or the rotation runs out of reasons to continue.
//
// Four things stop it, and they are different from one another:
//
//   - the attempt succeeded;
//   - the failure is not retryable, which for a client-class error means the
//     next candidate would reject it identically;
//   - bytes have already reached the client, after which there is nothing to
//     fail over to;
//   - the time budget is gone, as opposed to a candidate having failed quickly.
//
// The last is the one worth stating aloud. Rotating on a timeout looks like
// resilience and is not: the first-byte budget is shared across attempts by
// design, so a candidate that consumed it leaves the next one with nothing.
// Giving each attempt its own full budget instead is what pushes a slow request
// past the point where the proxies in front of the gateway give up, and an
// error from an intermediary tells the caller nothing about which upstream was
// slow.
func (p *Pipeline) rotate(
	ctx context.Context, candidates []catalog.Route, surface catalog.Surface, estimatedTokens int64,
	attempt func(ctx context.Context, route catalog.Route, n int) attemptOutcome,
) rotationResult {
	p.budget.StartRequest()
	ordered := p.strategy.Order(ctx, p.availableRoutes(ctx, candidates))
	if len(ordered) == 0 {
		return rotationResult{
			err: NewError(errcode.GatewayAllProvidersFailed, "No provider available: every circuit is open"),
		}
	}

	var res rotationResult
	for _, route := range ordered {
		if res.attempts >= maxRouteAttempts {
			break
		}
		if res.attempts > 0 && !p.budget.AllowRetry() {
			slog.WarnContext(ctx, "dataplane: global retry budget exhausted; stopping candidate rotation")
			break
		}
		res.route, res.keyID = route, pgtype.UUID{}

		// Two capacity filters, both of which skip rather than queue, and
		// neither of which counts as an attempt.
		//
		// Not counting is the whole point: nothing was sent upstream, so this
		// candidate has not tested anything and must not consume one of the
		// three tries or a unit of the global retry budget. Counting it would
		// make a single busy provider spend a request's whole failover
		// allowance without a single upstream call having been made.
		//
		// First, the declared allowance of the upstream account: a provider
		// with nothing left this minute is out of the running exactly as a
		// cooling-down one is.
		if ok, retryAfter := p.capacityAllows(ctx, route, estimatedTokens); !ok {
			if retryAfter <= 0 {
				retryAfter = time.Second
			}
			res.err = &Error{
				Code: errcode.GatewaySaturated, Message: "Upstream capacity saturated",
				RetryAfter: retryAfter,
			}
			continue
		}

		// Then backpressure: skip a provider that is already at its concurrency
		// cap rather than queueing behind it (see the Semaphore comment).
		// Streams hold their slot only until the first byte -- holding it for
		// the whole stream would turn a backpressure valve into a cap on how
		// many streams a provider may serve at once, which is a different limit
		// that nobody asked for.
		sem := p.semaphoreFor(route.ProviderID, route.Capacity.MaxConcurrency)
		if !sem.TryAcquire() {
			res.err = &Error{
				Code: errcode.GatewaySaturated, Message: "Upstream capacity saturated",
				RetryAfter: time.Second,
			}
			continue
		}
		res.attempts++
		started := time.Now()
		out := attempt(ctx, route, res.attempts)
		sem.Release()

		res.keyID, res.byok = out.keyID, out.byok
		if out.err == nil {
			// Clear any error left by an earlier candidate. Failing over is a
			// success: the request was served, and leaving the first
			// candidate's error in place would report it as the request's
			// outcome -- turning working failover into a failed request.
			res.err = nil
			return res
		}
		res.err = out.err
		p.applyBreaker(ctx, route, surface, out.cls, out.byok)

		if out.committed {
			// Bytes are out. Whatever went wrong, the client has part of a
			// response that a second attempt would not reproduce.
			return res
		}
		res.trail = append(res.trail, failedHop{
			RouteID:       uuidStr(route.ID),
			ProviderID:    uuidStr(route.ProviderID),
			ProviderKeyID: uuidStr(out.keyID),
			Class:         out.cls.Class.String(),
			HTTPStatus:    out.cls.upstreamStatus,
			ErrorCode:     out.err.Code,
			LatencyMs:     time.Since(started).Milliseconds(),
			Priority:      route.Priority,
			Weight:        route.Weight,
		})
		recordAttempt(ctx, route.ProviderSlug, out.cls)

		if !out.cls.Retryable() || out.budgetSpent {
			return res
		}
	}
	if res.err == nil {
		res.err = NewError(errcode.GatewayAllProvidersFailed, "Every candidate failed")
	}
	return res
}
