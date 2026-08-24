import type { GatewayConsoleTypes } from "@fairlb/api-client";

export type UsageRankMetric = "finance" | "requests";

/**
 * The ranking must be ordered by the metric currently on screen.
 *
 * The server orders its response too, but the client re-establishes the order
 * anyway: a stale cache or an error response must not leave "top requests" sitting
 * in amount order. The key breaks ties deterministically, so that equal values do
 * not leak the other dimension the upstream order happened to carry.
 */
export function rankUsageGroups(
  groups: readonly GatewayConsoleTypes.UsageGroup[],
  metric: UsageRankMetric,
): GatewayConsoleTypes.UsageGroup[] {
  const valueOf = (group: GatewayConsoleTypes.UsageGroup) =>
    metric === "finance" ? group.charged_nano : group.requests;

  return [...groups].sort((left, right) => {
    const leftValue = valueOf(left);
    const rightValue = valueOf(right);
    if (leftValue !== rightValue) return rightValue > leftValue ? 1 : -1;
    return left.key < right.key ? -1 : left.key > right.key ? 1 : 0;
  });
}
