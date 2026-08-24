import { describe, expect, it } from "vitest";
import { contrastRatio, relativeLuminance, rgbFromHex } from "./contrast";

/**
 * The instrument before anything that leans on it.
 *
 * Every expected value here is a published WCAG figure, not an output of this
 * module. That distinction is the whole point: a contrast function with a
 * transposed coefficient returns plausible ratios for every input, so a suite
 * that compared this code against itself would agree with a wrong answer.
 */
describe("contrastRatio", () => {
  it("reproduces the published extremes", () => {
    expect(contrastRatio([0, 0, 0], [255, 255, 255])).toBeCloseTo(21, 2);
    expect(contrastRatio([255, 255, 255], [255, 255, 255])).toBeCloseTo(1, 2);
  });

  it("reproduces the AA boundary colour", () => {
    // #767676 on white is the canonical 4.54:1 example: the darkest grey that
    // still passes AA for body text.
    expect(contrastRatio(rgbFromHex("#767676"), rgbFromHex("#FFFFFF"))).toBeCloseTo(4.54, 2);
    expect(contrastRatio(rgbFromHex("#777777"), rgbFromHex("#FFFFFF"))).toBeCloseTo(4.48, 2);
  });

  it("does not depend on argument order", () => {
    const a = rgbFromHex("#2B65EE");
    const b = rgbFromHex("#FFFFFF");
    expect(contrastRatio(a, b)).toBeCloseTo(contrastRatio(b, a), 10);
  });
});

describe("relativeLuminance", () => {
  it("anchors at the published endpoints", () => {
    expect(relativeLuminance([0, 0, 0])).toBeCloseTo(0, 6);
    expect(relativeLuminance([255, 255, 255])).toBeCloseTo(1, 6);
  });

  it("weights green above red above blue, as the coefficients say", () => {
    const red = relativeLuminance([255, 0, 0]);
    const green = relativeLuminance([0, 255, 0]);
    const blue = relativeLuminance([0, 0, 255]);
    expect(green).toBeGreaterThan(red);
    expect(red).toBeGreaterThan(blue);
    // The coefficients themselves, since a transposition would survive the
    // ordering check above.
    expect(green).toBeCloseTo(0.7152, 4);
    expect(red).toBeCloseTo(0.2126, 4);
    expect(blue).toBeCloseTo(0.0722, 4);
  });
});

describe("rgbFromHex", () => {
  it("parses the channels in order", () => {
    expect(rgbFromHex("#846D2A")).toEqual([0x84, 0x6d, 0x2a]);
  });

  it("refuses anything that is not #rrggbb", () => {
    // Returning a default here would hand every caller a colour they never
    // named, and the contrast assertions downstream would certify it.
    expect(() => rgbFromHex("#fff")).toThrow();
    expect(() => rgbFromHex("rgb(1,2,3)")).toThrow();
  });
});
