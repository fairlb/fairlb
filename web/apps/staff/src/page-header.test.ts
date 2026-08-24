import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import { adminPages } from "./registry";

/**
 * Every route this app mounts renders a page header, and therefore an h1.
 *
 * # Why it needs a test at all
 *
 * `/orgs/$orgId/access` shipped without one. It is this app's only detail page,
 * and the component behind it is shared with Cloud, where a record layout above
 * it supplies the header — so here it rendered a bare grid: no h1, no
 * breadcrumbs, no document title. Nothing reported it. The router matched, the
 * page worked, and the registry had even been taught the page's ancestor for a
 * breadcrumb that no component was rendering.
 *
 * Console has the equivalent assertion inside its accessibility e2e; this app
 * has no browser fixture, which is exactly why the gap lived here and not there.
 *
 * # Why the criterion is the source and not the DOM
 *
 * Rendering these pages needs a router, a query client, an authenticated
 * identity and a host provider — for a claim whose failure mode is "the file
 * never mentions a header at all". The static form catches that, and it catches
 * it for every route rather than for the handful a fixture would bother to
 * mount. What it deliberately does not claim is that the header is *reachable*
 * at runtime; a page that returns early before it could still pass.
 *
 * Child routes are not checked: they render inside their parent layout, which is
 * where their header comes from and where this test already looks.
 *
 * gate-honesty: no skip path. Three self-checks run before the assertion — the
 *   parsed route set must equal the registry's own, every component must resolve
 *   to a file on disk, and the predicate must be shown to reject a file that has
 *   no header. Any of them failing fails the test rather than reporting a clean
 *   scan.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = join(HERE, "../../..");
const REGISTRY = join(HERE, "registry.tsx");

const PACKAGE_ROOTS: Record<string, string> = {
  "@fairlb/gateway-staff-features": join(WEB_ROOT, "packages/gateway-staff-features/src"),
  "@fairlb/gateway-console-features": join(WEB_ROOT, "packages/gateway-console-features/src"),
};

function readIfPresent(path: string): string | undefined {
  try {
    return readFileSync(path, "utf8");
  } catch {
    return undefined;
  }
}

/** Resolves an import specifier to a source file, relative to the file using it. */
function resolveModule(spec: string, from: string = HERE): string | undefined {
  if (spec.startsWith(".")) {
    for (const ext of [".tsx", ".ts"]) {
      const candidate = join(from, spec) + ext;
      if (readIfPresent(candidate) !== undefined) return candidate;
    }
    return undefined;
  }
  for (const [pkg, root] of Object.entries(PACKAGE_ROOTS)) {
    if (!spec.startsWith(`${pkg}/`)) continue;
    for (const ext of [".tsx", ".ts"]) {
      const candidate = join(root, spec.slice(pkg.length + 1)) + ext;
      if (readIfPresent(candidate) !== undefined) return candidate;
    }
  }
  return undefined;
}

/** Component name to import specifier, for both the lazy and the plain form. */
function componentSources(registry: string): Map<string, string> {
  const map = new Map<string, string>();
  for (const m of registry.matchAll(/const (\w+) = lazy\(\(\) =>\s*import\("([^"]+)"\)/g)) {
    map.set(m[1]!, m[2]!);
  }
  for (const m of registry.matchAll(/import \{([^}]*)\} from "([^"]+)";/g)) {
    for (const raw of m[1]!.split(",")) {
      const name = raw.trim().replace(/^type\s+/, "");
      if (name) map.set(name, m[2]!);
    }
  }
  return map;
}

/**
 * Top-level route entries, as `path` to the file that defines its component.
 *
 * Two forms have to be followed, because the component wiring shared by both
 * operations consoles lives in `@fairlb/gateway-staff-features/admin-routes`
 * and reaches a route as a spread:
 *
 *   { path: "/gateway/models", icon: CubeIcon, ...gatewayModelsPage }
 *
 * The first version of this parse only understood `component: X`. When the
 * wiring moved out it matched six routes instead of twelve — and the self-check
 * below is why that failed loudly instead of quietly scanning half the app.
 */
