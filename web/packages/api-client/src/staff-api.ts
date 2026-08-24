// Every hook the self-hosted admin app needs: its own identity operations plus
// the gateway's admin operations.
//
// This is a separate namespace from the other admin plane rather than the same
// one. Both share the gateway half and then attach a different identity segment
// on top — this one is sign in, sign out and "who am I".
//
// Merging them would compile, but it would put hooks in reach of an app whose
// server does not mount those routes: autocomplete would offer operations that
// answer 404 at runtime, and the app would pull in a contract it has no use
// for. Keeping the namespaces apart is what makes each app's surface exactly
// what its server actually serves.
export * from "./gen/staff/endpoints";
export * from "./gen/gateway-staff/endpoints";
