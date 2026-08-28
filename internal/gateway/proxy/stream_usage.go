package proxy

import (
	"bytes"
	"encoding/json"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Shadow parsing of usage out of a stream.
//
// Each surface puts usage somewhere different. The criterion is the *surface*,
// not the protocol: chat and responses both belong to the openai protocol, yet
// their event shapes are entirely different.
//   - chat/completions: in the final chunk, where choices is an empty array and
//     usage is populated. The forced injection of
//     stream_options.include_usage is what guarantees it appears at all.
//   - messages: split across two events -- message_start carries input_tokens,
//     message_delta carries the running output_tokens.
//   - responses: only in the terminal `response.completed` event, and needing
//     no opt-in. Of that surface's event types it is the only one carrying
//     usage, under `.response.usage`.
//   - images: only in the terminal `image_generation.completed` event, in the
//     input_tokens/output_tokens spelling no other openai-protocol surface
//     uses. An image stream carries no text at all, so there is nothing for the
//     estimation fallback to work from and the hold has to be the bill instead.
//
// All parsing is best effort: any chunk that will not parse is skipped and the
// byte pass-through is never affected. If the upstream never sends usage,
// Present stays false and an estimate is used and marked as such.

// usageAccumulator accumulates usage and the produced text across chunks.
// The buckets mean what they mean in Usage -- the four are pairwise disjoint --
// and each dialect's consume does the normalising.
type usageAccumulator struct {
	in           int64
	out          int64
	cachedRead   int64
	cacheWrite   int64
	reasoning    int64
	audioIn      int64
	audioOut     int64
	imageIn      int64
	imageOut     int64
	cacheWrite5m int64
	cacheWrite1h int64
	serviceTier  string
	toolCalls    map[string]int64
	seen         bool
	resourceID   string
}

// consume parses one SSE frame and appends the text delta to textBuf, which is
// the input to the estimation fallback.
//
// *Every surface must appear in this switch by name*, for the reason ParseUsage
// records about its own switch: a surface that falls through to the default arm
// is not left unparsed, it is parsed by the *wrong* arm. The images surface
// proved it here as well as there. An image stream's terminal
// `image_generation.completed` frame does carry a usage object -- spelled
// input_tokens/output_tokens -- so consumeOpenAI found one, read every field it
// knows as zero, and reported Present. The request then settled a real
// generation at nothing, with estimated false and a usage row that looked
// perfectly healthy. AllSurfaces() holds this switch to the rule.
func (a *usageAccumulator) consume(
	surface catalog.Surface, frame []byte, textBuf *bytes.Buffer,
) {
	for _, data := range sseDataLines(frame) {
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		switch surface {
		case catalog.SurfaceMessages:
			a.consumeAnthropic(data, textBuf)
		case catalog.SurfaceResponses, catalog.SurfaceResponsesCompact:
			// Compact shares the arm rather than the default one, exactly as it
			// shares ParseUsage's. Nothing stops a caller putting stream:true
			// on it, and its events are that surface's, not chat's.
			a.consumeResponses(data, textBuf)
		case catalog.SurfaceGeminiInteractions:
			a.consumeInteraction(data, textBuf)
		case catalog.SurfaceGenerateContent:
			a.consumeGemini(data, textBuf)
		case catalog.SurfaceImages, catalog.SurfaceImagesEdit:
			a.consumeImage(data)
		default:
			a.consumeOpenAI(data, textBuf)
		}
	}
}

// consumeResponses parses the Responses surface's event stream.
//
// The intermediate events -- output_item.added, content_part.added,
// output_text.done and the rest -- are all ignored. Only two are read:
// `response.output_text.delta` accumulates text for the estimation fallback,
// and `response.completed` carries usage. *An interrupted stream therefore
// yields not a single token*: there is no running count to fall back on, and
// the whole request has to be estimated.
func (a *usageAccumulator) consumeResponses(data []byte, textBuf *bytes.Buffer) {
	var ev struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response *struct {
			ID          string               `json:"id"`
			ServiceTier string               `json:"service_tier"`
			ToolUsage   map[string]any       `json:"tool_usage"`
			Usage       *responsesUsage      `json:"usage"`
			Output      []responseOutputItem `json:"output"`
		} `json:"response"`
		ServiceTier string         `json:"service_tier"`
		ToolUsage   map[string]any `json:"tool_usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	if ev.Response != nil && ev.Response.ID != "" {
		a.resourceID = ev.Response.ID
	}
	switch ev.Type {
	case "response.output_text.delta":
		textBuf.WriteString(ev.Delta)
	case "response.completed", "response.incomplete":
		// incomplete carries usage too: a response cut short by
		// max_output_tokens still cost something.
		if ev.Response == nil || ev.Response.Usage == nil {
			return
		}
		serviceTier := ev.Response.ServiceTier
		if serviceTier == "" {
			serviceTier = ev.ServiceTier
		}
		u := ev.Response.Usage.usage(serviceTier, ev.Response.ToolUsage)
		// The same count the buffered arm takes, from the same place. An image
		// produced by the generation tool is reported nowhere in this surface's
		// usage object, so the completed answer's output items are the only
		// record that it happened at all.
		addGeneratedImages(&u, countGeneratedImages(ev.Response.Output))
		a.setUsage(u)
		a.mergeToolCalls(toolCallCounts(ev.ToolUsage))
	}
}

func (a *usageAccumulator) consumeInteraction(data []byte, textBuf *bytes.Buffer) {
	var ev struct {
		ID          string          `json:"id"`
		Delta       json.RawMessage `json:"delta"`
		Usage       json.RawMessage `json:"usage"`
		TotalUsage  json.RawMessage `json:"total_usage"`
		ServiceTier string          `json:"service_tier"`
		Metadata    *struct {
			TotalUsage json.RawMessage `json:"total_usage"`
		} `json:"metadata"`
		Interaction *struct {
			ID          string          `json:"id"`
			Usage       json.RawMessage `json:"usage"`
			ServiceTier string          `json:"service_tier"`
		} `json:"interaction"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	if len(ev.Delta) > 0 {
		var text string
		if json.Unmarshal(ev.Delta, &text) != nil {
			var delta struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(ev.Delta, &delta)
			text = delta.Text
		}
		textBuf.WriteString(text)
	}
	if ev.ID != "" {
		a.resourceID = ev.ID
	}
	usage := ev.Usage
	serviceTier := ev.ServiceTier
	if len(ev.TotalUsage) > 0 {
		usage = ev.TotalUsage
	}
	if ev.Metadata != nil && len(ev.Metadata.TotalUsage) > 0 {
		usage = ev.Metadata.TotalUsage
	}
	if ev.Interaction != nil {
		if ev.Interaction.ID != "" {
			a.resourceID = ev.Interaction.ID
		}
		if len(ev.Interaction.Usage) > 0 {
			usage = ev.Interaction.Usage
		}
		if ev.Interaction.ServiceTier != "" {
			serviceTier = ev.Interaction.ServiceTier
		}
	}
	if len(usage) == 0 {
		return
	}
	raw, err := json.Marshal(struct {
		Usage       json.RawMessage `json:"usage"`
		ServiceTier string          `json:"service_tier,omitempty"`
	}{Usage: usage, ServiceTier: serviceTier})
	if err == nil {
		a.setUsage(parseGeminiInteractionUsage(raw))
	}
}

func (a *usageAccumulator) consumeOpenAI(data []byte, textBuf *bytes.Buffer) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage       *openAIUsage   `json:"usage"`
		ServiceTier string         `json:"service_tier"`
		ToolUsage   map[string]any `json:"tool_usage"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	for _, c := range chunk.Choices {
		textBuf.WriteString(c.Delta.Content)
	}
	if chunk.Usage != nil {
		a.setUsage(chunk.Usage.usage(chunk.ServiceTier, chunk.ToolUsage))
	}
}

func (a *usageAccumulator) consumeAnthropic(data []byte, textBuf *bytes.Buffer) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Delta *struct {
			Text string `json:"text"`
		} `json:"delta"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			a.setUsage(ev.Message.Usage.usage())
		}
	case "content_block_delta":
		if ev.Delta != nil {
			textBuf.WriteString(ev.Delta.Text)
		}
	case "message_delta":
		// message_delta reports the *running* output total, so this
		// overwrites rather than adds.
		if ev.Usage != nil {
			u := ev.Usage.usage()
			a.seen = true
			a.out = u.Out
			if u.ServiceTier != "" {
				a.serviceTier = u.ServiceTier
			}
			a.mergeToolCalls(u.ToolCalls)
		}
	}
}

