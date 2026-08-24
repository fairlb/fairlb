import { LinkButton } from "@cloudflare/kumo/components/button";
import {
  gatewayConsoleApi,
  type GatewayConsoleTypes,
  ORG_CAPABILITIES,
  apiErrorMessage,
  hasOrgCapability,
} from "@fairlb/api-client";
import { browserTZ, type MessageKey, useI18n } from "@fairlb/i18n";
import {
  PageHeader,
  Alert,
  Button,
  Card,
  DataTable,
  DetailDrawer,
  Field,
  FormRow,
  InlineEmpty,
  Input,
  LoadingState,
  Select,
  StatusBadge as SharedStatusBadge,
  formatNano,
  RANGES,
  pickRange,
  useQuantizedRange,
  useCursorStack,
} from "@fairlb/ui";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import {
  OrgNotFound,
  useConsoleTitle,
  useImpersonating,
  useOrg,
  useApiKeyOptions,
  type ApiKeyOptions,
} from "./host";

// The request log: **one row per request**, complementing the aggregate view of the
// usage page. That page answers "how much did this month cost"; this one answers
// "what happened to the request I just sent".

const STATUSES = [
  { value: "", label: "logsStatusAll" },
  { value: "ok", label: "logsStatusOk" },
  { value: "upstream_error", label: "logsStatusUpstream" },
  { value: "client_error", label: "logsStatusClient" },
  { value: "canceled", label: "logsStatusCanceled" },
] satisfies { value: string; label: MessageKey }[];

export function RequestsPage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const org = useOrg(orgId);
  const impersonating = useImpersonating();
  // Host-driven fetching stays in the **outer** component. `LogsDetail` is the
  // presentation layer exported for tests, and a host hook inside it would force
  // every one of those tests to stand up a full provider first — when all they want
  // is to render a table.
  const apiKeys = useApiKeyOptions(
    org?.id ?? "",
    org !== undefined && hasOrgCapability(org, ORG_CAPABILITIES.keysManage),
  );
  useConsoleTitle(org ? t("logsTitle") : undefined);
  if (!org) return <OrgNotFound />;
  const canReadFinance = hasOrgCapability(org, ORG_CAPABILITIES.financeDetailsRead);
  const canFilterKeys = hasOrgCapability(org, ORG_CAPABILITIES.keysManage);
  return (
    <LogsDetail
      key={org.id}
      orgId={org.id}
      canReadFinance={canReadFinance}
      canFilterKeys={canFilterKeys}
      canExport={canReadFinance && canFilterKeys && !impersonating}
      apiKeys={apiKeys}
    />
  );
}

/**
 * The body of the page. **Exported for tests only** — `RequestsPage` above is the entry
 * point, and it reads the organization id from the route. This identifier appearing
 * in production code is by definition a misuse.
 */
export { LogsDetail as RequestsDetailForTest };

