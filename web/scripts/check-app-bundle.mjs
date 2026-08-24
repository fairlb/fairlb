#!/usr/bin/env node
// First-paint bundle budget for a single-page app, enforced per app.
//
// Route-level lazy loading only keeps paying off while every chunk has a hard
// ceiling; otherwise one accidental static import quietly pulls a lazy route
// back into the first paint. Vite's chunkSizeWarningLimit only warns, so it
// cannot serve as the gate.
//
// The entry chunk used to carry a fourth, tighter budget of its own. It was
// removed (ADR-0158): it is bounded twice already — as a chunk by
// `javascript`, and as the thing fetched before render by `initialJavascript`
// — and what the extra ceiling uniquely measured was *chunk topology*, which
// is not a cost anyone pays. It fired twice for topology shifts that left the
// first paint flat or better, and each time the only available answer was to
// raise it. A ceiling that has to move every time it is reached is not a
// ceiling. The entry size is still printed; it is information, not a gate.
//
// # One budget file per app
//
// Apps differ in shape: one may split routes and heavy libraries into separate
// chunks, another may compile to a single chunk where entry, first paint and
// largest JS are all the same number. So the budget belongs to the app and
// lives in `apps/<app>/bundle-budget.json`, next to the code whose size it
// constrains, together with the measured baseline and the reason behind every
// raise recorded in that file's `_comment`.
//
// Headroom is the measured size plus roughly 2%: a budget is only useful while
// it is tight, and every raise should come with a reason someone can state.
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { gzipSync } from "node:zlib";

const KIB = 1024;

const app = process.argv[2];
if (app === "--self-test") {
  selfTest();
  process.exit(0);
}
// argv[3] is the optional workspace root. It has to be guarded against flags:
// the Community caller passes no workspace and goes straight to `--label`, and
// treating that flag as a path would look for `apps/staff` under a directory
// named `--label`.
const workspaceArg =
  process.argv[3] && !process.argv[3].startsWith("--") ? process.argv[3] : undefined;
const workspace = workspaceArg
  ? pathToFileURL(`${resolve(process.cwd(), workspaceArg)}/`)
  : new URL("../", import.meta.url);

// Both products have an app called `staff`, and this script is shared, so the
// app name alone no longer identifies a build: one `make verify` log prints two
// `staff bundle budget ok` lines with different sizes against different
// budgets. Whoever reads a number here has to be able to tell which artefact it
// describes -- misattributing exactly these figures is a mistake this
// repository has already made once.
//
// The label is passed in rather than derived from the workspace path, because
// the path is not always the product's: `make verify-public` builds inside an
// rsync'd copy under /tmp, where deriving it yields `fairlb-public-verify.XXXX`
// -- a name that changes every run and says nothing. The caller is the only one
// that reliably knows which product it is.
const labelFlag = process.argv.indexOf("--label");
const label = labelFlag === -1 ? app : process.argv[labelFlag + 1];
if (labelFlag !== -1 && !label) {
  console.error("✗ --label was given without a value");
  process.exit(1);
}
// Budgets live in each app's own `bundle-budget.json` rather than in a table
// here: whoever changes an app should see its budget in the directory they are
// already working in. A missing file fails: forgetting to set a budget for a
// new app has to be red, not a silently unguarded build.
let budgets;
try {
  const raw = readFileSync(new URL(`apps/${app}/bundle-budget.json`, workspace), "utf8");
  budgets = Object.fromEntries(Object.entries(JSON.parse(raw).kib).map(([k, v]) => [k, v * KIB]));
} catch (err) {
  console.error(
    `✗ cannot read apps/${app}/bundle-budget.json — a new app has to declare a budget before it can be checked\n  ${err.message}`,
  );
  process.exit(1);
}
for (const k of ["initialJavascript", "javascript", "css"]) {
  if (typeof budgets[k] !== "number") {
    console.error(
      `✗ apps/${app}/bundle-budget.json is missing kib.${k} — one missing budget leaves that dimension unguarded`,
    );
    process.exit(1);
  }
}

