import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "./button";

/**
 * useCursorList is the one place "load more" accumulates and de-duplicates rows.
 *
 * Four call sites used to carry their own copy, in two shapes that were each
 * wrong in their own way. Filtering during render is O(n²): by the tenth page
 * every render performs tens of thousands of comparisons. Accumulating in an
 * effect with a Set is correct but turns the accumulation into an extra render.
 *
 * This hook accumulates in a ref and de-duplicates with a Set, so it is O(n)
 * with no extra render, and re-running the effect under strict mode cannot
 * append the same page twice — two identical rows in a financial ledger read as
 * "we were charged twice", which is the worst possible way to be wrong.
 *
 * When the filter changes, the caller changes `resetKey` and the accumulation is
 * cleared.
 */
export function useCursorList<T, C = string>(
  page: {
    data?: { items?: T[]; data?: T[]; next_cursor?: C } | undefined;
    isFetching: boolean;
    // keepPreviousData hands the *old filter's* page back under the new
    // filter's key while the new page loads; accumulating it would seed the
    // new list with rows that do not match. Callers using placeholderData
    // pass this through so those rows are shown but never accumulated.
    isPlaceholderData?: boolean;
  },
  idOf: (item: T) => string | number,
  resetKey?: string,
): {
  items: T[];
  nextCursor: C | undefined;
  loadMore: (() => void) | undefined;
  cursor: C | undefined;
  reset: () => void;
} {
  // The cursor's type is decided by the endpoint: most are opaque base64
  // strings, while the two ledgers use a bare int64. Hence a type parameter
  // rather than a hard-coded string.
  const [cursor, setCursor] = useState<C | undefined>(undefined);
  const [, bump] = useState(0);
  const accRef = useRef<T[]>([]);
  const seenRef = useRef<Set<string | number>>(new Set());
  const lastResetKey = useRef(resetKey);

  // Drop the accumulation when the filter changes: a list mixing results from
  // two different filters is harder to explain than an empty one.
  if (lastResetKey.current !== resetKey) {
    lastResetKey.current = resetKey;
    accRef.current = [];
    seenRef.current = new Set();
    if (cursor !== undefined) setCursor(undefined);
  }

  const rows = page.data?.items ?? page.data?.data;
  const placeholder = page.isPlaceholderData === true;
  useEffect(() => {
    if (!rows || placeholder) return;
    let added = false;
    for (const item of rows) {
      const id = idOf(item);
      if (seenRef.current.has(id)) continue;
      seenRef.current.add(id);
      accRef.current.push(item);
      added = true;
    }
    if (added) bump((n) => n + 1);
  }, [rows, idOf, placeholder]);

  const nextCursor = page.data?.next_cursor;
  const reset = useCallback(() => {
    accRef.current = [];
    seenRef.current = new Set();
    setCursor(undefined);
    bump((n) => n + 1);
  }, []);
  const loadMore = useMemo(
    () =>
      nextCursor !== undefined && nextCursor !== null && !page.isFetching
        ? () => setCursor(nextCursor)
        : undefined,
    [nextCursor, page.isFetching],
  );

  // While the placeholder shows, render it as-is: the accumulation was just
  // cleared for the new filter and would otherwise flash empty.
  const items = placeholder && rows ? rows : accRef.current;
  return { items, nextCursor, loadMore, cursor, reset };
}

/**
 * useScopedCursor is the page cursor for a list whose query also carries a
 * filter (search, generation). The cursor is only meaningful for the filter it
 * was minted under: a page-two cursor sent along with a new search term seeks
 * into the new result set at a position that belongs to the old one, and every
 * match sorting before it silently disappears — the page can read "no matches"
 * while matches exist.
 *
 * The cursor is therefore stored together with the scope it was set under and
 * reads back as undefined the moment the scope changes, synchronously, before
 * the query for the new scope is built. An effect that resets it would fire one
 * request with the stale cursor first.
 */
export function useScopedCursor<C = string>(
  scope: string,
): [C | undefined, (cursor: C | undefined) => void] {
  const [state, setState] = useState<{ scope: string; cursor: C | undefined }>({
    scope,
    cursor: undefined,
  });
  const set = useCallback((cursor: C | undefined) => setState({ scope, cursor }), [scope]);
  return [state.scope === scope ? state.cursor : undefined, set];
}

/** LoadMoreButton is the one shape of "load more": it renders nothing when there
 * is no next page. */
export function LoadMoreButton({
  onClick,
  pending,
  label,
}: {
  onClick: (() => void) | undefined;
  pending: boolean;
  label: string;
}) {
  if (!onClick) return null;
  return (
    <Button variant="outline" onClick={onClick} loading={pending}>
      {label}
    </Button>
  );
}