function LogsDetail({
  orgId,
  canReadFinance,
  canFilterKeys,
  canExport,
  apiKeys,
}: {
  orgId: string;
  canReadFinance: boolean;
  canFilterKeys: boolean;
  canExport: boolean;
  apiKeys?: ApiKeyOptions;
}) {
  const { t } = useI18n();
  const navigate = useNavigate();
  // The URL is the source of truth for the filters: a filtered view can be shared,
  // bookmarked, and stepped back to.
  const search = useSearch({ strict: false }) as {
    range?: string;
    status?: string;
    key?: string;
    model?: string;
    user?: string;
    request?: string;
  };
  const rangeKey = search.range ?? "24h";
  const status = search.status ?? "";
  const apiKeyId = canFilterKeys ? (search.key ?? "") : "";
  const openId = search.request ?? null;
  // 换页而不是累积：这一页的任务是「翻到我要找的那一条」，不是「把三万行装进浏览器」。
  // 两种分页各自住在 @fairlb/ui 里、各自命名（ADR-0196）——它们的失败方式不同，
  // 前者怕重复行，后者怕丢了回上一页的出口。
  const pages = useCursorStack();

  // Changing a filter returns to the first page: a cursor from page five belongs to
  // the previous result set and would page through unrelated rows. Replace rather
  // than push, because one history entry per filter tweak makes "back" take a dozen
  // presses to get anywhere.
  const setFilter = (patch: Record<string, string | undefined>) => {
    pages.reset();
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, ...patch }),
      replace: true,
    });
  };

  // Free-text filters are held locally and debounced into the URL; written straight
  // through, every keystroke fired its own query.
  const [modelDraft, setModelDraft] = useState(search.model ?? "");
  const [endUserDraft, setEndUserDraft] = useState(search.user ?? "");
  // The URL remains the source of truth: on back, forward, or opening a shared link
  // the drafts have to follow, or the debounce below writes the stale local value
  // straight back into the URL and the back button stops working.
  useEffect(() => setModelDraft(search.model ?? ""), [search.model]);
  useEffect(() => setEndUserDraft(search.user ?? ""), [search.user]);
  // Without the right to manage keys, a key filter arriving in a shared link neither
  // takes effect nor should keep sitting in the address bar pretending it did. The
  // query above already went out with an empty key, and this replaces the URL to
  // drop it while leaving the other filters and the detail deep link intact.
  useEffect(() => {
    if (canFilterKeys || search.key === undefined) return;
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, key: undefined }),
      replace: true,
    });
  }, [canFilterKeys, navigate, search.key]);
  useEffect(() => {
    const id = setTimeout(() => {
      if ((search.model ?? "") !== modelDraft) setFilter({ model: modelDraft || undefined });
    }, 300);
    return () => clearTimeout(id);
    // setFilter is a new function on every render; listing it would rebuild the
    // timer forever.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelDraft, search.model]);
  useEffect(() => {
    const id = setTimeout(() => {
      if ((search.user ?? "") !== endUserDraft) setFilter({ user: endUserDraft || undefined });
    }, 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endUserDraft, search.user]);

  const model = search.model ?? "";
  const endUser = search.user ?? "";
  // The time range is deliberately excluded: it always has a value, so counting it
  // would show this hint permanently, which is the same as having no hint.
  const activeFilterCount = [status, apiKeyId, model, endUser].filter(Boolean).length;

  // Opening a detail goes through the URL rather than state: a reload stays where it
  // was, and the link can be sent to a colleague. Opening pushes, so back closes the
  // drawer as one expects; closing replaces, so history keeps no empty entry.
  const setOpen = (id: string | null) =>
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, request: id ?? undefined }),
      replace: id === null,
    });

  const range = pickRange(rangeKey, "24h");
  // Recomputed only when the selected range changes. With the filters in the
  // dependency list too, every filter tweak shifted the time window, so consecutive
  // results were not comparable — and refetching is already triggered by the filter
  // parameters in the query key. Quantized to the hour, because a millisecond
  // timestamp in that key means the cache is never hit.
  const { from, to } = useQuantizedRange(range.hours);

  const filters = {
    from,
    to,
    ...(model ? { model } : {}),
    ...(status ? { status: status as GatewayConsoleTypes.ListRequestLogsStatus } : {}),
    ...(endUser ? { end_user_id: endUser } : {}),
    ...(apiKeyId ? { api_key_id: apiKeyId } : {}),
  };
  const logs = gatewayConsoleApi.useListRequestLogs(orgId, {
    ...filters,
    ...(pages.cursor ? { cursor: pages.cursor } : {}),
  });
  const hasRows = (logs.data?.items.length ?? 0) > 0;
  // Tests render this without `apiKeys` — they are after the table, not the filter
  // bar — so it degrades to an empty, settled state rather than throwing.
  const keys = apiKeys ?? { isPending: false, isError: false, error: null, items: [] };
  return (
    <div className="space-y-6">
      <PageHeader title={t("logsTitle")} description={t("logsDesc")} />

      {/* 筛选行直接是 FormRow，不再套一层 Toolbar：Kumo 的 Toolbar 根是
          `inline-flex w-fit`，按内容收缩，于是这一块的右边缘停在半路，而下面的
          表格卡是满宽的——同一列里两个块对不齐，窗口越宽差得越多。 */}
      <FormRow
        className={
          canFilterKeys
            ? "w-full sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-[10rem_10rem_11rem_minmax(10rem,1fr)_minmax(10rem,1fr)_auto]"
            : "w-full sm:grid-cols-2 lg:grid-cols-4"
        }
      >
        <FormRow.Item>
          <Field label={t("commonTimeRange")}>
            <Select
              value={rangeKey}
              onValueChange={(v) => setFilter({ range: v ?? undefined })}
              items={RANGES.map((r) => ({ value: r.key as string, label: t(r.label) }))}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field label={t("logsStatus")}>
            <Select
              value={status}
              onValueChange={(v) => setFilter({ status: v || undefined })}
              items={STATUSES.map((x) => ({ value: x.value, label: t(x.label) }))}
            />
          </Field>
        </FormRow.Item>
        {canFilterKeys && (
          <FormRow.Item>
            <Field label={t("playApiKey")}>
              <Select
                value={apiKeyId}
                disabled={keys.isPending}
                onValueChange={(v) => setFilter({ key: v || undefined })}
                items={[
                  { value: "", label: t("logsAllKeys") },
                  ...keys.items.map((k) => ({ value: k.id, label: k.name })),
                ]}
              />
            </Field>
          </FormRow.Item>
        )}
        <FormRow.Item>
          <Field label={t("logsModel")} htmlFor="model">
            <Input
              id="model"
              value={modelDraft}
              placeholder={t("logsModelPlaceholder")}
              onChange={(e) => setModelDraft(e.target.value)}
            />
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field label={t("logsEndUser")} htmlFor="enduser">
            <Input
              id="enduser"
              value={endUserDraft}
              placeholder={t("logsEndUserPlaceholder")}
              onChange={(e) => setEndUserDraft(e.target.value)}
            />
          </Field>
        </FormRow.Item>
        {canExport && (
          <FormRow.Actions>
            {/* Export is a link, not a button, because it is a browser download —
            but it should stand the same height as the controls beside it, which
            is exactly what a link-styled button is for. */}
            <LinkButton
              href={gatewayConsoleApi.getExportLogsCSVUrl(orgId, filters)}
              variant="outline"
            >
              {t("commonExportCsv")}
            </LinkButton>
          </FormRow.Actions>
        )}
      </FormRow>

      {/* Active filters must be visible at a glance and clearable in one press. With
          five controls spread across a row, "why does this find nothing" is usually
          a filter still set and forgotten about. The time range does not count
          towards it: it always has a value, so counting it would pin this line on
          screen forever. */}
      {activeFilterCount > 0 && (
        <div className="flex items-center gap-2 text-base text-kumo-subtle">
          <span>{t("logsFiltersActive", { count: activeFilterCount })}</span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setModelDraft("");
              setEndUserDraft("");
              setFilter({ status: undefined, key: undefined, model: undefined, user: undefined });
            }}
          >
            {t("logsClearFilters")}
          </Button>
        </div>
      )}

      {/* 加载中不算空：那会让分页与脚注在每次翻页时闪一下。 */}
      {logs.isError && <Alert>{apiErrorMessage(logs.error)}</Alert>}
      {canFilterKeys && keys.isError && <Alert>{apiErrorMessage(keys.error)}</Alert>}

      <Card>
        <LogTable
          items={logs.data?.items ?? []}
          loading={logs.isPending}
          filtered={activeFilterCount > 0}
          showSpend={canReadFinance}
        />
      </Card>

      {/* 空集时整行收起。两个按钮都 disabled、读数是「第 1 页 · 显示 0 条 · 已到末尾」，
          在一页都没有的时候，这一行没有回答任何问题。只要还在某一页上（不在首页）
          就保留，否则读者会失去回上一页的出口。 */}
      {(hasRows || !pages.atFirst) && (
        <div className="flex items-center gap-2">
          <Button variant="outline" disabled={pages.atFirst} onClick={pages.prev}>
            {t("logsPrevPage")}
          </Button>
          <Button
            variant="outline"
            disabled={!logs.data?.next_cursor}
            onClick={() => pages.next(logs.data!.next_cursor!)}
          >
            {t("logsNextPage")}
          </Button>
          {/* "Page N" alone does not answer "how big is the set I am in". Cursor
            pagination has no cheap total — the contract returns items and a next
            cursor, which is right, since counting means a second scan — but this can
            at least say how many rows are on this page and whether it is the last.
            **Do not add a count query just to produce a total**: the log table is a
            large partitioned one, and that number is both expensive and stale on
            arrival. */}
          <span className="text-base text-kumo-subtle">
            {t(logs.data?.next_cursor ? "logsPageN" : "logsPageLast", {
              page: pages.page,
              count: logs.data?.items.length ?? 0,
            })}
          </span>
        </div>
      )}

      {/* The time zone has to be stated. The commonest use of a timestamp on this
          page is lining it up against a server log, and "10:00:03 PM" without a zone
          cannot be lined up against anything.

          With no rows it says nothing: the note explains how a timestamp is
          rendered, and there is no timestamp on screen to explain. */}
      {hasRows && (
        <p className="text-base text-kumo-subtle">
          {t("usageTimezoneNote", { tz: browserTZ() || "UTC" })}
        </p>
      )}

      {/* Mounted unconditionally and driven by its open state: conditional rendering
          takes the open and close animations away with it. */}
      <LogDrawer
        orgId={orgId}
        requestId={openId}
        showSpend={canReadFinance}
        onClose={() => setOpen(null)}
      />
    </div>
  );
}

