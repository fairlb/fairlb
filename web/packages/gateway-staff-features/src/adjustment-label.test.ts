import { describe, expect, it } from "vitest";
import { adjustmentLabel } from "./adjustment-label";

describe("adjustmentLabel", () => {
  // "Group A at list price, group B at twenty percent off" lands on these two.
  it("says original price at 10000 and 8 tenths at 8000", () => {
    expect(adjustmentLabel(10_000)?.key).toBe("gwMultiplierOriginal");
    const b = adjustmentLabel(8_000);
    expect(b?.key).toBe("gwMultiplierDiscount");
    expect(b?.params.tenths).toBe("8"); // for the tenths idiom
    expect(b?.params.percent).toBe("20"); // for "20% off"
  });

  it("says markup above 10000", () => {
    const m = adjustmentLabel(12_000);
    expect(m?.key).toBe("gwMultiplierMarkup");
    expect(m?.params.percent).toBe("20");
  });

  // Half steps have to survive: 9500 is 9.5 tenths, neither 9 nor 10.
  it("keeps a half step readable", () => {
    expect(adjustmentLabel(9_500)?.params.tenths).toBe("9.5");
    expect(adjustmentLabel(9_500)?.params.percent).toBe("5");
  });

  // A whole number must not carry a decimal point; the trailing zero is noise.
  it("does not render a trailing zero", () => {
    expect(adjustmentLabel(8_000)?.params.tenths).not.toContain(".");
    expect(adjustmentLabel(12_000)?.params.percent).not.toContain(".");
  });

  // Null when there is nothing to report, rather than inventing "list price" —
  // that would be a false statement.
  it("returns null when there is no multiplier", () => {
    expect(adjustmentLabel(null)).toBeNull();
    expect(adjustmentLabel(undefined)).toBeNull();
  });

  // The bounds: 1 and 100000 are the two ends the database allows, and neither may
  // break the arithmetic.
  it("survives the allowed multiplier bounds", () => {
    expect(adjustmentLabel(1)?.key).toBe("gwMultiplierDiscount");
    expect(adjustmentLabel(100_000)?.key).toBe("gwMultiplierMarkup");
    expect(adjustmentLabel(100_000)?.params.percent).toBe("900");
  });
});
