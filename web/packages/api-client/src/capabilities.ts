/**
 * The organization capability vocabulary — a leaf module that imports nothing,
 * not even generated types.
 *
 * It lives on its own to break a real evaluation cycle: the navigation table is
 * assembled from a base registration plus per-feature injection, and injected
 * destinations declare capabilities too, so navigation and feature pages would
 * import each other. ESM does not reject such a cycle; it turns into a
 * `ReferenceError` when the exported const is still in its temporal dead zone,
 * and only at runtime. It fails in both directions, because whichever side is
 * evaluated first leaves the other side's const not yet executed.
 *
 * The vocabulary really is a leaf concept: it is the capability matrix the
 * server returns for the current session, and has nothing to do with how a
 * sidebar is ordered.
 */

/**
 * The capability vocabulary the server reports for the current session.
 * Permission-sensitive entry points must branch on these, never re-derive
 * permission from a role name.
 */
export const ORG_CAPABILITIES = {
  creditHealthRead: "credit_health.read",
  financeDetailsRead: "finance.details.read",
  financeManage: "finance.manage",
  keysManage: "keys.manage",
  webhooksManage: "webhooks.manage",
  membersManage: "members.manage",
} as const;

/**
 * The literal union of capability names, defined here rather than imported from
 * a generated schema. This module is a transitive dependency of the feature
 * packages every build shares, and importing a generated schema would pull that
 * whole contract into their typecheck file graph.
 *
 * The obvious risk of a hand-kept copy is that it drifts from the contract, so
 * a test pins the two together in both directions wherever the generated schema
 * is available.
 */
export type OrgCapability = (typeof ORG_CAPABILITIES)[keyof typeof ORG_CAPABILITIES];

export type CapabilityOrg = { capabilities?: readonly string[] };

/** Returns the empty set when the field is missing or malformed, so that
 * permission-sensitive entry points fail closed. */
export function orgCapabilities(org: CapabilityOrg): readonly string[] {
  return Array.isArray(org.capabilities) ? org.capabilities : [];
}

export function hasOrgCapability(org: CapabilityOrg, capability: OrgCapability): boolean {
  return orgCapabilities(org).includes(capability);
}
