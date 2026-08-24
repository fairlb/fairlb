import { describe, expect, it } from "vitest";
import { isBlankOrPositive, positiveIntOf } from "./limit-fields";

// Two questions, answered separately: is this acceptable to save, and what does
// it stand for. They differ on exactly one input -- the blank field -- and that
// input is a real setting rather than an omission, so the difference is the
// whole point of having both.
describe("reading a limit out of a text field", () => {
  it("reads a positive integer", () => {
    expect(positiveIntOf("60")).toBe(60);
    expect(positiveIntOf("  60  ")).toBe(60);
  });

  // Blank is "no limit". Returning 0 would be the opposite setting: a ceiling of
  // zero refuses every request, and nothing on screen would say so.
  it("reads blank as no limit rather than as zero", () => {
    expect(positiveIntOf("")).toBeUndefined();
    expect(positiveIntOf("   ")).toBeUndefined();
    expect(isBlankOrPositive("")).toBe(true);
  });

  // parseInt("12abc") is 12, which would save a limit nobody typed.
  it("refuses a number with something after it", () => {
    expect(positiveIntOf("12abc")).toBeUndefined();
    expect(isBlankOrPositive("12abc")).toBe(false);
  });

  it("refuses zero, negatives and fractions", () => {
    for (const bad of ["0", "-1", "1.5"]) {
      expect(positiveIntOf(bad)).toBeUndefined();
      expect(isBlankOrPositive(bad)).toBe(false);
    }
  });
});
