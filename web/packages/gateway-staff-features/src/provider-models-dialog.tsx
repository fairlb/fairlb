import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { RouteStatusBadge, WiringIntentCell, WiringTable } from "./wiring-table";
import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  Checkbox,
  Combobox,
  ConfirmDialog,
  DataTable,
  Field,
  FormDialog,
  FormRow,
  InlineEmpty,
  Input,
  LoadingState,
  StatusBadge,
  useDebounced,
} from "@fairlb/ui";
import { keepPreviousData } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import {
  canPriceFromReference,
  canWire,
  checkedOf,
  composerIssue,
  draftIssue,
  mergeRows,
  newModelDraft,
  planWiring,
  referencePriceTargets,
  resolveModelForUpstream,
  rowKey,
  upstreamFromSlug,
  type ManualEntry,
  type ModelLike,
  type NewModelDraft,
  type WiringRowView,
} from "./route-wiring";

/**
 * Editing the whole set of "which upstream models this provider serves".
 *
 * The entry point deliberately starts from the upstream. Built the other way round —
 * pick from the local catalog, append a row — it answers only **half the question**,
 * and in the commonest case, where most candidates are already configured, the picker
 * shows a pile of things already done: choose one and you get a duplicate and a 409.
 *
 * As it stands: **what is configured is always in the list, always ticked, and always
 * first**; candidates fetched from the upstream follow; manual entries land in the
 * same table. Ticking an unconfigured row creates it on save.
 *
 * **The checkbox (intent) and the status badge (what is stored) are two independent
 * things, and both are always rendered.** Normally they agree; after a partial
 * failure they **visibly disagree** — which is the only honest way to say "this one
 * did not take effect".
 */
/** How many deletions the confirmation names individually; beyond that it reports a
 * count. */
const MAX_NAMED_DELETES = 8;

