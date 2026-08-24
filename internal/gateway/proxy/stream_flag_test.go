package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The usage row's stream column follows the entry point taken, not what the
// caller claimed.
//
// Request.Stream is now the single source for both that column and the metric
// label, replacing a value each settle path set for itself. A single source is
// only an improvement if it is always right, and the way it goes wrong is a
// caller whose field disagrees with the method it called. Both entry points
// therefore normalise on the way in, and both directions are pinned here --
// the buffered one because a lying caller is the realistic mistake, the
// streaming one because nothing else in the suite asserts this column and the
// hardcoded `true` that used to guarantee it is gone.
func TestUsageRowStreamColumnFollowsTheEntryPoint(t *testing.T) {
	t.Run("buffered path records stream=false even when the caller says true", func(t *testing.T) {
		f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		})
		ctx := context.Background()
		plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
		f.topup(t, org, 1_000_000_000)
		f.seedCatalogAt(t, f.upstream.URL, "openai", "p-up", 10)
		f.seedModelWithRoutes(t, "openai/m", "openai", []string{"p-up"})

		if _, gerr := f.pipeline.Run(ctx, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/m","messages":[{"role":"user","content":"hi"}]}`),
			Credential:   plaintext, RequestID: "buffered-lying-caller",
			Stream: true, // the caller is wrong; the path taken is what counts
		}); gerr != nil {
			t.Fatalf("the request should have been served: %v", gerr)
		}
		assertStreamColumn(t, f, "buffered-lying-caller", false)
	})

	t.Run("streaming path records stream=true even when the caller omits it", func(t *testing.T) {
		f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		})
		ctx := context.Background()
		plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
		f.topup(t, org, 1_000_000_000)
		f.seedCatalogAt(t, f.upstream.URL, "openai", "p-up", 10)
		f.seedModelWithRoutes(t, "openai/m", "openai", []string{"p-up"})

		rec := httptest.NewRecorder()
		if gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
			Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: "/v1/chat/completions",
			Body:         []byte(`{"model":"openai/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
			Credential:   plaintext, RequestID: "streamed-silent-caller",
			// Stream deliberately left false.
		}, proxy.SurfaceOpenAI); gerr != nil {
			t.Fatalf("the request should have been served: %v", gerr)
		}
		assertStreamColumn(t, f, "streamed-silent-caller", true)
	})
}

func assertStreamColumn(t *testing.T, f *pipeFixture, requestID string, want bool) {
	t.Helper()
	var got bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT stream FROM usage_logs WHERE request_id = $1`, requestID).Scan(&got); err != nil {
		t.Fatalf("reading the usage row for %s: %v", requestID, err)
	}
	if got != want {
		t.Errorf("usage_logs.stream for %s: got %v, want %v", requestID, got, want)
	}
}
