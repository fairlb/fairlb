import type { ComponentPropsWithoutRef } from "react";
import { cn } from "./cn";
import { VENDOR_MARK_URLS } from "./vendor-marks/registry";

// The monogram is lettering inside a fixed box, not body copy: each size is
// chosen so two glyphs fit the box above it, which is why these sit below the
// 14px content floor. That is the decorative case the size rule names.
const sizes = {
  sm: { box: "size-6", text: "text-[0.625rem]" },
  base: { box: "size-8", text: "text-xs" }, // ui-ignore: monogram, sized by its box
  lg: { box: "size-10", text: "text-sm" }, // ui-ignore: monogram, sized by its box
} as const;

// Where the mechanical rule below collides, the catalog decides. Which of two
// vendors keeps the plain form is not something a reader could infer, so it is
// written down instead of being buried in a tie-break.
const MONOGRAM_EXCEPTIONS: Record<string, string> = {
  // openai and openrouter both reduce to OP; OR is what OpenRouter is called.
  openrouter: "OR",
  // mistral and minimax both reduce to MI; MiniMax is two words.
  minimax: "MM",
};

/**
 * The two letters shown when a vendor has no licensed artwork.
 *
 * Derived from the slug, not the label: five catalog labels lead with Chinese
 * (阿里云百炼, 智谱 AI, 火山方舟, 百度千帆, 腾讯混元), so a label-derived
 * monogram would mix scripts down a single column and fall out of the display
 * face, which ships Latin only. Two letters rather than one because eleven of
 * the nineteen catalog vendors share a first letter with a sibling — a tile
 * that cannot tell openai from openrouter is not doing an icon's job.
 */
export function vendorMonogram(id: string): string {
  const exception = MONOGRAM_EXCEPTIONS[id];
  if (exception) return exception;
  const [first = "", second] = id.split("-").filter(Boolean);
  const letters = second ? `${first.slice(0, 1)}${second.slice(0, 1)}` : first.slice(0, 2);
  return letters.toUpperCase() || "?";
}

export type VendorMarkProps = Omit<ComponentPropsWithoutRef<"span">, "children"> & {
  id: string;
  label?: string;
  size?: keyof typeof sizes;
};

/** A licensed vendor mark when present, otherwise the stable zero-risk monogram tile. */
export function VendorMark({
  id,
  label,
  size = "base",
  className,
  role,
  "aria-label": ariaLabel,
  ...props
}: VendorMarkProps) {
  const markUrl = VENDOR_MARK_URLS[id];
  const classes = cn("inline-flex shrink-0 items-center justify-center", sizes[size].box);

  return (
    <span
      {...props}
      className={cn(classes, className)}
      role={role ?? "img"}
      aria-label={ariaLabel ?? label ?? id}
    >
      {markUrl ? (
        <img className="size-full object-contain" src={markUrl} alt="" aria-hidden="true" />
      ) : (
        <span
          className={cn(
            // Two glyphs centred in a 24-40px box: the default tracking pushes them
            // against the rounded edge at the small size.
            "flb-display flex size-full items-center justify-center rounded-[22%] bg-[var(--flb-route)] font-semibold tracking-tight text-white", // ui-ignore: monogram letterforms, see above
            sizes[size].text,
          )}
          aria-hidden="true"
        >
          {vendorMonogram(id)}
        </span>
      )}
    </span>
  );
}
