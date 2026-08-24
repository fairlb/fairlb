import { useI18n } from "@fairlb/i18n";
import { Checkbox, DataTable, InlineEmpty, StatusBadge } from "@fairlb/ui";
import type { ReactNode } from "react";

/**
 * The shell both wiring dialogs render into.
 *
 * The two dialogs edit the same relation from its two ends -- a provider's
 * models, and a model's providers -- so they agree on the frame: a leading
 * checkbox column for intent, two identity columns, and a status column for
 * what is stored. What goes *inside* the two identity columns does not agree at
 * all (one side edits an upstream name in place, the other reviews a catalog
 * entry it is about to create), which is why this is a shell with a row
 * renderer rather than one table with a direction flag. A flag would put a
 * conditional in every cell and call the result shared.
 *
 * Measured before extracting: 110 of the two files' 1217 lines were byte-identical
 * in runs of three or more, and almost all of it was this frame.
 */
export function WiringTable({
  caption,
  columns,
  empty,
  rowCount,
  children,
}: {
  /** The table's accessible name, taken from the dialog title. */
  caption: string;
  /** The three named columns; the checkbox column is unnamed by design. */
  columns: readonly [string, string, string];
  empty: { title: string; description: string };
  rowCount: number;
  children: ReactNode;
}) {
  return (
    <DataTable caption={caption}>
      <DataTable.Header>
        <DataTable.Row>
          <DataTable.Head />
          {columns.map((column) => (
            <DataTable.Head key={column}>{column}</DataTable.Head>
          ))}
        </DataTable.Row>
      </DataTable.Header>
      <DataTable.Body>
        {children}
        {rowCount === 0 && (
          <DataTable.Row>
            <DataTable.Cell colSpan={columns.length + 1}>
              <InlineEmpty title={empty.title} description={empty.description} />
            </DataTable.Cell>
          </DataTable.Row>
        )}
      </DataTable.Body>
    </DataTable>
  );
}

/**
 * The intent checkbox: the left column on both sides.
 *
 * It is deliberately not the same thing as the status column beside it. This one
 * is what the operator wants; that one is what is stored. After a partial failure
 * the two visibly disagree, and that disagreement is precisely how "this one did
 * not take effect" is expressed -- so neither may be derived from the other.
 */
export function WiringIntentCell({
  checked,
  disabled,
  label,
  onToggle,
}: {
  checked: boolean;
  disabled: boolean;
  label: string;
  onToggle: (next: boolean) => void;
}) {
  return (
    <DataTable.Cell>
      <Checkbox
        checked={checked}
        disabled={disabled}
        onCheckedChange={(next) => onToggle(next === true)}
        aria-label={label}
      />
    </DataTable.Cell>
  );
}

/** Whether the stored route carries traffic. Identical on both sides. */
export function RouteStatusBadge({ enabled }: { enabled: boolean }) {
  const { t } = useI18n();
  return (
    <StatusBadge tone={enabled ? "success" : "neutral"}>
      {enabled ? t("gwDiscoverState_routed") : t("gwManualDisabled")}
    </StatusBadge>
  );
}
