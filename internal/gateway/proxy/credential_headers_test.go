package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/access/apikeys"
)

// Credential headers, end to end.
//
// These go through the *actual mounted route*. A unit test of CredentialOf
// proves the extraction is right, but the bug was that the handler never called
// it -- testing only the pure function would miss the same thing next time.
//
// This is also the first set of HTTP-level tests for the Anthropic surface.
// While /v1/messages had only unit coverage, "the official SDK's default
// credential form cannot get in at all" survived all the way to a deployed
// environment.

// anthropicUpstream returns a minimal but complete Anthropic response. The
// usage has to be parseable, or settlement takes the estimation branch and what
// gets tested is a different path.
func anthropicUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{
		"id": "msg_1", "type": "message", "role": "assistant",
		"content": [{"type": "text", "text": "hi"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 7}
	}`))
}

// mountDataplane mounts the dataplane routes the way the real assembly does,
// with the generic middleware on the outside.
func mountDataplane(f *pipeFixture) chi.Router {
	r := chi.NewRouter()
	r.Route("/v1", func(sub chi.Router) { f.pipeline.Mount(sub) })
	return r
}

// postMessages sends one /v1/messages request with the given headers.
func postMessages(f *pipeFixture, headers map[string]string) *httptest.ResponseRecorder {
	body := `{"model":"anthropic/co5","max_tokens":64,"messages":[{"role":"user","content":"who are you?"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mountDataplane(f).ServeHTTP(rec, req)
	return rec
}

// errCodeOf reads the error code out of the response body; both dialects'
// error structures carry error.code.
func errCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error body is not the expected shape: %v (%s)", err, rec.Body)
	}
	return body.Error.Code
}

// The Anthropic SDK constructed with api_key= sends x-api-key. Not accepting it
// makes the drop-in promise of /v1/messages empty.
func TestMessagesAcceptsXAPIKey(t *testing.T) {
	f := newPipeFixture(t, anthropicUpstream)
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "anthropic", "anthropic/co5", "claude-upstream", []string{"messages"})

	rec := postMessages(f, map[string]string{"x-api-key": plaintext})
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key should authenticate and forward successfully, got %d: %s", rec.Code, rec.Body)
	}

	// The negative control: the same path with the credential removed must
	// fail. Without it the green above says nothing -- it would be just as
	// green if authentication were skipped entirely.
	if rec := postMessages(f, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential should answer 401, got %d: %s", rec.Code, rec.Body)
	} else if code := errCodeOf(t, rec); code != "gateway.invalid_api_key" {
		t.Fatalf("error code with no credential = %q, want gateway.invalid_api_key", code)
	}

	// A forged x-api-key must fail too: accepting the header is not the same
	// as exempting it from checking.
	if rec := postMessages(f, map[string]string{"x-api-key": "sk-flb-v1-forged"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a forged key should answer 401, got %d: %s", rec.Code, rec.Body)
	}
}

// Bearer still works: this widened what is accepted, it did not replace it.
func TestMessagesStillAcceptsBearer(t *testing.T) {
	f := newPipeFixture(t, anthropicUpstream)
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "anthropic", "anthropic/co5", "claude-upstream", []string{"messages"})

	rec := postMessages(f, map[string]string{"Authorization": "Bearer " + plaintext})
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer should still work, got %d: %s", rec.Code, rec.Body)
	}
}

// Both headers present and disagreeing: take Authorization and do not error.
// Having both an auth token and an API key left over in the environment is
// common.
func TestBothHeadersAuthorizationWins(t *testing.T) {
	f := newPipeFixture(t, anthropicUpstream)
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "anthropic", "anthropic/co5", "claude-upstream", []string{"messages"})

	rec := postMessages(f, map[string]string{
		"Authorization": "Bearer " + plaintext,
		"x-api-key":     "sk-flb-v1-stale-residue",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("with both present Authorization should be used and pass, got %d: %s", rec.Code, rec.Body)
	}

	// The other way round: the bad one in Authorization, the good one in
	// x-api-key -- this must fail, or "Authorization wins" is a coincidence.
	// An implementation that simply tried both would make the case above green
	// too.
	rec = postMessages(f, map[string]string{
		"Authorization": "Bearer sk-flb-v1-forged",
		"x-api-key":     plaintext,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Authorization winning means a bad one gives 401, got %d: %s", rec.Code, rec.Body)
	}
}

// The OpenAI surface accepts x-api-key as well: the credential headers are
// uniform across the plane and are not split per protocol.
func TestOpenAISurfaceAcceptsXAPIKey(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"hi"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "up-model", []string{"chat"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", plaintext)
	rec := httptest.NewRecorder()
	mountDataplane(f).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the OpenAI surface should accept x-api-key too, got %d: %s", rec.Code, rec.Body)
	}
}

// An inbound credential must never leave the gateway with the request:
// accepting one more header does not change the outbound surface. The x-api-key
// the upstream sees must be the decrypted *upstream* credential, never the
// organization's virtual key.
func TestInboundCredentialNeverReachesUpstream(t *testing.T) {
	f := newPipeFixture(t, anthropicUpstream)
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "anthropic", "anthropic/co5", "claude-upstream", []string{"messages"})

	if rec := postMessages(f, map[string]string{"x-api-key": plaintext}); rec.Code != http.StatusOK {
		t.Fatalf("precondition: the request should succeed, got %d: %s", rec.Code, rec.Body)
	}
	if got := f.lastHeaders.Get("x-api-key"); got != "sk-upstream-secret" {
		t.Fatalf("the upstream should receive the upstream credential, got %q", got)
	}
	for _, h := range []string{"x-api-key", "Authorization"} {
		if v := f.lastHeaders.Get(h); strings.Contains(v, plaintext) {
			t.Fatalf("the virtual key leaked to the upstream in header %s: %q", h, v)
		}
	}
}
