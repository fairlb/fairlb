import { describe, expect, it } from "vitest";
import {
  canPriceFromReference,
  canWire,
  checkedOf,
  composerIssue,
  discoverSummary,
  draftIssue,
  newModelDraft,
  initiallyChecked,
  mergeRows,
  planWiring,
  referencePriceTargets,
  resolveModelForUpstream,
  rowKey,
  upstreamFromSlug,
  type DiscoveredLike,
  type ModelLike,
  type RouteLike,
  type WiringRow,
  type WiringRowView,
} from "./route-wiring";

const MODEL_A = "00000000-0000-7000-8000-00000000000a";
const MODEL_B = "00000000-0000-7000-8000-00000000000b";
const ROUTE_1 = "00000000-0000-7000-8000-000000000001";
const ROUTE_2 = "00000000-0000-7000-8000-000000000002";

const OPENAI: ModelLike = { id: MODEL_A, slug: "openai/gpt-4o" };
const CLAUDE: ModelLike = { id: MODEL_B, slug: "anthropic/claude-sonnet-4" };
const CATALOG = new Map([
  [MODEL_A, OPENAI],
  [MODEL_B, CLAUDE],
]);

// 名字随路由返回（服务端是内连接），夹具照办：默认按 model_id 从
// CATALOG 取，这样用例读起来仍是「这条路由指向那个模型」，而不必逐处手写。
function route(over: Partial<RouteLike> & Pick<RouteLike, "id" | "model_id">): RouteLike {
  const known = CATALOG.get(over.model_id);
  return {
    provider_model_id: "gpt-4o",
    enabled: true,
    model_slug: known?.slug ?? "",
    ...over,
  };
}

function merged(over: Partial<Parameters<typeof mergeRows>[0]> = {}) {
  return mergeRows({
    routes: [],
    discovered: null,
    complete: false,
    manual: [],
    ...over,
  });
}

/** Only the fields the tests care about. */
function view(over: Partial<WiringRowView> & Pick<WiringRowView, "checked">): WiringRowView {
  return {
    key: "k",
    upstream: "gpt-4o",
    modelId: MODEL_A,
    slug: "openai/gpt-4o",
    routeId: null,
    routeEnabled: false,
    origin: "upstream",
    onUpstream: null,
    discoveredState: null,
    ...over,
  };
}

describe("rowKey", () => {
  // The separator must be a character an upstream name cannot contain. Joined with
  // a slash these two pairs collapse onto the same string, and they are two
  // different configurations.
  it("does not fake a collision when the separator appears inside the ids", () => {
    expect(rowKey(`${MODEL_A}/x`, "y")).not.toBe(rowKey(MODEL_A, "x/y"));
  });

  it("gives the unknown state a key of its own", () => {
    expect(rowKey(null, "gpt-4o")).not.toBe(rowKey(MODEL_A, "gpt-4o"));
  });
});

