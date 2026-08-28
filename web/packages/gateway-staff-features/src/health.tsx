import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useDisplayDate, useI18n } from "@fairlb/i18n";
import {
  Alert,
  Card,
  DataTable,
  InlineEmpty,
  PageHeader,
  SectionHeading,
  StatusBadge,
  RowTitleLink,
  useAdminTitle,
} from "@fairlb/ui";
import { LinkButton } from "@cloudflare/kumo/components/button";
import { KillSwitchCard } from "./kill-switch";

export function GatewayHealthPage() {
  const { t } = useI18n();
  useAdminTitle(t("navGatewayHealth"));
  return <HealthContent />;
}

/**
 * The repair queue: jobs that reached a terminal state while their reservation
 * never moved.
 *
 * Three states, and they are three different sentences. Absent means the count
 * could not be read — rendered as such, never as zero, because on this question
 * "we could not tell" wearing the face of "nothing is stuck" is exactly how the
 * queue goes unwatched. Zero is an explicit all-clear, which is worth a line:
 * it says the check ran. Anything above zero is money sitting still on somebody's
 * account, so it is an Alert and not a statistic.
 */
function StuckMoneyCard({
  stuck,
  loading,
}: {
  stuck?: GatewayStaffTypes.GatewayStuckMoney;
  loading: boolean;
}) {
  const { t } = useI18n();
  const displayDate = useDisplayDate();
  if (loading) return null;
  return (
    <Card className="space-y-2">
      <SectionHeading>{t("gwStuckMoneyTitle")}</SectionHeading>
      {!stuck ? (
        <p className="text-base text-kumo-subtle">{t("gwStuckMoneyUnknown")}</p>
      ) : stuck.jobs === 0 ? (
        <p className="text-base">{t("gwStuckMoneyClear")}</p>
      ) : (
        <Alert variant="error">
          {t("gwStuckMoneyFound", { jobs: stuck.jobs })}
          {stuck.oldest_terminal_at
            ? ` ${t("gwStuckMoneyOldest", { time: displayDate(stuck.oldest_terminal_at) })}`
            : ""}
        </Alert>
      )}
      <p className="text-base text-kumo-subtle">{t("gwStuckMoneyHint")}</p>
    </Card>
  );
}

