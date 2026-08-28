package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/upstream"
)

// Building the outbound request, and keeping the gateway invisible to the
// upstream.
//
// *Upstream anonymity*: the upstream should see one ordinary API call and no
// sign that a gateway sits in the middle. Three things achieve it:
//
//   - Headers are built from an *allowlist*: start from nothing and add only
//     what the protocol requires plus the configured mappings. The reverse --
//     copy the client's headers and delete a few -- will always miss whatever
//     header appears next.
//   - httputil.ReverseProxy is not used: it appends X-Forwarded-For on its own,
//     which is the most recognisable proxy fingerprint there is.
//   - No client request header is ever forwarded. That guards the fingerprint
//     and also stops a organization using the egress to forge an identity upstream.
//
// The egress IP is still the server's. No gateway can hide that, and it is not
// part of what this promises.

// Headers the protocol requires on the way out. Nothing else may be written
// except through a header mapping.
const (
	hdrAuthorization           = "Authorization"
	hdrContentType             = "Content-Type"
	hdrAccept                  = "Accept"
	hdrAPIKey                  = "x-api-key"
	hdrGoogAPIKey              = "x-goog-api-key"
	hdrAnthropicVer            = "anthropic-version"
	defaultAnthropicAPIVersion = "2023-06-01"
	// apiKeyPlaceholder is what a header mapping writes to mean "the provider
	// credential here".
	apiKeyPlaceholder = "${api_key}"
)

// *No User-Agent is set*, deliberately.
//
// It once defaulted to the product name, which contradicts the anonymity stated
// above outright: any provider without a header mapping would announce the
// gateway to the upstream on every request.
//
// Hard-coding "Go-http-client/1.1" is wrong too: Go's default UA under HTTP/2
// is "Go-http-client/2.0", so hard-coding either one produces, under the other
// protocol, an inconsistency a stock client would never have -- which is itself
// a fingerprint. So none is set, and the transport fills in its own default for
// whichever protocol was actually negotiated. The outbound UA is therefore
// byte-identical to any directly connecting Go program's.
//
// An upstream that needs a particular UA -- some services key quota or routing
// off it -- gets one through an explicit header mapping: Go only supplies its
// default when the header is absent, so the mapping takes effect as usual.

// Target is where one forward is going: a candidate from the catalog plus the
// decrypted credential.
type Target struct {
	Protocol Protocol
	BaseURL  string
	APIKey   string
	Path     string // upstream path, e.g. /v1/chat/completions
	Stream   bool
	// Headers is the merged header mapping, route level already applied over
	// provider level.
	Headers map[string]string
	// Transport is the provider's addressing profile: which header carries the
	// credential, query parameters this upstream requires, a path shape that
	// differs from the standard one. The zero value is the ordinary case and
	// reproduces the behaviour that existed before the profile did.
	Transport catalog.Transport
	// UpstreamModel is what this upstream calls the model. It is read to
	// substitute the {model} placeholder in a path override, and by the two
	// hosted envelopes, which want the model in the address rather than in the
	// body.
	UpstreamModel string
	// Resource substitutes {resource} for stateful response/interaction
	// operations. It never comes from a model body and is escaped as one path
	// segment before URL assembly.
	Resource string
	// Method is the HTTP method; empty means POST, which every forwarded
	// request uses.
	//
	// It is a field rather than something the caller sets afterwards because a
	// signed request cannot be edited once built: the method is part of what
	// the signature covers, so changing it after the fact produces a request
	// the upstream rejects as an authentication failure.
	Method string
	// ExtraQuery is added to the query the profile already contributes, for
	// callers that need a parameter of their own -- a pagination cursor, say.
	// Same reason as Method: it has to be in place before the request is
	// signed, so it cannot be appended to the finished URL.
	ExtraQuery map[string]string
	// AbsoluteURL, when set, is the whole address and bypasses the base URL,
	// the profile's path overrides and the model substitution. Only the video
	// plane's artifact fetch uses it: the upstream reports where the bytes are,
	// and that address is not a path relative to its API.
	AbsoluteURL string
	// OmitCredential suppresses every form of authentication on this request.
	//
	// It exists for one case: an artifact whose address already carries its own
	// authorisation -- a presigned CDN link. Sending ours there would hand the
	// upstream API key to a third party, and sending an empty one produces a
	// bare "Bearer " that some hosts reject outright.
	OmitCredential bool
}

