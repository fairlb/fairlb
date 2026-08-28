import { CommandPalette as KumoCommandPalette } from "@cloudflare/kumo/components/command-palette";
import { MagnifyingGlassIcon } from "@phosphor-icons/react";
import { useI18n } from "@fairlb/i18n";
import { Sidebar } from "@cloudflare/kumo/components/sidebar";
import { useEffect, useState, type FunctionComponent } from "react";
import { matchesQuery } from "./search";

/**
 * One navigable destination in the palette.
 *
 * `breadcrumbs` says where the destination sits, so "Access tiers" arrives
 * carrying "it is under Gateway › Access & pricing". The path is matched against
 * the query as well: someone who knows the URL should not have to remember the
 * label.
 */
export type PaletteNavItem = {
  path: string;
  title: string;
  breadcrumbs: string[];
};

/**
 * A source of entity results — organizations, models, providers.
 *
 * A component rather than data, because each source owns its own fetching, its
 * own minimum query length and its own row shape, and renders nothing until it
 * has something to show.
 */
export type PaletteSource = FunctionComponent<{
  /** What the reader typed, already trimmed. */
  query: string;
  /** Hands a pick back to the palette to finish: close, then navigate. */
  onPick: (path: string) => void;
}>;

/**
 * useCommandPalette owns the palette's open state and the global shortcut.
 *
 * `preventDefault` is not optional: on Windows, Ctrl+K is the browser's own
 * search shortcut (Firefox throws focus into the bar beside the address bar), so
 * without it this shortcut is unavailable on half the machines it ships to.
 */
export function useCommandPalette() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() !== "k" || !(event.metaKey || event.ctrlKey)) return;
      event.preventDefault();
      setQuery("");
      setOpen(true);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);
  return {
    open,
    setOpen,
    query,
    setQuery,
    /** Open it and clear the previous query — a stale one reads as a stuck palette. */
    show: () => {
      setQuery("");
      setOpen(true);
    },
  };
}

/**
 * CommandPalette is the ⌘K palette both signed-in shells use.
 *
 * **It holds no index of its own.** The destinations arrive as `items` and the
 * entity results as `sources`, because each application's index is its own
 * registry — that is what keeps the palette from drifting away from the sidebar:
 * add a page, rename one, move it to another group, and the palette follows with
 * no second list to maintain.
 *
 * It lived in the operations console until the user console needed one too.
 * Copying it would have produced two answers to "how many characters before we
 * search", "does a pick close the palette first or navigate first", "what does an
 * empty result say" — and the day they disagreed, nothing would have reported it.
 *
 * `placeholder` stays with the caller for the opposite reason: it names the kinds
 * of thing *this* application holds, and the two shells hold different kinds.
 */
export function CommandPalette({
  open,
  setOpen,
  query,
  setQuery,
  items,
  sources = [],
  placeholder,
  onNavigate,
}: {
  open: boolean;
  setOpen: (open: boolean) => void;
  query: string;
  setQuery: (query: string) => void;
  /** Already filtered to what this reader may reach. */
  items: readonly PaletteNavItem[];
  sources?: readonly PaletteSource[];
  /** Names what this application's palette searches. */
  placeholder: string;
  /** Navigates to a picked path; the palette has closed by the time it runs. */
  onNavigate: (path: string) => void;
}) {
  const { t } = useI18n();
  const trimmed = query.trim();
  const nav = trimmed
    ? items.filter((item) => matchesQuery(trimmed, item.title, ...item.breadcrumbs, item.path))
    : items;

  const pick = (path: string) => {
    setOpen(false);
    onNavigate(path);
  };

  return (
    <KumoCommandPalette.Root
      open={open}
      onOpenChange={setOpen}
      // Kumo's prop is a mutable array; the palette never writes to it, and the
      // caller's list stays readonly so a source cannot mutate the index.
      items={[...nav]}
      value={query}
      onValueChange={setQuery}
      itemToStringValue={(item: PaletteNavItem) => item.title}
      onSelect={(item: PaletteNavItem) => pick(item.path)}
    >
      <KumoCommandPalette.Input autoFocus placeholder={placeholder} aria-label={placeholder} />
      <KumoCommandPalette.List>
        <KumoCommandPalette.Group>
          <KumoCommandPalette.GroupLabel>{t("paletteGroupPages")}</KumoCommandPalette.GroupLabel>
          {nav.map((item) => (
            <KumoCommandPalette.ResultItem
              key={item.path}
              value={item}
              title={item.title}
              breadcrumbs={item.breadcrumbs}
              icon={<MagnifyingGlassIcon />}
              onClick={() => pick(item.path)}
            />
          ))}
        </KumoCommandPalette.Group>
        {/* Entity results form their own groups; each renders null while the query
            is too short or matches nothing. */}
        {sources.map((Source, index) => (
          <Source key={index} query={trimmed} onPick={pick} />
        ))}
        {nav.length === 0 && (
          <KumoCommandPalette.Empty>{t("paletteEmpty")}</KumoCommandPalette.Empty>
        )}
      </KumoCommandPalette.List>
      <KumoCommandPalette.Footer />
    </KumoCommandPalette.Root>
  );
}

/**
 * The sidebar row that opens the palette.
 *
 * It reuses the sidebar's own menu button rather than drawing a search box: the
 * row geometry cannot drift from the navigation items beside it, and when the
 * rail collapses the row degrades to a magnifier with a tooltip for free.
 *
 * It brings its own group so that it is separated from the navigation sections
 * below it, which is where both shells put it — second in the rail, under the
 * brand, the position the reader's eye already goes to first.
 */
export function SidebarSearchRow({ onOpen }: { onOpen: () => void }) {
  const { t } = useI18n();
  return (
    <Sidebar.Group>
      <Sidebar.Menu>
        <Sidebar.MenuButton icon={MagnifyingGlassIcon} tooltip={t("paletteOpen")} onClick={onOpen}>
          <span className="flex-1 text-left">{t("paletteOpen")}</span>
          {/* The shortcut is written on the row rather than left to be discovered:
              a palette nobody knows the key for is a palette nobody opens. */}
          <kbd className="text-base text-kumo-subtle">⌘K</kbd>
        </Sidebar.MenuButton>
      </Sidebar.Menu>
    </Sidebar.Group>
  );
}
