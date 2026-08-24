import { gatewayStaffApi, type GatewayStaffTypes } from "@fairlb/api-client";

// The vendor registry, as this build ships it.
//
// It is read once and kept: the list is compiled into the server, so it cannot
// change while a page is open, and refetching it on every provider screen would
// spend a request on an answer that is already known.

export type Vendor = GatewayStaffTypes.GatewayVendor;

export function useVendors() {
  return gatewayStaffApi.useListGatewayVendors({
    query: { staleTime: Number.POSITIVE_INFINITY, gcTime: Number.POSITIVE_INFINITY },
  });
}

/**
 * The display name for a vendor slug, falling back to the slug itself.
 *
 * The fallback is what keeps a page readable while the registry query is still
 * in flight, and what keeps it readable at all if a provider carries a vendor
 * this build has never heard of. Showing the slug is ugly; showing nothing puts
 * a blank where an identity should be.
 */
export function vendorLabel(slug: string, vendors: readonly Vendor[] | undefined): string {
  return vendors?.find((v) => v.slug === slug)?.label ?? slug;
}

/** The registry entry for a slug, or undefined while the query is undecided. */
export function vendorBySlug(
  slug: string,
  vendors: readonly Vendor[] | undefined,
): Vendor | undefined {
  return vendors?.find((v) => v.slug === slug);
}

/** What a provider form starts as for a vendor. */
export type VendorPrefill = {
  slug: string;
  baseUrl: string;
  protocols: string[];
  transportText: string;
  /** True when the prefilled base URL still contains a placeholder to replace. */
  baseUrlNeedsEditing: boolean;
};

/**
 * Derives the starting state of the create form from a registry entry.
 *
 * A pure function, so what the form fills in is decided in one place and can be
 * asserted without rendering anything. `taken` are the slugs already in use: a
 * suggestion that collides would be refused on save, and the operator would be
 * looking at a field they did not type.
 */
export function prefillFromVendor(
  vendor: Vendor | undefined,
  taken: readonly string[],
  region?: string,
): VendorPrefill {
  if (!vendor)
    return { slug: "", baseUrl: "", protocols: [], transportText: "", baseUrlNeedsEditing: false };
  const chosen =
    vendor.base_urls.find((b) => (b.label ?? "") === (region ?? "")) ?? vendor.base_urls[0];
  return {
    slug: freeSlug(vendor.slug, taken),
    baseUrl: chosen?.url ?? "",
    protocols: [...vendor.default_protocols],
    transportText: vendor.transport ? JSON.stringify(vendor.transport, null, 2) : "",
    baseUrlNeedsEditing: chosen?.template === true,
  };
}

function freeSlug(base: string, taken: readonly string[]): string {
  if (!taken.includes(base)) return base;
  for (let n = 2; n < 100; n++) {
    const candidate = `${base}-${n}`;
    if (!taken.includes(candidate)) return candidate;
  }
  return base;
}
