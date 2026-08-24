//go:build live

// Acceptance against real upstreams.
//
// **This file is not part of the standard test run**: it calls real upstreams,
// incurs real cost, and its results are not reproducible. It is held behind the
// `live` build tag and run by hand; any leg whose key is not configured skips
// explicitly rather than quietly passing.
//
// What it checks *nothing else can*: every other gateway test in this
// repository runs against local fixtures, and those fixtures are written to the
// shapes we believe are correct. What is checked here is whether that belief
// matches the real upstreams -- whether the request shape is accepted, where
// usage hangs, which layer holds the response text.
//
// Two past defects share one pattern: the Anthropic SDK's default credential
// header being rejected outright, and the responses surface running the full
// pipeline while writing zero usage rows and charging nothing. The pattern is
// that **a surface with only unit coverage has no coverage**. These legs are
// the gate on it.
//
// It does not run the full pipeline -- that needs a database, wallets and a
// catalogue, and fixtures already cover it -- only the injection and parsing
// layers, which are exactly what a real upstream can tell us about.
package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// liveLeg configures one real upstream crossed with one surface.
type liveLeg struct {
	name         string
	keyEnv       string // environment variable holding the key; empty means skip this leg
	urlEnv       string // base URL override; empty uses defaultURL
	modelEnv     string
	defaultURL   string
	defaultModel string
	surface      catalog.Surface
	protocol     proxy.Protocol
	path         string
	// body builds the smallest possible request. The point is to check the
	// shape and how usage is counted, not model quality, so every leg uses a
	// one-word prompt and the smallest output cap.
	body func(model string, stream bool) string
}

func liveLegs() []liveLeg {
	return []liveLeg{
		{
			name: "openai/chat_completions", keyEnv: "LIVE_OPENAI_API_KEY",
			urlEnv: "LIVE_OPENAI_BASE_URL", modelEnv: "LIVE_OPENAI_MODEL",
			defaultURL: "https://api.openai.com", defaultModel: "gpt-4o-mini",
			surface: catalog.SurfaceChat, protocol: proxy.ProtocolOpenAI,
			path: "/v1/chat/completions",
			body: func(m string, stream bool) string {
				// The chat surface has to carry include_usage itself, or the
				// final chunk has no usage -- the same injection
				// RewriteRequest performs for that surface.
				opts := ""
				if stream {
					opts = `,"stream":true,"stream_options":{"include_usage":true}`
				}
				return fmt.Sprintf(
					`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]%s}`,
					m, opts)
			},
		},
		{
			// A vendor's OpenAI-compatible layer: the same surface, a
			// different implementation. How it differs -- whether the usage
			// details are populated, for instance -- is only knowable by
			// calling it.
			name: "gemini/chat_completions", keyEnv: "LIVE_GEMINI_API_KEY",
			urlEnv: "LIVE_GEMINI_BASE_URL", modelEnv: "LIVE_GEMINI_MODEL",
			defaultURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
			defaultModel: "gemini-2.0-flash",
			surface:      catalog.SurfaceChat, protocol: proxy.ProtocolOpenAI,
			path: "/chat/completions",
			body: func(m string, stream bool) string {
				opts := ""
				if stream {
					opts = `,"stream":true,"stream_options":{"include_usage":true}`
				}
				return fmt.Sprintf(
					`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]%s}`,
					m, opts)
			},
		},
		{
			name: "anthropic/messages", keyEnv: "LIVE_ANTHROPIC_API_KEY",
			urlEnv: "LIVE_ANTHROPIC_BASE_URL", modelEnv: "LIVE_ANTHROPIC_MODEL",
			defaultURL: "https://api.anthropic.com", defaultModel: "claude-haiku-4-5-20251001",
			surface: catalog.SurfaceMessages, protocol: proxy.ProtocolAnthropic,
			path: "/v1/messages",
			body: func(m string, stream bool) string {
				s := ""
				if stream {
					s = `,"stream":true`
				}
				return fmt.Sprintf(
					`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]%s}`,
					m, s)
			},
		},
		{
			// The responses surface. By default it reuses the OpenAI key
			// against the official endpoint; point it at a relay with the
			// dedicated environment variables. A relay offering every one of
			// its models only through /v1/responses is what motivated
			// supporting this surface in the first place.
			name: "openai/responses", keyEnv: "LIVE_RESPONSES_API_KEY",
			urlEnv: "LIVE_RESPONSES_BASE_URL", modelEnv: "LIVE_RESPONSES_MODEL",
			defaultURL: "https://api.openai.com", defaultModel: "gpt-4o-mini",
			surface: catalog.SurfaceResponses, protocol: proxy.ProtocolOpenAI,
			path: "/v1/responses",
			body: func(m string, stream bool) string {
				// responses uses input and max_output_tokens, and carries *no*
				// stream_options -- the upstream has no such parameter, which
				// is why RewriteRequest deliberately does not inject it here.
				s := ""
				if stream {
					s = `,"stream":true`
				}
				return fmt.Sprintf(`{"model":%q,"input":"hi","max_output_tokens":16%s}`, m, s)
			},
		},
	}
}

