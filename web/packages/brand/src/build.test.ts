import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, test } from "vitest";
import { brandBuild, loadBrandProfile } from "./build";
import { interpolateBrandText } from "./profile";

const fixture = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../fixtures/acme-route/profile.json",
);
const temporary: string[] = [];

function invalidProfile(edit: (profile: Record<string, any>) => void): string {
  const profile = JSON.parse(readFileSync(fixture, "utf8")) as Record<string, any>;
  const base = dirname(fixture);
  for (const key of Object.keys(profile.identity.assets)) {
    profile.identity.assets[key] = resolve(base, profile.identity.assets[key]);
  }
  for (const font of ["display", "body", "mono"]) {
    for (const source of profile.theme.fonts[font].sources) {
      source.path = resolve(base, source.path);
    }
  }
  edit(profile);
  const dir = mkdtempSync(resolve(tmpdir(), "fairlb-brand-"));
  temporary.push(dir);
  const path = resolve(dir, "profile.json");
  writeFileSync(path, JSON.stringify(profile));
  return path;
}

afterEach(() => {
  temporary.splice(0);
});

describe("BrandProfileV1", () => {
  test("loads the complete FairLB and white-label profiles", () => {
    expect(loadBrandProfile().profile.identity.name).toBe("FairLB");
    const acme = loadBrandProfile(fixture);
    expect(acme.profile.identity.name).toBe("Acme Route");
    expect(acme.css).toContain("#5B21B6");
    expect(acme.assets.some((asset) => asset.fileName === "brand/favicon.svg")).toBe(true);
  });

  test.each([
    ["unknown field", (profile: any) => (profile.identity.legacyName = "Acme")],
    ["missing localized field", (profile: any) => delete profile.identity.surfaceNames.console.zh],
    [
      "remote asset",
      (profile: any) => (profile.identity.assets.markSvg = "https://example.com/mark.svg"),
    ],
    ["invalid email", (profile: any) => (profile.operator.supportEmail = "support at acme")],
    ["invalid color", (profile: any) => (profile.theme.light.accent = "purple")],
    ["low contrast", (profile: any) => (profile.theme.light.ink = "#F8F7FC")],
    ["invalid link", (profile: any) => (profile.links.repository = "javascript:alert(1)")],
    [
      "private deployment claim",
      (profile: any) => (profile.marketing.offerings.privateDeployment = true),
    ],
  ])("rejects %s", (_, edit) => {
    expect(() => loadBrandProfile(invalidProfile(edit))).toThrow(/brand profile:/);
  });

  test("rejects unknown and unresolved content variables", () => {
    const profile = loadBrandProfile(fixture).profile;
    expect(interpolateBrandText(profile, "{brand} by {operator}")).toBe(
      "Acme Route by Acme Infrastructure Limited",
    );
    expect(() => interpolateBrandText(profile, "{unknown}")).toThrow(/unknown content variable/);
    expect(() => interpolateBrandText(profile, "{brand")).toThrow(/unresolved content variable/);
  });

  test("rejects a placeholder operator in production marketing builds", () => {
    const previous = process.env.MARKETING_ENV;
    process.env.MARKETING_ENV = "production";
    try {
      const path = invalidProfile((profile) => {
        profile.operator.legalName = "Legal entity not configured";
      });
      expect(() => brandBuild("marketing", path)).toThrow(/real operator/);
    } finally {
      if (previous === undefined) delete process.env.MARKETING_ENV;
      else process.env.MARKETING_ENV = previous;
    }
  });
});
