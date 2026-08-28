import { LinkButton } from "@cloudflare/kumo/components/button";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import {
  apiErrorMessage,
  gatewayConsoleApi,
  hasOrgCapability,
  ORG_CAPABILITIES,
  type GatewayConsoleTypes,
} from "@fairlb/api-client";
import { browserTZ, useI18n, type MessageKey } from "@fairlb/i18n";
import {
  Alert,
  Button,
  ConfirmDialog,
  DataTable,
  Field,
  FormRow,
  InlineEmpty,
  Input,
  ListPage,
  LoadingState,
  PageHeader,
  Select,
  StatusBadge,
  formatMoney,
  useCursorStack,
} from "@fairlb/ui";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { OrgNotFound, useConsoleTitle, useOrg } from "./host";

// Video jobs: one row per submission.
//
// A video is not a request. It is submitted, runs for a minute or two, and the
// film is fetched afterwards — so it does not belong on the request log, whose
// question is "what happened to the call I just made", and it is not answered by
// the usage page, whose question is "how much did this month cost". The one this
// page answers is "where is the clip I asked for, and what did it cost me".
//
// **Failed jobs are listed, not hidden.** A content refusal is the ordinary
// outcome on this plane rather than an edge case, and a failure that leaves no
// trace makes "why did my video fail" unanswerable — while the charge for it is
// zero either way, which is itself worth being able to see.

const STATUSES = [
  { value: "", label: "videosStatusAll" },
  { value: "queued", label: "videosStatusQueued" },
  { value: "in_progress", label: "videosStatusInProgress" },
  { value: "completed", label: "videosStatusCompleted" },
  { value: "failed", label: "videosStatusFailed" },
  { value: "canceled", label: "videosStatusCanceled" },
  { value: "expired", label: "videosStatusExpired" },
] satisfies { value: string; label: MessageKey }[];

const STATUS_LABELS: Record<GatewayConsoleTypes.VideoJobStatus, MessageKey> = {
  queued: "videosStatusQueued",
  in_progress: "videosStatusInProgress",
  completed: "videosStatusCompleted",
  failed: "videosStatusFailed",
  canceled: "videosStatusCanceled",
  expired: "videosStatusExpired",
};

/** Which jobs are still moving. Also decides whether the page polls at all. */
function isRunning(status: string): boolean {
  return status === "queued" || status === "in_progress";
}

/**
 * How often the list is re-read while something is still running.
 *
 * Only while: a page of finished jobs is a static document, and polling one is
 * spending a request per interval to be told nothing. A video takes minutes, so
 * this is slow on purpose — the progress bar it feeds moves in percent, not in
 * frames.
 */
const RUNNING_POLL_MS = 10_000;

export function VideosPage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const org = useOrg(orgId);
  useConsoleTitle(org ? t("videosTitle") : undefined);
  if (!org) return <OrgNotFound />;
  return (
    <VideosDetail
      key={org.id}
      orgId={org.id}
      canReadFinance={hasOrgCapability(org, ORG_CAPABILITIES.financeDetailsRead)}
    />
  );
}

/**
 * The body of the page. **Exported for tests only** — `VideosPage` above is the
 * entry point and reads the organization id from the route.
 */
export { VideosDetail as VideosDetailForTest };

