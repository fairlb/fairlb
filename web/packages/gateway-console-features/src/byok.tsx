import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { gatewayConsoleApi, type GatewayConsoleTypes, apiErrorMessage } from "@fairlb/api-client";
import { type MessageKey, useI18n } from "@fairlb/i18n";
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
  LoadMoreButton,
  LoadingState,
  Select,
  StatusBadge,
  VendorMark,
  useCursorList,
} from "@fairlb/ui";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

// Bring-your-own upstream provider keys.
//
// A key's plaintext exists only in the one request that adds it: it is encrypted on
// the way into storage and no endpoint reads it back. The interface therefore shows
// a mask and a status and offers no "reveal key" affordance at all.

// What a vendor's credential looks like, so the field can say what to paste
// rather than leaving a 401 to explain it. An unknown hint renders nothing: a
// newer server naming a shape this build has no wording for should not put a
// message key on screen.
const KEY_HINT_LABEL: Record<string, MessageKey> = {
  bearer: "byokKeyHintBearer",
  aws_keypair_json: "byokKeyHintAwsKeypair",
  gcp_service_account_json: "byokKeyHintGcpServiceAccount",
  kling_keypair_json: "byokKeyHintKlingKeypair",
};

const STATUS_LABEL: Record<string, MessageKey> = {
  active: "byokStatusActive",
  invalid: "byokStatusInvalid",
  disabled: "byokStatusDisabled",
};

// vendorKeyHint renders the credential-shape hint for the chosen vendor, and
// nothing at all when this build has no wording for what the server named.
function vendorKeyHint(
  v: GatewayConsoleTypes.OrgProviderVendor | undefined,
  t: (key: MessageKey) => string,
): string | undefined {
  const key = v?.key_hint ? KEY_HINT_LABEL[v.key_hint] : undefined;
  return key ? t(key) : undefined;
}

export function BYOKSection({
  orgId,
  adding,
  onAddingChange,
}: {
  orgId: string;
  /** The calling page owns the add dialog's open state, because the button that
   * opens it sits in the section heading row rather than inside this card. */
  adding: boolean;
  onAddingChange: (open: boolean) => void;
}) {
  const { t } = useI18n();
  const del = gatewayConsoleApi.useDeleteOrgProviderKey();
  const toasts = useKumoToastManager();
  const [deleting, setDeleting] = useState<{ id: string; name: string } | null>(null);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [generation, setGeneration] = useState(0);
  const queryClient = useQueryClient();
  const keys = gatewayConsoleApi.useListOrgProviderKeys(orgId, cursor ? { cursor } : undefined);
  const { items, nextCursor } = useCursorList<GatewayConsoleTypes.OrgProviderKey>(
    keys,
    (k) => k.id,
    String(generation),
  );
  // 改动之后回到第一页，并且清空缓存而不是标记为陈旧：`useCursorList` 按 id 去重
  // 且不替换已见的行，而 `refetch` 在新数据到达前仍交回旧的那一页——测试一把密钥
  // 之后它的状态会一直停在旧值（ADR-0185 那处同一机理）。
  const refresh = () => {
    setCursor(undefined);
    setGeneration((g) => g + 1);
    void queryClient.resetQueries({
      queryKey: gatewayConsoleApi.getListOrgProviderKeysQueryKey(orgId),
    });
  };
  // Which platforms a credential can be supplied for travels with the list
  // itself, so the rows and the add form always agree. Undefined while the query
  // is undecided, which is not the same as "none available" -- the dialog tells
  // those apart.
  const vendors = keys.data?.vendors;

  // Heading and description come from the calling page, not from inside this card:
  // a section's heading belongs outside its card, and repeating it here would put
  // two headings in a row.
  return (
    <Card className="space-y-4">
      {keys.isError && <Alert>{apiErrorMessage(keys.error)}</Alert>}

      {keys.isPending ? (
        <LoadingState label={t("loading")} />
      ) : items.length === 0 ? (
        <InlineEmpty title={t("byokNone")} />
      ) : (
        <ul className="space-y-2">
          {items.map((k) => (
            <KeyRow
              key={k.id}
              orgId={orgId}
              k={k}
              onChanged={() => refresh()}
              onDelete={() => setDeleting({ id: k.id, name: k.name })}
            />
          ))}
        </ul>
      )}
      <LoadMoreButton
        onClick={nextCursor ? () => setCursor(nextCursor) : undefined}
        pending={keys.isFetching}
        label={t("loadMore")}
      />

      <AddKeyDialog
        vendors={vendors}
        orgId={orgId}
        open={adding}
        onOpenChange={onAddingChange}
        onAdded={() => refresh()}
      />

      {/* One confirm dialog per section, driven by which row is pending — not one
          mounted instance per row. */}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t("byokDeleteConfirmTitle")}
        description={t("byokDeleteConfirmBody", { name: deleting?.name ?? "" })}
        confirmLabel={t("byokDelete")}
        pending={del.isPending}
        onConfirm={() => {
          if (!deleting) return;
          del.mutate(
            { orgId, keyId: deleting.id },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("byokDeletedDone") });
                setDeleting(null);
                refresh();
              },
            },
          );
        }}
      />
    </Card>
  );
}

