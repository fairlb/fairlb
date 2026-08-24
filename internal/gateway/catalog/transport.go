package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// The per-provider transport profile.
//
// Everything here is about *how to address an upstream*, never about what it is
// asked to do. What it covers is the small set of differences that stop an
// upstream speaking a dialect it genuinely speaks from being reachable at all:
// the credential in a different header, a mandatory query parameter, a path that
// does not carry the version segment the standard one does, a credential that
// has to be signed or exchanged for a token rather than copied.
//
// It stays out of protocol translation, which is the thing this gateway does not
// do. The line is drawn at *judgement*: no field here may decide anything about
// the caller's request. Renaming the model, choosing a temperature default,
// mapping one schema's fields onto another's -- those are translation, they get
// a bill wrong when they guess, and they are absent by construction.
//
// One field does reach the body, and it is worth being precise about why it is
// on this side of the line. Two hosted platforms publish the Anthropic Messages
// API with the request cut differently: the model name is a path segment rather
// than a body field, and an api-version string that is a header everywhere else
// is a body field there. Moving a value between the envelope and the body is not
// a judgement call -- the field is required, its value is fixed by the platform,
// and there is exactly one place it can go. See Envelope below, which is
// declared explicitly rather than inferred, so a profile can never acquire it by
// accident.

// The upstream paths the gateway builds on its own. They double as the key
// space of PathOverrides: an override names the path it replaces, which is
// unambiguous where an endpoint name would not be -- the images endpoint has
// two paths, and "images" cannot say which of them is meant.
const (
	PathChat                 = "/v1/chat/completions"
	PathResponses            = "/v1/responses"
	PathResponsesCompact     = "/v1/responses/compact"
	PathResponsesInputTokens = "/v1/responses/input_tokens"
	PathResponsesResource    = "/v1/responses/{resource}"
	PathResponsesCancel      = "/v1/responses/{resource}/cancel"
	PathResponsesInputItems  = "/v1/responses/{resource}/input_items"
	PathEmbeddings           = "/v1/embeddings"
	PathMessages             = "/v1/messages"
	PathMessagesCountTokens  = "/v1/messages/count_tokens"
	PathModels               = "/v1/models"
	PathImagesGenerate       = "/v1/images/generations"
	PathImagesEdit           = "/v1/images/edits"
	// The Gemini paths carry the model in the address rather than in the body,
	// and choose streaming by the method name rather than by a body flag. Both
	// are properties of that protocol, so the placeholder is substituted in the
	// default path and not only in an override.
	PathGenerateContent          = "/v1beta/models/{model}:generateContent"
	PathStreamGenerateContent    = "/v1beta/models/{model}:streamGenerateContent"
	PathGeminiModels             = "/v1beta/models"
	PathGeminiCountTokens        = "/v1beta/models/{model}:countTokens"
	PathGeminiEmbedContent       = "/v1beta/models/{model}:embedContent"
	PathGeminiBatchEmbedContents = "/v1beta/models/{model}:batchEmbedContents"
	PathGeminiInteractions       = "/v1beta/interactions"
	PathGeminiInteraction        = "/v1beta/interactions/{resource}"
	PathGeminiInteractionCancel  = "/v1beta/interactions/{resource}/cancel"
	modelPlaceholder             = "{model}"
	resourcePlaceholder          = "{resource}"
	maxConnectTimeoutMS          = 60_000
	minConnectTimeoutMS          = 50
)

// UpstreamPaths is the complete set, in a stable order so an error message can
// list it. Anything the gateway can send is in here; anything in here can be
// overridden.
func UpstreamPaths() []string {
	return []string{
		PathChat, PathResponses, PathResponsesCompact, PathResponsesInputTokens,
		PathResponsesResource, PathResponsesCancel, PathResponsesInputItems,
		PathEmbeddings, PathMessages, PathMessagesCountTokens,
		PathModels, PathImagesGenerate, PathImagesEdit,
		PathGenerateContent, PathStreamGenerateContent, PathGeminiModels,
		PathGeminiCountTokens, PathGeminiEmbedContent, PathGeminiBatchEmbedContents,
		PathGeminiInteractions, PathGeminiInteraction, PathGeminiInteractionCancel,
	}
}

