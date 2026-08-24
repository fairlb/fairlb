import { Button } from "./button";
import { Banner } from "@cloudflare/kumo/components/banner";
import { ArrowsClockwiseIcon } from "@phosphor-icons/react";
import { useI18n } from "@fairlb/i18n";
import { useEffect, useState, useSyncExternalStore } from "react";
import { LoadingState } from "./loading-state";
import { ErrorState } from "./status-pages";

/**
 * Where "this tab is running a shell whose build has been replaced" is handled.
 *
 * The frontend assets are embedded in a single binary, so one deployment carries
 * exactly one set of them: the moment a new container comes up, the hashed
 * chunks the running shell refers to are simply gone. Any open tab therefore
 * fails the next time it enters a lazily loaded route — whichever route that
 * happens to be.
 *
 * There are two channels here, sharing one signal but taking different actions:
 *
 *   - A failed preload is speculative. The router preloads on intent, so merely
 *     moving the pointer across a link fetches its chunk. The reader has clicked
 *     nothing, and reloading at that moment would swallow the form they are
 *     filling in. So this channel reports without acting: it raises the banner
 *     and leaves the timing to the reader.
 *   - A route that throws while rendering has already happened. That page cannot
 *     appear, and the previous one is already unmounted, so a reload costs
 *     nothing further. This channel reloads automatically, once.
 *
 * `preventDefault()` is deliberately not called on the preload error. The
 * bundler's preload helper catches the failure, and when the event has been
 * default-prevented it swallows the exception and returns normally — so the
 * dynamic import resolves to undefined, and a real navigation then receives not
 * "loading failed" but a module that does not exist, failing again in a shape
 * far harder to understand. Letting it throw as usual lets the second channel
 * catch it.
 */

/**
 * The key and time window of the one-shot sentinel. It suppresses only "we just
 * reloaded automatically", never a genuinely later change of build.
 *
 * The window has to span the whole reload-then-hit-it-again round trip, or it
 * fails to suppress the very thing it exists for: if a new build really is
 * broken and the reader takes a dozen seconds to reach the next lazily loaded
 * route, too short a window classifies that as a fresh incident, reloading again
 * at that interval — still an endlessly refreshing page.
 *
 * Erring long costs only this: two deployments within a minute means the second
 * does not recover automatically. The banner and the error page are both still
 * there, and one click reloads. That is not a failure state.
 */
const RELOAD_STAMP_KEY = "flb-stale-build-reload";
const RELOAD_LOOP_WINDOW_MS = 60_000;

/**
 * How each browser words this failure. The criterion is applied to the thing
 * being judged — the error text itself — rather than narrowed to an enumerated
 * allowlist: failing to recognise one degrades to the ordinary error page, which
 * is no worse than before.
 */
const STALE_CHUNK_SIGNS = [
  "failed to fetch dynamically imported module", // Chromium
  "error loading dynamically imported module", // Firefox
  "importing a module script failed", // Safari
  "unable to preload css for", // the preload helper's CSS-dependency branch
];

/** isStaleChunkError reports whether an exception means "this chunk is no longer
 * part of the deployed build". */
export function isStaleChunkError(error: unknown): boolean {
  const message =
    error instanceof Error
      ? error.message
      : typeof error === "string"
        ? error
        : String(error ?? "");
  const lower = message.toLowerCase();
  return STALE_CHUNK_SIGNS.some((sign) => lower.includes(sign));
}

/**
 * shouldAutoReload decides whether to reload this time.
 *
 * The sentinel exists to prevent an infinite refresh: if the new build really is
 * broken, the reload fails the same way and reloads again, pinning the reader to
 * a blank page that never settles. It is a time window rather than a boolean
 * because the mark must eventually give way to a genuinely later change of
 * build, which is a different incident and does deserve one automatic recovery.
 */
export function shouldAutoReload(
  now: number,
  stamp: string | null,
  windowMs: number = RELOAD_LOOP_WINDOW_MS,
): boolean {
  if (!stamp) return true;
  const at = Number(stamp);
  if (!Number.isFinite(at)) return true;
  const elapsed = now - at;
  // A clock moved backwards puts the stamp in the future. That is not "we just
  // tried", so treat it as never having tried.
  return elapsed < 0 || elapsed > windowMs;
}

/** sessionStorage throws in private mode or when storage is disabled; neither
 * reading nor writing it may take the page down with it. */
function readReloadStamp(): string | null {
  try {
    return window.sessionStorage.getItem(RELOAD_STAMP_KEY);
  } catch {
    return null;
  }
}

function writeReloadStamp(now: number): void {
  try {
    window.sessionStorage.setItem(RELOAD_STAMP_KEY, String(now));
  } catch {
    // If it cannot be stored, degrade to having no guard: the worst case is one
    // extra reload, and blocking recovery is the worse side to fail on.
  }
}

let staleDetected = false;
const listeners = new Set<() => void>();

