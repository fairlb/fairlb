#!/usr/bin/env node
/** Package a complete external brand profile and its local assets for Docker BuildKit. */

import { spawnSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";

const [profileArgument, outputArgument] = process.argv.slice(2);
if (!profileArgument || !outputArgument) {
  throw new Error("usage: pack-brand-profile.mjs <profile.json> <output.tar>");
}

const profilePath = path.resolve(profileArgument);
const profileBase = path.dirname(profilePath);
const outputPath = path.resolve(outputArgument);
if (existsSync(outputPath)) throw new Error(`refusing to overwrite ${outputPath}`);

const profile = JSON.parse(readFileSync(profilePath, "utf8"));
const temporary = mkdtempSync(path.join(os.tmpdir(), "fairlb-brand-bundle-"));

function sourceFile(value, label) {
  if (typeof value !== "string" || /^(?:https?:)?\/\//i.test(value)) {
    throw new Error(`${label} must be a local path`);
  }
  const source = path.resolve(profileBase, value);
  if (!statSync(source).isFile()) throw new Error(`${label} does not point to a file`);
  return source;
}

function include(source, relative) {
  const destination = path.join(temporary, relative);
  mkdirSync(path.dirname(destination), { recursive: true });
  copyFileSync(source, destination);
  return `./${relative.split(path.sep).join("/")}`;
}

try {
  for (const key of ["wordmarkSvg", "markSvg", "faviconSvg", "socialMarkSvg"]) {
    profile.identity.assets[key] = include(
      sourceFile(profile.identity.assets[key], `identity.assets.${key}`),
      `assets/${key}.svg`,
    );
  }
  for (const role of ["display", "body", "mono"]) {
    profile.theme.fonts[role].sources.forEach((font, index) => {
      font.path = include(
        sourceFile(font.path, `theme.fonts.${role}.sources[${index}].path`),
        `fonts/${role}-${index}.woff2`,
      );
    });
  }
  writeFileSync(path.join(temporary, "profile.json"), `${JSON.stringify(profile, null, 2)}\n`);
  mkdirSync(path.dirname(outputPath), { recursive: true });
  const archive = spawnSync("tar", ["-cf", outputPath, "-C", temporary, "."], {
    stdio: "inherit",
  });
  if (archive.error) throw archive.error;
  if (archive.status !== 0) throw new Error(`tar exited with ${archive.status}`);
  console.log(outputPath);
} finally {
  rmSync(temporary, { recursive: true, force: true });
}
