import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import {
  gatewayStaffApi,
  getResponseETag,
  type GatewayStaffTypes,
  apiErrorMessage,
} from "@fairlb/api-client";
import { type MessageKey, useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  Checkbox,
  ConfirmDialog,
  DataTable,
  Field,
  FormDialog,
  FormRow,
  InlineEmpty,
  Input,
  intSchema,
  LoadingState,
  PageHeader,
  RowActions,
  RowTitleLink,
  SectionHeading,
  Select,
  StatusBadge,
  useAdminTitle,
  validate,
} from "@fairlb/ui";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { AdjustmentEditor } from "./adjustment-editor";
import {
  HeaderMapEditor,
  type HeaderRow,
  headerRowsError,
  mapFromRows,
  rowsFromMap,
} from "./header-map";
import { adjustmentBps, type AdjustmentMode, DECIMAL_RATE, multiplyRate } from "./pricing-math";
import { protocolLabel, useProtocolItems } from "./providers-shared";

/** The four list prices are in the same order as on the detail page: one set of
 * quantities ordered differently in two places forces a reader to keep comparing. */
const RATE_KEYS = ["input", "output", "cache_read", "cache_write"] as const;
const RATE_LABELS: Record<(typeof RATE_KEYS)[number], string> = {
  input: "Input",
  output: "Output",
  cache_read: "Cache Read",
  cache_write: "Cache Write",
};

export const VISIBILITY_KEY: Record<string, MessageKey> = {
  public: "visibilityPublic",
  beta: "visibilityBeta",
  hidden: "visibilityHidden",
};

// `value` is the wire value and must stay lower case; `label` is for people and goes
// through the shared provider label.

export function GatewayModelsPage() {
  const { t } = useI18n();
  useAdminTitle(t("navGatewayModels"));
  return <ModelsContent />;
}

