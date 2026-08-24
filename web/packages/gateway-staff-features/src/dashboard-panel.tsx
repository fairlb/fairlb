import { LinkButton } from "@cloudflare/kumo/components/button";
import { gatewayStaffApi, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { Alert, LoadingState, OperationalSummary, SectionHeading } from "@fairlb/ui";

/** Product-owned health summary injected into an operations dashboard. */
export function GatewayDashboardPanel() {
  const { t } = useI18n();
  const health = gatewayStaffApi.useGetGatewayHealth({ query: { refetchInterval: 15_000 } });
  if (health.isError) return <Alert>{apiErrorMessage(health.error)}</Alert>;
  if (!health.data) return <LoadingState label={t("loading")} />;

  const providers = health.data.providers ?? [];
  const degraded = providers.filter((provider) => provider.breaker_status !== "closed");
  const retryPct = health.data.retry_budget.requests
    ? (health.data.retry_budget.retries / health.data.retry_budget.requests) * 100
    : 0;

  return (
    <section className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <SectionHeading>{t("navGatewayHealth")}</SectionHeading>
        <LinkButton href="/gateway/health" variant="outline" size="sm">
          {t("navGatewayHealth")}
        </LinkButton>
      </div>
      <OperationalSummary
        label={t("navGatewayHealth")}
        items={[
          {
            label: t("gwProviderStatus"),
            value: `${providers.length - degraded.length}/${providers.length}`,
            detail: degraded.length ? `${degraded.length} ${t("gwBreakerOpen")}` : undefined,
            tone: degraded.length ? "degraded" : "healthy",
          },
          {
            label: t("gwRetryBudget"),
            value: `${retryPct.toFixed(1)}%`,
            detail: `${health.data.retry_budget.retries}/${health.data.retry_budget.requests}`,
            tone: retryPct >= 10 ? "degraded" : "healthy",
          },
        ]}
      />
    </section>
  );
}
