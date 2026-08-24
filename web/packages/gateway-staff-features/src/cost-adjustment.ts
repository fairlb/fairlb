/**
 * Converting a cost multiplier between how it is stored and how it is edited.
 *
 * Storage holds one scalar in basis points; an operator works in "list price /
 * discount / markup, plus a percentage". Two screens each writing their own
 * conversion is two conversions that drift — the same 6000 shown as different
 * discounts in two places, both of which look right — so there is one.
 */

/** The multiplier that means "charge the list price"; also the stored default. */
export const BASE_BPS = 10_000;

/** The ceiling on a provider's cost multiplier, matching the database constraint. */
export const PROVIDER_MAX_BPS = 100_000;

export type AdjustmentMode = "original" | "discount" | "markup";

/** Two decimal places only: basis points are integers, so a third digit has nowhere
 * to go, and truncating it silently leaves the operator believing it was saved. */
const PERCENT = /^\d+(\.\d{1,2})?$/;

/** Unpacks a stored multiplier back into the editing form. */
export function modePercentFromBps(bps: number): { mode: AdjustmentMode; percent: string } {
  return {
    mode: bps < BASE_BPS ? "discount" : bps > BASE_BPS ? "markup" : "original",
    percent: String(Math.abs(bps - BASE_BPS) / 100),
  };
}

/**
 * Folds the editing form back into a stored multiplier.
 *
 * An invalid percentage yields NaN, which callers use to disable saving. **Do not**
 * send it: NaN serializes to null in JSON, and a null there means "omitted, keep the
 * current value" — so plainly invalid input would present itself as "saved
 * successfully, nothing changed".
 */
export function bpsFromModePercent(mode: AdjustmentMode, percent: string): number {
  if (mode === "original") return BASE_BPS;
  if (!PERCENT.test(percent)) return Number.NaN;
  const amount = Math.round(Number(percent) * 100);
  return mode === "discount" ? BASE_BPS - amount : BASE_BPS + amount;
}

export function isAdjustmentValid(bps: number, maxBps: number): boolean {
  return Number.isInteger(bps) && bps >= 1 && bps <= maxBps;
}
