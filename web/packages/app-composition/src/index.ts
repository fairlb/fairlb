import type { OrgCapability } from "@fairlb/api-client";
import { useI18n, type MessageKey } from "@fairlb/i18n";
import { isPathUnder, type PageHeaderBreadcrumbs } from "@fairlb/ui";
import { useLocation } from "@tanstack/react-router";
import type { ComponentType, FunctionComponent, ReactNode } from "react";

export { mountApp, type MountAppOptions } from "./mount-app";
export {
  RouteFallback,
  APP_ROUTER_DEFAULTS,
  buildPageRoute,
  RequireSession,
  wrapProviders,
  sessionIsUnauthenticated,
  type SessionQuery,
  type RequireSessionProps,
} from "./route-shell";

export type Provider = FunctionComponent<{ children: ReactNode }>;

/**
 * The segment-boundary path rule, re-exported.
 *
 * It moved down into `@fairlb/ui` when the navigation components needed it to
 * decide which item the URL is on: they sit below this package in the graph, and
 * a second copy of the rule here is exactly the drift its own note warns about.
 * The name stays exported from here because every route registry reads it from
 * this package.
 */
export { isPathUnder };

export interface AdminRouteChild {
  /** Relative to the owning resource route. Use "/" for its overview. */
  path: string;
  /**
   * How this child is named where its siblings are listed — the local navigation
   * of the area its parent owns.
   *
   * Optional because most children are the aspects of a record, and those are
   * named by the layout that knows what the record is. It is set on the children
   * of an *area*, whose rail would otherwise be a second hand-written copy of
   * this list.
   */
  navKey?: MessageKey;
  component: FunctionComponent;
  validateSearch?: (search: Record<string, unknown>) => Record<string, unknown>;
  children?: readonly AdminRouteChild[];
}

export interface AdminRoute {
  path: string;
  navKey: MessageKey;
  section: string;
  component: FunctionComponent;
  icon?: ComponentType<{ className?: string }>;
  group?: string;
  superadminOnly?: boolean;
  hideInNav?: boolean;
  /**
   * The nav entry this page belongs under, when the path cannot say so.
   *
   * Both "where am I" devices — the lit sidebar row and the breadcrumb — derive
   * their answer from the path, which works while a detail page lives directly
   * below its list. It does not work when a page is mounted under one noun and
   * presented under another: the community admin's per-team access page lives at
   * `/orgs/$orgId/access` because that is the object's name in the API, while
   * the sidebar entry that leads to it is `Teams`. With neither `/orgs/$orgId`
   * nor `/orgs` registered, the walk returned nothing and the page rendered with
   * no lit row and no breadcrumb — the one detail page in that app, and the only
   * page there that could not answer where it was.
   *
   * Naming the parent explicitly is the narrow fix. It is read by both devices,
   * so a page cannot end up lit under one heading and breadcrumbed under another.
   */
  navParent?: string;
  accountMenuOnly?: boolean;
  validateSearch?: (search: Record<string, unknown>) => Record<string, unknown>;
  /** Persistent task pages owned by this resource layout. */
  children?: readonly AdminRouteChild[];
}

export interface AdminSection {
  id: string;
  labelKey: MessageKey;
  order: number;
}

export interface AdminNavGroup {
  id: string;
  labelKey: MessageKey;
  section: string;
  icon?: ComponentType<{ className?: string }>;
}

export interface AdminOrgArea {
  value: string;
  path: string;
  labelKey: MessageKey;
  order: number;
}

export interface AdminDashboardPanel {
  id: string;
  order: number;
  component: FunctionComponent;
}

export type AdminBanner = FunctionComponent;
export type AdminPaletteSource = FunctionComponent<{
  query: string;
  onPick(path: string): void;
}>;

export interface AdminModule {
  id: string;
  routes: readonly AdminRoute[];
  sections?: readonly AdminSection[];
  navGroups?: readonly AdminNavGroup[];
  orgAreas?: readonly AdminOrgArea[];
  dashboardPanels?: readonly AdminDashboardPanel[];
  banners?: readonly AdminBanner[];
  providers?: readonly Provider[];
  paletteSources?: readonly AdminPaletteSource[];
  permissions?: readonly string[];
}

export interface AdminApp extends Required<Omit<AdminModule, "id" | "permissions">> {
  modules: readonly AdminModule[];
  permissions: readonly string[];
}

export function createAdminApp(modules: readonly AdminModule[]): AdminApp {
  return {
    modules,
    routes: modules.flatMap((module) => module.routes),
    sections: modules.flatMap((module) => module.sections ?? []).sort((a, b) => a.order - b.order),
    navGroups: modules.flatMap((module) => module.navGroups ?? []),
    orgAreas: modules.flatMap((module) => module.orgAreas ?? []).sort((a, b) => a.order - b.order),
    dashboardPanels: modules
      .flatMap((module) => module.dashboardPanels ?? [])
      .sort((a, b) => a.order - b.order),
    banners: modules.flatMap((module) => module.banners ?? []),
    providers: modules.flatMap((module) => module.providers ?? []),
    paletteSources: modules.flatMap((module) => module.paletteSources ?? []),
    permissions: [...new Set(modules.flatMap((module) => module.permissions ?? []))],
  };
}

