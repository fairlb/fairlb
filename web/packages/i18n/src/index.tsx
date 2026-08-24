import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { en, type CoreMessageKey } from "./messages";

export type Locale = "en" | "zh";
export type { CoreMessageKey };

/**
 * The dictionaries in this build, as a type-level table.
 *
 * The core dictionary is always present. An application that carries copy of
 * its own adds itself with a declaration merge:
 *
 *     declare module "@fairlb/i18n" {
 *       interface MessageDictionaries {
 *         console: ConsoleMessageKey;
 *       }
 *     }
 *
 * A table rather than a plain type alias, because the whole point is that the
 * set of dictionaries differs per application and no single file can know it.
 * The admin app declares nothing, so `MessageKey` there is exactly the core key
 * set — which is what makes a key that has moved out of the core dictionary a
 * compile error in the admin app rather than an empty label at run time.
 */
export interface MessageDictionaries {
  core: CoreMessageKey;
}

export type MessageKey = MessageDictionaries[keyof MessageDictionaries];

const STORAGE_KEY = "flb-locale";

/** Interpolation variables. Placeholders are written `{name}`, the same syntax the
 * server-side notification templates use. */
export type Vars = Record<string, string | number>;

interface I18nContextValue {
  locale: Locale;
  setLocale: (l: Locale) => void;
  t: (key: MessageKey, vars?: Vars) => string;
  formatNumber: (value: number) => string;
  formatDate: (value: string | number | Date) => string;
  formatDateTime: (value: string | number | Date) => string;
  /**
   * Second-resolution timestamp, for per-event rows only (request logs).
   *
   * `formatDateTime` uses `timeStyle: "short"`, which has no seconds. That is
   * right for "last signed in" or "created at", where the extra two digits are
   * noise. A request log page, however, exists to answer "what happened to the
   * request I just made": without seconds, several requests inside the same
   * minute can neither be told apart nor put in order — and those are exactly
   * the rows being looked at.
   *
   * Two formatters rather than adding seconds to `formatDateTime`: session
   * lists, credit expiry and payment records do not need them, and turning
   * seconds on everywhere spreads that noise across every page.
   */
  formatTimestamp: (value: string | number | Date) => string;
}

const I18nContext = createContext<I18nContextValue | null>(null);

/**
 * A dictionary an application contributes on top of the core one.
 *
 * The zh half is a function rather than an object so that it stays behind a
 * dynamic import, the same way the core zh half is: an application's own copy
 * is usually the larger of the two, and shipping it in the first payload would
 * undo the split for every reader who never switches language.
 */
export interface MessagePack {
  en: Record<string, string>;
  zh: () => Promise<Record<string, string>>;
}

const packs: MessagePack[] = [{ en, zh: async () => (await import("./messages.zh")).zh }];

/**
 * The merged en dictionary, rebuilt whenever a pack registers.
 *
 * en is merged eagerly because it is both the fallback and the answer to
 * `isMessageKey`, and neither may wait on a promise.
 */
let mergedEn: Record<string, string> = { ...en };
let mergedZh: Record<string, string> | undefined;

/**
 * registerMessages adds an application's own dictionary to the ones in play.
 *
 * Call it at module scope, before anything renders — an application's entry
 * module importing its `i18n` module for the side effect is enough, since ES
 * module evaluation finishes before the entry body runs. Registering later is
 * not an error, but any text already on screen was resolved without it.
 *
 * Two packs that define the same key would silently leave the later one
 * winning, and neither the type checker nor the linter can see that.
 *
 * That used to be checked outside the runtime by `check-i18n`, which was
 * deleted in 045c13ae along with the rest of `web/scripts/` and has not been
 * restored — so **nothing checks it today**. Stated here rather than left
 * reading as though the guard were still in place: a declaration that quietly
 * stops being true is worse than no declaration (ADR-0122). Restoring it is
 * registered in PROGRESS's todo list.
 */
export function registerMessages(pack: MessagePack): void {
  if (packs.includes(pack)) return;
  packs.push(pack);
  mergedEn = { ...mergedEn, ...pack.en };
  // Whatever zh was already assembled is now short of this pack's half. It has
  // to be dropped rather than patched: the pack's zh side is behind a dynamic
  // import and cannot be merged synchronously here.
  mergedZh = undefined;
}

