import { useI18n } from "@fairlb/i18n";
import { Field, FormRow, Input, Select } from "@fairlb/ui";
import type { ReactNode } from "react";
import { type AdjustmentMode, BASE_BPS } from "./cost-adjustment";

/**
 * The shared shape of "list price / discount / markup, a percentage, and the
 * resulting multiplier", used by the create-provider dialog and by a provider's
 * connection settings.
 *
 * Echoing the multiplier back is not decoration: a percentage is typed, a multiplier
 * is stored, and a conversion sits between them. Putting the result beside the input
 * makes a mistyped one visible on the spot.
 */
export function AdjustmentFields({
  id,
  mode,
  onModeChange,
  percent,
  onPercentChange,
  bps,
  valid,
  actions,
  className,
}: {
  id: string;
  mode: AdjustmentMode;
  onModeChange: (mode: AdjustmentMode) => void;
  percent: string;
  onPercentChange: (percent: string) => void;
  bps: number;
  valid: boolean;
  /** Appended to the right of the multiplier echo — a save button on a detail page,
   * nothing inside a create dialog. */
  actions?: ReactNode;
  className?: string;
}) {
  const { t } = useI18n();
  return (
    <FormRow
      className={
        className ?? "sm:grid-cols-2 lg:grid-cols-[minmax(10rem,1fr)_minmax(10rem,1fr)_auto]"
      }
    >
      <FormRow.Item>
        <Field label={t("gwPriceAdjustment")}>
          <Select
            value={mode}
            onValueChange={(value) => onModeChange((value as AdjustmentMode) ?? "original")}
            items={[
              { value: "original", label: t("gwAdjustmentOriginal") },
              { value: "discount", label: t("gwAdjustmentDiscount") },
              { value: "markup", label: t("gwAdjustmentMarkup") },
            ]}
          />
        </Field>
      </FormRow.Item>
      <FormRow.Item>
        <Field
          label={t("gwAdjustmentPercent")}
          htmlFor={id}
          error={valid ? undefined : t("gwAdjustmentInvalid")}
        >
          <div className="relative">
            <Input
              id={id}
              value={mode === "original" ? "0" : percent}
              disabled={mode === "original"}
              inputMode="decimal"
              onChange={(event) => onPercentChange(event.target.value)}
            />
            <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-kumo-subtle">
              %
            </span>
          </div>
        </Field>
      </FormRow.Item>
      <FormRow.Actions className="flex-nowrap">
        <span className="font-mono text-[0.9em]">
          {valid ? `× ${(bps / BASE_BPS).toFixed(4)}` : "—"}
        </span>
        {actions}
      </FormRow.Actions>
    </FormRow>
  );
}
