import { Checkbox as KumoCheckbox } from "@cloudflare/kumo/components/checkbox";
import { useI18n } from "@fairlb/i18n";
import {
  createElement,
  useContext,
  useId,
  useRef,
  useState,
  type AriaAttributes,
  type ComponentProps,
  type ReactElement,
  type ReactNode,
} from "react";
import { cn } from "./cn";
import { FormRowItemContext } from "./form-context";

type KumoCheckboxProps = ComponentProps<typeof KumoCheckbox>;
type AccessibleCheckboxLabel = string | ReactElement;

function hasUsableAccessibleName(props: CheckboxProps): boolean {
  const ariaLabel = props["aria-label"];
  const labelledBy = props["aria-labelledby"];
  if (typeof ariaLabel === "string" && ariaLabel.trim() !== "") return true;
  if (typeof labelledBy === "string" && labelledBy.trim() !== "") return true;
  if (typeof props.label === "string") return props.label.trim() !== "";
  return props.label != null;
}

/**
 * A single checkbox must carry its own accessible name. Wrapping it in a native
 * `<label>` establishes no labelable-control relationship with the rendered
 * `button[role=checkbox]`, so it can have visible text and still have no name.
 */
export type CheckboxProps = Omit<KumoCheckboxProps, "label" | "aria-label" | "aria-labelledby"> &
  (
    | { label: AccessibleCheckboxLabel; "aria-label"?: string; "aria-labelledby"?: string }
    | { label?: never; "aria-label": string; "aria-labelledby"?: string }
    | { label?: never; "aria-label"?: never; "aria-labelledby": string }
  );

export function Checkbox(props: CheckboxProps) {
  const dev = (import.meta as ImportMeta & { env?: { DEV?: boolean } }).env?.DEV;
  if (dev && !hasUsableAccessibleName(props)) {
    throw new Error("Checkbox requires a non-empty label, aria-label, or aria-labelledby");
  }
  return <KumoCheckbox {...props} />;
}

export type CheckboxGroupOption = {
  value: string;
  label: AccessibleCheckboxLabel;
  description?: ReactNode;
  disabled?: boolean;
  className?: string;
};

export type CheckboxGroupFieldProps = {
  legend: AccessibleCheckboxLabel;
  options: CheckboxGroupOption[];
  value?: string[];
  defaultValue?: string[];
  onValueChange?: (value: string[]) => void;
  hint?: ReactNode;
  error?: string;
  required?: boolean;
  disabled?: boolean;
  name?: string;
  columns?: 1 | 2;
  className?: string;
};

/**
 * The one field shape for a group of related checkboxes. An explicit
 * fieldset/legend gives sets such as protocols, endpoints or events a shared
 * accessible name.
 */
export function CheckboxGroupField({
  legend,
  options,
  value,
  defaultValue,
  onValueChange,
  hint,
  error,
  required,
  disabled,
  name,
  columns = 1,
  className,
}: CheckboxGroupFieldProps) {
  const { t } = useI18n();
  const inFormRow = useContext(FormRowItemContext);
  const dev = (import.meta as ImportMeta & { env?: { DEV?: boolean } }).env?.DEV;
  if (dev && typeof legend === "string" && legend.trim() === "") {
    throw new Error("CheckboxGroupField requires a non-empty legend");
  }
  const generated = useId();
  const fieldsetRef = useRef<HTMLFieldSetElement>(null);
  const [internalValue, setInternalValue] = useState(defaultValue ?? []);
  const selected = value ?? internalValue;
  const enabledValues = new Set(
    options.filter((option) => !option.disabled).map((option) => option.value),
  );
  const submitted = selected.filter((item) => enabledValues.has(item));
  const messageId = hint != null || error ? `${generated}-message` : undefined;
  const setSelected = (next: string[]) => {
    if (value === undefined) setInternalValue(next);
    onValueChange?.(next);
  };
  const choices = (
    <div className={cn("grid gap-2", columns === 2 && "sm:grid-cols-2")}>
      {options.map((option, index) => {
        const descriptionId =
          option.description != null ? `${generated}-option-${index}-description` : undefined;
        const describedBy = [descriptionId, messageId].filter(Boolean).join(" ") || undefined;
        return (
          <div key={option.value} className="grid gap-0.5">
            <Checkbox
              checked={selected.includes(option.value)}
              onCheckedChange={(checked) =>
                setSelected(
                  checked === true
                    ? [...selected, option.value]
                    : selected.filter((item) => item !== option.value),
                )
              }
              label={option.label}
              disabled={disabled || option.disabled}
              className={option.className}
              controlFirst
              {...({
                "aria-describedby": describedBy,
                "aria-invalid": Boolean(error) || undefined,
              } as AriaAttributes)}
            />
            {option.description != null && (
              <div id={descriptionId} className="pl-6 text-base text-kumo-subtle">
                {option.description}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
  // A hidden multiple select acts as the group's form proxy: it serialises the
  // selected values and makes `required` mean "choose at least one". A native
  // checkbox's own `required` forces one particular option and cannot express
  // that group-level constraint.
  const formProxy =
    name || required ? (
      <select
        multiple
        required={required}
        disabled={disabled}
        name={name}
        value={submitted}
        onChange={() => undefined}
        onInvalid={(event) => {
          event.preventDefault();
          fieldsetRef.current
            ?.querySelector<HTMLElement>(
              '[role="checkbox"]:not([disabled]):not([aria-disabled="true"])',
            )
            ?.focus();
        }}
        tabIndex={-1}
        aria-hidden="true"
        className="sr-only"
      >
        {/* Not the visual Select: these nodes exist only to proxy `required` and
            native FormData. Building them with createElement also keeps this
            invisible implementation detail from being mistaken for
            hand-written options in application code. */}
        {options.map((option) =>
          createElement(
            "option",
            { key: option.value, value: option.value, disabled: option.disabled },
            option.value,
          ),
        )}
      </select>
    ) : null;
  const message =
    error || hint != null ? (
      <p
        id={messageId}
        className={cn("text-base", error ? "text-kumo-danger" : "text-kumo-subtle")}
      >
        {error ?? hint}
      </p>
    ) : null;

  if (inFormRow) {
    return (
      <>
        <div className="text-base font-medium text-kumo-default sm:row-start-1" aria-hidden>
          {legend}
          {required && (
            <span className="ml-0.5 text-kumo-danger" title={t("required")}>
              *
            </span>
          )}
        </div>
        {/* The legend keeps the fieldset's native group name; the visible copy
            goes into the FormRow label track. */}
        <fieldset
          ref={fieldsetRef}
          disabled={disabled}
          aria-describedby={messageId}
          aria-invalid={Boolean(error)}
          className={cn("min-w-0 sm:row-start-2", className)}
          data-slot="checkbox-group-field"
        >
          <legend className="sr-only">
            {legend}
            {required && ` (${t("required")})`}
          </legend>
          {choices}
          {formProxy}
        </fieldset>
        {message && <div className="sm:row-start-3">{message}</div>}
      </>
    );
  }

  return (
    <fieldset
      ref={fieldsetRef}
      disabled={disabled}
      aria-describedby={messageId}
      aria-invalid={Boolean(error)}
      className={cn("grid gap-2", className)}
      data-slot="checkbox-group-field"
    >
      <legend className="text-base font-medium text-kumo-default">
        {legend}
        {required && (
          <>
            <span className="ml-0.5 text-kumo-danger" title={t("required")} aria-hidden>
              *
            </span>
            <span className="sr-only"> ({t("required")})</span>
          </>
        )}
      </legend>
      {choices}
      {formProxy}
      {message}
    </fieldset>
  );
}
