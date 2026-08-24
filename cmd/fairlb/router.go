package main

import (
	"github.com/fairlb/fairlb/settings"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/audit"
	"github.com/fairlb/fairlb/foundation/drivers"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/gateway"
	communityconfig "github.com/fairlb/fairlb/internal/community/config"
	communityorgauthz "github.com/fairlb/fairlb/internal/community/orgauthz"
	communitystaffapi "github.com/fairlb/fairlb/internal/community/staffapi"
	communitystaffauth "github.com/fairlb/fairlb/internal/community/staffauth"
	"github.com/fairlb/fairlb/web"
)

const rlWindow = time.Minute

// staffAPIPrefix is where the staff plane is mounted.
const staffAPIPrefix = "/api/staff/v1"

// buildRouter mounts the three planes.
//
// # One host, not three subdomains
//
// A multi-tenant deployment puts the console, the administrator plane and the
// data plane on three subdomains, each behind its own host guard -- that serves
// a SaaS need, where the operations plane can sit behind its own access proxy
// and the data plane can be scaled on its own. A self-hosted instance faces one
// machine with one hostname, and demanding three subdomains only adds two hops
// of reverse-proxy configuration.
// So the planes are split by path on one host, with nothing removed from the
// security stack: CSRF, body size limits, rate limiting and deny-by-default for
// anonymous callers all still apply. What is missing is hostname isolation, not
// a line of defence.
//
// CSRF uses the same-host guard. The origin-based guard needs a configured
// public origin, which a single instance does not have, and handing it an empty
// origin is a silent pass-through. This comment once said "CSRF as usual" while
// nothing at all was mounted -- a defence written down and a defence installed
// are not the same thing.
//
// # The three planes
//
//	/v1/*            data plane: OpenAI/Anthropic-compatible surface, key auth
//	/api/staff/v1/*  administrator plane: provider, model, route and pricing
//	                 configuration, session auth
//	/api/v1/*        console plane: usage, logs, available models -- the same
//	                 administrator session
//
// # Why the console plane uses the administrator session too
//
// Elsewhere that plane authenticates a organization user session. There are no organization
// users here, and the operator *is* the user, so building a second identity
// system for a single person would be pure duplication. Both planes therefore
// share one session. The organisation dimension degrades to the single
// bootstrapped one, and membership is decided by the org authorizer: any other
// organisation id gets a 404.
func buildRouter(
	cfg communityconfig.Config, pool *pgxpool.Pool, gatewayModule *gateway.Module,
	staffSvc *communitystaffauth.Service, drv *drivers.Drivers,
	orgID pgtype.UUID, keys *apikeys.Service, health *httpx.Health, set *settings.Store,
) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RealIP(cfg.TrustProxy, cfg.TrustProxyHops))
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover)

	// The same probe the internal address serves, offered here as well for a
	// proxy that can only reach the public port. It is the shared handler and
	// not a fixed 200 for the sake of the one answer that matters: 503 once
	// shutdown has begun, which is what makes DRAIN_GRACE_SECONDS a window
	// anything can act on rather than a pause nobody observes.
	r.Get("/healthz", health.Up)

	// ── Administrator plane ────────────────────────────────────────────────
	staffR := r.Route(staffAPIPrefix, func(ar chi.Router) {
		ar.Use(httpx.APISecurityHeaders)
		ar.Use(httpx.CSRFGuardSameHost())
		ar.Use(httpx.BodyLimit(2 << 20))
		ar.Use(httpx.AuthDomain(communitystaffauth.NewAuthenticator(staffSvc, cfg.Secure)))
		// Writes leave an audit record. The audit table and the code that
		// writes it are shared, and having the table without writing to it is
		// worse than not having it: the operator assumes the record is there
		// and only discovers it is empty when something goes wrong.
		ar.Use(httpx.Audit(audit.NewHook(pool)))
		// Deny anonymous callers by default and allow only the login
		// endpoints. The direction matters: forgetting to register a new
		// endpoint costs a 401, not an open door.
		ar.Use(httpx.ExceptPaths(anonymousStaffPaths(), httpx.RequireAuthenticated))
		ar.Use(httpx.Idempotency(pool))
		ar.Use(httpx.RateLimit(drv.RateLimit, httpx.IPKey, cfg.RateLimitPerIPRPM, rlWindow))
		// A tighter per-IP budget for the login endpoints: this is the only
		// password entry point, so guessing has to be expensive.
		ar.Use(httpx.ForPathPrefix(staffAPIPrefix+"/auth/",
			httpx.RateLimit(drv.RateLimit, httpx.PrefixedIPKey("staff-auth"),
				cfg.AuthRateLimitPerIPRPM, rlWindow)))
		// `/setup` gets the same tighter budget. It is anonymous, it takes a
		// token when one is configured, and what it creates is the account that
		// owns the instance — the one endpoint where guessing must be expensive.
		ar.Use(httpx.ForPathPrefix(staffAPIPrefix+"/setup",
			httpx.RateLimit(drv.RateLimit, httpx.PrefixedIPKey("staff-setup"),
				cfg.AuthRateLimitPerIPRPM, rlWindow)))
		ar.NotFound(httpx.NotFoundHandler())

		// The three endpoints specific to this deployment: log in, log out,
		// who am I. They go through the generated strict handler rather than
		// being hand-written, because a hand-written endpoint alongside a
		// specified one means two contracts for the same thing.
		communitystaffapi.HandlerFromMux(
			communitystaffapi.NewStrictHandlerWithOptions(
				communitystaffapi.NewServer(pool, staffSvc, keys, orgID, drv.Cache, cfg.Secure,
					communitystaffapi.SetupConfig{Version: version, Token: cfg.SetupToken}, set),
				[]communitystaffapi.StrictMiddlewareFunc{communitystaffapi.WithHTTPRequest},
				communitystaffapi.StrictHTTPServerOptions{
					RequestErrorHandlerFunc:  httpx.OAPIRequestError,
					ResponseErrorHandlerFunc: httpx.OAPIResponseError,
				},
			),
			ar,
		)

	})

	// ── Console plane ──────────────────────────────────────────────────────
	//
	// The same generated strict handler and the same paths as everywhere else,
	// so every frontend can share one feature package. The only differences are
	// the injected organisation authorizer and where the session comes from.
	consoleR := r.Route("/api/v1", func(cr chi.Router) {
		cr.Use(httpx.APISecurityHeaders)
		cr.Use(httpx.CSRFGuardSameHost())
		cr.Use(httpx.BodyLimit(2 << 20))
		cr.Use(httpx.AuthDomain(communitystaffauth.NewAuthenticator(staffSvc, cfg.Secure)))
		cr.Use(httpx.Audit(audit.NewHook(pool)))
		// The whole plane denies anonymous callers with no exemptions at all:
		// there is no login endpoint here (login lives on the administrator
		// plane), so the allowlist is empty.
		cr.Use(httpx.RequireAuthenticated)
		cr.Use(httpx.Idempotency(pool))
		cr.Use(httpx.RateLimit(drv.RateLimit, httpx.IPKey, cfg.RateLimitPerIPRPM, rlWindow))
		// The endpoints this deployment does not serve are refused before
		// request binding; the reasoning is in orgauthz/unserved.go.
		cr.Use(communityorgauthz.RejectUnserved)
		cr.NotFound(httpx.NotFoundHandler())

	})

	// ── Admin UI ───────────────────────────────────────────────────────────
	//
	// One host, so the single-page application is a catch-all rather than
	// dispatched by hostname: one machine, one domain, and `/` is the admin UI.
	//
	// Mounted last: the three routes above have already claimed the API
	// prefixes, and whatever is left falls through to here.
	//
	// With no embedded build, the asset filesystem is nil and the handler
	// renders a one-line placeholder instead of a 404 -- opening `/` during
	// development and reading "use the Vite dev server" answers "did I
	// misconfigure something?" very differently from reading "no such page".
	//
	// The content security policy takes its baseline with no additions: one
	// host, a same-origin data plane, no cross-origin calls and no third-party
	// challenge widget.
	r.Handle("/*", httpx.SPASecurityHeaders(
		httpx.SPA(web.StaffDist(),
			"fairlb: the admin UI is not embedded in this build. Use the Vite dev server in development; production images are built with -tags webembed.\n"),
		httpx.CSPExtra{}))

	// ── Data plane ─────────────────────────────────────────────────────────
	// Both version prefixes get their own router built by the same function, so
	// the middleware cannot drift between them. They do *not* carry the same
	// routes: /v1beta carries only Gemini-native endpoints, because mounting the
	// whole plane there would put the OpenAI-shaped catalogue on
	// GET /v1beta/models, which is where a Gemini client asks for its models and
	// would otherwise read a wrong-shaped 200 as an empty catalogue.
	newDataPlane := func() chi.Router {
		dr := chi.NewRouter()
		dr.Use(httpx.APISecurityHeaders)
		// The data plane's body limit is wider than the administrator plane's:
		// multimodal requests carry images.
		dr.Use(httpx.BodyLimit(64 << 20))
		dr.NotFound(httpx.NotFoundHandler())
		return dr
	}
	dataR, dataBetaR := newDataPlane(), newDataPlane()
	r.Mount("/v1", dataR)
	r.Mount("/v1beta", dataBetaR)

	if err := gatewayModule.Mount(gateway.Planes{
		DataPlane: dataR, DataPlaneV1Beta: dataBetaR, Console: consoleR, Admin: staffR,
	}); err != nil {
		panic(err) // all four planes are constructed immediately above
	}

	return r
}

