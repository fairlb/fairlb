package proxy

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/foundation/errcode"
)

type geminiModel struct {
	Name                       string   `json:"name"`
	BaseModelID                string   `json:"baseModelId"`
	DisplayName                string   `json:"displayName"`
	InputTokenLimit            int32    `json:"inputTokenLimit"`
	OutputTokenLimit           int32    `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type geminiModelList struct {
	Models        []geminiModel `json:"models"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

func (p *Pipeline) handleGeminiModels() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		models, gerr := p.geminiModelsForRequest(r)
		if gerr != nil {
			Write(w, SurfaceGemini, gerr)
			return
		}

		pageSize := 50
		if raw := r.URL.Query().Get("pageSize"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 1000 {
				Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "pageSize must be between 1 and 1000"))
				return
			}
			pageSize = n
		}
		offset, ok := decodeGeminiPageToken(r.URL.Query().Get("pageToken"))
		if !ok || offset > len(models) {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Invalid pageToken"))
			return
		}
		end := min(offset+pageSize, len(models))
		out := geminiModelList{Models: models[offset:end]}
		if end < len(models) {
			out.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
		}
		writeGeminiJSON(w, out)
	}
}

func (p *Pipeline) handleGeminiModel() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		models, gerr := p.geminiModelsForRequest(r)
		if gerr != nil {
			Write(w, SurfaceGemini, gerr)
			return
		}
		raw, err := url.PathUnescape(chi.URLParam(r, "*"))
		if err != nil {
			Write(w, SurfaceGemini, NewError(errcode.GatewayInvalidRequest, "Invalid model name"))
			return
		}
		name := "models/" + strings.TrimPrefix(raw, "models/")
		for _, model := range models {
			if model.Name == name {
				writeGeminiJSON(w, model)
				return
			}
		}
		Write(w, SurfaceGemini, NewError(errcode.GatewayModelNotFound, "Model not found or unavailable"))
	}
}

func (p *Pipeline) geminiModelsForRequest(r *http.Request) ([]geminiModel, *Error) {
	ctx := r.Context()
	id, gerr := p.auth.Authenticate(ctx, CredentialOf(r))
	if gerr != nil {
		return nil, gerr
	}
	if gerr := RequireScope(id, "inference"); gerr != nil {
		return nil, gerr
	}
	if gerr := p.guard.CheckTier(id); gerr != nil {
		return nil, gerr
	}
	models, err := p.catalog.ModelsForOrg(ctx, id.ModelTierID, id.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: reading the Gemini organization catalogue failed", "error", err)
		return nil, NewError(errcode.GatewayInternal, "Internal server error")
	}
	models = filterByKeyAllowlist(id, models)
	out := make([]geminiModel, 0, len(models))
	for _, model := range models {
		// A model owns no protocol; it is in the Gemini catalogue when some
		// Gemini method has been verified on one of its routes. The methods
		// list below is that test, so no separate protocol check is needed.
		methods := geminiMethodsForEndpoints(model.Endpoints)
		if len(methods) == 0 {
			continue
		}
		out = append(out, geminiModel{
			Name: "models/" + model.Slug, BaseModelID: model.Slug,
			DisplayName: model.DisplayName, InputTokenLimit: model.ContextWindow,
			OutputTokenLimit: model.MaxOutputTokens, SupportedGenerationMethods: methods,
		})
	}
	slices.SortFunc(out, func(a, b geminiModel) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func geminiMethodsForEndpoints(endpoints []string) []string {
	mapping := []struct{ endpoint, method string }{
		{"generate_content", methodGenerateContent},
		{"gemini_count_tokens", methodCountTokens},
		{"gemini_embed_content", methodEmbedContent},
		{"gemini_batch_embed_contents", methodBatchEmbedContents},
	}
	out := make([]string, 0, len(mapping))
	for _, item := range mapping {
		if slices.Contains(endpoints, item.endpoint) {
			out = append(out, item.method)
		}
	}
	return out
}

func decodeGeminiPageToken(token string) (int, bool) {
	if token == "" {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, false
	}
	offset, err := strconv.Atoi(string(raw))
	return offset, err == nil && offset >= 0
}

func writeGeminiJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("dataplane: writing Gemini model catalog failed", "error", err)
	}
}
