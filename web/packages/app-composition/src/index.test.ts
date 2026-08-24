import { ApiError } from "@fairlb/api-client";
import { expect, test } from "vitest";
import {
  createAdminApp,
  createConsoleApp,
  findAdminBreadcrumbParent,
  isAdminNavActive,
  isAdminRecordRoute,
  isPathUnder,
  resolveAdminRoute,
  resolveConsoleDestination,
  sessionIsUnauthenticated,
  type AdminModule,
  type AdminRoute,
  type ConsoleModule,
} from "./index";

const View = () => null;

test("admin composition derives routes, navigation metadata, panels and permissions together", () => {
  const identity: AdminModule = {
    id: "identity",
    routes: [{ path: "/users", navKey: "navOrgs", section: "operations", component: View }],
    sections: [{ id: "operations", labelKey: "usageTitle", order: 20 }],
    permissions: ["users:read"],
  };
  const gateway: AdminModule = {
    id: "gateway",
    routes: [
      {
        path: "/gateway/models",
        navKey: "navGatewayModels",
        section: "gateway",
        component: View,
      },
    ],
    sections: [{ id: "gateway", labelKey: "navSectionGateway", order: 10 }],
    dashboardPanels: [{ id: "gateway-health", order: 30, component: View }],
    permissions: ["models:read", "users:read"],
  };

  const app = createAdminApp([identity, gateway]);
  expect(app.modules.map((module) => module.id)).toEqual(["identity", "gateway"]);
  expect(app.routes.map((route) => route.path)).toEqual(["/users", "/gateway/models"]);
  expect(app.sections.map((section) => section.id)).toEqual(["gateway", "operations"]);
  expect(app.dashboardPanels.map((panel) => panel.id)).toEqual(["gateway-health"]);
  expect(app.permissions).toEqual(["users:read", "models:read"]);
});

test("admin route matching and breadcrumb ancestry share parameter semantics", () => {
  const routes: AdminModule["routes"] = [
    { path: "/orgs", navKey: "navOrgs", section: "ops", component: View },
    {
      path: "/orgs/$orgId",
      navKey: "navOrgs",
      section: "ops",
      component: View,
      hideInNav: true,
    },
    {
      path: "/orgs/$orgId/billing",
      navKey: "navOrgs",
      section: "ops",
      component: View,
      hideInNav: true,
    },
  ];
  expect(resolveAdminRoute(routes, "/orgs/org_1/billing")?.path).toBe("/orgs/$orgId/billing");
  expect(findAdminBreadcrumbParent(routes, "/orgs/org_1/billing")?.path).toBe("/orgs");
  expect(isAdminRecordRoute(routes[1]!)).toBe(true);
});

test("nested admin task paths resolve to their persistent resource descriptor", () => {
  const routes: AdminModule["routes"] = [
    { path: "/gateway/models", navKey: "navGatewayModels", section: "gateway", component: View },
    {
      path: "/gateway/models/$modelId",
      navKey: "navGatewayModels",
      section: "gateway",
      component: View,
      hideInNav: true,
      children: [
        { path: "/", component: View },
        { path: "routes", component: View },
        { path: "pricing", component: View },
      ],
    },
  ];
  expect(resolveAdminRoute(routes, "/gateway/models/model_1")?.path).toBe(
    "/gateway/models/$modelId",
  );
  expect(resolveAdminRoute(routes, "/gateway/models/model_1/routes")?.path).toBe(
    "/gateway/models/$modelId",
  );
  expect(resolveAdminRoute(routes, "/gateway/models/model_1/pricing")?.path).toBe(
    "/gateway/models/$modelId",
  );
  expect(resolveAdminRoute(routes, "/gateway/models/model_1/unknown")).toBeUndefined();
  expect(findAdminBreadcrumbParent(routes, "/gateway/models/model_1/pricing")?.path).toBe(
    "/gateway/models",
  );
});

