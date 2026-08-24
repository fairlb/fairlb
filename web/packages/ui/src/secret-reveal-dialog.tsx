import { Button } from "./button";
import { Checkbox } from "@cloudflare/kumo/components/checkbox";
import { ClipboardText } from "@cloudflare/kumo/components/clipboard-text";
import { Dialog } from "@cloudflare/kumo/components/dialog";
import { useI18n } from "@fairlb/i18n";
import { useEffect, useId, useRef, useState, type ComponentProps, type ReactNode } from "react";
import { Alert } from "./form";
import { ModalScaffold } from "./modal-scaffold";

/**
 * SecretRevealDialog is the creation dialog for anything that yields a one-time
 * secret.
 *
 * It has two stages. Once the form stage submits successfully the caller feeds
 * in the secret and the dialog switches to the reveal stage. The secret appears
 * only this once, so the reveal stage can only be left through "Done": Escape,
 * an outside click and every other close path are cancelled, and "Done" requires
 * ticking the acknowledgement first.
 *
 * When the secret is non-null from the start, the dialog opens directly in the
 * reveal stage, which suits purely revealing flows such as resetting a
 * second-factor secret.
 *
 * `reviewable` means this secret can be seen again later — a webhook signing key,
 * for instance. All of the one-time etiquette above is wrong in that case: a
 * checkbox reading "it will not be shown again" is simply false, and blocking
 * Escape puts an obstacle in front of something that can be reopened at will.
 *
 * This came out of a walkthrough: a webhook change had just fixed, as a
 * top-priority defect, a sentence in the interface that was untrue for the
 * reader — and then said the very same thing again through this reused dialog.
 * Eight browser tests caught none of it, because they assert that the key
 * appears, not that the words around it are true.
 *
 * It defaults to false, since an API key really is shown only once and that path
 * is unaffected.
 *
 * Always mounted: the caller controls it with `open` rather than rendering it
 * conditionally.
 */
export function SecretRevealDialog({
  open,
  onOpenChange,
  title,
  description,
  size = "base",
  error,
  submitLabel,
  submitDisabled,
  pending,
  onSubmit,
  children,
  secret,
  secretTitle,
  secretHint,
  secretLabel,
  onDone,
  reviewable = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  size?: ComponentProps<typeof Dialog>["size"];
  error?: ReactNode;
  submitLabel?: ReactNode;
  submitDisabled?: boolean;
  pending?: boolean;
  onSubmit?: () => void;
  /** Fields for the form stage; omitted for reveal-only use. */
  children?: ReactNode;
  /** null selects the form stage; a non-null value selects the reveal stage and
   * is the secret itself. */
  secret: string | null;
  /** Title for the reveal stage; falls back to `title`. */
  secretTitle?: ReactNode;
  /** Explanatory text for the reveal stage — the "shown only once" line, worded
   * by the caller for its own case. */
  secretHint: ReactNode;
  /** Field name shown above the secret value, such as "API key". */
  secretLabel: ReactNode;
  /** The only way out: the caller closes the dialog, clears the secret and
   * refetches here. */
  onDone: () => void;
  /** true when this secret can be seen again later: no acknowledgement, and the
   * close paths are not blocked. See the component note above. */
  reviewable?: boolean;
}) {
  const { t } = useI18n();
  const [acked, setAcked] = useState(false);
  const formId = useId();

  // By the time the closing animation runs, the caller has already cleared the
  // secret, as `onDone` requires. Holding the last content stops the reveal
  // stage flashing back to the form stage, the same reason ConfirmDialog caches
  // its title. The close-blocking decision uses the live secret; rendering uses
  // the cached one.
  const shownSecret = useRef<string | null>(null);
  useEffect(() => {
    if (open) shownSecret.current = secret;
  }, [open, secret]);
  const displaySecret = open ? secret : shownSecret.current;
  const revealing = displaySecret != null;

  // Reset the acknowledgement on entering or leaving the reveal stage.
  useEffect(() => {
    if (!open || !revealing) setAcked(false);
  }, [open, revealing]);

  return (
    <ModalScaffold
      open={open}
      size={size}
      title={displaySecret != null ? (secretTitle ?? title) : title}
      description={displaySecret == null ? description : undefined}
      disablePointerDismissal={secret != null && !reviewable}
      onOpenChange={(next, details) => {
        if (!next && secret != null && !reviewable) {
          // The secret appears only once, so "Done" is the only way out of the
          // reveal stage.
          details.cancel();
          return;
        }
        onOpenChange(next);
      }}
      footer={
        displaySecret != null ? (
          <Button disabled={!reviewable && !acked} onClick={onDone}>
            {t("secretRevealDone")}
          </Button>
        ) : (
          <>
            <Dialog.Close
              render={(props) => (
                <Button {...props} variant="ghost">
                  {t("cancel")}
                </Button>
              )}
            />
            <Button form={formId} type="submit" loading={pending} disabled={submitDisabled}>
              {submitLabel}
            </Button>
          </>
        )
      }
    >
      {displaySecret != null ? (
        <div className="space-y-4">
          <Alert variant="success">{secretHint}</Alert>
          <div className="grid gap-1.5">
            <span className="text-base font-medium">{secretLabel}</span>
            <ClipboardText size="sm" text={displaySecret} />
          </div>
          {/* "I have saved it — it will not be shown again" may only appear when
              it really will not be shown again. */}
          {!reviewable && (
            <Checkbox
              checked={acked}
              onCheckedChange={(checked) => setAcked(checked === true)}
              label={t("secretRevealAck")}
            />
          )}
        </div>
      ) : (
        <form
          id={formId}
          className="space-y-4"
          onSubmit={(event) => {
            event.preventDefault();
            onSubmit?.();
          }}
        >
          {error != null && <Alert>{error}</Alert>}
          {children}
        </form>
      )}
    </ModalScaffold>
  );
}