/**
 * isMessageKey reports whether a string that arrived at runtime is a key the
 * dictionaries actually have.
 *
 * `t()` only accepts a MessageKey, but the server hands out message keys too:
 * every entry in the settings registry carries a `description_key` whose text
 * the client resolves for the current locale. That string is not knowable at
 * compile time, so writing `t(key as MessageKey)` costs this: when no
 * dictionary has the key, nothing resolves and the key itself is what reaches
 * the screen. The assertion only turns the check off; it does not make the key
 * exist.
 *
 * Checked against the merged en, since the type of each zh dictionary already
 * guarantees it carries the same key set as its own en half. Merged rather than
 * core-only: the keys the server hands out are rendered by application pages
 * whose copy lives in that application's dictionary, so a core-only check would
 * reject every one of them and the explanation column would fall back to raw
 * server data.
 */
export function isMessageKey(key: string): key is MessageKey {
  return Object.prototype.hasOwnProperty.call(mergedEn, key);
}

/**
 * browserTZ returns the reader's IANA time zone name (`Asia/Shanghai` and the
 * like).
 *
 * Two uses: telling the server which zone to cut daily buckets on, and stating
 * on the page which zone the times are shown in. The second is not decoration —
 * day boundaries on the usage page and timestamps on the log page both get
 * compared against server-side logs, and that comparison cannot be made without
 * knowing the zone.
 *
 * Returns an empty string when unavailable, letting the server fall back to UTC.
 * It lives in this package rather than being written out per page because it is
 * the other half of what `Intl.DateTimeFormat` does, and this package already
 * owns that half.
 */
export function browserTZ(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    // Very old browsers have no resolvedOptions. This is only a label, and a
    // missing label must not blank the whole page.
    return "";
  }
}

function detect(): Locale {
  let saved: string | null = null;
  try {
    saved = localStorage.getItem(STORAGE_KEY);
  } catch {
    // Storage is unavailable in private mode; that must not block startup.
  }
  if (saved === "en" || saved === "zh") return saved;
  return navigator.language.toLowerCase().startsWith("zh") ? "zh" : "en";
}

/**
 * ready reports whether the dictionaries for this locale are in hand. en always
 * is — it is both the schema and the fallback — and zh is filled in on demand.
 *
 * The assembled zh lives at module level rather than in state: switching back to
 * a language that has already been loaded should not wait on the network again,
 * and the natural lifetime of that cache is the whole page anyway.
 */
function ready(locale: Locale): boolean {
  return locale === "en" || mergedZh !== undefined;
}

async function loadDict(locale: Locale): Promise<void> {
  if (ready(locale)) return;
  // Every pack at once rather than one after another: they are separate chunks
  // with no ordering between them, and awaiting them in turn would add one
  // round trip per application dictionary to the language switch.
  const halves = await Promise.all(packs.map((pack) => pack.zh()));
  mergedZh = Object.assign({}, ...halves) as Record<string, string>;
}

/**
 * localeTag converts the internal locale into a BCP 47 tag.
 *
 * This is the single source: everywhere an `Intl.*` object is constructed, the
 * tag comes from here. Chart axis time formatting is included — it used to have
 * no formatter at all and fell through to the charting library's built-in time
 * axis labels, whose language is a snapshot of `document.documentElement.lang`
 * taken the moment that module is evaluated, and never updated afterwards. The
 * visible effect was that opening a dashboard under one language and then
 * switching kept the old language on the axis until a full reload.
 */
