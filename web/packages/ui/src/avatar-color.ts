import { AVATAR_COLORS, AVATAR_INK } from "@fairlb/brand";

const utf8 = new TextEncoder();

/**
 * Picks an account's avatar colour from the brand palette.
 *
 * Keyed on the email: it is unique per account, always present, and cannot be
 * changed. Display names collide and are editable, so colouring by name would
 * reshuffle on a rename — and the colour exists precisely to tell two accounts
 * apart when their initials agree.
 *
 * FNV-1a over the UTF-8 bytes, spelled out rather than pulled in. It is a dozen
 * lines, and the mapping has to stay bit-for-bit stable: swapping the
 * implementation silently recolours every account in the product, and nothing
 * anywhere would report it. The test pins concrete email→colour pairs for that
 * reason, including a non-ASCII one.
 *
 * The encode step is load-bearing, not ceremony. Hashing `charCodeAt` directly
 * runs FNV over UTF-16 code units, which agrees with the specified algorithm for
 * ASCII and diverges for everything else — measured: `josé@example.com` landed
 * on a different palette entry under the two readings. That kind of divergence
 * survives exactly as long as nobody checks, and then a well-meaning swap to a
 * real FNV-1a library recolours a subset of accounts with every ASCII pin still
 * green.
 *
 * Its own module, not a helper inside the component, so the mapping can be
 * tested without loading React and the design system.
 */
export function avatarColor(email: string): string {
  let hash = 0x811c9dc5;
  for (const byte of utf8.encode(email)) {
    hash ^= byte;
    // The 32-bit FNV prime as shifts. `hash * 16777619` exceeds the range where
    // a double holds every integer, and the low bits start coming back wrong.
    hash = (hash + ((hash << 1) + (hash << 4) + (hash << 7) + (hash << 8) + (hash << 24))) >>> 0;
  }
  return AVATAR_COLORS[hash % AVATAR_COLORS.length] ?? AVATAR_COLORS[0];
}

/**
 * The avatar's inline style: background and foreground together.
 *
 * They travel as one value on purpose. The palette was solved for contrast
 * against `AVATAR_INK` specifically, so a caller that took the background and
 * supplied its own foreground would be reading a guarantee that no longer holds.
 * Returning both also keeps the colour out of the class list, where Tailwind's
 * static resolution would purge a computed value from the production build.
 */
export function avatarStyle(email: string): { backgroundColor: string; color: string } {
  return { backgroundColor: avatarColor(email), color: AVATAR_INK };
}
