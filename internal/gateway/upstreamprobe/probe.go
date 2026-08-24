// Package upstreamprobe builds and executes the one real request used to
// verify provider and organization credentials.
package upstreamprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

const (
	maxErrorBody = 2 << 10
	maxTraceBody = 64 << 10
)

type Spec struct {
	Protocol proxy.Protocol
	Surface  catalog.Surface
	Path     string
	Body     []byte
}

func SpecForEndpoint(endpoint, model string) (Spec, bool) {
	protocol, ok := catalog.ProtocolForEndpoint(endpoint)
	if !ok {
		return Spec{}, false
	}
	surface, ok := catalog.SurfaceForEndpoint(endpoint)
	if !ok {
		return Spec{}, false
	}
	body := map[string]any{"model": model}
	var path string
	switch endpoint {
	case "chat", "messages", "messages_count_tokens":
		if endpoint == "chat" {
			path = catalog.PathChat
		} else if endpoint == "messages_count_tokens" {
			path = catalog.PathMessagesCountTokens
		} else {
			path = catalog.PathMessages
		}
		if endpoint != "messages_count_tokens" {
			body["max_tokens"] = 1
		}
		body["messages"] = []map[string]string{{"role": "user", "content": "hi"}}
	case "responses":
		path = catalog.PathResponses
		body["input"] = "hi"
		body["max_output_tokens"] = 16
	case "responses_compact":
		path = catalog.PathResponsesCompact
		body["input"] = "hi"
	case "responses_input_tokens":
		path = catalog.PathResponsesInputTokens
		body["input"] = "hi"
	case "embeddings":
		path = catalog.PathEmbeddings
		body["input"] = "hi"
	case "generate_content":
		path = catalog.PathGenerateContent
		body = map[string]any{
			"contents":         []map[string]any{{"parts": []map[string]any{{"text": "hi"}}}},
			"generationConfig": map[string]any{"maxOutputTokens": 1},
		}
	case "gemini_count_tokens":
		path = catalog.PathGeminiCountTokens
		body = map[string]any{"contents": []map[string]any{{"parts": []map[string]any{{"text": "hi"}}}}}
	case "gemini_embed_content":
		path = catalog.PathGeminiEmbedContent
		body = map[string]any{"content": map[string]any{"parts": []map[string]any{{"text": "hi"}}}}
	case "gemini_batch_embed_contents":
		path = catalog.PathGeminiBatchEmbedContents
		body = map[string]any{"requests": []map[string]any{{
			"model":   "models/" + model,
			"content": map[string]any{"parts": []map[string]any{{"text": "hi"}}},
		}}}
	case "gemini_interactions":
		path = catalog.PathGeminiInteractions
		body["input"] = "hi"
	case "images":
		path = catalog.PathImagesGenerate
		body["prompt"] = "a dot"
		body["n"] = 1
		body["size"] = "1024x1024"
	default:
		return Spec{}, false
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Spec{}, false
	}
	return Spec{Protocol: proxy.Protocol(protocol), Surface: surface, Path: path, Body: raw}, true
}

func DefaultEndpoint(protocol string) string {
	switch proxy.Protocol(protocol) {
	case proxy.ProtocolAnthropic:
		return "messages"
	case proxy.ProtocolGemini:
		return "generate_content"
	default:
		return "chat"
	}
}

func SpecForProtocol(protocol proxy.Protocol, model string) (Spec, bool) {
	return SpecForEndpoint(DefaultEndpoint(string(protocol)), model)
}

type Input struct {
	Client       *http.Client
	Spec         Spec
	BaseURL      string
	APIKey       string
	Model        string
	Headers      map[string]string
	Transport    catalog.Transport
	Timeout      time.Duration
	CaptureTrace bool
}

type Trace struct {
	URL            string
	Request        string
	ResponseStatus string
	Response       *string
	Truncated      *bool
}

type Result struct {
	CheckedAt  time.Time
	OK         bool
	LatencyMs  *int
	StatusCode *int
	Message    string
	Trace      *Trace
}

func Run(ctx context.Context, in Input) Result {
	out := Result{CheckedAt: time.Now().UTC()}
	body, err := proxy.RewriteRequest(in.Spec.Surface, in.Spec.Body, in.Model, false, in.Transport)
	if err != nil {
		out.Message = "Could not build the probe body: " + err.Error()
		return out
	}
	if in.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.Timeout)
		defer cancel()
	}
	req, err := proxy.BuildRequest(ctx, proxy.Target{
		Protocol: in.Spec.Protocol, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Path: in.Spec.Path, Headers: in.Headers, Transport: in.Transport,
		UpstreamModel: in.Model,
	}, body)
	if err != nil {
		out.Message = "Could not build the request: " + err.Error()
		return out
	}

	var trace *Trace
	if in.CaptureTrace {
		trace = &Trace{URL: req.URL.String()}
		if dump, dumpErr := httputil.DumpRequestOut(req, true); dumpErr == nil {
			trace.Request = string(dump)
		} else {
			trace.Request = "(could not capture the request: " + dumpErr.Error() + ")"
		}
	}
	client := in.Client
	if client == nil {
		client = http.DefaultClient
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		out.Message = "Could not reach the upstream: " + err.Error()
		if trace != nil {
			trace.ResponseStatus = "(no response)"
			out.Trace = trace
		}
		return out
	}
	defer func() { _ = resp.Body.Close() }()

	if trace != nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxTraceBody+1))
		if truncated := len(raw) > maxTraceBody; truncated {
			raw = raw[:maxTraceBody]
			trace.Truncated = &truncated
		}
		trace.ResponseStatus = resp.Status
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if dump, dumpErr := httputil.DumpResponse(resp, true); dumpErr == nil {
			text := string(dump)
			trace.Response = &text
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		out.Trace = trace
	}

	latency := int(time.Since(started).Milliseconds())
	status := resp.StatusCode
	out.LatencyMs, out.StatusCode = &latency, &status
	if resp.StatusCode < http.StatusBadRequest {
		out.OK = true
		return out
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	out.Message = fmt.Sprintf("The upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	return out
}