function ModelsContent() {
  const { t } = useI18n();
  const protocolItems = useProtocolItems();
  const toasts = useKumoToastManager();
  const navigate = useNavigate();
  const models = gatewayStaffApi.useListGatewayModels();
  const update = gatewayStaffApi.useUpdateGatewayModel();
  // Filters live in the URL. Held in component state they snapped back to the default
  // on reload, and "look at this batch of unpriced models" could not be sent to
  // anyone. Every other list page in the product already works this way.
  const urlSearch = useSearch({ strict: false }) as {
    q?: string;
    protocol?: string;
    status?: string;
    pricing?: string;
    routing?: string;
  };
  // 空态要说哪一种空：设了筛选才谈得上「放宽或清掉」，一条都没设时那句话是让人
  // 去清一个不存在的东西。
  const modelsFiltered = Object.values(urlSearch).some(Boolean);
  const search = urlSearch.q ?? "";
  const protocol = urlSearch.protocol ?? "all";
  // Three **orthogonal** axes, each its own filter. Squeezed into a single select
  // they could not express "disabled **and** unpriced" — which is the commonest
  // question asked while tidying a catalog. Lifecycle, pricing state and routing
  // state combine freely by nature.
  const status = urlSearch.status ?? "all";
  const pricing = urlSearch.pricing ?? "all";
  const routing = urlSearch.routing ?? "all";
  const setFilter = (patch: Record<string, string | undefined>) =>
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, ...patch }),
      replace: true,
    });
  const setSearch = (v: string) => setFilter({ q: v || undefined });
  const setProtocol = (v: string) => setFilter({ protocol: v === "all" ? undefined : v });
  const setStatus = (v: string) => setFilter({ status: v === "all" ? undefined : v });
  const setPricing = (v: string) => setFilter({ pricing: v === "all" ? undefined : v });
  const setRouting = (v: string) => setFilter({ routing: v === "all" ? undefined : v });
  const [creating, setCreating] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [batchOpen, setBatchOpen] = useState(false);
  // Enabling and disabling is the second level of the kill switch, so it is confirmed
  // rather than fired on a single click.
  const [togglingModel, setTogglingModel] = useState<GatewayStaffTypes.GatewayModel | null>(null);

  // Keep the page header on error: a failure should not cost the reader their sense
  // of which page they are on.
  if (models.isError)
    return (
      <div className="space-y-6">
        <PageHeader title={t("navGatewayModels")} description={t("staffGatewayModelsDesc")} />
        <Alert>{apiErrorMessage(models.error)}</Alert>
      </div>
    );
  // Both the pricing status and the negative-margin flag come from the contract, the
  // former as a three-valued enum. An intersection type layered on top of that was a
  // leftover from before the contract carried them, and it weakened the enum back into
  // a plain string.
  const data = models.data?.items ?? [];
  const filtered = data.filter((m) => {
    if (
      search.trim() &&
      !`${m.slug} ${m.display_name ?? ""}`.toLowerCase().includes(search.trim().toLowerCase())
    )
      return false;
    // A model owns no protocol; "configured on anthropic" means some enabled
    // route's provider speaks it, which is what `protocols` carries.
    if (protocol !== "all" && !m.protocols.includes(protocol)) return false;
    // The three axes intersect.
    if (status === "enabled" && !m.enabled) return false;
    if (status === "disabled" && m.enabled) return false;
    // The filter and the badge share one predicate: everything the "unpriced" filter
    // returns must carry an "unpriced" badge on its row. Judged separately, the two
    // would eventually disagree.
    if (pricing !== "all" && modelPricingState(m) !== pricing) return false;
    if (routing === "no_route" && (m.route_count ?? 0) > 0) return false;
    if (routing === "routed" && (m.route_count ?? 0) === 0) return false;
    return true;
  });
  const refresh = () => void models.refetch();

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("navGatewayModels")}
        description={t("staffGatewayModelsDesc")}
        actions={
          <div className="flex gap-2">
            <Button
              variant="outline"
              disabled={selectedIds.length === 0}
              onClick={() => setBatchOpen(true)}
            >
              {t("gwBatchAdjustment", { count: selectedIds.length })}
            </Button>
            <Button onClick={() => setCreating(true)}>{t("gwNewModel")}</Button>
          </div>
        }
      />
      {update.isError && <Alert>{apiErrorMessage(update.error)}</Alert>}

      <Card className="space-y-3">
        {/* A filter belongs to the table it filters, so the toolbar sits inside the
            list card rather than in a card of its own. The card has no heading — it
            would only repeat the page title — and the count stays at the right of the
            toolbar row. */}
        <FormRow className="sm:grid-cols-2 xl:grid-cols-[minmax(14rem,1fr)_11rem_11rem_11rem_11rem]">
          <FormRow.Item>
            <Field label={t("gwModelSearch")} htmlFor="model-search">
              <Input
                id="model-search"
                type="search"
                value={search}
                placeholder={t("gwModelSearchHint")}
                onChange={(e) => setSearch(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field label={t("gwColProtocol")}>
              <Select
                value={protocol}
                onValueChange={(v) => setProtocol(v ?? "all")}
                items={[{ value: "all", label: t("gwFilterAllProtocols") }, ...protocolItems]}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field label={t("gwColStatus")}>
              <Select
                value={status}
                onValueChange={(v) => setStatus(v ?? "all")}
                items={[
                  { value: "all", label: t("gwFilterAllStatuses") },
                  { value: "enabled", label: t("gwEnabled") },
                  { value: "disabled", label: t("gwDisable") },
                ]}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field label={t("gwPricingState")}>
              <Select
                value={pricing}
                onValueChange={(v) => setPricing(v ?? "all")}
                items={[
                  { value: "all", label: t("gwFilterAllPricing") },
                  { value: "unpriced", label: t("gwFilterMissingPrice") },
                  { value: "free", label: t("gwFilterFree") },
                ]}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field label={t("gwColRoutes")}>
              <Select
                value={routing}
                onValueChange={(v) => setRouting(v ?? "all")}
                items={[
                  { value: "all", label: t("gwFilterAllRouting") },
                  { value: "routed", label: t("gwFilterRouted") },
                  { value: "no_route", label: t("gwFilterNoRoute") },
                ]}
              />
            </Field>
          </FormRow.Item>
        </FormRow>
        <div className="flex items-center justify-end">
          {/* Same reason: while the query is pending this line would read "showing 0
              of 0", which is equally untrue. */}
          {!models.isPending && (
            <span className="text-base text-kumo-subtle">
              {t("gwModelCount", { shown: filtered.length, total: data.length })}
            </span>
          )}
        </div>
        <DataTable caption={t("navGatewayModels")}>
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head>
                <Checkbox
                  aria-label={t("gwSelectAllModels")}
                  checked={
                    filtered.length > 0 && filtered.every((model) => selectedIds.includes(model.id))
                  }
                  onCheckedChange={(checked) =>
                    setSelectedIds(
                      checked === true
                        ? Array.from(
                            new Set([...selectedIds, ...filtered.map((model) => model.id)]),
                          )
                        : selectedIds.filter((id) => !filtered.some((model) => model.id === id)),
                    )
                  }
                />
              </DataTable.Head>
              <DataTable.Head>{t("gwColSlug")}</DataTable.Head>
              <DataTable.Head>{t("gwColVisibility")}</DataTable.Head>
              <DataTable.Head>{t("gwColRoutes")}</DataTable.Head>
              <DataTable.Head>{t("gwPricingState")}</DataTable.Head>
              <DataTable.Head>{t("gwColCaps")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwColPriceIn")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwColPriceOut")}</DataTable.Head>
              <DataTable.Head />
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            {filtered.map((m) => (
              <DataTable.Row key={m.id} interactive>
                <DataTable.Cell>
                  <Checkbox
                    aria-label={t("gwSelectModel", { slug: m.slug })}
                    checked={selectedIds.includes(m.id)}
                    onCheckedChange={(checked) =>
                      setSelectedIds(
                        checked === true
                          ? [...selectedIds, m.id]
                          : selectedIds.filter((id) => id !== m.id),
                      )
                    }
                  />
                </DataTable.Cell>
                {/* `relative` is what lets the row title link cover the whole cell,
                    and it is a real link: middle-click and copy-link-address both have
                    to work. */}
                <DataTable.Cell className="relative">
                  <span className="font-mono">
                    <RowTitleLink to="/gateway/models/$modelId" params={{ modelId: m.id }}>
                      {m.slug}
                    </RowTitleLink>
                    {m.is_free && <span className="ml-2 text-kumo-subtle">{t("gwFreeTag")}</span>}
                  </span>
                  {/* The display name was searchable but invisible: the filter matched
                      on it while the list never showed it. The identity column is now
                      the primary identifier as a link with the name on a second line,
                      like the provider list. */}
                  {m.display_name && <div className="text-kumo-subtle">{m.display_name}</div>}
                </DataTable.Cell>
                <DataTable.Cell>
                  {t(VISIBILITY_KEY[m.visibility] ?? "visibilityHidden")}
                </DataTable.Cell>
                <DataTable.Cell>{m.route_count ?? 0}</DataTable.Cell>
                <DataTable.Cell>
                  <ModelStateBadges model={m} />
                </DataTable.Cell>
                <DataTable.Cell className="font-mono">
                  {/* What probes have verified on the enabled routes -- the same
                      set the public catalog publishes. A route with nothing
                      verified yet shows nothing here, not a declaration. */}
                  {m.endpoints.length > 0 ? (
                    m.endpoints.join(", ")
                  ) : (
                    <span className="text-kumo-warning">
                      {(m.route_count ?? 0) > 0 ? t("gwNothingVerified") : t("gwNoRoute")}
                    </span>
                  )}
                </DataTable.Cell>
                <DataTable.Cell className="text-right font-mono">
                  {m.public_rates?.input ?? "—"}
                </DataTable.Cell>
                <DataTable.Cell className="text-right font-mono">
                  {m.public_rates?.output ?? "—"}
                </DataTable.Cell>
                {/* The end of a row carries only real actions; opening the detail page
                    is the job of the slug at the head of it. */}
                <DataTable.Cell className="text-right whitespace-nowrap">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => setTogglingModel(m)}
                    disabled={update.isPending}
                  >
                    {m.enabled ? t("gwDisable") : t("gwEnable")}
                  </Button>
                </DataTable.Cell>
              </DataTable.Row>
            ))}
            {/* No empty state while pending: "the catalog has no models" and "we have
                not looked yet" are different facts, and defaulting the data to an empty
                array lets the first speak for the second. */}
            {models.isPending ? (
              <DataTable.Row>
                <DataTable.Cell colSpan={9}>
                  <LoadingState label={t("loading")} />
                </DataTable.Cell>
              </DataTable.Row>
            ) : (
              filtered.length === 0 && (
                <DataTable.Row>
                  <DataTable.Cell colSpan={9}>
                    <InlineEmpty
                      title={t("gwNoModels")}
                      description={modelsFiltered ? t("emptyClearFilters") : undefined}
                    />
                  </DataTable.Cell>
                </DataTable.Row>
              )
            )}
          </DataTable.Body>
        </DataTable>
      </Card>

      <CreateModelDialog open={creating} onOpenChange={setCreating} onCreated={refresh} />
      <BatchAdjustmentDialog
        open={batchOpen}
        onOpenChange={setBatchOpen}
        models={data.filter((model) => selectedIds.includes(model.id))}
        onSaved={() => {
          // Saving takes effect immediately; there is no publish step to send anyone
          // to afterwards.
          setSelectedIds([]);
          refresh();
        }}
      />

      <ConfirmDialog
        open={togglingModel !== null}
        onOpenChange={(o) => !o && setTogglingModel(null)}
        destructive={togglingModel?.enabled ?? true}
        title={togglingModel?.enabled ? t("gwDisableConfirmTitle") : t("gwEnableConfirmTitle")}
        description={
          togglingModel?.enabled
            ? t("gwDisableConfirmBody", { slug: togglingModel?.slug ?? "" })
            : t("gwEnableConfirmBody", { slug: togglingModel?.slug ?? "" })
        }
        confirmLabel={togglingModel?.enabled ? t("gwDisable") : t("gwEnable")}
        pending={update.isPending}
        onConfirm={() => {
          if (!togglingModel) return;
          update.mutate(
            { modelId: togglingModel.id, data: { enabled: !togglingModel.enabled } },
            {
              onSuccess: () => {
                toasts.add({
                  variant: "success",
                  title: togglingModel.enabled ? t("gwDisabledDone") : t("gwEnabledDone"),
                });
                setTogglingModel(null);
                refresh();
              },
            },
          );
        }}
      />
    </div>
  );
}

