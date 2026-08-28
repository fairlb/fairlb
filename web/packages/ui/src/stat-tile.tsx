import { Tooltip } from "@cloudflare/kumo/components/tooltip";
import { InfoIcon } from "@phosphor-icons/react";

/**
 * StatTile is a single headline number.
 *
 * A current value with an optional note, rendered as a block of type rather than
 * as a bar chart with one bar — the latter draws a single number as a chart and
 * makes the reader decode an axis to get it back. Three separate versions
 * existed before this, at three different type sizes, one of which brought its
 * own card wrapper.
 */
export function StatTile({
  label,
  value,
  hint,
  hintPresentation = "inline",
  delta,
}: {
  label: string;
  value: string;
  hint?: string;
  /**
   * `inline` suits supporting information that must always be present.
   * `tooltip` suits a precise definition on a dashboard meant for scanning: the
   * definition stays next to the number and remains keyboard-reachable without
   * six explanations competing for attention at once.
   */
  hintPresentation?: "inline" | "tooltip";
  delta?: StatDelta;
}) {
  return (
    // A ring rather than a border, matching the Card's container edge: a border
    // takes up layout width, so side by side the two would not line up.
    <div className="rounded-xl px-5 py-4 ring ring-kumo-line">
      {/* In this design system's scale the base size is 14px — the body size the
          rule asks for — while the larger step is the display number. */}
      {/* The label reserves two lines (`2lh` is twice the line height, so it
          scales with the type size instead of a hard-coded pixel value).
          A row of tiles is a grid and the numbers have to share a baseline, but
          labels vary in length, and one that wraps drops its number a whole line
          below its neighbours, which reads as though that cell were missing.
          The cost is around 20px of extra height on tiles with a single-line
          label; what it buys is an aligned row. */}
      <div className="flex min-h-[2lh] items-start gap-1 text-base text-kumo-subtle">
        <span>{label}</span>
        {hint && hintPresentation === "tooltip" && (
          <Tooltip
            content={<span className="block max-w-64 leading-5">{hint}</span>}
            side="bottom"
            align="start"
            delay={200}
            render={
              <button
                type="button"
                aria-label={`${label}: ${hint}`}
                // No transition on the hover colour: a colour change on a pointer-speed
                // interaction has to be immediate, and the only animated property here
                // was that colour — so the reduced-motion escape hatch went with it.
                className="inline-flex size-5 shrink-0 items-center justify-center rounded-full text-kumo-subtle hover:text-kumo-default focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-kumo-focus"
              />
            }
          >
            <InfoIcon size={14} aria-hidden />
          </Tooltip>
        )}
      </div>
      <div className="mt-1 flex flex-wrap items-baseline gap-2">
        <span className="text-xl font-semibold">{value}</span>
        {delta && <Delta {...delta} />}
      </div>
      {hint && hintPresentation === "inline" && (
        <div className="mt-1 text-base text-kumo-subtle">{hint}</div>
      )}
    </div>
  );
}

/**
 * StatDelta is the comparison against the previous period.
 *
 * A number on its own cannot answer "is that a lot?" — is 1.1345 up or down? A
 * tile without a baseline is just a figure; an overview page once had six tiles
 * and not one of them carried a comparison.
 *
 * `higherIsWorse` decides which direction is coloured: spend going up is not bad
 * news, it means the business is growing, whereas errors going up is. The
 * direction belongs to the caller rather than an assumption here that up is
 * good.
 */
export type StatDelta = {
  /** Rate of change against the previous period, where 1 means +100%. Pass
   * undefined when the previous period was zero. */
  ratio?: number;
  /** Text shown when there is no baseline to compare against, such as "no data
   * for the previous period". */
  emptyLabel: string;
  higherIsWorse?: boolean;
};

function Delta({ ratio, emptyLabel, higherIsWorse }: StatDelta) {
  if (ratio === undefined) {
    return <span className="text-base text-kumo-inactive">{emptyLabel}</span>;
  }
  const pct = Math.round(ratio * 100);
  // When it rounds to zero, say "unchanged" rather than "+0%": the latter looks
  // like a precise measurement when it is in fact the bucket that both -0.4% and
  // +0.4% fall into.
  if (pct === 0) return <span className="text-base text-kumo-subtle">±0%</span>;
  const bad = higherIsWorse ? pct > 0 : false;
  return (
    <span className={bad ? "text-base text-kumo-danger" : "text-base text-kumo-subtle"}>
      {pct > 0 ? "+" : ""}
      {pct}%
    </span>
  );
}
