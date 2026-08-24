import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import {
  ApiError,
  gatewayStaffApi,
  getResponseETag,
  type GatewayStaffTypes,
  apiErrorMessage,
  apiErrorStatus,
} from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
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
  LoadingState,
  PageHeader,
  RecordPage,
  resolveNavValue,
  RowTitleLink,
  SectionHeading,
  Select,
  StatusBadge,
  Textarea,
  useAdminTitle,
  useCursorList,
  useScopedCursor,
  useDebounced,
  Combobox,
  LoadMoreButton,
} from "@fairlb/ui";
import { Outlet, useBlocker, useLocation, useNavigate, useParams } from "@tanstack/react-router";
import { useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useState } from "react";
import { adjustmentLabel } from "./adjustment-label";
import { multiplyRate } from "./pricing-math";
import { useCurrentStaffRole, useRecordBreadcrumb } from "./host";

type AdjustmentMode = "original" | "discount" | "markup";

function adjustmentFromBps(bps: number): { mode: AdjustmentMode; percent: string } {
  if (bps === 10_000) return { mode: "original", percent: "0" };
  const value = Math.abs(bps - 10_000) / 100;
  return { mode: bps < 10_000 ? "discount" : "markup", percent: String(value) };
}

function adjustmentBps(mode: AdjustmentMode, value: string): number | null {
  if (mode === "original") return 10_000;
  if (!/^(0|[1-9][0-9]*)(\.[0-9]{1,2})?$/.test(value.trim())) return null;
  const [whole, fraction = ""] = value.trim().split(".");
  const amount = Number(whole) * 100 + Number(fraction.padEnd(2, "0"));
  const result = mode === "discount" ? 10_000 - amount : 10_000 + amount;
  return result >= 1 && result <= 100_000 ? result : null;
}

/**
 * How a multiplier is displayed.
 *
 * Not as `× 0.8000`: that is the arithmetic form of the stored basis points, not the
 * form an operator thinks in. The data model was always right — "group A at list
 * price, group B at twenty percent off" is exactly what it holds — what was missing
 * was the wording here. The admin interface says "list price / 20% off / 20% markup"
 * and never exposes basis points.
 */
function useMultiplierLabel(): (bps?: number | null) => string {
  const { t } = useI18n();
  return (bps) => {
    const label = adjustmentLabel(bps);
    // null renders as a dash rather than inventing "list price": **not knowing** and
    // **list price** are two different facts.
    return label ? t(label.key, label.params) : "—";
  };
}

function ImpactValue({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-kumo-line bg-kumo-recessed p-3">
      <div className="text-base text-kumo-subtle">{label}</div>
      <div className="mt-1 font-mono text-lg font-semibold">{value}</div>
    </div>
  );
}

