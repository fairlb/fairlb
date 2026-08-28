package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fairlb/fairlb/foundation/money"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// nanoDecimal renders a nano integer as a decimal string in the main unit,
// trailing zeros removed. Money always crosses the wire as a string: as a JSON
// number it goes through the client's float parser, and then no two clients
// agree on the low decimal places.

// The injection layer. It performs no cross-protocol conversion; it does five
// things and no more: rewrite the authentication header, map the model name,
// force stream_options.include_usage on the OpenAI chat surface, parse usage
// for both dialects, and pass SSE through byte for byte. Apart from the model
// name and that one injected field the request body travels *unchanged*, so a
// new upstream parameter needs no gateway change. See
// docs/design/same-protocol-passthrough.md.

// Protocol is an upstream protocol dialect. Surface and protocol always agree;
// candidate filtering already guarantees it in the catalog.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGemini    Protocol = "gemini"
	// ProtocolVideo is the video job plane, not a dialect. Nothing on it goes
	// through RewriteRequest: a video request is shaped by its vendor's mapper
	// instead (ADR-0218, ADR-0219).
	ProtocolVideo Protocol = "video"
)

// Usage is the normalised usage of both dialects. The four billing buckets are
// pairwise disjoint.
//
// Normalising here is a hard requirement rather than a matter of taste, because
// the two dialects disagree about what "input" contains:
//   - OpenAI: prompt_tokens *includes* prompt_tokens_details.cached_tokens
//   - Anthropic: input_tokens *excludes* cache_read_input_tokens and
//     cache_creation_input_tokens; all three are counted separately
//
// Dropping each dialect's raw fields into one shape would count OpenAI's cached
// tokens twice (once at full price, once at the cache price) and Anthropic's
// cache writes not at all.
type Usage struct {
	In         int64 // uncached input (on OpenAI: prompt_tokens minus cached_tokens)
	Out        int64
	CachedRead int64
	CacheWrite int64 // input written to cache; priced separately upstream, so it cannot join In
	// Reasoning is recorded but never billed: OpenAI's completion_tokens
	// already includes it, so adding the column would double-count -- the same
	// trap as the two cache fields above.
	Reasoning int64

	// Pricing dimensions below. A zero value behaves exactly as if the
	// dimension did not exist.

	// AudioIn and AudioOut are audio tokens; on OpenAI they are a *subset* of
	// prompt and completion. They get their own fields only so they can carry
	// their own unit price; without one, billing charges them at the text
	// price.
	AudioIn  int64
	AudioOut int64
	// ImageIn is image input tokens, and like AudioIn it is a *subset* of the
	// prompt rather than something alongside it. Image models charge it well
	// above their text input rate -- gpt-image-2 at 8 against 5 per million --
	// so leaving it folded into In undercharges every image request. The
	// upstream has always reported it; there was simply nowhere to put it.
	ImageIn int64
	// ImageOut is image *output* tokens: what a model that generates images
	// reports for the pixels it produced, a subset of Out the same way.
	//
	// The gap it closes is larger than the input one. The reference dataset
	// prices one Gemini image model's output at 30 against a text output of
	// 2.5, and gpt-image at 32 against 10 -- so a generated image folded into
	// Out was billed at somewhere between a third and a twelfth of its cost.
	ImageOut int64
	// CacheWrite5m and CacheWrite1h are Anthropic's two cache-write TTLs, and
	// they are priced differently. The 1h tier costs more, so a single
	// CacheWrite bucket would charge the whole lot at the 5m price and
	// undercharge.
	CacheWrite5m int64
	CacheWrite1h int64
	// ServiceTier is the tier the upstream reports it actually served:
	// batch is roughly half price, priority carries a premium. Charging batch
	// at list price overcharges the organization; costing priority at list price
	// inflates the reported margin.
	ServiceTier string
	// ToolCalls is tool usage, tool name to count. Both protocols report it
	// *outside* usage, and its unit is requests rather than tokens, so the four
	// buckets cannot hold it. Some relays have been observed attaching an
	// image_generation tool to every request by default, with nobody having
	// configured it.
	ToolCalls map[string]int64
	// Present false means the upstream returned no usage, so the caller must
	// fall back to an estimate and mark the row estimated.
	Present bool
}

