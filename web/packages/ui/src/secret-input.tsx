import type { ComponentProps } from "react";
import { Button } from "./button";
import { Input } from "./form";

/**
 * SecretInput is the write-only control for a stored credential.
 *
 * The value never comes back from the server: the control shows whether one is
 * stored (`hint`, the head…tail mask computed at write time) and takes a
 * replacement. Three states are distinguishable to the reader and to the
 * submitter:
 *
 *   - nothing typed, a hint present → unchanged, the stored secret stays;
 *   - something typed → replace;
 *   - "Clear" pressed → remove; the field shows the clear as pending until
 *     saved, so an accidental press is visible and reversible.
 *
 * `autoComplete="off"` rather than the credential contract: this is an API
 * key for a third party, not the reader's own login, and a password manager
 * offering to save it as one is the wrong affordance.
 */
export function SecretInput({
  value,
  onValueChange,
  hint,
  clearing = false,
  onClear,
  clearLabel,
  undoLabel,
  placeholder,
  ...props
}: Omit<ComponentProps<typeof Input>, "type" | "value" | "onChange"> & {
  value: string;
  onValueChange(value: string): void;
  /** Display mask of the stored secret; undefined when nothing is stored. */
  hint?: string;
  /** Whether a clear is pending; the input is disabled and the mask struck through. */
  clearing?: boolean;
  /** Toggle the pending clear. Omitted when the secret cannot be cleared. */
  onClear?: () => void;
  clearLabel?: string;
  undoLabel?: string;
  placeholder?: string;
}) {
  return (
    <div className="flex min-w-0 items-center gap-2">
      <Input
        // ui-ignore: a pass-through wrapper. The accessible name arrives from the
        // call site through {...props}, whose type keeps id and aria-label.
        {...props}
        type="password"
        autoComplete="off"
        value={value}
        disabled={clearing || props.disabled}
        placeholder={clearing ? undefined : (placeholder ?? hint)}
        onChange={(event) => onValueChange(event.currentTarget.value)}
      />
      {hint && onClear && (
        <Button type="button" variant="secondary" size="sm" onClick={onClear}>
          {clearing ? undoLabel : clearLabel}
        </Button>
      )}
    </div>
  );
}