// Auth modes. The first two are the dialect defaults; header: carries the
// credential as the bare value of a header the operator names; the last two
// derive the credential per request instead of copying a stored one.
const (
	AuthBearer = "bearer"
	AuthAPIKey = "x-api-key"
	// AuthGoogAPIKey is the Gemini protocol's own header. It is the default
	// there, in the same way bearer is the default for the OpenAI protocol: not
	// a platform's variation, but how that API is authenticated.
	AuthGoogAPIKey = "x-goog-api-key"
	// AuthHeaderPrefix introduces a header name chosen by the operator. The
	// name itself is not resolved here: which header a mode maps to is part of
	// the outbound wire format, and that belongs with the code that writes it.
	AuthHeaderPrefix = "header:"
	// AuthAWSSigV4 signs each request with AWS Signature Version 4. The stored
	// credential is an access key pair rather than a token, and the value that
	// goes on the wire is computed from the request itself, so it cannot be
	// precomputed or cached.
	AuthAWSSigV4 = "aws_sigv4"
	// AuthGCPServiceAccount exchanges a service-account key for a short-lived
	// OAuth2 access token and sends that as a bearer token. The token expires
	// within the hour, so something has to refresh it; that is the whole reason
	// this is a mode rather than "paste the token in as the key".
	AuthGCPServiceAccount = "gcp_service_account"
)

// Envelope names a body framing some hosted platforms require. Empty -- the
// normal case -- means the request goes out exactly as the caller wrote it,
// apart from the two rewrites that have always been there.
//
// An envelope is declared, never inferred. Inferring it from the auth mode
// would be wrong in both directions: the same platform publishes an
// OpenAI-shaped endpoint that takes the same signature and no envelope at all,
// and an operator who signs requests for a proxy in front of one of these
// platforms is not asking for their body to be re-cut.
const (
	EnvelopeNone = ""
	// EnvelopeBedrock is the Anthropic Messages API as hosted on AWS Bedrock:
	// the model is a path segment, anthropic_version is a body field, and
	// streaming is chosen by the endpoint rather than by a body flag. The
	// streamed response is a binary frame protocol rather than SSE.
	EnvelopeBedrock = "bedrock"
	// EnvelopeVertex is the same API as hosted on Google Vertex AI: the model is
	// a path segment and anthropic_version is a body field. Streaming stays a
	// body flag and the streamed response is ordinary SSE.
	EnvelopeVertex = "vertex"
)

// Body values each envelope requires. They are constants of the hosting
// platform, not of a model or a deployment: a profile may pin a different one
// through AnthropicVersion, but leaving it empty has to produce a request that
// works, because an operator cannot be expected to know a magic string to make
// the endpoint answer at all.
const (
	bedrockAnthropicVersion = "bedrock-2023-05-31"
	vertexAnthropicVersion  = "vertex-2023-10-16"
)

// SigV4Profile is the non-secret half of an AWS signature: where the request is
// being signed for. The secret half is the credential, which lives encrypted
// with the provider's keys and never appears in this object -- this one is
// returned by the admin API alongside the rest of the provider record.
type SigV4Profile struct {
	// Region is the signing region, e.g. us-east-1. It cannot be derived from
	// the host: a private endpoint or a proxy in front of the service has a
	// hostname that says nothing about which region it fronts, and guessing
	// produces a signature the service rejects with a message about the
	// credential rather than about the region.
	Region string `json:"region"`
	// Service is the signing service name. Empty means the default below.
	Service string `json:"service,omitempty"`
}

// DefaultSigV4Service is the signing service name used when the profile does
// not pin one.
const DefaultSigV4Service = "bedrock"

// Transport is one provider's profile, already decoded.
type Transport struct {
	// Auth is how the credential is presented; empty means the dialect default.
	Auth string `json:"auth,omitempty"`
	// AnthropicVersion pins the Anthropic API version this provider should
	// receive. Empty means the default for however it is being carried. Where
	// it travels is decided by Envelope: a header on a direct Anthropic
	// endpoint, a body field on the two hosted ones. One setting, because an
	// operator pinning a version is answering one question.
	AnthropicVersion string `json:"anthropic_version,omitempty"`
	// Query is appended to every outbound URL for this provider.
	Query map[string]string `json:"query,omitempty"`
	// PathOverrides replaces a path from UpstreamPaths. A {model} placeholder
	// in the value is substituted with the route's upstream model name.
	PathOverrides map[string]string `json:"path_overrides,omitempty"`
	// StreamPathOverrides is PathOverrides for streamed requests, for upstreams
	// that put streaming at a different address instead of behind a body flag.
	// A path with no entry here falls back to PathOverrides, so a provider whose
	// two endpoints do coincide still needs only one map.
	StreamPathOverrides map[string]string `json:"stream_path_overrides,omitempty"`
	// Envelope is the body framing this upstream requires; see the constants.
	Envelope string `json:"envelope,omitempty"`
	// SigV4 is where AWS requests are signed for. Only meaningful with
	// AuthAWSSigV4, and refused on save with anything else -- a signing region
	// stored against a provider that never signs is a setting that saves and
	// does nothing.
	SigV4 *SigV4Profile `json:"sigv4,omitempty"`
	// ConnectTimeoutMS bounds establishing a connection to this provider. Zero
	// means the gateway's own default. It reaches the dialer through the request
	// context, so it applies wherever the forwarding transport is used and not
	// to the admin-side clients, which carry their own overall bound.
	ConnectTimeoutMS int `json:"connect_timeout_ms,omitempty"`
}

