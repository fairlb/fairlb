import { createContext } from "react";

/** The accessibility contract between a Field and its control; consumed only
 * inside this package. */
export const FieldContext = createContext<{
  controlId?: string;
  labelId?: string;
  describedBy?: string;
  invalid?: boolean;
} | null>(null);

/** When a Field sits inside a FormRow.Item, its three slots join the parent's
 * subgrid. */
export const FormRowItemContext = createContext(false);
