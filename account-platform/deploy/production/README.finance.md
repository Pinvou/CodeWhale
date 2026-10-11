# F02 个人金融与三项审批部署

本阶段在 F01 登录、个人钱包及 workload 身份之上加入个人充值、订单、权益、定向整笔退款、商品管理、付款异常处理，以及钱包调整、支付渠道恢复、商品恢复三项审批。原有运行约束见 `README.personal.md`；运行本阶段代码必须再应用 `compose.finance.yaml`。使用同一固定提交构建所有镜像及 Portal 静态资源，保留 F01 的五个 HTTP workload 客户端；本阶段不增加 Token 数据库、模型调用、组织金融或后继审批命令。

合并配置顺序为 `compose.personal.yaml`、`compose.personal-workload.yaml`、`compose.finance.yaml`。镜像变量必须指向本阶段的固定源码镜像，不得继续使用 F01 的钱包限定 Billing 或余额限定 Token 镜像。构建入口 `account-platform/Dockerfile` 支持 `SERVICE=approval-resources`、`event-relay` 及 `payment-simulator`；模拟器不在生产 compose 中部署，TestOnly 渠道不得正式收款。Compose 配置检查时不要展开含私密值的环境文件，也不要把完整配置输出到日志。

## 数据库和私有配置

先备份并通过受控迁移角色执行当前 Account、Billing 迁移。`production-new` 仅适用于已核实的新数据库；原部分退款迁移遇到非终态旧退款必须停止，不得清除数据绕过保护。历史数据库必须按迁移入口既有备份证据要求执行，不能把本阶段的迁移演练当作生产授权。

Account 私有环境增加 `APPROVAL_WRITER_DATABASE_URL`（独立 `account_approval_writer`）、`HUMAN_DELEGATION_SIGNING_KEY_BASE64URL`、`HUMAN_DELEGATION_SIGNING_KEY_ID` 与 `MANUAL_REVIEW_ON_CALL_USER_IDS`。普通 Account 服务角色不得替代审批授权写入身份。按已验证商务环境配置 H3C Mall 连接与凭据，个人充值使用服务端解析的客户标识，不能由浏览器提供客户或手机号事实。

Billing 私有环境必须配置 `MANUAL_REVIEW_ON_CALL_USER_IDS`、与 Account 签名对应的 `HUMAN_DELEGATION_VERIFICATION_KEYS_JSON`、明确的 `BILLING_PAYMENT_PROVIDER` 及该适配器要求的 merchant、Mall 和回调配置。真实渠道使用 `h3c_mall`、HTTPS `H3C_MALL_BASE_URL`、`H3C_MALL_APP_KEY`、`H3C_MALL_APP_SECRET`，以及 `PAYMENT_MERCHANT_ID`、`PAYMENT_CALLBACK_CLIENT_ID`、`PAYMENT_CALLBACK_CLIENT_SECRET`、`PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64` 和 `PAYMENT_CALLBACK_TOKEN_KEY_ID`。所有秘密只放进对应私有环境文件；回调签名密钥与 workload 客户端密钥分开保管。`BILLING_NORMAL_REFUND_CREATION_ENABLED` 默认为 false，启用前须明确确认业务入口，仍只允许 LEGACY 指定支付流水与权益包的整笔退款，PLAN 不退款。

`BILLING_RELAY_PRIVATE_ENV_FILE` 仅提供 Billing 普通 outbox 的 `DATABASE_URL`，使用既有 `billing_service` 角色的独立受控连接；本阶段迁移没有另建 Billing relay 数据库角色。Account relay 原私有文件另提供 `APPROVAL_RELAY_DATABASE_URL`（`account_approval_relay`）；两种发布连接、普通与授权 NKey 必须分别投递。普通 relay 的 `/health/ready` 不代表独立审批发布就绪，检查 Account relay 的 `/health/ready/approval-authorization`。Billing 普通 relay 发布退款请求等真实事件，Billing executor 消费退款及三项授权；缺少这个 relay 会使普通退款停留在队列中。

## 权限、资源与启动次序

重新渲染三个 workload rules 模板的已核验 subject。Account 只增加 Billing activation/proof/effect/result 读取；Token 只增加个人商品、渠道、订单和权益调用；Billing 向 Account 进行权限验证、人工委托、执行栅栏和授权投递确认。不授予 `internal.*` 通配权限，不给 Token 管理退款或管理商品接口权限。

依照 `../runbooks/nats-resources.md` 离线渲染、独立核验当前公开身份清单、三 consumer manifest 和五身份 broker ACL。F01 双身份 `authorization.conf` 不能用于 F02 executor。broker 只接收核验后的公开配置；每个运行角色只挂载自己的 NKey，公开身份、manifest、receipt 与私钥分别放在独立受控目录。Compose 中三个 SHA 由独立审核输入，不能由运行服务自行计算并信任。

首次启动先单独完成迁移、预置固定 stream 和 fresh-only 审批初始化，不能直接启动全部服务再让服务自动生成审批资源。使用 `approval-resources` profile 对每步显式传入 `-action` 与当前 `-expected-version`，按 runbook 的 status、quiesce、fence、initialize-consumers、activate、resume 次序执行。初始化前 receipt 目录为空且由操作员 UID 控制；输出 receipt 独立回读核验后再设置 `APPROVAL_RECEIPT_SHA256`。Billing 挂载只读 receipt，缺少真实回执或存在漂移时拒绝启动。Compose 的默认 operator 命令只有 status，不会自动启用审批。已有审批或 NATS 历史必须拒绝 fresh-only 路径，不能删除历史、重置 consumer 或扩大 ACL 来伪装首次安装。

当前 F04 `approval-resources` 镜像的 `activate` 和 `resume` 均会联合核验三条 Billing 和三条 Token consumer；必须在 Billing 的 `initialize-consumers` 完成后、`activate` 前，按 [模型审批资源说明](README.models.md#审批资源和-workload-身份) 显式执行 `token-consumers initialize` 并独立核验其回执。Billing 与 Token 使用各自的回执目录，不得合并、覆盖；关闭模型界面或启动 executor 都不能省略初始化。既有 F02 镜像仍遵循原三项 Billing 审批边界。

F01 TLS edge 只新增两个精确 POST 回调入口 `/api/v1/payment-callback/token` 和 `/api/v1/payment-callbacks`；回调自身仍执行客户身份、Bearer、provider/merchant/profile 和付款事实检查。Billing 内部与管理接口不向公网代理。Portal 经私网 `BILLING_HTTP_URL` 转发管理员已有授权操作，用户订单仍经 Token 网关校验个人上下文。

## 验证限制

本地真实 PostgreSQL、TLS NATS 与受控 HTTP 用例仅证明命中的迁移、金融状态和资源协议。生产 Linux 容器、外部 Mall/Keycloak、密钥投递与现场发布尚需部署环境验收；完整 E2E 依用户决定延期。编译、静态配置检查或模拟支付成功不能替代真实支付渠道和正式发布验收。

## Billing 数据库权限前置

启动 Compose 迁移作业前，数据库管理员须按 [Billing 数据库运行说明](../postgres/README.md) 在独立 Billing 数据库执行角色初始化。`billing-migrate` 私有环境文件使用 `billing_migrator` 的 DSN 和 `MIGRATION_ROLE=billing_service`；Billing 与 relay 只使用 runtime `billing_service` 连接，不得共用管理员或迁移凭据。已有部署同样先预配 owner 和单向成员关系，再提供备份回执执行前向硬化。Compose 不包含管理员数据库密码，不自动运行角色初始化；缺少角色会明确拒绝迁移。
