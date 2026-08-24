package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The Gemini protocol end to end.
//
// It differs from the other two in where the request is addressed rather than
// only in what it carries: the model is a path segment, streaming is a
// different method name, and the credential rides in a header of its own. Each
// of those is a place the request can be assembled wrongly in a way that reads
// as an upstream fault, so each is asserted against what the upstream actually
// received.

const geminiResponse = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}],
  "usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":500}}`

func TestGeminiForwardsModelInThePathAndCredentialInItsOwnHeader(t *testing.T) {
	var got *http.Request
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(geminiResponse))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalogAsVendor(t, "google", "gemini", "google/gemini-flash", "gemini-2.5-flash",
		[]string{"generate_content"})

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceGenerateContent, Protocol: proxy.ProtocolGemini,
		UpstreamPath: catalog.PathGenerateContent,
		Model:        "google/gemini-flash",
		Body:         []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`),
		Credential:   plaintext, RequestID: "gemini-basic",
	})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got == nil {
		t.Fatal("the upstream was never called")
	}

	// The model reaches the upstream in the address, under the name that
	// upstream knows it by -- not the catalogue's own slug.
	if want := "/v1beta/models/gemini-2.5-flash:generateContent"; got.URL.Path != want {
		t.Errorf("path = %q, want %q", got.URL.Path, want)
	}
	// And is therefore *not* in the body: this API defines no such field, and a
	// body carrying one is a body this gateway invented.
	var sent map[string]any
	if err := json.Unmarshal(f.lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	if _, present := sent["model"]; present {
		t.Errorf("the request body carries a model field: %s", f.lastBody)
	}
	if got.Header.Get("X-Goog-Api-Key") == "" {
		t.Errorf("the credential should ride in x-goog-api-key, headers were %v", got.Header)
	}
	if got.Header.Get("Authorization") != "" {
		t.Error("no bearer token should be sent on this protocol")
	}

	// Usage is metered from usageMetadata, so the response the caller gets back
	// carries this gateway's own annotation in the place this protocol keeps it.
	var back struct {
		UsageMetadata struct {
			Fairlb *struct {
				Cost string `json:"cost"`
			} `json:"fairlb"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(res.Body, &back); err != nil {
		t.Fatal(err)
	}
	// Deliberately absent. This protocol's official client validates responses
	// against models declared extra='forbid', so one added key makes every
	// response raise before the caller sees any text -- verified against
	// google-genai, which rejects `usageMetadata.fairlb` outright. The charge is
	// readable from the usage log and the console instead.
	if back.UsageMetadata.Fairlb != nil {
		t.Errorf("the response carries the gateway's usage annotation, which this protocol's SDK refuses to parse: %s", res.Body)
	}

	var in, out int64
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tokens_in, tokens_out, usage_estimated FROM usage_logs WHERE request_id = 'gemini-basic'`).
		Scan(&in, &out, &estimated); err != nil {
		t.Fatal(err)
	}
	if in != 1000 || out != 500 {
		t.Errorf("usage row recorded in=%d out=%d, want 1000 and 500", in, out)
	}
	if estimated {
		t.Error("the upstream reported usage, so nothing should have been estimated")
	}
}

func TestGeminiStreamsSSEAndMetersFromTheLastChunk(t *testing.T) {
	var got *http.Request
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],` +
				`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100}}` + "\n\n",
			`data: {"candidates":[{"content":{"parts":[{"text":" there"}]}}],` +
				`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":500}}` + "\n\n",
		} {
			_, _ = w.Write([]byte(frame))
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalogAsVendor(t, "google", "gemini", "google/gemini-flash", "gemini-2.5-flash",
		[]string{"generate_content"})

	rec := httptest.NewRecorder()
	if gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceGenerateContent, Protocol: proxy.ProtocolGemini,
		UpstreamPath: catalog.PathStreamGenerateContent,
		Model:        "google/gemini-flash",
		Body:         []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`),
		Credential:   plaintext, RequestID: "gemini-stream",
		UpstreamQuery: map[string]string{"alt": "sse"},
	}, proxy.SurfaceGemini); gerr != nil {
		t.Fatal(gerr)
	}

	// Streaming is a different address on this API, and the SSE selector has to
	// travel with it: without alt=sse the upstream answers a JSON array, which
	// this gateway does not parse and therefore could not meter.
	if want := "/v1beta/models/gemini-2.5-flash:streamGenerateContent"; got.URL.Path != want {
		t.Errorf("path = %q, want %q", got.URL.Path, want)
	}
	if got.URL.Query().Get("alt") != "sse" {
		t.Errorf("the stream selector did not reach the upstream: %q", got.URL.RawQuery)
	}
	if body := rec.Body.String(); !strings.Contains(body, "there") {
		t.Errorf("the stream was not passed through: %q", body)
	}

	// Each chunk restates the totals, so the last one is the answer. Summing
	// them would bill this request for 600 output tokens instead of 500.
	var out int64
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tokens_out, usage_estimated FROM usage_logs WHERE request_id = 'gemini-stream'`).
		Scan(&out, &estimated); err != nil {
		t.Fatal(err)
	}
	if out != 500 {
		t.Errorf("tokens_out = %d, want 500 (the last chunk's total, not the sum)", out)
	}
	if estimated {
		t.Error("the stream reported usage, so nothing should have been estimated")
	}
}

