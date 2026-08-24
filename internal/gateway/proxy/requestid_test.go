package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/httpx"
)

// The API contract promises that X-Request-Id is the end-to-end request id and
// equals the usage log's request id, which is what a support lookup goes on.
//
// That promise was once broken: the generic middleware wrote one id and the
// dataplane generated another for the database, so the id the customer held
// could not be found at all -- the documented investigation path did not work.
// These tests pin it down: the response header must *be* the key that was
// recorded.

func TestResponseRequestIDMatchesUsageLog(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up-model", []string{"chat"})

	rec := doDataplane(t, f, plaintext,
		`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	headerID := rec.Header().Get(httpx.HeaderRequestID)
	if headerID == "" {
		t.Fatal("the response must carry X-Request-Id")
	}
	var logged string
	if err := f.pool.QueryRow(ctx,
		`SELECT request_id FROM usage_logs WHERE org_id = $1`, org).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if headerID != logged {
		t.Errorf("response header %q differs from the recorded %q -- a customer with this id cannot find their request", headerID, logged)
	}
	// The hold uses the same key, so the header, the usage row and the reserved
	// funds all refer to one request.
	var holds int
	for _, h := range f.settler.Holds {
		if h.RequestID == headerID {
			holds++
		}
	}
	if holds != 1 {
		t.Errorf("there should be exactly one hold under this key, got %d", holds)
	}
}

// The same holds on the streaming path, where it matters more: once the stream
// is open the response headers can no longer be changed, so the header must be
// set before the first byte.
func TestStreamResponseRequestIDMatchesUsageLog(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, frame := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(frame))
			if fl != nil {
				fl.Flush()
			}
		}
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up-model", []string{"chat"})

	rec := doDataplane(t, f, plaintext,
		`{"model":"openai/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("the stream did not finish: %q", rec.Body.String())
	}

	headerID := rec.Header().Get(httpx.HeaderRequestID)
	var logged string
	if err := f.pool.QueryRow(ctx,
		`SELECT request_id FROM usage_logs WHERE org_id = $1`, org).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if headerID == "" || headerID != logged {
		t.Errorf("streaming response header %q differs from the recorded %q", headerID, logged)
	}
}

// A failed request has to be findable too: when a customer asks why their
// request errored, the id from the response header is all they have.
func TestFailedRequestIDStillMatchesUsageLog(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up-model", []string{"chat"})

	rec := doDataplane(t, f, plaintext,
		`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusOK {
		t.Fatal("an upstream 502 must not come back as 200")
	}

	headerID := rec.Header().Get(httpx.HeaderRequestID)
	var logged, status string
	if err := f.pool.QueryRow(ctx,
		`SELECT request_id, status FROM usage_logs WHERE org_id = $1`, org).Scan(&logged, &status); err != nil {
		t.Fatal(err)
	}
	if headerID != logged {
		t.Errorf("failed request: response header %q differs from the recorded %q -- the investigation path breaks right here", headerID, logged)
	}
	if status == "ok" {
		t.Errorf("an upstream 502 must not be recorded as ok")
	}
}

// doDataplane sends one request through the *actual mounted route*. It does not
// call the pipeline directly: the response header is set by the handler, so
// bypassing the handler would not test this at all.
func doDataplane(t *testing.T, f *pipeFixture, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	// The dataplane is mounted behind the generic middleware, so it is applied
	// here too: it writes an id first, and that is exactly the one the
	// dataplane has to overwrite.
	r.Use(httpx.RequestID)
	r.Route("/v1", func(sub chi.Router) { f.pipeline.Mount(sub) })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}
