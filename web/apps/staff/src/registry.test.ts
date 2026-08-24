import { expect, test } from "vitest";
import {
  ADMIN_SECTIONS,
  adminPages,
  breadcrumbParent,
  navPages,
  resolveAdminPage,
  HOME_PATH,
} from "./registry";

/**
 * The shape of the registry: array order is the only place sidebar order is
 * written down.
 *
 * The assertions are anchored to independent expected values rather than to
 * "these two agree": the sidebar order is spelled out as a literal array
 * instead of being compared against something derived from the registry, which
 * would only prove the two came from the same place.
 */
test("the sidebar shows ten entries in three sections, and detail pages take no slot", () => {
  // Spelled out per section rather than as one flat list, because the grouping
  // is the claim being made: each heading has to name what is under it.
  const inSection = (id: string) => navPages.filter((p) => p.section === id).map((p) => p.path);
  expect(ADMIN_SECTIONS.map((s) => s.id)).toEqual(["gateway", "observe", "workspace"]);
  expect(inSection("gateway")).toEqual([
    "/gateway/health",
    "/gateway/providers",
    "/gateway/models",
    "/gateway/tiers",
    // The gateway's runtime settings (ADR-0198): configure the gateway, so they
    // sit with it rather than in a fourth heading.
    "/settings",
  ]);
  expect(inSection("observe")).toEqual(["/usage", "/requests"]);
  expect(inSection("workspace")).toEqual(["/playground", "/keys", "/teams"]);
  // No entry may fall outside a heading: a page with an unknown section is
  // routed and reachable but invisible in the sidebar, which reads as missing.
  const known = new Set(ADMIN_SECTIONS.map((s) => s.id));
  expect(navPages.filter((p) => !known.has(p.section)).map((p) => p.path)).toEqual([]);
  // Every entry needs an icon: when the sidebar collapses to icons only, an
  // entry without one disappears entirely.
  expect(navPages.every((p) => p.icon !== undefined)).toBe(true);
  // Detail pages are routed but do not take a sidebar slot.
  expect(adminPages.filter((p) => p.hideInNav).map((p) => p.path)).toEqual([
    "/gateway/providers/$providerId",
    "/gateway/models/$modelId",
    "/orgs/$orgId/access",
  ]);
});

test("signing in lands on a mounted, visible page that reports status", () => {
  // The landing page is named in the registry rather than derived from array
  // order. What that gave up is the guarantee that it points somewhere real, so
  // the guarantee is asserted here instead: mounted, and present in the sidebar
  // so the reader can see where they are.
  expect(HOME_PATH).toBe("/gateway/health");
  expect(resolveAdminPage(HOME_PATH)?.path).toBe(HOME_PATH);
  expect(navPages.some((p) => p.path === HOME_PATH)).toBe(true);
});

test("a pathname resolves back to exactly one registry entry", () => {
  expect(resolveAdminPage("/gateway/models")?.path).toBe("/gateway/models");
  expect(resolveAdminPage("/gateway/models/m_123")?.path).toBe("/gateway/models/$modelId");
  expect(resolveAdminPage("/gateway/models/m_123/pricing")?.path).toBe("/gateway/models/$modelId");
  expect(resolveAdminPage("/gateway/models/m_123/routes")?.path).toBe("/gateway/models/$modelId");
  expect(resolveAdminPage("/gateway/providers/p_1/settings")?.path).toBe(
    "/gateway/providers/$providerId",
  );
  expect(resolveAdminPage("/gateway/providers/p_1")?.path).toBe("/gateway/providers/$providerId");
  // The per-team access page is routed even though it takes no sidebar slot;
  // the teams page links to it.
  expect(resolveAdminPage("/orgs/org_1/access")?.path).toBe("/orgs/$orgId/access");
  // An unrecognized path must not be forced onto some entry.
  expect(resolveAdminPage("/login")).toBeUndefined();
  // Pricing plans stay out: this deployment's operator is not charging anyone.
  expect(resolveAdminPage("/gateway/pricing-plans")).toBeUndefined();
});

test("only detail pages have a breadcrumb ancestor, and it is their list page", () => {
  expect(breadcrumbParent("/gateway/models/m_123")?.path).toBe("/gateway/models");
  expect(breadcrumbParent("/gateway/models/m_123/pricing")?.path).toBe("/gateway/models");
  expect(breadcrumbParent("/gateway/providers/p_1")?.path).toBe("/gateway/providers");
  // The per-team access page is the one detail page whose ancestor cannot be
  // read off its path: it is mounted under the object's own noun while the
  // entry that leads to it is Teams. Before `navParent` it had no ancestor at
  // all — and, for the same reason, no lit sidebar row — so this page was the
  // only one in the app that could not say where it was.
  expect(breadcrumbParent("/orgs/org_1/access")?.path).toBe("/teams");
  // A page with its own sidebar entry already answers "where am I" by being
  // highlighted; a breadcrumb there would repeat it.
  expect(breadcrumbParent("/gateway/models")).toBeUndefined();
  expect(breadcrumbParent("/gateway/health")).toBeUndefined();
  // An unrecognized path has no ancestor.
  expect(breadcrumbParent("/login")).toBeUndefined();
});
