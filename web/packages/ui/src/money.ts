/**
 * Money display and input parsing. It lives in the shared layer because both
 * applications need it, and each having its own copy was the root cause this
 * module was created to remove.
 *
 * This is presentation formatting only. Storage and arithmetic are always int64
 * nano units; nothing here takes part in a monetary calculation, it merely turns
 * nano into a human-readable string in main units.
 *
 * Before it existed, a credits page printed raw nano values — a balance shown as
 * 5000000000 — while an input further down the same page took main units and a
 * threshold input next to it took nano: three units in one card. The original
 * formatter was buried in a usage route, so any other page wanting a general
 * money function had to import it from "the usage page", which made no sense.
 */

/** formatNano converts nano to main units using integer arithmetic, keeping
 * floating-point error out of the presentation layer. */
export function formatNano(nano: number): string {
  // Four decimals, then trailing zeros trimmed. It goes through the fixed form
  // so the rounding rule is written once; both used to cut independently, and a
  // rule copied into two places is a rule that eventually differs between them.
  const fixed = formatNanoFixed(nano, 4);
  const [whole, frac = ""] = fixed.split(".");
  const trimmed = frac.replace(/0+$/, "");
  return trimmed ? `${whole}.${trimmed}` : whole!;
}

/**
 * formatNanoFixed is the fixed-decimal variant.
 *
 * `formatNano` strips trailing zeros — 1.5 rather than 1.5000 — which suits
 * running text. Money columns in a table, however, have to line up, and for
 * small values such as unit prices the trailing zeros are what reveal the
 * precision. Four separate local implementations of `(v/1e9).toFixed(n)` had
 * grown up around this need, two using two decimals and two using four.
 */
export function formatNanoFixed(nano: number, digits = 2): string {
  const neg = nano < 0;
  const abs = Math.abs(nano);
  // Round at the display boundary rather than cut. Cutting is not a smaller
  // rounding error, it is a one-directional one: 1234.56789 became 1234.5678,
  // always understating, and always by the same sign. On the pages that show a
  // balance broken into its parts — the whole point of which is that a reader
  // can add the rows up — the cut rows summed to a different number than the cut
  // total, so the page contradicted itself in the one place it was making an
  // arithmetic claim.
  //
  // Integer arithmetic throughout: `scale` is how many nano one displayed unit
  // is worth, so this never divides into a float and back.
  const scale = 10 ** (9 - digits);
  const units = Math.round(abs / scale);
  const perWhole = 10 ** digits;
  const whole = Math.floor(units / perWhole);
  const frac = String(units % perWhole).padStart(digits, "0");
  const s = digits > 0 ? `${whole}.${frac}` : String(whole);
  return neg ? `-${s}` : s;
}

/**
 * formatMoney shows an amount *with its currency*, and is the default in both
 * applications.
 *
 * An amount is an int64 nano value **plus a currency**: the currency is half of
 * what the amount means, and the presentation layer used to drop it. Every call
 * site assembled the pair by hand — one page had even grown its own local money
 * helper — while of 26 formatting calls on the other side only four had a
 * currency anywhere near them. The same number read as "0 USD" in one
 * application and "0.00" in the other, with 33 bare figures measured in total.
 *
 * `digits` means what it does in `formatNanoFixed`: give a digit count for the
 * fixed form, used where columns must align or where trailing zeros carry the
 * precision, and omit it for the trimmed form used in running text.
 *
 * With no currency it prints the number alone rather than assuming one. Guessing
 * a currency is worse than omitting it, because it asserts something to the
 * reader that nothing supports.
 */
export function formatMoney(
  nano: number,
  currency?: string,
  options?: { digits?: number },
): string {
  const amount =
    options?.digits === undefined ? formatNano(nano) : formatNanoFixed(nano, options.digits);
  return currency ? `${amount} ${currency}` : amount;
}

/** nanoToMain converts nano to a main-unit number, for places such as chart axes
 * that need a number rather than a string. */
export function nanoToMain(nano: number): number {
  return nano / 1_000_000_000;
}

/** mainToNano converts main-unit input to nano by padding the string, never
 * going through floating point. */
export function mainToNano(v: string): number {
  const [w = "0", f = ""] = v.trim().split(".");
  return Number(w) * 1_000_000_000 + Number(f.padEnd(9, "0").slice(0, 9));
}

/**
 * formatNanoInput is the lossless main-unit representation: all nine decimals
 * kept, trailing zeros trimmed.
 *
 * Form pre-fill must use it rather than `formatNano`, which keeps only four
 * decimals. Round-tripping through the shorter form means that opening a panel,
 * changing nothing and pressing save silently alters the price:
 *
 *     3333333333 → formatNano "3.3333" → mainToNano 3333300000   ✗ 33333 lost
 *
 * The division of labour: this function for anything editable, `formatNanoFixed`
 * for read-only tables where alignment matters.
 */
export function formatNanoInput(nano: number): string {
  const s = formatNanoFixed(nano, 9);
  return s.includes(".") ? s.replace(/0+$/, "").replace(/\.$/, "") : s;
}

/**
 * normalizeAmountInput tidies a pasted price by stripping the currency symbol
 * and thousands separators.
 *
 * Prices get copied from an upstream pricing page, where they are written as
 * `$3.00` or `1,250`. Without stripping, those are rejected as "not a number" —
 * but this is a difference in formatting, not a mistake by the person typing.
 *
 * The stripped set includes U+FFE5, the fullwidth yen a CJK keyboard actually
 * produces. It is written as an escape rather than literally so that this file
 * carries no fullwidth character of its own.
 */
export function normalizeAmountInput(v: string): string {
  return v
    .trim()
    .replace(/^[$¥\uFFE5]/, "")
    .replace(/,/g, "")
    .trim();
}

/**
 * Validation for money and numeric input.
 *
 * Two real gaps prompted it:
 * - a pricing form used `Number(x) || 0`, so invalid input silently became 0 and
 *   the model in question became free;
 * - a funds form used `Number(x)`, so "abc" became NaN and was serialised to
 *   null on its way to the server.
 *
 * Schemas rather than hand-written conditionals, so that "is this a usable
 * number" is decided once, with one set of messages, shared by both forms.
 */
import { z } from "zod";

const decimal = z
  .string()
  .trim()
  .refine((v) => v !== "" && Number.isFinite(Number(v)), { message: "notANumber" });

/** amountSchema validates a main-unit amount; with `positive` it additionally
 * requires a value greater than zero, as when granting credit. */
export function amountSchema(opts: { positive?: boolean; nonZero?: boolean } = {}) {
  return decimal.superRefine((v, ctx) => {
    const n = Number(v);
    if (opts.positive && n <= 0) ctx.addIssue({ code: "custom", message: "mustBePositive" });
    if (opts.nonZero && n === 0) ctx.addIssue({ code: "custom", message: "mustBeNonZero" });
  });
}

/** intSchema validates a non-negative integer. Prices are stored in nano and
 * markups in basis points, and neither accepts a fraction. */
export const intSchema = decimal.refine((v) => Number.isInteger(Number(v)) && Number(v) >= 0, {
  message: "mustBeNonNegativeInt",
});

/** validate returns a message key for `t()` to resolve, or undefined when the
 * value passes. */
export function validate(schema: z.ZodType<string>, value: string): string | undefined {
  const r = schema.safeParse(value);
  return r.success ? undefined : r.error.issues[0]?.message;
}
