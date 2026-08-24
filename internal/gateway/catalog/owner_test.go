package catalog

import "testing"

func TestOwnerOf(t *testing.T) {
	cases := []struct{ slug, want string }{
		{"openai/gpt-5.6-sol", "openai"},
		// The creator, not the serving provider or the protocol: a model
		// created by Google reports google, whichever protocol it is reached on.
		{"google/gemini-2.5-flash", "google"},
		// A deeper path still reports its first segment.
		{"openrouter/openai/gpt-4o", "openrouter"},
		// A bare slug has no creator segment, and a model owns no protocol to
		// fall back on: the field is omitted rather than guessed.
		{"gpt-4o", ""},
		// A leading slash is not a creator segment.
		{"/gpt-4o", ""},
	}
	for _, c := range cases {
		if got := ownerOf(c.slug); got != c.want {
			t.Errorf("ownerOf(%q) = %q, want %q", c.slug, got, c.want)
		}
	}
}
