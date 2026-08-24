import { LinkButton } from "@cloudflare/kumo/components/button";
import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { DotsThreeIcon } from "@phosphor-icons/react";
import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Card,
  Checkbox,
  ConfirmDialog,
  DataTable,
  Field,
  FormDialog,
  InlineEmpty,
  Input,
  PageHeader,
  RowActions,
  RowTitleLink,
  StatusBadge,
  useAdminTitle,
  useCursorList,
  useScopedCursor,
  useDebounced,
  LoadMoreButton,
} from "@fairlb/ui";
import { keepPreviousData, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

// Access tiers.
//
// This page expresses one thing: which models a tier may call. Pricing and routing
// each have their own place — putting all three in one form makes "open up one more
// model" look like it requires renegotiating a price.

export function GatewayTiersPage() {
  const { t } = useI18n();
  useAdminTitle(t("navGatewayTiers"));
  return <TiersContent />;
}

function TiersContent() {
  const { t } = useI18n();
  const [search, setSearch] = useState("");
  const [generation, setGeneration] = useState(0);
  const queryClient = useQueryClient();
  // 搜索发到服务端（ADR-0187/0191）。这条列表此前无声封顶 200 条，第 201 个档位
  // 不是「没勾」而是**根本不在列表里**；分页之后本地过滤更只搜得到第一页。
  const settledSearch = useDebounced(search, 250);
  // 游标只对它被铸出时的搜索词有效（useScopedCursor）
  const [cursor, setCursor] = useScopedCursor(`${settledSearch}|${generation}`);
  const tiers = gatewayStaffApi.useListGatewayTiers(
    { ...(settledSearch ? { q: settledSearch } : {}), ...(cursor ? { cursor } : {}) },
    // keepPreviousData 是正确性不是优化：没有它，每敲一个字母，正在打字的那个框
    // 就随整页一起被换成加载态。
    { query: { placeholderData: keepPreviousData } },
  );
  const create = gatewayStaffApi.useCreateGatewayTier();
  const update = gatewayStaffApi.useUpdateGatewayTier();
  const remove = gatewayStaffApi.useDeleteGatewayTier();
  const setDefault = gatewayStaffApi.useSetDefaultGatewayTier();

  const [creating, setCreating] = useState(false);
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  /**
   * Whether the new tier grants everything or grants exactly what it lists.
   *
   * It is asked at creation rather than left to a default, because the two
   * answers are opposite and neither is obviously the one intended. It starts
   * off: creating a tier is an act of restricting, and the reader who wanted
   * "everything" sees an unticked box and ticks it, while the reader who
   * wanted a restriction gets what they wanted by leaving it alone.
   */
  const [allowAll, setAllowAll] = useState(false);
  /**
   * Editing a tier's attributes: its name and description. **The page had no way to
   * do this at all**: models could be edited, a default set, a tier disabled or
   * deleted — but once created, a tier could never be renamed. The names seeded by
   * the initial migration were therefore untouchable, and the default tier, which
   * cannot be disabled or deleted either, could not be changed by any action at all.
   *
   * Two scalars means an in-page dialog rather than a detail page of its own. The
   * slug is not in this form: it is the stable identifier other records refer to, and
   * the contract declares it immutable.
   */
  const [renaming, setRenaming] = useState<GatewayStaffTypes.GatewayTier | null>(null);
  const [editName, setEditName] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editAllowAll, setEditAllowAll] = useState(false);
  const [deleting, setDeleting] = useState<GatewayStaffTypes.GatewayTier | null>(null);
  const [promoting, setPromoting] = useState<GatewayStaffTypes.GatewayTier | null>(null);
  // Enabling or disabling changes model access for every customer on the tier, so it
  // is confirmed. Delete on the same row always was; this was the one that fired
  // straight from the click.
  const [toggling, setToggling] = useState<GatewayStaffTypes.GatewayTier | null>(null);
  const toasts = useKumoToastManager();
  // The query parameter is the only switch that opens the model editor: it can be
  // linked to, survives a reload, and closes on back. This page has no detail page,
  // and the editor is a tier's actual content.
  const urlSearch = useSearch({ strict: false }) as { tier?: string };
  const navigate = useNavigate();
  const closeEditor = () => void navigate({ to: ".", search: {}, replace: true });

  // 改动之后回第一页并清空缓存（ADR-0185）：累积表按 id 去重且不替换已见的行。
  const refresh = () => {
    setCursor(undefined);
    setGeneration((g) => g + 1);
    void queryClient.resetQueries({ queryKey: gatewayStaffApi.getListGatewayTiersQueryKey() });
  };

  // 累积表在提前 return 之上：hook 不能站在条件返回之后。
  const cursored = useCursorList<GatewayStaffTypes.GatewayTier>(
    tiers,
    (tier) => tier.id,
    // 搜索词一变就丢掉累积：混着两个搜索词结果的列表比空列表更难解释。
    `${settledSearch}|${generation}`,
  );

  // Keep the page header on error: a failure should not cost the reader their sense
  // of which page they are on.
  if (tiers.isError)
    return (
      <div className="space-y-6">
        {/* The description belongs on the error branch too: it says what this page
            is for, which does not depend on whether the data arrived. Without it the
            header is a line shorter than usual, and the reader thinks they are
            somewhere else. */}
        <PageHeader title={t("navGatewayTiers")} description={t("tiersHint")} />
        <Alert>{apiErrorMessage(tiers.error)}</Alert>
      </div>
    );

  const { items, nextCursor } = cursored;
  const editing = items.find((tier) => tier.id === urlSearch.tier) ?? null;
  const mutError = create.error ?? update.error ?? remove.error ?? setDefault.error;

  return (
    <div className="space-y-6">
      {/* The page-level description belongs in the page header. */}
      <PageHeader
        title={t("navGatewayTiers")}
        description={t("tiersHint")}
        actions={<Button onClick={() => setCreating(true)}>{t("tierCreate")}</Button>}
      />

      {mutError && <Alert>{apiErrorMessage(mutError)}</Alert>}

      <FormDialog
        open={creating}
        onOpenChange={(next) => {
          setCreating(next);
          if (!next) {
            setSlug("");
            setName("");
            setDescription("");
            setAllowAll(false);
            create.reset();
          }
        }}
        title={t("tierCreate")}
        error={create.isError ? apiErrorMessage(create.error) : undefined}
        submitLabel={t("tierCreate")}
        submitDisabled={slug.trim() === ""}
        pending={create.isPending}
        onSubmit={() =>
          create.mutate(
            {
              data: {
                slug: slug.trim(),
                name: name.trim(),
                allow_all_models: allowAll,
                ...(description.trim() ? { description: description.trim() } : {}),
              },
            },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("tierCreatedDone") });
                setCreating(false);
                setSlug("");
                setName("");
                setDescription("");
                setAllowAll(false);
                refresh();
              },
            },
          )
        }
      >
        <Field label={t("tierSlug")} htmlFor="tier-slug" hint={t("tierSlugHint")}>
          <Input
            id="tier-slug"
            value={slug}
            autoFocus
            placeholder="vip"
            onChange={(e) => setSlug(e.target.value)}
          />
        </Field>
        <Field label={t("tierName")} htmlFor="tier-name">
          <Input id="tier-name" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        {/* The description had **no interface at either end**: a column in storage, a
            field in the contract, values seeded by the migration — and no way to
            write it or read it back. An input is what makes it a real field. */}
        <Field label={t("tierDescription")} htmlFor="tier-description" hint={t("optional")}>
          <Input
            id="tier-description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        {/* The tier's most consequential property, so it is on the form that
            creates it rather than only on a later edit. */}
        <Field label="" hint={t("tierAllowAllHint")}>
          <Checkbox
            checked={allowAll}
            onCheckedChange={(next) => setAllowAll(next === true)}
            label={t("tierAllowAll")}
          />
        </Field>
      </FormDialog>

      {/* Editing attributes: name and description. The slug is not here — it is the
          stable identifier other records refer to. */}
      <FormDialog
        open={renaming !== null}
        onOpenChange={(next) => {
          if (!next) {
            setRenaming(null);
            update.reset();
          }
        }}
        title={t("tierEdit")}
        description={renaming ? t("tierEditHint", { slug: renaming.slug }) : undefined}
        error={update.isError ? apiErrorMessage(update.error) : undefined}
        submitLabel={t("save")}
        submitDisabled={editName.trim() === ""}
        pending={update.isPending}
        onSubmit={() => {
          if (!renaming) return;
          update.mutate(
            {
              tierId: renaming.id,
              data: {
                name: editName.trim(),
                description: editDescription.trim(),
                allow_all_models: editAllowAll,
              },
            },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("tierUpdatedDone") });
                setRenaming(null);
                refresh();
              },
            },
          );
        }}
      >
        <Field label={t("tierName")} htmlFor="tier-edit-name">
          <Input
            id="tier-edit-name"
            value={editName}
            autoFocus
            onChange={(e) => setEditName(e.target.value)}
          />
        </Field>
        <Field label={t("tierDescription")} htmlFor="tier-edit-description" hint={t("optional")}>
          <Input
            id="tier-edit-description"
            value={editDescription}
            onChange={(e) => setEditDescription(e.target.value)}
          />
        </Field>
        {/* Turning this on clears the tier's model list server-side, in the
            same transaction. The hint says so rather than letting the reader
            discover it by watching a list they spent time on disappear. */}
        <Field label="" hint={t("tierAllowAllHint")}>
          <Checkbox
            checked={editAllowAll}
            onCheckedChange={(next) => setEditAllowAll(next === true)}
            label={t("tierAllowAll")}
          />
        </Field>
      </FormDialog>

      <Card className="space-y-3">
        <div className="max-w-md">
          <Field label={t("gwSearchTiers")} htmlFor="tier-search">
            <Input
              id="tier-search"
              type="search"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
          </Field>
        </div>
        {/* 搜索框在卡片里、恒在。空态若换掉整张卡，运营员打错一个字母就连输入框
            一起失去，没有办法把它改回来。空态的措辞也分两种：带着搜索词的空结果
            说的是「没有匹配」，不是「一个档位都没有」。 */}
        {items.length === 0 ? (
          <InlineEmpty title={settledSearch ? t("gwNoTierMatch") : t("tiersEmpty")} />
        ) : (
          <DataTable caption={t("navGatewayTiers")}>
            <DataTable.Header>
              <DataTable.Row>
                <DataTable.Head className="pr-3">{t("tierSlug")}</DataTable.Head>
                <DataTable.Head className="pr-3">{t("tierModelCount")}</DataTable.Head>
                <DataTable.Head className="pr-3">{t("gwColOrgs")}</DataTable.Head>
                <DataTable.Head>{""}</DataTable.Head>
              </DataTable.Row>
            </DataTable.Header>
            <DataTable.Body>
              {items.map((tier) => (
                <DataTable.Row key={tier.id} interactive>
                  {/* `relative` is what lets the row title link cover the whole cell.
                      The identity column is shaped like the provider list's: the
                      primary identifier is the link — the slug, since that is what
                      other records refer to — with the name and description on
                      following lines. */}
                  <DataTable.Cell className="relative pr-3">
                    <span className="font-mono">
                      <RowTitleLink to="." search={{ tier: tier.id }}>
                        {tier.slug}
                      </RowTitleLink>
                    </span>
                    {tier.is_default && (
                      <span className="ml-2">
                        <StatusBadge tone="neutral">{t("tierDefaultBadge")}</StatusBadge>
                      </span>
                    )}
                    {tier.status === "disabled" && (
                      <span className="ml-2">
                        <StatusBadge tone="warning">{t("tierStatusDisabled")}</StatusBadge>
                      </span>
                    )}
                    {tier.name && <div className="text-kumo-subtle">{tier.name}</div>}
                    {/* The description could be neither written nor read: a column in
                        storage, a field in the contract, values seeded by the
                        migration, and not one of the three screens showed it. */}
                    {tier.description && <div className="text-kumo-subtle">{tier.description}</div>}
                  </DataTable.Cell>
                  {/* Three readings, not two. "Every model" is a property of the
                      tier and not a count; a restricting tier with nothing listed
                      grants nothing, which has to look different from granting
                      everything, since printing zero for both is precisely how the
                      two used to be confused. */}
                  <DataTable.Cell className="pr-3 tabular-nums">
                    {tier.allow_all_models ? (
                      t("tierUnrestricted")
                    ) : tier.model_count === 0 ? (
                      <StatusBadge tone="warning">{t("tierAllowsNothing")}</StatusBadge>
                    ) : (
                      tier.model_count
                    )}
                  </DataTable.Cell>
                  <DataTable.Cell className="pr-3 tabular-nums">{tier.org_count}</DataTable.Cell>
                  <DataTable.Cell>
                    {/* The row action shell is a component rather than a hand-written
                        flex wrapper, so the rule lives in one place instead of being
                        copied from whichever page got it right. */}
                    {/* Two visible actions plus an overflow menu. "Only real actions
                        at the end of a row" says nothing about how many, so the count
                        kept climbing — five side by side on a non-default tier. The
                        rule now has a number: **at most two side by side, the rest in
                        the overflow**. The two kept outside are the most frequent
                        ones — edit the attributes, edit what the tier may call —
                        while the rare and destructive ones go into the menu. */}
                    <RowActions align="start">
                      <Button
                        variant="outline"
                        onClick={() => {
                          setEditName(tier.name ?? "");
                          setEditDescription(tier.description ?? "");
                          setEditAllowAll(tier.allow_all_models);
                          setRenaming(tier);
                        }}
                      >
                        {t("tierEdit")}
                      </Button>
                      {/* A real link: the editor is driven by a query parameter, so
                          middle-click and copy-link-address both have to work. */}
                      <LinkButton variant="outline" href={`/gateway/tiers?tier=${tier.id}`}>
                        {t("tierEditModels")}
                      </LinkButton>
                      {/* The default tier has only the first two actions: making it
                          the default is meaningless, and disabling or deleting it is
                          refused by the server — offering a button that is certain to
                          be rejected is worse than not offering it — so it has no
                          overflow menu at all. */}
                      {!tier.is_default && (
                        <DropdownMenu>
                          <DropdownMenu.Trigger
                            render={(props) => (
                              <Button
                                {...props}
                                size="sm"
                                variant="ghost"
                                icon={<DotsThreeIcon />}
                                aria-label={t("tierMoreActions", { slug: tier.slug })}
                              />
                            )}
                          />
                          <DropdownMenu.Content align="end">
                            {/* Items must be wrapped in a group: an item reads its
                                group's context, and without one it throws the moment
                                the menu opens. */}
                            <DropdownMenu.Group>
                              <DropdownMenu.Item onClick={() => setPromoting(tier)}>
                                {t("tierSetDefault")}
                              </DropdownMenu.Item>
                              <DropdownMenu.Item onClick={() => setToggling(tier)}>
                                {tier.status === "active" ? t("tierDisable") : t("tierEnable")}
                              </DropdownMenu.Item>
                              <DropdownMenu.Item variant="danger" onClick={() => setDeleting(tier)}>
                                {t("tierDelete")}
                              </DropdownMenu.Item>
                            </DropdownMenu.Group>
                          </DropdownMenu.Content>
                        </DropdownMenu>
                      )}
                    </RowActions>
                  </DataTable.Cell>
                </DataTable.Row>
              ))}
            </DataTable.Body>
          </DataTable>
        )}
        <LoadMoreButton
          onClick={nextCursor ? () => setCursor(nextCursor) : undefined}
          pending={tiers.isFetching}
          label={t("loadMore")}
        />
      </Card>

      <TierModelsEditor
        tier={editing}
        onClose={closeEditor}
        onSaved={() => {
          closeEditor();
          refresh();
          toasts.add({ variant: "success", title: t("tierModelsSave") });
        }}
      />

      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t("tierDeleteConfirmTitle")}
        description={t("tierDeleteConfirmBody", { slug: deleting?.slug ?? "" })}
        confirmLabel={t("tierDelete")}
        pending={remove.isPending}
        onConfirm={() => {
          if (!deleting) return;
          remove.mutate(
            { tierId: deleting.id },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("tierDeletedDone") });
                setDeleting(null);
                refresh();
              },
            },
          );
        }}
      />

      <ConfirmDialog
        open={toggling !== null}
        onOpenChange={(o) => !o && setToggling(null)}
        destructive={toggling?.status === "active"}
        title={
          toggling?.status === "active" ? t("tierDisableConfirmTitle") : t("tierEnableConfirmTitle")
        }
        description={
          toggling?.status === "active"
            ? t("tierDisableConfirmBody", { slug: toggling?.slug ?? "" })
            : t("tierEnableConfirmBody", { slug: toggling?.slug ?? "" })
        }
        confirmLabel={toggling?.status === "active" ? t("tierDisable") : t("tierEnable")}
        pending={update.isPending}
        onConfirm={() => {
          if (!toggling) return;
          update.mutate(
            {
              tierId: toggling.id,
              data: { status: toggling.status === "active" ? "disabled" : "active" },
            },
            {
              onSuccess: () => {
                toasts.add({
                  variant: "success",
                  title: toggling.status === "active" ? t("gwDisabledDone") : t("gwEnabledDone"),
                });
                setToggling(null);
                refresh();
              },
            },
          );
        }}
      />

      {/* Changing the default tier changes what every customer without an explicit
          tier may call, which is worth a confirmation. */}
      <ConfirmDialog
        open={promoting !== null}
        onOpenChange={(o) => !o && setPromoting(null)}
        destructive={false}
        title={t("tierSetDefaultConfirmTitle")}
        description={t("tierSetDefaultConfirmBody", { slug: promoting?.slug ?? "" })}
        confirmLabel={t("tierSetDefault")}
        pending={setDefault.isPending}
        onConfirm={() => {
          if (!promoting) return;
          setDefault.mutate(
            { tierId: promoting.id },
            {
              onSuccess: () => {
                toasts.add({ variant: "success", title: t("tierDefaultDone") });
                setPromoting(null);
                refresh();
              },
            },
          );
        }}
      />
    </div>
  );
}