function VideosDetail({ orgId, canReadFinance }: { orgId: string; canReadFinance: boolean }) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const toasts = useKumoToastManager();
  const search = useSearch({ strict: false }) as { status?: string; model?: string };
  const status = search.status ?? "";
  const pages = useCursorStack();

  const setFilter = (patch: Record<string, string | undefined>) => {
    pages.reset();
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, ...patch }),
      replace: true,
    });
  };

  // Free text is held locally and debounced into the URL, the same way the
  // request log does it: written straight through, every keystroke is a query.
  const [modelDraft, setModelDraft] = useState(search.model ?? "");
  useEffect(() => setModelDraft(search.model ?? ""), [search.model]);
  useEffect(() => {
    const id = setTimeout(() => {
      if ((search.model ?? "") !== modelDraft) setFilter({ model: modelDraft || undefined });
    }, 300);
    return () => clearTimeout(id);
    // setFilter is new on every render; listing it would rebuild the timer forever.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelDraft, search.model]);

  const model = search.model ?? "";
  const jobs = gatewayConsoleApi.useListVideoJobs(
    orgId,
    {
      ...(status ? { status: status as GatewayConsoleTypes.ListVideoJobsStatus } : {}),
      ...(model ? { model } : {}),
      ...(pages.cursor ? { cursor: pages.cursor } : {}),
    },
    {
      query: {
        refetchInterval: (query) =>
          (query.state.data?.items ?? []).some((j) => isRunning(j.status))
            ? RUNNING_POLL_MS
            : false,
      },
    },
  );

  const cancel = gatewayConsoleApi.useCancelVideoJob();
  const [cancelling, setCancelling] = useState<GatewayConsoleTypes.VideoJob | null>(null);
  const remove = gatewayConsoleApi.useDeleteVideoJob();
  const [deleting, setDeleting] = useState<GatewayConsoleTypes.VideoJob | null>(null);
  const items = jobs.data?.items ?? [];
  const hasRows = items.length > 0;
  const activeFilterCount = [status, model].filter(Boolean).length;

  return (
    <ListPage
      header={<PageHeader title={t("videosTitle")} description={t("videosDesc")} />}
      filters={
        <FormRow className="w-full sm:grid-cols-2 lg:grid-cols-3">
          <FormRow.Item>
            <Field label={t("videosStatus")}>
              <Select
                value={status}
                onValueChange={(v) => setFilter({ status: v || undefined })}
                items={STATUSES.map((x) => ({ value: x.value, label: t(x.label) }))}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            <Field label={t("logsModel")} htmlFor="video-model">
              <Input
                id="video-model"
                value={modelDraft}
                placeholder={t("logsModelPlaceholder")}
                onChange={(e) => setModelDraft(e.target.value)}
              />
            </Field>
          </FormRow.Item>
        </FormRow>
      }
      overlays={
        <>
          <ConfirmDialog
            open={cancelling !== null}
            onOpenChange={(open) => !open && setCancelling(null)}
            destructive
            title={t("videosCancelConfirmTitle")}
            /* Says outright that nothing is charged. It is the commitment this
             button exists to make good on, and a confirmation that left it to
             be inferred would be asking the reader to take the risk. */
            description={t("videosCancelConfirmBody")}
            confirmLabel={t("videosCancel")}
            pending={cancel.isPending}
            onConfirm={() =>
              cancelling &&
              cancel.mutate(
                { orgId, videoId: cancelling.id },
                {
                  onSuccess: () => {
                    setCancelling(null);
                    toasts.add({ variant: "success", title: t("videosCancelled") });
                    void jobs.refetch();
                  },
                  onError: (error) => {
                    setCancelling(null);
                    toasts.add({
                      variant: "error",
                      title: t("videosCancelFailed"),
                      description: apiErrorMessage(error),
                    });
                  },
                },
              )
            }
          />
          <ConfirmDialog
            open={deleting !== null}
            onOpenChange={(open) => !open && setDeleting(null)}
            destructive
            title={t("videosDeleteConfirmTitle")}
            /* Says what survives, not only what goes. Deleting the job removes the
             film and this record; the ledger entry that charged for it lives in
             the usage log and is untouched. A reader who assumed otherwise would
             be deleting rows to fix a bill. */
            description={t("videosDeleteConfirmBody")}
            confirmLabel={t("videosDelete")}
            pending={remove.isPending}
            onConfirm={() =>
              deleting &&
              remove.mutate(
                { orgId, videoId: deleting.id },
                {
                  onSuccess: () => {
                    setDeleting(null);
                    toasts.add({ variant: "success", title: t("videosDeleted") });
                    void jobs.refetch();
                  },
                  onError: (error) => {
                    setDeleting(null);
                    toasts.add({
                      variant: "error",
                      title: t("videosDeleteFailed"),
                      description: apiErrorMessage(error),
                    });
                  },
                },
              )
            }
          />
        </>
      }
    >
      {activeFilterCount > 0 && (
        <div className="flex items-center gap-2 text-base text-kumo-subtle">
          <span>{t("logsFiltersActive", { count: activeFilterCount })}</span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setModelDraft("");
              setFilter({ status: undefined, model: undefined });
            }}
          >
            {t("logsClearFilters")}
          </Button>
        </div>
      )}

      {jobs.isError && <Alert>{apiErrorMessage(jobs.error)}</Alert>}

      <VideoJobTable
        orgId={orgId}
        items={items}
        loading={jobs.isPending}
        filtered={activeFilterCount > 0}
        showSpend={canReadFinance}
        onCancel={setCancelling}
        onDelete={setDeleting}
      />

      {(hasRows || !pages.atFirst) && (
        <div className="flex items-center gap-2">
          <Button variant="outline" disabled={pages.atFirst} onClick={pages.prev}>
            {t("logsPrevPage")}
          </Button>
          <Button
            variant="outline"
            disabled={!jobs.data?.next_cursor}
            onClick={() => pages.next(jobs.data!.next_cursor!)}
          >
            {t("logsNextPage")}
          </Button>
          <span className="text-base text-kumo-subtle">
            {t(jobs.data?.next_cursor ? "logsPageN" : "logsPageLast", {
              page: pages.page,
              count: items.length,
            })}
          </span>
        </div>
      )}

      {hasRows && (
        <p className="text-base text-kumo-subtle">
          {t("usageTimezoneNote", { tz: browserTZ() || "UTC" })}
        </p>
      )}
    </ListPage>
  );
}