// Before any number is printed: is this build even measuring one copy of each
// library?
//
// This repository used to have two pnpm workspaces both listing
// `public/web/packages/*` as members, so whichever installed last owned those
// packages' node_modules and the other workspace's build resolved the shared
// libraries through two stores at once. A bundler keys modules by resolved
// path, so it emitted *two copies of the same library* and every number here
// described an artefact nobody ships. Measured then: the Community app read
// 300 KiB in its own resolution and 358 KiB in the split one; Cloud read 364
// and 474.
//
// That cause is gone -- there is one workspace root now (ADR-0151), and
// `check-single-web-workspace` holds it. This guard stays as the second one,
// measured at the artefact rather than at the configuration: it would still
// catch a split arriving by some other route, and the two look nothing alike
// from here.
//
// The criterion is deliberately "same name, same version, two paths". Two
// *versions* of a package coexisting is a legitimate thing pnpm does and a cost
// somebody chose; the same version in two places cannot be chosen and cannot be
// right.
//
// It refuses rather than reports, because the failure this replaces is a budget
// number that looks like a regression. Someone reading "over budget" reasonably
// goes looking at their own change, or raises the budget -- and the raise then
// stands as a permanent memorial to an install-order accident.
function findSplitResolutions(appDir, root) {
  const seen = new Map(); // "name@version" -> Map(realpath -> [consumer paths])
  const record = (consumer, dir) => {
    const modules = join(dir, "node_modules");
    if (!existsSync(modules)) return;
    const names = [];
    for (const entry of readdirSync(modules)) {
      if (entry.startsWith(".")) continue;
      if (entry.startsWith("@")) {
        const scope = join(modules, entry);
        if (!existsSync(scope)) continue;
        for (const inner of readdirSync(scope)) names.push(`${entry}/${inner}`);
        continue;
      }
      names.push(entry);
    }
    for (const name of names) {
      // The workspace's own packages are linked by design; they are the
      // consumers here, not a dependency that could be duplicated.
      if (name.startsWith("@fairlb/")) continue;
      // Type packages are erased at build time and cannot appear in a bundle.
      // They split exactly as the rest do, but listing them would put items in
      // the report that do not support the claim it makes about size.
      if (name.startsWith("@types/")) continue;
      let real;
      let manifest;
      try {
        real = realpathSync(join(modules, name));
        manifest = JSON.parse(readFileSync(join(real, "package.json"), "utf8"));
      } catch {
        continue; // not a resolvable package: nothing to compare
      }
      if (!manifest.version) continue;
      const key = `${name}@${manifest.version}`;
      if (!seen.has(key)) seen.set(key, new Map());
      const places = seen.get(key);
      if (!places.has(real)) places.set(real, []);
      places.get(real).push(consumer);
    }
  };

  // The app plus every workspace package it pulls in, since a duplicate only
  // matters if it actually reaches this bundle.
  const queue = [appDir];
  const visited = new Set();
  while (queue.length > 0) {
    const dir = queue.shift();
    const real = realpathSync(dir);
    if (visited.has(real)) continue;
    visited.add(real);
    record(relative(root, real) || ".", real);
    const scope = join(real, "node_modules", "@fairlb");
    if (!existsSync(scope)) continue;
    for (const entry of readdirSync(scope)) queue.push(join(scope, entry));
  }

  return [...seen.entries()].filter(([, places]) => places.size > 1);
}

function assertSingleResolution(appDir) {
  const split = findSplitResolutions(appDir, fileURLToPath(workspace));
  if (split.length === 0) return;

  console.error(
    `✗ ${label} is built on a split module resolution, so no size here would mean anything.`,
  );
  for (const [key, places] of split.slice(0, 5)) {
    console.error(`  ${key} resolves to ${places.size} different locations:`);
    for (const [real, consumers] of places) {
      console.error(`    ${consumers[0]} → ${shorten(real)}`);
    }
  }
  if (split.length > 5) console.error(`  …and ${split.length - 5} more.`);
  console.error(
    "\n  In this repository that normally means the two pnpm workspaces have been\n" +
      "  fighting over public/web/packages/*: both list them as members, so whichever\n" +
      "  installed last owns their node_modules and the other one's build then takes\n" +
      "  two copies of every shared library.\n" +
      "\n  Re-run `pnpm install` in this workspace to claim them back. For the Community\n" +
      "  numbers, build that tree on its own — deploy/scripts/verify-public-copy.sh,\n" +
      "  which is what `make verify-public` and CI both measure.",
  );
  process.exit(1);
}

