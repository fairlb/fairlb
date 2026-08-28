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

// CSS 也要扫，但只在**反向**那一半：`@fontsource-*` 只被样式表引用，
// 正向那一半不需要它（样式表里的 @import 不是「这个包没声明」那类缺陷）。
const styleExtensions = new Set([".css"]);

function stylesUnder(dir, isSelf = true) {
  const out = [];
  if (!isSelf && MEMBERS.has(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (["node_modules", "dist", ".astro", ".vite", "coverage"].includes(entry.name)) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...stylesUnder(full, false));
    else if (styleExtensions.has(path.extname(entry.name))) out.push(full);
  }
  return out;
}

// 样式表里的引用：`@import "pkg/…"` 与 `url("pkg/…")`。
function styleRefsIn(file) {
  const source = fs.readFileSync(file, "utf8");
  const out = [];
  for (const match of source.matchAll(/(?:@import\s+|url\(\s*)["']([^"']+)["']/g))
    out.push(match[1]);
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

// ===== 反向：声明了，但没有任何一处 import =====
//
// 这一半是补的。此前只走 import→声明一个方向，于是「删掉最后一处 import、
// 忘了删声明」永远没有读数——上线前清理时实测漏出七个：`public/web/apps/staff`
// 的 `react-dom` 与 `@tanstack/react-query`（两个 cloud app 各 import 它们几十处，
// 这个 app 是 0）、三个 app 的 `echarts` 与两个 app 的 `zod`（真正 import 它们的是
// `@fairlb/ui`，那里已声明）。knip 覆盖不到这一格：它报 `packages/*` 的未用导出，
// 不报 `apps/*` 的。
//
// 三类天然不会出现在 import 里，各有**判据**而不是各写一行豁免——
// 能写出判据的例外才不会烂成永久名单：
//
//   ① `@types/*`：环境类型，tsc 自动拾取，按定义永远不被 import。
//   ② 提供二进制、且那个二进制出现在本 manifest 的 scripts **或根 Makefile 的
//      配方行** 里。两处都要看：`knip` 与 `wrangler` 都不在任何 scripts 里，
//      只在 Makefile（`pnpm exec knip`、`./node_modules/.bin/wrangler`）。
//      **只看配方行，不看注释**：第一版扫整份 Makefile，于是 `markdown-it` 与
//      `playwright` 仅凭出现在两行 `#` 注释里就被豁免了（Makefile 的 exec 说明与
//      枚举面说明）。它们今天有 import、走不到这一步，但「名字被提了一嘴就免检」
//      正好会放过这道闸唯一要抓的那个形状。
//   ③ 按**包名**被工具解析的两个：`typescript`（每个 TS 感知的工具都要它在场，
//      而 `tsc` 只出现在成员的 typecheck 脚本里，根 manifest 上没有）与
//      `@astrojs/check`（`astro check` 间接拉起它，命令行上看不见 `astro-check`）。
//   ④ 同名也在**本 manifest 的** `peerDependencies` 里：那份 dev 副本是为了本包
//      自己 typecheck 时能解析出这个 peer（两个 api-client 的 `react` 就是——
//      生成的 react-query 钩子引用 React 类型，而没有一行 `import … from "react"`）。
//   ⑤ 它是本 manifest **另一个**依赖所要求的**非可选** peer。pnpm 不自动装 peer，
//      得由消费方声明——`cloud/web/apps/staff` 的 `playwright` 就是这样：它一行都
//      不 import（只 import `@vitest/browser-playwright`），而那个 provider 把
//      `playwright` 列为 `optional: false` 的 peer，删掉声明浏览器用例就跑不起来。
//      收紧②之后这条当场被误报了一次，判据是那次补的。
//
//      **必须看 `peerDependenciesMeta` 的 optional**：第一版没看，于是
//      `@cloudflare/kumo` 把 `echarts` 列为**可选** peer 这件事，把「app 声明了
//      echarts 却零 import」整个放过了——三条回归当场全漏。可选 peer 不构成
//      声明它的理由：本仓真正 import echarts 的是 `@fairlb/ui`，它自己声明着。
//
// 还有一类不需要判据，因为它本来就是「被用到」：源码里出现
// `node_modules/<包名>/…` 的路径串。三个字体包正是这样进构建的——
// `brand/src/profile.ts` 直接写
// `../node_modules/@fontsource-variable/archivo/files/….woff2`，
// 由 `build.ts` 读出字节打进品牌包。所以它们进 used 集合，不进豁免。
const RESOLVED_BY_NAME = new Set(["typescript", "@astrojs/check"]);

// 根 Makefile 的**配方行**（前导 TAB），注释与说明性文字不算。
//
// 本文件随 `public/` 逐字节发布，而那棵树里没有根 Makefile——所以读不到就当没有
// 这一半判据，而不是抛 ENOENT。今天只有 verify-integration 在仓根调它，读得到；
// 但一个住在可独立发布树里的脚本不该硬依赖私有仓的文件。
function makefileRecipes() {
  const at = path.join(web, "..", "..", "Makefile");
  if (!fs.existsSync(at)) return "";
  return fs
    .readFileSync(at, "utf8")
    .split("\n")
    .filter((line) => line.startsWith("\t"))
    .join("\n");
}
const makefile = makefileRecipes();

// 本 manifest 的其他依赖里，谁把 name 列成了**非可选** peer。
function providesPeerFor(dir, name, siblings) {
  for (const sibling of siblings) {
    if (sibling === name) continue;
    for (const base of [dir, web]) {
      const at = path.join(base, "node_modules", sibling, "package.json");
      if (!fs.existsSync(at)) continue;
      const manifest = JSON.parse(fs.readFileSync(at, "utf8"));
      const required =
        name in (manifest.peerDependencies ?? {}) &&
        (manifest.peerDependenciesMeta ?? {})[name]?.optional !== true;
      if (required) return sibling;
      break;
    }
  }
  return null;
}

function declaredBins(dir, name) {
  for (const base of [dir, web]) {
    const manifestPath = path.join(base, "node_modules", name, "package.json");
    if (!fs.existsSync(manifestPath)) continue;
    const bin = JSON.parse(fs.readFileSync(manifestPath, "utf8")).bin;
    if (!bin) return [];
    return typeof bin === "string" ? [name] : Object.keys(bin);
  }
  return null; // 解析不到：调用方按「没有二进制」处理，见下
}

// 反向那一半要求装过依赖：一棵没 install 的树上，每个声明都会「没人 import」。
// 与其在那种树上刷屏，不如说清楚这一半没答。
const installed = fs.existsSync(path.join(web, "node_modules"));

for (const dir of installed ? packageDirs : []) {
  const manifest = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
  // peer/optional 不在判决面内：它们是给消费方声明的，本包不 import 才是常态。
  const owned = Object.keys({ ...manifest.dependencies, ...manifest.devDependencies });
  const peers = new Set(Object.keys(manifest.peerDependencies ?? {}));
  const used = new Set();
  const sources = [...filesUnder(dir), ...stylesUnder(dir)];
  for (const file of filesUnder(dir)) for (const s of importsIn(file)) used.add(packageName(s));
  for (const file of stylesUnder(dir)) for (const s of styleRefsIn(file)) used.add(packageName(s));
  // 路径串引用：`node_modules/@scope/name/…` 或 `node_modules/name/…`
  for (const file of sources) {
    const text = fs.readFileSync(file, "utf8");
    for (const m of text.matchAll(/node_modules\/(@[\w.-]+\/[\w.-]+|[\w.-]+)/g)) used.add(m[1]);
  }
  const scripts = Object.values(manifest.scripts ?? {}).join(" ; ");
  for (const dependency of owned) {
    if (used.has(dependency)) continue;
    if (dependency.startsWith("@types/")) continue;
    if (RESOLVED_BY_NAME.has(dependency)) continue;
    if (peers.has(dependency)) continue;
    if (providesPeerFor(dir, dependency, owned)) continue;
    // 解析不到就当它没有二进制——**不能 continue**：上闸时实测过一次，
    // 把删掉的 `zod` 加回 manifest 而不重装，那条 `continue` 正好把它放过了，
    // 而「加回声明、没人 import」恰恰是这道闸唯一要抓的形状。
    const bins = declaredBins(dir, dependency) ?? [];
    const runnable = bins.some((b) =>
      new RegExp(`(^|[\\s;&|(/])${b.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\b`).test(
        scripts + "\n" + makefile,
      ),
    );
    if (runnable) continue;
    errors.push(
      `${manifest.name} declares ${dependency}, but nothing imports it, no script or Makefile rule runs its binary, and it is not one of the two packages tools resolve by name`,
    );
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
  `  package dependencies: ${packageDirs.length} manifests declare every imported package directly` +
    `${installed ? "，且声明的每个包都有 import、有脚本跑它的二进制、或有判据" : "（未安装，反向那一半没答）"}` +
    `; marketing TS6 exception is pinned`,
);
