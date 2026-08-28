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
  DataTable,
  Field,
  FormRow,
  InlineEmpty,
  Input,
  ListPage,
  LoadingState,
  PageHeader,
  Select,
  formatNanoFixed,
} from "@fairlb/ui";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { OrgNotFound, useConsoleTitle, useOrg } from "./host";

// The model catalog: a **looking-things-up** page. It lists what this organization
// may call and on which surface; issuing a call is not something it does.

export function ModelsPage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const org = useOrg(orgId);
  useConsoleTitle(org ? t("modelsTitle") : undefined);
  if (!org) return <OrgNotFound />;
  return (
    <ModelsDetail
      key={org.id}
      orgId={org.id}
      canReadFinance={hasOrgCapability(org, ORG_CAPABILITIES.financeDetailsRead)}
    />
  );
}

function ModelsDetail({ orgId, canReadFinance }: { orgId: string; canReadFinance: boolean }) {
  const { t } = useI18n();
  const models = gatewayConsoleApi.useListAvailableModels(orgId);
  // The search term lives in the URL like every other filter, so a filtered view can
  // be shared and the back button returns to it.
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { q?: string; modality?: string };
  const query = search.q ?? "";
  const modality = search.modality ?? "";

  const data = models.data?.items ?? [];
  const matched = filterModels(data, query, modality);
  // Grouped by creator: the first level of structure in a catalog is whose models
  // these are, not the alphabetical order of their slugs. Nine models read fine as a
  // flat list; two hundred leave nothing but the browser's find-in-page, by which
  // point the table is an unscrollable wall.
  const groups = groupByCreator(matched);

  return (
    <ListPage
      header={<PageHeader title={t("modelsTitle")} description={t("modelsDesc")} />}
      filters={
        /* 与其余每个筛选面同一种画法（FormRow），不再套 Kumo 的 Toolbar：
         那个根是 `inline-flex w-fit`，按内容收缩，右边缘会停在半路，
         而这一格所在的卡是满宽的。 */
        <FormRow className="sm:grid-cols-[minmax(16rem,24rem)_minmax(10rem,14rem)]">
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
          <FormRow.Item>
            {/* What a model produces, which is a different question from which
                endpoint it answers on -- a Gemini image model is reached on the
                same endpoint as its text models, so this axis cannot be derived
                from the protocol filter beside it. */}
            <Field label={t("gwModality")} htmlFor="models-modality">
              <Select
                id="models-modality"
                value={modality}
                onValueChange={(v) =>
                  void navigate({
                    to: ".",
                    search: (prev: Record<string, unknown>) => ({
                      ...prev,
                      modality: v || undefined,
                    }),
                    replace: true,
                  })
                }
                items={[
                  { value: "", label: t("gwFilterAllModalities") },
                  { value: "text", label: t("gwModalityText") },
                  { value: "image", label: t("gwModalityImage") },
                  { value: "video", label: t("gwModalityVideo") },
                ]}
              />
            </Field>
          </FormRow.Item>
        </FormRow>
      }
    >
      {/* Errors render in place; the page header does not disappear. */}
      {models.isError ? (
        <Alert>{apiErrorMessage(models.error)}</Alert>
      ) : models.isPending ? (
        <LoadingState label={t("loading")} />
      ) : data.length === 0 ? (
        <InlineEmpty title={t("modelsEmpty")} description={t("modelsEmptyHint")} />
      ) : matched.length === 0 ? (
        // "Nothing matched" and "the catalog is empty" are different facts and get
        // different words. One shared "no models available yet" reads as the gateway
        // having no models, when in fact the search term was just misspelled.
        <InlineEmpty title={t("modelsNoMatch")} description={t("modelsNoMatchHint")} />
      ) : (
        <ModelTable groups={groups} canReadFinance={canReadFinance} />
      )}

      {canReadFinance && (
        <p className="text-base text-kumo-subtle">
          {t("modelsPriceNote")}{" "}
          <Link to="/orgs/$orgId/usage" params={{ orgId }} className="underline">
            {t("modelsUsagePage")}
          </Link>
        </p>
      )}
    </ListPage>
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
  canReadFinance,
}: {
  groups: [string, GatewayConsoleTypes.AvailableModel[]][];
  canReadFinance: boolean;
}) {
  const { t } = useI18n();
  const all = groups.flatMap(([, items]) => items);
  // The context column appears only if some model in the table has one. A column of
  // nothing but `—` conveys nothing and merely squeezes the seven columns beside it;
  // as soon as one model has a value, it becomes a dimension worth comparing.
  const showContext = all.some((m) => m.context_window);
  const colCount = (showContext ? 4 : 3) + (canReadFinance ? 4 : 0);
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
              {canReadFinance &&
                (m.billing_unit && m.billing_unit !== "token" ? (
                  <UnitPrices unit={m.billing_unit} rates={m.unit_rates} />
                ) : (
                  <>
                    <Price nano={m.price_in_nano_per_mtok} free={m.is_free} />
                    <Price nano={m.price_out_nano_per_mtok} free={m.is_free} />
                    <Price nano={m.price_cache_read_nano_per_mtok} free={m.is_free} />
                    <Price nano={m.price_cache_write_nano_per_mtok} free={m.is_free} />
                  </>
                ))}
            </DataTable.Row>
          )),
        ])}
      </DataTable.Body>
    </DataTable>
  );
}

