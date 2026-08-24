import { cn } from "./cn";
import { createLink } from "@tanstack/react-router";
import { type AnchorHTMLAttributes, forwardRef } from "react";

/**
 * RowTitleLink is the only entry point for "the row title is the navigation".
 *
 * Once navigation was collapsed onto the title, discoverability rested entirely
 * on one colour signal — and three things undermined that signal: the row had no
 * hover feedback, since the table row component offers only default and selected
 * variants; the hit area was only as wide as the text, roughly 30px against a
 * 1250px row; and the link was set in a monospace face, which in a console
 * already means "an identifier you can copy".
 *
 * Two things are settled here:
 *
 * 1. Styling changes in one place. Five row-title links previously fell into two
 *    camps, one medium-weight with an underline offset and one monospace with
 *    none. The link carries no monospace of its own: that face belongs to the
 *    cell holding it, leaving colour and weight to carry the link semantics
 *    alone.
 * 2. The hit area fills the cell. An absolutely positioned pseudo-element covers
 *    the cell's padding and row height, which requires the cell itself to be
 *    positioned — call sites pass a `relative` class. This is still a real
 *    anchor, so middle-click, copy link address, the context menu and keyboard
 *    tabbing all keep working, and it does not trip the rule against clickable
 *    table rows because there is no click handler. The scope is the identity
 *    column only; checkbox columns and end-of-row actions are unaffected.
 *
 * It is built with the router's link factory rather than by forwarding props by
 * hand, because the factory preserves type inference for the destination and its
 * parameters, which hand-forwarding degrades to plain strings.
 *
 * It lives in the shared UI package rather than in an application because both
 * the applications' own pages and the shared feature packages need it, and the
 * former may not import the latter. Point 1 above is exactly why there must be
 * only one copy: splitting it in two recreates two of the five variants that
 * were collapsed in the first place.
 *
 * The router is therefore a peer dependency of this package: the link factory
 * has to use the same router instance as the host application, and only a peer
 * dependency guarantees that.
 */
const RowTitleAnchor = forwardRef<HTMLAnchorElement, AnchorHTMLAttributes<HTMLAnchorElement>>(
  function RowTitleAnchor({ className, ...props }, ref) {
    return (
      <a
        ref={ref}
        className={cn(
          "font-medium text-kumo-info underline-offset-4 hover:underline",
          "after:absolute after:inset-0",
          className,
        )}
        {...props}
      />
    );
  },
);

export const RowTitleLink = createLink(RowTitleAnchor);