// AuthMode returns how this provider's credential is presented, resolving the
// dialect default when the profile is silent.
//
// The dialect default is part of the current contract: an omitted override
// means bearer for OpenAI and x-api-key for Anthropic. Stored profiles are
// validated before this method is reached.
func (t Transport) AuthMode(protocol string) string {
	switch {
	case t.Auth == AuthBearer, t.Auth == AuthAPIKey, t.Auth == AuthGoogAPIKey,
		t.Auth == AuthAWSSigV4, t.Auth == AuthGCPServiceAccount:
		return t.Auth
	case strings.HasPrefix(t.Auth, AuthHeaderPrefix) &&
		strings.TrimPrefix(t.Auth, AuthHeaderPrefix) != "":
		return t.Auth
	case protocol == ProtocolAnthropic:
		return AuthAPIKey
	case protocol == ProtocolGemini:
		return AuthGoogAPIKey
	default:
		return AuthBearer
	}
}

// SigningRegion and SigningService report where an AWS request is signed for,
// applying the default service name. They read the profile rather than the
// caller's guess so that every construction point signs identically; a
// signature computed for the wrong region fails as an authentication error,
// which sends whoever reads it looking at the credential.
func (t Transport) SigningRegion() string {
	if t.SigV4 == nil {
		return ""
	}
	return t.SigV4.Region
}

func (t Transport) SigningService() string {
	if t.SigV4 == nil || t.SigV4.Service == "" {
		return DefaultSigV4Service
	}
	return t.SigV4.Service
}

// BodyEnvelope reports the framing this provider's body needs. It reads the
// declared value and nothing else: an envelope is never inferred from the auth
// mode, because the same signature also fronts endpoints that want no envelope.
func (t Transport) BodyEnvelope() string {
	switch t.Envelope {
	case EnvelopeBedrock, EnvelopeVertex:
		return t.Envelope
	default:
		return EnvelopeNone
	}
}

// EnvelopeAnthropicVersion is the value the envelope requires in the body:
// whatever the profile pinned, or the hosting platform's own constant.
// Empty when there is no envelope, where the version travels as a header
// instead.
func (t Transport) EnvelopeAnthropicVersion() string {
	env := t.BodyEnvelope()
	if env == EnvelopeNone {
		return ""
	}
	if t.AnthropicVersion != "" {
		return t.AnthropicVersion
	}
	if env == EnvelopeBedrock {
		return bedrockAnthropicVersion
	}
	return vertexAnthropicVersion
}

// The dialect names, declared here rather than imported so this file does not
// depend on the proxy layer, which depends on this one. ProtocolAnthropic is the
// one whose transport defaults differ; ProtocolOpenAI exists so the vendor
// registry and the surface table can name the other one instead of spelling a
// bare string that nothing checks.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	ProtocolGemini    = "gemini"
)

// PathFor returns the path to request, given the one the gateway would use on
// its own, the upstream's name for the model, and whether this request is
// streamed.
//
// A streamed request prefers the streaming map and falls back to the ordinary
// one, so an upstream whose two endpoints coincide -- which is nearly all of
// them -- is still described by a single map.
// The placeholder is substituted in the default path as well as in an override,
// because one protocol puts the model in the address by design rather than as a
// platform's re-cut of somebody else's API. Paths without a placeholder are
// unaffected, which is all of the others.
func (t Transport) PathFor(defaultPath, upstreamModel string, stream bool) string {
	override := ""
	if stream {
		override = t.StreamPathOverrides[defaultPath]
	}
	if override == "" {
		override = t.PathOverrides[defaultPath]
	}
	if override == "" {
		return strings.ReplaceAll(defaultPath, modelPlaceholder, upstreamModel)
	}
	return strings.ReplaceAll(override, modelPlaceholder, upstreamModel)
}

