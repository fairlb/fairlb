package proxy_test

import (
	"reflect"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The billing buckets must be pairwise disjoint. The two dialects disagree
// about what "input" contains, so getting the normalisation wrong double-charges
// one dialect and undercharges the other -- and reconciliation across the
// ledger, the usage rows and the rollups stays *green* throughout, because all
// three consume the same already-wrong charged amount. These assertions can
// therefore only be made against amounts computed by hand from each vendor's
// published prices, never against reconciliation.

// One vendor's tier: $2.50 input, $1.25 cache read, $10.00 output per million
// tokens.
var openAIPrice = catalog.Price{
	InNanoPerMTok:        2_500_000_000,
	CacheReadNanoPerMTok: 1_250_000_000,
	OutNanoPerMTok:       10_000_000_000,
}

// The other vendor's tier: $3.00 input, $0.30 cache read (0.1x), $3.75 cache
// write (1.25x), $15.00 output.
var anthropicPrice = catalog.Price{
	InNanoPerMTok:         3_000_000_000,
	CacheReadNanoPerMTok:  300_000_000,
	CacheWriteNanoPerMTok: 3_750_000_000,
	OutNanoPerMTok:        15_000_000_000,
}

func upstreamCostOf(t *testing.T, p catalog.Price, u proxy.Usage) int64 {
	t.Helper()
	q, err := catalog.Compute(catalog.Flat(p), catalog.Flat(p), catalog.Tokens{
		In: u.In, Out: u.Out, CachedRead: u.CachedRead, CacheWrite: u.CacheWrite,
	}, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return q.UpstreamUSDNano
}

func TestUsageBucketsOpenAI(t *testing.T) {
	// OpenAI semantics: prompt_tokens includes cached_tokens, and
	// completion_tokens includes reasoning_tokens.
	body := []byte(`{"usage":{"prompt_tokens":2006,"completion_tokens":300,
	  "total_tokens":2306,
	  "prompt_tokens_details":{"cached_tokens":1920},
	  "completion_tokens_details":{"reasoning_tokens":128}}}`)

	u := proxy.ParseUsage(catalog.SurfaceChat, body)
	if !u.Present {
		t.Fatal("usage was not parsed")
	}
	if u.In != 86 || u.CachedRead != 1920 || u.Out != 300 || u.CacheWrite != 0 {
		t.Fatalf("buckets normalised wrongly: In=%d CachedRead=%d Out=%d CacheWrite=%d, "+
			"want In=86 (2006-1920) CachedRead=1920 Out=300 CacheWrite=0",
			u.In, u.CachedRead, u.Out, u.CacheWrite)
	}
	// The four buckets must sum to the total the upstream reported; reasoning
	// is already inside the output bucket and is not counted again.
	if u.In+u.CachedRead+u.CacheWrite+u.Out != 2306 {
		t.Errorf("the buckets sum to %d, not total_tokens 2306", u.In+u.CachedRead+u.CacheWrite+u.Out)
	}

	// Computed by hand from the published prices: 86 full-price input, 1920
	// cache read, 300 output.
	want := int64(86*2_500 + 1920*1_250 + 300*10_000)
	if got := upstreamCostOf(t, openAIPrice, u); got != want {
		t.Errorf("upstream cost = %d nano, computed by hand %d nano", got, want)
	}
}

func TestUsageBucketsAnthropic(t *testing.T) {
	// Anthropic semantics: input_tokens excludes both cache reads and cache
	// creation; all three are counted separately.
	body := []byte(`{"usage":{"input_tokens":12,"output_tokens":6,
	  "cache_read_input_tokens":2048,"cache_creation_input_tokens":1024}}`)

	u := proxy.ParseUsage(catalog.SurfaceMessages, body)
	if !u.Present {
		t.Fatal("usage was not parsed")
	}
	if u.In != 12 || u.CachedRead != 2048 || u.CacheWrite != 1024 || u.Out != 6 {
		t.Fatalf("buckets normalised wrongly: In=%d CachedRead=%d CacheWrite=%d Out=%d",
			u.In, u.CachedRead, u.CacheWrite, u.Out)
	}

	// Computed by hand from the published prices: a cache write costs 1.25x the
	// base input price and must not be dropped.
	want := int64(12*3_000 + 2048*300 + 1024*3_750 + 6*15_000)
	if got := upstreamCostOf(t, anthropicPrice, u); got != want {
		t.Errorf("upstream cost = %d nano, computed by hand %d nano (difference %d -- was the cache write dropped?)",
			got, want, want-got)
	}
}

// A self-contradictory upstream, reporting more cached tokens than prompt
// tokens, must not produce a negative bucket: billing refuses negatives
// outright, turning an ordinary request into a 500.
func TestUsageBucketsClampsInconsistentUpstream(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,
	  "prompt_tokens_details":{"cached_tokens":150}}}`)
	u := proxy.ParseUsage(catalog.SurfaceChat, body)
	if u.In < 0 {
		t.Fatalf("a negative input bucket was produced: In=%d", u.In)
	}
	if _, err := catalog.Compute(catalog.Flat(openAIPrice), catalog.Flat(openAIPrice), catalog.Tokens{
		In: u.In, Out: u.Out, CachedRead: u.CachedRead, CacheWrite: u.CacheWrite,
	}, catalog.Rates{FXRate: "1"}); err != nil {
		t.Errorf("contradictory input broke billing; it should clamp to 0 and bill normally: %v", err)
	}
}

// The streaming and non-streaming paths must normalise to the same result;
// each parses usage separately, which is the easiest place for them to drift.
func TestUsageBucketsStreamMatchesNonStream(t *testing.T) {
	cases := []struct {
		name    string
		surface catalog.Surface
		frames  []string
		want    proxy.Usage
	}{
		{
			name:    "openai, usage in the final chunk",
			surface: catalog.SurfaceChat,
			frames: []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n",
				`data: {"choices":[],"usage":{"prompt_tokens":2006,"completion_tokens":300,` +
					`"prompt_tokens_details":{"cached_tokens":1920}}}` + "\n\n",
			},
			want: proxy.Usage{In: 86, CachedRead: 1920, Out: 300, Present: true},
		},
		{
			name:    "anthropic message_start + message_delta",
			surface: catalog.SurfaceMessages,
			frames: []string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":12,` +
					`"cache_read_input_tokens":2048,"cache_creation_input_tokens":1024,"output_tokens":1}}}` + "\n\n",
				`data: {"type":"message_delta","usage":{"output_tokens":6}}` + "\n\n",
			},
			want: proxy.Usage{In: 12, CachedRead: 2048, CacheWrite: 1024, Out: 6, Present: true},
		},
		{
			name:    "openai streaming, advanced dimensions and tool usage",
			surface: catalog.SurfaceChat,
			frames: []string{
				`data: {"choices":[],"service_tier":"priority",` +
					`"tool_usage":{"web_search":{"num_requests":3}},"usage":{` +
					`"prompt_tokens":2006,"completion_tokens":300,` +
					`"prompt_tokens_details":{"cached_tokens":1920,"cache_write_tokens":20,"audio_tokens":10},` +
					`"completion_tokens_details":{"reasoning_tokens":128,"audio_tokens":20}}}` + "\n\n",
			},
			want: proxy.Usage{
				In: 66, CachedRead: 1920, CacheWrite: 20, Out: 300, Reasoning: 128,
				AudioIn: 10, AudioOut: 20, ServiceTier: "priority",
				ToolCalls: map[string]int64{"web_search": 3}, Present: true,
			},
		},
		{
			// Every Gemini chunk restates the totals so far rather than a delta,
			// so the last one wins. Accumulating them instead would multiply a
			// long stream's bill by the number of chunks.
			name:    "gemini, running totals restated on every chunk",
			surface: catalog.SurfaceGenerateContent,
			frames: []string{
				`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],` +
					`"usageMetadata":{"promptTokenCount":2006,"cachedContentTokenCount":1920,` +
					`"candidatesTokenCount":100}}` + "\n\n",
				`data: {"candidates":[{"content":{"parts":[{"text":" there"}]}}],` +
					`"usageMetadata":{"promptTokenCount":2006,"cachedContentTokenCount":1920,` +
					`"candidatesTokenCount":300,"thoughtsTokenCount":128,"toolUsePromptTokenCount":40}}` + "\n\n",
			},
			want: proxy.Usage{In: 126, CachedRead: 1920, Out: 428, Reasoning: 128, Present: true},
		},
		{
			name:    "anthropic streaming, cache TTLs, service tier and tool usage",
			surface: catalog.SurfaceMessages,
			frames: []string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":12,` +
					`"cache_read_input_tokens":2048,"cache_creation_input_tokens":1024,` +
					`"cache_creation":{"ephemeral_5m_input_tokens":900,"ephemeral_1h_input_tokens":124},` +
					`"service_tier":"batch","server_tool_use":{"web_search_requests":2},"output_tokens":1}}}` + "\n\n",
				`data: {"type":"message_delta","usage":{"output_tokens":6,` +
					`"service_tier":"batch","server_tool_use":{"web_search_requests":2}}}` + "\n\n",
			},
			want: proxy.Usage{
				In: 12, CachedRead: 2048, CacheWrite: 1024, Out: 6,
				CacheWrite5m: 900, CacheWrite1h: 124, ServiceTier: "batch",
				ToolCalls: map[string]int64{"web_search_requests": 2}, Present: true,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := proxy.AccumulateForTest(c.surface, c.frames)
			// Usage contains a map of tool usage, so == will not do.
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("streaming normalised to %+v, want %+v", got, c.want)
			}
		})
	}
}

