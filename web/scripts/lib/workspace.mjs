// The workspace's own member list, for gates that must see every package.
//
// Four restored gates each carried a hand-written enumeration of apps -- one of
// them with a comment saying, in as many words, that adding an app without
// editing that line leaves the new app unguarded while the banner still prints
// "passed". It was right, and it had already happened twice: two refactors
// moved code into packages the enumeration did not list, and both times the
// only visible symptom was a slightly smaller count in the success line.
//
// Since ADR-0151 there is exactly one workspace root, so the member list is a
// fact on disk rather than something to keep in sync. Reading it also gives the
// right answer in a standalone Community tree: the workspace's private-tree
// patterns match nothing there, so the gates see exactly what that tree ships.
// (The scan that keeps private paths out of the published tree is why this
// sentence describes those patterns instead of quoting one.)
import { readFileSync, existsSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const WEB_ROOT = dirname(dirname(dirname(fileURLToPath(import.meta.url))));

/** Directories of every workspace member holding a package.json. */
export function members(webRoot = WEB_ROOT) {
  const yaml = readFileSync(join(webRoot, "pnpm-workspace.yaml"), "utf8");
  const patterns = [];
  let inPackages = false;
  for (const raw of yaml.split("\n")) {
    const line = raw.replace(/#.*$/, "").trimEnd();
    if (/^packages:\s*$/.test(line)) {
      inPackages = true;
      continue;
    }
    if (inPackages) {
      const m = /^\s+-\s*["']?([^"'\s]+)["']?\s*$/.exec(line);
      if (m) {
        patterns.push(m[1]);
        continue;
      }
      if (line.trim() !== "") break;
    }
  }
  if (patterns.length === 0) {
    throw new Error(
      "pnpm-workspace.yaml yielded no package patterns -- the parser and the file disagree",
    );
  }

  const found = new Set();
  for (const pattern of patterns) {
    // Only the two shapes this workspace actually uses: a literal directory, or
    // a single trailing `*`. Anything else should fail loudly rather than be
    // silently skipped, which is the failure mode this module exists to remove.
    if (pattern.endsWith("/*")) {
      const parent = resolve(webRoot, pattern.slice(0, -2));
      if (!existsSync(parent)) continue;
      for (const entry of readdirSync(parent)) {
        const dir = join(parent, entry);
        if (statSync(dir).isDirectory() && existsSync(join(dir, "package.json"))) found.add(dir);
      }
    } else if (!pattern.includes("*")) {
      const dir = resolve(webRoot, pattern);
      if (existsSync(join(dir, "package.json"))) found.add(dir);
    } else {
      throw new Error(
        `unsupported workspace pattern ${pattern} -- teach this parser rather than letting it skip`,
      );
    }
  }
  return [...found].sort();
}
