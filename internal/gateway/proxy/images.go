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
	if isImageSurface(surface) {
		return p.imageClient()
	}
	return p.client
}

// firstByteBudgetFor is the streaming counterpart of clientFor, and it exists
// because the same mistake was made twice.
//
// An image stream's frames are partial images, and `partial_images` defaults to
// *zero*. With none requested the only frame is the terminal result, which
// arrives when the whole generation does -- so on this surface the wait for the
// "first byte" is the wait for the generation, and the text budget cuts it off
// at a minute. It is the image timeout because the two are waiting for the very
// same thing; a streamed generation and an unstreamed one differ in delivery,
// not in how long the upstream needs.
func firstByteBudgetFor(surface catalog.Surface) time.Duration {
	if isImageSurface(surface) {
		return imageTimeout
	}
	return firstByteTimeout
}

// RunImageEdit handles a multipart image-edit request.
//
// The only difference from Run is the shape of the body: it is a stream with an
// upload in it rather than a byte slice, so it cannot go through
// RewriteRequest, which needs complete JSON. The model name is still
// substituted -- in the prefix the peek already holds, see
// PeekedMultipart.BodyFor -- so a route here says what its upstream calls the
// model, exactly as everywhere else. The upload itself is never touched.
//
// It used to not do that, and the price was a rule that held on this endpoint
// and nowhere else: the route's upstream model id had to equal the second
// segment of the public slug.
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

	// Reuse the pipeline's first half, but with the billing fields taken from
	// the multipart body rather than from JSON. The three beside the model are
	// what a per-image rate is looked up by; without them an edit on a
	// per-image model is quoted at the widest row of the card while the caller
	// asked for a narrower one, and the hold is then the wrong amount.
	probe := in
	probe.Body = peekedProbeBody(peeked)
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

	// The model name becomes the upstream's own, in the buffer already read.
	// It is the same substitution RewriteRequest makes on every JSON endpoint;
	// this one had to do it here because its body is a stream.
	outBody, err := peeked.BodyFor(route.ProviderModelID)
	if err != nil {
		p.settlementRecorder.VoidHold(ctx, prep.id.OrgID, requestID)
		return Result{}, NewError(errcode.GatewayInternal, err.Error())
	}
	req, err := BuildRequestStream(ctx, Target{
		Protocol: in.Protocol, BaseURL: baseURL, APIKey: apiKey,
		Path: in.UpstreamPath, Headers: MergeHeaders(route.ProviderHeaders, route.RouteHeaders),
		Transport: route.Transport, UpstreamModel: route.ProviderModelID,
	}, outBody, contentType)
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

// holdTTLFor is the lifetime of the reservation for a surface, and it is picked
// here for the same reason clientFor picks the client here: per surface, not
// per call site.
//
// Both image endpoints need the long one and only one of them was getting it.
// The edits path asked for it explicitly while generations went through the
// shared reserve with the default, even though the two share the 300-second
// upstream budget that is the whole reason the long TTL exists -- and a
// per-image reservation now covers the route's ceiling, so a hold that expires
// mid-request releases more than it used to.
//
// Zero means the billing layer's own default, which is what every other
// surface takes.
func holdTTLFor(surface catalog.Surface) time.Duration {
	if isImageSurface(surface) {
		return imageHoldTTL
	}
	return 0
}

// holdIsTheBill reports whether a request that produced no parseable usage must
// settle at the amount held rather than at an estimate.
//
// True on the image surface and only there, and it is not a rounding
// preference. An image's cost sits almost entirely on the output side, while
// the estimator works from produced *text* -- of which an image response has
// none at all. Estimating therefore does not approximate the bill, it collapses
// it: the upstream generates the image, the row reads zero output tokens, and a
// real generation is given away. The hold was computed from the same price
// table settlement would have used, so it is the closest true answer available.
//
// Shared by all three settlement paths rather than spelled in each. It used to
// live only in the buffered one, which is how a streamed generation came to
// settle at nothing.
func holdIsTheBill(surface catalog.Surface, estimated bool) bool {
	return estimated && isImageSurface(surface)
}

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

