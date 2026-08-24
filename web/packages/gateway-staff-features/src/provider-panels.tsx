import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useDisplayDate, useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  CheckboxGroupField,
  ConfirmDialog,
  DataTable,
  Field,
  FormDialog,
  FormRow,
  InlineEmpty,
  Input,
  LoadingState,
  RowActions,
  SectionHeading,
  Select,
  PageActionDock,
  StatusBadge,
  Textarea,
  LoadMoreButton,
  useCursorList,
} from "@fairlb/ui";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { useEffect, useState } from "react";
import {
  type AdjustmentMode,
  bpsFromModePercent,
  isAdjustmentValid,
  modePercentFromBps,
  PROVIDER_MAX_BPS,
} from "./cost-adjustment";
import { AdjustmentFields } from "./cost-adjustment-fields";
import { isBlankOrPositive, positiveIntOf } from "./limit-fields";
import {
  HeaderMapEditor,
  type HeaderRow,
  headerRowsError,
  mapFromRows,
  rowsFromMap,
  sameHeaderMap,
} from "./header-map";
import {
  useProtocolItems,
  TRANSPORT_PLACEHOLDER,
  keyStatusLabel,
  parseTransportText,
  sameTransport,
  transportToText,
} from "./providers-shared";
import { useVendors, vendorBySlug } from "./vendors";

/**
 * The single submit surface of a provider's settings face.
 *
 * The header mapping and the cost multiplier used to be two panels with a save button
 * each, while both wrote the same request — two presses for two edits, two separate
 * failures, and a moment in between where storage held half a configuration. Merged
 * into one submit, it **sends only the fields that are dirty**: sending everything
 * would rewrite the untouched groups with whatever is currently rendered, quietly
 * reverting a value somebody else just changed under the guise of "I only meant to
 * edit a header".
 *
 * The basics — name, base URL, dialects — were folded in from a separate header
 * dialog: the same request, the same one-form-one-submit shape, with no overlapping
 * fields, split across a dialog and a separate page for no reason. The criterion is **the shape
 * of the fields themselves**: this group contains a multi-row add-and-remove editor,
 * so the whole attribute surface belongs in a dedicated detail-page section rather
 * than a dialog. A handful of scalars, as on a model, still belongs in a header dialog.
 */
