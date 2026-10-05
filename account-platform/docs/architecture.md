# 账号平台当前架构

> 状态：基于当前代码和数据库迁移校准的实现快照，不替代目标需求
>
> 校准日期：2026-10-06
>
> 继承实现基线：A06 `c5f0a2bfab58287bac880ea1bb198ef73b50e39d`。旧 `75a29f8` 目标审计另见实现状态中的历史矩阵；本波客户端迁移以最终固定提交为准。
>
> 文档版本：以本文件所在 Git 提交为准
>
> 目标需求：`requirements.md`、`role-system-design.md`

## 1. 架构结论

当前代码已经形成身份、业务账号、资金账务和模型访问四个服务边界，但部分目标能力仍停留在需求模型，不能按目标架构宣称已完成：

- Keycloak 是登录身份、密码、OIDC 会话和当前 `platform-admin` Realm 角色的事实源。
- Account Service 是业务用户、组织树、成员、组织角色和活动上下文的事实源；当前活动上下文按 OIDC Session 保存，不是用户级全局上下文。
- Billing Service 是钱包、平衡账本、充值订单、聚合权益包、固定三元组定价和 Hold 的事实源。
- Token Gateway 是 Token Account、加密凭据、共享/路由策略和模型用量请求的事实源；当前模型推理协议仅支持 `/v1/chat/completions`。
- New API 是模型数据面和上游用量证据源，不承载品悟用户、组织或钱包真相。
- Token Portal 是个人中心和运营后台的 Web BFF，采用 OIDC Authorization Code + PKCE、Redis 服务端会话和 CSRF 防护。
- 桌面端账户 Gateway 路径使用与登录相同的 OIDC Access Token。A06 仍有旧内置模型入口，本波负责迁移；是否完成旧路径退役、孤立引用转换、凭据清理及失败恢复，必须依据本波最终固定源码与实际测试，不能从原审计推断已通过。

当前代码尚未实现的关键目标包括：注册钱包 Outbox Saga、用户级全局上下文、平台业务 RBAC、托管路由 Provisioning 状态机、模型目录/协议/公式版本、模型结果重放、组成级 BASE/BONUS 权益、目标退款模型、应收/拒付/强制退款状态机和账号注销 Saga。

## 2. 逻辑拓扑

```mermaid
flowchart LR
    Desktop["品悟桌面端\nReact + Tauri"]
    Browser["个人中心 / 运营后台\n浏览器"]
    Proxy["HTTPS 反向代理"]
    Portal["Token Portal\nWeb BFF"]
    KC["Keycloak\nOIDC 身份"]
    Account["Account Service\n用户、组织、角色、Session 上下文"]
    Billing["Billing Service\n钱包、账本、支付、定价、Hold"]
    Gateway["Token Gateway\n账号、凭据、路由、模型用量"]
    NewAPI["独立 New API\n模型数据面"]
    Provider["真实模型供应商"]
    Aliyun["短信 Adapter\n通用 HTTP / 阿里云"]
    Mall["新华三商城"]
    PG[("PostgreSQL\n按服务分库分用户")]
    Redis[("Redis\nPortal 会话")]
    NATS[("NATS JetStream")]
    Relays["Account / Billing / Token\nEvent Relay"]

    Desktop -->|"登录、注册（当前含密码）、业务 API"| Proxy
    Browser --> Proxy
    Proxy --> Portal
    Proxy --> KC
    Proxy --> Account
    Proxy --> Gateway
    Portal --> KC
    Portal --> Account
    Portal --> Billing
    Portal --> Gateway
    Desktop -->|"OIDC Bearer + chat/completions"| Gateway
    Account -->|"身份创建/删除（当前含注册密码）"| KC
    KC -->|"短信 Direct Grant 验证"| Account
    Account --> Aliyun
    Account -->|"仅配置企业商城时"| Mall
    Account -->|"账号 bootstrap 确保钱包"| Billing
    Billing -->|"显式管理员免单协调"| Gateway
    Gateway --> Account
    Gateway --> Billing
    Gateway --> NewAPI
    NewAPI --> Provider
    Billing --> Mall
    Account --> PG
    Billing --> PG
    Gateway --> PG
    KC --> PG
    Portal --> Redis
    Account --> Relays
    Billing --> Relays
    Gateway --> Relays
    Relays --> NATS
```

