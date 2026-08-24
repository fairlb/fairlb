import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import {
  ApiError,
  gatewayStaffApi,
  getResponseETag,
  type GatewayStaffTypes,
  apiErrorMessage,
  apiErrorStatus,
} from "@fairlb/api-client";
import { type MessageKey, useDisplayDate, useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  ConfirmDialog,
  DataTable,
  Field,
  FormActions,
  FormDialog,
  FormRow,
  InlineEmpty,
  Input,
  intSchema,
  LoadingState,
  PageActionDock,
  PageContentsNav,
  PageHeader,
  RecordPage,
  resolveNavValue,
  SectionHeading,
  Select,
  StatusBadge,
  Textarea,
  useAdminTitle,
  validate,
} from "@fairlb/ui";
import { ModelProvidersDialog } from "./model-providers-dialog";
import { useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useBlocker, useLocation, useParams } from "@tanstack/react-router";
import { createContext, useCallback, useContext, useEffect, useState } from "react";
import { ModelStateBadges, RoutePanel, VISIBILITY_KEY } from "./models";
import { AdjustmentEditor } from "./adjustment-editor";
import { adjustmentLabel } from "./adjustment-label";
import {
  adjustmentBps,
  adjustmentFromBps,
  type AdjustmentMode,
  DECIMAL_RATE,
  multiplierFromPublicRate,
  multiplyRate,
} from "./pricing-math";
import { ReadinessSteps } from "./readiness";
import { useCurrentStaffRole, useRecordBreadcrumb } from "./host";
import { protocolLabel } from "./providers-shared";

type RateKey = keyof GatewayStaffTypes.DraftTokenRatesUSDPerM;

const RATE_ROWS: { key: RateKey; label: string }[] = [
  { key: "input", label: "Input" },
  { key: "output", label: "Output" },
  { key: "cache_read", label: "Cache Read" },
  { key: "cache_write", label: "Cache Write" },
];

function localDateTime(iso?: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.valueOf())) return "";
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.valueOf() - offset).toISOString().slice(0, 16);
}

type ModelContextValue = {
  model: GatewayStaffTypes.GatewayModel;
  pricing?: GatewayStaffTypes.ModelPricingResource;
  pricingPending: boolean;
  pricingError: boolean;
  refreshModel: () => void;
  refreshPricing: () => void;
  refetchPricing: () => Promise<GatewayStaffTypes.ModelPricingResource | undefined>;
  setPricingDirty: (dirty: boolean) => void;
};

const ModelContext = createContext<ModelContextValue | null>(null);

function useModelRecord(): ModelContextValue {
  const value = useContext(ModelContext);
  if (!value) throw new Error("Model task pages must be rendered inside GatewayModelLayout");
  return value;
}

/** Persistent record chrome for model overview, routing and pricing tasks. */
export function GatewayModelLayout() {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const { modelId = "" } = useParams({ strict: false }) as { modelId?: string };
  const pathname = useLocation({ select: (location) => location.pathname });
  const modelQuery = gatewayStaffApi.useGetGatewayModel(modelId, {
    query: { enabled: modelId !== "" },
  });
  const queryClient = useQueryClient();
  const update = gatewayStaffApi.useUpdateGatewayModel();
  const pricing = gatewayStaffApi.useGetGatewayModelPricing(modelId);
  const model = modelQuery.data;
  const refreshModel = useCallback(() => {
    void modelQuery.refetch();
    void queryClient.invalidateQueries({
      queryKey: gatewayStaffApi.getListGatewayModelsQueryKey(),
    });
  }, [modelQuery, queryClient]);
  const refreshPricing = useCallback(() => void pricing.refetch(), [pricing]);
  const refetchPricing = useCallback(async () => (await pricing.refetch()).data, [pricing]);
  const [toggleConfirm, setToggleConfirm] = useState(false);
  const [editing, setEditing] = useState(false);
  const [pricingDirty, setPricingDirty] = useState(false);
  const blocker = useBlocker({
    shouldBlockFn: () => pricingDirty,
    enableBeforeUnload: pricingDirty,
    withResolver: true,
  });
  useAdminTitle(model?.display_name || model?.slug);
  const notFound = apiErrorStatus(modelQuery.error) === 404;
  const pendingLabel =
    modelQuery.isPending || modelQuery.isFetching ? t("loading") : t("gwModelNotFound");
  const breadcrumb = useRecordBreadcrumb(model?.slug ?? pendingLabel);
  const basePath = `/gateway/models/${modelId}`;
  const aspects = [
    { value: "overview", label: t("gwDetailOverview"), href: basePath },
    { value: "routes", label: t("gwDetailRoutes"), href: `${basePath}/routes` },
    { value: "pricing", label: t("gwPricing"), href: `${basePath}/pricing` },
  ];
  const active = resolveNavValue(aspects, pathname);

  return (
    <RecordPage
      header={
        <PageHeader
          breadcrumbs={breadcrumb}
          // See the note on the provider record page: state is part of the
          // identity, not an action.
          title={
            <span className="flex flex-wrap items-center gap-3">
              {model?.display_name || model?.slug || pendingLabel}
              {model && (
                <span className="font-mono text-[0.9em] text-kumo-subtle">{model.slug}</span>
              )}
              {model && (
                <StatusBadge tone={model.enabled ? "success" : "neutral"}>
                  {model.enabled ? t("gwEnabled") : t("gwDisabledTag")}
                </StatusBadge>
              )}
            </span>
          }
          actions={
            model && (
              <>
                <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
                  {t("gwEdit")}
                </Button>
                <Button size="sm" variant="outline" onClick={() => setToggleConfirm(true)}>
                  {model.enabled ? t("gwDisableModel") : t("gwEnableModel")}
                </Button>
              </>
            )
          }
          recordNav={{ value: active, items: aspects }}
        />
      }
    >
      <ModelEditDialog
        model={model}
        open={editing}
        onOpenChange={setEditing}
        onSaved={refreshModel}
      />

      <div className="min-w-0">
        {!model && (modelQuery.isPending || modelQuery.isFetching) ? (
          <LoadingState label={t("loading")} />
        ) : notFound ? (
          <InlineEmpty title={t("gwModelNotFound")} />
        ) : modelQuery.isError ? (
          <Alert>{apiErrorMessage(modelQuery.error)}</Alert>
        ) : !model ? (
          <InlineEmpty title={t("gwModelNotFound")} />
        ) : (
          <ModelContext.Provider
            value={{
              model,
              pricing: pricing.data,
              pricingPending: pricing.isPending,
              pricingError: pricing.isError,
              refreshModel,
              refreshPricing,
              refetchPricing,
              setPricingDirty,
            }}
          >
            <Outlet />
          </ModelContext.Provider>
        )}
      </div>

      <ConfirmDialog
        open={toggleConfirm}
        onOpenChange={setToggleConfirm}
        destructive={model?.enabled ?? false}
        title={model?.enabled ? t("gwDisableConfirmTitle") : t("gwEnableConfirmTitle")}
        description={
          model?.enabled
            ? t("gwDisableConfirmBody", { slug: model.slug })
            : t("gwEnableConfirmBody", { slug: model?.slug ?? "" })
        }
        confirmLabel={model?.enabled ? t("gwDisableModel") : t("gwEnableModel")}
        pending={update.isPending}
        onConfirm={() => {
          if (!model) return;
          update.mutate(
            { modelId: model.id, data: { enabled: !model.enabled } },
            {
              onSuccess: () => {
                toasts.add({
                  variant: "success",
                  title: model.enabled ? t("gwDisabledDone") : t("gwEnabledDone"),
                });
                setToggleConfirm(false);
                refreshModel();
              },
            },
          );
        }}
      />
      <ConfirmDialog
        open={blocker.status === "blocked"}
        onOpenChange={(open) => !open && blocker.reset?.()}
        destructive={false}
        title={t("gwLeaveUnsavedTitle")}
        description={t("gwLeaveUnsavedBody")}
        confirmLabel={t("gwLeaveUnsaved")}
        onConfirm={() => {
          setPricingDirty(false);
          blocker.proceed?.();
        }}
      />
    </RecordPage>
  );
}

