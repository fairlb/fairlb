package httpx

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/errcode"
)

// KeyFunc extracts the rate-limit key from a request; an empty string skips
// rate limiting for that request.
type KeyFunc func(r *http.Request) string

// IPKey rate-limits by client IP.
func IPKey(r *http.Request) string { return "ip:" + ClientIP(r) }

// PrefixedIPKey returns a namespaced per-IP key. Two limit tiers sharing one
// driver must use different key spaces, or they overwrite each other's bucket
// parameters.
func PrefixedIPKey(prefix string) KeyFunc {
	return func(r *http.Request) string { return prefix + ":ip:" + ClientIP(r) }
}

// ForPathPrefix applies mw only to requests whose path matches the prefix, e.g.
// a stricter rate-limit tier for authentication endpoints. The prefix is written
// as the full path on that plane, e.g. "/api/v1/auth/".
func ForPathPrefix(prefix string, mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, prefix) {
				wrapped.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit consults the limiter and writes the X-RateLimit-* headers; over the
// limit it answers 429 with Retry-After. A limit of zero or less disables the
// middleware entirely. A limiter failure fails open and is logged: losing the
// rate-limit backend should degrade throughput protection, not availability.
func RateLimit(lim ratelimit.Limiter, key KeyFunc, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k == "" {
				next.ServeHTTP(w, r)
				return
			}
			res, err := lim.Allow(r.Context(), k, limit, window)
			if err != nil {
				slog.ErrorContext(r.Context(), "rate limiter failed, allowing the request", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.Itoa(res.Limit))
			h.Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
			h.Set("X-RateLimit-Reset", strconv.FormatInt(res.ResetAt.Unix(), 10))
			if !res.Allowed {
				h.Set("Retry-After", strconv.Itoa(int(max(res.RetryAfter/time.Second, 1))))
				Error(w, r, errcode.CommonRateLimited, "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RealIP recovers the client address when the service runs behind trusted
// proxies (TRUST_PROXY).
//
// hops is how many trusted proxies are actually in front (TRUST_PROXY_HOPS,
// default 1). Every trusted proxy on the chain appends the peer it saw to the
// end of X-Forwarded-For, so counting hops entries from the right lands on the
// last node the client could not forge.
//
// Both directions of error are real, and they fail differently:
//
//   - Too far left picks a segment the client wrote itself, so per-IP rate
//     limiting can be bypassed with a forged header.
//   - Too far right, when a CDN is in front, collapses every user onto the
//     handful of edge addresses of that CDN, and they throttle each other.
//
// If there are fewer entries than hops, the leftmost one is used: that is the
// real peer as seen by the first trusted proxy, which is what a request that
// bypassed the front proxy and hit the origin directly looks like. With hops > 1
// the defense against that shape is network-level — only the front proxy may
// reach the origin — so raising hops has to be done together with locking the
// origin down, not before.
//
// Disabled entirely for direct-to-internet deployments.
func RealIP(trustProxy bool, hops int) func(http.Handler) http.Handler {
	if hops < 1 {
		// Config loading already rejects invalid values; this covers callers
		// that construct the middleware directly with a zero value.
		hops = 1
	}
	return func(next http.Handler) http.Handler {
		if !trustProxy {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				idx := len(parts) - hops
				if idx < 0 {
					idx = 0
				}
				if ip := strings.TrimSpace(parts[idx]); ip != "" {
					r.RemoteAddr = net.JoinHostPort(ip, "0")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the request's client address. Behind a trusted proxy RealIP
// has already written the recovered address into RemoteAddr.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