// RewriteRequest rewrites the request body: the model name becomes the
// upstream's real name, the OpenAI chat surface gains include_usage, and a
// provider that declares an envelope gets that envelope applied.
//
// *This is the only function in the gateway that edits a request body*, and it
// is worth keeping that true. Grepping for what happens to a caller's payload
// should reach one place and find the whole answer; the moment there are two,
// the answer is whatever the reader stops looking after.
//
// It uses json.Decoder with UseNumber rather than unmarshalling into a struct:
// the gateway does not know most of the fields in a request body, and a struct
// would drop them on the round trip. UseNumber additionally keeps large
// integers -- a seed, a very large max_tokens -- from losing precision via
// float64.
func RewriteRequest(
	surface catalog.Surface, body []byte, upstreamModel string, stream bool, tp catalog.Transport,
) ([]byte, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("proxy: invalid JSON in request body: %w", err)
	}

	// On the Gemini protocol the model is a path segment, not a body field. It is
	// not that the field is optional there: writing one puts a key in the body
	// that the API does not define, and the address is where the upstream reads
	// the model from anyway.
	if !geminiPathModelSurface(surface) {
		doc["model"] = upstreamModel
	} else {
		delete(doc, "model")
		// batchEmbedContents repeats the path model in every child request.
		// Rewriting those envelope fields is still same-protocol addressing,
		// not semantic translation, and is required by Gemini's validator.
		if surface == catalog.SurfaceGeminiBatchEmbedContents {
			if requests, ok := doc["requests"].([]any); ok {
				for _, raw := range requests {
					if request, ok := raw.(map[string]any); ok {
						request["model"] = geminiModelName(upstreamModel)
					}
				}
			}
		}
	}

	// Streaming chat/completions must carry include_usage or the final chunk
	// has no usage and settlement has nothing actual to charge against. The API
	// contract states that a caller-supplied value is overwritten: this is a
	// precondition of billing, not an option.
	//
	// The criterion is the *surface*, not the protocol. Responses belongs to the
	// same openai protocol as chat, but it has no stream_options parameter at all
	// -- its usage arrives in the terminal response.completed event -- so
	// injecting per protocol would hand the upstream a field it does not know.
	if surface == catalog.SurfaceChat && stream {
		doc["stream_options"] = map[string]any{"include_usage": true}
	}

	applyEnvelope(doc, tp, stream)

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("proxy: re-encoding request body: %w", err)
	}
	return out, nil
}

func geminiPathModelSurface(surface catalog.Surface) bool {
	switch surface {
	case catalog.SurfaceGenerateContent, catalog.SurfaceGeminiCountTokens,
		catalog.SurfaceGeminiEmbedContent, catalog.SurfaceGeminiBatchEmbedContents:
		return true
	default:
		return false
	}
}

func geminiModelName(model string) string {
	if strings.HasPrefix(model, "models/") {
		return model
	}
	return "models/" + model
}

// applyEnvelope re-cuts the request the way a hosted platform requires.
//
// Every line below moves a value between the envelope and the body, or writes a
// constant the platform fixes. None of them reads what the caller asked for, and
// that is the test to apply to anything added here: if a change would need to
// look at a message, a parameter or a tool definition to decide, it is a
// translation and it does not belong in this gateway at all.
//
//   - The model becomes a path segment on these platforms, so it is removed from
//     the body. Leaving it there is not harmless: both refuse the request.
//   - The api-version, which is a header on the direct endpoint, is a required
//     body field here. Same setting, same value space, different carrier.
//   - One of the two chooses streaming by endpoint rather than by a body flag,
//     and refuses a body that also carries the flag. The other keeps the flag,
//     and needs it set, so the flag is written rather than assumed.
func applyEnvelope(doc map[string]any, tp catalog.Transport, stream bool) {
	env := tp.BodyEnvelope()
	if env == catalog.EnvelopeNone {
		return
	}
	delete(doc, "model")
	doc["anthropic_version"] = tp.EnvelopeAnthropicVersion()
	if env == catalog.EnvelopeBedrock {
		delete(doc, "stream")
		return
	}
	doc["stream"] = stream
}

