import type { ReactNode } from "react";

/**
 * InlineEmpty is the empty state *inside* a card: a table with no rows, a list
 * with nothing in it.
 *
 * It deliberately does not wrap Kumo's `Empty`, for two reasons, the second of
 * which is decisive:
 *
 *  1. that component brings its own bordered, filled container, while nearly
 *     every call site is already inside a Card, and nesting those two layers is
 *     explicitly disallowed;
 *  2. it hard-codes its title as a 24px semibold `<h2>`, and its `size` variant
 *     only controls padding and gap — so passing the small size never touched
 *     the title at all. That put an empty state's title at the same level as the
 *     page `h1` and half again larger than the section heading of the card
 *     containing it, inherited by 61 call sites. One page ended up with four
 *     24px headings, three of which said "there is nothing here". Only the outer
 *     container can be overridden, never the title, so this component emits its
 *     own markup.
 *
 * The slots have fixed jobs:
 *   - `title` is a short noun phrase answering "what would have been here" — no
 *     full stop, no subordinate clause;
 *   - `description` is a sentence answering "why is it empty" or "what to do
 *     next";
 *   - `children` is that next step as something clickable, for the empty states
 *     where the reason there is nothing is that nothing has been set up yet. It
 *     is optional because most empty states are not that: "no results for this
 *     filter" has no action, and inventing one would be worse than none.
 *
 * The title is a `<p>` rather than a heading: an empty state is the *content* of
 * the section it sits in, not a sibling section of it. As an `h2` it appeared in
 * the document outline alongside the real section heading, so a screen-reader
 * user heard two sibling sections where there was one.
 *
 * Page-level empty states do not come through here. A full-screen "forbidden" or
 * "not found" still uses Kumo's `Empty` directly, where 24px is correct because
 * it really is that screen's main heading. The criterion is "inside a card
 * versus whole page", not "small versus large".
 */
export function InlineEmpty({
  title,
  description,
  icon,
  children,
}: {
  title: string;
  description?: string;
  icon?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div
      className="flex w-full flex-col items-center gap-1.5 px-6 py-8 text-center"
      data-slot="inline-empty"
    >
      {icon && <div className="mb-1.5">{icon}</div>}
      <p className="text-base font-medium text-kumo-default">{title}</p>
      {description && <p className="max-w-140 text-base text-kumo-subtle">{description}</p>}
      {children && <div className="mt-1.5 flex flex-wrap justify-center gap-2">{children}</div>}
    </div>
  );
}
