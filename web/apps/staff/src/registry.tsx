import {
  createAdminApp,
  findAdminBreadcrumbParent,
  resolveAdminRoute,
  type AdminModule,
  type AdminRoute,
  type AdminSection,
} from "@fairlb/app-composition";
import {
  ChartLineIcon,
  CubeIcon,
  KeyIcon,
  HeartbeatIcon,
  ListMagnifyingGlassIcon,
  GearIcon,
  PlugsIcon,
  ShieldCheckIcon,
  UsersThreeIcon,
} from "@phosphor-icons/react";
// The filter keys come from a leaf subpath, a module that imports nothing else.
// Importing them from the package root would pull the whole feature package
// into the entry chunk just to name a few strings.
import {
  REQUEST_SEARCH_KEYS,
  USAGE_SEARCH_KEYS,
} from "@fairlb/gateway-console-features/search-keys";
import {
  gatewayHealthPage,
  gatewayModelDetailPage,
  gatewayModelsPage,
  gatewayProviderDetailPage,
  gatewayProvidersPage,
  gatewayTiersPage,
} from "@fairlb/gateway-staff-features/admin-routes";
import { pickStrings } from "@fairlb/ui";
import { lazy } from "react";

const CommunityKeysPage = lazy(() =>
  import("./keys").then((module) => ({ default: module.CommunityKeysPage })),
);
const CommunitySettingsPage = lazy(() =>
  import("./settings").then((module) => ({ default: module.CommunitySettingsPage })),
);
const CommunityTeamsPage = lazy(() =>
  import("./teams").then((module) => ({ default: module.CommunityTeamsPage })),
);
const GatewayProvider = lazy(() =>
  import("./gateway-provider").then((module) => ({ default: module.GatewayProvider })),
);
const GatewayKillSwitchBanner = lazy(() =>
  import("@fairlb/gateway-staff-features/kill-switch").then((module) => ({
    default: module.GatewayKillSwitchBanner,
  })),
);

// The limits half of the organization access page. The pricing half is not mounted here for
// the same reason the pricing-plan pages are not: it assigns one of the plans
// this deployment has no screen to create, and its operator charges nobody.
// Mounted through the shell's own wrapper, which supplies the page header the
// shared component deliberately does not carry: in Cloud the same content sits
// under a record layout that already renders one.
const CommunityTeamAccessPage = lazy(() =>
  import("./team-access").then((module) => ({
    default: module.CommunityTeamAccessPage,
  })),
);
const UsagePage = lazy(() =>
  import("@fairlb/gateway-console-features/usage").then((module) => ({
    default: module.UsagePage,
  })),
);
const RequestsPage = lazy(() =>
  import("@fairlb/gateway-console-features/requests").then((module) => ({
    default: module.RequestsPage,
  })),
);

/**
 * The page registry for this admin app.
 *
 * # This table belongs to the shell, not to the feature packages
 *
 * What is shared between apps is the pages themselves; the registry is the half
 * that legitimately differs. A shell with dozens of destinations needs sections,
 * collapsible groups, a command-palette source and entity tabs. This shell has
 * none of those subjects, and copying the mechanisms over would produce four
 * structures that are permanently empty or single-element — and an empty
 * mechanism is harder to reason about than an absent one.
 *
 * # Why the paths keep their `/gateway/` prefix
 *
 * Not for symmetry: the feature packages hard-code these paths in their own
 * internal links. Mounting them under different paths would leave every
 * in-package link pointing at a route that does not exist — and an unmatched
 * route renders a 404 page rather than failing the build, so nothing would
 * report it. `mounted-destinations.test.ts` pins that constraint.
 */
export type AdminPage = AdminRoute;
/**
 * A registered page names its own section. It used to omit `section` and have
 * `"gateway"` stamped onto every entry, which is how nine entries ended up under
 * one heading that named only four of them.
 */
type RegisteredAdminPage = AdminRoute;

