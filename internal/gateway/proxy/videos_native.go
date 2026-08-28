package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/video"
)

// The vendor compatibility surfaces: this gateway answering at each vendor's own
// paths, in each vendor's own shapes, so that switching to it is a base URL and
// a key.
//
// # Addressing
//
//	https://api.<domain>/video/<vendor>  +  that vendor's own path
//
// A prefix per vendor rather than one shared namespace, for a reason that shows
// up immediately: Kling creates a job at POST /v1/videos/text2video, which
// collides with this gateway's own GET /v1/videos/{id}, and Veo's
// :predictLongRunning would land on the already-mounted Gemini plane. Under a
// prefix the collisions cannot arise, and the arrangement is the same one every
// upstream already uses -- a base URL with the vendor's paths under it -- so a
// client's own path handling is unchanged.
//
// The prefix carries no version segment because the vendors' paths carry their
// own, and no two of them agree: /v1, /api/v3, /v1beta, /api/v1.
//
// # What is shared, and what is not
//
// Everything between an admitted request and a job row is the same function the
// normalised plane calls (admitAndSubmitVideo), and every read goes through the
// same VideoJobs the console uses. What each surface owns is the two ends: how
// this vendor's request body is read, and how a job is written back in this
// vendor's shape.

// nativeIdempotencyWindow is how long two identical requests are treated as one.
//
// The normalised plane requires an Idempotency-Key, because a retry without one
// is a second paid job (ADR-0220). None of these vendors publishes such a
// header, so a client's SDK will never send it, and the key has to be derived
// from the request itself.
//
// A key derived from the body alone would mean the same request could never be
// made twice -- and "generate that again" is a thing people legitimately want.
// A key derived from nothing would mean one network-layer retry is a second
// charge, which is the failure that made idempotency mandatory in the first
// place. A window separates the two: an automatic retry arrives within seconds,
// a person asking for another take does not. The length is published in the
// customer documentation, and a caller who needs exact control uses /v1/videos
// and supplies their own key.
//
// The end user is part of the key, and that is not a refinement. The uniqueness
// index is per organization, so without it two end users of one organization
// who send the same prompt in the same minute collide -- and the second is
// answered with the first one's job, which means one customer's user downloads
// another's video.
const nativeIdempotencyWindow = time.Minute

// MountVideoNative mounts every compatibility surface this build publishes.
//
// Driven off the registry rather than a list here: a vendor that registers a
// surface becomes reachable, with no second place to keep in step.
//
// Declaration order does not decide matching. chi resolves through a radix
// trie in which a static segment beats a wildcard however the two were
// registered, so a literal path and a pattern that could swallow it coexist
// without either having to be first. TestNativeRoutesAreUnambiguous is what
// keeps two routes from claiming the same method and path.
func (p *Pipeline) MountVideoNative(r chi.Router) {
	for _, vendor := range video.NativeVendors() {
		surface, ok := video.NativeSurfaceFor(vendor)
		if !ok {
			continue
		}
		r.Route("/"+vendor, func(sub chi.Router) {
			for _, route := range surface.Routes() {
				sub.Method(route.Method, route.Path, p.handleNative(surface, route))
			}
			// Anything else this vendor publishes and this gateway does not
			// serve. Answered in the vendor's own error shape and naming the
			// path, because a client whose SDK reached a lip-sync endpoint
			// deserves to be told this gateway does not offer it rather than
			// that the path does not exist (ADR-0157).
			sub.NotFound(p.handleNativeUnsupported(surface))
			sub.MethodNotAllowed(p.handleNativeUnsupported(surface))
		})
	}
}

func (p *Pipeline) handleNativeUnsupported(surface video.NativeSurface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeNative(w, surface, video.NativeRoute{}, http.StatusNotFound,
			string(errcode.GatewayResourceNotFound),
			video.ErrNativeRouteUnsupported{Path: r.URL.Path}.Error()+
				"; the same models are reachable on POST /v1/videos")
	}
}

func (p *Pipeline) handleNative(surface video.NativeSurface, route video.NativeRoute) http.HandlerFunc {
	switch route.Kind {
	case video.NativeSubmit:
		return p.handleNativeSubmit(surface, route)
	case video.NativePoll, video.NativeArtifact:
		// Both are reads. The artifact route is one on this plane because the
		// vendor that has it answers a file *record* naming where the bytes
		// are, not the bytes -- so it renders like any other job, with this
		// deployment's own address where the vendor's URL goes.
		return p.handleNativeRead(surface, route)
	default:
		return p.handleNativeUnsupported(surface)
	}
}

