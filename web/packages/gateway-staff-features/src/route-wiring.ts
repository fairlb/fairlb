import type { MessageKey } from "@fairlb/i18n";

/**
 * The pure logic behind the list "which upstream models does this provider serve".
 *
 * The entry point deliberately starts from the upstream, not from the local catalog.
 * Built the other way round — pick from the catalog, append a row — the picker shows
 * **a pile of things already done** in the commonest case, where most candidates are
 * already configured: each one annotated "N already wired", and choosing one produces
 * a duplicate that answers 409. The order was backwards. The operator's question is
 * "which upstream models does this provider support", and the local catalog is only
 * half of that question's answer.
 */

type RowOrigin = "route" | "upstream" | "manual";
/** The same domain as a discovered model's `state` in the API contract. */
type DiscoveredState = "routed" | "mappable" | "unpriced" | "unknown";

/**
 * One row of the list. **A row's identity is the pair (local model, upstream name).**
 *
 * With the provider fixed, those two are exactly what remains of the stored unique
 * key over model, provider and upstream name — and **both directions are one to
 * many**:
 *   - one model behind two upstream aliases (a relay renaming it, or the same model
 *     sold under two identifiers) — keyed on the model alone, they overwrite
 *     each other;
 *   - one upstream name behind two local models (`(A,"gpt-4o")` and `(B,"gpt-4o")`
 *     are two distinct configurations) — keyed on the upstream name alone, likewise.
 * Either single dimension is wrong.
 */
export interface WiringRow {
  key: string;
  upstream: string;
  /** null means the upstream offers it but the local catalog has no such model. */
  modelId: string | null;
  slug: string;
  /**
   * The route that exists today, or null when there is none.
   * **Always derived from the routes query, never read out of a mutation response**:
   * after saving, everything refetches, the rows merge again, and the truth returns
   * on its own — which is also how a row that hit a 409 on create becomes
   * "configured".
   */
  routeId: string | null;
  routeEnabled: boolean;
  origin: RowOrigin;
  /**
   * Did the upstream report this name this time?
   * **null means unknowable** — never fetched, the fetch failed, or the enumeration
   * was incomplete. Absence from an incomplete reading only means it was not read;
   * it does not mean the upstream stopped offering it.
   */
  onUpstream: boolean | null;
  discoveredState: DiscoveredState | null;
}

/** A row plus what the operator currently intends and what went wrong last time. */
export interface WiringRowView extends WiringRow {
  checked: boolean;
  /** Why the last save failed on this row, already localized and ready to render. */
  error?: string;
  /**
   * The catalog entry this row would create alongside the route — only rows whose
   * `modelId` is null have one. `undefined` means not ticked yet, or that a local
   * model already exists.
   */
  draft?: NewModelDraft;
}

export interface ModelLike {
  id: string;
  slug: string;
}

export interface RouteLike {
  id: string;
  model_id: string;
  /** 模型的名字随路由返回，见 mergeRows 里为什么不能去目录里查。 */
  model_slug: string;
  provider_model_id: string;
  enabled: boolean;
}

export interface DiscoveredLike {
  upstream_model: string;
  state: string;
  model_id?: string | null;
  model_slug?: string;
}

export interface ManualEntry {
  modelId: string;
  slug: string;
  upstream: string;
}

export interface MergeInput {
  routes: readonly RouteLike[];
  /** null means never fetched, or the fetch failed. **Do not pass `?? []`**: that
   * lets "never checked" masquerade as "checked, and the upstream does not have it". */
  discovered: readonly DiscoveredLike[] | null;
  /** Whether this enumeration was complete. When false, nothing is ever marked as
   * "no longer offered upstream". */
  complete: boolean;
  manual: readonly ManualEntry[];
}

/**
 * The key of a row.
 *
 * Separated by NUL, because neither a uuid nor an upstream name can contain one.
 * Using `/`, `-` or `:` would collapse two different pairs onto the same string —
 * all three appear routinely inside upstream names. A null `modelId` is held by the
 * empty string: those rows still have to render and still have to be completable by
 * hand.
 */
export function rowKey(modelId: string | null, upstream: string): string {
  return `${modelId ?? ""}\u0000${upstream}`;
}

