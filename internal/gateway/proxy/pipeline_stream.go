package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The streaming pipeline. It shares the first half with the non-streaming one
// -- authentication, gates, hold, parsing -- and diverges at forwarding: the
// response is sent as it arrives, and the billing boundary moves with the first
// byte.

// RunStream executes a streaming request. Unlike Run it writes the response to
// w directly, because streaming has no step where a complete result is in hand
// before deciding how to write it.
//
// The returned *Error is meaningful only *before* the first byte goes out.
// After that a failure can only be conveyed inside the stream, and this returns
// nil having settled internally.
func (p *Pipeline) RunStream(ctx context.Context, w http.ResponseWriter, in Request, surface Surface) *Error {
	// This entry point *is* the streaming path, so the field states that rather
	// than trusting the caller to have set it. The handler already does, but a
	// direct caller that did not would otherwise write stream=false on a
	// streamed request's usage row and mislabel its metric.
	in.Stream = true
	started := time.Now()
	requestID := in.requestID()

	prep, gerr := p.admission.Prepare(ctx, in)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return gerr
	}
	prep, in, gerr = p.pinAffinity(ctx, prep, in)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return gerr
	}

	holdID, gerr := p.settlementRecorder.ReserveFor(ctx, prep.id, requestID, prep.estNano, holdTTLFor(in.Surface))
	if gerr != nil {
		p.settlementRecorder.RecordHoldRejection(ctx, in, gerr, started)
		return gerr
	}

	// Streaming rotates candidates too, but only up to the moment the first
	// byte is written. That moment is not an estimate: Pump withholds the
	// header until it has a complete frame to send, so "has anything reached
	// the client" is a fact this loop can read.
	//
	// The whole first-byte budget is shared across the attempts, which is what
	// keeps a rotating request from taking three times as long as a single one
	// and outliving the patience of whatever sits in front of the gateway. The
	// consequence is worth stating: a candidate that *times out* has spent the
	// budget for the request, not just for itself, so rotation stops there.
	// What rotation buys is recovery from candidates that fail fast --
	// refused connections, prompt 5xx, a rejected credential.
	byok := prep.byok
	streamStart := time.Now()

	var (
		outcome StreamOutcome
		// Which credential the *hop that produced the stream* used. Recorded per
		// hop rather than per request: rotation can move between vendors, and
		// only one of them served.
		byokKeyID pgtype.UUID
		keyID     pgtype.UUID
	)
	rot := p.executor.Rotate(ctx, prep.res.Routes, in.Surface, prep.inputTokens, func(ctx context.Context, route catalog.Route, _ int) attemptOutcome {
		var hopKeyID, hopBYOKKeyID pgtype.UUID
		var apiKey, baseURL string
		var gerr *Error
		if in.PinnedProviderKeyID.Valid || in.PinnedOrgProviderKeyID.Valid {
			hopKeyID, apiKey, baseURL, hopBYOKKeyID, gerr = p.pinnedCredentialFor(ctx, route, prep.id.OrgID, in)
		} else {
			hopKeyID, apiKey, baseURL, hopBYOKKeyID, gerr = p.credentialFor(ctx, route, byok)
		}
		if gerr != nil {
			if gerr.Code == errcode.GatewayStateRouteUnavailable {
				return attemptOutcome{cls: Classification{Class: ClassTerminal}, err: gerr}
			}
			return attemptOutcome{cls: Classification{Class: ClassProvider, CountsTowardHealth: true}, err: gerr}
		}
		keyID, byokKeyID = hopKeyID, hopBYOKKeyID

		body, err := RewriteRequest(in.Surface, in.Body, route.ProviderModelID, true, route.Transport)
		if err != nil {
			return attemptOutcome{
				cls: Classification{Class: ClassClient},
				err: NewError(errcode.GatewayInvalidRequest, err.Error()),
			}
		}
		req, err := BuildRequest(ctx, Target{
			Protocol: in.Protocol, BaseURL: baseURL, APIKey: apiKey,
			Path: in.UpstreamPath, Stream: true,
			Headers:   MergeHeaders(route.ProviderHeaders, route.RouteHeaders),
			Transport: route.Transport, UpstreamModel: route.ProviderModelID,
			ExtraQuery: in.UpstreamQuery, Method: in.Method, Resource: in.Resource,
		}, body)
		if err != nil {
			return attemptOutcome{
				cls: Classification{Class: ClassProvider, CountsTowardHealth: true},
				err: NewError(errcode.GatewayInternal, err.Error()),
			}
		}

		// The remaining budget, not a fresh one. Two attempts each waiting a
		// full minute is the failure this arithmetic exists to prevent. The
		// budget itself is per surface: see firstByteBudgetFor.
		remaining := firstByteBudgetFor(in.Surface) - time.Since(streamStart)
		if remaining <= 0 {
			return attemptOutcome{
				cls:         Classification{Class: ClassProvider, CountsTowardHealth: true},
				err:         NewError(errcode.GatewayUpstreamTimeout, "Upstream timed out"),
				keyID:       hopKeyID,
				byok:        hopBYOKKeyID.Valid,
				budgetSpent: true,
			}
		}
		// A deadline on this context would also bound the long body read that
		// follows a successful first byte, so the bound is a cancel that is
		// stopped the moment the headers arrive. The cause is what lets the
		// classifier tell this cancellation from the client's.
		attemptCtx, cancel := context.WithCancelCause(ctx)
		timer := time.AfterFunc(remaining, func() { cancel(errFirstByteBudget) })
		req = req.WithContext(attemptCtx)

		resp, err := p.streamClient().Do(req)
		if err != nil {
			timer.Stop()
			cancel(nil)
			cls := ClassifyTransportError(attemptCtx, err)
			cls.keyID = hopKeyID
			gerr := cls.Err
			if gerr == nil { // the client hung up
				gerr = NewError(errcode.GatewayInternal, "Request was cancelled")
			}
			return attemptOutcome{
				cls: cls, err: gerr, keyID: hopKeyID, byok: hopBYOKKeyID.Valid,
				budgetSpent: errors.Is(context.Cause(attemptCtx), errFirstByteBudget),
			}
		}

		if resp.StatusCode >= 400 {
			timer.Stop()
			cancel(nil)
			// The organization's credential was rejected upstream: mark it invalid
			// and notify. Whether this request then falls back to a shared
			// credential is the organization's own setting, honoured here exactly as
			// the non-streaming path honours it -- a switch that works on one
			// kind of request and not the other is worse than no switch.
			if hopBYOKKeyID.Valid && byokRejected(resp.StatusCode) {
				p.markBYOKInvalid(ctx, prep.id.OrgID, hopBYOKKeyID, resp.StatusCode)
				// Only this vendor's credential is dropped, and only when the
				// organization allowed the fallback: their credentials at other
				// platforms are separate accounts that this rejection says
				// nothing about.
				if choice, ok := byok.forVendor(route.ProviderVendor); ok && choice.Fallback {
					delete(byok, route.ProviderVendor)
				}
			}
			upstreamBody := readCapped(resp.Body)
			_ = resp.Body.Close()
			cls := classifyUpstreamStatus(in, resp.StatusCode, upstreamBody, resp.Header.Get("Retry-After"))
			cls.keyID = hopKeyID
			if hopBYOKKeyID.Valid {
				// Same rule as the non-streaming path: on a organization credential no
				// upstream failure is a health signal for this deployment. That
				// hop used the organization's credential, their quota and possibly
				// their own base URL, so a 500 from it says nothing about this
				// deployment's link to the provider -- and counting it would let
				// one organization's self-hosted gateway open the circuit for everyone.
				// keyID is cleared for the same reason: a 429 is key-class, and
				// recording it against a shared credential that never served the
				// request attributes it to the wrong row.
				cls.CountsTowardHealth = false
				cls.keyID = pgtype.UUID{}
			}
			return attemptOutcome{cls: cls, err: cls.Err, keyID: hopKeyID, byok: hopBYOKKeyID.Valid}
		}

		out, perr := NewStreamer().
			WithFirstByteBudget(firstByteBudgetFor(in.Surface)-time.Since(streamStart)).
			Pump(attemptCtx, w, upstreamStreamBody(route.Transport, resp.Body), in.Surface)
		timer.Stop()
		cancel(nil)
		// Closing per attempt is not optional. Pump's reader goroutine blocks
		// in a read that cancelling the context does not interrupt; only
		// closing the body releases it. A rotation that skipped this would leak
		// a goroutine and a connection for every candidate it moved past.
		_ = resp.Body.Close()

		if out.FirstByteSent && !out.Interrupted && !out.Canceled {
			p.breaker.RecordSuccess(ctx, route.ProviderID, hopKeyID)
			outcome = out
			return attemptOutcome{keyID: hopKeyID, byok: hopBYOKKeyID.Valid, committed: true}
		}
		if out.Interrupted || out.Canceled {
			// Bytes are out. Whatever happened next, this request is settled
			// against what was produced and cannot be retried.
			if out.Interrupted {
				p.breaker.RecordProviderFailure(ctx, route.ProviderID, "stream interrupted")
			}
			outcome = out
			return attemptOutcome{
				cls:       Classification{Class: ClassTerminal},
				err:       NewError(errcode.GatewayUpstreamTimeout, "The upstream stream was interrupted"),
				keyID:     hopKeyID,
				byok:      hopBYOKKeyID.Valid,
				committed: true,
			}
		}
		// Nothing reached the client. Another candidate may still serve this
		// request, unless what went wrong was the clock.
		if perr != nil {
			slog.ErrorContext(ctx, "dataplane: streaming failed before the first byte",
				"error", perr, "request_id", requestID, "provider", route.ProviderSlug)
		}
		return attemptOutcome{
			cls:         Classification{Class: ClassProvider, CountsTowardHealth: true, keyID: hopKeyID},
			err:         NewError(errcode.GatewayUpstreamTimeout, "Upstream returned nothing"),
			keyID:       hopKeyID,
			byok:        hopBYOKKeyID.Valid,
			budgetSpent: out.TimedOut,
		}
	})

	route := rot.route
	if !outcome.FirstByteSent {
		// Nothing was ever sent, by any candidate: void and charge nothing,
		// exactly as the non-streaming path does.
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		gerr := rot.err
		if gerr == nil {
			gerr = NewError(errcode.GatewayUpstreamTimeout, "Upstream returned nothing")
		}
		p.logFailure(ctx, prep.id, in, prep.modelSlug, route, rot, requestID, gerr, started)
		return gerr
	}

	// The first byte is out, so part of the service really was delivered and
	// this settles against what was produced. The client may already be gone,
	// so settlement must detach from the request context: using it would let a
	// cancellation kill the settlement transaction too, leaving the service
	// delivered and unpaid.
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleAfterStreamTimeout)
	defer cancel()
	if outcome.ResourceID != "" {
		p.rememberAffinityID(settleCtx, prep, in, outcome.ResourceID, upstreamResult{
			keyID: keyID, byok: byokKeyID.Valid, byokKeyID: byokKeyID,
		}, rot)
	}

	usage := outcome.Usage
	estimated := !usage.Present
	if estimated {
		usage = Usage{
			In:  prep.inputTokens,
			Out: EstimateTokens(outcome.Text),
			// Observed, not guessed, and priced per call -- see the same line
			// on the buffered path.
			ToolCalls: usage.ToolCalls,
		}
	}
	// Computed here and consumed in the token branch below. The unit-billed
	// branch never estimates, so it is not a condition there.
	holdIsBill := holdIsTheBill(in.Surface, estimated)
	// No unit-billed request reaches this path any more, and this branch is
	// what keeps that true rather than what handles it.
	//
	// Video never streams, and admission refuses `stream` on a per-image model
	// because no count both vendors agree on can be read out of a stream. If
	// that refusal is ever lifted, the failure has to be *this* -- a hold left
	// standing in front of an operator -- rather than the token arithmetic
	// below, which would settle a per-image model against the four explicit
	// zeros its price row stores and charge nothing for the whole generation.
	var quote catalog.Quote
	var cErr error
	if prep.unitBilled() {
		usage, estimated = unitUsage(), false
		cErr = errors.New("a unit-billed request reached the streaming path, " +
			"where the number of units it produced cannot be counted")
		slog.ErrorContext(settleCtx, "dataplane: unit-billed request reached the streaming path",
			"request_id", requestID, "model", prep.modelSlug, "surface", string(in.Surface))
		p.alertUnpriced(settleCtx, prep.modelSlug)
	} else if holdIsBill {
		// The upstream reported nothing and an image stream leaves nothing to
		// estimate from, so the hold is the bill (see holdIsTheBill). The usage
		// row still records the input side; only the charge follows the hold.
		quote = fallbackQuote(prep.estNano, prep.pricing.ratesForRoute(route))
	} else {
		quote, cErr = p.quoteFor(
			prep.priceTable.ForBilling(prep.res.ModelPricing.IsFree()),
			prep.priceTable,
			usage.BillingTokens(),
			byokKeyID.Valid, prep.pricing.ratesForRoute(route), prep.pricing.byokFeeBps,
		)
		if cErr != nil {
			slog.ErrorContext(settleCtx, "dataplane: streaming billing failed; settling at the held amount", "error", cErr, "request_id", requestID)
			quote = fallbackQuote(prep.estNano, prep.pricing.ratesForRoute(route))
		}
	}

	args := settleArgs{
		id: prep.id, requestID: requestID, quote: quote, usage: usage,
		units:     prep.units,
		estimated: estimated, model: prep.res.Model, route: route, in: in,
		pricing: prep.pricing, priceTable: prep.priceTable,
		pricingFallback: cErr != nil,
		httpStatus:      http.StatusOK, durationMs: int32(time.Since(started).Milliseconds()),
		// `stream` left the struct on main -- it is read from in.Stream now, at
		// the one boundary that also chooses the entry point. The credential
		// fields are this branch's: the hop that succeeded owns them, because
		// with credentials keyed by vendor there is no single request-level
		// "the organization's credential" to read.
		status: streamStatus(outcome), ttfbMs: int32(outcome.TTFB.Milliseconds()),
		byok: byokKeyID.Valid, orgProviderKeyID: byokKeyID,
		providerKeyID: sharedKeyIfUsed(byokKeyID.Valid, keyID), holdID: holdID,
		routeID: rot.routeID(), trail: rot.trailJSON(),
		attempts: int32(rot.attempts),
	}
	recordOutcome(settleCtx, string(in.Surface), statusOrOK(args.status), in.Stream,
		time.Duration(args.durationMs)*time.Millisecond)
	recordStreamTTFB(settleCtx, string(in.Surface), outcome.TTFB)
	pricingErr := cErr
	switch {
	case prep.unitBilled() && pricingErr != nil:
		// Nothing this side can price: the amount stays unsettled and the hold
		// stays held, which is what puts it in front of an operator instead of
		// charging a number no rate card produced.
		args.quote = catalog.Quote{}
		p.settlementRecorder.RecordUnsettled(settleCtx, requestID, prep.id, args.quote, usageLogParams(args), pricingErr)
	case errors.Is(pricingErr, catalog.ErrAdvancedPriceMissing):
		args.pricingIssue = pricingErr.Error()
		p.settlementRecorder.RecordPricingMissing(settleCtx, requestID, prep.id, prep.estNano, usageLogParams(args), pricingErr)
	default:
		if err := p.settlementRecorder.SettleAndLog(settleCtx, args); err != nil {
			// Streaming needs the fallback ledger most of all: on this path the
			// client is usually already gone, so when something goes wrong there is
			// nobody left who could notice.
			p.settlementRecorder.RecordUnsettled(settleCtx, requestID, prep.id, args.quote, usageLogParams(args), err)
		}
	}

	// A broken stream: the HTTP status has long been 200, so the error can only
	// be conveyed inside the stream.
	if outcome.Interrupted {
		if _, werr := w.Write(StreamErrorEvent(surface, "The upstream stream was interrupted")); werr == nil {
			_ = http.NewResponseController(w).Flush()
		}
	}
	return nil
}