/**
 * The price of a model that is not billed by token, across the four columns the
 * token rates would have used.
 *
 * One cell rather than four, because there is nothing to put in the other three:
 * this model has no input rate, no output rate and no cache rates. Before this
 * existed the four columns rendered the explicit zeros such a model stores and
 * called it **unpriced**, in the warning colour, beside models that really were
 * — a priced model reported as unconfigured, which is the one reading a
 * catalogue must never produce.
 *
 * The rate card is spelled out rather than reduced to a single headline number:
 * a per-second price usually varies by resolution and by whether there is sound,
 * and quoting the cheapest line as if it were the price is how a caller is
 * surprised by the bill.
 */
function UnitPrices({
  unit,
  rates,
}: {
  unit: string;
  rates?: GatewayConsoleTypes.AvailableModelUnitRate[];
}) {
  const { t } = useI18n();
  const lines = rates ?? [];
  return (
    <DataTable.Cell colSpan={4} className="text-right text-base">
      {lines.length === 0 ? (
        <span className="text-kumo-warning">{t("modelsUnpriced")}</span>
      ) : (
        <span className="flex flex-wrap justify-end gap-x-3 gap-y-1">
          {lines.map((r) => (
            <span
              key={`${r.unit}|${r.resolution ?? ""}|${r.audio ?? ""}|${r.variant ?? ""}`}
              className="tabular-nums"
            >
              <span className="text-kumo-subtle">{unitRateLabel(t, r)}</span>{" "}
              {formatNanoFixed(r.nano_per_unit, 4)}
            </span>
          ))}
        </span>
      )}
      <span className="sr-only">{t(UNIT_LABEL_KEY[unit] ?? "modelsPerSecond")}</span>
    </DataTable.Cell>
  );
}

/**
 * How one line of a rate card is introduced: the axes it varies on, and nothing
 * else. An empty axis means the rate does not vary on it, so naming it would
 * invent a distinction the price does not make.
 */
function unitRateLabel(
  t: (key: UnitLabelKey | "modelsAudioOn" | "modelsAudioOff") => string,
  rate: GatewayConsoleTypes.AvailableModelUnitRate,
): string {
  const parts = [t(UNIT_LABEL_KEY[rate.unit] ?? "modelsPerSecond")];
  if (rate.resolution) parts.push(rate.resolution);
  if (rate.audio === "on") parts.push(t("modelsAudioOn"));
  if (rate.audio === "off") parts.push(t("modelsAudioOff"));
  // The quality tier an image rate varies on, where a video rate uses sound.
  // Left out and two rows of one card read identically while charging
  // differently.
  if (rate.variant) parts.push(rate.variant);
  return parts.join(" · ");
}

type UnitLabelKey = "modelsPerSecond" | "modelsPerCall" | "modelsPerImage";

/**
 * How each billing unit is named. A lookup rather than a conditional: there are
 * three of them now, and the two-arm conditional this replaced labelled a
 * per-image rate "per second".
 */
const UNIT_LABEL_KEY: Record<string, UnitLabelKey> = {
  second: "modelsPerSecond",
  call: "modelsPerCall",
  image: "modelsPerImage",
};

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

/**
 * Case-insensitive substring match against slug, display name and protocol,
 * narrowed by output modality.
 *
 * The two are separate axes and both apply: a search for "gemini" under
 * "Image" is a question with an answer, and folding the modality into the
 * substring match would also match a model whose *name* happens to contain the
 * word.
 */
export function filterModels(
  models: GatewayConsoleTypes.AvailableModel[],
  query: string,
  modality = "",
): GatewayConsoleTypes.AvailableModel[] {
  const q = query.trim().toLowerCase();
  return models.filter((m) => {
    if (modality && !m.output_modalities.includes(modality as never)) return false;
    if (!q) return true;
    return [m.slug, m.display_name ?? "", ...m.protocols].some((s) => s.toLowerCase().includes(q));
  });
}

/**
 * The creator segment of a `<creator>/<name>` slug, or "" for a bare slug.
 *
 * The catalog's first level of structure is whose models these are. A model
 * owns no protocol -- the same slug may be reachable on several -- so the
 * creator, which the slug convention puts first, is the one grouping that is
 * stable, and it is also what `/v1/models` reports as `owned_by`.
 *
 * The bare-slug answer is now unreachable through the product: `models_slug_shape`
 * refuses a one-segment slug at the point of writing. It is kept anyway, here and
 * in the sort below, because the alternative is a model reporting *itself* as its
 * creator and heading its own group -- a wrong answer rendered to a customer,
 * where this is a missing one.
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
 * reads as though its contents had changed. Bare slugs, which the catalog can no
 * longer hold, would sort last under one heading -- see creatorOf.
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
