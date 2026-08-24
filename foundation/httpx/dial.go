package httpx

import (
	"context"
	"net"
	"net/http"
	"time"
)

// How this deployment dials the machines it calls out to.
//
// The bound lives in one place because more than one part of the process calls
// the same upstreams: the data plane forwards requests, the admin API tests a
// credential and discovers a provider's models, and a background worker probes
// liveness. They differ in how long they will wait for an *answer* -- relaying a
// long completion is not the same as asking whether a host is up -- but they
// have no reason to differ in how long they will wait for a *connection*.
// Differing quietly is how one of them ends up waiting half a minute on a host
// the others gave up on in five seconds.

// ConnectTimeout bounds establishing the TCP connection, separately from the
// budgets that bound waiting for an answer.
//
// It is short on purpose, and it is not the same knob as a response deadline: a
// provider that is thinking is legitimately slow, whereas a provider whose
// address will not accept a connection is not about to start. Without an
// explicit dialer the platform default applies, which is six times this and was
// chosen by nobody.
const ConnectTimeout = 5 * time.Second

// How many idle connections the pool keeps, per upstream host and in total.
//
// These are set because the inherited defaults are wrong for this workload, not
// because bigger is better. http.DefaultTransport leaves MaxIdleConnsPerHost
// unset, which means two: a gateway serving fifty concurrent requests to one
// upstream returns two connections to the pool and closes the other
// forty-eight, so the next batch pays a fresh TCP and TLS handshake each. The
// cost lands as latency on every request and as CPU on the machine, and it is
// invisible from the outside -- nothing errors, requests are just slower than
// they should be.
//
// *Why this matters less than it looks for the big providers, and more than it
// looks for everyone else.* The clone keeps ForceAttemptHTTP2, so an upstream
// that negotiates HTTP/2 multiplexes many requests over one connection and the
// per-host idle limit barely binds. Plenty of upstreams are HTTP/1.1 only --
// a local vLLM or Ollama, a self-hosted endpoint behind nginx, several
// regional providers -- and for those this is the whole ceiling. A gateway
// cannot know in advance which kind it is pointed at, so it must be configured
// for the worse case.
//
// The numbers: per-host is sized for a busy single upstream, and the total is
// per-host times a catalogue's worth of them, so adding providers does not
// quietly start evicting the connections of the busy one. Idle connections cost
// a file descriptor and an upstream-side socket, which is cheap next to a
// handshake per request.
const (
	maxIdleConnsPerHost = 128
	maxIdleConns        = 1024
)

// connectTimeoutCtxKey carries one destination's own connect bound down to the
// dialer.
//
// It travels on the context rather than on a per-destination Transport because
// a Transport owns a connection pool: building one per destination would
// multiply the pools, and cloning one per request would destroy connection
// reuse outright. One Transport, one pool, and the dialer reads the bound from
// the context of the request that triggered the dial.
//
// Two consequences worth stating. A reused connection is not dialled, so the
// bound only applies when a new connection is actually opened -- which is the
// only moment it means anything. And when two requests race to open the first
// connection to a host, whichever wins supplies the bound; since the pool is
// keyed by host and a host belongs to one destination, that is the same value
// either way.
type connectTimeoutCtxKey struct{}

// WithConnectTimeout attaches a destination-specific connect bound. A
// non-positive duration leaves the context alone, so "not configured" and "zero"
// both mean the default rather than an instant failure.
func WithConnectTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, connectTimeoutCtxKey{}, d)
}

// ConnectTimeoutFrom reads the bound a request carries, falling back to the
// deployment-wide default.
func ConnectTimeoutFrom(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(connectTimeoutCtxKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return ConnectTimeout
}

// UpstreamTransport returns the transport for calls to upstream providers: one
// pool, with the dial bound read per request from the context.
//
// It clones the process-wide default rather than building one from scratch:
// that keeps proxy support, HTTP/2 and the connection-pool settings, all of
// which a hand-rolled Transport silently drops -- nothing errors, requests just
// stop going through the proxy the operator configured. Mutating the shared
// default in place would instead change behaviour for every other HTTP client
// in the process, including ones with nothing to do with upstreams.
func UpstreamTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	cloned := t.Clone()
	cloned.MaxIdleConns = maxIdleConns
	cloned.MaxIdleConnsPerHost = maxIdleConnsPerHost
	cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{
			Timeout:   ConnectTimeoutFrom(ctx),
			KeepAlive: 30 * time.Second,
		}).DialContext(ctx, network, addr)
	}
	return cloned
}