function HealthContent() {
  const { t } = useI18n();
  const health = gatewayStaffApi.useGetGatewayHealth({ query: { refetchInterval: 15_000 } });

  // Keep the page header on error: a failure should not cost the reader their sense
  // of which page they are on.
  if (health.isError)
    return (
      <div className="space-y-6">
        <PageHeader title={t("navGatewayHealth")} description={t("staffGatewayHealthDesc")} />
        <Alert>{apiErrorMessage(health.error)}</Alert>
      </div>
    );
  const h = health.data;
  const providers = h?.providers ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title={t("navGatewayHealth")} description={t("staffGatewayHealthDesc")} />

      <KillSwitchCard counts={h?.switch_counts} />

      <StuckMoneyCard stuck={h?.stuck_money} loading={!h} />

      <Card className="space-y-2">
        <SectionHeading>{t("gwRetryBudget")}</SectionHeading>
        {/* The global retry budget is capped at 10%; approaching it means upstreams
            are flapping at scale. */}
        <p className="text-base">
          {h
            ? t("gwRetryRatio", {
                retries: h.retry_budget.retries,
                requests: h.retry_budget.requests,
                pct:
                  h.retry_budget.requests > 0
                    ? ((h.retry_budget.retries / h.retry_budget.requests) * 100).toFixed(1)
                    : "0.0",
                cap: 10,
              })
            : t("gwLoading")}
        </p>
      </Card>

      <section className="space-y-3">
        <SectionHeading>{t("gwProviderStatus")}</SectionHeading>
        <DataTable caption={t("gwProviderStatus")}>
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head>{t("gwColProvider")}</DataTable.Head>
              <DataTable.Head>{t("gwColBreaker")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwCol1hReq")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwCol1hErr")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwColErrRate")}</DataTable.Head>
              {/* Latency. Without these columns the page answers "is it erroring"
                  but not "has it got slower" — and an upstream degrading without
                  failing is the commonest kind of incident there is. */}
              <DataTable.Head className="text-right">{t("gwColP50")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwColP95")}</DataTable.Head>
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            {providers.map((p: GatewayStaffTypes.GatewayProviderHealth) => (
              <DataTable.Row key={p.provider_id} interactive>
                {/* Spotting a provider with a high error rate has to be followed by
                    opening it. The provider id was on the record all along while
                    this cell rendered as plain text. */}
                <DataTable.Cell className="relative font-mono">
                  <RowTitleLink
                    to="/gateway/providers/$providerId"
                    params={{ providerId: p.provider_id }}
                  >
                    {p.slug}
                  </RowTitleLink>
                </DataTable.Cell>
                <DataTable.Cell>
                  <BreakerBadge status={p.breaker_status} until={p.cooldown_until} />
                </DataTable.Cell>
                <DataTable.Cell className="text-right">{p.requests_1h}</DataTable.Cell>
                <DataTable.Cell className="text-right">{p.errors_1h}</DataTable.Cell>
                <DataTable.Cell className="text-right">
                  {p.requests_1h > 0 ? `${((p.errors_1h / p.requests_1h) * 100).toFixed(1)}%` : "—"}
                </DataTable.Cell>
                <LatencyCells latency={p.latency_1h} />
              </DataTable.Row>
            ))}
            {providers.length === 0 && (
              <DataTable.Row>
                <DataTable.Cell colSpan={7}>
                  {/* This page is where the community admin lands, so on a fresh
                      install an empty provider table is the first thing anyone
                      sees. "No providers" alone is a true statement and a dead
                      end; the reason there is nothing to report is that nothing
                      has been set up yet, and the next step belongs here. */}
                  <InlineEmpty title={t("gwNoProviders")} description={t("gwNoProvidersHint")}>
                    <LinkButton variant="outline" size="sm" href="/gateway/providers">
                      {t("gwAddFirstProvider")}
                    </LinkButton>
                  </InlineEmpty>
                </DataTable.Cell>
              </DataTable.Row>
            )}
          </DataTable.Body>
        </DataTable>
        {/* Breaker state lives in each instance's memory, so this is a snapshot as
            of the query and instances may disagree.

            Not shown with an empty table: the caveat is about how to read the
            breaker column, and on a fresh install there is no breaker to read —
            the empty state's next step is to add a provider. */}
        {providers.length > 0 && (
          <p className="mt-2 text-base text-kumo-subtle">{t("gwBreakerNote")}</p>
        )}
      </section>
    </div>
  );
}

/**
 * The p50 and p95 cells, which **make two honesty bits visible**.
 *
 * - No latency samples renders `—`, never `0 ms`. Rolled-up rows recorded before
 *   latency was tracked have requests but no samples, and drawing those as 0 ms
 *   makes that window look impossibly fast.
 * - A p95 beyond the largest trustworthy bound renders as a lower bound — "≥ 10s" —
 *   rather than a specific number. When a window mixes rows with different bounds,
 *   only the smallest of them can be trusted, and **inventing a precise figure is
 *   worse than saying "at least this much"**.
 */
function LatencyCells({ latency }: { latency?: GatewayStaffTypes.GatewayProviderLatency }) {
  const { t } = useI18n();
  if (!latency?.has_samples) {
    return (
      <>
        <DataTable.Cell className="text-right text-kumo-subtle">—</DataTable.Cell>
        <DataTable.Cell className="text-right text-kumo-subtle">—</DataTable.Cell>
      </>
    );
  }
  return (
    <>
      <DataTable.Cell className="text-right tabular-nums">
        {t("gwMillis", { ms: latency.p50_ms ?? 0 })}
      </DataTable.Cell>
      <DataTable.Cell className="text-right tabular-nums">
        {latency.p95_unbounded
          ? t("gwMillisAtLeast", { ms: latency.p95_ms ?? 0 })
          : t("gwMillis", { ms: latency.p95_ms ?? 0 })}
      </DataTable.Cell>
    </>
  );
}

function BreakerBadge({ status, until }: { status: string; until?: string | null }) {
  const { t } = useI18n();
  const displayDate = useDisplayDate();
  if (status === "open") {
    return (
      <span className="flex flex-wrap items-center gap-1">
        <StatusBadge tone="danger">{t("gwBreakerOpen")}</StatusBadge>
        {until && (
          <span className="text-base text-kumo-subtle">
            {t("gwBreakerUntil", { time: displayDate(until) })}
          </span>
        )}
      </span>
    );
  }
  if (status === "half_open") {
    return <StatusBadge tone="warning">{t("gwBreakerHalf")}</StatusBadge>;
  }
  return <StatusBadge tone="success">{t("gwBreakerClosed")}</StatusBadge>;
}
