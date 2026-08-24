export * from "./errors.gen";
export { ApiError, getResponseETag, type Problem } from "./mutator";
export { apiErrorMessage, apiErrorCode, apiErrorStatus } from "./error-message";
// The organization capability vocabulary. It belongs to this package because
// it is part of this API's vocabulary, and because both the app shell and the
// shared feature packages read it — keeping it in an app would make a feature
// package import an app.
export {
  ORG_CAPABILITIES,
  orgCapabilities,
  hasOrgCapability,
  type OrgCapability,
  type CapabilityOrg,
} from "./capabilities";
export { createAppQueryClient } from "./query-client";
// One hook namespace per UI plane. A plane can be assembled from more than one
// generated client; merging happens at a package boundary, so that the shape
// consumers see never depends on how the specs are split, while the clients
// themselves stay separable at file granularity.
//
// This package holds the segments every build serves. A deployment that mounts
// additional routes brings its own package, depends on this one, and merges the
// two halves there — the direction is one-way, and nothing here knows who is
// stacked on top.
//
// The self-hosted admin plane gets its own namespace rather than being folded
// into a shared one: both admin planes share the gateway operations and then
// attach a different identity segment each. See `staff-api.ts` for why the
// two are kept apart.
//
// The feature packages shared by every build see the gateway operations only.
// They must not depend on any one deployment's contract, and importing a merged
// namespace would pull that contract in through the module graph — types are
// erased at runtime but they still enter the typecheck file graph.
export * as gatewayConsoleApi from "./gateway-console-api";
export * as gatewayStaffApi from "./gateway-staff-api";
export type * as GatewayConsoleTypes from "./gateway-console-types";
export type * as GatewayStaffTypes from "./gateway-staff-types";

export * as communityStaffApi from "./staff-api";
export type * as CommunityStaffTypes from "./staff-types";
