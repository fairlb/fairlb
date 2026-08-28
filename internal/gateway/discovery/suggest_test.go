package discovery

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// What an unknown upstream model is offered as, and -- just as important --
// when nothing is offered at all.
//
// A slug cannot be changed after it is created, so a prefill that is merely
// plausible is worse than an empty field: the empty field asks, the plausible
// one gets accepted. That is why the platform cases below expect no suggestion
// rather than a guessed prefix.
func TestSuggestForNamesOnlyWhatItKnows(t *testing.T) {
	cases := []struct {
		name       string
		vendor     string
		upstream   string
		wantSlug   string // "" means: expect no suggestion at all
		wantSource SuggestionSource
		wantNamed  bool // the seeded catalog supplies a display name and windows
	}{
		{
			name:   "the seeded catalog knows the model outright",
			vendor: "openai", upstream: "gpt-5.6-sol",
			wantSlug: "openai/gpt-5.6-sol", wantSource: SourceSeed, wantNamed: true,
		},
		{
			name:   "a first-party vendor supplies the creator for a model not seeded",
			vendor: "deepseek", upstream: "deepseek-chat",
			wantSlug: "deepseek/deepseek-chat", wantSource: SourceVendor,
		},
		{
			name:   "the vendor's creator is not its own slug",
			vendor: "xai", upstream: "grok-4",
			wantSlug: "x-ai/grok-4", wantSource: SourceVendor,
		},
		{
			// An aggregator names the creator itself, which beats assembling
			// one: the upstream is stating it rather than being guessed at.
			name:   "an aggregator already reports a two-part name",
			vendor: "openrouter", upstream: "anthropic/claude-opus-5",
			wantSlug: "anthropic/claude-opus-5", wantSource: SourceUpstream,
		},
		{
			name:   "an upstream two-part name is normalised to lower case",
			vendor: "openrouter", upstream: "OpenAI/GPT-5.4",
			wantSlug: "openai/gpt-5.4", wantSource: SourceUpstream,
		},
		{
			// The three platform vendors, where the upstream name is a
			// deployment name or an ARN with no creator in it.
			name:   "Azure deployment names say nothing about who made the model",
			vendor: "azure-openai", upstream: "my-gpt-deployment",
		},
		{
			name:   "a Bedrock inference profile is not a slug and cannot be prefixed",
			vendor: "aws-bedrock", upstream: "anthropic.claude-opus-4-5-20250101-v1:0",
		},
		{
			name:   "a Vertex model id carries an @-suffix a slug cannot hold",
			vendor: "google-vertex", upstream: "claude-opus-4-5@20250101",
		},
		{
			name:   "a custom upstream has no registry entry to answer for it",
			vendor: "custom", upstream: "some-model",
		},
		{
			// Three segments is not a slug either, and prefixing would make it
			// four. There is nothing to fall back to.
			name:   "a three-part upstream name has no usable shape",
			vendor: "openrouter", upstream: "a/b/c",
		},
		{
			name:   "an unknown vendor answers for nothing",
			vendor: "not-a-vendor", upstream: "gpt-5.6-sol",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := suggestFor(c.vendor, c.upstream)
			if c.wantSlug == "" {
				if got != nil {
					t.Fatalf("expected no suggestion, got %q from %s", got.Slug, got.Source)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %q, got no suggestion", c.wantSlug)
			}
			if got.Slug != c.wantSlug {
				t.Errorf("slug: got %q, want %q", got.Slug, c.wantSlug)
			}
			if got.Source != c.wantSource {
				t.Errorf("source: got %q, want %q", got.Source, c.wantSource)
			}
			if named := got.DisplayName != "" && got.ContextWindow > 0; named != c.wantNamed {
				t.Errorf("display name and window present = %v, want %v "+
					"(only the seeded catalog knows them)", named, c.wantNamed)
			}
		})
	}
}

// Everything suggested has to be something the database would accept. A
// suggestion that fails on save is worse than none: it looks like the work is
// done until the moment it is submitted.
func TestEverySuggestionWouldBeAccepted(t *testing.T) {
	cases := []struct{ vendor, upstream string }{
		{"openai", "gpt-5.6-sol"},
		{"openai", "gpt-image-2"},
		{"anthropic", "claude-haiku-4-5"},
		{"google", "gemini-3.1-pro-preview"},
		{"deepseek", "deepseek-chat"},
		{"alibaba", "qwen-plus"},
		{"openrouter", "anthropic/claude-opus-5"},
		{"moonshot", "kimi-k2"},
		{"zhipu", "glm-4.6"},
	}
	for _, c := range cases {
		got := suggestFor(c.vendor, c.upstream)
		if got == nil {
			t.Errorf("%s/%s: no suggestion", c.vendor, c.upstream)
			continue
		}
		if !catalog.ValidModelSlug(got.Slug) {
			t.Errorf("%s/%s: suggested %q, which the database would refuse",
				c.vendor, c.upstream, got.Slug)
		}
	}
}
