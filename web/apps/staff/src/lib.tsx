import { Sidebar } from "@cloudflare/kumo/components/sidebar";
import { communityStaffApi } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { AccountMenu, AppShell, BrandMark, useAdminTitle } from "@fairlb/ui";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { AdminIdentityProvider, useAdminMe, type AdminIdentity } from "./admin-session";
import {
  isAdminNavActive,
  isPathUnder,
  RequireSession,
  wrapProviders,
} from "@fairlb/app-composition";
import {
  ADMIN_SECTIONS,
  adminApp,
  adminPages,
  navPages,
  resolveAdminPage,
  HOME_PATH,
} from "./registry";

export type { AdminIdentity } from "./admin-session";
export { useAdminMe, useCurrentAdmin } from "./admin-session";

// The app's own facade. Pages import from here, and the implementations live in
// the shared packages — so moving one does not touch every page.
export { apiErrorMessage } from "@fairlb/api-client";
export { useAdminTitle };

/** RequireAdmin sends an unauthenticated visitor to /login, and otherwise
 * provides the shell and the identity context.
 *
 * The guard itself is `RequireSession` in `@fairlb/app-composition` — the three
 * shells shared its shape and had drifted on what counts as "not signed in".
 * This one used to send the reader to /login on *any* failure, so a 500 or a
 * dropped connection looked like a logout.
 *
 * The identity provider stays here and wraps the shared guard's children,
 * because it is this app's own context, not the guard's. */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const me = useAdminMe();
  return (
    <RequireSession session={me} loginPath="/login">
      {(identity: AdminIdentity) => (
        <AdminIdentityProvider value={identity}>
          {wrapProviders(adminApp.providers, <AdminShell me={identity}>{children}</AdminShell>)}
        </AdminIdentityProvider>
      )}
    </RequireSession>
  );
}

/**
 * Whether the current route falls under a page.
 *
 * The rule lives in `@fairlb/app-composition` and all three shells share it.
 * This app used to compare with a bare `startsWith`, which has no segment
 * boundary — `/keys-extra` would light up `/keys` — and which cannot see a page
 * mounted under a different noun from the entry that leads to it. The shared
 * implementation handles both, `navParent` included.
 */
function isActive(pathname: string, path: string): boolean {
  const page = adminPages.find((candidate) => candidate.path === path);
  return page ? isAdminNavActive(adminPages, page, pathname) : isPathUnder(pathname, path);
}

/**
 * The sidebar navigation.
 *
 * Three headings, and **no collapsible groups**. Those are two separate
 * decisions and only the second one is about size: a collapsible group costs a
 * click and buys back vertical space a nine-entry sidebar does not need, so
 * there are none. A heading, on the other hand, is free — and without them all
 * nine entries sat under "Gateway" while five of them (Requests, Usage,
 * Playground, API keys, Teams) are not gateway configuration. The reader was
 * being told something untrue by the only label on screen.
 *
 * Order comes from `ADMIN_SECTIONS` and, within a section, from the registry
 * array. There is no second place either is written down.
 *
 * `pathname` is a prop rather than read from a hook so a test can place the
 * component at any route without standing up a router and a session.
 */
export function AdminNav({ pathname }: { pathname: string }) {
  const { t } = useI18n();
  return (
    <>
      {ADMIN_SECTIONS.map((section) => {
        const pages = navPages.filter((page) => page.section === section.id);
        // An empty section would render a heading over nothing. It cannot happen
        // with today's registry, but a heading is a promise about what follows.
        if (pages.length === 0) return null;
        return (
          <Sidebar.Group key={section.id}>
            <Sidebar.GroupLabel>{t(section.labelKey)}</Sidebar.GroupLabel>
            <Sidebar.Menu>
              {pages.map((page) => (
                <Sidebar.MenuButton
                  key={page.path}
                  icon={page.icon}
                  tooltip={t(page.navKey)}
                  href={page.path}
                  active={isActive(pathname, page.path)}
                >
                  {t(page.navKey)}
                </Sidebar.MenuButton>
              ))}
            </Sidebar.Menu>
          </Sidebar.Group>
        );
      })}
    </>
  );
}

/**
 * The admin layout.
 *
 * Three things a larger admin shell has are deliberately absent here, each for
 * a reason about this app rather than "not yet":
 *   - **No command palette.** A palette earns its keystroke by indexing dozens
 *     of destinations and several kinds of entity. With this few destinations
 *     it would be a layer that answers a question nobody has.
 *   - **No new-build banner.** Detecting that the server is serving a newer
 *     build requires polling an endpoint this backend does not mount, and a
 *     poll that can only ever fail is worse than none. Automatic reload on a
 *     failed chunk preload is still installed; that part is purely
 *     client-side.
 *   - **No navigation items in the account menu, and no role chip.** There is
 *     no personal account page to link to, and with a single identity and a
 *     single role the chip would state what the reader already knows.
 */
function AdminShell({ me, children }: { me: AdminIdentity; children: ReactNode }) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const pathname = useLocation({ select: (l) => l.pathname });
  const logout = communityStaffApi.useCommunityLogout({
    mutation: { onSettled: () => void navigate({ to: "/login" }) },
  });
  const page = resolveAdminPage(pathname);

  return (
    <AppShell
      routeKey={pathname}
      routeAnnouncement={page ? t(page.navKey) : undefined}
      brand={
        <Link to={HOME_PATH} aria-label={t("appCommunityAdmin")}>
          <BrandMark edition="community" name={t("appCommunityAdmin")} />
        </Link>
      }
      nav={<AdminNav pathname={pathname} />}
      banners={adminApp.banners.map((Banner, index) => (
        <Banner key={index} />
      ))}
      account={<AccountMenu name={me.name} email={me.email} onSignOut={() => logout.mutate()} />}
    >
      {children}
    </AppShell>
  );
}
