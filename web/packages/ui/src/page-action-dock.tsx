import type { ComponentPropsWithoutRef, ReactNode } from "react";
import { createContext, useContext } from "react";
import { createPortal } from "react-dom";
import { cn } from "./cn";

const PageActionDockTargetContext = createContext<HTMLElement | null>(null);

export const PageActionDockTargetProvider = PageActionDockTargetContext.Provider;

export type PageActionDockProps = ComponentPropsWithoutRef<"div"> & {
  status: ReactNode;
};

/**
 * Actions for a page-sized form. The dock is portalled into AppShell's fixed
 * action region, outside the scrolling content, so it cannot cover the final
 * field or inherit the geometry of the form that owns it.
 */
export function PageActionDock({ status, children, className, ...props }: PageActionDockProps) {
  const target = useContext(PageActionDockTargetContext);
  if (!target) return null;

  return createPortal(
    <div
      className={cn(
        "flex min-h-16 flex-wrap items-center justify-between gap-3 py-3",
        "pb-[max(0.75rem,env(safe-area-inset-bottom))]",
        className,
      )}
      data-slot="page-action-dock"
      {...props}
    >
      <div className="min-w-0 grow text-base text-kumo-subtle">{status}</div>
      <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">{children}</div>
    </div>,
    target,
  );
}
