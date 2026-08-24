import { Loader } from "@cloudflare/kumo/components/loader";

/** One loading state for queries: a visible spinner plus status text a screen
 * reader can pick up. */
export function LoadingState({ label }: { label: string }) {
  return (
    <div
      role="status"
      aria-live="polite"
      aria-busy="true"
      className="flex min-h-16 items-center justify-center"
    >
      <Loader />
      <span className="sr-only">{label}</span>
    </div>
  );
}
