package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The dataplane endpoints. The proxying endpoints use plain http.Handlers
// rather than the generated strict server the management APIs use: the request
// body has to pass through byte for byte, and a generated strongly typed
// handler would drop any field the gateway does not know about on the round
// trip.

// maxRequestBody caps a dataplane request body. The shared body-limit
// middleware is not mounted on this plane, so the limit is applied here and an
// overrun renders as the surface's own native 413.
const maxRequestBody = 8 << 20 // 8 MiB

// endpoint describes one proxying endpoint's surface and upstream path.
type endpoint struct {
	surface  catalog.Surface
	protocol Protocol
	path     string
	utility  bool // audited and rate-limited, but never placed on a billing hold
}

// The upstream paths come from the catalog's own list rather than being spelled
// out again here. That list is also the key space a provider's transport
// profile may override, and the two have to be the same set: a path this table
// can send but that list does not name would be unoverridable, and one named
// there but never sent would be an override that silently does nothing.
var endpoints = map[string]endpoint{
	"/chat/completions":       {surface: catalog.SurfaceChat, protocol: ProtocolOpenAI, path: catalog.PathChat},
	"/responses":              {surface: catalog.SurfaceResponses, protocol: ProtocolOpenAI, path: catalog.PathResponses},
	"/responses/compact":      {surface: catalog.SurfaceResponsesCompact, protocol: ProtocolOpenAI, path: catalog.PathResponsesCompact},
	"/responses/input_tokens": {surface: catalog.SurfaceResponsesInputTokens, protocol: ProtocolOpenAI, path: catalog.PathResponsesInputTokens, utility: true},
	"/embeddings":             {surface: catalog.SurfaceEmbeddings, protocol: ProtocolOpenAI, path: catalog.PathEmbeddings},
	"/messages":               {surface: catalog.SurfaceMessages, protocol: ProtocolAnthropic, path: catalog.PathMessages},
	"/messages/count_tokens":  {surface: catalog.SurfaceMessagesCountTokens, protocol: ProtocolAnthropic, path: catalog.PathMessagesCountTokens, utility: true},
	"/images/generations":     {surface: catalog.SurfaceImages, protocol: ProtocolOpenAI, path: catalog.PathImagesGenerate},
}

// imageEditEndpoint is listed separately: it is multipart and takes a different
// path from the JSON endpoints.
var imageEditEndpoint = endpoint{surface: catalog.SurfaceImagesEdit, protocol: ProtocolOpenAI, path: catalog.PathImagesEdit}

// Mount registers the proxying endpoints on the dataplane subrouter.
//
// The Gemini route is a wildcard rather than a path parameter because the model
// is a whole path segment *and* this gateway's catalogue names models with a
// slash in them (openai/gpt-4o); a `{model}` parameter would stop at the first
// slash and refuse a name the catalogue itself issued. The method name is
// appended to that segment after a colon, so the segment is split rather than
// routed on.
func (p *Pipeline) Mount(r chi.Router) {
	for route, ep := range endpoints {
		r.Post(route, p.handle(ep))
	}
	r.Post("/images/edits", p.handleImageEdit())
	r.Get("/responses/{response_id}", p.handleResponseResource(catalog.PathResponsesResource, http.MethodGet))
	r.Delete("/responses/{response_id}", p.handleResponseResource(catalog.PathResponsesResource, http.MethodDelete))
	r.Post("/responses/{response_id}/cancel", p.handleResponseResource(catalog.PathResponsesCancel, http.MethodPost))
	r.Get("/responses/{response_id}/input_items", p.handleResponseResource(catalog.PathResponsesInputItems, http.MethodGet))
}

// MountGemini registers only Gemini-native endpoints.
//
// It is separate from Mount because that protocol's clients default to a
// different version prefix, and the prefix must not carry the whole data
// plane: mounting everything under it would put the OpenAI-shaped
// catalogue on GET /v1beta/models, where a Gemini client asks for its model list
// and would read a 200 of the wrong shape as an empty catalogue.
func (p *Pipeline) MountGemini(r chi.Router) {
	r.Get("/models", p.handleGeminiModels())
	r.Get("/models/*", p.handleGeminiModel())
	r.Post("/models/*", p.handleGemini())
	r.Post("/interactions", p.handleInteractionCreate())
	r.Get("/interactions/*", p.handleInteractionResource())
	r.Post("/interactions/*", p.handleInteractionResource())
	r.Delete("/interactions/*", p.handleInteractionResource())
}

