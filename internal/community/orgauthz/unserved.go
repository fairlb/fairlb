package orgauthz

import (
	"net/http"
	"strings"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// UnservedSegment is the path segment of the console endpoints this deployment
// does not serve.
//
// # What is not served, and why
//
// BYOK means "the organization brings their own upstream credentials", and it exists
// for a party outside the deployment: the organization uses a contract they signed
// with the upstream themselves, and the gateway forwards and prices at zero
// cost. Here there is one instance, one organisation, and the operator is the
// user: every upstream credential they configure is already their own, and it
// goes through the provider credential endpoints.
//
// Serving both would not add a feature; it would add a second write path with
// no defensible precedence between them. Organization credentials take priority over
// provider credentials during routing, so the same person configuring a
// credential on two different pages would find the less visible one winning.
//
// # Why mount everything and then subtract, rather than mount only what is
// needed
//
// Every deployment shares one console specification and one generated handler,
// and mounting it is a single call. Mounting half of it means hand-writing a
// route table, and that table drifts from the specification.
//
// # Why the block is by path segment rather than by operation id
//
// The first version was middleware that ran inside the generated handler, where
// the operation id is available and which looked more robust. In practice one
// of the four endpoints returned 400 instead of 404: that middleware runs after
// request binding and validation, so a request with an invalid body hits the
// 400 first -- and the difference between 400 and 404 itself tells the caller
// the endpoint exists. The rule is that a refusal must happen before binding.
//
// Blocking by segment also captures any BYOK endpoint the specification grows
// later, so the direction of any miss is the safe one. The correspondence
// between the segment name and the specification is checked by
// TestCEDoesNotServeBYOK, which reads it out of the yaml rather than trusting
// anyone's memory.
const UnservedSegment = "provider-keys"

// RejectUnserved returns 404 for the unserved endpoints, before routing and
// before request binding.
//
// 404 rather than 403: "this deployment has no such resource" and "this API
// does not exist" are the same statement here, while a 403 would tell the
// caller "it is there, you just may not use it" -- a different thing entirely.
func RejectUnserved(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Compare whole segments rather than substrings: a public id could in
		// principle contain the same byte sequence, and what the criterion
		// means is "this segment appears in the path".
		for _, seg := range strings.Split(r.URL.Path, "/") {
			if seg == UnservedSegment {
				httpx.NotFoundHandler().ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
