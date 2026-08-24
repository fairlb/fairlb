import { gatewayConsoleApi } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { Card, SectionHeading, TrendChart, formatNano, useQuantizedRange } from "@fairlb/ui";
import { Link } from "@tanstack/react-router";

/**
 * The parts of a keys page that are about **gateway traffic** rather than about the
 * keys themselves: usage over the last seven days, a thirty-day spend curve for one
 * key, and (next door, in key-models) the list of models a key may name.
 *
 * They are separate exports rather than markup inside a keys page because they are
 * the only parts of it that read the gateway's usage endpoints. Inlined, they still
 * compiled and still rendered inside a shell that never served those endpoints —
 * the page looked healthy and quietly requested routes that answered 404.
 * **"It builds" and "the feature is still there" are not the same claim**, and only
 * the import makes the difference visible.
 */

/**
 * The usage tile of a keys page's side rail: requests and spend over the last seven
 * days, plus the way through to the usage page.
 *
 * Deliberately a card of its own rather than one card mixing this with whatever
 * else a rail shows: a shell that drops this tile must keep the rest, and mixing
 * them would mean picking fields inside the render — writing the boundary into JSX.
 *
 * Its fetch never blocks the main column: a failure leaves this tile showing `—`,
 * raises no alert, and does not touch the key list. A side rail is reference
 * material, and it going down must not stop the main task.
 */
export function KeyUsageRail({ orgId }: { orgId: string }) {
  const { t, formatNumber } = useI18n();
  // Last seven days, the same range the usage page defaults to, so the two places
  // report the same number under the same definition. Quantized to the hour, which
  // also makes this tile's query key identical to the usage page's seven-day one —
  // they share a single cache entry instead of each issuing its own request.
  const range = useQuantizedRange(7 * 24);
  const usage = gatewayConsoleApi.useGetUsage(orgId, { ...range, granularity: "day" });
  const totals = usage.data?.totals;

  return (
    <Card className="space-y-3">
      <SectionHeading>{t("keyRailUsageTitle")}</SectionHeading>
      <dl className="space-y-2 text-base">
        <div className="flex items-baseline justify-between gap-2">
          <dt className="text-kumo-subtle">{t("keyRailRequests7d")}</dt>
          <dd className="font-medium tabular-nums">
            {totals ? formatNumber(totals.requests) : "—"}
          </dd>
        </div>
        <div className="flex items-baseline justify-between gap-2">
          <dt className="text-kumo-subtle">{t("keyRailSpend7d")}</dt>
          <dd className="font-medium">
            {totals ? `${formatNano(totals.charged_nano)} ${totals.currency}` : "—"}
          </dd>
        </div>
      </dl>
      <Link
        to="/orgs/$orgId/usage"
        params={{ orgId }}
        className="text-base text-kumo-info hover:underline"
      >
        {t("keyRailSeeUsage")}
      </Link>
    </Card>
  );
}

/** The last thirty days of spend for a single key, drawn inside its drawer. */
export function KeySpendCurve({ orgId, keyId }: { orgId: string; keyId: string }) {
  const { t, formatNumber } = useI18n();
  // Quantized to the hour: the drawer remounts on every open, and a millisecond
  // timestamp would put opening the same key twice on the network twice.
  const { from, to } = useQuantizedRange(30 * 24);
  const usage = gatewayConsoleApi.useGetUsage(orgId, {
    from,
    to,
    granularity: "day",
    api_key_id: keyId,
  });
  const series = usage.data?.series ?? [];
  const currency = usage.data?.totals.currency ?? "USD";

  return (
    <div>
      <div className="mb-1 text-base font-medium">
        {t("keyctlSpend30d")}
        {usage.data && (
          <span className="ml-2 text-base font-normal text-kumo-subtle">
            {t("keyctlSpendTotal", {
              amount: formatNano(usage.data.totals.charged_nano),
              currency,
              count: formatNumber(usage.data.totals.requests),
            })}
          </span>
        )}
      </div>
      <TrendChart
        name={t("usageSpend")}
        points={series.map((p) => ({
          at: new Date(p.bucket_start).getTime(),
          value: p.charged_nano,
        }))}
        format={(v) => `${formatNano(v)} ${currency}`}
        height={120}
        emptyHint={t("keyctlNoSpend")}
      />
      {/* Roll-up runs on the hour, so the current hour is not in the numbers yet.
          Without saying so, seeing "0 requests" right after sending one reads as
          broken rather than as pending. */}
      <p className="mt-1 text-base text-kumo-subtle">{t("keyctlRollupLag")}</p>
    </div>
  );
}
