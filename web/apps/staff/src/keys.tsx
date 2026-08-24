import { ApiError, communityStaffApi, type CommunityStaffTypes } from "@fairlb/api-client";
import { useI18n, type MessageKey } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  Checkbox,
  ConfirmDialog,
  Field,
  FormDialog,
  FormRow,
  InlineEmpty,
  Input,
  LoadingState,
  PageHeader,
  ResponsiveResourceRow,
  SecretRevealDialog,
  Select,
  StatusBadge,
  formatNano,
  useAdminTitle,
} from "@fairlb/ui";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { useState } from "react";
import { apiErrorMessage } from "@fairlb/api-client";
import { useKeyModelOptions } from "@fairlb/gateway-console-features/key-models";

/**
 * The API key management page.
 *
 * # Why this page belongs to the app rather than to a feature package
 *
 * Managing keys as an administrator of the whole deployment is a different page
 * from managing them as a member of an organization: no organization in the
 * path, no membership roles to authorize against, no balance panel beside the
 * list. What it reuses are the shared UI components — the dialog that shows a
 * secret exactly once, the responsive resource row, the confirmation dialog.
 * What is written here is which endpoint to call and what a row shows.
 *
 * # Keys belong to a team
 *
 * A team is the level the model allowance and the rate ceilings are configured
 * at, so "this group may use only this model" is said by putting that group's
 * keys in their own team. The page therefore shows one team at a time; the
 * picker is the only navigation it needs, and with a single team it is the only
 * option and the page reads exactly as it did before teams existed.
 *
 * # No pagination
 *
 * The list endpoint caps at 200 and a single deployment has keys in the tens. If
 * the scale ever demands paging, there should first be a user journey that
 * explains it.
 */
