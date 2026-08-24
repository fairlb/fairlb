import type { LocalNavItem } from "./local-nav";
import type { RecordNavItem } from "./record-nav";

/**
 * Whether `pathname` falls on `path`, matching whole segments only.
 *
 * The one rule three shells share. A bare `startsWith` also matches a sibling
 * whose name merely begins with this one — `/keys-extra` lighting up `/keys` —
 * and each shell used to answer that differently: the console compared segments
 * while both admin shells used the bare prefix. A rule stated three times is a
 * rule that drifts, and the drift is silent because the wrong answer is only
 * ever "the wrong row is highlighted".
 *
 * `exact` is for a destination that owns only itself, such as an overview whose
 * children are separate destinations with their own permission boundary.
 *
 * It lives here, below the composition layer, because `resolveNavValue` needs
 * exactly this rule and the navigation components are further down the package
 * graph than the route registry is. `@fairlb/app-composition` re-exports it, so
 * there is still one implementation.
 */
export function isPathUnder(pathname: string, path: string, exact = false): boolean {
  if (path === "") return pathname === "" || pathname === "/";
  if (path === "/") return pathname === "/";
  if (exact) return pathname === path;
  return pathname === path || pathname.startsWith(`${path}/`);
}

/**
 * Which navigation item the current URL is on.
 *
 * Every one of the eight navigations in the two consoles used to answer this
 * with a hand-written chain — `pathname.endsWith("/models") ? "models" : …` —
 * and the chains shared two failure modes. `endsWith` ignores segment
 * boundaries, so `/billing/settings-x` would light up `/billing/settings`; and
 * the chain's fallback is its first item, so **any** route nested one level
 * deeper than the items themselves threw the highlight back to the overview.
 * The sidebars were moved off the same mistake earlier; these were not named at
 * the time.
 *
 * The rule is longest-match on the item's own `href`, which is the same
 * reduction the destination registry uses to resolve a path to a destination.
 * Longest rather than first because an overview's href is a prefix of all of its
 * siblings'.
 *
 * With nothing matching it returns the first item's value rather than empty: the
 * parent layout is mounted, so one of its aspects is on screen, and marking none
 * of them current would be a worse answer than marking the default one.
 */
export function resolveNavValue(
  items: readonly (Pick<RecordNavItem, "value" | "href"> | Pick<LocalNavItem, "value" | "href">)[],
  pathname: string,
): string {
  const current = pathname.split(/[?#]/)[0]!;
  let best: { value: string; href: string } | undefined;
  for (const item of items) {
    // A trailing slash on an item href would make every comparison off by one
    // segment; the registries do not write them, but the value also arrives from
    // template literals.
    const href = item.href.split(/[?#]/)[0]!.replace(/(.)\/$/, "$1");
    if (!isPathUnder(current, href)) continue;
    if (!best || href.length > best.href.length) best = { value: item.value, href };
  }
  return best?.value ?? items[0]?.value ?? "";
}
