import { createContext, useContext, type FunctionComponent, type ReactNode } from "react";
import type { CapabilityOrg } from "@fairlb/api-client";

/**
 * Everything this package requires of "an organization": a stable id (React keys
 * and request parameters) plus a capability set.
 *
 * **Deliberately not a full organization summary** — that is the shape a shell
 * with a directory of organizations happens to have, and welding it into this
 * contract would force a single-organization shell to fabricate a structure it
 * does not have.
 */
export type HostOrg = CapabilityOrg & { id: string };

/**
 * The host contract: **the package defines the interface, every shell that mounts
 * these pages supplies one implementation** — same arrangement as the host.tsx of
 * `@fairlb/gateway-staff-features`.
 *
 * ## Why this contract is wider than the staff one
 *
 * The staff pages are not scoped to an organization and do no capability gating,
 * so their host only answers "who am I" and "what are the breadcrumbs". Every page
 * here **resolves an organization first and then gates on capabilities**, and both
 * answers differ per shell: a shell serving many organizations reads the one named
 * by the URL and gets its capabilities from the server, while a shell serving a
 * single organization has a constant one and an always-true gate. Baking either
 * answer in would hand the single-organization case a multi-tenant semantics it
 * does not have.
 *
 * Four more things were held to the same test — "does it still hold under a
 * different shell?" — and kept out: error-message formatting and the capability
 * vocabulary live in `@fairlb/api-client` (**the vocabulary is part of that API's
 * definition**), while time-range presets, the forbidden page and the settings
 * section belong to `@fairlb/ui` (a pure predicate and pure layout, used by pages
 * outside this package too).
 */
export interface GatewayConsoleHost {
  /**
   * Resolve the current organization from the route parameters; `undefined` when
   * it does not resolve, which makes the page render `OrgNotFound`.
   *
   * Only `capabilities` is required of the returned shape — capability decisions
   * are all the package reads from it.
   */
  useOrg(orgId: string): HostOrg | undefined;
  /**
   * Set document.title. **Only the page name is handed over**: which segments are
   * joined around it is the shell's business, and shells do not have the same
   * segments available.
   */
  useTitle(title: string | undefined): void;
  /**
   * Whether the current session is impersonating someone. Export actions switch
   * themselves off when it is — **data must not be carried away as a file while
   * acting as another user**. A shell without impersonation returns false.
   */
  useImpersonating(): boolean;
  /** The whole-page answer when no organization resolves, including exits such as
   * "back to the list" that only the shell knows the destination of. */
  OrgNotFound: FunctionComponent;
  /** Context handed down by the settings-area shell: the organization, and whether
   * the viewer may change anything. */
  useOrgSettings(): { org: HostOrg; canManage: boolean };
  /**
   * The option source of the "filter by key" dropdown (one on the usage page, one
   * on the logs page).
   *
   * **This is the only member where shells genuinely call different endpoints**:
   * the same list of keys is reachable on a different plane depending on the
   * identity model the shell uses, so no single path can be baked in here.
   *
   * The return shape is deliberately not a react-query result: how a shell fetches
   * is the shell's business.
   */
  useApiKeyOptions(orgId: string, enabled: boolean): ApiKeyOptions;
}

/** The three states plus the options the "filter by key" dropdown needs. */
export interface ApiKeyOptions {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  items: readonly { id: string; name: string }[];
}

const HostContext = createContext<GatewayConsoleHost | null>(null);

export function GatewayConsoleHostProvider({
  host,
  children,
}: {
  host: GatewayConsoleHost;
  children: ReactNode;
}) {
  return <HostContext.Provider value={host}>{children}</HostContext.Provider>;
}

function useHost(): GatewayConsoleHost {
  const host = useContext(HostContext);
  // Nothing here means the shell forgot the provider. **No default implementation
  // on purpose**: a fallback that quietly answered for the shell would turn a
  // missing provider into wrong pages rather than a loud failure.
  if (!host) {
    throw new Error(
      "gateway console features must be rendered inside <GatewayConsoleHostProvider>",
    );
  }
  return host;
}

export function useOrg(orgId: string): HostOrg | undefined {
  return useHost().useOrg(orgId);
}

export function useConsoleTitle(title: string | undefined): void {
  useHost().useTitle(title);
}

export function useImpersonating(): boolean {
  return useHost().useImpersonating();
}

export function OrgNotFound() {
  const { OrgNotFound: Page } = useHost();
  return <Page />;
}

export function useOrgSettings(): { org: HostOrg; canManage: boolean } {
  return useHost().useOrgSettings();
}

export function useApiKeyOptions(orgId: string, enabled: boolean): ApiKeyOptions {
  return useHost().useApiKeyOptions(orgId, enabled);
}