仓库配置要求反向代理只公开认证、账号、Gateway 和 Portal 的受控路径。PostgreSQL、Redis、NATS、Relay、Billing 内部 API 和 New API 管理面保持在内部网络；这属于配置和代码基线，本次审计没有取得实际生产环境与版本证据。

当前注册密码信任边界尚未达到后续目标设计：桌面端把原始密码提交给 Account 的注册预检、短信挑战和注册接口，Account 在进程内校验后把密码传给 Keycloak Adapter 创建身份。Account 不持久化密码，但仍实际接收并处理原始密码；目标要求原始密码和密码策略只进入 Identity 边界。反向方向上，Keycloak 的短信 Direct Grant Authenticator 携带内部服务凭据调用 Account `/internal/v1/auth/sms/verify`，并按 Account `/internal/v1/auth/mobile/identity` 返回的当前权威 subject 登录，不按同手机号任选旧身份。

## 3. 服务边界

| 组件 | 当前职责 | 当前自有状态 | 主要目标差距 |
|---|---|---|---|
| Keycloak | 用户认证、密码、OIDC Token/Session、`platform-admin` Realm 角色、身份创建删除 | Keycloak 数据库 | 当前注册原始密码先经过 Account；密码策略、原始密码处理与注册幂等需收敛到 Identity 边界；平台业务角色不应继续依赖 Realm Role |
| Account Service | 用户/身份映射、手机注册及当前密码编排、商城账号同步、组织/节点、成员/角色、Session 上下文、邀请、审计 | Account 数据库 | 移除原始密码处理、注册 Saga、全局上下文、平台 RBAC、注销 Saga、标识预约与恢复 |
| Billing Service | 钱包、账本、聚合权益包、充值、固定定价、Hold、人工调整、支付修复和对账 | Billing 数据库 | 组成级权益、版本化单位/公式、目标支付退款状态机、应收与组织购买 |
| Token Gateway | Token Account、加密凭据、共享策略、路由、聊天代理、用量和 New API 核对 | Token 数据库 | 版本化模型目录、协议能力、托管 Provisioning、请求重放、持久派发和取消 |
| Token Portal | OIDC PKCE、Redis Session、CSRF、个人中心、运营后台和路径白名单代理 | Redis Session | 目标后台权限模型、缺失业务页面和发布级三语门禁 |
| Event Relay ×3 | 事务 Outbox 投递、重试和死信 | 各服务 Outbox/Inbox | 现有事件有限，不能替代缺失的跨域 Saga |
| New API | OpenAI 兼容代理、供应商调用日志和用量证据 | 独立数据库 | 仍需通过 Adapter 契约补齐协议、取消和证据能力 |

### 3.1 仓库交付形态

- 仓库同时提供 Docker Compose 和 Helm Chart；它们是可部署模板，不证明任何环境已经部署，也不证明高可用、备份、RPO 或 RTO。
- `mock-newapi` 和 `payment-simulator` 是 Compose 中的非生产测试支撑组件，不属于产品服务边界或生产逻辑拓扑；其隔离和替换决策见 `adr/ADR-007-payment-simulator.md`。
- Compose 包含上述测试支撑组件和可选真实 New API profile，供本地开发、合同测试、端到端验证和故障注入；生产 Helm 不部署这些工作负载。
- 当前真实支付 Adapter 是新华三商城；其代码能力只有下单、查询/同步和人工修复。非生产测试 Provider 提供的退款、回调和账单能力不得归入新华三或生产能力。
- 远程 Compose 已把 `payment-simulator` 主进程放入 `development-simulator` profile，但基础数据库初始化和 `payment-simulator-migrate` 尚未一并隔离；Billing 启动和 Helm 模板也仍保留 `PAYMENT_SIMULATOR_URL`、`PAYMENT_SIM_HMAC_*` 等模拟器命名的配置耦合。这些是待迁移为供应商中立 Payment Provider 配置并从生产清单彻底排除的实现债务，不代表模拟器属于生产架构。
- 生产部署状态仍未验证；A06 隔离 Compose 执行已有固定输入与环境证据，边界见 `implementation-status.md` 第 0 节。不能把历史矩阵中的 `NOT_DEPLOYED` 当作这些本地执行从未发生，也不能把本地执行升级为生产部署证明。

