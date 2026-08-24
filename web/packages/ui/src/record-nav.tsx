import { Link as KumoLink } from "@cloudflare/kumo/components/link";
import { useI18n } from "@fairlb/i18n";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { cn } from "./cn";

/** A set of URL-driven, page-level navigation items. For switching panels within
 * a page, keep using the design system's Tabs. */
export type RecordNavItem = {
  /** A stable value matching the current route; the item is marked as the
   * current page when it equals RecordNav.value. */
  value: string;
  label: ReactNode;
  /** The real destination, kept as SPA navigation by the link provider. */
  href: string;
  className?: string;
};

export type RecordNavProps = {
  /** The URL is the single source of truth, so this is controlled only and
   * offers no change callback. */
  value: string;
  items: RecordNavItem[];
  /** Accessible name for the nav element. It must be passed explicitly when a
   * page carries more than one set of navigation. */
  ariaLabel?: string;
  className?: string;
  listClassName?: string;
};

function preferredScrollBehavior(): ScrollBehavior {
  if (typeof window === "undefined") return "auto";
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
}

function revealHorizontally(
  viewport: HTMLDivElement | null,
  item: HTMLAnchorElement | null,
  behavior: ScrollBehavior,
) {
  if (!viewport || !item) return;
  const viewportRect = viewport.getBoundingClientRect();
  const itemRect = item.getBoundingClientRect();
  const delta =
    itemRect.left < viewportRect.left
      ? itemRect.left - viewportRect.left
      : itemRect.right > viewportRect.right
        ? itemRect.right - viewportRect.right
        : 0;
  if (Math.abs(delta) <= 1) return;
  // scrollIntoView would also scroll vertical ancestors such as the page or a
  // drawer; only the nav strip itself may scroll horizontally here.
  viewport.scrollTo({ left: viewport.scrollLeft + delta, behavior });
}

/**
 * RecordNav is the navigation between the aspects of one record.
 *
 * # When to use it rather than LocalNav
 *
 * The criterion is what the page header names:
 *
 * - **A record** — it carries an identity (a slug, a name), usually a status
 *   badge and record-level actions. Its child pages are *aspects of that one
 *   thing*: a provider's Models, Keys and Settings. Use `RecordNav`, directly
 *   under the header, so the header and its aspects read as one unit.
 * - **An area of the application** — Settings, Account, Billing. Its children
 *   are destinations, and modules inject more of them over time. Use `LocalNav`,
 *   a vertical rail, which stays legible as the list grows.
 *
 * # Why horizontal for records
 *
 * The two orientations spend different budgets. A strip costs about 40px of
 * *vertical* space on a page that scrolls anyway; a rail costs 216px of
 * *horizontal* space that nothing gives back — and horizontal is the scarce one
 * on a 1366px laptop.
 *
 * Measured on the composed provider settings page, form column width against
 * the 768px that page asks for:
 *
 * | viewport | with a section rail | with this strip |
 * |---|---|---|
 * | 1280 | 561px | 768px |
 * | 1366 | 658px | 768px |
 * | 1536 | 768px | 768px |
 *
 * `record-layout-metrics.browser.tsx` takes those numbers; this table is a
 * record of one run, not the criterion.
 *
 * # Why not tab roles
 *
 * These items change the URL and lead to another page; there is no `tabpanel`
 * associated with them, so `tablist`/`tab` would describe something that is not
 * happening. Real links plus `aria-current` keep opening in a new tab, copying
 * the address, and correct screen-reader semantics. That distinction — not the
 * orientation — was the real defect behind the earlier "no tabs" rule.
 *
 * On a narrow screen only the strip itself scrolls, and the current item is
 * brought into view automatically.
 */
