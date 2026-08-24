import { ApiError } from "./mutator";
import { errorCodeDefs, type ErrorCode } from "./errors.gen";

/**
 * apiErrorCode returns the server's error code so a call site can branch on it,
 * for example to attach an actionable hint to one specific failure. Do not read
 * `err.problem?.code` by hand in a page: that spreads the "is this an ApiError"
 * test across the whole UI.
 */
export function apiErrorCode(err: unknown): ErrorCode | undefined {
  return err instanceof ApiError ? (err.problem?.code as ErrorCode | undefined) : undefined;
}

/**
 * apiErrorStatus returns the HTTP status so a call site can tell "this record
 * does not exist" apart from "this request failed". Same reasoning as
 * `apiErrorCode`: do not read `err.status` by hand in a page.
 *
 * The case that motivates it is 404. A Problem `detail` is written for whoever
 * operates the server and is not translated, so rendering it verbatim shows the
 * user untranslated text — and "not found" belongs in a localized empty state
 * rather than an error banner in the first place.
 */
export function apiErrorStatus(err: unknown): number | undefined {
  return err instanceof ApiError ? err.status : undefined;
}

/**
 * apiErrorMessage maps an ApiError to text a user can read: the Problem
 * `detail` when the server sent one, otherwise the registered title for the
 * error code, otherwise the raw title or the status.
 */
export function apiErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.problem?.detail) return err.problem.detail;
    const code = err.problem?.code as ErrorCode | undefined;
    const def = code ? errorCodeDefs[code] : undefined;
    if (def) {
      const lang = typeof document === "undefined" ? "en" : document.documentElement.lang;
      return lang.toLowerCase().startsWith("zh") ? def.titleZh : def.title;
    }
    return err.problem?.title || `HTTP ${err.status}`;
  }
  return err instanceof Error ? err.message : String(err);
}
