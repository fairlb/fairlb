#!/usr/bin/env node
// 每个测试文件都必须真的有人跑它。
//
// `make web-test` = `pnpm -r test`，而 **pnpm 对没有该脚本的包静默跳过**。ADR-0029
// 记的就是这个形状的旧账：`apps/marketing` 没有 typecheck 脚本，于是
// `pnpm -r typecheck` 报绿的同时整个营销站一个文件都没查。测试这一面今天还没出事
// （实测 10 个测试文件 = admin 4 + admin browser 3 + console 2 + console e2e 1，
// 一个不漏），但护栏是空的：往 packages/ui 加一个 `foo.test.ts`，
// `pnpm -r test` 不会跑它，而 verify 第 8 步照样全绿。
//
// 两条判据：
//   ① 有测试文件的包必须有 test 脚本（否则 pnpm -r 静默跳过整个包）
//   ② 该包每个测试文件都得被它自己 runner 的 include 命中（否则跑了 runner 也漏这个文件）
//
// gate-honesty: 本脚本无跳过路径。结论行带「N 个包 / M 个测试文件全部有人跑」的计数；
//   扫到 0 个包或 0 个测试文件即失败（否则枚举面塌了会长得像通过）；
//   某个包有 test 脚本却推导不出任何 include 也失败——推导不出就是不知道有没有人跑，
//   不能当成跑了。
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join, matchesGlob, posix, relative } from "node:path";
import { WEB_ROOT, members } from "./lib/workspace.mjs";

const root = WEB_ROOT;

// vitest 不带 --config 时的默认 include（vitest 4）
const VITEST_DEFAULT = ["**/*.{test,spec}.?(c|m)[jt]s?(x)"];
const SKIP_DIRS = new Set(["node_modules", "dist", ".astro", "gen", "coverage"]);
const TEST_BODY = /^\s*(it|test|describe)\s*\(/m;

// 一个测试文件属于**离它最近**的那个包。不停在成员边界上，`cloud/web` 这个根成员
// 会把它下面每个 app 的测试文件都算成自己的，然后抱怨自己的 test 脚本没跑它们。
function walk(dir, out = [], isSelf = true) {
  if (!isSelf && MEMBER_DIRS.has(dir)) return out;
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    if (e.isDirectory()) {
      if (!SKIP_DIRS.has(e.name)) walk(join(dir, e.name), out, false);
    } else if (/\.(ts|tsx|mts|js|jsx)$/.test(e.name)) {
      out.push(join(dir, e.name));
    }
  }
  return out;
}

/** `test: "pnpm test:unit && pnpm test:layout"` → 展开成真正执行的命令列表 */
function resolveChain(scripts, name, seen = new Set()) {
  if (seen.has(name) || !scripts[name]) return [];
  seen.add(name);
  const out = [];
  for (const part of scripts[name].split("&&").map((s) => s.trim())) {
    const ref = part.match(/^(?:pnpm|npm run|yarn)\s+([\w:.-]+)$/);
    if (ref) out.push(...resolveChain(scripts, ref[1], seen));
    else out.push(part);
  }
  return out;
}

