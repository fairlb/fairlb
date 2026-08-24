import { Badge } from "@cloudflare/kumo/components/badge";
import type { ReactNode } from "react";

/** Status tone. Four levels cover every status enum in this codebase: fine,
 * broken, needs attention, and neutral. */
export type StatusTone = "success" | "danger" | "warning" | "neutral";

const VARIANT: Record<StatusTone, "success" | "error" | "warning" | "secondary"> = {
  success: "success",
  danger: "error",
  warning: "warning",
  neutral: "secondary",
};

/**
 * StatusBadge marks a status.
 *
 * Five hand-rolled implementations used to exist across two applications, in two
 * competing shapes: three rendered a pill, two rendered bare coloured text. The
 * same idea wore two faces inside one application, decided by which directory
 * the file happened to sit in.
 *
 * The colour tokens themselves were unified earlier; what was missing all along
 * was the component.
 */
export function StatusBadge({ tone, children }: { tone: StatusTone; children: ReactNode }) {
  return (
    <Badge
      variant={VARIANT[tone]}
      // Kumo 2.9's subtle warning text does not reach 4.5:1 in light mode.
      // Dot appearance keeps the orange warning cue, uses accessible default
      // text, and stays visually subordinate to filled danger badges.
      appearance={tone === "warning" ? "dot" : "filled"}
    >
      {children}
    </Badge>
  );
}
