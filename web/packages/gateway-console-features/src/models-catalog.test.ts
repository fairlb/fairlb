import type { GatewayConsoleTypes } from "@fairlb/api-client";
import { describe, expect, it } from "vitest";
import { creatorOf, filterModels, groupByCreator } from "./models";

function m(
  slug: string,
  protocols: string[],
  display_name?: string,
): GatewayConsoleTypes.AvailableModel {
  return {
    slug,
    protocols,
    display_name,
    endpoints: ["chat"],
  } as GatewayConsoleTypes.AvailableModel;
}

describe("filterModels", () => {
  const catalog = [
    m("openai/gpt-5.4", ["openai"], "GPT 5.4"),
    // Reachable on both protocols: the same slug wired to two providers.
    m("anthropic/claude-opus-5", ["anthropic", "openai"], "Claude Opus 5"),
  ];

  it("returns everything for a blank query", () => {
    expect(filterModels(catalog, "")).toHaveLength(2);
    expect(filterModels(catalog, "   ")).toHaveLength(2);
  });

  it("matches slug, display name and any protocol, case-insensitively", () => {
    expect(filterModels(catalog, "OPUS")).toHaveLength(1);
    expect(filterModels(catalog, "gpt 5.4")).toHaveLength(1); // display name
    expect(filterModels(catalog, "anthropic")).toHaveLength(1); // slug and protocol
    // A model reachable on several protocols matches on each of them.
    expect(filterModels(catalog, "openai")).toHaveLength(2);
  });

  it("returns empty rather than everything when nothing matches", () => {
    // "No match, so fall back to everything" is the classic way to get this wrong,
    // and it makes the search box look permanently broken.
    expect(filterModels(catalog, "zzz")).toHaveLength(0);
  });
});

describe("groupByCreator", () => {
  it("groups by the slug's creator segment and orders groups deterministically", () => {
    const a = groupByCreator([m("o/1", ["openai"]), m("a/1", ["anthropic"]), m("o/2", ["openai"])]);
    const b = groupByCreator([m("a/1", ["anthropic"]), m("o/1", ["openai"]), m("o/2", ["openai"])]);
    expect(a.map(([c]) => c)).toEqual(["a", "o"]);
    // Group order must not follow input order: the same catalog re-ordering itself
    // under a different filter reads as if its contents had changed.
    expect(a.map(([c]) => c)).toEqual(b.map(([c]) => c));
  });

  it("groups by creator, not by protocol: a model reachable on two protocols sits in one group", () => {
    // The slug convention puts the creator first, and a model owns no protocol, so
    // the creator is the one grouping that stays put when a second provider is wired.
    const groups = groupByCreator([
      m("anthropic/claude", ["anthropic", "openai"]),
      m("anthropic/haiku", ["openai"]),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]![0]).toBe("anthropic");
  });

  it("keeps the server's order within a group", () => {
    const [, items] = groupByCreator([m("o/b", ["openai"]), m("o/a", ["openai"])])[0]!;
    expect(items.map((x) => x.slug)).toEqual(["o/b", "o/a"]);
  });

  it("puts bare slugs last, under one heading, and loses no models", () => {
    const all = [m("bare", ["openai"]), m("a/1", ["anthropic"]), m("g/1", ["gemini"])];
    const groups = groupByCreator(all);
    expect(groups.map(([c]) => c)).toEqual(["a", "g", ""]);
    expect(groups.reduce((n, [, items]) => n + items.length, 0)).toBe(all.length);
    expect(creatorOf("bare")).toBe("");
    expect(creatorOf("/bare")).toBe("");
  });
});