// See the note on the provider aspect pages: no heading repeating the nav item.
export function GatewayModelOverviewPage() {
  const { model, pricing, pricingPending, pricingError } = useModelRecord();
  return (
    <ModelOverview
      model={model}
      pricing={pricing}
      pricingPending={pricingPending}
      pricingError={pricingError}
    />
  );
}

export function GatewayModelRoutesPage() {
  const { model } = useModelRecord();
  return <ModelRoutesPanel model={model} />;
}

export function GatewayModelPricingPage() {
  const { t } = useI18n();
  const { model, pricing, refreshPricing, refetchPricing, setPricingDirty } = useModelRecord();
  return (
    <div className="flex min-w-0 flex-col gap-6 2xl:flex-row 2xl:items-start 2xl:gap-10">
      <div className="min-w-0 flex-1">
        <ModelPricingPanel
          model={model}
          resource={pricing}
          onChanged={refreshPricing}
          onRefetch={refetchPricing}
          onDirtyChange={setPricingDirty}
        />
      </div>
      <PageContentsNav
        items={[
          { href: "#model-price-formula", label: t("gwPriceFormula") },
          { href: "#model-price-source", label: t("gwPriceProvenance") },
          { href: "#model-price-advanced", label: t("gwAdvancedPricingSection") },
        ]}
      />
    </div>
  );
}

/** Exported only so a regression test can render it; within the page it is used by
 * this file alone. */
export function ModelOverview({
  model,
  pricing,
  pricingPending,
  pricingError,
}: {
  model: GatewayStaffTypes.GatewayModel;
  pricing?: GatewayStaffTypes.ModelPricingResource;
  pricingPending: boolean;
  pricingError: boolean;
}) {
  const { t, formatNumber } = useI18n();
  const displayDate = useDisplayDate();
  const priced = pricing?.priced ? pricing : undefined;
  // Both queries are already in use by the routes panel and the pickers; they are
  // deduplicated and cost no extra request.
  const routes = gatewayStaffApi.useListGatewayRoutes(model.id);
  const providers = gatewayStaffApi.useListGatewayProviders();
  // Not one step is decidable until all three queries have arrived: while pending they
  // are undefined, and folding undefined into "no" marks every step incomplete — which
  // on a fully configured model asserts outright that it has no list price, no usable
  // provider and is unpublished, then returns null once the data lands, taking the
  // price card from one column to the other as it goes. The gate is here rather than
  // inside the checklist component, because that component receives conclusions and
  // cannot tell "false" from "not known yet".
  if (pricingPending || routes.isPending || providers.isPending) {
    return <LoadingState label={t("loading")} />;
  }
  // A failed query is equally undecidable: its data is undefined too, it has merely
  // settled, so the gate above does not catch it. Better no conclusion than a false
  // one — which is why this is not "treat it as incomplete".
  const verdictKnown = !pricingError && !routes.isError && !providers.isError;
  const completeRates =
    priced?.billing_mode === "free" ||
    (priced?.official_rates != null &&
      Object.values(priced.official_rates).every((value) => value != null));
  const publicReady = priced?.billing_mode === "free" || priced?.public_rates != null;
  // "Usable providers" is counted by the same rule the server's enable gate applies:
  // the route is enabled and the provider behind it is enabled. The model's own
  // route count includes disabled routes, so using it would show green at the same
  // moment enabling is refused.
  const enabledProviders = new Set(
    (providers.data?.items ?? []).filter((p) => p.enabled).map((p) => p.id),
  );
  const usableRoutes = (routes.data?.items ?? []).filter(
    (r) => r.enabled && enabledProviders.has(r.provider_id),
  ).length;
  // Each step links to the page where it can be acted on: a visible step you cannot
  // reach just makes the reader go hunting for the page themselves.
  const stepLink = (target: "pricing" | "routes", label: string) =>
    target === "pricing" ? (
      <Link
        to="/gateway/models/$modelId/pricing"
        params={{ modelId: model.id }}
        className="text-kumo-info underline-offset-4 hover:underline"
      >
        {label}
      </Link>
    ) : (
      <Link
        to="/gateway/models/$modelId/routes"
        params={{ modelId: model.id }}
        className="text-kumo-info underline-offset-4 hover:underline"
      >
        {label}
      </Link>
    );
  const steps = [
    { key: "official", label: stepLink("pricing", t("gwChecklistOfficial")), done: completeRates },
    {
      key: "route",
      label: stepLink("routes", t("gwChecklistRoute")),
      // The same rule the server's enable gate uses: an enabled route whose provider is
      // also enabled. The plain route count includes disabled ones, so relying on it
      // would leave this step green while enabling is being refused.
      done: usableRoutes > 0,
    },
    { key: "public", label: stepLink("pricing", t("gwChecklistPublic")), done: publicReady },
    {
      key: "publish",
      label: stepLink("pricing", t("gwChecklistPublish")),
      done: Boolean(model.enabled && priced),
    },
  ];

  return (
    <div className="space-y-6">
      {/* The readiness checklist gets a row to itself. It is a getting-started guide
          and disappears entirely once every step is done — **and precisely because it
          disappears, it must not decide the column layout**. Sharing a two-column grid
          with the price card left the narrower column permanently empty on a healthy
          model, which is the steady state: 347px of nothing in an 1100px container. */}
      {verdictKnown && <ReadinessSteps title={t("gwModelReadiness")} steps={steps} />}
      {/* Both cards render unconditionally, so the column count depends on nothing. */}
      <div className="grid gap-6 xl:grid-cols-2">
        <Card className="space-y-4">
          <SectionHeading>{t("gwModelIdentity")}</SectionHeading>
          {/* This block did not exist: the catalog attributes could be changed from a
              header dialog but read nowhere, so seeing their current values meant
              opening the edit form. */}
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-3 text-base">
            {/* A model owns no protocol: this is the set its enabled routes'
                providers speak, which is every protocol it can be called on. */}
            <dt className="text-kumo-subtle">{t("gwColProtocols")}</dt>
            <dd>
              {model.protocols.length > 0
                ? model.protocols.map((p) => t(protocolLabel(p))).join(" + ")
                : "—"}
            </dd>
            <dt className="text-kumo-subtle">{t("gwColVisibility")}</dt>
            <dd>{t(VISIBILITY_KEY[model.visibility] ?? "visibilityHidden")}</dd>
            <dt className="text-kumo-subtle">{t("gwContextWindow")}</dt>
            <dd className="font-mono">
              {model.context_window ? formatNumber(model.context_window) : "—"}
            </dd>
            <dt className="text-kumo-subtle">{t("gwMaxOutputTokens")}</dt>
            <dd className="font-mono">
              {model.max_output_tokens ? formatNumber(model.max_output_tokens) : "—"}
            </dd>
            <dt className="text-kumo-subtle">{t("gwColEndpoints")}</dt>
            <dd>
              {/* What probes have verified on the enabled routes -- the same set
                  the public catalog publishes. Nothing here was declared. */}
              {model.endpoints.length > 0 ? (
                <span className="flex flex-wrap gap-2">
                  {model.endpoints.map((endpoint) => (
                    <StatusBadge key={endpoint} tone="neutral">
                      {endpoint}
                    </StatusBadge>
                  ))}
                </span>
              ) : (
                <span className="text-kumo-subtle">—</span>
              )}
            </dd>
            <dt className="text-kumo-subtle">{t("gwColRoutes")}</dt>
            <dd>
              {/* Links to the face where something can be done, not back to the
                  overview. */}
              {stepLink("routes", String(model.route_count ?? 0))}
            </dd>
          </dl>
        </Card>

        <Card className="space-y-4">
          <SectionHeading>{t("gwCurrentPriceVersion")}</SectionHeading>
          {priced ? (
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-3 text-base">
              <dt className="text-kumo-subtle">{t("gwBillingMode")}</dt>
              <dd>{priced.billing_mode === "free" ? t("gwBillingFree") : t("gwBillingPaid")}</dd>
              <dt className="text-kumo-subtle">{t("gwSourceName")}</dt>
              <dd>{priced.source_name}</dd>
              <dt className="text-kumo-subtle">{t("gwCheckedAt")}</dt>
              <dd>{displayDate(priced.checked_at)}</dd>
              <dt className="text-kumo-subtle">{t("gwPriceUpdatedAt")}</dt>
              <dd>{displayDate(priced.updated_at)}</dd>
            </dl>
          ) : (
            <InlineEmpty title={t("gwNoActivePricing")} />
          )}
          {/* Signals the server has already computed used to never reach the model's
              own page — negative margin among them. The predicates are taken straight
              from those booleans rather than recomputed here, and the badge component
              is shared with the list page so the next signal cannot be added to only
              one of them. */}
          <div className="border-t border-kumo-line pt-4">
            <ModelStateBadges model={model} />
            <p className="mt-3 text-base text-kumo-subtle">
              {stepLink("pricing", t("gwOpenPricing"))}
            </p>
          </div>
        </Card>
      </div>
    </div>
  );
}

