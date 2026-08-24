import type { ComponentPropsWithoutRef } from "react";
import { cn } from "./cn";
import { FormRowItemContext } from "./form-context";

type FormRowProps =
  | ({ as: "form" } & ComponentPropsWithoutRef<"form">)
  | ({ as?: "div" } & ComponentPropsWithoutRef<"div">);

/**
 * Three-track container for horizontal forms.
 *
 * The call site expresses field widths with a responsive grid template; each
 * Item joins a subgrid so that the label, control and message rows line up
 * across the row. On small screens it degrades to an ordinary single column in
 * DOM order.
 */
function FormRowRoot(props: FormRowProps) {
  if (props.as === "form") {
    const { as: _as, className, children, ...formProps } = props;
    return (
      <form
        className={cn("grid gap-4 sm:items-start sm:gap-x-3 sm:gap-y-2", className)}
        {...formProps}
      >
        {children}
      </form>
    );
  }
  const { as: _as, className, children, ...divProps } = props;
  return (
    <div className={cn("grid gap-4 sm:items-start sm:gap-x-3 sm:gap-y-2", className)} {...divProps}>
      {children}
    </div>
  );
}

function FormRowItem({ className, children, ...props }: ComponentPropsWithoutRef<"div">) {
  return (
    <FormRowItemContext.Provider value>
      <div
        className={cn("grid min-w-0 gap-2 sm:row-span-3 sm:grid-rows-subgrid", className)}
        {...props}
      >
        {children}
      </div>
    </FormRowItemContext.Provider>
  );
}

function FormRowActions({ className, children, ...props }: ComponentPropsWithoutRef<"div">) {
  return (
    <div className="min-w-0 sm:row-span-3 sm:grid sm:grid-rows-subgrid">
      <div className={cn("flex flex-wrap items-center gap-2 sm:row-start-2", className)} {...props}>
        {children}
      </div>
    </div>
  );
}

export const FormRow = Object.assign(FormRowRoot, {
  Item: FormRowItem,
  Actions: FormRowActions,
});

/** A standalone action area for create and edit forms, keeping placement
 * separate from what the buttons mean. */
export function FormActions({ className, children, ...props }: ComponentPropsWithoutRef<"div">) {
  return (
    <div className={cn("flex flex-wrap items-center gap-2", className)} {...props}>
      {children}
    </div>
  );
}
