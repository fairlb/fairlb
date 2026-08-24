import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { useSidebar } from "@cloudflare/kumo/components/sidebar";
import { SignOutIcon, UserCircleIcon } from "@phosphor-icons/react";
import { useI18n, type Locale } from "@fairlb/i18n";
import type { ReactNode } from "react";
import { avatarStyle } from "./avatar-color";
import { cn } from "./cn";
import { SidebarIdentityRow } from "./sidebar-identity-row";
import { useTheme, type Theme } from "./theme";

/**
 * A language is always named in that language itself, and never goes through the
 * dictionary.
 *
 * The reason is that the reader of this particular item is precisely someone who
 * cannot read the current interface language: translating the name of a language
 * into the language they cannot read leaves them unable to recognise the option
 * they need. So each entry is its own endonym, identical under every interface
 * language.
 */
const LOCALE_LABEL: Record<Locale, string> = { en: "English", zh: "中文" }; // i18n-ignore: a language names itself, see above
const LOCALES: readonly Locale[] = ["en", "zh"];

/** The order of the three theme options: light, dark, then "follow the system". */
const THEMES: readonly Theme[] = ["light", "dark", "system"];
const THEME_LABEL_KEY = {
  light: "themeLight",
  dark: "themeDark",
  system: "themeSystem",
} as const;

/**
 * Aligns the highlight colour of radio rows with that of ordinary items.
 *
 * The design system uses different tokens for the two, verified in its build
 * output across versions. Without this override, moving down a single menu shows
 * two different hover colours — something neither the type system nor a test can
 * see and only the eye catches, so it is pinned here at the one place that owns
 * this menu.
 */
const RADIO_ITEM_CLASS = "data-highlighted:bg-kumo-overlay";

/**
 * Derives the avatar's initials from the name, or the email when there is none.
 *
 * How many characters are taken depends on the script: two for Latin letters,
 * one for CJK and emoji. This is not an aesthetic preference — two CJK
 * characters are about 28px wide at 14px and overflow a 24px circle.
 *
 * Slicing always goes through `Array.from`, by code point, rather than `slice`:
 * the latter cuts an emoji's surrogate pair in half and renders a replacement
 * character.
 */
export function initialsOf(name: string | undefined, email: string): string {
  const source = (name ?? "").trim() || (email.split("@")[0] ?? "").trim();
  if (!source) return "";
  const words = source.split(/\s+/).filter(Boolean);
  if (words.length >= 2) {
    return (firstChar(words[0]) + firstChar(words[1])).toLocaleUpperCase();
  }
  const chars = Array.from(source);
  const head = chars[0];
  // For an emoji, charCodeAt(0) yields the high surrogate (≥ 0xD800), which
  // lands on the "take one" side as well.
  const take = head !== undefined && head.charCodeAt(0) < 128 ? 2 : 1;
  return chars.slice(0, take).join("").toLocaleUpperCase();
}

function firstChar(word: string | undefined): string {
  return word ? (Array.from(word)[0] ?? "") : "";
}

/**
 * The avatar: the account's initials on a colour derived from the account, and a
 * generic icon only when there is neither a name nor an email to derive from.
 *
 * It renders **no remote image**, and that is the decision rather than an
 * omission (ADR-0146). Pointing this at an OAuth provider's `avatar_url` needs
 * two independent things to hold — the policy has to permit the origin, and the
 * viewer's network has to reach that CDN — and the second one is not ours to
 * guarantee. A photo that fails to load is a broken image with no fallback path,
 * so the slot is built on something that cannot fail instead.
 *
 * The initials matter. This slot exists to answer one question, "which account
 * am I in", and a shared generic icon makes every reader's trigger identical —
 * which is to say it answers nothing. The colour extends that to two accounts
 * whose initials happen to collide.
 *
 * Always `aria-hidden`: the name is carried by the adjacent text or by the
 * trigger's accessible name, so reading the initials again is noise.
 */
export function AccountAvatar({
  name,
  email,
  className,
}: {
  name?: string;
  email: string;
  className: string;
}) {
  return <InitialsAvatar text={name} seed={email} className={className} />;
}

/**
 * The initials circle behind the account avatar, generalised so that an
 * organization can have one too (ADR-0202): the same derivation of initials
 * from a name, the same palette keyed on a stable identifier. For an account
 * the identifier is the email, which also serves as the fallback text; for an
 * organization it is the id, so that two organizations named alike still get
 * two colours.
 */
