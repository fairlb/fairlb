import { communityStaffApi } from "@fairlb/api-client";
import { GatewayOrgLimitsPage } from "@fairlb/gateway-staff-features/org-access";
import { useI18n } from "@fairlb/i18n";
import { PageHeader, useAdminTitle } from "@fairlb/ui";
import { useParams } from "@tanstack/react-router";
import { useRecordBreadcrumb } from "./gateway-provider";

/**
 * The header for this deployment's only detail page.
 *
 * # Why the header lives in the shell rather than in the page
 *
 * `GatewayOrgLimitsPage` is one of two entry points into the same content, and
 * the other one — Cloud's `GatewayOrgAccessPage` — is mounted *below* a record
 * layout that already renders the org's name, status and record-level actions.
 * A header inside the shared component would give that surface two of them.
 * Which shell supplies the header is precisely the difference between the two
 * entry points, so it is answered where the difference lives: at the mount
 * point.
 *
 * # What it fixes
 *
 * The page had no `PageHeader` at all in this app: no h1, no breadcrumbs, no
 * document title. The registry had already been taught that its ancestor is
 * `/teams` (`navParent`), and a test asserts that derivation — so the trail was
 * being computed and then read by nobody. The sidebar half of "where am I" was
 * fixed without the page's own half.
 *
 * The name comes from the team list because there is no single-team read and the
 * gateway settings response carries the tier, not the org. The list is
 * unpaginated by contract ("oldest first", every row), so a lookup in it is not
 * a best-effort match that quietly degrades on a later page.
 */
export function CommunityTeamAccessPage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const teams = communityStaffApi.useCommunityListTeams();
  const team = teams.data?.items.find((item) => item.id === orgId);
  // While the list is in flight the title is the loading text rather than the
  // final label, for the same reason the record detail pages do it: a heading
  // that says the team's name before the name is known would be inventing one.
  const pendingLabel = teams.isPending || teams.isFetching ? t("loading") : t("teamAccess");
  const label = team?.name ?? pendingLabel;
  const breadcrumbs = useRecordBreadcrumb(label);
  useAdminTitle(label);
  return (
    <div className="space-y-6">
      <PageHeader breadcrumbs={breadcrumbs} title={label} description={t("teamAccessHint")} />
      <GatewayOrgLimitsPage />
    </div>
  );
}
