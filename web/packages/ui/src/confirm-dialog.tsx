import { Button } from "./button";
import { Dialog } from "@cloudflare/kumo/components/dialog";
import { useI18n } from "@fairlb/i18n";
import { useEffect, useRef, type ReactNode } from "react";
import { ModalScaffold } from "./modal-scaffold";

/**
 * ConfirmDialog is the one confirmation for destructive actions.
 *
 * Irreversible actions used to take three different shapes: fire on a single
 * click (six places, all using the lowest-emphasis button), go through the
 * browser's native confirm, or grow an inline confirmation inside the card.
 *
 * One instance per section: the caller drives `open` from a piece of state
 * holding the pending target, rather than rendering a dialog per row — the
 * latter mounts N dialogs in a long list.
 *
 * It is not rendered conditionally. Conditional rendering swallows the open and
 * close animations, so the component stays in the tree and is controlled by
 * `open`. The cost is that the title and description would blink as the target
 * clears during the closing animation, so the last non-empty content is cached
 * internally.
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  onConfirm,
  pending,
  destructive = true,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  /** Label for the confirm button; defaults to the generic "confirm". */
  confirmLabel?: string;
  onConfirm: () => void;
  pending?: boolean;
  /** Defaults to true, since destructive actions are why this component exists;
   * pass false for a confirmation that merely changes configuration. */
  destructive?: boolean;
}) {
  const { t } = useI18n();

  // The target is already cleared while the closing animation runs; keeping the
  // last content stops the text vanishing before the dialog fades.
  const shown = useRef<{ title: ReactNode; description?: ReactNode }>({ title, description });
  useEffect(() => {
    if (open) shown.current = { title, description };
  }, [open, title, description]);
  const body = open ? { title, description } : shown.current;

  return (
    <ModalScaffold
      open={open}
      onOpenChange={onOpenChange}
      role={destructive ? "alertdialog" : "dialog"}
      // Widened from 288px to 384px. A destructive confirmation has to state the
      // consequence, and at 288px a sentence like "deleting a plan that still
      // has members silently changes what those organizations may call" spills over
      // five lines — the container could not hold the very text the rule
      // requires. The narrow size was retired along with it.
      size="base"
      title={body.title}
      description={body.description}
      bodyClassName="p-0"
      footer={
        <>
          {/* Cancel is the way out; it should not compete with confirm for attention. */}
          <Dialog.Close
            render={(props) => (
              <Button {...props} variant="ghost" size="sm" autoFocus={destructive}>
                {t("cancel")}
              </Button>
            )}
          />
          <Button
            variant={destructive ? "destructive" : "primary"}
            size="sm"
            loading={pending}
            onClick={onConfirm}
          >
            {confirmLabel ?? t("confirm")}
          </Button>
        </>
      }
    >
      {null}
    </ModalScaffold>
  );
}
