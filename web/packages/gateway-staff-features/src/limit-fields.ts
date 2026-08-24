/**
 * Reading a limit out of a text field.
 *
 * The fields are text and not numbers because an empty field is a state a
 * number input cannot hold, and for a rate ceiling empty is a real setting: it
 * means there is no ceiling. A number input collapses that into 0, and 0 is the
 * opposite setting -- a ceiling of zero refuses every request.
 *
 * So the parsing is done here, once, and both halves of the question are
 * answered separately: is what was typed acceptable, and what value does it
 * stand for.
 */

/** The positive integer this text stands for, or undefined when it stands for none. */
export function positiveIntOf(text: string): number | undefined {
  const trimmed = text.trim();
  if (trimmed === "") return undefined;
  // Number() rather than parseInt(): parseInt("12abc") is 12, which would let a
  // typo through as a limit the operator never typed.
  const n = Number(trimmed);
  if (!Number.isFinite(n) || !Number.isInteger(n) || n < 1) return undefined;
  return n;
}

/**
 * Whether this text is acceptable in a field where blank means "no limit".
 *
 * Blank is valid and a malformed number is not; the difference matters, because
 * refusing to save on a blank field would make "no limit" unsettable through
 * the interface even though the contract accepts it.
 */
export function isBlankOrPositive(text: string): boolean {
  return text.trim() === "" || positiveIntOf(text) !== undefined;
}