/** markStaleBuild records that the running shell is not the deployed build; the
 * banner rises from this. */
function markStaleBuild(): void {
  if (staleDetected) return;
  staleDetected = true;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot(): boolean {
  return staleDetected;
}

/**
 * installStaleBuildDetection attaches the preload-failure listener and returns
 * the function that removes it. Call it once at the application entry point.
 */
export function installStaleBuildDetection(): () => void {
  const onPreloadError = () => markStaleBuild();
  window.addEventListener("vite:preloadError", onPreloadError);
  return () => window.removeEventListener("vite:preloadError", onPreloadError);
}

/**
 * The polling rhythm for build detection. Both applications share one copy,
 * because this is a policy rather than an implementation detail of either: two
 * copies means tuning one and forgetting the other, with nothing to report it.
 *
 * The interval and the refetch on focus each cover one shape: a tab left open
 * and untouched, and a tab returned to after a long absence. The endpoint it
 * polls is a few dozen bytes of public JSON, so this frequency is negligible.
 */
export const NEW_BUILD_QUERY_OPTIONS = {
  refetchInterval: 5 * 60_000,
  // A background tab need not poll: the refetch on focus covers the return.
  refetchIntervalInBackground: false,
  refetchOnWindowFocus: true,
  staleTime: 5 * 60_000,
  // This is a background probe, not something anyone asked for: on failure it
  // waits for the next round rather than retrying. The silent flag keeps the
  // global failure toast from firing — this request is expected to fail briefly
  // during a deployment, and toasting would put pure noise in front of every
  // open tab on every deploy.
  retry: false,
  meta: { silent: true },
} as const;

/**
 * The build identifier observed at startup. It is module-level rather than
 * component state because it has to survive component unmounts while resetting
 * on a page reload — "this page load" is exactly the scope it wants.
 */
let bootVersion: string | undefined;

/** For tests only: module-level state leaks between cases inside one test
 * process. */
export function resetStaleBuildForTest(): void {
  staleDetected = false;
  bootVersion = undefined;
}

/**
 * useStaleBuild folds both channels into a single boolean.
 *
 * Pass the build identifier reported by the server, which changes with every
 * deployment. The first value seen becomes the baseline, and any later mismatch
 * means the build has changed. Nothing has to be defined into the bundle at
 * build time: "the version observed at startup" is already a runtime fact.
 */
export function useStaleBuild(version?: string): boolean {
  const flagged = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
  useEffect(() => {
    if (!version) return;
    if (bootVersion === undefined) {
      bootVersion = version;
      return;
    }
    if (version !== bootVersion) markStaleBuild();
  }, [version]);
  return flagged;
}

/**
 * NewBuildBanner tells the reader a new build is live and a reload is all it
 * takes.
 *
 * It is deliberately not dismissible: this is a standing condition rather than
 * an announcement, and dismissing it would only leave the reader on a shell that
 * is going to fail eventually. The action that clears it is in the banner
 * itself, one click away.
 */
export function NewBuildBanner({ visible, onReload }: { visible: boolean; onReload?: () => void }) {
  const { t } = useI18n();
  if (!visible) return null;
  return (
    <Banner
      variant="default"
      role="status"
      aria-live="polite"
      icon={<ArrowsClockwiseIcon weight="fill" aria-hidden />}
      title={t("newBuildTitle")}
      description={t("newBuildBody")}
      action={
        <Button
          type="button"
          size="sm"
          variant="secondary"
          onClick={onReload ?? (() => location.reload())}
        >
          {t("reload")}
        </Button>
      }
    />
  );
}

/**
 * RouteErrorState is the route-level error fallback, used in place of ErrorState
 * directly.
 *
 * A stale-build failure has nothing in common with "this page is broken": a
 * message such as "Failed to fetch dynamically imported module:
 * …/assets/security-abc123.js" is something the reader can neither judge nor act
 * on. So this branch neither shows that URL nor expects anyone to press reload.
 */
export function RouteErrorState({ error, onReload }: { error: unknown; onReload?: () => void }) {
  const { t } = useI18n();
  const stale = isStaleChunkError(error);
  // Decided once, on the first render: after the reload writes the sentinel, a
  // later render would read back the value just written.
  const [recovering] = useState(() => stale && shouldAutoReload(Date.now(), readReloadStamp()));

  useEffect(() => {
    if (!recovering) return;
    writeReloadStamp(Date.now());
    // `onReload` plays the same role as ErrorState's retry hook: location.reload
    // cannot be replaced in a browser, and this seam is the only way a test can
    // demonstrate that the reload did — or did not — happen.
    (onReload ?? (() => location.reload()))();
  }, [recovering, onReload]);

  if (recovering) return <LoadingState label={t("newBuildReloading")} />;
  if (stale) return <ErrorState message={t("staleBuildBody")} />;
  return <ErrorState message={error instanceof Error ? error.message : String(error)} />;
}