// No `onOpen` callback: the detail affordance is a `<Link>` (see the comment further
// down), so opening the drawer is carried by the URL and the table need not hold a
// callback at all.
function LogTable({
  items,
  loading,
  filtered,
  showSpend,
}: {
  items: GatewayConsoleTypes.RequestLog[];
  loading: boolean;
  /** 是否设了时间范围之外的筛选——决定空态说哪一种空。 */
  filtered: boolean;
  showSpend: boolean;
}) {
  // The time column formats **with seconds**. A short time style stops at the
  // minute, so requests within the same minute can be neither told apart nor put in
  // order — while the whole point of this page is "what happened to the request I
  // just sent".
  const { formatTimestamp, formatNumber, t } = useI18n();
  if (loading) return <LoadingState label={t("loading")} />;
  if (items.length === 0)
    // 两种空是两回事。「没匹配上」才谈得上放宽或清掉筛选；一条筛选都没设时说
    // 「清掉筛选条件」，是让人去清一个不存在的东西——而时间范围按设计不计入
    // `filtered`（它永远有值，计入的话这一页就永远算「设了筛选」）。
    return filtered ? (
      <InlineEmpty title={t("logsEmpty")} description={t("logsEmptyHint")} />
    ) : (
      <InlineEmpty title={t("logsNothingYet")} description={t("logsNothingYetHint")} />
    );
  return (
    <DataTable caption={t("logsTitle")}>
      <DataTable.Header>
        <DataTable.Row>
          <DataTable.Head>{t("usageColTime")}</DataTable.Head>
          {/* A row's identity belongs in the table. Kept only inside the drawer,
              there was no way to tell one row from another — and the first thing
              anyone quotes when comparing notes with a server log is the request
              id. */}
          <DataTable.Head>{t("logDetailRequestId")}</DataTable.Head>
          <DataTable.Head>{t("logsModel")}</DataTable.Head>
          <DataTable.Head>{t("logsStatus")}</DataTable.Head>
          <DataTable.Head className="text-right">{t("usageTokens")}</DataTable.Head>
          {showSpend && <DataTable.Head className="text-right">{t("logsColSpend")}</DataTable.Head>}
          <DataTable.Head className="text-right">{t("logsColLatency")}</DataTable.Head>
          <DataTable.Head />
        </DataTable.Row>
      </DataTable.Header>
      <DataTable.Body>
        {items.map((l) => (
          <DataTable.Row key={l.request_id + l.created_at}>
            <DataTable.Cell className="whitespace-nowrap tabular-nums">
              {formatTimestamp(l.created_at)}
            </DataTable.Cell>
            {/* Only the tail is shown; the full id is on the title attribute and is
                copyable in the drawer. All 36 characters would push this table into
                horizontal scrolling, and the tail is what the eye compares
                anyway. */}
            <DataTable.Cell className="font-mono text-kumo-subtle" title={l.request_id}>
              …{l.request_id.slice(-8)}
            </DataTable.Cell>
            <DataTable.Cell className="font-mono">{l.model_slug}</DataTable.Cell>
            <DataTable.Cell>
              <LogStatusBadge status={l.status} httpStatus={l.http_status} code={l.error_code} />
            </DataTable.Cell>
            <DataTable.Cell className="text-right tabular-nums">
              {formatNumber((l.tokens_in ?? 0) + (l.tokens_out ?? 0))}
            </DataTable.Cell>
            {showSpend && (
              <DataTable.Cell className="text-right tabular-nums">
                {formatNano(l.charged_nano)}
              </DataTable.Cell>
            )}
            <DataTable.Cell className="text-right tabular-nums">
              {l.duration_ms ?? 0} ms
            </DataTable.Cell>
            {/* The drawer's open state already lives in the URL, which makes this a
                **destination** rather than an action. As a button, middle-click
                would open nothing and right-click would copy no link — and "send
                this request to a colleague" is one of the commonest things done on
                this page. A link renders a real `<a href>` while left-click still
                routes on the client, with no full page load. */}
            <DataTable.Cell className="text-right">
              <Link
                to="."
                search={(prev: Record<string, unknown>) => ({ ...prev, request: l.request_id })}
                aria-label={`${t("logsDetailLink")} ${l.request_id}`}
                className="text-base text-kumo-info hover:underline"
              >
                {t("logsDetailLink")}
              </Link>
            </DataTable.Cell>
          </DataTable.Row>
        ))}
      </DataTable.Body>
    </DataTable>
  );
}

