/**
 * The feature package for the pages whose subject is **the gateway itself**:
 * providers and their upstream credentials, the model catalog, prices and access
 * tiers, gateway health.
 *
 * It is a workspace package rather than a folder inside one app not for tidiness but
 * because more than one shell mounts this set, and a copy per app is two
 * implementations that drift. Its sibling `@fairlb/gateway-console-features` holds
 * the other half — the pages about one organization's own traffic.
 *
 * **Dependency direction**: this package knows only `@fairlb/{api-client,i18n,ui}`
 * and Kumo. What belongs to a shell (identity, the destination registry) is injected
 * through `GatewayStaffHostProvider`, see host.tsx. The package **knows no app**.
 *
 * The export surface is limited to what a shell has to mount, plus the few
 * sub-components a shell's own tests render on their own. Internal pieces — form
 * blocks, dialogs, pure helpers — stay inside: exporting one makes it a contract to
 * maintain.
 */
export { GatewayStaffHostProvider, type GatewayStaffHost } from "./host";
export { createGatewayAdminModule } from "./module";

export { GatewayHealthPage } from "./health";
export { GatewayDashboardPanel } from "./dashboard-panel";
export { GatewayKillSwitchBanner } from "./kill-switch";
export { GatewayPaletteResults, type GatewayPaletteSource } from "./palette";
export {
  GatewayModelLayout,
  GatewayModelOverviewPage,
  GatewayModelRoutesPage,
  GatewayModelPricingPage,
  ModelOverview,
} from "./model-detail";
export { GatewayModelsPage } from "./models";
export {
  GatewayPricingPlanLayout,
  GatewayPricingPlanDefaultPage,
  GatewayPricingPlanModelsPage,
  GatewayPricingPlansPage,
} from "./pricing-plans";
export {
  GatewayProviderLayout,
  GatewayProviderOverviewPage,
  GatewayProviderModelsPage,
  GatewayProviderKeysPage,
  GatewayProviderSettingsPage,
} from "./provider-detail";
export { GatewayProvidersPage } from "./providers";
export { GatewayTiersPage } from "./tiers";
export { GatewayOrgAccessPage, GatewayOrgLimitsPage } from "./org-access";
// The readiness checklist is exported because a shell's own tests render it alone.
export { ReadinessSteps } from "./readiness";
