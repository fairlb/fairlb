import { createContext, useContext, type ReactNode } from "react";

/**
 * The settings surface is one feature rendered by two operator consoles, each
 * with its own generated API client. The host contract is the narrow seam: the
 * two hooks the forms need, how an error becomes a sentence, and which keys or
 * sections are edited on a dedicated page elsewhere in that app (ADR-0198).
 *
 * The entry shape is declared here structurally rather than imported from
 * either client: both generated `SettingEntry` types satisfy it, and the
 * package must not depend on the hosted product's client.
 */
export type SettingKind =
  | "string"
  | "enum"
  | "string_list"
  | "email"
  | "bool"
  | "int"
  | "decimal_string"
  | "map_string_int"
  | "money"
  | "secret";

export interface SettingEntry {
  key: string;
  kind: SettingKind;
  section?: string;
  hint?: string;
  description?: string;
  description_key?: string;
  group: string;
  impact: "normal" | "high";
  enum?: string[];
  min?: number;
  max?: number;
  default?: unknown;
  value?: unknown;
  set: boolean;
  updated_at?: string;
  updated_by?: string;
}

export interface SettingsBatch {
  changes: Array<{ key: string; value: unknown }>;
  reason?: string;
}

/** The slice of a react-query result the forms read. */
export interface ListSettingsQuery {
  data?: { items: SettingEntry[] };
  isError: boolean;
  isPending: boolean;
  error: unknown;
  refetch: () => unknown;
}

/** The slice of a react-query mutation the forms drive. */
export interface PutSettingsMutation {
  mutate: (variables: { data: SettingsBatch }, options?: { onSuccess?: () => void }) => void;
  isPending: boolean;
  isError: boolean;
  error: unknown;
  reset: () => void;
}

/** A key or section that has its own page: the registry page renders a pointer. */
export interface DedicatedPage {
  href: string;
  labelKey: string;
}

export interface SettingsHost {
  useListSettings: () => ListSettingsQuery;
  usePutSettings: () => PutSettingsMutation;
  errorMessage: (error: unknown) => string;
  /** Keys edited elsewhere (e.g. the kill switch on the health page). */
  dedicatedPages?: Record<string, DedicatedPage>;
  /** Whole sections edited elsewhere (e.g. payment channels on an integrations page). */
  dedicatedSections?: Record<string, DedicatedPage>;
}

const SettingsHostContext = createContext<SettingsHost | null>(null);

export function SettingsHostProvider({
  host,
  children,
}: {
  host: SettingsHost;
  children: ReactNode;
}) {
  return <SettingsHostContext.Provider value={host}>{children}</SettingsHostContext.Provider>;
}

export function useSettingsHost(): SettingsHost {
  const host = useContext(SettingsHostContext);
  if (!host) {
    throw new Error("settings-features: no SettingsHostProvider above this tree");
  }
  return host;
}