/**
 * Editing a model's catalog attributes.
 *
 * The contract always accepted these four fields, while the interface neither
 * collected them at creation nor offered a way to edit them afterwards — changing a
 * display name meant calling the API by hand. The slug and the protocol are not among
 * them: the first is the key everything else refers to, and changing the second would
 * invalidate every existing route.
 */
function ModelEditDialog({
  model,
  open,
  onOpenChange,
  onSaved,
}: {
  model?: GatewayStaffTypes.GatewayModel;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const update = gatewayStaffApi.useUpdateGatewayModel();
  const [displayName, setDisplayName] = useState("");
  const [visibility, setVisibility] =
    useState<GatewayStaffTypes.GatewayModelInputVisibility>("public");
  const [contextWindow, setContextWindow] = useState("");
  const [maxOutput, setMaxOutput] = useState("");
  // The dialog stays mounted, so it refills from the current values on every open;
  // otherwise the previous edit's state lingers. Only this component's own state is
  // touched during render — clearing the error is a cross-store side effect and belongs
  // in the close handler.
  const [loadedFor, setLoadedFor] = useState<string | null>(null);
  if (open && model && loadedFor !== model.id) {
    setLoadedFor(model.id);
    setDisplayName(model.display_name ?? "");
    setVisibility(model.visibility as GatewayStaffTypes.GatewayModelInputVisibility);
    setContextWindow(model.context_window ? String(model.context_window) : "");
    setMaxOutput(model.max_output_tokens ? String(model.max_output_tokens) : "");
  }
  if (!open && loadedFor !== null) setLoadedFor(null);

  const errContext = contextWindow ? validate(intSchema, contextWindow) : undefined;
  const errMaxOutput = maxOutput ? validate(intSchema, maxOutput) : undefined;

  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        if (!next) update.reset();
        onOpenChange(next);
      }}
      title={t("gwEditModel")}
      description={t("gwEditModelHint", { slug: model?.slug ?? "" })}
      error={update.isError ? apiErrorMessage(update.error) : undefined}
      submitLabel={t("save")}
      submitDisabled={!model || Boolean(errContext) || Boolean(errMaxOutput)}
      pending={update.isPending}
      onSubmit={() => {
        if (!model) return;
        update.mutate(
          {
            modelId: model.id,
            data: {
              display_name: displayName.trim(),
              visibility,
              ...(contextWindow ? { context_window: Number(contextWindow) } : {}),
              ...(maxOutput ? { max_output_tokens: Number(maxOutput) } : {}),
            },
          },
          {
            onSuccess: () => {
              toasts.add({ variant: "success", title: t("gwModelSaved") });
              onOpenChange(false);
              onSaved();
            },
          },
        );
      }}
    >
      <Field label={t("gwDisplayName")} htmlFor="me-display" hint={t("gwDisplayNameHint")}>
        <Input
          id="me-display"
          value={displayName}
          onChange={(event) => setDisplayName(event.target.value)}
        />
      </Field>
      <Select
        label={t("gwColVisibility")}
        description={t("gwVisibilityHint")}
        value={visibility as string}
        onValueChange={(value) =>
          setVisibility((value ?? "public") as GatewayStaffTypes.GatewayModelInputVisibility)
        }
        items={[
          { value: "public", label: t("visibilityPublic") },
          { value: "beta", label: t("visibilityBeta") },
          { value: "hidden", label: t("visibilityHidden") },
        ]}
      />
      <FormRow className="sm:grid-cols-2">
        <FormRow.Item>
          <Field
            label={t("gwContextWindow")}
            htmlFor="me-ctx"
            error={errContext && t(errContext as MessageKey)}
          >
            <Input
              id="me-ctx"
              value={contextWindow}
              inputMode="numeric"
              onChange={(event) => setContextWindow(event.target.value)}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field
            label={t("gwMaxOutputTokens")}
            htmlFor="me-maxout"
            hint={t("gwMaxOutputHint")}
            error={errMaxOutput && t(errMaxOutput as MessageKey)}
          >
            <Input
              id="me-maxout"
              value={maxOutput}
              inputMode="numeric"
              onChange={(event) => setMaxOutput(event.target.value)}
            />
          </Field>
        </FormRow.Item>
      </FormRow>
    </FormDialog>
  );
}

