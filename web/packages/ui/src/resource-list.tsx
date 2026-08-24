import type { ComponentPropsWithoutRef, ReactNode } from "react";
import { cn } from "./cn";
import { ResponsiveResourceRow, type ResponsiveResourceRowProps } from "./responsive-resource-row";

export type ResourceListProps = ComponentPropsWithoutRef<"section"> & {
  label: string;
  children: ReactNode;
};

export function ResourceList({ label, children, className, ...props }: ResourceListProps) {
  return (
    <section
      aria-label={label}
      className={cn("overflow-hidden rounded-xl border border-kumo-line bg-kumo-base", className)}
      {...props}
    >
      <div role="list" className="divide-y divide-kumo-line px-4 sm:px-5">
        {children}
      </div>
    </section>
  );
}

export type ResourceListItemProps = ResponsiveResourceRowProps;

export function ResourceListItem(props: ResourceListItemProps) {
  return <ResponsiveResourceRow role="listitem" {...props} />;
}
