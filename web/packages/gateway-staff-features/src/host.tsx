import { createContext, useContext, type ReactNode } from "react";
import type { PageHeaderBreadcrumbs } from "@fairlb/ui";

/**
 * The host contract: **the package defines the interface, every shell that mounts
 * these pages supplies one implementation**. It is the front-end counterpart of the
 * inverted extension points the server side uses.
 *
 * The discipline is the same one: the interface lives with the feature, and each
 * shell injects its own implementation at its assembly point without knowing about
 * any other. Identity and the destination registry are exactly the two things a
 * shell answers differently — a shell may authenticate its operators through an
 * entirely different mechanism and mount these pages under its own routes — so those
 * two **must** be injected rather than looked up from in here.
 *
 * ## Why the contract has only two members
 *
 * Five things were being reached for from the surrounding app before this became a
 * package. Four of them turned out not to depend on which shell was hosting, and
 * they went where they belonged instead of into the contract:
 *
 * | what it was | where it went | why it is not a contract member |
 * |---|---|---|
 * | error message and status helpers | `@fairlb/api-client` | the app was only re-exporting them |
 * | the document title hook | `@fairlb/ui` | the title convention and the app name are the same everywhere |
 * | the row title link | `@fairlb/ui` | pure presentation, and used by pages outside this package |
 * | the command-palette query predicate | `@fairlb/ui` | a pure predicate that every palette source must share |
 *
 * Cutting it there has a checkable benefit: **no existing test needs a new provider
 * wrapped around it**. The two remaining members are called only by detail pages,
 * whose tests already render inside the real authentication wrapper, so the provider
 * goes there. Had the four above been contract members too, every test that does not
 * use that wrapper would have gone red for a reason unrelated to what it tests.
 */
export interface GatewayStaffHost {
  /**
   * The role of the signed-in operator. **This one field is all the package
   * reads**: writes to pricing and to access tiers are gated on it being the
   * highest role.
   *
   * A shell with several operator roles reports the real one; a shell with a single
   * operator identity always reports the highest. That way "changing prices needs
   * the highest privilege" is the same line of code either way, rather than a branch
   * per shell.
   */
  useCurrentStaffRole(): string;
  /**
   * The breadcrumbs of the current page, or `undefined` when it has no ancestor.
   *
   * The ancestor is derived from the **shell's own destination registry**, and that
   * registry belongs to the shell — this package should not even know which path it
   * has been mounted under.
   */
  useRecordBreadcrumb(current: string): PageHeaderBreadcrumbs | undefined;
}

const HostContext = createContext<GatewayStaffHost | null>(null);

export function GatewayStaffHostProvider({
  host,
  children,
}: {
  host: GatewayStaffHost;
  children: ReactNode;
}) {
  return <HostContext.Provider value={host}>{children}</HostContext.Provider>;
}

function useHost(): GatewayStaffHost {
  const host = useContext(HostContext);
  // Nothing here means the shell forgot the provider, and half a rendered page is
  // harder to diagnose than a thrown error. **No default implementation on
  // purpose**: a fallback that reported the highest role would silently open every
  // privilege gate the day a provider goes missing.
  if (!host) {
    throw new Error("gateway staff features must be rendered inside <GatewayStaffHostProvider>");
  }
  return host;
}

export function useCurrentStaffRole(): string {
  return useHost().useCurrentStaffRole();
}

export function useRecordBreadcrumb(current: string): PageHeaderBreadcrumbs | undefined {
  return useHost().useRecordBreadcrumb(current);
}
