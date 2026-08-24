import type { ReactNode } from "react";

/**
 * PageRail is the page-level two-column layout: a main column plus a side rail
 * on wide screens.
 *
 * A page with little content looks empty on a wide screen. The right answer is
 * to fill the spare width with a rail, not to narrow the container — the latter
 * is the approach this replaced.
 *
 * It belongs to the page, not to the shell. The first version made it a prop of
 * the application shell, which was the wrong shape: the rail's content depends
 * on the page's data, which the shell does not have, so handing it up required
 * context plus an effect — and every navigation then rendered one frame of the
 * previous page's rail before it snapped. The layout component this is modelled
 * on is likewise a page-level component that brings its own container, header
 * and two columns, rather than part of a shell.
 *
 * The breakpoint is 1280px. Below it the rail moves *below* the main column
 * instead of squeezing in beside it, because a 380px rail alongside the main
 * column at 1024px would crush the main column to just over 600px.
 *
 * DOM order and visual order always agree: the main task first, the supporting
 * summary after, so that screen-reader and keyboard order never run opposite to
 * what is seen. Critical state that has to be encountered first belongs in the
 * main column's page header, not in an inverted layout.
 *
 * Do not use it without something real to put in the rail. A rail filled for the
 * sake of filling is worse than the empty space.
 */
export function PageRail({ rail, children }: { rail: ReactNode; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-6 xl:flex-row xl:gap-[var(--flb-rail-gap,2rem)]">
      {/* min-w-0 is essential: without it a wide table in the main column pushes
          the rail out of the container instead of shrinking itself. */}
      <div className="min-w-0 grow">{children}</div>
      <div className="h-fit w-full shrink-0 xl:sticky xl:top-4 xl:w-[var(--flb-rail-width,23.75rem)]">
        {rail}
      </div>
    </div>
  );
}
