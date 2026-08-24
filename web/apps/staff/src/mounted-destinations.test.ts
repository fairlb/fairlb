import { readFileSync } from "node:fs";
import { glob } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import { resolveAdminPage } from "./registry";

/**
 * Every `/gateway/...` link inside a page this app mounts must resolve to a
 * route this app registers.
 *
 * # Why this has to be a test rather than something read off the code
 *
 * The feature package hard-codes destination paths in its own links — a row
 * title's `to`, the exits from a banner, or a focused-page href assembled from a template.
 * This app mounts only part of that package, so "does a mounted page link to an
 * unmounted one" is a fact that can change with every package update.
 *
 * When it is wrong the router renders a 404: nothing errors, no build fails,
 * nothing reaches a log. And mounting different subsets in different apps is
 * the long-term arrangement, not a transitional one — so the criterion belongs
 * in a test.
 *
 * # The scan is scoped by reachability, not by directory
 *
 * Only the mounted pages and the modules they transitively import are scanned.
 * Scanning the whole package would pull in the self-referencing links of pages
 * this app deliberately does not mount, and the criterion would immediately
 * degenerate into a hand-maintained exemption list — the kind of list nobody
 * ever comes back to check.
 *
 * gate-honesty: this file has no skip path. Two self-checks run before the
 *   assertion: failing to reach the entry files, failing to walk the transitive
 *   imports, or extracting no path literal at all each fail the test rather
 *   than reporting "no problems found".
 */

const PACKAGE_SRC = join(
  dirname(fileURLToPath(import.meta.url)),
  "../../../packages/gateway-staff-features/src",
);

/** Which exports of the feature package this app mounts. */
const MOUNTED_EXPORTS = [
  "GatewayProvidersPage",
  "GatewayProviderLayout",
  "GatewayProviderOverviewPage",
  "GatewayProviderModelsPage",
  "GatewayProviderKeysPage",
  "GatewayProviderSettingsPage",
  "GatewayModelsPage",
  "GatewayModelLayout",
  "GatewayModelOverviewPage",
  "GatewayModelRoutesPage",
  "GatewayModelPricingPage",
  "GatewayHealthPage",
  "GatewayTiersPage",
  "GatewayOrgLimitsPage",
  "GatewayKillSwitchBanner",
];

/** Reads the package's `index.ts` into a map of export name to source stem. */
function exportSources(): Map<string, string> {
  const index = readFileSync(join(PACKAGE_SRC, "index.ts"), "utf8");
  const map = new Map<string, string>();
  for (const m of index.matchAll(/export\s*\{([^}]*)\}\s*from\s*"\.\/([\w.-]+)"/g)) {
    for (const raw of m[1]!.split(",")) {
      const name = raw
        .trim()
        .replace(/^type\s+/, "")
        .split(/\s+as\s+/)[0]
        ?.trim();
      if (name) map.set(name, m[2]!);
    }
  }
  return map;
}

/** Resolves a relative import to a real file. Every module in the package is a
 * `.ts` or `.tsx` file; there are no index directories. */
function resolveLocal(fromFile: string, spec: string): string | undefined {
  const base = join(dirname(fromFile), spec);
  for (const ext of [".tsx", ".ts"]) {
    try {
      readFileSync(base + ext, "utf8");
      return base + ext;
    } catch {
      // try the next extension
    }
  }
  return undefined;
}

/** Walks out from the mounted entry points to every local module they reach. */
function reachableFiles(): Map<string, string> {
  const sources = exportSources();
  const seen = new Map<string, string>();
  const queue: string[] = [];
  for (const name of MOUNTED_EXPORTS) {
    const stem = sources.get(name);
    expect(
      stem,
      `the feature package no longer exports ${name}; its public surface changed`,
    ).toBeDefined();
    const file = resolveLocal(join(PACKAGE_SRC, "x"), `./${stem}`);
    expect(file, `cannot resolve a source file for ${stem}`).toBeDefined();
    queue.push(file!);
  }
  while (queue.length > 0) {
    const file = queue.shift()!;
    if (seen.has(file)) continue;
    const src = readFileSync(file, "utf8");
    seen.set(file, src);
    for (const m of src.matchAll(/(?:from|import)\s*"(\.[^"]+)"/g)) {
      const next = resolveLocal(file, m[1]!);
      if (next && !seen.has(next)) queue.push(next);
    }
  }
  return seen;
}

/** Extracts every `/gateway/...` path literal, normalizing a template
 * placeholder to a single path-parameter segment. */