interface PricingFormState {
  billingMode: "paid" | "free";
  rates: GatewayStaffTypes.DraftTokenRatesUSDPerM;
  adjustmentMode: AdjustmentMode;
  adjustmentPercent: string;
  sourceName: string;
  sourceUrl: string;
  checkedAt: string;
  /** Why the price changed. **Required, now that saving takes effect immediately** —
   * the audit log is the only source of the previous values when rolling one back. */
  reason: string;
  dimensions: GatewayStaffTypes.ModelPriceDimensionRate[];
  tools: GatewayStaffTypes.ModelPriceToolRate[];
}

// The form is initialized from **the current price row**. There is one source rather
// than a draft and a published slot to choose between, and when nothing is priced every
// field is left empty — **not four zeros**, which would look identical on screen while
// meaning something else entirely.
function pricingState(cur?: GatewayStaffTypes.ModelPricingResource): PricingFormState {
  const priced = cur?.priced ? cur : undefined;
  const adjustment = adjustmentFromBps(priced?.adjustment?.multiplier_bps ?? 10_000);
  return {
    billingMode: priced?.billing_mode ?? "paid",
    rates: priced?.official_rates ?? {
      input: null,
      output: null,
      cache_read: null,
      cache_write: null,
    },
    adjustmentMode: adjustment.mode,
    adjustmentPercent: adjustment.percent,
    sourceName: priced?.source_name ?? "",
    sourceUrl: priced?.source_url ?? "",
    checkedAt: priced?.checked_at
      ? localDateTime(priced.checked_at)
      : localDateTime(new Date().toISOString()),
    // The reason is **not prefilled from last time**: each change needs its own
    // justification, and copying the previous one is the same as writing nothing.
    reason: "",
    dimensions: priced?.dimension_rates ?? [],
    tools: priced?.tool_rates ?? [],
  };
}

interface ModelPricingConflict {
  local: PricingFormState;
  current?: GatewayStaffTypes.ModelPricingResource;
}

