# @fairlb/gateway-console-features

The feature package for the pages an organization sees about _its own_ traffic
through the gateway: usage, logs, the model catalog and the playground.

It is a workspace package rather than a folder inside one app because more than
one shell mounts this set. Its sibling `@fairlb/gateway-staff-features` holds the
other half — the provider, model and pricing pages, whose subject is how the
gateway itself is configured. A shell is assembled from the two.

**Dependency direction**: this package knows only `@fairlb/{api-client,i18n,ui}`
and Kumo. Everything that belongs to a shell (resolving an organization,
capability gating, how the title is composed, the settings-area context) is
injected through `GatewayConsoleHostProvider`, see host.tsx. The package knows no
app.

## 没有根桶

本包只从子路径导出（`package.json` 的 `exports`），没有 `.` 这一条。根桶此前存在过，
把每个子模块又导出一遍，而**零消费方**——每个 shell 都按子路径引它要的那一页。
一个没人走的第二条导入路径不会报错，只会让「从哪儿引」这件事出现两个答案。

姐妹包 `@fairlb/gateway-staff-features` 的根桶还在，那个还有一批浏览器夹具在用。
