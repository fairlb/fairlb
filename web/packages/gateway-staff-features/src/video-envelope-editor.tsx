import { gatewayStaffApi, type GatewayStaffTypes } from "@fairlb/api-client";
import { useI18n, type MessageKey } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Checkbox,
  Field,
  FormRow,
  Input,
  SectionHeading,
  Select,
  StatusBadge,
} from "@fairlb/ui";
import { useState } from "react";

/**
 * The video capability envelope of one route.
 *
 * # Why this is a form and not a text area
 *
 * The transport profile a few sections above is edited as raw JSON, and that is
 * right for it: which keys it may hold is the server's judgement, and a second
 * copy of those rules in the browser would drift towards accepting things the
 * server refuses.
 *
 * This is the opposite case. The envelope is what admission checks a request
 * against *before any money is held*, its shape is closed and stated in the
 * contract, and every field means something specific enough to label. A text
 * area here would turn a typo into a route that quietly serves nothing, found
 * only when a customer's request is refused.
 *
 * # Why the lists are free text and not menus
 *
 * Durations, resolutions and aspect ratios have no shared vocabulary across
 * vendors — 4/6/8, 5/10 and 4/8/12 are three real ones, and one vendor has no
 * resolution field at all. A menu here would be this build's guess at another
 * company's catalogue, out of date the first time they add a tier. So the axes
 * are typed in, and what is offered instead is the vendor's own recorded
 * defaults, one button away.
 *
 * # Why there is no "mark as observed"
 *
 * `source` separates what a vendor's capability endpoint answered from what a
 * person typed, and the rule is that the interface must never show the second
 * as the first. Nothing in this build observes an envelope, so everything saved
 * here is `declared` and says so. The distinction is displayed rather than
 * offered, which is the honest half of it.
 */
