import { CommandPalette } from "@cloudflare/kumo/components/command-palette";
import { gatewayStaffApi, type GatewayStaffTypes } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { PALETTE_MIN_QUERY } from "@fairlb/ui";
import type { FunctionComponent } from "react";

/**
 * The command palette's source of gateway entities: models and providers, found by
 * slug or display name.
 *
 * **Filtered locally, with no search request.** Both are catalog-sized data — tens
 * to hundreds of rows — neither list endpoint takes a query parameter, and both
 * lists are almost always already in the client cache by the time the palette opens.
 * Inventing search endpoints for them would not pay for itself. The day hundreds is
 * no longer enough, the query parameter goes on the endpoints and this becomes a
 * pass-through; the shape on the palette side does not have to change.
 */
/**
 * The shape of a palette entity source. **This package carries its own rather than
 * importing the shell's registry**: the structures happen to match, but the
 * dependency direction forbids a feature package knowing a shell's registry, and
 * each shell has its own.
 */
export type GatewayPaletteSource = FunctionComponent<{
  /** What the user typed, already trimmed. Each source decides for itself how many
   * characters to start on and whether to debounce. */
  query: string;
  /** Hands a pick back to the palette to finish: close, then navigate. */
  onPick: (path: string) => void;
}>;

const LIMIT = 5;

export const GatewayPaletteResults: GatewayPaletteSource = ({ query, onPick }) => {
  const { t } = useI18n();
  const enabled = query.length >= PALETTE_MIN_QUERY;
  // Only fetch once the palette is actually being used; a palette nobody opened
  // should cost no requests.
  // 检索发到服务端。此前是把整份目录拉下来在浏览器里 `matchesQuery` 过滤，
  // 目录分页之后那样只搜得到第一页——而命令面板正是「我记得有个模型叫 xxx」
  // 的入口，最需要搜到全量的恰恰是它。
  //
  // 换过来同时也更省：以前每次开面板都要拖 500 条模型 + 200 个供应商，
  // 现在两边各要 LIMIT 条。
  // models 只有搜索、还没有游标（ADR-0187：它的四个整集编辑面要先改造），
  // 所以这里仍要自己截断——但截的是**服务端已经筛过的结果**，不再是整份目录。
  const models = gatewayStaffApi.useListGatewayModels({ q: query }, { query: { enabled } });
  const providers = gatewayStaffApi.useListGatewayProviders(
    { q: query, limit: LIMIT },
    { query: { enabled } },
  );

  if (!enabled) return null;

  const modelHits = (models.data?.items ?? []).slice(0, LIMIT);
  const providerHits = providers.data?.items ?? [];

  if (modelHits.length === 0 && providerHits.length === 0) return null;

  return (
    <>
      {modelHits.length > 0 && (
        <CommandPalette.Group>
          <CommandPalette.GroupLabel>{t("navGatewayModels")}</CommandPalette.GroupLabel>
          {modelHits.map((m: GatewayStaffTypes.GatewayModel) => (
            <CommandPalette.ResultItem
              key={m.id}
              value={`model:${m.id}`}
              title={m.display_name || m.slug}
              description={m.display_name ? m.slug : undefined}
              breadcrumbs={[t("navSectionGateway"), t("navGatewayModels")]}
              onClick={() => onPick(`/gateway/models/${m.id}`)}
            />
          ))}
        </CommandPalette.Group>
      )}
      {providerHits.length > 0 && (
        <CommandPalette.Group>
          <CommandPalette.GroupLabel>{t("navGatewayProviders")}</CommandPalette.GroupLabel>
          {providerHits.map((p: GatewayStaffTypes.GatewayProvider) => (
            <CommandPalette.ResultItem
              key={p.id}
              value={`provider:${p.id}`}
              title={p.name || p.slug}
              description={p.name ? p.slug : undefined}
              breadcrumbs={[t("navSectionGateway"), t("navGatewayProviders")]}
              onClick={() => onPick(`/gateway/providers/${p.id}`)}
            />
          ))}
        </CommandPalette.Group>
      )}
    </>
  );
};
