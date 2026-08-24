import type { ReactNode } from "react";
import { cn } from "./cn";

/**
 * RowActions is the container for the group of action buttons at the end of a
 * table row.
 *
 * Why this has to be a component rather than a written rule: the button's root
 * classes make it a *block-level* flex container, so two buttons in one cell
 * each take a line and stack vertically. None of the three class names one
 * habitually reaches for can prevent that:
 *
 * - `whitespace-nowrap` applies to inline content and has no effect on a block
 *   box;
 * - `text-right` does nothing to a block box sized to its content — it stays on
 *   the left;
 * - `space-x-2` sets a left margin, which when stacked becomes an indent rather
 *   than horizontal spacing.
 *
 * Writing all three looks like defence in depth while being entirely dead. It is
 * a small example of a criterion parting company with the thing it judges: the
 * classes govern inline layout, and what they are aimed at is block-level.
 * (By contrast the badge component is inline-flex, so badges do sit side by
 * side — the two read identically at a glance and behave differently, which
 * makes this harder to catch by inspection.)
 *
 * Putting the buttons inside a flex container is what makes them flex items,
 * laid out in a row, and what makes `gap` take effect. Use this for every
 * end-of-row action group instead of hand-writing classes on the cell.
 */
export function RowActions({
  children,
  className,
  align = "end",
}: {
  children: ReactNode;
  className?: string;
  /** Defaults to the right, which is where end-of-row actions normally sit;
   * `start` is for a left-aligned action column. */
  align?: "start" | "end";
}) {
  return (
    <div
      className={cn(
        "flex flex-wrap items-center gap-2",
        align === "end" ? "justify-end" : "justify-start",
        className,
      )}
    >
      {children}
    </div>
  );
}