// publicStaffOps lists the operations reachable without a session, keyed by
// "METHOD /path" within the staff plane.
//
// The values are reasons, not decoration. Adding a line here adds an anonymous
// attack surface, so it should be a decision somebody made rather than a line
// somebody copied; an entry nobody can write a reason for is an entry that
// should not exist. The list is checked against the spec's `security: []`
// markers by TestPublicStaffOperationsMatchTheSpec — two places stating the
// same thing drift, and the direction they drift in is "more open than
// documented".
//
// An allowlist rather than a blocklist: an endpoint nobody remembers to
// register answers 401, which is the failure everyone notices immediately.
var publicStaffOps = map[string]string{
	"GET /meta": "the sign-in page reads it before anyone is signed in, to tell " +
		"'create the first administrator' from 'sign in'",
	"POST /setup": "creates the first administrator; by definition there is no " +
		"account to authenticate as yet",
	"POST /auth/login": "sign-in itself, which comes before being signed in",
}

// anonymousStaffPaths expands publicStaffOps into the full paths the middleware
// matches on.
func anonymousStaffPaths() map[string]bool {
	out := make(map[string]bool, len(publicStaffOps))
	for op := range publicStaffOps {
		method, path, _ := strings.Cut(op, " ")
		out[method+" "+staffAPIPrefix+path] = true
	}
	return out
}
