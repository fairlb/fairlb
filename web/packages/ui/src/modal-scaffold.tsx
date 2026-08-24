import { Button } from "./button";
import { Dialog } from "@cloudflare/kumo/components/dialog";
import { AlertDialog as AlertDialogPrimitive } from "@cloudflare/kumo/primitives/alert-dialog";
import { Dialog as DialogPrimitive } from "@cloudflare/kumo/primitives/dialog";
import { XIcon } from "@phosphor-icons/react";
import { useI18n } from "@fairlb/i18n";
import type { ComponentProps, ReactNode } from "react";
import { cn } from "./cn";
import { DialogTitle } from "./dialog-title";

export type ModalScaffoldProps = {
  open: boolean;
  onOpenChange: NonNullable<ComponentProps<typeof Dialog.Root>["onOpenChange"]>;
  disablePointerDismissal?: ComponentProps<typeof DialogPrimitive.Root>["disablePointerDismissal"];
  title: ReactNode;
  description?: ReactNode;
  size?: ComponentProps<typeof Dialog>["size"];
  role?: "dialog" | "alertdialog";
  children: ReactNode;
  footer?: ReactNode;
  headerActions?: ReactNode;
  className?: string;
  bodyClassName?: string;
  footerClassName?: string;
};

type ModalFrameProps = Omit<
  ModalScaffoldProps,
  "open" | "onOpenChange" | "disablePointerDismissal"
> & {
  panelClassName?: string;
};

/**
 * Four widths. Which one applies is decided by the widest incompressible thing
 * in the body, not by the number of fields: five single-line inputs fit in 384px,
 * while two fields where one is a textarea need 512px.
 *
 *   base  single-column form of single-line inputs, selects and switches; or a
 *         text-only confirmation
 *   lg    body contains a textarea, two columns of fields side by side, or two
 *         or more sections
 *   xl    body contains a table or a row-by-row list
 *
 * `sm` is retired. Its only user, ConfirmDialog, moved up to `base`, because a
 * destructive confirmation has to state its consequence and 288px spread one
 * such sentence over five lines. The size now has no call sites; the key remains
 * only because the `size` values come from the dialog component's union type and
 * removing it would introduce an `undefined` branch in the lookup below. Do not
 * use it for new dialogs.
 */
const sizeClasses = {
  base: "sm:w-[var(--flb-modal-width-base,24rem)]",
  sm: "sm:w-[var(--flb-modal-width-sm,18rem)]",
  lg: "sm:w-[var(--flb-modal-width-lg,32rem)]",
  xl: "sm:w-[var(--flb-modal-width-xl,48rem)]",
} as const;

function PrimitiveLayer({
  role,
  className,
  children,
}: {
  role: "dialog" | "alertdialog";
  className: string;
  children: ReactNode;
}) {
  const backdropClassName =
    "fixed inset-0 z-0 bg-kumo-recessed opacity-80 transition-all duration-150 motion-reduce:transition-none data-ending-style:opacity-0 data-starting-style:opacity-0";
  const portalClassName = "relative z-[var(--flb-z-overlay,80)]";
  if (role === "alertdialog") {
    return (
      <AlertDialogPrimitive.Portal className={portalClassName}>
        <AlertDialogPrimitive.Backdrop
          forceRender
          className={backdropClassName}
          data-slot="modal-backdrop"
        />
        <AlertDialogPrimitive.Popup className={className} data-slot="modal-panel">
          {children}
        </AlertDialogPrimitive.Popup>
      </AlertDialogPrimitive.Portal>
    );
  }
  return (
    <DialogPrimitive.Portal className={portalClassName}>
      <DialogPrimitive.Backdrop
        forceRender
        className={backdropClassName}
        data-slot="modal-backdrop"
      />
      <DialogPrimitive.Popup className={className} data-slot="modal-panel">
        {children}
      </DialogPrimitive.Popup>
    </DialogPrimitive.Portal>
  );
}

