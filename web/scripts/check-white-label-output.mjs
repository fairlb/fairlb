#!/usr/bin/env node
/** Verify the final browser artifact, not merely the profile loader. */

import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";

const [distArgument, surface] = process.argv.slice(2);
const surfaces = new Set(["marketing", "console", "operations", "communityAdmin"]);
if (!distArgument || !surfaces.has(surface)) {
  throw new Error(
    "usage: check-white-label-output.mjs <dist> marketing|console|operations|communityAdmin",
  );
}
const dist = path.resolve(distArgument);
const profilePath = process.env.BRAND_PROFILE_PATH;
const profile = profilePath
  ? JSON.parse(readFileSync(path.resolve(profilePath), "utf8"))
  : {
      identity: {
        name: "FairLB",
        surfaceNames: {
          marketing: { en: "FairLB", zh: "FairLB" },
          console: { en: "FairLB Console", zh: "FairLB 控制台" },
          operations: { en: "FairLB Operations", zh: "FairLB 运营后台" },
          communityAdmin: { en: "FairLB Admin", zh: "FairLB 管理台" },
        },
      },
      operator: { legalName: "FairLB" },
      theme: {
        light: {
          canvas: "#F5F7FA",
          surface: "#FFFFFF",
          ink: "#141821",
          accent: "#2457E6",
          healthy: "#0B8F83",
          degraded: "#B86E00",
        },
        dark: {
          canvas: "#0B1018",
          surface: "#131A24",
          ink: "#EEF3F8",
          accent: "#7EA2FF",
          healthy: "#4FD1C5",
          degraded: "#F4B860",
        },
      },
    };

const errors = [];
const requireFile = (relative) => {
  const file = path.join(dist, relative);
  if (!existsSync(file) || !statSync(file).isFile()) errors.push(`missing ${relative}`);
  return file;
};
const index = readFileSync(requireFile("index.html"), "utf8");
const expectedTitle = profile.identity.surfaceNames[surface].en;
if (!index.includes(`<title>${expectedTitle}</title>`) && !index.includes(expectedTitle)) {
  errors.push(`index title does not contain ${expectedTitle}`);
}
for (const asset of [
  "brand/profile.css",
  "brand/mark.svg",
  "brand/wordmark.svg",
  "brand/favicon.svg",
  "brand/social-mark.svg",
]) {
  requireFile(asset);
}
if (!index.includes("/brand/favicon.svg")) errors.push("index does not use the profile favicon");
if (!index.includes("/site.webmanifest")) errors.push("index does not link the profile manifest");
const webManifest = JSON.parse(readFileSync(requireFile("site.webmanifest"), "utf8"));
if (webManifest.name !== expectedTitle) errors.push("web manifest name drift");
if (webManifest.icons?.[0]?.src !== "/brand/favicon.svg") {
  errors.push("web manifest does not use the profile favicon");
}

const css = readFileSync(requireFile("brand/profile.css"), "utf8");
for (const mode of ["light", "dark"]) {
  for (const color of Object.values(profile.theme[mode])) {
    if (!css.toUpperCase().includes(String(color).toUpperCase())) {
      errors.push(`profile.css does not contain theme.${mode} ${color}`);
    }
  }
}

const textExtensions = new Set([".css", ".html", ".js", ".json", ".svg", ".txt", ".xml"]);
const textFiles = [];
const walk = (directory) => {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const file = path.join(directory, entry.name);
    if (entry.isDirectory()) walk(file);
    else if (textExtensions.has(path.extname(entry.name))) textFiles.push(file);
  }
};
walk(dist);

if (profile.identity.name !== "FairLB") {
  const forbiddenText = ["FairLB"];
  const forbiddenColors = [
    "#F5F7FA",
    "#141821",
    "#2457E6",
    "#1946C7",
    "#0B8F83",
    "#B86E00",
    "#0B1018",
    "#131A24",
    "#EEF3F8",
    "#7EA2FF",
    "#9BB7FF",
    "#4FD1C5",
    "#F4B860",
  ];
  for (const file of textFiles) {
    const body = readFileSync(file, "utf8");
    for (const value of forbiddenText) {
      if (body.includes(value)) errors.push(`${path.relative(dist, file)} leaks ${value}`);
    }
    const normalized = body.toUpperCase();
    for (const value of forbiddenColors) {
      if (normalized.includes(value)) errors.push(`${path.relative(dist, file)} leaks ${value}`);
    }
  }
  if (!textFiles.some((file) => readFileSync(file, "utf8").includes(expectedTitle))) {
    errors.push(`artifact never renders configured surface name ${expectedTitle}`);
  }
}

for (const obsolete of [
  "favicon.svg",
  "favicon.ico",
  "apple-touch-icon.png",
  "icon-192.png",
  "icon-512.png",
  "icon-maskable-512.png",
]) {
  if (existsSync(path.join(dist, obsolete)))
    errors.push(`obsolete static asset remains: ${obsolete}`);
}

if (surface === "marketing") {
  const manifest = JSON.parse(readFileSync(requireFile("marketing-manifest.json"), "utf8"));
  if (manifest.identity?.name !== profile.identity.name)
    errors.push("marketing manifest identity drift");
  if (manifest.operator?.legalName !== profile.operator.legalName) errors.push("operator drift");
  for (const legal of ["legal/terms/index.html", "zh/legal/terms/index.html"]) {
    if (!readFileSync(requireFile(legal), "utf8").includes(profile.operator.legalName)) {
      errors.push(`${legal} does not render the configured operator`);
    }
  }
  for (const image of ["og-cover.png", "og-cover-zh.png", "og/models-en.png", "og/models-zh.png"]) {
    const signature = readFileSync(requireFile(image)).subarray(0, 4).toString("hex");
    if (signature !== "89504e47") errors.push(`${image} is not a generated PNG`);
  }
  for (const file of textFiles.filter((candidate) => candidate.endsWith(".html"))) {
    const body = readFileSync(file, "utf8");
    if (/\{(?:brand|operator|supportEmail)\}/.test(body)) {
      errors.push(`${path.relative(dist, file)} contains an unresolved content variable`);
    }
  }
}

if (errors.length) {
  errors.forEach((error) => console.error(`✗ ${surface}: ${error}`));
  process.exitCode = 1;
} else {
  console.log(`✔ ${surface} brand artifact: identity, theme, icons, titles and leakage checks`);
}
