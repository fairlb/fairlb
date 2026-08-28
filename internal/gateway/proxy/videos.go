package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// The video job plane (ADR-0218).
//
// It is mounted beside the inference data plane and shares everything about
// admission -- authentication, the kill switch, the model tier, the key
// allowlist, budgets, rate limits, candidate resolution -- and nothing about
// request shape, billing unit or delivery. A video request is not forwarded on
// the protocol it arrived on, because there is no such protocol: the contract
// is this gateway's own and each vendor's mapper shapes the outbound call.
//
// Errors render on the OpenAI surface. The contract is ours, but the shape is
// the one every SDK in this space already parses.

// maxVideoRequestBody is generous because a request may carry reference images
// inline, and mean because a video request is otherwise small: prompt, a few
// scalars. It is well below the image edit path's 64 MiB, which has to carry
// whole source images as multipart.
const maxVideoRequestBody = 16 << 20

// maxIdempotencyKeyLength matches the shared middleware's bound, so the two
// agree about what a key may look like even though this route enforces its own.
const maxIdempotencyKeyLength = 255

// MountVideos registers the video job plane.
//
// # Why this route does not use the shared idempotency middleware
//
// A submit is a paid create and a retried one must not become a second job, so
// idempotency is not optional here. The generic middleware could not provide
// it:
//
//   - it is opt-in. A caller that omits the header gets no protection at all,
//     and the guarantee that matters most on this plane would depend on the
//     client having remembered.
//   - it caps the request body at 1 MiB, while this route accepts 16 MiB
//     because reference images may arrive as data URLs. The same request would
//     succeed without the header and fail with it, penalising the caller for
//     doing the safe thing.
//   - it answers in problem+json, and every other response from this route is
//     the OpenAI error shape. An SDK reading error.code on a 409 would get a
//     body it cannot parse.
//
// So idempotency is a property of the job resource instead: the key is
// required, it is stored on the row, and a unique index over
// (org_id, kind, idempotency_key) is what makes a duplicate impossible rather
// than unlikely. A repeat of a key returns the job it already created; a repeat
// with a different body is refused rather than answered with somebody else's
// video.
func (p *Pipeline) MountVideos(r chi.Router) {
	r.Post("/videos", p.handleVideoSubmit())
	r.Get("/videos", p.handleVideoList())
	// Before the {video_id} route, or chi would read "models" as an id.
	r.Get("/videos/models", p.handleVideoModels())
	r.Get("/videos/{video_id}", p.handleVideoGet())
	r.Delete("/videos/{video_id}", p.handleVideoDelete())
	r.Post("/videos/{video_id}/cancel", p.handleVideoCancel())
	r.Get("/videos/{video_id}/content", p.handleVideoContent())
}