export function ProviderConfigPanel({
  provider,
  onSaved,
  onDirtyChange,
}: {
  provider: GatewayStaffTypes.GatewayProvider;
  onSaved: () => void;
  /** The dirty flag is lifted to the detail page so it can guard against navigating
   * away. The predicate is still computed here; the page only forwards it. */
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const { t } = useI18n();
  const protocolItems = useProtocolItems();
  const vendors = useVendors().data?.items;
  const [vendor, setVendor] = useState(provider.vendor);
  const chosenVendor = vendorBySlug(vendor, vendors);
  const update = gatewayStaffApi.useUpdateGatewayProvider();
  const toasts = useKumoToastManager();

  // The basics used to live in a header dialog, writing the same request as this
  // face, in the same one-form-one-submit shape, with no overlapping fields — two
  // shapes holding one thing. An attribute surface containing a multi-row editor
  // belongs in the full settings section.
  const [name, setName] = useState(provider.name ?? "");
  const [baseUrl, setBaseUrl] = useState(provider.base_url);
  const [protocols, setProtocols] = useState<GatewayStaffTypes.GatewayProviderInputProtocolsItem[]>(
    () => [...provider.protocols],
  );
  const sameProtocols =
    protocols.length === provider.protocols.length &&
    [...protocols].sort().join() === [...provider.protocols].sort().join();
  // Dirtiness is judged per field, not per group: sending the base URL along with a
  // display-name change lets a stale page quietly roll back somebody
  // else's upstream migration. These three are independent scalars and can be spared
  // individually. The header mapping cannot: it is a single composite field, and
  // editing one row means sending the whole table.
  const nameDirty = name !== (provider.name ?? "");
  const baseUrlDirty = baseUrl !== provider.base_url;
  const protocolsDirty = !sameProtocols;
  const vendorDirty = vendor !== provider.vendor;
  const basicsDirty = nameDirty || baseUrlDirty || protocolsDirty || vendorDirty;
  // The protocols a provider may declare are the ones its vendor publishes, so
  // changing the vendor can invalidate a selection that was valid a moment ago.
  // The server refuses that combination; saying so here means the refusal is
  // legible next to the two fields that caused it.
  const protocolsFitVendor =
    !chosenVendor || protocols.every((f) => chosenVendor.protocols.includes(f));
  const basicsValid = baseUrl.trim() !== "" && protocols.length > 0 && protocolsFitVendor;

  const [rows, setRows] = useState<HeaderRow[]>(() => rowsFromMap(provider.headers));
  const headerErr = headerRowsError(rows);
  const nextHeaders = mapFromRows(rows);
  // An invalid edit is still an edit. It must keep the navigation guard and
  // action dock active even though it cannot yet be serialized for the API.
  const headerDirty = Boolean(headerErr) || !sameHeaderMap(nextHeaders, provider.headers);

  // The transport profile is edited as the object it is. A field-per-key form was
  // the obvious alternative and it is the wrong shape: two of the five keys are
  // maps of arbitrary size, so the form would be a nested editor for a setting
  // most providers never touch. What the operator copies out of the provider
  // recipes is a JSON object, and this lets them paste it.
  //
  // Only the parse is checked here. Which keys and values are legal is the
  // server's judgement, and duplicating that rule set in the browser would give
  // two answers to one question — the browser's would drift, and it would drift
  // towards accepting things the server refuses.
  const savedTransportText = transportToText(provider.transport);
  const [transportText, setTransportText] = useState(() => savedTransportText);
  const transportParsed = parseTransportText(transportText);
  const transportDirty = !transportParsed.ok
    ? transportText !== savedTransportText
    : !sameTransport(transportParsed.value, provider.transport);

  const current = provider.cost_multiplier_bps;
  const initial = modePercentFromBps(current);
  const [mode, setMode] = useState<AdjustmentMode>(initial.mode);
  const [percent, setPercent] = useState(initial.percent);
  const bps = bpsFromModePercent(mode, percent);
  const costValid = isAdjustmentValid(bps, PROVIDER_MAX_BPS);
  const costDirty = mode !== initial.mode || percent !== initial.percent;
  const costWriteDirty = costValid && bps !== current;

  /**
   * What this upstream account will take: two rate ceilings and a concurrency
   * cap.
   *
   * All three are held as text because an empty field is a state a number
   * cannot hold, and for the two ceilings empty is a real setting -- no
   * ceiling. Clearing one therefore has to be sent explicitly, which is what
   * the contract's `clear` list is for; omission means "leave it alone".
   */
  const savedRpm = provider.rate_limit_rpm == null ? "" : String(provider.rate_limit_rpm);
  const savedTpm = provider.rate_limit_tpm == null ? "" : String(provider.rate_limit_tpm);
  const savedConc = String(provider.max_concurrency);
  const [rpmText, setRpmText] = useState(savedRpm);
  const [tpmText, setTpmText] = useState(savedTpm);
  const [concText, setConcText] = useState(savedConc);
  const rpmValid = isBlankOrPositive(rpmText);
  const tpmValid = isBlankOrPositive(tpmText);
  // Concurrency has no blank state: there is always some number of simultaneous
  // calls past which an upstream stops answering.
  const concValid = positiveIntOf(concText) !== undefined;
  const capacityValid = rpmValid && tpmValid && concValid;
  const rpmDirty = rpmText.trim() !== savedRpm;
  const tpmDirty = tpmText.trim() !== savedTpm;
  const concDirty = concText.trim() !== savedConc;
  const capacityDirty = rpmDirty || tpmDirty || concDirty;

  const dirty = basicsDirty || headerDirty || transportDirty || costDirty || capacityDirty;
  const blocked =
    !basicsValid || Boolean(headerErr) || !transportParsed.ok || !costValid || !capacityValid;

  // Cleared on unmount: switching tabs is router navigation and unmounts this
  // component, and a dirty flag left behind on the page would block the next click
  // for no reason.
  useEffect(() => {
    onDirtyChange?.(dirty);
    return () => onDirtyChange?.(false);
  }, [dirty, onDirtyChange]);

  const discard = () => {
    setName(provider.name ?? "");
    // Including the vendor: leaving it out kept the panel dirty after a discard
    // and let the next save write the abandoned change -- which re-points every
    // organization credential matched to this provider.
    setVendor(provider.vendor);
    setBaseUrl(provider.base_url);
    setProtocols([...provider.protocols]);
    setRows(rowsFromMap(provider.headers));
    setTransportText(savedTransportText);
    setMode(initial.mode);
    setPercent(initial.percent);
    setRpmText(savedRpm);
    setTpmText(savedTpm);
    setConcText(savedConc);
  };

  // Clearing a ceiling travels in its own list: after JSON decoding, "sent as
  // null" and "not sent at all" are the same absent field, so emptying a box
  // and not touching it would arrive as the same request.
  const cleared: GatewayStaffTypes.GatewayProviderInputClearItem[] = [
    ...(rpmDirty && rpmText.trim() === "" ? (["rate_limit_rpm"] as const) : []),
    ...(tpmDirty && tpmText.trim() === "" ? (["rate_limit_tpm"] as const) : []),
  ];

  const submit = () => {
    if (blocked) {
      const sectionId = !basicsValid
        ? "provider-basics"
        : headerErr
          ? "provider-headers"
          : !transportParsed.ok
            ? "provider-transport"
            : !capacityValid
              ? "provider-capacity"
              : "provider-cost";
      const section = document.getElementById(sectionId);
      section?.scrollIntoView({ block: "start" });
      let control: HTMLElement | null = null;
      if (!basicsValid) {
        control =
          baseUrl.trim() === ""
            ? document.getElementById(`p-url-${provider.id}`)
            : !protocolsFitVendor
              ? document.getElementById(`p-vendor-${provider.id}`)
              : (section?.querySelector<HTMLElement>(
                  '[data-slot="checkbox-group-field"] [role="checkbox"]',
                ) ?? null);
      } else if (headerErr) {
        control = section?.querySelector<HTMLElement>('[aria-invalid="true"]') ?? null;
      } else if (!transportParsed.ok) {
        control = document.getElementById(`p-transport-${provider.id}`);
      } else if (!capacityValid) {
        const capacityId = !rpmValid
          ? `p-rpm-${provider.id}`
          : !tpmValid
            ? `p-tpm-${provider.id}`
            : `p-conc-${provider.id}`;
        control = document.getElementById(capacityId);
      } else if (!costValid) {
        control = document.getElementById(`provider-cost-${provider.id}`);
      }
      control?.focus({ preventScroll: true });
      return;
    }
    update.mutate(
      {
        providerId: provider.id,
        // Send **only the changed fields**: sending everything rewrites the
        // untouched ones with whatever is currently rendered, quietly reverting a
        // value someone else just changed. Omission means "keep the current value",
        // so not sending a field is how it is left alone.
        data: {
          ...(nameDirty ? { name } : {}),
          ...(baseUrlDirty ? { base_url: baseUrl.trim() } : {}),
          ...(protocolsDirty ? { protocols } : {}),
          ...(vendorDirty ? { vendor } : {}),
          ...(headerDirty ? { headers: nextHeaders } : {}),
          ...(transportDirty && transportParsed.ok ? { transport: transportParsed.value } : {}),
          ...(costWriteDirty ? { cost_multiplier_bps: bps } : {}),
          ...(rpmDirty && positiveIntOf(rpmText) !== undefined
            ? { rate_limit_rpm: positiveIntOf(rpmText) }
            : {}),
          ...(tpmDirty && positiveIntOf(tpmText) !== undefined
            ? { rate_limit_tpm: positiveIntOf(tpmText) }
            : {}),
          ...(concDirty ? { max_concurrency: positiveIntOf(concText) } : {}),
          ...(cleared.length > 0 ? { clear: cleared } : {}),
        },
      },
      {
        onSuccess: () => {
          toasts.add({ variant: "success", title: t("gwConfigSaved") });
          onSaved();
        },
      },
    );
  };

  return (
    <div className="space-y-6">
      {/* Narrowing the dialects to a set some route still relies on is refused with a
          conflict whose detail already names the route that blocked it — shown as-is,
          not rewritten. */}
      {update.isError && <Alert>{apiErrorMessage(update.error)}</Alert>}

      {/* A stack of cards: one card per independent object, with **each heading and
          description outside its card**. The three cards are still **one submit
          surface** — visual grouping and submit granularity are different things, and
          neither the per-field dirty check nor the single save button changed. */}
      <section id="provider-basics" className="scroll-mt-6 space-y-3">
        <SectionHeading as="h3">{t("gwSectionBasics")}</SectionHeading>
        <p className="text-base text-kumo-subtle">
          {t("gwProviderSlugFixed", { slug: provider.slug })}
        </p>
        <Card className="space-y-3">
          <Field
            label={t("gwVendorPick")}
            htmlFor={`p-vendor-${provider.id}`}
            hint={t("gwVendorPickEditHint")}
          >
            <Select
              value={vendor}
              aria-invalid={!protocolsFitVendor || undefined}
              onValueChange={(v) => setVendor(v ?? provider.vendor)}
              items={(vendors ?? []).map((v) => ({ value: v.slug, label: v.label }))}
            />
          </Field>
          <Field label={t("gwProviderName")} htmlFor={`p-name-${provider.id}`}>
            <Input
              id={`p-name-${provider.id}`}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Field>
          <Field
            label={t("gwColBaseUrl")}
            htmlFor={`p-url-${provider.id}`}
            error={baseUrl.trim() === "" ? t("required") : undefined}
          >
            <Input
              id={`p-url-${provider.id}`}
              value={baseUrl}
              required
              aria-invalid={baseUrl.trim() === "" || undefined}
              onChange={(e) => setBaseUrl(e.target.value)}
            />
          </Field>
          <CheckboxGroupField
            legend={t("gwColProtocols")}
            hint={t("gwEditProtocolsHint")}
            options={protocolItems.filter(
              (i) => !chosenVendor || chosenVendor.protocols.includes(i.value),
            )}
            value={protocols}
            required
            error={
              !protocolsFitVendor
                ? t("gwVendorProtocolMismatch")
                : protocols.length === 0
                  ? t("required")
                  : undefined
            }
            columns={2}
            onValueChange={(next) =>
              setProtocols(next as GatewayStaffTypes.GatewayProviderInputProtocolsItem[])
            }
          />
        </Card>
      </section>

      {/* Header mapping existed only in the server and in storage, with no way into
          it from the interface — so a provider that wants its key in a custom header
          rather than a bearer token, or that requires an attribution header, could
          only be onboarded by editing the database. Those are not edge cases; they
          are the first wall anyone hits connecting a second upstream. */}
      <section id="provider-headers" className="scroll-mt-6 space-y-3">
        <SectionHeading as="h3">{t("gwHdrProviderTitle")}</SectionHeading>
        <p className="text-base text-kumo-subtle">{t("gwHdrHint")}</p>
        <Card className="space-y-3">
          <HeaderMapEditor
            rows={rows}
            onChange={setRows}
            idPrefix={`p-${provider.id}`}
            disabled={update.isPending}
          />
          {headerErr && <Alert>{t(headerErr)}</Alert>}
        </Card>
      </section>

      {/* The transport profile: how to reach this upstream when the base URL and the
          protocol's defaults are not enough. Absent for most providers, and the only
          way to onboard the ones it is not absent for. */}
      <section id="provider-transport" className="scroll-mt-6 space-y-3">
        <SectionHeading as="h3">{t("gwTransportTitle")}</SectionHeading>
        <p className="text-base text-kumo-subtle">{t("gwTransportHint")}</p>
        <Card className="space-y-3">
          <Field label={t("gwTransportLabel")} htmlFor={`p-transport-${provider.id}`}>
            <Textarea
              id={`p-transport-${provider.id}`}
              rows={6}
              spellCheck={false}
              className="font-mono"
              value={transportText}
              disabled={update.isPending}
              aria-invalid={!transportParsed.ok || undefined}
              placeholder={TRANSPORT_PLACEHOLDER}
              onChange={(e) => setTransportText(e.target.value)}
            />
          </Field>
          {!transportParsed.ok && <Alert>{t("gwTransportNotJson")}</Alert>}
        </Card>
      </section>

      {/* What the upstream account will take. It belongs to the provider and not
          to a route because that is the shape the quota has: an account is rated
          at so many requests a minute across everything it serves. An account
          with a different quota is a different account, and therefore a second
          provider record. */}
      <section id="provider-capacity" className="scroll-mt-6 space-y-3">
        <SectionHeading as="h3">{t("gwProviderCapacity")}</SectionHeading>
        <p className="text-base text-kumo-subtle">{t("gwProviderCapacityHint")}</p>
        <Card className="space-y-3">
          <FormRow>
            <Field
              label={t("gwProviderRpm")}
              htmlFor={`p-rpm-${provider.id}`}
              hint={t("gwProviderRateHint")}
            >
              <Input
                id={`p-rpm-${provider.id}`}
                inputMode="numeric"
                value={rpmText}
                aria-invalid={!rpmValid || undefined}
                placeholder={t("orgRateLimitNone")}
                onChange={(e) => setRpmText(e.target.value)}
              />
            </Field>
            <Field
              label={t("gwProviderTpm")}
              htmlFor={`p-tpm-${provider.id}`}
              hint={t("gwProviderRateHint")}
            >
              <Input
                id={`p-tpm-${provider.id}`}
                inputMode="numeric"
                value={tpmText}
                aria-invalid={!tpmValid || undefined}
                placeholder={t("orgRateLimitNone")}
                onChange={(e) => setTpmText(e.target.value)}
              />
            </Field>
          </FormRow>
          <Field
            label={t("gwProviderConcurrency")}
            htmlFor={`p-conc-${provider.id}`}
            hint={t("gwProviderConcurrencyHint")}
          >
            <Input
              id={`p-conc-${provider.id}`}
              inputMode="numeric"
              value={concText}
              required
              aria-invalid={!concValid || undefined}
              onChange={(e) => setConcText(e.target.value)}
            />
          </Field>
          {!capacityValid && <Alert>{t("gwProviderCapacityInvalid")}</Alert>}
        </Card>
      </section>

      <section id="provider-cost" className="scroll-mt-6 space-y-3">
        <SectionHeading as="h3">{t("gwProviderCostScalar")}</SectionHeading>
        <p className="text-base text-kumo-subtle">{t("gwProviderCostScalarHint")}</p>
        <Card>
          <AdjustmentFields
            id={`provider-cost-${provider.id}`}
            mode={mode}
            onModeChange={setMode}
            percent={percent}
            onPercentChange={setPercent}
            bps={bps}
            valid={costValid}
          />
        </Card>
      </section>

      {/* The dock is rendered by AppShell outside the scrolling content. Keeping it
          mounted in every form state prevents layout shifts and makes dirty or
          invalid work visible from every section. */}
      <PageActionDock
        status={
          blocked ? t("gwInvalidChanges") : dirty ? t("gwUnsavedChanges") : t("gwConfigUpToDate")
        }
      >
        <Button variant="ghost" disabled={!dirty || update.isPending} onClick={discard}>
          {t("gwDiscardChanges")}
        </Button>
        <Button loading={update.isPending} disabled={!dirty || update.isPending} onClick={submit}>
          {t("save")}
        </Button>
      </PageActionDock>
    </div>
  );
}

// Managing one provider's keys.
//
// A key's plaintext appears once, in the request that creates it; it is encrypted on
// the way into storage and never read back, so the list shows a mask. That is
// deliberate: leaking a mask weakens nothing, whereas a key that can be read back
// will eventually be read.
export function KeyPanel({
  provider,
  onChanged,
}: {
  provider: GatewayStaffTypes.GatewayProvider;
  onChanged: () => void;
}) {
  const { t } = useI18n();
  const displayDate = useDisplayDate();
  const [keyCursor, setKeyCursor] = useState<string | undefined>(undefined);
  const [keyGeneration, setKeyGeneration] = useState(0);
  const keys = gatewayStaffApi.useListGatewayProviderKeys(
    provider.id,
    keyCursor ? { cursor: keyCursor } : undefined,
  );
  const { items: rows, nextCursor: keyNext } = useCursorList<GatewayStaffTypes.GatewayProviderKey>(
    keys,
    (k) => k.id,
    String(keyGeneration),
  );
  // 增删之后回第一页并清空缓存：useCursorList 按 id 去重且不替换已见的行（ADR-0185）。
  const resetKeyList = () => {
    setKeyCursor(undefined);
    setKeyGeneration((g) => g + 1);
  };
  const create = gatewayStaffApi.useCreateGatewayProviderKey();
  const remove = gatewayStaffApi.useDeleteGatewayProviderKey();
  const toasts = useKumoToastManager();

  const updateKey = gatewayStaffApi.useUpdateGatewayProviderKey();
  const testKey = gatewayStaffApi.useTestGatewayProvider();

  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");
  const [removingKey, setRemovingKey] = useState<{ id: string; name: string } | null>(null);
  const [togglingKey, setTogglingKey] = useState<{
    id: string;
    name: string;
    next: "active" | "disabled";
  } | null>(null);
  const [testingKey, setTestingKey] = useState<{ id: string; name: string } | null>(null);
  const [testModel, setTestModel] = useState("");
  const [testResult, setTestResult] = useState<GatewayStaffTypes.GatewayProviderTestResult | null>(
    null,
  );

  const refresh = () => {
    resetKeyList();
    void keys.refetch();
    onChanged();
  };

  return (
    <div className="space-y-6">
      {/* A failed list query has to say so. The test is **success**, not merely
          settled: a failed query's data is undefined as well, it has only stopped
          being pending — so `?? []` lets "the fetch failed" masquerade as "we fetched,
          and there is nothing". */}
      {keys.isError && <Alert>{apiErrorMessage(keys.error)}</Alert>}
      {create.isError && <Alert>{apiErrorMessage(create.error)}</Alert>}
      {remove.isError && <Alert>{apiErrorMessage(remove.error)}</Alert>}

      {/* A stack of cards. Laid out flat, the key table, the add-key form and the
          connectivity test were separated by a single top border each — and the first
          one had not even that. The table and the form are two halves of one object
          and share a card; the connectivity test is a different thing and gets its
          own. */}
      <section className="space-y-3">
        <SectionHeading>{t("gwKeys")}</SectionHeading>
        <Card className="space-y-4">
          <DataTable caption={t("gwKeys")}>
            <DataTable.Header>
              <DataTable.Row>
                <DataTable.Head>{t("gwColName")}</DataTable.Head>
                <DataTable.Head>{t("gwColMask")}</DataTable.Head>
                <DataTable.Head>{t("gwColStatus")}</DataTable.Head>
                <DataTable.Head>{t("gwColLastVerified")}</DataTable.Head>
                <DataTable.Head />
              </DataTable.Row>
            </DataTable.Header>
            <DataTable.Body>
              {rows.map((k) => (
                <DataTable.Row key={k.id}>
                  <DataTable.Cell>{k.name || "—"}</DataTable.Cell>
                  <DataTable.Cell className="font-mono">{k.secret_hint}</DataTable.Cell>
                  {/* An enum is mapped before it is rendered: `{k.status}` on its own
                      puts a raw wire identifier on screen, untranslated in every
                      language. Unknown values fall back to the raw string rather than
                      being swallowed — when the contract's domain grows, this should
                      surface it instead of rendering blank.
                      **Cooldown outranks status**: a key that is active but currently
                      cooling down has already been dropped from the routing
                      candidates, and calling it "active" would be untrue. */}
                  <DataTable.Cell>
                    {k.cooldown_until ? (
                      <StatusBadge tone="warning">
                        {t("gwKeyCoolingDown", { until: displayDate(k.cooldown_until) })}
                      </StatusBadge>
                    ) : (
                      <StatusBadge tone={k.status === "active" ? "success" : "neutral"}>
                        {keyStatusLabel(t, k.status)}
                      </StatusBadge>
                    )}
                  </DataTable.Cell>
                  <DataTable.Cell>
                    {k.last_verified_at ? (
                      <>
                        {displayDate(k.last_verified_at)}
                        {k.last_error && (
                          <span className="ml-2 text-kumo-danger">{k.last_error.slice(0, 80)}</span>
                        )}
                      </>
                    ) : (
                      <span className="text-kumo-subtle">{t("gwUnverified")}</span>
                    )}
                  </DataTable.Cell>
                  <DataTable.Cell>
                    {/* More than one button at the end of a row goes through the row
                        actions component: the button's own root is a block-level flex
                        box, so placing two side by side stacks them vertically, and
                        no amount of no-wrap classes changes that. */}
                    <RowActions>
                      {/* Rotating a key goes add new, verify new, disable old. Without
                          a per-row probe the test always used the first key in id
                          order, so the newly added one could never be verified. */}
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={k.status !== "active"}
                        onClick={() => setTestingKey({ id: k.id, name: k.name || k.secret_hint })}
                      >
                        {t("gwTest")}
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={updateKey.isPending}
                        onClick={() =>
                          setTogglingKey({
                            id: k.id,
                            name: k.name || k.secret_hint,
                            next: k.status === "active" ? "disabled" : "active",
                          })
                        }
                      >
                        {k.status === "active" ? t("gwDisable") : t("gwEnable")}
                      </Button>
                      <Button
                        size="sm"
                        variant="secondary-destructive"
                        onClick={() => setRemovingKey({ id: k.id, name: k.name || k.secret_hint })}
                      >
                        {t("gwDelete")}
                      </Button>
                    </RowActions>
                  </DataTable.Cell>
                </DataTable.Row>
              ))}
              {/* No empty state while pending. The empty text here asserts that a
                  provider without keys is never selected by the router — a claim about
                  routing behaviour, and a false one about a provider that does have
                  keys. `?? []` is exactly what lets "we have not looked yet"
                  masquerade as "we looked, and there is nothing". */}
              {keys.isPending ? (
                <DataTable.Row>
                  <DataTable.Cell colSpan={5}>
                    <LoadingState label={t("loading")} />
                  </DataTable.Cell>
                </DataTable.Row>
              ) : (
                !keys.isError &&
                rows.length === 0 && (
                  <DataTable.Row>
                    <DataTable.Cell colSpan={5}>
                      <InlineEmpty title={t("gwNoKeys")} description={t("gwNoKeysHint")} />
                    </DataTable.Cell>
                  </DataTable.Row>
                )
              )}
            </DataTable.Body>
          </DataTable>
          <LoadMoreButton
            onClick={keyNext ? () => setKeyCursor(keyNext) : undefined}
            pending={keys.isFetching}
            label={t("loadMore")}
          />

          <FormRow
            as="form"
            className="sm:grid-cols-2 lg:grid-cols-[minmax(10rem,1fr)_minmax(14rem,1fr)_auto]"
            onSubmit={(e) => {
              e.preventDefault();
              create.mutate(
                { providerId: provider.id, data: { name, secret } },
                {
                  onSuccess: () => {
                    setName("");
                    setSecret("");
                    // An immediate mutation always notifies: the new row appears in
                    // the table while the reader is still looking at the form.
                    toasts.add({ variant: "success", title: t("gwKeyAdded") });
                    refresh();
                  },
                },
              );
            }}
          >
            <FormRow.Item>
              <Field label={t("gwKeyNameOpt")} htmlFor={`k-name-${provider.id}`}>
                <Input
                  id={`k-name-${provider.id}`}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              <Field
                label={t("gwKeySecret")}
                htmlFor={`k-secret-${provider.id}`}
                hint={t("gwKeySecretHint")}
              >
                <Input
                  id={`k-secret-${provider.id}`}
                  type="password"
                  value={secret}
                  required
                  onChange={(e) => setSecret(e.target.value)}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Actions>
              <Button type="submit" loading={create.isPending} disabled={!secret}>
                {t("gwAddKey")}
              </Button>
            </FormRow.Actions>
          </FormRow>
        </Card>
      </section>

      {/* Disabling or enabling a key. Disabling removes it from the routing
          candidates, which changes where traffic goes, so it is confirmed. */}
      <ConfirmDialog
        open={togglingKey !== null}
        onOpenChange={(o) => !o && setTogglingKey(null)}
        destructive={togglingKey?.next === "disabled"}
        title={togglingKey?.next === "disabled" ? t("gwKeyDisableTitle") : t("gwKeyEnableTitle")}
        description={t(togglingKey?.next === "disabled" ? "gwKeyDisableBody" : "gwKeyEnableBody", {
          name: togglingKey?.name ?? "",
        })}
        confirmLabel={togglingKey?.next === "disabled" ? t("gwDisable") : t("gwEnable")}
        pending={updateKey.isPending}
        onConfirm={() => {
          if (!togglingKey) return;
          updateKey.mutate(
            {
              providerId: provider.id,
              keyId: togglingKey.id,
              data: { status: togglingKey.next },
            },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("gwKeyUpdated") });
                setTogglingKey(null);
                refresh();
              },
            },
          );
        }}
      />

      {/* The per-row probe. It still calls the upstream and still costs money, so it
          keeps the same confirmation the page uses elsewhere; and it asks for an
          upstream model name for the same reason the overview does — a probe has to
          know which model to call with. */}
      <FormDialog
        open={testingKey !== null}
        onOpenChange={(o) => {
          if (!o) {
            setTestingKey(null);
            setTestModel("");
            setTestResult(null);
            testKey.reset();
          }
        }}
        title={t("gwKeyTestTitle", { name: testingKey?.name ?? "" })}
        description={t("gwKeyTestHint")}
        error={testKey.isError ? apiErrorMessage(testKey.error) : undefined}
        submitLabel={testKey.isPending ? t("gwTesting") : t("gwTest")}
        submitDisabled={!testModel.trim() || testKey.isPending}
        pending={testKey.isPending}
        onSubmit={() => {
          if (!testingKey) return;
          testKey.mutate(
            {
              providerId: provider.id,
              data: { upstream_model: testModel.trim(), key_id: testingKey.id },
            },
            {
              onSuccess: (r) => {
                setTestResult(r);
                // The record is written on this row, so its "last verified" column
                // has to follow.
                refresh();
              },
            },
          );
        }}
      >
        <Field label={t("gwUpstreamModelLabel")} htmlFor="key-test-model">
          <Input
            id="key-test-model"
            value={testModel}
            required
            onChange={(e) => setTestModel(e.target.value)}
            placeholder="gpt-4o"
          />
        </Field>
        {testResult && <TestResult result={testResult} />}
      </FormDialog>

      <ConfirmDialog
        open={removingKey !== null}
        onOpenChange={(o) => !o && setRemovingKey(null)}
        title={t("gwKeyDeleteConfirmTitle")}
        description={t("gwKeyDeleteConfirmBody", { name: removingKey?.name ?? "" })}
        confirmLabel={t("gwDelete")}
        pending={remove.isPending}
        onConfirm={() => {
          if (!removingKey) return;
          remove.mutate(
            { providerId: provider.id, keyId: removingKey.id },
            {
              onSuccess: () => {
                setRemovingKey(null);
                toasts.add({ variant: "success", title: t("gwKeyDeleted") });
                refresh();
              },
            },
          );
        }}
      />
    </div>
  );
}