func (p *Pipeline) handleNativeSubmit(
	surface video.NativeSurface, route video.NativeRoute,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authenticated before anything reaches the database.
		//
		// The normalised plane can decode first because decoding is pure. This
		// surface cannot: it has to resolve the vendor's own model name against
		// the catalogue, and doing that for an unauthenticated caller would
		// both spend a query on them and tell them which upstream names this
		// deployment wires. Admission authenticates again a few lines down --
		// a hash and a cache read, which is the right price for not answering
		// that question to anyone who asks.
		if _, gerr := p.videoCaller(r); gerr != nil {
			writeNativeError(w, surface, route, gerr)
			return
		}
		body, gerr := readNativeBody(w, r)
		if gerr != nil {
			writeNativeError(w, surface, route, gerr)
			return
		}
		req, _, err := surface.Decode(video.NativeRequest{
			Route: route, Body: body,
			Path: chiParams(r), Query: flatQuery(r),
		})
		if err != nil {
			writeNativeError(w, surface, route,
				NewError(errcode.GatewayInvalidRequest, err.Error()))
			return
		}

		// The caller sent this vendor's own model name; admission resolves by
		// catalog slug. Joining the two is the one piece of work this surface
		// does that the normalised plane does not.
		slug, gerr := p.resolveNativeModel(r.Context(), surface.Vendor(), req.Model)
		if gerr != nil {
			writeNativeError(w, surface, route, gerr)
			return
		}
		req.Model = slug

		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)
		in := Request{
			Surface: catalog.SurfaceVideo, Protocol: ProtocolVideo,
			Body: body, Model: req.Model,
			Credential: CredentialOf(r), EndUserID: r.Header.Get("X-End-User-Id"),
			RequestID: requestID,
		}
		fingerprint := requestFingerprint(body)
		key := nativeIdempotencyKey(
			surface.Vendor(), r.URL.Path, in.EndUserID, fingerprint, time.Now())
		job, _, gerr := p.admitAndSubmitVideo(r.Context(), in, req, surface.Vendor(), key, fingerprint)
		if gerr != nil {
			writeNativeError(w, surface, route, gerr)
			return
		}
		// A replay is answered exactly like a fresh submit here. These APIs have
		// no second success shape to say "this one already existed", and
		// inventing one would be a field no client reads.
		writeNativeJob(w, r, p, surface, route, job)
	}
}

func (p *Pipeline) handleNativeRead(
	surface video.NativeSurface, route video.NativeRoute,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.videoCaller(r)
		if gerr != nil {
			writeNativeError(w, surface, route, gerr)
			return
		}
		in := video.NativeRequest{Route: route, Path: chiParams(r), Query: flatQuery(r)}
		job, gerr := p.nativeJobFor(r.Context(), id.OrgID, route, route.IDFrom(in))
		if gerr != nil {
			writeNativeError(w, surface, route, gerr)
			return
		}
		writeNativeJob(w, r, p, surface, route, job)
	}
}

// nativeJobFor reads one job by whichever identifier this route carries.
//
// Two shapes, because the vendors publish two: most hand back an opaque string,
// and one types its file identifier as int64. Both are narrowed to the caller's
// organization by the query itself -- an identifier from elsewhere has to be
// indistinguishable from one that never existed, and that matters more for the
// integer, which is far easier to guess than a UUID.
func (p *Pipeline) nativeJobFor(
	ctx context.Context, orgID pgtype.UUID, route video.NativeRoute, raw string,
) (gwdb.GatewayAsyncJob, *Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayInvalidRequest,
			"the request names no task")
	}
	if !route.IDAlias {
		return p.VideoJobs().Get(ctx, orgID, raw)
	}
	alias, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayResourceNotFound, "Video job not found")
	}
	job, err := p.gw.GetVideoJobByAlias(ctx, gwdb.GetVideoJobByAliasParams{
		NativeAlias: alias, OrgID: orgID,
	})
	if err != nil {
		return gwdb.GatewayAsyncJob{}, NewError(errcode.GatewayResourceNotFound, "Video job not found")
	}
	return job, nil
}

// resolveNativeModel turns the vendor's own model name into a catalog slug.
//
// A name already in slug form is taken as one. That is not a convenience: this
// surface renders the slug back as the model on every response, so a client
// that stores what it got and submits it again has to be able to. Admission
// still decides whether the model exists and whether this caller may use it.
//
// More than one match is refused rather than resolved. Two catalog models wired
// to the same upstream name on the same vendor have different rate cards, and
// picking one would charge against a card nobody chose -- the same reason the
// price import refuses routes that disagree.
func (p *Pipeline) resolveNativeModel(
	ctx context.Context, vendor, name string,
) (string, *Error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", NewError(errcode.GatewayInvalidRequest, "the request names no model")
	}
	slugs, err := p.gw.ResolveModelsByUpstreamID(ctx, gwdb.ResolveModelsByUpstreamIDParams{
		Vendor: vendor, ProviderModelID: name, Protocol: string(ProtocolVideo),
	})
	if err != nil {
		return "", NewError(errcode.GatewayInternal, "The model could not be resolved")
	}
	switch len(slugs) {
	case 0:
		// A name in slug form is taken as one, because this surface renders the
		// slug back as the model and a client that stores it has to be able to
		// send it again. Whether that slug is actually served by this vendor is
		// not decided here -- admission narrows the candidates to this vendor's
		// routes and refuses if there are none, which is the same answer
		// reached where the routes are already loaded.
		if strings.Contains(name, "/") {
			return name, nil
		}
		return "", NewError(errcode.GatewayModelNotFound,
			"no model on this gateway is wired to "+name+" on "+vendor)
	case 1:
		return slugs[0], nil
	default:
		return "", NewError(errcode.GatewayInvalidRequest,
			"more than one model here is wired to "+name+" on "+vendor+" ("+
				strings.Join(slugs, ", ")+"); name the one you mean")
	}
}

