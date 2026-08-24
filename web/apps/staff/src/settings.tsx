import { communityStaffApi, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  SettingsHostProvider,
  SettingsRegistryPage,
  type SettingsHost,
} from "@fairlb/settings-features";
import { PageHeader } from "@fairlb/ui";
import { useAdminTitle } from "./lib";

/**
 * The settings page of this deployment (ADR-0198): the registry this binary
 * assembled — the gateway's keys — rendered by the same editor the hosted
 * product's staff console uses. The kill switch keeps its own page; the
 * registry renders a pointer for it.
 */
const host: SettingsHost = {
  useListSettings: () => communityStaffApi.useCommunityListSettings(),
  usePutSettings: () => communityStaffApi.useCommunityPutSettings(),
  errorMessage: apiErrorMessage,
  dedicatedPages: {
    "gateway.kill_switch": { href: "/gateway/health", labelKey: "navGatewayHealth" },
  },
};

export function CommunitySettingsPage() {
  const { t } = useI18n();
  useAdminTitle(t("navGatewaySettings"));
  return (
    <div className="space-y-6">
      <PageHeader title={t("navGatewaySettings")} description={t("settingsPageDesc")} />
      <SettingsHostProvider host={host}>
        <SettingsRegistryPage />
      </SettingsHostProvider>
    </div>
  );
}
