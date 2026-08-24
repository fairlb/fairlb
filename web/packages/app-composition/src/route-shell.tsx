import { ApiError, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { Centered, ErrorState, LoadingState, NotFoundState, RouteErrorState } from "@fairlb/ui";
import { createRoute, useNavigate, type AnyRoute } from "@tanstack/react-router";
import { useEffect, type ReactNode } from "react";
import type { AdminRoute, AdminRouteChild, Provider } from "./index";

/**
 * The page-level loading state.
 *
 * `data-route-fallback` marks it apart from the `LoadingState` a page renders
 * while its own queries are in flight: the two are identical in the DOM (both
 * `role=status`), so without the marker "a loading state appeared" does not say
 * which one appeared. That ambiguity has already produced one wrong reading —
 * two variants measured as identical because what was being read was never this
 * component. The console shell used to render its fallback without the marker,
 * which is why the same reading could not be taken there at all.
 */
export function RouteFallback() {
  const { t } = useI18n();
  return (
    <div data-route-fallback>
      <LoadingState label={t("loading")} />
    </div>
  );
}

/**
 * The router options every FairLB shell uses.
 *
 * `defaultPendingComponent` — a Suspense boundary per match. Without it, "the
 * click did nothing" is the guaranteed outcome rather than an occasional one:
 * navigation runs inside a React transition, and a transition that meets an
 * *already mounted* boundary keeps the old content on screen. The boundary
 * inside the shell is exactly that, so nothing moves at all while the chunk
 * loads (measured at roughly 1.4s on a gateway page in local dev). A match
 * boundary is newly mounted on every navigation, and React shows a new
 * boundary's fallback immediately, which puts the loading state in the content
 * area and leaves the shell where it is.
 *
 * `defaultPendingMinMs` is its anti-flicker companion: with the chunk already
 * cached the fallback would otherwise appear for a single frame. Both ms
 * options only govern loader pending states and no route in this repository has
 * a loader; they are kept because those are the values wanted the day one
 * appears.
 *
 * `defaultPreload: "intent"` preloads a page's chunk once a link has hover,
 * focus or touch intent. It used to be console-only. The two operator shells
 * are where it buys the most — their pages are the heavy ones — so the
 * divergence was backwards, and the resolution is to give all three the
 * preload rather than to take it away from the one that had it.
 *
 * `notFoundMode: "root"` keeps the deepest fuzzy parent from swallowing an
 * unknown suffix, so a path removed from the registry lands on the root 404
 * instead of an ancestor's page.
 *
 * `defaultErrorComponent` — after a redeploy the chunks an already-open page
 * refers to are gone, and that surfaces here as a route error (ADR-0087).
 */
export const APP_ROUTER_DEFAULTS = {
  defaultPreload: "intent",
  defaultPendingComponent: RouteFallback,
  defaultPendingMs: 0,
  defaultPendingMinMs: 200,
  defaultNotFoundComponent: NotFoundState,
  notFoundMode: "root",
  defaultErrorComponent: ({ error }: { error: unknown }) => <RouteErrorState error={error} />,
} as const;

/**
 * Build one registry page's route, children included.
 *
 * The recursion carries no path context. The cloud shell's copy threaded a
 * `parentPath` through it and computed a full path at each level, but that value
 * never reached `createRoute` — TanStack resolves a child's path against its
 * parent route object, not against a string the builder assembled. The two
 * builders therefore produced identical trees, and one of them did arithmetic
 * to get there.
 */
export function buildPageRoute(parent: AnyRoute, page: AdminRoute | AdminRouteChild): AnyRoute {
  const route = createRoute({
    getParentRoute: () => parent,
    path: page.path,
    component: page.component,
    ...(page.validateSearch ? { validateSearch: page.validateSearch } : {}),
  });
  if (!page.children?.length) return route;
  return route.addChildren(page.children.map((child) => buildPageRoute(route, child)));
}

/**
 * What the guard below needs from a session query. Structural rather than the
 * generated hook's own type, because the three shells call three different
 * generated clients for the same three facts.
 */
export interface SessionQuery<T> {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  data: T | undefined;
  refetch: () => unknown;
}

/**
 * Whether a failed session read means "not signed in".
 *
 * A separate exported function because this one predicate is the whole
 * behavioural difference between the three shells' old guards, and it is worth
 * pinning without standing up a router: a 401 is a logout, and everything else
 * — a 500, a gateway error, a dropped connection, a parse failure — is a fault
 * the reader has to be told about. Treating them alike sends someone to the
 * login page, lets them sign in successfully, and never mentions what broke.
 */
export function sessionIsUnauthenticated(session: {
  isPending: boolean;
  isError: boolean;
  error: unknown;
}): boolean {
  return (
    !session.isPending &&
    session.isError &&
    session.error instanceof ApiError &&
    session.error.status === 401
  );
}

export interface RequireSessionProps<T> {
  session: SessionQuery<T>;
  /** Where an unauthenticated reader is sent. */
  loginPath: string;
  /**
   * Whether the login link carries the destination the reader was refused.
   *
   * True for surfaces people reach by deep link — a shared URL, a bookmark, a
   * link in an alert — where dropping it means signing in and landing somewhere
   * else with no way back.
   */
  carryReturnTo?: boolean;
  /**
   * What to render once the session resolves. It receives the identity.
   *
   * The guard deliberately does **not** wrap the module providers itself. Their
   * position relative to each app's identity context is load-bearing — a module
   * provider reads that context — and it is not the same nesting in every
   * shell. A guard that assumed one order would silently move the other app's
   * providers outside the context they read. Use `wrapProviders` inside this
   * callback, where the order is written down next to the context it belongs to.
   */
  children: (me: T) => ReactNode;
}

/**
 * Nest a module's contributed providers around a tree, outermost first.
 *
 * An empty array hands the tree back untouched, which is what a build with no
 * module mounted gets.
 */
export function wrapProviders(providers: readonly Provider[], tree: ReactNode): ReactNode {
  return providers.reduceRight((inner, Wrapper) => <Wrapper>{inner}</Wrapper>, tree);
}

/**
 * The authentication guard the three shells share.
 *
 * Two rules in it were divergent before, and the divergence was a defect rather
 * than three decisions:
 *
 *  1. **Only a 401 means "not signed in".** The two operator shells treated any
 *     failure that way, so a 500 or a dropped connection sent the operator to
 *     the login page — where they would sign in successfully, land back, and
 *     never be told what actually failed. Everything that is not a 401 gets the
 *     error state with a retry, which is the only screen that can say so.
 *  2. **The refused destination travels with the redirect** where the surface
 *     asks for it, so signing in returns the reader to the page they wanted.
 *
 * The redirect happens in an effect, not in the render body. Calling `navigate`
 * during render triggers a setState inside the router, which React reports as
 * "Cannot update a component while rendering a different component"; the real
 * consequence is intermittent double navigation and effects firing out of
 * order — hard to reproduce and harder to attribute. The effect must also run
 * unconditionally, before any early return, or the hook count changes between
 * renders (React #310).
 */
export function RequireSession<T>({
  session,
  loginPath,
  carryReturnTo = false,
  children,
}: RequireSessionProps<T>) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const unauthed = sessionIsUnauthenticated(session);

  useEffect(() => {
    if (!unauthed) return;
    void navigate(
      carryReturnTo
        ? {
            to: loginPath,
            search: { return_to: `${window.location.pathname}${window.location.search}` },
          }
        : { to: loginPath },
    );
  }, [unauthed, navigate, loginPath, carryReturnTo]);

  if (session.isPending)
    return (
      <Centered>
        <LoadingState label={t("loading")} />
      </Centered>
    );
  if (session.isError) {
    if (unauthed) return null;
    return (
      <Centered>
        <ErrorState
          message={apiErrorMessage(session.error)}
          onRetry={() => void session.refetch()}
        />
      </Centered>
    );
  }
  if (!session.data) return null;
  return <>{children(session.data)}</>;
}