/**
 * Which pages this app mounts. The criterion is whether the page has a subject
 * in a deployment the operator runs for themselves.
 *
 * Mounted: providers (with their upstream credentials and routes), the model
 * catalog (with its prices and routes), gateway health — and, since this
 * deployment can have more than one team, access tiers and the per-team access
 * page they are applied on.
 *
 * The tiers page was previously left out on the grounds that its subject is *a
 * set of customers* and there was only ever one. That was true while one
 * organization was all the schema allowed; it is not true now. "These people may
 * use only this model" is the question a tier answers, and with teams it is a
 * question this deployment can actually ask.
 *
 * Still not mounted: pricing plans and per-customer pricing assignment. Those
 * decide what a customer is *charged*, and an operator running this for
 * themselves is not charging anyone — the amounts would be moved from one of
 * their own pockets to the other.
 */
const registeredPages: RegisteredAdminPage[] = [
  {
    path: "/gateway/health",
    navKey: "navGatewayHealth",
    section: "gateway",
    icon: HeartbeatIcon,
    ...gatewayHealthPage,
  },
  {
    path: "/gateway/providers",
    navKey: "navGatewayProviders",
    section: "gateway",
    icon: PlugsIcon,
    ...gatewayProvidersPage,
  },
  {
    path: "/gateway/providers/$providerId",
    navKey: "navGatewayProviders",
    section: "gateway",
    ...gatewayProviderDetailPage,
  },
  {
    path: "/gateway/models",
    navKey: "navGatewayModels",
    section: "gateway",
    icon: CubeIcon,
    ...gatewayModelsPage,
  },
  {
    path: "/gateway/models/$modelId",
    navKey: "navGatewayModels",
    section: "gateway",
    ...gatewayModelDetailPage,
  },
  {
    path: "/gateway/tiers",
    navKey: "navGatewayTiers",
    section: "gateway",
    icon: ShieldCheckIcon,
    ...gatewayTiersPage,
  },
  {
    // The gateway's runtime settings — exchange rate, BYOK fee, anomaly
    // thresholds, affinity TTL (ADR-0198). They configure the gateway, so they
    // sit with the gateway pages rather than in a section of their own.
    path: "/settings",
    navKey: "navGatewaySettings",
    section: "gateway",
    icon: GearIcon,
    component: CommunitySettingsPage,
  },
  // The per-team access page. Its path keeps the `/orgs/` prefix the feature
  // package's own links use: mounting it elsewhere would leave those links
  // pointing at a route that does not exist, and an unmatched route renders a
  // 404 page rather than failing the build, so nothing would report it.
  {
    path: "/orgs/$orgId/access",
    navKey: "navTeams",
    section: "workspace",
    component: CommunityTeamAccessPage,
    hideInNav: true,
    // 它挂在 org 名词下，而侧栏里带人来的那一项叫 Teams。没有这一行，
    // 侧栏没有任何项高亮、面包屑也上溯不到父级——这是本 app 唯一的详情页。
    navParent: "/teams",
  },
  // ── Observability ───────────────────────────────────────────────────────
  //
  // These three come from the other feature package, the one written for the
  // surface an organization sees about itself. They are mounted here because
  // when you run the gateway you are also the one calling it: these pages
  // answer "what happened to my requests", while the pages above answer "how is
  // the gateway configured".
  //
  // The paths carry no organization segment, because this UI never names one.
  // A page that cannot read an organization id from its route parameters gets
  // an empty string, and the host implementation answers with the single
  // organization — the collapse of that dimension happens in the host, not in
  // the page.
  //
  // The rest of that package is deliberately left out. Its model page and the
  // catalog page above would be two views of the same models with one
  // organization, and two sidebar entries called "models" are simply
  // misleading. Its provider-key settings page has the same relationship with
  // the provider page's upstream credentials — two write paths to the same
  // thing, with no way to tell which one is in effect. And its dashboard's
  // subject is the organization itself.
  {
    path: "/usage",
    navKey: "usageTitle",
    section: "observe",
    component: UsagePage,
    icon: ChartLineIcon,
    validateSearch: (search) => pickStrings(search, USAGE_SEARCH_KEYS),
  },
  {
    path: "/requests",
    navKey: "logsTitle",
    section: "observe",
    component: RequestsPage,
    icon: ListMagnifyingGlassIcon,
    validateSearch: (search) => pickStrings(search, REQUEST_SEARCH_KEYS),
  },
  // The keys page is this app's own rather than a feature package's: managing
  // keys from the admin plane is a different page from managing them as an
  // organization member. What is reused are the shared UI components; what is
  // written here is which endpoint to call and what a row shows.
  {
    path: "/keys",
    navKey: "apiKeys",
    section: "workspace",
    component: CommunityKeysPage,
    icon: KeyIcon,
  },
  // Teams are this app's own page for the same reason keys are: the hosted
  // product's equivalent is a customer record with billing, membership and a
  // deletion window, and none of those exist here.
  {
    path: "/teams",
    navKey: "navTeams",
    section: "workspace",
    component: CommunityTeamsPage,
    icon: UsersThreeIcon,
  },
];