// The model editor edits the whole set: tick, then save, sending the complete set.
// Not saved row by row — that would make every tick an access change on its own, and
// leaving halfway would strand a half-configured tier.
// A dialog rather than a card spliced under the table, because it is a single form
// editing a single record.
// It stays mounted: a null tier means closed, and the query is gated on that.
function TierModelsEditor({
  tier,
  onClose,
  onSaved,
}: {
  tier: GatewayStaffTypes.GatewayTier | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const current = gatewayStaffApi.useListGatewayTierModels(tier?.id ?? "", {
    query: { enabled: tier != null },
  });
  // 候选从**搜索**来，不再是整份目录（ADR-0189）。
  //
  // 已配置的那些不依赖搜索：它们来自 `current`，是这个档位自己的端点，完整、
  // 且自带 slug 与 enabled。目录分页之后，「已配置恒在恒勾选」（ADR-0086）
  // 若还靠目录完整性来兑现，一个排在第三页的已配置模型会**根本不出现在列表里**
  // ——读者看到的不是「没勾」，是「没有这一项」。
  const [query, setQuery] = useState("");
  const settledQuery = useDebounced(query, 250);
  const candidates = gatewayStaffApi.useListGatewayModels(
    { q: settledQuery },
    { query: { enabled: tier != null, placeholderData: keepPreviousData } },
  );
  const save = gatewayStaffApi.useSetGatewayTierModels();
  const [picked, setPicked] = useState<Set<string> | null>(null);

  // Switching target discards the local selection: one tier's ticks must not carry
  // over into the next.
  useEffect(() => {
    setPicked(null);
    save.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- reset only when the target changes
  }, [tier?.id]);

  // The tier is already cleared while the close animation runs, so the last content
  // is retained to render against.
  const shown = useRef<GatewayStaffTypes.GatewayTier | null>(tier);
  useEffect(() => {
    if (tier) shown.current = tier;
  }, [tier]);
  const display = tier ?? shown.current;

  // The local selection is initialized only once the current set has arrived —
  // otherwise the reader first sees nothing ticked, which is exactly what
  // "unrestricted" looks like, and reads as a tier that has been emptied.
  const configured = current.data?.items ?? [];
  const selected = picked ?? new Set(configured.map((m) => m.id));
  // 列表 = 打开那一刻已配置的那些（恒在，取消勾选后也留着，否则一取消就消失、
  // 想勾回来都找不到）+ 搜索结果里还不在其中的。
  const configuredIds = new Set(configured.map((m) => m.id));
  // 两个来源的行只在这三样上被读到，故取它们的交集作类型。**`enabled` 是可选的**：
  // 已配置那一侧的契约把它写成非必填，而 `!m.enabled` 会把「不知道」画成「已停用」，
  // 所以下面判的是 `=== false`。
  const rows: { id: string; slug: string; enabled?: boolean }[] = [
    ...configured,
    ...(candidates.data?.items ?? []).filter((m) => !configuredIds.has(m.id)),
  ];
  const toggle = (id: string) => {
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setPicked(next);
  };

  return (
    <FormDialog
      open={tier !== null}
      onOpenChange={(next) => !next && onClose()}
      title={t("tierModelsTitle", { slug: display?.slug ?? "" })}
      error={
        current.isError
          ? apiErrorMessage(current.error)
          : save.isError
            ? apiErrorMessage(save.error)
            : undefined
      }
      submitLabel={t("tierModelsSave")}
      submitDisabled={current.isLoading || current.isError || display?.allow_all_models === true}
      pending={save.isPending}
      onSubmit={() => {
        if (!tier) return;
        save.mutate(
          { tierId: tier.id, data: { model_ids: [...selected] } },
          { onSuccess: onSaved },
        );
      }}
    >
      {/* An allow-all tier has nothing to pick: the write path refuses a list on
          one, so offering the checkboxes would be offering an action that
          cannot succeed. */}
      {display?.allow_all_models === true && (
        <p className="text-base text-kumo-subtle">{t("tierAllowAllLocked")}</p>
      )}

      {display?.allow_all_models !== true && selected.size === 0 && (
        <p className="text-base text-kumo-subtle">{t("tierModelsEmptyHint")}</p>
      )}

      {display?.allow_all_models !== true && (
        <Field label={t("gwSearchModel")} htmlFor="tier-model-search">
          <Input
            id="tier-model-search"
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={t("gwSearchModel")}
          />
        </Field>
      )}

      {/* The label goes through the checkbox's own field wrapper: a hand-written
          `<label>` around it produces a double label, and a missing accessible name
          warns in development. */}
      <div className="flex max-h-80 flex-col gap-1 overflow-y-auto">
        {display?.allow_all_models !== true &&
          rows.map((m) => (
            <Checkbox
              key={m.id}
              checked={selected.has(m.id)}
              onCheckedChange={() => toggle(m.id)}
              label={
                <span className="flex items-center gap-2 text-base">
                  <span className="font-mono">{m.slug}</span>
                  {m.enabled === false && (
                    <StatusBadge tone="neutral">{t("tierStatusDisabled")}</StatusBadge>
                  )}
                </span>
              }
            />
          ))}
      </div>
    </FormDialog>
  );
}
