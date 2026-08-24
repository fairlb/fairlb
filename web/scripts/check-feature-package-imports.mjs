#!/usr/bin/env node
// 两版共用的产品层功能包不得触碰平台段的 API 契约（ADR-0114 W16b）。
//
// # 它防的是什么
//
// `@fairlb/gateway-{staff,console}-features` 被商业版与社区版同时消费，且**整份
// 进开源导出树**。它们此前 import 的是合并命名空间 `staffApi` / `consoleApi`
// ——那两个各自 re-export 平台段与 gateway 段两份生成物，于是：
//
//   · 模块图上拽着 `gen/{staff,console}/`，而导出白名单是文件粒度的
//     （决策 11）：要么把整份平台契约送进公开仓，要么导出树编译不过
//   · 真的调到平台段的端点也不会有人叫——`gatewayStaffApi` 缺那个方法会编译期
//     报错，`staffApi` 有，于是社区版拿到一个 404 的调用。**W16b 实测抓到三处**
//     （`listSettings` / `putSetting` / `getMeta`），症状是页面渲染正常、
//     按钮点得动、请求 404
//
// 第二条是这道闸真正的价值：第一条 typecheck 早晚会红，第二条不会。
//
// # 判据
//
// 两个包的 src 里出现平台段的命名空间标识符（`staffApi` / `consoleApi` /
// `StaffTypes` / `ConsoleTypes`）或直接引 `gen/{staff,console}/` 即失败。
// 按标识符查而不是按 import 行查：`import * as X` 之后的用法一样要抓。
//
// gate-honesty: 本脚本无跳过路径。三条自检拦住「扫了个寂寞」——包目录不存在、
// 扫到的文件数为零、或允许的命名空间一次都没出现（说明标识符改名了而本闸不知道），
// 任一成立即失败而不是「没发现问题」。
import { readFileSync } from "node:fs";
import { glob } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const WEB = dirname(dirname(fileURLToPath(import.meta.url)));

const PACKAGES = ["gateway-staff-features", "gateway-console-features"];

/** 平台段的命名空间：功能包一个都不许用。 */
const FORBIDDEN = ["staffApi", "consoleApi", "StaffTypes", "ConsoleTypes"];
/** gateway 段的命名空间：出现次数为零说明标识符改名了，本闸已失效。 */
const EXPECTED = [
  "gatewayStaffApi",
  "gatewayConsoleApi",
  "GatewayStaffTypes",
  "GatewayConsoleTypes",
];

const hits = [];
let scanned = 0;
let expectedSeen = 0;

for (const pkg of PACKAGES) {
  const root = join(WEB, "packages", pkg, "src");
  let inPkg = 0;
  for await (const rel of glob("**/*.{ts,tsx}", { cwd: root })) {
    const file = join(root, rel);
    const body = readFileSync(file, "utf8");
    inPkg += 1;
    scanned += 1;
    for (const id of FORBIDDEN) {
      // 词边界：`staffApi` 不能匹配 `gatewayStaffApi`（后者含前者的大小写变体，
      // 但 `gatewayStaffApi` 里的是 `StaffApi`，故只需防 `\w` 前缀）
      if (new RegExp(`(?<![\\w.])${id}\\b`).test(body)) {
        hits.push(`${pkg}/src/${rel}: ${id}`);
      }
    }
    if (/from ["'][^"']*gen\/(staff|console)\//.test(body)) {
      hits.push(`${pkg}/src/${rel}: 直接引 gen/{staff,console}/`);
    }
    for (const id of EXPECTED) {
      if (new RegExp(`\\b${id}\\b`).test(body)) expectedSeen += 1;
    }
  }
  // 自检①：包目录空 = 路径写错或包改名，零命中会被读成「干净」
  if (inPkg < 3) {
    console.error(`✗ ${pkg}/src 只扫到 ${inPkg} 个文件——路径写错了？闸已失效，先修闸`);
    process.exit(1);
  }
}

// 自检②：总量守卫
if (scanned < 20) {
  console.error(`✗ 只扫到 ${scanned} 个文件——扫描面塌了，闸已失效`);
  process.exit(1);
}
// 自检③：gateway 段命名空间一次都没出现 = 标识符改过名，本闸盯着的是旧世界
if (expectedSeen === 0) {
  console.error("✗ 两个包里一个 gateway 段命名空间都没出现——标识符改名了？闸已失效，先修闸");
  process.exit(1);
}

if (hits.length > 0) {
  console.error("✗ 功能包引用了平台段的 API 契约（社区版没有那一段，调到就是 404）：");
  for (const h of hits.sort()) console.error(`    ${h}`);
  console.error(
    "\n处置：换成只含 gateway 段的 `gatewayStaffApi` / `gatewayConsoleApi`；" +
      "若那个端点确实只有平台段有，把它升成宿主契约的一条由各壳注入" +
      "（见 gateway-console-features/src/host.tsx 的 useApiKeyOptions）。",
  );
  process.exit(1);
}

console.log(
  `✔ 功能包未引用平台段契约（${PACKAGES.length} 个包 × ${scanned} 个文件，` +
    `gateway 段命名空间出现 ${expectedSeen} 处）`,
);
