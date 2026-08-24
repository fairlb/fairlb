#!/usr/bin/env node
// 这个文件被删过一次：`045c13ae "refactor: split Cloud and Community modules"`
// 一并删掉了 web/scripts/ 下 13 个 check-*.mjs，只有 check-app-bundle.mjs 回来了。
// 它的枚举走 `git ls-files` + 仓库根，与前端目录结构无关，所以恢复时**一行判据都没改**
// ——它当场在今天的 194 个 .md 上跑通。也就是说它空缺的这段时间里，
// 没有任何东西在拦下面这两类缺陷。
//
// 住在 `public/web/scripts/` 而不是 `deploy/scripts/`：它需要 markdown-it，
// 而那是 `public/web` 的 devDependency。判据必须是**真渲染出来的 token**，
// 正则替代不了——这两类缺陷的定义就是「源码看着对、渲染出来不对」。
//
// 它自己的量具自检本来就是无条件跑的，恢复时没有动；`--self-test` 只是补一个
// 与其他闸一致的入口点。
// markdown 渲染门禁：拦「源码里看着对、渲染出来不对」的块级缺陷。
//
// 目前两条（都实测发生过，不是假想）：
//   ① `---` / `===` 紧贴上一行 → 上一段被吞成 setext 标题，分隔线消失
//   ② 不从 1 开头的有序列表紧贴上一段 → 整串被吸进段落，编号消失（#467）
//
// 两条同源：**块级结构的成立与否取决于上一行是不是空行**，而这一点读原文看不出来。
// 判据一律是「真渲染出来的 token」，不是正则。
//
// ## 缺陷机理
//
// `---` 跟在**非空行**后面时，优先解析成 setext 标题的下划线，而不是分隔线
// （thematic break）。后果有两个，且都在原文里隐形：
//   ① 整个前置段落块变成 <h2>——正文突然成了标题，还进目录
//   ② 那条分隔线本身消失
// 两种写法只差一个空行，读 .md 原文完全看不出来。
//
// ## 为什么要有这道闸
//
// 同一形状连续两个 PR 出现两次。#451 手工修掉一处，commit message 里立了
// 「全仓 .md 扫过，只有这一处」的账——**那个说法当时是对的**
// （`git show a910811:docs/PROGRESS.md` 复核确实为空）。紧接着的 #452 又引入一处，
// 由 #457 修。所以这不是谁不够仔细：**手工扫描 + commit message 立账挡不住回流**，
// 因为 `make verify` 的八步里没有任何一步碰 markdown 渲染。
//
// ## 判据为什么不是正则
//
// 「量具本身要能自检」是本仓的旧账——颜色断言那次把 oklch 的 L/C/H 当成 RGB 读，
// 错得像「测出了真问题」。正则要判「这一行前面是不是非空行、是不是在代码块里、
// 是不是表格的一部分」，等于重写一遍解析器。这里直接用 markdown-it 真渲染，
// 判据是解析出的 token：setext 那条看有没有 `heading_open` 且 `markup` 是 `-`/`=`；
// 列表那条看「像编号的行」有没有落在 `list_item_open` 的 map 里。
//
// preset 取 `default` 而不是 `commonmark`：两者对本缺陷的判定一致，但严格 CommonMark
// **没有表格**，于是「GFM 表格后紧跟 ---」会被误判成 setext（实测同一段输入
// commonmark 命中 1、default 命中 0，而 GitHub 按 GFM 渲染成表格 + 分隔线）。
// 本仓文档表格用得多（CLAUDE.md、DESIGN），这不是假想的假阳性；
// 这些文档实际被阅读的渲染器就是 GitHub 的 GFM，量具该对齐它。
//
// gate-honesty: 本脚本无跳过路径。markdown-it 缺失时**失败而不是跳过**——verify
//   第 3 步的 check-generate 已经 `cd web && pnpm generate`，前端工具链本就是该步的
//   前提，这里不新增要求。三道自我诚实守卫：① 自检十一条已知答案（setext 五条、
//   列表被吞六条），不符即退出、不进入扫描（量具坏了不许报绿）——其中四条是**反例**，
//   钉住「紧贴段落也照常渲染」的写法不许被判成缺陷；② 扫到 0 个 .md 即失败（枚举面
//   写错不能长得像通过）；③ 索引里有、磁盘上没有的文件**一律失败**，不是记个数照打
//   通过——「检查了 5 个、跳过 92 个」和「全查过了」不能是同一盏绿灯。
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import MarkdownIt from "markdown-it";

