package proxy

import (
	"encoding/json"
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

func (p *Pipeline) handleResponseResource(path, method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resourceID := chi.URLParam(r, "response_id")
		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)
		id, model, gerr := p.resourceModel(r.Context(), CredentialOf(r), ProtocolOpenAI, resourceResponse, resourceID)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		res, gerr := p.RunUtility(r.Context(), Request{
			Surface: catalog.SurfaceResponsesResources, Protocol: ProtocolOpenAI,
			UpstreamPath: path, Method: method, Resource: resourceID, Model: model,
			Credential: CredentialOf(r), EndUserID: r.Header.Get("X-End-User-Id"),
			RequestID: requestID, UpstreamQuery: responseResourceQuery(r.URL.Query()),
		})
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		if method == http.MethodDelete {
			p.deleteAffinity(r.Context(), id.OrgID, ProtocolOpenAI, resourceResponse, resourceID)
		}
		writeProxyResult(w, r, res)
	}
}

func responseResourceQuery(query url.Values) map[string]string {
	allowed := map[string]bool{
		"after": true, "before": true, "include": true, "limit": true, "order": true,
	}
	out := make(map[string]string)
	for key, values := range query {
		if allowed[key] && len(values) > 0 {
			out[key] = values[len(values)-1]
		}
	}
	return out
}

func (p *Pipeline) handleInteractionCreate() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Failed to read the request body"))
			return
		}
		var probe struct {
			Agent      json.RawMessage `json:"agent"`
			Background bool            `json:"background"`
		}
		if json.Unmarshal(body, &probe) != nil {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Invalid JSON request body"))
			return
		}
		if len(probe.Agent) > 0 && string(probe.Agent) != "null" {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Managed agent interactions are not supported; use a model interaction"))
			return
		}
		// Background inference needs a hold that survives the initial HTTP call
		// and exact settlement on later retrieval. Until that worker exists,
		// refusing it is safer than charging an estimate as if it were final.
		if probe.Background {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Background interactions are not yet supported"))
			return
		}
		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)
		in := Request{
			Surface: catalog.SurfaceGeminiInteractions, Protocol: ProtocolGemini,
			UpstreamPath: catalog.PathGeminiInteractions, Body: body,
			Credential: CredentialOf(r), EndUserID: r.Header.Get("X-End-User-Id"),
			RequestID: requestID, Stream: StreamOf(body),
		}
		if in.Stream {
			if gerr := p.RunStream(r.Context(), w, in, SurfaceGemini); gerr != nil {
				Write(w, SurfaceGemini, gerr)
			}
			return
		}
		res, gerr := p.Run(r.Context(), in)
		if gerr != nil {
			Write(w, SurfaceGemini, gerr)
			return
		}
		writeProxyResult(w, r, res)
	}
}

func (p *Pipeline) handleInteractionResource() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := url.PathUnescape(chi.URLParam(r, "*"))
		if err != nil || raw == "" {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Invalid interaction id"))
			return
		}
		resourceID := strings.TrimSuffix(raw, "/cancel")
		path := catalog.PathGeminiInteraction
		method := r.Method
		if strings.HasSuffix(raw, "/cancel") {
			if r.Method != http.MethodPost || resourceID == "" {
				Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Invalid interaction operation"))
				return
			}
			path = catalog.PathGeminiInteractionCancel
		}
		requestID := httpx.NewRequestID()
		w.Header().Set(httpx.HeaderRequestID, requestID)
		id, model, gerr := p.resourceModel(r.Context(), CredentialOf(r), ProtocolGemini, resourceInteraction, resourceID)
		if gerr != nil {
			Write(w, SurfaceGemini, gerr)
			return
		}
		res, gerr := p.RunUtility(r.Context(), Request{
			Surface: catalog.SurfaceGeminiInteractions, Protocol: ProtocolGemini,
			UpstreamPath: path, Method: method, Resource: resourceID, Model: model,
			Credential: CredentialOf(r), EndUserID: r.Header.Get("X-End-User-Id"),
			RequestID: requestID,
		})
		if gerr != nil {
			Write(w, SurfaceGemini, gerr)
			return
		}
		if method == http.MethodDelete {
			p.deleteAffinity(r.Context(), id.OrgID, ProtocolGemini, resourceInteraction, resourceID)
		}
		writeProxyResult(w, r, res)
	}
}

func writeProxyResult(w http.ResponseWriter, r *http.Request, result Result) {
	if len(result.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(result.Status)
	if len(result.Body) > 0 {
		if _, err := w.Write(result.Body); err != nil {
			slog.ErrorContext(r.Context(), "dataplane: writing resource response failed", "error", err)
		}
	}
}
