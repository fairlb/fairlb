import type { AdminModule, Provider } from "@fairlb/app-composition";
import {
  gatewayHealthPage,
  gatewayModelDetailPage,
  gatewayModelsPage,
  gatewayProviderDetailPage,
  gatewayProvidersPage,
  gatewayTiersPage,
} from "./admin-routes";
import {
  CubeIcon,
  HeartbeatIcon,
  PlugsIcon,
  StorefrontIcon,
  TagIcon,
  UsersThreeIcon,
} from "@phosphor-icons/react";
import { lazy, type ReactNode } from "react";
import { GatewayDashboardPanel } from "./dashboard-panel";
import { GatewayStaffHostProvider, type GatewayStaffHost } from "./host";
import { GatewayKillSwitchBanner } from "./kill-switch";
import { GatewayPaletteResults } from "./palette";

const GatewayPricingPlanLayout = lazy(() =>
  import("./pricing-plans").then((module) => ({ default: module.GatewayPricingPlanLayout })),
);
const GatewayPricingPlanDefaultPage = lazy(() =>
  import("./pricing-plans").then((module) => ({ default: module.GatewayPricingPlanDefaultPage })),
);
const GatewayPricingPlanModelsPage = lazy(() =>
  import("./pricing-plans").then((module) => ({ default: module.GatewayPricingPlanModelsPage })),
);
const GatewayPricingPlansPage = lazy(() =>
  import("./pricing-plans").then((module) => ({ default: module.GatewayPricingPlansPage })),
);
const GatewayOrgAccessPage = lazy(() =>
  import("./org-access").then((module) => ({ default: module.GatewayOrgAccessPage })),
);

export function createGatewayAdminModule(host: GatewayStaffHost): AdminModule {
  const GatewayProvider: Provider = ({ children }: { children: ReactNode }) => (
    <GatewayStaffHostProvider host={host}>{children}</GatewayStaffHostProvider>
  );

  return {
    id: "gateway",
    permissions: ["gateway.read", "gateway.write", "gateway.pricing.write"],
    providers: [GatewayProvider],
    banners: [GatewayKillSwitchBanner],
    dashboardPanels: [{ id: "gateway-health", order: 20, component: GatewayDashboardPanel }],
    paletteSources: [GatewayPaletteResults],
    sections: [{ id: "gateway", labelKey: "navSectionGateway", order: 50 }],
    navGroups: [
      {
        id: "gw-commerce",
        labelKey: "navGroupCommerce",
        section: "gateway",
        icon: StorefrontIcon,
      },
    ],
    orgAreas: [
      {
        value: "access",
        path: "/orgs/$orgId/access",
        labelKey: "orgAreaAccess",
        order: 30,
      },
    ],
    routes: [
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
        group: "gw-commerce",
        icon: UsersThreeIcon,
        ...gatewayTiersPage,
      },
      {
        path: "/gateway/pricing-plans",
        navKey: "navGatewayPricingPlans",
        section: "gateway",
        group: "gw-commerce",
        component: GatewayPricingPlansPage,
        icon: TagIcon,
      },
      {
        path: "/gateway/pricing-plans/$pricingPlanId",
        navKey: "navGatewayPricingPlans",
        section: "gateway",
        component: GatewayPricingPlanLayout,
        hideInNav: true,
        children: [
          { path: "/", component: GatewayPricingPlanDefaultPage },
          { path: "models", component: GatewayPricingPlanModelsPage },
        ],
      },
      {
        path: "/orgs/$orgId/access",
        navKey: "navOrgs",
        section: "operations",
        component: GatewayOrgAccessPage,
        hideInNav: true,
      },
      {
        path: "/gateway/health",
        navKey: "navGatewayHealth",
        section: "gateway",
        icon: HeartbeatIcon,
        ...gatewayHealthPage,
      },
    ],
  };
}