// The Gemini protocol's own conventions, which match neither neighbour:
// promptTokenCount includes the cached part (as OpenAI does), thoughts are
// billed as output and sit *outside* candidatesTokenCount (as Anthropic's cache
// counts sit outside input), and tool-use input is a third addend that is in
// neither total.
func TestUsageBucketsGemini(t *testing.T) {
	body := []byte(`{"usageMetadata":{"promptTokenCount":2006,"cachedContentTokenCount":1920,
	  "candidatesTokenCount":300,"thoughtsTokenCount":128,"toolUsePromptTokenCount":40}}`)

	u := proxy.ParseUsage(catalog.SurfaceGenerateContent, body)
	if !u.Present {
		t.Fatal("usage was not parsed")
	}
	// 2006 - 1920 cached + 40 tool-use input = 126 uncached input.
	if u.In != 126 || u.CachedRead != 1920 {
		t.Errorf("input normalised wrongly: In=%d CachedRead=%d, want 126 and 1920", u.In, u.CachedRead)
	}
	// Thoughts are billed as output and are not already inside candidates.
	if u.Out != 428 || u.Reasoning != 128 {
		t.Errorf("output normalised wrongly: Out=%d Reasoning=%d, want 428 and 128", u.Out, u.Reasoning)
	}
	// This API has no cache-write count: context caching is billed by storage
	// duration, so there is nothing to report and a number here would be one
	// this gateway invented.
	if u.CacheWrite != 0 {
		t.Errorf("CacheWrite=%d, but nothing in this response reports one", u.CacheWrite)
	}

	// Computed by hand from a published tier: $1.25 input, $0.30 cache read,
	// $10.00 output per million tokens.
	// Prices chosen to divide evenly per token, so the expected value is
	// arithmetic rather than a claim about rounding.
	price := catalog.Price{
		InNanoPerMTok:        1_250_000_000,
		CacheReadNanoPerMTok: 300_000_000,
		OutNanoPerMTok:       10_000_000_000,
	}
	want := int64(126*1_250 + 1920*300 + 428*10_000)
	if got := upstreamCostOf(t, price, u); got != want {
		t.Errorf("upstream cost = %d nano, computed by hand %d nano (difference %d)", got, want, want-got)
	}
}