export function localeTag(locale: Locale): string {
  return locale === "zh" ? "zh-CN" : "en-US";
}

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(detect);
  // The first frame already has to know whether the dictionary is there: a
  // reader whose language is not the built-in one would otherwise see one frame
  // of the fallback before the switch, which is a visible flash. The lazy
  // initialiser reads the module-level cache synchronously, so a second mount
  // (switching back, or a route remount) is ready immediately.
  const [loadedForLocale, setLoaded] = useState(() => ready(locale));

  useEffect(() => {
    if (ready(locale)) {
      setLoaded(true);
      return;
    }
    let alive = true;
    setLoaded(false);
    void loadDict(locale)
      .then(() => {
        if (alive) setLoaded(true);
      })
      .catch((error: unknown) => {
        // A locale chunk is optional content, not an application availability
        // dependency. Keep the saved preference so selecting Chinese again (or
        // reloading later) retries the import, but render the English fallback
        // now instead of leaving the root blank forever.
        console.error("failed to load locale dictionary; falling back to English", error);
        if (alive) {
          setLocaleState("en");
          setLoaded(true);
        }
      });
    return () => {
      alive = false;
    };
  }, [locale]);

  useEffect(() => {
    document.documentElement.lang = locale === "zh" ? "zh-CN" : "en";
  }, [locale]);

  const formatters = useMemo(() => {
    const tag = localeTag(locale);
    const number = new Intl.NumberFormat(tag);
    const date = new Intl.DateTimeFormat(tag);
    const dateTime = new Intl.DateTimeFormat(tag, { dateStyle: "medium", timeStyle: "short" });
    // Only `timeStyle: "medium"` carries seconds. No extra constraint such as
    // hourCycle: whether the clock is 12- or 24-hour belongs to the locale;
    // what is being decided here is precision.
    const timestamp = new Intl.DateTimeFormat(tag, { dateStyle: "medium", timeStyle: "medium" });
    return {
      formatNumber: (value: number) => number.format(value),
      formatDate: (value: string | number | Date) => date.format(new Date(value)),
      formatDateTime: (value: string | number | Date) => dateTime.format(new Date(value)),
      formatTimestamp: (value: string | number | Date) => timestamp.format(new Date(value)),
    };
  }, [locale]);

  const setLocale = (l: Locale) => {
    try {
      localStorage.setItem(STORAGE_KEY, l);
    } catch {
      // The switch still applies to this session; after a reload the browser
      // preference takes over again.
    }
    setLocaleState(l);
  };
  const t = (key: MessageKey, vars?: Vars) => {
    // Fall back to en when the dictionary is not ready. `loadedForLocale`
    // already keeps this frame from rendering, so reaching here means either a
    // caller outside the provider (there is none) or a failed load — and in
    // that case English beats nothing.
    //
    // Falling back a second time, to the key itself, is what a key no
    // registered dictionary carries produces. That can only happen through
    // `isMessageKey` on a string the server supplied, or through an
    // application that renders before registering its own dictionary; both
    // print something a person can search the source for, which "undefined"
    // was not.
    const raw = (locale === "zh" ? mergedZh?.[key] : undefined) ?? mergedEn[key] ?? key;
    if (!vars) return raw;
    // A placeholder with no value is left as it stands: a visible `{count}` is
    // an obvious mistake, whereas substituting an empty string quietly drops a
    // number and costs far more to track down.
    return raw.replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k]) : m));
  };

  // Render nothing until the dictionary has arrived, and show no loading
  // indicator while waiting: the chunk is local and usually there within
  // milliseconds, so a flashed spinner is noisier than blank. A failed load
  // switches to English above, so this branch never leaves the root blank.
  if (!loadedForLocale) return null;

  return (
    <I18nContext.Provider value={{ locale, setLocale, t, ...formatters }}>
      {children}
    </I18nContext.Provider>
  );
}

export function useI18n(): I18nContextValue {
  const ctx = useContext(I18nContext);
  // Not a dictionary entry: this is a wiring error aimed at whoever wrote the
  // call, and it reaches the console only — never the screen.
  if (!ctx) throw new Error("useI18n must be called inside a LocaleProvider");
  return ctx;
}

/**
 * useDisplayDate renders an ISO string that may be absent and may be malformed.
 *
 * It replaces three copies of the same function body that had accumulated on
 * separate pages, each calling `toLocaleString()` — which follows the browser
 * locale rather than the application's language switch. Those copies existed
 * precisely because each lived at module scope where no hook is reachable, so
 * the single place they collapse into has to be a hook itself.
 *
 * A malformed string is returned unchanged rather than shown as an em dash: the
 * dash means "there is no value here", while an unparsable timestamp means
 * there is a value and it is wrong. Hiding it removes the only evidence of who
 * wrote it.
 */
export function useDisplayDate(): (iso?: string | null) => string {
  const { formatDateTime } = useI18n();
  return (iso?: string | null) => {
    if (!iso) return "—";
    const d = new Date(iso);
    return Number.isNaN(d.valueOf()) ? iso : formatDateTime(d);
  };
}