// nativeIdempotencyKey derives the key these APIs have no header for.
//
// The bucket is floored rather than rolling, so that two requests a moment
// apart land in the same one unless they straddle a boundary. A rolling window
// would need the earlier key to be found, which is the thing being derived.
func nativeIdempotencyKey(vendor, path, endUser, fingerprint string, now time.Time) string {
	bucket := now.UTC().Truncate(nativeIdempotencyWindow).Unix()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%d",
		vendor, path, endUser, fingerprint, bucket)))
	return "native-" + hex.EncodeToString(sum[:])
}

func writeNativeJob(
	w http.ResponseWriter, r *http.Request, p *Pipeline,
	surface video.NativeSurface, route video.NativeRoute, job gwdb.GatewayAsyncJob,
) {
	status, body, err := surface.Render(route, p.nativeJobOf(r, job))
	if err != nil {
		// A job that produced nothing is the caller's answer, not a fault of
		// ours. Rendering it as an internal error would send a client's retry
		// logic round again on a job that will never have a file.
		if errors.Is(err, video.ErrNativeNoArtifact) {
			writeNativeError(w, surface, route,
				NewError(errcode.GatewayJobNotReady, "This job has not produced a video"))
			return
		}
		writeNativeError(w, surface, route,
			NewError(errcode.GatewayInternal, "The job could not be rendered"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeNativeError(
	w http.ResponseWriter, surface video.NativeSurface, route video.NativeRoute, e *Error,
) {
	writeNative(w, surface, route, statusOf(e.Code), string(e.Code), e.Message)
}

func writeNative(
	w http.ResponseWriter, surface video.NativeSurface, route video.NativeRoute,
	httpStatus int, code, message string,
) {
	status, body := surface.RenderError(route, httpStatus, code, message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// nativeJobOf renders a job row as the plain value a surface reads.
//
// The model is this gateway's catalog slug rather than the name the caller
// sent. It is the name the job actually ran under here, resolveNativeModel
// accepts it back, and the row does not carry the caller's string -- three
// reasons pointing the same way.
func (p *Pipeline) nativeJobOf(r *http.Request, job gwdb.GatewayAsyncJob) video.NativeJob {
	out := video.NativeJob{
		ID:             VideoJobID(job),
		Alias:          job.NativeAlias,
		Model:          job.ModelSlug,
		Status:         video.Status(job.Status),
		UpstreamStatus: job.UpstreamStatus,
		Progress:       int(job.Progress),
		ErrorCode:      job.ErrorCode,
		ErrorMessage:   job.ErrorMessage,
		CreatedAt:      job.CreatedAt.Time,
		UpdatedAt:      job.UpdatedAt.Time,
	}
	detail := p.VideoJobs().Detail(r.Context(), job)
	out.DurationSeconds, out.Resolution = detail.Params.DurationSeconds, detail.Params.Resolution
	if job.Status == string(video.StatusCompleted) {
		out.ContentURL = videoContentURL(r, job)
	}
	return out
}

// videoContentURL is this deployment's own address for a finished job's bytes.
//
// Built from the request's own host because this deployment has no configured
// public address: it sits behind a reverse proxy that terminates whatever
// hostname the operator chose, and the one hostname certain to work for this
// caller is the one they just reached. The forwarded scheme is honoured for the
// same reason -- inside the proxy every request is plain HTTP, and a link built
// from that would be an http:// URL to an https:// deployment.
func videoContentURL(r *http.Request, job gwdb.GatewayAsyncJob) string {
	scheme := "http"
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v1/videos/" + VideoJobID(job) + "/content"
}

func readNativeBody(w http.ResponseWriter, r *http.Request) ([]byte, *Error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxVideoRequestBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, NewError(errcode.GatewayRequestTooLarge, "Request body is too large")
		}
		return nil, NewError(errcode.GatewayInvalidRequest, "Failed to read the request body")
	}
	return body, nil
}

func chiParams(r *http.Request) map[string]string {
	out := map[string]string{}
	if ctx := chi.RouteContext(r.Context()); ctx != nil {
		for i, k := range ctx.URLParams.Keys {
			if i < len(ctx.URLParams.Values) {
				out[k] = ctx.URLParams.Values[i]
			}
		}
	}
	return out
}

func flatQuery(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
