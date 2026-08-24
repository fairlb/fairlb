package catalog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

func TestTransportReadsAndWritesUseTheSameStrictSchema(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		strictErr string // substring the refusal must contain; empty means accepted
	}{
		{
			name: "empty object",
			raw:  `{}`,
		},
		{
			name: "a full azure profile",
			raw:  `{"auth":"header:api-key","query":{"api-version":"2024-10-21"},"path_overrides":{"/v1/chat/completions":"/openai/deployments/{model}/chat/completions"},"connect_timeout_ms":3000}`,
		},
		{
			name:      "an unknown key",
			raw:       `{"retries":3}`,
			strictErr: "unknown transport setting retries",
		},
		{
			name:      "an unknown auth mode",
			raw:       `{"auth":"basic"}`,
			strictErr: `unknown auth "basic"`,
		},
		{
			name:      "request signing with nowhere to sign for",
			raw:       `{"auth":"aws_sigv4"}`,
			strictErr: "needs a signing region",
		},
		{
			name: "request signing, complete",
			raw:  `{"auth":"aws_sigv4","sigv4":{"region":"us-east-1"},"envelope":"bedrock"}`,
		},
		{
			name: "token exchange",
			raw:  `{"auth":"gcp_service_account","envelope":"vertex"}`,
		},
		{
			name:      "a signing region on a provider that never signs",
			raw:       `{"sigv4":{"region":"us-east-1"}}`,
			strictErr: "only applies with auth",
		},
		{
			name:      "a misspelled key inside the nested object",
			raw:       `{"auth":"aws_sigv4","sigv4":{"region":"us-east-1","servce":"bedrock"}}`,
			strictErr: "unknown sigv4 setting servce",
		},
		{
			// The precise message has to win over "no region": the region *is*
			// there, under a key nobody reads, and being told it is missing
			// while looking straight at it is the least useful thing to say.
			name:      "the region key itself misspelled",
			raw:       `{"auth":"aws_sigv4","sigv4":{"regoin":"us-east-1"}}`,
			strictErr: "unknown sigv4 setting regoin",
		},
		{
			name:      "an unknown envelope",
			raw:       `{"envelope":"vertexai"}`,
			strictErr: `unknown envelope "vertexai"`,
		},
		{
			name:      "a stream override for a path the gateway never sends",
			raw:       `{"stream_path_overrides":{"/v1/completions":"/x"}}`,
			strictErr: "in stream_path_overrides is not a path this gateway sends",
		},
		{
			name:      "a header auth naming no header",
			raw:       `{"auth":"header:"}`,
			strictErr: "does not name a header",
		},
		{
			name:      "an override for a path the gateway never sends",
			raw:       `{"path_overrides":{"/v1/completions":"/x"}}`,
			strictErr: "is not a path this gateway sends",
		},
		{
			name:      "a relative override",
			raw:       `{"path_overrides":{"/v1/messages":"messages"}}`,
			strictErr: "absolute path",
		},
		{
			name:      "a query string smuggled into the path",
			raw:       `{"path_overrides":{"/v1/messages":"/messages?api-version=1"}}`,
			strictErr: "use the query setting",
		},
		{
			name:      "a state resource override drops the resource id",
			raw:       `{"path_overrides":{"/v1/responses/{resource}":"/responses"}}`,
			strictErr: "must retain the {resource} placeholder",
		},
		{
			name:      "a connect bound outside the range",
			raw:       `{"connect_timeout_ms":900000}`,
			strictErr: "connect_timeout_ms must be between",
		},
		{
			name:      "a profile that is not an object",
			raw:       `[1,2]`,
			strictErr: "must be a JSON object",
		},

		// Wrong types are rejected as a whole; partial profiles are unsafe.
		{
			name:      "a query value widened to a list",
			raw:       `{"query":{"api-version":["2024-10-21"]},"auth":"header:api-key","path_overrides":{"/v1/chat/completions":"/openai/deployments/{model}/chat/completions"},"connect_timeout_ms":3000}`,
			strictErr: "malformed",
		},
		{
			name:      "an auth widened to an object",
			raw:       `{"auth":{"mode":"bearer"},"query":{"api-version":"2024-10-21"},"connect_timeout_ms":3000}`,
			strictErr: "malformed",
		},
		{
			name:      "a connect bound widened to an object",
			raw:       `{"connect_timeout_ms":{"connect":3000,"read":9000},"auth":"header:api-key"}`,
			strictErr: "malformed",
		},
		{
			name:      "a path override widened to an object",
			raw:       `{"path_overrides":{"/v1/messages":{"path":"/m","method":"POST"}},"auth":"header:api-key","query":{"api-version":"2024-10-21"}}`,
			strictErr: "malformed",
		},
		{
			name:      "an anthropic version widened to a list",
			raw:       `{"anthropic_version":["2023-06-01","2024-01-01"],"auth":"header:api-key","connect_timeout_ms":3000}`,
			strictErr: "malformed",
		},
		{
			// One key failing halfway through must not leave the map holding
			// the entries it had already read. Half a routing table addresses
			// some requests correctly and the rest to nowhere, which is harder
			// to diagnose than losing the key outright.
			name:      "a query map that is widened only in its second entry",
			raw:       `{"query":{"api-version":"2024-10-21","scopes":["a","b"]},"auth":"header:api-key"}`,
			strictErr: "malformed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.ValidateTransport([]byte(tc.raw))
			switch {
			case tc.strictErr == "" && err != nil:
				t.Fatalf("the write path should have accepted this: %v", err)
			case tc.strictErr != "" && err == nil:
				t.Fatalf("the write path should have refused this")
			case tc.strictErr != "" && !strings.Contains(err.Error(), tc.strictErr):
				// The message has to name the offending thing, or the operator
				// is told only that something is wrong with an object they just
				// typed twelve keys into.
				t.Fatalf("the refusal does not name the problem:\n got=%q\nwant substring %q",
					err.Error(), tc.strictErr)
			}

			_, readErr := catalog.ParseTransport([]byte(tc.raw))
			if (err == nil) != (readErr == nil) {
				t.Fatalf("read/write validation diverged: write=%v read=%v", err, readErr)
			}
			if tc.strictErr != "" && !strings.Contains(readErr.Error(), tc.strictErr) {
				t.Fatalf("stored read refusal does not name the problem: %v", readErr)
			}
		})
	}
}

