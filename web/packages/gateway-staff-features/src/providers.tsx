import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n, type MessageKey } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  CheckboxGroupField,
  ConfirmDialog,
  DataTable,
  Field,
  FormRow,
  FormDialog,
  InlineEmpty,
  Input,
  LoadingState,
  PageHeader,
  SectionHeading,
  Select,
  StatusBadge,
  LoadMoreButton,
  RowTitleLink,
  Textarea,
  VendorMark,
  useAdminTitle,
  useCursorList,
  useScopedCursor,
  useDebounced,
} from "@fairlb/ui";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { keepPreviousData, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import {
  type AdjustmentMode,
  BASE_BPS,
  bpsFromModePercent,
  isAdjustmentValid,
  PROVIDER_MAX_BPS,
} from "./cost-adjustment";
import { AdjustmentFields } from "./cost-adjustment-fields";
import {
  ProviderStatusBadge,
  TRANSPORT_PLACEHOLDER,
  parseTransportText,
  protocolLabel,
  unfinishedPlaceholder,
  useProtocolItems,
} from "./providers-shared";
import { prefillFromVendor, useVendors, vendorBySlug, vendorLabel, type Vendor } from "./vendors";

export function GatewayProvidersPage() {
  const { t } = useI18n();
  useAdminTitle(t("navGatewayProviders"));
  return <ProvidersContent />;
}

// The list does overview and enable/disable only. A single provider's keys, headers,
// cost and discovery live on its detail page. As expanding rows inside the table,
// those four panels pushed every following provider out of the viewport, could not be
// linked to, and lost their state on collapse.
function ProvidersContent() {
  const { t } = useI18n();
  const [generation, setGeneration] = useState(0);
  const queryClient = useQueryClient();
  const vendorsQuery = useVendors();
  // Filters live in the URL, shaped like the model list's — the two pages do the
  // same job, and this one had no filters at all.
  const navigate = useNavigate();
  const urlSearch = useSearch({ strict: false }) as { q?: string };
  const search = urlSearch.q ?? "";
  const setSearch = (v: string) =>
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, q: v || undefined }),
      replace: true,
    });
  // 搜索发到服务端（ADR-0187）。此前是把 200 条全拉下来本地过滤，列表一分页
  // 那样只搜得到第一页。搜索词进 URL、不去抖：它由输入框直接驱动，而这一页的
  // 搜索本来就是「敲完看结果」，中间态没人读。
  const settledSearch = useDebounced(search, 250);
  // 游标只对它被铸出时的搜索词有效：换词即作废，否则第二页的游标会带进新搜索里，
  // 排在它之前的命中全部消失（useScopedCursor 的注释）。
  const [cursor, setCursor] = useScopedCursor(`${settledSearch}|${generation}`);
  const providers = gatewayStaffApi.useListGatewayProviders(
    { ...(settledSearch ? { q: settledSearch } : {}), ...(cursor ? { cursor } : {}) },
    // 查询键随搜索词变；没有它 data 会先变回 undefined，整页闪一次加载态。
    { query: { placeholderData: keepPreviousData } },
  );
  const { items: data, nextCursor } = useCursorList<GatewayStaffTypes.GatewayProvider>(
    providers,
    (p) => p.id,
    // 搜索词一变就丢掉累积：混着两个搜索词结果的列表比空列表更难解释。
    `${settledSearch}|${generation}`,
  );
  // 改动之后回第一页并清空缓存，理由同 ADR-0185：累积表按 id 去重且不替换已见的行。
  const refresh = () => {
    setCursor(undefined);
    setGeneration((g) => g + 1);
    void queryClient.resetQueries({ queryKey: gatewayStaffApi.getListGatewayProvidersQueryKey() });
  };
  const update = gatewayStaffApi.useUpdateGatewayProvider();
  const [creating, setCreating] = useState(false);
  const [toggling, setToggling] = useState<GatewayStaffTypes.GatewayProvider | null>(null);
  const toasts = useKumoToastManager();

  // Keep the page header on error: a failure should not cost the reader their sense
  // of which page they are on.
  if (providers.isError)
    return (
      <div className="space-y-6">
        <PageHeader title={t("navGatewayProviders")} description={t("staffGatewayProvidersDesc")} />
        <Alert>{apiErrorMessage(providers.error)}</Alert>
      </div>
    );
  const vendors = vendorsQuery.data?.items;

  // Enabling and disabling is one level of the kill switch and redirects live
  // traffic, so it is confirmed rather than fired on a single click.
  const doToggle = () => {
    if (!toggling) return;
    update.mutate(
      { providerId: toggling.id, data: { enabled: !toggling.enabled } },
      {
        onSuccess: () => {
          toasts.add({
            variant: "success",
            title: toggling.enabled ? t("gwDisabledDone") : t("gwEnabledDone"),
          });
          setToggling(null);
          refresh();
        },
      },
    );
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("navGatewayProviders")}
        description={t("staffGatewayProvidersDesc")}
        actions={<Button onClick={() => setCreating(true)}>{t("gwNewProvider")}</Button>}
      />
      {update.isError && <Alert>{apiErrorMessage(update.error)}</Alert>}

      <CreateProviderDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={() => refresh()}
      />

      {/* A single-card page does not repeat the card's title: the page title
          already says it. */}
      <Card className="space-y-3">
        {/* The toolbar sits inside the list card: a filter belongs to the table it
            filters. */}
        <FormRow className="sm:grid-cols-[minmax(16rem,1fr)]">
          <FormRow.Item>
            <Field label={t("gwProviderSearch")} htmlFor="provider-search">
              <Input
                id="provider-search"
                type="search"
                value={search}
                placeholder={t("gwProviderSearchHint")}
                onChange={(e) => setSearch(e.target.value)}
              />
            </Field>
          </FormRow.Item>
        </FormRow>
        <DataTable caption={t("navGatewayProviders")}>
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head>{t("gwColSlug")}</DataTable.Head>
              <DataTable.Head>{t("gwColVendor")}</DataTable.Head>
              <DataTable.Head>{t("gwColProtocols")}</DataTable.Head>
              <DataTable.Head>{t("gwColBaseUrl")}</DataTable.Head>
              <DataTable.Head>{t("gwColKeys")}</DataTable.Head>
              <DataTable.Head>{t("gwColRoutes")}</DataTable.Head>
              <DataTable.Head>{t("gwColStatus")}</DataTable.Head>
              <DataTable.Head />
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            {data.map((p) => (
              <DataTable.Row key={p.id} interactive>
                {/* `relative` is what lets the row title link cover the whole cell. */}
                <DataTable.Cell className="relative">
                  <span className="font-mono">
                    <RowTitleLink to="/gateway/providers/$providerId" params={{ providerId: p.id }}>
                      {p.slug}
                    </RowTitleLink>
                  </span>
                  {/* The display name was shown on **no screen at all**, while the
                      hint under that field in the create dialog promised "shown in
                      operator views" — the interface telling the operator something
                      untrue. The slug stays the primary identifier: it is the detail
                      page title, the route, and how the provider is referred to
                      everywhere. The name goes on a second line, because the whole
                      point of it is answering "which account is openai-main". */}
                  {p.name && <div className="text-kumo-subtle">{p.name}</div>}
                </DataTable.Cell>
                <DataTable.Cell>
                  {/* The tile is decorative: the label is right beside it, so
                      announcing the vendor twice would be the only thing it added. */}
                  <span className="flex items-center gap-2">
                    <VendorMark id={p.vendor} size="sm" aria-hidden="true" />
                    <span className="min-w-0 truncate">{vendorLabel(p.vendor, vendors)}</span>
                  </span>
                </DataTable.Cell>
                <DataTable.Cell>
                  {p.protocols.map((f) => t(protocolLabel(f))).join(" + ")}
                </DataTable.Cell>
                <DataTable.Cell className="max-w-[18rem] truncate font-mono">
                  {p.base_url}
                </DataTable.Cell>
                <DataTable.Cell className="space-x-2 whitespace-nowrap">
                  <span>{p.key_count ?? 0}</span>
                  {/* Without this, a provider with no key looks exactly like a
                      configured one and you have to open it to find out. It is an
                      operator hint, not a routing predicate: the provider can still
                      serve traffic for customers who bring their own key. */}
                  {(p.key_count ?? 0) === 0 && (
                    <StatusBadge tone="danger">{t("gwNoKeysBadge")}</StatusBadge>
                  )}
                </DataTable.Cell>
                {/* How many models it serves. With only a key count on the row, the
                    quantity that actually decides whether to open a provider — what
                    it is serving — was missing. The count includes enabled routes
                    only, as the contract states, and matches the readiness
                    checklist. */}
                <DataTable.Cell>{p.route_count ?? 0}</DataTable.Cell>
                <DataTable.Cell>
                  <ProviderStatusBadge enabled={p.enabled} autoDisabled={p.auto_disabled} />
                </DataTable.Cell>
                {/* The end of a row carries only actions that change state; opening
                    the detail page is the job of the slug at the head of it. A
                    "configure" link here pointed at exactly the same destination. */}
                <DataTable.Cell className="text-right whitespace-nowrap">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => setToggling(p)}
                    disabled={update.isPending}
                  >
                    {p.enabled ? t("gwDisable") : t("gwEnable")}
                  </Button>
                </DataTable.Cell>
              </DataTable.Row>
            ))}
            {/* No empty state while the query is pending: "there are no providers"
                and "we have not looked yet" are different facts, and defaulting the
                data to an empty array lets the first speak for the second. */}
            {providers.isPending ? (
              <DataTable.Row>
                <DataTable.Cell colSpan={7}>
                  <LoadingState label={t("loading")} />
                </DataTable.Cell>
              </DataTable.Row>
            ) : (
              data.length === 0 && (
                <DataTable.Row>
                  <DataTable.Cell colSpan={7}>
                    <InlineEmpty
                      title={settledSearch ? t("gwNoProviderMatch") : t("gwNoProviders")}
                    />
                  </DataTable.Cell>
                </DataTable.Row>
              )
            )}
          </DataTable.Body>
        </DataTable>
        <LoadMoreButton
          onClick={nextCursor ? () => setCursor(nextCursor) : undefined}
          pending={providers.isFetching}
          label={t("loadMore")}
        />
      </Card>

      <ConfirmDialog
        open={toggling !== null}
        onOpenChange={(o) => !o && setToggling(null)}
        destructive={toggling?.enabled ?? true}
        title={toggling?.enabled ? t("gwDisableConfirmTitle") : t("gwEnableConfirmTitle")}
        description={
          toggling?.enabled
            ? t("gwDisableConfirmBody", { slug: toggling?.slug ?? "" })
            : t("gwEnableConfirmBody", { slug: toggling?.slug ?? "" })
        }
        confirmLabel={toggling?.enabled ? t("gwDisable") : t("gwEnable")}
        pending={update.isPending}
        onConfirm={doToggle}
      />
    </div>
  );
}

