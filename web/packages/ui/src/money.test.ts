import { expect, test } from "vitest";
import { formatMoney, formatNano, formatNanoFixed, formatNanoInput, mainToNano } from "./money";

const NANO = 1_000_000_000;

/**
 * Display rounding.
 *
 * These formatters had cut the fraction rather than rounded it, which is not a
 * smaller error than rounding but a one-directional one — every amount came out
 * low, always. It stayed invisible until two pages started showing a balance
 * broken into the parts that make it up: the rows were cut, the total was cut,
 * and the rows no longer summed to the total, on a card whose only purpose is
 * that a reader can add them.
 */

test("the fraction is rounded, not cut", () => {
  // 1234.56789 — the digit past the display boundary is 9, so it rounds up.
  expect(formatNanoFixed(1_234_567_890_000, 4)).toBe("1234.5679");
  // Half rounds away from zero rather than to even: money readers expect the
  // school rule, and to-even is surprising in a column of figures.
  expect(formatNanoFixed(1_500_000, 2)).toBe("0.00");
  expect(formatNanoFixed(5_000_000, 2)).toBe("0.01");
  expect(formatNanoFixed(-1_234_567_890_000, 4)).toBe("-1234.5679");
});

test("rounding that carries does not lose the carry", () => {
  // 0.99995 at four decimals is 1.0000, not 0.9999 with a stray whole part.
  expect(formatNanoFixed(999_950_000, 4)).toBe("1.0000");
  expect(formatNanoFixed(999_999_999, 4)).toBe("1.0000");
  expect(formatNanoFixed(1_999_950_000, 4)).toBe("2.0000");
});

test("zero digits gives a whole number", () => {
  expect(formatNanoFixed(1_500_000_000, 0)).toBe("2");
  expect(formatNanoFixed(1_400_000_000, 0)).toBe("1");
});

test("the trimmed form and the fixed form round the same way", () => {
  // One rule, one place. They used to cut independently.
  expect(formatNano(1_234_567_890_000)).toBe("1234.5679");
  expect(formatNano(1_500_000_000)).toBe("1.5");
  expect(formatNano(2 * NANO)).toBe("2");
  expect(formatNano(-1_234_567_890_000)).toBe("-1234.5679");
});

test("form pre-fill stays lossless", () => {
  // The editable form keeps all nine decimals: round-tripping through a shorter
  // form means opening a panel, changing nothing and saving alters the price.
  expect(formatNanoInput(3_333_333_333)).toBe("3.333333333");
  expect(mainToNano(formatNanoInput(3_333_333_333))).toBe(3_333_333_333);
});

test("an amount carries its currency, and never invents one", () => {
  expect(formatMoney(1_234_567_890_000, "USD", { digits: 4 })).toBe("1234.5679 USD");
  expect(formatMoney(1_234_567_890_000)).toBe("1234.5679");
});
