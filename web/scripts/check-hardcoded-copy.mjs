#!/usr/bin/env node
// 界面文案必须走字典。
//
// 这条判据是补出来的：P5 五个新页面全部硬编码中文（244 处），而 make verify 八步全绿
// ——`zh: Record<MessageKey, string>` 只保证 en/zh 两边一致，管不到「你压根没调字典」。
// 类型系统在这里天然盲，故用一条文本检查补上。
//
// 它在 045c13ae 的拆分里连同整个 check-i18n.mjs 一起消失了，而**28 个 i18n-ignore
// 标记留在原地压制着一个不存在的闸**（ADR-0169）。跨词典对账那半已由
// deploy/scripts/check-i18n-dictionaries.py 重建，本文件只承担硬编码这一半。
//
// 判据：workspace 成员的 .tsx 里，注释与 import 以外的位置出现 CJK 即失败。
// 注释保持中文是本仓惯例（不面向终端用户），故排除。
//
// gate-honesty: 无跳过路径。枚举面来自 workspace 成员清单，扫到 0 个文件即失败。
import { existsSync, readFileSync } from "node:fs";
import { glob } from "node:fs/promises";
import { relative } from "node:path";

import { WEB_ROOT, members } from "./lib/workspace.mjs";

const CJK = /[一-鿿]/;

/**
 * 去掉注释与 import 行——注释里的中文是允许的。
 *
 * 块注释换成等量空行而不是直接删：直接删会连里面的换行一起没了，后面每行的下标
 * 都往前挪（实测 console/src/lib.tsx 漂 85 行）。
 *
 * 漂了的后果是静默的：报错行号指到不相干的行，而下面 `raw[i]` 取到的也是错行——
 * 等于 i18n-ignore 放行标记失效，你标的那行不被承认，却可能放行掉别处一条真违规。
 *
 * **import 那条曾经就是这么漂的**（ADR-0065 撞上）。原先写的是 `^\s*import`，
 * 而 `\s` 含换行、`m` 只让 `^` 能匹配行首**并不阻止 `\s*` 往回吃**：于是
 * 「空行 + import」会被整体替换成空串，一次吞掉一个换行。lib.tsx 里新加一句
 * 与上文空行隔开的 import，后面全部行号偏移 1，第 529 行那个 i18n-ignore
 * 当场失效、报了一条假违规。改用 `[^\S\n]*`（只吃水平空白）后行数守恒。
 *
 * 判据不再靠「本来就不漂」这种推断：下面 assertLineCountPreserved 每个文件实测一遍，
 * 漂了直接失败——这条不变量属于 strip 自己，不能交给调用方小心。
 */
function strip(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, "")) // 块注释与 {/* JSX 注释 */}
    .replace(/\/\/.*$/gm, "") // 行注释（含行尾的）
    .replace(/^[^\S\n]*import .*$/gm, ""); // 只吃行内缩进，别把上一行的换行也吃了
}

/**
 * strip 必须行数守恒——`raw[i]` 与 stripped[i] 指同一行是本检查的全部前提。
 *
 * 单独断言而不是「小心地写正则」：这类漂移不会报错，只会让放行标记静默失效，
 * 而失效之后的表现是「报了一条你看不懂的假违规」或者更糟的「放过了一条真违规」。
 */
function assertLineCountPreserved(file, src, stripped) {
  const a = src.split("\n").length;
  const b = stripped.split("\n").length;
  if (a !== b) {
    console.error(`${file}: strip 漂了行号（原 ${a} 行 → ${b} 行），i18n-ignore 会失效`);
    return false;
  }
  return true;
}

// 枚举面来自 workspace 成员清单，不是手写的 app 花括号。旧版那句「新增 app 要同批
// 加进花括号，不加则新 app 的硬编码文案无人看管而结论行照打通过」，说的正是这里。
const files = [];
for (const pkg of members()) {
  if (!existsSync(`${pkg}/src`)) continue;
  for await (const rel of glob("src/**/*.tsx", { cwd: pkg })) {
    // 测试文件不是界面：`.browser.tsx` / `.e2e.tsx` 里的中文是断言消息与夹具
    // 说明，永远不会到用户眼前。与 check-empty-state-copy 同一条排除。
    if (rel.includes(".browser.") || rel.includes(".e2e.")) continue;
    files.push(`${pkg}/${rel}`);
  }
}
if (files.length === 0) {
  console.error("✗ 一个 .tsx 都没扫到——枚举面塌了，不是通过");
  process.exit(1);
}

let bad = 0;
let suppressed = 0;
for (const abs of files.sort()) {
  const f = relative(WEB_ROOT, abs);
  const src = readFileSync(abs, "utf8");
  // 放行标记要在**原始行**上找：strip 会把注释连同标记一起删掉
  const raw = src.split("\n");
  const stripped = strip(src);
  if (!assertLineCountPreserved(f, src, stripped)) bad++;
  stripped.split("\n").forEach((line, i) => {
    // 语言切换器本身要用目标语言的名字显示（"中文"/"EN"），不该进字典——
    // 那是唯一合理的例外，用显式标记放行而不是特判文件名
    if (raw[i]?.includes("i18n-ignore")) {
      if (CJK.test(line)) suppressed++;
      return;
    }
    if (CJK.test(line)) {
      console.error(`${f}:${i + 1}: 界面文案未走 i18n 字典 → ${line.trim().slice(0, 80)}`);
      bad++;
    }
  });
}
if (bad > 0) {
  console.error(`\n✗ ${bad} 处硬编码文案。用 useI18n() 的 t()，词条加进 packages/i18n。`);
  process.exit(1);
}

console.log(
  `✔ 无硬编码界面文案（${files.length} 个 .tsx，${suppressed} 行由 i18n-ignore 显式放行）`,
);
