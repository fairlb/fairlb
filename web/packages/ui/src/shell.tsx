import { Button } from "./button";
import { Breadcrumbs } from "@cloudflare/kumo/components/breadcrumbs";
import { Sidebar, useSidebar } from "@cloudflare/kumo/components/sidebar";
import { Text } from "@cloudflare/kumo/components/text";
import { CircleHalfIcon, MoonIcon, SunIcon } from "@phosphor-icons/react";
import { useI18n, type MessageKey } from "@fairlb/i18n";
import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { cn } from "./cn";
import { PageActionDockTargetProvider } from "./page-action-dock";
import { RecordNav, type RecordNavProps } from "./record-nav";
import { useTheme, type Theme } from "./theme";

/**
 * AppShell is the layout every signed-in page shares: one sidebar and one
 * content column, and on a desktop nothing else (ADR-0202).
 *
 * The sidebar holds, top to bottom: the brand, the scope switcher where the
 * application has one, the navigation, and the account. There used to be a top
 * bar as well, carrying exactly the switcher and the account menu; once those
 * two moved into the rail it had nothing left to carry, and a strip kept only so
 * that its bottom edge could be aligned with the brand strip beside it was the
 * wrong way round — the alignment was the cost of the strip, not a reason for
 * it. The constant that pinned the two heights together, and the token behind
 * it, went with the bar. On a mobile viewport the sidebar is an off-canvas
 * sheet, so a bar does remain there, holding the two things the sheet cannot
 * while it is closed: the control that opens it, and the name of the scope the
 * page belongs to (`scopeLabel`).
 *
 * Navigation items are assembled by each application; the shell only fixes the
 * skeleton, so the applications share a structure.
 *
 * The height model is viewport-locked: the shell is one viewport tall with
 * overflow hidden, the document never scrolls, and exactly one container — the
 * content area — does. It was once written as a minimum height with no ceiling,
 * which handed scrolling back to the document and produced three measured
 * failures:
 *   1. the sidebar, brand included, scrolled away with the page;
 *   2. the sidebar's content is itself a scroll area, which never engages when
 *      its height is unbounded, so its scroll fade and thin scrollbar were both
 *      dead;
 *   3. the sidebar's footer anchored to the bottom of the *page* rather than the
 *      viewport, putting whatever it held off-screen on a long page scrolled to
 *      the top.
 *
 * The small viewport unit is used rather than the dynamic one, matching what the
 * sidebar component itself uses, so that a mobile address bar appearing and
 * disappearing does not make the shell twitch.
 */
export function AppShell(props: AppShellProps) {
  return (
    // The className reaches the provider's own wrapper, which is already a
    // positioned flex container, so no extra box is needed to cap the height.
    <Sidebar.Provider className="h-svh overflow-hidden">
      <ShellFrame {...props} />
    </Sidebar.Provider>
  );
}

