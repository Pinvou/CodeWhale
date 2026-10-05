# Pinvou Account Platform

账号平台是独立于 New API 模型数据面的账号、组织、计费与模型访问控制服务集合。目标规划与当前实现分开维护：

- [文档索引](docs/README.md)
- [当前架构](docs/architecture.md)
- [目标需求规划](docs/requirements.md)
- [历史实现审计与当前边界](docs/implementation-status.md)
- [角色设计规划](docs/role-system-design.md)
- [短信供应商设计规划](docs/sms-verification-provider-design.md)
- [调用流程](docs/call-flows.md)
- [部署与运维](docs/deployment.md)
- [架构决策记录](docs/adr/)

父目录的原架构设计与实施计划保留方案背景。规划中的任务、部署目标和验收条件不能替代当前可执行契约，也不表示本波全部实现或已发布。

## 服务

- `account-service`：用户、组织、成员、角色和活动上下文。
- `identity-service`：注册密码策略、短期不透明引用和身份供应商管理协议。
- `billing-service`：钱包、账本、支付、预占、退款和对账。
- `token-gateway`：Token 授权、路由、New API 代理和计费编排。
- `token-portal`：OIDC Authorization Code + PKCE、Redis 服务端会话和同源 CSRF 保护的 Web BFF；OIDC Token 不进入浏览器 JavaScript。
- `payment-simulator`：仅开发/测试使用的三渠道支付模拟器。

组织只能由具备 Account 平台组织创建权限的后台管理员创建，并指定初始 Owner；普通用户没有自建组织入口。Keycloak Realm Role 不授予业务权限，Account 用户状态及当前有效授权共同约束后台访问。

## Web Portal

已继承的后台财务读取和个人用量明细以十进制字符串返回金额，避免浏览器 JSON number 丢失 BIGINT 精度；未知用量金额仍为 `null`。原有内部余额和用量状态接口保持原契约。后台用量输出显式字段，与管理页面的 snake_case 字段一致。

后台权限由 Account 原子权限码及明确的平台/组织作用域决定；授权起止、撤销和最后有效超级管理员保护由 Account 持久事实约束。后台成员变更与对应版本的成员事件在同一事务中提交。

自动用量补偿仅处理已持久化的 `SETTLEMENT_PENDING` 和 `REVIEW`。补偿回写绑定本次 claim 的代次与租约，过期批次项不再执行，外部调用截止于当前租约期限；没有明确消费证据时继续保护预占，超额证据保留人工核对。New API 的单渠道错误日志不能证明整个请求最终未消费。代理已持久化的明确释放意图可重试原 Billing 释放；Billing 已确认释放而本地记录失败时，只补齐本地终态。开发 mock 不提供真实 New API 消费日志，其模型输出不能代替真实供应商的补偿证据验收。

开发 Compose 启动后访问 `http://127.0.0.1:18105/`。登录使用普通在线 OIDC 会话（`openid profile email`），不请求 `offline_access`；服务端 Redis 会话和浏览器 Cookie 的期限遵循身份服务返回的刷新有效期。ID Token 经签名、issuer、audience、nonce 和身份绑定校验，浏览器仅收到不透明会话及 CSRF Cookie。

刷新使用带所有者的 Redis 锁与会话原值比较，退出删除后的会话不会被在途刷新重建。真实过期返回 401，无效同源/CSRF 请求返回 403，Redis 或刷新服务暂时故障返回 503 并保留重试会话。Portal 只转发管理 API，模型请求继续使用 Token Gateway 的数据面。

前端统一执行 `npm --prefix pinvou3-app run build:account-portal`；生成资源嵌入 `token-portal`。`npm --prefix pinvou3-app run test:account-platform-ui` 使用已安装的 Chrome、Chromium 或 Edge（可用 `CHROME` 指定路径），验证真实 React、BFF 客户端及二维码，API 响应受控；不能替代真实服务或 Safari 验收。

当前已实现账号/组织与会话授权、钱包与平衡账本、预占结算、三渠道支付模拟、新华三商城支付宝下单与支付查询、Token 凭据加密轮换、确定性路由、OpenAI 兼容 JSON/SSE 代理、独立 Web Portal、桌面端 OIDC、可靠领域事件、OpenTelemetry/Prometheus 可观测性、Helm 模板和恢复 Runbook。新华三待支付订单由 Billing 在服务端持续查询，客户端关闭二维码页面后仍可按商城事实完成入账。商城合同暂未提供的退款、关闭、支付回调与账单对账接口保持关闭，运营台同步禁用这些操作，不以本地模拟冒充真实能力。

## 桌面端 OIDC

A07 已把内置模型迁移到账户 Gateway 并退役旧入口及 `PINVOU_ACCOUNT_PLATFORM_ENABLED` 开关；本波保全该已审行为。本地独立模型继续保留。账号路径需配置：

