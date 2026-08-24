package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The image endpoints. Generations take JSON and have the same shape as the
// non-streaming pipeline; edits are multipart and need the model read out
// before the body is streamed through.
//
// Image work is far slower than text completion, so a longer timeout than the
// non-streaming budget is needed or the gateway cuts off perfectly normal
// requests itself. Both image endpoints get it: edits reach it directly, and
// generations through clientFor, which selects by surface rather than by which
// function happens to be running.

// imageTimeout is the overall timeout for the image endpoints.
const imageTimeout = 300 * time.Second

// imageClient returns the image-only client: a longer timeout, on the same
// Transport so the connection pool is shared.
func (p *Pipeline) imageClient() *http.Client {
	return &http.Client{Transport: p.client.Transport, Timeout: imageTimeout}
}

// clientFor picks the client by surface, which is what the timeout actually
// depends on. Selecting per call site instead is how the generations endpoint
// came to run on the text budget: it shares the non-streaming path with chat,
// and that path knew nothing about images.
func (p *Pipeline) clientFor(surface catalog.Surface) *http.Client {
	if surface == catalog.SurfaceImages {
		return p.imageClient()
	}
	return p.client
}

// RunImageEdit handles a multipart image-edit request.
//
// The only difference from Run is the shape of the body: it is a stream rather
// than a byte slice, so it cannot go through RewriteRequest, which needs
// complete JSON. Mapping the model name would mean substituting it inside the
// multipart body, and that means rewriting the whole multipart stream --
// expensive and easy to get wrong. The trade taken here is that *the edits
// endpoint requires the route's upstream model id to match the model part of
// the public slug*, which the admin UI validates at configuration time. The
// gateway only routes and bills; it does not rewrite the multipart body.
func (p *Pipeline) RunImageEdit(
	ctx context.Context, in Request, contentType string, body io.Reader,
) (Result, *Error) {
	in.Stream = false // images are never streamed
	started := time.Now()
	requestID := in.requestID()

	peeked, err := PeekMultipartModel(body, contentType)
	if err != nil {
		gerr := NewError(errcode.GatewayInvalidRequest, err.Error())
		// Without even a model name there is no identity to speak of, so this
		// only reaches the metrics.
		recordRejected(ctx, string(in.Surface), gerr.Code)
		recordOutcome(ctx, string(in.Surface), failureStatus(gerr.Code), in.Stream, time.Since(started))
		return Result{}, gerr
	}

	// Reuse the pipeline's first half, but with the model name taken from the
	// multipart body rather than from JSON.
	probe := in
	probe.Body = []byte(`{"model":` + quoteJSON(peeked.Model) + `}`)
	prep, gerr := p.admission.Prepare(ctx, probe)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return Result{}, gerr
	}

	// Image requests run long, so the hold's TTL has to outlast them. The
	// default would do, but the intent is stated explicitly.
	holdID, gerr := p.settlementRecorder.ReserveFor(ctx, prep.id, requestID, prep.estNano, imageHoldTTL)
	if gerr != nil {
		p.settlementRecorder.RecordHoldRejection(ctx, in, gerr, started)
		return Result{}, gerr
	}

	route := prep.res.Routes[0]
	byok := prep.byok
	keyID, apiKey, baseURL, byokKeyID, gerr := p.credentialFor(ctx, route, byok)
	usedBYOK := byokKeyID.Valid
	if gerr != nil {
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		return Result{}, gerr
	}

	req, err := BuildRequestStream(ctx, Target{
		Protocol: in.Protocol, BaseURL: baseURL, APIKey: apiKey,
		Path: in.UpstreamPath, Headers: MergeHeaders(route.ProviderHeaders, route.RouteHeaders),
		Transport: route.Transport, UpstreamModel: route.ProviderModelID,
	}, peeked.Body, contentType)
	if err != nil {
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		return Result{}, NewError(errcode.GatewayInternal, err.Error())
	}

	resp, err := p.imageClient().Do(req)
	if err != nil {
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		cls := ClassifyTransportError(ctx, err)
		p.applyBreaker(ctx, route, in.Surface, cls, usedBYOK)
		gerr := NewError(errcode.GatewayUpstreamTimeout, "Upstream is unreachable")
		p.logFailure(ctx, prep.id, in, prep.modelSlug, route, rotationResult{attempts: 1, keyID: keyID, route: route}, requestID, gerr, started)
		return Result{}, gerr
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		return Result{}, NewError(errcode.GatewayUpstreamTimeout, "Reading the upstream response failed")
	}
	if resp.StatusCode >= 400 {
		// The organization's credential was rejected upstream: mark it invalid and
		// notify, and later requests fall back to a shared credential at full
		// price. This path does no within-request rotation, so there is no
		// fallback retry -- this request fails, and that is precisely the
		// signal the organization should see immediately.
		if usedBYOK && byokRejected(resp.StatusCode) {
			p.markBYOKInvalid(ctx, prep.id.OrgID, byokKeyID, resp.StatusCode)
		}
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		cls := ClassifyStatus(resp.StatusCode, respBody, resp.Header.Get("Retry-After"))
		cls.keyID = keyID
		p.applyBreaker(ctx, route, in.Surface, cls, usedBYOK)
		p.logFailure(ctx, prep.id, in, prep.modelSlug, route, rotationResult{attempts: 1, keyID: keyID, route: route}, requestID, cls.Err, started)
		return Result{}, cls.Err
	}
	p.breaker.RecordSuccess(ctx, route.ProviderID, keyID)

	return p.settleImage(ctx, settleImageArgs{
		prep: prep, in: in, route: route, requestID: requestID,
		respBody: respBody, httpStatus: resp.StatusCode, started: started,
		byok: usedBYOK, byokKeyID: byokKeyID, holdID: holdID,
		providerKeyID: sharedKeyIfUsed(usedBYOK, keyID),
	})
}