export function GatewayPricingPlansPage() {
  const { t } = useI18n();
  const multiplierLabel = useMultiplierLabel();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [search, setSearch] = useState("");
  const [generation, setGeneration] = useState(0);
  useAdminTitle(t("navGatewayPricingPlans"));

  // 搜索发到服务端（ADR-0187/0191）。此前这一页把**全部**方案拉下来在客户端过滤——
  // 这条列表原先根本没有上界，而一旦分页，本地过滤就只搜得到第一页。
  const settledSearch = useDebounced(search, 250);
  // 游标只对它被铸出时的搜索词有效（useScopedCursor）
  const [cursor, setCursor] = useScopedCursor(`${settledSearch}|${generation}`);
  const plans = gatewayStaffApi.useListGatewayPricingPlans(
    { ...(settledSearch ? { q: settledSearch } : {}), ...(cursor ? { cursor } : {}) },
    // keepPreviousData 是正确性不是优化：搜索词一变查询键就变，没有它 data 先变回
    // undefined，正在被打字的搜索框会连同整张表一起被换成加载态。
    { query: { placeholderData: keepPreviousData } },
  );
  const { items: filtered, nextCursor } = useCursorList<GatewayStaffTypes.PricingPlan>(
    plans,
    (p) => p.id,
    // 搜索词一变就丢掉累积：混着两个搜索词结果的列表比空列表更难解释。
    `${settledSearch}|${generation}`,
  );
  // 改动之后回第一页并清空缓存（ADR-0185）：累积表按 id 去重且不替换已见的行，
  // 不清就会拿旧行盖住新值。
  const refreshPlans = () => {
    setCursor(undefined);
    setGeneration((g) => g + 1);
    void queryClient.resetQueries({
      queryKey: gatewayStaffApi.getListGatewayPricingPlansQueryKey(),
    });
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("navGatewayPricingPlans")}
        description={t("gwPricingPlanNotSubscription")}
        actions={<Button onClick={() => setCreating(true)}>{t("gwCreatePricingPlan")}</Button>}
      />
      {plans.isError && <Alert>{apiErrorMessage(plans.error)}</Alert>}
      <Card className="space-y-3">
        {/* Filters live in the list card's toolbar. The card has no heading of its
            own, because it would only repeat the page title. */}
        <div className="max-w-md">
          <Field label={t("gwSearchPricingPlans")} htmlFor="plan-search">
            <Input
              id="plan-search"
              type="search"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
          </Field>
        </div>
        <DataTable caption={t("navGatewayPricingPlans")}>
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head>{t("name")}</DataTable.Head>
              <DataTable.Head>{t("gwDefaultAdjustment")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("gwColOrgs")}</DataTable.Head>
              {/* The status here is whether the plan can still be assigned to new
                  customers — not a draft-versus-published state, of which there is
                  none. There is no actions column: opening the detail page is the
                  job of the plan name at the head of the row. */}
              <DataTable.Head>{t("gwColStatus")}</DataTable.Head>
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            {filtered.map((plan) => (
              <DataTable.Row key={plan.id} interactive>
                {/* `relative` is what lets the row title link cover the whole cell. */}
                <DataTable.Cell className="relative">
                  <RowTitleLink
                    to="/gateway/pricing-plans/$pricingPlanId"
                    params={{ pricingPlanId: plan.id }}
                  >
                    {plan.name}
                  </RowTitleLink>
                  <div className="mt-1 font-mono text-[0.9em] text-kumo-subtle">{plan.slug}</div>
                </DataTable.Cell>
                <DataTable.Cell className="font-mono">
                  {multiplierLabel(plan.default_adjustment?.multiplier_bps)}
                </DataTable.Cell>
                <DataTable.Cell className="text-right tabular-nums">
                  {plan.org_count}
                </DataTable.Cell>
                <DataTable.Cell>
                  <div className="flex flex-wrap gap-1.5">
                    <StatusBadge tone={plan.status === "active" ? "success" : "neutral"}>
                      {plan.status === "active" ? t("gwEnabled") : t("gwDisabledDone")}
                    </StatusBadge>
                    {plan.is_default && (
                      <StatusBadge tone="neutral">{t("gwDefaultPlan")}</StatusBadge>
                    )}
                  </div>
                </DataTable.Cell>
              </DataTable.Row>
            ))}
            {filtered.length === 0 && (
              <DataTable.Row>
                <DataTable.Cell colSpan={4}>
                  <InlineEmpty
                    title={settledSearch ? t("gwNoPricingPlanMatch") : t("gwNoPricingPlans")}
                  />
                </DataTable.Cell>
              </DataTable.Row>
            )}
          </DataTable.Body>
        </DataTable>
        <LoadMoreButton
          onClick={nextCursor ? () => setCursor(nextCursor) : undefined}
          pending={plans.isFetching}
          label={t("loadMore")}
        />
      </Card>
      <CreatePricingPlanDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(plan) => {
          refreshPlans();
          void navigate({
            to: "/gateway/pricing-plans/$pricingPlanId",
            params: { pricingPlanId: plan.id },
          });
        }}
      />
    </div>
  );
}

function CreatePricingPlanDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (plan: GatewayStaffTypes.PricingPlan) => void;
}) {
  const { t } = useI18n();
  const create = gatewayStaffApi.useCreateGatewayPricingPlan();
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [mode, setMode] = useState<AdjustmentMode>("original");
  const [percent, setPercent] = useState("0");
  const bps = adjustmentBps(mode, percent);
  return (
    <FormDialog
      size="lg"
      open={open}
      onOpenChange={onOpenChange}
      title={t("gwCreatePricingPlan")}
      error={create.isError ? apiErrorMessage(create.error) : undefined}
      submitLabel={t("gwCreatePricingPlan")}
      submitDisabled={!slug.trim() || !name.trim() || bps == null}
      pending={create.isPending}
      onSubmit={() => {
        if (bps == null) return;
        create.mutate(
          {
            data: {
              slug: slug.trim(),
              name: name.trim(),
              ...(description.trim() ? { description: description.trim() } : {}),
              default_adjustment: { multiplier_bps: bps },
            },
          },
          {
            onSuccess: (plan) => {
              onOpenChange(false);
              onCreated(plan);
            },
          },
        );
      }}
    >
      <FormRow className="sm:grid-cols-2">
        <FormRow.Item>
          <Field label={t("gwPlanSlug")} htmlFor="new-plan-slug">
            <Input
              id="new-plan-slug"
              required
              value={slug}
              onChange={(event) => setSlug(event.target.value)}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field label={t("name")} htmlFor="new-plan-name">
            <Input
              id="new-plan-name"
              required
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
        </FormRow.Item>
      </FormRow>
      <Field label={t("gwPlanDescription")} htmlFor="new-plan-description" hint={t("optional")}>
        <Textarea
          id="new-plan-description"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
        />
      </Field>
      <PlanAdjustmentEditor
        mode={mode}
        percent={percent}
        onChange={(m, p) => {
          setMode(m);
          setPercent(p);
        }}
      />
    </FormDialog>
  );
}

type PricingPlanContextValue = {
  plan: GatewayStaffTypes.PricingPlan;
  refetchPlan: () => Promise<GatewayStaffTypes.PricingPlan | undefined>;
  setDefaultDirty: (dirty: boolean) => void;
};

const PricingPlanContext = createContext<PricingPlanContextValue | null>(null);

function usePricingPlanRecord(): PricingPlanContextValue {
  const value = useContext(PricingPlanContext);
  if (!value)
    throw new Error("Pricing plan pages must be rendered inside GatewayPricingPlanLayout");
  return value;
}