func (a *usageAccumulator) setUsage(u Usage) {
	a.in, a.out = u.In, u.Out
	a.cachedRead, a.cacheWrite = u.CachedRead, u.CacheWrite
	a.reasoning, a.audioIn, a.audioOut = u.Reasoning, u.AudioIn, u.AudioOut
	a.imageIn, a.imageOut = u.ImageIn, u.ImageOut
	a.cacheWrite5m, a.cacheWrite1h = u.CacheWrite5m, u.CacheWrite1h
	a.serviceTier, a.toolCalls, a.seen = u.ServiceTier, u.ToolCalls, u.Present
}

func (a *usageAccumulator) mergeToolCalls(next map[string]int64) {
	if len(next) == 0 {
		return
	}
	if a.toolCalls == nil {
		a.toolCalls = make(map[string]int64, len(next))
	}
	// A terminating SSE chunk usually reports running totals, so when the same
	// tool appears twice the larger value wins rather than the sum. Adding
	// would double-charge whenever message_start and message_delta both carry a
	// snapshot.
	for tool, calls := range next {
		if calls > a.toolCalls[tool] {
			a.toolCalls[tool] = calls
		}
	}
}

// result produces the normalised usage.
func (a *usageAccumulator) result() Usage {
	return Usage{
		In: a.in, Out: a.out, CachedRead: a.cachedRead, CacheWrite: a.cacheWrite,
		Reasoning: a.reasoning, AudioIn: a.audioIn, AudioOut: a.audioOut,
		ImageIn: a.imageIn, ImageOut: a.imageOut,
		CacheWrite5m: a.cacheWrite5m, CacheWrite1h: a.cacheWrite1h,
		ServiceTier: a.serviceTier, ToolCalls: a.toolCalls, Present: a.seen,
	}
}