// requestFingerprint identifies the body a key was first used with.
func requestFingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// handleVideoSubmit admits a video job.
//
// The order below is the whole argument of ADR-0220, so it is worth stating:
// everything that can refuse the request happens before anything that costs
// money. Decode refuses an unknown parameter; the envelope refuses a value the
// model cannot serve; the price is then computed exactly, from parameters
// already validated, with no upstream involved and nothing estimated.
func (p *Pipeline) handleVideoSubmit() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxVideoRequestBody))
		if err != nil {
			// Only an oversized body is a size problem. A dropped connection
			// reported as "too large" sends the caller off shrinking a request
			// that was never the trouble.
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				Write(w, SurfaceOpenAI, NewError(errcode.GatewayRequestTooLarge, "Request body is too large"))
				return
			}
			Write(w, SurfaceOpenAI, NewError(errcode.GatewayInvalidRequest, "Failed to read the request body"))
			return
		}
		idempotencyKey := strings.TrimSpace(r.Header.Get(httpx.HeaderIdempotencyKey))
		if idempotencyKey == "" {
			Write(w, SurfaceOpenAI, NewError(errcode.GatewayInvalidRequest,
				"Submitting a video job requires an "+httpx.HeaderIdempotencyKey+" header: "+
					"a retry without one would be charged as a second job"))
			return
		}
		if len(idempotencyKey) > maxIdempotencyKeyLength {
			Write(w, SurfaceOpenAI, NewError(errcode.GatewayInvalidRequest,
				httpx.HeaderIdempotencyKey+" is too long"))
			return
		}

		req, err := video.Decode(body)
		if err != nil {
			// Every one of these is the caller's to fix and every one names the
			// offending field, so the text goes back as written rather than
			// being flattened into "invalid request".
			var unknown video.ErrUnknownParameter
			switch {
			case errors.As(err, &unknown):
				Write(w, SurfaceOpenAI, NewError(errcode.GatewayVideoParamsUnsupported, err.Error()))
			default:
				Write(w, SurfaceOpenAI, NewError(errcode.GatewayInvalidRequest, err.Error()))
			}
			return
		}

		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)

		in := Request{
			Surface: catalog.SurfaceVideo, Protocol: ProtocolVideo,
			Body: body, Model: req.Model,
			Credential: CredentialOf(r), EndUserID: r.Header.Get("X-End-User-Id"),
			RequestID: requestID,
		}
		job, replayed, gerr := p.admitAndSubmitVideo(
			r.Context(), in, req, "", idempotencyKey, requestFingerprint(body))
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		if replayed {
			writeJSON(w, http.StatusOK, videoJobPayload(job))
			return
		}
		writeJSON(w, http.StatusAccepted, videoJobPayload(job))
	}
}

