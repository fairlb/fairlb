import { describe, expect, it } from "vitest";
import { vendorMonogram } from "./vendor-mark";

// The catalog slugs this build ships, from
// `internal/gateway/catalog/vendors.go`. The frontend learns them at runtime
// from the registry endpoint, so there is nothing to import here; keep this
// list in sync when a vendor is added, because the uniqueness assertion below
// is what stops the new tile from being an invisible duplicate of a sibling.
const CATALOG_SLUGS = [
  "openai",
  "anthropic",
  "google",
  "google-vertex",
  "azure-openai",
  "aws-bedrock",
  "xai",
  "mistral",
  "openrouter",
  "groq",
  "deepseek",
  "alibaba",
  "moonshot",
  "zhipu",
  "volcengine",
  "baidu",
  "tencent",
  "minimax",
  "custom",
] as const;

describe("vendorMonogram", () => {
  it("gives every catalog vendor a distinct tile", () => {
    const byMonogram = new Map<string, string[]>();
    for (const slug of CATALOG_SLUGS) {
      const monogram = vendorMonogram(slug);
      byMonogram.set(monogram, [...(byMonogram.get(monogram) ?? []), slug]);
    }
    const collisions = [...byMonogram].filter(([, slugs]) => slugs.length > 1);
    expect(collisions).toEqual([]);
    expect(byMonogram.size).toBe(CATALOG_SLUGS.length);
  });

  it("reads the first letter of each segment for platform vendors", () => {
    expect(vendorMonogram("google-vertex")).toBe("GV");
    expect(vendorMonogram("azure-openai")).toBe("AO");
    expect(vendorMonogram("aws-bedrock")).toBe("AB");
  });

  it("separates the pairs a single letter would merge", () => {
    expect(vendorMonogram("openai")).toBe("OP");
    expect(vendorMonogram("openrouter")).toBe("OR");
    expect(vendorMonogram("mistral")).toBe("MI");
    expect(vendorMonogram("minimax")).toBe("MM");
  });

  it("stays on the slug, so a Chinese-first label never reaches the tile", () => {
    // 阿里云百炼 / 智谱 AI / 火山方舟 / 百度千帆 / 腾讯混元 are the catalog
    // labels; the display face ships Latin only.
    expect(vendorMonogram("alibaba")).toBe("AL");
    expect(vendorMonogram("zhipu")).toBe("ZH");
    expect(vendorMonogram("volcengine")).toBe("VO");
  });

  it("answers for a vendor this build has never heard of", () => {
    expect(vendorMonogram("some-new-vendor")).toBe("SN");
    expect(vendorMonogram("")).toBe("?");
  });
});
