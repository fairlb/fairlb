import { type MessageKey, useI18n } from "@fairlb/i18n";
import { Button, Input } from "@fairlb/ui";
import { Fragment } from "react";

// The header-mapping editor, shared by the provider level and the per-route level.
//
// The semantics of the injection layer decide this editor's shape:
//   1. an empty value **is not** "left blank", it means "remove this header" — so
//      empty-valued rows must be savable, and cannot be dropped as defaults the way
//      forms usually drop them;
//   2. `${api_key}` inside a value is replaced with the decrypted provider key, so
//      the key itself never has to be written into the configuration in the clear;
//   3. the route level overrides the provider level key for key, and HTTP header
//      names are case-insensitive.
//
// Editing state is an array rather than an object: in an object, rows whose key is
// still half-typed collide with one another — two rows both momentarily keyed `""`
// collapse into one — and the input focus jumps away with them.

export type HeaderRow = { id: number; k: string; v: string };

let seq = 0;

/** A fresh empty row. The id exists only as a React key and never leaves the client. */
function newHeaderRow(): HeaderRow {
  seq += 1;
  return { id: seq, k: "", v: "" };
}

export function rowsFromMap(m?: Record<string, string> | null): HeaderRow[] {
  return Object.entries(m ?? {}).map(([k, v]) => {
    seq += 1;
    return { id: seq, k, v };
  });
}

/** Folds the rows into a request body. A row whose key is blank once trimmed counts
 * as unfinished and is dropped whole. */
export function mapFromRows(rows: HeaderRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const k = r.k.trim();
    if (k === "") continue;
    // Values are kept verbatim, never trimmed: trimming would quietly turn "typed
    // two spaces" into an instruction to remove the header, and those two have
    // wildly different consequences. Removing a header requires an explicitly empty
    // value.
    out[k] = r.v;
  }
  return out;
}

/**
 * The reason saving is blocked, as a message key, or undefined when nothing blocks it.
 *
 * Duplicate keys are compared case-insensitively, because header names are
 * normalized on the way out: `x-title` and `X-Title` become the same header on the
 * outbound request, and the later one silently overwrites the earlier.
 */
export function headerRowsError(rows: HeaderRow[]): MessageKey | undefined {
  const seen = new Set<string>();
  for (const r of rows) {
    const k = r.k.trim();
    if (k === "") {
      if (r.v !== "") return "gwHdrKeyRequired"; // a value with no key is a half-written row
      continue;
    }
    const norm = k.toLowerCase();
    if (seen.has(norm)) return "gwHdrDupKey";
    seen.add(norm);
  }
  return undefined;
}

/** Whether the edited result matches what the server already holds, which is what
 * disables the save button. */
export function sameHeaderMap(
  a: Record<string, string>,
  b?: Record<string, string> | null,
): boolean {
  const other = b ?? {};
  const ka = Object.keys(a);
  const kb = Object.keys(other);
  if (ka.length !== kb.length) return false;
  return ka.every((k) => Object.prototype.hasOwnProperty.call(other, k) && other[k] === a[k]);
}

export function HeaderMapEditor({
  rows,
  onChange,
  idPrefix,
  disabled,
}: {
  rows: HeaderRow[];
  onChange: (next: HeaderRow[]) => void;
  idPrefix: string;
  disabled?: boolean;
}) {
  const { t } = useI18n();
  const patch = (id: number, part: Partial<HeaderRow>) =>
    onChange(rows.map((r) => (r.id === id ? { ...r, ...part } : r)));
  const invalidKeyIds = new Set<number>();
  const seenKeys = new Set<string>();
  for (const row of rows) {
    const key = row.k.trim();
    if (key === "") {
      if (row.v !== "") invalidKeyIds.add(row.id);
      continue;
    }
    const normalized = key.toLowerCase();
    if (seenKeys.has(normalized)) invalidKeyIds.add(row.id);
    seenKeys.add(normalized);
  }

  return (
    <div className="space-y-2">
      {rows.length > 0 && (
        <div className="grid items-center gap-2 sm:grid-cols-[1fr_1fr_auto]">
          <span className="text-base font-medium text-kumo-default">{t("gwHdrKey")}</span>
          <span className="text-base font-medium text-kumo-default">{t("gwHdrValue")}</span>
          <span />
          {rows.map((r) => (
            <Fragment key={r.id}>
              <Input
                id={`${idPrefix}-hk-${r.id}`}
                aria-label={t("gwHdrKey")}
                aria-invalid={invalidKeyIds.has(r.id) || undefined}
                value={r.k}
                disabled={disabled}
                placeholder="X-Title"
                onChange={(e) => patch(r.id, { k: e.target.value })}
              />
              <Input
                id={`${idPrefix}-hv-${r.id}`}
                aria-label={t("gwHdrValue")}
                value={r.v}
                disabled={disabled}
                placeholder="${api_key}"
                onChange={(e) => patch(r.id, { v: e.target.value })}
              />
              <Button
                size="sm"
                variant="secondary-destructive"
                disabled={disabled}
                onClick={() => onChange(rows.filter((x) => x.id !== r.id))}
              >
                {t("gwDelete")}
              </Button>
            </Fragment>
          ))}
        </div>
      )}
      {rows.length === 0 && <p className="text-base text-kumo-subtle">{t("gwHdrNone")}</p>}
      <Button
        size="sm"
        variant="outline"
        disabled={disabled}
        onClick={() => onChange([...rows, newHeaderRow()])}
      >
        {t("gwHdrAddRow")}
      </Button>
    </div>
  );
}