// sseDataLines extracts the payload of every data: line in one frame.
// A frame may hold several data lines, which SSE permits, and they come back
// one per line rather than concatenated -- in both dialects each such line is
// already an independent JSON document.
func sseDataLines(frame []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) > 0 {
			out = append(out, payload)
		}
	}
	return out
}

// StreamErrorEvent renders an in-stream error event, for when the first byte
// has gone out and the HTTP status can no longer be changed. That is exactly
// why the stream-interrupted code is absent from the error-code registry: it
// has no HTTP status of its own and can only be conveyed inside the stream.
func StreamErrorEvent(surface Surface, message string) []byte {
	var payload any
	switch surface {
	case SurfaceAnthropic:
		payload = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": "api_error", "message": message, "code": CodeStreamInterrupted,
			},
		}
	case SurfaceGemini:
		// The same shape a Gemini failure takes outside a stream, so a client
		// parses one error type rather than two. The status is INTERNAL because
		// this is the gateway's own failure, not the upstream refusing.
		payload = map[string]any{
			"error": map[string]any{
				"code": 500, "message": message, "status": "INTERNAL",
				"details": []any{map[string]any{
					"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
					"reason": CodeStreamInterrupted,
					"domain": errorDomain,
				}},
			},
		}
	default:
		payload = map[string]any{
			"error": map[string]any{
				"message": message, "type": "api_error", "code": CodeStreamInterrupted,
			},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	if surface == SurfaceAnthropic {
		b.WriteString("event: error\n")
	}
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	return b.Bytes()
}

// consumeGemini parses the Gemini protocol's stream.
//
// Every chunk is a whole response object, and every chunk carries
// usageMetadata with the totals *so far* rather than a delta. So the values are
// replaced rather than accumulated, and the last chunk to carry them wins --
// summing them would multiply a long stream's bill by the number of chunks.
//
// There is no terminal sentinel on this stream: it ends when the body ends. An
// interrupted stream therefore keeps whatever the last chunk reported, which is
// more than the other two protocols can offer -- and is why the text is still
// accumulated for the estimator, for a stream that dies before any chunk lands.
func (a *usageAccumulator) consumeGemini(data []byte, textBuf *bytes.Buffer) {
	var ev struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	for _, c := range ev.Candidates {
		for _, part := range c.Content.Parts {
			textBuf.WriteString(part.Text)
		}
	}
	if ev.UsageMetadata == nil {
		return
	}
	// The same normalisation the buffered parser applies, called rather than
	// spelled again: one arithmetic, so a streamed request and a buffered one
	// cannot be priced differently.
	u := ev.UsageMetadata.usage()
	a.in, a.cachedRead = u.In, u.CachedRead
	a.out, a.reasoning = u.Out, u.Reasoning
	a.audioIn, a.audioOut = u.AudioIn, u.AudioOut
	// Carried rather than dropped, and this is not a spare line: Gemini's image
	// models stream from this arm, and their generated image is reported as
	// image output tokens priced well above text output. Losing the breakdown
	// here billed every streamed image at the text rate.
	a.imageIn, a.imageOut = u.ImageIn, u.ImageOut
	a.seen = true
}

// consumeImage parses an image stream's usage.
//
// Only the terminal `image_generation.completed` frame carries it, and it
// carries it in the same shape the buffered response uses, so this calls the
// buffered parser rather than spelling the arithmetic a second time -- the same
// reason consumeGemini calls its own. One arithmetic means a streamed
// generation and an unstreamed one cannot be priced differently.
//
// There is no textBuf parameter because there is no text: an image stream's
// frames are partial images and then a result. That is precisely why this arm
// has to exist -- with nothing to estimate from, a missed usage object does not
// degrade to an estimate, it degrades to zero.
func (a *usageAccumulator) consumeImage(data []byte) {
	if u := ParseImageUsage(data); u.Present {
		a.setUsage(u)
	}
}