export function VideoEnvelopeEditor({
  route,
  value,
  onChange,
  disabled,
  idPrefix,
}: {
  route: GatewayStaffTypes.GatewayRoute;
  value: GatewayStaffTypes.VideoEnvelope;
  onChange: (next: GatewayStaffTypes.VideoEnvelope) => void;
  disabled?: boolean;
  idPrefix: string;
}) {
  const { t } = useI18n();
  const [prefillError, setPrefillError] = useState<string | null>(null);
  const patch = (next: Partial<GatewayStaffTypes.VideoEnvelope>) => onChange({ ...value, ...next });

  // The vendor's recorded defaults, fetched only when asked for. It is a
  // prefill and nothing more: these numbers were read out of a vendor's
  // published contract when this build's mapper was written, so they come back
  // marked `declared` and stay that way. From the moment somebody saves them,
  // the person who saved them is the one answering for the configuration.
  const prefill = gatewayStaffApi.useGetGatewayVendorVideoEnvelope(
    route.provider_slug,
    { upstream_model: route.provider_model_id },
    { query: { enabled: false, retry: false } },
  );

  return (
    <div className="space-y-3 border-t border-kumo-line pt-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <SectionHeading level="sub" as="h4">
          {t("gwEnvelopeTitle")}
        </SectionHeading>
        <div className="flex items-center gap-2">
          {/* Always `declared` today, and shown anyway: an operator reading a
              configuration has to be able to tell a checked fact from a typed
              one, and a badge that only ever appears once it can say both is a
              badge nobody trusts when it does. */}
          <StatusBadge tone="neutral">
            {t(value.source === "observed" ? "gwEnvelopeObserved" : "gwEnvelopeDeclared")}
          </StatusBadge>
          <Button
            variant="secondary"
            size="sm"
            disabled={disabled || prefill.isFetching}
            onClick={() => {
              setPrefillError(null);
              void prefill.refetch().then((r) => {
                if (r.data) {
                  // Replaces the whole envelope rather than merging: a merge
                  // would leave a duration this vendor does not accept sitting
                  // beside the ones it does, and the operator would have no way
                  // to tell which came from where.
                  onChange(r.data.envelope);
                  return;
                }
                setPrefillError(t("gwEnvelopePrefillFailed"));
              });
            }}
          >
            {t("gwEnvelopePrefill")}
          </Button>
        </div>
      </div>
      <p className="text-base text-kumo-subtle">{t("gwEnvelopeHint")}</p>
      {prefillError && <Alert>{prefillError}</Alert>}

      <FormRow className="sm:grid-cols-3">
        <FormRow.Item>
          <TokenListField
            id={`${idPrefix}-durations`}
            label={t("gwEnvelopeDurations")}
            hint={t("gwEnvelopeDurationsHint")}
            values={(value.durations_seconds ?? []).map(String)}
            onChange={(next) =>
              patch({
                durations_seconds: next.map(Number).filter((n) => Number.isFinite(n) && n > 0),
              })
            }
            disabled={disabled}
            numeric
          />
        </FormRow.Item>
        <FormRow.Item>
          <TokenListField
            id={`${idPrefix}-resolutions`}
            label={t("gwEnvelopeResolutions")}
            hint={t("gwEnvelopeResolutionsHint")}
            values={value.resolutions ?? []}
            onChange={(next) => patch({ resolutions: next })}
            disabled={disabled}
          />
        </FormRow.Item>
        <FormRow.Item>
          <TokenListField
            id={`${idPrefix}-aspects`}
            label={t("gwEnvelopeAspectRatios")}
            hint={t("gwEnvelopeAspectRatiosHint")}
            values={value.aspect_ratios ?? []}
            onChange={(next) => patch({ aspect_ratios: next })}
            disabled={disabled}
          />
        </FormRow.Item>
      </FormRow>

      <FormRow className="sm:grid-cols-2">
        <FormRow.Item>
          <Field label={t("gwEnvelopeAudio")} htmlFor={`${idPrefix}-audio`}>
            <Select
              id={`${idPrefix}-audio`}
              value={value.audio ?? "never"}
              disabled={disabled}
              onValueChange={(v) => patch({ audio: v as GatewayStaffTypes.VideoEnvelopeAudio })}
              items={AUDIO_CHOICES.map((v) => ({ value: v, label: t(AUDIO_LABELS[v]) }))}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field
            label={t("gwEnvelopeCancel")}
            htmlFor={`${idPrefix}-cancel`}
            hint={t("gwEnvelopeCancelHint")}
          >
            <Select
              id={`${idPrefix}-cancel`}
              value={value.cancel ?? "never"}
              disabled={disabled}
              onValueChange={(v) => patch({ cancel: v as GatewayStaffTypes.VideoEnvelopeCancel })}
              items={CANCEL_CHOICES.map((v) => ({ value: v, label: t(CANCEL_LABELS[v]) }))}
            />
          </Field>
        </FormRow.Item>
      </FormRow>

      <FormRow className="sm:grid-cols-4">
        <FormRow.Item>
          <CountField
            id={`${idPrefix}-maxn`}
            label={t("gwEnvelopeMaxN")}
            value={value.max_n}
            onChange={(n) => patch({ max_n: n })}
            disabled={disabled}
          />
        </FormRow.Item>
        <FormRow.Item>
          <CountField
            id={`${idPrefix}-maxref`}
            label={t("gwEnvelopeMaxRefImages")}
            hint={t("gwEnvelopeMaxRefImagesHint")}
            value={value.max_reference_images}
            onChange={(n) => patch({ max_reference_images: n })}
            disabled={disabled}
          />
        </FormRow.Item>
        <FormRow.Item>
          <CountField
            id={`${idPrefix}-maxchars`}
            label={t("gwEnvelopeMaxPromptChars")}
            hint={t("gwEnvelopeMaxPromptCharsHint")}
            value={value.max_prompt_chars}
            onChange={(n) => patch({ max_prompt_chars: n })}
            disabled={disabled}
          />
        </FormRow.Item>
        <FormRow.Item>
          <CountField
            id={`${idPrefix}-maxjob`}
            label={t("gwEnvelopeMaxJobSeconds")}
            hint={t("gwEnvelopeMaxJobSecondsHint")}
            value={value.max_job_seconds}
            onChange={(n) => patch({ max_job_seconds: n })}
            disabled={disabled}
          />
        </FormRow.Item>
      </FormRow>

      <div className="flex flex-wrap gap-x-6 gap-y-2">
        <Checkbox
          label={t("gwEnvelopeImageToVideo")}
          checked={value.supports_image_to_video === true}
          disabled={disabled}
          onCheckedChange={(v) => patch({ supports_image_to_video: v === true })}
        />
        <Checkbox
          label={t("gwEnvelopeLastFrame")}
          checked={value.supports_last_frame === true}
          disabled={disabled}
          onCheckedChange={(v) => patch({ supports_last_frame: v === true })}
        />
        <Checkbox
          label={t("gwEnvelopeNegativePrompt")}
          checked={value.supports_negative_prompt === true}
          disabled={disabled}
          onCheckedChange={(v) => patch({ supports_negative_prompt: v === true })}
        />
      </div>
    </div>
  );
}