export function GatewayPricingPlanLayout() {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const pathname = useLocation({ select: (location) => location.pathname });
  const staffRole = useCurrentStaffRole();
  const { pricingPlanId = "" } = useParams({ strict: false }) as { pricingPlanId?: string };
  const plan = gatewayStaffApi.useGetGatewayPricingPlan(pricingPlanId);
  const getUrl = gatewayStaffApi.getGetGatewayPricingPlanUrl(pricingPlanId);
  const [copyOpen, setCopyOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteConflict, setDeleteConflict] = useState<string | null>(null);
  const [defaultDirty, setDefaultDirty] = useState(false);
  const blocker = useBlocker({
    shouldBlockFn: () => defaultDirty,
    enableBeforeUnload: defaultDirty,
    withResolver: true,
  });
  const etag = getResponseETag(getUrl) ?? `"${plan.data?.updated_at ?? "current"}"`;
  const remove = gatewayStaffApi.useDeleteGatewayPricingPlan({
    request: { headers: { "If-Match": etag } },
  });
  useAdminTitle(plan.data?.name);
  const pendingLabel =
    plan.isPending || plan.isFetching ? t("loading") : t("gwPricingPlanNotFound");
  const breadcrumb = useRecordBreadcrumb(plan.data?.name ?? pendingLabel);
  const basePath = `/gateway/pricing-plans/${pricingPlanId}`;
  const aspects = [
    { value: "default", label: t("gwDefaultAdjustment"), href: basePath },
    { value: "models", label: t("gwModelExceptions"), href: `${basePath}/models` },
  ];
  const active = resolveNavValue(aspects, pathname);

  return (
    <RecordPage
      header={
        <PageHeader
          breadcrumbs={breadcrumb}
          // See the note on the provider record page: both of these say what the
          // plan *is*, so they belong to the identity rather than to the row of
          // things a reader can press.
          title={
            <span className="flex flex-wrap items-center gap-3">
              {plan.data?.name ?? pendingLabel}
              {plan.data && (
                <span className="font-mono text-[0.9em] text-kumo-subtle">{plan.data.slug}</span>
              )}
              {plan.data?.is_default && (
                <StatusBadge tone="neutral">{t("gwDefaultPlan")}</StatusBadge>
              )}
              {plan.data && (
                <StatusBadge tone={plan.data.status === "active" ? "success" : "neutral"}>
                  {plan.data.status === "active" ? t("gwEnabled") : t("gwDisabledDone")}
                </StatusBadge>
              )}
            </span>
          }
          description={plan.data?.description || undefined}
          actions={
            plan.data && (
              <>
                <Button variant="outline" onClick={() => setCopyOpen(true)}>
                  {t("gwCopyPlan")}
                </Button>
                {staffRole === "superadmin" && !plan.data.is_default && (
                  <Button
                    variant="destructive"
                    disabled={defaultDirty}
                    onClick={() => {
                      setDeleteConflict(null);
                      setDeleteOpen(true);
                    }}
                  >
                    {t("gwDeletePlan")}
                  </Button>
                )}
              </>
            )
          }
          recordNav={{ value: active, items: aspects }}
        />
      }
    >
      {deleteConflict && <Alert>{deleteConflict}</Alert>}
      <div className="min-w-0">
        {!plan.data && (plan.isPending || plan.isFetching) ? (
          <LoadingState label={t("loading")} />
        ) : apiErrorStatus(plan.error) === 404 ? (
          <InlineEmpty title={t("gwPricingPlanNotFound")} />
        ) : plan.isError ? (
          <Alert>{apiErrorMessage(plan.error)}</Alert>
        ) : plan.data ? (
          <PricingPlanContext.Provider
            value={{
              plan: plan.data,
              refetchPlan: async () => (await plan.refetch()).data,
              setDefaultDirty,
            }}
          >
            <Outlet />
          </PricingPlanContext.Provider>
        ) : (
          <InlineEmpty title={t("gwPricingPlanNotFound")} />
        )}
      </div>
      {plan.data && (
        <CopyPricingPlanDialog plan={plan.data} open={copyOpen} onOpenChange={setCopyOpen} />
      )}
      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        destructive
        title={t("gwDeletePlanTitle")}
        description={t("gwDeletePlanBody", { name: plan.data?.name ?? "" })}
        confirmLabel={t("gwDeletePlan")}
        pending={remove.isPending}
        onConfirm={() =>
          remove.mutate(
            { pricingPlanId },
            {
              onSuccess: async () => {
                setDeleteOpen(false);
                await queryClient.invalidateQueries({
                  queryKey: gatewayStaffApi.getListGatewayPricingPlansQueryKey(),
                });
                toasts.add({ variant: "success", title: t("gwPricingPlanDeleted") });
                void navigate({ to: "/gateway/pricing-plans" });
              },
              onError: async (error) => {
                setDeleteOpen(false);
                if (error instanceof ApiError && error.status === 412) {
                  await plan.refetch();
                  setDeleteConflict(t("gwDeletePlanChanged"));
                  return;
                }
                if (error instanceof ApiError && error.status === 409) {
                  setDeleteConflict(t("gwDeletePlanReferenced"));
                  return;
                }
                setDeleteConflict(apiErrorMessage(error));
              },
            },
          )
        }
      />
      <ConfirmDialog
        open={blocker.status === "blocked"}
        onOpenChange={(open) => !open && blocker.reset?.()}
        destructive={false}
        title={t("gwLeaveUnsavedTitle")}
        description={t("gwLeaveUnsavedBody")}
        confirmLabel={t("gwLeaveUnsaved")}
        onConfirm={() => {
          setDefaultDirty(false);
          blocker.proceed?.();
        }}
      />
    </RecordPage>
  );
}

