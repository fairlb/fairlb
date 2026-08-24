import { describe, expect, it } from "vitest";
import {
  BASE_BPS,
  bpsFromModePercent,
  isAdjustmentValid,
  modePercentFromBps,
  PROVIDER_MAX_BPS,
} from "./cost-adjustment";

describe("purchase cost multiplier edit-state conversion", () => {
  it("splitting into edit state and folding back to bps are inverses", () => {
    for (const bps of [1, 6_000, BASE_BPS, 12_500, PROVIDER_MAX_BPS]) {
      const { mode, percent } = modePercentFromBps(bps);
      expect(bpsFromModePercent(mode, percent)).toBe(bps);
    }
  });

  it("reads the base multiplier as original mode, not a 0% discount", () => {
    expect(modePercentFromBps(BASE_BPS)).toEqual({ mode: "original", percent: "0" });
    expect(modePercentFromBps(6_000)).toEqual({ mode: "discount", percent: "40" });
    expect(modePercentFromBps(12_000)).toEqual({ mode: "markup", percent: "20" });
  });

  it("ignores the percent input in original mode, whatever is left over in it", () => {
    expect(bpsFromModePercent("original", "37")).toBe(BASE_BPS);
    expect(bpsFromModePercent("original", "garbage")).toBe(BASE_BPS);
  });

  it("returns NaN for an invalid percentage instead of quietly folding back to a valid multiplier", () => {
    for (const bad of ["", "-1", "1e2", "0.005", "abc", "10%"]) {
      expect(bpsFromModePercent("discount", bad)).toBeNaN();
    }
  });

  it("accepts two decimal places and rejects a third", () => {
    expect(bpsFromModePercent("discount", "12.34")).toBe(BASE_BPS - 1_234);
    expect(bpsFromModePercent("markup", "0.01")).toBe(BASE_BPS + 1);
    expect(bpsFromModePercent("discount", "12.345")).toBeNaN();
  });

  it("guards the range at [1, max]: neither a full discount nor a markup past the cap passes", () => {
    expect(isAdjustmentValid(bpsFromModePercent("discount", "100"), PROVIDER_MAX_BPS)).toBe(false);
    expect(isAdjustmentValid(bpsFromModePercent("discount", "99.99"), PROVIDER_MAX_BPS)).toBe(true);
    expect(isAdjustmentValid(bpsFromModePercent("markup", "900"), PROVIDER_MAX_BPS)).toBe(true);
    expect(isAdjustmentValid(bpsFromModePercent("markup", "900.01"), PROVIDER_MAX_BPS)).toBe(false);
    expect(isAdjustmentValid(Number.NaN, PROVIDER_MAX_BPS)).toBe(false);
  });
});
