#!/usr/bin/env node
/** Verify committed brand icons without image libraries or a browser. */

import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC_ROOT = path.resolve(SCRIPT_DIR, "../..");
const REPOSITORY_ROOT = path.dirname(PUBLIC_ROOT);
const BRAND_SOURCE = path.join(PUBLIC_ROOT, "web/packages/brand/src");
const PNG_SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

const COMMON_ICONS = [["apple-touch-icon.png", 180, 180]];

const PWA_ICONS = [
  ["icon-192.png", 192, 192],
  ["icon-512.png", 512, 512],
  ["icon-maskable-512.png", 512, 512],
];

const MANIFEST_ICONS = [
  { src: "/favicon.svg", type: "image/svg+xml", sizes: "any" },
  { src: "/apple-touch-icon.png", type: "image/png", sizes: "180x180" },
  { src: "/icon-192.png", type: "image/png", sizes: "192x192" },
  { src: "/icon-512.png", type: "image/png", sizes: "512x512" },
  {
    src: "/icon-maskable-512.png",
    type: "image/png",
    sizes: "512x512",
    purpose: "maskable",
  },
];

function stripSvgComments(source) {
  return source.replace(/<!--[\s\S]*?-->\s*/g, "");
}

function canonicalMark(source) {
  return `${stripSvgComments(source).trim()}\n`;
}

function parseBrandColors(source) {
  const theme = {};
  for (const mode of ["light", "dark"]) {
    const match = source.match(new RegExp(`${mode}:\\s*\\{([\\s\\S]*?)\\n\\s*\\},`));
    if (!match) throw new Error(`cannot read default theme.${mode} from brand/src/profile.ts`);
    theme[mode] = Object.fromEntries(
      [...match[1].matchAll(/(\w+):\s*"(#[0-9A-Fa-f]{6})"/g)].map((entry) => [entry[1], entry[2]]),
    );
  }
  return {
    canvas: { light: theme.light.canvas, dark: theme.dark.canvas },
    route: { light: theme.light.accent, dark: theme.dark.accent },
  };
}

function pngDimensions(file) {
  const data = readFileSync(file);
  if (data.length < 24 || !data.subarray(0, 8).equals(PNG_SIGNATURE)) {
    throw new Error("not a PNG (signature/IHDR unavailable)");
  }
  if (data.subarray(12, 16).toString("ascii") !== "IHDR") {
    throw new Error("not a PNG (first chunk is not IHDR)");
  }
  return [data.readUInt32BE(16), data.readUInt32BE(20)];
}

function icoSizes(file) {
  const data = readFileSync(file);
  if (data.length < 6 || data.readUInt16LE(0) !== 0 || data.readUInt16LE(2) !== 1) {
    throw new Error("not an ICO (invalid directory header)");
  }
  const count = data.readUInt16LE(4);
  if (count === 0 || data.length < 6 + count * 16) {
    throw new Error("not an ICO (truncated directory)");
  }
  const sizes = [];
  for (let index = 0; index < count; index += 1) {
    const offset = 6 + index * 16;
    const width = data[offset] === 0 ? 256 : data[offset];
    const height = data[offset + 1] === 0 ? 256 : data[offset + 1];
    if (width !== height) throw new Error(`non-square ICO entry ${width}x${height}`);
    sizes.push(width);
  }
  return sizes.sort((left, right) => left - right);
}

function requireFile(file, label, errors) {
  if (existsSync(file)) return true;
  errors.push(`${label} is missing`);
  return false;
}

function checkPng(file, width, height, label, errors) {
  if (!requireFile(file, label, errors)) return;
  try {
    const actual = pngDimensions(file);
    if (actual[0] !== width || actual[1] !== height) {
      errors.push(`${label} is ${actual[0]}x${actual[1]}, expected ${width}x${height}`);
    }
  } catch (error) {
    errors.push(`${label}: ${error.message}`);
  }
}

