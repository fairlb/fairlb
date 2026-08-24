/**
 * Licensed third-party vendor artwork, keyed by the catalog slug.
 *
 * Empty by design, and adding an entry is deliberately two edits rather than
 * one: ADR-0145 decision 5 admits a third-party mark only once TRADEMARKS.md
 * records its source and licence, so dropping a file into this directory must
 * not be enough to ship it. A directory glob would have made it enough — and
 * would also have needed the bundler's ambient types inside a package that
 * compiles with `"types": []`.
 *
 * The value is the URL the consuming app serves the artwork at. See the README
 * in this directory for what makes a mark admissible.
 */
export const VENDOR_MARK_URLS: Record<string, string> = {};
