import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  type AnyRoute,
} from "@tanstack/react-router";
import { lazy, Suspense } from "react";
import { APP_ROUTER_DEFAULTS, buildPageRoute, RouteFallback } from "@fairlb/app-composition";
import { LoginPage } from "./login";
import { SetupPage } from "./setup";
import { adminPages, HOME_PATH } from "./registry";

/**
 * The route tree and the router options live here rather than in `main.tsx`,
 * because importing `main.tsx` renders the whole application through
 * `createRoot(...)` — a test cannot reach it. What a test needs to reach is
 * where the two loading states below are mounted, and re-assembling a router in
 * the test the way `main.tsx` does would only assert against that copy.
 */

const RequireAdmin = lazy(() =>
  import("./lib").then((module) => ({ default: module.RequireAdmin })),
);

const rootRoute = createRootRoute({ component: Outlet });

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
});

// The first-run wizard sits next to /login at the root, not under the
// authenticated layout: the whole premise of this route is that no account
// exists yet, and putting it behind the guard would require signing in before
// the first sign-in identity can be created.
const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  component: SetupPage,
});

/**
 * The authenticated layout route. The shell mounts here, so navigating between
 * pages only swaps the Outlet.
 *
 * Exported for `router.test.ts`, which reads the element tree this component
 * returns. Everything else goes through `createAdminRouter`.
 */
export const adminLayoutRoute: AnyRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "admin",
  component: () => (
    <RequireAdmin>
      {/* This boundary has to be inside the shell. The only Suspense boundary
          used to sit outside `RouterProvider`, so a page suspending — every page
          here is a `lazy()` — replaced the whole application, sidebar and top bar
          included, with a full-screen "Loading…": indistinguishable from a full
          page reload. Cloud's `route-suspense.browser.tsx` measures that in a
          real browser on the identical shape. */}
      <Suspense fallback={<RouteFallback />}>
        <Outlet />
      </Suspense>
    </RequireAdmin>
  ),
});

/**
 * `/` redirects to the first sidebar entry.
 *
 * There is no dashboard here. A dashboard would summarize numbers this
 * deployment does not have, and a landing page holding nothing but the same
 * links as the sidebar is worse than landing on the first one directly.
 *
 * The route still has to be registered: the brand link in the shell, a
 * bookmark, and typing the host name after deploying all hit `/`. Without it,
 * a 404 greets everyone on their first visit.
 *
 * The redirect happens in `beforeLoad` rather than in a component, because a
 * component-level redirect renders one frame before correcting itself, and that
 * frame is empty.
 */
const rootRedirectRoute = createRoute({
  getParentRoute: () => adminLayoutRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: HOME_PATH });
  },
  component: () => null,
});

const pageRoutes = adminPages.map((page) => buildPageRoute(adminLayoutRoute, page));

/** Build the application router. This is the only assembly path. */
export function createAdminRouter() {
  return createRouter({
    routeTree: rootRoute.addChildren([
      loginRoute,
      setupRoute,
      adminLayoutRoute.addChildren([rootRedirectRoute, ...pageRoutes]),
    ]),
    ...APP_ROUTER_DEFAULTS,
  });
}
