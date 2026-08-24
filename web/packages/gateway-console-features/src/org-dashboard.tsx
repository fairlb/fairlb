import { ArrowRightIcon } from "@phosphor-icons/react";
import { gatewayConsoleApi, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Card,
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
  usePreviousRange,
  useQuantizedRange,
} from "@fairlb/ui";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { rankUsageGroups } from "./usage-ranking";

/**
 * Change against the previous period.
 *
 * **A previous period of 0 yields no baseline** rather than "+∞%" or "+100%": the
 * growth rate from 0 to 5 is undefined, and inventing one just makes it look like a
 * measurement. Two zeros are equally baseline-less — "unchanged" and "no data" are
 * not the same statement.
 */
function delta(current: number | undefined, previous: number | undefined, emptyLabel: string) {
  if (current === undefined || previous === undefined || previous === 0) return { emptyLabel };
  return { ratio: (current - previous) / previous, emptyLabel };
}

// The overview panel: spend, requests, tokens, error rate, p50–p95 latency, top
// models and a trend line.
//
// Deliberately a **summary** rather than a second usage page. This one answers "is
// everything all right just now"; changing dimensions, reading the breakdown or
// exporting a CSV is the usage page's job. Doing both jobs in both places only
// makes it unclear which one to open.

export function OrgDashboard({
  orgId,
  canReadFinance,
}: {
  orgId: string;
  canReadFinance: boolean;
}) {
  const { t, formatNumber } = useI18n();
  const navigate = useNavigate();
  // The range lives in the URL like every other filter. Held in component state it
  // snapped back to the default on reload, and "look at this stretch" could not be
  // sent to anyone.
  const search = useSearch({ strict: false }) as { range?: string };
  const rangeKey = search.range ?? "7d";
  const range = pickRange(rangeKey, "7d");
  const setRangeKey = (v: string) =>
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, range: v }),
      replace: true,
    });

  // Quantized to the hour: a millisecond-precision timestamp in the query key means
  // a different key on every mount, and a staleTime that never once gets hit.
  const { from, to } = useQuantizedRange(range.hours);

  const usage = gatewayConsoleApi.useGetUsage(orgId, {
    from,
    to,
    granularity: range.granularity,
    group_by: "model",
  });

  // The previous period, for the comparison. A lone `1.1345 USD` cannot answer "is
  // that a lot?", and without a baseline none of the tiles could. This range is
  // quantized to the hour as well, so this fetch caches like the main one — and
  // because both keys change together, a new period never gets shown against an old
  // baseline.
  const prev = usePreviousRange(range.hours);
  const before = gatewayConsoleApi.useGetUsage(orgId, {
    ...prev,
    granularity: range.granularity,
  });
  const prevTotals = before.data?.totals;

  const d = usage.data;
  const currency = d?.totals.currency ?? "USD";
  const lat = d?.totals.latency;
  const fmtMoney = (n: number) => `${formatNano(n)} ${currency}`;
  const fmtCount = formatNumber;
  const rankedGroups = rankUsageGroups(d?.groups ?? [], canReadFinance ? "finance" : "requests");

  // A row of zeros does read as broken to someone who has just started, but
  // replacing the whole panel with a sentence overcorrects: it erases the tiles and
  // the chart frames too, so a new reader never learns what this page will show
  // them later. **The skeleton stays, with one explanatory line and no call to
  // action**: the zeros get explained where they are, the scaffolding survives, and
  // telling people what to do next stays the job of the one card that owns it —
  // otherwise this line contradicts a step that card has already ticked off.
  const empty = d != null && d.totals.requests === 0;

  return (
    <div className="space-y-4">
      {/* This section needs its own heading. As a bare field between two cards, the
        reader saw "Time range / Last 7 days" with nothing saying what it filtered. */}
      <SectionHeading>{t("usageTitle")}</SectionHeading>
      <FormRow className="sm:grid-cols-[10rem_minmax(0,1fr)]">
        <FormRow.Item>
          <Field label={t("commonTimeRange")}>
            <Select
              value={rangeKey}
              onValueChange={(v) => setRangeKey(v ?? "7d")}
              items={RANGES.map((r) => ({ value: r.key as string, label: t(r.label) }))}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Actions className="sm:justify-end">
          {/* The arrow is an icon, not a character. Baked into the message catalog
            it becomes prose a translator will edit, and it would be the one arrow
            on the site that is not an icon like all the others. */}
          <Link
            to="/orgs/$orgId/usage"
            params={{ orgId }}
            className="inline-flex items-center gap-1 text-base text-kumo-subtle hover:underline"
          >
            {t("dashViewAll")}
            <ArrowRightIcon aria-hidden className="size-4 shrink-0" />
          </Link>
        </FormRow.Actions>
      </FormRow>

      {/* Errors render in place and the range picker stays put. Loading and empty
          are kept distinct: an in-flight request used to render as dashes plus "no
          usage", which is an answer rather than a wait. */}
      {usage.isError ? (
        <Alert>{apiErrorMessage(usage.error)}</Alert>
      ) : usage.isPending ? (
        <LoadingState label={t("loading")} />
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {/* Only the three "how much" tiles carry a comparison. Tokens do not,
              because they move with request count and repeating that says nothing
              new; latency does not, because comparing it means comparing
              distributions, and p95 rising 5% is not the same event as p50 rising
              5%. */}
            {canReadFinance && (
              <StatTile
                label={t("usageSpend")}
                value={d ? fmtMoney(d.totals.charged_nano) : "—"}
                delta={delta(d?.totals.charged_nano, prevTotals?.charged_nano, t("dashNoBaseline"))}
              />
            )}
            <StatTile
              label={t("usageRequests")}
              value={d ? fmtCount(d.totals.requests) : "—"}
              delta={delta(d?.totals.requests, prevTotals?.requests, t("dashNoBaseline"))}
            />
            <StatTile
              label={t("usageTokens")}
              value={d ? fmtCount(d.totals.tokens_in + d.totals.tokens_out) : "—"}
              hint={
                d
                  ? t("usageTokensHint", {
                      in: fmtCount(d.totals.tokens_in),
                      out: fmtCount(d.totals.tokens_out),
                    })
                  : undefined
              }
            />
            <StatTile
              label={t("usageErrors")}
              value={d ? fmtCount(d.totals.errors ?? 0) : "—"}
              // The one tile where up is bad: rising spend means the business is
              // growing, rising errors do not.
              delta={{
                ...delta(d?.totals.errors, prevTotals?.errors, t("dashNoBaseline")),
                higherIsWorse: true,
              }}
              hint={
                d && d.totals.requests > 0
                  ? t("usageErrorRate", {
                      rate: (((d.totals.errors ?? 0) / d.totals.requests) * 100).toFixed(2),
                    })
                  : undefined
              }
            />
            {/* The two latency tiles: with no samples, say so rather than printing
                0 ms. Historical ranges from before latency was recorded look
                exactly like this, and a 0 makes them look impossibly fast. */}
            <StatTile
              label={t("dashLatencyP50")}
              value={lat?.has_samples ? `${lat.p50_ms ?? 0} ms` : "—"}
              hint={
                lat?.has_samples
                  ? t("dashLatencyMean", { ms: lat.mean_ms ?? 0 })
                  : d
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

          {/* The empty state replaces only the two data cards; the tiles stay. The
            title is a short noun phrase and the explanation goes in the description
            slot, because an empty state's title renders at **exactly the same
            weight as the page's own h1** — a whole sentence there makes "no data"
            the heaviest element on the page. The description deliberately does not
            say "go create a key": that belongs to the onboarding card, and saying
            it twice contradicts a step that card has already ticked off. */}
          {empty ? (
            <Card>
              <InlineEmpty title={t("dashNoUsageTitle")} description={t("dashNoUsageHint")} />
            </Card>
          ) : (
            <>
              {/* Card headings are h3 because the section heading above them is an
                h2, and outline levels must not be skipped. `as` changes only the
                outline, never the look, so a skipped level is invisible on screen
                and shows up solely to a screen reader. */}
              <Card className="space-y-3">
                <SectionHeading as="h3">
                  {t(canReadFinance ? "usageSpendTrend" : "usageRequestTrend")}
                </SectionHeading>
                <TrendChart
                  name={t(canReadFinance ? "usageSpend" : "usageRequests")}
                  points={(d?.series ?? []).map((p) => ({
                    at: new Date(p.bucket_start).getTime(),
                    value: canReadFinance ? p.charged_nano : p.requests,
                  }))}
                  format={canReadFinance ? fmtMoney : fmtCount}
                  height={140}
                />
              </Card>

              <Card className="space-y-3">
                <SectionHeading as="h3">{t("dashTopModels")}</SectionHeading>
                {/* Top five only: an overview answers "where is the money going",
                    and the full ranking lives on the usage page. */}
                <RankBars
                  name={t("dashTopModels")}
                  items={rankedGroups.slice(0, 5).map((g) => ({
                    label: g.label || g.key,
                    value: canReadFinance ? g.charged_nano : g.requests,
                  }))}
                  format={canReadFinance ? fmtMoney : fmtCount}
                />
              </Card>
            </>
          )}
        </>
      )}
    </div>
  );
}
