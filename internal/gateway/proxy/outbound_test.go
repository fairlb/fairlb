package proxy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The core assertion of upstream anonymity: the outbound header set is
// *exactly* the allowlist, and one extra header counts as a leak.
//
// The value of this test is in "exactly" rather than "contains". A containment
// assertion cannot stop a newly added proxy fingerprint, and that is precisely
// how such problems arise -- swap the HTTP library or add a middleware and one
// more header appears.
//
// The auth axis is part of the criterion, not a variation on it: the allowlist
// has to be exact for a provider whose profile moves the credential into
// another header too, or the exactness assertion silently stops covering every
// provider that has a profile.
func TestOutboundHeadersExactlyAllowlist(t *testing.T) {
	cases := []struct {
		name      string
		protocol  proxy.Protocol
		transport catalog.Transport
		apiKey    string
	}{
		{"openai/default", proxy.ProtocolOpenAI, catalog.Transport{}, "sk-upstream"},
		{"anthropic/default", proxy.ProtocolAnthropic, catalog.Transport{}, "sk-upstream"},
		{"openai/x-api-key", proxy.ProtocolOpenAI, catalog.Transport{Auth: catalog.AuthAPIKey}, "sk-upstream"},
		{"openai/named-header", proxy.ProtocolOpenAI, catalog.Transport{Auth: "header:api-key"}, "sk-upstream"},
		{"anthropic/bearer", proxy.ProtocolAnthropic, catalog.Transport{Auth: catalog.AuthBearer}, "sk-upstream"},
		{"anthropic/named-header", proxy.ProtocolAnthropic, catalog.Transport{Auth: "header:api-key"}, "sk-upstream"},
		// The signing modes are the reason the allowlist takes the whole
		// target: they add headers, and one of those headers depends on the
		// credential rather than on the profile.
		{
			"anthropic/sigv4 with a static credential", proxy.ProtocolAnthropic,
			catalog.Transport{
				Auth:     catalog.AuthAWSSigV4,
				Envelope: catalog.EnvelopeBedrock,
				SigV4:    &catalog.SigV4Profile{Region: "us-east-1"},
			},
			`{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"secret"}`,
		},
		{
			"anthropic/sigv4 with a temporary credential", proxy.ProtocolAnthropic,
			catalog.Transport{
				Auth:     catalog.AuthAWSSigV4,
				Envelope: catalog.EnvelopeBedrock,
				SigV4:    &catalog.SigV4Profile{Region: "us-east-1"},
			},
			`{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"secret","session_token":"tok"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := proxy.Target{
				Protocol: tc.protocol, BaseURL: "https://up.test", APIKey: tc.apiKey, Path: "/v1/x",
				Transport: tc.transport,
			}
			req, err := proxy.BuildRequest(context.Background(), target, []byte(`{"model":"m"}`))
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for k := range req.Header {
				got = append(got, k)
			}
			want := proxy.OutboundAllowlist(target)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("the outbound header set must equal the allowlist exactly:\n got=%v\nwant=%v", got, want)
			}
		})
	}
}

// The exactness assertion above compares two lists this package writes, so it
// cannot see a header whose *name* is wrong -- only one that is missing or
// extra. The names a signature puts on the wire are therefore anchored here by
// hand, against a spelling taken from the signing protocol rather than from the
// code under test.
//
// Deleting this leaves that axis unwitnessed: rename the constant and every
// other assertion in this file still passes.
func TestSigV4WritesTheProtocolsOwnHeaderNames(t *testing.T) {
	target := proxy.Target{
		Protocol: proxy.ProtocolAnthropic, BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey: `{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG"}`,
		Path:   catalog.PathMessages,
		Transport: catalog.Transport{
			Auth:     catalog.AuthAWSSigV4,
			Envelope: catalog.EnvelopeBedrock,
			SigV4:    &catalog.SigV4Profile{Region: "us-east-1"},
		},
	}
	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1}`)
	req, err := proxy.BuildRequest(context.Background(), target, body)
	if err != nil {
		t.Fatal(err)
	}

	auth := req.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256 ",
		"Credential=AKIAIOSFODNN7EXAMPLE/",
		"/us-east-1/bedrock/aws4_request",
		"SignedHeaders=",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization is missing %q:\n%s", want, auth)
		}
	}
	// The date is the signing timestamp in the protocol's basic format. Its
	// shape is asserted rather than its value: the value is the clock.
	date := req.Header.Get("X-Amz-Date")
	if len(date) != len("20060102T150405Z") || !strings.HasSuffix(date, "Z") ||
		date[8] != 'T' {
		t.Errorf("X-Amz-Date = %q, want a basic-format timestamp", date)
	}
	// The payload hash has to be the hash of the payload that is actually
	// being sent, and it has to be signed. An unsigned hash header would let
	// the body be swapped for one it no longer describes.
	sum := sha256.Sum256(body)
	if got, want := req.Header.Get("X-Amz-Content-Sha256"), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("X-Amz-Content-Sha256 = %q, want the hash of the body that goes out (%q)", got, want)
	}
	if !strings.Contains(auth, "x-amz-content-sha256") {
		t.Errorf("the payload hash header must itself be signed:\n%s", auth)
	}
	if req.Header.Get("X-Amz-Security-Token") != "" {
		t.Error("a static credential must not produce a session-token header")
	}
}

// A temporary credential adds its session token, and the signature covers it.
func TestSigV4CarriesASessionToken(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", Path: catalog.PathMessages,
		APIKey: `{"access_key_id":"AKIA","secret_access_key":"s","session_token":"session-token-value"}`,
		Transport: catalog.Transport{
			Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "eu-west-1"},
		},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "session-token-value" {
		t.Errorf("X-Amz-Security-Token = %q", got)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("the session token must be inside SignedHeaders, or it can be stripped in flight")
	}
}

// Two requests differing only in their body must not carry the same signature.
//
// This is the property that makes the payload hash worth setting at all, and it
// is the one a plausible-looking mistake would break: hashing the wrong buffer,
// or hashing before the body is final, produces a signature that still has the
// right shape and passes every assertion above.
func TestSigV4CoversTheBody(t *testing.T) {
	sign := func(body string) string {
		req, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", Path: catalog.PathMessages,
			APIKey: `{"access_key_id":"AKIA","secret_access_key":"s"}`,
			Transport: catalog.Transport{
				Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"},
			},
		}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return req.Header.Get("Authorization")
	}
	if sign(`{"max_tokens":1}`) == sign(`{"max_tokens":2}`) {
		t.Fatal("the signature does not depend on the body")
	}
}

// The signature is computed after the header mapping, not before.
//
// Ordering is invisible in every other assertion: a signature computed first is
// still well formed, still contains every field, and is still rejected by the
// upstream on every request -- as an authentication failure, which sends
// whoever reads it to look at the credential. So the ordering is pinned by
// checking that a mapped header is inside SignedHeaders.
func TestSigV4SignsTheHeadersTheMappingAdded(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", Path: catalog.PathMessages,
		APIKey:  `{"access_key_id":"AKIA","secret_access_key":"s"}`,
		Headers: map[string]string{"X-Amzn-Bedrock-Trace": "ENABLED"},
		Transport: catalog.Transport{
			Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"},
		},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amzn-bedrock-trace") {
		t.Errorf("a header added by the mapping must be inside the signature:\n%s",
			req.Header.Get("Authorization"))
	}
}

// A signature commits to a hash of the whole body, and a streamed body cannot
// supply one. Refusing is the honest answer; buffering an unbounded upload to
// hash it is not.
func TestSigV4RefusesAStreamedBody(t *testing.T) {
	_, err := proxy.BuildRequestStream(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", Path: catalog.PathImagesEdit,
		APIKey: `{"access_key_id":"AKIA","secret_access_key":"s"}`,
		Transport: catalog.Transport{
			Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"},
		},
	}, strings.NewReader("--boundary--"), "multipart/form-data; boundary=boundary")
	if err == nil {
		t.Fatal("a streamed body must not produce a request claiming to be signed")
	}
}

// ${api_key} in a header mapping means "the provider credential", and for a
// derived mode that is a credential *document* rather than a token. Copying an
// access key pair -- or a service-account file with a private key in it -- into
// a header of the operator's choosing sends it upstream in clear text, past
// every redaction rule, and nobody typing that placeholder was asking for it.
//
// Both modes, because the mapping does not know which one is configured and the
// service-account case is the worse of the two.
func TestDerivedCredentialsAreNotInterpolatedIntoHeaders(t *testing.T) {
	for _, tp := range []catalog.Transport{
		{Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"}},
		{Auth: catalog.AuthGCPServiceAccount},
	} {
		_, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", Path: catalog.PathMessages,
			APIKey:    `{"access_key_id":"AKIA","secret_access_key":"s"}`,
			Headers:   map[string]string{"X-Copy-Of-It": "${api_key}"},
			Transport: tp,
		}, []byte(`{}`))
		if err == nil {
			t.Fatalf("%s: a credential document must not be copyable into a header", tp.Auth)
		}
		if !strings.Contains(err.Error(), "X-Copy-Of-It") {
			t.Errorf("%s: the refusal must name the mapping entry: %v", tp.Auth, err)
		}
	}

	// The positive control, and it is the load-bearing half: the placeholder is
	// still the documented way to place a copied credential, and a guard that
	// refused it everywhere would break every Azure-shaped provider.
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", Path: catalog.PathChat,
		APIKey:  "sk-plain",
		Headers: map[string]string{"api-key": "${api_key}"},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("api-key"); got != "sk-plain" {
		t.Errorf("the placeholder must still work for a copied credential: %q", got)
	}
}

// A credential that is not the shape this mode needs is refused where it can be
// read, rather than sent as an empty signature.
func TestSigV4RefusesACredentialOfTheWrongShape(t *testing.T) {
	for _, secret := range []string{
		"AKIAIOSFODNN7EXAMPLE",          // the bare access key id, pasted alone
		`{"access_key_id":"AKIA"}`,      // half of it
		`{"secret_access_key":"no-id"}`, // the other half
	} {
		_, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", Path: catalog.PathMessages,
			APIKey: secret,
			Transport: catalog.Transport{
				Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"},
			},
		}, []byte(`{}`))
		if err == nil {
			t.Errorf("secret %q should have been refused", secret)
		}
	}
}

// Each auth form puts the credential in its own header and in no other. The
// negative half is the load-bearing one: a form that adds its header without
// removing the default one would leave the credential in two places, and one of
// those places is the header the upstream authenticates on.
func TestAuthFormPlacesCredentialInExactlyOneHeader(t *testing.T) {
	all := []string{"Authorization", "X-Api-Key", "Api-Key"}
	cases := []struct {
		name      string
		protocol  proxy.Protocol
		transport catalog.Transport
		header    string
		value     string
	}{
		{"openai default is bearer", proxy.ProtocolOpenAI, catalog.Transport{}, "Authorization", "Bearer sk-up"},
		{"anthropic default is x-api-key", proxy.ProtocolAnthropic, catalog.Transport{}, "X-Api-Key", "sk-up"},
		{
			"named header carries the bare value", proxy.ProtocolOpenAI,
			catalog.Transport{Auth: "header:api-key"},
			"Api-Key", "sk-up",
		},
		{
			"the profile overrides the dialect default", proxy.ProtocolAnthropic,
			catalog.Transport{Auth: catalog.AuthBearer},
			"Authorization", "Bearer sk-up",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := proxy.BuildRequest(context.Background(), proxy.Target{
				Protocol: tc.protocol, BaseURL: "https://up.test", APIKey: "sk-up", Path: "/v1/x",
				Transport: tc.transport,
			}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get(tc.header); got != tc.value {
				t.Errorf("%s = %q, want %q", tc.header, got, tc.value)
			}
			for _, h := range all {
				if h == tc.header {
					continue
				}
				if got := req.Header.Get(h); got != "" {
					t.Errorf("the credential also leaked into %s = %q", h, got)
				}
			}
		})
	}
}

// A provider may pin its own version header. Asserted against a real upstream
// rather than against req.Header: the question is what the upstream receives.
func TestAnthropicVersionIsPerProvider(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	for _, tc := range []struct{ configured, want string }{
		{"", "2023-06-01"},           // the gateway's own default
		{"2099-01-01", "2099-01-01"}, // pinned by the provider
	} {
		req, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: proxy.ProtocolAnthropic, BaseURL: up.URL, APIKey: "k", Path: "/v1/messages",
			Transport: catalog.Transport{AnthropicVersion: tc.configured},
		}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := up.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got != tc.want {
			t.Errorf("configured %q: the upstream received %q, want %q", tc.configured, got, tc.want)
		}
	}
}

// The deployment-path shape: the version parameter and the deployment segment
// have to appear together on the final URL. Either one alone is a request that
// cannot work, and the failure -- a 404 -- looks identical to a wrong base URL.
func TestPathOverrideAndQueryReachTheUpstream(t *testing.T) {
	var gotPath, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: up.URL, APIKey: "k",
		Path:          catalog.PathChat,
		UpstreamModel: "my-deployment",
		Transport: catalog.Transport{
			Auth:  "header:api-key",
			Query: map[string]string{"api-version": "2024-10-21"},
			PathOverrides: map[string]string{
				catalog.PathChat: "/openai/deployments/{model}/chat/completions",
			},
		},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := up.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if want := "/openai/deployments/my-deployment/chat/completions"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if want := "api-version=2024-10-21"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
}

// A path override with no {model} placeholder is used verbatim, which is the
// shape every compatibility layer whose base URL already carries the version
// segment needs.
func TestPathOverrideWithoutPlaceholder(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test/v1beta/openai", APIKey: "k",
		Path: catalog.PathChat,
		Transport: catalog.Transport{PathOverrides: map[string]string{
			catalog.PathChat: "/chat/completions",
		}},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://up.test/v1beta/openai/chat/completions"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}
}

// The set of paths this layer sends and the set a profile may override have to
// be the same set.
//
// They are two lists in two packages, and the failure of either direction is
// silent: a path that can be sent but not overridden makes an upstream
// unreachable with no error anywhere, and one that can be overridden but is
// never sent is a setting that saves and does nothing. Neither shows up as a
// failing request; both show up as a configuration that "should work".
func TestOverridablePathsMatchWhatIsSent(t *testing.T) {
	sent := proxy.ExportSentUpstreamPaths()
	overridable := catalog.UpstreamPaths()
	for _, p := range sent {
		if !slices.Contains(overridable, p) {
			t.Errorf("%q is sent upstream but cannot be overridden", p)
		}
	}
	// The reverse direction allows the catalogue addresses, which discovery and
	// the liveness probe request without their being data-plane endpoints. There
	// is one per protocol that publishes a catalogue at its own address.
	for _, p := range overridable {
		if p == catalog.PathModels || p == catalog.PathGeminiModels {
			continue
		}
		if !slices.Contains(sent, p) {
			t.Errorf("%q can be overridden but is never sent", p)
		}
	}
}

// A provider's own connect bound reaches the dialer, and a provider without one
// gets the gateway's default. The bound travels on the context because the
// transport is shared; this asserts the value that arrives, which is the part
// that would break silently if the plumbing were dropped.
func TestConnectTimeoutIsPerProvider(t *testing.T) {
	ctx := context.Background()
	if got := proxy.ExportConnectTimeoutFor(ctx, proxy.Target{}); got != proxy.ExportConnectTimeout() {
		t.Errorf("a provider with no profile should get the default %s: %s",
			proxy.ExportConnectTimeout(), got)
	}
	target := proxy.Target{Transport: catalog.Transport{ConnectTimeoutMS: 1500}}
	if got, want := proxy.ExportConnectTimeoutFor(ctx, target), 1500*time.Millisecond; got != want {
		t.Errorf("the provider's own bound should reach the dialer: got %s, want %s", got, want)
	}
}

// No proxy fingerprint header may ever appear. ReverseProxy adds
// X-Forwarded-For on its own, which is why it is not used.
func TestOutboundHasNoProxyFingerprints(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", APIKey: "k", Path: "/v1/x",
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{
		"Via", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Real-Ip", "X-Request-Id", "X-Gateway", "Cf-Connecting-Ip",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("proxy fingerprint header %s = %q must not appear", h, v)
		}
	}
}

// The two dialects shape the authentication header differently.
func TestAuthHeaderRewritePerProtocol(t *testing.T) {
	openai, _ := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", APIKey: "sk-up", Path: "/v1/x",
	}, []byte(`{}`))
	if got := openai.Header.Get("Authorization"); got != "Bearer sk-up" {
		t.Errorf("the OpenAI surface should use Bearer: %q", got)
	}
	if openai.Header.Get("x-api-key") != "" {
		t.Error("the OpenAI surface must not carry x-api-key")
	}

	anth, _ := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", APIKey: "sk-up", Path: "/v1/x",
	}, []byte(`{}`))
	if got := anth.Header.Get("x-api-key"); got != "sk-up" {
		t.Errorf("the Anthropic surface should use x-api-key: %q", got)
	}
	if anth.Header.Get("anthropic-version") == "" {
		t.Error("the Anthropic surface must carry anthropic-version")
	}
	if anth.Header.Get("Authorization") != "" {
		t.Error("the Anthropic surface must not carry Authorization")
	}
}

// The header mapping: add, override, delete, and ${api_key} substitution.
func TestHeaderMappingSemantics(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", APIKey: "secret-key", Path: "/v1/x",
		Headers: map[string]string{
			"HTTP-Referer":  "https://fairlb.test", // added
			"User-Agent":    "OpenAI/Python 1.0",   // overridden
			"Authorization": "",                    // deleted
			"api-key":       "${api_key}",          // substituted, as some hosted deployments want
		},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("HTTP-Referer"); got != "https://fairlb.test" {
		t.Errorf("the added header did not take effect: %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "OpenAI/Python 1.0" {
		t.Errorf("the override did not take effect: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("an empty value should delete the header: %q", got)
	}
	if got := req.Header.Get("api-key"); got != "secret-key" {
		t.Errorf("${api_key} was not substituted: %q", got)
	}
}

// Two levels of mapping: on a shared key the route level wins over the
// provider level.
func TestMergeHeadersRouteOverridesProvider(t *testing.T) {
	got := proxy.MergeHeaders(
		map[string]string{"a": "provider", "b": "keep"},
		map[string]string{"a": "route", "c": "added"},
	)
	if got["a"] != "route" || got["b"] != "keep" || got["c"] != "added" {
		t.Fatalf("merge semantics do not match: %v", got)
	}
	if proxy.MergeHeaders(nil, nil) != nil {
		t.Error("both levels empty should return nil")
	}
}

// No client request header is ever forwarded: it guards the fingerprint and
// stops a organization using the egress to forge an identity upstream.
func TestClientHeadersNeverForwarded(t *testing.T) {
	// BuildRequest does not take the client request at all -- its signature
	// rules the possibility out. This test fixes that design in place: if
	// anyone ever adds a client-headers field to Target, the exact-allowlist
	// assertion above turns red immediately.
	target := proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", APIKey: "k", Path: "/v1/x",
	}
	req, _ := proxy.BuildRequest(context.Background(), target, []byte(`{}`))
	for k := range req.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-") &&
			!slices.Contains(proxy.OutboundAllowlist(target), k) {
			t.Errorf("an x- header outside the allowlist appeared: %s", k)
		}
	}
}

// Joining the upstream URL: a trailing slash on the base changes nothing.
func TestUpstreamURLJoin(t *testing.T) {
	for _, base := range []string{"https://up.test", "https://up.test/"} {
		req, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: proxy.ProtocolOpenAI, BaseURL: base, APIKey: "k", Path: "/v1/chat/completions",
		}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := req.URL.String(); got != "https://up.test/v1/chat/completions" {
			t.Errorf("base=%q → %q", base, got)
		}
		if req.Method != http.MethodPost {
			t.Errorf("the method should be POST: %s", req.Method)
		}
	}
}

// Anonymity asserted from the *upstream's* point of view: no outbound header
// may carry the product name, and the user agent must be byte-identical to a
// stock Go client connecting directly.
//
// This cannot be checked against OutboundAllowlist -- that list is written by
// us, and if it were wrong the check would be wrong with it. So a real test
// upstream is started and what it actually receives is inspected.
func TestUpstreamSeesNoProductFingerprint(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	// Take a stock Go client's baseline at the same time: no headers set,
	// connecting directly to the same upstream.
	var baseline http.Header
	baseUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		baseline = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer baseUp.Close()
	stock, err := http.NewRequest(http.MethodPost, baseUp.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(stock)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	for _, protocol := range []proxy.Protocol{proxy.ProtocolOpenAI, proxy.ProtocolAnthropic} {
		req, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: protocol, BaseURL: up.URL, APIKey: "sk-secret", Path: "/v1/x",
		}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := up.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		for name, vals := range got {
			for _, v := range vals {
				if strings.Contains(strings.ToLower(name+v), "fairlb") {
					t.Errorf("the %s surface leaks the product name upstream: %s: %s", protocol, name, v)
				}
			}
		}
		// The user agent must match the stock client's; any difference is
		// itself an identifiable fingerprint.
		if ua, want := got.Get("User-Agent"), baseline.Get("User-Agent"); ua != want {
			t.Errorf("%s surface user agent = %q, a stock Go client sends %q", protocol, ua, want)
		}
	}
}