export function InitialsAvatar({
  text,
  seed,
  className,
}: {
  /** What the initials are taken from; falls back to the seed when empty. */
  text?: string;
  /** The identifier the colour is derived from. */
  seed: string;
  className: string;
}) {
  const initials = initialsOf(text, seed);
  if (!initials) {
    return <UserCircleIcon aria-hidden className={cn("shrink-0 text-kumo-subtle", className)} />;
  }
  return (
    <span
      aria-hidden
      // The background has to be a style attribute rather than a class: Tailwind
      // resolves class names statically, so a computed `bg-[…]` is purged from
      // the production build and the circle ships transparent — while staying
      // readable in development, where the class is generated on demand.
      //
      // The foreground rides along for a different reason. `text-white` works
      // (measured, not assumed), but it lets the pair drift apart: the palette's
      // contrast is only guaranteed against this one foreground, and a class in
      // a separate list is one edit away from disagreeing with it.
      style={avatarStyle(seed)}
      className={cn(
        "flex shrink-0 items-center justify-center rounded-full",
        "font-medium",
        className,
      )}
    >
      {initials}
    </span>
  );
}

export type AccountMenuProps = {
  /** Display name. When empty the whole block falls back to the email, and the
   * second line then does not repeat it. */
  name?: string;
  email: string;
  /** A metadata chip after the name, such as a role. Purely presentational; it
   * takes no part in the accessible name. */
  badge?: ReactNode;
  /**
   * Navigation items scoped to the person, rendered below the identity block and
   * above the preferences, bringing their own separator and group.
   *
   * When there are none, pass undefined and never an empty fragment: a fragment
   * is always truthy, so the guard below would still render a stray separator
   * and an empty group.
   *
   * Before putting anything here, answer this: is it *mine*? If it is not, it
   * belongs to some section of the product and not to this menu. Links must use
   * the menu's plain item with an href rather than its dedicated link item —
   * only the former goes through the link provider, and the latter reloads the
   * whole page.
   */
  navItems?: ReactNode;
  /** Items below the preferences and above sign-out, such as "send feedback".
   * The same emptiness constraint as navItems applies. */
  helpItems?: ReactNode;
  onSignOut: () => void;
};

/**
 * AccountMenu is the account menu at the bottom of the sidebar, shared by every
 * application (ADR-0083; moved from the top bar into the rail's footer by
 * ADR-0202, which changed where the trigger sits and what shape it has, and
 * nothing about what the menu contains).
 *
 * What is shared is the chrome, not the contents. Four things must agree
 * everywhere: trigger geometry, the identity block, language and theme, and
 * sign-out. There used to be two word-for-word copies, which had already drifted
 * into two different trigger heights occupying the same slot of the same bar. Which pages appear in the menu, by contrast, should differ by each
 * application's own criteria — one has a personal account page and another does
 * not — so that goes through the `navItems` slot rather than being hard-coded.
 * Looking alike ought to be the result of criteria agreeing, not a premise.
 *
 * Three shapes here are deliberate; read the reasons before changing them:
 *
 * 1. The identity block gives the name and the email on two lines, not one. One
 *    application used to print the same string in the trigger and immediately
 *    below it in the menu header — saying it twice — while the email, which is
 *    what actually distinguishes accounts, appeared nowhere. The other had only
 *    the email and nothing at all on the trigger. The two lines share a type
 *    size and are layered by colour and weight instead, because the content
 *    size rule fixes body text at 14px and forbids the smaller steps.
 *
 * 2. Language and theme are radio groups, not cycling buttons. The original was
 *    one row each that advanced on click: the current value was invisible — the
 *    theme icon never changed and the label described only the next state — and
 *    getting from light to system meant passing through dark. Worse, the two
 *    rows read in opposite moods, a noun stacked above a verb phrase, for what
 *    is the same kind of action. A radio group lays all the options out at once,
 *    ticks the current one, and brings the correct menu-item-radio role and
 *    checked state for free.
 *
 * 3. The content has a fixed width. Menu width used to be decided by the length
 *    of the email, so one long address stretched three short items into an
 *    enormous menu. A fixed width plus truncation on both lines settles it.
 *
 * The menu's Label must be wrapped in the menu's Group: the label is the
 * underlying primitive's group label, which reads the group context while
 * rendering and throws outright when there is none. In a production build that
 * surfaces as an opaque error code, and the root error boundary replaces the
 * whole tree with a generic failure page. The crash happens the moment the menu
 * opens rather than on any particular page, and the top bar is present on every
 * signed-in page — which is why the browser test for this actually opens it.
 */