// settleImage settles an image request.
//
// Two families reach here and they settle differently (ADR-0227). A token-billed
// model is charged from what the upstream reported, exactly as chat is. A
// per-image model is charged from what the caller asked for: that amount was
// computed and held during admission, the upstream's own usage object says
// nothing about it, and re-deriving it here would be a second answer to a
// question already settled.
func (p *Pipeline) settleImage(ctx context.Context, a settleImageArgs) (Result, *Error) {
	if a.prep.unitBilled() {
		return p.settleImageUnits(ctx, a)
	}
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
			usage, true, nil, true, a.prep.units)
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

	return p.settleImageAt(ctx, a, quote, usage, estimated, err, err != nil, a.prep.units)
}

// settleImageUnits settles a per-image model.
//
// The quantity is read from the response, not carried over from the hold. What
// was reserved is the route's declared ceiling, because how many images a
// request will come back with is not knowable before it is made; what is owed
// is how many it came back with. The amount is computed here rather than at
// admission because whether this ran on the organization's own credential is
// known only now -- and a BYOK request owes the service fee, not the list
// price.
//
// The usage row carries no token counts, because there are none, and carries
// the settled quantity vector instead.
func (p *Pipeline) settleImageUnits(ctx context.Context, a settleImageArgs) (Result, *Error) {
	units := settledUnits(ctx, a.prep, a.in, a.route, a.respBody)
	quote, gerr := p.quoteOrRefuse(ctx, a.prep, func() (catalog.Quote, error) {
		return p.unitCharge(a.prep, a.route, a.byok, units)
	})
	if gerr != nil {
		// The rate card priced this vector during admission, so failing now
		// means the price changed underneath a request in flight. The hold is
		// left standing for the operator's repair queue rather than voided:
		// the image was generated and somebody has to decide what it cost.
		p.settlementRecorder.RecordUnsettled(ctx, a.requestID, a.prep.id,
			catalog.Quote{}, usageLogParams(settleArgs{
				id: a.prep.id, requestID: a.requestID, units: units,
				model: a.prep.res.Model, route: a.route, in: a.in,
				pricing: a.prep.pricing, priceTable: a.prep.priceTable,
				httpStatus: a.httpStatus, holdID: a.holdID,
			}), errors.New("pricing a per-image request failed after it was served"))
		return Result{}, gerr
	}
	return p.settleImageAt(ctx, a, quote, unitUsage(), false, nil, false, units)
}

// unitUsage is the usage a unit-billed request records: none, stated
// explicitly.
//
// Valid-but-zero rather than absent, matching what the job plane writes: NULL
// in these columns means "the upstream did not report", and here the fact is
// that there is nothing to report. The unsettled-replay encoder also refuses a
// row whose token dimensions are missing, and this path can reach that queue.
func unitUsage() Usage { return Usage{Present: true} }

