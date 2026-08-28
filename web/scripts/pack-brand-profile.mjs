#!/usr/bin/env node
/**
 * Build the runtime brand bundle a server mounts (ADR-0214).
 *
 *   pack-brand-profile.mjs <profile.json> <output-dir>
 *
 * The bundle used to be a tar fed to `docker build --secret`, which is what made
 * a brand a build input and a product line an image. It now goes to the host
 * instead: `deploy.sh` writes it next to the compose file and the binary serves
 * it over the default brand embedded in the artifact.
 *
 * **The layout is not a format of its own -- it mirrors the URLs.** Every file
 * here lands at `/brand/<its path>`, which is exactly where `brandBuild` emits
 * the default one. That is what lets the overlay be a plain file shadow with no
 * mapping table and no "is a bundle mounted" branch on either side.
 *
 *   profile.json                what the page's JSON island gets
 *   profile.css                 @font-face rules and the --flb-profile-* block
 *   {wordmark,mark,favicon,social-mark}.svg
 *   fonts/{display,body,mono}-N.woff2
 *
 * `site.webmanifest` is deliberately absent: its `name` is the *surface* name,
 * and one bundle serves the console and the operations app at once. The server
 * derives it per surface from profile.json.
 *
 * **Packing is the validation gate.** It runs the same `loadBrandProfile` the
 * build runs, so a profile that would have failed the build fails here instead
 * -- unknown or missing fields, remote assets, non-WOFF2 fonts, contrast below
 * AA. The server then only has to read files, and no rule gets a second
 * implementation in Go.
 */

import { mkdirSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { loadBrandProfile } from "../packages/brand/src/build.ts";

const [profileArgument, outArgument] = process.argv.slice(2);
if (!profileArgument || !outArgument) {
  throw new Error("usage: pack-brand-profile.mjs <profile.json>|default <output-dir>");
}
// Every deployment carries an explicit bundle, FairLB's included -- otherwise the
// default line would be the one path that never exercises the code every other
// line depends on. `default` is how it asks for the in-repo profile, which is a
// TypeScript constant rather than a JSON file and so cannot be named by path.
const profilePath = profileArgument === "default" ? undefined : path.resolve(profileArgument);

const out = path.resolve(outArgument);
// Refuse to empty a directory that is not one of ours. Callers pass a path
// under a build directory, and the cost of being wrong here is somebody's tree.
if (existsDirectory(out)) {
  const stray = readdirSync(out).filter((entry) => entry !== "profile.json" && entry !== "fonts");
  const known = new Set([
    "profile.css",
    "wordmark.svg",
    "mark.svg",
    "favicon.svg",
    "social-mark.svg",
  ]);
  const foreign = stray.filter((entry) => !known.has(entry));
  if (foreign.length) {
    throw new Error(
      `pack-brand-profile: ${out} holds files this script does not own (${foreign.join(", ")}); ` +
        "point it at a directory it may replace",
    );
  }
  rmSync(out, { recursive: true });
}

const loaded = loadBrandProfile(profilePath);

mkdirSync(path.join(out, "fonts"), { recursive: true });
writeFileSync(path.join(out, "profile.json"), JSON.stringify(loaded.profile, null, 2));
writeFileSync(path.join(out, "profile.css"), loaded.css);
for (const asset of loaded.assets) {
  // fileName is the served path ("brand/fonts/body-0.woff2"); the bundle root is
  // the brand directory itself, so the prefix comes off and nothing else moves.
  const relative = asset.fileName.replace(/^brand\//, "");
  if (relative === asset.fileName) {
    throw new Error(`pack-brand-profile: unexpected asset outside /brand: ${asset.fileName}`);
  }
  writeFileSync(path.join(out, relative), asset.source);
}

function existsDirectory(candidate) {
  try {
    return statSync(candidate).isDirectory();
  } catch {
    return false;
  }
}

console.log(
  `✔ brand bundle for ${loaded.profile.identity.name}: ${loaded.assets.length + 2} files in ${out}`,
);
