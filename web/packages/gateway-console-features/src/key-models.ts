import { gatewayConsoleApi } from "@fairlb/api-client";

// **A module of its own, deliberately not sharing a file with key-widgets**: a hook
// can only be imported statically, never lazily, while the spend curve next door
// drags a chart library behind it. Sharing a file would pull that chart into the
// entry chunk of whichever app imports the hook — and a bundle-size gate watches
// that chunk.
/**
 * Feeds the options of the "allowed models" list.
 *
 * Fetches only while the drawer is actually open: it feeds that one dropdown, and
 * fetching unconditionally meant pulling the whole catalog on every visit to the
 * keys page, drawer open or not.
 *
 * Returns a bare array of strings rather than a query result: **the caller should
 * not inherit this package's fetching state**, it only needs "which models can be
 * picked". A failed fetch is an empty array, and the field then reads as having no
 * models to offer.
 */
export function useKeyModelOptions(orgId: string, enabled: boolean): string[] {
  const models = gatewayConsoleApi.useListAvailableModels(orgId, {
    query: { enabled },
  });
  return (models.data?.items ?? []).map((m) => m.slug);
}