/**
 * The status badge encodes the outcome twice: in colour and in words.
 *
 * Colour alone separates success from failure for nobody with a colour vision
 * deficiency, so the text itself has to carry the result.
 */
function LogStatusBadge({
  status,
  httpStatus,
  code,
}: {
  status: string;
  httpStatus: number;
  code?: string;
}) {
  const { t } = useI18n();
  const ok = status === "ok";
  const tone = ok ? "success" : status === "canceled" ? "neutral" : "danger";
  return (
    <SharedStatusBadge tone={tone}>
      {ok ? t("logsOkStatus", { status: httpStatus }) : code || status}
    </SharedStatusBadge>
  );
}

/**
 * The request detail side panel.
 *
 * Layering, the pinned title, body scrolling, the close button and focus restoration
 * all come from the shared detail drawer. Assembled directly from the underlying
 * primitive it is easy to omit the required viewport element, which silently
 * disables swipe-to-close and the touch scroll lock on mobile; and a second
 * hand-built drawer would keep drifting away from the first in padding, width and
 * close behaviour.
 *
 * A null `requestId` means closed: **the component stays mounted**, because
 * conditional rendering takes the closing animation with it.
 */
function LogDrawer({
  orgId,
  requestId,
  showSpend,
  onClose,
}: {
  orgId: string;
  requestId: string | null;
  showSpend: boolean;
  onClose: () => void;
}) {
  const { t } = useI18n();
  return (
    <DetailDrawer
      open={requestId !== null}
      onOpenChange={(next) => {
        if (!next) onClose();
      }}
      title={t("logDetailTitle")}
      closeLabel={t("commonClose")}
    >
      {/* Content and fetching are decoupled: no request goes out while `requestId`
          is null, yet the shell stays mounted so the closing animation survives. */}
      {requestId && <LogDrawerBody orgId={orgId} requestId={requestId} showSpend={showSpend} />}
    </DetailDrawer>
  );
}

