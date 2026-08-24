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
 * SettingsSection is one block of a settings sub-page: a heading and description
 * outside the card, then the content.
 *
 * It moved up into the shared package rather than into the host contract because
 * it is pure typography with no coupling to the shell, and pages in the shared
 * feature packages need it too.
 */
export function SettingsSection({
  title,
  description,
  children,
}: {
  title: ReactNode;
  description?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="grid gap-3">
      <div className="grid gap-1.5">
        {title}
        {description && <p className="text-base text-kumo-subtle">{description}</p>}
      </div>
      {children}
    </section>
  );
}
