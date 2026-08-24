import { Sidebar } from "@cloudflare/kumo/components/sidebar";
import { CaretUpDownIcon } from "@phosphor-icons/react";
import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { cn } from "./cn";

export type SidebarIdentityRowProps = Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> & {
  /** A 24px element: an avatar, an initials circle, an icon in a circle. It is
   * all that remains of the row when the sidebar is collapsed. */
  avatar: ReactNode;
  /** First line; truncates. */
  primary: string;
  /** Second line, subdued; truncates. */
  secondary?: string;
  /** Shown beside the collapsed rail, where the two lines are clipped away. */
  tooltip: string;
  /** A caret at the end of the row, for a row that opens a menu. Hidden when
   * collapsed — there is no room, and the tooltip already says what the row is. */
  caret?: boolean;
};

/**
 * SidebarIdentityRow is the shape shared by the two rows in the sidebar that
 * name a *thing* rather than a destination: the scope switcher in the header
 * and the account menu in the footer (ADR-0202).
 *
 * It is a thin layer over the design system's own menu button, and that is the
 * point: the row takes the rail's horizontal padding, hover and focus
 * treatment, and the collapsed-state clipping and tooltip from the same place
 * every navigation item does, so the three cannot drift into three different
 * row heights or three different hover colours. What it adds is the two-line
 * layout — a name, and the line that distinguishes two things with the same
 * name — which a navigation item never needs.
 *
 * The rest props land on the button itself, so the row can serve directly as a
 * dropdown menu's trigger: the menu hands it `aria-haspopup`, `aria-expanded`
 * and its click handler, and the caller adds the accessible name.
 *
 * Both lines are 14px, the content text size. The second line is layered by
 * colour and weight, not by a smaller size — the design rule forbids the
 * smaller steps for content text.
 *
 * It must be rendered inside a `Sidebar.Menu`, which the shell provides for
 * both slots, and inside a `Sidebar.Provider`, which the shell's root is.
 */
export const SidebarIdentityRow = forwardRef<HTMLButtonElement, SidebarIdentityRowProps>(
  function SidebarIdentityRow(
    { avatar, primary, secondary, tooltip, caret = false, className, ...rest },
    ref,
  ) {
    return (
      <Sidebar.MenuButton
        ref={ref}
        icon={avatar}
        tooltip={tooltip}
        className={cn("min-h-11 py-1.5", className)}
        {...rest}
      >
        <span className="grid min-w-0 flex-1 gap-0.5 text-left leading-5">
          <span className="truncate font-medium">{primary}</span>
          {secondary && <span className="truncate font-normal text-kumo-subtle">{secondary}</span>}
        </span>
        {caret && (
          <CaretUpDownIcon
            aria-hidden
            className="size-4 shrink-0 text-kumo-subtle group-data-[state=collapsed]/sidebar:hidden"
          />
        )}
      </Sidebar.MenuButton>
    );
  },
);