export function GatewayPricingPlanDefaultPage() {
  const { t } = useI18n();
  const multiplierLabel = useMultiplierLabel();
  const { plan, refetchPlan, setDefaultDirty } = usePricingPlanRecord();
  const savedBps = plan.default_adjustment?.multiplier_bps ?? 10_000;
  const initial = adjustmentFromBps(savedBps);
  const [mode, setMode] = useState<AdjustmentMode>(initial.mode);
  const [percent, setPercent] = useState(initial.percent);
  const [loadedBps, setLoadedBps] = useState(savedBps);
  const [reason, setReason] = useState("");
  const [conflict, setConflict] = useState<{ localBps: number; currentBps: number } | null>(null);
  const bps = adjustmentBps(mode, percent);
  const dirty = bps == null || bps !== savedBps;
  const getUrl = gatewayStaffApi.getGetGatewayPricingPlanUrl(plan.id);
  const etag = getResponseETag(getUrl) ?? `"${plan.updated_at}"`;
  const save = gatewayStaffApi.useUpdateGatewayPricingPlan({
    request: { headers: { "If-Match": etag } },
  });

  useEffect(() => setDefaultDirty(dirty), [dirty, setDefaultDirty]);
  useEffect(() => {
    if (savedBps === loadedBps) return;
    const value = adjustmentFromBps(savedBps);
    setMode(value.mode);
    setPercent(value.percent);
    setLoadedBps(savedBps);
  }, [loadedBps, savedBps]);

  const discard = () => {
    const value = adjustmentFromBps(savedBps);
    setMode(value.mode);
    setPercent(value.percent);
    setReason("");
    setConflict(null);
  };
  const submit = async () => {
    if (bps == null || !reason.trim()) return;
    setConflict(null);
    try {
      await save.mutateAsync({
        pricingPlanId: plan.id,
        data: { default_adjustment: { multiplier_bps: bps }, reason: reason.trim() },
      });
      setReason("");
      await refetchPlan();
    } catch (error) {
      if (error instanceof ApiError && (error.status === 409 || error.status === 412)) {
        const latest = await refetchPlan();
        const currentBps = latest?.default_adjustment?.multiplier_bps ?? 10_000;
        setLoadedBps(currentBps);
        const local = adjustmentFromBps(bps);
        setMode(local.mode);
        setPercent(local.percent);
        setConflict({ localBps: bps, currentBps });
      }
    }
  };

  return (
    <div className="max-w-3xl space-y-4">
      {save.isError && !conflict && <Alert>{apiErrorMessage(save.error)}</Alert>}
      {conflict && (
        <Card className="space-y-4">
          <SectionHeading>{t("gwPricingConflictTitle")}</SectionHeading>
          <p className="text-base text-kumo-subtle">{t("gwPlanConflictBody")}</p>
          <div className="grid gap-3 sm:grid-cols-2">
            <ImpactValue label={t("gwLocalChanges")} value={multiplierLabel(conflict.localBps)} />
            <ImpactValue
              label={t("gwCurrentServerValue")}
              value={multiplierLabel(conflict.currentBps)}
            />
          </div>
          <FormActions className="justify-end">
            <Button
              variant="outline"
              onClick={() => {
                const value = adjustmentFromBps(conflict.currentBps);
                setMode(value.mode);
                setPercent(value.percent);
                setLoadedBps(conflict.currentBps);
                save.reset();
                setConflict(null);
              }}
            >
              {t("gwUseCurrentServerValue")}
            </Button>
            <Button
              onClick={() => {
                save.reset();
                setConflict(null);
              }}
            >
              {t("gwKeepLocalChanges")}
            </Button>
          </FormActions>
        </Card>
      )}
      <Card className="space-y-4">
        {/* No heading: it would repeat the nav item directly above it. The hint
            is the card's lead line instead. Same on the model-exceptions face. */}
        <div>
          <p className="text-base text-kumo-subtle">{t("gwDefaultAdjustmentHint")}</p>
        </div>
        <PlanAdjustmentEditor
          mode={mode}
          percent={percent}
          onChange={(nextMode, nextPercent) => {
            setMode(nextMode);
            setPercent(nextPercent);
          }}
        />
        <span className="font-mono text-[0.9em]">{multiplierLabel(bps ?? undefined)}</span>
        <Field label={t("gwPriceChangeReason")} htmlFor="plan-price-reason">
          <Textarea
            id="plan-price-reason"
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </Field>
        <FormActions className="justify-end">
          <Button variant="ghost" disabled={!dirty || save.isPending} onClick={discard}>
            {t("gwDiscardChanges")}
          </Button>
          <Button
            loading={save.isPending}
            disabled={!dirty || bps == null || !reason.trim() || save.isPending}
            onClick={() => void submit()}
          >
            {t("save")}
          </Button>
        </FormActions>
      </Card>
    </div>
  );
}

