// Package upstreamprobe builds and executes the one real request used to
// verify provider and organization credentials.
package upstreamprobe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/video"
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
	// ContentType is set only when the body is not JSON. The image edits
	// endpoint is the one such probe: its request is a multipart upload, so it
	// is sent as written instead of going through RewriteRequest -- which is
	// safe here and only here, because a probe already addresses the upstream's
	// own model id and has nothing to rewrite.
	ContentType string
}

// SpecForEndpoint builds the smallest request that proves an endpoint works.
//
// vendor is consulted for the video plane only. Every other endpoint is a
// dialect endpoint whose request shape follows from the protocol, and on those
// the vendor stays inert -- which is ADR-0140's rule, still in force
// everywhere except the one plane ADR-0219 carved out.
func SpecForEndpoint(endpoint, model, vendor string) (Spec, bool) {
	protocol, ok := catalog.ProtocolForEndpoint(endpoint)
	if !ok {
		return Spec{}, false
	}
	surface, ok := catalog.SurfaceForEndpoint(endpoint)
	if !ok {
		return Spec{}, false
	}
	if endpoint == "video" {
		return videoSpec(model, vendor)
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
	case "images_edits":
		return imagesEditSpec(protocol, surface, model)
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
	return SpecForEndpoint(DefaultEndpoint(string(protocol)), model, "")
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
	body := in.Spec.Body
	if in.Spec.ContentType == "" {
		// JSON: the model name still goes through the one function allowed to
		// edit a body. A multipart probe skips it because there is nothing to
		// rewrite -- the spec was built with the upstream's own model id.
		var err error
		if body, err = proxy.RewriteRequest(in.Spec.Surface, in.Spec.Body, in.Model, false, in.Transport); err != nil {
			out.Message = "Could not build the probe body: " + err.Error()
			return out
		}
	}
	if in.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.Timeout)
		defer cancel()
	}
	target := proxy.Target{
		Protocol: in.Spec.Protocol, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Path: in.Spec.Path, Headers: in.Headers, Transport: in.Transport,
		UpstreamModel: in.Model,
	}
	var req *http.Request
	var err error
	if in.Spec.ContentType != "" {
		req, err = proxy.BuildRequestStream(ctx, target, bytes.NewReader(body), in.Spec.ContentType)
	} else {
		req, err = proxy.BuildRequest(ctx, target, body)
	}
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

// videoSpec builds the probe body through the vendor's own mapper, so the probe
// sends exactly the request the data plane would.
//
// A probe that built its own approximation would report its own mistake as the
// provider's, which is the failure the transport profile is applied here to
// avoid. The clip is the shortest and smallest the vendor's own envelope
// allows: this endpoint is never probed automatically precisely because each
// probe generates a real video, so the one an operator does ask for should cost
// as little as it can.
func videoSpec(model, vendor string) (Spec, bool) {
	mapper, ok := video.MapperFor(vendor)
	if !ok {
		return Spec{}, false
	}
	envelope := mapper.Envelope(model)
	req := video.Request{Prompt: "a dot", N: 1}
	if len(envelope.DurationsSeconds) > 0 {
		req.DurationSeconds = slices.Min(envelope.DurationsSeconds)
	}
	if len(envelope.Resolutions) > 0 {
		req.Resolution = envelope.Resolutions[0]
	}
	if len(envelope.AspectRatios) > 0 {
		req.AspectRatio = envelope.AspectRatios[0]
	}
	// Resolved the same way admission resolves it, so the probe asks for what
	// this model actually does about sound rather than for a combination the
	// upstream would refuse -- a refusal here reads as "the endpoint is not
	// there", which is the one conclusion a probe must not reach by accident.
	out, err := mapper.Submit(req, model, envelope.ResolveAudio(req))
	if err != nil {
		return Spec{}, false
	}
	return Spec{
		Protocol: proxy.ProtocolVideo, Surface: catalog.SurfaceVideo,
		Path: out.Path, Body: out.Body,
	}, true
}

// probePNG is a one-pixel PNG, the smallest thing that can stand in for an
// upload.
const probePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// imagesEditSpec builds the smallest multipart edit request.
//
// It is the only probe whose body is not JSON, and the reason the endpoint gets
// a probe of its own at all is that a vendor can serve image generation without
// serving edits -- several take an input image on the generations call instead.
// While the two shared one capability key that was unaskable, and a route
// verified on generations answered every edit request with the upstream's 404.
//
// What it reliably answers is exactly that question: a vendor with no edits
// path replies 404 or 405 and the verdict is `unsupported`. What it cannot
// always answer is the other half -- an upstream that validates the upload
// before the endpoint may reject one pixel with a 400, which reads as `failed`
// rather than `ok`. That is the same treatment relays already get when they say
// 400 where they mean unsupported, and the answer is the same one: an operator
// marks it by hand. Sending a realistic image instead would mean shipping one,
// and it would still be some other upstream's turn to dislike it.
func imagesEditSpec(protocol string, surface catalog.Surface, model string) (Spec, bool) {
	png, err := base64.StdEncoding.DecodeString(probePNG)
	if err != nil {
		return Spec{}, false
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", model); err != nil {
		return Spec{}, false
	}
	if err := w.WriteField("prompt", "a dot"); err != nil {
		return Spec{}, false
	}
	part, err := w.CreateFormFile("image", "probe.png")
	if err != nil {
		return Spec{}, false
	}
	if _, err := part.Write(png); err != nil {
		return Spec{}, false
	}
	if err := w.Close(); err != nil {
		return Spec{}, false
	}
	return Spec{
		Protocol: proxy.Protocol(protocol), Surface: surface,
		Path: catalog.PathImagesEdit, Body: buf.Bytes(), ContentType: w.FormDataContentType(),
	}, true
}