// settleAfterStreamTimeout budgets settlement after the stream ends, detached
// from the possibly cancelled request context.
const settleAfterStreamTimeout = 30 * time.Second

// streamStatus maps how the stream ended onto the usage row's status column.
func streamStatus(o StreamOutcome) string {
	switch {
	case o.Canceled:
		return "canceled" // client hung up; what was produced is still billed
	case o.Interrupted:
		return "upstream_error"
	default:
		return "ok"
	}
}

// streamClient returns the streaming-only client.
func (p *Pipeline) streamClient() *http.Client { return p.streamHTTP }

// newStreamClient builds the streaming-only client: *no* overall timeout (long
// reasoning is legitimate, and Client.Timeout counts reading the body too, so
// it would cut a long stream in half), but a first-byte deadline on the
// *response headers*.
//
// The deadline belongs here rather than anywhere else because this is the
// stretch that is exposed to an intermediary's no-response timeout: while the
// headers are awaited the client has received nothing at all, so to a proxy in
// front of us the origin looks silent. The wait inside Pump happens after the
// 200 has gone out, where that timeout no longer applies.
//
// Built *once*: the Transport holds the connection pool, and cloning it per
// request would destroy connection reuse entirely.
func newStreamClient(base http.RoundTripper) *http.Client {
	if base == nil {
		base = http.DefaultTransport
	}
	if t, ok := base.(*http.Transport); ok {
		// Cloning is required -- mutating in place would poison the
		// process-wide http.DefaultTransport.
		cloned := t.Clone()
		// The widest surface's budget, because this client is built once and
		// shared by all of them. It is a backstop, not the bound: every attempt
		// arms a context cancel at its own surface's remaining budget before
		// Do, and that cancel bounds the header wait too. So a chat stream
		// still gives up after the minute this constant used to name.
		cloned.ResponseHeaderTimeout = imageTimeout
		base = cloned
	}
	// A custom RoundTripper, which is what tests inject, is left alone: it has
	// no response-header timeout to set, and the first-byte bound then rests
	// entirely on the Streamer half.
	return &http.Client{Transport: base}
}

// readCapped reads an upstream error body, bounded so a huge response cannot
// exhaust memory.
func readCapped(r interface{ Read([]byte) (int, error) }) []byte {
	const maxErrBody = 64 << 10
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for len(buf) < maxErrBody {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}