const md = new MarkdownIt("default");

// 调 git 一律用剥掉 GIT_* 的环境。
//
// **git 跑钩子时会设 `GIT_DIR`，而 `GIT_WORK_TREE` 缺省取 cwd**——于是
// `git rev-parse --show-toplevel` 返回的不再是仓库根，而是**当前目录**。
// `make check-markdown` 会先 `cd web`，所以 pre-push 里 root 被解析成 `<仓库>/web`，
// `ls-files` 仍列出 97 条仓库根相对路径，其中只有 5 条在 `web/` 下存在——
// 门禁于是只查了 5 个文件，**却照打 ✔**（#465 合入后第一次真从钩子跑就撞上，
// 输出是「扫描 5 个 .md，另有 92 个……未检查」）。
// 剥掉 GIT_* 后 git 回到从 cwd 向上正常发现，`web/` 的上一层就是仓库根。
const gitEnv = Object.fromEntries(
  Object.entries(process.env).filter(([k]) => !k.startsWith("GIT_")),
);
// stderr 丢弃：没有仓库时 git 会先向 stderr 写一句 "not a git repository"，
// 而那条路径已经有回退了——把它印出来只会让一次正常的验收看起来像出了事。
const git = (args) =>
  execFileSync("git", args, {
    encoding: "utf8",
    env: gitEnv,
    stdio: ["ignore", "pipe", "ignore"],
  });

/**
 * 剥掉文件头的 YAML frontmatter，返回 [正文行数组, 行号偏移]。
 *
 * 必须剥：frontmatter 的收尾 `---` 天生紧贴非空行，不剥的话每个带 frontmatter 的
 * 文件都会被判成缺陷（实测 `---\ntitle: x\n---` 解析成 hr + setext h2）。本仓有
 * 6 处：营销站 3 个 content collection、blog/why-a-gateway.md、
 * changelog/2026-07-28.md、.claude/skills/kumo-design/SKILL.md。
 *
 * 只在首行恰为 `---` 且后面找得到闭合行时才剥。找不到闭合就不是 frontmatter，
 * 原样交给解析器——那种情况下的 `---` 反而可能真是缺陷。
 */
function stripFrontMatter(lines) {
  if (lines[0] !== "---") return [lines, 0];
  for (let i = 1; i < lines.length; i++) {
    if (lines[i] === "---" || lines[i] === "...") return [lines.slice(i + 1), i + 1];
  }
  return [lines, 0];
}

/** 把源码切成 [正文行数组, 行号偏移, token 流]，三个检查共用 */
function parseBody(source) {
  const [body, offset] = stripFrontMatter(source.replace(/\r\n/g, "\n").split("\n"));
  return [body, offset, md.parse(body.join("\n"), {})];
}

/**
 * 一个文件里所有「被 setext 下划线吞掉」的位置（行号 1-based、相对全文）。
 *
 * `-` 和 `=` 都收：机理完全相同（下划线优先于分隔线/正文），只差吞出来的是 H2 还是 H1。
 * 本仓 ATX 标题一统，实测全仓 setext 命中为 0，所以收 `=` 不会误伤「有意为之的
 * setext 标题」——真要有人想用 setext，那是另一个决定，届时这道闸会当场叫出来。
 */
function setextHits(source) {
  const [, offset, tokens] = parseBody(source);
  return tokens
    .filter((t) => t.type === "heading_open" && (t.markup === "-" || t.markup === "="))
    .map((t) => ({
      // token.map 是正文里的 [起始行, 结束行)，0-based
      paragraph: t.map[0] + offset + 1, // 被吞掉的段落首行
      underline: t.map[1] + offset, // 那条 `---` / `===` 所在行
      tag: t.tag, // h2（`---`）或 h1（`===`）
    }));
}

