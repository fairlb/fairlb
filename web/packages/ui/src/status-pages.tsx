import { Button } from "./button";
import { Empty } from "@cloudflare/kumo/components/empty";
import { Link as KumoLink } from "@cloudflare/kumo/components/link";
import { CompassIcon, WarningCircleIcon } from "@phosphor-icons/react";
import { useI18n } from "@fairlb/i18n";
import { InlineEmpty } from "./inline-empty";

/** NotFoundState is the route-level 404; unmatched paths used to fall through to
 * the router's own default page. */
export function NotFoundState() {
  const { t } = useI18n();
  return (
    <div className="flex min-h-[50vh] items-center justify-center">
      <Empty
        icon={<CompassIcon size={48} className="text-kumo-inactive" />}
        title={t("notFoundTitle")}
        description={t("notFoundBody")}
        contents={<KumoLink href="/">{t("backHome")}</KumoLink>}
      />
    </div>
  );
}

/** ErrorState is the route-level fallback for a render error. `message` carries
 * the exception text, and reloading is the most direct way back. */
export function ErrorState({ message, onRetry }: { message?: string; onRetry?: () => void }) {
  const { t } = useI18n();
  return (
    <div className="flex min-h-[50vh] items-center justify-center">
      <Empty
        icon={<WarningCircleIcon size={48} className="text-kumo-danger" />}
        title={t("errorTitle")}
        description={message}
        contents={
          <Button variant="secondary" onClick={onRetry ?? (() => window.location.reload())}>
            {t("reload")}
          </Button>
        }
      />
    </div>
  );
}

/**
 * Forbidden answers "this page belongs to another role", in place.
 *
 * It lives here rather than in an application because both consumers of the
 * shared feature packages need it, and it is one line of text with no coupling
 * to the shell. Leaving it in an application would mean injecting it through the
 * host contract, which is a steep price for a single empty state.
 *
 * Its sibling "organisation not found" deliberately did not move: that one
 * carries a link back to a list, and where "back" points is exactly the kind of
 * thing only the shell knows, so it stays in the host contract.
 */
export function Forbidden() {
  const { t } = useI18n();
  return (
    <div className="py-6">
      <InlineEmpty title={t("forbiddenTitle")} description={t("forbiddenBody")} />
    </div>
  );
}
