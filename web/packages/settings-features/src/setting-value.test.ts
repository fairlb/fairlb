import type { SettingEntry } from "./host";
import { describe, expect, it } from "vitest";
import { encodeSecret, encodeValue, isDirty, needsReason, toDraft } from "./setting-value";

function entry(over: Partial<SettingEntry>): SettingEntry {
  return {
    key: "test.key",
    kind: "string",
    group: "operations",
    impact: "normal",
    value: null,
    set: false,
    ...over,
  } as SettingEntry;
}

describe("设置项的值编码", () => {
  // 金额键：主单位小数串上线，存储与范围都是 nano。判据与服务端 KindMoney 同源：
  // 最多九位小数、不为负、无符号；越界按主单位报。
  it("金额键按主单位字符串提交，范围按 nano 比较、按主单位报错", () => {
    const money = entry({ kind: "money", min: 0, max: 1_000_000_000_000_000 });
    expect(encodeValue(money, "10.00")).toEqual({ ok: true, value: "10.00" });
    expect(encodeValue(money, "0.000000001")).toEqual({ ok: true, value: "0.000000001" });
    // 空 = 未配置，与服务端同一约定
    expect(encodeValue(money, "")).toEqual({ ok: true, value: "" });
    for (const bad of ["1e3", "-1", "$10", "10,000", "0.0000000001", "abc", "10."]) {
      expect(encodeValue(money, bad), `应拒绝 ${JSON.stringify(bad)}`).toEqual({
        ok: false,
        error: { code: "not_money" },
      });
    }
    expect(encodeValue(money, "1000000.000000001")).toEqual({
      ok: false,
      error: { code: "out_of_range", min: "0", max: "1000000" },
    });
  });

  // 这条是本轮修的那个缺陷的判据本体。旧实现把草稿原样提交，于是 int 键收到
  // JSON 字符串 `"500"`，服务端 `json.Unmarshal(raw, &int64)` 必然失败——
  // 断言值本身而不是断言「保存成功」：后者在桩不忠实时会假绿。
  it("整数键提交 JSON number，不是字符串", () => {
    const r = encodeValue(entry({ kind: "int" }), "500");
    expect(r).toEqual({ ok: true, value: 500 });
    expect(typeof (r as { value: unknown }).value).toBe("number");
    // 序列化之后也要是裸数字——中间任何一层再包一次引号都等于没修
    expect(JSON.stringify({ value: (r as { value: unknown }).value })).toBe('{"value":500}');
  });

  it("整数键拒绝服务端也会拒的那些形态", () => {
    const int = entry({ kind: "int" });
    for (const bad of ["", " ", "1.5", "abc", "1e3", "0x10", "十"]) {
      expect(encodeValue(int, bad).ok, `应拒绝 ${JSON.stringify(bad)}`).toBe(false);
    }
    // Number() 会把这些收下，正则不收——两侧判据必须与服务端同源
    expect(encodeValue(int, "1e3")).toEqual({ ok: false, error: { code: "not_integer" } });
  });

  it("超出 2^53 的整数按非法处理，而不是静默丢精度", () => {
    // 9007199254740993 存不进 double，Number() 给回 ...992——提交上去是另一个数，
    // 且两侧都不会报错。这是「安静地算错」，比报错更贵。
    const r = encodeValue(entry({ kind: "int" }), "9007199254740993");
    expect(r).toEqual({ ok: false, error: { code: "not_integer" } });
  });

  it("区间在提交前就拦下，且下界为 0 时仍然生效", () => {
    const e = entry({ kind: "int", min: 0, max: 100 });
    expect(encodeValue(e, "-1")).toEqual({
      ok: false,
      error: { code: "out_of_range", min: 0, max: 100 },
    });
    // 服务端刚修掉的哨兵坑的前端镜像：min 为 0 不等于「没有下界」
    expect(encodeValue(e, "0")).toEqual({ ok: true, value: 0 });
    expect(encodeValue(e, "101").ok).toBe(false);
    expect(encodeValue(e, "100")).toEqual({ ok: true, value: 100 });
  });

  it("没有区间时不做范围判断", () => {
    expect(encodeValue(entry({ kind: "int" }), "-999999")).toEqual({ ok: true, value: -999999 });
  });

  it("十进制字符串保持字符串形态——转成 number 会在小数位失真", () => {
    const e = entry({ kind: "decimal_string" });
    expect(encodeValue(e, "7.15")).toEqual({ ok: true, value: "7.15" });
    expect(typeof (encodeValue(e, "7.15") as { value: unknown }).value).toBe("string");
    for (const bad of ["", "7.15e2", "abc", "7,15"]) {
      expect(encodeValue(e, bad).ok, `应拒绝 ${JSON.stringify(bad)}`).toBe(false);
    }
    expect(encodeValue(e, "-0.5")).toEqual({ ok: true, value: "-0.5" });
  });

  it("布尔键提交真布尔", () => {
    expect(encodeValue(entry({ kind: "bool" }), "true")).toEqual({ ok: true, value: true });
    expect(encodeValue(entry({ kind: "bool" }), "false")).toEqual({ ok: true, value: false });
  });

  it("字符串列表按逗号切分并丢掉空项", () => {
    expect(encodeValue(entry({ kind: "string_list" }), "a.com, b.com , ,c.com")).toEqual({
      ok: true,
      value: ["a.com", "b.com", "c.com"],
    });
    expect(encodeValue(entry({ kind: "string_list" }), "")).toEqual({ ok: true, value: [] });
  });

  it("映射键要合法 JSON 对象，且值必须是整数", () => {
    const e = entry({ kind: "map_string_int", min: 0, max: 100 });
    expect(encodeValue(e, '{"a": 50}')).toEqual({ ok: true, value: { a: 50 } });
    expect(encodeValue(e, "[1,2]").ok).toBe(false); // 数组不是映射
    expect(encodeValue(e, "{").ok).toBe(false);
    expect(encodeValue(e, '{"a": "50"}')).toEqual({
      ok: false,
      error: { code: "not_integer" },
    });
    expect(encodeValue(e, '{"a": 101}').ok).toBe(false);
    expect(encodeValue(e, '{"a": -1}').ok).toBe(false);
  });

  it("邮箱允许空串（= 未配置），形态交给服务端判", () => {
    expect(encodeValue(entry({ kind: "email" }), "")).toEqual({ ok: true, value: "" });
    expect(encodeValue(entry({ kind: "email" }), "ops@example.com")).toEqual({
      ok: true,
      value: "ops@example.com",
    });
  });
});