// keyFor resolves this leg's key: the dedicated variable wins, and the
// responses leg falls back to the OpenAI key.
func (l liveLeg) keyFor() string {
	if v := os.Getenv(l.keyEnv); v != "" {
		return v
	}
	if l.surface == catalog.SurfaceResponses {
		return os.Getenv("LIVE_OPENAI_API_KEY")
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const liveTimeout = 60 * time.Second

// TestLiveUpstreams is the entry point for a live run:
//
//	go test -tags live -run TestLiveUpstreams -v ./internal/gateway/proxy/
//
// Renaming it is more dangerous than it looks. Anything that selects this test
// by name keeps exiting 0 once the name stops matching, because `go test -run`
// treats "nothing matched" as success -- so a runner pointed at a test that no
// longer exists is permanently, silently green. Rename it and you must rename
// it everywhere it is selected, then confirm that a deliberate typo still fails.
//
// gate-honesty: a leg whose key is not configured calls t.Skip here, and that
// skip **never disappears from the verdict**. Under -v, `go test` prints a
// `--- SKIP:` line naming each skipped leg beside the `--- PASS:` lines, so the
// output always states which upstreams were actually exercised and which were
// not. A run in which every leg skipped is visibly a run that proved nothing
// about any upstream, and must not be read as "the live acceptance passed".
func TestLiveUpstreams(t *testing.T) {
	for _, leg := range liveLegs() {
		t.Run(leg.name, func(t *testing.T) {
			apiKey := leg.keyFor()
			if apiKey == "" {
				t.Skipf("%s is not configured, skipping this leg", leg.keyEnv)
			}
			baseURL := envOr(leg.urlEnv, leg.defaultURL)
			model := envOr(leg.modelEnv, leg.defaultModel)

			t.Run("non-streaming", func(t *testing.T) {
				liveNonStream(t, leg, baseURL, apiKey, model)
			})
			t.Run("streaming", func(t *testing.T) {
				liveStream(t, leg, baseURL, apiKey, model)
			})
		})
	}
}

// liveNonStream checks the non-streaming path: the request is accepted, usage
// parses, and the response text can be extracted.
func liveNonStream(t *testing.T, leg liveLeg, baseURL, apiKey, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	req, err := proxy.BuildRequest(ctx, proxy.Target{
		Protocol: leg.protocol, BaseURL: baseURL, APIKey: apiKey, Path: leg.path,
	}, []byte(leg.body(model, false)))
	if err != nil {
		t.Fatalf("building the outbound request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the upstream returned %d: %s", resp.StatusCode, truncate(raw))
	}

	// 1. Usage must parse. Failing that, the estimation fallback takes over --
	// and that is a gate, not a bill. Relying on it long-term means the basis
	// of billing has slid from the upstream's actual usage to our own character
	// heuristic.
	usage := proxy.ParseUsage(leg.surface, raw)
	if !usage.Present {
		t.Errorf("no usage parsed, so the estimation fallback would take over. Response: %s", truncate(raw))
	}
	if usage.In <= 0 || usage.Out <= 0 {
		t.Errorf("the usage buckets are implausible: %+v (response: %s)", usage, truncate(raw))
	}

	// 2. The response text must be extractable. This pins the fix that made the
	// branch depend on the surface: branching on protocol leaves the responses
	// surface always empty here, which estimates the output side as zero and
	// leaves the outbound review judging nothing.
	if text := proxy.ResponseTextOf(leg.surface, raw); text == "" {
		t.Errorf("no response text extracted -- the %s surface's response shape does not match our parsing. Response: %s",
			leg.surface, truncate(raw))
	}
}

// liveStream checks the streaming path: the SSE parses, usage arrives in the
// terminal event, and the text deltas accumulate.
func liveStream(t *testing.T, leg liveLeg, baseURL, apiKey, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	req, err := proxy.BuildRequest(ctx, proxy.Target{
		Protocol: leg.protocol, BaseURL: baseURL, APIKey: apiKey, Path: leg.path, Stream: true,
	}, []byte(leg.body(model, true)))
	if err != nil {
		t.Fatalf("building the outbound request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the upstream returned %d: %s", resp.StatusCode, truncate(raw))
	}

	frames := splitSSE(raw)
	if len(frames) == 0 {
		t.Fatalf("no SSE frame was received: %s", truncate(raw))
	}
	usage := proxy.AccumulateForTest(leg.surface, frames)
	if !usage.Present {
		t.Errorf("no usage parsed from the stream (%d frames). Last frame: %s",
			len(frames), truncate([]byte(frames[len(frames)-1])))
	}
	if usage.In <= 0 || usage.Out <= 0 {
		t.Errorf("the streaming usage buckets are implausible: %+v", usage)
	}

	// Streaming and non-streaming must normalise to the same shape. Each parses
	// usage separately, which is the easiest place for them to drift -- and the
	// difference should surface here rather than at the end of the month.
	t.Logf("%s streaming usage: %+v (%d frames)", leg.name, usage, len(frames))
}

// splitSSE splits SSE frames on blank lines, keeping the data: lines as they
// are, since the accumulator parses per frame.
func splitSSE(raw []byte) []string {
	var out []string
	for _, blk := range strings.Split(string(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))), "\n\n") {
		if strings.TrimSpace(blk) == "" {
			continue
		}
		out = append(out, blk+"\n\n")
	}
	return out
}

// truncate shortens an upstream response so failure output does not flood the
// screen, and so a long response does not land whole in a log.
func truncate(b []byte) string {
	const max = 800
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
