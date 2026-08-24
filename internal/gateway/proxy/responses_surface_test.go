package proxy_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The Responses surface's usage shapes are taken from what a real upstream
// actually sent rather than hand-written from a specification: one relay
// speaking the OpenAI protocol, called once non-streaming and once streaming,
// with the bodies copied in verbatim.
const (
	responsesUsageJSON = `{"usage":{"input_tokens":303,` +
		`"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},` +
		`"output_tokens":11,"output_tokens_details":{"reasoning_tokens":0},` +
		`"total_tokens":314}}`

	// Of the event types observed, only the terminal one carries usage, and it
	// sits under .response.usage.
	responsesDoneFrame = `data: {"type":"response.completed","response":{"usage":{` +
		`"input_tokens":303,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},` +
		`"output_tokens":11,"output_tokens_details":{"reasoning_tokens":0},` +
		`"total_tokens":314}}}` + "\n\n"
)

// Streaming and non-streaming must normalise to the same result; each parses
// usage separately, which is the easiest place for them to drift.
func TestResponsesUsageStreamMatchesNonStream(t *testing.T) {
	want := proxy.Usage{In: 303, Out: 11, Present: true}

	// Usage contains a map of tool usage, so == will not do; DeepEqual is used
	// and the full difference printed.
	if got := proxy.ParseUsage(catalog.SurfaceResponses, []byte(responsesUsageJSON)); !reflect.DeepEqual(got, want) {
		t.Errorf("non-streaming parse = %+v, want %+v", got, want)
	}

	frames := []string{
		`data: {"type":"response.created","response":{}}` + "\n\n",
		`data: {"type":"response.output_text.delta","delta":"Hi"}` + "\n\n",
		responsesDoneFrame,
	}
	if got := proxy.AccumulateForTest(catalog.SurfaceResponses, frames); !reflect.DeepEqual(got, want) {
		t.Errorf("streaming parse = %+v, want %+v", got, want)
	}
}

// The cache details are a *subset* of input_tokens on this dialect, so they are
// subtracted. That is the exact opposite of Anthropic's additive reading, and it
// is the easiest place to get billing backwards.
func TestResponsesCacheBucketsAreSubtracted(t *testing.T) {
	body := `{"usage":{"input_tokens":2000,` +
		`"input_tokens_details":{"cached_tokens":1500,"cache_write_tokens":300},` +
		`"output_tokens":50,"output_tokens_details":{"reasoning_tokens":20}}}`

	got := proxy.ParseUsage(catalog.SurfaceResponses, []byte(body))
	want := proxy.Usage{In: 200, CachedRead: 1500, CacheWrite: 300, Out: 50, Reasoning: 20, Present: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the four buckets = %+v, want %+v (2000 - 1500 - 300 = 200)", got, want)
	}
	// The four buckets are pairwise disjoint: adding them back must equal the
	// total input the upstream reported.
	if got.In+got.CachedRead+got.CacheWrite != 2000 {
		t.Errorf("the buckets sum to %d, not the reported input_tokens of 2000, so they overlap or one is missing",
			got.In+got.CachedRead+got.CacheWrite)
	}
}

// If the upstream reports additively -- relays that rewrite usage really do
// this -- the subtraction goes negative. It must not be clamped to zero: that
// would give those tokens away, since the parent already is the uncached
// part.
func TestResponsesHandlesAdditiveUpstream(t *testing.T) {
	body := `{"usage":{"input_tokens":100,` +
		`"input_tokens_details":{"cached_tokens":900,"cache_write_tokens":0},` +
		`"output_tokens":5}}`

	got := proxy.ParseUsage(catalog.SurfaceResponses, []byte(body))
	if got.In != 100 {
		t.Errorf("on the additive reading In should be input_tokens itself (100), got %d; "+
			"clamping to 0 would give those 100 tokens away", got.In)
	}
	if got.CachedRead != 900 {
		t.Errorf("cache read = %d, should be kept as 900", got.CachedRead)
	}
}