function ModalFrame({
  title,
  description,
  size = "base",
  role = "dialog",
  children,
  footer,
  headerActions,
  className,
  panelClassName,
  bodyClassName,
  footerClassName,
}: ModalFrameProps) {
  const panelClassNameResolved = cn(
    // The panel sets no z-index of its own; it covers the backdrop purely by
    // being positioned and coming after it in the DOM.
    //
    // It used to carry `z-10`. Popups opened from *inside* a dialog — selects,
    // comboboxes, tooltips — are portalled into this same overlay container and
    // have `z-index: auto`, so a panel at `z-10` sat permanently on top of them:
    // no dropdown inside a dialog could be opened at all, and a hit test at the
    // option's coordinates returned the panel rather than the option. Without
    // it, all three share stacking level 0 and DOM order decides: backdrop,
    // panel, then whatever opened last — which is exactly "last opened wins".
    "fixed top-1/2 left-1/2 w-full max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2",
    "grid max-h-[var(--flb-modal-max-block,88dvh)] grid-rows-[auto_minmax(0,1fr)_auto] overflow-hidden rounded-xl bg-kumo-base p-0 text-kumo-default shadow-xl ring ring-kumo-line",
    // Drop the header's bottom border when the body is empty. ConfirmDialog
    // passes a null body with no padding, so the body has zero height and the
    // header's bottom border meets the footer's top border exactly: two lines at
    // 10% opacity stack into a single 2px rule at roughly 19%, heavier and
    // darker than any other divider in the product, across every instance.
    //
    // The criterion is applied to the rendered result via CSS `:empty` rather
    // than to `children == null`, because the latter is true even for an empty
    // fragment — a fragment is always truthy — and that is exactly how the next
    // body-less dialog is likely to be written. `:empty` asks whether there are
    // really any child nodes.
    "[&:has([data-slot=modal-body]:empty)_[data-slot=modal-header]]:border-b-0",
    "transition-[scale,opacity] duration-150 motion-reduce:transition-none data-ending-style:scale-90 data-ending-style:opacity-0 data-starting-style:scale-90 data-starting-style:opacity-0",
    sizeClasses[size],
    panelClassName,
    className,
  );
  return (
    <PrimitiveLayer role={role} className={panelClassNameResolved}>
      <div
        className="flex min-w-0 items-start justify-between gap-4 border-b border-kumo-line px-6 py-4"
        data-slot="modal-header"
      >
        <div className="grid min-w-0 gap-1.5">
          <DialogTitle>{title}</DialogTitle>
          {description != null && (
            <Dialog.Description className="text-base text-kumo-subtle">
              {description}
            </Dialog.Description>
          )}
        </div>
        {headerActions != null && (
          <div className="flex shrink-0 items-center gap-1">{headerActions}</div>
        )}
      </div>
      <div
        className={cn("min-h-0 overflow-y-auto px-6 py-5", bodyClassName)}
        data-slot="modal-body"
      >
        {children}
      </div>
      {footer != null && (
        <div
          className={cn(
            "flex flex-wrap items-center justify-end gap-2 border-t border-kumo-line px-6 pt-4",
            "pb-[var(--flb-safe-bottom-modal,max(1rem,env(safe-area-inset-bottom)))]",
            footerClassName,
          )}
          data-slot="modal-footer"
        >
          {footer}
        </div>
      )}
    </PrimitiveLayer>
  );
}

/**
 * The standard dialog skeleton: the title and the actions stay put and only the
 * body scrolls. The underlying primitives keep responsibility for the focus
 * trap, Escape handling, making the background inert, and restoring focus on
 * close.
 */
export function ModalScaffold({
  open,
  onOpenChange,
  disablePointerDismissal,
  role = "dialog",
  ...props
}: ModalScaffoldProps) {
  return role === "alertdialog" ? (
    <Dialog.Root open={open} onOpenChange={onOpenChange} role="alertdialog">
      <ModalFrame {...props} role="alertdialog" />
    </Dialog.Root>
  ) : (
    <Dialog.Root
      open={open}
      onOpenChange={onOpenChange}
      disablePointerDismissal={disablePointerDismissal}
    >
      <ModalFrame {...props} role="dialog" />
    </Dialog.Root>
  );
}

export type DetailDrawerProps = Omit<
  ModalScaffoldProps,
  "size" | "role" | "headerActions" | "disablePointerDismissal"
> & {
  width?: "base" | "wide";
  headerActions?: ReactNode;
  closeLabel?: string;
};

/**
 * A detail drawer on the right. Whether the URL drives it is the caller's
 * decision; the close semantics and the safe-area inset are settled here.
 *
 * The width follows the same criterion as the dialog sizes above — the widest
 * incompressible thing in the body, not the number of fields. In practice that
 * means: `base` (32rem) for reading a record, `wide` (48rem) when the body is an
 * editable **form**, because a form row carries label, control and hint side by
 * side and that is what `FormColumn` sets 48rem for. A form squeezed into 32rem
 * is the same defect as a form squeezed by a navigation rail, arriving by a
 * different route.
 */
export function DetailDrawer({
  open,
  onOpenChange,
  width = "base",
  headerActions,
  closeLabel,
  ...props
}: DetailDrawerProps) {
  const { t } = useI18n();
  const label = closeLabel ?? t("dismiss");
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <ModalFrame
        {...props}
        size="xl"
        role="dialog"
        panelClassName={cn(
          "top-0 right-0 bottom-0 left-auto h-dvh max-h-dvh max-w-none translate-x-0 translate-y-0 rounded-none",
          "w-full sm:rounded-l-xl",
          width === "base"
            ? "sm:w-[var(--flb-drawer-width-base,32rem)]"
            : "sm:w-[var(--flb-drawer-width-wide,48rem)]",
        )}
        headerActions={
          <>
            {headerActions}
            <Dialog.Close
              render={(closeProps) => (
                <Button
                  {...closeProps}
                  variant="ghost"
                  size="sm"
                  shape="square"
                  icon={<XIcon />}
                  aria-label={label}
                  title={label}
                />
              )}
            />
          </>
        }
      />
    </Dialog.Root>
  );
}