const toRoutes = (pages: readonly RegisteredAdminPage[]): AdminRoute[] => [...pages];

/**
 * The sidebar's headings, in order.
 *
 * Three plain groups rather than one, and deliberately **not** collapsible: the
 * registry's own note that this app is too small for collapsible groups is
 * right, and it stayed right — a collapsible group costs a click and buys back
 * vertical space this sidebar does not need. But that argument was applied to
 * the *labels* too, and the labels are free. The result was every entry under
 * "Gateway", including Requests, Usage, API keys and Teams — which have nothing
 * to do with configuring the gateway.
 *
 * The three answer three different questions: how is the gateway set up, what
 * has it been doing, and what do I use it with.
 */
export const ADMIN_SECTIONS: readonly AdminSection[] = [
  { id: "gateway", labelKey: "navSectionGateway", order: 10 },
  { id: "observe", labelKey: "navSectionObserve", order: 20 },
  { id: "workspace", labelKey: "navSectionWorkspace", order: 30 },
];

/** The pages this deployment owns rather than borrowing from a feature package. */
const COMMUNITY_PATHS = ["/keys", "/teams", "/settings"];

const communityModule: AdminModule = {
  id: "community",
  permissions: ["community.identity", "community.keys.write"],
  routes: toRoutes(registeredPages.filter((page) => COMMUNITY_PATHS.includes(page.path))),
};

const gatewayModule: AdminModule = {
  id: "gateway",
  permissions: ["gateway.read", "gateway.write"],
  providers: [GatewayProvider],
  banners: [GatewayKillSwitchBanner],
  routes: toRoutes(registeredPages.filter((page) => !COMMUNITY_PATHS.includes(page.path))),
};

/** Community is an explicit identity + gateway composition, not a stripped Cloud shell. */
const adminModules: readonly AdminModule[] = [gatewayModule, communityModule];
export const adminApp = createAdminApp(adminModules);
export const adminPages: readonly AdminPage[] = adminApp.routes;

/** Sidebar entries: the registry minus detail pages. Array order is sidebar
 * order — there is no second place where that order is written down. */
export const navPages: readonly AdminPage[] = adminPages.filter((p) => !p.hideInNav);

/**
 * Where signing in lands.
 *
 * Named, not derived. It used to be `navPages[0].path`, which meant the landing
 * page was whatever happened to be first in the array — and that was the
 * providers list, so an operator signing in was answered with "here is what you
 * configured" rather than "here is whether it is working". On a fresh install it
 * was an empty list.
 *
 * Health answers the question someone actually arrives with, and it carries its
 * own first-run state pointing at provider creation when there is nothing to
 * report yet. `registry.test.ts` pins that this path is mounted and visible in
 * the sidebar, which is the part deriving it from the array used to guarantee.
 */
export const HOME_PATH = "/gateway/health";

/** Which registry entry the current pathname falls on. */
export function resolveAdminPage(pathname: string): AdminPage | undefined {
  return resolveAdminRoute(adminPages, pathname);
}

/**
 * The breadcrumb ancestor of a page. Only a page the sidebar cannot answer
 * "where am I" for has one, which on the registry is exactly `hideInNav`. The
 * ancestor is derived rather than stored in a `parent` field, so it cannot
 * disagree with the paths.
 *
 * The walk goes up one segment at a time rather than simply dropping the last
 * segment. Those are equivalent while every detail page happens to sit one
 * level below its list — and that is a fact about today's registry, not an
 * invariant.
 */
export function breadcrumbParent(pathname: string): AdminPage | undefined {
  return findAdminBreadcrumbParent(adminPages, pathname);
}