describe("编辑态回显", () => {
  it("各 kind 的回显与编码互为逆运算", () => {
    const cases: Array<[SettingEntry, unknown]> = [
      [entry({ kind: "int" }), 500],
      [entry({ kind: "bool" }), true],
      [entry({ kind: "bool" }), false],
      [entry({ kind: "decimal_string" }), "7.15"],
      [entry({ kind: "string_list" }), ["a", "b"]],
      [entry({ kind: "map_string_int" }), { a: 1, b: 2 }],
      [entry({ kind: "string" }), "hello"],
    ];
    for (const [e, value] of cases) {
      const round = encodeValue({ ...e, value }, toDraft(e.kind, value));
      expect(round, `${e.kind} 的往返`).toEqual({ ok: true, value });
    }
  });

  it("空值回显成空串而不是字面量 null", () => {
    expect(toDraft("string", null)).toBe("");
    expect(toDraft("int", undefined)).toBe("");
  });
});

describe("脏判定与写入门槛", () => {
  it("按折算后的值判脏，不按草稿串", () => {
    const e = entry({ kind: "string_list", value: ["a", "b"] });
    expect(isDirty(e, "a, b")).toBe(false);
    expect(isDirty(e, "a,b")).toBe(false); // 同一个值的两种写法
    expect(isDirty(e, "a, b, c")).toBe(true);
  });

  it('整数的 500 与 "500" 不算同一个值——正是这个区分让缺陷可见', () => {
    expect(isDirty(entry({ kind: "int", value: 500 }), "500")).toBe(false);
    expect(isDirty(entry({ kind: "int", value: 500 }), "600")).toBe(true);
  });

  it("草稿非法时视为脏，让保存可点以便看见错误", () => {
    expect(isDirty(entry({ kind: "int", value: 500 }), "abc")).toBe(true);
  });

  it("高影响键要原因，普通键不要", () => {
    expect(needsReason(entry({ impact: "high" }))).toBe(true);
    expect(needsReason(entry({ impact: "normal" }))).toBe(false);
  });
});

describe("密钥键的三态（ADR-0198）", () => {
  it("草稿空且未要求清除 = 不进批次；非空 = 替换；清除 = 写空串", () => {
    expect(encodeSecret("", false)).toBeNull();
    expect(encodeSecret("   ", false)).toBeNull();
    expect(encodeSecret("sk-new", false)).toEqual({ value: "sk-new" });
    expect(encodeSecret("ignored", true)).toEqual({ value: "" });
  });

  it("密钥的草稿永远从空开始，空草稿不算脏", () => {
    const secret = entry({ kind: "secret", set: true, hint: "sk-…cdef" });
    expect(toDraft("secret", undefined)).toBe("");
    expect(isDirty(secret, "")).toBe(false);
    expect(isDirty(secret, "sk-new")).toBe(true);
  });
});