/**
 * Merges the three sources into one list. **Order is priority is render order.**
 *
 * 1. Existing routes: what is stored. Always present, always ticked, **always
 *    first** — this is where "what is configured today must appear in the list, and
 *    appear as selected" is actually implemented.
 * 2. Upstream candidates: **an upstream name already taken by a route does not get
 *    its own row**, or "the upstream reports gpt-4o" would draw a second, unticked
 *    gpt-4o beside the configured one — exactly the self-contradiction of annotating
 *    a candidate with "N already wired".
 * 3. Manual additions: skipped when they collide with an existing row's key.
 */
export function mergeRows(input: MergeInput): WiringRow[] {
  const rows: WiringRow[] = [];
  const byKey = new Map<string, number>();
  // Upstream names already taken by a route on this provider. **Collected by name,
  // not by (model, name)** — the server's own "routed" test asks whether anything on
  // this provider uses this upstream name, without reference to a model, so the two
  // sides answer the same question.
  const takenUpstream = new Set<string>();

  for (const r of input.routes) {
    const key = rowKey(r.model_id, r.provider_model_id);
    takenUpstream.add(r.provider_model_id);
    if (byKey.has(key)) continue; // the unique key rules this out; skip rather than overwrite
    byKey.set(key, rows.length);
    rows.push({
      key,
      upstream: r.provider_model_id,
      modelId: r.model_id,
      // 名字随路由返回，不再拿 model_id 去目录里查（ADR-0189）。
      // 目录分页之后那次查表会落空，而落空的表现是 `?? ""`——**已配置的那一行
      // 名字变成空白**，读者看到的是一条没有模型的路由。
      slug: r.model_slug,
      routeId: r.id,
      routeEnabled: r.enabled,
      origin: "route",
      onUpstream: null,
      discoveredState: null,
    });
  }

  const seenUpstream = new Set<string>();
  for (const d of input.discovered ?? []) {
    seenUpstream.add(d.upstream_model);
    if (takenUpstream.has(d.upstream_model)) continue;
    const key = rowKey(d.model_id ?? null, d.upstream_model);
    if (byKey.has(key)) continue;
    byKey.set(key, rows.length);
    rows.push({
      key,
      upstream: d.upstream_model,
      modelId: d.model_id ?? null,
      slug: d.model_slug ?? "",
      routeId: null,
      routeEnabled: false,
      origin: "upstream",
      onUpstream: true,
      discoveredState: d.state as DiscoveredState,
    });
  }

  for (const e of input.manual) {
    const key = rowKey(e.modelId, e.upstream);
    if (byKey.has(key)) continue;
    byKey.set(key, rows.length);
    rows.push({
      key,
      upstream: e.upstream,
      modelId: e.modelId,
      slug: e.slug,
      routeId: null,
      routeEnabled: false,
      origin: "manual",
      onUpstream: null,
      discoveredState: null,
    });
  }

  // Fill in "did the upstream report it this time". **Only a complete enumeration can
  // establish absence**: not being read in a truncated result is not the same as the
  // upstream no longer offering it, so when the enumeration is incomplete every row
  // stays null and the interface draws no conclusion.
  if (input.discovered !== null && input.complete) {
    for (const row of rows) {
      if (row.onUpstream === null) row.onUpstream = seenUpstream.has(row.upstream);
    }
  }
  return rows;
}

/** A row starts ticked exactly when it is stored. Editing a whole set has to start
 * from **the current configuration**, never from the empty set. */
export const initiallyChecked = (row: WiringRow): boolean => row.routeId !== null;

/**
 * Whether this row counts as ticked right now.
 *
 * An overlay consulted with `??`, rather than a set seeded up front: after a
 * refetch **every row nobody touched follows the truth automatically**, and only the
 * rows the operator still disagrees with keep their override. A seeded set would have
 * to be resynchronized by hand after every save.
 */
export const checkedOf = (row: WiringRow, over: ReadonlyMap<string, boolean>): boolean =>
  over.get(row.key) ?? initiallyChecked(row);