describe("mergeRows", () => {
  // "What is configured today must be in the list, and be selected" lands here.
  it("puts existing routes first and marks them checked", () => {
    const rows = merged({
      routes: [
        route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" }),
        route({ id: ROUTE_2, model_id: MODEL_B, provider_model_id: "claude-sonnet-4" }),
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows.every((r) => r.routeId !== null)).toBe(true);
    expect(rows.every(initiallyChecked)).toBe(true);
    // The model name is joined in from the catalog: the routes contract
    // deliberately does not carry a slug.
    expect(rows[0]?.slug).toBe("openai/gpt-4o");
  });

  // An implementation keyed on the model alone must fail this one.
  it("keeps one model's two upstream aliases as two rows", () => {
    const rows = merged({
      routes: [
        route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" }),
        route({ id: ROUTE_2, model_id: MODEL_A, provider_model_id: "gpt-4o-2024-08-06" }),
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows[0]?.key).not.toBe(rows[1]?.key);
  });

  // An implementation keyed on the upstream name alone must fail this one.
  it("keeps one upstream id pointed at two models as two rows", () => {
    const rows = merged({
      routes: [
        route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "shared" }),
        route({ id: ROUTE_2, model_id: MODEL_B, provider_model_id: "shared" }),
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows[0]?.key).not.toBe(rows[1]?.key);
  });

  // This is the assertion that keeps out the self-contradiction of listing a
  // candidate annotated "N already wired".
  it("does not list an upstream id that a route already occupies", () => {
    const rows = merged({
      routes: [route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" })],
      discovered: [
        { upstream_model: "gpt-4o", state: "routed", model_id: MODEL_A },
        { upstream_model: "gpt-4o-mini", state: "mappable", model_id: MODEL_A },
      ],
      complete: true,
    });
    expect(rows.filter((r) => r.upstream === "gpt-4o")).toHaveLength(1);
    expect(rows.find((r) => r.upstream === "gpt-4o")?.routeId).toBe(ROUTE_1);
    expect(rows.find((r) => r.upstream === "gpt-4o-mini")?.routeId).toBeNull();
  });

  it("does not add a manual entry that is already listed", () => {
    const rows = merged({
      routes: [route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" })],
      manual: [{ modelId: MODEL_A, slug: "openai/gpt-4o", upstream: "gpt-4o" }],
    });
    expect(rows).toHaveLength(1);
  });

  // ── The three states of onUpstream ──

  it("cannot tell whether upstream still offers a model when the read was truncated", () => {
    const rows = merged({
      routes: [route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" })],
      discovered: [{ upstream_model: "something-else", state: "unknown" }],
      complete: false,
    });
    // Not read in a truncated result is not "no longer offered upstream".
    expect(rows.find((r) => r.routeId === ROUTE_1)?.onUpstream).toBeNull();
  });

  it("marks every configured route as gone when a complete read returns nothing", () => {
    const rows = merged({
      routes: [
        route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" }),
        route({ id: ROUTE_2, model_id: MODEL_B, provider_model_id: "claude-sonnet-4" }),
      ],
      discovered: [],
      complete: true,
    });
    expect(rows).toHaveLength(2);
    expect(rows.every((r) => r.onUpstream === false)).toBe(true);
  });

  it("cannot tell anything when the upstream was never fetched", () => {
    const rows = merged({
      routes: [route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" })],
      discovered: null,
      complete: true, // even when complete, nothing was fetched, so nothing is decidable
    });
    expect(rows[0]?.onUpstream).toBeNull();
  });
});

describe("planWiring", () => {
  // Each of the four cells asserted, **including the two no-op cells** — an
  // implementation that always creates or always deletes fails on those.
  it("creates only for a checked row that has no route", () => {
    const plan = planWiring([view({ checked: true, routeId: null })]);
    expect(plan.creates).toHaveLength(1);
    expect(plan.deletes).toHaveLength(0);
  });

  it("deletes only for an unchecked row that has a route", () => {
    const plan = planWiring([view({ checked: false, routeId: ROUTE_1 })]);
    expect(plan.deletes).toHaveLength(1);
    expect(plan.deletes[0]?.routeId).toBe(ROUTE_1);
    expect(plan.creates).toHaveLength(0);
  });

  it("does nothing for an unchecked row that has no route", () => {
    const plan = planWiring([view({ checked: false, routeId: null })]);
    expect(plan.creates).toHaveLength(0);
    expect(plan.deletes).toHaveLength(0);
  });

  it("does nothing for a checked row that already has its route", () => {
    const plan = planWiring([view({ checked: true, routeId: ROUTE_1 })]);
    expect(plan.creates).toHaveLength(0);
    expect(plan.deletes).toHaveLength(0);
  });

  // An unknown row with no draft still creates nothing: ticked, but with no slug
  // there is no entry to create.
  it("never plans a create for an unknown row with no draft", () => {
    const plan = planWiring([view({ checked: true, routeId: null, modelId: null })]);
    expect(plan.creates).toHaveLength(0);
  });

  // The second cell of the table: an unknown row carrying a draft means "create the
  // catalog entry, then create the route".
  it("plans a create-with-new-model for an unknown row that has a draft", () => {
    const plan = planWiring([
      view({
        checked: true,
        routeId: null,
        modelId: null,
        upstream: "brand-new",
        draft: newModelDraft({ slug: "openai/brand-new", source: "vendor" }),
      }),
    ]);
    expect(plan.creates).toHaveLength(1);
    expect(plan.creates[0]?.modelId).toBeNull();
    expect(plan.creates[0]?.newModel?.slug).toBe("openai/brand-new");
  });
});

describe("canWire / newModelDraft / draftIssue", () => {
  // Rejected here, this forced the operator to go create the model in the catalog
  // and come back to re-fetch.
  it("lets an unknown row be checked so it can create its catalog entry", () => {
    expect(canWire({ modelId: null, origin: "upstream" } as WiringRow)).toBe(true);
    expect(canWire({ modelId: MODEL_A, origin: "route" } as WiringRow)).toBe(true);
  });

  // The draft takes what the server suggested and nothing else. It used to derive
  // the slug here, from the bare upstream name -- which is not a slug, and produced
  // catalog entries named `gpt-4o` where the convention and now the database both
  // say `openai/gpt-4o`. A slug cannot be changed once created, so that mistake was
  // permanent; the rule that decides the name lives on the server, in one place.
  it("takes the whole draft from the server's suggestion", () => {
    expect(
      newModelDraft({
        slug: "google/gemini-3.1-flash-image",
        display_name: "Gemini 3.1 Flash Image",
        context_window: 1_050_000,
        max_output_tokens: 128_000,
        output_modalities: ["text", "image"],
        source: "seed",
      }),
    ).toEqual({
      slug: "google/gemini-3.1-flash-image",
      displayName: "Gemini 3.1 Flash Image",
      contextWindow: "1050000",
      maxOutputTokens: "128000",
      // Carried, never derived. It is the one field about this model that no
      // upstream name could have supplied, and getting it from anywhere but the
      // seed would file an image model under text.
      outputModalities: ["text", "image"],
      source: "seed",
      manualProbe: false,
    });
  });

  // No suggestion means the server could not name the model -- an Azure deployment
  // name, a Bedrock ARN. Every field stays empty so the operator is asked, rather
  // than handed something plausible for a value that cannot be corrected later.
  it("leaves everything empty when there was no suggestion", () => {
    expect(newModelDraft()).toEqual({
      slug: "",
      displayName: "",
      contextWindow: "",
      maxOutputTokens: "",
      // Empty rather than ["text"]: the draft says nothing, and the column's
      // own default is what decides. Prefilling text here would be this file
      // deriving a value again, which is the mistake the comment above records.
      outputModalities: [],
      source: null,
      manualProbe: false,
    });
  });

  it("blocks a draft that is missing its slug", () => {
    expect(draftIssue(newModelDraft())).toBe("gwWiringDraftSlugRequired");
  });

  // Coarser than the database's pattern on purpose -- the exact one lives in the
  // migration -- but it catches the mistake somebody actually makes, right next to
  // the field: a bare upstream name.
  it("blocks a slug that is not two segments", () => {
    const withSlug = (slug: string) => ({ ...newModelDraft(), slug });
    expect(draftIssue(withSlug("gpt-4o"))).toBe("gwWiringDraftSlugShape");
    expect(draftIssue(withSlug("openai/"))).toBe("gwWiringDraftSlugShape");
    expect(draftIssue(withSlug("/gpt-4o"))).toBe("gwWiringDraftSlugShape");
    expect(draftIssue(withSlug("a/b/c"))).toBe("gwWiringDraftSlugShape");
    expect(draftIssue(withSlug("openai/gpt-4o"))).toBeNull();
  });

  it("blocks a token count that is not a whole number", () => {
    const draft = { ...newModelDraft(), slug: "openai/gpt-4o" };
    expect(draftIssue({ ...draft, contextWindow: "1e6" })).toBe("gwWiringDraftNumber");
    expect(draftIssue({ ...draft, maxOutputTokens: "-1" })).toBe("gwWiringDraftNumber");
    // Blank is allowed: it means "leave it unset", which the server handles.
    expect(draftIssue({ ...draft, contextWindow: "" })).toBeNull();
  });
});

describe("checkedOf", () => {
  const row = { key: "k", routeId: ROUTE_1 } as WiringRow;
  const fresh = { key: "k", routeId: null } as WiringRow;

  it("follows the database when nobody has touched the row", () => {
    expect(checkedOf(row, new Map())).toBe(true);
    expect(checkedOf(fresh, new Map())).toBe(false);
  });

  it("uses the override once someone has, including when it agrees", () => {
    expect(checkedOf(row, new Map([["k", false]]))).toBe(false);
    expect(checkedOf(fresh, new Map([["k", true]]))).toBe(true);
    // An override that agrees with the truth still applies, or unticking and
    // reticking a row would jump around inexplicably.
    expect(checkedOf(row, new Map([["k", true]]))).toBe(true);
  });
});

describe("canWire", () => {
  // This case is the written form of whether an unpriced model may be wired.
  it("blocks only the rows that have no local model to point at", () => {
    expect(canWire({ modelId: null } as WiringRow)).toBe(false);
    expect(canWire({ modelId: MODEL_A } as WiringRow)).toBe(true);
    // Unpriced is allowed: creating a route has no pricing gate; the real gates are
    // enabling it and the run-time check.
    expect(canWire({ modelId: MODEL_A, discoveredState: "unpriced" } as WiringRow)).toBe(true);
  });
});

describe("upstreamFromSlug", () => {
  it("takes the segment after the last slash", () => {
    expect(upstreamFromSlug("openai/gpt-4o")).toBe("gpt-4o");
  });

  it("returns the whole slug when there is no slash", () => {
    expect(upstreamFromSlug("gpt-4o")).toBe("gpt-4o");
  });

  // The one input where lastIndexOf and indexOf disagree.
  it("cuts at the last slash, not the first", () => {
    expect(upstreamFromSlug("vendor/openai/gpt-4o")).toBe("gpt-4o");
  });

  it("yields an empty string for a slug that ends in a slash", () => {
    expect(upstreamFromSlug("openai/")).toBe("");
  });
});

describe("resolveModelForUpstream", () => {
  const models = [OPENAI, CLAUDE, { id: "c", slug: "gpt-4o" }];

  // Mirrors the server ordering that puts exact slug matches first.
  it("prefers an exact slug match over a suffix match", () => {
    expect(resolveModelForUpstream("gpt-4o", models)?.id).toBe("c");
  });

  it("falls back to the suffix match", () => {
    expect(resolveModelForUpstream("claude-sonnet-4", models)?.id).toBe(MODEL_B);
  });

  // A model owns no protocol, so the provider's dialects play no part: the
  // same Claude model is wired to an openai-only relay by name alone.
  it("matches by name alone, whatever the provider speaks", () => {
    expect(resolveModelForUpstream("claude-sonnet-4", models)?.id).toBe(MODEL_B);
  });

  // **Never settle for something approximate**: that turns "wired to the wrong
  // model" into a silent default-value accident.
  it("returns null rather than an approximation when nothing matches", () => {
    expect(resolveModelForUpstream("nope-9000", models)).toBeNull();
  });

  it("returns null for a blank name", () => {
    expect(resolveModelForUpstream("   ", models)).toBeNull();
  });
});

describe("composerIssue", () => {
  const rows = merged({
    routes: [route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "gpt-4o" })],
  });

  it("names the missing half", () => {
    expect(composerIssue(null, "gpt-4o", rows)).toBe("gwWiringPickModel");
    expect(composerIssue(MODEL_A, "  ", rows)).toBe("gwUpstreamRequired");
  });

  it("catches a pair that is already on the list", () => {
    expect(composerIssue(MODEL_A, "gpt-4o", rows)).toBe("gwWiringAlreadyListed");
    // The same model under a different upstream name is legal and must pass.
    expect(composerIssue(MODEL_A, "gpt-4o-mini", rows)).toBeNull();
  });
});

// The three cases for the old client-side outcome classifier were deleted with it:
// the rule that a conflict on create and a missing row on delete both count as
// "already" now lives only on the server, with its own assertions there. A predicate
// should exist once; judging it on both sides is exactly the shape worth removing.

describe("discoverSummary", () => {
  const discovered: DiscoveredLike[] = [
    { upstream_model: "a", state: "routed" },
    { upstream_model: "b", state: "mappable" },
    { upstream_model: "c", state: "unpriced" },
    { upstream_model: "d", state: "unknown" },
  ];
  const routes = [
    route({ id: ROUTE_1, model_id: MODEL_A, provider_model_id: "a" }),
    route({ id: ROUTE_2, model_id: MODEL_B, provider_model_id: "vanished" }),
  ];

  it("splits the total into four and counts what upstream stopped offering", () => {
    const s = discoverSummary(discovered, routes, true);
    expect(s.total).toBe(4);
    expect(s.routed + s.mappable + s.unpriced + s.unknown).toBe(s.total);
    expect(s.gone).toBe(1);
  });

  // Absence from an incomplete reading is not a conclusion.
  it("cannot count what is gone when the read was truncated", () => {
    expect(discoverSummary(discovered, routes, false).gone).toBeNull();
  });
});

describe("canPriceFromReference", () => {
  const draft = newModelDraft({ slug: "openai/gpt-4o", source: "vendor" });

  // The two states the upstream fetch reports as wired-but-not-sellable.
  it("offers it on an unpriced model and on a row creating its own", () => {
    expect(canPriceFromReference(view({ checked: true, discoveredState: "unpriced" }))).toBe(true);
    expect(canPriceFromReference(view({ checked: true, modelId: null, draft }))).toBe(true);
  });

  // A model that already has a price is not in the state this offer resolves,
  // and quietly repricing one from a dialog about routing would be a side
  // effect nobody asked for. (The import refuses it too — this is the interface
  // agreeing with the server rather than a second rule.)
  it("stays off a model that already has a price", () => {
    expect(canPriceFromReference(view({ checked: true, discoveredState: "mappable" }))).toBe(false);
    expect(canPriceFromReference(view({ checked: true, discoveredState: "routed" }))).toBe(false);
  });

  // The price is filled in after the wiring lands, so an unticked row is not
  // being wired and has nothing to attach a price to.
  it("stays off an unticked row", () => {
    expect(canPriceFromReference(view({ checked: false, discoveredState: "unpriced" }))).toBe(
      false,
    );
  });

  // An already routed row is a question for the model's own page. Saving here
  // must not start meaning "and revisit the prices of everything on the list".
  it("stays off a row that is already stored", () => {
    expect(
      canPriceFromReference(view({ checked: true, routeId: ROUTE_1, discoveredState: "unpriced" })),
    ).toBe(false);
  });
});

describe("referencePriceTargets", () => {
  const planned = [{ key: "a" }, { key: "b" }, { key: "c" }];

  // Matched by index, exactly as the per-row outcomes are: a row that creates
  // its own model has no id beforehand, so the result is the first place that
  // id exists.
  it("takes the model id from the result, and only for the rows asked for", () => {
    const got = referencePriceTargets(
      planned,
      [
        { outcome: "done", model_id: MODEL_A },
        { outcome: "done", model_id: MODEL_B },
        { outcome: "already", model_id: MODEL_A },
      ],
      new Set(["a", "c"]),
    );
    // MODEL_A once, not twice: row "c" landed on the same model as row "a",
    // and asking the import for it twice would report it twice.
    expect(got).toEqual([MODEL_A]);
  });

  // Asking to price a model whose creation failed turns one reported failure
  // into two, the second of which is a consequence of the first and tells the
  // reader nothing.
  it("leaves out a row that failed", () => {
    expect(
      referencePriceTargets(
        planned,
        [{ outcome: "failed", model_id: null, detail: "slug already taken" }],
        new Set(["a"]),
      ),
    ).toEqual([]);
  });

  // A create whose model id is missing has nothing to name. Sending an empty
  // string would ask the server about a model that cannot exist.
  it("leaves out a row the server could not attribute to a model", () => {
    expect(referencePriceTargets(planned, [{ outcome: "done" }], new Set(["a"]))).toEqual([]);
  });
});
