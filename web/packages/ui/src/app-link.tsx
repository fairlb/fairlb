import { type LinkComponentProps } from "@cloudflare/kumo/utils";
import { Link, useLinkProps, useRouter } from "@tanstack/react-router";
import { forwardRef, type ForwardedRef } from "react";

/**
 * Kumo links use the SPA router for app pages, while downloads and external
 * destinations keep the browser's native document-navigation semantics.
 *
 * It lives in this shared package because every shell needs the very same seam:
 * they render the same `Sidebar`, the same `PageHeader` breadcrumbs and local
 * navigation. Link-based navigation can only override inferred current state
 * through the `data-navigation-current` protocol read below. Copying it would
 * mean two implementations of three ARIA invariants,
 * with no test in either application able to catch them drifting apart.
 * Applications import it directly from this package.
 *
 * Both `href` and `to` must be read: Kumo 2.8.0 is internally inconsistent —
 * Breadcrumbs.Link passes only the (type-deprecated) `to`, while
 * Sidebar/Dropdown pass both. Kumo's own default link component falls back
 * `href ?? to`; reading only `href` collapses every breadcrumb to "/".
 *
 * ## Why `activeOptions` is set here rather than per call site
 *
 * TanStack marks a link active by **path prefix** by default, and an active
 * `<Link>` silently gains `aria-current="page"`. Every ancestor link therefore
 * claimed to be the current page: standing on `/orgs/org_abc`, the crumb
 * pointing at `/orgs` announced itself as the current page alongside the
 * real current segment — two "you are here" answers in one breadcrumb trail,
 * one of them a link to somewhere else.
 *
 * `exact` is what fixes that. `includeSearch: false` is what keeps the fix from
 * overshooting: `exact` otherwise tightens the **search** comparison too
 * (`partial: !exact`), so `/orgs?status=suspended` would stop counting as
 * `/orgs` and the sidebar entry for the page you are literally on would lose
 * its `aria-current`. Filters do not change which page you are on. Both halves
 * are measured in `app-link.browser.tsx`.
 *
 * Set here, not at the call sites, because the call sites are Kumo's
 * (Breadcrumbs/Sidebar/Dropdown) — this component is the only seam we own.
 *
 * ## Why `aria-current="true"` is derived here too
 *
 * `exact` above is what stops an ancestor from claiming `aria-current="page"`.
 * It leaves the other half of "where am I" unanswered: standing on
 * `/orgs/org_abc`, the sidebar's Organizations row **is** lit — Kumo's `Sidebar`
 * turns its `active` prop into `data-active` plus a background colour, and
 * nothing else. That cue exists only in the visual channel, so a screen-reader
 * user is told nothing at all (WCAG 1.3.1). ARIA's word for it is
 * `aria-current="true"` — "the current item", as distinct from `"page"`, "the
 * current page"; screen readers read the two differently.
 *
 * Kumo cannot be asked to emit it: `MenuButton`/`MenuSubButton` hand-pick props
 * in their **link** branch (`onClick` only) and rest-spread solely in their
 * button branch, so anything extra passed at the call site type-checks and then
 * never reaches the DOM. This is Kumo's habitual hand-picked-prop form, not an
 * oversight, so a version bump does not fix it.
 *
 * `data-active` **does** reach us, though, and that is exactly the signal:
 * "the design system marked this row as the active one". Translating it here
 * costs no call-site changes and cannot drift out of sync with the highlight,
 * because it is derived from the very attribute that draws the highlight.
 * Rebuilding the row from Base UI primitives would
 * work too, at the price of copying Kumo's classes and owning the style drift —
 * not worth it when the seam we already own carries the signal.
 *
 * **The `active && not-this-page-itself` formula falls out for free**: we always
 * pass `"true"`, and TanStack's `STATIC_ACTIVE_PROPS` overrides it with
 * `"page"` last in its prop merge when the link resolves to the current page.
 * So the exact page says `"page"`, its ancestors say `"true"`, and this holds
 * for both highlight criteria in use (admin's prefix `isActive`, console's mix
 * of prefix and exact) without either app restating the rule.
 *
 * Only `Sidebar` emits `data-active` through `LinkProvider` (Breadcrumbs and
 * Dropdown do not — checked against the dist), so nothing else is affected.
 */
type AppLinkProps = LinkComponentProps & {
  /** Kumo `Sidebar` marks the active row with this; it produces no ARIA of its own. */
  "data-active"?: boolean | "true";
  /** A navigation component may own current-page state instead of path inference. */
  "data-navigation-current"?: "page" | "false";
};

type ControlledNavigationLinkProps = Omit<LinkComponentProps, "href" | "to"> & {
  target: string;
  current: boolean;
};
type RouterAnchorProps = ReturnType<typeof useLinkProps> & { "data-status"?: string };

function parseNavigationTarget(target: string, parseSearch: (search: string) => unknown) {
  const url = new URL(target, "https://fairlb.invalid");
  return {
    pathname: url.pathname,
    search: parseSearch(url.search),
    hash: decodeURIComponent(url.hash.slice(1)),
  };
}

/**
 * TanStack appends its inferred `aria-current` after caller props. Controlled
 * navigation puts its answer after the router-computed anchor props.
 */
const ControlledNavigationLink = forwardRef<HTMLAnchorElement, ControlledNavigationLinkProps>(
  function ControlledNavigationLink({ target, current, children, ...rest }, ref) {
    const router = useRouter();
    const destination = parseNavigationTarget(target, router.options.parseSearch);
    const routerProps = useLinkProps(
      {
        ...rest,
        to: destination.pathname,
        search: destination.search,
        hash: destination.hash,
        activeOptions: { exact: true, includeSearch: false },
        activeProps: {},
        inactiveProps: {},
      },
      ref as ForwardedRef<Element>,
    ) as RouterAnchorProps;
    const {
      "aria-current": _routerCurrent,
      "data-status": _routerStatus,
      ...linkProps
    } = routerProps;
    return (
      <a {...linkProps} aria-current={current ? "page" : undefined}>
        {children}
      </a>
    );
  },
);

export const AppLink = forwardRef<HTMLAnchorElement, AppLinkProps>(function AppLink(
  { href, to, "data-active": dataActive, "data-navigation-current": navigationCurrent, ...rest },
  ref,
) {
  const target = href ?? to ?? "/";
  const documentNavigation =
    target.startsWith("/api/") ||
    target.startsWith("http://") ||
    target.startsWith("https://") ||
    target.startsWith("mailto:") ||
    target.startsWith("tel:");
  // Re-emitted below rather than left in `rest`: it is Kumo's styling hook and
  // the browser fixtures select on it.
  const current = dataActive ? "true" : undefined;

  if (navigationCurrent !== undefined && !documentNavigation) {
    return (
      <ControlledNavigationLink
        ref={ref}
        target={target}
        current={navigationCurrent === "page"}
        {...rest}
      />
    );
  }

  // `aria-current` goes after `rest` in **both** branches, so the rule reads the
  // same way whichever one runs. TanStack still upgrades it to "page" on an exact
  // match — its own merge puts STATIC_ACTIVE_PROPS last, after everything here.
  if (documentNavigation)
    return <a ref={ref} href={target} data-active={dataActive} {...rest} aria-current={current} />;
  return (
    <Link
      ref={ref}
      to={target}
      activeOptions={{ exact: true, includeSearch: false }}
      data-active={dataActive}
      {...rest}
      aria-current={current}
    />
  );
});
