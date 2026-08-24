import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import {
  apiErrorMessage,
  apiErrorStatus,
  gatewayStaffApi,
  type GatewayStaffTypes,
} from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  ConfirmDialog,
  FormColumn,
  InlineEmpty,
  LoadingState,
  PageContentsNav,
  PageHeader,
  RecordPage,
  resolveNavValue,
  useAdminTitle,
} from "@fairlb/ui";
import { useQueryClient } from "@tanstack/react-query";
import { Outlet, useBlocker, useLocation, useParams } from "@tanstack/react-router";
import { createContext, useCallback, useContext, useState } from "react";
import { useRecordBreadcrumb } from "./host";
import { ProviderModelsPanel } from "./provider-models";
import { ProviderOverviewPanel } from "./provider-overview";
import { KeyPanel, ProviderConfigPanel } from "./provider-panels";
import { ProviderStatusBadge } from "./providers-shared";

type ProviderContextValue = {
  provider: GatewayStaffTypes.GatewayProvider;
  refreshProvider: () => void;
  setConfigDirty: (dirty: boolean) => void;
};

const ProviderContext = createContext<ProviderContextValue | null>(null);

function useProviderRecord(): ProviderContextValue {
  const value = useContext(ProviderContext);
  if (!value) throw new Error("Provider task pages must be rendered inside GatewayProviderLayout");
  return value;
}

/** Persistent record header and task navigation for every provider page. */
export function GatewayProviderLayout() {
  const { t } = useI18n();
  const { providerId = "" } = useParams({ strict: false }) as { providerId?: string };
  const pathname = useLocation({ select: (location) => location.pathname });
  const providerQuery = gatewayStaffApi.useGetGatewayProvider(providerId, {
    query: { enabled: providerId !== "" },
  });
  const queryClient = useQueryClient();
  const update = gatewayStaffApi.useUpdateGatewayProvider();
  const toasts = useKumoToastManager();
  const [toggling, setToggling] = useState(false);
  const [configDirty, setConfigDirty] = useState(false);
  const blocker = useBlocker({
    shouldBlockFn: () => configDirty,
    enableBeforeUnload: configDirty,
    withResolver: true,
  });
  const provider = providerQuery.data;
  const refreshProvider = useCallback(() => {
    void providerQuery.refetch();
    void queryClient.invalidateQueries({
      queryKey: gatewayStaffApi.getListGatewayProvidersQueryKey(),
    });
  }, [providerQuery, queryClient]);

  useAdminTitle(provider?.slug);
  const notFound = apiErrorStatus(providerQuery.error) === 404;
  const pendingLabel =
    providerQuery.isPending || providerQuery.isFetching ? t("loading") : t("gwProviderNotFound");
  const breadcrumb = useRecordBreadcrumb(provider?.slug ?? pendingLabel);
  const basePath = `/gateway/providers/${providerId}`;
  const aspects = [
    { value: "overview", label: t("gwProviderTabOverview"), href: basePath },
    { value: "models", label: t("gwProviderTabModels"), href: `${basePath}/models` },
    { value: "keys", label: t("gwKeys"), href: `${basePath}/keys` },
    { value: "settings", label: t("gwProviderTabSettings"), href: `${basePath}/settings` },
  ];
  const active = resolveNavValue(aspects, pathname);

  const doToggle = () => {
    if (!provider) return;
    update.mutate(
      { providerId: provider.id, data: { enabled: !provider.enabled } },
      {
        onSuccess: () => {
          toasts.add({
            variant: "success",
            title: provider.enabled ? t("gwDisabledDone") : t("gwEnabledDone"),
          });
          setToggling(false);
          refreshProvider();
        },
      },
    );
  };

  return (
    <RecordPage
      header={
        <PageHeader
          breadcrumbs={breadcrumb}
          // The state badge belongs to the identity, not to the actions: it is
          // not something a reader can press, and in the actions row it wrapped
          // along with the buttons on a narrow viewport and was announced inside
          // the action group. Same shape as the organization record page.
          title={
            <span className="flex flex-wrap items-center gap-3">
              {provider?.slug ?? pendingLabel}
              {provider?.name && (
                <span className="text-base font-normal text-kumo-subtle">{provider.name}</span>
              )}
              {provider && (
                <ProviderStatusBadge
                  enabled={provider.enabled}
                  autoDisabled={provider.auto_disabled}
                />
              )}
            </span>
          }
          // No wrapper element: the header already lays the actions out as a
          // wrapping, end-justified row.
          actions={
            provider && (
              <Button size="sm" variant="outline" onClick={() => setToggling(true)}>
                {provider.enabled ? t("gwDisableProvider") : t("gwEnableProvider")}
              </Button>
            )
          }
          recordNav={{ value: active, items: aspects }}
        />
      }
    >
      <div className="space-y-6">
        {update.isError && <Alert>{apiErrorMessage(update.error)}</Alert>}
        {!provider && (providerQuery.isPending || providerQuery.isFetching) ? (
          <LoadingState label={t("loading")} />
        ) : notFound ? (
          <InlineEmpty title={t("gwProviderNotFound")} />
        ) : providerQuery.isError ? (
          <Alert>{apiErrorMessage(providerQuery.error)}</Alert>
        ) : !provider ? (
          <InlineEmpty title={t("gwProviderNotFound")} />
        ) : (
          <ProviderContext.Provider value={{ provider, refreshProvider, setConfigDirty }}>
            <Outlet />
          </ProviderContext.Provider>
        )}
      </div>

      <ConfirmDialog
        open={toggling}
        onOpenChange={setToggling}
        destructive={provider?.enabled ?? true}
        title={provider?.enabled ? t("gwDisableConfirmTitle") : t("gwEnableConfirmTitle")}
        description={
          provider?.enabled
            ? t("gwDisableConfirmBody", { slug: provider?.slug ?? "" })
            : t("gwEnableConfirmBody", { slug: provider?.slug ?? "" })
        }
        confirmLabel={provider?.enabled ? t("gwDisableProvider") : t("gwEnableProvider")}
        pending={update.isPending}
        onConfirm={doToggle}
      />
      <ConfirmDialog
        open={blocker.status === "blocked"}
        onOpenChange={(open) => !open && blocker.reset?.()}
        destructive={false}
        title={t("gwLeaveUnsavedTitle")}
        description={t("gwLeaveUnsavedConfigBody")}
        confirmLabel={t("gwLeaveUnsaved")}
        onConfirm={() => {
          setConfigDirty(false);
          blocker.proceed?.();
        }}
      />
    </RecordPage>
  );
}