// The detector's own three cases, run in the frontend gate.
//
// It fires only in a state CI never reaches -- both jobs install their own
// workspace last -- so nothing else would notice it going quiet. The third case
// is the one worth having: two *versions* of a package is a legitimate thing
// pnpm does, and a check that flagged it would be a check people learn to
// ignore.
function selfTest() {
  const root = mkdtempSync(join(tmpdir(), "bundle-guard-"));
  const pkg = (dir, name, version) => {
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "package.json"), JSON.stringify({ name, version }));
    return dir;
  };
  const link = (from, to) => {
    mkdirSync(dirname(from), { recursive: true });
    symlinkSync(to, from);
  };

  const storeA = pkg(join(root, "store-a", "dep"), "dep", "1.0.0");
  const storeB = pkg(join(root, "store-b", "dep"), "dep", "1.0.0");
  const storeC = pkg(join(root, "store-c", "dep"), "dep", "2.0.0");
  const app = pkg(join(root, "apps", "app"), "app", "0.0.0");
  const shared = pkg(join(root, "packages", "shared"), "@fairlb/shared", "0.0.0");
  link(join(app, "node_modules", "@fairlb", "shared"), shared);
  link(join(app, "node_modules", "dep"), storeA);

  const names = (found) => found.map(([key]) => key).sort();

  // One store: nothing to report.
  link(join(shared, "node_modules", "dep"), storeA);
  let found = findSplitResolutions(app, root);
  if (found.length !== 0) {
    fail(`a single resolution was reported as split: ${names(found).join(", ")}`);
  }

  // Two stores, same version: the state this exists for.
  rmSync(join(shared, "node_modules", "dep"));
  link(join(shared, "node_modules", "dep"), storeB);
  found = findSplitResolutions(app, root);
  if (found.length !== 1 || found[0][0] !== "dep@1.0.0") {
    fail(`a split resolution went unreported: ${JSON.stringify(names(found))}`);
  }

  // Two versions: legitimate, and must stay quiet.
  rmSync(join(shared, "node_modules", "dep"));
  link(join(shared, "node_modules", "dep"), storeC);
  found = findSplitResolutions(app, root);
  if (found.length !== 0) {
    fail(`two versions of a package were reported as a split: ${names(found).join(", ")}`);
  }

  rmSync(root, { recursive: true, force: true });
  console.log("✔ bundle guard self-test ok (single, split and two-version cases)");
}

function fail(message) {
  console.error(`✗ bundle guard self-test: ${message}`);
  process.exit(1);
}

// A store path is long and almost entirely noise. The one thing worth reading
// off it is which workspace's store the copy came from, because that is the
// difference between the two resolutions.
function shorten(real) {
  const marker = real.lastIndexOf("/node_modules/.pnpm/");
  if (marker < 0) return real;
  return `${real.slice(0, marker).split("/").slice(-2).join("/")} store`;
}

assertSingleResolution(fileURLToPath(new URL(`apps/${app}/`, workspace)));

const dist = new URL(`apps/${app}/dist/`, workspace);
const assetsPath = fileURLToPath(new URL("assets/", dist));
const html = readFileSync(new URL("index.html", dist), "utf8");
const entry = html.match(/<script[^>]+src="(?:\.\/|\/)assets\/([^"]+\.js)"/)?.[1];

if (!entry) {
  console.error(`✗ no entry chunk found for ${label}; build it first`);
  process.exit(1);
}

const files = readdirSync(assetsPath).filter((file) => /\.(?:js|css)$/.test(file));
const sizes = files.map((file) => ({
  file,
  gzip: gzipSync(readFileSync(join(assetsPath, file))).byteLength,
}));
const failures = sizes.filter(
  ({ file, gzip }) => gzip > (file.endsWith(".css") ? budgets.css : budgets.javascript),
);

const initialFiles = new Set([entry]);
for (const match of html.matchAll(
  /<link[^>]+rel="modulepreload"[^>]+href="(?:\.\/|\/)assets\/([^"]+\.js)"/g,
)) {
  initialFiles.add(match[1]);
}
const initialJavascriptSize = sizes
  .filter(({ file }) => initialFiles.has(file))
  .reduce((sum, item) => sum + item.gzip, 0);

if (initialJavascriptSize > budgets.initialJavascript) {
  console.error(
    `✗ ${label} first-paint JS (entry + modulepreload): ${(initialJavascriptSize / KIB).toFixed(1)} KiB gzip, over the ${(budgets.initialJavascript / KIB).toFixed(0)} KiB budget`,
  );
  process.exit(1);
}

if (failures.length > 0) {
  for (const item of failures) {
    const limit = item.file.endsWith(".css") ? budgets.css : budgets.javascript;
    console.error(
      `✗ ${label} ${item.file}: ${(item.gzip / KIB).toFixed(1)} KiB gzip, over the ${(limit / KIB).toFixed(0)} KiB budget`,
    );
  }
  process.exit(1);
}

const entrySize = sizes.find((item) => item.file === entry)?.gzip ?? 0;
const largest = sizes
  .filter((item) => item.file.endsWith(".js"))
  .sort((a, b) => b.gzip - a.gzip)[0];
console.log(
  `✔ ${label} bundle budget ok (first-paint JS ${(initialJavascriptSize / KIB).toFixed(1)} KiB, entry ${(entrySize / KIB).toFixed(1)} KiB, largest chunk ${(largest.gzip / KIB).toFixed(1)} KiB gzip)`,
);