// A response with no usage at all leaves Present false, which is what sends
// settlement to the estimate rather than charging zero.
// Audio is priced on its own axis, and this protocol reports it as a modality
// breakdown *inside* the totals rather than as a separate count. Reading only
// the totals bills a spoken prompt at the text rate, which on a long recording
// is most of the input cost.
func TestUsageBucketsGeminiAudioModality(t *testing.T) {
	body := []byte(`{"usageMetadata":{"promptTokenCount":57610,"candidatesTokenCount":120,
	  "promptTokensDetails":[{"modality":"AUDIO","tokenCount":57600},{"modality":"TEXT","tokenCount":10}],
	  "candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":80},{"modality":"TEXT","tokenCount":40}]}}`)

	u := proxy.ParseUsage(catalog.SurfaceGenerateContent, body)
	if u.AudioIn != 57600 || u.AudioOut != 80 {
		t.Errorf("audio not split out: AudioIn=%d AudioOut=%d, want 57600 and 80", u.AudioIn, u.AudioOut)
	}
	// The breakdown is a subset of the parent count, so the totals do not move:
	// pricing subtracts the audio share before charging the text rate.
	if u.In != 57610 || u.Out != 120 {
		t.Errorf("totals moved: In=%d Out=%d, want 57610 and 120", u.In, u.Out)
	}
}

func TestUsageBucketsGeminiAbsent(t *testing.T) {
	for _, body := range []string{`{}`, `{"candidates":[]}`, `not json`} {
		if u := proxy.ParseUsage(catalog.SurfaceGenerateContent, []byte(body)); u.Present {
			t.Errorf("%s: reported usage where the response has none", body)
		}
	}
}

// The same self-contradiction guard as the other protocols: an upstream
// reporting more cached tokens than prompt tokens must not produce a negative
// bucket, because billing refuses negatives and an ordinary request would 500.
func TestUsageBucketsGeminiClampsInconsistentUpstream(t *testing.T) {
	body := []byte(`{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":150,
	  "candidatesTokenCount":10}}`)
	u := proxy.ParseUsage(catalog.SurfaceGenerateContent, body)
	if u.In < 0 {
		t.Fatalf("a negative input bucket was produced: In=%d", u.In)
	}
}