export type AppShellProps = {
  /** The brand slot, usually a link home. It is the first row of the sidebar
   * header and, on a mobile viewport, also sits in the bar that opens the
   * sheet. */
  brand: ReactNode;
  /**
   * The row beneath the brand that names the scope the navigation belongs to —
   * the console's organization switcher. An application without a scope leaves
   * it out and the row is not rendered. Pass a `SidebarIdentityRow` (or a menu
   * whose trigger is one); the shell supplies the surrounding menu list.
   */
  scopeSwitcher?: ReactNode;
  /**
   * The plain name of that same scope, for the mobile bar.
   *
   * A string rather than a node, and deliberately not the switcher itself: the
   * bar is 56px and the switcher is a two-line menu button that has to live
   * inside a `Sidebar.Menu`. What the bar owes the reader is the *answer*, not
   * the control.
   *
   * It exists because the sidebar is an off-canvas sheet on a mobile viewport,
   * so everything in it — the switcher included — is off-screen while the reader
   * is looking at the page. Before this, the bar held the trigger and a second
   * copy of the brand, and a console page said nothing at all about which
   * organization's keys or invoices were on screen. That gap was inherited, not
   * chosen: ADR-0061 declined to give the console breadcrumbs partly because
   * "the mobile bar still carries the switcher chip showing the org name", and
   * ADR-0202 later moved that chip into the rail without revisiting the
   * sentence that depended on it.
   *
   * Applications with a single scope (the operations consoles) and the console's
   * own global pages leave it unset, and the bar keeps the brand name.
   */
  scopeLabel?: string;
  /**
   * Sidebar navigation content; the groups and menus are assembled by the
   * application.
   *
   * It is required: every signed-in page has a rail. A page whose scope has no
   * application navigation of its own still passes that scope's navigation —
   * the console's global pages list the organizations and the account — because
   * the rail now also carries the switcher and the account, and a page without
   * it would have no way to change scope or to sign out. This replaces an
   * earlier arrangement in which a page could omit navigation and the rail was
   * not rendered at all (ADR-0143); the invariant behind that arrangement, that
   * an empty rail must never be shown, still holds, and is now met by every
   * scope having something to show.
   */
  nav: ReactNode;
  /**
   * The account row at the bottom of the sidebar, normally an `AccountMenu`.
   * Optional only so that layout fixtures can leave it out; every application
   * passes it.
   */
  account?: ReactNode;
  /** The banner area at the top of the content column, above the scroll
   * region; the reason for that position is at the point where it is rendered. */
  banners?: ReactNode;
  /**
   * A stable identity for the route, usually the pathname. When it changes the
   * shell moves keyboard focus to main and announces the new page. Do not fold
   * the query string or hash into it: filtering, pagination and URL-driven
   * drawers must not steal the current focus.
   */
  routeKey: string;
  /** Text announced after a route change. It defaults to a generic phrase, so
   * that nothing reads out the previous page's title while the new one is still
   * suspended. */
  routeAnnouncement?: string;
  children: ReactNode;
};

/**
 * The content column's geometry is defined exactly once, here.
 *
 * The width and the horizontal padding are fixed elsewhere; what this constant
 * does is make the banner area and `<main>` read the *same* definition. Written
 * out as two literals, changing one and forgetting the other reports nothing and
 * simply misaligns quietly on a wide screen: the banner's left edge parts
 * company with the page title, by an amount that varies with viewport width and
 * is nearly invisible in a narrow development window.
 *
 * Vertical padding stays out of it: main and the banner area legitimately differ
 * there.
 */
const CONTENT_COLUMN =
  "mx-auto w-full max-w-[var(--flb-content-max,87.5rem)] px-[var(--flb-content-pad-inline,1rem)] sm:px-[var(--flb-content-pad-inline-sm,1.5rem)] lg:px-[var(--flb-content-pad-inline-lg,2.5rem)]";

/**
 * The rail's horizontal padding, as the sidebar component applies it to its
 * own content: narrower in the collapsed state, where the inner width is all of
 * 35px. The switcher and account rows sit in the header and footer rather than
 * in the content, so they have to state the same padding themselves, or the
 * three rows of the rail start at three different x positions.
 */
const RAIL_PADDING = "px-[11px] group-not-data-[state=collapsed]/sidebar:px-3.5";