## 4. 当前数据所有权

### 4.1 Account 域

当前迁移包含：

- `users`、`identities`：业务用户及 OIDC 身份映射；用户状态包括 `PROVISIONING`、`ACTIVE`、`DISABLED`、`DELETED`。
- `organizations`、`organization_units`、`organization_unit_closure`：组织及树形闭包。
- `memberships`、`membership_units`、`role_bindings`：成员、节点范围和组织角色。
- `invitations`：当前邀请使用电子邮箱和一次性 Token，不支持需求目标中的手机号/用户名邀请。
- `user_sessions`：按 issuer + OIDC Session 保存当前上下文。
- `slider_captcha_challenges`、`sms_challenges`：滑块和短信挑战元数据。
- `mall_user_sync_jobs`：新华三商城用户同步任务。
- `audit_logs`、`outbox_events`、`inbox_messages`、`inbox_aggregate_versions`：审计和可靠事件。

手机注册接口先在 Account 事务中准备稳定用户 ID 与恢复意图，再在 Keycloak 创建或恢复唯一标记 Identity，最后在 Account 事务完成身份关联与 `PROVISIONING` 状态；成功返回 `201`，注册响应不调用 Billing。首次注册的桌面流程随后单独登录，恢复响应须按 `password_preserved` 提示使用首次提交的密码。登录后的 `/v1/account/bootstrap` 会先确保 Account Session，再同步调用 Billing `EnsurePersonalWallet`，成功后把用户激活为 `ACTIVE`。Billing 失败会阻断本次 bootstrap/激活，但不会把已经成功的注册响应改为失败，也不会删除已提交的 Identity 或 Account。

当前注册使用 users 中的业务事实和专用 prepare/complete 审计意图预先固定注册 ID；Keycloak 创建携带该不透明标记。远端接受但响应丢失或本地完成失败时，重新滑块/SMS 后只恢复同一意图、完整事实和唯一 subject，不猜测用户名/手机号、不删除或重置已接受的身份。恢复响应 `password_preserved=true` 表示首次密码保留。这个闭环不等同于目标中所有预约、注销或异步钱包 Saga；注册钱包 Outbox、后台重试、SLA 和 `REPAIR_REQUIRED` 仍未实现。

### 4.2 Billing 域

当前迁移实际包含：

- `wallets`、`ledger_accounts`、`journal_entries`、`ledger_postings`：钱包缓存余额和平衡账本；数据库触发器保护分录平衡和已入账记录不可变。
- `credit_lots`、`entitlement_packages`、`usage_hold_allocations`：批次、聚合权益包和 Hold 分配。
- `recharge_products`、`pricing_versions`：商品以及固定 `input/output/cached_input` 三元组价格。
- `usage_holds`、`idempotency_records`：预占/结算和通用幂等记录。
- `payment_orders`、`payment_transactions`、`refunds`、`payment_notifications`：当前支付和退款模型。
- `manual_adjustments`、`payment_order_repairs`、`reconciliation_runs`、`reconciliation_cases`：运营调整、修复和对账。
- Billing 自有 Outbox/Inbox 表。

当前权益包以 `payment_order_id` 关联订单，只保存总积分，不保存 BASE/BONUS 组成及各自累计消费。`payment_transactions` 对 `payment_order_id` 有唯一约束，实际是订单一对一支付流水；这与目标的一对多流水及“每个成功流水独立权益包”不同。

当前没有 `metering_unit_definitions`、`protocol_reservation_formula_versions`、`pricing_version_unit_rates`、`model_usage_requests`、`stream_response_events`、`upstream_dispatch_attempts`、`refund_requests`、`refund_amount_reservations`、`chargeback_cases`、`forced_refund_cases`、`payment_evidence` 或独立应收表。这些名称属于目标模型，不是现有数据结构。

