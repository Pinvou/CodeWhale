# Pinvou Account Platform

账号平台是独立于当前生产 New API 的新服务集合。架构与实施基线见：

- [`../docs/账号组织Token与计费系统-架构设计.md`](../docs/账号组织Token与计费系统-架构设计.md)
- [`../docs/账号组织Token与计费系统-实施任务与验证计划.md`](../docs/账号组织Token与计费系统-实施任务与验证计划.md)
- [`docs/adr/`](docs/adr/)

## 服务

- `account-service`：用户、组织、成员、角色和活动上下文。
- `billing-service`：钱包、账本、支付、预占、退款和对账。
- `token-gateway`：Token 授权、路由、New API 代理和计费编排。
- `token-portal`：OIDC Authorization Code + PKCE、Redis 服务端会话和同源 CSRF 保护的 Web BFF；OIDC Token 不进入浏览器 JavaScript。
- `payment-simulator`：仅开发/测试使用的三渠道支付模拟器。

当前已实现账号/组织与会话授权、钱包与平衡账本、预占结算、三渠道支付模拟、退款与对账基础、Token 凭据加密轮换、确定性路由、OpenAI 兼容 JSON/SSE 代理、独立 Web Portal、桌面端 OIDC、可靠领域事件、OpenTelemetry/Prometheus 可观测性、生产 Helm 和恢复 Runbook。真实支付、真实 New API 和生产集群演练仍按实施计划的后续门禁推进。

## 桌面端 OIDC

新账号入口由 `PINVOU_ACCOUNT_PLATFORM_ENABLED` 显式开启，默认关闭，关闭时原有 `llmapi_hub` 入口和模型路径不变。开启时还需配置：

- `PINVOU_ACCOUNT_OIDC_ISSUER`，或配置 `PINVOU_ACCOUNT_OIDC_TOKEN_URL`；
- `PINVOU_ACCOUNT_OIDC_CLIENT_ID`（公开客户端，桌面内登录必须启用 Direct Access Grants；Web Portal 仍使用 Authorization Code + PKCE）；
- `PINVOU_ACCOUNT_SERVICE_URL`；
- `PINVOU_TOKEN_GATEWAY_URL`。

远程端点必须使用 HTTPS；仅本机开发允许 `http://127.0.0.1`/`localhost`。桌面端在应用内收集账号密码并通过 Rust 一次性提交给 Keycloak Token Endpoint；密码不持久化、不写日志，完成请求后即释放。Refresh Token 只写入 macOS Keychain、Windows Credential Manager 或 Linux Secret Service；系统凭据存储不可用时会明确拒绝登录，不降级写入设置文件。启用账号平台后，内置模型运行时直接使用同一 OIDC 会话的 Access Token 调用 `PINVOU_TOKEN_GATEWAY_URL/v1/chat/completions`，不会再把 New API 用户 Token 当作聊天身份。WebView 只接收登录状态和经过路由白名单限制的业务 JSON。注册、找回密码、第三方身份和复杂 MFA 不经过该密码接口，需使用相应的独立流程。

## Token Gateway 安全默认值

- `TOKEN_LOCAL_KEK_BASE64` 必须解码为 32 字节，仅允许开发/测试环境使用。
- `production`/`production-new` 会拒绝本地密钥 Provider，必须接入 KMS/Vault 实现。
- `TOKEN_PROVIDER_ALLOWED_HOSTS` 是逗号分隔的精确主机白名单；生产环境同时强制 HTTPS。
- Gateway 不重试模型调用。上游结果不确定、流式断开或缺失 usage 时保留 Hold 并进入 `REVIEW`。
- 桌面聊天未显式发送 `Idempotency-Key` 时，Gateway 使用用户、OIDC 会话和请求体生成稳定键；相同请求的网络重试不会重复调用上游。
- 支付回调地址由 Billing 的 `BILLING_PAYMENT_CALLBACK_URL` 固定配置，客户端不能提交回调 URL。

`mock-newapi` 只属于开发 Compose 后端网络，不发布宿主机端口；可用于验证正常、限流、断流和缺失 usage 场景。

真实模型数据面使用独立部署的官方 New API。开发命令启用 `real-newapi` Compose profile，生产 Helm 以固定版本和镜像摘要部署仅供 Token Gateway 私网访问的 New API；初始化与升级流程见 [`deploy/runbooks/newapi-deployment.md`](deploy/runbooks/newapi-deployment.md)。

## 本地命令

统一从仓库根执行：

```text
node scripts/account-platform.mjs verify
node scripts/account-platform.mjs dev up
node scripts/account-platform.mjs dev down
node scripts/account-platform.mjs test integration
node scripts/account-platform.mjs test e2e
node scripts/account-platform.mjs test fault
node scripts/account-platform.mjs test security
```

Integration 使用独立 Compose project 并在结束后删除测试卷；不得指向开发或生产 project。`e2e` 进一步执行三种模拟支付渠道、并发幂等、Hold 结算、部分退款和对账旅程；`fault` 验证 Billing 在支付回调期间重启后自动收敛，以及 NATS 停止期间本地账本事务不丢失。三者都在每次运行时只在内存中生成 Compose 凭据，不会写入仓库。真实 New API 和真实支付测试需要明确 opt-in，普通测试不读取真实凭据或访问生产服务。运维入口见 [`deploy/runbooks/`](deploy/runbooks/README.md)。
