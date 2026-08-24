import { useI18n } from "@fairlb/i18n";
import { lazy, Suspense, type ComponentProps } from "react";
import type { RankBars as RankBarsImpl, TrendChart as TrendChartImpl } from "./charts-impl";
import { LoadingState } from "./loading-state";

/**
 * Charts are loaded on demand.
 *
 * Measured: bundling the charting library and the chart components into the main
 * entry took the console's JavaScript from 253 kB gzipped to 456 kB — a 203 kB
 * increase, close to double. Charts appear on two or three pages and always
 * below the fold, so making everyone who opens the sign-in page download 200 kB
 * first is not defensible. Splitting them into their own chunk returns the main
 * entry to its previous size and fetches the rest on arrival at a page with a
 * chart.
 *
 * The cost is one extra round trip the first time such a page opens, covered by
 * the loader — which is the loading state the chart already had, so no new shape
 * is introduced.
 *
 * StatTile is not here: it is a plain block of numbers with no charting
 * dependency, so it is exported statically.
 */
const Impl = {
  Trend: lazy(() => import("./charts-impl").then((m) => ({ default: m.TrendChart }))),
  Rank: lazy(() => import("./charts-impl").then((m) => ({ default: m.RankBars }))),
};

function ChartFallback({ height }: { height?: number }) {
  const { t } = useI18n();
  return (
    <div style={{ minHeight: height ?? 180 }}>
      <LoadingState label={t("loading")} />
    </div>
  );
}

export function TrendChart(props: ComponentProps<typeof TrendChartImpl>) {
  return (
    <Suspense fallback={<ChartFallback height={props.height} />}>
      <Impl.Trend {...props} />
    </Suspense>
  );
}

export function RankBars(props: ComponentProps<typeof RankBarsImpl>) {
  return (
    <Suspense fallback={<ChartFallback height={props.height} />}>
      <Impl.Rank {...props} />
    </Suspense>
  );
}

export { StatTile, type StatDelta } from "./stat-tile";
// The two pure tick formatters: they are criteria, so they must be testable on
// their own.
export { axisTimeFormat, integerTickFormat } from "./chart-format";
export type { RankItem, TrendPoint } from "./charts-impl";
