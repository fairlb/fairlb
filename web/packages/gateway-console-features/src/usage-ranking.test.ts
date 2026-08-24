import { describe, expect, it } from "vitest";
import { rankUsageGroups } from "./usage-ranking";

const groups = [
  {
    key: "model/high-spend",
    requests: 1,
    tokens_in: 1,
    tokens_out: 1,
    charged_nano: 900,
  },
  {
    key: "model/z-busy",
    requests: 20,
    tokens_in: 20,
    tokens_out: 20,
    charged_nano: 100,
  },
  {
    key: "model/a-busy",
    requests: 20,
    tokens_in: 20,
    tokens_out: 20,
    charged_nano: 50,
  },
];

describe("rankUsageGroups", () => {
  it("ranks a non-financial view by requests with a deterministic key tie-break", () => {
    expect(rankUsageGroups(groups, "requests").map((group) => group.key)).toEqual([
      "model/a-busy",
      "model/z-busy",
      "model/high-spend",
    ]);
  });

  it("keeps the financial view ranked by charge", () => {
    expect(rankUsageGroups(groups, "finance").map((group) => group.key)).toEqual([
      "model/high-spend",
      "model/z-busy",
      "model/a-busy",
    ]);
  });

  it("does not mutate cached API data", () => {
    const original = groups.map((group) => group.key);
    rankUsageGroups(groups, "requests");
    expect(groups.map((group) => group.key)).toEqual(original);
  });
});
