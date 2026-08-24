import { Button } from "./button";
import { Dialog } from "@cloudflare/kumo/components/dialog";
import { useI18n } from "@fairlb/i18n";
import { useId, type ComponentProps, type ReactNode } from "react";
import { Alert } from "./form";
import { ModalScaffold } from "./modal-scaffold";

/**
 * FormDialog is the one dialog shell for creating or editing a record.
 *
 * Each create dialog used to assemble the root, title, error alert and action
 * row by hand, copying the cancel and submit semantics from a neighbour. This
 * shell fixes the structure: title, description, error, fields, then cancel and
 * submit. What happens after success — navigate, or toast and refetch — stays
 * with the caller's mutation handler. A creation that yields a one-time secret
 * uses SecretRevealDialog instead.
 *
 * Always mounted: the caller controls it with `open` rather than rendering it
 * conditionally.
 */
export function FormDialog({
  open,
  onOpenChange,
  title,
  description,
  size = "base",
  error,
  submitLabel,
  submitVariant = "primary",
  submitDisabled,
  pending,
  preventCloseWhilePending = false,
  onSubmit,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  size?: ComponentProps<typeof Dialog>["size"];
  /** Error bar inside the dialog, usually the result of apiErrorMessage; shown
   * in place above the form. */
  error?: ReactNode;
  submitLabel: ReactNode;
  submitVariant?: "primary" | "destructive";
  submitDisabled?: boolean;
  pending?: boolean;
  /** Keep a security-sensitive request mounted until its success handoff has run. */
  preventCloseWhilePending?: boolean;
  onSubmit: () => void;
  children: ReactNode;
}) {
  const { t } = useI18n();
  const formId = useId();
  return (
    <ModalScaffold
      open={open}
      onOpenChange={(next) => {
        if (!next && pending && preventCloseWhilePending) return;
        onOpenChange(next);
      }}
      title={title}
      description={description}
      size={size}
      footer={
        <>
          <Dialog.Close
            render={(props) => (
              <Button {...props} variant="ghost" disabled={pending && preventCloseWhilePending}>
                {t("cancel")}
              </Button>
            )}
          />
          <Button
            form={formId}
            type="submit"
            variant={submitVariant}
            loading={pending}
            disabled={submitDisabled}
          >
            {submitLabel}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit();
        }}
      >
        {error != null && <Alert>{error}</Alert>}
        {children}
      </form>
    </ModalScaffold>
  );
}
