import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import {
  gatewayStaffApi,
  getResponseETag,
  type GatewayStaffTypes,
  apiErrorMessage,
} from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  DataTable,
  Field,
  FormRow,
  Input,
  LoadingState,
  Combobox,
  SectionHeading,
  StatusBadge,
  useDebounced,
} from "@fairlb/ui";
import { useParams } from "@tanstack/react-router";
import { keepPreviousData, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { effectivePlanMultiplier, multiplyRate } from "./pricing-math";
import { useCurrentStaffRole } from "./host";

/**
 * What one organization has been granted, in the deployment that bills them: what
 * they may call, how fast, and at what price.
 */
export function GatewayOrgAccessPage() {
  return <OrgAccessContent withPricing />;
}

/**
 * The same page without the pricing half, for a deployment that charges nobody.
 *
 * It is a second entry point rather than a flag on the host contract because
 * what differs is *which cards a shell mounts*, and that is the shell's own
 * decision everywhere else in this codebase — the app hands the composition an
 * explicit list rather than a set of switches. A deployment run for its owner
 * would otherwise be offered a control that assigns one of the plans it has no
 * page to create, moving money from one of their pockets to the other.
 */
export function GatewayOrgLimitsPage() {
  return <OrgAccessContent withPricing={false} />;
}

function OrgAccessContent({ withPricing }: { withPricing: boolean }) {
  const { t } = useI18n();
  const staffRole = useCurrentStaffRole();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const queryClient = useQueryClient();
  const settings = gatewayStaffApi.useGetOrgGatewaySettings(orgId);
  // 两个选择器的候选都从**搜索**来，不再是整份列表（ADR-0189/0191）。档位与方案
  // 两条列表分页之后，「把这个组织换到某个档位」若还靠列表完整性来兑现，一个排在
  // 第二页的档位就**根本选不到**——而界面上看不出少了什么。
  const [tierQuery, setTierQuery] = useState("");
  const settledTierQuery = useDebounced(tierQuery, 250);
  const tiers = gatewayStaffApi.useListGatewayTiers(
    settledTierQuery ? { q: settledTierQuery } : undefined,
    // keepPreviousData 是正确性不是优化：搜索词一变查询键就变，没有它 data 先变回
    // undefined，正在被打字的那个输入框会被换掉。
    { query: { placeholderData: keepPreviousData } },
  );
  const refreshSettings = () =>
    void queryClient.invalidateQueries({
      queryKey: gatewayStaffApi.getGetOrgGatewaySettingsQueryKey(orgId),
    });
  const putAccess = gatewayStaffApi.usePutOrgGatewaySettings({
    mutation: { onSuccess: refreshSettings },
  });
  const assignment = gatewayStaffApi.useGetGatewayOrgPricingPlan(orgId);
  const [planQuery, setPlanQuery] = useState("");
  const settledPlanQuery = useDebounced(planQuery, 250);
  const plans = gatewayStaffApi.useListGatewayPricingPlans(
    settledPlanQuery ? { q: settledPlanQuery } : undefined,
    { query: { placeholderData: keepPreviousData } },
  );
  const models = gatewayStaffApi.useListGatewayModels();
  const assignmentUrl = gatewayStaffApi.getGetGatewayOrgPricingPlanUrl(orgId);
  const assignUrl = gatewayStaffApi.getAssignGatewayOrgPricingPlanUrl(orgId);
  const assignmentEtag =
    getResponseETag(assignUrl, assignmentUrl) ??
    `"${assignment.data?.pricing_plan_id ?? "default"}"`;
  const refreshAssignment = () =>
    void queryClient.invalidateQueries({
      queryKey: gatewayStaffApi.getGetGatewayOrgPricingPlanQueryKey(orgId),
    });
  const assign = gatewayStaffApi.useAssignGatewayOrgPricingPlan({
    mutation: { onSuccess: refreshAssignment },
    request: { headers: { "If-Match": assignmentEtag } },
  });
  const toasts = useKumoToastManager();

  const cur = settings.data;
  const [tierId, setTierId] = useState<string | null>(null);
  const [accessReason, setAccessReason] = useState("");
  /**
   * The two rate ceilings, held as the text that is in the field.
   *
   * Text rather than a number, because an empty field is a state a number
   * cannot hold: "no ceiling" is a real setting here and has to survive being
   * typed, cleared and retyped without collapsing into zero. `undefined` means
   * "not edited", so the saved value is what is shown until somebody changes
   * it.
   */
  const [rpm, setRpm] = useState<string | undefined>(undefined);
  const [tpm, setTpm] = useState<string | undefined>(undefined);
  const [planId, setPlanId] = useState<string | null | undefined>(undefined);
  const [priceReason, setPriceReason] = useState("");

  const allPlans = plans.data?.items ?? [];
  const selectablePlans = allPlans.filter((item) => item.status === "active");
  const selectedPlanId =
    planId === undefined
      ? assignment.data?.inherited_default
        ? null
        : assignment.data?.pricing_plan_id
      : planId;
  // 当前方案与将要换成的方案各自单读，不从列表里按 id 找。列表分页之后那个 `find`
  // 会在方案排到第二页时返回 undefined，而下面的 `?? 10_000` 会把它画成「1.0x」
  // ——一个具体的、错的价格，不是「读不到」。
  const currentPlanQuery = gatewayStaffApi.useGetGatewayPricingPlan(
    assignment.data?.pricing_plan_id ?? "",
    { query: { enabled: assignment.data?.pricing_plan_id != null } },
  );
  const currentPlan = currentPlanQuery.data;
  const defaultPlan = allPlans.find((item) => item.is_default);
  const nextPlanId = selectedPlanId == null ? defaultPlan?.id : selectedPlanId;
  const nextPlanQuery = gatewayStaffApi.useGetGatewayPricingPlan(nextPlanId ?? "", {
    query: { enabled: nextPlanId != null },
  });
  const nextPlan = nextPlanQuery.data;
  const currentOverrides = gatewayStaffApi.useListGatewayPricingPlanModelOverrides(
    currentPlan?.id ?? "",
    {
      query: { enabled: currentPlan?.id != null },
    },
  );
  const nextOverrides = gatewayStaffApi.useListGatewayPricingPlanModelOverrides(
    nextPlan?.id ?? "",
    {
      query: { enabled: nextPlan?.id != null },
    },
  );

  if (settings.isError && !cur) return <Alert>{apiErrorMessage(settings.error)}</Alert>;
  if (!cur) return <LoadingState label={t("loading")} />;

  const shownTier = tierId ?? cur.tier_id;
  const savedRpm = cur.rate_limit_rpm == null ? "" : String(cur.rate_limit_rpm);
  const savedTpm = cur.rate_limit_tpm == null ? "" : String(cur.rate_limit_tpm);
  const shownRpm = rpm ?? savedRpm;
  const shownTpm = tpm ?? savedTpm;
  // The write is whole-row, so an unchanged field still has to be sent. What
  // "dirty" decides is only whether the Save button does anything.
  const accessDirty = shownTier !== cur.tier_id || shownRpm !== savedRpm || shownTpm !== savedTpm;
  const selectableTiers = (tiers.data?.items ?? []).filter(
    (item) => item.status === "active" || item.id === cur.tier_id,
  );
  // 当前档位恒在，且不依赖搜索：它来自这个组织自己的 settings（带 slug），而搜索
  // 结果里没有它是常态——一敲字就把「现在是哪个档位」从控件里抹掉，读者会以为
  // 这个组织没有档位。这也是「已配置的恒在」（ADR-0086）在单选控件上的形态。
  const tierOptions: PickerOption[] = dedupeOptions([
    { value: cur.tier_id, label: cur.tier_slug },
    ...selectableTiers.map((item: GatewayStaffTypes.GatewayTier) => ({
      value: item.id,
      label: item.allow_all_models
        ? `${item.slug} (${t("tierUnrestricted")})`
        : item.model_count === 0
          ? `${item.slug} (${t("tierAllowsNothing")})`
          : item.slug,
    })),
  ]);
  // 「跟随默认」是一个真选项而不是空值：它与「显式选中恰好是默认的那个方案」
  // 是两件事——前者跟着默认走，后者钉死。
  const planOptions: PickerOption[] = dedupeOptions([
    { value: DEFAULT_PLAN, label: t("orgPricingPlanDefault") },
    ...(currentPlan ? [{ value: currentPlan.id, label: planLabel(currentPlan) }] : []),
    ...selectablePlans
      .filter((item) => !item.is_default)
      .map((item) => ({ value: item.id, label: planLabel(item) })),
  ]);
  const currentBps = currentPlan?.default_adjustment?.multiplier_bps ?? 10_000;
  const nextBps = nextPlan?.default_adjustment?.multiplier_bps ?? 10_000;
  const priceDirty =
    planId !== undefined &&
    (selectedPlanId == null
      ? !assignment.data?.inherited_default
      : selectedPlanId !== assignment.data?.pricing_plan_id);

  const submitAccess = (event: FormEvent) => {
    event.preventDefault();
    if (!accessDirty || !accessReason.trim()) return;
    // A blank field is not zero: it is the absence of a ceiling, and the
    // contract says that by leaving the field out. Sending 0 would fail the
    // schema's minimum, and sending it as a number would be a ceiling of zero
    // -- which refuses every request.
    const limit = (value: string) => {
      const n = Number(value.trim());
      return value.trim() === "" || !Number.isFinite(n) || n < 1 ? undefined : Math.floor(n);
    };
    putAccess.mutate(
      {
        orgId,
        data: {
          tier_id: shownTier,
          reason: accessReason.trim(),
          ...(limit(shownRpm) === undefined ? {} : { rate_limit_rpm: limit(shownRpm) }),
          ...(limit(shownTpm) === undefined ? {} : { rate_limit_tpm: limit(shownTpm) }),
        },
      },
      {
        onSuccess: () => {
          setAccessReason("");
          setTierId(null);
          setRpm(undefined);
          setTpm(undefined);
          toasts.add({ variant: "success", title: t("orgAccessSaved") });
        },
      },
    );
  };

  const submitPricing = (event: FormEvent) => {
    event.preventDefault();
    if (!priceDirty || !priceReason.trim() || staffRole !== "superadmin") return;
    assign.mutate(
      {
        orgId,
        data: {
          pricing_plan_id: selectedPlanId ?? null,
          reason: priceReason.trim(),
        },
      },
      {
        onSuccess: () => {
          setPlanId(undefined);
          setPriceReason("");
          toasts.add({ variant: "success", title: t("gwPricingPlanAssigned") });
        },
      },
    );
  };

  return (
    // Two columns only when there are two cards. A two-column grid holding one
    // card is a half-width form beside an empty half, which reads as something
    // that failed to load rather than as a page with one thing on it.
    <div
      className={
        withPricing
          ? "grid gap-6 xl:grid-cols-2 xl:items-start"
          : "grid max-w-2xl gap-6 items-start"
      }
    >
      <Card className="space-y-4">
        <div>
          <SectionHeading>{t("orgModelAccessCard")}</SectionHeading>
          <p className="mt-1 text-base text-kumo-subtle">{t("orgModelAccessHint")}</p>
        </div>
        {settings.isError && <Alert>{apiErrorMessage(settings.error)}</Alert>}
        {putAccess.isError && <Alert>{apiErrorMessage(putAccess.error)}</Alert>}
        <div className="flex flex-wrap items-center gap-3 rounded-lg border border-kumo-line bg-kumo-recessed p-4 text-base">
          <span className="font-mono text-[0.9em] font-medium">{cur.tier_slug}</span>
          {!cur.tier_explicit && (
            <StatusBadge tone="neutral">{t("orgAccessInherited")}</StatusBadge>
          )}
          {cur.tier_status === "disabled" && (
            <StatusBadge tone="danger">{t("orgAccessTierDisabled")}</StatusBadge>
          )}
        </div>
        {cur.tier_status === "disabled" && <Alert>{t("orgAccessDisabledHint")}</Alert>}
        <form className="space-y-4" onSubmit={submitAccess}>
          <Field label={t("orgAccessTier")} htmlFor="org-access-tier">
            <Combobox<PickerOption>
              items={tierOptions}
              value={tierOptions.find((option) => option.value === shownTier) ?? null}
              onValueChange={(option) => setTierId(option?.value ?? cur.tier_id)}
              // 输入框的文字由 Combobox 自己管；这里只订阅它来驱动服务端查询。控制
              // `inputValue` 会让选中之后输入框一直是空的。
              onInputValueChange={setTierQuery}
              itemToStringLabel={(option) => option.label}
              isItemEqualToValue={(a, b) => a.value === b.value}
              autoHighlight
            >
              <Combobox.TriggerInput id="org-access-tier" placeholder={t("gwSearchTiers")} />
              <Combobox.Content>
                <Combobox.Empty>{t("gwNoTierMatch")}</Combobox.Empty>
                <Combobox.List>
                  {(option: PickerOption) => (
                    <Combobox.Item key={option.value} value={option}>
                      {option.label}
                    </Combobox.Item>
                  )}
                </Combobox.List>
              </Combobox.Content>
            </Combobox>
          </Field>
          {/* The ceilings sit on this form rather than one of their own: they
              are the other half of the same agreement, and the write is one
              row. Blank means no ceiling, which is why the hint says so --
              an empty number field otherwise reads as a value nobody filled
              in yet. */}
          <FormRow>
            <Field label={t("orgRateLimitRpm")} htmlFor="org-rpm" hint={t("orgRateLimitHint")}>
              <Input
                id="org-rpm"
                inputMode="numeric"
                value={shownRpm}
                placeholder={t("orgRateLimitNone")}
                onChange={(event) => setRpm(event.target.value)}
              />
            </Field>
            <Field label={t("orgRateLimitTpm")} htmlFor="org-tpm" hint={t("orgRateLimitHint")}>
              <Input
                id="org-tpm"
                inputMode="numeric"
                value={shownTpm}
                placeholder={t("orgRateLimitNone")}
                onChange={(event) => setTpm(event.target.value)}
              />
            </Field>
          </FormRow>
          <Field label={t("orgAccessReason")} htmlFor="access-reason" hint={t("orgReasonRequired")}>
            <Input
              id="access-reason"
              value={accessReason}
              onChange={(event) => setAccessReason(event.target.value)}
            />
          </Field>
          <Button
            type="submit"
            loading={putAccess.isPending}
            disabled={!accessDirty || !accessReason.trim()}
          >
            {t("save")}
          </Button>
        </form>
      </Card>

      {!withPricing ? null : (
        <Card className="space-y-4">
          <div>
            <SectionHeading>{t("orgPricingPlanCard")}</SectionHeading>
            <p className="mt-1 text-base text-kumo-subtle">{t("orgPricingPlanHint")}</p>
          </div>
          {(assignment.isError ||
            plans.isError ||
            assign.isError ||
            currentOverrides.isError ||
            nextOverrides.isError) && (
            <Alert>
              {apiErrorMessage(
                assignment.error ??
                  plans.error ??
                  assign.error ??
                  currentOverrides.error ??
                  nextOverrides.error,
              )}
            </Alert>
          )}
          {assignment.data && (
            <div className="rounded-lg border border-kumo-line bg-kumo-recessed p-4">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-medium">{assignment.data.pricing_plan_name}</span>
                <span className="font-mono text-[0.9em] text-kumo-subtle">
                  {assignment.data.pricing_plan_slug}
                </span>
                {assignment.data.inherited_default && (
                  <StatusBadge tone="neutral">{t("orgAccessInherited")}</StatusBadge>
                )}
              </div>
            </div>
          )}
          {/* There is no "pending plan change" block, because a plan change takes
            effect immediately. With no scheduled effective date there is also no
            state in which a record is locked by a queued change and cannot be
            edited. */}
          <form className="space-y-4" onSubmit={submitPricing}>
            <Field label={t("orgPricingPlanSelect")} htmlFor="org-pricing-plan">
              <Combobox<PickerOption>
                items={planOptions}
                value={
                  planOptions.find((option) => option.value === (selectedPlanId ?? DEFAULT_PLAN)) ??
                  null
                }
                onValueChange={(option) =>
                  setPlanId(!option || option.value === DEFAULT_PLAN ? null : option.value)
                }
                onInputValueChange={setPlanQuery}
                itemToStringLabel={(option) => option.label}
                isItemEqualToValue={(a, b) => a.value === b.value}
                autoHighlight
              >
                <Combobox.TriggerInput
                  id="org-pricing-plan"
                  placeholder={t("gwSearchPricingPlans")}
                />
                <Combobox.Content>
                  <Combobox.Empty>{t("gwNoPricingPlanMatch")}</Combobox.Empty>
                  <Combobox.List>
                    {(option: PickerOption) => (
                      <Combobox.Item key={option.value} value={option}>
                        {option.label}
                      </Combobox.Item>
                    )}
                  </Combobox.List>
                </Combobox.Content>
              </Combobox>
            </Field>
            {priceDirty && (
              <PlanPriceComparison
                models={(models.data?.items ?? []).slice(0, 4)}
                oldDefaultBps={currentBps}
                newDefaultBps={nextBps}
                oldOverrides={currentOverrides.data?.items ?? []}
                newOverrides={nextOverrides.data?.items ?? []}
              />
            )}
            {/* No "effective from" cell: a plan change is immediate, so this row
              carries only the reason. */}
            <FormRow>
              <FormRow.Item>
                <Field
                  label={t("orgAccessReason")}
                  htmlFor="pricing-reason"
                  hint={t("orgReasonRequired")}
                >
                  <Input
                    id="pricing-reason"
                    value={priceReason}
                    onChange={(event) => setPriceReason(event.target.value)}
                  />
                </Field>
              </FormRow.Item>
            </FormRow>
            {staffRole !== "superadmin" && (
              <p className="text-base text-kumo-subtle">{t("gwSuperadminAssignsPlan")}</p>
            )}
            <Button
              type="submit"
              loading={assign.isPending}
              disabled={!priceDirty || !priceReason.trim() || staffRole !== "superadmin"}
            >
              {t("orgPricingPlanApply")}
            </Button>
          </form>
        </Card>
      )}
    </div>
  );
}

function PlanPriceComparison({
  models,
  oldDefaultBps,
  newDefaultBps,
  oldOverrides,
  newOverrides,
}: {
  models: GatewayStaffTypes.GatewayModel[];
  oldDefaultBps: number;
  newDefaultBps: number;
  oldOverrides: GatewayStaffTypes.PricingPlanModelOverride[];
  newOverrides: GatewayStaffTypes.PricingPlanModelOverride[];
}) {
  const { t } = useI18n();
  return (
    <div className="overflow-hidden rounded-lg ring ring-kumo-line">
      <DataTable caption={t("orgPricingPlanCard")}>
        <DataTable.Header>
          <DataTable.Row>
            <DataTable.Head>{t("gwModel")}</DataTable.Head>
            <DataTable.Head className="text-right">{t("gwOldPrice")}</DataTable.Head>
            <DataTable.Head className="text-right">{t("gwNewPrice")}</DataTable.Head>
          </DataTable.Row>
        </DataTable.Header>
        <DataTable.Body>
          {models.map((model) => {
            const oldBps = effectivePlanMultiplier(model.id, oldDefaultBps, oldOverrides);
            const newBps = effectivePlanMultiplier(model.id, newDefaultBps, newOverrides);
            const rates = model.public_rates;
            return (
              <DataTable.Row key={model.id}>
                <DataTable.Cell className="font-mono">{model.slug}</DataTable.Cell>
                <DataTable.Cell className="text-right font-mono">
                  <RateComparisonStack rates={rates} multiplier={oldBps} />
                </DataTable.Cell>
                <DataTable.Cell className="text-right font-mono">
                  <RateComparisonStack rates={rates} multiplier={newBps} />
                </DataTable.Cell>
              </DataTable.Row>
            );
          })}
        </DataTable.Body>
      </DataTable>
    </div>
  );
}

function RateComparisonStack({
  rates,
  multiplier,
}: {
  rates?: GatewayStaffTypes.TokenRatesUSDPerM;
  multiplier: number;
}) {
  const rows = [
    ["Input", rates?.input],
    ["Output", rates?.output],
    ["Cache Read", rates?.cache_read],
    ["Cache Write", rates?.cache_write],
  ] as const;
  return (
    <div className="space-y-1">
      {rows.map(([label, value]) => (
        <div key={label} className="flex justify-end gap-2">
          <span className="text-kumo-subtle">{label}</span>
          <span>{value == null ? "—" : `$${multiplyRate(value, multiplier)}`}</span>
        </div>
      ))}
    </div>
  );
}

/** 「跟随默认方案」这一项的值。它不是某个方案的 id，故取一个 uuid 不可能撞上的字面量。 */
const DEFAULT_PLAN = "__default__";

type PickerOption = { value: string; label: string };

function planLabel(plan: GatewayStaffTypes.PricingPlan): string {
  return `${plan.name} (${plan.slug})`;
}

/**
 * 保留首次出现的那一项。
 *
 * 「当前选中的恒在」是靠把它拼在搜索结果**前面**兑现的，而搜索结果里往往也有它。
 * 留前面那份：它带的标签来自这个组织自己的记录，是选中态要显示的那一个。
 */
function dedupeOptions(options: PickerOption[]): PickerOption[] {
  const seen = new Set<string>();
  return options.filter((option) => {
    if (seen.has(option.value)) return false;
    seen.add(option.value);
    return true;
  });
}
