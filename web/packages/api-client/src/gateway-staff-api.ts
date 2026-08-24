// The gateway's admin hooks on their own; see `gateway-console-api.ts` for why
// this is separate from the merged namespace.
//
// Note the split against `staff-api.ts`: that one is what an admin app shell
// needs (identity plus gateway), this one is what the shared feature packages
// need (gateway only — they should not know about any identity segment).
export * from "./gen/gateway-staff/endpoints";