function ModelPricingPanel({
  model,
  resource,
  onChanged,
  onRefetch,
  onDirtyChange,
}: {
  model: GatewayStaffTypes.GatewayModel;
  resource?: GatewayStaffTypes.ModelPricingResource;
  onChanged: () => void;
  onRefetch: () => Promise<GatewayStaffTypes.ModelPricingResource | undefined>;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const { t } = useI18n();
  const staffRole = useCurrentStaffRole();
  const [form, setForm] = useState<PricingFormState>(() => pricingState(resource));
  const [baseline, setBaseline] = useState(() => JSON.stringify(pricingState(resource)));
  const [hydratedAt, setHydratedAt] = useState<string | undefined>(resource?.updated_at);
  const [advanced, setAdvanced] = useState(false);
  const [pendingFreeAction, setPendingFreeAction] = useState<"save" | null>(null);
  const [conflict, setConflict] = useState<ModelPricingConflict | null>(null);
  const [focusFieldId, setFocusFieldId] = useState<string | null>(null);
  // Warning codes that have to be acknowledged before saving. Pricing below cost is
  // not refused outright, but it has to be an informed decision.
  const [acknowledged, setAcknowledged] = useState<string[]>([]);
  // Which row is being typed into in reverse. Mid-typing states such as "8." imply no
  // multiplier, and if the field were always the computed value the input would fight
  // the cursor.
  const [publicDraft, setPublicDraft] = useState<{ key: RateKey; text: string } | null>(null);
  const multiplier = adjustmentBps(form.adjustmentMode, form.adjustmentPercent);
  const label = adjustmentLabel(multiplier);
  const dirty = JSON.stringify(form) !== baseline;
  /**
   * Typing in a published-price field derives the selling multiplier backwards.
   *
   * When nothing can be derived it **keeps the text and leaves the multiplier alone**:
   * a mid-typing state such as "8." or an empty string looks exactly like a genuinely
   * invalid pair while someone is still typing, and clearing the multiplier would turn
   * the other three rows into dashes at the same moment.
   */
  const setPublicRate = (key: RateKey, text: string) => {
    setPublicDraft({ key, text });
    const bps = multiplierFromPublicRate(form.rates[key], text);
    if (bps == null) return;
    const next = adjustmentFromBps(bps);
    setForm((prev) => ({
      ...prev,
      adjustmentMode: next.mode,
      adjustmentPercent: next.percent,
    }));
  };
  const getUrl = gatewayStaffApi.getGetGatewayModelPricingUrl(model.id);
  const saveUrl = gatewayStaffApi.getSaveGatewayModelPricingUrl(model.id);
  const etag = getResponseETag(saveUrl, getUrl) ?? `"${resource?.updated_at ?? "new"}"`;
  const save = gatewayStaffApi.useSaveGatewayModelPricing({
    request: { headers: { "If-Match": etag } },
  });

  useEffect(() => {
    onDirtyChange(dirty);
    return () => onDirtyChange(false);
  }, [dirty, onDirtyChange]);

  useEffect(() => {
    if (!dirty && resource?.updated_at !== hydratedAt) {
      const state = pricingState(resource);
      setForm(state);
      setBaseline(JSON.stringify(state));
      setHydratedAt(resource?.updated_at);
    }
  }, [dirty, hydratedAt, resource]);

  useEffect(() => {
    if (!focusFieldId) return;
    if ((focusFieldId.startsWith("dimension-") || focusFieldId.startsWith("tool-")) && !advanced) {
      setAdvanced(true);
      return;
    }
    const frame = requestAnimationFrame(() => {
      const field = document.getElementById(focusFieldId);
      if (field instanceof HTMLElement) {
        field.focus();
        setFocusFieldId(null);
      }
    });
    return () => cancelAnimationFrame(frame);
  }, [advanced, focusFieldId]);

  const firstInvalidRate = RATE_ROWS.find(
    ({ key }) =>
      form.billingMode === "paid" &&
      (form.rates[key] == null || !DECIMAL_RATE.test(form.rates[key] ?? "")),
  );
  const dimensionKeys = new Set<string>();
  const firstInvalidDimension = form.dimensions.findIndex((row) => {
    const key = dimensionRowKey(row);
    const invalid =
      !DECIMAL_RATE.test(row.rate_usd_per_m) ||
      !isValidMinInputTokens(row.min_input_tokens) ||
      dimensionKeys.has(key);
    dimensionKeys.add(key);
    return invalid;
  });
  const toolNames = new Set<string>();
  const firstInvalidTool = form.tools.findIndex((row) => {
    const name = row.tool.trim();
    const invalid =
      name === "" ||
      name.length > 200 ||
      toolNames.has(name) ||
      !DECIMAL_RATE.test(row.rate_usd_per_call);
    if (name) toolNames.add(name);
    return invalid;
  });
  const checkedAtInvalid = !form.checkedAt || Number.isNaN(new Date(form.checkedAt).valueOf());
  const reasonInvalid = dirty && form.reason.trim() === "";
  const invalid =
    firstInvalidRate != null ||
    firstInvalidDimension >= 0 ||
    firstInvalidTool >= 0 ||
    multiplier == null ||
    form.sourceName.trim() === "" ||
    checkedAtInvalid ||
    reasonInvalid;

  const payload = (): GatewayStaffTypes.ModelPricingInput => ({
    billing_mode: form.billingMode,
    official_rates: form.rates,
    adjustment: { multiplier_bps: multiplier ?? 10_000 },
    source_name: form.sourceName.trim(),
    ...(form.sourceUrl.trim() ? { source_url: form.sourceUrl.trim() } : {}),
    checked_at: new Date(form.checkedAt).toISOString(),
    // Saving takes effect immediately, so a reason is required: nothing else records
    // why the price changed.
    reason: form.reason.trim(),
    ...(acknowledged.length > 0
      ? { acknowledged_risks: acknowledged as GatewayStaffTypes.PricingRiskCode[] }
      : {}),
    dimension_rates: form.dimensions,
    tool_rates: form.tools.map((row) => ({
      tool: row.tool.trim(),
      rate_usd_per_call: row.rate_usd_per_call,
    })),
  });

  const savePricing = async (): Promise<GatewayStaffTypes.ModelPricingResource | null> => {
    if (invalid) {
      if (firstInvalidRate) setFocusFieldId(`pricing-rate-${firstInvalidRate.key}`);
      else if (multiplier == null) setFocusFieldId("pricing-adjustment-percent");
      else if (form.sourceName.trim() === "") setFocusFieldId("pricing-source-name");
      else if (checkedAtInvalid) setFocusFieldId("pricing-checked-at");
      else if (reasonInvalid) setFocusFieldId("pricing-change-reason");
      else if (firstInvalidDimension >= 0)
        setFocusFieldId(`dimension-rate-${firstInvalidDimension}`);
      else if (firstInvalidTool >= 0) {
        const tool = form.tools[firstInvalidTool];
        setFocusFieldId(
          tool?.tool.trim() && DECIMAL_RATE.test(tool.rate_usd_per_call)
            ? `tool-name-${firstInvalidTool}`
            : tool?.tool.trim()
              ? `tool-rate-${firstInvalidTool}`
              : `tool-name-${firstInvalidTool}`,
        );
      }
      return null;
    }
    setConflict(null);
    try {
      const saved = await save.mutateAsync({ modelId: model.id, data: payload() });
      const clean = { ...form, reason: "" };
      setForm(clean);
      setBaseline(JSON.stringify(clean));
      setPublicDraft(null);
      setHydratedAt(saved.updated_at);
      setAcknowledged([]);
      onChanged();
      return saved;
    } catch (error) {
      if (error instanceof ApiError && (error.status === 409 || error.status === 412)) {
        const latest = await onRefetch();
        setConflict({ local: form, current: latest });
      }
      // A 422 means there are unacknowledged risks. The server lists them individually
      // in the body; the interface collects the warning codes into the acknowledged set
      // and submits again. Blocking risks cannot be acknowledged.
      return null;
    }
  };

  return (
    <div className="space-y-6">
      {save.isError && !conflict && <Alert>{apiErrorMessage(save.error)}</Alert>}
      {conflict && (
        <ModelPricingConflictPanel
          conflict={conflict}
          onKeepLocal={() => {
            save.reset();
            setConflict(null);
          }}
          onUseCurrent={() => {
            const state = pricingState(conflict.current);
            setForm(state);
            setBaseline(JSON.stringify(state));
            setHydratedAt(conflict.current?.updated_at);
            setPublicDraft(null);
            setAcknowledged([]);
            save.reset();
            setConflict(null);
          }}
        />
      )}
      <Card id="model-price-formula" className="scroll-mt-6 overflow-hidden p-0">
        <div className="border-b border-kumo-line px-6 py-5">
          <div className="flex flex-wrap items-start justify-between gap-4">
            <div>
              <SectionHeading>{t("gwPriceFormula")}</SectionHeading>
              <p className="mt-1 text-base text-kumo-subtle">{t("gwPriceFormulaHint")}</p>
            </div>
            {/* This is the multiplier **the form is computing right now**, not the one
                stored. Sharing a single label with a column showing the stored value put
                the same word above two different numbers for one model. */}
            <div className="min-w-48 rounded-lg border border-kumo-line bg-kumo-recessed px-4 py-3 text-right">
              <div className="text-base text-kumo-subtle">{t("gwEffectiveMultiplier")}</div>
              <div className="font-mono text-lg font-semibold">
                {multiplier == null ? "—" : `× ${(multiplier / 10_000).toFixed(4)}`}
              </div>
              {/* `× 0.8000` is the arithmetic form, not the one an operator thinks in.
                  Languages differ on how to phrase a discount, so each translation
                  decides; no string is assembled here. */}
              {label && (
                <div className="text-base text-kumo-subtle">{t(label.key, label.params)}</div>
              )}
            </div>
          </div>
          <FormRow className="mt-5 sm:grid-cols-2 lg:grid-cols-[minmax(12rem,1fr)_minmax(10rem,1fr)_minmax(9rem,.6fr)]">
            <FormRow.Item>
              <Field
                label={t("gwBillingMode")}
                hint={staffRole !== "superadmin" ? t("gwSuperadminFreeMode") : undefined}
              >
                <Select
                  disabled={staffRole !== "superadmin"}
                  value={form.billingMode}
                  onValueChange={(value) =>
                    setForm({ ...form, billingMode: value === "free" ? "free" : "paid" })
                  }
                  items={[
                    { value: "paid", label: t("gwBillingPaid") },
                    { value: "free", label: t("gwBillingFree") },
                  ]}
                />
              </Field>
            </FormRow.Item>
            <AdjustmentEditor
              inputId="pricing-adjustment-percent"
              mode={form.adjustmentMode}
              percent={form.adjustmentPercent}
              onChange={(mode, percent) =>
                setForm({ ...form, adjustmentMode: mode, adjustmentPercent: percent })
              }
            />
          </FormRow>
          {form.billingMode === "free" && (
            <Alert variant="info">{t("gwFreeRetainsOfficialRates")}</Alert>
          )}
        </div>

        <DataTable caption={t("gwPriceFormula")} className="min-w-[44rem]">
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head>{t("gwBillingItem")}</DataTable.Head>
              <DataTable.Head>{t("gwOfficialPrice")}</DataTable.Head>
              {/* There is no adjustment column: it held the same constant on all four
                  rows, and the same number already appears at the top of the card. A
                  quantity that does not vary by row, placed in a column, says one thing
                  five times. */}
              <DataTable.Head className="text-right">{t("gwPublicPrice")}</DataTable.Head>
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            {RATE_ROWS.map(({ key, label }) => {
              const value = form.rates[key];
              const missing =
                form.billingMode === "paid" && (value == null || !DECIMAL_RATE.test(value));
              const shown = multiplyRate(value, multiplier ?? 10_000);
              // When nothing can be computed — the list price is empty or invalid — no
              // dash is shown: this cell is an input, and a placeholder-looking dash
              // reads as content waiting to be overwritten.
              const publicShown = shown === "—" ? "" : shown;
              return (
                <DataTable.Row key={key}>
                  <DataTable.Cell className="font-medium">{label}</DataTable.Cell>
                  <DataTable.Cell>
                    <div className="relative max-w-56">
                      <span className="pointer-events-none absolute inset-y-0 left-3 flex items-center text-kumo-subtle">
                        $
                      </span>
                      <Input
                        id={`pricing-rate-${key}`}
                        aria-label={`${label} ${t("gwOfficialPrice")}`}
                        aria-invalid={missing}
                        className="pl-7 font-mono"
                        inputMode="decimal"
                        placeholder={t("gwMissingRate")}
                        value={value ?? ""}
                        onChange={(event) =>
                          setForm({
                            ...form,
                            rates: { ...form.rates, [key]: event.target.value || null },
                          })
                        }
                      />
                    </div>
                    {missing && (
                      <p className="mt-1 text-base text-kumo-danger">{t("gwRateRequired")}</p>
                    )}
                  </DataTable.Cell>
                  {/* **The published price can be entered directly.** The number in an
                      operator's head is usually "what do I want to sell this for", and
                      read-only this cell left them reversing a multiplier on a
                      calculator. Typing here derives it instead — and **there is one
                      multiplier, shared by all four rows**, so editing any row moves the
                      other three. That has to be said in the hint rather than
                      discovered. Under free pricing the published price is always zero
                      and is not an editable quantity at all. */}
                  <DataTable.Cell className="text-right">
                    {form.billingMode === "free" ? (
                      <span className="font-mono text-base font-semibold">$0</span>
                    ) : (
                      <div className="relative ml-auto max-w-56">
                        <span className="pointer-events-none absolute inset-y-0 left-3 flex items-center text-kumo-subtle">
                          $
                        </span>
                        <Input
                          id={`pricing-public-${key}`}
                          aria-label={`${label} ${t("gwPublicPrice")}`}
                          className="pl-7 text-right font-mono font-semibold"
                          inputMode="decimal"
                          value={publicDraft?.key === key ? publicDraft.text : publicShown}
                          onChange={(event) => setPublicRate(key, event.target.value)}
                        />
                      </div>
                    )}
                  </DataTable.Cell>
                </DataTable.Row>
              );
            })}
          </DataTable.Body>
        </DataTable>
      </Card>

      <Card id="model-price-source" className="max-w-3xl scroll-mt-6 space-y-4">
        <SectionHeading>{t("gwPriceProvenance")}</SectionHeading>
        <FormRow className="sm:grid-cols-2 lg:grid-cols-3">
          <FormRow.Item>
            <Field
              label={t("gwSourceName")}
              htmlFor="pricing-source-name"
              error={form.sourceName.trim() === "" ? t("gwSourceNameRequired") : undefined}
            >
              <Input
                id="pricing-source-name"
                required
                value={form.sourceName}
                onChange={(event) => setForm({ ...form, sourceName: event.target.value })}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field label={t("gwSourceUrl")} htmlFor="pricing-source-url" hint={t("optional")}>
              <Input
                id="pricing-source-url"
                type="url"
                value={form.sourceUrl}
                onChange={(event) => setForm({ ...form, sourceUrl: event.target.value })}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field
              label={t("gwCheckedAt")}
              htmlFor="pricing-checked-at"
              error={checkedAtInvalid ? t("gwCheckedAtInvalid") : undefined}
            >
              <Input
                id="pricing-checked-at"
                type="datetime-local"
                required
                value={form.checkedAt}
                onChange={(event) => setForm({ ...form, checkedAt: event.target.value })}
              />
            </Field>
          </FormRow.Item>
        </FormRow>
        <Field
          label={t("gwPriceChangeReason")}
          htmlFor="pricing-change-reason"
          error={reasonInvalid ? t("required") : undefined}
        >
          <Textarea
            id="pricing-change-reason"
            required
            value={form.reason}
            onChange={(event) => setForm({ ...form, reason: event.target.value })}
          />
        </Field>
      </Card>

      <Card id="model-price-advanced" className="max-w-3xl scroll-mt-6 space-y-4">
        <button
          type="button"
          className="flex w-full items-center justify-between gap-4 text-left focus-visible:rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-kumo-focus"
          aria-expanded={advanced}
          onClick={() => setAdvanced(!advanced)}
        >
          <SectionHeading>
            {t("gwAdvancedPricing", { count: form.dimensions.length + form.tools.length })}
          </SectionHeading>
          <span aria-hidden="true" className="text-kumo-subtle">
            {advanced ? "−" : "+"}
          </span>
        </button>
        {advanced && (
          <>
            <AdvancedRatesEditor
              rows={form.dimensions}
              baseRates={form.rates}
              multiplier={multiplier ?? 10_000}
              onChange={(dimensions) => setForm({ ...form, dimensions })}
            />
            <ToolRatesEditor rows={form.tools} onChange={(tools) => setForm({ ...form, tools })} />
          </>
        )}
      </Card>

      <PageActionDock
        status={
          invalid ? t("gwInvalidChanges") : dirty ? t("gwUnsavedChanges") : t("gwConfigUpToDate")
        }
      >
        <Button
          variant="ghost"
          disabled={!dirty || save.isPending}
          onClick={() => {
            const state = pricingState(resource);
            setForm(state);
            setBaseline(JSON.stringify(state));
            setPublicDraft(null);
            setAcknowledged([]);
            setConflict(null);
            save.reset();
          }}
        >
          {t("gwDiscardChanges")}
        </Button>
        {/* The footer holds only discard and save: saving is what takes effect, so
            there is no draft to save and nothing to preview and publish. */}
        <Button
          loading={save.isPending}
          disabled={!dirty || save.isPending}
          onClick={() => {
            const switchingFree = form.billingMode === "free" && resource?.billing_mode !== "free";
            if (switchingFree) setPendingFreeAction("save");
            else void savePricing();
          }}
        >
          {t("save")}
        </Button>
      </PageActionDock>

      <ConfirmDialog
        open={pendingFreeAction != null}
        onOpenChange={(open) => !open && setPendingFreeAction(null)}
        destructive
        title={t("gwFreeConfirmTitle")}
        description={t("gwFreeConfirmBody", { slug: model.slug })}
        confirmLabel={t("save")}
        pending={save.isPending}
        onConfirm={() => {
          void savePricing().then((saved) => {
            if (saved) setPendingFreeAction(null);
          });
        }}
      />
    </div>
  );
}

function ModelPricingConflictPanel({
  conflict,
  onKeepLocal,
  onUseCurrent,
}: {
  conflict: ModelPricingConflict;
  onKeepLocal: () => void;
  onUseCurrent: () => void;
}) {
  const { t } = useI18n();
  const current = pricingState(conflict.current);
  const localMultiplier = adjustmentBps(
    conflict.local.adjustmentMode,
    conflict.local.adjustmentPercent,
  );
  const currentMultiplier = adjustmentBps(current.adjustmentMode, current.adjustmentPercent);
  return (
    <Card tone="danger" className="space-y-4">
      <div>
        <SectionHeading>{t("gwPricingConflictTitle")}</SectionHeading>
        <p className="mt-1 text-base text-kumo-subtle">{t("gwPricingConflictBody")}</p>
      </div>
      <div className="overflow-hidden rounded-lg ring ring-kumo-line">
        <DataTable caption={t("gwPricingConflictTitle")}>
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head>{t("gwBillingItem")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwLocalChanges")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwCurrentServerValue")}</DataTable.Head>
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            <DataTable.Row>
              <DataTable.Cell>{t("gwEffectiveMultiplier")}</DataTable.Cell>
              <DataTable.Cell className="text-right font-mono">
                {localMultiplier == null ? "—" : `× ${(localMultiplier / 10_000).toFixed(4)}`}
              </DataTable.Cell>
              <DataTable.Cell className="text-right font-mono">
                {currentMultiplier == null ? "—" : `× ${(currentMultiplier / 10_000).toFixed(4)}`}
              </DataTable.Cell>
            </DataTable.Row>
            {RATE_ROWS.map(({ key, label }) => (
              <DataTable.Row key={key}>
                <DataTable.Cell>{label}</DataTable.Cell>
                <DataTable.Cell className="text-right font-mono">
                  {conflict.local.rates[key] ?? "—"}
                </DataTable.Cell>
                <DataTable.Cell className="text-right font-mono">
                  {current.rates[key] ?? "—"}
                </DataTable.Cell>
              </DataTable.Row>
            ))}
          </DataTable.Body>
        </DataTable>
      </div>
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="outline" onClick={onUseCurrent}>
          {t("gwUseCurrentServerValue")}
        </Button>
        <Button onClick={onKeepLocal}>{t("gwKeepLocalChanges")}</Button>
      </div>
    </Card>
  );
}