// admitAndSubmitVideo is everything between a decoded request and a job row,
// and it is deliberately the only copy.
//
// Two transports reach it: /v1/videos, and each vendor's compatibility surface.
// What happens in here is a chain of judgements about money -- what the model
// accepts, what the request costs, whether this key has already been used --
// and a second implementation of that chain is how two surfaces come to
// disagree about whether a customer was charged. The same argument VideoJobs
// makes for the read side.
//
// The caller keeps two jobs: building the inbound Request, and rendering
// whatever comes back. Those are the parts that really do differ, because one
// surface answers in the OpenAI error shape and the others answer in their
// vendor's.
//
// `replayed` says this key had already created a job. It is a different answer
// from a fresh submit -- 200 rather than 202 on the normalised plane -- and
// collapsing the two would tell a caller retrying after a timeout that they had
// just been charged again.
// vendor, when set, narrows the candidate routes to that upstream. A vendor
// compatibility surface sets it; /v1/videos leaves it empty.
//
// This is the rule ADR-0218 decision six required of vendor_options -- "naming
// a vendor is required, and it restricts candidates to that vendor's routes" --
// which ADR-0229 removed along with the field, on the reasoning that a surface
// names its vendor implicitly. It does, but the narrowing has to be applied
// somewhere, and until it was here a request to one vendor's surface could be
// served by another's route while carrying the first one's parameters.
func (p *Pipeline) admitAndSubmitVideo(
	ctx context.Context, in Request, req video.Request, vendor, idempotencyKey, fingerprint string,
) (job gwdb.GatewayAsyncJob, replayed bool, gerr *Error) {
	started := time.Now()
	prep, gerr := p.admission.Prepare(ctx, in)
	// refuse records and answers. Every refusal below Prepare goes through it,
	// because a refusal that leaves no usage row and no metric is invisible to
	// the organization, to the metrics and to support -- and the envelope
	// refusals below are the ones a video caller will hit most often. Recording
	// only some of them is worse than recording none: "model not found" would
	// appear in the usage log and "this model does not do twelve seconds" would
	// not.
	refuse := func(e *Error) (gwdb.GatewayAsyncJob, bool, *Error) {
		p.settlementRecorder.RecordRejection(ctx, prep, in, in.RequestID, e, started)
		return gwdb.GatewayAsyncJob{}, false, e
	}
	if gerr != nil {
		return refuse(gerr)
	}

	// The union across candidate routes is what the model as a whole accepts.
	// Validation happens against the union because the hold is taken before a
	// route is chosen; the per-route envelopes narrow the candidate set
	// afterwards, and narrowing can only change who serves the job, never what
	// it costs (ADR-0221).
	//
	// On /v1/videos nothing narrows the candidate set to one vendor, and that is
	// deliberate: every route of a slug serves the same model, so moving
	// between them is always legitimate, while a request able to pin a vendor
	// would be a request able to change failover.
	//
	// A compatibility surface is the exception, and it is not the caller
	// choosing: the request arrived in one vendor's shape and carries that
	// vendor's own parameters, so serving it from another's route would send
	// those parameters somewhere they mean nothing.
	candidates := prep.res.Routes
	if vendor != "" {
		candidates = routesOfVendor(candidates, vendor)
		if len(candidates) == 0 {
			return refuse(NewError(errcode.GatewayModelNotFound,
				"this model is not served by "+vendor+" on this gateway"))
		}
	}

	envelopes := routeEnvelopes(ctx, candidates)
	accepts := video.Union(envelopes)
	audioOn := accepts.ResolveAudio(req)
	if err := accepts.Validate(req, audioOn); err != nil {
		return refuse(NewError(errcode.GatewayVideoParamsUnsupported, err.Error()))
	}

	// The union said some route can serve this; now keep only the routes that
	// actually can. Without this the request would be dispatched to a candidate
	// the gateway already knows will reject it -- the hold would be voided and
	// the caller kept whole, but the job would fail for a reason that was
	// answerable here (ADR-0221).
	candidates = coveringRoutes(candidates, envelopes, req, audioOn)
	if len(candidates) == 0 {
		return refuse(NewError(errcode.GatewayVideoParamsUnsupported,
			"no route for this model can serve that combination of parameters"))
	}

	// Priced here only to refuse an unpriced model before anything is reserved.
	// The charge that is actually held is quoted again per candidate, once the
	// credential is known: work billed to the organization's own upstream
	// account is charged a service fee rather than the list rate.
	units, gerr := p.videoUnits(ctx, prep, req, audioOn)
	if gerr != nil {
		return refuse(gerr)
	}

	// Everything above this line can refuse, and nothing above it has cost the
	// caller anything.
	//
	// A repeat of a key is answered with the job it already created rather than
	// a second paid one. Checked here as well as enforced by the unique index:
	// the check answers the ordinary retry cheaply, and the index is what makes
	// two *concurrent* retries impossible, which no check-then-act can.
	if existing, found := p.videoJobForKey(ctx, prep.id.OrgID, idempotencyKey); found {
		if existing.RequestFingerprint != fingerprint {
			return refuse(NewError(errcode.GatewayInvalidRequest,
				"This "+httpx.HeaderIdempotencyKey+" was already used for a different request"))
		}
		return existing, true, nil
	}

	job, gerr = p.submitVideoJob(ctx, prep, in, req, audioOn, units, candidates,
		idempotencyKey, fingerprint, envelopeJobSeconds(accepts))
	if gerr != nil {
		return refuse(gerr)
	}
	return job, false, nil
}

// routesOfVendor keeps only the routes that reach one upstream.
func routesOfVendor(routes []catalog.Route, vendor string) []catalog.Route {
	out := make([]catalog.Route, 0, len(routes))
	for _, rt := range routes {
		if rt.ProviderVendor == vendor {
			out = append(out, rt)
		}
	}
	return out
}

// envelopeJobSeconds is how long this model's jobs may run, which sizes the
// hold. It comes from the model rather than a global constant because a short
// clip and a long render are an order of magnitude apart.
func envelopeJobSeconds(e video.Envelope) int {
	if e.MaxJobSeconds > 0 {
		return e.MaxJobSeconds
	}
	return 900
}

