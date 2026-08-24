// The gateway's console types on their own; see `gateway-console-api.ts` for
// why this is separate from the merged namespace.
//
// Types are erased at runtime and never reach the bundle, but they do enter the
// typecheck file graph: a build that does not contain a generated schema cannot
// compile a file that imports it, even for types only.
export type * from "./gen/gateway-console/endpoints.schemas";
