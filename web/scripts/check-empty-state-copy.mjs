#!/usr/bin/env node
// 空态的 `title` 槽只放短名词短语，句子归 `description`（ADR-0094 §一）。
//
// ## 为什么需要这一道
//
// 第五轮评审实测：61 个 `InlineEmpty` 调用点里只有 9 处传了 `description`，
// **19 处把整句话塞进 `title`**——极端的一条是
// 「The figures below are missing, not zero. Treat this page as blind …」，
// 两句话渲染成一个标题。而那之前 `InlineEmpty` 的标题是 24px（与页面 h1 同级），
// 于是每张空卡都在用页题的音量说「这里什么都没有」。
//
// 排版那一半由组件修掉了（`inline-empty.tsx`），**文案这一半没有任何东西守着**：
// 把句子写回 `title` 不会让任何检查变红，只会让那句话挤在一行小字里。
//
// ## 判据只有一条：`title` 里不许出现句子
//
// 句子的两个可机器判定的信号：**以句号结尾**、或**含破折号从句**。
// 判据刻意**不查长度**——实测过，加「>28 字符」会误伤两条合法的名词短语
// （`No models in this provider's families` 37 字符、
// `You don't have access to this page` 34 字符），
// 而为它们开豁免就等于把判据换成一张允许清单。
// 只查「是不是句子」时，当前 59 个可解析调用点**违规为 0、豁免为 0**。
//
// 同理没有去查「有没有传 description」：空态本来就有不需要多说一句的
// （`No access tiers`），硬性要求会逼出一堆废话。
//
// gate-honesty: 两处跳过，都是**判据自身的边界**，不是「因故不检查」。
//   ① 只检查 `title={t("…")}` 这种静态键；`title={someVar}` 解不出词条，
//      跳过它们不影响结论——结论行报出**参与判定的调用点数**，
//      而不是「全部通过」。今天 0 处动态标题。
//   ② 只读 en 词典：zh 的句号是「。」、破折号是「——」，两种写法都在判据里，
//      但键集由 `Record<XMessageKey, string>` 强制一致，en 判完 zh 不会多出键。
//   两条零匹配守卫：扫到 0 个源文件、或 0 个可解析调用点，一律 exit 1——
//   枚举面塌了会长得像通过，而那正是本轮反复抓到的形状。
//
// **词典不止一本**（ADR-0116 决策 3.1 e：按消费方拆开之后是三本）。三本都要读：
// 下面那句 `if (text === undefined) continue` 对「不在字典里的键」是**静默跳过**的
// ——本来它跳过的只是拼错的键（那是 check-i18n 的判据），可只读核心那一本的话，
// 商业版两个 app 的空态标题会整批落进这条跳过，`checked` 少一截而横幅照打通过。
// 这正是本文件上面那段注释反复讲的同一个形状，只不过这次塌的是词典面而不是文件面。
import { existsSync, readFileSync } from "node:fs";
import { glob } from "node:fs/promises";
import { relative } from "node:path";

import { WEB_ROOT, members } from "./lib/workspace.mjs";

const PACKAGES = members();

/** 句子的两个信号：句末标点、破折号从句。中英两套标点都算。 */
const SENTENCE = /[.。]\s*$|—|——/;

/**
 * en 词典：workspace 成员的 src 下任何 `messages.ts`（`.zh.ts` 是它的对照本，
 * 由 check-i18n-dictionaries 管）。**推导而不是手写**——手写的那份清单正是
 * 上面那段注释在抱怨的东西。
 */
async function dictionaries() {
  const found = [];
  for (const pkg of PACKAGES) {
    for await (const rel of glob("src/**/messages.ts", { cwd: pkg })) {
      found.push(`${pkg}/${rel}`);
    }
  }
  return found.sort();
}

function readDictionary(path) {
  const src = readFileSync(path, "utf8");
  const dict = new Map();
  // 词条值可能跨行 + 用 `+` 拼接，逐条把字符串字面量拼回来
  const re = /^ {2}([A-Za-z][\w]*):\s*((?:"(?:[^"\\]|\\.)*"\s*\+?\s*)+),?\s*$/gm;
  for (const m of src.matchAll(re)) {
    const parts = [...m[2].matchAll(/"((?:[^"\\]|\\.)*)"/g)].map((p) => JSON.parse(`"${p[1]}"`));
    dict.set(m[1], parts.join(""));
  }
  return dict;
}