function gatewayPaths(sources: Iterable<string>): Set<string> {
  const found = new Set<string>();
  for (const src of sources) {
    for (const m of src.matchAll(/["'`](\/gateway\/[^"'`\s]*)/g)) {
      const path = m[1]!
        .replace(/\$\{[^}]*\}/g, "param")
        .split(/[?#]/)[0]!
        .replace(/\/$/, "");
      found.add(path);
    }
  }
  return found;
}

test("every gateway link inside a mounted page resolves to a route this app registers", () => {
  const files = reachableFiles();
  const paths = gatewayPaths(files.values());

  // ── Self-checks: a collapsed scan must not look like a pass ────────────
  expect(
    files.size,
    "no source file was scanned at all; entry resolution is broken",
  ).toBeGreaterThanOrEqual(MOUNTED_EXPORTS.length);
  // Proves the transitive walk really left the entry files: the models dialog
  // is reachable only through provider-detail → provider-models.
  expect([...files.keys()].some((f) => f.endsWith("provider-models-dialog.tsx"))).toBe(true);
  expect(
    paths.size,
    "no /gateway path was extracted; the pattern or the literal form changed",
  ).toBeGreaterThanOrEqual(5);
  // A model-detail path assembled as a template has to appear, otherwise the
  // placeholder branch of the extractor never ran.
  expect([...paths].some((p) => p.startsWith("/gateway/models/param"))).toBe(true);

  // ── The criterion itself ───────────────────────────────────────────────
  const unrouted = [...paths].filter((p) => resolveAdminPage(p) === undefined);
  expect(
    unrouted,
    "these paths appear in mounted pages but have no route in registry.ts; " +
      "in the running app they render a 404, with no error and no failing build",
  ).toEqual([]);
});

test("unmounted pages really are outside the scan, or the test above proves nothing", () => {
  const files = reachableFiles();
  // The pricing-plan pages are the ones this app deliberately does not mount:
  // they decide what a customer is charged, and an operator running this for
  // themselves is not charging anyone. If they leaked into the reachable set,
  // the assertion above would fail because their own paths have no route —
  // which would be the criterion telling the truth. This pins the opposite fact
  // directly: they should never have been scanned in the first place.
  const scanned = [...files.keys()];
  expect(scanned.some((f) => f.endsWith("pricing-plans.tsx"))).toBe(false);
  // And that page really does exist in the package and really does carry
  // /gateway links — otherwise the assertion above would be vacuously true.
  const plans = readFileSync(join(PACKAGE_SRC, "pricing-plans.tsx"), "utf8");
  expect(gatewayPaths([plans]).size).toBeGreaterThan(0);
  // The tiers page, by contrast, is now mounted, so it *is* in the scan — and
  // the assertion above is what proves its links resolve.
  expect(scanned.some((f) => f.endsWith("tiers.tsx"))).toBe(true);
});

/**
 * Every path the other feature package asks `canAccess` about must be a path
 * this app can answer for.
 *
 * # Why the criterion lives here
 *
 * Pages in that package hide an entry point when its destination is not
 * reachable, and they delegate that decision to the host's `canAccess`. This
 * host answers it by looking the path up in the registry — so the moment the
 * package adds a destination, this app silently decides it is unreachable. The
 * entry point simply stops appearing: no error, no build failure, a UI that
 * looks entirely normal with one feature missing.
 *
 * This test extracts those literals and asks the registry about each one. A
 * missing one means either mounting the page or recording it in
 * `DELIBERATELY_ABSENT` below with the reason. Either is fine; what is not
 * allowed is nobody knowing.
 */
const CONSOLE_PACKAGE_SRC = join(
  dirname(fileURLToPath(import.meta.url)),
  "../../../packages/gateway-console-features/src",
);

/** Destinations deliberately not mounted: an entry here means somebody looked
 * at it and decided. */
const DELIBERATELY_ABSENT = new Set<string>();

test("every destination the other feature package asks about is one this app can answer for", async () => {
  const files: string[] = [];
  for await (const rel of glob("**/*.{ts,tsx}", { cwd: CONSOLE_PACKAGE_SRC })) files.push(rel);
  expect(
    files.length,
    "no source file found in the other feature package; wrong path?",
  ).toBeGreaterThan(5);

  const dests = new Set<string>();
  for (const rel of files) {
    const src = readFileSync(join(CONSOLE_PACKAGE_SRC, rel), "utf8");
    for (const m of src.matchAll(/canAccess\(\s*\w+\s*,\s*["'](\/[^"']+)["']\s*\)/g)) {
      dests.add(m[1]!);
    }
  }
  // Zero-match guard: with nothing extracted this test is vacuously true, and
  // that is exactly what its failure mode looks like.
  expect(
    dests.size,
    "no canAccess destination was extracted; the pattern no longer matches how the package writes them",
  ).toBeGreaterThan(0);

  const missing = [...dests].filter(
    (d) => resolveAdminPage(d) === undefined && !DELIBERATELY_ABSENT.has(d),
  );
  expect(
    missing,
    `the other feature package asks whether these destinations are reachable, and registry.ts has none of them: ${missing.join(", ")}. ` +
      "Mount the page, or record it in DELIBERATELY_ABSENT with the reason.",
  ).toEqual([]);
});
