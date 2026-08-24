import type { MessageKey } from "@fairlb/i18n";

/**
 * Turns a multiplier in basis points into something a person can read.
 *
 * A pricing plan is a way of putting customers into price groups — one group at list
 * price, another at twenty percent off. That is exactly what the data models: a plan
 * default multiplier, with per-model exceptions that **replace** it rather than
 * compound with it. What the interface must not do is show that model as
 * `× 0.8000`.
 *
 * **Languages do not agree on how to say it.** Some express a discount as the
 * fraction of the price still paid, in tenths, where others say the percentage taken
 * off. So this returns a message key and both parameters and lets each translation
 * pick: the `tenths` form or the `percent` form. Forcing one idiom through a literal
 * translation reads wrong in the other language.
 */
export interface AdjustmentLabel {
  key: MessageKey;
  params: { percent: string; tenths: string };
}

/** Strips trailing zeros: `8.0` becomes `8`, `8.50` becomes `8.5`. A whole number
 * should not be shown with a pointless decimal. */
function trim(n: number): string {
  return String(Number(n.toFixed(2)));
}

export function adjustmentLabel(bps: number | null | undefined): AdjustmentLabel | null {
  if (bps == null) return null;
  if (bps === 10_000) {
    return { key: "gwMultiplierOriginal", params: { percent: "0", tenths: "10" } };
  }
  // Discount: 8000 is twenty percent off, or eight tenths of the price. Computed
  // against 10000, which is what a basis-point multiplier is defined against.
  if (bps < 10_000) {
    return {
      key: "gwMultiplierDiscount",
      params: { percent: trim((10_000 - bps) / 100), tenths: trim(bps / 1_000) },
    };
  }
  // Markup: 12000 is twenty percent on top. No language uses the tenths idiom for a
  // markup, so both parameters are still supplied and the wording stays consistent.
  return {
    key: "gwMultiplierMarkup",
    params: { percent: trim((bps - 10_000) / 100), tenths: trim(bps / 1_000) },
  };
}