// A dimension row is identified by all four of its axes. Leaving the context
// band out of the key would report two bands of one bucket as a duplicate,
// which is exactly the configuration long-context pricing is made of.
function dimensionRowKey(row: GatewayStaffTypes.ModelPriceDimensionRate): string {
  return `${row.bucket}:${row.service_tier ?? "standard"}:${row.variant ?? ""}:${
    row.min_input_tokens ?? 0
  }`;
}

// The editor only ever produces non-negative integers, so this guards what
// arrives from elsewhere -- a stored row, or a future caller of this component.
function isValidMinInputTokens(value: number | undefined): boolean {
  return value === undefined || (Number.isInteger(value) && value >= 0);
}

function AdvancedRatesEditor({
  rows,
  baseRates,
  multiplier,
  onChange,
}: {
  rows: GatewayStaffTypes.ModelPriceDimensionRate[];
  baseRates: GatewayStaffTypes.DraftTokenRatesUSDPerM;
  multiplier: number;
  onChange: (rows: GatewayStaffTypes.ModelPriceDimensionRate[]) => void;
}) {
  const { t } = useI18n();
  const bucketLabel: Record<string, string> = {
    input: "Input",
    output: "Output",
    cache_read: "Cache Read",
    cache_write: "Cache Write",
    audio_input: t("gwAudioInput"),
    audio_output: t("gwAudioOutput"),
  };
  return (
    <div className="space-y-3">
      <p className="text-base text-kumo-subtle">{t("gwAdvancedPricingHint")}</p>
      <p className="text-base text-kumo-subtle">{t("gwMinInputTokensHint")}</p>
      {rows.map((row, index) => {
        const inheritedKey = row.bucket.replace("audio_", "") as RateKey;
        const inherited = inheritedKey in baseRates ? baseRates[inheritedKey] : null;
        const dimensionKey = dimensionRowKey(row);
        const duplicate =
          rows.findIndex((candidate) => dimensionRowKey(candidate) === dimensionKey) !== index;
        const invalidRate = !DECIMAL_RATE.test(row.rate_usd_per_m);
        const invalidMinInput = !isValidMinInputTokens(row.min_input_tokens);
        return (
          <FormRow
            key={`${index}-${row.bucket}`}
            // Six columns since the context band joined the row. The two
            // flexible ones give up a rem each so the row's minimum width grows
            // by 5rem rather than 9rem: at the xl breakpoint the content column
            // has little slack left, and a wider minimum here overflows the
            // card rather than wrapping.
            className="rounded-lg border border-kumo-line p-4 xl:grid-cols-[minmax(9rem,1fr)_minmax(9rem,1fr)_8rem_7rem_10rem_minmax(15rem,auto)]"
          >
            <FormRow.Item>
              <Field label={t("gwBillingItem")}>
                <Select
                  value={row.bucket}
                  onValueChange={(value) => {
                    const copy = [...rows];
                    copy[index] = {
                      ...row,
                      bucket: value as GatewayStaffTypes.ModelPriceDimensionRateBucket,
                    };
                    onChange(copy);
                  }}
                  items={Object.entries(bucketLabel).map(([value, label]) => ({ value, label }))}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              <Field label={t("gwServiceTier")}>
                <Select
                  value={row.service_tier ?? "standard"}
                  onValueChange={(value) => {
                    const copy = [...rows];
                    copy[index] = {
                      ...row,
                      service_tier: value as GatewayStaffTypes.ModelPriceDimensionRateServiceTier,
                    };
                    onChange(copy);
                  }}
                  items={[
                    { value: "standard", label: t("gwTierStandard") },
                    { value: "priority", label: t("gwTierPriority") },
                    { value: "batch", label: t("gwTierBatch") },
                  ]}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              <Field label={t("gwCacheTtl")}>
                <Select
                  value={row.variant ?? "none"}
                  onValueChange={(value) => {
                    const copy = [...rows];
                    copy[index] = {
                      ...row,
                      variant: value && value !== "none" ? value : undefined,
                    };
                    onChange(copy);
                  }}
                  items={[
                    { value: "none", label: t("gwCacheTtlNone") },
                    { value: "5m", label: t("gwCacheTtlFiveMinutes") },
                    { value: "1h", label: t("gwCacheTtlOneHour") },
                  ]}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              <Field
                label={t("gwMinInputTokens")}
                htmlFor={`dimension-min-input-${index}`}
                error={invalidMinInput ? t("gwMinInputTokensInvalid") : undefined}
              >
                <Input
                  id={`dimension-min-input-${index}`}
                  aria-label={t("gwMinInputTokens")}
                  aria-invalid={invalidMinInput}
                  inputMode="numeric"
                  value={String(row.min_input_tokens ?? 0)}
                  onChange={(event) => {
                    const copy = [...rows];
                    // Non-digits are dropped as they are typed rather than
                    // parsed into NaN. Parsing would put the string "NaN" in
                    // the box, and 0 is a meaningful band here, so neither
                    // "keep the bad value" nor "fall back to 0" is a safe
                    // reading of a stray keystroke.
                    const digits = event.target.value.replace(/\D/g, "");
                    copy[index] = { ...row, min_input_tokens: digits === "" ? 0 : Number(digits) };
                    onChange(copy);
                  }}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              <Field
                label={t("gwOverridePrice")}
                htmlFor={`dimension-rate-${index}`}
                error={
                  invalidRate
                    ? t("gwAdvancedRateInvalid")
                    : duplicate
                      ? t("gwAdvancedRateDuplicate")
                      : undefined
                }
              >
                <Input
                  id={`dimension-rate-${index}`}
                  aria-label={t("gwOverridePrice")}
                  aria-invalid={invalidRate || duplicate}
                  inputMode="decimal"
                  value={row.rate_usd_per_m}
                  onChange={(event) => {
                    const copy = [...rows];
                    copy[index] = { ...row, rate_usd_per_m: event.target.value };
                    onChange(copy);
                  }}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Actions className="justify-between lg:flex-nowrap">
              <div className="text-base text-kumo-subtle">
                <div>
                  {t("gwInheritedPrice")}: {inherited ?? "—"}
                </div>
                <div>
                  {t("gwEffectivePrice")}: {multiplyRate(row.rate_usd_per_m, multiplier)}
                </div>
              </div>
              <Button variant="ghost" onClick={() => onChange(rows.filter((_, i) => i !== index))}>
                {t("remove")}
              </Button>
            </FormRow.Actions>
          </FormRow>
        );
      })}
      <FormActions>
        <Button
          variant="outline"
          onClick={() =>
            onChange([
              ...rows,
              {
                bucket: "cache_write",
                service_tier: "standard",
                variant: "5m",
                min_input_tokens: 0,
                rate_usd_per_m: "0",
              },
            ])
          }
        >
          {t("gwAddOverride")}
        </Button>
      </FormActions>
    </div>
  );
}