/**
 * The collapse control sits at the right end of the brand row on a desktop, and
 * the sheet's close control takes the same place on a mobile viewport. Never
 * both, and never in the footer:
 *   - why not the footer: the footer is the account row, and in the collapsed
 *     state the rail's inner width is 35px — an avatar and a 34px button cannot
 *     share it, and stacking them there would put the collapse control below
 *     the account, where nobody looks for it. The brand row, by contrast, stacks
 *     its two items vertically when collapsed and both fit;
 *   - why the mobile sheet gets a close control of its own: when the sheet is
 *     open it covers the whole viewport, including the bar that holds the open
 *     control, so a reader who opened it had nothing visible to close it with
 *     but the Escape key. The trigger component has no mobile branch and reads
 *     the desktop open state, so it is not simply reused here;
 *   - why not hide one with a CSS breakpoint: the component's breakpoint is a
 *     JavaScript value and configurable, so a CSS breakpoint drifts away from
 *     it, and CSS cannot undo the accessibility difference that inert creates.
 *
 * The mobile bar's own trigger stays, since with the sheet closed it is the one
 * control that can open it. Its label and expanded state are corrected here
 * rather than rebuilt: the component's built-in label is not localised, and it
 * reports the desktop open state, which on a mobile sheet is the opposite of
 * what is visible. Rest props are spread last, so overriding both from this one
 * shared entry point is enough.
 */
function ShellSidebarTrigger() {
  const { isMobile, open, openMobile } = useSidebar();
  const { t } = useI18n();
  const expanded = isMobile ? openMobile : open;
  const label = expanded ? t("closeNavigation") : t("openNavigation");
  return <Sidebar.Trigger aria-expanded={expanded} aria-label={label} title={label} />;
}

function ShellSidebarClose() {
  const { t } = useI18n();
  const label = t("closeNavigation");
  return <Sidebar.Close aria-label={label} title={label} />;
}

/**
 * ShellFrame is AppShell's inner layer, split out for exactly one reason: the
 * sidebar hook must be called inside the provider, and AppShell is the layer
 * that renders the provider.
 */
