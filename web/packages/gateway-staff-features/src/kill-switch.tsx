import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { gatewayStaffApi, type GatewayStaffTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { Button, Card, Field, FormDialog, SectionHeading, Textarea } from "@fairlb/ui";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

// The operator surface of the three-level kill switch.
//
// The mechanism itself is on the server: the global level is a setting the proxy
// reads on every request — and a failed read is treated as "not tripped", because a
// database wobble must not pull the switch on an operator's behalf — while the
// provider and model levels are their own enabled flags.
//
// What was missing is this layer: during an incident the switch has to be findable
// in thirty seconds, pullable, and impossible to forget about afterwards. Buried in
// a list of key-value settings next to a retention period, it was none of those.

/**
 * Reads the current value of the global switch.
 *
 * It uses a **dedicated endpoint** rather than picking the key out of a general
 * settings list, because that list is not part of every shell's API. Reading it from
 * there meant calling an endpoint that answers 404 in some deployments — and the
 * symptom was a banner that never appears and a switch whose button does nothing,
 * with **the page itself rendering perfectly**.
 */
function useKillSwitch() {
  // A 30s stale time with a 60s poll: the banner is mounted on every page, so it
  // must not become a source of load. Nor does pulling the switch need sub-second
  // propagation — whoever pulled it just clicked, and their own page invalidates
  // and refetches immediately.
  const q = gatewayStaffApi.useGetGatewayKillSwitch({
    query: { staleTime: 30_000, refetchInterval: 60_000 },
  });
  return { query: q, entry: q.data, active: q.data?.active === true };
}

/**
 * The banner shown across the whole shell while the switch is pulled.
 *
 * "Still tripped, and nobody remembers" is the classic failure of a switch like
 * this: its control lives on the gateway health page, and operators spend most of
 * their time elsewhere. It reuses the styling of other persistent
 * dangerous-state notices, because that is what it is.
 */
export function GatewayKillSwitchBanner() {
  const { t } = useI18n();
  const { active } = useKillSwitch();
  if (!active) return null;
  return (
    <div className="bg-kumo-danger-tint px-4 py-2 text-center text-base font-medium text-kumo-danger">
      {t("gwKillActiveBanner")}{" "}
      <Link to="/gateway/health" className="underline">
        {t("gwKillGoManage")}
      </Link>
    </div>
  );
}

/**
 * The three-level switch card at the top of the gateway health page.
 *
 * The counts for the second and third levels are **passed in by the caller from the
 * health response**, where the server counts the whole table, rather than being
 * derived here by fetching the provider and model lists and counting them. Two
 * reasons: the page issues two fewer requests, and — more importantly — those lists
 * are capped. Counting them locally answers "how many are disabled *in the first
 * page of results*", so **the day a cap takes effect these two numbers silently go
 * wrong**.
 */
export function KillSwitchCard({ counts }: { counts?: GatewayStaffTypes.GatewayKillSwitchCounts }) {
  const { t } = useI18n();
  const toasts = useKumoToastManager();
  const { query, entry, active } = useKillSwitch();
  const put = gatewayStaffApi.usePutGatewayKillSwitch();

  // The dialog demands a reason: an audit trail without a "why" is not an audit
  // trail. Restoring demands one too — "pulled it by mistake" and "the upstream is
  // fixed" are entirely different entries in an incident record.
  const [confirming, setConfirming] = useState(false);
  const [reason, setReason] = useState("");

  const pull = () =>
    put.mutate(
      { data: { active: !active, reason: reason.trim() } },
      {
        onSuccess: () => {
          setConfirming(false);
          setReason("");
          toasts.add({
            variant: "success",
            title: active ? t("gwKillRestored") : t("gwKillPulled"),
          });
          void query.refetch();
        },
      },
    );

  return (
    <Card className="space-y-3">
      <SectionHeading>{t("gwKillTitle")}</SectionHeading>

      {/* Level one: global. Its exact semantics are spelled out on screen — anyone
          about to pull it should know what it does and does not cover. */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <div className="font-medium">
            {t("gwKillGlobal")}:{" "}
            <span className={active ? "text-kumo-danger" : "text-kumo-success"}>
              {active ? t("gwKillStateActive") : t("gwKillStateOff")}
            </span>
          </div>
          <p className="text-base text-kumo-subtle">{t("gwKillGlobalHint")}</p>
        </div>
        <Button
          variant={active ? "primary" : "destructive"}
          size="sm"
          onClick={() => setConfirming(true)}
          disabled={entry === undefined}
        >
          {active ? t("gwKillRestore") : t("gwKillPull")}
        </Button>
      </div>

      {/* Levels two and three: a way in, plus how many are currently disabled. The
          controls themselves live on their own pages and are not rebuilt here.
          While the counts are unavailable, **no number is written**: "could not
          read it" and "none are disabled" are different facts, and on a kill-switch
          card "0 disabled" is a dangerous lie. The link is offered either way. */}
      <div className="flex flex-wrap gap-x-6 gap-y-1 border-t border-kumo-line pt-3 text-base">
        <span>
          {counts
            ? t("gwKillProviderLevel", {
                off: counts.providers_disabled,
                total: counts.providers_total,
              })
            : t("gwKillLevelCountUnknown")}{" "}
          <Link to="/gateway/providers" className="underline">
            {t("gwKillGoManage")}
          </Link>
        </span>
        <span>
          {counts
            ? t("gwKillModelLevel", {
                off: counts.models_disabled,
                total: counts.models_total,
              })
            : t("gwKillLevelCountUnknown")}{" "}
          <Link to="/gateway/models" className="underline">
            {t("gwKillGoManage")}
          </Link>
        </span>
      </div>

      <FormDialog
        size="lg"
        open={confirming}
        onOpenChange={setConfirming}
        title={active ? t("gwKillRestoreTitle") : t("gwKillPullTitle")}
        description={active ? t("gwKillRestoreBody") : t("gwKillPullBody")}
        error={put.isError ? apiErrorMessage(put.error) : undefined}
        submitLabel={active ? t("gwKillRestore") : t("gwKillPull")}
        submitVariant={active ? "primary" : "destructive"}
        submitDisabled={!reason.trim()}
        pending={put.isPending}
        onSubmit={pull}
      >
        <Field label={t("gwKillReason")} htmlFor="kill-reason" required>
          <Textarea
            id="kill-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            maxLength={500}
            required
          />
        </Field>
      </FormDialog>
    </Card>
  );
}
