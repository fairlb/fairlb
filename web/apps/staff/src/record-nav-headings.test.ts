import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * No aspect page opens with a heading that repeats its own nav item.
 *
 * # The defect
 *
 * Six of the nine aspect pages across the three record layouts rendered a
 * `SectionHeading` built from the *same message key* the `RecordNav` item above
 * it uses. On the provider's models face the word appeared three times running —
 * the nav item, the wrapper heading, and the panel's own card heading — because
 * all three are the word "Models". The other three aspect pages never had one,
 * so the same strip carried two different shapes.
 *
 * # Coverage boundary
 *
 * Same file only, and the gateway staff feature package only. That is enough for the whole defect as
 * it existed — every `RecordNav` and every heading that repeated one lived in
 * one file together — and a cross-file version would need to resolve components
 * to render trees, which is a different instrument. A page that grows the same
 * duplication by putting the heading in a panel module is **not** caught here.
 *
 * gate-honesty: no skip path. The scan must find every record layout, each must
 *   yield at least two nav labels, the heading pattern must match something
 *   somewhere in the package, and the predicate is shown to reject a source that
 *   has the defect. Any of those failing fails the test.
 */

// The package's own tsconfig carries no node types, so a source-scanning test
// cannot live beside the files it scans. It lives in the app that mounts them,
// next to the sibling scan that checks their links resolve.
const SRC = join(
  dirname(fileURLToPath(import.meta.url)),
  "../../../packages/gateway-staff-features/src",
);

/** The array literal starting at `open`, by bracket depth rather than by
 * looking for a closing token — the items contain nested arrays and template
 * literals, and a first-match search walks off the end of the wrong one. */
function arrayLiteralAt(source: string, open: number): string {
  let depth = 0;
  for (let i = open; i < source.length; i += 1) {
    if (source[i] === "[") depth += 1;
    else if (source[i] === "]") {
      depth -= 1;
      if (depth === 0) return source.slice(open, i);
    }
  }
  return "";
}

/**
 * The message keys a file's record navigation uses as item labels.
 *
 * The header takes `recordNav={{ value, items }}` rather than a rendered
 * `<RecordNav>`; the slot was narrowed to the component's props so that nothing
 * else can be put in it. `items` is written both ways — inline, or as a named
 * array when the same list is also read to decide which item is current — and
 * both have to be understood, or the check goes quiet on whichever one it does
 * not know. That is not hypothetical: the first version of this file matched
 * only `<RecordNav>` and went blind the moment the slot changed. Its self-check
 * is what reported it.
 */
function navLabelKeys(source: string): string[] {
  const slot = source.indexOf("recordNav={{");
  if (slot < 0) return [];
  const items = /items: (\[|[A-Za-z_$][\w$]*)/.exec(source.slice(slot));
  if (!items) return [];
  let block: string;
  if (items[1] === "[") {
    block = arrayLiteralAt(source, slot + items.index + items[0].length - 1);
  } else {
    const declaration = source.indexOf(`const ${items[1]} = [`);
    if (declaration < 0) return [];
    block = arrayLiteralAt(source, source.indexOf("[", declaration));
  }
  return [...block.matchAll(/label: t\("([^"]+)"\)/g)].map((m) => m[1]!);
}

/** The message keys a file uses as section headings. */
function headingKeys(source: string): string[] {
  return [
    ...source.matchAll(/<SectionHeading(?:\s[^>]*)?>\{t\("([^"]+)"\)\}<\/SectionHeading>/g),
  ].map((m) => m[1]!);
}

function repeats(source: string): string[] {
  const headings = new Set(headingKeys(source));
  return navLabelKeys(source).filter((key) => headings.has(key));
}

test("no record page heads its content with the label of the nav item that reached it", () => {
  const files = readdirSync(SRC)
    .filter((name) => name.endsWith(".tsx"))
    .map((name) => [name, readFileSync(join(SRC, name), "utf8")] as const);

  const withNav = files.filter(([, source]) => source.includes("recordNav={{"));
  // Self-check: the three record layouts in this package. A scan that found
  // fewer would simply be looking at less and reporting a clean run.
  expect(withNav.map(([name]) => name).sort()).toEqual([
    "model-detail.tsx",
    "pricing-plans.tsx",
    "provider-detail.tsx",
  ]);
  for (const [name, source] of withNav) {
    expect(navLabelKeys(source).length, `${name}: no nav labels were extracted`).toBeGreaterThan(1);
  }
  // Self-check: the heading pattern has to match something, or every comparison
  // below is against an empty set.
  expect(files.flatMap(([, source]) => headingKeys(source)).length).toBeGreaterThan(5);

  const offenders = withNav.flatMap(([name, source]) =>
    repeats(source).map((key) => `${name}: ${key}`),
  );
  expect(offenders, "these headings repeat the nav item that reached them").toEqual([]);
});

const INLINE_ITEMS = `
      <PageHeader
        recordNav={{
          value: active,
          items: [
            { value: "overview", label: t("tabOverview"), href: base },
            { value: "keys", label: t("tabKeys"), href: \`\${base}/keys\` },
          ],
        }}
      />
      <SectionHeading>{t("tabOverview")}</SectionHeading>
`;

const NAMED_ITEMS = `
  const aspects = [
    { value: "overview", label: t("tabOverview"), href: basePath },
    { value: "keys", label: t("tabKeys"), href: \`\${basePath}/keys\` },
  ];
      <PageHeader recordNav={{ value: active, items: aspects }} />
      <SectionHeading>{t("tabOverview")}</SectionHeading>
`;

test.each([
  ["inline items", INLINE_ITEMS],
  ["named items", NAMED_ITEMS],
])("the check rejects a page that repeats its nav item (%s)", (_shape, defective) => {
  // A positive control on the predicate itself, so the assertion above cannot go
  // quiet by extracting nothing. Both shapes are covered because the real files
  // use both.
  expect(navLabelKeys(defective)).toEqual(["tabOverview", "tabKeys"]);
  expect(repeats(defective)).toEqual(["tabOverview"]);
});
