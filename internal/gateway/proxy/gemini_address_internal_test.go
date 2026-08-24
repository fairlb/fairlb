package proxy

import "testing"

// The address parser, tested directly because the two things it gets right are
// invisible from outside: a request with no credential is refused before the
// model is ever looked up, so an end-to-end test cannot tell a decoded name
// from an undecoded one.
func TestParseGeminiAddress(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantModel  string
		wantMethod string
		wantOK     bool
	}{
		{
			// The catalogue issues names with a slash in them, and encoding that
			// slash is the correct way to keep it inside one path segment.
			name: "percent-encoded slash", raw: "google%2Fgemini-2.5-flash:generateContent",
			wantModel: "google/gemini-2.5-flash", wantMethod: "generateContent", wantOK: true,
		},
		{
			name: "plain slash", raw: "google/gemini-2.5-flash:streamGenerateContent",
			wantModel: "google/gemini-2.5-flash", wantMethod: "streamGenerateContent", wantOK: true,
		},
		{
			// A colon inside the model id: the split has to take the last one.
			name: "versioned model id", raw: "anthropic.claude-v1:0:generateContent",
			wantModel: "anthropic.claude-v1:0", wantMethod: "generateContent", wantOK: true,
		},
		{"no method", "gemini-2.5-flash", "", "", false},
		{"empty method", "gemini-2.5-flash:", "", "", false},
		{"empty model", ":generateContent", "", "", false},
		{"invalid encoding", "gemini%zz:generateContent", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, method, ok := parseGeminiAddress(c.raw)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if model != c.wantModel || method != c.wantMethod {
				t.Errorf("got (%q, %q), want (%q, %q)", model, method, c.wantModel, c.wantMethod)
			}
		})
	}
}