function ShellFrame({
  brand,
  scopeSwitcher,
  scopeLabel,
  nav,
  account,
  banners,
  routeKey,
  routeAnnouncement,
  children,
}: AppShellProps) {
  const { isMobile, setOpenMobile } = useSidebar();
  const { t } = useI18n();
  const mainRef = useRef<HTMLElement>(null);
  const previousRoute = useRef<string | undefined>(undefined);
  const previousSheetRoute = useRef(routeKey);
  const [announcement, setAnnouncement] = useState("");
  const [actionDockTarget, setActionDockTarget] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    if (previousRoute.current === routeKey) return;
    // On the first paint the browser's focus should stay where it is; only
    // subsequent in-app navigation is handled.
    if (previousRoute.current === undefined) {
      previousRoute.current = routeKey;
      return;
    }
    previousRoute.current = routeKey;
    setAnnouncement("");
    const frame = requestAnimationFrame(() => {
      const main = mainRef.current;
      main?.focus({ preventScroll: true });
      setAnnouncement(routeAnnouncement ?? t("pageChanged"));
    });
    return () => cancelAnimationFrame(frame);
  }, [routeKey, routeAnnouncement, t]);

  // The mobile sheet closes when the route changes. Nothing else would close
  // it: the full-screen sheet has no backdrop to tap, and the sidebar component
  // only closes itself on Escape or its own close control — so a reader who
  // picked a destination from the sheet was left looking at the sheet, with
  // the page they asked for loaded behind it. The effect is keyed on the route
  // rather than on link clicks so that it also covers the switcher and the
  // account menu, which navigate without a link in the nav.
  //
  // It compares against the previous route rather than running on every
  // render so that it cannot close a sheet the reader has just opened.
  useEffect(() => {
    if (previousSheetRoute.current === routeKey) return;
    previousSheetRoute.current = routeKey;
    if (isMobile) setOpenMobile(false);
  }, [routeKey, isMobile, setOpenMobile]);

  const focusMain = () => mainRef.current?.focus({ preventScroll: false });
  return (
    <PageActionDockTargetProvider value={actionDockTarget}>
      <a
        href="#main-content"
        onClick={focusMain}
        className={cn(
          "fixed top-3 left-4 z-[var(--flb-z-skip,100)] -translate-y-20 rounded-lg bg-kumo-base px-3 py-2",
          "font-medium text-kumo-default shadow-md ring ring-kumo-line",
          "focus:translate-y-0 focus:outline-none focus-visible:ring-2 focus-visible:ring-kumo-brand",
          "motion-reduce:transition-none",
        )}
      >
        {t("skipToContent")}
      </a>
      <Sidebar fullScreenOnMobile>
        {/* The component's header is a single fixed-height row. It is turned
            into a column here so that it can hold two: the brand row, and the
            scope row beneath it. */}
        <Sidebar.Header className="h-auto flex-col items-stretch gap-0 px-0">
          <div
            data-slot="shell-brand-row"
            className={cn(
              "flex h-14 shrink-0 items-center gap-1 px-3",
              // Collapsed, the row stacks: the mark above the control. The
              // brand's name is hidden by its slot; the mark itself stays, so
              // the rail is still signed.
              "group-data-[state=collapsed]/sidebar:h-auto group-data-[state=collapsed]/sidebar:flex-col",
              "group-data-[state=collapsed]/sidebar:gap-2 group-data-[state=collapsed]/sidebar:px-[11px] group-data-[state=collapsed]/sidebar:py-3",
            )}
          >
            <div className="flex min-w-0 flex-1 items-center group-data-[state=collapsed]/sidebar:[&_[data-slot=brand-name]]:hidden">
              {brand}
            </div>
            {isMobile ? <ShellSidebarClose /> : <ShellSidebarTrigger />}
          </div>
          {scopeSwitcher && (
            <div
              data-slot="shell-scope-switcher"
              className={cn("border-t border-kumo-line py-1.5", RAIL_PADDING)}
            >
              {/* The menu list is the shell's: a menu button supplies its own
                  list item, and the list it belongs in is this one. */}
              <Sidebar.Menu>{scopeSwitcher}</Sidebar.Menu>
            </div>
          )}
        </Sidebar.Header>
        <Sidebar.Content>{nav}</Sidebar.Content>
        {/* The footer must be a direct child of the sidebar: the component
            recognises it by its display name in order to keep it out of the
            peek area, and a wrapper component would make it unrecognisable. A
            conditional is fine — a false or null child is dropped before the
            check. */}
        {account && (
          <Sidebar.Footer className={cn("h-auto items-stretch py-1.5", RAIL_PADDING)}>
            <Sidebar.Menu className="w-full">{account}</Sidebar.Menu>
          </Sidebar.Footer>
        )}
      </Sidebar>
      {/* min-h-0 is essential: a flex child defaults to min-height:auto and is
          pushed open by its content, which would leave the overflow container
          below without the bounded height it needs to scroll. */}
      <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-kumo-recessed text-kumo-default">
        {isMobile && (
          // The one bar that remains: on a mobile viewport the sidebar is an
          // off-canvas sheet, translated away and inert when closed, so the
          // control that opens it has to live outside it — and the brand
          // beside that control keeps one way home visible while the sheet is
          // closed. The sheet carries its own copy of the brand, but only one
          // of the two is ever in the accessibility tree.
          <header className="flex h-14 shrink-0 items-center gap-3 border-b border-kumo-line bg-kumo-base px-3">
            <ShellSidebarTrigger />
            <div
              className={cn(
                "flex min-w-0 items-center gap-2",
                // When there is a scope to name, the brand shrinks to its mark.
                // The product's name is on the first row of the sheet this bar
                // opens; the organization's name is nowhere else on the screen,
                // and only one of the two fits beside the trigger. The mark
                // keeps the way home visible, and hiding the name by slot is
                // what the collapsed rail already does, so both states clip the
                // same element. `BrandMark` carries the full name as its
                // accessible name either way.
                scopeLabel && "[&_[data-slot=brand-name]]:hidden",
              )}
            >
              {brand}
              {scopeLabel && (
                <span
                  data-slot="shell-scope-label"
                  className="min-w-0 truncate font-medium text-kumo-default"
                >
                  {scopeLabel}
                </span>
              )}
            </div>
          </header>
        )}
        {/* Banners are the first thing in the content column and sit *outside*
            the scroll container: an impersonation banner is a security signal
            and must not scroll away with the content. */}
        {banners && (
          // The banner's *content* aligns with the content column while its
          // background still spans the full width.
          //
          // Banners used to be a full-width strip with its own padding while
          // main was centred within a maximum width, so on a wide screen the
          // banner's text began nearly half a screen away from the page title,
          // and the two did not read as parts of the same page. The offset
          // varies with viewport width, so it is nearly invisible in a narrow
          // development window and obvious above about 1440px.
          //
          // The bottom border follows the sticky-border rule: this block is
          // pinned outside the scroll region, and without a divider its tinted
          // background blurs into the content area below.
          //
          // An empty slot has to be judged by CSS rather than by truthiness. The
          // caller passes a fragment containing a conditional banner and an
          // announcement banner that returns null when there is nothing to
          // announce — and a fragment element is always truthy. A truthiness
          // guard therefore opened a 25px blank band at the top of every page
          // with no announcement. Whether there is anything is only known after
          // rendering, so the criterion is delegated to `:has()` on an empty
          // child.
          <div className="shrink-0 border-b border-kumo-line bg-kumo-base has-[>div:empty]:hidden">
            <div className={cn(CONTENT_COLUMN, "grid gap-2 py-3")}>{banners}</div>
          </div>
        )}
        {/* The scroll container and the content column's width limit are kept
            separate: the overflow belongs to the outer element so the scrollbar
            sits at the viewport's right edge. Put it on main and the scrollbar
            appears in the middle of the content column. */}
        <div className="min-h-0 flex-1 overflow-y-auto">
          {/* The container has exactly one width. Content that needs to be
              narrower narrows itself through FormColumn; the shell no longer
              picks a width per page. The side rail belongs to the page through
              PageRail, so the shell takes no aside. The geometry comes from
              CONTENT_COLUMN — the same definition the banner area reads. */}
          <main
            ref={mainRef}
            id="main-content"
            tabIndex={-1}
            className={cn(
              CONTENT_COLUMN,
              "py-[var(--flb-content-pad-block,1.25rem)] focus:outline-none",
            )}
          >
            {children}
          </main>
        </div>
        <div className="shrink-0 border-t border-kumo-line bg-kumo-base has-[>div:empty]:hidden">
          <div className={CONTENT_COLUMN} ref={setActionDockTarget} />
        </div>
        <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
          {announcement}
        </div>
      </div>
    </PageActionDockTargetProvider>
  );
}