/**
 * 一个文件里所有「有序列表被吸进上一段」的位置（行号 1-based、相对全文）。
 *
 * ## 机理
 *
 * CommonMark 只允许**从 1 开头**的有序列表打断段落。写 `4.` 紧贴上一段时，
 * 整串 4/5/6/7 不会成为列表，而是被当作上一段的续行——编号退化成字面文本，
 * 几条内容挤成一段跑掉的连排文字。和 setext 那条同源：源码里看着是列表，
 * 渲染出来不是，读原文查不出来。
 *
 * ## 为什么补这一条
 *
 * 不是假想。#467 之前 PROGRESS.md 里就有一处真的这样（`*需要你的资源或决策：*`
 * 后面紧跟 `4.`，四条决策项全被吞）。ADR-0066 落地时这道闸只查 setext，
 * 于是那处缺陷从闸下走过去了——补上之前，同一个形状随时会再回来。
 *
 * ## 判据边界
 *
 * 只在**上一行非空**时判缺陷：上面有空行还不成列表，那是别的原因（比如缩进进了
 * 别的块），不归这条管。围栏/缩进代码块里的 `3. foo` 由 fence/code_block 的 map
 * 排除，不误报。同一段里被吞的多条只报一次——四条各报一遍会让人以为要改四个地方，
 * 实际只需要在第一条前面补一个空行。
 */
function listSwallowHits(source) {
  const [lines, offset, tokens] = parseBody(source);

  // 已经是列表项、或落在代码块里的行，都不算被吞
  const covered = new Set();
  for (const t of tokens) {
    if (!t.map) continue;
    if (t.type !== "list_item_open" && t.type !== "fence" && t.type !== "code_block") continue;
    for (let l = t.map[0]; l < t.map[1]; l++) covered.add(l);
  }
  // 行 → 它所属段落的起始行，用来把同一段里的多条折成一条
  const paragraphOf = new Map();
  for (const t of tokens) {
    if (t.type !== "paragraph_open" || !t.map) continue;
    for (let l = t.map[0]; l < t.map[1]; l++) paragraphOf.set(l, t.map[0]);
  }

  const hits = [];
  const seenParagraph = new Set();
  for (let i = 1; i < lines.length; i++) {
    if (!/^ {0,3}\d{1,9}[.)][ \t]+\S/.test(lines[i])) continue;
    if (covered.has(i)) continue; // 真渲染成列表了，或在代码块里
    if (lines[i - 1].trim() === "") continue; // 上面有空行 → 另有原因，不归这条管
    const para = paragraphOf.get(i);
    if (para !== undefined && seenParagraph.has(para)) continue; // 同一段只报首条
    if (para !== undefined) seenParagraph.add(para);
    hits.push({
      line: i + offset + 1,
      marker: lines[i].trim().slice(0, 24),
      paragraph: (para ?? i) + offset + 1,
    });
  }
  return hits;
}

// 自检：喂已知答案，量具坏了必须自己叫出来。头两条是最小对照——同样的三行内容，
// 只差一个空行，必须渲染出**不同**结果；两者要是给出同一个答案，这道闸就是摆设。
// 后两条钉住 frontmatter：剥少了会满仓假阳性，剥多了或偏移算错会指错行号。
const SELF_CHECK = [
  ["紧贴上一行 → setext H2", "段落\n---\n", [{ paragraph: 1, underline: 2, tag: "h2" }]],
  ["空行分隔 → 分隔线", "段落\n\n---\n", []],
  ["`===` 紧贴上一行 → setext H1", "段落\n===\n", [{ paragraph: 1, underline: 2, tag: "h1" }]],
  ["frontmatter 收尾不算缺陷", "---\ntitle: x\n---\n\n# 标题\n", []],
  [
    "frontmatter 之后仍能定位",
    "---\ntitle: x\n---\n\n正文\n---\n\n更多\n",
    [{ paragraph: 5, underline: 6, tag: "h2" }],
  ],
];

for (const [name, source, want] of SELF_CHECK) {
  const got = setextHits(source);
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    console.error(`✗ 自检未通过：${name}`);
    console.error(`    期望 ${JSON.stringify(want)}`);
    console.error(`    实得 ${JSON.stringify(got)}`);
    console.error("  量具失灵，这次运行不产出结论——绿灯不作数。");
    process.exit(1);
  }
}