// A refusal reaches a Gemini client in the shape that protocol defines. Answered
// in another protocol's shape, an ordinary error becomes an unparseable response
// and the SDK reports something other than what happened.
func TestGeminiErrorsAreRenderedInItsOwnShape(t *testing.T) {
	rec := httptest.NewRecorder()
	proxy.Write(rec, proxy.SurfaceGemini,
		proxy.NewError("gateway.model_not_found", "No such model"))

	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Type   string `json:"@type"`
				Reason string `json:"reason"`
				Domain string `json:"domain"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Code != 404 || rec.Code != 404 {
		t.Errorf("status = %d, body code = %d, want 404 for both", rec.Code, body.Error.Code)
	}
	// The canonical status is not derivable from the HTTP code, and clients
	// switch on it.
	if body.Error.Status != "NOT_FOUND" {
		t.Errorf("status = %q, want NOT_FOUND", body.Error.Status)
	}
	if len(body.Error.Details) != 1 || body.Error.Details[0].Reason != "gateway.model_not_found" {
		t.Errorf("the gateway's own code is missing from the details: %+v", body.Error.Details)
	}
	if body.Error.Details[0].Domain != "fairlb" {
		t.Errorf("domain = %q; the reason is this gateway's vocabulary and has to say so",
			body.Error.Details[0].Domain)
	}
}

// A Gemini client authenticates with x-goog-api-key, so a gateway serving that
// protocol has to accept it. Rejecting it would fail every request from an
// unmodified SDK while claiming to serve the protocol.
//
// The `?key=` query form is refused on purpose: a credential in a URL is a
// credential in access logs, proxy logs and browser history.
func TestGeminiCredentialComesFromItsOwnHeader(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*http.Request)
		want    string
	}{
		{"the Gemini header", func(r *http.Request) { r.Header.Set("x-goog-api-key", "sk-goog") }, "sk-goog"},
		{"still accepts a bearer token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer sk-bearer")
		}, "sk-bearer"},
		{"Authorization outranks it", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer sk-bearer")
			r.Header.Set("x-goog-api-key", "sk-goog")
		}, "sk-bearer"},
		{"a credential in the query string is not one", func(r *http.Request) {
			r.URL.RawQuery = "key=sk-in-the-url"
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", nil)
			c.prepare(req)
			if got := proxy.CredentialOf(req); got != c.want {
				t.Errorf("CredentialOf = %q, want %q", got, c.want)
			}
		})
	}
}