function LogDrawerBody({
  orgId,
  requestId,
  showSpend,
}: {
  orgId: string;
  requestId: string;
  showSpend: boolean;
}) {
  const { formatDateTime, formatNumber, t } = useI18n();
  const q = gatewayConsoleApi.useGetRequestLog(orgId, requestId);
  const d = q.data;
  return (
    <>
      {q.isError && <Alert>{apiErrorMessage(q.error)}</Alert>}
      {q.isPending && <LoadingState label={t("loading")} />}
      {d && (
        <div className="space-y-3 text-base">
          <dl className="space-y-2">
            <Row
              k={t("logDetailRequestId")}
              v={<code className="text-base">{d.request_id}</code>}
            />
            <Row k={t("logDetailTime")} v={formatDateTime(d.created_at)} />
            <Row k={t("logsModel")} v={d.model_slug} />
            <Row k={t("logDetailSurface")} v={d.surface} />
            <Row k={t("logDetailProvider")} v={d.provider_slug || "—"} />
            <Row
              k={t("logsStatus")}
              v={
                <LogStatusBadge status={d.status} httpStatus={d.http_status} code={d.error_code} />
              }
            />
            {/* The chain of routing attempts: more than one means a failover
                happened, which is the leading explanation for "why was it slow". */}
            <Row
              k={t("logDetailAttempts")}
              v={
                (d.route_attempts ?? 1) > 1
                  ? t("logDetailFailover", { count: d.route_attempts ?? 0 })
                  : t("logDetailDirect")
              }
            />
            <Row k={t("logDetailStream")} v={d.stream ? t("commonYes") : t("commonNo")} />
            <Row k={t("logsColLatency")} v={`${d.duration_ms ?? 0} ms`} />
            {d.stream && <Row k={t("logDetailTtft")} v={`${d.ttft_ms ?? 0} ms`} />}
          </dl>
          <hr className="border-kumo-line" />
          {/* The four token buckets are mutually exclusive, so they are listed
              separately — summed together, a cache hit becomes invisible. */}
          <dl className="space-y-2">
            <Row k={t("logDetailIn")} v={formatNumber(d.tokens_in ?? 0)} />
            <Row k={t("logDetailOut")} v={formatNumber(d.tokens_out ?? 0)} />
            <Row k={t("logDetailCacheRead")} v={formatNumber(d.tokens_cached_read ?? 0)} />
            <Row k={t("logDetailCacheWrite")} v={formatNumber(d.tokens_cache_write ?? 0)} />
            {(d.tokens_reasoning ?? 0) > 0 && (
              <Row k={t("logDetailReasoning")} v={formatNumber(d.tokens_reasoning ?? 0)} />
            )}
          </dl>
          <hr className="border-kumo-line" />
          {(showSpend || d.end_user_id) && (
            <dl className="space-y-2">
              {showSpend && (
                <Row
                  k={t("logsColSpend")}
                  v={`${formatNano(d.charged_nano)} ${d.charged_currency ?? "USD"}`}
                />
              )}
              {d.end_user_id && <Row k={t("logsEndUser")} v={d.end_user_id} />}
            </dl>
          )}
          {d.usage_estimated && (
            <Alert>{t(showSpend ? "logDetailEstimated" : "logDetailUsageEstimated")}</Alert>
          )}
        </div>
      )}
    </>
  );
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex justify-between gap-4">
      <dt className="shrink-0 text-kumo-subtle">{k}</dt>
      <dd className="text-right break-all">{v}</dd>
    </div>
  );
}
