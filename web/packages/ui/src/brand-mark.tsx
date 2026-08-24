import { BRAND_MARK_URL, BRAND_NAME, type BrandEdition } from "@fairlb/brand";
import type { ComponentPropsWithoutRef } from "react";
import { cn } from "./cn";

export type BrandMarkProps = ComponentPropsWithoutRef<"span"> & {
  edition?: BrandEdition;
  name?: string;
  showName?: boolean;
  size?: "sm" | "base" | "lg";
};

const sizes = {
  sm: "size-5",
  base: "size-7",
  lg: "size-9",
} as const;

/** The single product signature used by every FairLB surface.
 *
 * With `showName={false}` the mark is purely decorative: the caller renders the
 * visible name beside it and owns the accessible name. The wrapper then carries
 * no `aria-label` — a generic element with an aria-label and no text content is
 * an axe `aria-prohibited-attr` violation (serious), not a stylistic choice. */
export function BrandMark({
  edition = "cloud",
  name,
  showName = true,
  size = "base",
  className,
  ...props
}: BrandMarkProps) {
  const label = name ?? (edition === "community" ? `${BRAND_NAME} Community` : BRAND_NAME);
  const hasSurfaceLabel = label !== BRAND_NAME;
  return (
    <span
      className={cn("inline-flex min-w-0 items-center gap-2", className)}
      aria-label={showName ? label : undefined}
      aria-hidden={showName ? undefined : true}
      {...props}
    >
      <img className={cn("shrink-0", sizes[size])} src={BRAND_MARK_URL} alt="" aria-hidden="true" />
      {showName && (
        <>
          {hasSurfaceLabel && (
            <span
              data-slot="brand-name"
              className="flb-display truncate text-lg font-semibold text-kumo-default sm:hidden"
            >
              {BRAND_NAME}
            </span>
          )}
          {/* The name carries a slot so the shell can hide it in the collapsed
              rail, where only the mark fits; the mark itself never hides. */}
          <span
            data-slot="brand-name"
            className={cn(
              "flb-display truncate text-lg font-semibold text-kumo-default",
              hasSurfaceLabel && "hidden sm:inline",
            )}
          >
            {label}
          </span>
        </>
      )}
    </span>
  );
}
