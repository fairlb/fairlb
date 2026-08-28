package proxy

// These are the canonical upstream usage wire objects. Buffered responses and
// SSE events have different envelopes, but once they reach a usage object they
// must use the same arithmetic or the delivery mode changes the bill.

// The image breakdowns below are read on every surface that can carry one, not
// only on the images endpoints.
//
// A modality is an axis of its own (ADR-0226): Google's image models are
// reached on the same surface as its text ones, and an OpenAI-compatible relay
// can put an image model behind chat/completions. The ledger has to follow. An
// image token is priced several times a text one -- for a Gemini image model 30
// against 12, for gpt-image 32 against 10 -- so a breakdown nowhere to record
// leaves every generated image billed at the text output rate.
//
// Like audio, image tokens are a *subset* of their parent count, which is what
// makes BuildCharge's subtract-then-price correct and what makes a model with
// no image rate configured bill exactly as it did before. An upstream that
// reports no breakdown leaves them zero and is unaffected.
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
		ImageTokens      int64 `json:"image_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
		ImageTokens     int64 `json:"image_tokens"`
	} `json:"output_tokens_details"`
}

func (w responsesUsage) usage(serviceTier string, tools map[string]any) Usage {
	u := Usage{
		In: w.InputTokens, Out: w.OutputTokens,
		ServiceTier: serviceTier, ToolCalls: toolCallCounts(tools), Present: true,
	}
	if d := w.InputTokensDetails; d != nil {
		u.CachedRead, u.CacheWrite = d.CachedTokens, d.CacheWriteTokens
		u.In = subsetIn(w.InputTokens, d.CachedTokens, d.CacheWriteTokens)
		// Clamped to the parent, exactly as ParseImageUsage does: the subset
		// relation is what makes the subtraction correct, and an upstream that
		// contradicts it must not turn a served request into a billing error.
		u.ImageIn = min(nonNegative(d.ImageTokens), u.In)
	}
	if d := w.OutputTokensDetails; d != nil {
		u.Reasoning = d.ReasoningTokens
		u.ImageOut = min(nonNegative(d.ImageTokens), u.Out)
	}
	return u
}

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
		AudioTokens      int64 `json:"audio_tokens"`
		ImageTokens      int64 `json:"image_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
		AudioTokens     int64 `json:"audio_tokens"`
		ImageTokens     int64 `json:"image_tokens"`
	} `json:"completion_tokens_details"`
}

func (w openAIUsage) usage(serviceTier string, tools map[string]any) Usage {
	u := Usage{
		In: w.PromptTokens, Out: w.CompletionTokens,
		ServiceTier: serviceTier, ToolCalls: toolCallCounts(tools), Present: true,
	}
	if d := w.PromptTokensDetails; d != nil {
		u.CachedRead, u.CacheWrite = d.CachedTokens, d.CacheWriteTokens
		u.AudioIn = d.AudioTokens
		u.In = subsetIn(w.PromptTokens, d.CachedTokens, d.CacheWriteTokens)
		u.ImageIn = min(nonNegative(d.ImageTokens), u.In)
	}
	if d := w.CompletionTokensDetails; d != nil {
		u.Reasoning, u.AudioOut = d.ReasoningTokens, d.AudioTokens
		u.ImageOut = min(nonNegative(d.ImageTokens), u.Out)
	}
	return u
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ServiceTier   string         `json:"service_tier"`
	ServerToolUse map[string]any `json:"server_tool_use"`
}

func (w anthropicUsage) usage() Usage {
	u := Usage{
		In: w.InputTokens, Out: w.OutputTokens,
		CachedRead: w.CacheReadInputTokens, CacheWrite: w.CacheCreationInputTokens,
		ServiceTier: w.ServiceTier, ToolCalls: toolCallCounts(w.ServerToolUse), Present: true,
	}
	if c := w.CacheCreation; c != nil {
		u.CacheWrite5m, u.CacheWrite1h = c.Ephemeral5m, c.Ephemeral1h
	}
	return u
}
