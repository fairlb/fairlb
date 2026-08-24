import { expect, test } from "vitest";
import { isPathUnder, resolveNavValue } from "./nav-path";

/**
 * The two failures the hand-written chains had, stated as cases.
 *
 * Six of the eight navigations decided their current item with
 * `pathname.endsWith("/models") ? "models" : … : "overview"`. Both of the cases
 * below fail under that form, which is what makes them the interesting ones:
 * the first because `endsWith` does not know where a segment ends, the second
 * because the chain's fallback is its first item.
 */

const PROVIDER = [
  { value: "overview", href: "/gateway/providers/p1" },
  { value: "models", href: "/gateway/providers/p1/models" },
  { value: "keys", href: "/gateway/providers/p1/keys" },
  { value: "settings", href: "/gateway/providers/p1/settings" },
];

test("the most specific matching item wins, not the first prefix", () => {
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1")).toBe("overview");
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1/models")).toBe("models");
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1/settings")).toBe("settings");
});

test("a route nested below an item still marks that item, not the overview", () => {
  // The chain answered "overview" here, because none of its `endsWith` arms
  // matched and the fallback is the first aspect.
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1/keys/k9")).toBe("keys");
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1/models/m1/edit")).toBe("models");
});

test("a sibling whose name merely starts the same does not match", () => {
  // `endsWith("/settings")` is false here, but a bare `startsWith` on the href
  // would be true — and this is the shape that made the sidebars wrong before.
  const billing = [
    { value: "overview", href: "/orgs/o1/billing" },
    { value: "settings", href: "/orgs/o1/billing/settings" },
  ];
  expect(resolveNavValue(billing, "/orgs/o1/billing/settings-archive")).toBe("overview");
  expect(resolveNavValue(billing, "/orgs/o1/billing/settings")).toBe("settings");
  expect(isPathUnder("/orgs/o1/billing/settings-archive", "/orgs/o1/billing/settings")).toBe(false);
});

test("a query string or hash is not part of the path", () => {
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1/keys?cursor=2")).toBe("keys");
  expect(resolveNavValue(PROVIDER, "/gateway/providers/p1/models#top")).toBe("models");
});

test("with nothing matching, the first item is current", () => {
  // The parent layout is mounted, so one of its aspects is on screen; marking
  // none of them is a worse answer than marking the default.
  expect(resolveNavValue(PROVIDER, "/somewhere/else")).toBe("overview");
  expect(resolveNavValue([], "/anything")).toBe("");
});

test("a trailing slash on an item href does not shift the comparison", () => {
  const items = [
    { value: "overview", href: "/account/" },
    { value: "security", href: "/account/security/" },
  ];
  expect(resolveNavValue(items, "/account")).toBe("overview");
  expect(resolveNavValue(items, "/account/security")).toBe("security");
});