// endpointURL assembles the address: the profile's path override if there is
// one, then the query parameters it requires.
//
// The query is set through URL.Query rather than being folded into the path.
// url.JoinPath escapes "?" into the path, so a question mark reaching it turns
// the parameters into part of the path name -- an address that looks almost
// right in a log and cannot work.
func endpointURL(t Target) (string, error) {
	// An absolute URL is the address, not a path under one. Artifact downloads
	// name a host of the upstream's choosing, and joining that onto the
	// provider's base URL yields something like
	// https://api.vendor.test/https:/cdn.other/v.mp4 -- a path that exists
	// nowhere and answers 404.
	if t.AbsoluteURL != "" {
		return t.AbsoluteURL, nil
	}
	path := t.Transport.PathFor(t.Path, t.UpstreamModel, t.Stream)
	path = strings.ReplaceAll(path, "{resource}", url.PathEscape(t.Resource))
	endpoint, err := url.JoinPath(strings.TrimRight(t.BaseURL, "/"), path)
	if err != nil {
		return "", fmt.Errorf("proxy: joining the upstream URL: %w", err)
	}
	if len(t.Transport.Query) == 0 && len(t.ExtraQuery) == 0 {
		return endpoint, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("proxy: parsing the upstream URL: %w", err)
	}
	q := u.Query()
	for k, v := range t.Transport.Query {
		q.Set(k, v)
	}
	// The caller's own parameters are added on top of the profile's rather than
	// replacing them: a mandatory parameter dropped from the second page
	// onwards reads like the upstream's pagination being broken.
	for k, v := range t.ExtraQuery {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// methodOf is the method to send, defaulting to the one every forward uses.
func methodOf(t Target) string {
	if t.Method == "" {
		return http.MethodPost
	}
	return t.Method
}

// authHeaderFor maps the profile's auth mode onto the wire: which header name
// carries the credential, and whether it is prefixed as a bearer token.
//
// The mapping lives here rather than in the catalog because header names are
// part of the outbound format, and this file is where the outbound format is
// written down.
func authHeaderFor(t Target) (name string, bearer bool) {
	switch mode := t.Transport.AuthMode(string(t.Protocol)); mode {
	case catalog.AuthAPIKey:
		return hdrAPIKey, false
	case catalog.AuthGoogAPIKey:
		return hdrGoogAPIKey, false
	case catalog.AuthBearer:
		return hdrAuthorization, true
	case catalog.AuthAWSSigV4, catalog.AuthGCPServiceAccount, catalog.AuthKlingJWT:
		// All three derive their value per request -- a signature over this
		// exact request, a token that expires within the hour, a token that
		// expires within the half hour -- and all three put the result in
		// Authorization. The value is written later, once every other header is
		// in place, because a signature covers the headers that exist when it
		// is computed.
		return hdrAuthorization, false
	default:
		return http.CanonicalHeaderKey(strings.TrimPrefix(mode, catalog.AuthHeaderPrefix)), false
	}
}

// setAuth writes the credential in the shape this provider expects, and the
// version header the Anthropic dialect requires.
//
// The dialect decides when the profile is silent, so a provider with no profile
// is addressed exactly as it was before profiles existed.
//
// The derived modes are skipped here and handled after every other header is
// written; setAuth would have nothing to copy for them anyway.
func setAuth(h http.Header, t Target) {
	if t.OmitCredential {
		return
	}
	mode := t.Transport.AuthMode(string(t.Protocol))
	if !upstream.Handles(mode) {
		name, bearer := authHeaderFor(t)
		if bearer {
			h.Set(name, "Bearer "+t.APIKey)
		} else {
			h.Set(name, t.APIKey)
		}
	}
	// An envelope carries the version in the body instead, so sending the
	// header as well would put the same setting on the wire twice, once where
	// the upstream reads it and once where nothing does. The second copy is
	// pure gateway fingerprint.
	if t.Protocol == ProtocolAnthropic && t.Transport.BodyEnvelope() == catalog.EnvelopeNone {
		// The client's own version header is never forwarded -- no client
		// header is -- so this is the only place the value can come from. A
		// provider that has pinned one gets it; everything else gets the
		// gateway's default.
		version := t.Transport.AnthropicVersion
		if version == "" {
			version = defaultAnthropicAPIVersion
		}
		h.Set(hdrAnthropicVer, version)
	}
}

// acceptFor is what this request will take back.
//
// Streaming is normally SSE, but one envelope answers a streamed request with a
// binary frame protocol of its own and asking it for SSE gets a refusal rather
// than a conversion. The frames are re-framed as SSE on the way back, which is
// why nothing downstream has to know this happened.
func acceptFor(t Target) string {
	switch {
	case t.Stream && t.Transport.BodyEnvelope() == catalog.EnvelopeBedrock:
		return upstream.EventStreamContentType
	case t.Stream:
		return "text/event-stream"
	default:
		return "application/json"
	}
}

// refuseCredentialInterpolation stops a header mapping from copying a derived
// credential into a header of its own choosing.
//
// The ${api_key} placeholder means "the provider credential", and for the
// copied modes that is a token going where the operator says it should go --
// the whole point of the mapping. For a derived mode the same placeholder now
// expands to a *credential document*: an access key pair, or a service-account
// file with a private key in it. Interpolating that into an arbitrary header
// sends a private key to the upstream in clear text, past every log redaction
// rule, and the operator who typed the placeholder was not asking for that.
//
// Refused rather than silently emptied. An empty expansion would produce a
// header that is present and meaningless, which is the failure this codebase
// keeps refusing to ship: a setting that appears to work.
func refuseCredentialInterpolation(t Target) error {
	if !upstream.Handles(t.Transport.AuthMode(string(t.Protocol))) {
		return nil
	}
	for name, value := range t.Headers {
		if strings.Contains(value, apiKeyPlaceholder) {
			return fmt.Errorf(
				"proxy: the header mapping for %q asks for %s, but this provider's credential "+
					"is a document rather than a token and must not be copied into a header",
				name, apiKeyPlaceholder)
		}
	}
	return nil
}

// presentCredential writes the derived credential, if this provider uses one.
// It runs last on purpose: a signature covers the header set as it stands when
// it is computed, so anything written afterwards either invalidates it or
// travels outside it.
func presentCredential(ctx context.Context, req *http.Request, t Target, payload []byte, payloadReadable bool) error {
	if t.OmitCredential {
		return nil
	}
	mode := t.Transport.AuthMode(string(t.Protocol))
	if !upstream.Handles(mode) {
		return nil
	}
	return upstream.Present(ctx, upstream.Presentation{
		Mode: mode, Profile: t.Transport, Secret: t.APIKey,
		HTTP: req, Payload: payload, PayloadReadable: payloadReadable,
	})
}

// MergeHeaders merges the two levels of header mapping; on a shared key the
// route level wins over the provider level.
func MergeHeaders(providerLevel, routeLevel map[string]string) map[string]string {
	if len(providerLevel) == 0 && len(routeLevel) == 0 {
		return nil
	}
	out := make(map[string]string, len(providerLevel)+len(routeLevel))
	for k, v := range providerLevel {
		out[k] = v
	}
	for k, v := range routeLevel {
		out[k] = v
	}
	return out
}

// BuildRequest builds the outbound request. Headers are constructed from the
// allowlist and then the mapping is applied; client headers are never copied.
func BuildRequest(ctx context.Context, t Target, body []byte) (*http.Request, error) {
	endpoint, err := endpointURL(t)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(withConnectTimeout(ctx, t), methodOf(t), endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("proxy: building the upstream request: %w", err)
	}

	// Accept-Encoding and User-Agent are both left to the transport: the first
	// is content negotiation, the second is explained above, and in both cases
	// the transport's own default is exactly what "looks like a direct
	// connection" requires.
	h := req.Header
	h.Set(hdrContentType, "application/json")
	h.Set(hdrAccept, acceptFor(t))

	// Rewrite the authentication header; the two dialects shape it differently,
	// and the provider's profile overrides that where an upstream needs it.
	setAuth(h, t)

	// The header mapping is applied last of the copied headers and may override
	// anything above: some hosted deployments want an api-key header instead of
	// a bearer token, some relays want an attribution header, and so on. An
	// empty value deletes the header.
	if err := refuseCredentialInterpolation(t); err != nil {
		return nil, err
	}
	applyHeaderMapping(h, t.Headers, t.APIKey)

	// A derived credential comes after the mapping, not before: a mapping that
	// edited a signed header afterwards would leave a signature covering a
	// request that no longer exists, and the upstream would reject it as a bad
	// credential.
	if err := presentCredential(ctx, req, t, body, true); err != nil {
		return nil, err
	}
	return req, nil
}

// applyHeaderMapping applies the header mapping: a new key is added, an
// existing one overwritten, an empty value deletes. `${api_key}` inside a value
// is replaced with the decrypted provider credential, so the credential itself
// never has to sit in plaintext configuration.
func applyHeaderMapping(h http.Header, mapping map[string]string, apiKey string) {
	for k, v := range mapping {
		if v == "" {
			h.Del(k)
			continue
		}
		h.Set(k, strings.ReplaceAll(v, apiKeyPlaceholder, apiKey))
	}
}

// OutboundAllowlist is the baseline set of outbound headers: with no mapping
// configured, an outbound request's header set must be *exactly* these. Any
// extra header means a proxy fingerprint is leaking. It is exported so tests
// can assert on it -- that is the only machine-checkable form the anonymity
// principle has.
//
// It takes the whole target rather than a dialect and a profile, because the
// header set depends on both of those *and* on the credential: a signature adds
// two headers always and a third only for a temporary credential. Narrowing the
// input to what seems relevant is how this list would quietly stop describing
// signing providers, and the exactness assertion -- the part that makes it
// worth having -- would then have to be relaxed for exactly the providers whose
// header set is hardest to predict.
//
// Note what that costs, because it was measured rather than reasoned about: this
// list and the request builder now resolve the auth header through the same
// function, so a test comparing the two cannot see a wrong header name -- only
// an extra one. That is what this list is for and it still does it. Which header
// is the *right* one is anchored separately, by a test that spells the names out
// by hand; deleting that test leaves this axis unwitnessed.
//
// The names come back *canonicalised*, in http.Header's map-key form. Case is
// not a fingerprint: HTTP/1.1 header names are case-insensitive and HTTP/2
// mandates lower case, so under either transport the upstream sees what a
// direct connection would send.
//
// Note that User-Agent is *not* in the allowlist: this layer does not set it,
// the transport supplies the protocol's default as the request goes out (see
// above), and so it never appears in req.Header.
func OutboundAllowlist(t Target) []string {
	authName, _ := authHeaderFor(t)
	names := []string{hdrContentType, hdrAccept, authName}
	if t.Protocol == ProtocolAnthropic && t.Transport.BodyEnvelope() == catalog.EnvelopeNone {
		names = append(names, hdrAnthropicVer)
	}
	names = append(names,
		upstream.ExtraHeaders(t.Transport.AuthMode(string(t.Protocol)), t.APIKey)...)
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = http.CanonicalHeaderKey(n)
	}
	return out
}

// BuildRequestStream builds an outbound request whose body is a stream, used
// for forwarding multipart uploads. The only differences from BuildRequest are
// that Content-Type comes from the original request (it carries the boundary)
// and the body is a Reader rather than a byte slice. Headers are still built
// from the allowlist and upstream anonymity is unchanged.
func BuildRequestStream(ctx context.Context, t Target, body io.Reader, contentType string) (*http.Request, error) {
	endpoint, err := endpointURL(t)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(withConnectTimeout(ctx, t), methodOf(t), endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("proxy: building the upstream request: %w", err)
	}
	h := req.Header
	// The boundary has to travel verbatim or the upstream cannot parse the
	// multipart body.
	h.Set(hdrContentType, contentType)
	h.Set(hdrAccept, "application/json")
	setAuth(h, t)
	if err := refuseCredentialInterpolation(t); err != nil {
		return nil, err
	}
	applyHeaderMapping(h, t.Headers, t.APIKey)
	// The body is a reader here, so a mode that has to hash the whole payload
	// cannot be satisfied and says so. No upstream reached by a signature takes
	// a multipart upload, so this refuses a combination rather than a use.
	if err := presentCredential(ctx, req, t, nil, false); err != nil {
		return nil, err
	}
	return req, nil
}