// imageHoldTTL is how long an image request's hold stays valid: far longer than
// a text request's, and long enough to cover the sweeper's window after the
// request itself has timed out.
const imageHoldTTL = 30 * time.Minute

type settleImageArgs struct {
	prep          prepared
	in            Request
	route         catalog.Route
	requestID     string
	respBody      []byte
	httpStatus    int
	started       time.Time
	byok          bool
	byokKeyID     pgtype.UUID
	providerKeyID pgtype.UUID
	// holdID is the reservation this request was settled against.
	holdID pgtype.UUID
}

// settleImage settles against image usage. Current image models report their
// usage in tokens, so the same price columns and the same billing formula apply
// with nothing added.
func (p *Pipeline) settleImage(ctx context.Context, a settleImageArgs) (Result, *Error) {
	usage := ParseImageUsage(a.respBody)
	estimated := !usage.Present
	if estimated {
		// The upstream reported no usage: *fall back to the held amount*, the
		// same as the billing-failure branch below. An earlier version used
		// Usage{In: inputTokens}, zeroing the output side -- but an image's
		// cost is exactly on the output side, so that gave a real generated
		// image away for almost nothing. The usage row still records the input
		// side; the charge follows the hold.
		usage = Usage{In: a.prep.inputTokens}
		return p.settleImageAt(ctx, a,
			fallbackQuote(a.prep.estNano, a.prep.pricing.ratesForRoute(a.route)),
			usage, true, nil, true)
	}
	quote, err := p.quoteFor(
		a.prep.priceTable.ForBilling(a.prep.res.ModelPricing.IsFree()),
		a.prep.priceTable,
		usage.BillingTokens(),
		a.byok, a.prep.pricing.ratesForRoute(a.route), a.prep.pricing.byokFeeBps,
	)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: image billing failed; settling at the held amount", "error", err, "request_id", a.requestID)
		quote = fallbackQuote(a.prep.estNano, a.prep.pricing.ratesForRoute(a.route))
	}

	return p.settleImageAt(ctx, a, quote, usage, estimated, err, err != nil)
}

// settleImageAt settles and records with the given quote. Both fallback paths
// share it so neither drifts from the other.
func (p *Pipeline) settleImageAt(
	ctx context.Context, a settleImageArgs, quote catalog.Quote, usage Usage, estimated bool,
	pricingErr error, pricingFallback bool,
) (Result, *Error) {
	args := settleArgs{
		id: a.prep.id, requestID: a.requestID, quote: quote, usage: usage,
		estimated: estimated, model: a.prep.res.Model, route: a.route, in: a.in,
		pricing: a.prep.pricing, priceTable: a.prep.priceTable,
		pricingFallback: pricingFallback,
		httpStatus:      a.httpStatus, durationMs: int32(time.Since(a.started).Milliseconds()),
		attempts: 1,
		byok:     a.byok, orgProviderKeyID: a.byokKeyID,
		providerKeyID: a.providerKeyID, holdID: a.holdID,
	}
	recordOutcome(ctx, string(a.in.Surface), statusOrOK(args.status), a.in.Stream,
		time.Duration(args.durationMs)*time.Millisecond)
	authoritativeErr := pricingErr
	if errors.Is(authoritativeErr, catalog.ErrAdvancedPriceMissing) {
		args.pricingIssue = authoritativeErr.Error()
		p.settlementRecorder.RecordPricingMissing(ctx, a.requestID, a.prep.id, a.prep.estNano, usageLogParams(args), authoritativeErr)
	} else if err := p.settlementRecorder.SettleAndLog(ctx, args); err != nil {
		p.settlementRecorder.RecordUnsettled(ctx, a.requestID, a.prep.id, args.quote, usageLogParams(args), err)
	}

	body := AnnotateUsage(a.respBody, estimated, args.quote.ChargedNano, a.prep.id.WalletCurrency)
	return Result{Status: a.httpStatus, Body: body}, nil
}

// ParseImageUsage parses usage out of an image response; current image models
// report it in tokens.
func ParseImageUsage(body []byte) Usage {
	var r struct {
		Usage *struct {
			InputTokens       int64 `json:"input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
			TotalTokens       int64 `json:"total_tokens"`
			InputTokensDetail *struct {
				ImageTokens int64 `json:"image_tokens"`
				TextTokens  int64 `json:"text_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil || r.Usage == nil {
		return Usage{}
	}
	return Usage{In: r.Usage.InputTokens, Out: r.Usage.OutputTokens, Present: true}
}

// quoteJSON embeds a string into JSON safely. The model name comes from client
// input and must never be concatenated raw.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