export function ProviderModelsDialog({
  open,
  onOpenChange,
  provider,
  discovered,
  complete,
  discoverError,
  discoverPending,
  onDiscover,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  provider: GatewayStaffTypes.GatewayProvider;
  /** null means never fetched, or the fetch failed. **Do not pass `?? []`**: that
   * lets "never checked" masquerade as "checked, and the upstream does not have it". */
  discovered: readonly GatewayStaffTypes.DiscoveredModel[] | null;
  /**
   * Why the last fetch failed; null when none was attempted.
   *
   * **"Nothing was fetched" and "a fetch happened but reached no conclusion" have to
   * be said separately.** Both leave `discovered` null — neither supports any claim
   * about the upstream — but to a reader they are different sentences. A banner saying
   * "the catalog has not been fetched from the upstream yet" above a fetch that just
   * failed is simply untrue.
   */
  discoverError: string | null;
  /** Whether this enumeration was complete. When false, nothing is marked as "no
   * longer offered upstream". */
  complete: boolean;
  discoverPending: boolean;
  /** Fetching really calls the upstream and really costs money, so the mutation
   * belongs to **the face**; this is only a second entry point to the same action. */
  onDiscover: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  // The dialog issues both queries itself — they are deduplicated, so this costs no
  // extra request — because **only the query object itself distinguishes the three
  // states**. `data?.data ?? []` launders "not fetched yet" and "the fetch failed"
  // into "fetched, and it is empty".
  const routes = gatewayStaffApi.useListGatewayProviderRoutes(provider.id);
  const batchWire = gatewayStaffApi.useBatchWireProviderRoutes();
  const importPrices = gatewayStaffApi.useImportGatewayReferencePrices();

  const [over, setOver] = useState<Map<string, boolean>>(new Map());
  // The catalog entry an unknown row will create once ticked, one per row. Inline
  // rather than behind a separate review dialog: a review belongs on the row it is
  // going to change.
  const [drafts, setDrafts] = useState<Map<string, NewModelDraft>>(new Map());
  // The rows whose price should be filled in from the reference dataset once
  // the wiring lands. Held by row key, like every other per-row intent here.
  //
  // Off by default. Wiring a model and pricing it are two decisions, and the
  // second one puts a number on an invoice: it is offered, on the rows where it
  // is the difference between "attached" and "sellable", and it is never
  // assumed.
  const [priceRows, setPriceRows] = useState<Set<string>>(new Set());
  const [manual, setManual] = useState<ManualEntry[]>([]);
  const [errors, setErrors] = useState<Map<string, string>>(new Map());
  const [pending, setPending] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [pick, setPick] = useState<ModelLike | null>(null);
  const [upstream, setUpstream] = useState("");
  const [query, setQuery] = useState("");
  // 候选从搜索来（ADR-0189）。这个弹窗本来就有 `query`——它此前只驱动本地过滤，
  // 现在直接发到服务端；目录分页之后，本地过滤只能过滤第一页。
  //
  // 已配置的那些不受影响：它们来自 `routes`，且名字与协议随路由返回。
  const settledQuery = useDebounced(query, 250);
  const models = gatewayStaffApi.useListGatewayModels(
    { q: settledQuery },
    { query: { enabled: open, placeholderData: keepPreviousData } },
  );

  // Reopening discards local edits, the same way switching targets does elsewhere.
  useEffect(() => {
    setOver(new Map());
    setDrafts(new Map());
    setPriceRows(new Set());
    setManual([]);
    setErrors(new Map());
    setPick(null);
    setUpstream("");
    setQuery("");
  }, [provider.id, open]);

  const ready = routes.isSuccess && models.isSuccess;
  const catalog = useMemo(() => models.data?.items ?? [], [models.data]);
  // Every catalog model is a candidate: a model owns no protocol, so there is
  // nothing to match against the provider's dialects. The name filter is the
  // server's (`q`).
  const options = useMemo(() => catalog.map((m) => ({ id: m.id, slug: m.slug })), [catalog]);

  const rows = useMemo(
    () =>
      ready
        ? mergeRows({
            routes: routes.data.items,
            discovered,
            complete,
            manual,
          })
        : [],
    [ready, routes.data, discovered, complete, manual],
  );
  const view: WiringRowView[] = rows.map((r) => ({
    ...r,
    checked: checkedOf(r, over),
    error: errors.get(r.key),
    // Only ticked unknown rows carry a draft: an unticked row must not hold a pending
    // entry, or the confirmation's "K new entries" would count things nobody asked
    // for.
    ...(r.modelId === null && checkedOf(r, over) ? { draft: drafts.get(r.key) } : {}),
  }));
  const plan = planWiring(view);
  const newModelAdds = plan.creates.filter((c) => c.newModel !== undefined).length;
  // A row with an incomplete draft blocks submission: a slug is immutable once
  // created, so it is better stopped here.
  const draftBlocked = view.some((r) => r.draft !== undefined && draftIssue(r.draft) !== null);
  // Ticked *and* still eligible. Recomputed from the rows rather than trusted
  // from the set on its own: unticking a row must take its pricing intent with
  // it, and a stale key would otherwise price a model this save never touched.
  const priceKeys = new Set(
    view.filter((r) => canPriceFromReference(r) && priceRows.has(r.key)).map((r) => r.key),
  );
  // Rows that will be wired and **still** have no price when this save
  // finishes. Rows the operator has asked to price are not among them: warning
  // about a problem that was solved two lines up on the same screen teaches
  // people to skip the warning.
  const unpricedAdds = plan.creates.filter(
    (c) =>
      view.find((r) => r.key === c.key)?.discoveredState === "unpriced" && !priceKeys.has(c.key),
  ).length;

  const issue = composerIssue(pick?.id ?? null, upstream, rows);

  /**
   * Choosing a model **must also write the query**: the combobox's input value is
   * controlled, so setting only the selection leaves the input blank — a model
   * resolved programmatically stays invisible and the reader concludes nothing was
   * found. That is exactly how the first version got it wrong, and the test covering
   * "type an upstream name, get the model back" caught it.
   */
  const selectModel = (m: ModelLike | null) => {
    setPick(m);
    setQuery(m?.slug ?? "");
  };

  const addManual = () => {
    if (!pick || issue) return;
    setManual((prev) => [
      ...prev,
      { modelId: pick.id, slug: pick.slug, upstream: upstream.trim() },
    ]);
    // A newly added row starts ticked: adding it by hand already said "I want this".
    setOver((prev) => new Map(prev).set(rowKey(pick.id, upstream.trim()), true));
    setPick(null);
    setUpstream("");
    setQuery("");
  };

  /**
   * One batch request, with a per-row outcome in the response.
   *
   * Of the two objections once raised against a batch endpoint, one does not hold and
   * the other addresses a different shape. "Per-row truth is a granularity a batch
   * cannot give" **does not hold**: granularity is decided by **the response shape**,
   * not by whether the request carried one item or many. "Replacing the whole set has
   * no meaning for these records" **still holds word for word**, which is why what is
   * sent is an explicit list of creates and deletes rather than a whole-set replace.
   *
   * **Creating before deleting is now guaranteed by the server's transaction**, not by
   * the order two loops happen to be written in here — an order that closing a tab is
   * enough to break. The server has a test witnessing it.
   *
   * Translating a conflict or a missing row into "already" moved to the server too,
   * so that predicate exists once.
   */
  const submit = async () => {
    setPending(true);
    const failed = new Map<string, string>();
    const failedNames: string[] = [];
    let ok = 0;
    let already = 0;
    let toPrice: string[] = [];

    try {
      const res = await batchWire.mutateAsync({
        providerId: provider.id,
        data: {
          creates: plan.creates.map((c) => ({
            // One or the other: an existing catalog entry is referenced by id, while
            // an unknown row carries the entry to create.
            ...(c.newModel
              ? {
                  new_model: {
                    slug: c.newModel.slug.trim(),
                    ...(c.newModel.displayName.trim()
                      ? { display_name: c.newModel.displayName.trim() }
                      : {}),
                  },
                }
              : { model_id: c.modelId ?? "" }),
            provider_model_id: c.upstream,
          })),
          deletes: plan.deletes.map((d) => ({ model_id: d.modelId, route_id: d.routeId })),
        },
      });
      // **Matched back by index**: the results correspond one to one, in order, with
      // the creates followed by the deletes, as the contract specifies. It is the only
      // correspondence that holds in every case — a row that creates its own model has
      // no model id beforehand, and none afterwards either if the creation failed, so
      // matching on (model, upstream) would skip precisely the rows that most need to
      // report an error.
      const planned = [...plan.creates, ...plan.deletes];
      res.results.forEach((r, i) => {
        if (r.outcome === "failed") {
          const key = planned[i]?.key ?? rowKey(r.model_id ?? null, r.provider_model_id);
          const row = rows.find((x) => x.key === key);
          failed.set(key, r.detail || t("gwWiringRowFailed"));
          failedNames.push(`${row?.slug || r.provider_model_id} → ${r.provider_model_id}`);
          return;
        }
        ok += 1;
        if (r.outcome === "already") already += 1;
      });
      // Read off the same index correspondence, and only after the wiring
      // landed: the import matches a model through its routes, so before this
      // request there is nothing for it to match on — and for a row that
      // created its own catalog entry, no model id either.
      toPrice = referencePriceTargets(planned, res.results, priceKeys);
    } catch (err) {
      // The whole request failed — network, authentication, a provider that no longer
      // exists. That is a different fact from "some rows did not go through": there are
      // no per-row errors to speak of, so the batch is marked as failed rather than
      // smearing one sentence across every row.
      setPending(false);
      toasts.add({ variant: "warning", title: apiErrorMessage(err) });
      return;
    }
    // Pricing runs before the dialog reports and closes, so that its outcome is
    // in the same breath as the wiring's. Its failure is **not** the wiring's
    // failure though: the routes are stored either way, and saying otherwise
    // would send someone to redo a save that worked.
    const priced = toPrice.length > 0 ? await priceFromReference(toPrice) : null;

    setPending(false);
    setErrors(failed);
    // **The overrides are not cleared**: failed rows must keep the operator's intent,
    // so that the disagreement between checkbox and badge stays visible. Successful
    // rows pick up a real route id from the refetch below, and the `??` in the tick
    // lookup follows the truth on its own, so leaving their overrides in place is
    // harmless.
    onSaved();
    toasts.add({
      variant: failed.size === 0 ? "success" : "warning",
      title: t("gwRoutesCreated", { ok: String(ok), failed: String(failed.size) }),
      description:
        [
          already > 0 ? t("gwWiringAlreadySoCount", { n: String(already) }) : "",
          failedNames.join(", "),
        ]
          .filter(Boolean)
          .join(" · ") || undefined,
    });
    if (priced !== null) toasts.add(priced);
    if (failed.size === 0) onOpenChange(false);
  };

  /**
   * Fills the reference prices in for the models just wired, and turns the
   * result into something worth reading.
   *
   * Every model it declined to price is **named**, not merely subtracted from a
   * count. That is the same rule the import itself follows, and for the same
   * reason: a model with no price refuses traffic, so "3 of 5 priced" leaves
   * two silent failures behind exactly where a silent failure hurts.
   *
   * Nothing it writes is marked as checked, and the notification says so. The
   * dataset is a reference; agreeing to charge against it is a separate act,
   * done on the model's page.
   */
  const priceFromReference = async (modelIds: string[]) => {
    try {
      const report = await importPrices.mutateAsync({ data: { model_ids: modelIds } });
      const wrote = report.results.filter((r) => r.outcome === "priced");
      const missed = report.results.filter((r) => r.outcome !== "priced");
      return {
        variant: missed.length === 0 ? ("success" as const) : ("warning" as const),
        title: t("gwWiringPricedFromReference", {
          ok: String(wrote.length),
          n: String(report.results.length),
        }),
        description:
          [
            missed.length > 0
              ? t("gwWiringNotPricedFromReference", {
                  names: missed
                    .slice(0, MAX_NAMED_DELETES)
                    .map((r) => `${r.model}: ${r.detail}`)
                    .join("; "),
                })
              : "",
            wrote.length > 0 ? t("gwWiringReferencePricesUnverified") : "",
          ]
            .filter(Boolean)
            .join(" · ") || undefined,
      };
    } catch (err) {
      return {
        variant: "warning" as const,
        title: t("gwWiringPriceImportFailed"),
        description: apiErrorMessage(err),
      };
    }
  };

  return (
    <>
      <FormDialog
        open={open}
        onOpenChange={onOpenChange}
        size="xl"
        title={t("gwWiringTitle", { slug: provider.slug })}
        description={t("gwWiringHint")}
        error={errors.size > 0 ? t("gwWiringSomeFailed") : undefined}
        submitLabel={t("gwWiringSubmit", {
          add: String(plan.creates.length),
          del: String(plan.deletes.length),
        })}
        submitDisabled={
          !ready ||
          (plan.creates.length === 0 && plan.deletes.length === 0) ||
          draftBlocked ||
          pending
        }
        pending={pending}
        onSubmit={() => setConfirming(true)}
      >
        <UpstreamBanner
          discovered={discovered}
          complete={complete}
          discoverError={discoverError}
          discoverPending={discoverPending}
          onDiscover={onDiscover}
        />

        {/* The three-state gate over both queries: **while either is pending or
            failed, not one checkbox is rendered**. "Everything configured is here and
            ticked" is this dialog's central promise, and while a query is pending that
            is unknown — drawing an all-empty checklist would be a false statement
            about the current configuration. */}
        {routes.isPending || models.isPending ? (
          <LoadingState label={t("loading")} />
        ) : routes.isError ? (
          <Alert>{apiErrorMessage(routes.error)}</Alert>
        ) : models.isError ? (
          <Alert>{apiErrorMessage(models.error)}</Alert>
        ) : (
          <>
            <Composer
              options={options}
              pick={pick}
              upstream={upstream}
              query={query}
              issue={issue}
              onPick={(m) => {
                selectModel(m);
                // Choosing a model prefills the upstream name by the server's own
                // rule; it stays editable.
                if (m && upstream.trim() === "") setUpstream(upstreamFromSlug(m.slug));
              }}
              onUpstream={(next) => {
                setUpstream(next);
                // And the reverse: typing an upstream name resolves the model. A miss
                // leaves it blank for the operator to choose, **never approximated**.
                if (!pick) selectModel(resolveModelForUpstream(next, options));
              }}
              onQuery={setQuery}
              onAdd={addManual}
            />
            <ModelWiringTable
              caption={t("gwWiringTitle", { slug: provider.slug })}
              rows={view}
              pending={pending}
              priceKeys={priceKeys}
              onPriceToggle={(key, next) =>
                setPriceRows((prev) => {
                  const out = new Set(prev);
                  if (next) out.add(key);
                  else out.delete(key);
                  return out;
                })
              }
              onToggle={(key, next) => {
                setOver((prev) => new Map(prev).set(key, next));
                // Ticking an unknown row seeds a draft with the slug prefilled from the
                // upstream name. **Unticking does not discard it**: someone who fills in
                // half of it and mis-clicks finds their work still there.
                const row = rows.find((r) => r.key === key);
                if (next && row && row.modelId === null && !drafts.has(key)) {
                  setDrafts((prev) => new Map(prev).set(key, newModelDraft(row.upstream)));
                }
              }}
              onDraft={(key, next) => setDrafts((prev) => new Map(prev).set(key, next))}
            />
          </>
        )}
      </FormDialog>

      {/* A sibling rather than a child: inside the `<form>`, the confirm button would
          entangle itself with form submission semantics. There is a second benefit —
          pressing enter in the combobox with no highlighted option lets the surrounding
          form submit, and with this gate in the way the consequence of that mis-press
          is merely a confirmation dialog. */}
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        // A deletion cuts the traffic through it immediately, so it gets the
        // destructive dialog; a purely additive change gets the neutral one.
        destructive={plan.deletes.length > 0}
        title={t("gwWiringConfirmTitle")}
        description={[
          // **Both numbers appear, including a zero one**: printing only the non-zero
          // one makes "3 added" read as a purely additive change, leaving the reader no
          // way to know this dialog deletes things at all.
          t("gwWiringConfirmBody", {
            add: String(plan.creates.length),
            del: String(plan.deletes.length),
            slug: provider.slug,
          }),
          // Creating catalog entries is **a third thing**, and one that has only just
          // become reachable — so this clause is appended only when there actually are
          // any. Saying "0 new catalog entries" is noise, unlike the two counts above,
          // which answer "does this dialog delete things".
          newModelAdds > 0
            ? t("gwWiringConfirmNewModels", {
                n: String(newModelAdds),
                slugs: plan.creates
                  .filter((c) => c.newModel)
                  .slice(0, MAX_NAMED_DELETES)
                  .map((c) => c.newModel?.slug ?? "")
                  .join(", "),
              })
            : "",
          // Deletions are **named**, not just counted — the lesson of "3 failed" that
          // never says which 3, applied before the fact rather than after.
          // The tail clause is appended only when there really are more: folded into a
          // single message it would render "and 0 more", which is untrue. Changing the
          // domain of a value means **re-reading what everything downstream says on the
          // new domain**.
          plan.deletes.length > 0
            ? [
                t("gwWiringConfirmRemoving", {
                  names: plan.deletes
                    .slice(0, MAX_NAMED_DELETES)
                    .map((d) => `${d.slug || d.upstream} → ${d.upstream}`)
                    .join(", "),
                }),
                plan.deletes.length > MAX_NAMED_DELETES
                  ? t("gwWiringConfirmRemovingMore", {
                      more: String(plan.deletes.length - MAX_NAMED_DELETES),
                    })
                  : "",
              ]
                .filter(Boolean)
                .join(" ")
            : "",
          unpricedAdds > 0 ? t("gwWiringConfirmUnpriced", { n: String(unpricedAdds) }) : "",
          // Writing a price is its own consequence and gets its own sentence.
          priceKeys.size > 0 ? t("gwWiringConfirmPricing", { n: String(priceKeys.size) }) : "",
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

/**
 * The five states of the upstream dimension, each saying its own thing and none
 * masquerading as another.
 *
 * The primary entry point for fetching is on **the face**: it really calls the
 * upstream and really costs money, while a button inside a dialog beside a column of
 * checkboxes reads as a free local operation. This offers a second entry point to the
 * same action only when nothing has been fetched or the fetch failed, and its label is
 * **word for word** the one on the face — "this action costs money" only needs to be
 * learned once.
 */
function UpstreamBanner({
  discovered,
  complete,
  discoverError,
  discoverPending,
  onDiscover,
}: {
  discovered: readonly GatewayStaffTypes.DiscoveredModel[] | null;
  complete: boolean;
  discoverError: string | null;
  discoverPending: boolean;
  onDiscover: () => void;
}) {
  const { t } = useI18n();
  if (discoverPending) return <Alert variant="info">{t("gwDiscoverRunning")}</Alert>;
  // A fetch happened but reached no conclusion. **A different sentence** from "nothing
  // has been fetched" below: saying "the catalog has not been fetched from the upstream
  // yet" above a fetch that just failed is untrue. Both leave `discovered` null —
  // neither supports any claim about the upstream — but the wording must differ.
  if (discoverError !== null) {
    return (
      <Alert>
        {discoverError}{" "}
        <button type="button" className="underline" onClick={onDiscover}>
          {t("gwDiscoverRun")}
        </button>
      </Alert>
    );
  }
  if (discovered === null) {
    return (
      <Alert variant="info">
        {t("gwDiscoverNeverRun")}{" "}
        <button type="button" className="underline" onClick={onDiscover}>
          {t("gwDiscoverRun")}
        </button>
      </Alert>
    );
  }
  // A complete enumeration returning nothing: every configuration held locally now
  // points at nothing, which is the state most worth raising an alarm about.
  if (complete && discovered.length === 0) {
    return <Alert variant="warning">{t("gwDiscoverEmptyUpstream")}</Alert>;
  }
  // Incomplete: candidates are merged in as usual, but absence proves nothing, and
  // each row's upstream flag stays null.
  if (!complete) return <Alert variant="warning">{t("gwDiscoverCoveragePartial")}</Alert>;
  return null;
}

/** Adding by hand: choose a model, type the upstream name, add. Each field uses the
 * control its own shape calls for. */
function Composer({
  options,
  pick,
  upstream,
  query,
  issue,
  onPick,
  onUpstream,
  onQuery,
  onAdd,
}: {
  options: ModelLike[];
  pick: ModelLike | null;
  upstream: string;
  query: string;
  issue: ReturnType<typeof composerIssue>;
  onPick: (m: ModelLike | null) => void;
  onUpstream: (next: string) => void;
  onQuery: (next: string) => void;
  onAdd: () => void;
}) {
  const { t } = useI18n();
  if (options.length === 0) {
    return (
      <InlineEmpty
        title={t("gwAddRoutesNoCandidates")}
        description={t("gwAddRoutesNoCandidatesHint")}
      />
    );
  }
  return (
    <div className="rounded-lg bg-kumo-recessed p-4">
      {/* Three aligned tracks — label, control, message — rather than aligning the
          boxes by their bottom edges: the two fields have hints of different lengths,
          and bottom alignment would leave their inputs at different heights. */}
      <FormRow className="sm:grid-cols-[1fr_1fr_auto]">
        <FormRow.Item>
          <Field label={t("gwColModel")} htmlFor="wiring-model">
            <Combobox<ModelLike>
              items={options}
              value={pick}
              onValueChange={(m) => onPick(m ?? null)}
              inputValue={query}
              // The selection path writes the query in one place; writing it here too
              // would leave two owners fighting over what the input should show.
              onInputValueChange={(next, details) => {
                if (details.reason !== "item-press") onQuery(next);
              }}
              itemToStringLabel={(m) => m.slug}
              isItemEqualToValue={(a, b) => a.id === b.id}
              autoHighlight
            >
              <Combobox.TriggerInput id="wiring-model" placeholder={t("gwSearchModel")} />
              <Combobox.Content>
                <Combobox.List>
                  {(m: ModelLike, index: number) => (
                    <Combobox.Item key={m.id} value={m} index={index}>
                      <span className="font-mono">{m.slug}</span>
                    </Combobox.Item>
                  )}
                </Combobox.List>
                <Combobox.Empty />
              </Combobox.Content>
            </Combobox>
          </Field>
        </FormRow.Item>
        <FormRow.Item>
          <Field
            label={t("gwColUpstreamModel")}
            htmlFor="wiring-upstream"
            hint={t("gwAddModelRouteUpstreamHint")}
            error={issue !== null && upstream.trim() !== "" ? t(issue) : undefined}
          >
            <Input
              id="wiring-upstream"
              value={upstream}
              onChange={(e) => onUpstream(e.target.value)}
              placeholder="gpt-4o"
            />
          </Field>
        </FormRow.Item>
        <FormRow.Actions>
          <Button variant="outline" disabled={issue !== null} onClick={onAdd}>
            {t("gwWiringAdd")}
          </Button>
        </FormRow.Actions>
      </FormRow>
    </div>
  );
}

function ModelWiringTable({
  caption,
  rows,
  pending,
  priceKeys,
  onToggle,
  onPriceToggle,
  onDraft,
}: {
  caption: string;
  rows: WiringRowView[];
  pending: boolean;
  /** The rows whose price is to be filled in from the reference dataset. */
  priceKeys: ReadonlySet<string>;
  onToggle: (key: string, next: boolean) => void;
  onPriceToggle: (key: string, next: boolean) => void;
  onDraft: (key: string, next: NewModelDraft) => void;
}) {
  const { t } = useI18n();
  return (
    <WiringTable
      caption={caption}
      columns={[t("gwColUpstreamModel"), t("gwColModel"), t("gwColStatus")]}
      empty={{ title: t("gwWiringEmpty"), description: t("gwWiringEmptyHint") }}
      rowCount={rows.length}
    >
      {rows.map((r) => (
        <DataTable.Row key={r.key}>
          {/* **取消勾选即删除路由** —— 编辑一整套集合本来就是这个意思，
              不是它的副作用。确认弹窗把删除条数单起一行并点名删的是谁。 */}
          <WiringIntentCell
            checked={r.checked}
            disabled={pending || !canWire(r)}
            label={r.upstream}
            onToggle={(next) => onToggle(r.key, next)}
          />
          <DataTable.Cell className="font-mono">{r.upstream}</DataTable.Cell>
          <DataTable.Cell>
            {r.modelId ? (
              <Link
                to="/gateway/models/$modelId"
                params={{ modelId: r.modelId }}
                hash="model-routes"
                className="font-mono text-kumo-info hover:underline"
              >
                {r.slug}
              </Link>
            ) : r.draft ? (
              // A ticked unknown row: **reviewed inline**, along with the catalog
              // entry it will create. A slug is immutable once created and the
              // prefill is only a guess at the server's rule, so it has to be visible
              // and editable before "reviewed" means anything. The review sits on the
              // row it changes, not in a second dialog.
              <NewModelFields
                draft={r.draft}
                disabled={pending}
                onChange={(next) => onDraft(r.key, next)}
              />
            ) : (
              // An unticked unknown row: says plainly that there is no local model,
              // and offers another way out.
              <span className="space-x-2">
                <span className="text-kumo-subtle">{t("gwWiringNoLocalModel")}</span>
                <Link to="/gateway/models" className="text-kumo-info hover:underline">
                  {t("gwWiringNoLocalModelHint")}
                </Link>
              </span>
            )}
          </DataTable.Cell>
          {/* 存的是什么由这一列说；左边那个勾选框说的是操作者想要什么。
                两者的分工见 WiringIntentCell 的注释。 */}
          <DataTable.Cell className="space-x-2 whitespace-nowrap">
            {r.routeId !== null && <RouteStatusBadge enabled={r.routeEnabled} />}
            {r.discoveredState === "unpriced" && (
              <StatusBadge tone="warning">{t("gwDiscoverState_unpriced")}</StatusBadge>
            )}
            {/* A row that will create its model says so plainly: what it creates is
             **disabled and unpriced**. Wired is true; sellable is not. */}
            {r.draft !== undefined && (
              <StatusBadge tone="neutral">{t("gwWiringWillCreate")}</StatusBadge>
            )}
            {r.onUpstream === false && (
              <StatusBadge tone="warning">{t("gwDiscoverState_gone")}</StatusBadge>
            )}
            {/* The way out of the state the two badges above describe, offered
                  on the row that is in it. Both `unpriced` and "will create"
                  mean the same thing after this save — wired and still refusing
                  traffic — and the only remaining step is four rates that the
                  bundled dataset already has. Left as a dead-end signal, that
                  step is a separate page, per model, retyped from a vendor's
                  price list.

                  It is an offer, never an assumption: what it writes is marked
                  unverified, and confirming it stays a separate act on the
                  model's own page. */}
            {canPriceFromReference(r) && (
              <Checkbox
                label={t("gwWiringPriceFromReference")}
                // The visible label is the same on every row, so the spoken
                // one names the row as well. It **contains** the visible text
                // rather than replacing it: a spoken name that does not
                // include what is written next to it leaves voice control
                // with nothing to say.
                aria-label={t("gwWiringPriceFromReferenceFor", { upstream: r.upstream })}
                checked={priceKeys.has(r.key)}
                disabled={pending}
                onCheckedChange={(next) => onPriceToggle(r.key, next === true)}
              />
            )}
            {r.error && <span className="text-base text-kumo-danger">{r.error}</span>}
          </DataTable.Cell>
        </DataTable.Row>
      ))}
    </WiringTable>
  );
}

/**
 * The inline review of an unknown row.
 *
 * Both fields are editable: the slug (prefilled from the upstream name) and the
 * display name. There is no protocol to choose — a model owns none; the route is
 * probed on whatever this provider speaks.
 */
function NewModelFields({
  draft,
  disabled,
  onChange,
}: {
  draft: NewModelDraft;
  disabled: boolean;
  onChange: (next: NewModelDraft) => void;
}) {
  const { t } = useI18n();
  const issue = draftIssue(draft);
  return (
    <div className="space-y-1">
      <Input
        aria-label={t("gwWiringDraftSlug")}
        value={draft.slug}
        disabled={disabled}
        className="font-mono"
        onChange={(e) => onChange({ ...draft, slug: e.target.value })}
      />
      {issue && <p className="text-base text-kumo-danger">{t(issue)}</p>}
    </div>
  );
}
