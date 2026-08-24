import type { GatewayStaffTypes } from "@fairlb/api-client";
import { useI18n, type MessageKey } from "@fairlb/i18n";
import { StatusBadge } from "@fairlb/ui";

// `value` is the wire value and must stay lower case; `label` is for people and goes
// through the shared provider label.
// The probeable endpoints, grouped by protocol, mirroring the server's own mapping.
// The first of each protocol is its canonical endpoint, and therefore the default.
export type ProbeEndpoint = GatewayStaffTypes.TestGatewayProviderBodyEndpoint;
const PROBE_ENDPOINTS: Record<string, readonly ProbeEndpoint[]> = {
  openai: [
    "chat",
    "responses",
    "responses_compact",
    "responses_input_tokens",
    "embeddings",
    "images",
  ],
  anthropic: ["messages", "messages_count_tokens"],
  gemini: [
    "generate_content",
    "gemini_count_tokens",
    "gemini_embed_content",
    "gemini_batch_embed_contents",
    "gemini_interactions",
  ],
};

// A provider speaking several dialects can be probed on the union of their
// endpoints, with each protocol's canonical one kept first.
export const probeEndpointsFor = (protocols: readonly string[]): readonly ProbeEndpoint[] => {
  const out: ProbeEndpoint[] = [];
  for (const f of protocols) {
    for (const ep of PROBE_ENDPOINTS[f] ?? []) {
      if (!out.includes(ep)) out.push(ep);
    }
  }
  return out.length > 0 ? out : ["chat"];
};

// How a protocol is named on screen.
//
// Through i18n rather than a bare wire value, and worded so it cannot be read as
// a company: "openai" the protocol is spoken by dozens of platforms, and showing
// it as the brand name is what made a DeepSeek upstream display as "OpenAI".
export function protocolLabel(protocol: string): MessageKey {
  switch (protocol) {
    case "anthropic":
      return "gwProtocolAnthropic";
    case "openai":
      return "gwProtocolOpenai";
    case "gemini":
      return "gwProtocolGemini";
    default:
      return "gwProtocolUnknown";
  }
}

/** The protocol choices, translated. A hook because the labels are. */
export function useProtocolItems(): { value: string; label: string }[] {
  const { t } = useI18n();
  return [
    { value: "openai", label: t("gwProtocolOpenai") },
    { value: "anthropic", label: t("gwProtocolAnthropic") },
    { value: "gemini", label: t("gwProtocolGemini") },
  ];
}

/**
 * Maps a key's status enum onto a translated label.
 *
 * Rendering `{k.status}` directly puts a raw wire identifier on screen, untranslated
 * in every language.
 *
 * **Unknown values fall back to the raw string**: the contract's domain today is
 * active or disabled, and the day a third value is added this should surface it
 * rather than render blank — swallowing it would make a new status invisible in the
 * interface.
 */
export function keyStatusLabel(
  t: (key: "gwKeyStatus_active" | "gwKeyStatus_disabled") => string,
  status: string,
): string {
  if (status === "active") return t("gwKeyStatus_active");
  if (status === "disabled") return t("gwKeyStatus_disabled");
  return status;
}

// Three states, not two: "disabled by health checks" and "disabled by an operator"
// must look different, because the first re-enables itself once the provider
// recovers and the second never does. Someone reading "disabled" needs to know
// whether it will come back on its own.
export function ProviderStatusBadge({
  enabled,
  autoDisabled,
}: {
  enabled: boolean;
  autoDisabled: boolean;
}) {
  const { t } = useI18n();
  if (enabled) return <StatusBadge tone="success">{t("gwEnabled")}</StatusBadge>;
  if (autoDisabled) return <StatusBadge tone="warning">{t("gwAutoDisabled")}</StatusBadge>;
  return <StatusBadge tone="neutral">{t("gwManualDisabled")}</StatusBadge>;
}

// ===== The transport profile field =====
//
// Shared by the create dialog and the settings face: both offer the same JSON
// editor, one prefilled from a vendor preset and one from the stored profile,
// and two implementations of "is this text a profile" would eventually disagree
// about which text saves.

export type TransportProfile = Record<string, unknown>;

/** Shown when the field is empty: the shape, not an example to paste blindly. */
export const TRANSPORT_PLACEHOLDER = '{ "auth": "header:api-key" }';

/**
 * An empty profile renders as an empty field rather than as `{}`. "Nothing is
 * configured" and "an object that says nothing" are the same state for this
 * setting, and showing braces makes a provider that needs no profile look like
 * one whose profile was emptied on purpose.
 */
/**
 * The placeholder a preset left behind, or "" when the profile is finished.
 *
 * Only the path-override values are examined, and `{model}` is excluded: the
 * gateway substitutes that one per request, so it is the placeholder that is
 * *meant* to survive. Scanning the raw JSON text instead would flag the object's
 * own braces and refuse every profile ever typed.
 *
 * The server refuses the same thing on save; this is here so the form says it
 * where the value is, rather than after a round trip.
 */
export function unfinishedPlaceholder(profile: TransportProfile | undefined): string {
  if (!profile) return "";
  for (const key of ["path_overrides", "stream_path_overrides"]) {
    const table = profile[key];
    if (typeof table !== "object" || table === null) continue;
    for (const dest of Object.values(table as Record<string, unknown>)) {
      if (typeof dest !== "string") continue;
      const left = dest.match(/\{[^}]*\}/g)?.find((token) => token !== "{model}");
      if (left) return left;
    }
  }
  return "";
}

export function transportToText(profile: TransportProfile | undefined): string {
  if (!profile || Object.keys(profile).length === 0) return "";
  return JSON.stringify(profile, null, 2);
}

/**
 * An empty field means the empty object, which is how a profile is cleared —
 * omitting the field would mean "leave it alone", and then there would be no way
 * to remove one.
 */
export function parseTransportText(
  text: string,
): { ok: true; value: TransportProfile } | { ok: false } {
  const trimmed = text.trim();
  if (trimmed === "") return { ok: true, value: {} };
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      return { ok: false };
    }
    return { ok: true, value: parsed as TransportProfile };
  } catch {
    return { ok: false };
  }
}

/**
 * Compared by value rather than by text: reformatting the JSON is not an edit,
 * and treating it as one would arm the unsaved-changes guard against a stray
 * keystroke that changed nothing.
 */
export function sameTransport(
  next: TransportProfile,
  current: TransportProfile | undefined,
): boolean {
  const currentKeys = current ? Object.keys(current).sort() : [];
  const nextKeys = Object.keys(next).sort();
  if (currentKeys.join() !== nextKeys.join()) return false;
  return nextKeys.every(
    (k) => JSON.stringify(next[k]) === JSON.stringify((current as TransportProfile)[k]),
  );
}
