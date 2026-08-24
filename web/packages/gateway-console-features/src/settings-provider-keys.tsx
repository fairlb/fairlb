import { useI18n } from "@fairlb/i18n";
import { Button, SectionHeading, SettingsSection } from "@fairlb/ui";
import { useState } from "react";
import { BYOKSection } from "./byok";
import { useOrgSettings } from "./host";

/**
 * Bring-your-own-key provider credentials.
 *
 * This lives in the settings area rather than inline on the overview page, where it
 * used to sit behind a `canManage &&` — credential management mixed into a
 * monitoring view. With the gate moved up to the local task area, someone without the
 * right who types the URL gets a forbidden page instead of a blank one.
 */
export function SettingsProviderKeysPage() {
  const { org } = useOrgSettings();
  return <ProviderKeys orgId={org.id} />;
}

function ProviderKeys({ orgId }: { orgId: string }) {
  const { t } = useI18n();
  const [adding, setAdding] = useState(false);
  return (
    <SettingsSection
      title={
        // The create action of a section-level list sits at the right of its heading row.
        <div className="flex flex-wrap items-center justify-between gap-2">
          <SectionHeading>{t("byokTitle")}</SectionHeading>
          <Button onClick={() => setAdding(true)}>{t("byokAdd")}</Button>
        </div>
      }
      description={t("byokIntro")}
    >
      <BYOKSection orgId={orgId} adding={adding} onAddingChange={setAdding} />
    </SettingsSection>
  );
}
