# HTTP 客户端兼容层范围

当前客户端兼容策略的生产接线只有 Token gateway readiness：`installPersonalExecution` 从 `PINVOU_CLIENT_RELEASE_MANIFEST` 构造策略，并把 `Policy.Manifest` 提供给 readiness 响应。`Policy.Wrap` 或 `Policy.WrapExcept` 未挂载到 Account、Billing、Token 或 Portal 的生产 HTTP 路由。

因此，缺少 `PINVOU_CLIENT_RELEASE_MANIFEST` 时使用代码内置发布清单仍是既有行为；显式配置为无效 JSON 时，消费该策略的 Token gateway 管理运行入口在启动 HTTP 前失败。独立 personal profile 不读取这份清单，也不注册该 readiness 链路。HTTP 请求的事实校验、权限、幂等和资金边界继续由各自处理器负责，readiness 清单不能替代这些校验。后续若要给具体路由启用版本拒绝，必须单独列出路由、方法、豁免和兼容承诺，并增加对应的真实 handler 守卫。

生产入口清单：核对 `cmd/` 下全部命令目录及 `internal/` 的生产 Go 源码后，只有 `token-gateway/personal_execution.go` 调用 `clientcompat.FromEnv` 并将其 Manifest 接入 readiness。Account service 另通过 `WithClientCompatibility` 配置内部服务版本查询，不调用该策略的 HTTP wrapper。`internal/platform/clientcompat/policy.go` 中 `WrapExcept` 对 `Wrap` 的内部委托属于库实现，不是生产路由挂载。

验证分别覆盖三层：`clientcompat` 的 FromEnv 回归实际执行无配置默认、合法显式清单和损坏清单拒绝；范围守卫按已登记的选择器及 `FromEnv` 引用扫描 `cmd/` 与 `internal/` 的生产 Go 源码，这是静态模式守卫，不是 Go 类型或控制流证明；已有 wrapper 单测只验证显式构造的 handler，不证明全服务装配。权威架构和实现状态文档采用同一边界。关联 #1954 的范围澄清与回归；未来若改变 HTTP 装配，须另行确认受影响入口和兼容承诺，并补真实选定路由验证，本次不启用或删除 wrapper。
