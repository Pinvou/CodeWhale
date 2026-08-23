# Pinvou Account Platform

账号平台是独立于 New API 模型数据面的账号、组织、计费与模型访问控制服务集合。目标需求与当前实现分开维护，文档见：

- [`docs/README.md`](docs/README.md)
- [`docs/architecture.md`](docs/architecture.md)
- [`docs/requirements.md`](docs/requirements.md)
- [`docs/implementation-status.md`](docs/implementation-status.md)
- [`docs/role-system-design.md`](docs/role-system-design.md)
- [`docs/sms-verification-provider-design.md`](docs/sms-verification-provider-design.md)
- [`docs/call-flows.md`](docs/call-flows.md)
- [`docs/deployment.md`](docs/deployment.md)
- [`docs/adr/`](docs/adr/)

根目录下的旧架构设计与实施计划保留为历史方案基线，不作为当前完成度和实际部署形态的事实源。

## 服务

- `account-service`：用户、组织、成员、角色和活动上下文。
- `identity-service`：注册密码策略、短期不透明引用和身份供应商管理协议。
- `billing-service`：钱包、账本、支付、预占、退款和对账。
- `token-gateway`：Token 授权、路由、New API 代理和计费编排。
- `token-portal`：OIDC Authorization Code + PKCE、Redis 服务端会话和同源 CSRF 保护的 Web BFF；OIDC Token 不进入浏览器 JavaScript。
- `payment-simulator`：仅开发/测试使用的三渠道支付模拟器。

当前已实现账号/组织与会话授权、钱包与平衡账本、预占结算、三渠道支付模拟、新华三商城支付宝下单与支付查询、Token 凭据加密轮换、确定性路由、OpenAI 兼容 JSON/SSE 代理、独立 Web Portal、桌面端 OIDC、可靠领域事件、OpenTelemetry/Prometheus 可观测性、Helm 目标模板和恢复 Runbook。当前实际环境使用单机 Compose，尚不具备 Helm 目标形态所代表的高可用能力。新华三待支付订单由 Billing 在服务端持续查询，客户端关闭二维码页面后仍可按商城事实完成入账。商城合同暂未提供的退款、关闭、支付回调与账单对账接口保持关闭，运营台同步禁用这些操作，不以本地模拟冒充真实能力。

## 桌面端 OIDC

新账号入口由 `PINVOU_ACCOUNT_PLATFORM_ENABLED` 显式开启，默认关闭，关闭时原有 `llmapi_hub` 入口和模型路径不变。开启时还需配置：

- `PINVOU_ACCOUNT_OIDC_ISSUER`，或配置 `PINVOU_ACCOUNT_OIDC_TOKEN_URL`；
- `PINVOU_ACCOUNT_OIDC_CLIENT_ID`（公开客户端，桌面内登录必须启用 Direct Access Grants；Web Portal 仍使用 Authorization Code + PKCE）；
- `PINVOU_ACCOUNT_SERVICE_URL`；
- `PINVOU_IDENTITY_SERVICE_URL`；
- `PINVOU_TOKEN_GATEWAY_URL`。

远程端点必须使用 HTTPS；仅本机开发允许 `http://127.0.0.1`/`localhost`。桌面端支持原有账号/密码、中国大陆手机号/密码和手机号/短信验证码登录；新注册必须填写与原有账号体系共用的唯一用户名，先由 Identity Adapter 完成密码规则预检并取得短期不透明引用，再通过一次性滑块挑战及可配置短信 Provider 验证。Account 和短信 API 不接收原始密码。密码按原始 8 至 128 位可打印 ASCII 序列校验，至少包含一个字母和一个数字，允许中间半角空格但明确拒绝首尾空格；不 trim、不规范化、不转换大小写。密码不持久化、不写日志，Identity 创建完成或短期引用过期后释放。注册操作、唯一值预约和恢复阶段持久保存；桌面把短期查询凭据优先写入系统凭据库，重启后可继续轮询，Identity/Account 成功即返回可登录的 `PROVISIONING`，钱包由后台异步初始化，超过 SLA 转入 `REPAIR_REQUIRED`。Refresh Token 只写入 macOS Keychain、Windows Credential Manager 或 Linux Secret Service；系统凭据存储不可用时会明确拒绝登录，不降级写入设置文件。启用账号平台后，内置模型运行时直接使用同一 OIDC 会话的 Access Token 调用 `PINVOU_TOKEN_GATEWAY_URL/v1/chat/completions`，不会再把 New API 用户 Token 当作聊天身份。WebView 只接收登录状态和经过路由白名单限制的业务 JSON。注册成功后服务异步创建个人钱包和同手机号商城账号，完成前禁止平台模型与充值，本地独立模型不受影响。

## Token Gateway 安全默认值

- `TOKEN_LOCAL_KEK_BASE64` 必须解码为 32 字节，仅允许开发/测试环境使用。
- `production`/`production-new` 会拒绝本地密钥 Provider，必须接入 KMS/Vault 实现。
- `TOKEN_PROVIDER_ALLOWED_HOSTS` 是逗号分隔的精确主机白名单；生产环境同时强制 HTTPS。
- Gateway 不重试模型调用。上游结果不确定、流式断开或缺失 usage 时保留 Hold 并进入 `REVIEW`。
- 桌面聊天未显式发送 `Idempotency-Key` 时，Gateway 使用用户、OIDC 会话和请求体生成稳定键；相同请求的网络重试不会重复调用上游。
- 支付回调地址由 Billing 的 `BILLING_PAYMENT_CALLBACK_URL` 固定配置，客户端不能提交回调 URL。

`mock-newapi` 只属于开发 Compose 后端网络，不发布宿主机端口；可用于验证正常、限流、断流和缺失 usage 场景。

真实模型数据面使用独立部署的官方 New API。开发命令启用 `real-newapi` Compose profile，生产 Helm 以固定版本和镜像摘要部署仅供 Token Gateway 私网访问的 New API；初始化与升级流程见 [`deploy/runbooks/newapi-deployment.md`](deploy/runbooks/newapi-deployment.md)。

Token Gateway 在个人或组织上下文第一次调用尚无可用路由的受支持模型时，会幂等创建平台托管的默认 Token Account、加密凭据与路由。该账号不会出现在普通用户的 Token 管理接口中，也不能由普通用户修改；显式指定 Token Account 的请求不会触发默认路由回退。

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
