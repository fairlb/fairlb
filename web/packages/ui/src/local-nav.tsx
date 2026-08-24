import { Link as KumoLink } from "@cloudflare/kumo/components/link";
import { CaretDownIcon } from "@phosphor-icons/react";
import { useI18n } from "@fairlb/i18n";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { cn } from "./cn";

export type LocalNavItem = {
  value: string;
  label: ReactNode;
  href: string;
  description?: ReactNode;
};

export type LocalNavProps = {
  value: string;
  items: readonly LocalNavItem[];
  ariaLabel?: string;
  className?: string;
};

/**
 * Navigation within one resource or settings area.
 *
 * These are links to peer pages, not tabs. On wide screens the choices form a
 * quiet vertical rail next to the work; below xl the same links live behind one
 * compact disclosure so a product sidebar and a local rail never squeeze the
 * content at the same time.
 */
export function LocalNav({ value, items, ariaLabel, className }: LocalNavProps) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const listId = useId();
  const current = items.find((item) => item.value === value) ?? items[0];

  if (items.length === 0) return null;

  return (
    <div
      className={cn(
        // The width comes from the same token the grid track reserves, so the
        // rail and its track cannot disagree.
        "min-w-0 xl:sticky xl:top-4 xl:w-[var(--flb-section-nav-width,12rem)] xl:shrink-0 xl:self-start",
        className,
      )}
      data-slot="local-nav"
    >
      <button
        type="button"
        aria-expanded={open}
        aria-controls={listId}
        className={cn(
          "flex min-h-11 w-full items-center justify-between gap-3 rounded-lg border border-kumo-line",
          "bg-kumo-base px-3 text-left text-base font-medium text-kumo-default xl:hidden",
          "hover:bg-kumo-tint focus:outline-none focus-visible:ring-2 focus-visible:ring-kumo-brand",
        )}
        onClick={() => setOpen((currentOpen) => !currentOpen)}
      >
        <span className="min-w-0 truncate">{current?.label}</span>
        <CaretDownIcon
          aria-hidden
          className={cn(
            "size-4 shrink-0 transition-transform motion-reduce:transition-none",
            open && "rotate-180",
          )}
        />
      </button>
      <nav
        aria-label={ariaLabel ?? t("localNavigation")}
        id={listId}
        className={cn(
          // Below xl this is the panel a disclosure button opened, so it needs
          // the surface of a panel. From xl it is the rail itself, and a rail is
          // chrome: the framed box put a second rectangle of equal visual weight
          // next to the content card, so the page read as "two cards" rather
          // than "navigation and content". The current item already carries a
          // tint and a brand bar, which is the whole of the structure a reader
          // needs here.
          "mt-2 rounded-lg border border-kumo-line bg-kumo-base p-1.5",
          "xl:mt-0 xl:rounded-none xl:border-0 xl:bg-transparent xl:p-0",
          !open && "hidden xl:block",
        )}
      >
        <ul className="grid gap-1 text-kumo-default">
          {items.map((item) => {
            const active = item.value === value;
            return (
              <li key={item.value}>
                <KumoLink
                  href={item.href}
                  // `current` rather than the default `inline`: it emits
                  // `text-current`, so the colour comes from the list above and
                  // there is no same-element colour conflict to lose. The
                  // underline the variant still carries is removed by the
                  // unlayered rule in tokens.css.
                  variant="current"
                  data-navigation-current={active ? "page" : "false"}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "relative grid min-h-10 gap-0.5 rounded-md px-3 py-2",
                    "hover:bg-kumo-tint",
                    "focus:outline-none focus-visible:ring-2 focus-visible:ring-kumo-brand",
                    active &&
                      "bg-kumo-tint font-medium before:absolute before:inset-y-2 before:left-0 before:w-0.5 before:rounded-full before:bg-kumo-brand",
                  )}
                  onClick={() => setOpen(false)}
                >
                  <span>{item.label}</span>
                  {item.description && (
                    <span className="text-base font-normal text-kumo-subtle">
                      {item.description}
                    </span>
                  )}
                </KumoLink>
              </li>
            );
          })}
        </ul>
      </nav>
    </div>
  );
}

export type PageContentsNavItem = {
  href: `#${string}`;
  label: ReactNode;
};

