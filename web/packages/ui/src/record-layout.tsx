import type { ReactNode } from "react";
import { cn } from "./cn";
import { LocalNav, type LocalNavItem } from "./local-nav";

/**
 * The two page layouts a navigable parent can have.
 *
 * Both used to be the same hand-written grid, repeated in four files —
 * `grid min-w-0 gap-6 xl:grid-cols-[12rem_minmax(0,1fr)] xl:items-start` — with
 * the rail width stated there *and again* inside `LocalNav` as `xl:w-48`. Two
 * copies of one number in two files is a number that eventually disagrees with
 * itself, and the disagreement shows up as a rail that overflows its track.
 *
 * Which one a page uses is decided by the criterion documented on `RecordNav`:
 * a header that names a **record** takes `RecordPage`, a header that names an
 * **area** takes `SectionPage`.
 *
 * Neither owns permission gating, the remount `key` on a context switch, or the
 * unsaved-changes blocker. Those read differently on every page that has them,
 * and folding them in would only produce a component whose body is a parameter
 * switch.
 */

/**
 * A record and its aspects: header (with its `RecordNav`) above, content at full
 * width below.
 *
 * There is no rail, which is the point — the aspects live in the header strip,
 * so the record's own content keeps the whole content column. Measured on the
 * provider settings page, a long form reaches its full 768px from 1280px up;
 * with a section rail beside it, the same form was 561px at 1280 and 658px at
 * 1366, and only reached 768px at 1536.
 */
export function RecordPage({
  header,
  children,
  className,
}: {
  header: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-6", className)} data-slot="record-page">
      {header}
      <div className="min-w-0">{children}</div>
    </div>
  );
}

/**
 * An area and its sibling destinations: header above, a vertical rail beside the
 * content from `xl` up, and below `xl` the rail collapses into the disclosure
 * `LocalNav` renders for itself.
 *
 * The rail is described by its items rather than passed in as a node. Which
 * items are visible is still the caller's answer — that is a permission
 * question, and the array arrives already filtered — but *which component* fills
 * this slot is not: a `ReactNode` here accepts a `RecordNav`, and "a record's
 * aspects go in the header, an area's destinations go in the rail" is then a
 * rule stated in a comment and hoped for. Narrowing the type hands it to the
 * compiler instead, which is the same stance the page header takes about its own
 * description element.
 */
export function SectionPage({
  header,
  navValue,
  navItems,
  navLabel,
  children,
  className,
}: {
  header: ReactNode;
  /** The item the current URL is on; `resolveNavValue` answers this. */
  navValue: string;
  /** Already filtered to what this reader may reach. */
  navItems: readonly LocalNavItem[];
  /** Only needed when a page carries more than one navigation. */
  navLabel?: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-6", className)} data-slot="section-page">
      {header}
      <div
        className={cn(
          "grid min-w-0 items-start",
          "gap-[var(--flb-section-nav-gap,1.5rem)]",
          "xl:grid-cols-[var(--flb-section-nav-width,12rem)_minmax(0,1fr)]",
        )}
      >
        <LocalNav value={navValue} items={navItems} ariaLabel={navLabel} />
        <div className="min-w-0">{children}</div>
      </div>
    </div>
  );
}