- `PINVOU_ACCOUNT_OIDC_ISSUER`，或配置 `PINVOU_ACCOUNT_OIDC_TOKEN_URL`；
- `PINVOU_ACCOUNT_OIDC_CLIENT_ID`（公开客户端，桌面内登录必须启用 Direct Access Grants；Web Portal 仍使用 Authorization Code + PKCE）；
- `PINVOU_ACCOUNT_SERVICE_URL`；
- `PINVOU_IDENTITY_SERVICE_URL`；
- `PINVOU_TOKEN_GATEWAY_URL`。

远程端点必须使用 HTTPS；开发和测试服务可使用明确受控的 HTTP 例外，桌面 HTTP 入口限回环地址。桌面端支持原有账号/密码、中国大陆手机号/密码和手机号/短信验证码登录；新注册必须填写与原有账号体系共用的唯一用户名，先由 Identity Adapter 完成密码规则预检并取得短期不透明引用，再通过一次性滑块挑战及可配置短信 Provider 验证。Account 和短信 API 不接收原始密码。密码按原始 8 至 128 位可打印 ASCII 序列校验，至少包含一个字母和一个数字，允许中间半角空格但明确拒绝首尾空格；不 trim、不规范化、不转换大小写。密码不持久化、不写日志，Identity 创建完成或短期引用过期后释放。

注册操作、唯一值预约和恢复阶段持久保存；桌面 Rust 优先将查询凭据写入系统凭据库，成功持久化后可跨重启继续轮询。持久化失败时仅保留绑定注册事实和会话代次的进程内恢复记录，并明确提示重启后无法恢复；查询令牌不进入 WebView，也不写设置文件。Identity/Account 成功即返回可登录的 `PROVISIONING`，钱包由后台异步初始化，超过 SLA 转入 `REPAIR_REQUIRED`。钱包完成前禁止平台模型与充值，本地独立模型不受影响。

Refresh Token 只写入 macOS Keychain、Windows Credential Manager 或 Linux Secret Service；系统凭据存储不可用时明确拒绝登录，不降级写入设置文件。内置模型运行时使用同一 OIDC 会话的 Access Token 调用 `PINVOU_TOKEN_GATEWAY_URL/v1/chat/completions`。WebView 只接收登录状态和经过路由白名单限制的业务 JSON。企业商城是可选能力，未配置时不阻断注册、钱包或模型；配置时由独立客户映射任务同步已验证手机号，映射未就绪只关闭依赖它的新支付。

短信保留 `http` 的 `/send`、`/verify` 及服务 Token 契约；`aliyun`/`aliyun-managed` 使用厂商托管验证码，`http-generated` 显式启用平台生成及 HMAC 核验。同一手机号发送冷却跨用途共享。发送结果未知保留冷却；只有 Aliyun 完整官方前置拒绝、明确 `Success=false` 且无送达标识时解除冷却。

## Token Gateway 安全默认值

- `TOKEN_LOCAL_KEK_BASE64` 必须解码为 32 字节，仅允许开发/测试环境使用。
- `production`/`production-new` 会拒绝本地密钥 Provider，必须接入 KMS/Vault 实现。
- `TOKEN_PROVIDER_ALLOWED_HOSTS` 是逗号分隔的精确主机白名单；生产环境同时强制 HTTPS。
- Gateway 不重试模型调用。上游结果不确定、流式断开或缺失 usage 时保留 Hold 并进入 `REVIEW`。
- Hold 到期进入 `REVIEW` 并保留预占；自动超时不代表免单。平台管理员可对非执行中的调用明确作最终免单：Token 先持久化 actor/reason/hold 意图，Billing 行锁裁决免单或既有扣费的唯一赢家，Token 按权威结果收尾。执行中的 `UPSTREAM_ACTIVE` 拒绝免单，已扣费返回冲突。成功免单的两个领域均为 `RELEASED`，收费为零，真实用量与原计算证据保留，后续消费证据不能重扣同一调用。跨服务故障通过原补偿 worker 重试持久意图，普通自动释放不会自行生成管理员免单。
- `TOKEN_GATEWAY_MAX_INPUT_TOKENS` 是计算预占的输入规模假设，当前不执行 tokenizer 计数。省略输出上限时 Gateway 会向上游补入默认 `max_tokens`；若实际用量费用超过预占，结算进入 `REVIEW`，不会超额扣款。真实模型的输入规模仍需后续供应商验收确认。
- Keycloak API 凭据须为 `typ=Bearer` 的已验签 access token；ID/Refresh token 不可代替。共享 JWKS 获取最多等待 5 秒，失败后可重新获取。
- 桌面聊天由客户端按每次逻辑调用发送 `Idempotency-Key`；同一次调用的网络重试复用原键，重新发起或重新生成使用新键。Gateway 拒绝缺键请求，不自动生成键。
- 支付回调地址由 Billing 的 `BILLING_PAYMENT_CALLBACK_URL` 固定配置，客户端不能提交回调 URL。
- 模拟支付重试保持通知 ID 和付款事实不变，仅刷新签名时间；Billing 保留 5 分钟验签窗口，重复通知只允许 `timestamp` 改变。