function VideoJobTable({
  orgId,
  items,
  loading,
  filtered,
  showSpend,
  onCancel,
  onDelete,
}: {
  orgId: string;
  items: GatewayConsoleTypes.VideoJob[];
  loading: boolean;
  filtered: boolean;
  showSpend: boolean;
  onCancel: (job: GatewayConsoleTypes.VideoJob) => void;
  onDelete: (job: GatewayConsoleTypes.VideoJob) => void;
}) {
  const { formatTimestamp, formatNumber, t } = useI18n();
  if (loading) return <LoadingState label={t("loading")} />;
  if (items.length === 0)
    return filtered ? (
      <InlineEmpty title={t("videosEmpty")} description={t("videosEmptyHint")} />
    ) : (
      <InlineEmpty title={t("videosNothingYet")} description={t("videosNothingYetHint")} />
    );
  return (
    <DataTable caption={t("videosTitle")}>
      <DataTable.Header>
        <DataTable.Row>
          <DataTable.Head>{t("usageColTime")}</DataTable.Head>
          <DataTable.Head>{t("logsModel")}</DataTable.Head>
          <DataTable.Head>{t("videosColPrompt")}</DataTable.Head>
          <DataTable.Head>{t("videosStatus")}</DataTable.Head>
          <DataTable.Head className="text-right">{t("videosColBilled")}</DataTable.Head>
          {showSpend && <DataTable.Head className="text-right">{t("logsColSpend")}</DataTable.Head>}
          <DataTable.Head />
        </DataTable.Row>
      </DataTable.Header>
      <DataTable.Body>
        {items.map((job) => (
          <DataTable.Row key={job.id}>
            <DataTable.Cell className="whitespace-nowrap">
              {formatTimestamp(job.created_at)}
            </DataTable.Cell>
            <DataTable.Cell className="font-mono">
              <div>{job.model}</div>
              <div className="text-base text-kumo-subtle">{shape(job)}</div>
            </DataTable.Cell>
            {/* The prompt, truncated by the cell rather than by us: cutting it
                here would decide a width the layout has not chosen yet, and the
                full text is what tells two otherwise identical jobs apart. */}
            <DataTable.Cell className="max-w-80 truncate" title={job.prompt}>
              {job.prompt}
            </DataTable.Cell>
            <DataTable.Cell>
              <JobStatus job={job} />
            </DataTable.Cell>
            <DataTable.Cell className="text-right tabular-nums">
              {job.billed_units && job.billed_unit
                ? t(job.billed_unit === "call" ? "videosBilledCalls" : "videosBilledSeconds", {
                    count: formatNumber(job.billed_units),
                  })
                : "—"}
            </DataTable.Cell>
            {showSpend && (
              <DataTable.Cell className="text-right tabular-nums">
                {/* Always with its currency, and zero is a real answer here
                    rather than a missing one: a failed or cancelled job is not
                    charged, and showing that plainly is the point. */}
                {job.charged_nano === undefined
                  ? "—"
                  : formatMoney(job.charged_nano, job.charged_currency ?? "USD")}
              </DataTable.Cell>
            )}
            <DataTable.Cell className="text-right">
              <JobActions orgId={orgId} job={job} onCancel={onCancel} onDelete={onDelete} />
            </DataTable.Cell>
          </DataTable.Row>
        ))}
      </DataTable.Body>
    </DataTable>
  );
}

