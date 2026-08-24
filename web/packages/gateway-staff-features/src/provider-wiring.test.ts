import { describe, expect, it } from "vitest";
import { mergeProviderRows, providerRowIssue, type ProviderRow } from "./provider-wiring";
import { planSet, rowKey } from "./route-wiring";

const P_A = "00000000-0000-7000-8000-0000000000a1";
const P_B = "00000000-0000-7000-8000-0000000000b1";
const ROUTE_1 = "00000000-0000-7000-8000-000000000001";
const ROUTE_2 = "00000000-0000-7000-8000-000000000002";

const PROVIDERS = [
  { id: P_A, slug: "up-a" },
  { id: P_B, slug: "up-b" },
  { id: "00000000-0000-7000-8000-0000000000c1", slug: "anth-only" },
];

function merged(over: Partial<Parameters<typeof mergeProviderRows>[0]> = {}) {
  return mergeProviderRows({
    routes: [],
    providers: PROVIDERS,
    modelSlug: "openai/gpt-4o",
    checkedOver: new Map(),
    upstreamOver: new Map(),
    errors: new Map(),
    ...over,
  });
}

describe("mergeProviderRows", () => {
  // Same rule as the provider side: what is configured is always present, always
  // ticked, and always first.
  it("puts configured routes first and marks them checked", () => {
    const rows = merged({
      routes: [
        {
          id: ROUTE_1,
          provider_id: P_A,
          provider_slug: "alpha",
          provider_model_id: "gpt-4o",
          enabled: true,
        },
      ],
    });
    expect(rows[0]?.providerId).toBe(P_A);
    expect(rows[0]?.checked).toBe(true);
    expect(rows[0]?.configured).toBe(true);
  });

  // Every provider is a candidate: a model owns no protocol, so an
  // anthropic-only relay is as valid a home for "openai/gpt-4o" as any other --
  // the route is probed on what that relay speaks.
  it("offers every channel, whatever it speaks", () => {
    const rows = merged();
    expect(rows.map((r) => r.providerSlug).sort()).toEqual(["anth-only", "up-a", "up-b"]);
  });

  // A provider already taken by a route gets no second row, or "already served by
  // A" would sit beside an unticked A.
  it("does not list a channel that already has a route", () => {
    const rows = merged({
      routes: [
        {
          id: ROUTE_1,
          provider_id: P_A,
          provider_slug: "alpha",
          provider_model_id: "gpt-4o",
          enabled: true,
        },
      ],
    });
    expect(rows.filter((r) => r.providerId === P_A)).toHaveLength(1);
  });

  // One provider under two aliases is a legal configuration — the unique key is a
  // triple — so both rows must be present.
  it("keeps two aliases on the same channel as two rows", () => {
    const rows = merged({
      routes: [
        {
          id: ROUTE_1,
          provider_id: P_A,
          provider_slug: "alpha",
          provider_model_id: "gpt-4o",
          enabled: true,
        },
        {
          id: ROUTE_2,
          provider_id: P_A,
          provider_slug: "alpha",
          provider_model_id: "gpt-4o-2024-08-06",
          enabled: true,
        },
      ],
    });
    expect(rows.filter((r) => r.providerId === P_A)).toHaveLength(2);
    expect(rows[0]?.key).not.toBe(rows[1]?.key);
  });

  // The upstream name here is **a guess**: prefilled from the last segment of the
  // slug, and overridable.
  it("prefills the upstream name from the slug and lets it be overridden", () => {
    expect(merged()[0]?.upstream).toBe("gpt-4o");
    const key = rowKey(P_A, "gpt-4o");
    const rows = merged({ upstreamOver: new Map([[key, "custom-name"]]) });
    expect(rows.find((r) => r.key === key)?.upstream).toBe("custom-name");
  });

  // A configured route's address comes from the row itself, not from a lookup
  // in the provider list: a provider on a later page of a paginated list is
  // still a live provider.
  it("labels a configured route by the slug the row carries", () => {
    const rows = merged({
      providers: [],
      routes: [
        {
          id: ROUTE_1,
          provider_id: "00000000-0000-7000-8000-0000000000c1",
          provider_slug: "anth-only",
          provider_model_id: "x",
          enabled: true,
        },
      ],
    });
    expect(rows.some((r) => r.providerSlug === "anth-only")).toBe(true);
  });
});

describe("planSet on the model side", () => {
  const row = (over: Partial<ProviderRow>): ProviderRow => ({
    key: "k",
    providerId: P_A,
    providerSlug: "up-a",
    upstream: "gpt-4o",
    routeId: null,
    routeEnabled: false,
    configured: false,
    checked: false,
    ...over,
  });

  // Each of the four cells asserted, **including the two no-op cells** — the same
  // table and the same implementation as the provider side.
  it("creates only for a checked row with no route", () => {
    const p = planSet([row({ checked: true, routeId: null })]);
    expect(p.creates).toHaveLength(1);
    expect(p.deletes).toHaveLength(0);
  });

  it("deletes only for an unchecked row that has a route", () => {
    const p = planSet([row({ checked: false, routeId: ROUTE_1 })]);
    expect(p.deletes).toHaveLength(1);
    expect(p.creates).toHaveLength(0);
  });

  it("does nothing for an unchecked row with no route", () => {
    const p = planSet([row({ checked: false, routeId: null })]);
    expect(p.creates).toHaveLength(0);
    expect(p.deletes).toHaveLength(0);
  });

  it("does nothing for a checked row that already has its route", () => {
    const p = planSet([row({ checked: true, routeId: ROUTE_1 })]);
    expect(p.creates).toHaveLength(0);
    expect(p.deletes).toHaveLength(0);
  });
});

describe("providerRowIssue", () => {
  // A route built from an empty upstream name answers 404 on first use, so a ticked
  // row must have one.
  it("blocks a checked row with an empty upstream name", () => {
    expect(providerRowIssue({ checked: true, upstream: "  " } as ProviderRow)).toBe(true);
    expect(providerRowIssue({ checked: true, upstream: "x" } as ProviderRow)).toBe(false);
    // An unticked row does not block anything: it is never submitted.
    expect(providerRowIssue({ checked: false, upstream: "" } as ProviderRow)).toBe(false);
  });
});
