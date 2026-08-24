import type { ReactNode } from "react";
import { Field, Input } from "./form";
import { mainToNano, normalizeAmountInput } from "./money";

/**
 * NanoPriceField takes a unit price in main units and stores it in nano.
 *
 * Storage is an int64 nano value, but there is no reason to make a person type
 * `3000000000`. Every other money input here already accepts main units and
 * converts, and the public models endpoint has always emitted decimal strings —
 * the pricing form was the sole exception. This component closes that gap.
 *
 * Two deliberate choices:
 *
 *  1. The raw nano value is echoed under the field. Anyone reconciling against
 *     the API or the database sees the nano field there, and this small line is
 *     the bridge between the interface and that number. Without it the
 *     conversion becomes a black box nobody can verify.
 *  2. Normalisation — stripping the currency symbol and thousands separators —
 *     happens on blur, not on every keystroke. Rewriting the input while
 *     someone is typing moves their cursor.
 */
export function NanoPriceField({
  label,
  id,
  value,
  onChange,
  hint,
  error,
  disabled,
}: {
  label: string;
  id: string;
  /** A main-unit string. It may contain a currency symbol and thousands
   * separators, which are normalised on blur. */
  value: string;
  onChange: (next: string) => void;
  hint?: ReactNode;
  error?: string;
  disabled?: boolean;
}) {
  const normalized = normalizeAmountInput(value);
  // Do not echo nano for invalid input: showing NaN, or a 0 conjured from
  // nowhere, is worse than showing nothing — it looks like a real value when no
  // storable value exists at this moment.
  const valid = normalized !== "" && Number.isFinite(Number(normalized));
  const nano = valid ? mainToNano(normalized) : null;
  // What is shown here is the raw nano value: a machine number for people to
  // check against, not a localised display value. The grouping separator is
  // fixed rather than following the interface language, because otherwise it
  // would no longer line up with the integer in the API.
  const nanoText = nano === null ? "" : nano.toLocaleString("en-US"); // ui-ignore no-raw-date-format: machine number, see above

  return (
    <Field
      label={label}
      htmlFor={id}
      error={error}
      hint={
        <>
          {hint}
          {hint && nano !== null && " · "}
          {nano !== null && <span className="font-mono">= {nanoText} nano</span>}
        </>
      }
    >
      <Input
        id={id}
        value={value}
        inputMode="decimal"
        disabled={disabled}
        className="text-right font-mono"
        onChange={(e) => onChange(e.target.value)}
        onBlur={() => normalized !== value && onChange(normalized)}
      />
    </Field>
  );
}