// ConnectTimeout returns the profile's connect bound, or zero for "use the
// gateway's default".
func (t Transport) ConnectTimeout() time.Duration {
	if t.ConnectTimeoutMS <= 0 {
		return 0
	}
	return time.Duration(t.ConnectTimeoutMS) * time.Millisecond
}

// ParseTransport applies the same schema on stored reads as on writes. A
// profile is either fully understood or unusable; silently dropping an address
// or authentication setting would send requests somewhere other than the
// operator configured.
func ParseTransport(raw []byte) (Transport, error) {
	return ValidateTransport(raw)
}

// ValidateTransport validates the single current transport schema: every key
// has to be understood and every value has to be actionable.
//
// Rejecting an unknown key is the difference between this column and the one it
// replaced. That one accepted anything and nothing read it, so a profile typed
// into it was indistinguishable from a profile that worked. A setting that
// saves and then silently does nothing is worse than one that is missing: the
// missing one sends you looking, the silent one tells you the problem is
// somewhere else.
//
// It returns the decoded profile so the caller can store a canonical form.
func ValidateTransport(raw []byte) (Transport, error) {
	if len(raw) == 0 {
		return Transport{}, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Transport{}, fmt.Errorf(
			"the transport profile must be a JSON object: %s", err.Error())
	}
	known := []string{
		"auth", "anthropic_version", "query", "path_overrides",
		"stream_path_overrides", "envelope", "sigv4", "connect_timeout_ms",
	}
	var unknown []string
	for k := range probe {
		if !slices.Contains(known, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Transport{}, fmt.Errorf(
			"unknown transport setting %s; this version understands %s",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}

	var t Transport
	if err := json.Unmarshal(raw, &t); err != nil {
		return Transport{}, fmt.Errorf("the transport profile is malformed: %s", err.Error())
	}

	// Checked before the auth mode, not after. A region typed under a
	// misspelled key leaves Region empty, so the auth check would fire first
	// and say "no region" to somebody looking straight at the region they
	// typed. The precise message has to win.
	if nested, ok := probe["sigv4"]; ok {
		if err := checkSigV4Keys(nested); err != nil {
			return Transport{}, err
		}
	}

	switch {
	case t.Auth == "" || t.Auth == AuthBearer || t.Auth == AuthAPIKey ||
		t.Auth == AuthGoogAPIKey:
	case t.Auth == AuthAWSSigV4:
		// Without a region there is nothing to sign for, and the failure would
		// arrive as an authentication error on every request -- pointing at the
		// credential, which is not what is wrong.
		if t.SigV4 == nil || strings.TrimSpace(t.SigV4.Region) == "" {
			return Transport{}, fmt.Errorf(
				`auth %q needs a signing region: "sigv4": {"region": "us-east-1"}`, t.Auth)
		}
	case t.Auth == AuthGCPServiceAccount:
	case strings.HasPrefix(t.Auth, AuthHeaderPrefix):
		name := strings.TrimPrefix(t.Auth, AuthHeaderPrefix)
		// A header name is a token, and the standard library refuses to send a
		// request carrying one that is not. Checking for a handful of obviously
		// bad characters let names like "foo(bar)" save cleanly and then fail at
		// every request, where the failure reads as the provider being down: the
		// transport error counts against provider health and the circuit opens.
		// The operator is told their upstream is unreachable, which is not what
		// happened.
		if name == "" || !httpguts.ValidHeaderFieldName(name) {
			return Transport{}, fmt.Errorf("auth %q does not name a header", t.Auth)
		}
		// The credential must not be routed into a header that carries something
		// else. Two separate reasons, either one sufficient:
		//
		// Nothing downstream treats these as secret. A credential in
		// Content-Type or User-Agent is a plain field to every access log,
		// tracing agent and CDN between here and the upstream, none of which has
		// a redaction rule for it -- and the value is written where the
		// operator would never think to look for a leak.
		//
		// And the header it displaces is one the request needs. Overwriting
		// Content-Type does not produce an authenticated request; it produces a
		// request the upstream cannot parse, with the credential inside it.
		if reservedOutboundHeader(name) {
			return Transport{}, fmt.Errorf(
				"auth %q would put the credential in a header that carries something else", t.Auth)
		}
	default:
		return Transport{}, fmt.Errorf(
			"unknown auth %q; use %s, %s, %s, %s, %s, or header:<name>",
			t.Auth, AuthBearer, AuthAPIKey, AuthGoogAPIKey, AuthAWSSigV4, AuthGCPServiceAccount)
	}

	// A signing region on a provider that does not sign is exactly the shape
	// this validation exists to refuse: it saves, it reads back, and it has no
	// effect on any request.
	if t.SigV4 != nil && t.Auth != AuthAWSSigV4 {
		return Transport{}, fmt.Errorf(
			"the sigv4 setting only applies with auth %q", AuthAWSSigV4)
	}

	switch t.Envelope {
	case EnvelopeNone, EnvelopeBedrock, EnvelopeVertex:
	default:
		return Transport{}, fmt.Errorf(
			"unknown envelope %q; use %s or %s", t.Envelope, EnvelopeBedrock, EnvelopeVertex)
	}

	for k := range t.Query {
		if strings.TrimSpace(k) == "" {
			return Transport{}, fmt.Errorf("a query parameter name must not be empty")
		}
	}

	for _, m := range []struct {
		setting   string
		overrides map[string]string
	}{
		{"path_overrides", t.PathOverrides},
		{"stream_path_overrides", t.StreamPathOverrides},
	} {
		if err := checkPathOverrides(m.setting, m.overrides); err != nil {
			return Transport{}, err
		}
	}

	if t.ConnectTimeoutMS != 0 &&
		(t.ConnectTimeoutMS < minConnectTimeoutMS || t.ConnectTimeoutMS > maxConnectTimeoutMS) {
		return Transport{}, fmt.Errorf(
			"connect_timeout_ms must be between %d and %d", minConnectTimeoutMS, maxConnectTimeoutMS)
	}
	return t, nil
}

// checkPathOverrides applies the same rules to both override maps. Naming the
// setting in the message matters once there are two of them: "the override for
// /v1/messages is not absolute" does not say which of the two maps to look in.
func checkPathOverrides(setting string, overrides map[string]string) error {
	valid := UpstreamPaths()
	for k, v := range overrides {
		if !slices.Contains(valid, k) {
			return fmt.Errorf(
				"%q in %s is not a path this gateway sends; overridable paths are %s",
				k, setting, strings.Join(valid, ", "))
		}
		if !strings.HasPrefix(v, "/") {
			return fmt.Errorf(
				"the %s entry for %q must be an absolute path, starting with a slash", setting, k)
		}
		// A query string smuggled into the path would be escaped into the path
		// itself when the URL is assembled, producing an address that looks
		// almost right and cannot work. Query parameters have their own key.
		if strings.ContainsAny(v, "?#") {
			return fmt.Errorf(
				"the %s entry for %q must not contain a query string or fragment; use the query setting",
				setting, k)
		}
		if strings.Contains(k, resourcePlaceholder) && !strings.Contains(v, resourcePlaceholder) {
			return fmt.Errorf(
				"the %s entry for %q must retain the %s placeholder", setting, k, resourcePlaceholder)
		}
	}
	return nil
}

// checkSigV4Keys refuses unknown keys inside the nested object, on the same
// reasoning as the top level. Nesting is where a strict rule usually stops
// being applied, and a misspelled key inside an object that is otherwise valid
// is the easiest kind to store and never notice.
func checkSigV4Keys(raw json.RawMessage) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("the sigv4 setting must be a JSON object: %s", err.Error())
	}
	known := []string{"region", "service"}
	var unknown []string
	for k := range probe {
		if !slices.Contains(known, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown sigv4 setting %s; this version understands %s",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return nil
}

// reservedOutboundHeader reports whether a header carries something other than
// the credential on an outbound request.
//
// The list is short on purpose: it names the headers the gateway itself sets on
// every request, plus the one a client would expect to identify the caller.
// A profile is free to use any other header -- vendors do pick unusual ones --
// and the point is not to police the choice but to stop the credential landing
// somewhere it changes the meaning of the request or escapes redaction.
func reservedOutboundHeader(name string) bool {
	switch strings.ToLower(name) {
	case "content-type", "accept", "anthropic-version", "user-agent", "host", "content-length":
		return true
	default:
		return false
	}
}