function checkIco(file, label, errors) {
  if (!requireFile(file, label, errors)) return;
  try {
    const actual = icoSizes(file);
    const expected = [16, 32, 48];
    if (actual.join(",") !== expected.join(",")) {
      errors.push(`${label} entries are ${actual.join("/")}, expected 16/32/48`);
    }
  } catch (error) {
    errors.push(`${label}: ${error.message}`);
  }
}

function readManifest(file, label, errors) {
  if (!requireFile(file, label, errors)) return undefined;
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    errors.push(`${label}: invalid JSON (${error.message})`);
    return undefined;
  }
}

function checkManifest(directory, colors, errors) {
  const manifestPath = path.join(directory, "site.webmanifest");
  const manifest = readManifest(manifestPath, "site.webmanifest", errors);
  if (!manifest) return;

  if (manifest.theme_color !== colors.route.light) {
    errors.push(
      `site.webmanifest theme_color is ${manifest.theme_color}, expected BRAND_COLORS.route.light ${colors.route.light}`,
    );
  }
  if (manifest.background_color !== colors.canvas.dark) {
    errors.push(
      `site.webmanifest background_color is ${manifest.background_color}, expected BRAND_COLORS.canvas.dark ${colors.canvas.dark}`,
    );
  }

  if (!Array.isArray(manifest.icons)) {
    errors.push("site.webmanifest icons is not an array");
    return;
  }
  for (const expected of MANIFEST_ICONS) {
    const declared = manifest.icons.find((icon) => icon.src === expected.src);
    if (!declared) {
      errors.push(`site.webmanifest does not declare ${expected.src}`);
      continue;
    }
    for (const property of ["type", "sizes", "purpose"]) {
      if ((expected[property] ?? undefined) !== (declared[property] ?? undefined)) {
        errors.push(
          `site.webmanifest ${expected.src} ${property} is ${declared[property]}, expected ${expected[property]}`,
        );
      }
    }
  }

  for (const icon of manifest.icons) {
    if (typeof icon.src !== "string" || !icon.src.startsWith("/")) {
      errors.push(`site.webmanifest has an invalid icon src ${icon.src}`);
      continue;
    }
    const file = path.join(directory, icon.src.slice(1));
    if (!requireFile(file, `manifest icon ${icon.src}`, errors)) continue;
    const dimensions = typeof icon.sizes === "string" && icon.sizes.match(/^(\d+)x(\d+)$/);
    if (icon.type === "image/png" && dimensions) {
      checkPng(
        file,
        Number(dimensions[1]),
        Number(dimensions[2]),
        `manifest icon ${icon.src}`,
        errors,
      );
    }
  }
}

function checkApp(app, mark, colors) {
  const errors = [];
  // Derived, never written twice. When both halves were hand-written a typo in
  // the `public/` half left `app` resolving, so the whole per-app scan returned
  // clean instead of loud -- the failure mode this function's first line exists
  // to prevent.
  app = { ...app, directory: app.directory ?? path.join(app.app, "public") };
  // The app itself must exist: a path that stopped resolving because a directory
  // was renamed has to be loud, or this whole check quietly passes on nothing.
  if (!existsSync(app.app)) return [`${app.name}: app directory is missing (${app.app})`];
  // Its `public/` need not. Vite's static directory is optional, and these apps
  // have no static assets left to put in one -- the pre-paint theme script, the
  // last occupant, is emitted by the brand plugin now so that three byte-identical
  // copies cannot drift. Everything below asks "is there anything stale in here",
  // and an absent directory answers that.
  if (!existsSync(app.directory)) return errors;

  if (app.buildOwned) {
    const obsolete = [
      // Emitted by the brand plugin so the three served surfaces cannot drift
      // apart. A copy that comes back wins the build (publicDir is copied after
      // the bundle) and loses in dev (the plugin middleware answers first), so
      // the release image and the developer would disagree about the pre-paint
      // theme with nothing saying so.
      "theme-init.js",
      "favicon.svg",
      "favicon.ico",
      "apple-touch-icon.png",
      "icon-192.png",
      "icon-512.png",
      "icon-maskable-512.png",
      "site.webmanifest",
    ];
    for (const file of obsolete) {
      if (existsSync(path.join(app.directory, file))) {
        errors.push(`${file} duplicates the build-time BrandProfileV1 asset`);
      }
    }
    return errors.map((error) => `${app.name}: ${error}`);
  }

  const favicon = path.join(app.directory, "favicon.svg");
  if (requireFile(favicon, "favicon.svg", errors)) {
    const actual = stripSvgComments(readFileSync(favicon, "utf8"));
    if (actual !== canonicalMark(mark)) {
      errors.push("favicon.svg does not byte-match comment-stripped brand/src/mark.svg");
    }
  }
  checkIco(path.join(app.directory, "favicon.ico"), "favicon.ico", errors);
  for (const [file, width, height] of COMMON_ICONS) {
    checkPng(path.join(app.directory, file), width, height, file, errors);
  }
  if (app.pwa) {
    for (const [file, width, height] of PWA_ICONS) {
      checkPng(path.join(app.directory, file), width, height, file, errors);
    }
    checkManifest(app.directory, colors, errors);
  }
  return errors.map((error) => `${app.name}: ${error}`);
}

