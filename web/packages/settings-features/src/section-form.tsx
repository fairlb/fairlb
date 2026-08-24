import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { isMessageKey, useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Field,
  FormActions,
  FormDialog,
  Input,
  SecretInput,
  SectionHeading,
  Select,
  StatusBadge,
  Textarea,
  formatNano,
} from "@fairlb/ui";
import { useState, type FormEvent, type ReactNode } from "react";
import { useSettingsHost, type SettingEntry } from "./host";
import {
  encodeSecret,
  encodeValue,
  isDirty,
  needsReason,
  toDraft,
  type EncodeError,
} from "./setting-value";

/**
 * 设置表单的机械部分（ADR-0198）：一个 section 的键是一张表单、一次保存、一次原因。
 * 系统参数页与各集成页共用它——集成页只是「哪些 section、配上什么状态读数」的不同。
 */
/**
 * 一个分区里的键按 **section** 成形（ADR-0198）：同一集成的键（一个支付渠道的凭据与
 * 产品、一个登录方式的 client id 与 secret）是一张表单、一次保存、一个事务；没有 section
 * 的键各自成一张单行表单。判据来自服务端的 `section` 字段，前端不再按键名前缀猜。
 */
export type Form = { section: string | null; entries: SettingEntry[] };

export function formsOf(entries: readonly SettingEntry[]): Form[] {
  const forms: Form[] = [];
  const bySection = new Map<string, Form>();
  for (const entry of entries) {
    if (!entry.section) {
      forms.push({ section: null, entries: [entry] });
      continue;
    }
    let form = bySection.get(entry.section);
    if (!form) {
      form = { section: entry.section, entries: [] };
      bySection.set(entry.section, form);
      forms.push(form);
    }
    form.entries.push(entry);
  }
  return forms;
}

/** 词条键由服务端交出（ADR-0068）；词典里没有时回落到服务端的中文说明，再不济给一句话。 */
export function useDescription(entry: SettingEntry): string {
  const { t } = useI18n();
  if (entry.description_key && isMessageKey(entry.description_key)) return t(entry.description_key);
  return entry.description || t("settingsNoDescription");
}

export function KeyLabel({ entry }: { entry: SettingEntry }) {
  const { t } = useI18n();
  return (
    <span className="flex flex-wrap items-center gap-2">
      <span className="font-mono text-[0.9em]">{entry.key}</span>
      {entry.impact === "high" && (
        <StatusBadge tone="warning">{t("settingsImpactHigh")}</StatusBadge>
      )}
      {!entry.set && <StatusBadge tone="neutral">{t("settingsDefaultBadge")}</StatusBadge>}
    </span>
  );
}

/** 出处行：只报「某人某时改成了这个值」；未设置由行上的 Default 徽章表达，这里不说第二遍。 */
function Provenance({ entry }: { entry: SettingEntry }) {
  const { t, formatDateTime } = useI18n();
  if (!entry.set) return null;
  if (!entry.updated_at) return null;
  return (
    <span>
      {t("settingsChangedBy", {
        who: entry.updated_by || "—",
        when: formatDateTime(entry.updated_at),
      })}
    </span>
  );
}

type Change = { key: string; value: unknown };

/**
 * 一张表单 = 一个 section 的全部键（或一个独立键）。
 *
 * 提交只带**改过的**键：服务端在「已存 + 本次」合并后的值上跑 section 规则，
 * 所以没改的键不必重发；但规则要看整个 section，界面上就得把整个 section 摆在一起，
 * 否则「全有或全无」会在一个只看得见一半的表单上报错。
 */