function BatchAdjustmentDialog({
  open,
  onOpenChange,
  models,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  models: GatewayStaffTypes.GatewayModel[];
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [mode, setMode] = useState<"original" | "discount" | "markup">("original");
  const [percent, setPercent] = useState("0");
  // Saving takes effect immediately, so a bulk price change requires a reason as
  // well.
  const [reason, setReason] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const hundredths = /^\d+(\.\d{1,2})?$/.test(percent)
    ? Math.round(Number(percent) * 100)
    : Number.NaN;
  const multiplierBps =
    mode === "original" ? 10_000 : mode === "discount" ? 10_000 - hundredths : 10_000 + hundredths;
  const valid =
    Number.isInteger(multiplierBps) &&
    multiplierBps >= 1 &&
    multiplierBps <= 100_000 &&
    reason.trim() !== "";

  const apply = async () => {
    if (!valid || models.length === 0) return;
    setPending(true);
    setError(null);
    try {
      for (const model of models) {
        const cur = await gatewayStaffApi.getGatewayModelPricing(model.id);
        // The bulk edit changes **the selling multiplier** and nothing else, writing
        // the rest back unchanged: a model with no price must not have one conjured
        // into existence here.
        if (!cur.priced || !cur.official_rates || !cur.source_name) {
          throw new Error(t("gwBatchMissingPricing", { slug: model.slug }));
        }
        const checkedAt = cur.checked_at;
        if (!checkedAt) throw new Error(t("gwBatchMissingProvenance", { slug: model.slug }));
        const getUrl = gatewayStaffApi.getGetGatewayModelPricingUrl(model.id);
        const saveUrl = gatewayStaffApi.getSaveGatewayModelPricingUrl(model.id);
        const etag = getResponseETag(saveUrl, getUrl);
        if (!etag) throw new Error(t("gwBatchConflictReload", { slug: model.slug }));
        await gatewayStaffApi.saveGatewayModelPricing(
          model.id,
          {
            billing_mode: cur.billing_mode ?? "paid",
            official_rates: cur.official_rates,
            adjustment: { multiplier_bps: multiplierBps },
            source_name: cur.source_name,
            ...(cur.source_url ? { source_url: cur.source_url } : {}),
            checked_at: checkedAt,
            reason: reason.trim(),
            ...(cur.dimension_rates ? { dimension_rates: cur.dimension_rates } : {}),
            ...(cur.tool_rates ? { tool_rates: cur.tool_rates } : {}),
          },
          { headers: { "If-Match": etag } },
        );
      }
      onOpenChange(false);
      onSaved();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("gwBatchFailed"));
    } finally {
      setPending(false);
    }
  };

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title={t("gwBatchAdjustment", { count: models.length })}
      description={t("gwBatchAdjustmentHint")}
      error={error}
      submitLabel={t("gwCreateBatchDrafts")}
      submitDisabled={!valid || models.length === 0}
      pending={pending}
      onSubmit={() => void apply()}
    >
      <div className="flex max-h-32 flex-wrap gap-1.5 overflow-y-auto">
        {models.map((model) => (
          <StatusBadge key={model.id} tone="neutral">
            {model.slug}
          </StatusBadge>
        ))}
      </div>
      <FormRow className="sm:grid-cols-2">
        <FormRow.Item>
          <Field label={t("gwPriceAdjustment")}>
            <Select
              value={mode}
              onValueChange={(value) => setMode((value as typeof mode) ?? "original")}
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
            htmlFor="batch-adjustment-percent"
            error={valid ? undefined : t("gwAdjustmentInvalid")}
          >
            <div className="relative">
              <Input
                id="batch-adjustment-percent"
                disabled={mode === "original"}
                inputMode="decimal"
                value={mode === "original" ? "0" : percent}
                onChange={(event) => setPercent(event.target.value)}
              />
              <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-kumo-subtle">
                %
              </span>
            </div>
          </Field>
        </FormRow.Item>
      </FormRow>
      {/* With saving immediate, a bulk price change requires a reason too: the audit
          log is the only source of the previous values when rolling one back. */}
      <Field label={t("gwPriceChangeReason")} htmlFor="batch-reason" hint={t("orgReasonRequired")}>
        <Input
          id="batch-reason"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
      </Field>
      <Alert variant="warning">{t("gwBatchAppliesImmediately")}</Alert>
    </FormDialog>
  );
}

/**
 * The single place a model's current state is rendered.
 *
 * Collecting it here is not about saving code, it is that **one fact may have only one
 * way of being said**. The negative-margin signal, for instance, is computed on the
 * server and used to be rendered only by the list — so "this model is being sold at a
 * loss" was invisible on the very page that most needed to say it.
 *
 * Sharing one component between both places also means the next signal cannot be added
 * to only one of them.
 */
export function ModelStateBadges({ model }: { model: GatewayStaffTypes.GatewayModel }) {
  const { t } = useI18n();
  return (
    <div className="flex flex-wrap gap-1.5">
      {/* Enabled state comes first: it has more consequence than pricing state —
          **a disabled model serves nothing at all**, while unpriced only refuses a
          class of requests. As grey text trailing the slug, it was visually outweighed
          on the same line by a red "unpriced" badge: the most consequential state
          encoded the most weakly. */}
      {!model.enabled && <StatusBadge tone="warning">{t("gwDisabledTag")}</StatusBadge>}
      <PricingState model={model} />
      {/* Directly after the pricing state, because it qualifies it: this price
          exists, and nobody has compared it with the vendor's own list. It is the
          state a bulk import leaves behind, several hundred rows at a time, and
          without it the *list* cannot tell a rate somebody agreed to charge from
          one a dataset suggested — the model's page could, through an empty
          checked-on date, but only one model at a time and only if you go and
          look. */}
      {model.price_verified === false && (
        <StatusBadge tone="warning">{t("gwUnverifiedTag")}</StatusBadge>
      )}
    </div>
  );
}

/**
 * The **single** predicate for "does this model have a price", shared by the list
 * filter and the row badge.
 *
 * It reads the server's pricing status and nothing else.
 *
 * Testing the rate object as well — "no rates, or the status says unpriced" — is
 * **one question with two predicates**, and they agree only when the server happens to
 * populate both fields. The overview page is where they came apart: there the record
 * comes from a single-record read that sends the rates only when the model is priced,
 * so one card carried both "billing mode: usage based" and "unpriced, service
 * refused".
 *
 * The server's definition is whether a pricing row exists. Recomputing that here would
 * only create a second definition. The local fallback applies solely when the status
 * is absent — a pricing service that is not wired up — and treating "unknown" as
 * "none" is the conservative side to fail on.
 */
export function modelPricingState(
  model: GatewayStaffTypes.GatewayModel,
): "free" | "active" | "unpriced" {
  if (model.is_free || model.pricing_status === "free") return "free";
  if (model.pricing_status != null)
    return model.pricing_status === "active" ? "active" : "unpriced";
  return model.public_rates != null ? "active" : "unpriced";
}

