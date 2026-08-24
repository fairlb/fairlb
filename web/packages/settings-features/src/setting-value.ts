import { formatNano, mainToNano } from "@fairlb/ui";
import type { SettingEntry, SettingKind as HostSettingKind } from "./host";

/**
 * 设置项的「值 ↔ 编辑态」换算（ADR-0068）。
 *
 * 这是本轮修的那个缺陷所在：此前编辑器把草稿字符串**原样**当 JSON 值提交
 * （`value = draft`），而注册表对 `int` 键的校验是 `json.Unmarshal(raw, &int64)`。
 * 于是 19 个注册键里 10 个整数键一律收到 `"500"` 这样的 JSON 字符串、一律 400，
 * 「保存」按钮点下去只会得到一句校验错误——**能力在、入口在、但这条路走不通**。
 *
 * 抽成纯函数而不是留在组件里，是为了让判据可以被单测直接钉住：
 * 「int 键提交的必须是 JSON number」这句话，在组件里只能靠端到端点一遍才看得见。
 *
 * 判据与服务端 `public/settings/registry.go` 的 `Spec.Validate` 同源——
 * 两侧都拒绝的东西，前端提前拒绝只是省一次往返；两侧判据一旦分家，
 * 表现是「前端说没问题、后端说不行」，那比没有前端校验更难查。
 */

export type SettingKind = HostSettingKind;

/** 编辑态：所有 kind 都用字符串承载，控件形态由 kind 决定。 */
export function toDraft(kind: SettingKind, value: unknown): string {
  if (value === null || value === undefined) return "";
  switch (kind) {
    case "string_list":
      return Array.isArray(value) ? value.join(", ") : "";
    case "map_string_int":
      // 结构化值没有「逗号分隔」这种忠实的扁平写法，直接给 JSON 原文。
      // 拿 key=value 之类的自造语法去糊，只会让人猜转义规则。
      return JSON.stringify(value, null, 2);
    case "bool":
      return value === true ? "true" : "false";
    case "secret":
      // 密钥不回读：草稿永远从空开始，空 = 不动它（见 encodeSecret）。
      return "";
    default:
      return String(value);
  }
}

/**
 * 密钥的三态提交（ADR-0198）：草稿空且未要求清除 = 不动（不进这批）；
 * 草稿非空 = 替换；要求清除 = 写 `""`（服务端把它当作删除）。
 * 返回 null 表示这一键不进批次。
 */
export function encodeSecret(draft: string, clearing: boolean): { value: string } | null {
  if (clearing) return { value: "" };
  const raw = draft.trim();
  if (raw === "") return null;
  return { value: raw };
}

export type EncodeError =
  | { code: "required" }
  | { code: "not_integer" }
  | { code: "not_decimal" }
  | { code: "not_money" }
  | { code: "not_json" }
  // min/max 是给人看的：int 键给整数，money 键给换回主单位的小数串。
  | { code: "out_of_range"; min: number | string; max: number | string };

export type EncodeResult = { ok: true; value: unknown } | { ok: false; error: EncodeError };

/** 十进制数值字符串：与服务端 registry.go 的 decimalPattern 逐字同形（不收科学计数法与前后空白）。 */
const DECIMAL = /^-?\d+(\.\d+)?$/;

/** 整数字面量：`Number()` 会接受 "1e3"、" 12 "、"0x10"，而服务端的 int64 反序列化不接受。 */
const INTEGER = /^-?\d+$/;

/** 金额：无符号、无货币符号、最多九位小数——与服务端 money.ParseDecimalNano 同一条文法再加「不为负」。 */
const MONEY = /^\d+(\.\d{1,9})?$/;

/**
 * encodeValue 把编辑态草稿折成要提交的 JSON 值。
 *
 * 使用契约里的 `min`/`max` 在提交前拒绝越界值。
 */
