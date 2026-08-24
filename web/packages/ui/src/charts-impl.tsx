import { Chart, ChartPalette, TimeseriesChart } from "@cloudflare/kumo/components/chart";
import { useI18n } from "@fairlb/i18n";
import { useLayoutEffect, useMemo, useState } from "react";
import { axisTimeFormat, integerTickFormat } from "./chart-format";
import { echarts } from "./echarts";
import { InlineEmpty } from "./inline-empty";
import { LoadingState } from "./loading-state";
import { useTheme } from "./theme";

/**
 * The chart layer.
 *
 * Before this was unified, each application had its own approach: one drew SVG
 * by hand with colours from CSS variables, the other used a different charting
 * library with hex values hard-coded in JSX. The same line chart looked
 * different on the two sides and handled dark mode by different mechanisms.
 * Everything now goes through one charting component; light and dark are driven
 * by a single flag, and colours and tooltips belong to the component.
 */

/** useIsDark turns the theme setting into the boolean the charts need, following
 * the system preference when the theme is "system". */
function useIsDark(): boolean {
  const { theme } = useTheme();
  if (theme !== "system") return theme === "dark";
  return typeof window !== "undefined" && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

/**
 * The axis label colour comes from the semantic token rather than two hard-coded
 * hex values; otherwise a theme change would move the whole interface and leave
 * the charts behind.
 *
 * The CSS variable cannot be read directly. Measured, its value is a
 * `light-dark(…, …)` expression, and reading the property yields that
 * unevaluated function text, which a canvas fill style cannot interpret. So the
 * browser is asked to do the evaluation: mount a probe element carrying the
 * token's class and read its computed colour.
 *
 * Read synchronously on the first frame through a lazy initialiser, to avoid a
 * flash of the fallback colour, and read again on a theme change — `data-mode`
 * is written in an effect, so reading during render would see the previous
 * frame's value.
 */
function readSubtleColor(): string {
  if (typeof document === "undefined") return "#71717a";
  const probe = document.createElement("span");
  probe.className = "text-kumo-subtle";
  probe.style.display = "none";
  document.body.appendChild(probe);
  const color = getComputedStyle(probe).color;
  probe.remove();
  return color || "#71717a";
}

function useAxisColor(isDark: boolean): string {
  const [color, setColor] = useState(readSubtleColor);
  useLayoutEffect(() => setColor(readSubtleColor()), [isDark]);
  return color;
}

export type TrendPoint = { at: number; value: number };

/**
 * TrendChart draws a single time series.
 *
 * The charting library lays the time axis out itself, so only millisecond
 * timestamps are needed. The hand-written predecessor required the caller to
 * format times into label strings first, and could only fit the first and last
 * on the axis.
 */
export function TrendChart({
  points,
  name,
  format,
  height = 180,
  loading,
  emptyHint,
  integer,
  ariaDescription,
}: {
  points: TrendPoint[];
  /** Series name; appears in the tooltip. */
  name: string;
  format: (v: number) => string;
  height?: number;
  loading?: boolean;
  emptyHint?: string;
  /** For count series such as sign-ups: label whole numbers only. */
  integer?: boolean;
  /** Defaults to a localised summary of the point count and peak; a caller may
   * supply something more specific. */
  ariaDescription?: string;
}) {
  const { t, locale } = useI18n();
  const isDark = useIsDark();

  // The time axis formatter is passed explicitly rather than falling back to the
  // library's built-in labels; the criterion and the reasoning are in
  // chart-format.ts.
  const axisTime = useMemo(() => axisTimeFormat(locale), [locale]);

  if (loading)
    return (
      <div style={{ minHeight: height }}>
        <LoadingState label={t("loading")} />
      </div>
    );
  if (points.length === 0) return <InlineEmpty title={emptyHint ?? t("vizNoData")} />;

  const peak = points.reduce((a, p) => (p.value > a ? p.value : a), 0);
  const yTick = integer ? integerTickFormat(format) : format;

  return (
    <TimeseriesChart
      echarts={echarts}
      height={height}
      isDarkMode={isDark}
      gradient
      data={[
        {
          name,
          // A single series takes the first categorical colour: the palette is
          // owned by the design system rather than hard-coded per page.
          color: ChartPalette.categorical(0, isDark),
          data: points.map((p) => [p.at, p.value] as [number, number]),
        },
      ]}
      xAxisTickFormat={axisTime}
      yAxisTickFormat={yTick}
      tooltipValueFormat={format}
      tooltipFollowCursor="x"
      ariaDescription={
        ariaDescription ?? t("vizTrendAria", { name, count: points.length, peak: format(peak) })
      }
    />
  );
}

export type RankItem = { label: string; value: number };

/**
 * RankBars draws a magnitude ranking as horizontal bars, which suits long names.
 *
 * The time series component only understands a time axis and a ranking needs a
 * category axis, so this configures the lower-level chart directly. Labels sit
 * on the axis rather than inside the bars: a short bar would always clip them.
 */
export function RankBars({
  items,
  format,
  height,
  loading,
  emptyHint,
  name,
  ariaDescription,
}: {
  items: RankItem[];
  format: (v: number) => string;
  height?: number;
  loading?: boolean;
  emptyHint?: string;
  /** What is being ranked; becomes the accessible name of both the chart and its
   * text alternative. */
  name: string;
  ariaDescription?: string;
}) {
  const { t } = useI18n();
  const isDark = useIsDark();
  const axis = useAxisColor(isDark);

  // The lower-level chart has no loading skeleton of its own — that belongs to
  // the time series component — so the placeholder is provided here.
  if (loading)
    return (
      <div style={{ minHeight: height ?? 180 }}>
        <LoadingState label={t("loading")} />
      </div>
    );
  if (items.length === 0) return <InlineEmpty title={emptyHint ?? t("vizNoData")} />;

  // A category axis reads bottom-up, which is the wrong way round for a ranking,
  // so the data is reversed on its way in.
  const rows = [...items].reverse();
  const rankDescription = ariaDescription ?? t("vizRankAria", { name, count: items.length });

  return (
    <figure>
      <Chart
        echarts={echarts}
        height={height ?? Math.max(120, rows.length * 34 + 24)}
        isDarkMode={isDark}
        options={{
          aria: { enabled: true, description: rankDescription },
          grid: {
            left: 8,
            right: 72,
            top: 8,
            bottom: 8,
            outerBoundsMode: "same",
            outerBoundsContain: "axisLabel",
          },
          xAxis: { type: "value", show: false },
          yAxis: {
            type: "category",
            data: rows.map((r) => r.label),
            axisLine: { show: false },
            axisTick: { show: false },
            axisLabel: { color: axis, fontFamily: "ui-monospace, monospace", fontSize: 11 },
          },
          tooltip: { trigger: "item", valueFormatter: (v: unknown) => format(Number(v)) },
          series: [
            {
              type: "bar" as const,
              name,
              color: ChartPalette.categorical(0, isDark),
              data: rows.map((r) => r.value),
              barMaxWidth: 14,
              itemStyle: { borderRadius: [0, 3, 3, 0] },
              label: {
                show: true,
                position: "right",
                formatter: (p: { value?: unknown }) => format(Number(p.value ?? 0)),
                color: axis,
                fontSize: 11,
              },
            },
          ],
        }}
      />
      <figcaption className="sr-only">{rankDescription}</figcaption>
      <ol className="sr-only" aria-label={t("vizRankListLabel", { name })}>
        {items.map((item, index) => (
          <li key={`${item.label}:${index}`}>{`${item.label}: ${format(item.value)}`}</li>
        ))}
      </ol>
    </figure>
  );
}