/** Rendering a connectivity test's result. The test itself lives on the overview
 *  face; this layer stays here and is exported to it, because drawing the result has
 *  nothing to do with which face ran it. */
export function TestResult({ result }: { result: GatewayStaffTypes.GatewayProviderTestResult }) {
  const { t } = useI18n();
  return (
    <>
      {result.ok ? (
        <div className="mt-2 text-base text-kumo-success">
          {t("gwTestPass")}
          {result.latency_ms != null && t("gwTestLatency", { ms: result.latency_ms })}
        </div>
      ) : (
        <div className="mt-2 text-base text-kumo-danger">
          <div>
            {t("gwTestFail")}
            {result.status_code != null && t("gwTestUpstream", { status: result.status_code })}
          </div>
          {/* The upstream's own words are the only clue distinguishing a bad
              credential from a bad model name; shown verbatim, never rewritten. */}
          {result.message && (
            <pre className="mt-1 overflow-x-auto rounded bg-kumo-tint p-2 text-base whitespace-pre-wrap">
              {result.message}
            </pre>
          )}
        </div>
      )}
      {/* The trace is available on success as well as failure: "it connected but the
          answer is wrong" is diagnosed from it too. */}
      {result.trace && <ProbeTrace trace={result.trace} />}
    </>
  );
}