// handleImageEdit handles a multipart image edit. The body is not buffered
// whole: it is read only as far as the model field and the rest is streamed
// through. See multipart.go for the reasoning.
func (p *Pipeline) handleImageEdit() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ep := imageEditEndpoint

		// Images are large, so this limit is wider than the JSON endpoints'.
		body := http.MaxBytesReader(w, r.Body, maxImageRequestBody)

		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)

		res, gerr := p.RunImageEdit(ctx, Request{
			Surface:      ep.surface,
			Protocol:     ep.protocol,
			UpstreamPath: ep.path,
			Credential:   CredentialOf(r),
			EndUserID:    r.Header.Get("X-End-User-Id"),
			RequestID:    requestID,
		}, r.Header.Get("Content-Type"), body)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		if _, err := w.Write(res.Body); err != nil {
			slog.ErrorContext(ctx, "dataplane: writing the image response failed", "error", err)
		}
	}
}

// maxImageRequestBody caps an image endpoint's request body; images are large
// to begin with.
const maxImageRequestBody = 64 << 20 // 64 MiB

// errorSurfaceFor is the shape a refusal takes: a caller speaking one protocol
// must be answered in that protocol's error format, whatever went wrong.
func errorSurfaceFor(protocol Protocol) Surface {
	switch protocol {
	case ProtocolAnthropic:
		return SurfaceAnthropic
	case ProtocolGemini:
		return SurfaceGemini
	default:
		return SurfaceOpenAI
	}
}

func (p *Pipeline) handle(ep endpoint) http.HandlerFunc {
	surface := errorSurfaceFor(ep.protocol)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if ok := asMaxBytes(err, &mbe); ok {
				Write(w, surface, NewError(errcode.GatewayRequestTooLarge, "Request body exceeds the limit"))
				return
			}
			Write(w, surface, NewError(errcode.GatewayInvalidRequest, "Failed to read the request body"))
			return
		}

		// The dataplane overwrites the generic middleware's request id with its
		// own. The API contract promises that X-Request-Id equals the usage
		// log's request id, which is what a support lookup goes on; when the
		// two were generated independently, the one the customer held could not
		// be found in the database at all. It has to be set before the first
		// byte goes out -- once a stream is open the response headers can no
		// longer be changed.
		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)

		req := Request{
			Surface:      ep.surface,
			Protocol:     ep.protocol,
			UpstreamPath: ep.path,
			Body:         body,
			Credential:   CredentialOf(r),
			EndUserID:    r.Header.Get("X-End-User-Id"),
			RequestID:    requestID,
			Stream:       StreamOf(body),
		}

		if req.Stream {
			// Streaming: the response is sent as it arrives. Once the first
			// byte is out the HTTP status is fixed and an error can only be
			// conveyed inside the stream, so this *Error is meaningful only
			// before that first byte.
			if gerr := p.RunStream(ctx, w, req, surface); gerr != nil {
				Write(w, surface, gerr)
			}
			return
		}

		var res Result
		var gerr *Error
		if ep.utility {
			res, gerr = p.RunUtility(ctx, req)
		} else {
			res, gerr = p.Run(ctx, req)
		}
		if gerr != nil {
			Write(w, surface, gerr)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		if _, err := w.Write(res.Body); err != nil {
			slog.ErrorContext(ctx, "dataplane: writing the response failed", "error", err)
		}
	}
}

