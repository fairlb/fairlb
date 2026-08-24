import { localeTag, type Locale } from "@fairlb/i18n";

/**
 * Two pure functions for chart ticks.
 *
 * They live in their own module for two reasons: they are criteria, so they have
 * to be testable on their own; and nothing here depends on the charting library,
 * so this module is not dragged into the lazily loaded chart chunk.
 */

/**
 * axisTimeFormat builds the tick formatter for a time axis.
 *
 * It must be passed to the chart explicitly rather than letting the chart fall
 * back to the charting library's built-in time-axis labels. That built-in set
 * takes its language from `document.documentElement.lang` as it was at the
 * moment the module was evaluated — a module-level constant that is never
 * updated afterwards. In this application `lang` is written by the locale
 * provider inside an effect, and the chart chunk is loaded lazily, so opening a
 * dashboard under one language and then switching leaves the old language on the
 * axis until a full reload.
 *
 * The month is printed only on the tick that falls on a month boundary; every
 * other tick shows just the day number. This mirrors the built-in time axis's
 * own layering, and copying it is deliberate: putting the month on every tick
 * across a 30-day window makes the labels long enough to collide, and the first
 * two ticks of a month visibly run together into a single unreadable run of
 * text. Day numbers across a month boundary are anchored by that one labelled
 * tick, and the full date is available in the tooltip.
 *
 * It is deliberately not a purely numeric form such as `8/4`: that renders
 * identically in every language, which makes "does the axis follow the
 * application's language" unobservable, and in turn would silently reduce the
 * assertion that no CJK appears under en to one that can never fail. A criterion
 * has to be falsifiable to keep a regression out.
 */
export function axisTimeFormat(locale: Locale): (at: number) => string {
  const tag = localeTag(locale);
  const monthFmt = new Intl.DateTimeFormat(tag, { month: "short" });
  const dayFmt = new Intl.DateTimeFormat(tag, { day: "numeric" });
  return (at: number) => {
    const d = new Date(at);
    return d.getDate() === 1 ? monthFmt.format(d) : dayFmt.format(d);
  };
}

/**
 * integerTickFormat keeps a count series' axis to whole numbers.
 *
 * When the peak value is 1, the default tick algorithm cuts five steps and the
 * axis reads 0.2 / 0.4 / 0.6 / 0.8 — "0.4 sign-ups". The chart component does
 * not expose a minimum interval, so the suppression happens at the label layer
 * instead: the grid lines stay, so no spacing information is lost, but the
 * numbers no longer lie.
 */
export function integerTickFormat(format: (v: number) => string): (v: number) => string {
  return (v: number) => (Number.isInteger(v) ? format(v) : "");
}
