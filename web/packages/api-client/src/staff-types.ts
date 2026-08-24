// Every type the self-hosted admin app needs: its own identity segment plus the
// gateway's admin operations.
//
// `Problem` and `ProblemResponse` are the error envelope, and every spec
// carries its own copy — an error shape should be complete in each document on
// its own. The copies are identical, so one is re-exported explicitly to
// disambiguate. Which one wins does not matter; what matters is not leaving TS
// to choose between two exports of the same name.
export type * from "./gen/gateway-staff/endpoints.schemas";
export type { Problem, ProblemResponse } from "./gen/staff/endpoints.schemas";
export type * from "./gen/staff/endpoints.schemas";