// videoUnits turns an admitted request into its billable quantity vector, and
// proves the model can price it before anything is reserved.
//
// The unit comes from the model's rate card, never from this call site: a model
// priced per generation looked up per second misses every row and answers
// "unpriced" while its rates sit right there.
func (p *Pipeline) videoUnits(
	ctx context.Context, prep prepared, req video.Request, audioOn bool,
) (catalog.Units, *Error) {
	unit, err := prep.unitPriceTable.BillingUnit()
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: cannot tell what unit this model is billed in",
			"model", prep.modelSlug, "error", err)
		p.alertUnpriced(ctx, prep.modelSlug)
		return catalog.Units{}, NewError(errcode.GatewayModelUnpriced, "Model is temporarily unavailable")
	}
	units := video.BillingUnits(req, audioOn, unit)
	// A dry run against the list price. It answers the only question that has
	// to be settled before a reservation exists -- can this model price what
	// was asked for -- without deciding the amount, which depends on a route
	// nobody has chosen yet.
	list, cost := prep.billingUnitPrices()
	if _, gerr := p.quoteOrRefuse(ctx, prep, func() (catalog.Quote, error) {
		return catalog.ComputeUnits(list, cost, units, prep.pricing.rates)
	}); gerr != nil {
		return catalog.Units{}, gerr
	}
	return units, nil
}

// quoteOrRefuse maps a pricing failure onto the answer the caller gets.
//
// Shared by both unit-priced planes -- the job plane here and the synchronous
// image surface -- because the two failures are the same fact seen twice: the
// caller asked for a value the model's rate card cannot price, and the honest
// answer is to refuse before anything is reserved rather than to fall back to
// some other row's number.
func (p *Pipeline) quoteOrRefuse(
	ctx context.Context, prep prepared, compute func() (catalog.Quote, error),
) (catalog.Quote, *Error) {
	quote, err := compute()
	if err == nil {
		return quote, nil
	}
	if errors.Is(err, catalog.ErrUnitPriceMissing) {
		slog.ErrorContext(ctx, "dataplane: request names a unit with no rate; refusing to serve",
			"model", prep.modelSlug, "error", err)
		p.alertUnpriced(ctx, prep.modelSlug)
		return catalog.Quote{}, NewError(errcode.GatewayModelUnpriced, "Model is temporarily unavailable")
	}
	slog.ErrorContext(ctx, "dataplane: pricing a unit-billed request failed", "error", err, "model", prep.modelSlug)
	return catalog.Quote{}, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
}

// routeEnvelopes parses each candidate's declared envelope. A route whose
// envelope is unreadable is dropped rather than treated as unlimited: a
// malformed capability claim must never widen what the gateway will accept.
func routeEnvelopes(ctx context.Context, routes []catalog.Route) []video.Envelope {
	out := make([]video.Envelope, 0, len(routes))
	for _, rt := range routes {
		e, err := video.ParseEnvelope(rt.VideoEnvelope)
		if err != nil {
			// An empty envelope rather than a dropped entry: the result is
			// positionally paired with the routes it describes, and an empty
			// envelope declares nothing, so a route with an unreadable claim
			// is excluded from both the union and the covering set. A
			// malformed capability claim must never widen what is accepted.
			slog.ErrorContext(ctx, "dataplane: route has an unreadable video envelope; ignoring its claim",
				"route", rt.ID, "provider", rt.ProviderSlug, "error", err)
			e = video.Envelope{}
		}
		out = append(out, e)
	}
	return out
}

// coveringRoutes keeps the candidates whose own envelope covers the request.
//
// envelopes is parallel to routes: routeEnvelopes returns one entry per route
// in order, so a route whose envelope failed to parse would misalign the two.
// That is why routeEnvelopes substitutes an empty envelope rather than
// dropping the entry -- an empty envelope covers nothing, so such a route is
// filtered out here, which is the same conservative outcome by a route that
// keeps the two lists the same length.
func coveringRoutes(routes []catalog.Route, envelopes []video.Envelope, r video.Request, audioOn bool) []catalog.Route {
	out := make([]catalog.Route, 0, len(routes))
	for i, rt := range routes {
		if i < len(envelopes) && envelopes[i].Covers(r, audioOn) {
			out = append(out, rt)
		}
	}
	return out
}

// routesOfVendor keeps only the candidates belonging to one upstream platform.
