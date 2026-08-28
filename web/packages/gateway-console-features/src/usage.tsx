import { LinkButton } from "@cloudflare/kumo/components/button";
import {
  gatewayConsoleApi,
  type GatewayConsoleTypes,
  ORG_CAPABILITIES,
  apiErrorMessage,
  hasOrgCapability,
} from "@fairlb/api-client";
import { browserTZ, useI18n } from "@fairlb/i18n";
import {
  PageHeader,
  Alert,
  Card,
  DataTable,
  Field,
  FormRow,
  InlineEmpty,
  LoadingState,
  RankBars,
  SectionHeading,
  Select,
  StatTile,
  TrendChart,
  formatNano,
  RANGES,
  pickRange,
  useQuantizedRange,
} from "@fairlb/ui";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useEffect } from "react";
import { rankUsageGroups } from "./usage-ranking";
import {
  OrgNotFound,
  useConsoleTitle,
  useImpersonating,
  useOrg,
  useApiKeyOptions,
  type ApiKeyOptions,
} from "./host";

export function UsagePage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const org = useOrg(orgId);
  const impersonating = useImpersonating();
  // Host-driven fetching stays in the **outer** component, as it does on the logs
  // page: the inner one is presentation, and a test only wants the chart and table.
  const apiKeys = useApiKeyOptions(
    org?.id ?? "",
    org !== undefined && hasOrgCapability(org, ORG_CAPABILITIES.keysManage),
  );
  useConsoleTitle(org ? t("usageTitle") : undefined);
  if (!org) return <OrgNotFound />;
  const canReadFinance = hasOrgCapability(org, ORG_CAPABILITIES.financeDetailsRead);
  return (
    <UsageDetail
      key={org.id}
      orgId={org.id}
      canReadFinance={canReadFinance}
      canFilterKeys={hasOrgCapability(org, ORG_CAPABILITIES.keysManage)}
      canExport={canReadFinance && !impersonating}
      apiKeys={apiKeys}
    />
  );
}

