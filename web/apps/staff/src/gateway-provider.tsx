import { ORG_CAPABILITIES, communityStaffApi } from "@fairlb/api-client";
import {
  GatewayConsoleHostProvider,
  type GatewayConsoleHost,
} from "@fairlb/gateway-console-features/host";
import { GatewayStaffHostProvider } from "@fairlb/gateway-staff-features/host";
import { useI18n } from "@fairlb/i18n";
import { Centered, useAdminTitle, type PageHeaderBreadcrumbs } from "@fairlb/ui";
import type { ReactNode } from "react";
import { useCurrentAdmin } from "./admin-session";
import { useAdminRecordBreadcrumb } from "@fairlb/app-composition";
import { adminPages } from "./registry";

/** Exported because the shell renders one page's header itself: the team access
 * page is mounted standalone here and has no record layout above it to supply
 * one. Both callers must derive the ancestor the same way, so there is one. */
export function useRecordBreadcrumb(current: string): PageHeaderBreadcrumbs | undefined {
  return useAdminRecordBreadcrumb(adminPages, current);
}

const gatewayStaffHost = {
  useCurrentStaffRole: () => useCurrentAdmin().role,
  useRecordBreadcrumb,
};

const communityOrgCapabilities = Object.values(ORG_CAPABILITIES);

function CommunityOrgNotFound() {
  const { t } = useI18n();
  return <Centered>{t("notFoundTitle")}</Centered>;
}

const gatewayConsoleHost: GatewayConsoleHost = {
  useOrg: () => ({ id: useCurrentAdmin().org_id, capabilities: communityOrgCapabilities }),
  useTitle: useAdminTitle,
  useImpersonating: () => false,
  OrgNotFound: CommunityOrgNotFound,
  useOrgSettings: () => ({
    org: { id: useCurrentAdmin().org_id, capabilities: communityOrgCapabilities },
    canManage: true,
  }),
  useApiKeyOptions: (_orgId, enabled) => {
    const query = communityStaffApi.useCommunityListKeys(undefined, { query: { enabled } });
    return {
      isPending: query.isPending,
      isError: query.isError,
      error: query.error,
      items: query.data?.items ?? [],
    };
  },
};

export function GatewayProvider({ children }: { children: ReactNode }) {
  return (
    <GatewayStaffHostProvider host={gatewayStaffHost}>
      <GatewayConsoleHostProvider host={gatewayConsoleHost}>{children}</GatewayConsoleHostProvider>
    </GatewayStaffHostProvider>
  );
}
