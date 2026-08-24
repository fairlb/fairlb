// The gateway's console hooks on their own, without any host application's
// contract merged in.
//
// The shared feature packages import this rather than the merged console
// namespace: importing the merged one would drag a whole host contract into
// their module graph, and those packages are meant to compile in any build.
//
// Same reasoning and same shape as `staff-api.ts`: give each consumer the
// half it actually needs.
export * from "./gen/gateway-console/endpoints";