function UsageDetail({
  orgId,
  canReadFinance,
  canFilterKeys,
  canExport,
  apiKeys,
}: {
  orgId: string;
  canReadFinance: boolean;
  canFilterKeys: boolean;
  canExport: boolean;
  apiKeys?: ApiKeyOptions;
}) {
  const { formatNumber, t } = useI18n();
  const navigate = useNavigate();
  // Filters live in the URL: the view can be shared, bookmarked, and stepped back to.
  const search = useSearch({ strict: false }) as { range?: string; group?: string; key?: string };
  const rangeKey = search.range ?? "7d";
  const groupBy: "model" | "api_key" =
    canFilterKeys && search.group === "api_key" ? "api_key" : "model";
  const apiKeyId = canFilterKeys ? (search.key ?? "") : "";
  const setFilter = (patch: Record<string, string | undefined>) =>
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, ...patch }),
      replace: true,
    });
  const range = pickRange(rangeKey, "7d");

  // Someone without the right who opens a shared key drill-down link still only
  // reads traffic by model. The URL is cleaned with a replace, and only the key
  // filter and an explicit api_key grouping are dropped — unrelated filters such as
  // the time range survive untouched.
  useEffect(() => {
    if (canFilterKeys) return;
    const clearKey = search.key !== undefined;
    const clearGroup = search.group === "api_key";
    if (!clearKey && !clearGroup) return;
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({
        ...prev,
        ...(clearKey ? { key: undefined } : {}),
        ...(clearGroup ? { group: undefined } : {}),
      }),
      replace: true,
    });
  }, [canFilterKeys, navigate, search.group, search.key]);

  // from/to change with the selected range and nothing else. Memoizing `new Date()`
  // on the range only pins the instant **within one render** — nothing holds it
  // across mounts, so every visit to the page produced a fresh query key. Quantized
  // to the hour, the pair is byte-identical for everyone within the same hour.
  const { from, to } = useQuantizedRange(range.hours);

  const usage = gatewayConsoleApi.useGetUsage(orgId, {
    from,
    to,
    granularity: range.granularity,
    group_by: groupBy,
    ...(apiKeyId ? { api_key_id: apiKeyId } : {}),
    // Day boundaries follow the reader's own time zone. Left unsent, the server
    // cuts days in UTC while the labels below render locally, so a daily bar spans
    // two local days and its date can be off by a whole one.
    tz: browserTZ(),
  });
  // Tests render this without `apiKeys`, so it degrades to an empty, settled state
  // rather than throwing.
  const keys = apiKeys ?? { isPending: false, isError: false, error: null, items: [] };

  const data = usage.data;
  const currency = data?.totals.currency ?? "USD";
  const lat = data?.totals.latency;
  const fmtMoney = (nano: number) => formatNano(nano) + " " + currency;
  const fmtCount = formatNumber;
  const rankedGroups = rankUsageGroups(data?.groups ?? [], canReadFinance ? "finance" : "requests");

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("usageTitle")}
        description={t(canReadFinance ? "usageDesc" : "usageTrafficDesc")}
      />

      {/* Filters sit in one row above the chart; the field wrapper is what gives
          each select its accessible label. */}
      <FormRow
        className={
          canFilterKeys
            ? "sm:grid-cols-2 lg:grid-cols-[10rem_10rem_11rem_auto]"
            : "sm:grid-cols-[10rem]"
        }
      >
        <FormRow.Item>
          <Field label={t("commonTimeRange")}>
            <Select
              value={rangeKey}
              onValueChange={(v) => setFilter({ range: v ?? undefined })}
              items={RANGES.map((r) => ({ value: r.key as string, label: t(r.label) }))}
            />
          </Field>
        </FormRow.Item>
        {canFilterKeys && (
          <>
            <FormRow.Item>
              <Field label={t("usageGroupBy")}>
                <Select
                  value={groupBy as string}
                  onValueChange={(v) => setFilter({ group: v ?? undefined })}
                  items={[
                    { value: "model", label: t("usageByModel") },
                    { value: "api_key", label: t("usageByKey") },
                  ]}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              {/* Key names belong to the credential-management context, so without
                  that right the list request is never sent at all. */}
              <Field label={t("apiKey")}>
                <Select
                  value={apiKeyId}
                  disabled={keys.isPending}
                  onValueChange={(v) => setFilter({ key: v || undefined })}
                  items={[
                    { value: "", label: t("logsAllKeys") },
                    ...keys.items.map((k) => ({ value: k.id, label: k.name })),
                  ]}
                />
              </Field>
            </FormRow.Item>
          </>
        )}
        {canExport && (
          <FormRow.Actions>
            {/* The CSV is a plain link rather than a fetch: a native download does
              not have to hold the whole export in memory first. The URL comes from
              the generated helper rather than being assembled by hand — a hand-made
              one is identical today but would not follow the spec when the path
              changes. */}
            <LinkButton
              href={gatewayConsoleApi.getExportUsageCSVUrl(orgId, {
                from,
                to,
                granularity: range.granularity,
                ...(apiKeyId ? { api_key_id: apiKeyId } : {}),
                tz: browserTZ(),
              })}
              variant="outline"
            >
              {t("commonExportCsv")}
            </LinkButton>
          </FormRow.Actions>
        )}
      </FormRow>

      {/* Errors render in place, leaving the header and the filters where they are,
          instead of replacing the whole page. Loading and empty stay distinct: an
          in-flight request rendered as "no data" reads as an answer. */}
      {usage.isError ? (
        <Alert>{apiErrorMessage(usage.error)}</Alert>
      ) : usage.isPending ? (
        <LoadingState label={t("loading")} />
      ) : (
        <>
          {/* Headline numbers are stat tiles, not charts with a single bar.
              Latency is among them because this page's own description promises
              "traffic, tokens, spend, latency, and errors over time", and
              `totals.latency` was in the response all along — fetched here and
              thrown away. Without it the detail page is a strict subset of the
              summary: you see a p95 on the dashboard, click through to dig, and
              latency disappears. The two render it the same way, including the
              has_samples and p95_unbounded branches. */}
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {canReadFinance && (
              <StatTile
                label={t("usageSpend")}
                value={data ? fmtMoney(data.totals.charged_nano) : "—"}
              />
            )}
            <StatTile
              label={t("usageRequests")}
              value={data ? fmtCount(data.totals.requests) : "—"}
            />
            <StatTile
              label={t("usageTokens")}
              value={data ? fmtCount(data.totals.tokens_in + data.totals.tokens_out) : "—"}
              hint={
                data
                  ? t("usageTokensHint", {
                      in: fmtCount(data.totals.tokens_in),
                      out: fmtCount(data.totals.tokens_out),
                    })
                  : undefined
              }
            />
            {/* Each unit gets its own tile, and only when there is any.
                Seconds of video and prepaid generations are not added together
                any more than either is added into the token tile beside them:
                they are different dimensions, and one number covering both
                denotes nothing while still looking like an answer. A tile of
                nothing but zero conveys nothing either, and squeezes the ones
                beside it — the same rule the model catalog applies to a column
                of dashes. */}
            {data && data.totals.billed_seconds > 0 && (
              <StatTile
                label={t("usageBilledSeconds")}
                value={fmtCount(data.totals.billed_seconds)}
                hint={t("usageBilledSecondsHint")}
              />
            )}
            {data && data.totals.billed_calls > 0 && (
              <StatTile
                label={t("usageBilledCalls")}
                value={fmtCount(data.totals.billed_calls)}
                hint={t("usageBilledCallsHint")}
              />
            )}
            {data && data.totals.billed_images > 0 && (
              <StatTile
                label={t("usageBilledImages")}
                value={fmtCount(data.totals.billed_images)}
                hint={t("usageBilledImagesHint")}
              />
            )}
            <StatTile
              label={t("usageErrors")}
              value={data ? fmtCount(data.totals.errors ?? 0) : "—"}
              hint={
                data && data.totals.requests > 0
                  ? t("usageErrorRate", {
                      rate: (((data.totals.errors ?? 0) / data.totals.requests) * 100).toFixed(2),
                    })
                  : undefined
              }
            />
            {/* With no samples, say so rather than printing 0 ms. Historical ranges
                from before latency was recorded look exactly like this, and a 0
                would make them look impossibly fast. */}
            <StatTile
              label={t("dashLatencyP50")}
              value={lat?.has_samples ? `${lat.p50_ms ?? 0} ms` : "—"}
              hint={
                lat?.has_samples
                  ? t("dashLatencyMean", { ms: lat.mean_ms ?? 0 })
                  : data
                    ? t("dashLatencyNone")
                    : undefined
              }
            />
            <StatTile
              label={t("dashLatencyP95")}
              value={
                lat?.has_samples
                  ? lat.p95_unbounded
                    ? t("dashP95AtLeast", { ms: lat.p95_ms ?? 0 })
                    : `${lat.p95_ms ?? 0} ms`
                  : "—"
              }
            />
          </div>

          <Card className="space-y-3">
            <SectionHeading>
              {t(canReadFinance ? "usageSpendTrend" : "usageRequestTrend")}
            </SectionHeading>
            <TrendChart
              name={t(canReadFinance ? "usageSpend" : "usageRequests")}
              points={(data?.series ?? []).map((p) => ({
                at: new Date(p.bucket_start).getTime(),
                value: canReadFinance ? p.charged_nano : p.requests,
              }))}
              format={canReadFinance ? fmtMoney : fmtCount}
            />
          </Card>

          <Card className="space-y-3">
            <SectionHeading>
              {groupBy === "model" ? t("usageByModel") : t("usageByKey")}
            </SectionHeading>
            <RankBars
              name={groupBy === "model" ? t("usageByModel") : t("usageByKey")}
              items={rankedGroups.map((g) => ({
                label: g.label || g.key,
                value: canReadFinance ? g.charged_nano : g.requests,
              }))}
              format={canReadFinance ? fmtMoney : fmtCount}
            />
          </Card>

          {/* The table view: a chart must not be the only way to read the numbers,
              and this is also the accessible fallback. */}
          <Card>
            <SectionHeading>{t("usageBreakdown")}</SectionHeading>
            <UsageTable
              series={data?.series ?? []}
              granularity={range.granularity}
              currency={currency}
              showSpend={canReadFinance}
            />
          </Card>

          {/* The two notes about how these numbers are defined sit below the data
              rather than in the header: they answer "why does this number look
              wrong", and a reader only needs them after seeing the number and
              becoming suspicious. The roll-up-lag note in particular belongs here,
              because this is the page where someone is most likely to notice that
              the request they just sent is missing. */}
          <p className="text-base text-kumo-subtle">
            {t("keyctlRollupLag")} {t("usageTimezoneNote", { tz: browserTZ() || "UTC" })}
          </p>
        </>
      )}
    </div>
  );
}