const DICTIONARIES = await dictionaries();
if (DICTIONARIES.length < 2) {
  console.error(`✗ 只找到 ${DICTIONARIES.length} 本词典——枚举面塌了`);
  process.exit(1);
}
const dict = new Map();
for (const path of DICTIONARIES) {
  const one = readDictionary(path);
  // **逐本判零匹配，不是判合计**：合计非零时某一本整个塌掉看不出来，
  // 而那一本对应的那批空态标题会安静地退出判定面。
  if (one.size === 0) {
    console.error(`✗ ${relative(WEB_ROOT, path)} 解出 0 条——解析器与词典的形状对不上了`);
    process.exit(1);
  }
  for (const [k, v] of one) dict.set(k, v);
}

const violations = [];
let files = 0;
let checked = 0;
let dynamic = 0;

// **枚举面含 packages 全部**（与 check-i18n 同一张面）：判据是「谁渲染空态」，
// 不是「在哪个目录下」。
//
// 两次实测都是同一个形状：ADR-0114 W14 把 gateway 运营页上提成
// `packages/gateway-staff-features`，只扫 apps/ 时读数从 79 文件 / 60 个空态标题
// 掉到 58 / 43；W15 又把 `Forbidden` 收进 `packages/ui`，只扫 `packages/gateway-*`
// 时那一处标题再次悄悄掉出去（60 → 59）。**两次横幅都照打「✔ 通过」**——
// 枚举面塌了长得和干净一模一样，只有结论行里的计数会小一截，
// 而没人会去比对上一次的计数。
//
// 枚举面来自 workspace 成员清单（`./lib/workspace.mjs`），不再是手写的 app 花括号。
// 上面那段说的「新增 app 必须同批加进来」正是它取代的东西：加一个 app 现在自动进面。
const sources = [];
for (const pkg of PACKAGES) {
  if (!existsSync(`${pkg}/src`)) continue;
  for await (const rel of glob("src/**/*.tsx", { cwd: pkg })) {
    sources.push(`${pkg}/${rel}`);
  }
}
for (const abs of sources.sort()) {
  const path = relative(WEB_ROOT, abs);
  if (path.includes(".browser.") || path.includes(".e2e.")) continue;
  files++;
  const src = readFileSync(abs, "utf8");
  for (const m of src.matchAll(/<InlineEmpty\b([\s\S]*?)\/>/g)) {
    const key = /title=\{t\("([^"]+)"/.exec(m[1])?.[1];
    if (!key) {
      dynamic++;
      continue;
    }
    const text = dict.get(key);
    if (text === undefined) continue; // 键不在 en 词典里是 check-i18n 的判据，不是这里的
    checked++;
    if (SENTENCE.test(text)) {
      const line = src.slice(0, m.index).split("\n").length;
      violations.push({ path, line, key, text });
    }
  }
}

if (files === 0 || checked === 0) {
  console.error(`✗ 枚举面塌了：扫到 ${files} 个源文件、${checked} 个可判定调用点`);
  process.exit(1);
}

if (violations.length > 0) {
  console.error(`\x1b[31m✗ ${violations.length} 处空态把句子写进了 title（ADR-0094 §一）：\x1b[0m`);
  for (const v of violations) {
    console.error(`  · ${v.path}:${v.line}  ${v.key}`);
    console.error(`    ${JSON.stringify(v.text)}`);
  }
  console.error("");
  console.error("  title 只放**短名词短语**（「这里本来该有什么」，不带句号从句），");
  console.error("  句子移到 description（「为什么没有」或「下一步做什么」）。");
  console.error("  例：modelsEmpty / modelsEmptyHint 那一对。");
  process.exit(1);
}

console.log(
  `\x1b[32m✔ 空态文案检查通过\x1b[0m\x1b[90m（${files} 个源文件，${checked} 个空态标题参与判定` +
    `${dynamic > 0 ? `，另 ${dynamic} 处标题非静态词条、未参与` : ""}）\x1b[0m`,
);
