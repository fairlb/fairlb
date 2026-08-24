import { rowKey, upstreamFromSlug, type SetRow } from "./route-wiring";

/**
 * The pure logic behind the list "which providers is this model served by".
 *
 * It and route-wiring.ts are **two projections of the same set**. Once one dimension
 * of the unique key over model, provider and upstream name is fixed, what remains is
 * a row's identity: fixing the provider leaves (model, upstream name); **fixing the
 * model leaves (provider, upstream name)**. Both directions are still one to many —
 * one provider behind two aliases, one alias on two providers.
 *
 * What the two share is the **four-cell truth table** and the row identity rule, not
 * the merging: the candidate sources are entirely different, and forcing them into
 * one function would only grow a thicket of direction checks.
 *
 * **The one real asymmetry is the strength of the evidence for the upstream name.**
 * On the provider side it comes back from a real enumeration and is a fact; here it
 * is guessed from the slug and is only a prefill. So each row's upstream name is
 * **editable in place**, and nothing here ever carries a "no longer offered
 * upstream" badge — on this side there is no evidence for that conclusion.
 */
export interface ProviderRow extends SetRow {
  providerId: string;
  providerSlug: string;
  /** The name this provider knows the model by. Configured rows take the stored
   * value; candidate rows carry an editable guess derived from the slug. */
  upstream: string;
  routeEnabled: boolean;
  /** Whether this route is stored. Derived from the routes query, never from a
   * mutation response. */
  configured: boolean;
  /** Why the last save failed on this row, already localized and ready to render. */
  error?: string;
}

interface ProviderLike {
  id: string;
  slug: string;
}

interface ModelRouteLike {
  id: string;
  provider_id: string;
  /** The upstream's address, carried by the route itself. See mergeProviderRows
   * for why this must not be looked up in the provider list. */
  provider_slug: string;
  provider_model_id: string;
  enabled: boolean;
}

export interface MergeProviderInput {
  routes: readonly ModelRouteLike[];
  /** Every provider is a candidate: a model owns no protocol, so there is no
   * dialect to match. */
  providers: readonly ProviderLike[];
  modelSlug: string;
  /** Tick overrides. Rows nobody touched follow what is stored. */
  checkedOver: ReadonlyMap<string, boolean>;
  /** Upstream-name overrides: the ones edited inline. */
  upstreamOver: ReadonlyMap<string, string>;
  errors: ReadonlyMap<string, string>;
}

/**
 * Merges the configured routes with the provider candidates into one list.
 * **Order is priority is render order.**
 *
 * 1. Existing routes: what is stored. Always present, always ticked, **always
 *    first** — the same rule as on the provider side.
 * 2. Candidates: **a provider already taken by a route does not get its own row**, or "this model is already served by A" would be drawn beside a
 *    second, unticked A.
 *
 * A provider serving the model under two aliases produces two rows from step one and
 * none from step two — that provider is already in the list, and a third alias is
 * added through the composer.
 */
export function mergeProviderRows(input: MergeProviderInput): ProviderRow[] {
  const rows: ProviderRow[] = [];
  const taken = new Set<string>();

  for (const r of input.routes) {
    const key = rowKey(r.provider_id, r.provider_model_id);
    taken.add(r.provider_id);
    rows.push({
      key,
      providerId: r.provider_id,
      // The address comes from the route itself, not from a lookup into the
      // provider list. That lookup was safe while the list was a capped registry
      // fetched whole; once it paginated (ADR-0187) a provider merely sitting on
      // a later page fell through to the `id.slice(0, 8)` fallback — a fallback
      // that exists for a *deleted* provider, so a live one was being labelled as
      // if its configuration had gone. Same shape as the delivery log's endpoint
      // address (ADR-0186), and the same fix: the row carries what it needs.
      providerSlug: r.provider_slug,
      upstream: input.upstreamOver.get(key) ?? r.provider_model_id,
      routeId: r.id,
      routeEnabled: r.enabled,
      configured: true,
      checked: input.checkedOver.get(key) ?? true,
      ...(input.errors.has(key) ? { error: input.errors.get(key) } : {}),
    });
  }

  for (const p of input.providers) {
    if (taken.has(p.id)) continue;
    // Prefilled by inverting the server's slug-to-upstream-name rule, through the
    // same function the provider side uses. **Only a prefill**: nothing on this side
    // has enumerated the upstream, so whether the guess is right cannot be checked
    // here, and it must stay editable.
    const guess = upstreamFromSlug(input.modelSlug);
    const key = rowKey(p.id, guess);
    rows.push({
      key,
      providerId: p.id,
      providerSlug: p.slug,
      upstream: input.upstreamOver.get(key) ?? guess,
      routeId: null,
      routeEnabled: false,
      configured: false,
      checked: input.checkedOver.get(key) ?? false,
      ...(input.errors.has(key) ? { error: input.errors.get(key) } : {}),
    });
  }
  return rows;
}

/** Whether this row blocks submission: the upstream name may not be blank, because a
 * route built from an empty name answers 404 on first use. */
export function providerRowIssue(row: ProviderRow): boolean {
  return row.checked && row.upstream.trim() === "";
}
