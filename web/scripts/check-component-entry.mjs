#!/usr/bin/env node
// 有应用级入口的组件，必须从那个入口引，不从设计系统直接引。
//
// # 它防的是什么
//
// `@fairlb/ui` 的 `Button` 只做一件事：把 FairLB 的默认值（`variant="primary"`、
// `size="base"`）钉上去。Kumo 自己的默认是 `variant="secondary"`——所以「直接引
// Kumo 的 Button 且不写 variant」渲染出来的是**另一种按钮**，而它和别处的主操作
// 按钮长得只差一点，没有任何读数会报。
//
// 实测（ADR-0170 立项时）：17 个文件直接引 Kumo 的 Button，其中 2 处没写 variant。
// 那 2 处都在 `secret-reveal-dialog.tsx`，是全仓唯一一个主操作按钮不是 primary 的
// 弹窗——两个兄弟弹窗（form-dialog、confirm-dialog）都显式给了 primary。
//
// 立项时记的是「32 个文件绕过包装」，实测不成立：其中 14 个只引 `LinkButton`，
// 那是另一个组件（按钮外观的真链接），本闸不管它。
//
// # 判据
//
// 整文件解析 import 语句，不逐行匹配：`import {\n  Button,\n} from ...` 换行之后
// 逐行正则就看不见了，而那正是 prettier 在名字变长时会做的事。
//
// gate-honesty: 无跳过路径。每条规则的例外是**唯一那个实现文件**，且例外文件里
// 必须真的出现那个 import——不出现说明实现搬家了、本闸盯着的是旧世界，失败而不是通过。
import { readFileSync } from "node:fs";
import { glob } from "node:fs/promises";
import { relative } from "node:path";

import { WEB_ROOT, members } from "./lib/workspace.mjs";

const RULES = [
  {
    module: "@cloudflare/kumo/components/button",
    name: "Button",
    entry: '@fairlb/ui 的 Button（包内用 "./button"）',
    // 唯一的实现文件：包装本身当然要引它包的那个。
    implementation: "packages/ui/src/button.tsx",
    why: "Kumo 默认 variant 是 secondary，FairLB 默认是 primary——直接引且不写 variant 会渲染出另一种按钮",
  },
];

/** 文件里 `from "<module>"` 的那条 import 引进了哪些名字（含多行形态）。 */
function importedFrom(src, module) {
  const names = [];
  const re = new RegExp(
    String.raw`import\s*(?:type\s*)?\{([^}]*)\}\s*from\s*["']${module.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}["']`,
    "gs",
  );
  for (const m of src.matchAll(re)) {
    for (const part of m[1].split(",")) {
      const base = part
        .trim()
        .replace(/^type\s+/, "")
        .split(/\s+as\s+/)[0]
        .trim();
      if (base) names.push(base);
    }
  }
  return names;
}

const files = [];
for (const pkg of members()) {
  for await (const rel of glob("src/**/*.{ts,tsx}", { cwd: pkg })) files.push(`${pkg}/${rel}`);
}
if (files.length === 0) {
  console.error("✗ 一个源文件都没扫到——枚举面塌了，不是通过");
  process.exit(1);
}

const violations = [];
const implementationSeen = new Map(RULES.map((r) => [r.id ?? r.implementation, false]));
let checked = 0;

for (const abs of files.sort()) {
  const path = relative(WEB_ROOT, abs);
  const src = readFileSync(abs, "utf8");
  for (const rule of RULES) {
    if (!src.includes(rule.module)) continue;
    checked += 1;
    const names = importedFrom(src, rule.module);
    if (!names.includes(rule.name)) continue;
    if (path === rule.implementation) {
      implementationSeen.set(rule.implementation, true);
      continue;
    }
    violations.push({ path, rule });
  }
}

for (const [implementation, seen] of implementationSeen) {
  if (!seen) {
    console.error(
      `✗ ${implementation} 里没有出现它被豁免的那条 import——实现搬家了？闸已失效，先修闸`,
    );
    process.exit(1);
  }
}

if (violations.length > 0) {
  console.error("✗ 组件绕过了它的应用级入口：");
  for (const v of violations.sort((a, b) => a.path.localeCompare(b.path))) {
    console.error(`    ${v.path}: 直接引 ${v.rule.module} 的 ${v.rule.name}`);
    console.error(`      ${v.rule.why}`);
    console.error(`      改用 ${v.rule.entry}`);
  }
  process.exit(1);
}

console.log(
  `✔ 组件入口检查通过（${files.length} 个源文件，${checked} 处相关 import，${RULES.length} 条规则）`,
);
