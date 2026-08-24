import type { ComponentProps } from "react";
import { Input } from "./form";

type ControlledInputProps = Omit<ComponentProps<typeof Input>, "type" | "value" | "onChange"> & {
  value: string;
  onValueChange(value: string): void;
};

/** Controlled password input with the browser credential contract kept in one place. */
export function CredentialInput({
  value,
  onValueChange,
  autoComplete,
  ...props
}: ControlledInputProps) {
  return (
    <Input
      // ui-ignore: a pass-through wrapper. The accessible name arrives from the
      // call site through {...props}, whose type keeps id and aria-label; the
      // rule reads this file alone and cannot see that.
      {...props}
      type="password"
      value={value}
      autoComplete={autoComplete ?? "current-password"}
      onChange={(event) => onValueChange(event.currentTarget.value)}
    />
  );
}

/** Controlled OTP input that opts into password-manager and mobile-keyboard support. */
export function OneTimeCodeInput({ value, onValueChange, ...props }: ControlledInputProps) {
  return (
    <Input
      // ui-ignore: a pass-through wrapper, as above.
      {...props}
      type="text"
      value={value}
      inputMode="numeric"
      autoComplete="one-time-code"
      onChange={(event) => onValueChange(event.currentTarget.value)}
    />
  );
}