export function SettingsForm({ form, onSaved }: { form: Form; onSaved: () => void }) {
  const { t } = useI18n();
  const host = useSettingsHost();
  const put = host.usePutSettings();
  const toasts = useKumoToastManager();
  const [drafts, setDrafts] = useState<Record<string, string>>(() =>
    Object.fromEntries(form.entries.map((e) => [e.key, toDraft(e.kind, e.value)])),
  );
  const [clearing, setClearing] = useState<Record<string, boolean>>({});
  const [errors, setErrors] = useState<Record<string, EncodeError>>({});
  // 高影响键的原因弹层：待提交的那批先存下来，原因填完再一起发。
  // 包一层而不是裸数组：「开没开」不该寄托在「待提交批恰好非空」上。
  const [pending, setPending] = useState<{ changes: Change[] } | null>(null);
  const [reason, setReason] = useState("");

  const commit = (changes: Change[], why: string) => {
    put.mutate(
      { data: why ? { changes, reason: why } : { changes } },
      {
        onSuccess: () => {
          toasts.add({ variant: "success", title: t("settingsSaved") });
          setPending(null);
          setReason("");
          setClearing({});
          onSaved();
        },
      },
    );
  };

  const collect = (): { changes: Change[]; high: boolean } | null => {
    const changes: Change[] = [];
    const nextErrors: Record<string, EncodeError> = {};
    let high = false;
    for (const entry of form.entries) {
      const draft = drafts[entry.key] ?? "";
      if (entry.kind === "secret") {
        const enc = encodeSecret(draft, clearing[entry.key] === true);
        if (enc) {
          changes.push({ key: entry.key, value: enc.value });
          high ||= needsReason(entry);
        }
        continue;
      }
      if (!isDirty(entry, draft)) continue;
      const encoded = encodeValue(entry, draft);
      if (!encoded.ok) {
        nextErrors[entry.key] = encoded.error;
        continue;
      }
      changes.push({ key: entry.key, value: encoded.value });
      high ||= needsReason(entry);
    }
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length > 0) return null;
    return { changes, high };
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const batch = collect();
    if (!batch || batch.changes.length === 0) return;
    // ADR-0043 §二：影响面超出运营后台的变更必须确认，且这一类的「确认」
    // 就是那句原因——它进审计 meta，是事后唯一能复核这次变更的东西。
    // 服务端同样强制（ADR-0198）；这里拦只是省一次往返。
    if (batch.high) {
      setPending({ changes: batch.changes });
      return;
    }
    commit(batch.changes, "");
  };

  const dirty = form.entries.some(
    (entry) =>
      clearing[entry.key] === true ||
      (entry.kind === "secret"
        ? (drafts[entry.key] ?? "").trim() !== ""
        : isDirty(entry, drafts[entry.key] ?? "")),
  );
  const formId = form.section ?? form.entries[0]!.key;

  return (
    <div className="space-y-2">
      {/* 纵向堆叠的裸 form，不用 FormRow：FormRow.Item 的契约是一个 Item 一个 Field——
          它让子树里每个 Field 变成 display:contents 并锁定行号，N 个 Field 放进一个 Item
          会被 grid 自动放置摆成 N 列（一次真实的溢出事故）。 */}
      <form onSubmit={submit} className="grid gap-4">
        {form.section && (
          <SectionHeading level="sub" as="h3">
            <span className="font-mono text-[0.9em] text-kumo-subtle">{form.section}</span>
          </SectionHeading>
        )}
        {form.entries.map((entry) => (
          <SettingField
            key={entry.key}
            entry={entry}
            draft={drafts[entry.key] ?? ""}
            onDraft={(next) => setDrafts((d) => ({ ...d, [entry.key]: next }))}
            clearing={clearing[entry.key] === true}
            onClear={() => setClearing((c) => ({ ...c, [entry.key]: !c[entry.key] }))}
            error={errors[entry.key]}
          />
        ))}
        <FormActions>
          <Button type="submit" loading={put.isPending} disabled={!dirty}>
            {t("save")}
          </Button>
        </FormActions>
      </form>
      {/* 弹层开着时错误已在弹层里，行下不再重复同一句 */}
      {put.isError && pending === null && <Alert>{host.errorMessage(put.error)}</Alert>}
      {/* 每张表单各挂一个 FormDialog。ConfirmDialog 的文档提醒「长列表别每行一个」，
          那条针对的是会增长的列表；这里是注册表驱动的固定表单集，且每张有自己的
          草稿与 mutation，共用一个实例反而要把这两样提到父层。
          未打开时 Base UI 不往 DOM 里挂内容，静态成本只是空的 React 子树。 */}
      <FormDialog
        open={pending !== null}
        onOpenChange={(open) => {
          if (!open) {
            setPending(null);
            setReason("");
            put.reset();
          }
        }}
        title={t("settingsConfirmTitle", { key: formId })}
        description={
          <span className="grid gap-1">
            {(pending?.changes ?? []).map((c) => (
              <span key={c.key} className="font-mono text-[0.9em]">
                {c.key}:{" "}
                {describeChange(
                  form.entries.find((e) => e.key === c.key),
                  c.value,
                )}
              </span>
            ))}
            <span>{t("settingsConfirmBatchBody")}</span>
          </span>
        }
        error={put.isError ? host.errorMessage(put.error) : undefined}
        submitLabel={t("save")}
        submitDisabled={!reason.trim()}
        pending={put.isPending}
        onSubmit={() => {
          const why = reason.trim();
          if (!why || !pending) return;
          commit(pending.changes, why);
        }}
      >
        <Field
          label={t("settingsReasonLabel")}
          htmlFor={`${controlId(formId)}-reason`}
          hint={t("settingsReasonHint")}
          required
        >
          <Input
            id={`${controlId(formId)}-reason`}
            value={reason}
            autoFocus
            required
            maxLength={500}
            onChange={(e) => setReason(e.target.value)}
          />
        </Field>
      </FormDialog>
    </div>
  );
}

