import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { RouteStatusBadge, WiringIntentCell, WiringTable } from "./wiring-table";
import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  ConfirmDialog,
  DataTable,
  FormDialog,
  Input,
  LoadingState,
  useDebounced,
} from "@fairlb/ui";
import { keepPreviousData } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { mergeProviderRows, providerRowIssue, type ProviderRow } from "./provider-wiring";
import { planSet } from "./route-wiring";

/**
 * Editing the whole set of "which providers serve this model".
 *
 * Configuring a model ends with choosing the providers behind it, and **several may
 * be chosen**. An inline form that adds one at a time is the wrong shape for that,
 * especially since the provider side of the very same set is already edited as a
 * whole — one set with two different interactions, the narrower one on the side
 * operators actually use.
 *
 * **Editing the set manages membership, not attributes**: priority, weight,
 * endpoints, headers and limits stay on the table's own inline row editor.
 * Membership is a boolean and an attribute is not, and the two shapes do not belong
 * on one surface.
 */
export function ModelProvidersDialog({
  open,
  onOpenChange,
  model,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  model: GatewayStaffTypes.GatewayModel;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  // **Only the query object itself distinguishes the three states**: `data?.data ??
  // []` launders "not fetched yet" and "the fetch failed" into "fetched, and it is
  // empty".
  const routes = gatewayStaffApi.useListGatewayRoutes(model.id);
  // 候选从搜索来（ADR-0189）：供应商目录自 ADR-0187 起分页，无参数调用只拿第一页。
  // 已配置的那些不受影响——它们来自 `routes`，且地址随路由返回。
  const [providerQuery, setProviderQuery] = useState("");
  const settledProviderQuery = useDebounced(providerQuery, 250);
  const providers = gatewayStaffApi.useListGatewayProviders(
    { q: settledProviderQuery },
    { query: { enabled: open, placeholderData: keepPreviousData } },
  );
  const batchWire = gatewayStaffApi.useBatchWireProviderRoutes();

  const [checkedOver, setCheckedOver] = useState<Map<string, boolean>>(new Map());
  const [upstreamOver, setUpstreamOver] = useState<Map<string, string>>(new Map());
  const [errors, setErrors] = useState<Map<string, string>>(new Map());
  const [pending, setPending] = useState(false);
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    setCheckedOver(new Map());
    setUpstreamOver(new Map());
    setErrors(new Map());
  }, [model.id, open]);

  const ready = routes.isSuccess && providers.isSuccess;
  const rows = useMemo(
    () =>
      ready
        ? mergeProviderRows({
            routes: routes.data.items,
            providers: providers.data.items,
            modelSlug: model.slug,
            checkedOver,
            upstreamOver,
            errors,
          })
        : [],
    [ready, routes.data, providers.data, model.slug, checkedOver, upstreamOver, errors],
  );
  const plan = planSet(rows);
  const blocked = rows.some(providerRowIssue);

  /**
   * The batch endpoint, but **called once per provider**: its scope is a single
   * provider, while a batch on this side can span several. Within one provider it is
   * still a single transaction that creates before it deletes.
   */
  const submit = async () => {
    setPending(true);
    const failed = new Map<string, string>();
    let ok = 0;
    let already = 0;
    const byProvider = new Map<string, { creates: ProviderRow[]; deletes: ProviderRow[] }>();
    for (const r of plan.creates) {
      const g = byProvider.get(r.providerId) ?? { creates: [], deletes: [] };
      g.creates.push(r);
      byProvider.set(r.providerId, g);
    }
    for (const r of plan.deletes) {
      const g = byProvider.get(r.providerId) ?? { creates: [], deletes: [] };
      g.deletes.push(r);
      byProvider.set(r.providerId, g);
    }

    for (const [providerId, g] of byProvider) {
      try {
        const res = await batchWire.mutateAsync({
          providerId,
          data: {
            creates: g.creates.map((r) => ({
              model_id: model.id,
              provider_model_id: r.upstream.trim(),
            })),
            deletes: g.deletes.map((r) => ({ model_id: model.id, route_id: r.routeId ?? "" })),
          },
        });
        // Matched back by index: the results correspond one to one, in order, with
        // the creates followed by the deletes, as the contract specifies.
        const planned = [...g.creates, ...g.deletes];
        res.results.forEach((x, i) => {
          if (x.outcome === "failed") {
            const key = planned[i]?.key;
            if (key) failed.set(key, x.detail || t("gwWiringRowFailed"));
            return;
          }
          ok += 1;
          if (x.outcome === "already") already += 1;
        });
      } catch (err) {
        // This whole provider failed: mark every row under it, rather than leaving
        // them looking untouched.
        for (const r of [...g.creates, ...g.deletes]) failed.set(r.key, apiErrorMessage(err));
      }
    }
    setPending(false);
    setErrors(failed);
    onSaved();
    toasts.add({
      variant: failed.size === 0 ? "success" : "warning",
      title: t("gwRoutesCreated", { ok: String(ok), failed: String(failed.size) }),
      description: already > 0 ? t("gwWiringAlreadySoCount", { n: String(already) }) : undefined,
    });
    if (failed.size === 0) onOpenChange(false);
  };

  return (
    <>
      <FormDialog
        open={open}
        onOpenChange={onOpenChange}
        size="xl"
        title={t("gwModelProvidersTitle", { slug: model.slug })}
        description={t("gwModelProvidersHint")}
        error={errors.size > 0 ? t("gwWiringSomeFailed") : undefined}
        submitLabel={t("gwWiringSubmit", {
          add: String(plan.creates.length),
          del: String(plan.deletes.length),
        })}
        submitDisabled={
          !ready || (plan.creates.length === 0 && plan.deletes.length === 0) || blocked || pending
        }
        pending={pending}
        onSubmit={() => setConfirming(true)}
      >
        {/* The three-state gate over both queries: **while either is pending or
            failed, not one checkbox is rendered**. "Everything configured is here
            and ticked" is this dialog's central promise, and while a query is
            pending that is simply unknown. */}
        {/* 搜索框：候选来自服务端（ADR-0189）。已配置的那些不受它影响——
            它们来自 `routes`，无论搜什么都留在表里、也留着勾。 */}
        <Input
          id="model-provider-search"
          aria-label={t("gwProviderSearch")}
          type="search"
          value={providerQuery}
          onChange={(e) => setProviderQuery(e.target.value)}
          placeholder={t("gwProviderSearch")}
        />
        {routes.isPending || providers.isPending ? (
          <LoadingState label={t("loading")} />
        ) : routes.isError ? (
          <Alert>{apiErrorMessage(routes.error)}</Alert>
        ) : providers.isError ? (
          <Alert>{apiErrorMessage(providers.error)}</Alert>
        ) : (
          <ProviderWiringTable
            caption={t("gwModelProvidersTitle", { slug: model.slug })}
            rows={rows}
            pending={pending}
            onToggle={(key, next) => setCheckedOver((prev) => new Map(prev).set(key, next))}
            onUpstream={(key, next) => setUpstreamOver((prev) => new Map(prev).set(key, next))}
          />
        )}
      </FormDialog>

      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        destructive={plan.deletes.length > 0}
        title={t("gwModelProvidersConfirmTitle")}
        // **Both numbers appear, including a zero one**: printing only the non-zero
        // one makes "3 added" read as a purely additive change, leaving the reader
        // no way to know this dialog deletes things at all.
        description={[
          t("gwModelProvidersConfirmBody", {
            add: String(plan.creates.length),
            del: String(plan.deletes.length),
            slug: model.slug,
          }),
          plan.deletes.length > 0
            ? t("gwWiringConfirmRemoving", {
                names: plan.deletes.map((d) => `${d.providerSlug} → ${d.upstream}`).join(", "),
              })
            : "",
        ]
          .filter(Boolean)
          .join(" ")}
        confirmLabel={t("gwWiringApply")}
        pending={pending}
        onConfirm={() => {
          setConfirming(false);
          void submit();
        }}
      />
    </>
  );
}

