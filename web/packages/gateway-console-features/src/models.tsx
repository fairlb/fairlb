import { Badge } from "@cloudflare/kumo/components/badge";
import {
  gatewayConsoleApi,
  type GatewayConsoleTypes,
  ORG_CAPABILITIES,
  apiErrorMessage,
  hasOrgCapability,
} from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  PageHeader,
  Card,
  DataTable,
  Field,
  FormRow,
  Input,
  InlineEmpty,
  LoadingState,
  formatNanoFixed,
} from "@fairlb/ui";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { surfaceOf } from "./playground-surface";
import { OrgNotFound, useCanAccess, useConsoleTitle, useOrg } from "./host";

// The model catalog.
//
// The playground is a route of its own rather than something that expands here: a
// catalog is a **looking-things-up** task and a playground is a **doing-things**
// task. Each row's "try it" carries the model's slug across in `?model=`.

export function ModelsPage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const org = useOrg(orgId);
  const canAccess = useCanAccess();
  useConsoleTitle(org ? t("modelsTitle") : undefined);
  if (!org) return <OrgNotFound />;
  return (
    <ModelsDetail
      key={org.id}
      orgId={org.id}
      canUsePlayground={canAccess(org, "/playground")}
      canReadFinance={hasOrgCapability(org, ORG_CAPABILITIES.financeDetailsRead)}
    />
  );
}

function ModelsDetail({
  orgId,
  canUsePlayground,
  canReadFinance,
}: {
  orgId: string;
  canUsePlayground: boolean;
  canReadFinance: boolean;
}) {
  const { t } = useI18n();
  const models = gatewayConsoleApi.useListAvailableModels(orgId);
  // The search term lives in the URL like every other filter, so a filtered view can
  // be shared and the back button returns to it.
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { q?: string };
  const query = search.q ?? "";

  const data = models.data?.items ?? [];
  const matched = filterModels(data, query);
  // Grouped by creator: the first level of structure in a catalog is whose models
  // these are, not the alphabetical order of their slugs. Nine models read fine as a
  // flat list; two hundred leave nothing but the browser's find-in-page, by which
  // point the table is an unscrollable wall.
  const groups = groupByCreator(matched);

  return (
    <div className="space-y-6">
      <PageHeader title={t("modelsTitle")} description={t("modelsDesc")} />

      {/* 与其余每个筛选面同一种画法（FormRow），不再套 Kumo 的 Toolbar：
          那个根是 `inline-flex w-fit`，按内容收缩，右边缘会停在半路，
          而下面的表格卡是满宽的。 */}
      <FormRow className="sm:grid-cols-[minmax(16rem,24rem)]">
        <FormRow.Item>
          <Field label={t("modelsSearch")} htmlFor="models-q">
            <Input
              id="models-q"
              value={query}
              placeholder={t("modelsSearchHint")}
              autoComplete="off"
              onChange={(event) =>
                void navigate({
                  to: ".",
                  search: (prev: Record<string, unknown>) => ({
                    ...prev,
                    q: event.target.value || undefined,
                  }),
                  replace: true,
                })
              }
            />
          </Field>
        </FormRow.Item>
      </FormRow>

      {/* Errors render in place; the page header does not disappear. */}
      {models.isError ? (
        <Card>
          <Alert>{apiErrorMessage(models.error)}</Alert>
        </Card>
      ) : models.isPending ? (
        <Card>
          <LoadingState label={t("loading")} />
        </Card>
      ) : data.length === 0 ? (
        <Card>
          <InlineEmpty title={t("modelsEmpty")} description={t("modelsEmptyHint")} />
        </Card>
      ) : matched.length === 0 ? (
        // "Nothing matched" and "the catalog is empty" are different facts and get
        // different words. One shared "no models available yet" reads as the gateway
        // having no models, when in fact the search term was just misspelled.
        <Card>
          <InlineEmpty title={t("modelsNoMatch")} description={t("modelsNoMatchHint")} />
        </Card>
      ) : (
        <Card>
          <ModelTable
            groups={groups}
            orgId={orgId}
            canUsePlayground={canUsePlayground}
            canReadFinance={canReadFinance}
          />
        </Card>
      )}

      {canReadFinance && (
        <p className="text-base text-kumo-subtle">
          {t("modelsPriceNote")}{" "}
          <Link to="/orgs/$orgId/usage" params={{ orgId }} className="underline">
            {t("modelsUsagePage")}
          </Link>
        </p>
      )}
    </div>
  );
}