export function CommunityKeysPage() {
  const { t, formatDate } = useI18n();
  const toasts = useKumoToastManager();
  useAdminTitle(t("apiKeys"));

  const teams = communityStaffApi.useCommunityListTeams();
  const teamItems = teams.data?.items ?? [];
  // undefined means "not chosen yet", which resolves to the first team once the
  // list arrives. Defaulting to a literal id would need one before the fetch.
  const [teamId, setTeamId] = useState<string | undefined>(undefined);
  const team = teamItems.find((item) => item.id === teamId) ?? teamItems[0];

  const keys = communityStaffApi.useCommunityListKeys(team ? { team_id: team.id } : undefined, {
    query: { enabled: team != null },
  });
  const create = communityStaffApi.useCommunityCreateKey();
  const revoke = communityStaffApi.useCommunityRevokeKey();

  const [creating, setCreating] = useState(false);
  const [keyName, setKeyName] = useState("");
  const [plaintext, setPlaintext] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<{ id: string; name: string } | null>(null);
  const [editing, setEditing] = useState<CommunityStaffTypes.ApiKey | null>(null);

  const rows: CommunityStaffTypes.ApiKey[] = keys.data?.items ?? [];

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("apiKeys")}
        description={t("apiKeysDesc")}
        actions={
          <Button onClick={() => setCreating(true)} disabled={team == null}>
            {t("createKey")}
          </Button>
        }
      />

      {keys.isError && <Alert>{apiErrorMessage(keys.error)}</Alert>}
      {teams.isError && <Alert>{apiErrorMessage(teams.error)}</Alert>}

      {/* One team at a time. The picker is hidden while there is only one:
          a control with a single option is a control that answers a question
          nobody has yet asked. */}
      {teamItems.length > 1 && team && (
        <Card>
          <Select
            label={t("teamPick")}
            value={team.id}
            onValueChange={(value) => setTeamId(value ?? team.id)}
            items={teamItems.map((item) => ({
              value: item.id,
              label:
                item.status === "suspended" ? `${item.name} (${t("teamSuspended")})` : item.name,
            }))}
          />
        </Card>
      )}

      <Card>
        {keys.isPending || teams.isPending ? (
          <LoadingState label={t("loading")} />
        ) : rows.length === 0 ? (
          <InlineEmpty title={t("keysEmpty")} />
        ) : (
          <div>
            {rows.map((k) => (
              <ResponsiveResourceRow
                key={k.id}
                title={
                  <>
                    {k.name}
                    {k.status !== "active" && (
                      <span className="ml-1 rounded bg-kumo-tint px-1.5 py-0.5 text-base font-normal">
                        {t("keyRevoked")}
                      </span>
                    )}
                    {/* A key that can call nothing is the state most worth
                        seeing from the list: it looks healthy everywhere else
                        and refuses everything. */}
                    {k.model_access &&
                      !k.model_access.allow_all &&
                      k.model_access.models.length === 0 && (
                        <span className="ml-2">
                          <StatusBadge tone="warning">{t("keyNoModels")}</StatusBadge>
                        </span>
                      )}
                  </>
                }
                description={
                  <>
                    <code>{k.prefix}…</code> · {formatDate(k.created_at)} · {limitsSummary(k, t)}
                  </>
                }
                metadata={<>{formatNano(k.total_spent_nano)}</>}
                actions={
                  k.status === "active" ? (
                    <>
                      <Button size="sm" variant="outline" onClick={() => setEditing(k)}>
                        {t("keyLimits")}
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => setRevoking({ id: k.id, name: k.name })}
                      >
                        {t("keyRevoke")}
                      </Button>
                    </>
                  ) : undefined
                }
              />
            ))}
          </div>
        )}
      </Card>

      {/* The plaintext appears exactly once: SecretRevealDialog owns the only
          exit from the reveal state. */}
      <SecretRevealDialog
        open={creating}
        onOpenChange={(open) => {
          setCreating(open);
          if (!open) {
            setKeyName("");
            create.reset();
          }
        }}
        title={t("createKey")}
        error={create.isError ? apiErrorMessage(create.error) : undefined}
        submitLabel={t("createKey")}
        submitDisabled={!keyName.trim()}
        pending={create.isPending}
        onSubmit={() =>
          create.mutate(
            { data: { name: keyName.trim(), ...(team ? { team_id: team.id } : {}) } },
            {
              onSuccess: (r) => {
                setPlaintext(r.key);
                void keys.refetch();
              },
            },
          )
        }
        secret={plaintext}
        secretHint={t("keyCreatedOnce")}
        secretLabel={t("keyPlainLabel")}
        onDone={() => {
          setCreating(false);
          setPlaintext(null);
          setKeyName("");
        }}
      >
        <Field label={t("keyName")} htmlFor="key-name">
          <Input
            id="key-name"
            value={keyName}
            autoFocus
            required
            maxLength={100}
            onChange={(e) => setKeyName(e.target.value)}
          />
        </Field>
        {/* Which team the key lands in is stated even when there is one, because
            the answer decides what the key may reach and how fast. */}
        {team && (
          <Field label={t("teamPick")} hint={t("keyTeamHint")}>
            <Select
              value={team.id}
              onValueChange={(value) => setTeamId(value ?? team.id)}
              items={teamItems.map((item) => ({ value: item.id, label: item.name }))}
            />
          </Field>
        )}
      </SecretRevealDialog>

      <KeyLimitsDialog
        apiKey={editing}
        teamId={team?.id ?? ""}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null);
          void keys.refetch();
          toasts.add({ variant: "success", title: t("keyLimitsSaved") });
        }}
      />

      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(o) => !o && setRevoking(null)}
        title={t("keyRevoke")}
        description={revoking?.name ?? ""}
        confirmLabel={t("keyRevoke")}
        pending={revoke.isPending}
        onConfirm={() => {
          if (!revoking) return;
          revoke.mutate(
            { keyId: revoking.id },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("keyRevoke") });
                setRevoking(null);
                void keys.refetch();
              },
              onError: (e) => {
                toasts.add({
                  variant: "error",
                  title: e instanceof ApiError ? apiErrorMessage(e) : t("errorTitle"),
                });
                setRevoking(null);
              },
            },
          );
        }}
      />
    </div>
  );
}

/**
 * One line saying what a key is limited to.
 *
 * "No limits" is written out rather than left blank: a blank reads as something
 * that has not loaded, and the reader cannot tell it from a key whose limits
 * simply have not arrived yet.
 */
function limitsSummary(k: CommunityStaffTypes.ApiKey, t: Translate): string {
  const parts: string[] = [];
  if (k.rate_limit_rpm) parts.push(`${k.rate_limit_rpm} RPM`);
  if (k.rate_limit_tpm) parts.push(`${k.rate_limit_tpm} TPM`);
  if (k.spend_limit_nano) parts.push(formatNano(k.spend_limit_nano));
  if (k.model_access && !k.model_access.allow_all && k.model_access.models.length > 0) {
    parts.push(`${k.model_access.models.length} models`);
  }
  return parts.length > 0 ? parts.join(" · ") : t("keyNoLimits");
}

/** The translator's shape, which the i18n package does not export as a type. */
type Translate = (key: MessageKey, vars?: Record<string, string | number>) => string;

/**
 * Editing one key's limits.
 *
 * The fields are text and not numbers because an empty field is a state a
 * number input cannot hold, and empty is a real setting here: it means no
 * ceiling. Clearing one therefore travels in the contract's `clear` list —
 * after JSON decoding, "sent as null" and "not sent at all" are the same absent
 * field, so emptying a box and not touching it would arrive identically.
 */