/** The clip's shape, as one line: what was asked for, not what came back. */
function shape(job: GatewayConsoleTypes.VideoJob): string {
  return [
    job.duration_seconds ? `${job.duration_seconds}s` : "",
    job.resolution ?? "",
    job.aspect_ratio ?? "",
  ]
    .filter(Boolean)
    .join(" · ");
}

/**
 * The status, and for a failure the upstream's own words.
 *
 * The message is shown rather than summarised. A content refusal is the common
 * failure here, its reason is the only thing that tells the customer what to
 * change, and a badge saying "failed" with the reason hidden behind a support
 * ticket is the whole problem this column exists to avoid.
 */
function JobStatus({ job }: { job: GatewayConsoleTypes.VideoJob }) {
  const { t } = useI18n();
  const tone =
    job.status === "completed"
      ? "success"
      : job.status === "failed" || job.status === "expired"
        ? "danger"
        : job.status === "canceled"
          ? "neutral"
          : "warning";
  return (
    <div className="space-y-1">
      <StatusBadge tone={tone}>
        {t(STATUS_LABELS[job.status])}
        {job.status === "in_progress" && job.progress ? ` ${job.progress}%` : ""}
      </StatusBadge>
      {job.error && (
        <p className="max-w-80 text-base text-kumo-subtle">{job.error.message || job.error.code}</p>
      )}
    </div>
  );
}

/**
 * What can still be done to a job.
 *
 * Cancel is offered only where the model says it can be stopped, and never as a
 * button that would refuse: the vendors range from a real cancel, through
 * cancel-while-queued-only, to none at all, and a control that fails two thirds
 * of the time teaches people not to trust the other third.
 *
 * The download is a plain link because it is a browser download of a file that
 * can be hundreds of megabytes; routing it through fetch would buffer the whole
 * clip in the tab to hand it straight back to the browser.
 *
 * It carries `download` because the endpoint answers `video/mp4` with no
 * `Content-Disposition`, and a bare same-tab link to an inline media type makes
 * the browser navigate away and play the film — replacing the page the reader
 * was on, under a button labelled "Download". Same-origin, so the attribute is
 * honoured and its value becomes the suggested filename.
 */
function JobActions({
  orgId,
  job,
  onCancel,
  onDelete,
}: {
  orgId: string;
  job: GatewayConsoleTypes.VideoJob;
  onCancel: (job: GatewayConsoleTypes.VideoJob) => void;
  onDelete: (job: GatewayConsoleTypes.VideoJob) => void;
}) {
  const { t } = useI18n();
  if (isRunning(job.status)) {
    const stoppable =
      job.cancel === "anytime" || (job.cancel === "queued_only" && job.status === "queued");
    return stoppable ? (
      <Button variant="ghost" size="sm" onClick={() => onCancel(job)}>
        {t("videosCancel")}
      </Button>
    ) : (
      // Said rather than left blank. "This model cannot be stopped" is an answer;
      // an empty cell is the reader wondering whether the button failed to load.
      <span className="text-base text-kumo-subtle" title={t("videosNotCancelableHint")}>
        {t("videosNotCancelable")}
      </span>
    );
  }
  // Offered only where it works, the same rule the cancel button follows. Being
  // terminal is not enough: a job whose charge has not settled or voided yet is
  // the only row pointing at its reservation, so the server refuses it and says
  // so through `deletable` rather than leaving the interface to infer it from a
  // status that does not carry the answer.
  const del = job.deletable ? (
    <Button variant="ghost" size="sm" onClick={() => onDelete(job)}>
      {t("videosDelete")}
    </Button>
  ) : null;
  if (job.status !== "completed") return del;
  // Past its retention window the film is gone, and that is a normal ending
  // rather than a fault. A dead download button would be worse than saying so.
  if (!job.artifact?.available)
    return (
      <div className="flex items-center gap-1">
        <span className="text-base text-kumo-subtle">{t("videosArtifactGone")}</span>
        {del}
      </div>
    );
  return (
    <div className="flex items-center gap-1">
      <LinkButton
        href={gatewayConsoleApi.getGetVideoJobContentUrl(orgId, job.id)}
        download={`${job.id}.mp4`}
        variant="outline"
        size="sm"
      >
        {t("videosDownload")}
      </LinkButton>
      {del}
    </div>
  );
}
