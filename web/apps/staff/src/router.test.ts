import { Outlet } from "@tanstack/react-router";
import { Suspense, type ReactElement } from "react";
import { expect, test } from "vitest";
import { RouteFallback } from "@fairlb/app-composition";
import { adminLayoutRoute, createAdminRouter } from "./router";

/**
 * Where the page-level loading state is mounted (the "the whole console looks
 * like it reloaded" defect).
 *
 * Every page in this application is a `lazy()`, so entering any of them
 * suspends. Two things decide what the reader sees while that happens, and both
 * are one line each — which is exactly why they need holding down:
 *
 *   1. A boundary **inside the shell**. The only Suspense boundary used to sit
 *      outside `RouterProvider`; when its fallback showed, the entire
 *      application — sidebar, top bar, everything — was replaced by a
 *      full-screen "Loading…", which on screen is indistinguishable from a full
 *      page reload.
 *   2. `defaultPendingComponent`, which is what makes TanStack wrap **each
 *      match** in its own Suspense. A newly mounted boundary shows its fallback
 *      immediately, so the loading state lands in the content area; without it
 *      a navigation inside a React transition meets only the long-mounted
 *      boundary above and keeps the old screen frozen instead.
 *
 * These are **shape** assertions, not behaviour: this application has no
 * browser-mode test setup, and adding Playwright to the Community tree to hold
 * down two lines is the wrong trade. The behaviour behind the shape is measured
 * in a real browser in Cloud's `route-suspense.browser.tsx`, on the identical
 * shape (same shell contract, same shared packages, same router version), with
 * a red control for each half. What is verified here is that this application
 * still has that shape — which is the half that actually rots.
 *
 * The layout component is **called**, not rendered: it holds no hooks of its
 * own, so calling it yields the element tree directly and no DOM is needed.
 */

test("the authenticated layout wraps its Outlet in a Suspense boundary inside the shell", () => {
  const layout = adminLayoutRoute.options.component as () => ReactElement;
  const shell = layout();

  // The guard is the outermost element: the boundary must be *inside* it, or
  // the fallback takes the sidebar and the top bar down with the page.
  const boundary = (shell.props as { children: ReactElement }).children;
  expect(boundary.type, "壳内那道 Suspense 不见了：fallback 会换掉整个应用").toBe(Suspense);

  const props = boundary.props as { fallback: ReactElement; children: ReactElement };
  expect(props.fallback.type).toBe(RouteFallback);
  expect(props.children.type).toBe(Outlet);
});

test("每个 match 自己一道 Suspense 边界", () => {
  expect(createAdminRouter().options.defaultPendingComponent).toBe(RouteFallback);
});
