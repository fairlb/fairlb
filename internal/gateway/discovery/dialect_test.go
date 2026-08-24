package discovery

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// Which dialect the catalogue is listed in.
//
// A fixed preference for openai was right while both dialects served their
// catalogue at one address with one cursor. A third protocol broke that: Gemini
// keeps its catalogue at another path, behind another header, with another
// cursor and another response shape. So for the vendor the registry ships as
// speaking both, the preference sent discovery to the OpenAI address, where the
// answer is a 401 or a 200 this parser reads as an empty catalogue -- a
// conclusion rather than an error.
func TestCatalogDialectFollowsTheVendorsDefault(t *testing.T) {
	cases := []struct {
		name      string
		vendor    string
		protocols []string
		want      proxy.Protocol
	}{
		{
			// The case that was wrong: the registry publishes both for google,
			// and the create form lets an operator declare both.
			name:   "vendor default wins over the openai preference",
			vendor: "google", protocols: []string{"gemini", "openai"},
			want: proxy.ProtocolGemini,
		},
		{
			// The vendor's default is not declared here, so it cannot be used.
			name:   "falls back to what the provider actually speaks",
			vendor: "google", protocols: []string{"openai"},
			want: proxy.ProtocolOpenAI,
		},
		{
			name:   "unknown vendor keeps the openai preference",
			vendor: "not-in-this-build", protocols: []string{"anthropic", "openai"},
			want: proxy.ProtocolOpenAI,
		},
		{
			name:   "single dialect answers itself",
			vendor: "anthropic", protocols: []string{"anthropic"},
			want: proxy.ProtocolAnthropic,
		},
		{
			name:   "nothing declared falls back rather than panicking",
			vendor: "custom", protocols: nil,
			want: proxy.ProtocolOpenAI,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := catalogDialect(c.vendor, c.protocols); got != c.want {
				t.Errorf("catalogDialect(%q, %v) = %q, want %q", c.vendor, c.protocols, got, c.want)
			}
		})
	}
}