/**
 * The element carrying the page header's description.
 *
 * Plain text gets a `<p>`, anything else a `<div>`. The description is typed as
 * ReactNode, so a caller may legitimately pass a component — one detail page
 * passes a clipboard-text component, which renders a div. A block-level element
 * inside a `<p>` is invalid HTML: the browser closes the paragraph early, and
 * React reports a hydration error about a div not being allowed inside a p.
 *
 * This is deliberately not written as a comment telling callers to avoid
 * block-level elements. That would hand the invariant to the caller when the
 * type system cannot enforce it, since ReactNode accepts anything. The criterion
 * lives here instead, and callers need not know about it.
 */
function PageHeaderDescription({ children }: { children: ReactNode }) {
  const className = "max-w-3xl text-base text-kumo-subtle";
  // Only a string or a number is certainly paragraph content; any element is
  // treated as potentially containing something block-level.
  return typeof children === "string" || typeof children === "number" ? (
    <p className={className}>{children}</p>
  ) : (
    <div className={className}>{children}</div>
  );
}

/**
 * The shape of a breadcrumb trail: two levels, all strings. Both of those are
 * invariants.
 *
 * - Strings rather than ReactNode, because `current` has to be the record's
 *   plain name. A detail page's title is a rich node carrying a slug and a
 *   status badge, and reusing it here would push a badge into the navigation
 *   row. Narrowing the type hands that invariant to the compiler instead of
 *   writing it in a comment and hoping the caller reads it — the same stance as
 *   the page header's description element above.
 * - Two levels only, because every breadcrumb trail that exists is two levels
 *   deep, routes go at most three segments deep, and the parent is always the
 *   corresponding list page. No array shape is reserved for a third level: when
 *   one genuinely appears, change this — by then it will be clear what the third
 *   level means.
 */