// 列表被吞那条的自检。前两条是最小对照：同样四行，只差一个空行，必须给出**不同**答案。
// 后四条钉住边界——这几条不是凑数的，写第一版规则时我凭推断以为「表格/无序列表/`1.`
// 开头的有序列表紧贴段落」也会坏，实测下来它们**都照常渲染**，那一版的「坏样本」
// 其实全是好样本。判据必须锚在实测行为上，所以把这些反例固化进自检。
const LIST_SELF_CHECK = [
  ["`4.` 紧贴段落 → 被吞", "段落\n4. 甲\n5. 乙\n", [{ line: 2, paragraph: 1 }]],
  ["空行分隔 → 正常列表", "段落\n\n4. 甲\n5. 乙\n", []],
  ["`1.` 开头能打断段落 → 不是缺陷", "段落\n1. 甲\n2. 乙\n", []],
  ["无序列表能打断段落 → 不是缺陷", "段落\n- 甲\n- 乙\n", []],
  ["围栏代码块里的编号 → 不是缺陷", "段落\n```\n4. 甲\n```\n", []],
  ["同一段多条只报一次", "段落\n4. 甲\n5. 乙\n6. 丙\n", [{ line: 2, paragraph: 1 }]],
];

for (const [name, source, want] of LIST_SELF_CHECK) {
  const got = listSwallowHits(source).map(({ line, paragraph }) => ({ line, paragraph }));
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    console.error(`✗ 自检未通过：${name}`);
    console.error(`    期望 ${JSON.stringify(want)}`);
    console.error(`    实得 ${JSON.stringify(got)}`);
    console.error("  量具失灵，这次运行不产出结论——绿灯不作数。");
    process.exit(1);
  }
}

/**
 * `--self-test` 在这里只是一个**入口点**，不是新的自检。
 *
 * 本文件的量具自检（setext 五条 + 列表被吞六条，共十一条已知答案）在上面**无条件**
 * 跑过了——那比一个要靠调用方记得传的旗标更强，它没法被跳过。加这个旗标只为与
 * check-brand-assets / check-ui-conventions 的两行 Makefile 写法保持一致：
 * 先证明闸会红，再拿它去判仓库。
 *
 * 实测（把 setextHits 改成恒返回空数组）：上面那段自检当场报
 * 「量具失灵，这次运行不产出结论——绿灯不作数」并退 1。
 *
 * 它必须站在**枚举之前**：这一段报的是上面那十一条夹具，而那些夹具不需要
 * 仓库。放在枚举之后的话，`--self-test` 会白跑一次 `git ls-files` 加全量 stat，
 * 而且在没有 .git 的地方（发布用的 tarball、fetch 之前的 CI 阶段）会死在一条
 * 与自检无关的 git 报错上。
 */
if (process.argv.includes("--self-test")) {
  console.log("v markdown-render self-test ok (11 known answers, checked unconditionally above)");
  process.exit(0);
}

// 枚举走 git ls-files 而不是 find / shell glob 递归：仓库根的 .claude/worktrees/ 下
// 挂着其他分支的检出，递归扫描会穿透进去（#348 同源）。--others --exclude-standard
// 把尚未 git add 的新文件也纳入，「先跑 verify 再 add」的顺序不留盲区；
// 冲突状态下 --cached 会把同一路径的多个 stage 各列一次，故去重。
//
// **git 不在时要能走另一条路**，而不是炸掉。`verify-public` 把 `public/` 复制到一个
// 临时目录再验收，那份副本没有 .git；发布给自托管者的 tarball 也没有。回退走文件系统
// 遍历，以本树为根（脚本在 `<tree>/web/scripts/` 下）——上面那段担心的
// `.claude/worktrees/` 只存在于仓库根，而回退路径只在没有仓库的地方生效，那里也就
// 没有别的检出可穿透。两条路的枚举面不同是对的：在仓库里它该看全仓的 markdown，
// 在 Community 副本里它该看 Community 的。
//
// 回退**只接「这里不是仓库」这一种失败**。git 装坏了、权限不对、ls-files 自己出错，
// 都应当停下来说话，而不是安静地换成一个更窄的扫描面然后照打通过——那正是这道闸
// 存在要防的形态。走了哪条路也印在结论里，否则「扫了 21 个」与「扫了 194 个」
// 长得一样正常。
function enumerate() {
  let top;
  try {
    top = git(["rev-parse", "--show-toplevel"]).trim();
  } catch (error) {
    const status = /** @type {{ status?: number }} */ (error).status;
    // git 用 128 表示「不是仓库」。其余退出码与 spawn 失败（git 不存在）一律抛出。
    if (status !== 128) throw error;
    const tree = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
    const skip = new Set(["node_modules", "dist", ".git", ".claude", ".astro", "coverage"]);
    const out = [];
    const walk = (dir, prefix) => {
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        if (entry.isDirectory()) {
          if (!skip.has(entry.name)) walk(join(dir, entry.name), `${prefix}${entry.name}/`);
        } else if (entry.name.endsWith(".md")) {
          out.push(`${prefix}${entry.name}`);
        }
      }
    };
    walk(tree, "");
    return { root: tree, how: "文件系统遍历（此处不是 git 仓库）", files: out.sort() };
  }
  return {
    root: top,
    how: "git ls-files",
    files: [
      ...new Set(
        git(["-C", top, "ls-files", "--cached", "--others", "--exclude-standard", "--", "*.md"])
          .split("\n")
          .filter(Boolean),
      ),
    ],
  };
}

