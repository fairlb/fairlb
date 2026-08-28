import { Text } from "@cloudflare/kumo/components/text";
import type { ReactNode } from "react";

/**
 * SectionHeading is the heading of a card or a section.
 *
 * Four hand-written variants had accumulated across the two applications, and
 * the most common of them resolved to exactly the classes the design system's
 * heading variant already produces — the component's implementation copied out
 * by hand, which could then be neither unified nor evolved.
 *
 * The level convention: the page header emits h1, a top-level card heading is h2
 * (the default), and a nested section inside a card is h3. Never skip a level
 * relative to the nearest ancestor heading.
 *
 * The lint rule cannot check this. `as` changes only the document outline and
 * nothing visual — within one `level` the size is fixed — so a skipped level is
 * completely invisible on screen; one page ended up with h1 followed by three
 * h3s and then an h2. The rule can only check whether this component was used at
 * all. Not adding a regex rule for the level is deliberate: a rule that cannot be
 * stated precisely gets ignored everywhere, which teaches people to ignore the
 * guard. So the convention is written here instead — follow it when editing.
 *
 * `level` is the size axis and `as` is the outline axis; keeping them separate
 * is deliberate. `sub` exists because a dialog title and this component used to
 * resolve to byte-identical classes, which left "what this dialog is" and "one
 * section of it" typographically indistinguishable. The only thing separating
 * them was the header's bottom border — and that scrolls out of view as soon as
 * the body moves. Inside a dialog, always pass `level="sub"`, which gives three
 * legible steps: title, section, then field label.
 *
 * The scaffold deliberately does not demote body headings automatically: styling
 * that comes from an invisible ancestor cannot be found by grep when something
 * needs changing, which is the same fault as pushing layout width into the
 * shell.
 */
export function SectionHeading({
  children,
  as,
  level = "section",
}: {
  children: ReactNode;
  /**
   * Position in the document outline; use `h3` for a nested section. Styling
   * does not follow it. Defaults from `level`: `section` gives h2, `sub` gives
   * h3.
   */
  as?: "h2" | "h3" | "h4";
  /**
   * The size step. `section` is 16px/600, for page and card sections; `sub` is
   * 14px/600, used only for sections inside a dialog.
   *
   * Measured: the design system's heading3 is 16px, not 18px — it shifts the
   * whole typographic scale down one step. Two earlier comments in this file
   * read the class names as standard Tailwind and recorded 18px.
   */
  level?: "section" | "sub";
}) {
  const tag = as ?? (level === "sub" ? "h3" : "h2");
  // The text component has no 14px/600 step — its bold option only reaches
  // medium weight — so `sub` uses the body variant at base size and adds the
  // weight through the escape-hatch class prop, which is merged last and
  // therefore reliably wins over the variant classes. A plain `className` would
  // not work: this component only honours the escape hatch, and an ordinary
  // className lands in rest props.
  return level === "sub" ? (
    <Text variant="body" size="base" as={tag} DANGEROUS_className="font-semibold">
      {children}
    </Text>
  ) : (
    <Text variant="heading3" as={tag}>
      {children}
    </Text>
  );
}

/**
 * SettingsSection is one block of a settings sub-page: a heading, a description
 * and the section's own action outside the card, then the content.
 *
 * It moved up into the shared package rather than into the host contract because
 * it is pure typography with no coupling to the shell, and pages in the shared
 * feature packages need it too.
 *
 * `title` is a string and the heading is emitted here, rather than the caller
 * passing a `SectionHeading` node. Every call site passed exactly that node, and
 * the one section that needed an action beside its heading had to hand-write a
 * `flex … justify-between` *inside* the title slot to get it — which is the
 * caller reconstructing this component's own layout because the component
 * refused to have an opinion about it. The action now has a slot, and the
 * heading row's geometry belongs here, where it is the same on every page.
 *
 * `actions` is the section's own control — "Add", "Enable", "Rotate". It is not
 * a place for the page's actions; those belong in `PageHeader.actions`. On a
 * narrow viewport it wraps below the description rather than squeezing it.
 *
 * `title` is optional, and the case it is optional for is narrow: a sub-page
 * whose single section *is* the page. Its area rail has already named it one
 * column to the left, so a heading here would repeat that name verbatim one line
 * below it — which is the defect ADR-0150 §三.2 names. The section still needs
 * its description and its action, so it stays a section and loses only the
 * heading. A page with more than one section gives every one of them a title.
 */
export function SettingsSection({
  title,
  as,
  description,
  actions,
  children,
}: {
  /** Omitted only when this section is the whole sub-page; see above. */
  title?: string;
  /** Position in the document outline; see `SectionHeading`. */
  as?: "h2" | "h3" | "h4";
  description?: ReactNode;
  /** The section's own control, at the right of the heading row. */
  actions?: ReactNode;
  children: ReactNode;
}) {
  // With nothing to put in it the heading row is not rendered at all: an empty
  // flex box still takes the section's row gap, which reads as a stray blank
  // band above the content.
  const heading = title || description || actions;
  return (
    <section className="grid gap-3">
      {heading && (
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="grid min-w-0 gap-1.5">
            {title && <SectionHeading as={as}>{title}</SectionHeading>}
            {description && <p className="text-base text-kumo-subtle">{description}</p>}
          </div>
          {actions && (
            <div
              data-slot="settings-section-actions"
              className="flex shrink-0 flex-wrap items-center gap-2"
            >
              {actions}
            </div>
          )}
        </div>
      )}
      {children}
    </section>
  );
}