function PricingState({ model }: { model: GatewayStaffTypes.GatewayModel }) {
  const { t } = useI18n();
  const state = modelPricingState(model);
  if (state === "free") return <StatusBadge tone="neutral">{t("gwFreeTag")}</StatusBadge>;
  return state === "active" ? (
    <StatusBadge tone="success">{t("gwPricingActive")}</StatusBadge>
  ) : (
    <StatusBadge tone="danger">{t("gwUnpricedTag")}</StatusBadge>
  );
}

/**
 * The create dialog collects four sections: identity, catalog attributes, pricing, and
 * an optional first provider.
 *
 * The four attributes — display name, visibility, context window, maximum output —
 * were always accepted by the create request; the dialog simply did not collect them
 * and the detail page had no way to edit them either, which made them a black hole:
 * unset at creation and unchangeable afterwards.
 *
 * **Pricing used to be kept out**, on the grounds that it was inherently a three-step
 * flow — draft, impact preview, publish — needing the highest privilege to publish.
 * With those three collapsed into one save, that reason no longer holds: pricing is
 * just another chained sub-resource, and it is **the one that blocks enabling**. A
 * model with no price fails closed, so creating one without a price only ever produces
 * a half-finished record. The advanced dimensional and tool prices stay on the detail
 * page, because they do not decide whether a model can go live.
 *
 * The four list prices are all-or-nothing, which the server enforces. Leaving them
 * empty means "create the catalog entry now, price it later" — still legal, and
 * completed from the focused pricing page after landing.
 */
function CreateModelDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
}) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const toasts = useKumoToastManager();
  const create = gatewayStaffApi.useCreateGatewayModel();
  const providers = gatewayStaffApi.useListGatewayProviders();
  const [slug, setSlug] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [visibility, setVisibility] =
    useState<GatewayStaffTypes.GatewayModelInputVisibility>("public");
  const [contextWindow, setContextWindow] = useState("");
  const [maxOutput, setMaxOutput] = useState("");
  const [providerId, setProviderId] = useState("");
  const [upstreamModel, setUpstreamModel] = useState("");
  const [chaining, setChaining] = useState(false);
  const [rates, setRates] = useState<GatewayStaffTypes.DraftTokenRatesUSDPerM>({
    input: null,
    output: null,
    cache_read: null,
    cache_write: null,
  });
  const [adjustmentMode, setAdjustmentMode] = useState<AdjustmentMode>("original");
  const [adjustmentPercent, setAdjustmentPercent] = useState("0");
  const [sourceName, setSourceName] = useState("");

  const errContext = contextWindow ? validate(intSchema, contextWindow) : undefined;
  const errMaxOutput = maxOutput ? validate(intSchema, maxOutput) : undefined;
  const multiplier = adjustmentBps(adjustmentMode, adjustmentPercent);
  // The four list prices are all present or all empty — **the partial state is the
  // only one worth blocking**: the server refuses it, and its error lands at the top of
  // the dialog, far from the field that caused it.
  const filledRates = RATE_KEYS.filter((key) => (rates[key] ?? "").trim() !== "");
  const badRate = filledRates.find((key) => !DECIMAL_RATE.test((rates[key] ?? "").trim()));
  const pricingPartial = filledRates.length > 0 && filledRates.length < RATE_KEYS.length;
  const pricingReady = filledRates.length === RATE_KEYS.length && !badRate && multiplier != null;
  const pricingBlocked =
    pricingPartial || Boolean(badRate) || (filledRates.length > 0 && multiplier == null);
  // Every provider is a candidate: a model owns no protocol, so there is no
  // dialect to match. The route is probed on whatever its provider speaks.
  const provList = providers.data?.items ?? [];
  const picked = provList.find((p) => p.id === providerId);
  const routeReady = Boolean(providerId && upstreamModel.trim());

  const reset = () => {
    setSlug("");
    setDisplayName("");
    setVisibility("public");
    setContextWindow("");
    setMaxOutput("");
    setProviderId("");
    setUpstreamModel("");
    setRates({ input: null, output: null, cache_read: null, cache_write: null });
    setAdjustmentMode("original");
    setAdjustmentPercent("0");
    setSourceName("");
    create.reset();
  };

  const finish = (modelId: string, destination: "overview" | "pricing" | "routes") => {
    onOpenChange(false);
    reset();
    onCreated();
    void navigate(
      destination === "pricing"
        ? { to: "/gateway/models/$modelId/pricing", params: { modelId } }
        : {
            to: "/gateway/models/$modelId",
            params: { modelId },
            hash: destination === "routes" ? "model-routes" : "",
          },
    );
  };

  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        // The dialog cannot be closed while the second request is in flight: by then
        // the model already exists.
        if (chaining) return;
        onOpenChange(next);
        if (!next) reset();
      }}
      size="lg"
      title={t("gwNewModel")}
      description={t("gwCreateModelDialogHint")}
      error={create.isError ? apiErrorMessage(create.error) : undefined}
      submitLabel={t("gwCreateAndConfigure")}
      submitDisabled={
        !slug.trim() || Boolean(errContext) || Boolean(errMaxOutput) || pricingBlocked
      }
      pending={create.isPending || chaining}
      onSubmit={() =>
        create.mutate(
          {
            data: {
              slug: slug.trim(),
              enabled: false,
              visibility,
              ...(displayName.trim() ? { display_name: displayName.trim() } : {}),
              ...(contextWindow ? { context_window: Number(contextWindow) } : {}),
              ...(maxOutput ? { max_output_tokens: Number(maxOutput) } : {}),
            },
          },
          {
            onSuccess: (created) => {
              if (!pricingReady && !routeReady) {
                // A missing price is the first reason a model fails closed, and the
                // next step is the focused pricing page.
                finish(created.id, "pricing");
                return;
              }
              setChaining(true);
              // Called directly rather than through the hook: a global failure
              // notification is attached to the mutation cache, so the hook would raise
              // two notices about one event, and the global one omits "the model was
              // created". The navigation happens once, in the finally branch: split
              // across then and catch, anything thrown inside then would be caught by
              // the same chain's catch and reported as a failure that never happened.
              let pricingOk = true;
              let routeOk = true;
              void (async () => {
                // **The two sub-resources succeed or fail independently**: a failed
                // price write must not discard an already configured route, or vice
                // versa. The model itself exists by now, and rolling that back is not
                // something this code can do.
                if (pricingReady) {
                  try {
                    await gatewayStaffApi.saveGatewayModelPricing(created.id, {
                      billing_mode: "paid",
                      official_rates: rates,
                      adjustment: { multiplier_bps: multiplier ?? 10_000 },
                      source_name: sourceName.trim(),
                      checked_at: new Date().toISOString(),
                      // The reason for an initial price is this sentence, not a copy of
                      // the previous one — there is no previous one. It goes through the
                      // message catalog rather than being hard-coded: an audit log is
                      // read by people, in the language of whoever acted.
                      reason: t("gwInitialPricingReason"),
                    });
                  } catch (error: unknown) {
                    pricingOk = false;
                    toasts.add({
                      variant: "error",
                      title: t("gwModelCreatedPricingFailed"),
                      description: apiErrorMessage(error),
                    });
                  }
                }
                if (routeReady) {
                  try {
                    await gatewayStaffApi.createGatewayRoute(created.id, {
                      provider_id: providerId,
                      provider_model_id: upstreamModel.trim(),
                    });
                  } catch (error: unknown) {
                    routeOk = false;
                    toasts.add({
                      variant: "error",
                      title: t("gwModelCreatedRouteFailed"),
                      description: apiErrorMessage(error),
                    });
                  }
                }
              })().finally(() => {
                setChaining(false);
                // Land on whichever page still has work outstanding, since that is
                // where the retry is. Only when everything succeeded does it land on the
                // overview, where the enable button in the header is the next step.
                if (!routeOk) finish(created.id, "routes");
                else if (!pricingOk || !pricingReady) finish(created.id, "pricing");
                else finish(created.id, "overview");
              });
            },
          },
        )
      }
    >
      <SectionHeading level="sub" as="h3">
        {t("gwSectionIdentity")}
      </SectionHeading>
      <Field label={t("gwSlugLabel")} htmlFor="m-slug" hint={t("gwSlugHint")}>
        <Input
          id="m-slug"
          value={slug}
          autoFocus
          required
          onChange={(e) => setSlug(e.target.value)}
        />
      </Field>
      <SectionHeading level="sub" as="h3">
        {t("gwSectionAttributes")}
      </SectionHeading>
      <Field label={t("gwDisplayName")} htmlFor="m-display" hint={t("gwDisplayNameHint")}>
        <Input
          id="m-display"
          value={displayName}
          onChange={(e) => setDisplayName(e.target.value)}
        />
      </Field>
      <Select
        label={t("gwColVisibility")}
        description={t("gwVisibilityHint")}
        value={visibility as string}
        onValueChange={(v) =>
          setVisibility((v ?? "public") as GatewayStaffTypes.GatewayModelInputVisibility)
        }
        items={Object.keys(VISIBILITY_KEY).map((value) => ({
          value,
          label: t(VISIBILITY_KEY[value] ?? "visibilityHidden"),
        }))}
      />
      <FormRow className="sm:grid-cols-2">
        <FormRow.Item>
          <Field
            label={t("gwContextWindow")}
            htmlFor="m-ctx"
            error={errContext && t(errContext as MessageKey)}
          >
            <Input
              id="m-ctx"
              value={contextWindow}
              inputMode="numeric"
              onChange={(e) => setContextWindow(e.target.value)}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field
            label={t("gwMaxOutputTokens")}
            htmlFor="m-maxout"
            hint={t("gwMaxOutputHint")}
            error={errMaxOutput && t(errMaxOutput as MessageKey)}
          >
            <Input
              id="m-maxout"
              value={maxOutput}
              inputMode="numeric"
              onChange={(e) => setMaxOutput(e.target.value)}
            />
          </Field>
        </FormRow.Item>
      </FormRow>

      <SectionHeading level="sub" as="h3">
        {t("gwSectionPricing")}
      </SectionHeading>
      <p className="text-base text-kumo-subtle">{t("gwCreatePricingHint")}</p>
      <FormRow className="sm:grid-cols-2">
        {RATE_KEYS.map((key) => {
          const raw = rates[key] ?? "";
          const invalid = raw.trim() !== "" && !DECIMAL_RATE.test(raw.trim());
          return (
            <FormRow.Item key={key}>
              <Field
                label={`${RATE_LABELS[key]} ${t("gwOfficialPrice")}`}
                htmlFor={`m-rate-${key}`}
                error={invalid ? t("gwRateInvalid") : undefined}
                // The published price sits under each field: once the multiplier is
                // anything but list price, that is the number actually being decided.
                hint={
                  !invalid && raw.trim() !== "" && multiplier != null && multiplier !== 10_000
                    ? `${t("gwPublicPrice")}: $${multiplyRate(raw.trim(), multiplier)}`
                    : undefined
                }
              >
                <div className="relative">
                  <span className="pointer-events-none absolute inset-y-0 left-3 flex items-center text-kumo-subtle">
                    $
                  </span>
                  <Input
                    id={`m-rate-${key}`}
                    className="pl-7 font-mono"
                    inputMode="decimal"
                    value={raw}
                    onChange={(e) => setRates({ ...rates, [key]: e.target.value || null })}
                  />
                </div>
              </Field>
            </FormRow.Item>
          );
        })}
      </FormRow>
      <FormRow className="sm:grid-cols-2">
        <AdjustmentEditor
          inputId="m-adjustment-percent"
          mode={adjustmentMode}
          percent={adjustmentPercent}
          onChange={(mode, percent) => {
            setAdjustmentMode(mode);
            setAdjustmentPercent(percent);
          }}
        />
      </FormRow>
      <Field
        label={t("gwSourceName")}
        htmlFor="m-source-name"
        hint={t("gwSourceNameCreateHint")}
        error={
          filledRates.length > 0 && sourceName.trim() === "" ? t("gwSourceNameRequired") : undefined
        }
      >
        <Input
          id="m-source-name"
          value={sourceName}
          onChange={(e) => setSourceName(e.target.value)}
        />
      </Field>
      {/* A partly filled set is the **only** shape worth blocking: the server's
          completeness constraint refuses it, and that error lands at the top of the
          dialog, far from the field that caused it. All empty is a legitimate "price it
          later". */}
      {pricingPartial && <Alert variant="warning">{t("gwCreatePricingPartial")}</Alert>}

      <SectionHeading level="sub" as="h3">
        {t("gwSectionFirstRoute")}
      </SectionHeading>
      <p className="text-base text-kumo-subtle">{t("gwFirstRouteHint")}</p>
      <Select
        label={t("gwColProvider")}
        value={providerId}
        onValueChange={(v) => setProviderId(v ?? "")}
        items={provList.map((p) => ({
          value: p.id,
          label: `${p.slug} (${p.protocols.map((f) => t(protocolLabel(f))).join(" + ")})`,
        }))}
      />
      {/* A provider with no key is not blocked: it can still serve customers who bring
          their own, so refusing here would be the wrong answer. */}
      {picked && (picked.key_count ?? 0) === 0 && (
        <p className="text-base text-kumo-danger">{t("gwFirstRouteNoKeys")}</p>
      )}
      <Field label={t("gwColUpstreamModel")} htmlFor="m-upstream" hint={t("gwUpstreamHint")}>
        <Input
          id="m-upstream"
          value={upstreamModel}
          onChange={(e) => setUpstreamModel(e.target.value)}
        />
      </Field>
      <p className="text-base text-kumo-subtle">{t("gwRouteProbedHint")}</p>

      <Alert variant="info">{t("gwCreateModelDisabledNote")}</Alert>
    </FormDialog>
  );
}

