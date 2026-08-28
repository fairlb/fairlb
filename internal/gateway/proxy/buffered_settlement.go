package proxy

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

type bufferedCompletion struct {
	prep      prepared
	in        Request
	requestID string
	upstream  upstreamResult
	rotation  rotationResult
	byok      byokChoice
	holdID    pgtype.UUID
	started   time.Time
}

// ReserveFor creates the billing boundary before any upstream attempt, with the
// lifetime that surface's requests need.
//
// There used to be a no-TTL Reserve beside it, and every caller but one used
// it -- which is how the image generations endpoint came to hold with the
// default lifetime while the edits endpoint next to it asked for the long one.
// The TTL is now picked from the surface (holdTTLFor), so a caller cannot
// forget it by calling the shorter-named function.
func (r *SettlementRecorder) ReserveFor(
	ctx context.Context, id Identity, requestID string, amountNano int64, ttl time.Duration,
) (pgtype.UUID, *Error) {
	in := billingHoldInput(id, requestID, amountNano)
	in.TTL = ttl
	holdID, err := r.pipeline.billing.Hold(ctx, in)
	if err != nil {
		return pgtype.UUID{}, mapHoldError(ctx, err)
	}
	return holdID, nil
}

// RecordExecutionFailure releases an undelivered request and writes its
// failure trail. It is shared by every buffered response path.
func (r *SettlementRecorder) RecordExecutionFailure(
	ctx context.Context, prep prepared, in Request, requestID string,
	rotation rotationResult, started time.Time,
) {
	r.VoidHold(ctx, prep.id.OrgID, requestID)
	r.pipeline.logFailure(
		ctx, prep.id, in, prep.modelSlug, rotation.route, rotation,
		requestID, rotation.err, started,
	)
}

// CompleteBuffered prices, settles and records a response after execution has
// succeeded. The response body is returned only after the durable accounting
// decision has been made.
func (r *SettlementRecorder) CompleteBuffered(
	ctx context.Context, completion bufferedCompletion,
) (Result, *Error) {
	p := r.pipeline
	prep, in := completion.prep, completion.in
	upstream, rotation := completion.upstream, completion.rotation
	route := rotation.route

	// A unit-billed request is charged for what the caller asked for, and that
	// amount was fixed and reserved during admission (ADR-0227). It never
	// reaches the token arithmetic below: the upstream's usage object, when it
	// sends one at all, describes tokens this model is not billed in, and
	// pricing them would charge a second, unrelated amount on top.
	if prep.unitBilled() {
		return r.completeBufferedUnits(ctx, completion, route)
	}

	usage := ParseUsage(in.Surface, upstream.body)
	estimated := !usage.Present
	if estimated {
		usage = Usage{
			In:  prep.inputTokens,
			Out: EstimateTokens(ResponseTextOf(in.Surface, upstream.body)),
			// Carried through the estimate rather than replaced by it: a tool
			// call was *observed*, not guessed, and it is priced per call
			// rather than per token. Dropping it here is what let a Responses
			// answer that produced images but reported no usage bill for the
			// prose alone.
			ToolCalls: usage.ToolCalls,
		}
	}
	holdIsBill := holdIsTheBill(in.Surface, estimated)

	price := prep.priceTable.ForBilling(prep.res.ModelPricing.IsFree())
	cost := prep.priceTable
	quote, pricingErr := p.quoteFor(
		price, cost, usage.BillingTokens(), upstream.byok,
		prep.pricing.ratesForRoute(route), prep.pricing.byokFeeBps,
	)
	if holdIsBill {
		quote = fallbackQuote(prep.estNano, prep.pricing.ratesForRoute(route))
		pricingErr = nil
	}
	if pricingErr != nil {
		slog.ErrorContext(ctx, "dataplane: billing failed",
			"error", pricingErr, "request_id", completion.requestID,
			"pricing_missing", errors.Is(pricingErr, catalog.ErrAdvancedPriceMissing))
		quote = fallbackQuote(prep.estNano, prep.pricing.ratesForRoute(route))
	}

	args := settleArgs{
		id: prep.id, requestID: completion.requestID, quote: quote, usage: usage,
		estimated: estimated, model: prep.res.Model, route: route, in: in,
		pricing: prep.pricing, priceTable: prep.priceTable,
		pricingFallback:  pricingErr != nil,
		httpStatus:       upstream.status,
		durationMs:       int32(time.Since(completion.started).Milliseconds()),
		attempts:         int32(upstream.attempts),
		byok:             upstream.byok,
		orgProviderKeyID: byokKeyOf(upstream),
		providerKeyID:    sharedKeyOf(upstream),
		holdID:           completion.holdID,
		routeID:          rotation.routeID(),
		trail:            rotation.trailJSON(),
	}
	// The duration comes off args rather than being measured again, so the
	// metric and the usage row can never disagree about how long this took.
	recordOutcome(ctx, string(in.Surface), statusOrOK(args.status), in.Stream,
		time.Duration(args.durationMs)*time.Millisecond)
	if errors.Is(pricingErr, catalog.ErrAdvancedPriceMissing) {
		args.pricingIssue = pricingErr.Error()
		r.RecordPricingMissing(
			ctx, completion.requestID, prep.id, prep.estNano,
			usageLogParams(args), pricingErr,
		)
	} else if err := r.SettleAndLog(ctx, args); err != nil {
		r.RecordUnsettled(
			ctx, completion.requestID, prep.id, args.quote,
			usageLogParams(args), err,
		)
	}

	body := AnnotateUsage(upstream.body, estimated, args.quote.ChargedNano, prep.id.WalletCurrency)
	return Result{Status: upstream.status, Body: body}, nil
}

