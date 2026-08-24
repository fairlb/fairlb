import { describe, expect, it } from "vitest";
import {
  adjustmentBps,
  adjustmentFromBps,
  decimalToNano,
  effectivePlanMultiplier,
  multiplierFromPublicRate,
  multiplyRate,
} from "./pricing-math";

describe("pricing formula preview", () => {
  it("applies discounts and markups without floating point", () => {
    expect(multiplyRate("3", 8_000)).toBe("2.4");
    expect(multiplyRate("0.075", 12_000)).toBe("0.09");
  });

  it("preserves explicit zero and distinguishes a missing rate", () => {
    expect(multiplyRate("0", 12_000)).toBe("0");
    expect(multiplyRate(null, 12_000)).toBe("—");
  });

  it("ceil-rounds sub-nano multiplication once", () => {
    expect(multiplyRate("0.000000001", 8_000)).toBe("0.000000001");
  });

  it("rejects exponent, negative, and over-precision inputs", () => {
    expect(decimalToNano("1e-3")).toBeNull();
    expect(decimalToNano("-1")).toBeNull();
    expect(decimalToNano("0.0000000001")).toBeNull();
  });

  it("replaces the plan default with a model exception instead of multiplying twice", () => {
    const overrides = [{ model_id: "model-vip", adjustment: { multiplier_bps: 7_500 } }];
    expect(effectivePlanMultiplier("model-vip", 9_000, overrides)).toBe(7_500);
    expect(effectivePlanMultiplier("model-standard", 9_000, overrides)).toBe(9_000);
  });
});

describe("reverse price entry", () => {
  it("recovers the multiplier that produced a public price", () => {
    expect(multiplierFromPublicRate("10", "8")).toBe(8_000);
    expect(multiplierFromPublicRate("10", "12.5")).toBe(12_500);
    expect(multiplierFromPublicRate("3", "3")).toBe(10_000);
  });

  it("round-trips: forward then reverse lands on the same bps", () => {
    // The forward direction rounds up; if the reverse truncated, a round trip would
    // drift by one basis point — 8000 coming back as 7999.
    for (const bps of [8_000, 8_500, 9_999, 12_000, 10_001]) {
      const shown = multiplyRate("7.35", bps);
      expect(multiplierFromPublicRate("7.35", shown), `bps=${bps}`).toBe(bps);
    }
  });

  it("refuses pairs that cannot yield a legal multiplier", () => {
    expect(multiplierFromPublicRate("0", "5")).toBeNull(); // no multiplier implies a free rate
    expect(multiplierFromPublicRate("", "5")).toBeNull();
    expect(multiplierFromPublicRate("10", "")).toBeNull();
    expect(multiplierFromPublicRate("10", "abc")).toBeNull();
    expect(multiplierFromPublicRate("10", "0")).toBeNull(); // 0 is below the allowed range
    expect(multiplierFromPublicRate("1", "11")).toBeNull(); // 11x is above the ceiling
  });
});

describe("adjustment mode <-> bps", () => {
  it("round-trips through the form representation", () => {
    for (const bps of [10_000, 8_000, 9_150, 12_000, 10_025]) {
      const form = adjustmentFromBps(bps);
      expect(adjustmentBps(form.mode, form.percent), `bps=${bps}`).toBe(bps);
    }
  });

  it("rejects out-of-range and over-precision percentages", () => {
    expect(adjustmentBps("discount", "100")).toBeNull(); // free of charge; the multiplier hits 0
    expect(adjustmentBps("markup", "900.001")).toBeNull();
    expect(adjustmentBps("discount", "-5")).toBeNull();
    expect(adjustmentBps("original", "whatever")).toBe(10_000);
  });
});