const { root, how, files: listed } = enumerate();

// 自我诚实守卫：枚举面塌了不能长得像通过
if (listed.length === 0) {
  console.error(`✗ 一个 .md 都没扫到（${how}，根 ${root}）——枚举面塌了，不是通过`);
  process.exit(1);
}

// 自我诚实守卫：列出来却读不到的文件**必须失败**。
// 第一版只是记个数、照打 ✔，结果 GIT_DIR 那个 bug 让它「查了 5 个、跳过 92 个」还是绿灯
// ——正是 ADR-0042 点名的形态。两种成因都该停下来看：root 解错（本闸自己坏了），
// 或文件删了还没 stage（那就 stage 掉再跑）。
const missing = listed.filter((rel) => !existsSync(join(root, rel)));
if (missing.length > 0) {
  console.error(
    `✗ 索引里有 ${listed.length} 个 .md，其中 ${missing.length} 个在磁盘上读不到——` +
      `本次只能检查 ${listed.length - missing.length} 个，这不算通过`,
  );
  for (const rel of missing.slice(0, 5)) console.error(`    ${rel}`);
  if (missing.length > 5) console.error(`    …… 另 ${missing.length - 5} 个`);
  console.error(
    `\n  解析出的仓库根：${root}\n` +
      "  两种成因：① 这个根不对——`git rev-parse --show-toplevel` 在 GIT_DIR 已设时\n" +
      "  返回的是 cwd 而非仓库根（git 跑钩子时必设），本脚本已剥掉 GIT_* 规避，\n" +
      "  若仍出现说明规避失效；② 文件删了还没 stage——`git add -A` 后重跑。",
  );
  process.exit(1);
}

const files = listed;

const problems = [];
let sawSetext = false;
let sawList = false;
for (const rel of files) {
  const source = readFileSync(join(root, rel), "utf8");
  for (const hit of setextHits(source)) {
    sawSetext = true;
    const span =
      hit.paragraph === hit.underline - 1
        ? `第 ${hit.paragraph} 行`
        : `第 ${hit.paragraph}–${hit.underline - 1} 行`;
    const rule = hit.tag === "h1" ? "`===`" : "`---`";
    problems.push(`${rel}:${hit.underline} —— ${span}整段被吞成 <${hit.tag}>，这条 ${rule} 消失`);
  }
  for (const hit of listSwallowHits(source)) {
    sawList = true;
    problems.push(
      `${rel}:${hit.line} —— 「${hit.marker}」起的有序列表被吸进第 ${hit.paragraph} 行那一段，编号消失`,
    );
  }
}

if (problems.length > 0) {
  console.error(`✗ markdown 渲染检查未通过（${problems.length} 处）：`);
  for (const p of problems) console.error(`  · ${p}`);
  if (sawSetext) {
    console.error(
      "\n  机理（setext）：`---` / `===` 紧跟非空行时是 setext 标题的下划线，不是分隔线——\n" +
        "  上面那一段会整段变成标题，分隔线本身消失。原文里看不出来，得渲染才知道。\n" +
        "  修法：在下划线前面补一个空行。",
    );
  }
  if (sawList) {
    console.error(
      "\n  机理（列表被吞）：CommonMark 只允许**从 1 开头**的有序列表打断段落。写 `4.`\n" +
        "  紧贴上一段时，整串编号不成列表，而是被当成那一段的续行，编号退化成字面文本。\n" +
        '  修法：在第一条编号前面补一个空行（补完仍会渲染成 <ol start="4">，续编号不丢）。',
    );
  }
  process.exit(1);
}

console.log(`✔ markdown 渲染检查通过（${how}，扫描 ${files.length} 个 .md，每一个都检查了）`);
