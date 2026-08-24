export const DECIMAL_RATE = /^(0|[1-9][0-9]*)(\.[0-9]{1,9})?$/;

export function decimalToNano(value: string): bigint | null {
  if (!DECIMAL_RATE.test(value)) return null;
  const [whole, fraction = ""] = value.split(".");
  return BigInt(whole ?? "0") * 1_000_000_000n + BigInt(fraction.padEnd(9, "0"));
}

function nanoToDecimal(value: bigint): string {
  const whole = value / 1_000_000_000n;
  const fraction = (value % 1_000_000_000n).toString().padStart(9, "0").replace(/0+$/, "");
  return fraction ? `${whole}.${fraction}` : whole.toString();
}

/** Formula preview stays integer-only: decimal USD/M -> nano -> bps -> decimal. */
export function multiplyRate(value: string | null | undefined, multiplierBps: number): string {
  if (value == null || value === "") return "—";
  const nano = decimalToNano(value);
  if (nano == null) return "—";
  return nanoToDecimal((nano * BigInt(multiplierBps) + 9_999n) / 10_000n);
}

export function effectivePlanMultiplier(
  modelId: string,
  defaultMultiplierBps: number,
  overrides: { model_id: string; adjustment: { multiplier_bps: number } }[],
): number {
  return (
    overrides.find((override) => override.model_id === modelId)?.adjustment.multiplier_bps ??
    defaultMultiplierBps
  );
}

/**
 * The "mode plus percentage" representation of a selling multiplier, shared by model
 * pricing and the create dialog.
 *
 * Basis points are the storage form; the interface never collects them directly, it
 * collects list price / discount / markup plus a percentage. Written twice, "8.5%
 * off" would be accepted in one place and rejected in the other — while both write
 * to the same column.
 */
export type AdjustmentMode = "original" | "discount" | "markup";

export function adjustmentFromBps(bps: number): { mode: AdjustmentMode; percent: string } {
  if (bps === 10_000) return { mode: "original", percent: "0" };
  const hundredths = Math.abs(bps - 10_000);
  const whole = Math.floor(hundredths / 100);
  // Trailing zeros are stripped from the **fractional part only**. Stripping them
  // from the assembled string instead — `.replace(/\.00$/, "").replace(/0$/, "")` —
  // lets the second replace eat a digit off a whole number: 8000 basis points became
  // "20.00", then "20", then **"2"**. A model priced at 0.8 opened its pricing form
  // reading "2% discount", and saving it unchanged repriced it to 0.98. The values
  // it hit were exactly the round tens, which are the ones actually used.
  const fraction = String(hundredths % 100)
    .padStart(2, "0")
    .replace(/0+$/, "");
  return {
    mode: bps < 10_000 ? "discount" : "markup",
    percent: fraction ? `${whole}.${fraction}` : String(whole),
  };
}

function parseHundredths(value: string): number | null {
  if (!/^(0|[1-9][0-9]*)(\.[0-9]{1,2})?$/.test(value.trim())) return null;
  const [whole, fraction = ""] = value.trim().split(".");
  const parsed = Number(whole) * 100 + Number(fraction.padEnd(2, "0"));
  return Number.isSafeInteger(parsed) ? parsed : null;
}

export function adjustmentBps(mode: AdjustmentMode, percent: string): number | null {
  if (mode === "original") return 10_000;
  const hundredths = parseHundredths(percent);
  if (hundredths == null) return null;
  const bps = mode === "discount" ? 10_000 - hundredths : 10_000 + hundredths;
  return bps >= 1 && bps <= 100_000 ? bps : null;
}

/**
 * Entering it the other way round: given the provider's own rate and the price you
 * want to publish, recover the multiplier.
 *
 * Operators think "I know what I want to sell this model for", while the form
 * collects a multiplier — which otherwise means reaching for a calculator.
 * **Storage still keeps the provider's rate plus a multiplier**: that rate is the
 * public anchor customers compare against, and pricing relative to it is the point.
 * Writing the published price back as a new base would make "how much cheaper than
 * the provider" permanently uncomputable.
 *
 * The forward direction rounds up, so this rounds to nearest rather than truncating:
 * truncating would turn a price derived at 0.8 back into 7999 basis points, drifting
 * a little on every round trip. `null` means no valid multiplier exists for the pair
 * — notably when the provider's rate is zero, where no published price implies one.
 */
export function multiplierFromPublicRate(
  official: string | null | undefined,
  publicRate: string,
): number | null {
  if (official == null || official === "") return null;
  const officialNano = decimalToNano(official);
  const publicNano = decimalToNano(publicRate);
  if (officialNano == null || publicNano == null || officialNano === 0n) return null;
  const bps = Number((publicNano * 10_000n + officialNano / 2n) / officialNano);
  return bps >= 1 && bps <= 100_000 ? bps : null;
}
