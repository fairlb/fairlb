package httpx_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// The transport must carry its own dialer, keep what the platform default
// provides, and read the bound from the request's context.
//
// The assertions are on configuration rather than on a live dial. That was the
// first version of this test elsewhere in the repo and it is worth not
// repeating: it dialled 192.0.2.1, the range reserved for documentation, and
// *the connection succeeded* -- some networks answer for addresses nobody
// should answer for, so a blackhole address is not an instrument that can be
// relied on.
func TestUpstreamTransportCarriesItsOwnDialer(t *testing.T) {
	tr, ok := httpx.UpstreamTransport().(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport, got %T", httpx.UpstreamTransport())
	}
	// Not `DialContext == nil`. That was this test's first assertion and it
	// could never have failed: Transport.Clone copies the platform default's
	// dialer along with everything else, so the field is non-nil whether or not
	// this package installed anything. A reverse probe that removed the
	// installation left the test green, which is how the weakness surfaced.
	//
	// What has to be true is that the dialer is *ours*, so compare identities.
	// gate-honesty: this skip fires only where http.DefaultTransport is not an
	// *http.Transport, which does not happen on any platform this builds for. If
	// it ever does, what is lost is the one assertion that the dialer is *ours*
	// -- the rest of the function still runs, so the test reports success while
	// the property it exists to guard goes unchecked. The reading to trust is
	// therefore "this test passed and was not skipped"; a skip shows up as SKIP
	// in the test output.
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("the platform default is not an *http.Transport; nothing to compare against")
	}
	ours := reflect.ValueOf(tr.DialContext).Pointer()
	platform := reflect.ValueOf(def.DialContext).Pointer()
	if ours == platform {
		t.Fatal("the transport still carries the platform's dialer, so the connect bound is whatever the platform chose")
	}
	// Cloning the platform default rather than building one is load-bearing: a
	// hand-rolled Transport silently drops these, and nothing errors -- requests
	// simply stop going through the proxy the operator configured.
	if tr.Proxy == nil {
		t.Error("proxy support must survive the clone")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("HTTP/2 must survive the clone")
	}
	// Each caller gets its own clone; sharing one would let one caller's later
	// adjustment reach every other caller.
	other, _ := httpx.UpstreamTransport().(*http.Transport)
	if other == tr {
		t.Error("each call should return its own transport")
	}
}

// The bound a request carries wins, and its absence means the default rather
// than zero -- zero as a duration is an instant failure, so "not configured"
// must not be spelled the same way.
func TestConnectTimeoutFromReadsTheRequestsOwnBound(t *testing.T) {
	base := context.Background()

	if got := httpx.ConnectTimeoutFrom(base); got != httpx.ConnectTimeout {
		t.Errorf("an unmarked context should get the deployment default: %s", got)
	}
	if got := httpx.ConnectTimeoutFrom(httpx.WithConnectTimeout(base, 2*time.Second)); got != 2*time.Second {
		t.Errorf("a destination's own bound should win: %s", got)
	}
	// Zero and negative both mean "not configured". A destination that stored no
	// override must not end up unable to dial at all.
	for _, d := range []time.Duration{0, -1 * time.Second} {
		if got := httpx.ConnectTimeoutFrom(httpx.WithConnectTimeout(base, d)); got != httpx.ConnectTimeout {
			t.Errorf("%s should fall back to the default, got %s", d, got)
		}
	}
}

// The published number and the wired number are the same one. This is the half
// a configuration assertion can carry on its own: it cannot prove the dialer
// honours the bound -- that is the standard library's contract -- but it does
// catch the value drifting away from what the operator was told.
func TestConnectTimeoutMatchesTheDocumentedValue(t *testing.T) {
	if httpx.ConnectTimeout != 5*time.Second {
		t.Errorf("the documented connect timeout is 5s: %s", httpx.ConnectTimeout)
	}
}

// The upstream pool must not inherit the standard library's per-host idle
// limit.
//
// That default is two, and inheriting it is not a visible failure: requests
// still succeed, they just pay a TCP and TLS handshake each because the pool
// refused to keep their connection. The only symptom is latency nobody can
// attribute, which is why this is pinned rather than left to review -- a future
// edit that rebuilds the transport from scratch, or drops the two assignments
// as noise, would silently reintroduce it.
//
// Asserting "not the default, and at least this much" rather than an exact
// number: the point is the ceiling being raised deliberately, not the specific
// value, which is free to be tuned.
func TestUpstreamTransportDoesNotInheritTheDefaultIdleLimit(t *testing.T) {
	rt := httpx.UpstreamTransport()
	tr, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport, got %T", rt)
	}
	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost is %d, which is the stdlib default (%d) or lower: "+
			"a gateway serving concurrent requests to one upstream would discard almost every connection",
			tr.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns (%d) is below MaxIdleConnsPerHost (%d): the process-wide ceiling "+
			"would evict connections the per-host limit was raised to keep", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	// HTTP/2 must survive the clone: it is what keeps the per-host limit from
	// binding at all on upstreams that speak it.
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 was lost: HTTP/2 upstreams would fall back to one request per connection")
	}
}