function UsageTable({
  series,
  granularity,
  currency,
  showSpend,
}: {
  series: GatewayConsoleTypes.UsagePoint[];
  granularity: "day" | "hour";
  currency: string;
  showSpend: boolean;
}) {
  // Bucket labels go through the locale-aware formatters. Assembling them by hand
  // from the month and day fields produces the one date on the whole site that
  // renders the same regardless of the reader's language.
  const { formatDate, formatDateTime, formatNumber, t } = useI18n();
  const bucketLabel = (iso: string) =>
    granularity === "hour" ? formatDateTime(iso) : formatDate(iso);
  if (series.length === 0)
    return <InlineEmpty title={t("usageEmpty")} description={t("usageEmptyHint")} />;
  return (
    <DataTable caption={t("usageBreakdown")}>
      <DataTable.Header>
        <DataTable.Row>
          <DataTable.Head>{t("usageColTime")}</DataTable.Head>
          <DataTable.Head className="text-right">{t("usageColRequests")}</DataTable.Head>
          <DataTable.Head className="text-right">{t("usageColTokensIn")}</DataTable.Head>
          <DataTable.Head className="text-right">{t("usageColTokensOut")}</DataTable.Head>
          {showSpend && (
            <DataTable.Head className="text-right">
              {t("usageColSpend", { currency })}
            </DataTable.Head>
          )}
          <DataTable.Head className="text-right">{t("usageColErrors")}</DataTable.Head>
        </DataTable.Row>
      </DataTable.Header>
      <DataTable.Body>
        {series.map((p) => (
          <DataTable.Row key={p.bucket_start}>
            <DataTable.Cell className="whitespace-nowrap">
              {bucketLabel(p.bucket_start)}
            </DataTable.Cell>
            {/* Tabular figures so the digits line up down the column. */}
            <DataTable.Cell className="text-right tabular-nums">
              {formatNumber(p.requests)}
            </DataTable.Cell>
            <DataTable.Cell className="text-right tabular-nums">
              {formatNumber(p.tokens_in)}
            </DataTable.Cell>
            <DataTable.Cell className="text-right tabular-nums">
              {formatNumber(p.tokens_out)}
            </DataTable.Cell>
            {showSpend && (
              <DataTable.Cell className="text-right tabular-nums">
                {formatNano(p.charged_nano)}
              </DataTable.Cell>
            )}
            <DataTable.Cell className="text-right tabular-nums">
              {formatNumber(p.errors ?? 0)}
            </DataTable.Cell>
          </DataTable.Row>
        ))}
      </DataTable.Body>
    </DataTable>
  );
}
