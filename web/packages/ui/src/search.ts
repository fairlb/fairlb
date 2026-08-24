/**
 * pickStrings is the reader used by route search validation: it accepts only the
 * string keys that are actually present.
 *
 * It performs no enumeration check. The URL can be edited by anyone, and invalid
 * values fall back within the page itself.
 *
 * Every key being optional is a hard constraint: a required key typed as
 * `| undefined` would force *every link pointing at that route* to pass search
 * explicitly, turning cross-page links into type errors across the board.
 *
 * It lives in the shared package because both applications need it, so their
 * filtering behaviour is guaranteed identical by having one implementation.
 */
export function pickStrings<K extends string>(
  s: Record<string, unknown>,
  keys: readonly K[],
): Partial<Record<K, string>> {
  const out: Partial<Record<K, string>> = {};
  for (const k of keys) if (typeof s[k] === "string" && s[k]) out[k] = s[k] as string;
  return out;
}

/**
 * The local filtering criterion for the command palette.
 *
 * It sits in the shared package for the same reason as `pickStrings`: the
 * palette's sources are split across a feature package and the application
 * itself, and both sides must apply the same criterion. Otherwise "wait for two
 * characters" and "case-insensitive substring" would differ between the two
 * groups of results — an inconsistency nobody would ever report.
 */

/** Do not search on fewer than two characters: a single letter matches half the
 * database, which is both slow and useless. */
export const PALETTE_MIN_QUERY = 2;

/**
 * matchesQuery is the filtering criterion: a case-insensitive substring hit on
 * any of the candidate fields.
 *
 * There is no fuzzy-matching library here, and none is added for this: fuzzy
 * matching pays off once there are more results than can be ranked by eye, and
 * the palette shows five per group. Highlighting is optional, so omitting it
 * costs no functionality.
 */
export function matchesQuery(query: string, ...fields: (string | undefined)[]): boolean {
  const q = query.toLowerCase();
  return fields.some((f) => f?.toLowerCase().includes(q));
}
