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

当前已实现账号/组织与会话授权、钱包与平衡账本、预占结算、三渠道支付模拟、退款与对账基础、Token 凭据加密轮换、确定性路由、OpenAI 兼容 JSON/SSE 代理、Web Portal、桌面端 OIDC、领域事件投递、可观测性、Helm 和恢复 Runbook。真实支付、真实 New API 和生产集群演练仍按后续门禁推进。

## Web Portal

开发 Compose 启动后访问 `http://127.0.0.1:18105/`。登录使用普通在线 OIDC 会话（`openid profile email`），不请求 `offline_access`；服务端 Redis 会话和浏览器 Cookie 的期限遵循身份服务返回的刷新有效期。ID Token 经签名、issuer、audience、nonce 和身份绑定校验，浏览器仅收到不透明会话及 CSRF Cookie。

刷新使用带所有者的 Redis 锁与会话原值比较，退出删除后的会话不会被在途刷新重建。真实过期返回 401，无效同源/CSRF 请求返回 403，Redis 或刷新服务暂时故障返回 503 并保留重试会话。Portal 只转发管理 API，模型请求继续使用 Token Gateway 的数据面。

前端统一执行 `npm --prefix pinvou3-app run build:account-portal`；生成资源嵌入 `token-portal`。`npm --prefix pinvou3-app run test:account-platform-ui` 使用已安装的 Chrome、Chromium 或 Edge（可用 `CHROME` 指定路径），验证真实 React、BFF 客户端及二维码，API 响应受控；不能替代真实服务或 Safari 验收。

## 桌面端 OIDC

新账号入口由 `PINVOU_ACCOUNT_PLATFORM_ENABLED` 显式开启，默认关闭，关闭时原有 `llmapi_hub` 入口和模型路径不变。开启时还需配置：

- `PINVOU_ACCOUNT_OIDC_ISSUER`，或分别配置 `PINVOU_ACCOUNT_OIDC_AUTHORIZATION_URL` 与 `PINVOU_ACCOUNT_OIDC_TOKEN_URL`；
- `PINVOU_ACCOUNT_OIDC_CLIENT_ID`（公开客户端，必须启用 Authorization Code、PKCE S256 和 loopback redirect）；
- `PINVOU_ACCOUNT_SERVICE_URL`；
- `PINVOU_TOKEN_GATEWAY_URL`。

远程端点必须使用 HTTPS；仅本机开发允许 `http://127.0.0.1`/`localhost`。桌面端使用系统浏览器和临时 loopback 回调，Refresh Token 只写入 macOS Keychain、Windows Credential Manager 或 Linux Secret Service；系统凭据存储不可用时会明确拒绝登录，不降级写入设置文件。WebView 只接收登录状态和经过路由白名单限制的业务 JSON。

## Token Gateway 安全默认值

- `TOKEN_LOCAL_KEK_BASE64` 必须解码为 32 字节，仅允许开发/测试环境使用。
- `production`/`production-new` 会拒绝本地密钥 Provider，必须接入 KMS/Vault 实现。
- `TOKEN_PROVIDER_ALLOWED_HOSTS` 是逗号分隔的精确主机白名单；生产环境同时强制 HTTPS。
- Gateway 不重试模型调用。上游结果不确定、流式断开或缺失 usage 时保留 Hold 并进入 `REVIEW`。
- Hold 到期进入 `REVIEW` 并保留预占；明确的用量结算或未消费释放才能结束该预占。
- `TOKEN_GATEWAY_MAX_INPUT_TOKENS` 是计算预占的输入规模假设，当前不执行 tokenizer 计数。省略输出上限时 Gateway 会向上游补入默认 `max_tokens`；若实际用量费用超过预占，结算进入 `REVIEW`，不会超额扣款。真实模型的输入规模仍需后续供应商验收确认。
- Keycloak API 凭据须为 `typ=Bearer` 的已验签 access token；ID/Refresh token 不可代替。共享 JWKS 获取最多等待 5 秒，失败后可重新获取。
- 支付回调地址由 Billing 的 `BILLING_PAYMENT_CALLBACK_URL` 固定配置，客户端不能提交回调 URL。
- 模拟支付重试保持通知 ID 和付款事实不变，仅刷新签名时间；Billing 保留 5 分钟验签窗口，重复通知只允许 `timestamp` 改变。

`mock-newapi` 只属于开发 Compose 后端网络，不发布宿主机端口；可用于验证正常、限流、断流和缺失 usage 场景。

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

真实 New API 和真实支付测试需要明确 opt-in，普通测试不读取真实凭据或访问生产服务。

本地验证需要 Go 1.26.6 和 Node.js；契约测试检查生产 Go 类型序列化、注册路由请求校验及 Token eligible/错误响应。Token 仓储和上下文使用 fixture，这部分不执行付款或数据库端到端链路。设置 `PINVOU_ACCOUNT_TEST_DATABASE_URL` 可启用真实 PostgreSQL 回归，连接必须指向 loopback 测试实例；每个测试创建并清理独占随机 schema。未设置时这些数据库用例会明确跳过，不能将其算作数据库验证通过。CI 配置了该变量，实际执行这些用例。

设置 `PINVOU_PORTAL_TEST_REDIS_URL` 可启用真实 Redis 的一次性登录流、会话期限、刷新锁、退出竞态及连接中断回归。测试仅清理自己的随机键；未配置时明确跳过，CI 提供独立 Redis 并实际执行。

Integration 使用独立 Compose project 并在结束后删除测试卷；不得指向开发或生产 project。`e2e` 进一步执行三种模拟支付渠道、并发幂等、Hold 结算、部分退款和对账旅程；`fault` 验证 Billing 在支付回调期间重启后自动收敛，以及 NATS 停止期间本地账本事务不丢失。三者都在每次运行时只在内存中生成 Compose 凭据，不会写入仓库。真实 New API 和真实支付测试需要明确 opt-in，普通测试不读取真实凭据或访问生产服务。运维入口见 [`deploy/runbooks/`](deploy/runbooks/README.md)。