export function GatewayPricingPlanModelsPage() {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const { plan, refetchPlan } = usePricingPlanRecord();
  const overrides = gatewayStaffApi.useListGatewayPricingPlanModelOverrides(plan.id);
  const getUrl = gatewayStaffApi.getGetGatewayPricingPlanUrl(plan.id);
  const overrideGetUrl = gatewayStaffApi.getListGatewayPricingPlanModelOverridesUrl(plan.id);
  const overrideSaveUrl = gatewayStaffApi.getReplaceGatewayPricingPlanModelOverridesUrl(plan.id);
  const replace = gatewayStaffApi.useReplaceGatewayPricingPlanModelOverrides({
    request: {
      headers: {
        "If-Match":
          getResponseETag(overrideSaveUrl, overrideGetUrl, getUrl) ?? `"${plan.updated_at}"`,
      },
    },
  });
  return (
    <div className="space-y-4">
      {replace.isError && <Alert>{apiErrorMessage(replace.error)}</Alert>}
      <ModelOverridesEditor
        rows={overrides.data?.items ?? []}
        defaultMultiplierBps={plan.default_adjustment?.multiplier_bps ?? 10_000}
        pending={replace.isPending}
        onReplace={(rows) =>
          replace.mutate(
            {
              pricingPlanId: plan.id,
              data: {
                overrides: rows.map((row) => ({
                  model_id: row.model_id,
                  adjustment: row.adjustment,
                })),
              },
            },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("commonSaved") });
                void overrides.refetch();
                void refetchPlan();
              },
            },
          )
        }
      />
    </div>
  );
}

type PlanAdjustmentProps = {
  mode: AdjustmentMode;
  percent: string;
  onChange: (mode: AdjustmentMode, percent: string) => void;
};

