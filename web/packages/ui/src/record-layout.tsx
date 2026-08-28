import type { ReactNode } from "react";
import { Card } from "./form";
import { cn } from "./cn";
import { PageRail } from "./page-rail";
import {
  LocalNav,
  PageContentsNav,
  type LocalNavItem,
  type PageContentsNavItem,
} from "./local-nav";

/**
 * The page layouts. What a page's header names picks between them:
 *
 *  - a **record** — `RecordPage`, aspects in the header strip, content full
 *    width;
 *  - an **area** — `SectionPage`, sibling destinations in a vertical rail;
 *  - **one kind of thing** — `ListPage`, filters and the list in one card.
 *
 * The criterion separating the first two is documented on `RecordNav`. A page
 * that is none of them — several independent sections under one title — keeps
 * the plain shell; see the note on `ListPage` for why it is not stretched to
 * cover that.
 *
 * The first two used to be the same hand-written grid, repeated in four files —
 * `grid min-w-0 gap-6 xl:grid-cols-[12rem_minmax(0,1fr)] xl:items-start` — with
 * the rail width stated there *and again* inside `LocalNav` as `xl:w-48`. Two
 * copies of one number in two files is a number that eventually disagrees with
 * itself, and the disagreement shows up as a rail that overflows its track.
 *
 * `ContentsLayout` at the bottom of this file is not a fourth archetype: it is a
 * page *body*, used inside a `RecordPage` and on its own, and it carries a table
 * of contents rather than navigation.
 *
 * None of them owns permission gating, the remount `key` on a context switch, or
 * the unsaved-changes blocker. Those read differently on every page that has
 * them, and folding them in would only produce a component whose body is a
 * parameter switch.
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

/**
 * A page body that carries an anchor table of contents beside it.
 *
 * This is the third place a vertical strip can appear next to content, and it
 * is the only one that is not navigation: every entry it lists is already on
 * this page. It therefore sits *after* the content in both DOM and visual
 * order, which is also why it is a body-level layout rather than a page-level
 * one — `RecordPage` bodies use it (a provider's settings aspect) and whole
 * pages use it (platform health), and a page-level component could not serve
 * the first.
 *
 * It replaces the same five-class string written out in five files. The gap is
 * wider than the section rail's because there is no rule beside the content
 * here — the contents nav draws its own left border, and at 1.5rem the two read
 * as one block.
 *
 * `contents` takes the items rather than a node, for the reason given on
 * `SectionPage`: a `ReactNode` slot here would accept a `LocalNav`, and "at most
 * one vertical navigation in a page's content area" would then be a rule stated
 * in a comment. `PageContentsNav` renders nothing for an empty array, so a page
 * whose sections are still loading degrades to a single column rather than to a
 * gap.
 */
export function ContentsLayout({
  contents,
  children,
  className,
}: {
  contents: readonly PageContentsNavItem[];
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex min-w-0 flex-col gap-6 2xl:flex-row 2xl:items-start 2xl:gap-10",
        className,
      )}
      data-slot="contents-layout"
    >
      <div className="min-w-0 flex-1">{children}</div>
      <PageContentsNav items={contents} />
    </div>
  );
}

/**
 * A list of one kind of thing: header above, a filter row and the list itself
 * inside a single card, and the dialogs the rows open after both.
 *
 * This is the third archetype, and it was the last to be written down. The other
 * two got a component because the same grid had been hand-written in four files;
 * this one had been hand-written in every list page there is, and had drifted
 * further than a grid can — the container was a `Card`, a bare `section`, or a
 * `section` wrapping a `Card`, so one operations list read as a table floating
 * on the page background while its neighbours sat on a card, and two more
 * carried a heading that repeated the page title one line above it.
 *
 * `filters` is a `ReactNode` while the two navigation slots take props, and that
 * is not an inconsistency: a filter row's track count differs on every page, so
 * there is no shape here for a type to pin. What can be stated mechanically —
 * that the filters sit inside the same card as the list, at the top — is what
 * this component owns.
 *
 * `overlays` exists because the dialogs and drawers a list opens are not list
 * content. They must be rendered unconditionally, so they are always present in
 * the tree, and putting them in `children` would place them inside the card and
 * inside the list's own vertical rhythm. Naming the slot puts them after it.
 *
 * Not every page with a table is a list page. A page that shows several
 * independent sections — a dashboard with charts and a breakdown, an
 * organization overview — is not one kind of thing with filters over it, and
 * forcing it through here would only produce a card that has to be argued out
 * of. Those keep the plain shell.
 */
export function ListPage({
  header,
  filters,
  rail,
  overlays,
  children,
  className,
}: {
  header: ReactNode;
  /** The filter or search row; it becomes the first thing inside the card. */
  filters?: ReactNode;
  /** Supporting summary beside the list from `xl` up; see `PageRail`. */
  rail?: ReactNode;
  /** Dialogs and drawers the rows open. Rendered after the card, never inside
   * it, and never conditionally — see the note above. */
  overlays?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const body = (
    <Card className="space-y-3">
      {filters}
      {children}
    </Card>
  );
  return (
    <div className={cn("space-y-6", className)} data-slot="list-page">
      {header}
      {rail ? <PageRail rail={rail}>{body}</PageRail> : body}
      {overlays}
    </div>
  );
}