/**
 * Whether this row can be ticked at all.
 *
 * A null `modelId` **is allowed**: the missing model can be created on the spot, so
 * ticking the row means "create the catalog entry and wire it", with slug and
 * display name reviewed inline. Rejecting it because the create request needs a model
 * id described the shape correctly but drew the conclusion too early.
 *
 * An unpriced model **is allowed** too. Creating a route has no pricing gate on the
 * server — it validates that both ends exist and returns 201 for a model with no
 * price; what the route serves is found out by probing, not declared. The real gates are *enabling*
 * the route and the run-time price check. An interface stricter than its own storage
 * is exactly the shape worth removing; and since a model's readiness checklist lists
 * "wire a provider" and "set the list price" as two steps in no particular order,
 * blocking the first would delete one of them. The consequence is carried instead by
 * the row's "unpriced" badge and by the itemized counts in the confirmation dialog.
 *
 * The predicate is still spelled out rather than being a constant `true`: **which
 * rows cannot be ticked is a question whose answer changes**, and written as `true`
 * the next reader can see neither that a gate was ever here nor why it is now open.
 */
export const canWire = (row: WiringRow): boolean =>
  row.modelId !== null || row.origin === "upstream";

/**
 * The catalog entry an unknown row would create.
 *
 * **A slug is immutable once created**, and there is no reliable mechanical mapping
 * between an upstream name and a local slug — prefilling one is only a guess. So both
 * fields have to be **editable on the row itself**, visible and changeable, before
 * "reviewed" means anything. Inline rather than behind a separate review dialog,
 * because a review belongs on the row it is going to change. There is no protocol
 * to choose: a model owns none, and the route is probed on whatever its provider
 * speaks.
 */
export interface NewModelDraft {
  slug: string;
  displayName: string;
}

/** The draft's initial values: slug prefilled from the upstream name. */
export function newModelDraft(upstream: string): NewModelDraft {
  return { slug: upstream, displayName: "" };
}

/** Whether the draft can be submitted; when it cannot, a message key rather than a
 * finished string. */
export function draftIssue(draft: NewModelDraft): MessageKey | null {
  if (draft.slug.trim() === "") return "gwWiringDraftSlugRequired";
  return null;
}

interface RouteCreate {
  key: string;
  /** null means this row creates a catalog entry too, described by `newModel`. */
  modelId: string | null;
  upstream: string;
  slug: string;
  /** Present means "create the catalog entry and wire it". Exactly one of this and
   * `modelId` is set. */
  newModel?: NewModelDraft;
}

interface RouteDelete {
  key: string;
  modelId: string;
  routeId: string;
  upstream: string;
  slug: string;
}

export interface WiringPlan {
  creates: RouteCreate[];
  deletes: RouteDelete[];
}

/**
 * What saving does is decided entirely by two bits: whether the row has a route
 * today, and whether it is ticked.
 *
 * | routeId | checked | modelId | action |
 * |---|---|---|---|
 * | null    | true    | set     | create |
 * | null    | true    | null    | **create catalog entry, then create route** |
 * | set     | false   | set     | **delete** |
 * | null    | false   | —       | nothing |
 * | set     | true    | —       | nothing |
 *
 * **Unticking a row deletes its route** — that is what editing a whole set means, not
 * a side effect of it, which is why the confirmation dialog has to state the deletion
 * count on its own line. The second row of the table is the other one worth stating:
 * ticking a name the upstream reports but the local catalog lacks means "create this
 * model and wire it", so the dialog itemizes **new catalog entries** separately too.
 *
 * A null `modelId` with no draft is already blocked in the interface; re-checking it
 * here guarantees that exactly one of a create's two targets is set.
 */
/**
 * The three things a row in a whole-set edit **actually needs**: its identity,
 * whether it is stored, and whether the operator ticked it.
 *
 * Rows on the provider side also carry a model, an upstream name and a draft; rows on
 * the model side carry a provider and an upstream name. Those are each side's payload
 * and have nothing to do with the truth table, which knows only these three.
 */
export interface SetRow {
  key: string;
  routeId: string | null;
  checked: boolean;
}