账本和额度使用整数 `microcredit`，当前换算为 `10,000 microcredits = 1 展示积分`；支付金额使用人民币分。

### 4.3 Token 域

当前迁移包含：

- `token_accounts`、`provider_credentials`：模型访问账号和加密凭据；管理模式为 `USER` 或 `PLATFORM`。
- `token_share_policies`、`token_explicit_members`、`routing_policies`：共享和路由策略。
- `usage_requests`、`usage_attempts`：模型请求、上游关联、Hold 和结算状态。
- `authorization_cache_versions`、`token_audit_logs` 以及 Token 自有 Outbox/Inbox 表。

当前没有 `managed_route_provisionings`。首次请求缺省路由时，Gateway 在请求内通过数据库 advisory lock 同步创建平台 Token Account 和默认路由；没有 `PENDING/READY/FAILED/REPAIR_REQUIRED` 代次、退避或人工修复状态机。

原已审自管 Token Account、凭据、eligible 和单笔 usage API 及 Portal 白名单继续保留；普通接口过滤平台托管账号并禁止修改 `PLATFORM` 账号。新内置模型迁移不能据目标设计删除原业务能力。供应商密钥只在 Gateway 内存中解密，普通用户接口不返回明文；本波客户端入口以最终源码审核为准。

本波为保全内置模型选择补充 `GET /v1/models` 只读契约，返回当前上下文授权模型的去重 `data: [{id}]`，不输出凭据/所有者，不按钱包余额隐藏，不创建路由、Hold 或用量。初次尚未创建托管账号时可只读投影已配置的受支持默认模型；已有平台托管账号必须尊重管理员禁用决定。响应上限 4096 个模型、2 MiB，超限失败而非截断。该接口与客户端各自独立审核，不等同于目标 TOK-37 或完整版本化目录已交付；最终完成度以固定输入与实际验证为准。

## 5. 身份、上下文和权限

### 5.1 当前权限模型

- 平台运营后台当前直接依赖 Keycloak `platform-admin` Realm Role，尚未实现目标中的 Account 平台角色、临时有效期、授权版本和 Break-glass 恢复机制。
- `OWNER`、`ORG_ADMIN`、`BILLING_ADMIN`、`UNIT_ADMIN`、`MEMBER`、`AUDITOR` 是 Account 域组织角色，通过 `role_bindings` 关联成员。
- 组织所有者具备普通接口保护，但“禁止最后一个有效超级管理员主动注销”无法由当前业务模型完整执行，因为用户主动注销和平台超级管理员生命周期均未实现。目标上 Break-glass 只能用于管理员异常失联后的恢复，不能作为主动注销绕过手段。

### 5.2 当前上下文

当前上下文绑定 OIDC Session：

- `PERSONAL`：个人钱包和个人路由。
- `ORGANIZATION`：指定用户已加入的有效组织，使用组织角色、路由和钱包。

同一用户的不同设备/会话可以选择不同上下文。目标需求要求用户级全局活动上下文、版本递增、跨端刷新和请求开始快照，当前尚未实现。

## 6. 当前模型调用与计费链路

当前 `/v1/chat/completions` 链路为：

1. 解析 OIDC 主体和 Account 当前 Session 上下文。
2. 解析模型和最大输出 Token，列出并确定性选择可用路由；无缺省路由时同步确保平台路由。
3. 从 Billing 读取固定三元组价格，以平台最大输入 Token 和请求最大输出 Token 计算最大费用。
4. 创建 `usage_request`，向 Billing 创建 Hold。
5. 解密所选凭据，记录上游尝试，调用 New API。
6. 根据返回 usage 结算；证据不确定时进入 `REVIEW`，后台按 New API 请求日志核对，不自动重发模型请求。

当前边界和目标差距：