/**
 * **One** table, with protocols separated by group header rows.
 *
 * Not a card per protocol each carrying its own header row: every such table would
 * measure its own column widths, so the price columns of two groups would not line
 * up — putting numbers of the same dimension in two misaligned columns is exactly
 * what a table exists to prevent. One table also drops the repeated header rows.
 * The group header is a full-width `<th scope="colgroup">`, so a screen reader
 * announces it as the title of a group rather than as a row of data.
 */
function ModelTable({
  groups,
  orgId,
  canUsePlayground,
  canReadFinance,
}: {
  groups: [string, GatewayConsoleTypes.AvailableModel[]][];
  orgId: string;
  canUsePlayground: boolean;
  canReadFinance: boolean;
}) {
  const { t } = useI18n();
  const all = groups.flatMap(([, items]) => items);
  // The context column appears only if some model in the table has one. A column of
  // nothing but `—` conveys nothing and merely squeezes the seven columns beside it;
  // as soon as one model has a value, it becomes a dimension worth comparing.
  const showContext = all.some((m) => m.context_window);
  const colCount = (showContext ? 4 : 3) + (canReadFinance ? 4 : 0) + (canUsePlayground ? 1 : 0);
  return (
    <DataTable caption={t("modelsTitle")}>
      <DataTable.Header>
        <DataTable.Row>
          <DataTable.Head>{t("modelsColModel")}</DataTable.Head>
          {/* This column holds endpoints, which are **surfaces**: they answer "which
            URL do I post to", not "can this model see an image". Calling it
            "capabilities" made it answer a question it does not answer — the real
            capabilities are the next column along. */}
          <DataTable.Head>{t("modelsColSurface")}</DataTable.Head>
          <DataTable.Head>{t("modelsColCaps")}</DataTable.Head>
          {showContext && (
            <DataTable.Head className="text-right">{t("modelsColContext")}</DataTable.Head>
          )}
          {canReadFinance && (
            <>
              <DataTable.Head className="text-right">{t("modelsColIn")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("modelsColOut")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("modelsColCacheRead")}</DataTable.Head>
              <DataTable.Head className="text-right">{t("modelsColCacheWrite")}</DataTable.Head>
            </>
          )}
          {canUsePlayground && (
            /* An empty header is an unnamed column to a screen reader. The action
               column having no visible title is deliberate — a title there would
               outshout the actions — so it gets a name only assistive technology
               reads, rather than an empty `<th>`. */
            <DataTable.Head className="text-right">
              <span className="sr-only">{t("modelsColActions")}</span>
            </DataTable.Head>
          )}
        </DataTable.Row>
      </DataTable.Header>
      <DataTable.Body>
        {groups.flatMap(([creator, items]) => [
          <DataTable.Row key={`group-${creator || "other"}`}>
            <th
              scope="colgroup"
              colSpan={colCount}
              className="bg-kumo-recessed px-3 py-2 text-left font-medium"
            >
              {/* The creator segment of the slug, as the slug spells it: it is
                  the same token the caller writes and `owned_by` reports. */}
              {creator || t("modelsGroupOther")}{" "}
              <span className="font-normal text-kumo-subtle">({items.length})</span>
            </th>
          </DataTable.Row>,
          ...items.map((m) => (
            <DataTable.Row key={m.slug}>
              <DataTable.Cell>
                <div className="font-mono text-base">{m.slug}</div>
                {m.display_name && (
                  <div className="text-base text-kumo-subtle">{m.display_name}</div>
                )}
              </DataTable.Cell>
              <DataTable.Cell>
                <span className="flex flex-wrap gap-1">
                  {m.endpoints.map((e) => (
                    <Badge key={e}>{e}</Badge>
                  ))}
                </span>
              </DataTable.Cell>
              <DataTable.Cell>
                <Capabilities caps={m.capabilities} />
              </DataTable.Cell>
              {showContext && (
                <DataTable.Cell className="text-right tabular-nums">
                  {fmtK(m.context_window)}
                </DataTable.Cell>
              )}
              {canReadFinance && (
                <>
                  <Price nano={m.price_in_nano_per_mtok} free={m.is_free} />
                  <Price nano={m.price_out_nano_per_mtok} free={m.is_free} />
                  <Price nano={m.price_cache_read_nano_per_mtok} free={m.is_free} />
                  <Price nano={m.price_cache_write_nano_per_mtok} free={m.is_free} />
                </>
              )}
              {canUsePlayground && (
                <DataTable.Cell className="text-right">
                  {/* "Try it" navigates to the playground carrying the model, rather
                  than expanding a card here and pressing looking-things-up and
                  doing-things into one scrollbar.

                  Whether the link appears is decided by `surfaceOf`, not by a
                  hard-coded `includes("chat")`. The hard-coded version rendered a
                  dead `—` for every model that only speaks the messages surface —
                  six of nine rows with an empty last column, while the data plane
                  had been serving /v1/messages all along. */}
                  {surfaceOf(m) ? (
                    <Link
                      to="/orgs/$orgId/playground"
                      params={{ orgId }}
                      search={{ model: m.slug }}
                      className="text-base underline"
                    >
                      {t("modelsTry")}
                    </Link>
                  ) : (
                    <span className="text-base text-kumo-inactive">—</span>
                  )}
                </DataTable.Cell>
              )}
            </DataTable.Row>
          )),
        ])}
      </DataTable.Body>
    </DataTable>
  );
}