/**
 * **The four-cell truth table itself**, shared by both projections.
 *
 * | routeId | checked | action |
 * |---|---|---|
 * | null    | true    | create |
 * | set     | false   | **delete** |
 * | null    | false   | nothing |
 * | set     | true    | nothing |
 *
 * It returns **the rows themselves**, not a mapped request shape: the two sides send
 * different fields — one holds the provider fixed, the other the model — so mapping
 * belongs to each caller. What is shared is this table, not that mapping.
 *
 * The cost of a second copy is not the extra lines, it is that **two copies of a
 * predicate drift apart**. That has happened twice here — once with the selection
 * rules on the publishing and consuming sides, once with the interface being
 * stricter than the server — and both times the symptom was that nobody reported
 * anything.
 */
export function planSet<T extends SetRow>(rows: readonly T[]): { creates: T[]; deletes: T[] } {
  const creates: T[] = [];
  const deletes: T[] = [];
  for (const r of rows) {
    if (r.routeId === null && r.checked) creates.push(r);
    else if (r.routeId !== null && !r.checked) deletes.push(r);
  }
  return { creates, deletes };
}

export function planWiring(rows: readonly WiringRowView[]): WiringPlan {
  const creates: RouteCreate[] = [];
  const deletes: RouteDelete[] = [];
  for (const r of rows) {
    if (r.routeId === null && r.checked) {
      if (r.modelId === null && r.draft === undefined) continue;
      creates.push({
        key: r.key,
        modelId: r.modelId,
        upstream: r.upstream,
        slug: r.modelId === null ? (r.draft?.slug ?? "") : r.slug,
        ...(r.modelId === null && r.draft ? { newModel: r.draft } : {}),
      });
    } else if (r.routeId !== null && !r.checked && r.modelId !== null) {
      deletes.push({
        key: r.key,
        modelId: r.modelId,
        routeId: r.routeId,
        upstream: r.upstream,
        slug: r.slug,
      });
    }
  }
  return { creates, deletes };
}

/**
 * Whether this row can offer "and fill its price in from the reference prices".
 *
 * Two shapes qualify, and they are the two the upstream fetch reports as not
 * sellable:
 *
 *   - `unpriced`: the model exists here and has no price. Wiring it changes
 *     nothing a caller can see — an unpriced model is refused rather than
 *     served free, so it stays out of the catalogue however many routes it has.
 *   - a row creating its own catalog entry: what it creates is disabled and
 *     carries no price at all, so it lands in exactly the state above.
 *
 * Only ticked rows qualify, because the price is filled in **after** the wiring
 * lands: an unticked row is not being wired, and pricing a model this save is
 * not touching would be a side effect nobody asked for.
 *
 * A row that is already routed is deliberately not offered it. Its price is a
 * separate question, asked on the model's own page, and answering it from a
 * dialog about which upstream models a provider serves would quietly widen what
 * "save" means here.
 */
export const canPriceFromReference = (row: WiringRowView): boolean =>
  row.checked &&
  row.routeId === null &&
  (row.discoveredState === "unpriced" || row.draft !== undefined);

/**
 * The models a save should ask the reference-price import to fill in.
 *
 * Matched back **by index**, exactly as the caller matches its per-row
 * outcomes: a row that creates its own model has no model id beforehand, so
 * pairing on the model would skip precisely the rows this feature exists for.
 * The id comes from the result, which is where the newly created model's id
 * first appears.
 *
 * A failed row is left out. Its model may not exist, and asking to price
 * something that was not created would turn one reported failure into two.
 */
export function referencePriceTargets(
  planned: readonly { key: string }[],
  // The row shape is the batch endpoint's own, `detail` included. Naming only
  // the two fields this function reads would make a faithful fixture fail to
  // typecheck, and the fixture that then compiles is one that no longer looks
  // like what the server sends.
  results: readonly { outcome: RowOutcome; model_id?: string | null; detail?: string }[],
  wanted: ReadonlySet<string>,
): string[] {
  const out: string[] = [];
  for (const [i, res] of results.entries()) {
    const key = planned[i]?.key;
    if (key === undefined || !wanted.has(key)) continue;
    if (res.outcome === "failed" || !res.model_id) continue;
    if (!out.includes(res.model_id)) out.push(res.model_id);
  }
  return out;
}

