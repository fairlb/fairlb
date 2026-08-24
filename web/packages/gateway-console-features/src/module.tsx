import { ORG_CAPABILITIES } from "@fairlb/api-client";
import type { ConsoleModule, ConsoleRoute, Provider } from "@fairlb/app-composition";
import { pickStrings } from "@fairlb/ui";
import { ChartLineIcon, CubeIcon, ListMagnifyingGlassIcon, PlayIcon } from "@phosphor-icons/react";
import { lazy, type FunctionComponent, type ReactNode } from "react";
import { GatewayConsoleHostProvider, type GatewayConsoleHost } from "./host";
import { useKeyModelOptions } from "./key-models";
import {
  MODEL_SEARCH_KEYS,
  PLAYGROUND_SEARCH_KEYS,
  REQUEST_SEARCH_KEYS,
  USAGE_SEARCH_KEYS,
} from "./search-keys";

/**
 * 每个 org 作用域页面的路径，只写这一次。
 *
 * 目的地表与路由表要的是不同的东西（门控与侧栏 vs 组件与 `validateSearch`，
 * 理由见 `ConsoleRoute` 的文档），但**路径**在两处指同一页：路由路径机械地等于
 * `/orgs/$orgId` 接上目的地路径。此前两处各写一遍，改一处不改另一处不会红——
 * 表现只是这一页照常打得开而侧栏不再高亮它（ADR-0192）。
 */
const ORG_PATHS = {
  models: "/models",
  playground: "/playground",
  requests: "/requests",
  usage: "/usage",
  providerKeys: "/settings/provider-keys",
} as const;

/** 路由器挂载 org 作用域页面的前缀。 */
const ORG_ROUTE_PREFIX = "/orgs/$orgId";

function orgRoute(
  path: string,
  component: FunctionComponent,
  keys: readonly string[],
): ConsoleRoute {
  return {
    path: `${ORG_ROUTE_PREFIX}${path}`,
    component,
    validateSearch: (search: Record<string, unknown>) => pickStrings(search, keys),
  };
}

export function createGatewayConsoleModule(host: GatewayConsoleHost): ConsoleModule {
  const GatewayProvider: Provider = ({ children }: { children: ReactNode }) => (
    <GatewayConsoleHostProvider host={host}>{children}</GatewayConsoleHostProvider>
  );
  const ModelsPage = lazy(() =>
    import("./models").then((module) => ({ default: module.ModelsPage })),
  );
  const PlaygroundPage = lazy(() =>
    import("./playground").then((module) => ({ default: module.PlaygroundPage })),
  );
  const RequestsPage = lazy(() =>
    import("./requests").then((module) => ({ default: module.RequestsPage })),
  );
  const UsagePage = lazy(() => import("./usage").then((module) => ({ default: module.UsagePage })));
  const SettingsProviderKeysPage = lazy(() =>
    import("./settings-provider-keys").then((module) => ({
      default: module.SettingsProviderKeysPage,
    })),
  );
  const OrgDashboard = lazy(() =>
    import("./org-dashboard").then((module) => ({ default: module.OrgDashboard })),
  );
  const KeyUsageRail = lazy(() =>
    import("./key-widgets").then((module) => ({ default: module.KeyUsageRail })),
  );
  const KeySpendCurve = lazy(() =>
    import("./key-widgets").then((module) => ({ default: module.KeySpendCurve })),
  );

  return {
    id: "gateway",
    permissions: ["gateway.read", "gateway.keys.write"],
    providers: [GatewayProvider],
    onboardingSteps: {
      first_request: {
        label: "obStepFirstRequest",
        to: "/orgs/$orgId/playground",
        capability: ORG_CAPABILITIES.playgroundUse,
        prerequisites: ["create_key", "top_up"],
      },
    },
    onboardingLinks: [{ to: "/orgs/$orgId/models", label: "obBrowseModels" }],
    onboardingDoneLinks: [{ to: "/orgs/$orgId/requests", label: "obViewLogs" }],
    destinations: [
      {
        id: "models",
        path: ORG_PATHS.models,
        scope: "organization",
        order: 20,
        labelKey: "modelsTitle",
        sidebar: { group: "build", icon: CubeIcon },
      },
      {
        id: "playground",
        path: ORG_PATHS.playground,
        scope: "organization",
        order: 40,
        labelKey: "playTitle",
        capability: ORG_CAPABILITIES.playgroundUse,
        sidebar: { group: "build", icon: PlayIcon },
      },
      {
        id: "requests",
        path: ORG_PATHS.requests,
        scope: "organization",
        order: 60,
        labelKey: "logsTitle",
        sidebar: { group: "observe", icon: ListMagnifyingGlassIcon },
      },
      {
        id: "usage",
        path: ORG_PATHS.usage,
        scope: "organization",
        order: 70,
        labelKey: "usageTitle",
        sidebar: { group: "observe", icon: ChartLineIcon },
      },
      {
        id: "settings-provider-keys",
        path: ORG_PATHS.providerKeys,
        scope: "organization",
        order: 110,
        labelKey: "settingsAreaProviderKeys",
        capability: ORG_CAPABILITIES.keysManage,
        settingsArea: {
          value: "provider-keys",
          labelKey: "settingsAreaProviderKeys",
          order: 30,
          component: SettingsProviderKeysPage,
        },
      },
    ],
    routes: [
      orgRoute(ORG_PATHS.models, ModelsPage, MODEL_SEARCH_KEYS),
      orgRoute(ORG_PATHS.playground, PlaygroundPage, PLAYGROUND_SEARCH_KEYS),
      orgRoute(ORG_PATHS.requests, RequestsPage, REQUEST_SEARCH_KEYS),
      orgRoute(ORG_PATHS.usage, UsagePage, USAGE_SEARCH_KEYS),
    ],
    orgPanels: [OrgDashboard],
    keyRails: [KeyUsageRail],
    keyPanels: [KeySpendCurve],
    keyModels: useKeyModelOptions,
  };
}