`mock-newapi` 只属于开发 Compose 后端网络，不发布宿主机端口；可用于验证正常、限流、断流和缺失 usage 场景。

真实模型数据面使用独立部署的官方 New API。开发命令启用 `real-newapi` Compose profile，生产 Helm 以固定版本和镜像摘要部署仅供 Token Gateway 私网访问的 New API；初始化与升级流程见 [`deploy/runbooks/newapi-deployment.md`](deploy/runbooks/newapi-deployment.md)。

Token Gateway 在个人或组织上下文第一次调用尚无可用路由的受支持模型时，会幂等创建平台托管的默认 Token Account、加密凭据与路由。该账号不会出现在普通用户的 Token 管理接口中，也不能由普通用户修改；显式指定 Token Account 的请求不会触发默认路由回退。

默认配置只用于首次创建。后台已经持久化的账号禁用、凭据验证失败、路由降级或删除不会被后续模型调用自动覆盖；配置变更也不会自动重写已创建的账号。重新启用或调整已存在的账号应通过后台运营入口完成。

后台充值退款必须携带至少 16 字符的 `Idempotency-Key`：客户端为每次逻辑退款生成独立键，响应未知时以同一金额、原因和键重试。新增后台价格、台账和对账快照的金额读取均为精确十进制字符串；台账以 `posting_id` 标识每笔 posting，同一 journal 可以包含多笔钱包 posting。

运行状态中的 NewAPI 探针经既有 Token Gateway 管理鉴权中介调用配置内的精确 `GET /api/status`，沿用 Gateway 的 HTTPS、主机允许列表与 CA 信任。Portal 不获取 NewAPI 管理密钥或 CA，也不增加到其数据端口的网络权限。补偿计数或最近错误无法读取时，现有错误列表明确披露不可读取，不能据零计数判断任务已经全部收尾。

模型调用必须携带每次逻辑调用的 `Idempotency-Key`（16–128 个 UTF-8 字符）。同一次调用的网络重试复用原键；用户重新发起或重新生成同一提示词时使用新键。缺键返回 400，重复键返回 409 且不重复请求模型或结算，不再按用户、会话和请求体推导永久键。

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

本地验证需要 Go 1.26.6 和 Node.js；契约测试检查生产 Go 类型序列化、注册路由请求校验及 Token eligible/错误响应。Token 仓储和上下文使用 fixture，这部分不执行付款或数据库端到端链路。设置 `PINVOU_ACCOUNT_TEST_DATABASE_URL` 可启用真实 PostgreSQL 回归，连接必须指向 loopback 测试实例，测试角色须有 `CREATEDB` 权限；每个夹具从 `template0` 创建并清理独占随机数据库，内部仅使用自身 schema 与 `pg_catalog`，避免数据库级扩展被其他夹具的清理删除。未设置时这些数据库用例会明确跳过，不能将其算作数据库验证通过。CI 配置了该变量，实际执行这些用例。

设置 `PINVOU_PORTAL_TEST_REDIS_URL` 可启用真实 Redis 的一次性登录流、会话期限、刷新锁、退出竞态及连接中断回归。测试仅清理自己的随机键；未配置时明确跳过，CI 提供独立 Redis 并实际执行。

Integration 使用独立 Compose project 并在结束后删除测试卷；不得指向开发或生产 project。`e2e` 进一步执行三种模拟支付渠道、并发幂等、Hold 结算、部分退款和对账旅程；`fault` 验证 Billing 在支付回调期间重启后自动收敛，以及 NATS 停止期间本地账本事务不丢失。三者都在每次运行时只在内存中生成 Compose 凭据，不会写入仓库。真实 New API 和真实支付测试需要明确 opt-in，普通测试不读取真实凭据或访问生产服务。运维入口见 [`deploy/runbooks/`](deploy/runbooks/README.md)。

首次观察到渠道部分或全额退款而本地尚未入账的充值进入 `MANUAL_REVIEW`，不猜测原额或净额积分。创建恢复、主动同步、关闭返回及迟到成功通知都遵守该状态；已入账订单保留既有退款流程。

新个人 PLAN 的期限和积分按订单冻结的商品快照，从实际入账时起算；原 LEGACY 订单和普通余额不被补加期限。组织 OWNER/BILLING_ADMIN 的原四档普通充值在开发支付模拟器中保留，个人停售旧商品不重新开放，个人 PLAN 不可发放至组织钱包；企业支付适配器不被宣称支持原组织商品。

注册创建响应丢失或本地完成失败时，可用相同用户名和手机号重新完成滑块及短信验证。服务只按已持久化的专用注册意图及远端唯一标记恢复，不按用户名或手机号接管其他 Keycloak 用户。注册响应始终包含 `password_preserved` 布尔值；为 `true` 时保留首次提交的密码，本次重试密码不生效，请通过普通密码登录。软删除保留 Keycloak 身份与角色，因此旧用户名仍占用；手机号可以配新用户名注册，手机号密码及短信登录按当前 Account 身份解析，不选择同手机号的任意旧身份。