export type PageHeaderBreadcrumbs = {
  /** Path of the ancestor list page. It has to be a real destination, never `#`
   * or an empty string. */
  parentHref: string;
  /** The ancestor's name, taken from the same message the sidebar uses. */
  parentLabel: string;
  /** The current record's name. While the data is still loading, pass the
   * loading text. */
  current: string;
};

/**
 * PageHeader is the page header every page shares: breadcrumbs, title, actions,
 * then the record navigation, in that order when all four are present.
 *
 * Whether to pass `breadcrumbs` follows a criterion rather than page-by-page
 * preference: if the sidebar's active state already answers "where am I", do not
 * pass them. One application derives them from its route registry, so the
 * decision does not rest with the caller; the other produces no record layer
 * structurally and passes them nowhere.
 *
 * `recordNav` describes the aspects of the record this header names. They belong
 * to the header rather than to the page body so that the identity and the
 * aspects of one thing cannot drift apart by a hand-written margin, and so that
 * a page cannot put them below its first section by mistake. Pages whose header
 * names an *area* rather than a record leave it unset and take a `SectionPage`
 * rail instead; the two are picked by the criterion documented on `RecordNav`.
 *
 * It takes the props rather than a node so that the slot accepts nothing else —
 * see the note on `SectionPage` for why that rule is given to the compiler.
 */
export function PageHeader({
  title,
  description,
  breadcrumbs,
  actions,
  recordNav,
}: {
  title: ReactNode;
  description?: ReactNode;
  breadcrumbs?: PageHeaderBreadcrumbs;
  actions?: ReactNode;
  recordNav?: RecordNavProps;
}) {
  return (
    <div className="grid gap-[var(--flb-page-header-gap,0.75rem)]">
      {breadcrumbs && (
        // The breadcrumbs component appears exactly once, here; a lint rule keeps
        // application code from using it directly. Its link goes through
        // whichever link provider the application installed, and internally it
        // passes only the (type-deprecated) destination prop and no href — every
        // link adapter falls back across both, or all breadcrumb links collapse
        // to "/".
        <Breadcrumbs size="sm">
          <Breadcrumbs.Link href={breadcrumbs.parentHref}>
            {breadcrumbs.parentLabel}
          </Breadcrumbs.Link>
          <Breadcrumbs.Separator />
          <Breadcrumbs.Current>{breadcrumbs.current}</Breadcrumbs.Current>
        </Breadcrumbs>
      )}
      <div className="flex flex-col items-stretch justify-between gap-3 sm:flex-row sm:items-start">
        <div className="grid min-w-0 gap-1.5">
          <Text variant="heading2" as="h1">
            {title}
          </Text>
          {description && <PageHeaderDescription>{description}</PageHeaderDescription>}
        </div>
        {actions && (
          // Named so a test can ask what is in this row. What must not be in it
          // is a state badge: it is not pressable, it wrapped along with the
          // buttons on a narrow viewport, and a reader on a screen reader met it
          // inside the action group. State belongs in `title`.
          <div
            data-slot="page-header-actions"
            className="flex shrink-0 flex-wrap items-center gap-2 sm:justify-end"
          >
            {actions}
          </div>
        )}
      </div>
      {recordNav && <RecordNav {...recordNav} />}
    </div>
  );
}

/** usePageTitle sets document.title per page. An empty title leaves it alone, so
 * it can wait until the data has arrived. */