export function RecordNav({ value, items, ariaLabel, className, listClassName }: RecordNavProps) {
  const { t } = useI18n();
  const activeRef = useRef<HTMLAnchorElement | null>(null);
  const viewportRef = useRef<HTMLDivElement | null>(null);
  const contentRef = useRef<HTMLDivElement | null>(null);
  const [edges, setEdges] = useState({ start: false, end: false });
  // Reordering without changing the count can move the current item to the other
  // end while leaving the container's size untouched, so no resize is observed.
  // Driving the re-measure from the sequence of values avoids depending on the
  // identity of the caller's array.
  const itemOrderKey = JSON.stringify(items.map((item) => item.value));

  const updateEdges = useCallback(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    const next = {
      start: viewport.scrollLeft > 1,
      end: viewport.scrollLeft + viewport.clientWidth < viewport.scrollWidth - 1,
    };
    setEdges((current) =>
      current.start === next.start && current.end === next.end ? current : next,
    );
  }, []);

  const revealActive = useCallback((behavior: ScrollBehavior) => {
    revealHorizontally(viewportRef.current, activeRef.current, behavior);
  }, []);

  useEffect(() => {
    revealActive(preferredScrollBehavior());
    const frame = requestAnimationFrame(updateEdges);
    return () => cancelAnimationFrame(frame);
  }, [itemOrderKey, revealActive, updateEdges, value]);

  useEffect(() => {
    const viewport = viewportRef.current;
    const content = contentRef.current;
    if (!viewport || !content) return;

    let frame = 0;
    const measureAfterLayout = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        // Font loading, a locale change or a viewport zoom can all change the
        // width while `value` stays the same. The observer watches both the
        // content and the viewport; re-measuring the edges also confirms the
        // current item is still in view.
        revealActive("auto");
        updateEdges();
      });
    };
    viewport.addEventListener("scroll", updateEdges, { passive: true });
    const observer =
      typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measureAfterLayout);
    observer?.observe(viewport);
    observer?.observe(content);
    measureAfterLayout();
    return () => {
      cancelAnimationFrame(frame);
      viewport.removeEventListener("scroll", updateEdges);
      observer?.disconnect();
    };
  }, [itemOrderKey, revealActive, updateEdges]);

  if (items.length === 0) return null;

  return (
    <nav
      aria-label={ariaLabel ?? t("recordNavigation")}
      className={cn("relative isolate min-w-0 font-medium", className)}
      data-slot="record-nav"
    >
      <div
        ref={viewportRef}
        data-slot="record-nav-viewport"
        className={cn(
          "min-w-0 overflow-x-auto overscroll-x-contain border-b border-kumo-hairline",
          "[scrollbar-width:thin]",
          listClassName,
        )}
      >
        <div
          ref={contentRef}
          data-slot="record-nav-content"
          className="flex w-max min-w-full items-stretch gap-2 text-kumo-default"
        >
          {items.map((item) => {
            const active = item.value === value;
            return (
              <KumoLink
                key={item.value}
                ref={active ? activeRef : undefined}
                href={item.href}
                // See the note in LocalNav: `current` inherits the colour from
                // the row, and tokens.css removes the decoration.
                variant="current"
                // Hands the link adapter the controlled answer instead of
                // letting the router infer one.
                //
                // **It has no consumer that needs it today.** It was added for a
                // nav driven by a query string (`/health?tab=jobs`), and there
                // is none in the tree — the only `?tab=` left is in a browser
                // fixture. Path-driven navs do not need it either: `AppLink`
                // sets `activeOptions.exact`, so an overview href is not
                // inferred as current on a sibling. Kept because it is the
                // protocol `AppLink` documents and `LocalNav` emits the same
                // attribute; noted here so the next reader does not go looking
                // for the case it is protecting against.
                data-navigation-current={active ? "page" : "false"}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "relative -mb-px flex min-h-9 shrink-0 items-center whitespace-nowrap px-2",
                  "border-b-2 border-transparent",
                  "hover:bg-kumo-tint",
                  "focus:rounded focus:outline-none focus-visible:ring-2 focus-visible:ring-kumo-brand",
                  active && "border-kumo-brand font-medium",
                  item.className,
                )}
                onClick={(event) => {
                  revealHorizontally(
                    viewportRef.current,
                    event.currentTarget,
                    preferredScrollBehavior(),
                  );
                }}
              >
                {item.label}
              </KumoLink>
            );
          })}
        </div>
      </div>
      {edges.start && (
        <span
          aria-hidden="true"
          data-slot="record-nav-edge-start"
          className="pointer-events-none absolute inset-y-0 left-0 w-6 bg-linear-to-r from-kumo-recessed to-transparent"
        />
      )}
      {edges.end && (
        <span
          aria-hidden="true"
          data-slot="record-nav-edge-end"
          className="pointer-events-none absolute inset-y-0 right-0 w-6 bg-linear-to-l from-kumo-recessed to-transparent"
        />
      )}
    </nav>
  );
}
