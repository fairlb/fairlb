import {
  useContext,
  useId,
  useLayoutEffect,
  useRef,
  isValidElement,
  type ComponentProps,
  type ComponentPropsWithoutRef,
  type AriaAttributes,
  type LabelHTMLAttributes,
  type ReactNode,
} from "react";
import { Banner } from "@cloudflare/kumo/components/banner";
import { Combobox as KumoCombobox } from "@cloudflare/kumo/components/combobox";
import { Input as KumoInput, Textarea as KumoTextarea } from "@cloudflare/kumo/components/input";
import { Select as KumoSelect } from "@cloudflare/kumo/components/select";
import { useI18n } from "@fairlb/i18n";
import { cn } from "./cn";
import { FieldContext, FormRowItemContext } from "./form-context";

/**
 * Field passes its own label id down to the control inside it.
 *
 * The underlying Input decides whether it has an accessible name purely from its
 * props, and cannot see the `<label for>` Field renders outside it. Without this
 * context, the control inside every Field would emit an accessibility warning in
 * development — even though the association genuinely holds. It also upgrades
 * that association from `for` alone to `for` plus `aria-labelledby`, so a screen
 * reader can follow either path.
 */
function useFieldLabelId(explicit: {
  "aria-label"?: string;
  "aria-labelledby"?: string;
}): string | undefined {
  const field = useContext(FieldContext);
  if (explicit["aria-labelledby"]) return explicit["aria-labelledby"];
  if (explicit["aria-label"]) return undefined;
  return field?.labelId;
}

/**
 * Input is a thin wrapper over the design system's Input.
 *
 * What preceded it was a hand-rolled `<input>` with a string of utility classes
 * — not filling a gap in the design system but rebuilding something it already
 * had, and had built correctly, using a ring rather than a border so that edges
 * stay crisp under a shadow. Keeping the name and passing className through
 * meant no call site had to change.
 *
 * The font size is deliberately not overridden: the base size in this design
 * system's scale is 14px, which is exactly the content baseline used here.
 */
export function Input({ className, ...props }: ComponentProps<typeof KumoInput>) {
  const field = useContext(FieldContext);
  const labelledBy = useFieldLabelId(props);
  const describedBy = props["aria-describedby"] ?? field?.describedBy;
  return (
    <KumoInput
      className={cn("w-full", className)}
      {...props}
      id={props.id ?? field?.controlId}
      aria-labelledby={labelledBy}
      aria-describedby={describedBy}
      aria-invalid={props["aria-invalid"] ?? field?.invalid}
    />
  );
}

export function Textarea({ className, ...props }: ComponentProps<typeof KumoTextarea>) {
  const field = useContext(FieldContext);
  const labelledBy = useFieldLabelId(props);
  return (
    <KumoTextarea
      className={cn("min-h-20 w-full", className)}
      {...props}
      id={props.id ?? field?.controlId}
      aria-labelledby={labelledBy}
      aria-describedby={props["aria-describedby"] ?? field?.describedBy}
      aria-invalid={props["aria-invalid"] ?? field?.invalid}
    />
  );
}

type SelectProps<T, Multiple extends boolean | undefined = false> = Parameters<
  typeof KumoSelect<T, Multiple>
>[0] &
  Pick<AriaAttributes, "aria-label" | "aria-labelledby" | "aria-describedby" | "aria-invalid">;

/** Accessibility adapter for Select: used on its own it keeps the design
 * system's own label API, and placed inside a Field it shares that Field's
 * accessibility contract. */
function SelectRoot<T, Multiple extends boolean | undefined = false>({
  className,
  ...props
}: SelectProps<T, Multiple>) {
  const field = useContext(FieldContext);
  const containerRef = useRef<HTMLDivElement>(null);
  const labelledBy = useFieldLabelId(props);
  const describedBy = props["aria-describedby"] ?? field?.describedBy;
  const invalid = props["aria-invalid"] ?? field?.invalid;

  // The Select forwards aria-label and aria-labelledby to its trigger, but every
  // other aria attribute stops at the Root, which renders no DOM of its own. A
  // display:contents container locates the real trigger so the rest can be
  // applied before the browser commits layout. The control id has to land on the
  // real button too, so that clicking the Field's `<label for>` keeps the
  // browser's native focus and activation behaviour.
  // This accessibility adapter can be deleted once the component exposes
  // trigger props for id, aria-describedby and aria-invalid.
  useLayoutEffect(() => {
    const trigger = containerRef.current?.querySelector<HTMLElement>(
      "[data-kumo-component=Select][data-kumo-part=trigger]",
    );
    if (!trigger) return;
    if (field?.controlId) trigger.id = field.controlId;
    if (describedBy) trigger.setAttribute("aria-describedby", describedBy);
    else trigger.removeAttribute("aria-describedby");
    if (invalid !== undefined) trigger.setAttribute("aria-invalid", String(invalid));
    else trigger.removeAttribute("aria-invalid");
  }, [describedBy, field?.controlId, invalid]);

  const accessibleProps = {
    ...props,
    className: cn(field && "w-full", className),
    "aria-labelledby": labelledBy,
    "aria-describedby": describedBy,
    "aria-invalid": invalid,
  };
  return (
    <div ref={containerRef} className="contents">
      <KumoSelect<T, Multiple> {...accessibleProps} />
    </div>
  );
}