// Managing a model's upstream configuration.
//
// This is the easiest thing in the catalog to get wrong and the most consequential:
// a model's advertised capabilities are the union of what probes have verified on
// its enabled routes. Nothing is declared here -- a route is probed on every
// endpoint of the protocols its provider speaks -- so capabilities follow what
// actually serves the traffic, not what the model's own metadata claims.
export function RoutePanel({
  model,
  onChanged,
}: {
  model: GatewayStaffTypes.GatewayModel;
  onChanged: () => void;
}) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const routes = gatewayStaffApi.useListGatewayRoutes(model.id);
  const providers = gatewayStaffApi.useListGatewayProviders();
  const create = gatewayStaffApi.useCreateGatewayRoute();
  const update = gatewayStaffApi.useUpdateGatewayRoute();
  const remove = gatewayStaffApi.useDeleteGatewayRoute();
  // The third level of the kill switch, confirmed like the others.
  const [togglingRoute, setTogglingRoute] = useState<GatewayStaffTypes.GatewayRoute | null>(null);

  const [providerId, setProviderId] = useState("");
  const [upstreamModel, setUpstreamModel] = useState("");
  // Priority and weight are collected here. Left to the server's defaults and shown
  // nowhere, the two knobs that make multi-provider failover work were unreachable:
  // primary and backup could only be weighted equally and picked at random, and
  // changing that meant calling the API by hand.
  const [priority, setPriority] = useState("100");
  const [weight, setWeight] = useState("1");
  // Editing happens in a dialog: as an expanding row inside the table it pushed every
  // following route out of the way.
  const [editingRoute, setEditingRoute] = useState<GatewayStaffTypes.GatewayRoute | null>(null);
  const [removingRoute, setRemovingRoute] = useState<{ id: string; label?: string } | null>(null);

  const errPriority = validate(intSchema, priority);
  const errWeight = validate(intSchema, weight);

  const refresh = () => {
    void routes.refetch();
    onChanged();
  };
  const rows = routes.data?.items ?? [];
  // Every provider is a candidate: a model owns no protocol, so there is no
  // dialect to match. The same slug wired to an openai-only provider and to an
  // anthropic-only one is reachable on both surfaces. The label says what each
  // provider speaks, which is what the route will be probed on.
  const provList = providers.data?.items ?? [];

  return (
    <div className="space-y-3">
      <SectionHeading as="h3">{t("gwRoutes")}</SectionHeading>
      {/* A failed list query has to say so, or `?? []` lets it masquerade as "no
          routes are configured". */}
      {routes.isError && <Alert>{apiErrorMessage(routes.error)}</Alert>}
      {create.isError && <Alert>{apiErrorMessage(create.error)}</Alert>}
      {update.isError && <Alert>{apiErrorMessage(update.error)}</Alert>}
      {remove.isError && <Alert>{apiErrorMessage(remove.error)}</Alert>}

      <DataTable caption={t("gwRoutes")}>
        <DataTable.Header>
          <DataTable.Row>
            <DataTable.Head>{t("gwColProvider")}</DataTable.Head>
            <DataTable.Head>{t("gwColUpstreamModel")}</DataTable.Head>
            <DataTable.Head>{t("gwColEndpoints")}</DataTable.Head>
            <DataTable.Head className="text-right">{t("gwColPriority")}</DataTable.Head>
            <DataTable.Head className="text-right">{t("gwColWeight")}</DataTable.Head>
            {/* Headers and per-provider limits used to appear only in an "advanced"
                card, as the words "configured" or "inherited" — a card offering neither
                new information nor an action. They belong in this table: "what does this
                route override" is the same layer of fact as its priority and weight. */}
            <DataTable.Head>{t("gwRouteOverrides")}</DataTable.Head>
            <DataTable.Head>{t("gwColStatus")}</DataTable.Head>
            <DataTable.Head />
          </DataTable.Row>
        </DataTable.Header>
        <DataTable.Body>
          {rows.map((r) => (
            <DataTable.Row key={r.id}>
              <DataTable.Cell className="font-mono">
                {r.provider_slug ?? r.provider_id.slice(0, 8)}
              </DataTable.Cell>
              <DataTable.Cell className="font-mono">{r.provider_model_id}</DataTable.Cell>
              <DataTable.Cell className="font-mono">
                {/* One badge per endpoint of each protocol the provider speaks —
                      the route's only capability record. Green means "a call went
                      through", red "the upstream said no"; nothing here was
                      declared. Image endpoints staying unverified is by design:
                      probing one is expensive, and it runs only on request. */}
                <RouteProbes modelId={model.id} route={r} onChanged={refresh} />
              </DataTable.Cell>
              <DataTable.Cell className="text-right">{r.priority}</DataTable.Cell>
              <DataTable.Cell className="text-right">{r.weight}</DataTable.Cell>
              <DataTable.Cell>
                <div className="flex flex-wrap gap-1.5">
                  {r.headers && <StatusBadge tone="neutral">Header</StatusBadge>}
                  {(r.context_window || r.max_output_tokens) && (
                    <StatusBadge tone="neutral">{t("gwRouteLimitsTitle")}</StatusBadge>
                  )}
                  {!r.headers && !r.context_window && !r.max_output_tokens && (
                    <span className="text-kumo-subtle">{t("gwInherit")}</span>
                  )}
                </div>
              </DataTable.Cell>
              <DataTable.Cell>
                {r.enabled ? (
                  <span className="text-kumo-success">{t("gwEnable")}</span>
                ) : (
                  <span className="text-kumo-subtle">{t("gwDisable")}</span>
                )}
              </DataTable.Cell>
              <DataTable.Cell>
                <RowActions>
                  <Button size="sm" variant="outline" onClick={() => setEditingRoute(r)}>
                    {t("gwEdit")}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => setTogglingRoute(r)}
                    disabled={update.isPending}
                  >
                    {r.enabled ? t("gwDisable") : t("gwEnable")}
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary-destructive"
                    onClick={() => setRemovingRoute({ id: r.id, label: r.provider_slug })}
                  >
                    {t("gwDelete")}
                  </Button>
                </RowActions>
              </DataTable.Cell>
            </DataTable.Row>
          ))}
          {/* No empty state while pending. This one asserts that the model will not
              appear in the model list and that every request to it answers 404 — a
              claim it has not yet earned the right to make about a model whose routes
              are configured. */}
          {routes.isPending ? (
            <DataTable.Row>
              <DataTable.Cell colSpan={8}>
                <LoadingState label={t("loading")} />
              </DataTable.Cell>
            </DataTable.Row>
          ) : (
            !routes.isError &&
            rows.length === 0 && (
              <DataTable.Row>
                <DataTable.Cell colSpan={8}>
                  <InlineEmpty title={t("gwNoRoutes")} description={t("gwNoRoutesHint")} />
                </DataTable.Cell>
              </DataTable.Row>
            )
          )}
        </DataTable.Body>
      </DataTable>

      <div className="space-y-3 border-t border-kumo-line pt-3">
        <FormRow className="sm:grid-cols-2 xl:grid-cols-[minmax(12rem,1fr)_minmax(12rem,1fr)_9rem_9rem]">
          <FormRow.Item>
            <Field label={t("gwColProvider")}>
              <Select
                placeholder={t("gwSelectProvider")}
                value={providerId || undefined}
                onValueChange={(v) => setProviderId(v ?? "")}
                items={provList.map((p) => ({
                  value: p.id,
                  label: `${p.slug} (${p.protocols.map((f) => t(protocolLabel(f))).join(" + ")})`,
                }))}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field
              label={t("gwColUpstreamModel")}
              htmlFor={`r-um-${model.id}`}
              hint={t("gwRouteUpstreamHint")}
            >
              <Input
                id={`r-um-${model.id}`}
                value={upstreamModel}
                onChange={(e) => setUpstreamModel(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field
              label={t("gwColPriority")}
              htmlFor={`r-pr-${model.id}`}
              hint={t("gwPriorityHint")}
              error={errPriority && t(errPriority as MessageKey)}
            >
              <Input
                id={`r-pr-${model.id}`}
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field
              label={t("gwColWeight")}
              htmlFor={`r-wt-${model.id}`}
              hint={t("gwWeightHint")}
              error={errWeight && t(errWeight as MessageKey)}
            >
              <Input
                id={`r-wt-${model.id}`}
                value={weight}
                onChange={(e) => setWeight(e.target.value)}
              />
            </Field>
          </FormRow.Item>
        </FormRow>
        <FormRow className="sm:grid-cols-[minmax(16rem,1fr)_auto]">
          <FormRow.Item>
            <p className="text-base text-kumo-subtle">{t("gwRouteProbedHint")}</p>
          </FormRow.Item>
          <FormRow.Actions>
            <Button
              onClick={() =>
                create.mutate(
                  {
                    modelId: model.id,
                    data: {
                      provider_id: providerId,
                      provider_model_id: upstreamModel,
                      priority: Number(priority),
                      weight: Number(weight),
                    },
                  },
                  {
                    onSuccess: () => {
                      toasts.add({ variant: "success", title: t("gwRouteCreated") });
                      setUpstreamModel("");
                      refresh();
                    },
                  },
                )
              }
              disabled={
                create.isPending ||
                !providerId ||
                !upstreamModel ||
                Boolean(errPriority || errWeight)
              }
            >
              {t("gwAddRoute")}
            </Button>
          </FormRow.Actions>
        </FormRow>
      </div>

      <ConfirmDialog
        open={removingRoute !== null}
        onOpenChange={(o) => !o && setRemovingRoute(null)}
        title={t("gwRouteDeleteConfirmTitle")}
        description={t("gwRouteDeleteConfirmBody", {
          provider: removingRoute?.label ?? "",
          model: model.slug,
        })}
        confirmLabel={t("gwDelete")}
        pending={remove.isPending}
        onConfirm={() => {
          if (!removingRoute) return;
          remove.mutate(
            { modelId: model.id, routeId: removingRoute.id },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("gwRouteDeleted") });
                setRemovingRoute(null);
                refresh();
              },
            },
          );
        }}
      />
      <ConfirmDialog
        open={togglingRoute !== null}
        onOpenChange={(o) => !o && setTogglingRoute(null)}
        destructive={togglingRoute?.enabled ?? true}
        title={togglingRoute?.enabled ? t("gwDisableConfirmTitle") : t("gwEnableConfirmTitle")}
        description={
          togglingRoute?.enabled
            ? t("gwRouteToggleOffBody", {
                provider: togglingRoute?.provider_slug ?? "",
                model: model.slug,
              })
            : t("gwRouteToggleOnBody", {
                provider: togglingRoute?.provider_slug ?? "",
                model: model.slug,
              })
        }
        confirmLabel={togglingRoute?.enabled ? t("gwDisable") : t("gwEnable")}
        pending={update.isPending}
        onConfirm={() => {
          if (!togglingRoute) return;
          update.mutate(
            {
              modelId: model.id,
              routeId: togglingRoute.id,
              data: { enabled: !togglingRoute.enabled },
            },
            {
              onSuccess: () => {
                toasts.add({
                  variant: "success",
                  title: togglingRoute.enabled ? t("gwDisabledDone") : t("gwEnabledDone"),
                });
                setTogglingRoute(null);
                refresh();
              },
            },
          );
        }}
      />
      {editingRoute && (
        <RouteEditDialog
          model={model}
          route={editingRoute}
          onClose={() => setEditingRoute(null)}
          onSaved={() => {
            toasts.add({ variant: "success", title: t("commonSaved") });
            setEditingRoute(null);
            refresh();
          }}
        />
      )}
    </div>
  );
}