/**
 * The **exact inverse** of the server's slug-to-upstream-name rule, not a heuristic.
 *
 * The classifier matches a slug that either equals the upstream name or ends with a
 * slash followed by it — which together is "the part after the last slash", or the
 * whole string when there is no slash.
 *
 * Prefilled rather than left blank: blank means copying `gpt-4o` out of
 * `openai/gpt-4o` by hand, and one mistyped character produces a configuration that
 * **saves fine and answers 404 at run time**.
 */
export function upstreamFromSlug(slug: string): string {
  const cut = slug.lastIndexOf("/");
  return cut === -1 ? slug : slug.slice(cut + 1);
}

/**
 * Picks the local model for a typed upstream name, using **the same rule** the
 * server's classifier uses: the slug either equals the name or ends with a slash
 * followed by it; exact matches win, then the first in slug order.
 *
 * A hit is only a **prefill** — the row names the model it chose, and it can be
 * cleared and redone. **A miss returns null and lets the operator choose, rather
 * than settling for something approximate**: an approximate default would turn
 * "wired to the wrong model" into a silent accident whose symptom is a run-time 404,
 * indistinguishable from "the model was never created" or "the tier is not open".
 */
export function resolveModelForUpstream(
  upstream: string,
  models: readonly ModelLike[],
): ModelLike | null {
  const name = upstream.trim();
  if (name === "") return null;
  const candidates = models
    .filter((m) => m.slug === name || m.slug.endsWith(`/${name}`))
    .sort((a, b) => {
      const exact = Number(b.slug === name) - Number(a.slug === name);
      return exact !== 0 ? exact : a.slug.localeCompare(b.slug);
    });
  return candidates[0] ?? null;
}

/** Whether the composer row can be submitted; when it cannot, a message key rather
 * than a finished string, as every validator here does. */
export function composerIssue(
  modelId: string | null,
  upstream: string,
  rows: readonly WiringRow[],
): MessageKey | null {
  if (modelId === null) return "gwWiringPickModel";
  if (upstream.trim() === "") return "gwUpstreamRequired";
  if (rows.some((r) => r.key === rowKey(modelId, upstream.trim()))) {
    return "gwWiringAlreadyListed";
  }
  return null;
}

export interface DiscoverSummary {
  total: number;
  routed: number;
  mappable: number;
  unpriced: number;
  unknown: number;
  /** Configured locally, no longer reported upstream. **null means undecidable**:
   * from an incomplete enumeration, absence is not a conclusion. */
  gone: number | null;
}

/** A one-line summary of a fetch. The parts always add up to the total. */
export function discoverSummary(
  discovered: readonly DiscoveredLike[],
  routes: readonly RouteLike[],
  complete: boolean,
): DiscoverSummary {
  const counts = { routed: 0, mappable: 0, unpriced: 0, unknown: 0 };
  for (const d of discovered) {
    if (d.state in counts) counts[d.state as keyof typeof counts] += 1;
  }
  const seen = new Set(discovered.map((d) => d.upstream_model));
  return {
    total: discovered.length,
    ...counts,
    gone: complete ? routes.filter((r) => !seen.has(r.provider_model_id)).length : null,
  };
}

/**
 * How one write turned out. The same domain the batch endpoint's per-item outcome
 * uses.
 *
 * **A conflict on create and a not-found on delete both mean "the world already
 * agrees with you"**, not failure:
 *   - creating a route that hits the unique key means that exact (model, provider,
 *     upstream name) is stored right now, which was the goal;
 *   - deleting a route that is not there means it is already gone, which was also
 *     the goal.
 *
 * Counting those as failures makes people retry **a goal already achieved**. Both
 * count towards the success total, but the notification names them on their own line
 * — "K of these were already so" — **never silently**, because it means the list in
 * front of the operator is stale and somebody else may be editing at the same time.
 *
 * **The classification itself lives only on the server**: the batch response carries
 * the outcome directly, and the client no longer re-derives it from HTTP status
 * codes. The old client-side classifier and its unit test were deleted together, and
 * the witnesses moved server-side, where "conflict counts as already" and "missing
 * counts as already" have an assertion each.
 */
export type RowOutcome = "done" | "already" | "failed";
