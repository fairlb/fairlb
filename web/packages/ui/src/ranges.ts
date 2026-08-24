import type { MessageKey } from "@fairlb/i18n";
import { useMemo } from "react";

/**
 * The time range filter, shared by three pages: usage, logs and the overview.
 *
 * Each page used to carry its own copy, differing in both order and default —
 * one listed the shortest range last and defaulted to seven days, another listed
 * it first and defaulted to it. The same dropdown looked different and behaved
 * differently from page to page, so a reader had to find the option again on
 * every page. The order is now uniformly nearest-first.
 *
 * The defaults still differ, deliberately, according to what each page is for:
 * logs answer "what happened to the request I just made" and default to 24
 * hours, while usage and the overview answer "what did this period cost" and
 * default to seven days.
 */
export const RANGES = [
  { key: "24h", label: "range24h", days: 1, hours: 24, granularity: "hour" },
  { key: "7d", label: "range7d", days: 7, hours: 24 * 7, granularity: "day" },
  { key: "30d", label: "range30d", days: 30, hours: 24 * 30, granularity: "day" },
] as const satisfies readonly {
  key: string;
  label: MessageKey;
  days: number;
  hours: number;
  granularity: "day" | "hour";
}[];

export type RangeKey = (typeof RANGES)[number]["key"];

/** pickRange looks a range up by key, falling back when the value is not valid —
 * the range parameter lives in the URL, where anyone can edit it. */
export function pickRange(key: string | undefined, fallback: RangeKey) {
  return RANGES.find((r) => r.key === key) ?? RANGES.find((r) => r.key === fallback)!;
}

const HOUR_MS = 3600 * 1000;

/**
 * quantizedRange snaps both ends of a "last N hours" window to the hour.
 *
 * The reason is that the query cache had never once been hit. Five call sites
 * each built their own millisecond-precision timestamp, and that timestamp went
 * into the query key along with the rest of the parameters — so every mount
 * produced a brand new key:
 *
 *     leave the overview page, come back 1.2 seconds later
 *     ?from=…T02:11:15.839Z&to=…   ← first visit
 *     ?from=…T02:11:17.011Z&to=…   ← on return: a different key, a fresh request
 *
 * The 30-second stale time on those queries was therefore dead code that could
 * never match, and the stale entries piled up until garbage collection. A memo
 * on each page only froze the instant *within* a single render; nothing held it
 * across mounts.
 *
 * Snapped to the hour, every key within the same hour is byte-for-byte
 * identical, which is what finally makes both the client cache and the HTTP
 * layer's validators meaningful: the URL is no longer single-use.
 *
 * `to` is always strictly greater than now. Rollups are bucketed by hour and the
 * filter is `bucket_start < to`, so truncating `to` down to the current hour
 * would exclude the bucket in progress — which reads as "the request I just made
 * is missing from the overview". It therefore always rounds up to the *next*
 * hour, and must not collapse to equality exactly on the hour.
 */
export function quantizedRange(hours: number, now: Date = new Date()) {
  const to = Math.floor(now.getTime() / HOUR_MS) * HOUR_MS + HOUR_MS;
  return {
    from: new Date(to - hours * HOUR_MS).toISOString(),
    to: new Date(to).toISOString(),
  };
}

/**
 * useQuantizedRange is the hook form of `quantizedRange`.
 *
 * Its only dependency is `hours`. On a remount the memo recomputes, but it
 * produces the *same string* — and that is precisely the difference from the
 * per-page `useMemo(() => new Date(), [])` it replaced.
 */
export function useQuantizedRange(hours: number) {
  return useMemo(() => quantizedRange(hours), [hours]);
}

/**
 * previousRange is the equally long window immediately before the current one,
 * used for period-over-period comparison.
 *
 * It shares the same snapping as `quantizedRange`, so changing the range moves
 * both query keys together — there is no way to end up with a new period
 * compared against an old baseline, which is the kind of error nobody spots by
 * looking.
 */
export function previousRange(hours: number, now: Date = new Date()) {
  const cur = quantizedRange(hours, now);
  const from = new Date(new Date(cur.from).getTime() - hours * HOUR_MS);
  return { from: from.toISOString(), to: cur.from };
}

/** usePreviousRange is the hook form of `previousRange`. */
export function usePreviousRange(hours: number) {
  return useMemo(() => previousRange(hours), [hours]);
}