test("console composition sorts destinations and resolves the most specific deep link", () => {
  const cloud: ConsoleModule = {
    id: "cloud",
    destinations: [
      { id: "overview", path: "", scope: "organization", labelKey: "usageTitle", order: 10 },
      {
        id: "settings",
        path: "settings",
        scope: "organization",
        labelKey: "apiKeys",
        order: 90,
      },
    ],
    permissions: ["org:read"],
  };
  const gateway: ConsoleModule = {
    id: "gateway",
    destinations: [
      {
        id: "models",
        path: "settings/models",
        scope: "organization",
        labelKey: "modelsTitle",
        order: 50,
      },
    ],
    routes: [{ path: "/orgs/$orgId/settings/models", component: View }],
    permissions: ["gateway:read"],
  };

  const app = createConsoleApp([cloud, gateway]);
  expect(app.destinations.map((destination) => destination.id)).toEqual([
    "overview",
    "models",
    "settings",
  ]);
  expect(resolveConsoleDestination(app.destinations, "settings/models/m_1")?.id).toBe("models");
  expect(resolveConsoleDestination(app.destinations, "settings/billing")?.id).toBe("settings");
  expect(app.routes.map((route) => route.path)).toEqual(["/orgs/$orgId/settings/models"]);
  expect(app.permissions).toEqual(["org:read", "gateway:read"]);
});

test("path matching respects segment boundaries", () => {
  expect(isPathUnder("/keys", "/keys")).toBe(true);
  expect(isPathUnder("/keys/k_1", "/keys")).toBe(true);
  // The case a bare `startsWith` gets wrong. It is not hypothetical for a
  // registry that grows: a sibling only has to share a prefix.
  expect(isPathUnder("/keys-extra", "/keys")).toBe(false);
  // The root would otherwise be under every path.
  expect(isPathUnder("/anything", "/")).toBe(false);
  expect(isPathUnder("/", "/")).toBe(true);
  // `exact` is for a destination that owns only itself.
  expect(isPathUnder("/settings/members", "/settings", true)).toBe(false);
  expect(isPathUnder("/settings", "/settings", true)).toBe(true);
});

test("a page mounted under another noun lights its declared nav parent", () => {
  const teams: AdminRoute = {
    path: "/teams",
    navKey: "navTeams",
    section: "workspace",
    component: View,
  };
  // Mounted under the object's own noun; the entry that leads to it is Teams.
  const access: AdminRoute = {
    path: "/orgs/$orgId/access",
    navKey: "navTeams",
    section: "workspace",
    component: View,
    hideInNav: true,
    navParent: "/teams",
  };
  const routes = [teams, access];

  expect(isAdminNavActive(routes, teams, "/orgs/org_1/access")).toBe(true);
  // Without the declaration the sidebar has nothing to light: this is the state
  // the community admin's one detail page was in.
  const orphan = { ...access, navParent: undefined };
  expect(isAdminNavActive([teams, orphan], teams, "/orgs/org_1/access")).toBe(false);
  // The breadcrumb reads the same declaration, so the two devices cannot
  // disagree about which heading a page belongs under.
  expect(findAdminBreadcrumbParent(routes, "/orgs/org_1/access")?.path).toBe("/teams");
  expect(findAdminBreadcrumbParent([teams, orphan], "/orgs/org_1/access")).toBeUndefined();
});

/**
 * The one rule the three shells' guards disagreed on.
 *
 * Two of them treated *any* failed session read as a logout, so a 500 or a
 * dropped connection sent the reader to the login page — where they would sign
 * in successfully, land back, and never learn what actually failed. This pins
 * the surviving rule: only a 401 is a logout.
 */
test("only a 401 means the session is unauthenticated", () => {
  const idle = { isPending: false, isError: false, error: null };
  expect(sessionIsUnauthenticated(idle)).toBe(false);
  expect(sessionIsUnauthenticated({ ...idle, isPending: true, isError: true })).toBe(false);

  const failed = (error: unknown) => ({ isPending: false, isError: true, error });
  expect(sessionIsUnauthenticated(failed(new ApiError(401)))).toBe(true);
  expect(sessionIsUnauthenticated(failed(new ApiError(500)))).toBe(false);
  expect(sessionIsUnauthenticated(failed(new ApiError(502)))).toBe(false);
  // A 403 is "signed in, not allowed here" — sending that reader to /login
  // offers the one action that cannot help them.
  expect(sessionIsUnauthenticated(failed(new ApiError(403)))).toBe(false);
  // Not every failure is an HTTP answer: a dropped connection or a body that
  // will not parse arrives as a plain Error, and it is not a logout either.
  expect(sessionIsUnauthenticated(failed(new TypeError("Failed to fetch")))).toBe(false);
});