// Responses has no stream_options parameter, so injecting per protocol would hand
// the upstream a field it does not know. That is exactly why the criterion is
// the surface rather than the protocol.
func TestResponsesStreamDoesNotInjectStreamOptions(t *testing.T) {
	in := []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`)

	out, err := proxy.RewriteRequest(catalog.SurfaceResponses, in, "up-model", true, catalog.Transport{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["stream_options"]; has {
		t.Error("the Responses surface must not have stream_options injected; the upstream has no such parameter")
	}
	if doc["model"] != "up-model" {
		t.Errorf("the model name was not rewritten: %v", doc["model"])
	}

	// The other direction: the chat surface *must* have it injected, or the
	// final chunk carries no usage and settlement has nothing actual to charge
	// against.
	out, err = proxy.RewriteRequest(catalog.SurfaceChat, in, "up-model", true, catalog.Transport{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["stream_options"]; !has {
		t.Error("the chat surface must have stream_options.include_usage injected")
	}
}

// The hold estimate has to recognise Responses' max_output_tokens.
func TestMaxTokensReadsResponsesField(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int64
	}{
		{"responses max_output_tokens", `{"max_output_tokens":16}`, 16},
		{"chat max_tokens", `{"max_tokens":32}`, 32},
		{"chat max_completion_tokens wins", `{"max_tokens":32,"max_completion_tokens":64}`, 64},
		{"neither given", `{}`, 0},
	} {
		if got := proxy.MaxTokensOf([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Extracting response text must branch on the *surface*, the same criterion
// ParseUsage and RewriteRequest use.
//
// Responses puts the text in `output[].content[].text` and chat puts it in
// `choices[].message.content`. Both belong to the openai protocol, so branching
// on protocol inevitably leaves one of them always empty. That is not a display
// problem: the extracted text decides how much to charge -- it is the output-side
// estimate when the upstream reports no usage.
func TestResponseTextOfBranchesBySurface(t *testing.T) {
	for _, tc := range []struct {
		name    string
		surface catalog.Surface
		body    string
		want    string
	}{
		{
			"responses: output[].content[].text",
			catalog.SurfaceResponses,
			`{"id":"resp_1","output":[{"type":"message","content":[
				{"type":"output_text","text":"Hel"},{"type":"output_text","text":"lo"}]}]}`,
			"Hello",
		},
		{
			// A reasoning model emits a reasoning item with no content
			// first; skip it rather than stopping there.
			"responses: skips a reasoning item with no text",
			catalog.SurfaceResponses,
			`{"output":[{"type":"reasoning","summary":[]},
				{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`,
			"answer",
		},
		{
			"chat: choices[].message.content",
			catalog.SurfaceChat,
			`{"choices":[{"message":{"content":"hi"}}]}`,
			"hi",
		},
		{
			"messages: content[].text",
			catalog.SurfaceMessages,
			`{"content":[{"type":"text","text":"hi"}]}`,
			"hi",
		},
	} {
		if got := proxy.ResponseTextOf(tc.surface, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Tool usage is shaped differently by each vendor and is still moving, so
// values are read by *shape* rather than by enumerating known key names.
func TestToolUsageParsedFromBothShapes(t *testing.T) {
	// An OpenAI-compatible relay: tool_usage at the top level, with
	// num_requests one level in.
	oa := `{"usage":{"prompt_tokens":10,"completion_tokens":1},
	        "tool_usage":{"web_search":{"num_requests":3},
	                      "image_gen":{"input_tokens":0,"output_tokens":0}}}`
	got := proxy.ParseUsage(catalog.SurfaceChat, []byte(oa))
	if got.ToolCalls["web_search"] != 3 {
		t.Errorf("web_search count = %v, want 3", got.ToolCalls["web_search"])
	}
	if _, has := got.ToolCalls["image_gen"]; has {
		t.Error("a tool with a count of zero must not enter the map; it would be recorded as real usage")
	}

	// Anthropic: usage.server_tool_use, holding the count directly.
	an := `{"usage":{"input_tokens":10,"output_tokens":1,
	        "server_tool_use":{"web_search_requests":2}}}`
	got = proxy.ParseUsage(catalog.SurfaceMessages, []byte(an))
	if got.ToolCalls["web_search_requests"] != 2 {
		t.Errorf("Anthropic tool count = %v, want 2", got.ToolCalls["web_search_requests"])
	}

	// An unrecognised shape is skipped whole rather than guessed at: recording
	// it wrongly bills the wrong count, whereas not recording it merely leaves
	// the cost absorbed by the markup.
	weird := `{"usage":{"input_tokens":1},"tool_usage":{"x":"yes","y":[1,2]}}`
	if got = proxy.ParseUsage(catalog.SurfaceResponses, []byte(weird)); got.ToolCalls != nil {
		t.Errorf("an unrecognised shape should be skipped whole: %v", got.ToolCalls)
	}
}
