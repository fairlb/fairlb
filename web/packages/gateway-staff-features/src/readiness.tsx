import { useI18n } from "@fairlb/i18n";
import { Card, SectionHeading } from "@fairlb/ui";
import type { ReactNode } from "react";

type ReadinessStep = {
  /** The step's text. A step you can act on is given as a real link by the caller. */
  label: ReactNode;
  key: string;
  done: boolean;
};

/**
 * The shared "what is still missing before this can serve traffic" checklist.
 *
 * Two rules are expressed in the type. Each step's `done` is derived by the caller
 * from **the same data the server gates on** — a checklist that reads all green
 * while the thing still refuses to start is worse than no checklist. And a step you
 * can act on is given as a real link, because a visible step you cannot reach just
 * makes the reader go hunting for the page themselves.
 *
 * When every step is done the whole thing stops rendering: it is a getting-started
 * guide, not a permanent dashboard.
 */
export function ReadinessSteps({
  title,
  steps,
  className,
  headingAs = "h2",
}: {
  title: ReactNode;
  steps: ReadinessStep[];
  className?: string;
  headingAs?: "h2" | "h3";
}) {
  const { t } = useI18n();
  if (steps.every((step) => step.done)) return null;

  return (
    <Card className={className ? `space-y-4 ${className}` : "space-y-4"}>
      <SectionHeading as={headingAs}>{title}</SectionHeading>
      <ol className="divide-y divide-kumo-line">
        {steps.map((step, index) => (
          <li key={step.key} className="flex items-center gap-3 py-3 first:pt-0 last:pb-0">
            <span
              aria-hidden="true"
              className={`grid size-7 shrink-0 place-items-center rounded-full border text-base font-semibold ${
                step.done
                  ? "border-kumo-success bg-kumo-success-tint text-kumo-success"
                  : "border-kumo-line bg-kumo-recessed text-kumo-subtle"
              }`}
            >
              {step.done ? "✓" : index + 1}
            </span>
            <span className={step.done ? "text-kumo-default" : "text-kumo-subtle"}>
              {step.label}
            </span>
            <span className="ml-auto text-base text-kumo-subtle">
              {step.done ? t("gwChecklistDone") : t("gwChecklistPending")}
            </span>
          </li>
        ))}
      </ol>
    </Card>
  );
}
