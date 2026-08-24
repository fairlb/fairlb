package proxy_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// openAIStream is a typical OpenAI SSE stream: two content chunks, a final
// chunk carrying usage, and [DONE].
const openAIStream = `data: {"choices":[{"delta":{"content":"Hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500}}

data: [DONE]

`

// anthropicStream splits usage across two events: message_start carries the
// input, message_delta the running output.
const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":800,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"Hi"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":" there"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":300}}

`

// Byte-for-byte pass-through: what the client receives must equal what the
// upstream sent, with no re-encoding and no changed frame boundaries.
func TestPumpBytePassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	s := proxy.NewStreamer()

	out, err := s.Pump(context.Background(), rec, strings.NewReader(openAIStream), catalog.SurfaceChat)
	if err != nil && err != io.EOF {
		t.Fatalf("the pass-through should not error: %v", err)
	}
	if rec.Body.String() != openAIStream {
		t.Fatalf("the bytes must match exactly:\n got=%q\nwant=%q", rec.Body.String(), openAIStream)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !out.FirstByteSent {
		t.Error("the first byte should have been sent")
	}
}

// OpenAI streaming usage comes from the final chunk, which the forced
// include_usage injection guarantees exists.
func TestPumpParsesOpenAIUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	out, _ := proxy.NewStreamer().Pump(
		context.Background(), rec, strings.NewReader(openAIStream), catalog.SurfaceChat)

	if !out.Usage.Present {
		t.Fatal("the final chunk carries usage and it should have been parsed")
	}
	if out.Usage.In != 1000 || out.Usage.Out != 500 {
		t.Fatalf("usage does not match: %+v", out.Usage)
	}
	if out.Text != "Hello world" {
		t.Errorf("the accumulated text should be Hello world: %q", out.Text)
	}
}

// Anthropic's usage spans two events, and message_delta reports a running
// total, so it overwrites rather than adds.
func TestPumpParsesAnthropicUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	out, _ := proxy.NewStreamer().Pump(
		context.Background(), rec, strings.NewReader(anthropicStream), catalog.SurfaceMessages)

	if !out.Usage.Present {
		t.Fatal("usage should have been parsed")
	}
	if out.Usage.In != 800 {
		t.Errorf("the input comes from message_start: %d", out.Usage.In)
	}
	if out.Usage.Out != 300 {
		t.Errorf("the output comes from message_delta's running total: %d", out.Usage.Out)
	}
	if out.Text != "Hi there" {
		t.Errorf("accumulated text: %q", out.Text)
	}
}

// No usage from the upstream means Present is false and the caller estimates.
// The text still accumulates -- it is what the estimate is computed from.
func TestPumpNoUsageFallsBackToText(t *testing.T) {
	const noUsage = `data: {"choices":[{"delta":{"content":"abc"}}]}

data: [DONE]

`
	rec := httptest.NewRecorder()
	out, _ := proxy.NewStreamer().Pump(
		context.Background(), rec, strings.NewReader(noUsage), catalog.SurfaceChat)

	if out.Usage.Present {
		t.Error("the upstream sent no usage, so it must not be marked as parsed")
	}
	if out.Text != "abc" {
		t.Errorf("text must accumulate for the estimate: %q", out.Text)
	}
}

// A malformed chunk must not interrupt the pass-through: shadow parsing is off
// to the side, and anything that will not parse is skipped.
func TestPumpMalformedChunkDoesNotBreakPassthrough(t *testing.T) {
	const mixed = `data: {"choices":[{"delta":{"content":"ok"}}]}

data: {not json at all

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

`
	rec := httptest.NewRecorder()
	out, err := proxy.NewStreamer().Pump(
		context.Background(), rec, strings.NewReader(mixed), catalog.SurfaceChat)
	if err != nil && err != io.EOF {
		t.Fatalf("a malformed chunk must not fail the pass-through: %v", err)
	}
	if rec.Body.String() != mixed {
		t.Error("a malformed chunk must still pass through unchanged")
	}
	if !out.Usage.Present || out.Usage.In != 10 {
		t.Errorf("usage after a malformed chunk should still be parsed: %+v", out.Usage)
	}
}

// The in-stream error event renders in each dialect's shape; the first byte is
// already out, so it can only be conveyed inside the stream.
func TestStreamErrorEvent(t *testing.T) {
	oa := string(proxy.StreamErrorEvent(proxy.SurfaceOpenAI, "the upstream stream broke"))
	if !strings.HasPrefix(oa, "data: ") || !strings.HasSuffix(oa, "\n\n") {
		t.Errorf("OpenAI error event format: %q", oa)
	}
	if !strings.Contains(oa, proxy.CodeStreamInterrupted) {
		t.Errorf("the stream_interrupted code must be present: %q", oa)
	}

	an := string(proxy.StreamErrorEvent(proxy.SurfaceAnthropic, "the upstream stream broke"))
	if !strings.HasPrefix(an, "event: error\n") {
		t.Errorf("the Anthropic error event must carry an event line: %q", an)
	}
	if !strings.Contains(an, `"type":"error"`) {
		t.Errorf("the Anthropic event needs type=error at the top level: %q", an)
	}
}