export function encodeValue(entry: SettingEntry, draft: string): EncodeResult {
  const raw = draft.trim();
  switch (entry.kind) {
    case "int": {
      if (raw === "") return { ok: false, error: { code: "required" } };
      if (!INTEGER.test(raw)) return { ok: false, error: { code: "not_integer" } };
      const n = Number(raw);
      // Number.isSafeInteger 而不是 isInteger：超过 2^53 的十进制串会静默丢精度，
      // 提交上去是一个和用户键入的**不同的数**，而两边都不会报错。
      if (!Number.isSafeInteger(n)) return { ok: false, error: { code: "not_integer" } };
      const range = rangeOf(entry);
      if (range && (n < range.min || n > range.max)) {
        return { ok: false, error: { code: "out_of_range", ...range } };
      }
      return { ok: true, value: n };
    }
    case "bool":
      return { ok: true, value: raw === "true" };
    case "decimal_string": {
      // 金额与汇率禁浮点：这一类的线上形态就是**字符串**，
      // 转成 JSON number 会在小数位上失真。这里只校验形状，不做数值转换。
      if (raw === "") return { ok: false, error: { code: "required" } };
      if (!DECIMAL.test(raw)) return { ok: false, error: { code: "not_decimal" } };
      return { ok: true, value: raw };
    }
    case "money": {
      // 主单位小数串上线（"10.00"），存储仍是 nano——换算在服务端。空串 = 未配置，
      // 与服务端 KindMoney 同一约定，故不判 required。契约里的 min/max 以 nano 计，
      // 这里折成 nano 比较，报错时换回主单位，因为填表的人从未见过 nano。
      if (raw === "") return { ok: true, value: "" };
      if (!MONEY.test(raw)) return { ok: false, error: { code: "not_money" } };
      const range = rangeOf(entry);
      if (range) {
        const n = mainToNano(raw);
        if (n < range.min || n > range.max) {
          return {
            ok: false,
            error: { code: "out_of_range", min: formatNano(range.min), max: formatNano(range.max) },
          };
        }
      }
      return { ok: true, value: raw };
    }
    case "string_list":
      return {
        ok: true,
        value: raw
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
      };
    case "map_string_int": {
      if (raw === "") return { ok: true, value: {} };
      let parsed: unknown;
      try {
        parsed = JSON.parse(raw);
      } catch {
        return { ok: false, error: { code: "not_json" } };
      }
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
        return { ok: false, error: { code: "not_json" } };
      }
      const range = rangeOf(entry);
      for (const v of Object.values(parsed as Record<string, unknown>)) {
        if (typeof v !== "number" || !Number.isSafeInteger(v)) {
          return { ok: false, error: { code: "not_integer" } };
        }
        if (range && (v < range.min || v > range.max)) {
          return { ok: false, error: { code: "out_of_range", ...range } };
        }
      }
      return { ok: true, value: parsed };
    }
    case "secret":
      // 密钥走 encodeSecret（三态）；这里只给一个与其余 kind 同形的退路。
      return { ok: true, value: raw };
    case "email":
      // 空 = 未配置（服务端 KindEmail 显式接受空串），故不判 required。
      // 邮箱形态交给服务端 mail.ParseAddress——在前端重写一遍 RFC 5322 的子集，
      // 只会造出「前端说不合法、后端其实接受」的第二套判据。
      return { ok: true, value: raw };
    case "enum":
    case "string":
    default:
      return { ok: true, value: raw };
  }
}

function rangeOf(entry: SettingEntry): { min: number; max: number } | undefined {
  // 两端都缺省 = 不约束（与服务端 Spec.Range 为 nil 同义）。
  // 单端出现时另一端不能默认成 0——那正是服务端刚修掉的哨兵坑：
  // `Min: 0` 被读成「不约束」，负数一路写进库。
  if (entry.min === undefined && entry.max === undefined) return undefined;
  return {
    min: entry.min ?? Number.MIN_SAFE_INTEGER,
    max: entry.max ?? Number.MAX_SAFE_INTEGER,
  };
}

/** 值有没有被改过。比较折算后的 JSON 而不是草稿串——`"1, 2"` 与 `"1,2"` 是同一个值。 */
export function isDirty(entry: SettingEntry, draft: string): boolean {
  if (entry.kind === "secret") return draft.trim() !== "";
  const encoded = encodeValue(entry, draft);
  if (!encoded.ok) return true; // 非法草稿一定与现值不同，让「保存」可点以便看到错误
  return JSON.stringify(encoded.value) !== JSON.stringify(entry.value ?? null);
}

/**
 * 写入前拦不拦（ADR-0043 §二 的判据，落到 impact 字段上）。
 *
 * kill-switch 例外**不在这里**：它在设置页压根不渲染可写控件（只读 + 指向
 * `/gateway/health`），故不需要在门槛这一层再判一次键名。
 */
export function needsReason(entry: SettingEntry): boolean {
  return entry.impact === "high";
}