function ToolRatesEditor({
  rows,
  onChange,
}: {
  rows: GatewayStaffTypes.ModelPriceToolRate[];
  onChange: (rows: GatewayStaffTypes.ModelPriceToolRate[]) => void;
}) {
  const { t } = useI18n();
  const normalizedNames = rows.map((row) => row.tool.trim()).filter(Boolean);
  const duplicateNames = new Set(
    normalizedNames.filter((name, index) => normalizedNames.indexOf(name) !== index),
  );
  return (
    <div className="space-y-3 border-t border-kumo-line pt-4">
      <div>
        <SectionHeading as="h3">{t("gwToolPriceTitle")}</SectionHeading>
        <p className="mt-1 text-base text-kumo-subtle">{t("gwVersionedToolPriceHint")}</p>
      </div>
      {rows.length === 0 && (
        <InlineEmpty title={t("gwToolPriceNone")} description={t("gwToolPriceNoneHint")} />
      )}
      {rows.map((row, index) => {
        const duplicate = duplicateNames.has(row.tool.trim());
        const nameTooLong = row.tool.trim().length > 200;
        const invalidRate = !DECIMAL_RATE.test(row.rate_usd_per_call);
        return (
          <FormRow
            key={`${index}-${row.tool}`}
            className="rounded-lg border border-kumo-line p-4 sm:grid-cols-2 lg:grid-cols-[minmax(12rem,1fr)_minmax(10rem,.6fr)_auto]"
          >
            <FormRow.Item>
              <Field
                label={t("gwToolName")}
                htmlFor={`tool-name-${index}`}
                error={
                  row.tool.trim() === ""
                    ? t("gwToolNameRequired")
                    : nameTooLong
                      ? t("gwToolNameTooLong")
                      : duplicate
                        ? t("gwToolNameDuplicate")
                        : undefined
                }
              >
                <Input
                  id={`tool-name-${index}`}
                  aria-label={t("gwToolName")}
                  aria-invalid={row.tool.trim() === "" || nameTooLong || duplicate}
                  value={row.tool}
                  onChange={(event) => {
                    const copy = [...rows];
                    copy[index] = { ...row, tool: event.target.value };
                    onChange(copy);
                  }}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Item>
              <Field
                label={t("gwToolPricePerCall")}
                htmlFor={`tool-rate-${index}`}
                error={invalidRate ? t("gwToolRateInvalid") : undefined}
              >
                <Input
                  id={`tool-rate-${index}`}
                  aria-label={t("gwToolPricePerCall")}
                  aria-invalid={invalidRate}
                  inputMode="decimal"
                  value={row.rate_usd_per_call}
                  onChange={(event) => {
                    const copy = [...rows];
                    copy[index] = { ...row, rate_usd_per_call: event.target.value };
                    onChange(copy);
                  }}
                />
              </Field>
            </FormRow.Item>
            <FormRow.Actions>
              <Button
                variant="ghost"
                onClick={() => onChange(rows.filter((_, rowIndex) => rowIndex !== index))}
              >
                {t("remove")}
              </Button>
            </FormRow.Actions>
          </FormRow>
        );
      })}
      <FormActions>
        <Button
          variant="outline"
          onClick={() => onChange([...rows, { tool: "", rate_usd_per_call: "0" }])}
        >
          {t("gwToolAddRow")}
        </Button>
      </FormActions>
    </div>
  );
}