// The edit dialog remounts per target: the form's field state is initialized from the
// route it is given.
function RouteEditDialog({
  model,
  route,
  onClose,
  onSaved,
}: {
  model: GatewayStaffTypes.GatewayModel;
  route: GatewayStaffTypes.GatewayRoute;
  onClose: () => void;
  onSaved: () => void;
}) {
  return (
    <RouteEditForm
      open
      onOpenChange={(next) => !next && onClose()}
      model={model}
      route={route}
      onSaved={onSaved}
    />
  );
}

// Editing an existing upstream configuration.
//
// Without this a route was frozen once created: the interface offered only enable and
// delete, so changing the upstream model name meant deleting and
// recreating it — which loses that route's priority and weight, and leaves the model
// one provider short for the duration. The endpoint always supported these fields;
// what was missing was a way in.
function RouteEditForm({
  open,
  onOpenChange,
  model,
  route,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  model: GatewayStaffTypes.GatewayModel;
  route: GatewayStaffTypes.GatewayRoute;
  onSaved: () => void;
}) {
  const modelId = model.id;
  const { t } = useI18n();
  const update = gatewayStaffApi.useUpdateGatewayRoute();
  const [upstream, setUpstream] = useState(route.provider_model_id);
  const [priority, setPriority] = useState(String(route.priority));
  const [weight, setWeight] = useState(String(route.weight));
  const [rows, setRows] = useState<HeaderRow[]>(() => rowsFromMap(route.headers));
  // Limit overrides: empty means inherit the model's value.
  const [ctxWindow, setCtxWindow] = useState(
    route.context_window == null ? "" : String(route.context_window),
  );
  const [maxOut, setMaxOut] = useState(
    route.max_output_tokens == null ? "" : String(route.max_output_tokens),
  );
  const [ignoresCap, setIgnoresCap] = useState(route.quirks?.ignores_max_output_tokens === true);

  const errPriority = validate(intSchema, priority);
  const errWeight = validate(intSchema, weight);
  const errHeaders = headerRowsError(rows);
  const intErr = (v: string) => (v.trim() === "" ? undefined : validate(intSchema, v));
  const errCtx = intErr(ctxWindow);
  const errMaxOut = intErr(maxOut);
  const headers = mapFromRows(rows);
  const invalid =
    Boolean(errPriority || errWeight || errHeaders) ||
    Boolean(errCtx || errMaxOut) ||
    upstream.trim() === "";

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      size="lg"
      title={t("gwRouteEditTitle")}
      error={update.isError ? apiErrorMessage(update.error) : undefined}
      submitLabel={t("save")}
      submitDisabled={invalid}
      pending={update.isPending}
      onSubmit={() =>
        update.mutate(
          {
            modelId,
            routeId: route.id,
            data: {
              provider_model_id: upstream.trim(),
              priority: Number(priority),
              weight: Number(weight),
              headers,
              context_window: ctxWindow.trim() === "" ? undefined : Number(ctxWindow),
              max_output_tokens: maxOut.trim() === "" ? undefined : Number(maxOut),
              quirks: { ignores_max_output_tokens: ignoresCap },
            },
          },
          { onSuccess: onSaved },
        )
      }
    >
      <FormRow className="sm:grid-cols-3">
        <FormRow.Item>
          <Field
            label={t("gwColUpstreamModel")}
            htmlFor={`re-um-${route.id}`}
            hint={t("gwRouteUpstreamHint")}
          >
            <Input
              id={`re-um-${route.id}`}
              value={upstream}
              onChange={(e) => setUpstream(e.target.value)}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field
            label={t("gwColPriority")}
            htmlFor={`re-pr-${route.id}`}
            hint={t("gwPriorityHint")}
            error={errPriority && t(errPriority as MessageKey)}
          >
            <Input
              id={`re-pr-${route.id}`}
              value={priority}
              onChange={(e) => setPriority(e.target.value)}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field
            label={t("gwColWeight")}
            htmlFor={`re-wt-${route.id}`}
            hint={t("gwWeightHint")}
            error={errWeight && t(errWeight as MessageKey)}
          >
            <Input
              id={`re-wt-${route.id}`}
              value={weight}
              onChange={(e) => setWeight(e.target.value)}
            />
          </Field>
        </FormRow.Item>
      </FormRow>

      <div className="space-y-3 border-t border-kumo-line pt-3">
        <SectionHeading level="sub" as="h4">
          {t("gwRouteLimitsTitle")}
        </SectionHeading>
        <FormRow className="sm:grid-cols-3">
          <FormRow.Item>
            <Field
              label={t("gwRouteCtxWindow")}
              htmlFor={`cw2-${route.id}`}
              hint={t("gwRouteLimitFallback")}
              error={errCtx && t(errCtx as MessageKey)}
            >
              <Input
                id={`cw2-${route.id}`}
                value={ctxWindow}
                onChange={(e) => setCtxWindow(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field
              label={t("gwRouteMaxOut")}
              htmlFor={`mo-${route.id}`}
              hint={t("gwRouteLimitFallback")}
              error={errMaxOut && t(errMaxOut as MessageKey)}
            >
              <Input
                id={`mo-${route.id}`}
                value={maxOut}
                onChange={(e) => setMaxOut(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item className="grid gap-2">
            <div className="flex min-h-9 items-center sm:row-start-2">
              <Checkbox
                label={t("gwRouteIgnoresCap")}
                checked={ignoresCap}
                onCheckedChange={(v) => setIgnoresCap(v === true)}
              />
            </div>
            <p className="text-base text-kumo-subtle sm:row-start-3">
              {t("gwRouteIgnoresCapHint")}
            </p>
          </FormRow.Item>
        </FormRow>
      </div>

      <div className="space-y-2 border-t border-kumo-line pt-3">
        <SectionHeading level="sub" as="h4">
          {t("gwHdrRouteTitle")}
        </SectionHeading>
        <p className="text-base text-kumo-subtle">
          {t("gwHdrHint")} {t("gwHdrRouteHint")}
        </p>
        <HeaderMapEditor
          rows={rows}
          onChange={setRows}
          idPrefix={`r-${route.id}`}
          disabled={update.isPending}
        />
        {errHeaders && <Alert>{t(errHeaders)}</Alert>}
      </div>
    </FormDialog>
  );
}

// What is known per endpoint of one route — the route's only capability record —
// and the operator's hand on it.
//
// One badge per endpoint of each protocol the provider speaks. Green is "a call
// went through", red "the upstream said there is nothing here" (and the route
// is skipped for that endpoint), amber "an inconclusive answer" (shown, never
// acted on), grey "not looked at yet" (callable, not listed). The menu on each
// badge is the override: publish an endpoint the worker will not probe on its
// own (images), or say "do not send this here"; clearing hands it back.
//
// On failure the **upstream's own words** go on the title attribute: they are the
// only clue distinguishing a bad credential from a bad model name from an
// unsupported endpoint, and rewriting or omitting them leaves the operator
// guessing. That is not hypothetical — with nine models all red, it was the
// phrase "unsupported endpoint" in the raw message that identified the actual
// cause.
function RouteProbes({
  modelId,
  route,
  onChanged,
}: {
  modelId: string;
  route: GatewayStaffTypes.GatewayRoute;
  onChanged: () => void;
}) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const override = gatewayStaffApi.useSetGatewayRouteProbe();
  const probe = gatewayStaffApi.useProbeGatewayRoute();
  const probes = route.probes ?? [];
  if (probes.length === 0) return <span className="text-kumo-subtle">{t("gwInherit")}</span>;
  const fail = (error: unknown) =>
    toasts.add({
      variant: "error",
      title: t("gwProbeActionFailed"),
      description: apiErrorMessage(error),
    });
  const set = (endpoint: string, status: GatewayStaffTypes.GatewayRouteProbeOverrideStatus) =>
    override.mutate(
      { modelId, routeId: route.id, endpoint, data: { status } },
      { onSuccess: onChanged, onError: fail },
    );
  const run = (endpoint: string) =>
    probe.mutate(
      { modelId, routeId: route.id, data: { endpoints: [endpoint] } },
      {
        onSuccess: () => toasts.add({ variant: "success", title: t("gwProbeRequested") }),
        onError: fail,
      },
    );
  return (
    <div className="flex flex-wrap gap-1.5">
      {probes.map((p) => {
        // A verified endpoint whose latest probe failed keeps its verdict --
        // one inconclusive sample does not move it -- but the row carries the
        // failure, and a green badge that hides a 401 is a lie: it shows amber
        // with the upstream's words.
        const degraded = p.status === "ok" && (p.status_code ?? 0) >= 400;
        const tone =
          p.status === "ok"
            ? degraded
              ? "warning"
              : "success"
            : p.status === "unsupported"
              ? "danger"
              : p.status === "failed"
                ? "warning"
                : "neutral";
        const title =
          p.status === "failed" || p.status === "unsupported"
            ? p.error || t("gwProbeUnsupported")
            : degraded
              ? t("gwProbeVerifiedButLastFailed", { error: p.error || String(p.status_code) })
              : p.status === "unverified" && p.probe_mode === "manual"
                ? t("gwProbeImagesManual")
                : undefined;
        const byOperator = p.source === "operator";
        return (
          <DropdownMenu key={p.endpoint}>
            <DropdownMenu.Trigger
              render={(props) => (
                <button
                  {...props}
                  type="button"
                  className="inline-flex items-center gap-1 rounded"
                  title={title}
                  aria-label={t("gwProbeMenuFor", { endpoint: p.endpoint })}
                >
                  <StatusBadge tone={tone}>
                    {byOperator ? `${p.endpoint} ✎` : p.endpoint}
                  </StatusBadge>
                </button>
              )}
            />
            <DropdownMenu.Content align="start">
              <DropdownMenu.Group>
                <DropdownMenu.Item onClick={() => run(p.endpoint)}>
                  {t("gwProbeRunNow")}
                </DropdownMenu.Item>
                <DropdownMenu.Item onClick={() => set(p.endpoint, "ok")}>
                  {t("gwProbeMarkSupported")}
                </DropdownMenu.Item>
                <DropdownMenu.Item onClick={() => set(p.endpoint, "unsupported")}>
                  {t("gwProbeMarkUnsupported")}
                </DropdownMenu.Item>
                {byOperator && (
                  <DropdownMenu.Item onClick={() => set(p.endpoint, "unverified")}>
                    {t("gwProbeClearOverride")}
                  </DropdownMenu.Item>
                )}
              </DropdownMenu.Group>
            </DropdownMenu.Content>
          </DropdownMenu>
        );
      })}
    </div>
  );
}