function appsForScope(scope) {
  const publicApps = [
    {
      name: "community/staff",
      app: path.join(PUBLIC_ROOT, "web/apps/staff"),
      pwa: false,
      buildOwned: true,
    },
  ];
  const cloudApps = [
    {
      name: "cloud/marketing",
      app: path.join(REPOSITORY_ROOT, "cloud/web/apps/marketing"),
      pwa: false,
      buildOwned: true,
    },
    {
      name: "cloud/console",
      app: path.join(REPOSITORY_ROOT, "cloud/web/apps/console"),
      pwa: false,
      buildOwned: true,
    },
    {
      name: "cloud/staff",
      app: path.join(REPOSITORY_ROOT, "cloud/web/apps/staff"),
      pwa: false,
      buildOwned: true,
    },
  ];
  if (scope === "public") return publicApps;
  if (scope === "cloud") return cloudApps;
  if (scope === "all") return [...publicApps, ...cloudApps];
  throw new Error(`unknown scope ${scope}; expected public, cloud, or all`);
}

function run(scope) {
  const mark = readFileSync(path.join(BRAND_SOURCE, "mark.svg"), "utf8");
  const colors = parseBrandColors(readFileSync(path.join(BRAND_SOURCE, "profile.ts"), "utf8"));
  const apps = appsForScope(scope);
  const errors = apps.flatMap((app) => checkApp(app, mark, colors));
  if (errors.length > 0) {
    for (const error of errors) console.error(`✗ ${error}`);
    process.exitCode = 1;
    return;
  }
  console.log(
    `✔ brand assets: ${apps.length} app(s) use build-time BrandProfileV1 assets with no static duplicates`,
  );
}

function syntheticPng(width, height) {
  const data = Buffer.alloc(24);
  PNG_SIGNATURE.copy(data, 0);
  data.write("IHDR", 12, "ascii");
  data.writeUInt32BE(width, 16);
  data.writeUInt32BE(height, 20);
  return data;
}

function syntheticIco(sizes) {
  const data = Buffer.alloc(6 + sizes.length * 16);
  data.writeUInt16LE(0, 0);
  data.writeUInt16LE(1, 2);
  data.writeUInt16LE(sizes.length, 4);
  sizes.forEach((size, index) => {
    const offset = 6 + index * 16;
    data[offset] = size === 256 ? 0 : size;
    data[offset + 1] = size === 256 ? 0 : size;
  });
  return data;
}