function quoted(src, key) {
  // include: ["a", "b"] / testMatch: "x"
  const arr = src.match(new RegExp(`${key}\\s*:\\s*\\[([^\\]]*)\\]`));
  if (arr) return [...arr[1].matchAll(/["'`]([^"'`]+)["'`]/g)].map((m) => m[1]);
  const one = src.match(new RegExp(`${key}\\s*:\\s*["'\`]([^"'\`]+)["'\`]`));
  return one ? [one[1]] : [];
}

/**
 * vitest 配置里的 `include` **只认 `test:` 块里的那一个**。
 *
 * `quoted` 拿的是全文第一个匹配，而 vite 配置里 `include` 是个常见键——
 * `optimizeDeps: { include: [...] }` 就长在 `test:` 前面。实测（ADR-0056 给
 * admin 的浏览器配置加 optimizeDeps 时）：读到的是那份依赖清单，
 * `src/**\/*.browser.tsx` 整个消失，于是**六个浏览器测试文件一起报「没人跑」**
 * ——包括本次没碰过的四个。这次是假红，但同一机理反过来就是假绿：
 * 另一个键的数组恰好命中了测试文件名，本检查就会认为它有人跑。
 *
 * 所以从 `test:` 那一层开始截。截不到 `test:` 就退回全文（配置形态千奇百怪，
 * 这里宁可回到旧行为，也不要在推导失败时装作推导成功）。
 */
function vitestInclude(src) {
  const at = src.search(/\btest\s*:\s*\{/);
  return quoted(at >= 0 ? src.slice(at) : src, "include");
}

/** 一条命令 → 它会跑哪些文件（相对包根的 glob） */
function includesOf(cmd, pkgDir) {
  const cfg = cmd.match(/--config\s+(\S+)/)?.[1];
  const read = (f) => (existsSync(join(pkgDir, f)) ? readFileSync(join(pkgDir, f), "utf8") : null);

  // `pnpm --filter './apps/x' test`：把 test 转交给另一个 workspace 成员。
  // 那个成员自己在枚举面里、自己被审一遍，所以这条命令在**本包**里不覆盖任何
  // 文件——返回空集而不是「推导失败」。**目标必须真是个成员**：不是就按推导
  // 失败处理，否则一条指向不存在的包的 --filter 会被读成「没有要跑的东西」。
  const delegate = cmd.match(/--filter\s+['"]?([^'"\s]+)['"]?\s+(\S+)/);
  if (delegate && /\bpnpm\b/.test(cmd)) {
    const target = join(pkgDir, delegate[1]);
    return MEMBER_DIRS.has(target) ? [] : null;
  }

  // `node scripts/foo.mjs`：不是 runner，是链条里的一个自检步骤。它跑的就是
  // 它自己那一个文件，覆盖面按字面给——比返回空集诚实，因为那个文件万一是
  // 测试文件，它确实被跑了。
  const bare = cmd.match(/^node\s+(\S+\.m?js)\b/);
  if (bare) return [bare[1]];

  if (/\bvitest\b/.test(cmd)) {
    if (!cfg) return VITEST_DEFAULT;
    const src = read(cfg);
    if (src === null) return null; // 配置指向不存在的文件 → 推导失败
    const inc = vitestInclude(src);
    return inc.length > 0 ? inc : VITEST_DEFAULT;
  }
  if (/\bplaywright\b/.test(cmd)) {
    const src = read(cfg ?? "playwright.config.ts");
    if (src === null) return null;
    const dir = (quoted(src, "testDir")[0] ?? ".").replace(/^\.\//, "");
    const match = quoted(src, "testMatch");
    return (match.length > 0 ? match : ["**/*.@(spec|test).?(c|m)[jt]s?(x)"]).map((m) =>
      posix.join(dir, m.includes("/") ? m : `**/${m}`),
    );
  }
  return null; // 不认识的 runner → 推导失败，按不知道处理
}

// 枚举面 = workspace 成员。旧写法只看本树的 apps/ 与 packages/，拆成两棵树之后
// 它看不见 cloud/web 那一半——而这道闸防的正是「有测试文件却没人跑」，
// 枚举面少一半时它报的「全部被 runner 命中」照样是绿的。
const pkgDirs = members().filter((dir) => dir !== root);
const MEMBER_DIRS = new Set(members());

const problems = [];
let totalTests = 0;
let covered = 0;

for (const pkgDir of pkgDirs) {
  const pkg = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
  const rel = relative(root, pkgDir);
  const testFiles = walk(pkgDir)
    .filter((f) => TEST_BODY.test(readFileSync(f, "utf8")))
    .map((f) => relative(pkgDir, f).split("\\").join("/"));
  totalTests += testFiles.length;

  if (testFiles.length === 0) continue;

  if (!pkg.scripts?.test) {
    problems.push(
      `${pkg.name}（${rel}）有 ${testFiles.length} 个测试文件却没有 test 脚本——` +
        `pnpm -r test 会静默跳过整个包，门禁照绿：\n      ${testFiles.join("\n      ")}`,
    );
    continue;
  }

  const patterns = [];
  for (const cmd of resolveChain(pkg.scripts, "test")) {
    const inc = includesOf(cmd, pkgDir);
    if (inc === null) {
      problems.push(
        `${pkg.name} 的 test 链里这条命令推导不出 include：\`${cmd}\`——推导不出就不能算跑过`,
      );
      continue;
    }
    patterns.push(...inc);
  }
  if (patterns.length === 0) {
    problems.push(`${pkg.name} 有 test 脚本但推导不出任何 include`);
    continue;
  }

  for (const f of testFiles) {
    if (patterns.some((p) => matchesGlob(f, p))) covered++;
    else
      problems.push(
        `${pkg.name} 的 ${f} 不被任何 runner 命中——它自己那个包的 include 是：\n      ` +
          patterns.join("\n      "),
      );
  }
}

// 自我诚实守卫：枚举面塌了不能长得像通过
if (pkgDirs.length === 0) {
  console.error("✗ 一个 workspace 包都没扫到——枚举面塌了，不是通过");
  process.exit(1);
}
if (totalTests === 0) {
  console.error("✗ 全仓一个前端测试文件都没扫到——判定式坏了，不是通过");
  process.exit(1);
}

if (problems.length > 0) {
  console.error(`✗ 测试接线检查未通过（${problems.length} 处）：`);
  for (const p of problems) console.error(`  · ${p}`);
  console.error(
    "\n  修法：给该包补 test 脚本（并入 pnpm -r test），或把文件名改成 runner 认得的形态。",
  );
  process.exit(1);
}

console.log(
  `✔ 测试接线检查通过（${pkgDirs.length} 个包，${totalTests} 个测试文件全部被 runner 命中${
    covered === totalTests ? "" : `（命中 ${covered}）`
  }）`,
);