export const Select = Object.assign(SelectRoot, {
  Option: KumoSelect.Option,
  Group: KumoSelect.Group,
  GroupLabel: KumoSelect.GroupLabel,
  Separator: KumoSelect.Separator,
});

/**
 * Combobox is the adapter for a searchable selector.
 *
 * How to choose: when the candidates are a short, ordered enumeration — a
 * protocol, a visibility, a role — use `Select` above. When the candidates are a
 * catalogue that grows with the business — models, providers, organizations — use this
 * one. Most existing Select call sites are the former; do not convert them by
 * imitation.
 *
 * Unlike Select, this needs no layout-effect accessibility adapter. Select's trigger is
 * rendered internally and is out of the caller's reach, so its aria attributes
 * have to be applied by query before layout is committed. Combobox is a compound
 * component whose trigger input the caller renders directly; its rest props
 * reach the underlying input, which merges caller props last so they win, and
 * the id is picked up explicitly. Ordinary prop flow is enough.
 *
 * The wrapper does only the four things the component cannot do itself:
 *  1. join the Field accessibility contract — the description id is generated
 *     inside Field and is not readable from outside, and Field's own control-id
 *     inference reads the child's `id`, which fails for a compound component
 *     whose child is a Root rather than an input;
 *  2. replace the hard-coded English defaults for the clear and show-options
 *     labels;
 *  3. replace the hard-coded English default for the empty state;
 *  4. undo the fixed maximum width on the trigger input's wrapper div, since a
 *     control inside a Field should fill its column. Class merging puts the
 *     caller's classes last, so overriding is enough.
 *
 * Inside a Field, do not also pass the component's own `label` prop: that wraps
 * another Field around it, and two label layers both contributing
 * `aria-labelledby` make a screen reader read the name twice.
 */
type ComboboxRootProps<Value, Multiple extends boolean | undefined = false> = Parameters<
  typeof KumoCombobox<Value, Multiple>
>[0];

/** The Root itself needs no change, but the library's exported object is not
 * mutated in place with Object.assign either — that would modify the library. */
function ComboboxRoot<Value, Multiple extends boolean | undefined = false>(
  props: ComboboxRootProps<Value, Multiple>,
) {
  return <KumoCombobox<Value, Multiple> {...props} />;
}

function ComboboxTriggerInput({
  className,
  ...props
}: ComponentProps<typeof KumoCombobox.TriggerInput>) {
  const { t } = useI18n();
  const field = useContext(FieldContext);
  const labelledBy = useFieldLabelId(props);
  return (
    <KumoCombobox.TriggerInput
      clearLabel={t("comboboxClear")}
      showOptionsLabel={t("comboboxShowOptions")}
      {...props}
      // This className lands on the wrapper div — the inner input gets its own,
      // computed separately, which overrides anything passed here. Its only
      // purpose is to lift the fixed maximum width.
      className={cn("max-w-none", className)}
      id={props.id ?? field?.controlId}
      aria-labelledby={labelledBy}
      aria-describedby={props["aria-describedby"] ?? field?.describedBy}
      aria-invalid={props["aria-invalid"] ?? field?.invalid}
    />
  );
}

/** Falls back to the localised message; a caller's own children win. */
function ComboboxEmpty({ children, ...props }: ComponentProps<typeof KumoCombobox.Empty>) {
  const { t } = useI18n();
  return <KumoCombobox.Empty {...props}>{children ?? t("comboboxNoMatch")}</KumoCombobox.Empty>;
}

export const Combobox = Object.assign(ComboboxRoot, {
  TriggerInput: ComboboxTriggerInput,
  Content: KumoCombobox.Content,
  List: KumoCombobox.List,
  Item: KumoCombobox.Item,
  Empty: ComboboxEmpty,
});

export function Label({ className, ...props }: LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn("text-base font-medium text-kumo-default", className)} {...props} />;
}