function PlanAdjustmentFields({ mode, percent, onChange }: PlanAdjustmentProps) {
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
          error={bps == null ? t("gwAdjustmentInvalid") : undefined}
        >
          <div className="relative">
            <Input
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

function PlanAdjustmentEditor(props: PlanAdjustmentProps) {
  return (
    <FormRow className="sm:grid-cols-2">
      <PlanAdjustmentFields {...props} />
    </FormRow>
  );
}

function ModelOverridesEditor({
  rows,
  defaultMultiplierBps,
  pending,
  onReplace,
}: {
  rows: GatewayStaffTypes.PricingPlanModelOverride[];
  defaultMultiplierBps: number;
  pending: boolean;
  onReplace: (rows: GatewayStaffTypes.PricingPlanModelOverride[]) => void;
}) {
  const { t } = useI18n();
  const multiplierLabel = useMultiplierLabel();
  const [selectedModel, setSelectedModel] = useState("");
  // 「添加一个覆盖价」的候选从搜索来，不再是整份目录（ADR-0189）。
  //
  // 已配置的那些走 `rows`——方案自己的端点，完整，且自带 model_slug，所以它们
  // 不依赖目录是否完整。这里的目录只用来**挑一个还没配过的模型**。
  const [query, setQuery] = useState("");
  const settledQuery = useDebounced(query, 250);
  const catalog = gatewayStaffApi.useListGatewayModels(
    { q: settledQuery },
    { query: { placeholderData: keepPreviousData } },
  );
  const models = catalog.data?.items ?? [];
  const [mode, setMode] = useState<AdjustmentMode>("original");
  const [percent, setPercent] = useState("0");
  const bps = adjustmentBps(mode, percent);
  const matchesDefault = bps != null && bps === defaultMultiplierBps;
  const model = models.find((item) => item.id === selectedModel);
  const available = models.filter(
    (candidate) => !rows.some((row) => row.model_id === candidate.id),
  );
  const publicRates = model?.public_rates ?? null;
  return (
    <Card className="space-y-4">
      <div>
        <p className="text-base text-kumo-subtle">{t("gwModelExceptionsHint")}</p>
      </div>
      {rows.length === 0 ? (
        <InlineEmpty title={t("gwNoModelExceptions")} description={t("gwNoModelExceptionsHint")} />
      ) : (
        <div className="overflow-hidden rounded-lg ring ring-kumo-line">
          <DataTable caption={t("gwModelExceptions")}>
            <DataTable.Header>
              <DataTable.Row>
                <DataTable.Head>{t("gwModel")}</DataTable.Head>
                <DataTable.Head>{t("gwPriceAdjustment")}</DataTable.Head>
                <DataTable.Head>{t("gwPublicPrice")}</DataTable.Head>
                <DataTable.Head>{t("gwCustomerPrice")}</DataTable.Head>
                <DataTable.Head />
              </DataTable.Row>
            </DataTable.Header>
            <DataTable.Body>
              {rows.map((row) => (
                <DataTable.Row key={row.model_id}>
                  <DataTable.Cell className="font-mono">{row.model_slug}</DataTable.Cell>
                  <DataTable.Cell className="font-mono">
                    {multiplierLabel(row.adjustment.multiplier_bps)}
                  </DataTable.Cell>
                  <DataTable.Cell className="font-mono">
                    {row.public_rates
                      ? `${row.public_rates.input} / ${row.public_rates.output}`
                      : "—"}
                  </DataTable.Cell>
                  <DataTable.Cell className="font-mono">
                    {row.effective_customer_rates
                      ? `${row.effective_customer_rates.input} / ${row.effective_customer_rates.output}`
                      : "—"}
                  </DataTable.Cell>
                  <DataTable.Cell className="text-right">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() =>
                        onReplace(rows.filter((item) => item.model_id !== row.model_id))
                      }
                    >
                      {t("remove")}
                    </Button>
                  </DataTable.Cell>
                </DataTable.Row>
              ))}
            </DataTable.Body>
          </DataTable>
        </div>
      )}
      <div className="rounded-lg border border-kumo-line bg-kumo-recessed p-4">
        <FormRow className="sm:grid-cols-2 xl:grid-cols-[minmax(12rem,1fr)_minmax(10rem,1fr)_minmax(9rem,.6fr)_auto]">
          <FormRow.Item>
            <Field label={t("gwAddModelException")} htmlFor="override-model">
              {/* Combobox 而不是 Select：目录只在服务端按 `q` 筛（ADR-0189），
                  一个固定下拉装不下它。已配置的那些走上面的表，不受这里影响。 */}
              <Combobox<{ id: string; slug: string }>
                items={available.map((item) => ({ id: item.id, slug: item.slug }))}
                value={available.find((m) => m.id === selectedModel) ?? null}
                onValueChange={(m) => setSelectedModel(m?.id ?? "")}
                onInputValueChange={(next) => setQuery(next)}
                itemToStringLabel={(m) => m.slug}
                isItemEqualToValue={(a, b) => a.id === b.id}
                autoHighlight
              >
                <Combobox.TriggerInput id="override-model" placeholder={t("gwSearchModel")} />
                <Combobox.Content>
                  <Combobox.List>
                    {(m: { id: string; slug: string }, index: number) => (
                      <Combobox.Item key={m.id} value={m} index={index}>
                        <span className="font-mono">{m.slug}</span>
                      </Combobox.Item>
                    )}
                  </Combobox.List>
                  <Combobox.Empty />
                </Combobox.Content>
              </Combobox>
            </Field>
          </FormRow.Item>
          <PlanAdjustmentFields
            mode={mode}
            percent={percent}
            onChange={(m, p) => {
              setMode(m);
              setPercent(p);
            }}
          />
          <FormRow.Actions>
            <Button
              disabled={!model || bps == null || matchesDefault || pending}
              onClick={() => {
                if (!model || bps == null) return;
                onReplace([
                  ...rows,
                  {
                    model_id: model.id,
                    model_slug: model.slug,
                    model_display_name: model.display_name,
                    adjustment: { multiplier_bps: bps },
                    public_rates: model.public_rates,
                    effective_customer_rates: model.public_rates
                      ? {
                          input: multiplyRate(model.public_rates.input, bps),
                          output: multiplyRate(model.public_rates.output, bps),
                          cache_read: multiplyRate(model.public_rates.cache_read, bps),
                          cache_write: multiplyRate(model.public_rates.cache_write, bps),
                        }
                      : undefined,
                  },
                ]);
                setSelectedModel("");
              }}
            >
              {t("gwAddOverride")}
            </Button>
          </FormRow.Actions>
        </FormRow>
        {model && matchesDefault && (
          <p className="mt-3 text-base text-kumo-subtle" role="status">
            {t("gwOverrideMatchesDefault")}
          </p>
        )}
        {publicRates && bps != null && (
          <div className="mt-4 grid gap-2 text-base sm:grid-cols-4">
            {Object.entries(publicRates).map(([key, value]) => (
              <div key={key} className="rounded border border-kumo-line bg-kumo-base p-3">
                <div className="text-kumo-subtle">{key.replaceAll("_", " ")}</div>
                <div className="mt-1 font-mono">
                  {value ?? "—"} → {multiplyRate(value, bps)}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </Card>
  );
}

function CopyPricingPlanDialog({
  plan,
  open,
  onOpenChange,
}: {
  plan: GatewayStaffTypes.PricingPlan;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const copy = gatewayStaffApi.useCopyGatewayPricingPlan();
  const [slug, setSlug] = useState(`${plan.slug}-copy`);
  const [name, setName] = useState(`${plan.name} Copy`);
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      size="base"
      title={t("gwCopyPlan")}
      description={t("gwCopyPlanHint")}
      error={copy.isError ? apiErrorMessage(copy.error) : undefined}
      submitLabel={t("gwCopyPlan")}
      submitDisabled={!slug.trim() || !name.trim()}
      pending={copy.isPending}
      onSubmit={() =>
        copy.mutate(
          { pricingPlanId: plan.id, data: { slug: slug.trim(), name: name.trim() } },
          {
            onSuccess: (created) => {
              onOpenChange(false);
              void navigate({
                to: "/gateway/pricing-plans/$pricingPlanId",
                params: { pricingPlanId: created.id },
              });
            },
          },
        )
      }
    >
      <Field label={t("gwPlanSlug")} htmlFor="copy-plan-slug">
        <Input
          id="copy-plan-slug"
          required
          value={slug}
          onChange={(event) => setSlug(event.target.value)}
        />
      </Field>
      <Field label={t("name")} htmlFor="copy-plan-name">
        <Input
          id="copy-plan-name"
          required
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
      </Field>
    </FormDialog>
  );
}
