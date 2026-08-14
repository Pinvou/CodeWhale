# ADR-009：服务端 HTTP 框架统一为 Gin

- 状态：Accepted
- 日期：2026-08-14

## 背景

账号平台最初直接使用 Go `net/http` 的 `ServeMux`。服务增多后，路由组、认证中间件、方法不匹配处理和 Panic Recovery 需要统一约束，同时不能破坏 Token Gateway 的 SSE 透传、现有 OpenAPI 路径和领域测试。

## 决策

所有账号平台服务端路由统一使用 Gin 1.12。Gin 负责路由匹配、路由组、Recovery 和认证中间件编排；领域 Handler 暂时保留标准 `http.ResponseWriter` 与 `*http.Request` 签名，通过公共适配器接入 Gin。

适配器必须把 Gin 路径参数写回 `Request.PathValue`，并把 Gin 路由模板转换为稳定、低基数的 `Request.Pattern`。出站 HTTP 客户端、SSE 底层接口、Prometheus Handler 和 `http.Server` 继续使用 Go 标准库；Gin 不替代这些能力。

构建使用 `nomsgpack` 标签，因为账号平台只提供 JSON/HTML/SSE，不开放 MsgPack API。

## 影响

- Account、Billing、Token Gateway、Token Portal、Payment Simulator、Mock New API 和 Event Relay HTTP 路由使用同一 Gin 配置。
- 认证失败、Panic Recovery、405 和动态路径指标具有统一行为。
- 领域代码不依赖 `gin.Context`，后续可逐个 Handler 迁移绑定与校验，而无需一次性改变业务层。
- Gin 版本升级必须运行全量 Go、契约、Integration 和 SSE 故障测试。

## 回滚

路由注册可以退回 `http.ServeMux`，领域 Handler、OpenAPI 合同和 Repository 接口无需变化。回滚时同时移除 Gin 中间件、适配器及依赖锁变更。