function KeyLimitsDialog({
  apiKey,
  teamId,
  onClose,
  onSaved,
}: {
  apiKey: CommunityStaffTypes.ApiKey | null;
  teamId: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const update = communityStaffApi.useCommunityUpdateKey();
  // The options are what this team may actually reach, not the whole catalogue:
  // the write path refuses a model the team's tier does not admit, and offering
  // one here would be offering a choice that cannot be saved.
  const models = useKeyModelOptions(teamId, apiKey !== null);

  const [rpm, setRpm] = useState("");
  const [tpm, setTpm] = useState("");
  const [allowAll, setAllowAll] = useState(true);
  const [allowed, setAllowed] = useState<string[]>([]);
  // Which key the fields were last filled from. Comparing against it is what
  // reloads them when the dialog is reopened on a different key -- without it
  // the second key opens showing the first one's limits.
  const [loadedFor, setLoadedFor] = useState<string | null>(null);

  if (apiKey && loadedFor !== apiKey.id) {
    setLoadedFor(apiKey.id);
    setRpm(apiKey.rate_limit_rpm ? String(apiKey.rate_limit_rpm) : "");
    setTpm(apiKey.rate_limit_tpm ? String(apiKey.rate_limit_tpm) : "");
    setAllowAll(apiKey.model_access?.allow_all ?? true);
    setAllowed(apiKey.model_access?.models ?? []);
    update.reset();
  }

  const numberOf = (text: string): number | undefined => {
    const trimmed = text.trim();
    if (trimmed === "") return undefined;
    const n = Number(trimmed);
    return Number.isInteger(n) && n >= 1 ? n : undefined;
  };
  const valid =
    (rpm.trim() === "" || numberOf(rpm) !== undefined) &&
    (tpm.trim() === "" || numberOf(tpm) !== undefined);

  return (
    <FormDialog
      open={apiKey !== null}
      onOpenChange={(next) => !next && onClose()}
      title={t("keyLimits")}
      description={apiKey?.name}
      error={update.isError ? apiErrorMessage(update.error) : undefined}
      submitLabel={t("save")}
      submitDisabled={!valid}
      pending={update.isPending}
      onSubmit={() => {
        if (!apiKey) return;
        const clear: CommunityStaffTypes.UpdateKeyRequestClearItem[] = [];
        const data: CommunityStaffTypes.UpdateKeyRequest = {
          model_access: { allow_all: allowAll, models: allowAll ? [] : allowed },
        };
        if (numberOf(rpm) !== undefined) data.rate_limit_rpm = numberOf(rpm);
        else clear.push("rate_limit_rpm");
        if (numberOf(tpm) !== undefined) data.rate_limit_tpm = numberOf(tpm);
        else clear.push("rate_limit_tpm");
        if (clear.length > 0) data.clear = clear;
        update.mutate({ keyId: apiKey.id, data }, { onSuccess: onSaved });
      }}
    >
      <FormRow>
        <Field label={t("keyRpm")} htmlFor="key-rpm" hint={t("keyRateHint")}>
          <Input
            id="key-rpm"
            inputMode="numeric"
            value={rpm}
            placeholder={t("keyNoLimits")}
            onChange={(e) => setRpm(e.target.value)}
          />
        </Field>
        <Field label={t("keyTpm")} htmlFor="key-tpm" hint={t("keyRateHint")}>
          <Input
            id="key-tpm"
            inputMode="numeric"
            value={tpm}
            placeholder={t("keyNoLimits")}
            onChange={(e) => setTpm(e.target.value)}
          />
        </Field>
      </FormRow>

      <Field label={t("keyModelAccess")} hint={t("keyModelAccessHint")}>
        <Checkbox
          checked={allowAll}
          onCheckedChange={(next) => setAllowAll(next === true)}
          label={t("keyAllowAll")}
        />
      </Field>

      {!allowAll && (
        <div className="flex max-h-64 flex-col gap-1 overflow-y-auto">
          {models.length === 0 && <p className="text-base text-kumo-subtle">{t("keyNoCatalog")}</p>}
          {models.map((slug) => (
            <Checkbox
              key={slug}
              checked={allowed.includes(slug)}
              onCheckedChange={() =>
                setAllowed(
                  allowed.includes(slug) ? allowed.filter((x) => x !== slug) : [...allowed, slug],
                )
              }
              label={<span className="font-mono text-base">{slug}</span>}
            />
          ))}
        </div>
      )}

      {/* An empty allowlist refuses every model. It is a legal setting and
          sometimes the intended one, so it is said rather than blocked. */}
      {!allowAll && allowed.length === 0 && <Alert>{t("keyAllowsNothing")}</Alert>}
    </FormDialog>
  );
}
