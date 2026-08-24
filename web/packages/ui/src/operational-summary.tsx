import type { ReactNode } from "react";
import { cn } from "./cn";

export type OperationalSummaryItem = {
  label: string;
  value: ReactNode;
  detail?: ReactNode;
  tone?: "healthy" | "degraded" | "neutral";
};

export type OperationalSummaryProps = {
  label: string;
  items: readonly OperationalSummaryItem[];
  className?: string;
};

const dots = {
  healthy: "bg-[var(--flb-healthy)]",
  degraded: "bg-[var(--flb-degraded)]",
  neutral: "bg-kumo-line",
} as const;

/** Scan-first status band. It deliberately is not a row of equal-weight cards. */
export function OperationalSummary({ label, items, className }: OperationalSummaryProps) {
  return (
    <section
      aria-label={label}
      className={cn(
        "grid overflow-hidden rounded-xl border border-kumo-line bg-kumo-base sm:grid-cols-2 xl:grid-cols-4",
        className,
      )}
    >
      {items.map((item) => (
        <div
          className="min-w-0 border-b border-kumo-line p-4 last:border-b-0 sm:odd:border-r xl:border-r xl:border-b-0 xl:last:border-r-0"
          key={item.label}
        >
          <div className="flex items-center gap-2 text-base text-kumo-subtle">
            <span
              className={cn("size-2 shrink-0 rounded-full", dots[item.tone ?? "neutral"])}
              aria-hidden="true"
            />
            <span>{item.label}</span>
          </div>
          <div className="mt-1 break-words text-xl font-semibold text-kumo-default tabular-nums">
            {item.value}
          </div>
          {item.detail != null && (
            <div className="mt-1 break-words text-base text-kumo-subtle">{item.detail}</div>
          )}
        </div>
      ))}
    </section>
  );
}
