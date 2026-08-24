package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

func TestAgentOperationsAreMountedOnTheirNativeVersionPlanes(t *testing.T) {
	p := &Pipeline{}
	v1 := chi.NewRouter()
	v1beta := chi.NewRouter()
	p.Mount(v1)
	p.MountGemini(v1beta)

	wantV1 := []string{
		"POST /responses/compact", "POST /responses/input_tokens",
		"POST /messages/count_tokens", "GET /responses/{response_id}",
		"DELETE /responses/{response_id}", "POST /responses/{response_id}/cancel",
		"GET /responses/{response_id}/input_items",
	}
	wantBeta := []string{
		"GET /models", "GET /models/*", "POST /models/*",
		"POST /interactions", "GET /interactions/*", "POST /interactions/*",
		"DELETE /interactions/*",
	}
	assertRoutes := func(router chi.Router, want []string) {
		t.Helper()
		var got []string
		if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			got = append(got, method+" "+route)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, route := range want {
			if !slices.Contains(got, route) {
				t.Errorf("route %s is not mounted; got %v", route, got)
			}
		}
	}
	assertRoutes(v1, wantV1)
	assertRoutes(v1beta, wantBeta)
}

func TestGeminiCatalogMethodsComeOnlyFromVerifiedRouteOperations(t *testing.T) {
	got := geminiMethodsForEndpoints([]string{
		"chat", "generate_content", "gemini_count_tokens", "gemini_embed_content",
	})
	want := []string{methodGenerateContent, methodCountTokens, methodEmbedContent}
	if !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

func TestGeminiBatchEmbeddingRewritesOnlyAddressingFields(t *testing.T) {
	in := []byte(`{"requests":[{"model":"models/local/slug","content":{"parts":[{"text":"hi"}]},"unknown":9007199254740993}],"top_unknown":true}`)
	out, err := RewriteRequest(catalog.SurfaceGeminiBatchEmbedContents, in, "upstream-embed", false, catalog.Transport{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Requests []map[string]any `json:"requests"`
		Top      bool             `json:"top_unknown"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Requests[0]["model"]; got != "models/upstream-embed" {
		t.Fatalf("nested model = %v", got)
	}
	if got := doc.Requests[0]["unknown"].(json.Number).String(); got != "9007199254740993" || !doc.Top {
		t.Fatalf("unknown fields were not preserved: %s, %v", got, doc.Top)
	}
}

func TestUtilityUsageIsAuditableWithoutInventingOutput(t *testing.T) {
	tests := []struct {
		surface catalog.Surface
		body    string
		wantIn  int64
		cached  int64
	}{
		{catalog.SurfaceMessagesCountTokens, `{"input_tokens":42}`, 42, 0},
		{catalog.SurfaceGeminiCountTokens, `{"totalTokens":42,"cachedContentTokenCount":7}`, 35, 7},
		{catalog.SurfaceResponsesInputTokens, `{"input_tokens":42}`, 42, 0},
	}
	for _, tt := range tests {
		u := parseUtilityUsage(tt.surface, []byte(tt.body))
		if !u.Present || u.In != tt.wantIn || u.CachedRead != tt.cached || u.Out != 0 {
			t.Errorf("%s usage = %+v", tt.surface, u)
		}
	}
}

func TestGeminiBatchEmbeddingUsageAggregatesItemsWithoutDoubleCounting(t *testing.T) {
	perItem := ParseUsage(catalog.SurfaceGeminiBatchEmbedContents, []byte(`{
		"embeddings":[
			{"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":2}},
			{"usageMetadata":{"promptTokenCount":7}}
		]
	}`))
	if !perItem.Present || perItem.In != 15 || perItem.CachedRead != 2 {
		t.Fatalf("per-item aggregate = %+v", perItem)
	}

	topLevel := ParseUsage(catalog.SurfaceGeminiBatchEmbedContents, []byte(`{
		"usageMetadata":{"promptTokenCount":20},
		"embeddings":[{"usageMetadata":{"promptTokenCount":20}}]
	}`))
	if !topLevel.Present || topLevel.In != 20 {
		t.Fatalf("top-level aggregate was double-counted: %+v", topLevel)
	}
}

func TestGeminiEmbeddingUsesTotalTokenCountWhenNoGenerationBreakdownExists(t *testing.T) {
	usage := ParseUsage(catalog.SurfaceGeminiEmbedContent, []byte(`{
		"usageMetadata":{"totalTokenCount":13,"cachedContentTokenCount":3}
	}`))
	if !usage.Present || usage.In != 10 || usage.CachedRead != 3 || usage.Out != 0 {
		t.Fatalf("embedding usage = %+v", usage)
	}
}

func TestGeminiInteractionBillsSeparateThoughtAndToolUseTokens(t *testing.T) {
	usage := ParseUsage(catalog.SurfaceGeminiInteractions, []byte(`{
		"service_tier":"priority",
		"usage":{
			"total_input_tokens":100,
			"total_cached_tokens":20,
			"total_output_tokens":25,
			"total_thought_tokens":7,
			"total_tool_use_tokens":5,
			"input_tokens_by_modality":[{"modality":"audio","tokens":12}],
			"output_tokens_by_modality":[{"modality":"audio","tokens":3}],
			"tool_use_tokens_by_modality":[{"modality":"audio","tokens":2}],
			"grounding_tool_count":[{"type":"google_search","count":2}]
		}
	}`))
	if !usage.Present || usage.In != 85 || usage.CachedRead != 20 || usage.Out != 32 ||
		usage.Reasoning != 7 || usage.AudioIn != 14 || usage.AudioOut != 3 ||
		usage.ServiceTier != "priority" || usage.ToolCalls["google_search"] != 2 {
		t.Fatalf("interaction usage = %+v", usage)
	}
}

func TestResponsesCompactUsesResponsesUsageShape(t *testing.T) {
	usage := ParseUsage(catalog.SurfaceResponsesCompact, []byte(`{
		"usage":{
			"input_tokens":12,
			"input_tokens_details":{"cached_tokens":2},
			"output_tokens":4,
			"output_tokens_details":{"reasoning_tokens":1}
		}
	}`))
	if !usage.Present || usage.In != 10 || usage.CachedRead != 2 || usage.Out != 4 || usage.Reasoning != 1 {
		t.Fatalf("compact usage = %+v", usage)
	}
}

func TestAffinityReferenceAndStorageOptOut(t *testing.T) {
	kind, id := affinityReference(Request{
		Surface: catalog.SurfaceResponses, Body: []byte(`{"previous_response_id":"resp_1"}`),
	})
	if kind != resourceResponse || id != "resp_1" {
		t.Fatalf("response affinity = %q, %q", kind, id)
	}
	kind, id = affinityReference(Request{
		Surface: catalog.SurfaceResponsesCompact, Body: []byte(`{"previous_response_id":"resp_compact"}`),
	})
	if kind != resourceResponse || id != "resp_compact" {
		t.Fatalf("compact affinity = %q, %q", kind, id)
	}
	kind, id = affinityReference(Request{
		Surface: catalog.SurfaceResponsesInputTokens, Body: []byte(`{"previous_response_id":"resp_count"}`),
	})
	if kind != resourceResponse || id != "resp_count" {
		t.Fatalf("input-token affinity = %q, %q", kind, id)
	}
	kind, id = affinityReference(Request{
		Surface: catalog.SurfaceGeminiInteractions, Resource: "int_1",
	})
	if kind != resourceInteraction || id != "int_1" {
		t.Fatalf("interaction affinity = %q, %q", kind, id)
	}
	if requestStoresResource([]byte(`{"store":false}`)) || !requestStoresResource([]byte(`{"store":true}`)) || !requestStoresResource([]byte(`{}`)) {
		t.Fatal("store tri-state was not respected")
	}
}

func TestStreamShadowParserCapturesStatefulIDs(t *testing.T) {
	var acc usageAccumulator
	var text bytes.Buffer
	acc.consume(catalog.SurfaceResponses, []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\"}}\n\n"), &text)
	if acc.resourceID != "resp_stream" {
		t.Fatalf("response id = %q", acc.resourceID)
	}
	acc.consume(catalog.SurfaceGeminiInteractions, []byte("data: {\"interaction\":{\"id\":\"int_stream\",\"usage\":{\"total_input_tokens\":3,\"total_output_tokens\":2}}}\n\n"), &text)
	got := acc.result()
	if acc.resourceID != "int_stream" || !got.Present || got.In != 3 || got.Out != 2 {
		t.Fatalf("interaction stream = %q, %+v", acc.resourceID, acc.result())
	}
}

func TestInteractionStreamReadsObjectDeltaAndCumulativeMetadataUsage(t *testing.T) {
	var acc usageAccumulator
	var text bytes.Buffer
	acc.consume(catalog.SurfaceGeminiInteractions, []byte("data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"hello\"},\"metadata\":{\"total_usage\":{\"total_input_tokens\":10,\"total_output_tokens\":4,\"total_thought_tokens\":2}}}\n\n"), &text)
	usage := acc.result()
	if text.String() != "hello" || !usage.Present || usage.In != 10 || usage.Out != 6 || usage.Reasoning != 2 {
		t.Fatalf("interaction stream text=%q usage=%+v", text.String(), usage)
	}
}

func TestResourcePlaceholderIsEscapedAsOneSegment(t *testing.T) {
	req, err := BuildRequest(context.Background(), Target{
		Protocol: ProtocolOpenAI, BaseURL: "https://up.example", APIKey: "k",
		Path: catalog.PathResponsesResource, Resource: "resp:one",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.Path, "/v1/responses/resp:one"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