/** 确认弹层里的「从 → 到」：密钥两端都只给掩码或占位，明文不进弹层。 */
function describeChange(entry: SettingEntry | undefined, next: unknown): string {
  if (!entry) return JSON.stringify(next ?? null);
  if (entry.kind === "secret") {
    const from = entry.set ? (entry.hint ?? "•••") : "—";
    const to = next === "" ? "—" : "•••";
    return `${from} → ${to}`;
  }
  return `${JSON.stringify(entry.value ?? null)} → ${JSON.stringify(next ?? null)}`;
}

function SettingField({
  entry,
  draft,
  onDraft,
  clearing,
  onClear,
  error,
}: {
  entry: SettingEntry;
  draft: string;
  onDraft: (next: string) => void;
  clearing: boolean;
  onClear: () => void;
  error: EncodeError | undefined;
}) {
  const { t } = useI18n();
  const description = useDescription(entry);
  const hasRange = entry.min !== undefined || entry.max !== undefined;
  return (
    <Field
      label={<KeyLabel entry={entry} />}
      htmlFor={controlId(entry.key)}
      hint={
        <span className="grid gap-1">
          <span>{description}</span>
          {hasRange && entry.kind === "int" && (
            <span>
              {t("settingsRangeHint", {
                min: entry.min ?? "−∞",
                max: entry.max ?? "∞",
              })}
            </span>
          )}
          {/* 金额键的 min/max 在契约里以 nano 计；提示换回主单位，填表的人从未见过 nano */}
          {hasRange && entry.kind === "money" && (
            <span>
              {t("settingsRangeHint", {
                min: entry.min === undefined ? "−∞" : formatNano(entry.min),
                max: entry.max === undefined ? "∞" : formatNano(entry.max),
              })}
            </span>
          )}
          {entry.kind === "secret" && (
            <span>{entry.set ? t("settingsSecretSetHint") : t("settingsSecretUnsetHint")}</span>
          )}
          <Provenance entry={entry} />
        </span>
      }
      error={error ? <ErrorText error={error} /> : undefined}
    >
      <SettingControl
        entry={entry}
        draft={draft}
        onDraft={onDraft}
        clearing={clearing}
        onClear={onClear}
      />
      {/* 未配置的汇率不是一个普通的 "0"：它意味着 CNY 计费当下就在报错。
          把它藏在说明文字里等于让一个正在发生的故障和正常值长得一样。 */}
      {entry.key === "gateway.fx_usd_cny" && String(entry.value ?? "0") === "0" && (
        <Alert variant="warning">{t("settingsFxUnconfigured")}</Alert>
      )}
    </Field>
  );
}

