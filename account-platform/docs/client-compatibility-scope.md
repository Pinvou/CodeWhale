# HTTP 客户端兼容层范围

当前客户端兼容策略的生产接线只有 Token gateway readiness：`installPersonalExecution` 从 `PINVOU_CLIENT_RELEASE_MANIFEST` 构造策略，并把 `Policy.Manifest` 提供给 readiness 响应。未将 `Policy.Wrap` 或 `Policy.WrapExcept` 全局挂载到 Account、Billing、Token 或 Portal 的 HTTP 路由。

因此，缺少 `PINVOU_CLIENT_RELEASE_MANIFEST` 时使用代码内置发布清单仍是既有行为；显式配置为无效 JSON 时启动失败。HTTP 请求的事实校验、权限、幂等和资金边界继续由各自处理器负责，readiness 清单不能替代这些校验。后续若要给具体路由启用版本拒绝，必须单独列出路由、方法、豁免和兼容承诺，并增加对应的真实 handler 守卫。
