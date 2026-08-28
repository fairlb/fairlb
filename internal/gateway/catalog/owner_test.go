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
		// Neither of the next two can reach the catalog any more --
		// models_slug_shape refuses them on the way in. They are kept because
		// the answer this function gives if one ever does is the difference
		// between omitting owned_by and publishing a wrong one.
		{"gpt-4o", ""},
		{"/gpt-4o", ""},
	}
	for _, c := range cases {
		if got := ownerOf(c.slug); got != c.want {
			t.Errorf("ownerOf(%q) = %q, want %q", c.slug, got, c.want)
		}
	}
}
