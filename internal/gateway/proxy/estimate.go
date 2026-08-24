package proxy

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Estimating input-side tokens.
//
// This uses a deterministic character heuristic rather than a real tokenizer,
// for three reasons:
//   - The hold is a *gate*, not a bill. Settlement always uses the actual usage
//     the upstream reported, so estimating high only ties up a little of the
//     customer's balance and estimating low is corrected at settlement.
//   - Not every vendor publishes a tokenizer, so being exact would only work
//     for some dialects and would leave the two measured differently.
//   - Taking on a BPE dependency is a library-level decision, and some
//     implementations fetch vocabulary assets at runtime.
//
// The condition for revisiting this: when estimated usage accounts for more
// than a small share of settled value. That would mean upstreams commonly fail
// to report usage, and the estimate would have turned from a gate into the
// basis of the bill.

// Empirical constants: Latin text runs about 4 bytes per token, dense scripts
// such as Chinese, Japanese and Korean closer to 1.5 characters per token. The
// estimate takes the larger of the two and errs high on purpose -- a high hold
// merely reserves a little extra balance, while a low one can let a request
// through the gate that the balance cannot cover.
const (
	bytesPerToken = 4
	runesPerToken = 2
)

// EstimateTokens estimates the token count of a piece of text.
func EstimateTokens(s string) int64 {
	if s == "" {
		return 0
	}
	byBytes := int64(len(s)) / bytesPerToken
	byRunes := int64(utf8.RuneCountInString(s)) / runesPerToken
	n := max(byBytes, byRunes)
	if n < 1 {
		return 1 // non-empty text counts as at least one
	}
	return n
}

// EstimateRequestTokens estimates the input tokens of a request body.
//
// It does not parse each dialect's differing message structure but estimates
// over the whole body as text. Besides the messages, a body carries tool
// definitions, system prompts, image URLs and more, and all of those count
// toward the upstream's input charge. Parsing structurally would miss whatever
// new field the gateway does not know about -- the same conclusion the
// pass-through principle reaches everywhere else.
func EstimateRequestTokens(body []byte) int64 {
	// Key names and punctuation never reach the upstream's tokenizer, but their
	// share of the body is stable, so estimating over the whole thing is close
	// enough -- the output cap dominates the hold anyway.
	return EstimateTokens(string(body))
}

// ResponseTextOf extracts the text content of a response, which is the input to
// the estimation fallback: it converts the output side into tokens when the
// upstream reported no usage.
//
// The criterion is the *surface*, not the protocol, exactly as in ParseUsage and
// RewriteRequest. Chat and responses belong to the same openai protocol yet put
// the text in different places, so branching per protocol would leave one of them
// always returning an empty string -- and that is not cosmetic: the output side
// would be estimated as zero, meaning service delivered and not charged for.
func ResponseTextOf(surface catalog.Surface, body []byte) string {
	switch surface {
	case catalog.SurfaceMessages:
		var r struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &r) != nil {
			return ""
		}
		var b []byte
		for _, c := range r.Content {
			b = append(b, c.Text...)
		}
		return string(b)
	case catalog.SurfaceGenerateContent:
		// candidates[].content.parts[].text, with every candidate counted: the
		// upstream bills for all of them, so estimating from the first would
		// under-charge a request that asked for several.
		var r struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal(body, &r) != nil {
			return ""
		}
		var b []byte
		for _, c := range r.Candidates {
			for _, part := range c.Content.Parts {
				b = append(b, part.Text...)
			}
		}
		return string(b)
	case catalog.SurfaceResponses:
		// output is an array of items, and the text sits in a message item's
		// content[].text. A reasoning model emits a reasoning item with no
		// content first; an empty array is skipped naturally, so there is no
		// need to filter by type. Any new item type carrying text is counted
		// along with the rest, which is what the pass-through principle
		// implies.
		var r struct {
			Output []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(body, &r) != nil {
			return ""
		}
		var b []byte
		for _, item := range r.Output {
			for _, c := range item.Content {
				b = append(b, c.Text...)
			}
		}
		return string(b)
	default:
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &r) != nil {
			return ""
		}
		var b []byte
		for _, c := range r.Choices {
			b = append(b, c.Message.Content...)
		}
		return string(b)
	}
}
