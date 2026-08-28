import { afterEach, describe, expect, test, vi } from "vitest";
import { DEFAULT_BRAND_PROFILE, RUNTIME_PROFILE_ELEMENT_ID } from "./profile";

/**
 * How a page's brand reaches the bundle.
 *
 * `BRAND_PROFILE` is resolved once, when the module initializes, and everything
 * downstream (`BRAND_NAME`, the message catalogues, the mark) is a constant read
 * off it. That is why these tests re-import the module for each case rather than
 * calling a function: the resolution *is* the module's initialization, and a
 * test that called a getter would be testing something the app never runs.
 */
const island = (body: string) => ({
  getElementById(id: string) {
    return id === RUNTIME_PROFILE_ELEMENT_ID ? { textContent: body } : null;
  },
});

async function loadEntry() {
  vi.resetModules();
  return import("./index");
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the profile a page is rendered with", () => {
  test("comes from the island the server filled in", async () => {
    const profile = structuredClone(DEFAULT_BRAND_PROFILE) as Record<string, any>;
    profile.identity.name = "YouModel AI";
    profile.identity.assets.markSvg = "/brand/mark.svg";
    vi.stubGlobal("document", island(JSON.stringify(profile)));

    const brand = await loadEntry();
    expect(brand.BRAND_NAME).toBe("YouModel AI");
    expect(brand.BRAND_MARK_URL).toBe("/brand/mark.svg");
  });

  // Node tests have no document to read, and this fallback is why every package
  // that imports the brand can still be unit-tested without a DOM. It is safe
  // because the *server* is fail-closed: a deployment whose bundle will not load
  // does not start, so a page that exists always carries an island.
  test("falls back to the default outside a browser", async () => {
    const brand = await loadEntry();
    expect(brand.BRAND_NAME).toBe(DEFAULT_BRAND_PROFILE.identity.name);
  });

  // An empty island is a server that rendered the page without filling it in --
  // not a brand with no name. Falling back is the same answer as no island at
  // all, which is the only one that keeps the app rendering.
  test("falls back when the island is empty", async () => {
    vi.stubGlobal("document", island("   "));
    const brand = await loadEntry();
    expect(brand.BRAND_NAME).toBe(DEFAULT_BRAND_PROFILE.identity.name);
  });

  // The build-time define is marketing's path and must keep winning there: a
  // static site is rendered before any island exists, so reading one would hand
  // every page the default brand.
  test("prefers the build-time define when there is one", async () => {
    const profile = structuredClone(DEFAULT_BRAND_PROFILE) as Record<string, any>;
    profile.identity.name = "Built In";
    vi.stubGlobal("__FAIRLB_BRAND_PROFILE__", profile);
    vi.stubGlobal("document", island(JSON.stringify({ identity: { name: "Ignored" } })));

    const brand = await loadEntry();
    expect(brand.BRAND_NAME).toBe("Built In");
  });
});