- 路由已有确定性优先级、显式账号失败不回退、上下文/共享策略检查，但没有独立模型目录和 `CATALOG_VISIBLE → Provisioning READY → CONTEXT_ELIGIBLE → RESERVABLE` 状态管线。
- Hold 在上游调用前创建，并按较早到期权益包分配；未知消费或到期进入 REVIEW 并保留预占，不能自动免单。没有组成级消费顺序、追加预占或长调用续租。显式最终免单的跨域协调继续保留实际 usage/成本证据，并通过持久意图恢复。
- 定价只支持输入、输出、缓存输入三元组；没有协议公式、版本化计量单位、显式零价/配置缺失区分和逐单位来源快照。
- 幂等键必须由客户端按每次逻辑调用提供，缺键拒绝；同调用重试复用键，新调用使用新键，不按用户/Session/body 派生永久键。重复请求返回 `USAGE_REPLAY_BLOCKED`/409，不回放结果；版本化规范指纹、流事件存储、结果/墓碑分层保留及 `410` 属后续目标。
- 流式响应直接转发给客户端，未先持久化；请求上下文取消可能中止上游，不具备断线继续执行和按序重放。
- 没有持久派发租约/意图状态机，也没有模型请求取消命令及 Provider 取消能力契约。

## 7. 当前支付、权益与退款

充值以 Billing `product_id` 为可信输入，金额、外部 SKU、BASE/BONUS 商品字段、展示积分和有效期由服务端目录派生。当前 UAT 目录为：

| 商品 | 商城 SKU | 金额 | 总展示积分 | 有效期 | 状态 |
|---|---|---:|---:|---:|---|
| 体验版 | `0502AJON` | ¥0.10 | 2,000 | 30 天 | UAT 暂定 |
| 标准版 | `0502AJ0Q` | ¥10.00 | 4,000 | 30 天 | UAT 暂定 |
| 高级版 | `0502AJOR` | ¥30.00 | 9,000 | 30 天 | UAT 暂定 |
| 旗舰版 | `0502AJOP` | ¥200.00 | 50,000 | 30 天 | UAT 暂定 |

新华三 Adapter 当前只支持支付宝下单、订单查询/同步和人工修复，不支持主动退款、关单、回调或账单。客户端关闭二维码页后，Billing 后台仍可继续查询。仓库中的非生产测试 Provider 支持更多动作，但只用于开发/测试，不能扩张生产能力声明。

商城确认支付成功后，Billing 幂等创建一个聚合权益包和账本分录。虽然商品保存 BASE/BONUS 数值，履约后只保存合计额度，因此无法执行目标中的 BONUS 优先消费、组成级授予或精确退款撤销。

现有普通退款是兼容旧模型的订单级、可输入金额的部分退款：

- 请求绑定 `payment_order_id`，接受 `amount_fen`，按订单聚合积分比例计算撤销量，并包含 `PARTIALLY_REFUNDED`。
- 本地事务会先预约退款/额度，再调用 Provider；但没有退款 Outbox 恢复，也没有支付流水 + 权益包目标锁定。
- 没有“任一 BASE/BONUS 已消费即拒绝”的累计消费证据，也无法在成功后精确撤销该支付产生的全部未消费 BASE 和关联 BONUS。

上述全 BASE 退款与 BONUS 撤销是后续目标，不改变当前已审行为：新个人 PLAN 不可退款，原 LEGACY/普通余额继续走既有退款流程。首次观察到 Provider 部分或全额退款而本地尚未入账时，同事务进入 `MANUAL_REVIEW`，创建、查询、关闭和迟到成功回调均不能再自动入账；不据此猜测原额/净额积分。退款模型变更须另行确认与迁移，不在本波实现。

## 8. 一致性与事件

平台不使用跨服务分布式事务。三个服务具备本地 Outbox/Inbox、Relay、重试和死信基础，当前主要事件为：

- `account.membership.changed.v1`
- `billing.recharge.succeeded.v1`
- `token.usage.settled.v1`

本地业务状态与对应 Outbox 写入具备事务原子性；Relay 以至少一次语义重试投递，默认达到 12 次失败后进入死信，需审计后人工重新入队。因此该机制保留并重试事件，但业务最终收敛仍依赖死信处置，不能无条件宣称最终投递。现有事件集也没有实现注册钱包初始化、账号注销、目标退款和托管路由 Provisioning 等 Saga。

## 9. 客户端与 Portal

