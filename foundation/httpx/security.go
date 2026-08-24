package httpx

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/errcode"
)

// APISecurityHeaders is the browser security baseline for API planes:
// no MIME sniffing, no caching (responses carry sensitive data), no referrer.
func APISecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// CSPExtra carries per-plane additions to the SPA's Content-Security-Policy.
// The zero value yields exactly the baseline policy, so a deployment that serves
// the SPA and the API from the same origin needs nothing here.
//
// ConnectSrc is for split-host deployments: when the browser app talks to an API
// on another hostname, XHR/fetch to that origin has to be allowed explicitly
// (the API side needs a matching CORS allowance — both halves or neither).
// ScriptSrc/FrameSrc exist for embedded third-party widgets, which typically
// need their script and their iframe origin allowed together.
// Empty strings are filtered out.
type CSPExtra struct {
	ConnectSrc []string
	ScriptSrc  []string
	FrameSrc   []string
}

// SPACSP is spaCSP for callers that compute the policy per request.
func SPACSP(extra CSPExtra) string { return spaCSP(extra) }

// spaCSP builds the least-privilege CSP for the built SPA: no inline scripts,
// no embedding, no base-tag hijacking. style 'unsafe-inline' is the one opening,
// kept for runtime styles from theming and third-party components.
//
// frame-src is emitted only when FrameSrc is non-empty; otherwise the policy
// keeps falling back to default-src 'self'. An explicit frame-src *replaces*
// that fallback, so it must always include 'self'.
func spaCSP(extra CSPExtra) string {
	join := func(directive string, extras []string) string {
		out := directive
		for _, src := range extras {
			if src != "" {
				out += " " + src
			}
		}
		return out
	}
	csp := "default-src 'self'; " + join("script-src 'self'", extra.ScriptSrc) +
		"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
		join("connect-src 'self'", extra.ConnectSrc) + "; "
	if hasNonEmpty(extra.FrameSrc) {
		csp += join("frame-src 'self'", extra.FrameSrc) + "; "
	}
	return csp + "frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

func hasNonEmpty(ss []string) bool {
	for _, s := range ss {
		if s != "" {
			return true
		}
	}
	return false
}

// SPASecurityHeaders is the browser security baseline for the SPA entrypoint.
func SPASecurityHeaders(next http.Handler, extra CSPExtra) http.Handler {
	csp := spaCSP(extra)
	return SPASecurityHeadersFunc(next, func(*http.Request) string { return csp })
}

// SPASecurityHeadersFunc is SPASecurityHeaders with the policy computed per
// request. It exists for allowances that follow a runtime setting — the
// Turnstile origin is let in only while a site key is configured, and that key
// changes without a restart — while the fixed parts stay a constant.
func SPASecurityHeadersFunc(next http.Handler, policy func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy(r))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// BodyLimit caps request bodies. Once the cap is exceeded, subsequent reads
// return *http.MaxBytesError; whoever reads the body (the idempotency
// middleware, or a handler) turns that into a 413.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRFGuard is the cross-site write defense for cookie-authenticated planes,
// in the order OWASP's CSRF Prevention cheat sheet prescribes: safe methods pass
// through; a present Origin must match allowedOrigin exactly (scheme + host +
// effective port); with no Origin, fall back to the origin of Referer; with
// neither header, deny.
//
// allowedOrigin looks like "https://console.example.com". The empty string means
// single-host mode and disables the guard entirely — see CSRFGuardSameHost for
// why that default is dangerous when the value comes from optional config.
//
// Rejecting non-browser clients that send neither header is intended: the cookie
// plane only serves browsers, and programmatic access uses bearer tokens on the
// data plane.
func CSRFGuard(allowedOrigin string) func(http.Handler) http.Handler {
	want, ok := normalizeOrigin(allowedOrigin)
	return func(next http.Handler) http.Handler {
		if allowedOrigin == "" {
			return next
		}
		if !ok {
			panic("httpx: invalid CSRFGuard allowedOrigin: " + allowedOrigin)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			// Bearer credentials are not subject to CSRF: the attack depends on
			// the browser attaching credentials *automatically* (cookies), and
			// browsers never attach an Authorization header cross-site — setting
			// one requires passing a CORS preflight. The exemption is therefore
			// keyed on "a bearer header is present", not on "no Origin header":
			// the latter would let ordinary cross-site form posts through too.
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if got, ok := normalizeOrigin(origin); ok && got == want {
					next.ServeHTTP(w, r)
					return
				}
				Error(w, r, errcode.CommonForbidden, "Cross-site request refused.")
				return
			}
			if referer := r.Header.Get("Referer"); referer != "" {
				if got, ok := normalizeOrigin(referer); ok && got == want {
					next.ServeHTTP(w, r)
					return
				}
			}
			// Neither Origin nor Referer: deny (OWASP — no header, no trust).
			Error(w, r, errcode.CommonForbidden, "Missing Origin or Referer header.")
		})
	}
}

// CSRFGuardSameHost is the CSRF defense for single-host deployments. The test
// changes from "same origin as the configured allowedOrigin" to "same hostname
// as the request's own Host"; everything else matches CSRFGuard (safe methods
// pass, bearer passes, Origin first, Referer as fallback, deny when both are
// absent).
//
// # Why not just reuse CSRFGuard
//
// A self-hosted single instance has no "configured public origin" to compare
// against — it only knows the name it was reached by. Reusing CSRFGuard would
// force every operator to set a public-URL setting, and *not* setting it makes
// the guard silently vanish (CSRFGuard("") returns next unchanged). Putting a
// security precondition behind an optional setting means the default deployment
// has no protection and no way to notice.
//
// # Hostname only — not scheme, not port
//
// TLS usually terminates at a reverse proxy: the browser sends
// Origin: https://host while the app sees r.Host as "host:8080" with r.TLS nil.
// Comparing scheme or port would classify every normal deployment as cross-site.
// The hostname is the one dimension that survives the hop, and it is exactly the
// dimension CSRF is about — an attacker's page lives on a different hostname.
//
// The cost is that this trusts the Host header, so the reverse proxy must pass
// the original Host through (the common ones do by default). If a proxy rewrites
// Host, the browser's Origin no longer matches and the result is a *rejection*,
// not an admission: the failure mode is "every write returns 403", not "the
// defense quietly disappeared".
func CSRFGuardSameHost() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}
			self := config.NormalizeHost(hostOnly(r.Host))
			if origin := r.Header.Get("Origin"); origin != "" {
				if got, ok := originHost(origin); ok && got == self {
					next.ServeHTTP(w, r)
					return
				}
				Error(w, r, errcode.CommonForbidden, "Cross-site request refused.")
				return
			}
			if referer := r.Header.Get("Referer"); referer != "" {
				if got, ok := originHost(referer); ok && got == self {
					next.ServeHTTP(w, r)
					return
				}
			}
			Error(w, r, errcode.CommonForbidden, "Missing Origin or Referer header.")
		})
	}
}

// originHost returns the hostname of an origin/URL (lowercased, port stripped);
// false when it does not parse as an http(s) URL.
func originHost(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return "", false
	}
	return config.NormalizeHost(u.Hostname()), true
}

// hostOnly strips the port from "host:port". r.Host is not a URL, so it cannot
// be handed to url.Parse directly.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// normalizeOrigin canonicalizes an origin/URL to "scheme://host:port", making
// the default port explicit so that comparisons cannot differ on it.
func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := config.NormalizeHost(u.Hostname())
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port), true
}