export function usePageTitle(title: string | undefined) {
  useEffect(() => {
    if (title) document.title = title;
  }, [title]);
}

/**
 * The message key for the application's display name.
 *
 * It is a context rather than a parameter on every call site because the main
 * call sites are not in the shell at all: each page in the shared staff feature
 * package sets its own title, and that package is used by more than one shell
 * and should not know which product it has been mounted under — the same
 * reasoning as the rest of the host contract.
 *
 * Different shells do carry different display names, and that is deliberate
 * rather than an oversight: what each one administers is different, so what each
 * one is called should be too.
 */
const AppNameKeyContext = createContext<MessageKey>("appAdmin");

export function AppNameProvider({
  messageKey,
  children,
}: {
  messageKey: MessageKey;
  children: ReactNode;
}) {
  return <AppNameKeyContext.Provider value={messageKey}>{children}</AppNameKeyContext.Provider>;
}

/** The current application's localised display name. It is the single source for
 * the sidebar brand, the authentication pages and the document title. */
export function useAppName(): string {
  const { t } = useI18n();
  return t(useContext(AppNameKeyContext));
}

/**
 * useAdminTitle sets the document title for administrative pages, as
 * `<page> · <application>`.
 *
 * The application name comes from the dictionary rather than being written in
 * place: the sidebar brand uses the very same message, and two copies means a
 * rename will miss one — and the one it misses is visible only on a browser tab.
 *
 * It lives in the shared package because the shared staff feature package needs
 * it too. Copying it there would work, but that would be two implementations of
 * one title convention, and those drift.
 */
export function useAdminTitle(title: string | undefined) {
  const appName = useAppName();
  usePageTitle(title ? `${title} · ${appName}` : undefined);
}

/**
 * Centered is the full-screen centring container used by authentication pages
 * and loading states.
 *
 * A minimum height is right here: this is not the shell, it has no inner scroll
 * region to divide a height between, and it only has to fill at least one screen
 * and grow normally when the content exceeds it. The small viewport unit avoids
 * being jostled by a mobile address bar.
 */
export function Centered({ children }: { children: ReactNode }) {
  return (
    <main className="flex min-h-svh items-center justify-center bg-kumo-recessed p-4 text-kumo-default">
      {children}
    </main>
  );
}

const NEXT_THEME: Record<Theme, Theme> = { light: "dark", dark: "system", system: "light" };
const THEME_ICON = { light: SunIcon, dark: MoonIcon, system: CircleHalfIcon } as const;
const THEME_LABEL_KEY = {
  light: "themeLight",
  dark: "themeDark",
  system: "themeSystem",
} as const;

/**
 * LangThemeToggle is the compact language and theme switch, used only on pages
 * seen before signing in.
 *
 * After signing in, both belong to the account menu, where the theme is a
 * three-option radio group with the current value visible. A sign-in page has
 * neither the shell nor that menu, so this is the only way to change language
 * there. The two shapes differing is correct: there is no room to lay three
 * options out here, and the only thing that matters before signing in is getting
 * the interface into a language the reader can read.
 */
export function LangThemeToggle() {
  const { locale, setLocale, t } = useI18n();
  const { theme, setTheme } = useTheme();
  const Icon = THEME_ICON[theme];
  const next = NEXT_THEME[theme];
  const label = t("switchThemeTo", { theme: t(THEME_LABEL_KEY[next]) });
  return (
    <div className="flex items-center gap-1">
      <Button size="sm" variant="ghost" onClick={() => setLocale(locale === "en" ? "zh" : "en")}>
        {/* i18n-ignore */ locale === "en" ? "中文" : "EN"}
      </Button>
      <Button
        size="sm"
        variant="ghost"
        shape="square"
        icon={<Icon />}
        aria-label={label}
        title={label}
        onClick={() => setTheme(next)}
      />
    </div>
  );
}
