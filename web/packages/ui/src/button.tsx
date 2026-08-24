import {
  Button as KumoButton,
  type ButtonProps as KumoButtonProps,
} from "@cloudflare/kumo/components/button";

/**
 * Application button entry point. Its public variants and sizes are exactly the
 * design system's current vocabulary; the wrapper only establishes FairLB's
 * primary/base defaults.
 */
export type ButtonProps = KumoButtonProps;

export function Button({ variant = "primary", size = "base", ...props }: ButtonProps) {
  return <KumoButton variant={variant} size={size} {...props} />;
}