/** 键名里有点号，直接当 DOM id 会让 querySelector 把它当 class 选择器。 */
function controlId(key: string): string {
  return `setting-${key.replace(/\./g, "-")}`;
}

function ErrorText({ error }: { error: EncodeError }) {
  const { t } = useI18n();
  switch (error.code) {
    case "required":
      return <>{t("settingsErrRequired")}</>;
    case "not_integer":
      return <>{t("settingsErrNotInteger")}</>;
    case "not_decimal":
      return <>{t("settingsErrNotDecimal")}</>;
    case "not_money":
      return <>{t("settingsErrNotMoney")}</>;
    case "not_json":
      return <>{t("settingsErrNotJson")}</>;
    case "out_of_range":
      return <>{t("settingsErrOutOfRange", { min: error.min, max: error.max })}</>;
  }
}

/**
 * 控件按 kind 选（ADR-0068）。
 *
 * 此前只分了 `enum` 与「其余」两支，其余一律文本框、且**提交的就是那个字符串**。
 * 于是 10 个整数键点保存必然 400——控件形态与值形态是同一件事的两半，
 * 只挑一半分支就等于两半都没分对。
 */
function SettingControl({
  entry,
  draft,
  onDraft,
  clearing,
  onClear,
}: {
  entry: SettingEntry;
  draft: string;
  onDraft: (next: string) => void;
  clearing: boolean;
  onClear: () => void;
}): ReactNode {
  const { t } = useI18n();
  const id = controlId(entry.key);
  switch (entry.kind) {
    case "secret":
      return (
        <SecretInput
          id={id}
          value={draft}
          onValueChange={onDraft}
          hint={entry.set ? (entry.hint ?? "•••") : undefined}
          clearing={clearing}
          onClear={onClear}
          clearLabel={t("settingsSecretClear")}
          undoLabel={t("settingsSecretUndoClear")}
          placeholder={entry.set ? undefined : t("settingsSecretPlaceholder")}
        />
      );
    case "enum":
      return (
        <Select /* ui-ignore -- SettingControl 始终渲染在 SettingsEditor 的 FieldContext 内 */
          value={draft}
          onValueChange={(v) => onDraft(v ?? "")}
          items={(entry.enum ?? []).map((opt) => ({ value: opt, label: opt || "—" }))}
        />
      );
    case "bool":
      return (
        <Select /* ui-ignore -- SettingControl 始终渲染在 SettingsEditor 的 FieldContext 内 */
          value={draft}
          onValueChange={(v) => onDraft(v ?? "false")}
          items={[
            { value: "true", label: "true" },
            { value: "false", label: "false" },
          ]}
        />
      );
    case "map_string_int":
      return (
        <Textarea
          id={id}
          rows={4}
          className="font-mono text-[0.9em]"
          value={draft}
          onChange={(e) => onDraft(e.target.value)}
        />
      );
    case "int":
      return (
        <Input
          id={id}
          inputMode="numeric"
          value={draft}
          onChange={(e) => onDraft(e.target.value)}
        />
      );
    case "money":
      return (
        <Input
          id={id}
          inputMode="decimal"
          placeholder="0.00"
          value={draft}
          onChange={(e) => onDraft(e.target.value)}
        />
      );
    default:
      return <Input id={id} value={draft} onChange={(e) => onDraft(e.target.value)} />;
  }
}