/**
 * A small table of contents for sections that remain on the current page.
 *
 * The rail appears from `2xl`, not `xl`. It is a convenience, not navigation —
 * every section it lists is already on screen — so it may only take a column
 * when there is one to spare. At `xl` a page carrying this alongside a long form
 * left the form 561px against the 768px it asks for, and the reader lost more to
 * the shortfall than the shortcut gave back. Below `2xl` it collapses to a
 * single disclosure.
 */
export function PageContentsNav({
  items,
  ariaLabel,
  className,
}: {
  items: readonly PageContentsNavItem[];
  ariaLabel?: string;
  className?: string;
}) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [activeHref, setActiveHref] = useState(items[0]?.href);
  const navRef = useRef<HTMLElement>(null);
  const listId = useId();

  useEffect(() => {
    const nav = navRef.current;
    if (!nav) return;
    const targets = items
      .map((item) => document.getElementById(item.href.slice(1)))
      .filter((target): target is HTMLElement => target !== null);
    if (targets.length === 0) return;

    let scrollRoot: HTMLElement | Window = window;
    let ancestor = nav.parentElement;
    while (ancestor) {
      const overflowY = window.getComputedStyle(ancestor).overflowY;
      if (overflowY === "auto" || overflowY === "scroll") {
        scrollRoot = ancestor;
        break;
      }
      ancestor = ancestor.parentElement;
    }

    let frame = 0;
    const update = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        const rootTop = scrollRoot instanceof Window ? 0 : scrollRoot.getBoundingClientRect().top;
        const threshold = rootTop + 48;
        let active = targets[0]!;
        if (
          scrollRoot instanceof HTMLElement &&
          scrollRoot.scrollTop + scrollRoot.clientHeight >= scrollRoot.scrollHeight - 2
        ) {
          setActiveHref(`#${targets[targets.length - 1]!.id}`);
          return;
        }
        for (const target of targets) {
          if (target.getBoundingClientRect().top <= threshold) active = target;
        }
        setActiveHref(`#${active.id}`);
      });
    };

    update();
    scrollRoot.addEventListener("scroll", update, { passive: true });
    window.addEventListener("resize", update);
    return () => {
      cancelAnimationFrame(frame);
      scrollRoot.removeEventListener("scroll", update);
      window.removeEventListener("resize", update);
    };
  }, [items]);

  if (items.length === 0) return null;
  return (
    <div
      className={cn(
        "order-first min-w-0 2xl:order-last 2xl:sticky 2xl:top-4 2xl:w-44 2xl:shrink-0 2xl:self-start",
        className,
      )}
      data-slot="page-contents-nav"
    >
      <button
        type="button"
        aria-expanded={open}
        aria-controls={listId}
        className={cn(
          "flex min-h-11 w-full items-center justify-between gap-3 rounded-lg border border-kumo-line",
          "bg-kumo-base px-3 text-left text-base font-medium text-kumo-default 2xl:hidden",
          "hover:bg-kumo-tint focus:outline-none focus-visible:ring-2 focus-visible:ring-kumo-brand",
        )}
        onClick={() => setOpen((currentOpen) => !currentOpen)}
      >
        <span>{t("pageContents")}</span>
        <CaretDownIcon
          aria-hidden
          className={cn(
            "size-4 shrink-0 transition-transform motion-reduce:transition-none",
            open && "rotate-180",
          )}
        />
      </button>
      <nav
        ref={navRef}
        aria-label={ariaLabel ?? t("pageContents")}
        id={listId}
        className={cn("mt-2 border-l border-kumo-line pl-4 2xl:mt-0", !open && "hidden 2xl:block")}
      >
        <p className="mb-2 text-base font-medium text-kumo-subtle">{t("pageContents")}</p>
        <ul className="grid gap-1.5 text-base">
          {items.map((item) => {
            const active = item.href === activeHref;
            return (
              <li key={item.href}>
                <a
                  href={item.href}
                  aria-current={active ? "location" : undefined}
                  className={cn(
                    "block rounded-sm text-kumo-default underline-offset-4 hover:underline",
                    "focus:outline-none focus-visible:ring-2 focus-visible:ring-kumo-brand",
                    active && "font-medium text-kumo-link",
                  )}
                  onClick={() => {
                    setActiveHref(item.href);
                    setOpen(false);
                  }}
                >
                  {item.label}
                </a>
              </li>
            );
          })}
        </ul>
      </nav>
    </div>
  );
}
