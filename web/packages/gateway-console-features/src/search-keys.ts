/**
 * The search parameters each page reads. Filters live in the URL, so that a view
 * can be linked to and survives a reload.
 *
 * # Why this is a leaf module of its own
 *
 * A shell needs these in its **route table** (`validateSearch`), and the route
 * table lives in the entry chunk, while the pages themselves arrive through
 * `lazy()`. Exporting these constants from the page files would let the shell's one
 * static import drag the whole feature package into the entry — `lazy()` still
 * there, the code splitting gone.
 *
 * That is not hypothetical: it turned an end-to-end assertion red, the one checking
 * that an unauthenticated deep link requests no page chunk at all. Hence the rule
 * for this file: **it may not import anything.**
 */

/** Usage page: time range, grouping axis, filter by key. */
export const USAGE_SEARCH_KEYS = ["range", "group", "key"] as const;

/** Requests page. `request` is the open detail id, carried through the same URL channel. */
export const REQUEST_SEARCH_KEYS = ["range", "status", "key", "model", "user", "request"] as const;

/**
 * Model catalog: free-text filter.
 *
 * The page read `q` through `useSearch({ strict: false })` while registering no
 * contract for it, which made it the one filtered list in the console whose
 * filter was not declared anywhere. It worked, and that is the problem: nothing
 * would have noticed if it stopped.
 */
export const MODEL_SEARCH_KEYS = ["q"] as const;

/** Video jobs: status and model. */
export const VIDEO_SEARCH_KEYS = ["status", "model"] as const;
