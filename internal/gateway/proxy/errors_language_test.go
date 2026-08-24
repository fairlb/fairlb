package proxy

import (
	"encoding/json"
	"github.com/fairlb/fairlb/foundation/testutil/testx"
	"net/http/httptest"
	"testing"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// What these tests pin down is the behaviour of `Write` itself: which message
// reaches the client for a given status.
//
// Three rules together describe that behaviour, and each needs its own test,
// because any one of them can be broken while the others stay green:
//
//   - a 5xx swaps in the generic message, whatever the call site passed;
//   - a 4xx passes the call site's message through verbatim;
//   - upstream text, where there is any, outranks both.
//
// What they deliberately do *not* check is whether a particular call site wrote
// its message in English. A test cannot reach that: enumerating the call sites
// means copying them, and a copy only ever proves that the copy matches.
//
// **The first version of this file was wrong, recorded here so it is not
// repeated**: it passed `Write` a non-English message and then asserted the
// emitted string was English. But a 4xx passes the message through verbatim, so
// what it asserted against was *the very string it had just supplied* -- the
// criterion was anchored to its own input and proved nothing, while still
// presenting as a failing test that looked like a real defect.

func messageOf(t *testing.T, surface Surface, e *Error) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Write(rec, surface, e)
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s/%s: parsing the response body failed: %v", surface, e.Code, err)
	}
	return body.Error.Message
}

// Every 5xx swaps in the English generic message, *whatever the call site
// passed*.
//
// This rule has the widest reach: every 5xx shares that one sentence, and a 5xx
// is exactly what outside monitoring picks up most often. A non-English string
// is fed in deliberately -- here it is not the expected value but the
// *contaminant*, and what is asserted is that the contaminant did not get out.
// That is a different thing from the circular assertion described above.
func TestServerErrorsAlwaysUseEnglishGenericMessage(t *testing.T) {
	fiveXX := []string{
		errcode.GatewayInternal,           // 500
		errcode.GatewayUpstreamTimeout,    // 502
		errcode.GatewayAllProvidersFailed, // 502
		errcode.GatewayModelUnpriced,      // 503
		errcode.GatewayModelDisabled,      // 503
	}
	for _, surface := range []Surface{SurfaceOpenAI, SurfaceAnthropic} {
		for _, code := range fiveXX {
			got := messageOf(t, surface, NewError(code, "интérnal wording that must never reach a client"))
			if got != genericServerMessage {
				t.Errorf("%s/%s: a 5xx should swap in the generic message\n got: %q\nwant: %q",
					surface, code, got, genericServerMessage)
			}
			if testx.HasCJK(got) {
				t.Errorf("%s/%s: the outward message contains non-English text %q", surface, code, got)
			}
		}
	}
}

// A 4xx *passes the call site's message through verbatim*. That is Write's
// contract, and this test pins the contract itself.
//
// It is the exact opposite of the previous one: that requires a swap, this
// requires no swap. Only together do they describe what `Write` does. Pin only
// one of them and inverting the 5xx condition -- writing `< 500`, say -- still
// leaves the other passing.
func TestClientErrorsPassCallSiteMessageThrough(t *testing.T) {
	const sentinel = "call-site sentinel 12345"
	fourXX := []string{
		errcode.GatewayInvalidApiKey,       // 401
		errcode.GatewayInsufficientCredits, // 402
		errcode.GatewayOrgSuspended,        // 403
		errcode.GatewayModelNotFound,       // 404
		errcode.GatewayRequestTooLarge,     // 413
		errcode.GatewayRateLimited,         // 429
		errcode.GatewayInvalidRequest,      // 400
	}
	for _, surface := range []Surface{SurfaceOpenAI, SurfaceAnthropic} {
		for _, code := range fourXX {
			if got := messageOf(t, surface, NewError(code, sentinel)); got != sentinel {
				t.Errorf("%s/%s: a 4xx should pass its message through verbatim\n got: %q\nwant: %q",
					surface, code, got, sentinel)
			}
		}
	}
}

// The generic message is itself English. This is a *separate* test: the one
// above asserts that a 5xx uses it, this asserts that it is English. Change the
// constant to something non-English and the one above stays green.
func TestGenericServerMessageIsEnglish(t *testing.T) {
	if genericServerMessage == "" {
		t.Fatal("genericServerMessage is empty -- every 5xx would carry an error with no explanation")
	}
	if testx.HasCJK(genericServerMessage) {
		t.Errorf("genericServerMessage contains non-English text %q, and it is the outward wording of every 5xx",
			genericServerMessage)
	}
}

// The upstream's own text passes through verbatim, and must *not* be translated
// away by the "outward text is English" rule.
//
// This is the guard rail on that rule: taken to its extreme, the next step is
// "then translate the non-English errors the upstream sends too", and that
// means putting words in the upstream's mouth -- a developer needs the original
// wording to locate the bad parameter.
//
// The sample is therefore deliberately **neither English nor ASCII**: it has to
// be unmistakably the upstream's own words rather than something this code
// could have produced, so that a translation applied anywhere along the path is
// visible in the byte comparison.
func TestUpstreamMessageIsPassedThroughVerbatim(t *testing.T) {
	const upstream = "Модель не поддерживает параметр: top_k"
	for _, surface := range []Surface{SurfaceOpenAI, SurfaceAnthropic} {
		got := messageOf(t, surface, &Error{
			Code:            errcode.GatewayInvalidRequest,
			Message:         "Invalid request",
			UpstreamMessage: upstream,
		})
		if got != upstream {
			t.Errorf("%s: the upstream text was altered\n got: %q\nwant: %q", surface, got, upstream)
		}
	}
}

// Upstream text wins on a 5xx too: Write's rule is that the generic message is
// not substituted when there is upstream text.
//
// This branch is pinned on its own: it is the *exception* to the 5xx rule, and
// an exception is what a refactor most easily flattens away -- simplify the
// condition to "every 5xx is swapped" and the three tests above all stay
// green.
func TestUpstreamMessageWinsOverGenericOn5xx(t *testing.T) {
	const upstream = "upstream said: capacity exceeded"
	got := messageOf(t, SurfaceOpenAI, &Error{
		Code:            errcode.GatewayAllProvidersFailed, // 502
		Message:         "All providers failed",
		UpstreamMessage: upstream,
	})
	if got != upstream {
		t.Errorf("with upstream text on a 5xx the upstream text should be sent\n got: %q\nwant: %q", got, upstream)
	}
}