export function AccountMenu({
  name,
  email,
  badge,
  navItems,
  helpItems,
  onSignOut,
}: AccountMenuProps) {
  const { locale, setLocale, t } = useI18n();
  const { theme, setTheme } = useTheme();
  const { isMobile } = useSidebar();
  const primary = name?.trim() || email;
  // When the primary line is already the email — an account with no display name
  // set — the secondary line does not repeat it.
  const secondary = primary === email ? undefined : email;

  return (
    <DropdownMenu>
      <DropdownMenu.Trigger
        render={(props) => (
          // The trigger is the sidebar's identity row: avatar, name, email. Both
          // lines are visible at a glance, which is what answers "which account
          // am I in" — the email being what actually tells two accounts apart.
          // Collapsed, only the avatar remains and the row's tooltip carries
          // the name; the accessible name covers that state for a reader who
          // cannot see the tooltip.
          <SidebarIdentityRow
            {...props}
            avatar={<AccountAvatar name={name} email={email} className="size-6" />}
            primary={primary}
            secondary={secondary}
            tooltip={primary}
            caret
            aria-label={t("accountMenu", { name: primary })}
          />
        )}
      />
      {/* The menu opens beside the rail on a desktop and above the row inside
          the mobile sheet; aligned to the start so that its top edge meets the
          row's, whichever side it is on. */}
      <DropdownMenu.Content align="start" side={isMobile ? "top" : "right"} className="w-64">
        <DropdownMenu.Group>
          <DropdownMenu.Label className="flex items-center gap-2.5 px-2 py-2 font-normal">
            <AccountAvatar name={name} email={email} className="size-8" />
            {/* min-w-0 is what makes truncation possible: without it a grid child
                sizes to its content, and a long email breaks the fixed width. */}
            <span className="grid min-w-0 gap-0.5">
              <span className="flex min-w-0 items-center gap-1.5">
                <span className="truncate font-medium text-kumo-default">{primary}</span>
                {/* The chip must not shrink: on this line only the name should
                    compress, since it is the one that truncates. A squeezed chip
                    turns "Superadmin" into "Superadm…", and a role is exactly
                    the kind of short word that changes meaning when it loses a
                    character. */}
                {badge && <span className="shrink-0">{badge}</span>}
              </span>
              {secondary && <span className="truncate text-kumo-subtle">{secondary}</span>}
            </span>
          </DropdownMenu.Label>
        </DropdownMenu.Group>

        {navItems && (
          <>
            <DropdownMenu.Separator />
            <DropdownMenu.Group>{navItems}</DropdownMenu.Group>
          </>
        )}

        <DropdownMenu.Separator />
        {/* Radio rows are always inset, or the text in the menu does not line up:
            an item with an icon starts its text 32px in (8px of padding, a 16px
            icon, 8px of margin), while a radio row has no icon and starts at
            8px — so an ordinary entry and a theme option would each begin at a
            different place, an icon's width apart. The inset prop is exactly
            that 32px and exists for this.
            The group labels are deliberately *not* inset: they are section
            headings and belong further out than the entries. */}
        <DropdownMenu.Group>
          <DropdownMenu.Label>{t("language")}</DropdownMenu.Label>
          <DropdownMenu.RadioGroup
            value={locale}
            onValueChange={(value) => setLocale(value as Locale)}
          >
            {LOCALES.map((item) => (
              <DropdownMenu.RadioItem key={item} value={item} inset className={RADIO_ITEM_CLASS}>
                {LOCALE_LABEL[item]}
                <DropdownMenu.RadioItemIndicator />
              </DropdownMenu.RadioItem>
            ))}
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Group>
        <DropdownMenu.Group>
          <DropdownMenu.Label>{t("theme")}</DropdownMenu.Label>
          <DropdownMenu.RadioGroup
            value={theme}
            onValueChange={(value) => setTheme(value as Theme)}
          >
            {THEMES.map((item) => (
              <DropdownMenu.RadioItem key={item} value={item} inset className={RADIO_ITEM_CLASS}>
                {t(THEME_LABEL_KEY[item])}
                <DropdownMenu.RadioItemIndicator />
              </DropdownMenu.RadioItem>
            ))}
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Group>

        {helpItems && (
          <>
            <DropdownMenu.Separator />
            <DropdownMenu.Group>{helpItems}</DropdownMenu.Group>
          </>
        )}

        <DropdownMenu.Separator />
        {/* Sign-out is deliberately not marked as dangerous: red is reserved for
            irreversible actions such as deleting an account. Signing out can be
            undone by signing back in, and colouring it red spends that signal
            early, so that by the time a genuinely destructive entry arrives it
            no longer stands out. The separator above already says "this is the
            last thing here", which is enough. */}
        <DropdownMenu.Item icon={SignOutIcon} onClick={onSignOut}>
          {t("signOut")}
        </DropdownMenu.Item>
      </DropdownMenu.Content>
    </DropdownMenu>
  );
}