func TestParseTransportRejectsAnUnusableConnectBound(t *testing.T) {
	if _, err := catalog.ParseTransport([]byte(`{"connect_timeout_ms":1}`)); err == nil {
		t.Fatal("an unusable stored connect bound must be rejected")
	}
	got, err := catalog.ParseTransport([]byte(`{"connect_timeout_ms":2500}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2500 * time.Millisecond; got.ConnectTimeout() != want {
		t.Errorf("connect timeout = %s, want %s", got.ConnectTimeout(), want)
	}
}

// The dialect decides the auth form when the profile is silent, which is what
// keeps a provider with no profile behaving exactly as it did before profiles
// existed.
func TestAuthModeDefaultsFollowTheDialect(t *testing.T) {
	var none catalog.Transport
	if got := none.AuthMode(catalog.ProtocolAnthropic); got != catalog.AuthAPIKey {
		t.Errorf("the anthropic default should be %s: %s", catalog.AuthAPIKey, got)
	}
	if got := none.AuthMode("openai"); got != catalog.AuthBearer {
		t.Errorf("the openai default should be %s: %s", catalog.AuthBearer, got)
	}
	// The derived modes are returned as themselves on both dialects: which one
	// applies is the provider's decision, not the surface's.
	for _, mode := range []string{catalog.AuthAWSSigV4, catalog.AuthGCPServiceAccount} {
		tp := catalog.Transport{Auth: mode}
		for _, protocol := range []string{"openai", catalog.ProtocolAnthropic} {
			if got := tp.AuthMode(protocol); got != mode {
				t.Errorf("%s on the %s dialect resolved to %s", mode, protocol, got)
			}
		}
	}
}

// The envelope is read from the declared value and from nothing else.
//
// Inferring it from the auth mode would be wrong in both directions, and both
// are real configurations: one platform publishes an endpoint that takes the
// same signature and no envelope at all, and a token can front any endpoint at
// all. So a profile must never acquire an envelope it did not ask for.
func TestEnvelopeIsDeclaredNotInferred(t *testing.T) {
	signed := catalog.Transport{
		Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"},
	}
	if got := signed.BodyEnvelope(); got != catalog.EnvelopeNone {
		t.Errorf("signing alone produced envelope %q", got)
	}
	if got := (catalog.Transport{Auth: catalog.AuthGCPServiceAccount}).BodyEnvelope(); got != catalog.EnvelopeNone {
		t.Errorf("a token alone produced envelope %q", got)
	}
	// The positive control: a declared one is honoured, or every assertion
	// above would pass on a function that always returns none.
	if got := (catalog.Transport{Envelope: catalog.EnvelopeBedrock}).BodyEnvelope(); got != catalog.EnvelopeBedrock {
		t.Errorf("a declared envelope was dropped: %q", got)
	}
}

// The signing service has a default because almost nobody needs to change it,
// and a region has none because there is no value that could be right.
func TestSigningTargetDefaults(t *testing.T) {
	tp := catalog.Transport{Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "eu-west-2"}}
	if got := tp.SigningService(); got != catalog.DefaultSigV4Service {
		t.Errorf("service = %q, want the default %q", got, catalog.DefaultSigV4Service)
	}
	if got := tp.SigningRegion(); got != "eu-west-2" {
		t.Errorf("region = %q", got)
	}
	tp.SigV4.Service = "bedrock-runtime"
	if got := tp.SigningService(); got != "bedrock-runtime" {
		t.Errorf("a pinned service was ignored: %q", got)
	}
	// No profile at all: the region is empty rather than a guess, and the
	// caller refuses instead of signing for somewhere nobody named.
	if got := (catalog.Transport{}).SigningRegion(); got != "" {
		t.Errorf("region = %q, want empty", got)
	}
}

// Both override maps are keyed on the same path set, and a streamed request
// prefers the streaming one.
func TestStreamPathOverridesPreferAndFallBack(t *testing.T) {
	tp := catalog.Transport{
		PathOverrides:       map[string]string{catalog.PathMessages: "/a/{model}"},
		StreamPathOverrides: map[string]string{catalog.PathMessages: "/b/{model}"},
	}
	if got := tp.PathFor(catalog.PathMessages, "m", false); got != "/a/m" {
		t.Errorf("unary path = %q", got)
	}
	if got := tp.PathFor(catalog.PathMessages, "m", true); got != "/b/m" {
		t.Errorf("streamed path = %q", got)
	}
	only := catalog.Transport{PathOverrides: map[string]string{catalog.PathChat: "/chat"}}
	if got := only.PathFor(catalog.PathChat, "m", true); got != "/chat" {
		t.Errorf("with no streaming entry the ordinary one applies: %q", got)
	}
	if got := (catalog.Transport{}).PathFor(catalog.PathChat, "m", true); got != catalog.PathChat {
		t.Errorf("with no overrides at all the gateway's own path applies: %q", got)
	}
}

// Every path the gateway can send must be overridable, and nothing else may be.
// The two halves are checked against the handler's own table elsewhere; here
// the concern is only that the list is complete and has no duplicates, since it
// doubles as the message an operator reads after a typo.
func TestUpstreamPathsIsAStableSet(t *testing.T) {
	paths := catalog.UpstreamPaths()
	seen := map[string]bool{}
	for _, p := range paths {
		// A versioned absolute path. The version segment is not always "v1":
		// one protocol's own paths are published under v1beta, and pinning the
		// exact segment here would reject a path this gateway really sends.
		// Both version segments, each with its boundary slash. Testing "/v1"
		// alone would also accept "/v1chat" and "/v123/x", which is what this
		// assertion exists to catch.
		versioned := strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/v1beta/")
		if !versioned || strings.Contains(p, "//") {
			t.Errorf("%q does not look like an upstream path", p)
		}
		if seen[p] {
			t.Errorf("%q is listed twice", p)
		}
		seen[p] = true
	}
	for _, want := range []string{
		catalog.PathChat, catalog.PathResponses, catalog.PathEmbeddings,
		catalog.PathMessages, catalog.PathModels,
		catalog.PathImagesGenerate, catalog.PathImagesEdit,
		catalog.PathGenerateContent, catalog.PathStreamGenerateContent, catalog.PathGeminiModels,
	} {
		if !seen[want] {
			t.Errorf("%q is missing from the overridable set", want)
		}
	}
}

// The credential goes in the header the operator names, and only into a header
// that can carry it.
//
// Both halves of this were reachable through the write path: a name that is not
// a valid HTTP token saved cleanly and then failed on every request, and a name
// like Content-Type saved cleanly and put the credential somewhere no log
// redaction would ever look for it. The first is worse than it sounds -- the
// per-request failure is a transport error, so it counts against provider
// health and opens the circuit, and the operator is told the upstream is down.
func TestAuthHeaderNameIsATokenAndNotAReservedHeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		auth    string
		refused bool
	}{
		// Reserved: the gateway sets these itself, and none of them is treated
		// as a secret by anything downstream.
		{"content type", `{"auth":"header:Content-Type"}`, true},
		{"accept", `{"auth":"header:Accept"}`, true},
		{"anthropic version", `{"auth":"header:anthropic-version"}`, true},
		{"user agent, mixed case", `{"auth":"header:User-AGENT"}`, true},
		{"host", `{"auth":"header:host"}`, true},

		// Not HTTP tokens. net/http refuses to send these, so accepting them
		// stores a provider that cannot serve a single request.
		{"parentheses", `{"auth":"header:foo(bar)"}`, true},
		{"comma", `{"auth":"header:foo,bar"}`, true},
		{"slash", `{"auth":"header:foo/bar"}`, true},
		{"at sign", `{"auth":"header:foo@bar"}`, true},
		{"empty", `{"auth":"header:"}`, true},

		// The positive control. A guard that refused everything would pass every
		// assertion above and make the feature useless; these are the shapes
		// real vendors use.
		{"azure api-key", `{"auth":"header:api-key"}`, false},
		{"vendor token", `{"auth":"header:X-Vendor-Token"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.ValidateTransport([]byte(tc.auth))
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("refused=%v want %v (err=%v)", refused, tc.refused, err)
			}
		})
	}
}
