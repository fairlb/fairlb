import { Text } from "@cloudflare/kumo/components/text";
import type { ReactNode } from "react";
import { BrandMark } from "./brand-mark";
import { cn } from "./cn";
import { Card } from "./form";
import { Centered, LangThemeToggle } from "./shell";

export type AuthShellProps = {
  appName: ReactNode;
  title: ReactNode;
  description?: ReactNode;
  /** The notice slot sits outside the card, so news about a service outage never
   * mixes with the form's own validation errors. */
  notice?: ReactNode;
  /** Defaults to the language/theme toggle; pass null to hide it deliberately. */
  toolbar?: ReactNode;
  environment?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  className?: string;
};

/** The shared layout for authentication pages. The form itself, the notice query
 * and the page title stay with the application that renders them. */
export function AuthShell({
  appName,
  title,
  description,
  notice,
  toolbar,
  environment,
  children,
  footer,
  className,
}: AuthShellProps) {
  return (
    <Centered>
      <div className={cn("w-full max-w-sm space-y-4", className)}>
        {notice}
        <Card className="space-y-5">
          <div className="flex min-w-0 items-center justify-between gap-3">
            <div className="flex min-w-0 items-center gap-2 text-base font-medium text-kumo-strong">
              {/* Decorative: the visible appName beside it is the accessible name. */}
              <BrandMark showName={false} />
              <span className="truncate">{appName}</span>
            </div>
            {environment != null && (
              <span className="shrink-0 rounded-md bg-kumo-tint px-2 py-1 text-base text-kumo-subtle">
                {environment}
              </span>
            )}
          </div>
          <div className="flex items-start justify-between gap-3">
            <Text variant="heading3" as="h1">
              {title}
            </Text>
            {toolbar === undefined ? <LangThemeToggle /> : toolbar}
          </div>
          {description != null && <p className="text-base text-kumo-subtle">{description}</p>}
          {children}
          {footer != null && <div className="text-base text-kumo-subtle">{footer}</div>}
        </Card>
      </div>
    </Centered>
  );
}