/** Field is the minimal combination of a label, a control and a message. */
export function Field({
  label,
  htmlFor,
  error,
  hint,
  required,
  className,
  children,
}: {
  label: Exclude<ReactNode, boolean | null | undefined>;
  htmlFor?: string;
  error?: ReactNode;
  hint?: ReactNode;
  /** Render the required marker. Being required used to be entirely invisible
   * until the browser's own bubble appeared on submit. */
  required?: boolean;
  className?: string;
  children: ReactNode;
}) {
  const { t } = useI18n();
  const inFormRow = useContext(FormRowItemContext);
  const generated = useId();
  const childControlId =
    isValidElement<{ id?: string }>(children) && typeof children.props.id === "string"
      ? children.props.id
      : undefined;
  // An explicit htmlFor wins; otherwise the child control's own id is reused,
  // and only then is a stable one generated. Controls inside the Field take that
  // id from context, so the visible label can natively focus an input, a
  // textarea, or a Select that exposes no trigger props — while aria-label can
  // still override the accessible name independently.
  const controlId = htmlFor ?? childControlId ?? `${generated}-control`;
  const labelId = `${controlId}-label`;
  const descriptionId = error ? `${generated}-error` : hint ? `${generated}-hint` : undefined;
  const isRequired =
    required ??
    (isValidElement<{ required?: boolean }>(children) && Boolean(children.props.required));
  return (
    <div className={cn("grid gap-2", inFormRow && "sm:contents", className)} data-slot="field">
      <Label
        id={labelId}
        htmlFor={controlId}
        data-slot="field-label"
        className={cn(inFormRow && "sm:row-start-1")}
      >
        {label}
        {isRequired && (
          <span className="ml-0.5 text-kumo-danger" title={t("required")} aria-hidden>
            *
          </span>
        )}
      </Label>
      <div className={cn("min-w-0", inFormRow && "sm:row-start-2")} data-slot="field-control">
        <FieldContext.Provider
          value={{ controlId, labelId, describedBy: descriptionId, invalid: Boolean(error) }}
        >
          {children}
        </FieldContext.Provider>
      </div>
      {/* In this design system's scale the base size is 14px, not 16px — which
          is the body size the rule asks for. */}
      {hint && !error && (
        <p
          id={descriptionId}
          className={cn("text-base text-kumo-subtle", inFormRow && "sm:row-start-3")}
          data-slot="field-message"
        >
          {hint}
        </p>
      )}
      {error && (
        <p
          id={descriptionId}
          className={cn("text-base text-kumo-danger", inFormRow && "sm:row-start-3")}
          data-slot="field-message"
        >
          {error}
        </p>
      )}
    </div>
  );
}

/**
 * Card is the page's card container.
 *
 * A ring rather than a border: combined with a shadow, a border makes the edge
 * look blurred. The vertical padding is one step smaller than the horizontal
 * because line height already contributes visual space above and below.
 */
export type CardTone = "default" | "warning" | "danger";

export type CardProps = ComponentPropsWithoutRef<"div"> & {
  /** The semantic edge colour is only for a persistent warning or danger area.
   * Body colour stays at the default, so the whole card does not drop to low
   * contrast. */
  tone?: CardTone;
};

export function Card({ className, children, tone = "default", ...props }: CardProps) {
  return (
    <div
      className={cn(
        "rounded-xl bg-kumo-base px-[var(--flb-card-pad-inline,1.5rem)] py-[var(--flb-card-pad-block,1.25rem)] shadow-xs ring",
        tone === "default" && "ring-kumo-line",
        tone === "warning" && "bg-kumo-warning-tint/20 ring-kumo-warning/45",
        tone === "danger" && "bg-kumo-danger-tint/20 ring-kumo-danger/45",
        className,
      )}
      data-tone={tone}
      {...props}
    >
      {children}
    </div>
  );
}

/** Alert renders an error, success or informational bar. */
/**
 * Alert is the in-page message bar, now carried by the design system's Banner.
 *
 * Banner offers four variants and none of them is success, while several places
 * here need one — a password change confirmed, recovery codes, a newly created
 * key shown in the clear. Success therefore uses the neutral variant with a
 * success-token skin, written exactly the way Banner writes its own error
 * variant and only swapping the token family. The result shares its visual
 * origin with the other variants rather than being a parallel system.
 *
 * Keeping the Alert name and its variant API meant none of the sixty-odd call
 * sites had to change.
 */
export function Alert({
  variant = "error",
  children,
}: {
  variant?: "error" | "success" | "info" | "warning";
  children: ReactNode;
}) {
  if (variant === "success") {
    return (
      <Banner
        role="status"
        aria-live="polite"
        className="bg-kumo-success-tint/60 text-kumo-success"
      >
        {children}
      </Banner>
    );
  }
  // Warning works the same way as success: the variant does not exist, so the
  // token family is swapped rather than a parallel system being built.
  // It answers the case neither error nor info can: not a failure of this page,
  // but something that genuinely needs attention — an unconfigured exchange rate
  // has this shape, where billing in that currency is failing right now while
  // this page itself is fine.
  // It uses status rather than alert because it describes a standing condition,
  // not an event that has just occurred.
  if (variant === "warning") {
    return (
      <Banner
        role="status"
        aria-live="polite"
        className="bg-kumo-warning-tint/60 text-kumo-warning"
      >
        {children}
      </Banner>
    );
  }
  return (
    <Banner
      variant={variant === "info" ? "default" : "error"}
      role={variant === "error" ? "alert" : "status"}
      aria-live={variant === "error" ? "assertive" : "polite"}
    >
      {children}
    </Banner>
  );
}
