// orval: one target per OpenAPI document, each producing TanStack Query hooks.
// The generated files are committed, and a drift check compares a fresh run
// against them.
//
// There is one target per spec file rather than one per UI plane: the specs are
// split physically so that each can be shipped, reviewed and versioned on its
// own. Consumers never see that split — a namespace module merges the generated
// clients into one namespace per plane, so a UI import stays the same no matter
// which document an operation is defined in. The merge happens at a package
// boundary rather than in this config because the clients have to remain
// separable at file granularity.
//
// This config lists only the specs every build serves. A deployment that mounts
// additional routes keeps its own config next to its own package, so this file
// never names a document that is not next to it.
import { defineConfig } from "orval";

export default defineConfig({
  gatewayConsole: {
    input: "../../../api/gateway-console.yaml",
    output: {
      target: "./src/gen/gateway-console/endpoints.ts",
      mode: "split",
      client: "react-query",
      httpClient: "fetch",
      baseUrl: "/api/v1",
      clean: true,
      override: {
        mutator: { path: "./src/mutator.ts", name: "customFetch" },
        fetch: { includeHttpResponseReturnType: false },
      },
    },
  },
  gatewayStaff: {
    input: "../../../api/gateway-staff.yaml",
    output: {
      target: "./src/gen/gateway-staff/endpoints.ts",
      mode: "split",
      client: "react-query",
      httpClient: "fetch",
      baseUrl: "/api/staff/v1",
      clean: true,
      override: {
        mutator: { path: "./src/mutator.ts", name: "customFetch" },
        fetch: { includeHttpResponseReturnType: false },
      },
    },
  },
  // The self-hosted admin plane's own identity segment: sign in, sign out and
  // "who am I". Its baseUrl is the same `/api/staff/v1` as gatewayStaff because
  // a self-hosted deployment serves everything from one host and separates the
  // planes by path, so these operations are mounted under the same prefix as
  // the gateway's own admin operations.
  staff: {
    input: "../../../api/staff.yaml",
    output: {
      target: "./src/gen/staff/endpoints.ts",
      mode: "split",
      client: "react-query",
      httpClient: "fetch",
      baseUrl: "/api/staff/v1",
      clean: true,
      override: {
        mutator: { path: "./src/mutator.ts", name: "customFetch" },
        fetch: { includeHttpResponseReturnType: false },
      },
    },
  },
});
