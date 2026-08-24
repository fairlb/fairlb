/**
 * The search parameters the staff pages read.
 *
 * # Why this is a leaf module of its own
 *
 * Same rule as the console package's file of the same name, for the same
 * reason: a shell needs these in its **route table**, and the route table is in
 * the entry chunk while the pages arrive through `lazy()`. Importing them from
 * `module.tsx` would pull that module's static imports — the dashboard panel,
 * the kill-switch banner, the palette source — into the entry chunk of a shell
 * that may mount none of them.
 *
 * So the rule for this file is the same: **it may not import anything.**
 */

/** The access-tier editor is opened by id, so it can be linked to and reloaded. */
export const TIER_SEARCH_KEYS = ["tier"] as const;

/** The model catalog's orthogonal filter axes. */
export const MODEL_FILTER_KEYS = ["q", "protocol", "status", "pricing", "routing"] as const;

/** The provider list's free-text filter. */
export const PROVIDER_SEARCH_KEYS = ["q"] as const;
