import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  ConfirmDialog,
  DataTable,
  InlineEmpty,
  LoadingState,
  RowActions,
  SectionHeading,
  StatusBadge,
} from "@fairlb/ui";
import { Link } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { ProviderModelsDialog } from "./provider-models-dialog";
import { discoverSummary } from "./route-wiring";
import { useVendors, vendorBySlug } from "./vendors";

/**
 * The route table from the provider's point of view.
 *
 * It replaces the old one-off "discover upstream models" view, whose transient
 * result disappeared on navigation or reload.
 *
 * **This table answers one question: what does this provider serve right now.**
 * It used to answer "and what else does the upstream have that is not configured"
 * as well, at the cost of a column set that drifted with state: a checkbox column
 * appeared only after a fetch, and two colSpans had to move with it. Candidates now
 * belong to the configure-models dialog, and this table's columns are fixed.
 *
 * A fetch leaves two things on this face: a one-line **summary**, carrying the time
 * it was taken — the result lives in component state and is lost on leaving the page,
 * so for as long as it is on screen it must say how old it is — and a "no longer
 * offered upstream" badge on individual routes. That badge is decidable **only after
 * a complete enumeration**; otherwise absence merely means it was not read.
 */
export function ProviderModelsPanel({ provider }: { provider: GatewayStaffTypes.GatewayProvider }) {
  // The shared formatter follows **the application's language**, while
  // `toLocaleString` follows the browser's. After the language has been switched the
  // two disagree — and what this prints is how fresh a reading is, where being misread
  // is worse than not being printed.
  const { t, formatDateTime } = useI18n();
  const queryClient = useQueryClient();
  const toasts = useKumoToastManager();

  const routes = gatewayStaffApi.useListGatewayProviderRoutes(provider.id);
  const discover = gatewayStaffApi.useDiscoverProviderModels();
  const updateRoute = gatewayStaffApi.useUpdateGatewayRoute();
  const removeRoute = gatewayStaffApi.useDeleteGatewayRoute();

  const [result, setResult] = useState<GatewayStaffTypes.DiscoverModelsResult | null>(null);
  const [wiring, setWiring] = useState(false);
  // Acting on a route from the provider side. Disabling and deleting are both
  // confirmed, because they redirect real traffic — the same restraint the model-side
  // table shows.
  const [togglingRoute, setTogglingRoute] = useState<GatewayStaffTypes.GatewayRoute | null>(null);
  const [removingRoute, setRemovingRoute] = useState<GatewayStaffTypes.GatewayRoute | null>(null);

  const routeRows = routes.data?.items ?? [];

  // The set of model names the upstream reported. **It exists only when the fetch
  // succeeded and the enumeration was complete.** null means this dimension has not
  // been checked, which is a different fact from "checked, and the upstream does not
  // have it".
  //
  // The completeness half matters because **absence is only a conclusion within a
  // complete enumeration**. Testing success alone is not enough: a truncated catalog
  // — too many models, too many pages, a stalled cursor — also reports success, and
  // every local route past the truncation point would then be mislabelled as no
  // longer offered upstream.
  const upstreamNames =
    result?.ok === true && result.complete
      ? new Set(result.models.map((m) => m.upstream_model))
      : null;

  // A fetch can only be classified when it succeeded; `?? null` makes both "never
  // fetched" and "the fetch failed" arrive as null.
  const discovered = result?.ok === true ? result.models : null;
  const summary =
    discovered !== null ? discoverSummary(discovered, routeRows, result?.complete === true) : null;

  const run = () => discover.mutate({ providerId: provider.id }, { onSuccess: setResult });
  const vendors = useVendors().data?.items;
  // Undefined while the registry query is undecided: the button stays enabled
  // then, because "we do not know yet" must not read as "there is nothing here".
  const hasNoListing = vendorBySlug(provider.vendor, vendors)?.model_listing === false;

  const refresh = () => {
    void routes.refetch();
    // 这一页自己不再订阅目录（模型名随路由返回，ADR-0189），所以让查询键失效
    // 而不是 refetch 一个没有观察者的查询——接线弹窗里那份才是要刷新的那个。
    void queryClient.invalidateQueries({
      queryKey: gatewayStaffApi.getListGatewayModelsQueryKey(),
    });
  };

  return (
    <div className="space-y-6">
      {discover.isError && <Alert>{apiErrorMessage(discover.error)}</Alert>}
      {routes.isError && <Alert>{apiErrorMessage(routes.error)}</Alert>}
      {updateRoute.isError && <Alert>{apiErrorMessage(updateRoute.error)}</Alert>}
      {removeRoute.isError && <Alert>{apiErrorMessage(removeRoute.error)}</Alert>}

      <section className="space-y-3">
        <SectionHeading>{t("gwProviderModelsHeading")}</SectionHeading>
        <p className="text-base text-kumo-subtle">{t("gwProviderModelsHint")}</p>

        <Card className="space-y-3">
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" onClick={() => setWiring(true)}>
              {t("gwAddModelRoute")}
            </Button>
            {/* Fetching really calls the upstream and really costs money, so it runs
                only on an explicit click and never on page load. The button **stays on
                this face**: inside a dialog, sitting beside checkboxes, it would read
                as a free local operation. */}
            {/* Two platforms publish no catalogue at all. The button is left in
                place but disabled, with the reason beside it: removing it entirely
                would leave the operator wondering where the feature went, while
                leaving it live spends a click on an answer that is already known. */}
            <Button
              variant="outline"
              loading={discover.isPending}
              disabled={hasNoListing}
              onClick={run}
            >
              {discover.isPending ? t("gwDiscoverRunning") : t("gwDiscoverRun")}
            </Button>
          </div>
          {hasNoListing && <p className="text-base text-kumo-subtle">{t("gwVendorNoListing")}</p>}

          {/* An upstream failure is this fetch's *result*, not a broken request, so
              its message is surfaced as a notice rather than as an error. */}
          {result && !result.ok && <Alert>{result.message ?? ""}</Alert>}
          {result?.ok && result.message && <Alert variant="warning">{result.message}</Alert>}

          {/* One summary line, in place of the dozen candidate rows that used to be
              interleaved into the table. **It prints when the fetch happened**: the
              result lives in component state and is lost on leaving the page, so while
              it is on screen it has to say how old it is — otherwise a ten-minute-old
              "no longer offered upstream" looks exactly as fresh as one from a second
              ago. */}
          {summary && (
            <p className="text-base text-kumo-subtle">
              {t("gwDiscoverSummary", {
                total: String(summary.total),
                at: formatDateTime(result?.checked_at ?? ""),
                coverage: t(
                  result?.complete ? "gwDiscoverCoverageFull" : "gwDiscoverCoveragePartial",
                ),
                routed: String(summary.routed),
                mappable: String(summary.mappable),
                unpriced: String(summary.unpriced),
                unknown: String(summary.unknown),
              })}
              {summary.gone !== null && summary.gone > 0 && (
                <span className="text-kumo-warning">
                  {" · "}
                  {t("gwDiscoverSummaryGone", { n: String(summary.gone) })}
                </span>
              )}
            </p>
          )}

          <DataTable caption={t("gwProviderModelsHeading")}>
            <DataTable.Header>
              <DataTable.Row>
                <DataTable.Head>{t("gwColModel")}</DataTable.Head>
                <DataTable.Head>{t("gwDiscoverUpstream")}</DataTable.Head>
                <DataTable.Head>{t("gwColStatus")}</DataTable.Head>
                <DataTable.Head />
              </DataTable.Row>
            </DataTable.Header>
            <DataTable.Body>
              {routeRows.map((r) => {
                // 模型名随路由返回（ADR-0189）。此前是拿 model_id 去目录里查，
                // 而目录分页之后查不到就渲染成「—」——一条真实存在的路由被画成
                // 「没有模型」。同投递日志端点地址、wiring 的供应商名一个坑。
                const modelSlug = r.model_slug;
                // "No longer offered upstream" is decidable only after a **complete**
                // fetch; otherwise the cell is left empty.
                const gone = upstreamNames != null && !upstreamNames.has(r.provider_model_id);
                return (
                  <DataTable.Row key={r.id}>
                    <DataTable.Cell className="font-mono">
                      {modelSlug ? (
                        // Land on the face where something can be done: this route's
                        // priority, weight and endpoints are all edited on the model
                        // detail page's providers face.
                        <Link
                          to="/gateway/models/$modelId"
                          params={{ modelId: r.model_id }}
                          hash="model-routes"
                          className="text-kumo-info hover:underline"
                        >
                          {modelSlug}
                        </Link>
                      ) : (
                        "—"
                      )}
                    </DataTable.Cell>
                    <DataTable.Cell className="font-mono">{r.provider_model_id}</DataTable.Cell>
                    <DataTable.Cell className="space-x-2 whitespace-nowrap">
                      <StatusBadge tone={r.enabled ? "success" : "neutral"}>
                        {r.enabled ? t("gwDiscoverState_routed") : t("gwManualDisabled")}
                      </StatusBadge>
                      {gone && (
                        <StatusBadge tone="warning">{t("gwDiscoverState_gone")}</StatusBadge>
                      )}
                    </DataTable.Cell>
                    {/* Seeing a route from the provider side without being able to
                        disable or delete it is the gap these actions close; they are
                        shaped like the model-side table's. */}
                    <DataTable.Cell>
                      <RowActions>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={updateRoute.isPending}
                          onClick={() => setTogglingRoute(r)}
                        >
                          {r.enabled ? t("gwDisable") : t("gwEnable")}
                        </Button>
                        <Button
                          size="sm"
                          variant="secondary-destructive"
                          onClick={() => setRemovingRoute(r)}
                        >
                          {t("gwDelete")}
                        </Button>
                      </RowActions>
                    </DataTable.Cell>
                  </DataTable.Row>
                );
              })}

              {/* No empty state while pending: `?? []` lets "we have not looked yet"
                  masquerade as "we looked, and there is nothing", and those call for
                  entirely different responses. The test is **success**, not merely
                  settled — a failed query already has its notice above. */}
              {routes.isPending ? (
                <DataTable.Row>
                  <DataTable.Cell colSpan={4}>
                    <LoadingState label={t("loading")} />
                  </DataTable.Cell>
                </DataTable.Row>
              ) : (
                !routes.isError &&
                routeRows.length === 0 && (
                  <DataTable.Row>
                    <DataTable.Cell colSpan={4}>
                      <InlineEmpty
                        title={t("gwNoProviderRoutes")}
                        description={t("gwNoProviderRoutesHint")}
                      />
                    </DataTable.Cell>
                  </DataTable.Row>
                )
              )}
            </DataTable.Body>
          </DataTable>
        </Card>
      </section>

      <ConfirmDialog
        open={togglingRoute !== null}
        onOpenChange={(o) => !o && setTogglingRoute(null)}
        destructive={togglingRoute?.enabled ?? false}
        title={togglingRoute?.enabled ? t("gwDisableConfirmTitle") : t("gwEnableConfirmTitle")}
        description={t(togglingRoute?.enabled ? "gwRouteToggleOffBody" : "gwRouteToggleOnBody", {
          provider: provider.slug,
          model: togglingRoute?.model_slug ?? "",
        })}
        confirmLabel={togglingRoute?.enabled ? t("gwDisable") : t("gwEnable")}
        pending={updateRoute.isPending}
        onConfirm={() => {
          if (!togglingRoute) return;
          updateRoute.mutate(
            {
              modelId: togglingRoute.model_id,
              routeId: togglingRoute.id,
              data: { enabled: !togglingRoute.enabled },
            },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("gwRouteUpdated") });
                setTogglingRoute(null);
                refresh();
              },
            },
          );
        }}
      />

      <ConfirmDialog
        open={removingRoute !== null}
        onOpenChange={(o) => !o && setRemovingRoute(null)}
        title={t("gwRouteDeleteConfirmTitle")}
        description={t("gwRouteDeleteConfirmBody", {
          provider: provider.slug,
          model: removingRoute?.model_slug ?? "",
        })}
        confirmLabel={t("gwDelete")}
        pending={removeRoute.isPending}
        onConfirm={() => {
          if (!removingRoute) return;
          removeRoute.mutate(
            { modelId: removingRoute.model_id, routeId: removingRoute.id },
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

      {/* `discovered` is passed as null rather than `?? []`: inside the dialog,
          "never fetched or the fetch failed" and "fetched, and the upstream does not
          have it" look different. */}
      <ProviderModelsDialog
        open={wiring}
        onOpenChange={setWiring}
        provider={provider}
        discovered={discovered}
        complete={result?.complete === true}
        // When a fetch happened but reached no conclusion, the reason is handed to
        // the dialog: that is a different sentence from "nothing has been fetched".
        discoverError={result && !result.ok ? (result.message ?? t("gwDiscoverRun")) : null}
        discoverPending={discover.isPending}
        onDiscover={run}
        onSaved={refresh}
      />
    </div>
  );
}