// The full trace appears only where the server permits it, and the client hides the
// toggle unless the server says so. The key sits in the request headers in plaintext,
// and that is deliberate: "is the key I stored the same one actually being sent" can
// only be answered by seeing it. Which is why this carries a conspicuous warning
// rather than being displayed quietly.
function ProbeTrace({ trace }: { trace: GatewayStaffTypes.GatewayProbeTrace }) {
  const { t } = useI18n();
  return (
    <div className="mt-3 space-y-2 rounded-xl border border-kumo-warning-line bg-kumo-warning-tint p-2">
      <div className="text-base font-medium text-kumo-warning">⚠ {t("gwTraceWarn")}</div>
      <div className="font-mono text-[0.9em] break-all text-kumo-subtle">{trace.url}</div>
      <div>
        <div className="text-base font-medium">{t("gwTraceRequest")}</div>
        <pre className="mt-1 max-h-64 overflow-auto rounded bg-kumo-base p-2 text-base whitespace-pre-wrap">
          {trace.request}
        </pre>
      </div>
      <div>
        <div className="text-base font-medium">
          {t("gwTraceResponse")} · {trace.response_status}
        </div>
        <pre className="mt-1 max-h-64 overflow-auto rounded bg-kumo-base p-2 text-base whitespace-pre-wrap">
          {trace.response}
        </pre>
        {trace.truncated && (
          <div className="mt-1 text-base text-kumo-subtle">{t("gwTraceTruncated")}</div>
        )}
      </div>
    </div>
  );
}