/**
 * One unit price cell.
 *
 * "Unpriced" and "free" are shown differently even though both are 0 in the data,
 * because they mean opposite things: one is a price nobody got round to setting, the
 * other is deliberate. Rendered identically, the first kind can never be found.
 *
 * **Fixed decimals rather than the trimming formatter**: prices are a read-only
 * column, and the trimming formatter strips trailing zeros, which cancels out the
 * right alignment and the tabular figures it sits with — `10` beside `0.075`, ragged
 * down the column. Four decimals is the money display precision used everywhere
 * else, so matching it neither loses more precision nor invents a second rule for
 * unit prices.
 */
function Price({ nano, free }: { nano?: number; free?: boolean }) {
  const { t } = useI18n();
  if (free)
    return (
      <DataTable.Cell className="text-right text-base text-kumo-success">
        {t("modelsFree")}
      </DataTable.Cell>
    );
  if (!nano)
    return (
      <DataTable.Cell className="text-right text-base text-kumo-warning">
        {t("modelsUnpriced")}
      </DataTable.Cell>
    );
  return (
    <DataTable.Cell className="text-right tabular-nums">{formatNanoFixed(nano, 4)}</DataTable.Cell>
  );
}

function fmtK(n?: number): string {
  if (!n) return "—";
  return n >= 1000 ? `${Math.round(n / 1000)}K` : String(n);
}

/**
 * The **actual capabilities** of a model — vision, tools and so on.
 *
 * A different question from the endpoints column: endpoints answer "which URL do I
 * post to", this answers "what can the model do". The data is free-form, editable
 * and served as-is, so **only keys whose value is true are rendered**:
 * `{"vision": false}` states that the model explicitly lacks it, and showing that as
 * a badge would say precisely the opposite.
 *
 * Nothing recorded renders `—` rather than blank, because blank reads as "it did not
 * load".
 */
function Capabilities({ caps }: { caps?: Record<string, unknown> }) {
  const enabled = Object.entries(caps ?? {})
    .filter(([, v]) => v === true)
    .map(([k]) => k)
    .sort();
  if (enabled.length === 0) return <span className="text-base text-kumo-inactive">—</span>;
  return (
    <span className="flex flex-wrap gap-1">
      {enabled.map((c) => (
        <Badge key={c} variant="secondary">
          {c}
        </Badge>
      ))}
    </span>
  );
}

/** Case-insensitive substring match against slug, display name and protocol. */
export function filterModels(
  models: GatewayConsoleTypes.AvailableModel[],
  query: string,
): GatewayConsoleTypes.AvailableModel[] {
  const q = query.trim().toLowerCase();
  if (!q) return models;
  return models.filter((m) =>
    [m.slug, m.display_name ?? "", ...m.protocols].some((s) => s.toLowerCase().includes(q)),
  );
}

/**
 * The creator segment of a `<creator>/<name>` slug, or "" for a bare slug.
 *
 * The catalog's first level of structure is whose models these are. A model
 * owns no protocol -- the same slug may be reachable on several -- so the
 * creator, which the slug convention puts first, is the one grouping that is
 * stable, and it is also what `/v1/models` reports as `owned_by`.
 */
export function creatorOf(slug: string): string {
  const cut = slug.indexOf("/");
  return cut > 0 ? slug.slice(0, cut) : "";
}

/**
 * Groups models by creator, keeping the server's order within each group.
 *
 * Groups are ordered by name rather than by first appearance: the latter would
 * give the same catalog a different group order under a different filter, which
 * reads as though its contents had changed. Bare slugs, which have no creator,
 * sort last under one heading.
 */
export function groupByCreator(
  models: GatewayConsoleTypes.AvailableModel[],
): [string, GatewayConsoleTypes.AvailableModel[]][] {
  const byCreator = new Map<string, GatewayConsoleTypes.AvailableModel[]>();
  for (const m of models) {
    const creator = creatorOf(m.slug);
    const list = byCreator.get(creator);
    if (list) list.push(m);
    else byCreator.set(creator, [m]);
  }
  return [...byCreator.entries()].sort(([a], [b]) => {
    if (a === "") return 1;
    if (b === "") return -1;
    return a.localeCompare(b);
  });
}