// asMaxBytes reports whether this is a body-too-large error.
func asMaxBytes(err error, target **http.MaxBytesError) bool {
	for e := err; e != nil; {
		if mbe, ok := e.(*http.MaxBytesError); ok {
			*target = mbe
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// The Gemini protocol's method names, which sit after a colon on the model
// segment rather than on a path of their own.
const (
	methodGenerateContent       = "generateContent"
	methodStreamGenerateContent = "streamGenerateContent"
	methodCountTokens           = "countTokens"
	methodEmbedContent          = "embedContent"
	methodBatchEmbedContents    = "batchEmbedContents"
)

// geminiEndpoints is the same kind of table as `endpoints` above, keyed by
// method name because that is where this protocol puts the distinction. Written
// down rather than branched on inline so that "every path this gateway can send"
// stays answerable by reading tables -- the check that a path can also be
// overridden reads exactly these.
var geminiEndpoints = map[string]endpoint{
	methodGenerateContent: {
		surface: catalog.SurfaceGenerateContent, protocol: ProtocolGemini, path: catalog.PathGenerateContent,
	},
	methodStreamGenerateContent: {
		surface: catalog.SurfaceGenerateContent, protocol: ProtocolGemini, path: catalog.PathStreamGenerateContent,
	},
	methodCountTokens: {
		surface: catalog.SurfaceGeminiCountTokens, protocol: ProtocolGemini, path: catalog.PathGeminiCountTokens, utility: true,
	},
	methodEmbedContent: {
		surface: catalog.SurfaceGeminiEmbedContent, protocol: ProtocolGemini, path: catalog.PathGeminiEmbedContent,
	},
	methodBatchEmbedContents: {
		surface: catalog.SurfaceGeminiBatchEmbedContents, protocol: ProtocolGemini, path: catalog.PathGeminiBatchEmbedContents,
	},
}

// handleGemini serves POST /models/<model>:<method>.
//
// Two things differ from every other endpoint, and both are properties of the
// API rather than of any one platform: the model is in the address, and
// streaming is a different method name instead of a body flag. The handler
// therefore parses the address before it can say what was asked for.
//
// A streamed request must ask for SSE explicitly (`alt=sse`), which is how this
// API distinguishes a stream of events from a single JSON array delivered in
// pieces. The array form is refused rather than served: this gateway meters what
// it forwards, and it cannot read usage out of a shape it does not parse.
// Answering it as if it were SSE would be worse -- the client would see a
// well-formed stream of the wrong framing.
// parseGeminiAddress splits `<model>:<method>` out of the address.
//
// Two things make this more than a Split. chi hands back the *escaped* path
// whenever the request carried one, and percent-encoding is the correct way for
// a client to put a slash inside a single path segment -- which this gateway's
// catalogue names need, since it issues ids like openai/gpt-4o. Without
// decoding, the careful client is the one refused. And the split is at the
// *last* colon: the method never contains one, while a model id can (some
// platforms version theirs that way), so cutting at the first would take the
// version for a method name.
func parseGeminiAddress(raw string) (model, method string, ok bool) {
	seg, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", false
	}
	cut := strings.LastIndex(seg, ":")
	if cut <= 0 {
		return "", "", false
	}
	model, method = seg[:cut], seg[cut+1:]
	if model == "" || method == "" {
		return "", "", false
	}
	return model, method, true
}

func (p *Pipeline) handleGemini() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		model, method, ok := parseGeminiAddress(chi.URLParam(r, "*"))
		if !ok {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest,
				"The address must name a model and a method, as models/<model>:generateContent"))
			return
		}
		ep, served := geminiEndpoints[method]
		if !served {
			// A method this gateway does not serve is a 404 about the address,
			// not a 400 about the body: nothing was wrong with what was sent.
			Write(w, SurfaceGemini, NewError(errcode.GatewayModelNotFound,
				"Unsupported Gemini model method "+method))
			return
		}
		stream := method == methodStreamGenerateContent
		if stream && r.URL.Query().Get("alt") != "sse" {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest,
				"Streaming requires alt=sse; this gateway does not serve the JSON array form"))
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if ok := asMaxBytes(err, &mbe); ok {
				Write(w, SurfaceGemini, NewError(errcode.GatewayRequestTooLarge, "Request body exceeds the limit"))
				return
			}
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Failed to read the request body"))
			return
		}

		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)

		var query map[string]string
		if stream {
			// The upstream needs the same selector this request carried: it is
			// what makes the answer SSE rather than an array, and the outbound
			// URL is built from the profile rather than copied from the caller.
			query = map[string]string{"alt": "sse"}
		}
		req := Request{
			Surface:       ep.surface,
			Protocol:      ep.protocol,
			UpstreamPath:  ep.path,
			Body:          body,
			Credential:    CredentialOf(r),
			EndUserID:     r.Header.Get("X-End-User-Id"),
			RequestID:     requestID,
			Model:         model,
			UpstreamQuery: query,
			// Set at the same boundary that chooses the entry point, so the
			// recorded fact and the dispatch cannot disagree. On this protocol
			// it comes from the method name rather than from a body flag.
			Stream: stream,
		}

		if stream {
			if gerr := p.RunStream(ctx, w, req, SurfaceGemini); gerr != nil {
				Write(w, SurfaceGemini, gerr)
			}
			return
		}

		var res Result
		var gerr *Error
		if ep.utility {
			res, gerr = p.RunUtility(ctx, req)
		} else {
			res, gerr = p.Run(ctx, req)
		}
		if gerr != nil {
			Write(w, SurfaceGemini, gerr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		if _, err := w.Write(res.Body); err != nil {
			slog.ErrorContext(ctx, "dataplane: writing the response failed", "error", err)
		}
	}
}
