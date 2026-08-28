export { cn } from "./cn";
export { Button, type ButtonProps } from "./button";
export { CredentialInput, OneTimeCodeInput } from "./credential-input";
export { SecretInput } from "./secret-input";
export { ThemeProvider, type Theme } from "./theme";
export {
  Input,
  Textarea,
  Select,
  Combobox,
  Label,
  Field,
  Card,
  Alert,
  type CardProps,
  type CardTone,
} from "./form";
export { FormRow, FormActions } from "./form-row";
export { FormColumn } from "./form-column";
export { PageRail } from "./page-rail";
export {
  CheckboxGroupField,
  Checkbox,
  type CheckboxProps,
  type CheckboxGroupFieldProps,
  type CheckboxGroupOption,
} from "./checkbox-group-field";
export { ResponsiveResourceRow, type ResponsiveResourceRowProps } from "./responsive-resource-row";
export { BrandMark, type BrandMarkProps } from "./brand-mark";
export { VendorMark, type VendorMarkProps } from "./vendor-mark";
export {
  OperationalSummary,
  type OperationalSummaryItem,
  type OperationalSummaryProps,
} from "./operational-summary";
export {
  ResourceList,
  ResourceListItem,
  type ResourceListProps,
  type ResourceListItemProps,
} from "./resource-list";
export { useCursorList, useScopedCursor, LoadMoreButton } from "./cursor-list";
export { useCursorStack } from "./cursor-stack";
export {
  AppShell,
  PageHeader,
  usePageTitle,
  useAdminTitle,
  // Application display name: defaults to the generic admin label, and a shell
  // swaps in its own at the point where it is assembled.
  AppNameProvider,
  useAppName,
  Centered,
  type AppShellProps,
} from "./shell";
export { AuthShell, type AuthShellProps } from "./auth-shell";
// The one account menu. Both applications used to carry a word-for-word copy,
// which had already drifted into two different trigger heights. What is shared
// is the chrome — trigger geometry, identity block, language, theme, sign-out —
// while which pages appear in the menu stays with each application through the
// navItems and helpItems slots.
export {
  AccountAvatar,
  AccountMenu,
  InitialsAvatar,
  initialsOf,
  type AccountMenuProps,
} from "./account-menu";
export { SidebarIdentityRow, type SidebarIdentityRowProps } from "./sidebar-identity-row";
export type { PageHeaderBreadcrumbs } from "./shell";
export { SectionHeading, SettingsSection } from "./section-heading";
export { pickStrings, matchesQuery, PALETTE_MIN_QUERY } from "./search";
export { NotFoundState, ErrorState, Forbidden } from "./status-pages";
// Where a tab running an outdated build is handled: a failed preload only raises
// a banner, and only a route that fails to render triggers one automatic reload.
export {
  RouteErrorState,
  NewBuildBanner,
  useStaleBuild,
  installStaleBuildDetection,
  isStaleChunkError,
  shouldAutoReload,
  resetStaleBuildForTest,
  NEW_BUILD_QUERY_OPTIONS,
} from "./stale-build";
export { InlineEmpty } from "./inline-empty";
export { LoadingState } from "./loading-state";
export { useDebounced } from "./debounced";
export {
  formatMoney,
  formatNano,
  formatNanoFixed,
  formatNanoInput,
  nanoToMain,
  mainToNano,
  amountSchema,
  intSchema,
  validate,
} from "./money";
export { NanoPriceField } from "./nano-price-field";
export { sessionDeviceLabel } from "./session-device";
export { RowActions } from "./row-actions";
export { StatusBadge, type StatusTone } from "./status-badge";
export {
  TrendChart,
  RankBars,
  StatTile,
  axisTimeFormat,
  integerTickFormat,
  type TrendPoint,
  type RankItem,
  type StatDelta,
} from "./charts";
export { ConfirmDialog } from "./confirm-dialog";
export { FormDialog } from "./form-dialog";
export {
  ModalScaffold,
  DetailDrawer,
  type ModalScaffoldProps,
  type DetailDrawerProps,
} from "./modal-scaffold";
export { PageActionDock, type PageActionDockProps } from "./page-action-dock";
export { CopyAction, type CopyActionProps, type ClipboardStatus } from "./copy-action";
export { SecretRevealDialog } from "./secret-reveal-dialog";
// DataTable is the only way into a table here: type scale and the horizontal
// scroll container are settled in one place, so application code never imports
// the underlying table directly.
export { DataTable, type DataTableProps } from "./data-table";
export {
  LocalNav,
  PageContentsNav,
  type LocalNavItem,
  type LocalNavProps,
  type PageContentsNavItem,
} from "./local-nav";
export { RecordNav, type RecordNavItem, type RecordNavProps } from "./record-nav";
// Which item of a navigation the URL is on, and the segment-boundary rule it
// rests on. Both navigations and the route registry read the same one.
export { isPathUnder, resolveNavValue } from "./nav-path";
export { ContentsLayout, ListPage, RecordPage, SectionPage } from "./record-layout";
export {
  CommandPalette,
  SidebarSearchRow,
  useCommandPalette,
  type PaletteNavItem,
  type PaletteSource,
} from "./command-palette";
// The one implementation of "the row title is the navigation". Both applications
// and the shared feature packages use this same copy; see the note at the top of
// row-title-link.tsx.
export { RowTitleLink } from "./row-title-link";
// The implementation behind the design system's LinkProvider. Every shell mounts
// this same one, so the three ARIA invariants have a single implementation.
export { AppLink } from "./app-link";
// Time range filtering. Application pages and the shared feature packages'
// usage, log and overview panels all use this same copy, because the default
// range and the quantisation rule must exist in exactly one place.
export { RANGES, pickRange, useQuantizedRange, usePreviousRange, type RangeKey } from "./ranges";