function ModelRoutesPanel({ model }: { model: GatewayStaffTypes.GatewayModel }) {
  const { t } = useI18n();
  const routes = gatewayStaffApi.useListGatewayRoutes(model.id);
  const [editingProviders, setEditingProviders] = useState(false);
  const refresh = () => void routes.refetch();
  // There is no "advanced" card: it redrew each route's provider, priority and weight
  // from the table above, showed headers and limits as the single words "configured" or
  // "inherited", and still sent you back to that table's inline edit to change them —
  // offering neither new information nor an action. Those two are a column on the route
  // table now.
  return (
    <div className="space-y-6">
      <Card className="space-y-4">
        <div className="flex items-start justify-between gap-4">
          <div>
            <SectionHeading>{t("gwRoutingCapabilities")}</SectionHeading>
            <p className="text-base text-kumo-subtle">{t("gwRoutingCapabilitiesHint")}</p>
          </div>
          {/* Membership is edited as a whole set; attributes stay on the table's inline
              row editor. Membership is a boolean and an attribute is not, and the two
              shapes do not belong on one surface. */}
          <Button variant="outline" onClick={() => setEditingProviders(true)}>
            {t("gwEditModelProviders")}
          </Button>
        </div>
        <RoutePanel model={model} onChanged={refresh} />
      </Card>
      <ModelProvidersDialog
        open={editingProviders}
        onOpenChange={setEditingProviders}
        model={model}
        onSaved={refresh}
      />
    </div>
  );
}