function KeyRow({
  orgId,
  k,
  onChanged,
  onDelete,
}: {
  orgId: string;
  k: GatewayConsoleTypes.OrgProviderKey;
  onChanged: () => void;
  onDelete: () => void;
}) {
  const { formatDate, t } = useI18n();
  const test = gatewayConsoleApi.useTestOrgProviderKey();
  const [testing, setTesting] = useState(false);
  const [model, setModel] = useState("");
  const [result, setResult] = useState<GatewayConsoleTypes.OrgProviderKeyTestResult | null>(null);

  const invalid = k.status === "invalid";

  const closeTest = () => {
    setTesting(false);
    setModel("");
    setResult(null);
    test.reset();
  };

  return (
    <li className="rounded-md border border-kumo-line p-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-base">
        {/* Decorative: the vendor name sits next to it. */}
        <span className="flex items-center gap-2 font-medium">
          <VendorMark id={k.vendor} size="sm" aria-hidden="true" />
          {k.vendor_label ?? k.vendor}
        </span>
        <span className="min-w-0 truncate">{k.name}</span>
        <span className="font-mono text-base text-kumo-subtle">{k.secret_hint}</span>
        {/* The status is words, not only a colour: "the provider rejected this key"
            has to be readable without colour vision. */}
        <StatusBadge tone={invalid ? "danger" : k.status === "active" ? "success" : "neutral"}>
          {t(STATUS_LABEL[k.status] ?? "byokStatusActive")}
        </StatusBadge>
        <span className="text-base text-kumo-subtle">
          {t("byokColVerified")}:{" "}
          {k.last_verified_at ? formatDate(k.last_verified_at) : t("byokNeverVerified")}
        </span>
        <div className="ml-auto flex shrink-0 items-center gap-2">
          {!testing && (
            <Button variant="outline" size="sm" onClick={() => setTesting(true)}>
              {t("byokTest")}
            </Button>
          )}
          <Button variant="secondary-destructive" size="sm" onClick={() => onDelete()}>
            {t("byokDelete")}
          </Button>
        </div>
      </div>

      {invalid && <p className="mt-2 text-base text-kumo-danger">{t("byokInvalidNote")}</p>}

      {/* The connectivity test is a single-field inline action, so it expands in
          place on demand rather than living on the row. Left permanently open,
          every row would carry a labelled input plus two lines of explanation, and
          three keys would already be a long page — while most visits are here just
          to glance at the status. */}
      {testing && (
        <FormRow
          as="form"
          className="mt-3 sm:grid-cols-[minmax(12rem,1fr)_auto]"
          onSubmit={(e) => {
            e.preventDefault();
            test.mutate(
              { orgId, keyId: k.id, data: { upstream_model: model.trim() } },
              {
                onSuccess: (r) => {
                  setResult(r);
                  onChanged(); // the status may have moved (a 401 marks it invalid)
                },
              },
            );
          }}
        >
          <FormRow.Item>
            {/* The explanation goes in the field's own hint slot rather than a
                separate paragraph: it is this field's hint, and a hint only grows
                downward, so it does not move the buttons beside it. */}
            <Field label={t("byokTestModel")} htmlFor={`m-${k.id}`} hint={t("byokTestNote")}>
              <Input
                id={`m-${k.id}`}
                value={model}
                placeholder="gpt-4o-mini"
                autoFocus
                required
                onChange={(e) => setModel(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Actions>
            <Button
              type="submit"
              variant="outline"
              loading={test.isPending}
              disabled={!model.trim()}
            >
              {t("byokTest")}
            </Button>
            <Button type="button" variant="ghost" onClick={closeTest}>
              {t("cancel")}
            </Button>
          </FormRow.Actions>
        </FormRow>
      )}

      {testing && result && (
        <p className="mt-2 text-base">
          <span className={result.ok ? "text-kumo-success" : "text-kumo-danger"}>
            {result.ok ? t("byokTestPass") : t("byokTestFail")}
          </span>
          {result.latency_ms != null && (
            <span className="ml-2 text-kumo-subtle tabular-nums">{result.latency_ms} ms</span>
          )}
          {/* The upstream's own words, verbatim: telling a bad credential from a
              bad model name from an exhausted quota depends entirely on them. */}
          {result.message && (
            <span className="ml-2 break-all text-kumo-subtle">{result.message}</span>
          )}
        </p>
      )}
    </li>
  );
}

function AddKeyDialog({
  orgId,
  vendors,
  open,
  onOpenChange,
  onAdded,
}: {
  orgId: string;
  /** The platforms this deployment routes to. Undefined means the list has not
   * answered yet; empty means it answered and there are none, and those two
   * states must not render the same way. */
  vendors: GatewayConsoleTypes.OrgProviderVendor[] | undefined;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAdded: () => void;
}) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const create = gatewayConsoleApi.useCreateOrgProviderKey();
  // No default. Falling back to the first vendor made both the placeholder and
  // the submit guard unreachable, so a credential could be filed under whichever
  // platform sorted first -- and then sent, in clear, to that platform's
  // endpoint. Which account a key belongs to is not a thing to guess.
  const [vendor, setVendor] = useState<string>("");
  const chosen = vendor;
  const chosenVendor = vendors?.find((v) => v.vendor === chosen);
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  // No fallback by default: falling back to the gateway's own credentials silently
  // means paying the full listed price, so that choice has to be made deliberately.
  const [allowFallback, setAllowFallback] = useState(false);

  const reset = () => {
    // The plaintext should not sit in component state a moment longer than needed:
    // both closing the dialog and a successful submit clear it immediately.
    setSecret("");
    setName("");
    setBaseUrl("");
    setVendor("");
    setAllowFallback(false);
    create.reset();
  };

  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
      title={t("byokAdd")}
      description={t("byokSecretNote")}
      error={create.isError ? apiErrorMessage(create.error) : undefined}
      submitLabel={t("byokCreate")}
      submitDisabled={!chosen || !name.trim() || !secret.trim()}
      pending={create.isPending}
      onSubmit={() =>
        create.mutate(
          {
            orgId,
            data: {
              vendor: chosen,
              name: name.trim(),
              secret: secret.trim(),
              ...(baseUrl.trim() ? { base_url: baseUrl.trim() } : {}),
              allow_fallback: allowFallback,
            },
          },
          {
            onSuccess: () => {
              onOpenChange(false);
              reset();
              toasts.add({ variant: "success", title: t("byokCreatedDone") });
              onAdded();
            },
          },
        )
      }
    >
      {vendors !== undefined && vendors.length === 0 ? (
        // Not an empty select: a credential can only be supplied for a platform
        // this deployment routes to, so with none configured there is nothing to
        // choose and saying why beats an empty list somebody has to interpret.
        <Alert>{t("byokNoVendors")}</Alert>
      ) : (
        <Field label={t("byokColVendor")} hint={vendorKeyHint(chosenVendor, t)}>
          <Select
            value={chosen}
            placeholder={t("byokVendorPick")}
            onValueChange={(v) => setVendor(v ?? "")}
            items={(vendors ?? []).map((v) => ({ value: v.vendor, label: v.label }))}
          />
        </Field>
      )}
      <Field label={t("byokColName")} htmlFor="byok-name">
        <Input
          id="byok-name"
          value={name}
          placeholder="prod"
          autoFocus
          required
          onChange={(e) => setName(e.target.value)}
        />
      </Field>
      <Field label={t("byokSecret")} htmlFor="byok-secret">
        <Input
          id="byok-secret"
          type="password"
          autoComplete="off"
          value={secret}
          placeholder="sk-…"
          required
          onChange={(e) => setSecret(e.target.value)}
        />
      </Field>
      <Field
        label={t("byokBaseUrl")}
        htmlFor="byok-base"
        hint={
          chosenVendor?.base_url_hint
            ? t("byokBaseUrlHintKnown", { url: chosenVendor.base_url_hint })
            : t("byokBaseUrlHint")
        }
      >
        <Input
          id="byok-base"
          value={baseUrl}
          placeholder={chosenVendor?.base_url_hint ?? "https://…"}
          onChange={(e) => setBaseUrl(e.target.value)}
        />
      </Field>
      <Checkbox
        checked={allowFallback}
        onCheckedChange={(next) => setAllowFallback(next === true)}
        label={
          <span>
            {t("byokAllowFallback")}
            <span className="block text-base text-kumo-subtle">{t("byokAllowFallbackHint")}</span>
          </span>
        }
        controlFirst
      />
    </FormDialog>
  );
}