/**
 * The create dialog collects three sections: the basics, an optional first key, and
 * the cost multiplier.
 *
 * The key is the one item that needs a second request. Its ciphertext is bound to the
 * row's id, so the row has to exist before anything can be encrypted, and the
 * contract accordingly has no "create a provider with a key attached" field. Hence
 * the chain: once the main resource exists, the second request goes out, and either
 * way the dialog navigates to the detail page. The provider is already created, and
 * staying in the dialog to resubmit would collide on the duplicate slug — turning
 * "add a key" into an unintelligible conflict error.
 */
/**
 * The vendor choices, grouped by kind so the list reads as "who is this" rather
 * than as an alphabet. The registry's own order is kept inside each group, and
 * the custom entry stays last: it is the answer when none of the others is.
 */
function vendorItems(
  vendors: readonly Vendor[] | undefined,
  t: (key: MessageKey) => string,
): { value: string; label: string }[] {
  const order: { kind: string; heading: MessageKey }[] = [
    { kind: "first_party", heading: "gwVendorKindFirstParty" },
    { kind: "platform", heading: "gwVendorKindPlatform" },
    { kind: "aggregator", heading: "gwVendorKindAggregator" },
    { kind: "custom", heading: "gwVendorKindCustom" },
  ];
  const out: { value: string; label: string }[] = [];
  for (const group of order) {
    for (const v of vendors ?? []) {
      if (v.kind !== group.kind) continue;
      out.push({ value: v.slug, label: `${v.label} · ${t(group.heading)}` });
    }
  }
  return out;
}

function CreateProviderDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
  /** Slugs already in use, so a suggested one does not collide on save. */
}) {
  const { t } = useI18n();
  const protocolItems = useProtocolItems();
  const create = gatewayStaffApi.useCreateGatewayProvider();
  const navigate = useNavigate();
  const toasts = useKumoToastManager();
  const vendorsQuery = useVendors();
  const vendors = vendorsQuery.data?.items;
  // Which platform this upstream belongs to. Everything below starts from it:
  // the base URL, the protocols on offer and the transport profile are all
  // properties of the platform, and typing them out of a recipe is exactly what
  // this choice replaces.
  const [vendor, setVendor] = useState("");
  const [region, setRegion] = useState("");
  const chosen = vendorBySlug(vendor, vendors);
  const [slug, setSlug] = useState("");
  // Multi-select: one upstream can speak several protocols at once.
  const [protocols, setProtocols] = useState<GatewayStaffTypes.GatewayProviderInputProtocolsItem[]>(
    ["openai"],
  );
  const [baseUrl, setBaseUrl] = useState("");
  const [transportText, setTransportText] = useState("");
  const transportParsed = parseTransportText(transportText);
  // A field the operator has edited is never overwritten by a later vendor
  // change. Retyping a base URL because the platform was corrected afterwards is
  // exactly the small loss that makes a form feel hostile.
  const [touched, setTouched] = useState<Record<string, boolean>>({});
  // 预填 slug 时要避开已占用的名字（openai 占了就填 openai-2）。
  //
  // 此前这份「已占用」是整页列表传进来的，列表一分页就只剩当前页——预填会给出
  // 一个已经被占的名字，提交时撞服务端 409。改成按 vendor 查：`freeSlug` 关心的
  // 只有以这个 vendor 名开头的那几条，而 `q=<vendor>` 恰好只返回它们，
  // **不依赖列表完整性**。服务端的唯一约束仍然是权威，这里只是别让人白填一次。
  const relatedProviders = gatewayStaffApi.useListGatewayProviders(
    { q: vendor, limit: 200 },
    { query: { enabled: vendor !== "" } },
  );
  const takenSlugs = useMemo(
    () => (relatedProviders.data?.items ?? []).map((p) => p.slug),
    [relatedProviders.data],
  );
  const markTouched = (field: string) => setTouched((prev) => ({ ...prev, [field]: true }));
  const [name, setName] = useState("");
  const [keyName, setKeyName] = useState("");
  const [secret, setSecret] = useState("");
  const [mode, setMode] = useState<AdjustmentMode>("original");
  const [percent, setPercent] = useState("0");
  // While the second request is in flight the dialog can neither be closed nor
  // resubmitted: by then the provider already exists.
  const [chaining, setChaining] = useState(false);

  const bps = bpsFromModePercent(mode, percent);
  const costValid = isAdjustmentValid(bps, PROVIDER_MAX_BPS);

  // Applying a preset is one function, called by the vendor choice and by the
  // region choice, so the two cannot fill the form in differently.
  //
  // forceBaseURL separates the two cases. A vendor change must not overwrite a
  // URL somebody has typed. Choosing an endpoint *is* a request to change that
  // URL, so honouring `touched` there left the select reading "International"
  // beside the mainland host -- and that is what got saved, on a pair of
  // endpoints that take different credentials.
  const applyVendor = (next: string, nextRegion: string, forceBaseURL = false) => {
    const fill = prefillFromVendor(vendorBySlug(next, vendors), takenSlugs, nextRegion);
    if (!touched.slug) setSlug(fill.slug);
    if (forceBaseURL || !touched.baseUrl) setBaseUrl(fill.baseUrl);
    // A protocol choice is about one vendor and means nothing for the next:
    // a set ticked for DeepSeek would be sent for OpenAI, unseen once the
    // form shows the single fixed protocol. So a vendor change starts the
    // protocols over from the new vendor's default and forgets the edit --
    // unlike the base URL, which is the operator's own and survives.
    if (next !== vendor) {
      setProtocols(fill.protocols as GatewayStaffTypes.GatewayProviderInputProtocolsItem[]);
      setTouched((prev) => ({ ...prev, protocols: false }));
    } else if (!touched.protocols) {
      setProtocols(fill.protocols as GatewayStaffTypes.GatewayProviderInputProtocolsItem[]);
    }
    if (!touched.transport) setTransportText(fill.transportText);
  };
  // 占用清单是异步到的，而 applyVendor 是同步跑的：vendor 刚变那一刻清单还是上一次
  // 的。清单到了之后重算一次，条件是读者自己还没动过这个字段——动过就轮不到预填说话。
  useEffect(() => {
    if (touched.slug || vendor === "" || !relatedProviders.isSuccess) return;
    setSlug(prefillFromVendor(vendorBySlug(vendor, vendors), takenSlugs, region).slug);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [takenSlugs, relatedProviders.isSuccess, vendor, region, vendors, touched.slug]);

  const chooseVendor = (next: string) => {
    setVendor(next);
    setRegion("");
    applyVendor(next, "");
  };
  const chooseRegion = (next: string) => {
    setRegion(next);
    applyVendor(vendor, next, true);
  };

  // A base URL still carrying a preset's placeholder is refused on save, so the
  // form says so where the value is rather than after a round trip.
  const baseUrlUnfinished = /[{}]/.test(baseUrl);
  // The same rule for the profile, where the hosted-platform presets actually
  // keep their placeholders: a project and a region that only the operator can
  // fill in.
  const transportUnfinished =
    transportParsed.ok && unfinishedPlaceholder(transportParsed.value) !== "";

  const reset = () => {
    setVendor("");
    setRegion("");
    setTouched({});
    setSlug("");
    setBaseUrl("");
    setTransportText("");
    setProtocols(["openai"]);
    setName("");
    setKeyName("");
    setSecret("");
    setMode("original");
    setPercent("0");
    create.reset();
  };

  /**
   * Where it lands depends on whether a first key was entered.
   *
   * The detail page's default face is the overview, but that must not wash out the
   * intent here: **with no key, the next step is unavoidably to add one**, so it goes
   * straight to the keys face. With a key, it lands on the overview, where the
   * readiness checklist is and the next step is to fetch models.
   * Navigation is the feedback; no notification is stacked on top of it.
   */
  const finish = (providerId: string, hasKey: boolean) => {
    onOpenChange(false);
    reset();
    onCreated();
    void navigate({
      to: "/gateway/providers/$providerId",
      params: { providerId },
      hash: hasKey ? "" : "provider-keys",
    });
  };

  const submit = () =>
    create.mutate(
      {
        data: {
          slug,
          vendor,
          // Sent only when the operator changed it: left alone, the server
          // applies the vendor's default, which is the same set the form
          // showed -- one rule, held in one place.
          ...(touched.protocols || chosen?.slug === "custom" ? { protocols } : {}),
          base_url: baseUrl,
          ...(name.trim() ? { name: name.trim() } : {}),
          // Sent only when there is one: an empty object would be stored as a
          // profile that says nothing, which is a third state nobody needs.
          ...(transportParsed.ok && Object.keys(transportParsed.value).length > 0
            ? { transport: transportParsed.value }
            : {}),
          // Omitted means the stored default applies. Sent only when the operator
          // actually adjusted it, so that "left alone" is not written as an
          // explicit assignment.
          ...(costValid && bps !== BASE_BPS ? { cost_multiplier_bps: bps } : {}),
        },
      },
      {
        onSuccess: (created) => {
          if (!secret.trim()) {
            finish(created.id, false);
            return;
          }
          setChaining(true);
          let keyOk = true;
          // Called directly rather than through the mutation hook: a global failure
          // notification is attached to the mutation cache, so the hook would raise
          // two notices about one event — and the global one is precisely the one
          // that omits "the provider was created", which is the only thing that
          // matters at this moment.
          void gatewayStaffApi
            .createGatewayProviderKey(created.id, {
              secret: secret.trim(),
              ...(keyName.trim() ? { name: keyName.trim() } : {}),
            })
            .catch((error: unknown) => {
              // A failed second request means the provider exists without a key, so
              // the keys face is still where this should land.
              keyOk = false;
              toasts.add({
                variant: "error",
                title: t("gwProviderCreatedKeyFailed"),
                description: apiErrorMessage(error),
              });
            })
            .finally(() => {
              setChaining(false);
              finish(created.id, keyOk);
            });
        },
      },
    );

  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        if (chaining) return;
        onOpenChange(next);
        if (!next) reset();
      }}
      size="lg"
      title={t("gwNewProvider")}
      description={t("gwCreateProviderDialogHint")}
      error={create.isError ? apiErrorMessage(create.error) : undefined}
      submitLabel={t("gwCreate")}
      submitDisabled={
        !vendor ||
        !slug ||
        !baseUrl ||
        baseUrlUnfinished ||
        protocols.length === 0 ||
        !transportParsed.ok ||
        transportUnfinished ||
        !costValid
      }
      pending={create.isPending || chaining}
      onSubmit={submit}
    >
      <SectionHeading level="sub" as="h3">
        {t("gwSectionBasics")}
      </SectionHeading>
      {/* The first question, because every field under it is an answer that
          follows from it. */}
      <Field label={t("gwVendorPick")} hint={t("gwVendorPickHint")}>
        <Select
          value={vendor}
          placeholder={t("gwVendorPlaceholder")}
          onValueChange={(v) => chooseVendor(v ?? "")}
          items={vendorItems(vendors, t)}
        />
      </Field>
      {chosen &&
        chosen.base_urls.length > 1 && (
          // Several endpoints is a real choice, not a mirror pair: a mainland-China
          // host and an international one take different credentials.
          <Field label={t("gwVendorRegion")}>
            <Select
              value={region || (chosen.base_urls[0]?.label ?? "")}
              onValueChange={(v) => chooseRegion(v ?? "")}
              items={chosen.base_urls.map((b) => ({
                value: b.label ?? "",
                label: b.label ?? b.url,
              }))}
            />
          </Field>
        )}
      {chosen && !chosen.model_listing && (
        <p className="text-base text-kumo-subtle">{t("gwVendorNoListing")}</p>
      )}
      <Field label={t("gwColSlug")} htmlFor="p-slug">
        <Input
          id="p-slug"
          value={slug}
          autoFocus
          required
          onChange={(e) => {
            markTouched("slug");
            setSlug(e.target.value);
          }}
        />
      </Field>
      {/* Protocol follows the vendor. A vendor that publishes one protocol has
          settled the question by being picked, so there is nothing to tick; one
          that publishes several starts with the set its preset is wired for
          and can be narrowed or widened; the custom vendor has no entry to
          answer for it and must be told. Limited to what the platform
          publishes either way: a protocol it does not speak produces routes
          that are probed red on every endpoint. */}
      {chosen && chosen.protocols.length === 1 ? (
        <Field label={t("gwColProtocols")} hint={t("gwProtocolsFixedHint")}>
          <p className="text-base">{t(protocolLabel(chosen.protocols[0] ?? ""))}</p>
        </Field>
      ) : (
        <CheckboxGroupField
          legend={t("gwColProtocols")}
          hint={chosen?.slug === "custom" ? t("gwProtocolsHint") : t("gwProtocolsDefaultHint")}
          options={protocolItems.filter((i) => !chosen || chosen.protocols.includes(i.value))}
          value={protocols}
          required
          columns={2}
          onValueChange={(next) => {
            markTouched("protocols");
            setProtocols(next as GatewayStaffTypes.GatewayProviderInputProtocolsItem[]);
          }}
        />
      )}
      <Field
        label={t("gwColBaseUrl")}
        htmlFor="p-url"
        error={baseUrlUnfinished ? t("gwBaseUrlUnfinished") : undefined}
      >
        <Input
          id="p-url"
          value={baseUrl}
          required
          onChange={(e) => {
            markTouched("baseUrl");
            setBaseUrl(e.target.value);
          }}
        />
      </Field>
      <Field label={t("gwProviderName")} htmlFor="p-name" hint={t("gwProviderNameHint")}>
        <Input id="p-name" value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      {/* Collapsed, because most upstreams need nothing here and an open JSON box
          reads as a field somebody has to fill in. Prefilled from the vendor for
          the ones that do. */}
      <details open={transportText !== ""}>
        <summary className="cursor-pointer text-base text-kumo-subtle">
          {t("gwTransportAdvanced")}
        </summary>
        <div className="pt-2">
          <Field
            label={t("gwTransportLabel")}
            htmlFor="p-transport"
            hint={t("gwTransportHint")}
            error={
              !transportParsed.ok
                ? t("gwTransportNotJson")
                : transportUnfinished
                  ? t("gwTransportUnfinished")
                  : undefined
            }
          >
            <Textarea
              id="p-transport"
              rows={6}
              className="font-mono"
              placeholder={TRANSPORT_PLACEHOLDER}
              value={transportText}
              onChange={(e) => {
                markTouched("transport");
                setTransportText(e.target.value);
              }}
            />
          </Field>
        </div>
      </details>

      <SectionHeading level="sub" as="h3">
        {t("gwSectionInitialKey")}
      </SectionHeading>
      <p className="text-base text-kumo-subtle">{t("gwSectionInitialKeyHint")}</p>
      <Field label={t("gwKeyNameOpt")} htmlFor="p-key-name">
        <Input id="p-key-name" value={keyName} onChange={(e) => setKeyName(e.target.value)} />
      </Field>
      <Field label={t("gwKeySecret")} htmlFor="p-secret" hint={t("gwKeySecretHint")}>
        <Input
          id="p-secret"
          type="password"
          autoComplete="off"
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
        />
      </Field>

      <SectionHeading level="sub" as="h3">
        {t("gwProviderCostScalar")}
      </SectionHeading>
      <p className="text-base text-kumo-subtle">{t("gwProviderCostScalarHint")}</p>
      <AdjustmentFields
        id="p-cost"
        mode={mode}
        onModeChange={setMode}
        percent={percent}
        onPercentChange={setPercent}
        bps={bps}
        valid={costValid}
      />
    </FormDialog>
  );
}
