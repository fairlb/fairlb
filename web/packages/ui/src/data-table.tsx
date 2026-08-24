// ui-ignore no-kumo-table: this file is the single entry point the rule exists to
// funnel everything through; the ban is aimed at application code.
import { Table } from "@cloudflare/kumo/components/table";
import type { ComponentProps } from "react";
import { cn } from "./cn";

/**
 * DataTable is the only entry point for every table here.
 *
 * It settles two things that convention alone could not hold:
 *
 * 1. The horizontal scroll container. Three variants used to coexist: putting
 *    the overflow on the Card itself, which scrolled the card and carried its
 *    section heading sideways along with the table; nesting another div inside
 *    the Card; and a bare bordered box with no Card at all, whose corner radius
 *    did not even match. The container is built in here, so call sites only deal
 *    with the Card.
 * 2. The type scale comes from the underlying Table and nothing else. No font
 *    size is overridden here. Measured, this design system shifts the whole
 *    typographic scale down one step, so the base size at the table root is
 *    already the 14px the rule asks for. What call sites should do is stop
 *    writing a smaller size onto cells, rather than changing the root here: of
 *    193 cells, 25 had been set to 12px, and those 25 are the deviation.
 */
type DataTableAccessibleName =
  | { caption: string; ariaLabel?: never }
  | { caption?: never; ariaLabel: string };

export type DataTableProps = Omit<ComponentProps<typeof Table>, "aria-label"> &
  DataTableAccessibleName & {
    /** Classes for the horizontal scroll container. Do not move the overflow out
     * to the surrounding Card. */
    scrollClassName?: string;
  };

function DataTableRoot({
  className,
  scrollClassName,
  caption,
  ariaLabel,
  children,
  ...props
}: DataTableProps) {
  return (
    <div
      className={cn("overflow-x-auto overscroll-x-contain", scrollClassName)}
      data-slot="table-scroll"
    >
      <Table {...props} className={className} aria-label={caption == null ? ariaLabel : undefined}>
        {caption != null && <caption className="sr-only">{caption}</caption>}
        {children}
      </Table>
    </div>
  );
}

type StickyColumnProps = { sticky?: "start" | "end" };

function kumoSticky(sticky: StickyColumnProps["sticky"]): "left" | "right" | undefined {
  return sticky === "start" ? "left" : sticky === "end" ? "right" : undefined;
}

/** In a wide table, key columns stick to the start and action columns to the
 * end. Which columns count as key remains the caller's decision. */
function DataTableHead({
  sticky,
  ...props
}: Omit<ComponentProps<typeof Table.Head>, "sticky"> & StickyColumnProps) {
  return <Table.Head sticky={kumoSticky(sticky)} {...props} />;
}

function DataTableCell({
  sticky,
  className,
  ...props
}: Omit<ComponentProps<typeof Table.Cell>, "sticky"> & StickyColumnProps) {
  return (
    <Table.Cell
      sticky={kumoSticky(sticky)}
      className={cn(
        sticky &&
          "group-hover/interactive-row:bg-kumo-fill-hover group-hover/interactive-row:before:to-kumo-fill-hover",
        className,
      )}
      {...props}
    />
  );
}

/**
 * `interactive` gives a hover background to rows that actually lead somewhere.
 *
 * It is opt-in rather than global: audit, health and impact rows lead nowhere,
 * and colouring them too would demote discoverability to decoration.
 *
 * The background uses the token that is semantically named for hover. That token
 * does *not* distinguish hover from selected — measured in the browser in both
 * modes, the hover fill and the tint used by the selected variant are the same
 * value in dark mode, and differ by half a percent in light mode, which is
 * invisible. Selection is signalled by the checkbox, never by the background;
 * choosing the hover token here is a semantic alignment, so that the day the two
 * tokens diverge the distinction appears for free.
 *
 * Hover itself is plainly visible, because the card surface it sits on is far
 * from either value.
 *
 * No transition is applied: hover colour changes are required to be immediate.
 */
function DataTableRow({
  interactive,
  className,
  ...props
}: ComponentProps<typeof Table.Row> & { interactive?: boolean }) {
  return (
    <Table.Row
      className={cn(interactive && "group/interactive-row hover:bg-kumo-fill-hover", className)}
      {...props}
    />
  );
}

export const DataTable = Object.assign(DataTableRoot, {
  Header: Table.Header,
  Head: DataTableHead,
  Body: Table.Body,
  Row: DataTableRow,
  Cell: DataTableCell,
  Footer: Table.Footer,
  CheckHead: Table.CheckHead,
  CheckCell: Table.CheckCell,
  ResizeHandle: Table.ResizeHandle,
});