// completeBufferedUnits settles a unit-billed synchronous request.
//
// Three things are known now that were not at admission: how many images came
// back, which route served it, and whether it ran on the organization's own
// credential. The first decides the quantity -- what was reserved is the
// route's declared ceiling, because how many images a request produces is not
// knowable before it is made. The third is what moves the customer's figure: a
// BYOK request owes the service fee instead of the list price, which is why
// this goes through unitCharge rather than calling ComputeUnits directly.
func (r *SettlementRecorder) completeBufferedUnits(
	ctx context.Context, completion bufferedCompletion, route catalog.Route,
) (Result, *Error) {
	p := r.pipeline
	prep, in, upstream := completion.prep, completion.in, completion.upstream
	units := settledUnits(ctx, prep, in, route, upstream.body)
	quote, gerr := p.quoteOrRefuse(ctx, prep, func() (catalog.Quote, error) {
		return p.unitCharge(prep, route, upstream.byok, units)
	})

	args := settleArgs{
		id: prep.id, requestID: completion.requestID, quote: quote,
		// No token counts, stated explicitly rather than left absent: NULL in
		// those columns means "the upstream did not report", and here the fact
		// is that there is nothing to report.
		usage: Usage{Present: true},
		units: units,
		model: prep.res.Model, route: route, in: in,
		pricing: prep.pricing, priceTable: prep.priceTable,
		httpStatus:       upstream.status,
		durationMs:       int32(time.Since(completion.started).Milliseconds()),
		attempts:         int32(upstream.attempts),
		byok:             upstream.byok,
		orgProviderKeyID: byokKeyOf(upstream),
		providerKeyID:    sharedKeyOf(upstream),
		holdID:           completion.holdID,
		routeID:          completion.rotation.routeID(),
		trail:            completion.rotation.trailJSON(),
	}
	recordOutcome(ctx, string(in.Surface), statusOrOK(args.status), in.Stream,
		time.Duration(args.durationMs)*time.Millisecond)
	if gerr != nil {
		// The rate card priced this vector at admission, so a failure now means
		// the price moved underneath a request in flight. The hold stays
		// standing for the operator's repair queue rather than being voided:
		// the upstream produced the output, and somebody has to decide what it
		// cost.
		r.RecordUnsettled(ctx, completion.requestID, prep.id, catalog.Quote{},
			usageLogParams(args), errors.New("pricing a unit-billed request failed after it was served"))
		return Result{}, gerr
	}
	if err := r.SettleAndLog(ctx, args); err != nil {
		r.RecordUnsettled(ctx, completion.requestID, prep.id, args.quote, usageLogParams(args), err)
	}

	body := AnnotateUsage(upstream.body, false, args.quote.ChargedNano, prep.id.WalletCurrency)
	return Result{Status: upstream.status, Body: body}, nil
}
