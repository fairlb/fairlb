import { AVATAR_COLORS, AVATAR_INK } from "@fairlb/brand";
import { contrastRatio, rgbFromHex } from "./contrast";
import { describe, expect, it } from "vitest";
import { avatarColor, avatarStyle } from "./avatar-color";

describe("avatarColor", () => {
  /**
   * The pinned pairs are the point of this file.
   *
   * Nothing in the product reports a changed avatar colour: every account keeps
   * rendering, just in a different colour, so replacing the hash — or reordering
   * the palette — would recolour everyone silently. These values were taken from
   * the implementation once and are frozen here; a diff on this list is the only
   * warning anyone gets.
   *
   * The hash itself is verified separately below, so a failure here means the
   * mapping moved, not that the arithmetic is wrong.
   */
  it("maps a known account to a fixed colour", () => {
    expect(avatarColor("ada@example.com")).toBe("#846D2A");
    expect(avatarColor("grace@example.com")).toBe("#187C79");
    expect(avatarColor("linus@example.com")).toBe("#367D4D");
    expect(avatarColor("bo@example.com")).toBe("#AC4D8D");
    // A non-ASCII address, because that is where the hash's input encoding shows.
    // Every pin above is ASCII, where UTF-8 bytes and UTF-16 code units agree —
    // so before this line the suite could not tell the two apart, and an
    // implementation that hashed code units passed as readily as one that hashed
    // bytes. It does not any more: this entry moves if the encoding changes.
    expect(avatarColor("josé@example.com")).toBe("#2B65EE");
  });

  /**
   * FNV-1a's published 32-bit vectors, run through the palette.
   *
   * This is what makes the pins above meaningful rather than circular: the
   * expected values here were not produced by this code. `""` hashes to the
   * offset basis 0x811c9dc5, `"a"` to 0xe40c292c, `"foobar"` to 0xbf9cf968 —
   * take those modulo the palette length and the index follows.
   *
   * All three vectors are ASCII, which is why they cannot by themselves show
   * that the input is encoded to UTF-8 before hashing; the non-ASCII pin above
   * is what covers that half.
   */
  it("implements FNV-1a, not merely something stable", () => {
    expect(avatarColor("")).toBe(AVATAR_COLORS[0x811c9dc5 % AVATAR_COLORS.length]);
    expect(avatarColor("a")).toBe(AVATAR_COLORS[0xe40c292c % AVATAR_COLORS.length]);
    expect(avatarColor("foobar")).toBe(AVATAR_COLORS[0xbf9cf968 % AVATAR_COLORS.length]);
  });

  it("gives the same account the same colour every time", () => {
    expect(avatarColor("ada@example.com")).toBe(avatarColor("ada@example.com"));
  });

  it("separates accounts whose initials would collide", () => {
    // Both render "A", which is the case the colour exists for.
    expect(avatarColor("ada@example.com")).not.toBe(avatarColor("aaron@example.com"));
  });

  it("only ever returns a palette entry", () => {
    const palette = new Set<string>(AVATAR_COLORS);
    for (let i = 0; i < 500; i++) {
      expect(palette.has(avatarColor(`user${i}@example.com`))).toBe(true);
    }
  });

  it("reaches every colour, so no entry is unreachable", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 500; i++) seen.add(avatarColor(`user${i}@example.com`));
    expect(seen.size).toBe(AVATAR_COLORS.length);
  });
});

/**
 * The palette's contrast constraints, as a test rather than a note.
 *
 * These values were solved for, not picked, and the band that satisfies all
 * three is narrow — roughly 0.13 to 0.18 relative luminance. The arithmetic
 * itself lives in `./contrast` and proves itself against published WCAG figures
 * in its own suite, so a failure here means a colour moved, not that the
 * measurement is wrong. Adjusting one entry
 * because it "looks a bit dark" is exactly how a palette drifts out of the band,
 * and on the theme the author was not looking at. Stating the constraint here
 * means the next person gets a failing test instead of a shipped regression.
 */
describe("AVATAR_COLORS", () => {
  const LIGHT_SURFACE = rgbFromHex("#FFFFFF"); // --flb-surface, light
  const DARK_SURFACE = rgbFromHex("#131A24"); // --flb-surface, dark
  const INK = rgbFromHex(AVATAR_INK);

  it("carries its initials at AA", () => {
    for (const color of AVATAR_COLORS) {
      expect(
        contrastRatio(rgbFromHex(color), INK),
        `${color} against ${AVATAR_INK}`,
      ).toBeGreaterThanOrEqual(4.5);
    }
  });

  it("pairs every background with that foreground", () => {
    for (const email of ["ada@example.com", "grace@example.com", "bo@example.com"]) {
      expect(avatarStyle(email)).toEqual({
        backgroundColor: avatarColor(email),
        color: AVATAR_INK,
      });
    }
  });

  it("stays separable from the page in both themes", () => {
    for (const color of AVATAR_COLORS) {
      expect(
        contrastRatio(rgbFromHex(color), LIGHT_SURFACE),
        `${color} on the light surface`,
      ).toBeGreaterThanOrEqual(4.5);
      expect(
        contrastRatio(rgbFromHex(color), DARK_SURFACE),
        `${color} on the dark surface`,
      ).toBeGreaterThanOrEqual(3);
    }
  });

  it("has no duplicate entries, which would waste a slot", () => {
    expect(new Set<string>(AVATAR_COLORS).size).toBe(AVATAR_COLORS.length);
  });
});
