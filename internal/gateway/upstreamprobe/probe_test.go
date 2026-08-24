package upstreamprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

func TestSpecForEndpointCoversEveryProbeSurface(t *testing.T) {
	tests := map[string]string{
		"chat": catalog.PathChat, "messages": catalog.PathMessages,
		"responses": catalog.PathResponses, "embeddings": catalog.PathEmbeddings,
		"generate_content": catalog.PathGenerateContent, "images": catalog.PathImagesGenerate,
		"responses_compact":           catalog.PathResponsesCompact,
		"responses_input_tokens":      catalog.PathResponsesInputTokens,
		"messages_count_tokens":       catalog.PathMessagesCountTokens,
		"gemini_count_tokens":         catalog.PathGeminiCountTokens,
		"gemini_embed_content":        catalog.PathGeminiEmbedContent,
		"gemini_batch_embed_contents": catalog.PathGeminiBatchEmbedContents,
		"gemini_interactions":         catalog.PathGeminiInteractions,
	}
	for endpoint, wantPath := range tests {
		t.Run(endpoint, func(t *testing.T) {
			spec, ok := SpecForEndpoint(endpoint, "model-x")
			if !ok || spec.Path != wantPath {
				t.Fatalf("spec = %#v, %v; want path %q", spec, ok, wantPath)
			}
			var body map[string]any
			if err := json.Unmarshal(spec.Body, &body); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(endpoint, "gemini_") && endpoint != "generate_content" && body["model"] != "model-x" {
				t.Fatalf("model was not encoded: %s", spec.Body)
			}
			if endpoint == "messages_count_tokens" {
				if _, present := body["max_tokens"]; present {
					t.Fatalf("count_tokens does not accept the generation-only max_tokens field: %s", spec.Body)
				}
			}
		})
	}
}

func TestRunSharesAuthenticationTraceAndErrorBounds(t *testing.T) {
	var auth, version string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("X-Api-Key")
		version = r.Header.Get("Anthropic-Version")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("x", maxErrorBody*2)))
	}))
	defer upstream.Close()
	spec, _ := SpecForEndpoint("messages", "claude-test")
	result := Run(context.Background(), Input{
		Client: upstream.Client(), Spec: spec, BaseURL: upstream.URL,
		APIKey: "secret", Model: "claude-test", CaptureTrace: true,
	})
	if result.OK || result.StatusCode == nil || *result.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected result: %+v", result)
	}
	if auth != "secret" || version == "" {
		t.Fatalf("authentication headers = %q, %q", auth, version)
	}
	if len(result.Message) > maxErrorBody+80 {
		t.Fatalf("error body was not bounded: %d", len(result.Message))
	}
	if result.Trace == nil || result.Trace.Request == "" || result.Trace.Response == nil {
		t.Fatalf("trace not captured: %+v", result.Trace)
	}
}

func TestRunHonorsTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	spec, _ := SpecForEndpoint("chat", "gpt-test")
	result := Run(context.Background(), Input{
		Client: upstream.Client(), Spec: spec, BaseURL: upstream.URL,
		APIKey: "secret", Model: "gpt-test", Timeout: 10 * time.Millisecond,
	})
	if result.OK || !strings.Contains(result.Message, "deadline exceeded") {
		t.Fatalf("timeout result = %+v", result)
	}
}
