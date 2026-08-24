import { expect, test } from "vitest";
import { createGatewayConsoleModule } from "./module";
import type { GatewayConsoleHost } from "./host";

/**
 * 目的地表与路由表说的必须是同一批路径。
 *
 * 这两张表要的东西不同（门控与侧栏 vs 组件与 `validateSearch`，理由见
 * `ConsoleRoute` 的文档），但**路径**在两处指同一页：路由路径机械地等于
 * `/orgs/$orgId` 接上目的地路径。
 *
 * 此前两处各手写一遍。改一处不改另一处的表现是：那一页照常打得开，但侧栏不再
 * 高亮它——因为「哪个目的地拥有当前路径」是按目的地表算的。没有报错、没有构建
 * 失败、没有日志。所以判据必须是一条用例（ADR-0192）。
 *
 * gate-honesty: 无跳过路径。两条自检先跑——两张表任一为空、或前缀假设不再成立，
 * 都让用例失败而不是报「没发现问题」。
 */

const ORG_ROUTE_PREFIX = "/orgs/$orgId";

/** 这个模块只在装配时用 host 取数据，构造它本身不调任何一个字段。 */
const HOST = {} as GatewayConsoleHost;

test("every org-scoped route path is a declared destination path", () => {
  const module = createGatewayConsoleModule(HOST);
  const destinations = module.destinations.map((d) => d.path);
  const routes = (module.routes ?? []).map((r) => r.path);

  // ── 自检：两张表塌成空集时，下面的断言会真空通过 ──────────────────
  expect(destinations.length, "目的地表是空的").toBeGreaterThan(3);
  expect(routes.length, "路由表是空的").toBeGreaterThan(3);
  // 前缀假设本身也要成立：全部路由都不带这个前缀时，下面筛出来的是空集。
  expect(
    routes.filter((p) => p.startsWith(ORG_ROUTE_PREFIX)).length,
    `没有一条路由以 ${ORG_ROUTE_PREFIX} 开头：前缀变了，本用例在检查一个不存在的东西`,
  ).toBeGreaterThan(3);

  // ── 判据 ────────────────────────────────────────────────────────────
  const orphans = routes
    .filter((p) => p.startsWith(ORG_ROUTE_PREFIX))
    .map((p) => p.slice(ORG_ROUTE_PREFIX.length))
    .filter((p) => !destinations.includes(p));
  expect(
    orphans,
    `这些路由挂了一个目的地表里没有的路径：${orphans.join(", ")}。` +
      "页面照常打得开，但侧栏不会高亮它。",
  ).toEqual([]);
});