// ModelOf reads the model name out of the request body, which is the input to
// both routing and billing.
func ModelOf(body []byte) (string, error) {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("proxy: invalid JSON in request body: %w", err)
	}
	if probe.Model == "" {
		return "", fmt.Errorf("proxy: missing model field")
	}
	return probe.Model, nil
}

// StreamOf reports whether the request asked for streaming.
func StreamOf(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

// MaxTokensOf reads the caller's output cap, used to size the hold. The field
// name is shared but its standing differs: OpenAI's max_tokens is optional and
// falls back to the model's own cap, Anthropic's is mandatory.
func MaxTokensOf(body []byte) int64 {
	var probe struct {
		MaxTokens           int64 `json:"max_tokens"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
		// The Responses surface calls it max_output_tokens.
		MaxOutputTokens int64 `json:"max_output_tokens"`
		// Gemini nests its cap under generationConfig. Reading all of them from
		// one probe rather than branching on the surface keeps this a question
		// about the body: no caller sends two of these, and a body that did
		// would be answering the same question twice.
		GenerationConfig struct {
			MaxOutputTokens int64 `json:"maxOutputTokens"`
		} `json:"generationConfig"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.GenerationConfig.MaxOutputTokens > 0 {
		return probe.GenerationConfig.MaxOutputTokens
	}
	if probe.MaxOutputTokens > 0 {
		return probe.MaxOutputTokens
	}
	if probe.MaxCompletionTokens > 0 {
		return probe.MaxCompletionTokens
	}
	return probe.MaxTokens
}

// EndUserOf reads the end-user attribution identifier: `user` on the OpenAI
// surfaces, `metadata.user_id` on Anthropic's. The X-End-User-Id header outranks
// both and is handled in the handler.
func EndUserOf(protocol Protocol, body []byte) string {
	switch protocol {
	case ProtocolAnthropic:
		var probe struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(body, &probe)
		return probe.Metadata.UserID
	default:
		var probe struct {
			User string `json:"user"`
		}
		_ = json.Unmarshal(body, &probe)
		return probe.User
	}
}

// subsetIn derives the uncached input bucket on the "details are a subset of
// the parent" reading.
//
// Both OpenAI surfaces use that reading -- their published schema says the
// cached tokens are present in the prompt -- so the subtraction is required.
// But a result below zero must *not* be clamped to zero: that means this
// upstream is really reporting them additively, the way Anthropic does (relays
// that rewrite usage genuinely do this), in which case the parent already is
// the uncached part and clamping would give those tokens away for free. The
// specification decides what should be, the upstream decides what is, and both
// readings have to come out right.
func subsetIn(parent, cached, cacheWrite int64) int64 {
	sub := cached + cacheWrite
	if sub > parent {
		return parent // additive reading: parent is already the uncached part
	}
	return parent - sub
}

// ParseUsage parses the usage of an upstream response.
//
// The criterion is the *surface*, not the protocol: chat/completions and
// responses both belong to the openai protocol, yet their usage field names and
// nesting differ completely. Branching on protocol would leave Responses' usage
// entirely unparsed, which looks like "the upstream sent no usage" and sends
// every such request down the estimation fallback -- and an estimate can be off
// by an order of magnitude.
//
// With no usage returned, Present is false and the caller estimates and marks
// the row estimated.
//
// Every surface must appear in this switch by name. A surface that falls
// through to the default arm is not "unparsed" -- it is parsed by the *wrong*
// arm, and the failure is silent in a specific way: the usage object exists, so
// Present comes back true, while the field names do not match and every count
// reads zero. The caller then sees a fully reported usage of nothing, skips the
// estimate, and bills the request at zero. This is not hypothetical; it is what
// the images surface did until it was given its own arm.
func ParseUsage(surface catalog.Surface, body []byte) Usage {
	switch surface {
	case catalog.SurfaceVideo:
		// Deliberately empty, and deliberately not the default arm. A video
		// job's charge is a pure function of its admitted parameters, computed
		// before the upstream is called (ADR-0220), so there is no usage object
		// to read and Present must stay false. Reaching the default arm instead
		// would come back Present with every count at zero, which is the exact
		// silent failure described above.
		return Usage{}
	case catalog.SurfaceImages, catalog.SurfaceImagesEdit:
		// Image responses use the input_tokens/output_tokens spelling, which no
		// other openai-protocol surface uses.
		return ParseImageUsage(body)
	case catalog.SurfaceResponses, catalog.SurfaceResponsesCompact:
		var r struct {
			Usage       *responsesUsage      `json:"usage"`
			ServiceTier string               `json:"service_tier"`
			ToolUsage   map[string]any       `json:"tool_usage"`
			Output      []responseOutputItem `json:"output"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return Usage{}
		}
		images := countGeneratedImages(r.Output)
		if r.Usage == nil {
			// No usage object, which is the ordinary "the caller must
			// estimate" answer -- except that an image the tool produced is
			// not estimable and is not in that object anyway. Present stays
			// false so the text side is still estimated; the tool count rides
			// along, and the estimate paths keep it rather than overwriting
			// it. Returning early here instead billed those images at nothing.
			if images == 0 {
				return Usage{}
			}
			u := Usage{}
			addGeneratedImages(&u, images)
			return u
		}
		u := r.Usage.usage(r.ServiceTier, r.ToolUsage)
		addGeneratedImages(&u, images)
		return u
	case catalog.SurfaceGenerateContent, catalog.SurfaceGeminiEmbedContent:
		return parseGeminiUsage(body)
	case catalog.SurfaceGeminiBatchEmbedContents:
		return parseGeminiBatchUsage(body)
	case catalog.SurfaceGeminiInteractions:
		return parseGeminiInteractionUsage(body)
	case catalog.SurfaceMessages:
		var r struct {
			Usage *anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
			return Usage{}
		}
		return r.Usage.usage()
	default:
		var r struct {
			Usage       *openAIUsage   `json:"usage"`
			ServiceTier string         `json:"service_tier"`
			ToolUsage   map[string]any `json:"tool_usage"`
		}
		if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
			return Usage{}
		}
		return r.Usage.usage(r.ServiceTier, r.ToolUsage)
	}
}

// AnnotateUsage hangs the gateway's extension -- the estimation flag and the
// charged amount -- off the response body's usage object. The rest of the body
// is preserved as is.
//
// *Only the `usage` object, never Gemini's `usageMetadata`.* That protocol's
// official client validates the response against generated models declared
// `extra='forbid'`, so one added key does not travel unnoticed the way it does
// on the other two: `GenerateContentResponse.model_validate` raises before the
// caller sees any text. Verified against google-genai, not inferred. The cost of
// leaving that protocol un-annotated is that its callers read the charge from
// the usage log or the console rather than off the response, which is a smaller
// cost than every response failing to parse.
func AnnotateUsage(body []byte, estimated bool, costNano int64, currency string) []byte {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return body // unparseable: return it untouched, an annotation must never block a response
	}
	// Two protocols spell it "usage"; the Gemini one spells it "usageMetadata"
	// and is deliberately not annotated (see above), so a document that has only
	// the latter comes back untouched.
	usage, ok := doc["usage"].(map[string]any)
	if !ok {
		return body
	}
	usage["fairlb"] = map[string]any{
		"estimated": estimated,
		"cost":      money.FormatNanoExact(costNano),
		"currency":  currency,
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// BillingTokens folds normalised usage into billable quantities, pricing
// dimensions included.
//
// It is one method rather than the same expression written out on each of the
// three paths, which is what it used to be: adding a bucket then meant
// remembering three places, and missing one shows up as "that path charges a
// little less" -- no error, nothing obvious, discovered only at reconciliation.
func (u Usage) BillingTokens() catalog.Tokens {
	return catalog.Tokens{
		In: u.In, Out: u.Out, CachedRead: u.CachedRead, CacheWrite: u.CacheWrite,
		AudioIn: u.AudioIn, AudioOut: u.AudioOut,
		ImageIn: u.ImageIn, ImageOut: u.ImageOut,
		CacheWrite5m: u.CacheWrite5m, CacheWrite1h: u.CacheWrite1h,
		ServiceTier: u.ServiceTier, ToolCalls: u.ToolCalls,
	}
}

// toolCallCounts folds each vendor's differently shaped tool usage into "tool
// name to count".
//
// The key names and the nesting differ between vendors and are still moving
// (Anthropic's `server_tool_use`, an OpenAI-compatible relay's
// `tool_usage.web_search.num_requests`), so values are read by *shape* rather
// than by enumerating known key names. Two forms are recognised:
//
//	{"web_search_requests": 3}                 the count directly
//	{"web_search": {"num_requests": 3}}        one level of nesting
//
// Anything unrecognised is skipped whole; no count is invented. A new shape
// must first gain a parsing regression test built from a real payload before it
// reaches billable usage -- guessing the count wrong bills the wrong amount.
func toolCallCounts(raw map[string]any) map[string]int64 {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]int64, len(raw))
	for name, v := range raw {
		switch t := v.(type) {
		case float64:
			if t > 0 {
				out[name] = int64(t)
			}
		case map[string]any:
			if n, ok := t["num_requests"].(float64); ok && n > 0 {
				out[name] = int64(n)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseGeminiUsage normalises the Gemini protocol's usageMetadata into the four
// buckets.
//
// The conventions have to be read off the API rather than assumed from the
// neighbouring protocols, because this one differs from both:
//
//   - promptTokenCount is the total input and *includes* the cached part, so
//     cached tokens are subtracted out the way the OpenAI protocol's are. Getting
//     this backwards does not produce an obviously broken number; it produces a
//     plausible one, wrong by exactly the cached amount.
//   - thoughtsTokenCount is billed as output and is *not* included in
//     candidatesTokenCount, so the two are added rather than one subtracted from
//     the other. It is also reported as reasoning, which is a description of the
//     same tokens rather than a fifth bucket.
//   - toolUsePromptTokenCount is input that the model itself caused, and it sits
//     outside promptTokenCount. Left out, it is inference this deployment paid
//     for and did not charge for.
//   - There is no cache-write count. Context caching on this API is billed by
//     storage duration rather than by tokens written, so nothing here can report
//     it, and inventing a number would put an invented figure on a bill.
//   - The per-modality breakdowns are a *subset* of their parent count, the way
//     the OpenAI protocol's audio split is, and audio is priced on its own axis.
//     Reading only the totals bills a spoken prompt at the text rate, which is
//     the failure the audio buckets exist to prevent.
func parseGeminiUsage(body []byte) Usage {
	var r struct {
		UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.UsageMetadata == nil {
		return Usage{}
	}
	return r.UsageMetadata.usage()
}

// parseGeminiBatchUsage accepts both the native top-level aggregate and the
// per-result usageMetadata shape emitted by some Gemini-compatible endpoints.
// A top-level aggregate wins because adding both representations would charge
// the same work twice. Without one, every item is summed into one synchronous
// batch charge; an item without usage remains unbillable rather than estimated
// independently from an embedding vector.
func parseGeminiBatchUsage(body []byte) Usage {
	var r struct {
		UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
		Embeddings    []struct {
			UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
		} `json:"embeddings"`
		Responses []struct {
			UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Usage{}
	}
	if r.UsageMetadata != nil {
		return r.UsageMetadata.usage()
	}
	var total Usage
	for _, item := range r.Embeddings {
		if item.UsageMetadata != nil {
			total = addUsage(total, item.UsageMetadata.usage())
		}
	}
	for _, item := range r.Responses {
		if item.UsageMetadata != nil {
			total = addUsage(total, item.UsageMetadata.usage())
		}
	}
	return total
}

func addUsage(a, b Usage) Usage {
	a.In += b.In
	a.Out += b.Out
	a.CachedRead += b.CachedRead
	a.CacheWrite += b.CacheWrite
	a.Reasoning += b.Reasoning
	a.AudioIn += b.AudioIn
	a.AudioOut += b.AudioOut
	a.ImageIn += b.ImageIn
	a.CacheWrite5m += b.CacheWrite5m
	a.CacheWrite1h += b.CacheWrite1h
	a.Present = a.Present || b.Present
	if a.ServiceTier == "" {
		a.ServiceTier = b.ServiceTier
	} else if b.ServiceTier != "" && a.ServiceTier != b.ServiceTier {
		a.ServiceTier = ""
	}
	if len(b.ToolCalls) > 0 {
		if a.ToolCalls == nil {
			a.ToolCalls = make(map[string]int64, len(b.ToolCalls))
		}
		for tool, count := range b.ToolCalls {
			a.ToolCalls[tool] += count
		}
	}
	return a
}

// parseGeminiInteractionUsage reads the Interactions API's snake_case usage
// object. These totals are intentionally kept separate from usageMetadata:
// accepting one shape as the other would produce a present-but-zero bill.
func parseGeminiInteractionUsage(body []byte) Usage {
	var r struct {
		Usage       *interactionUsage `json:"usage"`
		ServiceTier string            `json:"service_tier"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return Usage{}
	}
	return r.Usage.usage(r.ServiceTier)
}

type interactionUsage struct {
	TotalInputTokens   int64                      `json:"total_input_tokens"`
	TotalOutputTokens  int64                      `json:"total_output_tokens"`
	TotalCachedTokens  int64                      `json:"total_cached_tokens"`
	TotalThoughtTokens int64                      `json:"total_thought_tokens"`
	TotalToolUseTokens int64                      `json:"total_tool_use_tokens"`
	InputByModality    []interactionModalityCount `json:"input_tokens_by_modality"`
	OutputByModality   []interactionModalityCount `json:"output_tokens_by_modality"`
	ToolUseByModality  []interactionModalityCount `json:"tool_use_tokens_by_modality"`
	GroundingToolCount []struct {
		Type  string `json:"type"`
		Count int64  `json:"count"`
	} `json:"grounding_tool_count"`
}

type interactionModalityCount struct {
	Modality string `json:"modality"`
	Tokens   int64  `json:"tokens"`
}

func (u interactionUsage) usage(serviceTier string) Usage {
	toolCalls := make(map[string]int64, len(u.GroundingToolCount))
	for _, tool := range u.GroundingToolCount {
		if tool.Type != "" && tool.Count > 0 {
			toolCalls[tool.Type] += tool.Count
		}
	}
	if len(toolCalls) == 0 {
		toolCalls = nil
	}
	return Usage{
		In: subsetIn(u.TotalInputTokens, u.TotalCachedTokens, 0) + u.TotalToolUseTokens,
		// Interactions reports generated output and billable thinking as
		// separate totals. Unlike OpenAI's reasoning breakdown, thought tokens
		// are not included in total_output_tokens and must be added once.
		Out:        u.TotalOutputTokens + u.TotalThoughtTokens,
		CachedRead: u.TotalCachedTokens,
		Reasoning:  u.TotalThoughtTokens,
		AudioIn: interactionModalityTokens(u.InputByModality, "audio") +
			interactionModalityTokens(u.ToolUseByModality, "audio"),
		AudioOut: interactionModalityTokens(u.OutputByModality, "audio"),
		ImageIn: interactionModalityTokens(u.InputByModality, "image") +
			interactionModalityTokens(u.ToolUseByModality, "image"),
		ImageOut:    interactionModalityTokens(u.OutputByModality, "image"),
		ServiceTier: serviceTier,
		ToolCalls:   toolCalls,
		Present:     true,
	}
}

func interactionModalityTokens(details []interactionModalityCount, modality string) int64 {
	var n int64
	for _, detail := range details {
		if strings.EqualFold(detail.Modality, modality) {
			n += detail.Tokens
		}
	}
	return n
}

// geminiUsageMetadata is that protocol's usage object. It is one type, read by
// both the buffered parser and the streaming accumulator: two spellings of the
// same arithmetic drift, and the way that drift shows is one path charging a
// little less than the other with nothing failing.
type geminiUsageMetadata struct {
	PromptTokenCount        int64                 `json:"promptTokenCount"`
	TotalTokenCount         int64                 `json:"totalTokenCount"`
	CachedContentTokenCount int64                 `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int64                 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64                 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64                 `json:"toolUsePromptTokenCount"`
	PromptTokensDetails     []geminiModalityCount `json:"promptTokensDetails"`
	CandidatesTokensDetails []geminiModalityCount `json:"candidatesTokensDetails"`
}

// geminiModalityCount is one entry of a per-modality breakdown.
type geminiModalityCount struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
}

// modalityTokens totals one modality's entries of a breakdown. A modality this
// function is not asked about is ignored rather than folded in: a new one the
// vendor adds is priced at the text rate until somebody teaches the caller
// about it, which is the direction that does not invent a charge.
func modalityTokens(details []geminiModalityCount, modality string) int64 {
	var n int64
	for _, d := range details {
		if strings.EqualFold(d.Modality, modality) {
			n += d.TokenCount
		}
	}
	return n
}

// usage normalises this protocol's counts into the four buckets.
func (m geminiUsageMetadata) usage() Usage {
	// Embedding endpoints may report only totalTokenCount. With no generation
	// output in that operation, the total is input; when the detailed fields
	// exist they remain authoritative and totalTokenCount is only a checksum.
	if m.PromptTokenCount == 0 && m.CandidatesTokenCount == 0 &&
		m.ThoughtsTokenCount == 0 && m.ToolUsePromptTokenCount == 0 &&
		m.TotalTokenCount > 0 {
		return Usage{
			In:         subsetIn(m.TotalTokenCount, m.CachedContentTokenCount, 0),
			CachedRead: m.CachedContentTokenCount,
			AudioIn:    modalityTokens(m.PromptTokensDetails, "AUDIO"),
			ImageIn:    modalityTokens(m.PromptTokensDetails, "IMAGE"),
			Present:    true,
		}
	}
	return Usage{
		In:         subsetIn(m.PromptTokenCount, m.CachedContentTokenCount, 0) + m.ToolUsePromptTokenCount,
		CachedRead: m.CachedContentTokenCount,
		Out:        m.CandidatesTokenCount + m.ThoughtsTokenCount,
		Reasoning:  m.ThoughtsTokenCount,
		AudioIn:    modalityTokens(m.PromptTokensDetails, "AUDIO"),
		AudioOut:   modalityTokens(m.CandidatesTokensDetails, "AUDIO"),
		ImageIn:    modalityTokens(m.PromptTokensDetails, "IMAGE"),
		// The candidates breakdown is where a generated image lands, and this
		// protocol is how Google's image models are reached at all -- they sit
		// on generate_content beside the text models. Left unread, every image
		// they produce is billed at the model's text output rate.
		ImageOut: modalityTokens(m.CandidatesTokensDetails, "IMAGE"),
		Present:  true,
	}
}

// imageGenerationTool is the name a generated image is counted under.
//
// It is the tool's own name upstream, so an operator prices it by writing the
// same word into the model's per-call tool rates that they read in their own
// request.
const imageGenerationTool = "image_generation"

// countGeneratedImages counts the images a Responses answer produced through
// the image generation tool.
//
// This is the one surface where an image is produced and the upstream reports
// *nothing* about it: its usage object carries the text model's tokens and
// neither image tokens nor the tool's cost. Left alone, a caller generates
// images through this gateway and pays only for the prose around them.
//
// So the count comes from the output items, which is the same rule the images
// surface settles on -- how many the response actually carried -- applied to
// the place this surface puts them. Partial renders stream under a different
// event and never appear in a completed answer's output array, so they cannot
// be counted twice.
func countGeneratedImages(items []responseOutputItem) int64 {
	var n int64
	for _, it := range items {
		if it.Type == imageGenerationTool+"_call" {
			n++
		}
	}
	return n
}

// responseOutputItem is one entry of a Responses answer's output array, read
// only for what kind of thing it is.
type responseOutputItem struct {
	Type string `json:"type"`
}

// addGeneratedImages folds a generated-image count into tool usage, leaving a
// count the upstream already reported alone.
//
// Larger wins rather than sum, for the same reason mergeToolCalls does it that
// way: a relay that does report tool_usage is reporting a total, and adding the
// output items to it would charge each image twice.
func addGeneratedImages(u *Usage, images int64) {
	if images <= 0 {
		return
	}
	if u.ToolCalls == nil {
		u.ToolCalls = make(map[string]int64, 1)
	}
	if images > u.ToolCalls[imageGenerationTool] {
		u.ToolCalls[imageGenerationTool] = images
	}
}
