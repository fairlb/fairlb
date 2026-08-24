import { describe, expect, it } from "vitest";
import { previousRange, quantizedRange } from "./ranges";

describe("quantizedRange", () => {
  it("returns byte-identical bounds for two moments in the same clock hour", () => {
    // These two instants are 1.2 seconds apart — the scale of leaving a page and
    // coming back. Before snapping, they produced two different query keys, so
    // the cache could never be hit.
    const a = quantizedRange(24, new Date("2026-08-04T02:11:15.839Z"));
    const b = quantizedRange(24, new Date("2026-08-04T02:11:17.011Z"));
    expect(a).toEqual(b);
  });

  it("still moves when the clock crosses into the next hour", () => {
    // The counter-example: snapping must not pin the window down. Crossing into
    // the next hour has to move it, or new data never arrives.
    const a = quantizedRange(24, new Date("2026-08-04T02:59:59.999Z"));
    const b = quantizedRange(24, new Date("2026-08-04T03:00:00.000Z"));
    expect(a).not.toEqual(b);
  });

  it("keeps `to` strictly after now, including exactly on the hour", () => {
    // Rollups are bucketed by hour and the filter is `bucket_start < to`. If
    // `to` equalled the current hour, the bucket in progress would be excluded —
    // the symptom being "the request I just made is missing from the overview".
    // Exactly on the hour is the input most likely to collapse to equality, so
    // it is pinned separately.
    for (const iso of [
      "2026-08-04T02:00:00.000Z",
      "2026-08-04T02:00:00.001Z",
      "2026-08-04T02:30:00.000Z",
      "2026-08-04T02:59:59.999Z",
    ]) {
      const now = new Date(iso);
      const { to } = quantizedRange(24, now);
      expect(new Date(to).getTime()).toBeGreaterThan(now.getTime());
    }
  });

  it("preserves the requested window length exactly", () => {
    const now = new Date("2026-08-04T02:11:15.839Z");
    for (const hours of [24, 24 * 7, 24 * 30]) {
      const { from, to } = quantizedRange(hours, now);
      const spanHours = (new Date(to).getTime() - new Date(from).getTime()) / 3_600_000;
      expect(spanHours).toBe(hours);
    }
  });

  it("lands both bounds on an exact hour boundary", () => {
    const { from, to } = quantizedRange(24 * 7, new Date("2026-08-04T02:11:15.839Z"));
    for (const iso of [from, to]) {
      const d = new Date(iso);
      expect([d.getUTCMinutes(), d.getUTCSeconds(), d.getUTCMilliseconds()]).toEqual([0, 0, 0]);
    }
  });
});

describe("previousRange", () => {
  it("sits immediately before the current window and is the same length", () => {
    const now = new Date("2026-08-04T02:11:15.839Z");
    const cur = quantizedRange(24 * 7, now);
    const prev = previousRange(24 * 7, now);
    // Adjacent: the previous period ends exactly where the current one begins,
    // with neither a gap nor an overlap.
    expect(prev.to).toBe(cur.from);
    const len = (r: { from: string; to: string }) =>
      new Date(r.to).getTime() - new Date(r.from).getTime();
    expect(len(prev)).toBe(len(cur));
  });

  it("is quantized too, so both queries stay cacheable together", () => {
    const a = previousRange(24, new Date("2026-08-04T02:11:15.839Z"));
    const b = previousRange(24, new Date("2026-08-04T02:11:17.011Z"));
    expect(a).toEqual(b);
  });
});