// settleImageAt settles and records with the given quote. Both fallback paths
// share it so neither drifts from the other.
func (p *Pipeline) settleImageAt(
	ctx context.Context, a settleImageArgs, quote catalog.Quote, usage Usage, estimated bool,
	pricingErr error, pricingFallback bool, units catalog.Units,
) (Result, *Error) {
	args := settleArgs{
		id: a.prep.id, requestID: a.requestID, quote: quote, usage: usage,
		units:     units,
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

// ParseImageUsage parses usage out of an image response, for the image models
// that report in tokens. A model billed per produced image never reaches here:
// its charge is a function of the request, settled before the body is read.
//
// Both image breakdowns are carried through rather than dropped. They used to
// be parsed and thrown away, which billed image tokens at the model's text
// rates -- for gpt-image-2 that is 5 against a real 8 per million on input, and
// 10 against 32 on output, the bucket that dominates a generation. Like audio,
// image tokens are a subset of their parent count, so BuildCharge subtracts
// them before pricing the remainder and a model with no image rate configured
// still bills exactly as before.
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
			OutputTokensDetail *struct {
				ImageTokens int64 `json:"image_tokens"`
				TextTokens  int64 `json:"text_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil || r.Usage == nil {
		return Usage{}
	}
	u := Usage{In: r.Usage.InputTokens, Out: r.Usage.OutputTokens, Present: true}
	if d := r.Usage.InputTokensDetail; d != nil {
		// Clamped to the input total rather than trusted outright: the
		// subset relation is what makes the subtraction in BuildCharge
		// correct, and an upstream that contradicts it must not be able to
		// turn a served request into a billing error.
		u.ImageIn = min(nonNegative(d.ImageTokens), u.In)
	}
	if d := r.Usage.OutputTokensDetail; d != nil {
		// The output side, clamped the same way. This is the bucket that
		// dominates a generation: the upstream prices its image output tokens
		// several times its text output, and with nowhere to record them every
		// generated image was billed at the text rate.
		u.ImageOut = min(nonNegative(d.ImageTokens), u.Out)
	}
	return u
}

// nonNegative floors a reported count at zero. A negative token count is
// meaningless and only ever arrives from a malformed upstream body.
func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
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

// ── Per-image billing ────────────────────────────────────────────────────
//
// An image model may be billed either way, and which one is a property of the
// price row rather than of this endpoint (ADR-0227). gpt-image and Gemini's
// image models report tokens and settle from the response; Seedream and the
// vendors like it sell images by the piece, and their charge is a pure function
// of the request -- how many, at what size, at what quality -- so it is known
// before the upstream is called and the hold is that exact amount.

// syncUnits is the billable quantity vector of a unit-priced request on a
// synchronous surface.
//
// It is dispatched on the surface rather than on the body's shape: a body that
// merely happens to carry an `n` is not evidence of what is being billed, and
// guessing here would put a quantity on a request the rate card never meant to
// count.
func (p *Pipeline) syncUnits(ctx context.Context, prep prepared, in Request) (catalog.Units, *Error) {
	// The unit comes from the model's rate card, never from this call site: a
	// model priced per generation looked up per image misses every row and
	// answers "unpriced" while its rates sit right there.
	unit, err := prep.unitPriceTable.BillingUnit()
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: cannot tell what unit this model is billed in",
			"model", prep.modelSlug, "error", err)
		p.alertUnpriced(ctx, prep.modelSlug)
		return catalog.Units{}, NewError(errcode.GatewayModelUnpriced, "Model is temporarily unavailable")
	}
	switch in.Surface {
	case catalog.SurfaceImages, catalog.SurfaceImagesEdit:
		// The ceiling, not a count read out of the request. This vector is the
		// *hold*; settlement keeps this rate row and replaces only the count.
		params := imageParamsOf(in.Body)
		key := catalog.UnitKey{Unit: unit, Resolution: params.Size, Variant: params.Quality}
		return imageUnits(key, maxImagesOf(prep.res.Routes)), nil
	default:
		// Reached only if a surface is given the units family without anybody
		// deciding what it counts. Refusing is the only safe answer: serving it
		// would charge zero for something the operator priced.
		slog.ErrorContext(ctx, "dataplane: surface is billed by unit but nothing says what it counts",
			"surface", string(in.Surface), "model", prep.modelSlug)
		return catalog.Units{}, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}
}

// imageParams are the request fields a per-image rate is looked up by.
//
// Size and Quality and nothing else. `n` used to be here as the quantity, and
// removing it is the point rather than a tidy-up: the quantity is now read from
// the response (see imagesInResponse), and leaving a request field that decides
// money in a function that also decides money is how the two got confused.
type imageParams struct {
	// Size and Quality select the rate row. Empty means the caller took the
	// upstream's default, and an empty axis on the card matches it -- so a
	// model with one flat price per image needs no axes configured at all.
	Size    string
	Quality string
}

// imageParamsOf reads them out of a JSON body.
//
// Reading is not rewriting: RewriteRequest remains the only function that edits
// a request body, and the pass-through guarantee is untouched (ADR-0140,
// ADR-0219). Billing has always had to read `model` out of this body; a
// per-image rate needs two more fields to pick a row.
//
// A malformed or absent field yields the zero value rather than an error. The
// body is forwarded to the upstream either way, and the upstream is the
// authority on whether it is valid; refusing here would reject requests the
// vendor accepts.
func imageParamsOf(body []byte) imageParams {
	var doc struct {
		Size    string `json:"size"`
		Quality string `json:"quality"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return imageParams{}
	}
	return imageParams{Size: doc.Size, Quality: doc.Quality}
}

// imagesInResponse counts the images an image response actually carries.
//
// This is the billable quantity on a per-image model, and it is read from the
// response because the request cannot answer it. `n` is OpenAI's word for it,
// and the vendor that made per-image billing worth having does not have that
// parameter at all: Seedream batches with sequential_image_generation and
// max_images and can return fifteen. A charge derived from `n` was therefore
// wrong in both directions at once -- fifteen images billed as one, and an `n`
// the upstream ignored billed as four.
//
// `data` is the one thing every OpenAI-shaped image API agrees on, and unlike a
// request field it cannot be set by a caller who is not the one paying for it.
//
// The count is deliberately taken by decoding into a slice of empty structs:
// the elements are skipped rather than materialised, so counting a 64 MiB
// response of base64 does not copy it. `data` absent is reported as absent, not
// as zero -- zero would settle a served generation at nothing, and the caller
// falls back to the hold instead.
func imagesInResponse(body []byte) (int64, bool) {
	var doc struct {
		Data *[]struct{} `json:"data"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Data == nil {
		return 0, false
	}
	return int64(len(*doc.Data)), true
}

// imageUnits turns the rate-row parameters and a count into a quantity vector.
//
// `image` counts one unit per produced image; `call` counts one per request
// however many it produced. That difference is why they are separate units and
// not one name with a footnote: for four images the two answers differ
// fourfold, and neither reading announces itself in a bill.
//
// The count reaches here from two different places on purpose. At admission it
// is the route's declared ceiling, because the reservation has to cover the
// most the request could produce; at settlement it is what the response
// actually contained. Passing it in rather than deriving it here is what keeps
// those two facts from being confused for one.
func imageUnits(key catalog.UnitKey, images int64) catalog.Units {
	quantity := int64(1)
	if key.Unit == catalog.UnitImage && images > 1 {
		quantity = images
	}
	return catalog.Units{Quantities: map[catalog.UnitKey]int64{key: quantity}}
}

// heldRateKey is the rate row this request was admitted against.
//
// Settlement re-counts the images but must not re-resolve the row. The row was
// resolved at admission from the body admission actually had, and on the edits
// endpoint that is not the request body at all -- it is the small fields peeked
// out of the multipart stream, because the real body is an upload. Reading the
// request body again at settlement therefore found nothing there and looked up
// a different row from the one that was held and priced.
//
// It is also the only place the two could disagree: a size the card does not
// price is already refused at admission, so a key that reached here is one that
// exists.
func heldRateKey(held catalog.Units) (catalog.UnitKey, bool) {
	if len(held.Quantities) != 1 {
		return catalog.UnitKey{}, false
	}
	for key := range held.Quantities {
		return key, true
	}
	return catalog.UnitKey{}, false
}

// maxImagesOf is the largest number of images any candidate route could return.
//
// The most conservative of the candidates, for the same reason catalog.HoldCap
// takes the most conservative token cap: the hold is taken before a route is
// picked, so it has to cover whichever one ends up serving. An undeclared route
// counts as one, which is what an endpoint does with a request that names no
// number.
func maxImagesOf(routes []catalog.Route) int64 {
	most := int64(1)
	for _, r := range routes {
		if n := int64(r.MaxImages); n > most {
			most = n
		}
	}
	return most
}

// peekedProbeBody renders the fields read ahead out of a multipart body as the
// JSON the pipeline's first half expects.
//
// It is not the request: the original multipart stream is what reaches the
// upstream, byte for byte. This is only how admission and pricing are handed
// what they need to read.
func peekedProbeBody(p PeekedMultipart) []byte {
	doc := map[string]any{"model": p.Model}
	if p.Size != "" {
		doc["size"] = p.Size
	}
	if p.Quality != "" {
		doc["quality"] = p.Quality
	}
	body, err := json.Marshal(doc)
	if err != nil {
		// Marshalling a map of strings cannot fail in practice; falling back to
		// the model alone keeps the request servable rather than turning an
		// impossible error into a 500.
		return []byte(`{"model":` + quoteJSON(p.Model) + `}`)
	}
	return body
}

// settledUnits is the quantity vector a served per-image request is charged on:
// the rate row that was held, with the count the response actually carried.
//
// A response with no readable `data` array falls back to the held quantity.
// That is deliberately the conservative direction and not a guess: the images
// were generated and the upstream has charged for them, so settling at zero
// would give away a generation, while settling at the ceiling never charges for
// more than the reservation the organization had already been checked against.
//
// A count *above* the ceiling is charged in full rather than clipped. It means
// the route's declared maximum is wrong -- the upstream returned more than it
// said it could -- and clipping would quietly absorb the difference instead of
// putting a number in front of an operator.
func settledUnits(
	ctx context.Context, prep prepared, in Request, route catalog.Route, respBody []byte,
) catalog.Units {
	if !isImageSurface(in.Surface) {
		// The mirror of syncUnits' default arm, and it exists for the same
		// reason: admission names the surfaces it knows how to count and
		// refuses the rest, so settlement must not silently count a surface
		// admission never admitted. Reaching here means a new unit-billed
		// surface was given an admission arm and no settlement arm.
		slog.ErrorContext(ctx, "dataplane: surface is billed by unit but settlement cannot count it",
			"surface", string(in.Surface), "model", prep.modelSlug)
		return prep.units
	}
	key, ok := heldRateKey(prep.units)
	if !ok {
		return prep.units
	}
	images, ok := imagesInResponse(respBody)
	if !ok {
		// The reservation stands in for a count nobody could read, and it is
		// the ceiling -- so on this family the fallback can be several times
		// the truth, unlike the token family's, whose hold is an estimate of
		// the same request. It stays the answer because the alternative is
		// giving away a generation the upstream has already charged for, but
		// it must not be a silent one: a 200 from one of these endpoints
		// always carries `data`, so reaching here means this route is
		// answering in a shape nothing here understands.
		slog.WarnContext(ctx, "dataplane: could not count the images in an upstream response; charging the reservation",
			"model", prep.modelSlug, "route_id", route.ID, "provider", route.ProviderSlug)
		return prep.units
	}
	if ceiling := maxImagesOf(prep.res.Routes); images > ceiling {
		// The declared ceiling is wrong, and this is the only place that can
		// notice. The charge is still the real count -- clipping it would
		// absorb the difference quietly -- but the reservation this request
		// passed its budget check against was too small, so the number goes in
		// front of an operator with the route that produced it.
		slog.WarnContext(ctx, "dataplane: an image response exceeded the route's declared maximum; raise max_images",
			"model", prep.modelSlug, "route_id", route.ID, "provider", route.ProviderSlug,
			"declared", ceiling, "returned", images)
	}
	return imageUnits(key, images)
}

// isImageSurface reports whether a surface is one of the image endpoints.
//
// Generations and edits are separate surfaces because their *capability* is
// separate -- a vendor can serve one and not the other -- but everything about
// how they are served is the same: the same protocol, the same rate card, the
// same budget, the same billing families, the same reasons an estimate cannot
// stand in for a missing usage report. Asking the question once is what keeps
// the two from drifting apart on any of that.
func isImageSurface(s catalog.Surface) bool {
	return s == catalog.SurfaceImages || s == catalog.SurfaceImagesEdit
}
