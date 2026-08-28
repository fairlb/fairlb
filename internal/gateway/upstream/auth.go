// Package upstream carries the parts of reaching an upstream that are neither
// addressing nor payload: credentials that have to be *derived* per request
// rather than copied, and wire framings that have to be unwrapped before the
// rest of the gateway can read them.
//
// Both belong together because both are answers to the same question -- what
// this particular platform demands of the envelope -- and neither may look at
// what the caller asked for. Nothing in this package reads a message, a
// temperature or a tool definition. The signature covers the bytes without
// inspecting them; the framing moves bytes between two containers without
// deciding anything about their contents.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Presentation is one outbound request that needs a derived credential.
type Presentation struct {
	// Mode is the resolved auth mode from the provider's profile.
	Mode string
	// Profile carries the non-secret half: where an AWS request is signed for.
	Profile catalog.Transport
	// Secret is the decrypted provider credential. Both derived modes store a
	// JSON document rather than a bare token, because both need more than one
	// field and a delimiter-joined string cannot say which field is missing.
	Secret string
	// HTTP is the request being sent. It must already carry every other header:
	// a signature covers the headers that exist when it is computed, so
	// anything added afterwards either invalidates it or travels unsigned.
	HTTP *http.Request
	// Payload is the exact bytes of the request body.
	Payload []byte
	// PayloadReadable is false when the body is a stream rather than a buffer.
	// Signing needs a hash of the whole payload, so it cannot proceed; a bearer
	// token does not care.
	PayloadReadable bool
}

// Handles reports whether a mode needs this package. Everything else is a
// header the request builder writes on its own.
func Handles(mode string) bool {
	return mode == catalog.AuthAWSSigV4 ||
		mode == catalog.AuthGCPServiceAccount ||
		mode == catalog.AuthKlingJWT
}

// Present writes the derived credential onto the request.
//
// It is called last, after every other header is in place, because that is the
// only ordering under which a signature is valid.
func Present(ctx context.Context, p Presentation) error {
	switch p.Mode {
	case catalog.AuthAWSSigV4:
		return signAWS(ctx, p)
	case catalog.AuthGCPServiceAccount:
		return setGCPBearer(ctx, p)
	case catalog.AuthKlingJWT:
		return setKlingBearer(p)
	default:
		return fmt.Errorf("upstream: %q is not a derived credential", p.Mode)
	}
}

// ExtraHeaders lists the header names a mode adds beyond the one carrying the
// credential, given the credential itself.
//
// It takes the secret because one of them is conditional on it: an AWS
// signature carries a session-token header when, and only when, the stored
// credential is a temporary one. Deciding that from the profile alone is
// impossible, and the alternative -- leaving the header off the list -- would
// mean the exactness check on the outbound header set has to be relaxed for
// every signing provider, which is where it is worth the most.
//
// A secret that will not parse yields the unconditional set. This runs on a
// path that only describes the request; refusing here would turn a bad
// credential into a different and less informative failure than the one the
// send itself produces.
func ExtraHeaders(mode, secret string) []string {
	switch mode {
	case catalog.AuthAWSSigV4:
		names := []string{amzDateHeader, amzContentSHAHeader}
		if c, err := parseAWSCredential(secret); err == nil && c.SessionToken != "" {
			names = append(names, amzSecurityTokenHeader)
		}
		return names
	default:
		return nil
	}
}

// decodeCredential decodes the stored JSON credential into dst, with a message
// that says what shape was expected. The operator pasting a credential into
// this field has a vendor console open, not this source file.
func decodeCredential(secret string, dst any, shape string) error {
	if err := json.Unmarshal([]byte(secret), dst); err != nil {
		return fmt.Errorf(
			"upstream: this provider's credential must be %s, and this one is not valid JSON: %w",
			shape, err)
	}
	return nil
}