function adminPathMatcher(path: string): RegExp {
  const body = path
    .split("/")
    .map((segment) =>
      segment.startsWith("$") ? "[^/]+" : segment.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"),
    )
    .join("/");
  return new RegExp(`^${body}$`);
}

/** Resolve a child descriptor into the full path it represents. */
export function adminChildPath(parentPath: string, childPath: string): string {
  if (childPath === "/" || childPath === "") return parentPath;
  return `${parentPath.replace(/\/$/, "")}/${childPath.replace(/^\//, "")}`;
}

function routeOwnsPath(route: AdminRoute, pathname: string): boolean {
  if (adminPathMatcher(route.path).test(pathname)) return true;
  const visit = (parentPath: string, children: readonly AdminRouteChild[]): boolean =>
    children.some((child) => {
      const fullPath = adminChildPath(parentPath, child.path);
      return (
        adminPathMatcher(fullPath).test(pathname) ||
        (child.children !== undefined && visit(fullPath, child.children))
      );
    });
  return route.children !== undefined && visit(route.path, route.children);
}

/** Resolve a concrete pathname against a product's declarative admin routes. */
export function resolveAdminRoute(
  routes: readonly AdminRoute[],
  pathname: string,
): AdminRoute | undefined {
  return routes.find((route) => routeOwnsPath(route, pathname));
}

/**
 * Whether the sidebar row for `route` is the current one.
 *
 * Two ways to be current: the pathname is under this route's own path, or it is
 * under a hidden route that names this one as its `navParent`. Both go through
 * `isPathUnder`, so the segment-boundary rule is stated once for every shell.
 */
export function isAdminNavActive(
  routes: readonly AdminRoute[],
  route: AdminRoute,
  pathname: string,
): boolean {
  if (isPathUnder(pathname, route.path, route.path === "/")) return true;
  const current = resolveAdminRoute(routes, pathname);
  return current?.navParent === route.path;
}

/** A hidden record route needs a list breadcrumb; account-menu pages do not. */
export function isAdminRecordRoute(route: AdminRoute): boolean {
  return route.hideInNav === true && !route.accountMenuOnly;
}

/**
 * The record page's breadcrumb: "which list it belongs to, what that list is
 * called, where it links" all derived from the registry, so a page only says
 * what the record is called. The ancestor label is `t(parent.navKey)` -- the
 * same source as the sidebar entry, so renaming a page moves both. Returns
 * `undefined` where there is no ancestor (a list page, an account page).
 *
 * One implementation for every shell (ADR-0206): the two staff apps used to
 * carry an identical copy each, closing over their own registry.
 */
export function useAdminRecordBreadcrumb(
  routes: readonly AdminRoute[],
  current: string,
): PageHeaderBreadcrumbs | undefined {
  const { t } = useI18n();
  const pathname = useLocation({ select: (location) => location.pathname });
  const parent = findAdminBreadcrumbParent(routes, pathname);
  if (!parent) return undefined;
  return { parentHref: parent.path, parentLabel: t(parent.navKey), current };
}

/** Find the nearest visible route ancestor, skipping hidden intermediate tabs. */
export function findAdminBreadcrumbParent(
  routes: readonly AdminRoute[],
  pathname: string,
): AdminRoute | undefined {
  const route = resolveAdminRoute(routes, pathname);
  if (!route || !isAdminRecordRoute(route)) return undefined;
  if (route.navParent !== undefined) {
    const declared = routes.find((item) => item.path === route.navParent);
    return declared && !declared.hideInNav ? declared : undefined;
  }
  const segments = route.path.split("/").filter(Boolean);
  for (let count = segments.length - 1; count >= 1; count--) {
    const candidate = routes.find((item) => item.path === `/${segments.slice(0, count).join("/")}`);
    if (candidate && !candidate.hideInNav) return candidate;
  }
  return undefined;
}

export type ConsoleNavGroup = "overview" | "build" | "observe" | "manage";

export interface ConsoleDestination {
  id: string;
  path: string;
  scope: "global" | "organization";
  labelKey: MessageKey;
  order: number;
  capability?: OrgCapability;
  /**
   * The label this destination takes when it appears as one item of its own
   * area's local navigation, where the area's name is already the page title.
   *
   * `/billing` is "Billing" in the sidebar and in a breadcrumb, but "Overview"
   * in its own rail — the two are different questions and a destination that
   * answers only the first forces the rail to be written out again by hand.
   * Unset means the two labels are the same, which is true of every child.
   */
  areaLabelKey?: MessageKey;
  accessExact?: boolean;
  sidebar?: {
    group: ConsoleNavGroup;
    icon: ComponentType<{ className?: string }>;
    exact?: boolean;
  };
  settingsArea?: Omit<ConsoleSettingsArea, "path" | "capability">;
}

