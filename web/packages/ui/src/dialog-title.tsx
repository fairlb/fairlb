import { Dialog } from "@cloudflare/kumo/components/dialog";
import { Text } from "@cloudflare/kumo/components/text";
import type { ReactNode } from "react";

/**
 * DialogTitle is the heading of a dialog.
 *
 * Seven call sites had hand-copied the utility classes that the design system's
 * heading variant already produces — the same situation section headings were in
 * before they became a component: visually already a heading, but not going
 * through one, so it could be neither unified nor evolved. The lint rule that
 * guards this only matches `<h1..6 className=`, so it cannot see a dialog title.
 *
 * The text component wraps *inside* the dialog title rather than replacing it
 * through a render prop, for two measured reasons:
 *  1. the dialog title applies no classes of its own and passes straight through
 *     to the underlying `<h2>`; that element has to be emitted by the dialog for
 *     it to carry the id `aria-labelledby` points at;
 *  2. the text component does not accept `className` — anything passed lands in
 *     rest props and overrides the variant classes it computed for itself. Going
 *     through a render prop feeds it exactly that merged className.
 *
 * For the same reason the variant class names are not imported and assembled by
 * hand: that constant exists only in the type declarations and is tree-shaken
 * out of the runtime build.
 */
export function DialogTitle({ children }: { children: ReactNode }) {
  return (
    <Dialog.Title>
      <Text variant="heading3" as="span">
        {children}
      </Text>
    </Dialog.Title>
  );
}