function topLevelRoutes(registry: string): Map<string, string> {
  const found = new Map<string, string>();
  const imports = componentSources(registry);
  for (const block of registry.matchAll(/\n {2}\{\n([\s\S]*?)\n {2}\},/g)) {
    const body = block[1]!;
    const path = /^ {4}path: "([^"]+)"/m.exec(body);
    if (!path) continue;
    const direct = /^ {4}component: (\w+)/m.exec(body);
    if (direct) {
      const file = resolveModule(imports.get(direct[1]!) ?? "");
      if (file) found.set(path[1]!, file);
      continue;
    }
    const spread = /^ {4}\.\.\.(\w+),/m.exec(body);
    if (!spread) continue;
    const wiringFile = resolveModule(imports.get(spread[1]!) ?? "");
    // A spread whose source cannot be read is a broken scan, not a route with
    // no component: leaving it out would silently shrink the set the
    // self-check compares against the registry, and that comparison is the
    // only thing standing between this test and a clean report on half the app.
    if (!wiringFile) continue;
    const wiring = readFileSync(wiringFile, "utf8");
    const decl = new RegExp(`export const ${spread[1]!} = \\{([\\s\\S]*?)\\n\\} as const;`).exec(
      wiring,
    );
    if (!decl) continue;
    const comp = /component: (\w+)/.exec(decl[1]!);
    if (!comp) continue;
    const lazySpec = new RegExp(
      `const ${comp[1]!} = lazy\\(\\(\\) =>\\s*import\\("([^"]+)"\\)`,
    ).exec(wiring);
    if (!lazySpec) continue;
    const file = resolveModule(lazySpec[1]!, dirname(wiringFile));
    if (file) found.set(path[1]!, file);
  }
  return found;
}

const HAS_HEADER = /<PageHeader\b/;
const HAS_LAYOUT = /<(RecordPage|SectionPage)\b/;

test("every mounted top-level route renders a page header", () => {
  const registry = readFileSync(REGISTRY, "utf8");
  const routes = topLevelRoutes(registry);

  // Self-check one: the parse and the registry itself have to describe the same
  // set of routes. Without it a regex that stopped matching would simply scan
  // fewer pages and report a clean run.
  expect([...routes.keys()].sort()).toEqual([...adminPages].map((p) => p.path).sort());

  const missing: string[] = [];
  for (const [path, file] of routes) {
    // Self-check two: an unresolvable component is a broken scan, not a pass.
    expect(file, `${path}: cannot resolve this route's component to a source file`).toBeDefined();
    if (!HAS_HEADER.test(readFileSync(file, "utf8"))) missing.push(`${path} (${file})`);
  }
  expect(missing, "these routes render no PageHeader, so they have no h1").toEqual([]);
});

test("the header predicate rejects a file that has none", () => {
  // Self-check three, as a positive control on the predicate rather than on the
  // scan: the shared access page is the component that carries no header by
  // design, because in Cloud a record layout above it supplies one. If this ever
  // matches, the assertion above has stopped meaning anything.
  const shared = resolveModule("@fairlb/gateway-staff-features/org-access");
  expect(shared, "the shared access page moved").toBeDefined();
  expect(HAS_HEADER.test(readFileSync(shared!, "utf8"))).toBe(false);
});

/**
 * Every route that owns sub-routes is one of the two page layouts.
 *
 * Which of the two a page should be is a judgement — is the header naming a
 * record or an area — and no test can make it. What a test can hold is that the
 * page is *classified at all*: `RecordPage` and `SectionPage` each carry the
 * grid and the rail rules for their kind, and a layout that is neither renders
 * its children in a bare div, which looks nearly right and is measured by
 * nothing. The geometric probes cannot see this either — they mount hand-written
 * miniatures, so they answer "does this shape behave" and not "does any real
 * page have it".
 *
 * The mixed-up case — a record's aspects put in the rail, or an area's
 * destinations put in the header — is not checked here because it is no longer
 * expressible: both slots take their own component's props rather than a node.
 *
 * Coverage boundary: this app's routes. Cloud's operations console mounts the
 * same three gateway record layouts, and its own two layouts are asserted where
 * they are really mounted.
 */
test("every route with sub-routes is either a record page or a section page", () => {
  const registry = readFileSync(REGISTRY, "utf8");
  const routes = topLevelRoutes(registry);
  const withChildren = adminPages.filter((page) => (page.children?.length ?? 0) > 0);

  // Self-check, spelled out rather than counted: these are the parent routes
  // this app mounts. A scan that matched none of them would otherwise report a
  // clean run, and a count would go on passing if the set changed underneath.
  expect(withChildren.map((page) => page.path).sort()).toEqual([
    "/gateway/models/$modelId",
    "/gateway/providers/$providerId",
  ]);

  const unclassified: string[] = [];
  for (const page of withChildren) {
    const file = routes.get(page.path);
    expect(file, `${page.path}: the parse did not find this route`).toBeDefined();
    if (!HAS_LAYOUT.test(readFileSync(file!, "utf8"))) unclassified.push(`${page.path} (${file})`);
  }
  expect(unclassified, "these parent routes use neither page layout").toEqual([]);
});