/*
 * The aspect pages render no heading repeating their own nav item.
 *
 * Each of these used to open with a `SectionHeading` built from the *same*
 * message key the `RecordNav` item above it uses, so the word appeared twice
 * about 40px apart — and on the models face three times, because the panel's own
 * card is called "Models" too. Three of the nine aspect pages across the record
 * layouts never had one, so the strip plus the panel's own headings was already
 * the majority shape; this makes it the only one.
 *
 * Removing the wrapper moves the panels' own headings up a level: they are now
 * the top-level headings of the page, so they are h2 rather than h3. Skipping
 * that half would leave h1 followed by h3 — invisible on screen, and exactly the
 * defect `SectionHeading`'s own note describes.
 */
export function GatewayProviderOverviewPage() {
  const { provider } = useProviderRecord();
  return <ProviderOverviewPanel provider={provider} />;
}

export function GatewayProviderModelsPage() {
  const { provider } = useProviderRecord();
  return <ProviderModelsPanel provider={provider} />;
}

export function GatewayProviderKeysPage() {
  const { provider, refreshProvider } = useProviderRecord();
  return <KeyPanel provider={provider} onChanged={refreshProvider} />;
}

export function GatewayProviderSettingsPage() {
  const { t } = useI18n();
  const { provider, refreshProvider, setConfigDirty } = useProviderRecord();
  const items = [
    { href: "#provider-basics" as const, label: t("gwSectionBasics") },
    { href: "#provider-headers" as const, label: t("gwHdrProviderTitle") },
    { href: "#provider-transport" as const, label: t("gwTransportTitle") },
    { href: "#provider-capacity" as const, label: t("gwProviderCapacity") },
    { href: "#provider-cost" as const, label: t("gwProviderCostScalar") },
  ];
  return (
    <div className="flex min-w-0 flex-col gap-6 2xl:flex-row 2xl:items-start 2xl:gap-10">
      <FormColumn className="min-w-0 flex-1">
        <ProviderConfigPanel
          provider={provider}
          onSaved={refreshProvider}
          onDirtyChange={setConfigDirty}
        />
      </FormColumn>
      <PageContentsNav items={items} />
    </div>
  );
}
