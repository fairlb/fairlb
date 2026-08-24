import type { ComponentPropsWithoutRef, ReactNode } from "react";
import { cn } from "./cn";

export type ResponsiveResourceRowProps = Omit<ComponentPropsWithoutRef<"div">, "title"> & {
  /** The resource's primary identifier. A long name is allowed to wrap naturally
   * rather than squeezing out the actions. */
  title: ReactNode;
  /** Stable attributes such as a prefix, creation time or type. */
  description?: ReactNode;
  /** More volatile status or summary values, such as last use and totals. */
  metadata?: ReactNode;
  /** Takes its own line on a narrow screen and follows the information block on
   * a wide one, rather than being pushed to the far edge of the card. */
  actions?: ReactNode;
};

/**
 * A responsive row for the console's key resource lists. It keeps the
 * information density of a list while moving the actions after the information
 * on a narrow screen. Data meant for comparison across many dimensions should
 * still use the horizontally scrollable DataTable rather than being turned
 * mechanically into cards.
 */
export function ResponsiveResourceRow({
  title,
  description,
  metadata,
  actions,
  className,
  ...props
}: ResponsiveResourceRowProps) {
  return (
    <div
      className={cn(
        "flex min-w-0 flex-col gap-3 py-3 text-base sm:flex-row sm:flex-wrap sm:items-start sm:gap-4",
        className,
      )}
      data-slot="responsive-resource-row"
      {...props}
    >
      <div className="min-w-0 max-w-full space-y-1">
        <div className="break-words font-medium text-kumo-default">{title}</div>
        {description != null && <div className="break-words text-kumo-subtle">{description}</div>}
        {metadata != null && (
          <div className="break-words text-kumo-subtle tabular-nums">{metadata}</div>
        )}
      </div>
      {actions != null && (
        <div className="flex shrink-0 flex-wrap items-center gap-1">{actions}</div>
      )}
    </div>
  );
}
