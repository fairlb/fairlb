import { LinkButton } from "@cloudflare/kumo/components/button";
import { isMessageKey, useI18n, type MessageKey } from "@fairlb/i18n";
import {
  Alert,
  Card,
  FormColumn,
  LoadingState,
  PageContentsNav,
  SectionHeading,
  StatusBadge,
} from "@fairlb/ui";
import { useSettingsHost, type DedicatedPage, type SettingEntry } from "./host";
import { formsOf, KeyLabel, SettingsForm, useDescription, type Form } from "./section-form";

/**
 * The registry-driven settings editor both operator consoles render (ADR-0198):
 * every registered key becomes a control, grouped by what an operator is
 * trying to do, with a section's keys on one form. Keys or sections the host
 * edits on a dedicated page render as a pointer to it instead of a second form.
 *
 * Group order: who gets in → how is this priced → the brakes → how long data is
 * kept. The registry returns keys in lexical order, which splits the three
 * pricing keys apart by prefix and interleaves them with retention windows —
 * an order that tells the reader nothing (ADR-0068).
 */
const GROUP_ORDER = ["access", "billing", "operations", "retention"] as const;

const GROUP_LABEL: Record<(typeof GROUP_ORDER)[number], { title: MessageKey; hint: MessageKey }> = {
  access: { title: "settingsGroupAccess", hint: "settingsGroupAccessHint" },
  billing: { title: "settingsGroupBilling", hint: "settingsGroupBillingHint" },
  operations: { title: "settingsGroupOperations", hint: "settingsGroupOperationsHint" },
  retention: { title: "settingsGroupRetention", hint: "settingsGroupRetentionHint" },
};

/**
 * Load the registry before mounting either side of the long-page layout. The
 * contents nav discovers section targets in an effect; mounting it while the
 * query is still pending leaves it with no targets and the child's later query
 * render cannot rerun that parent effect. Rendering the groups and their nav
 * together gives the two surfaces one source of truth and one commit.
 */
export function SettingsRegistryPage() {
  const { t } = useI18n();
  const host = useSettingsHost();
  const list = host.useListSettings();

  if (list.isError) return <Alert>{host.errorMessage(list.error)}</Alert>;
  if (list.isPending) return <LoadingState label={t("loading")} />;

  const items = list.data?.items ?? [];
  // An unrecognised group is not dropped silently: when the server adds a
  // group the interface has not learned yet, those keys would vanish as a
  // whole, and "one row fewer" does not look like a defect. Folding them into
  // the last group at least keeps them editable.
  const known = new Set<string>(GROUP_ORDER);
  const byGroup = new Map<string, SettingEntry[]>();
  for (const e of items) {
    const g = known.has(e.group) ? e.group : "operations";
    byGroup.set(g, [...(byGroup.get(g) ?? []), e]);
  }
  const groups = GROUP_ORDER.filter((group) => (byGroup.get(group)?.length ?? 0) > 0);
  const refetch = () => void list.refetch();

  return (
    <div className="flex min-w-0 flex-col gap-6 2xl:flex-row 2xl:items-start 2xl:gap-10">
      <FormColumn className="min-w-0 flex-1">
        <div className="space-y-8">
          <p className="text-base text-kumo-subtle">{t("settingsHotReloadHint")}</p>
          {groups.map((group) => (
            <section id={`system-settings-${group}`} key={group} className="scroll-mt-6 grid gap-3">
              <div className="grid gap-1.5">
                <SectionHeading>{t(GROUP_LABEL[group].title)}</SectionHeading>
                <p className="text-base text-kumo-subtle">{t(GROUP_LABEL[group].hint)}</p>
              </div>
              <Card className="space-y-6">
                {formsOf(byGroup.get(group) ?? []).map((form) => {
                  const key = form.section ?? form.entries[0]!.key;
                  const dedicatedKey =
                    form.section === null ? host.dedicatedPages?.[form.entries[0]!.key] : undefined;
                  const dedicatedSection =
                    form.section !== null ? host.dedicatedSections?.[form.section] : undefined;
                  if (dedicatedKey) {
                    return (
                      <DedicatedKeyPointer key={key} entry={form.entries[0]!} page={dedicatedKey} />
                    );
                  }
                  if (dedicatedSection) {
                    return (
                      <DedicatedSectionPointer key={key} form={form} page={dedicatedSection} />
                    );
                  }
                  return <SettingsForm key={key} form={form} onSaved={refetch} />;
                })}
              </Card>
            </section>
          ))}
        </div>
      </FormColumn>
      <PageContentsNav
        items={groups.map((group) => ({
          href: `#system-settings-${group}`,
          label: t(GROUP_LABEL[group].title),
        }))}
      />
    </div>
  );
}

function pageLabel(t: (key: MessageKey) => string, page: DedicatedPage): string {
  return isMessageKey(page.labelKey) ? t(page.labelKey) : page.labelKey;
}

/**
 * A key with a dedicated editing page renders one pointer row here (ADR-0043
 * §2): the kill switch wants its global banner and health context, the sign-up
 * gate wants the invite-code set, and both exist only on their own page. Two
 * entrances with two confirmation texts would be two sources of truth.
 */
function DedicatedKeyPointer({ entry, page }: { entry: SettingEntry; page: DedicatedPage }) {
  const { t } = useI18n();
  const description = useDescription(entry);
  return (
    <div className="grid gap-1.5">
      <KeyLabel entry={entry} />
      <p className="text-base text-kumo-subtle">{description}</p>
      <p className="flex flex-wrap items-center gap-2 text-base text-kumo-subtle">
        <span>{t("settingsDedicatedPointer")}</span>
        <LinkButton variant="ghost" size="sm" href={page.href}>
          {pageLabel(t, page)}
        </LinkButton>
        <span className="font-mono text-[0.9em]">({String(entry.value ?? "—")})</span>
      </p>
    </div>
  );
}

/** A whole section with its own page (an integration): one pointer, the form lives there. */
function DedicatedSectionPointer({ form, page }: { form: Form; page: DedicatedPage }) {
  const { t } = useI18n();
  const set = form.entries.filter((e) => e.set).length;
  return (
    <div className="grid gap-1.5">
      <span className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[0.9em]">{form.section}</span>
        <StatusBadge tone={set > 0 ? "success" : "neutral"}>
          {t("settingsSectionSetCount", { set, total: form.entries.length })}
        </StatusBadge>
      </span>
      <p className="flex flex-wrap items-center gap-2 text-base text-kumo-subtle">
        <span>{t("settingsDedicatedPointer")}</span>
        <LinkButton variant="ghost" size="sm" href={page.href}>
          {pageLabel(t, page)}
        </LinkButton>
      </p>
    </div>
  );
}
