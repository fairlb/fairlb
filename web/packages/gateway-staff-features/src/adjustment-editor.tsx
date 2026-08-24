import { useI18n } from "@fairlb/i18n";
import { Field, FormRow, Input, Select } from "@fairlb/ui";
import { adjustmentBps, type AdjustmentMode } from "./pricing-math";

/**
 * The input surface for a selling multiplier, shared by the pricing page of a model's
 * detail page and by the create-model dialog.
 *
 * It is shared because the same quantity is collected in both places. With its valid
 * range and its two-decimal rule written down in only one of them, the other is
 * guaranteed to diverge.
 *
 * It renders two `FormRow.Item`s rather than a self-contained row: the two callers
 * lay their rows out with different column counts, so how these two cells sit is
 * theirs to decide.
 */
export function AdjustmentEditor({
  mode,
  percent,
  onChange,
  inputId,
}: {
  mode: AdjustmentMode;
  percent: string;
  onChange: (mode: AdjustmentMode, percent: string) => void;
  inputId?: string;
}) {
  const { t } = useI18n();
  const bps = adjustmentBps(mode, percent);
  return (
    <>
      <FormRow.Item>
        <Field label={t("gwPriceAdjustment")}>
          <Select
            value={mode}
            onValueChange={(value) => onChange((value as AdjustmentMode) ?? "original", percent)}
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
          htmlFor={inputId}
          error={bps == null ? t("gwAdjustmentInvalid") : undefined}
        >
          <div className="relative">
            <Input
              id={inputId}
              aria-label={t("gwAdjustmentPercent")}
              disabled={mode === "original"}
              inputMode="decimal"
              value={mode === "original" ? "0" : percent}
              onChange={(event) => onChange(mode, event.target.value)}
            />
            <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-kumo-subtle">
              %
            </span>
          </div>
        </Field>
      </FormRow.Item>
    </>
  );
}
