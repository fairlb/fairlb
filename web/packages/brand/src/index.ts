import { DEFAULT_BRAND_PROFILE, RUNTIME_PROFILE_ELEMENT_ID, type BrandProfileV1 } from "./profile";

declare const __FAIRLB_BRAND_PROFILE__: BrandProfileV1 | undefined;

/**
 * The profile the server put in the page.
 *
 * Module scripts are deferred, so the document is fully parsed by the time this
 * module initializes and the island is always readable here. Returning
 * `undefined` therefore means "nobody put one there", not "not yet".
 */
function runtimeProfile(): BrandProfileV1 | undefined {
  if (typeof document === "undefined") return undefined;
  const island = document.getElementById(RUNTIME_PROFILE_ELEMENT_ID);
  const body = island?.textContent?.trim();
  if (!body) return undefined;
  return JSON.parse(body) as BrandProfileV1;
}

/**
 * Three sources, in the order they are authoritative.
 *
 * 1. `__FAIRLB_BRAND_PROFILE__` — the build-time define. **Only marketing sets
 *    it.** A static site has no runtime: its brand has to be resolved while the
 *    pages are being rendered, so the profile is a build input there and stays
 *    one (ADR-0214).
 * 2. The JSON island — how console, operations and the Community admin get
 *    theirs. One image serves every brand; the server fills the island from
 *    whatever profile bundle is mounted, so nothing brand-specific is compiled
 *    in.
 * 3. `DEFAULT_BRAND_PROFILE` — node tests, which have no document to read.
 *
 * **The fallback cannot mask a misconfigured deployment.** The server refuses to
 * start when `BRAND_PROFILE_DIR` names a bundle it cannot load, so a browser
 * that gets a page at all gets one with the island filled in. That is where
 * fail-closed belongs: at the boundary the configuration crosses, not in a
 * bundle that also has to run under vitest.
 */
export const BRAND_PROFILE: BrandProfileV1 =
  typeof __FAIRLB_BRAND_PROFILE__ !== "undefined"
    ? __FAIRLB_BRAND_PROFILE__
    : (runtimeProfile() ?? DEFAULT_BRAND_PROFILE);
export const BRAND_NAME = BRAND_PROFILE.identity.name;
export const BRAND_MARK_URL = BRAND_PROFILE.identity.assets.markSvg;

export const BRAND_COLORS = {
  canvas: { light: BRAND_PROFILE.theme.light.canvas, dark: BRAND_PROFILE.theme.dark.canvas },
  surface: { light: BRAND_PROFILE.theme.light.surface, dark: BRAND_PROFILE.theme.dark.surface },
  ink: { light: BRAND_PROFILE.theme.light.ink, dark: BRAND_PROFILE.theme.dark.ink },
  route: { light: BRAND_PROFILE.theme.light.accent, dark: BRAND_PROFILE.theme.dark.accent },
  healthy: { light: BRAND_PROFILE.theme.light.healthy, dark: BRAND_PROFILE.theme.dark.healthy },
  degraded: { light: BRAND_PROFILE.theme.light.degraded, dark: BRAND_PROFILE.theme.dark.degraded },
} as const;

export { interpolateBrandText } from "./profile";

export type {
  BrandColors,
  BrandLocale,
  BrandProfileV1,
  BrandSurface,
  LocalFont,
  LocalFontSource,
  LocalizedLink,
  LocalizedText,
} from "./profile";

export type BrandEdition = "cloud" | "community";

/**
 * Background colours for the identity avatar, picked deterministically per
 * account. White is the only foreground; there is no per-theme variant.
 *
 * Every entry satisfies three contrast constraints at once, which is why the
 * numbers look arbitrary — they were solved for, not chosen:
 *
 * - >= 4.5:1 against white, so the initials inside the circle meet AA for text.
 * - >= 4.5:1 against the light surface and >= 3.0:1 against the dark one, so the
 *   circle is separable from the page in both themes and one table can serve
 *   both. Tuning per theme would double the table and double what has to be
 *   re-measured whenever a colour changes.
 *
 * The band those constraints leave is narrow (relative luminance ~0.13–0.18),
 * so do not adjust a value by eye: contrast against white and contrast against
 * the dark surface pull in opposite directions, and a colour that looks right on
 * the theme you are looking at is exactly how the other one breaks. All three
 * constraints are asserted per entry in `ui/src/avatar-color.test.ts`, so an
 * edit that leaves the band fails there rather than shipping.
 */
/**
 * The one foreground the palette was solved against. It is exported rather than
 * written as a class because `text-white` would make the pair separable: the
 * colours below are only readable under *this* value, and a theme that drops
 * Tailwind's default palette would silently take the class with it, leaving dark
 * initials on a dark circle. Nothing in the product renders that combination
 * loudly enough for anyone to notice.
 */
export const AVATAR_INK = "#FFFFFF";

export const AVATAR_COLORS = [
  "#CD382D", // red
  "#A25F25", // orange
  "#846D2A", // amber
  "#367D4D", // green
  "#187C79", // teal
  "#377790", // cyan
  "#2B65EE", // blue
  "#715ECF", // indigo
  "#954DCB", // violet
  "#AC4D8D", // magenta
] as const;