export interface ConsoleSettingsArea {
  value: string;
  path: string;
  labelKey: MessageKey;
  order: number;
  component: FunctionComponent;
  capability?: OrgCapability;
}

/**
 * Gateway 模块注入的路由条目（扩展点，console 侧）。
 *
 * staff 的 `AdminRoute` 把导航元数据与组件收在一条里，console 分成两半：目的地表
 * 还要服务能力门控与设置任务域（`ConsoleDestination`），而路由要的是组件与
 * `validateSearch`。合并成一条会逼平台的 auth/invite 这类**非 org 作用域路由**
 * 也长出目的地元数据，那是为对称付的假账。
 *
 * `path` 是 TanStack 的完整路由路径（含 `$orgId` 参数），与目的地表的 org 相对
 * 路径**问的不是同一个问题**——后者是「侧栏与门控怎么认这一页」，前者是「路由器
 * 怎么挂这一页」。但那条路径本身仍然只该写一次：它机械地等于
 * `/orgs/$orgId` 接上目的地的路径，而两处手抄会在有人改动其中一处时静默漂开
 * ——表现只是某一页的侧栏不再高亮，没有任何读数会报（ADR-0192）。
 */
export interface ConsoleRoute {
  path: string;
  component: FunctionComponent;
  /** 筛选进 URL（ADR-0022）。键必须全部可选，理由同 main.tsx 的注释。 */
  validateSearch?: (search: Record<string, unknown>) => Record<string, unknown>;
}

export type ConsoleOrgPanel = FunctionComponent<{ orgId: string; canReadFinance: boolean }>;
export type ConsoleKeyRail = FunctionComponent<{ orgId: string }>;
export type ConsoleKeyPanel = FunctionComponent<{ orgId: string; keyId: string }>;
export type ConsoleKeyModels = (orgId: string, enabled: boolean) => string[];

export interface OnboardingStep {
  label: MessageKey;
  to: string;
  capability?: OrgCapability;
  prerequisites?: readonly string[];
}

export interface OnboardingLink {
  to: string;
  label: MessageKey;
}

export interface ConsoleModule {
  id: string;
  destinations: readonly ConsoleDestination[];
  routes?: readonly ConsoleRoute[];
  orgPanels?: readonly ConsoleOrgPanel[];
  providers?: readonly Provider[];
  onboardingSteps?: Readonly<Record<string, OnboardingStep>>;
  onboardingLinks?: readonly OnboardingLink[];
  onboardingDoneLinks?: readonly OnboardingLink[];
  keyRails?: readonly ConsoleKeyRail[];
  keyPanels?: readonly ConsoleKeyPanel[];
  keyModels?: ConsoleKeyModels;
  permissions?: readonly string[];
}

export function createConsoleApp(modules: readonly ConsoleModule[]) {
  const destinations = modules
    .flatMap((module) => module.destinations)
    .sort((a, b) => a.order - b.order);
  return {
    modules,
    destinations,
    routes: modules.flatMap((module) => module.routes ?? []),
    settingsAreas: destinations
      .filter(
        (
          destination,
        ): destination is ConsoleDestination & {
          settingsArea: NonNullable<ConsoleDestination["settingsArea"]>;
        } => destination.settingsArea !== undefined,
      )
      .map((destination) => ({
        ...destination.settingsArea,
        path: destination.path,
        capability: destination.capability,
      }))
      .sort((a, b) => a.order - b.order),
    orgPanels: modules.flatMap((module) => module.orgPanels ?? []),
    providers: modules.flatMap((module) => module.providers ?? []),
    onboardingSteps: Object.assign({}, ...modules.map((module) => module.onboardingSteps ?? {})),
    onboardingLinks: modules.flatMap((module) => module.onboardingLinks ?? []),
    onboardingDoneLinks: modules.flatMap((module) => module.onboardingDoneLinks ?? []),
    keyRails: modules.flatMap((module) => module.keyRails ?? []),
    keyPanels: modules.flatMap((module) => module.keyPanels ?? []),
    keyModels:
      [...modules].reverse().find((module) => module.keyModels !== undefined)?.keyModels ??
      (() => []),
    permissions: [...new Set(modules.flatMap((module) => module.permissions ?? []))],
  };
}

/**
 * Which registered destination owns `path`.
 *
 * Longest match wins, so a broad shell entry such as `/settings` cannot swallow
 * a narrower one that carries its own capability (`/settings/provider-keys`).
 */
export function resolveConsoleDestination(
  destinations: readonly ConsoleDestination[],
  path: string,
): ConsoleDestination | undefined {
  return destinations
    .filter(
      (destination) =>
        destination.scope === "organization" &&
        isPathUnder(path, destination.path, destination.accessExact),
    )
    .reduce<ConsoleDestination | undefined>(
      (best, candidate) => (!best || candidate.path.length > best.path.length ? candidate : best),
      undefined,
    );
}