const AUDIO_CHOICES = ["never", "optional", "always"] as const;
const AUDIO_LABELS: Record<(typeof AUDIO_CHOICES)[number], MessageKey> = {
  never: "gwEnvelopeAudioNever",
  optional: "gwEnvelopeAudioOptional",
  always: "gwEnvelopeAudioAlways",
};

const CANCEL_CHOICES = ["never", "queued_only", "anytime"] as const;
const CANCEL_LABELS: Record<(typeof CANCEL_CHOICES)[number], MessageKey> = {
  never: "gwEnvelopeCancelNever",
  queued_only: "gwEnvelopeCancelQueuedOnly",
  anytime: "gwEnvelopeCancelAnytime",
};

/**
 * A count where zero and absent mean the same thing: "no ceiling stated". The
 * field is left empty for both rather than showing a 0 an operator would read
 * as "none allowed".
 */
function CountField({
  id,
  label,
  hint,
  value,
  onChange,
  disabled,
}: {
  id: string;
  label: string;
  hint?: string;
  value: number | undefined;
  onChange: (next: number) => void;
  disabled?: boolean;
}) {
  return (
    <Field label={label} htmlFor={id} hint={hint}>
      <Input
        id={id}
        inputMode="numeric"
        value={value ? String(value) : ""}
        disabled={disabled}
        onChange={(e) => {
          const n = Number(e.target.value.trim());
          onChange(Number.isFinite(n) && n > 0 ? Math.floor(n) : 0);
        }}
      />
    </Field>
  );
}

/**
 * A list of short values, entered one at a time and removed by clicking.
 *
 * Enter and comma both commit, because a reader typing "4, 6, 8" expects all
 * three and typing "4" then Enter expects one. Duplicates are dropped silently:
 * the list is a set, and a repeated entry is a slip rather than an instruction.
 */
function TokenListField({
  id,
  label,
  hint,
  values,
  onChange,
  disabled,
  numeric,
}: {
  id: string;
  label: string;
  hint?: string;
  values: readonly string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
  numeric?: boolean;
}) {
  const { t } = useI18n();
  const [draft, setDraft] = useState("");
  const commit = (raw: string) => {
    const added = raw
      .split(",")
      .map((v) => v.trim())
      .filter((v) => v !== "" && !values.includes(v));
    if (added.length > 0) onChange([...values, ...added]);
    setDraft("");
  };
  return (
    <Field label={label} htmlFor={id} hint={hint}>
      <div className="space-y-2">
        <Input
          id={id}
          value={draft}
          disabled={disabled}
          inputMode={numeric ? "numeric" : undefined}
          onChange={(e) => {
            // A comma commits as it is typed, so pasting a whole list works
            // without the reader having to press anything.
            if (e.target.value.includes(",")) commit(e.target.value);
            else setDraft(e.target.value);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              commit(draft);
            }
          }}
          onBlur={() => draft.trim() !== "" && commit(draft)}
        />
        {values.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {values.map((v) => (
              <button
                key={v}
                type="button"
                disabled={disabled}
                className="inline-flex items-center gap-1 rounded border border-kumo-line px-2 py-0.5 text-base"
                aria-label={t("gwEnvelopeRemoveValue", { value: v })}
                onClick={() => onChange(values.filter((x) => x !== v))}
              >
                <span className="tabular-nums">{v}</span>
                <span aria-hidden>×</span>
              </button>
            ))}
          </div>
        )}
      </div>
    </Field>
  );
}
