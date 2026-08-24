/**
 * WCAG relative luminance and contrast ratio.
 *
 * One copy, because the failure mode of this arithmetic is that it keeps
 * working. A transposed coefficient or a linearisation applied in the wrong
 * order still returns ratios in a plausible range, so a wrong copy reads as
 * correct and quietly certifies unreadable colour pairs. There were two copies
 * before this file — one in the console's dismiss-button contrast test, one in
 * the avatar palette test — and only one of them checked itself.
 *
 * Both consumers are test suites today. It lives here rather than in either of
 * them because they sit in different workspaces: `cloud/web` can import from
 * `public/web`, and the reverse is not allowed, so this is the only place both
 * can reach. Nothing in the rendered product imports it, so it is dropped from
 * every bundle.
 *
 * Verified against the published values in `contrast.test.ts` — an
 * implementation of this kind has to prove itself before anything relies on it.
 */

/** An opaque colour as 0-255 channels. */
export type Rgb = readonly [number, number, number];

/** Undoes the sRGB transfer function for one channel. */
function linearize(channel: number): number {
  const v = channel / 255;
  return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
}

/** WCAG relative luminance, 0 (black) to 1 (white). */
export function relativeLuminance([r, g, b]: Rgb): number {
  return 0.2126 * linearize(r) + 0.7152 * linearize(g) + 0.0722 * linearize(b);
}

/**
 * WCAG contrast ratio, 1:1 to 21:1. Order-independent: the lighter colour is
 * placed on top by the formula, not by the caller.
 */
export function contrastRatio(a: Rgb, b: Rgb): number {
  const [hi, lo] = [relativeLuminance(a), relativeLuminance(b)].sort((x, y) => y - x) as [
    number,
    number,
  ];
  return (hi + 0.05) / (lo + 0.05);
}

/** Parses `#rrggbb`. Throws rather than returning a colour nobody asked for. */
export function rgbFromHex(hex: string): Rgb {
  const match = /^#([0-9a-f]{6})$/i.exec(hex);
  if (!match?.[1]) throw new Error(`not a #rrggbb colour: ${hex}`);
  const n = Number.parseInt(match[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}
