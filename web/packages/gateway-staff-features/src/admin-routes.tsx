import { pickStrings } from "@fairlb/ui";
import { lazy } from "react";

import { MODEL_FILTER_KEYS, PROVIDER_SEARCH_KEYS, TIER_SEARCH_KEYS } from "./search-keys";

/**
 * Gateway 管理页的**组件接线**：路径挂哪个组件、有哪些子页、认哪些查询参数。
 *
 * 两个运营台此前各写了一遍这套接线——`module.tsx`（Cloud）与 `apps/staff/src/registry.tsx`
 * （社区版）里有 12 个逐字相同的 `lazy(() => import(...))` 与同样的子页数组、同样的
 * `validateSearch`，而没有任何东西比对过两份。改一边不改另一边不会有任何报错。
 *
 * 共享的只到这里。**顺序与呈现各自声明**，因为它们本来就不同，实测过：
 *
 *   · 侧栏顺序不同：社区版把 health 放第一（它是 `HOME_PATH`，理由写在那边），
 *     Cloud 放最后。整条路由共享会把其中一边的侧栏顺序改掉。
 *   · `/gateway/tiers` 的 section / group / icon 不同：Cloud 归 `gw-commerce` 组，
 *     社区版侧栏没有这个组（ADR-0149 的三分区）。
 *   · `/orgs/$orgId/access` 社区版挂自己的包装页（补了共享组件刻意不带的页头）。
 *
 * 这个模块是叶子：只 import `lazy` 与筛选键常量，不 import 任何组件、不 import 图标。
 * `module.tsx` 静态引了 dashboard panel、kill-switch、palette 与 host provider 四个真实组件，
 * 社区版若从那里取接线会把它们一起拖进入口 chunk。
 */

const GatewayHealthPage = lazy(() =>
  import("./health").then((module) => ({ default: module.GatewayHealthPage })),
);
const GatewayModelsPage = lazy(() =>
  import("./models").then((module) => ({ default: module.GatewayModelsPage })),
);
const GatewayModelLayout = lazy(() =>
  import("./model-detail").then((module) => ({ default: module.GatewayModelLayout })),
);
const GatewayModelOverviewPage = lazy(() =>
  import("./model-detail").then((module) => ({ default: module.GatewayModelOverviewPage })),
);
const GatewayModelRoutesPage = lazy(() =>
  import("./model-detail").then((module) => ({ default: module.GatewayModelRoutesPage })),
);
const GatewayModelPricingPage = lazy(() =>
  import("./model-detail").then((module) => ({ default: module.GatewayModelPricingPage })),
);
const GatewayProvidersPage = lazy(() =>
  import("./providers").then((module) => ({ default: module.GatewayProvidersPage })),
);
const GatewayProviderLayout = lazy(() =>
  import("./provider-detail").then((module) => ({ default: module.GatewayProviderLayout })),
);
const GatewayProviderOverviewPage = lazy(() =>
  import("./provider-detail").then((module) => ({ default: module.GatewayProviderOverviewPage })),
);
const GatewayProviderModelsPage = lazy(() =>
  import("./provider-detail").then((module) => ({ default: module.GatewayProviderModelsPage })),
);
const GatewayProviderKeysPage = lazy(() =>
  import("./provider-detail").then((module) => ({ default: module.GatewayProviderKeysPage })),
);
const GatewayProviderSettingsPage = lazy(() =>
  import("./provider-detail").then((module) => ({ default: module.GatewayProviderSettingsPage })),
);
const GatewayTiersPage = lazy(() =>
  import("./tiers").then((module) => ({ default: module.GatewayTiersPage })),
);

export const gatewayHealthPage = { component: GatewayHealthPage } as const;

export const gatewayProvidersPage = {
  component: GatewayProvidersPage,
  validateSearch: (search: Record<string, unknown>) => pickStrings(search, PROVIDER_SEARCH_KEYS),
} as const;

export const gatewayProviderDetailPage = {
  component: GatewayProviderLayout,
  hideInNav: true,
  children: [
    { path: "/", component: GatewayProviderOverviewPage },
    { path: "models", component: GatewayProviderModelsPage },
    { path: "keys", component: GatewayProviderKeysPage },
    { path: "settings", component: GatewayProviderSettingsPage },
  ],
} as const;

export const gatewayModelsPage = {
  component: GatewayModelsPage,
  validateSearch: (search: Record<string, unknown>) => pickStrings(search, MODEL_FILTER_KEYS),
} as const;

export const gatewayModelDetailPage = {
  component: GatewayModelLayout,
  hideInNav: true,
  children: [
    { path: "/", component: GatewayModelOverviewPage },
    { path: "routes", component: GatewayModelRoutesPage },
    { path: "pricing", component: GatewayModelPricingPage },
  ],
} as const;

export const gatewayTiersPage = {
  component: GatewayTiersPage,
  validateSearch: (search: Record<string, unknown>) => pickStrings(search, TIER_SEARCH_KEYS),
} as const;
