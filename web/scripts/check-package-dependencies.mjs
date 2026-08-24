import fs from "node:fs";
import path from "node:path";
import { builtinModules } from "node:module";
import { WEB_ROOT, members } from "./lib/workspace.mjs";

// 枚举面 = workspace 成员（ADR-0151 之后只有一个根），不是「本树的 apps/ 与
// packages/」。旧写法在拆成两棵树之后只看得见 public/web 那一半，而它报的
// 「N 个 manifest 全部直接声明」读起来完全正常。
const web = WEB_ROOT;
const packageDirs = [web, ...members()];

const builtins = new Set([...builtinModules, ...builtinModules.map((name) => `node:${name}`)]);
const sourceExtensions = new Set([
  ".js",
  ".jsx",
  ".mjs",
  ".cjs",
  ".ts",
  ".tsx",
  ".mts",
  ".cts",
  ".astro",
]);

function packageName(specifier) {
  if (specifier.startsWith("astro:")) return "astro";
  if (specifier.startsWith("@")) return specifier.split("/").slice(0, 2).join("/");
  return specifier.split("/")[0];
}

// 一个文件属于**离它最近**的那个 manifest，故遇到另一个成员目录就停。
// 旧写法靠 `isRoot && ["apps","packages"]` 这个约定来划界；拆成两棵树之后
// `cloud/web` 自己也是成员，它的 apps/ 与 packages/ 又各是成员，那个约定
// 就把几百个文件记到了根 manifest 名下。
const MEMBERS = new Set(packageDirs);
function filesUnder(dir, isSelf = true) {
  const out = [];
  if (!isSelf && MEMBERS.has(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (["node_modules", "dist", ".astro", ".vite", "coverage"].includes(entry.name)) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...filesUnder(full, false));
    else if (sourceExtensions.has(path.extname(entry.name))) out.push(full);
  }
  return out;
}

function importsIn(file) {
  const source = fs.readFileSync(file, "utf8");
  const out = [];
  const staticImports =
    /^\s*(?:import|export)\s+(?:type\s+)?(?:[\w*{},\s]+?\sfrom\s+)?["']([^"']+)["']/gm;
  const dynamicImports = /\bimport\(\s*["']([^"']+)["']\s*\)/g;
  for (const pattern of [staticImports, dynamicImports]) {
    for (const match of source.matchAll(pattern)) out.push(match[1]);
  }
  return out;
}

const errors = [];
for (const dir of packageDirs) {
  const manifest = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
  const declared = new Set([
    ...Object.keys(manifest.dependencies ?? {}),
    ...Object.keys(manifest.devDependencies ?? {}),
    ...Object.keys(manifest.peerDependencies ?? {}),
    ...Object.keys(manifest.optionalDependencies ?? {}),
  ]);
  for (const file of filesUnder(dir)) {
    for (const specifier of importsIn(file)) {
      if (specifier.startsWith(".") || specifier.startsWith("/") || builtins.has(specifier))
        continue;
      const dependency = packageName(specifier);
      if (!declared.has(dependency)) {
        errors.push(
          `${path.relative(web, file)} imports ${dependency}, but ${manifest.name} does not declare it`,
        );
      }
    }
  }
}

// 按包名找，不按路径找：这个包在拆分中搬过一次家，而写死的路径搬不动。
const marketingDir = packageDirs.find(
  (dir) =>
    JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")).name ===
    "@fairlb/cloud-marketing",
);
if (!marketingDir) {
  console.error("✗ 找不到 @fairlb/marketing——枚举面塌了，不是通过");
  process.exit(1);
}
const marketing = JSON.parse(fs.readFileSync(path.join(marketingDir, "package.json"), "utf8"));
if (!/^\^?6\./.test(marketing.devDependencies?.typescript ?? "")) {
  errors.push(
    "@fairlb/marketing must stay on TypeScript 6: @astrojs/check 0.9.x accepts ^5 || ^6, not TypeScript 7",
  );
}

if (errors.length > 0) {
  console.error(`✗ package dependency policy failed:\n  ${errors.join("\n  ")}`);
  process.exit(1);
}
console.log(
  `  package dependencies: ${packageDirs.length} manifests declare every imported package directly; marketing TS6 exception is pinned`,
);
