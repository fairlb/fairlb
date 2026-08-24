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

// Reserve creates the billing boundary before any upstream attempt.
func (r *SettlementRecorder) Reserve(
	ctx context.Context, id Identity, requestID string, amountNano int64,
) (pgtype.UUID, *Error) {
	return r.ReserveFor(ctx, id, requestID, amountNano, 0)
}

// ReserveFor is Reserve with an explicit TTL for long-running request types.
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

	usage := ParseUsage(in.Surface, upstream.body)
	estimated := !usage.Present
	if estimated {
		usage = Usage{
			In:  prep.inputTokens,
			Out: EstimateTokens(ResponseTextOf(in.Surface, upstream.body)),
		}
	}
	holdIsTheBill := estimated && in.Surface == catalog.SurfaceImages

	price := prep.priceTable.ForBilling(prep.res.ModelPricing.IsFree())
	cost := prep.priceTable
	quote, pricingErr := p.quoteFor(
		price, cost, usage.BillingTokens(), upstream.byok,
		prep.pricing.ratesForRoute(route), prep.pricing.byokFeeBps,
	)
	if holdIsTheBill {
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