function ProviderWiringTable({
  rows,
  pending,
  onToggle,
  onUpstream,
  caption,
}: {
  rows: ProviderRow[];
  pending: boolean;
  onToggle: (key: string, next: boolean) => void;
  onUpstream: (key: string, next: string) => void;
  /** The table's accessible name, taken from the dialog title, the same way the
   * provider-side dialog does it. */
  caption: string;
}) {
  const { t } = useI18n();
  return (
    <WiringTable
      caption={caption}
      columns={[t("gwColProvider"), t("gwColUpstreamModel"), t("gwColStatus")]}
      empty={{ title: t("gwModelProvidersEmpty"), description: t("gwModelProvidersEmptyHint") }}
      rowCount={rows.length}
    >
      {rows.map((r) => (
        <DataTable.Row key={r.key}>
          <WiringIntentCell
            checked={r.checked}
            disabled={pending}
            label={r.providerSlug}
            onToggle={(next) => onToggle(r.key, next)}
          />
          <DataTable.Cell className="font-mono">{r.providerSlug}</DataTable.Cell>
          <DataTable.Cell>
            {/* **Editable in place**: on this side the upstream name is guessed
                  from the slug rather than enumerated. To find out what a given
                  provider actually calls it, open that provider's own page — its
                  fetched catalogue is stored now, so the answer is there on arrival
                  rather than only for as long as somebody keeps the tab open. */}
            <Input
              aria-label={t("gwColUpstreamModel")}
              value={r.upstream}
              disabled={pending}
              className="font-mono"
              onChange={(e) => onUpstream(r.key, e.target.value)}
            />
            {providerRowIssue(r) && (
              <p className="text-base text-kumo-danger">{t("gwUpstreamRequired")}</p>
            )}
          </DataTable.Cell>
          {/* 存的是什么由这一列说；左边那个勾选框说的是操作者想要什么。
              两者的分工见 WiringIntentCell 的注释。 */}
          <DataTable.Cell className="space-x-2 whitespace-nowrap">
            {r.configured && <RouteStatusBadge enabled={r.routeEnabled} />}
            {r.error && <span className="text-base text-kumo-danger">{r.error}</span>}
          </DataTable.Cell>
        </DataTable.Row>
      ))}
    </WiringTable>
  );
}