function writeSelfTestFixture(root) {
  mkdirSync(root, { recursive: true });
  const mark = '<svg xmlns="http://www.w3.org/2000/svg"><path d="M1 1 H2" /></svg>\n';
  const colors = {
    route: { light: "#2457E6", dark: "#7EA2FF" },
    canvas: { light: "#F5F7FA", dark: "#0B1018" },
  };
  writeFileSync(path.join(root, "favicon.svg"), mark);
  writeFileSync(path.join(root, "favicon.ico"), syntheticIco([16, 32, 48]));
  writeFileSync(path.join(root, "apple-touch-icon.png"), syntheticPng(180, 180));
  writeFileSync(path.join(root, "icon-192.png"), syntheticPng(192, 192));
  writeFileSync(path.join(root, "icon-512.png"), syntheticPng(512, 512));
  writeFileSync(path.join(root, "icon-maskable-512.png"), syntheticPng(512, 512));
  writeFileSync(
    path.join(root, "site.webmanifest"),
    `${JSON.stringify({
      icons: MANIFEST_ICONS,
      theme_color: colors.route.light,
      background_color: colors.canvas.dark,
    })}\n`,
  );
  return { mark, colors };
}

function selfTest() {
  const temporary = mkdtempSync(path.join(os.tmpdir(), "fairlb-brand-assets-"));
  const fixture = path.join(temporary, "public");
  try {
    const { mark, colors } = writeSelfTestFixture(fixture);
    const app = { name: "fixture", app: temporary, directory: fixture, pwa: true };
    const baseline = checkApp(app, mark, colors);
    if (baseline.length > 0)
      throw new Error(`self-test fixture is invalid: ${baseline.join("; ")}`);

    // The two halves of the directory guard. They are opposites and both have to
    // hold: an absent `public/` is a legitimate state now, an absent app is the
    // path in the descriptor having gone stale, and conflating them is how a
    // renamed directory turns this whole check into a silent pass.
    if (
      checkApp({ ...app, directory: path.join(temporary, "no-such-dir") }, mark, colors).length > 0
    )
      throw new Error("an app with no static assets should pass, not fail");
    if (
      !checkApp({ ...app, app: path.join(temporary, "no-such-app") }, mark, colors).some((e) =>
        /app directory is missing/.test(e),
      )
    )
      throw new Error("a stale app path did not fail");

    const expectFailure = (name, mutate, pattern) => {
      writeSelfTestFixture(fixture);
      mutate();
      const errors = checkApp(app, mark, colors);
      if (!errors.some((error) => pattern.test(error))) {
        throw new Error(
          `${name} negative control did not fail: ${errors.join("; ") || "no errors"}`,
        );
      }
    };

    expectFailure(
      "SVG source",
      () => writeFileSync(path.join(fixture, "favicon.svg"), mark.replace("H2", "H3")),
      /byte-match/,
    );
    expectFailure(
      "PNG IHDR size",
      () => writeFileSync(path.join(fixture, "apple-touch-icon.png"), syntheticPng(179, 180)),
      /179x180, expected 180x180/,
    );
    expectFailure(
      "ICO directory size",
      () => writeFileSync(path.join(fixture, "favicon.ico"), syntheticIco([16, 32])),
      /expected 16\/32\/48/,
    );
    expectFailure(
      "manifest brand token",
      () => {
        const file = path.join(fixture, "site.webmanifest");
        const manifest = JSON.parse(readFileSync(file, "utf8"));
        manifest.theme_color = "#000000";
        writeFileSync(file, `${JSON.stringify(manifest)}\n`);
      },
      /BRAND_COLORS\.route\.light/,
    );
    console.log(
      "✔ brand asset self-test: SVG drift, PNG size, ICO entries, and manifest token all report red",
    );
  } finally {
    rmSync(temporary, { recursive: true, force: true });
  }
}

const args = process.argv.slice(2);
if (args.includes("--self-test")) {
  selfTest();
} else {
  const scopeIndex = args.indexOf("--scope");
  if (scopeIndex < 0 || !args[scopeIndex + 1]) {
    throw new Error("usage: check-brand-assets.mjs --scope public|cloud|all, or --self-test");
  }
  run(args[scopeIndex + 1]);
}