- Portal 采用 OIDC Authorization Code + PKCE、Redis 服务端 Session、CSRF 和严格代理路径白名单。
- 桌面 OIDC Access Token 保留在 Rust 进程，Refresh Token 只存系统凭据库，不持久化密码。
- 本波客户端迁移设计包含盘点、配置备份、会话/计划任务模型 ID 转换和旧凭据清理重试；须按最终源码实际验证孤立引用、坏数据、失败恢复及运行时不再调用旧路径，不能沿用原审计的测试结论。
- 自管 Token 与平台托管 Token 的权限边界保留；管理后台提供平台 Token Account、凭据元数据、路由、用量、钱包、订单、调整和修复。客户端入口不能仅因本轮旧内置模型迁移而删除。
- 桌面与 Web 当前路由已有中/英/日词条测试，但尚无覆盖全部目标页面、错误码和发布制品的三语发布门禁。
- 发布、安装、运行三层迁移门禁及不计费 readiness 仍未形成完整、可部署证明。

## 10. 安全边界

- 远程用户入口要求 HTTPS；开发态 HTTP 仅限 loopback。
- Portal 内部服务凭据不下发浏览器或桌面端；桌面 OIDC Token 不进入 WebView JavaScript。
- 当前注册原始密码会进入 Account 的预检、短信挑战和注册处理，再传给 Keycloak Adapter；虽然不落 Account 数据库，但该信任边界仍需按目标收敛到 Identity Adapter，且不得 trim、记录或跨越无关业务层。
- Provider 地址使用 HTTPS 和主机白名单；生产密钥目标为 KMS/Vault，本地 KEK 只用于开发/测试。
- Keycloak `/admin/` 应限制在受控网络或隧道内。
- 日志不得记录密码、短信验证码、OIDC Token、供应商密钥或支付密钥。

## 11. 审计证据与主要风险

以下是原 `75a29f8` 文档审计列出的历史验证，不表示本波重跑；当前固定提交、执行日志和限制另由本地分波审核记录给出：

- `node scripts/account-platform.mjs test unit`：所有 Go 包测试通过（项目内置 Go 工具链，`nomsgpack` 标签）。
- `npm test`（`account-platform/`）：OpenAPI lint 和 36 个合同测试通过。
- `node --test tests/account_platform_state.test.mjs tests/ui_language_coverage.test.mjs tests/scheduled_tasks_unit.test.js`（`pinvou3-app/`）：39 个测试通过。
- `node scripts/account-platform.mjs verify`：Go 单测与 Vet、合同测试、架构守卫和差异检查通过。

| 项目 | 当前事实 | 风险/下一步 |
|---|---|---|
| 部署 | Compose/Helm 模板及 A06 隔离运行证据；无生产环境验收 | 不得宣称生产部署、HA、备份或 RPO/RTO 已完成 |
| 注册 | 注册成功返回 `PROVISIONING` 且不依赖 Billing；后续 bootstrap 同步确保钱包，Billing 故障会阻断激活 | 改为持久 Saga、Outbox 初始化、后台重试和 `REPAIR_REQUIRED` 可恢复状态 |
| 权限 | 平台后台依赖 Keycloak Realm Role | 建立 Account 平台 RBAC、临时授权、最后超级管理员保护和受限 Break-glass |
| 模型调用 | Hold 前置和事后核对已有基础 | 补目录/公式/Provisioning、派发持久化、重放和取消 |
| 权益 | 权益包仅聚合总额 | 重构为 BASE/BONUS 组成和守恒约束后才能实现目标退款 |
| 支付 | 新华三只有下单与查询；数据模型是一单一流水 | 目标一对多流水、独立状态机、退款/拒付/应收和证据模型均待实现 |
| 客户端切换 | 本波迁移实现须按最终固定输入独立审核与实测 | 不把旧快照、源码存在或开发测试作为发布/安装准入及制品回滚证明 |
| 测试 | A06 已实际执行真 PG/NATS/Redis、Keycloak 扩展、Compose devup/E2E/fault/auth 与真实 NewAPI HTTP 健康 | A07 同输入才可继承；改变输入须重验。模拟支付/模型/短信、离线 Helm/TLS 测试不代表真实供应商、生产 TLS、Kubernetes 或发布验收 |
