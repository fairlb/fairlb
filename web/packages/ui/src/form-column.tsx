import type { ReactNode } from "react";
import { cn } from "./cn";

/**
 * FormColumn holds a column of form fields to a readable line length.
 *
 * Narrowness is a property of the content, not of the page. The shell's
 * container has a single width, and content that needs a short line narrows
 * itself rather than making the shell narrow on its behalf. The alternative —
 * giving the container two widths chosen by the shape of the page — was
 * rejected, at the cost of two measured problems: the content column jumping
 * sideways by about 125px between pages on a wide screen, and, more
 * fundamentally, being unable to express a page that holds both a wide table and
 * a narrow form.
 *
 * Use it on forms only. Tables, row lists and card grids should not be wrapped:
 * they are meant to fill the width, and wrapping them recreates the two-width
 * scheme one page at a time.
 *
 * The width is 768px rather than a prose measure. A prose measure is the line
 * length for continuous text, around 65 characters, whereas a form row carries
 * three tracks — label, control, hint — and needs 768px for those to sit side by
 * side on a desktop without crowding. Explanatory text keeps the same width.
 *
 * Left-aligned, not centred: the page header, tables and the side rail all start
 * at the left edge of the content area, and a centred form would sit out of line
 * with them, reading as though it belonged to a different container.
 */
export function FormColumn({ children, className }: { children: ReactNode; className?: string }) {
  return (
    // The slot is how the geometry regression finds this column on a composed
    // page; the width it is supposed to have is only checkable if the element
    // holding it can be selected without knowing the page.
    <div data-slot="form-column" className={cn("w-full max-w-3xl", className)}>
      {children}
    </div>
  );
}
