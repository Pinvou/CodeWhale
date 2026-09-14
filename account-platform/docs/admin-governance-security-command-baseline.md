# 治理与安全审批命令事实与兼容基线

> 子任务：`GSA-02`（DESIGN）
>
> 状态：DESIGN DONE / 身份 B 整批审核 APPROVED / 不新增产品运行能力 / 未部署
>
> 代码基线：`88d37a53`
>
> 需求合同：[管理后台治理与安全及审批中心整改设计](admin-governance-security-redesign.md)

## 0. 文档效力与读法

本文只冻结当前仓库中十四类审批命令的事实、版本和兼容缺口，供后续 GSA 子任务引用；不新增 API、状态、恢复规则或 activation，不把目标设计写成当前实现。目标 proof、presenter、cursor、Relay、consumer 和发布顺序仍以整改设计为唯一合同。

本文中的版本必须分层理解：

- **policy version / snapshot schema version** 来自 Account `approval_command_policies` 与[静态策略注册表](../internal/platform/approvalpolicy/registry.go)，不是领域对象自己的策略版本；
- **wire/interface version** 是 HTTP `/v1`、授权事件 `.v1` 或 Token handler contract v1；当前没有治理审批 v2 wire；
- Provider/Product normalized command 内的字符串 `policy_version` 是 Billing 的 `payment-stop-loss/v1` 领域策略，不等于 Account policy version 2/1；
- “当前持久快照”指 Portal/领域事实源写入 `approval_requests` 与 `approval_request_snapshots` 的实际 JSON，不等同于 registry 的 `critical_resource_fields` 元数据。

## 1. 当前共享审批链

### 1.1 API、持久化和授权事件

当前 Account 服务在 [`service.go`](../internal/account/service.go) 注册原生 `/admin/v1` 接口；Portal BFF/聚合 OpenAPI 才把它们暴露为 `/account/admin/v1` 命名空间，公开合同位于 [`admin.yaml`](../contracts/openapi/admin.yaml)。以下列表使用 Account 服务原生路径，调用方不得把 Portal 前缀带到服务间直连：

- `GET /admin/v1/approval-policies`；
- `GET|POST /admin/v1/approval-requests`；
- `GET|PUT /admin/v1/approval-requests/{approval_request_id}`；
- `POST /admin/v1/approval-requests/{approval_request_id}/actions/submit|withdraw|decide`。

其中现有 `GET /admin/v1/approval-policies` 额外要求 `platform.roles.read`；这是当前策略读取接口的权限事实，不得直接复用于后续只返回 `visible/requestable` 的 Directory。

[`approvals.go`](../internal/account/approvals.go) 实现 create/update/submit/withdraw/decide、`supersedes_request_id`、当前权限复核、非自审、版本 CAS、状态事件和授权 Outbox；Begin/Complete 的持久状态机在 [`approval_execution.go`](../internal/account/approval_execution.go)，HTTP 适配在 [`approval_execution_http.go`](../internal/account/approval_execution_http.go)。领域 executor 直连 Account 原生 `/internal/v1/approval-requests/{id}/executions[/complete]`，不经过 `/account` Portal 命名空间；批准事件的当前唯一线格式是 [`account.approval-command.authorized.v1`](../contracts/events/account.approval-command.authorized.v1.schema.json)。GSA-01 已在提交 `88d37a53` 补齐静态 executable/动态 Token activation 推进门禁与 SUBMITTED 的安全 REJECT/WITHDRAW，但没有交付本专项的 owner-domain activation、proof、v2 presenter 或恢复协议。

### 1.2 当前近期认证事实

当前审批入口的 `humanApprovalPrincipal` 只要求 issuer、subject、session ID 非空且 `AuthenticatedAt` 非零；Account create/update/submit/decide 没有校验认证年龄。Token [`tokenCommandSpecs`](../internal/token/command_governance.go) 虽把三类批准命令标为 `RecentAuth=true`，但浏览器 `/commands` 路径会先因批准命令而返回 `DIRECT_APPROVAL_COMMAND_FORBIDDEN`，所以后置的 15 分钟判断不保护 Account 审批链。Billing 商品管理的 `requireRecentProductAuthentication` 也不等于 Provider/Product 恢复审批。

Token 三类高风险对话框当前传入 `recentAuth`，[`AdminHighRiskActionDialog.jsx`](../../pinvou3-app/src/features/account-platform/AdminHighRiskActionDialog.jsx) 只显示“需要近期认证”的提示条；该 UI 提示既不校验认证时间，也不能替代服务端门禁。Provider/Product 当前业务页没有可证明等价的审批近期认证执法。

因此，整改设计第 8.5 节的“Token 三命令和 Provider/Product 申请、提交、批准均为 15 分钟”是**待实现的稳定目标合同**，不是当前代码或 UI 已经落实的事实；缺口由 GSA-13～15 和 Portal GSA-17～19 闭合。Wallet 明确沿用当前合同且不新增近期认证；若以后改变，必须追加 policy version。

## 2. 六类既有可执行命令

### 2.1 版本与入口矩阵

本表只有 Wallet 的 `/admin/v1/adjustments*` 是 Billing 服务原生路径；带 `/billing/admin/v1` 或 `/token/admin/v1` 的其余业务入口均为 Portal BFF 命名空间，服务间调用不得照搬这些前缀。

| 命令 | 当前业务入口与事实源 | Account p/s | 当前 wire / consumer / executor | 当前 activation 与部署前置 |
|---|---|---:|---|---|
| `WALLET_ADJUSTMENT` | Billing 原生 `POST /admin/v1/adjustments` 创建 `manual_adjustments`；Portal 在业务页基于该行创建并提交 Account draft。旧 Billing 原生 `POST /admin/v1/adjustments/{id}/approve` 固定返回 `APPROVAL_WORKFLOW_REQUIRED`。来源见 [`admin.go`](../internal/billing/admin.go)、[`AdminPlatformView.jsx`](../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx)。 | 1 / 1 | Account admin/internal v1 + 授权事件 v1；Billing 当前共享 `billing-command-executor-v1` consumer 分派到 [`approval_execution_http.go`](../internal/billing/approval_execution_http.go)，executor identity=`billing-command-executor`。 | 静态 `Executable=true`；当前没有 Billing owner-domain 持久 activation。GSA-03 必须以前向兼容投影保持 ACTIVE，不能使既有能力回退。 |
| `PAYMENT_PROVIDER_RESUME` | `GET /billing/admin/v1/payment-provider-controls` 只读持久控制、probe/observation/evaluation/mapping 事实并返回 `resume_draft`；Portal 只能据此创建 Account draft，不能主动探测 Provider。来源见 [`provider_recovery.go`](../internal/billing/provider_recovery.go) 与 [`admin-finance.js`](../../pinvou3-app/src/features/account-platform/admin-finance.js)。 | 2 / 1 | Account admin/internal v1 + 授权事件 v1；同一 Billing `billing-command-executor-v1` 共享 consumer 分派到 provider executor。 | 静态 `Executable=true`、无 Billing 持久 activation；GSA-03 保持兼容 ACTIVE，GSA-15 才新增 proof/revision 校验。 |
| `PAYMENT_PRODUCT_RESUME` | 与 Provider 同一只读控制接口；仅持久 product control 满足条件时返回独立 `resume_draft`，Portal 创建 Account draft。 | 1 / 1 | Account admin/internal v1 + 授权事件 v1；同一 Billing 共享 consumer 分派到 product executor。 | 同上；不得把未来三个 Billing durable 写成当前事实。 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` | Portal 调 `GET /token/admin/v1/models/{model_id}/impact/lifecycle` 取得脱敏影响投影，再创建/提交 Account draft；计划形态先由 `POST /token/admin/v1/models/{model_id}/lifecycle-plans` 持久化领域计划。PRE 兼容期仍保留旧 `POST /token/admin/v1/models/{model_id}/lifecycle` 直写选择器。来源见 [`admin-client.js`](../../pinvou3-app/src/features/account-platform/admin-client.js)、[`AdminModelDetailWorkspace.jsx`](../../pinvou3-app/src/features/account-platform/AdminModelDetailWorkspace.jsx)、[`service.go`](../internal/token/service.go)。 | 2 / 2 | Account v1 + 授权事件 v1；Token handler contract v1；独立 `token-model-lifecycle-executor-v1` durable，经 Token 服务原生 `/internal/v1/commands/model-lifecycle/{precheck\|execute\|result}`，executor identity=`token-command-executor`。 | 复用 Token 持久 `PRE_ACTIVATION/ACTIVE/SAFE_ROLLBACK` 与 epoch；仅 ACTIVE 注册 consumer/允许新批准执行，PRE/SAFE 失败关闭相应新链。不得在 Account 建第二真相。 |
| `TOKEN_MODEL_CATALOG_ROLLBACK` | Portal 调 `GET /token/admin/v1/model-catalog/impact/rollback?source_catalog_version=...` 取得影响投影，再创建/提交 Account draft。旧 `POST /token/admin/v1/model-catalog:rollback` 只属于PRE兼容直写，不是当前审批申请入口。来源见 [`admin-client.js`](../../pinvou3-app/src/features/account-platform/admin-client.js)、[`AdminModelTokenWorkspace.jsx`](../../pinvou3-app/src/features/account-platform/AdminModelTokenWorkspace.jsx)、[`catalog_rollback_governance.go`](../internal/token/catalog_rollback_governance.go)。 | 2 / 2 | Account v1 + 授权事件 v1；Token handler contract v1；独立 `token-catalog-rollback-executor-v1` durable，经 Token 服务原生 `/internal/v1/commands/catalog-rollback/{precheck\|execute\|result}`。 | 与生命周期命令共享同一 Token activation/epoch 真相，但 durable 与执行结果独立。 |
| `TOKEN_CREDENTIAL_REVOKE` | 凭据详情页调 `GET /token/admin/v1/token-accounts/{token_account_id}/impact/credential?action=REVOKE&credential_id=...` 取得脱敏影响投影，再调用 Account create+submit；来源见 [`admin-client.js`](../../pinvou3-app/src/features/account-platform/admin-client.js) 与 [`AdminTokenAccountDetailWorkspace.jsx`](../../pinvou3-app/src/features/account-platform/AdminTokenAccountDetailWorkspace.jsx)。`POST /token/admin/v1/credential-results/{result_ref}/revoke-window` 只撤销凭据隔离登记/轮换窗口，与本审批命令相互独立，不能作为其入口。 | 1 / 1 | Account v1 + 授权事件 v1；Token handler contract v1；独立 `token-credential-revoke-executor-v1` durable，经 Token 服务原生 `/internal/v1/commands/credential-revoke/{precheck\|execute\|result}`。 | 与另两类 Token 命令复用 activation/epoch；不因读取历史结果而放宽 ACTIVE 写门禁。 |

六类当前策略的自包含权限与风险基线如下；六类均为一名审批人、禁止自审、申请有效期24小时、执行窗口5分钟：

| 命令 | 申请权限 | 批准权限 | risk class |
|---|---|---|---|
| `WALLET_ADJUSTMENT` | `platform.billing.repairs.request` | `platform.billing.repairs.approve` | `FINANCIAL_FACT_CHANGE` |
| `PAYMENT_PROVIDER_RESUME` | `platform.billing.payment-controls.resume.request` | `platform.billing.payment-controls.resume.approve` | `FINANCIAL_FACT_CHANGE` |
| `PAYMENT_PRODUCT_RESUME` | `platform.billing.payment-controls.resume.request` | `platform.billing.payment-controls.resume.approve` | `FINANCIAL_FACT_CHANGE` |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` | `platform.models.lifecycle.request` | `platform.models.lifecycle.approve` | `GOVERNANCE_OR_NORMAL_STATE_MACHINE` |
| `TOKEN_MODEL_CATALOG_ROLLBACK` | `platform.models.catalog.rollback.request` | `platform.models.catalog.rollback.approve` | `GOVERNANCE_OR_NORMAL_STATE_MACHINE` |
| `TOKEN_CREDENTIAL_REVOKE` | `platform.tokens.credentials.revoke.request` | `platform.tokens.credentials.revoke.approve` | `GOVERNANCE_OR_NORMAL_STATE_MACHINE` |

`p/s` 分别为 Account policy version 和 snapshot schema version。上述字段及 executor identity 以[注册表](../internal/platform/approvalpolicy/registry.go)为准。数据库追加历史必须完整保留：[`000018`](../migrations/account/000018_approval_commands.up.sql) 首次登记 Wallet p1/s1；[`000024`](../migrations/account/000024_model_catalog_commands.up.sql) 首次登记 lifecycle/catalog p1/s1 `AUDIT_ONLY`；[`000033`](../migrations/account/000033_payment_provider_resume_policy.up.sql) 首次登记 Provider p1/s1；[`000038`](../migrations/account/000038_payment_product_resume_policy.up.sql) 新增 Product p1/s1，并**追加** Provider p2/s1；[`000045`](../migrations/account/000045_token_command_governance.up.sql) **追加** lifecycle/catalog p2/s2，并新增 Credential p1/s1。注册表只选取当前版本，不能据此抹去历史行；后续语义或结构变化必须追加新版本，不得原地修改任何已发布版本。

### 2.2 当前实际持久命令与快照

本节的字段集合只限定为仓库内置 Portal、Billing `resume_draft` 或 Token 影响投影按当前标准流程产生、并由 Account 持久化的标准 JSON。当前 Account 的 Billing 通用 safe-object 校验与 Billing 侧反序列化仍可容忍部分未知字段，因此“标准产物包含这些字段”**不等于**服务端已经实现 `additionalProperties:false` 的 closed schema，也不覆盖非标准手工/历史输入；严格拒绝未知字段及 legacy/new golden vectors 仍由 GSA-05、GSA-12～15交付。

| 命令 | normalized command | 当前 critical / financial snapshot | 当前校验位置与已知缺口 |
|---|---|---|---|
| `WALLET_ADJUSTMENT` | 内置 Portal 标准产物包含 `adjustment_id,wallet_id,amount_microcredits,requested_by` | 标准 critical 为 `{adjustment_version}`；标准 financial 为 `{wallet_id,delta_microcredits}`。 | Portal 组装见 [`AdminPlatformView.jsx`](../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx)，Billing 执行前重读并校验见 [`approval_execution_http.go`](../internal/billing/approval_execution_http.go)。registry 的通用四字段 metadata 不是该持久 JSON schema；GSA-12/15 必须以实际结构交付 closed presenter/proof，不能猜填 metadata。 |
| `PAYMENT_PROVIDER_RESUME` | 标准 `resume_draft` 的 normalized 与 critical 包含同一八字段 `ProviderResumeVersions`：`provider_id,provider_version,provider_control_version,probe_order_id,policy_version,observation_high_watermark,evaluation_high_watermark,active_mapping_set_fingerprint`。 | 内置 Portal 标准 financial 为 `{new_orders_enabled:false,provider_control_version,product_control_version:null}`。 | [`provider_recovery.go`](../internal/billing/provider_recovery.go) 重读持久 Billing 事实、逐字段比对并执行；当前没有专项 closed proof，且 `policy_version` 为领域字符串，不是 Account p2。 |
| `PAYMENT_PRODUCT_RESUME` | 标准 `resume_draft` 的 normalized 与 critical 包含同一十四字段 `ProductResumeVersions`：Provider 八字段去掉 mapping fingerprint并增加 `product_id,product_control_version,catalog_item_id,catalog_version,admission_version,mapping_id,mapping_version`。 | 内置 Portal 标准 financial 为 `{new_orders_enabled:false,provider_control_version,product_control_version}`。 | [`provider_recovery.go`](../internal/billing/provider_recovery.go) 逐字段重读；当前没有专项 closed proof。 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` | 当前计划形态为 `model_id,lifecycle_plan_id,lifecycle_plan_version,target_lifecycle,effective_at,existing_session_cutoff_at,replacement_model_id`；既有即时历史允许缺少 plan ID/version，与 critical 中 version 0 兼容。 | critical 为 registry 冻结的 13 字段模型/目录/生命周期/价格/路由/Provisioning/inflight事实；financial 为 Billing 六字段价格覆盖投影。 | Account [`approval_snapshot.go`](../internal/account/approval_snapshot.go) 调 Token 聚合快照，Token [`approval_snapshot.go`](../internal/token/approval_snapshot.go) 读取两域事实；这是现有快照链，不是目标 proof，也不具备目标 2 秒/closed proof 合同。 |
| `TOKEN_MODEL_CATALOG_ROLLBACK` | 内置 Token 标准投影包含 `{source_catalog_version}`。 | 标准 critical 为当前/source catalog、diff、Billing版本及三个集合fingerprint、policy version共8字段；标准 financial 同模型命令六字段。 | Token [`catalog_rollback_governance.go`](../internal/token/catalog_rollback_governance.go) 与 [`approval_snapshot.go`](../internal/token/approval_snapshot.go) 校验；GSA-14 才补 proof/revision 目标。 |
| `TOKEN_CREDENTIAL_REVOKE` | 内置 Token 标准投影包含 `token_account_id,expected_account_version,credential_id,credential_version`。 | 标准 critical 为账户/凭据版本、状态、fingerprint、route/lease/inflight集合、replacement版本、policy version共11字段；标准 financial 为 `{}`。 | Token [`approval_snapshot.go`](../internal/token/approval_snapshot.go) 与 [`command_governance.go`](../internal/token/command_governance.go) 校验；秘密与隔离会话不进入快照。 |

以上仅记录当前形状。目标 proof_input 的类型、null/0、禁止字段、RFC 8785 与 legacy golden 仍由整改设计第 8.2 节和 GSA-05/13～15负责，本文不创建第二套 schema。

### 2.3 当前 consumer 与恢复边界

- Billing [`main.go`](../cmd/billing-service/main.go) 当前只启动一个 `billing-command-executor-v1`，使用普通 `Consume`，由 [`approval_consumer.go`](../internal/billing/approval_consumer.go) 按三类命令分派；[`nats_resources.go`](../internal/platform/events/nats_resources.go) 为这个共享 durable 固定 `MaxDeliver=12`，[`nats.go`](../internal/platform/events/nats.go) 在第12次处理错误时发布通用 dead-letter，发布成功后调用 `TermWithReason`。这不是目标中的三个命令专属 durable，也不等于“应用层持久记录最多12次领域尝试 + DLQ PubAck/确认式收口”，当前同样没有 owner-domain activation/epoch。GSA-03、06～09、15～16分别拥有 activation、事件/Relay、consumer/DLQ、proof 和失败恢复增量。
- Token [`approval_consumer_supervisor.go`](../internal/token/approval_consumer_supervisor.go) 当前在 ACTIVE+正 epoch 下注册三个 `ConsumeUntilResolved` durable；[`nats_resources.go`](../internal/platform/events/nats_resources.go) 将其保持为 `MaxDeliver=-1`。真实检查点是 consumer 开始处理时读取 activation/epoch、调用 Token execute 前重读、调用 Account Complete 前再重读；Account Begin 与 Token handler 还各自执行独立 gate。不能把这些检查概括为每次领域调用前后都有成对读取。目标 proof、Account control epoch、Relay fencing 和历史 cutover 仍未实现。
- 当前 Billing/Token 都消费同一 v1 授权事件；人工 replay 禁令、sidecar、proof-era canonical bytes、Billing 应用层12次尝试与确认式 DLQ 是后续 GSA-05～09 的目标，不能从当前 consumer 行为推导为已交付。

## 3. 八类 policy-only 财务命令

### 3.1 共同现状

以下八类均已由 [`000018_approval_commands.up.sql`](../migrations/account/000018_approval_commands.up.sql) 和[静态注册表](../internal/platform/approvalpolicy/registry.go)登记为 Account policy version 1 / snapshot schema version 1、`APPROVAL_REQUIRED`、request=`platform.billing.repairs.request`、approve=`platform.billing.repairs.approve`、risk=`FINANCIAL_FACT_CHANGE`、24小时有效期、5分钟执行窗口、metadata executor=`billing-command-executor`。但是它们均不在 `approvalpolicy.Executable` 集合中；GSA-01 后 create/update/submit/APPROVE/Begin 失败关闭，已有申请只读，SUBMITTED 仍可安全 REJECT/WITHDRAW。

这些 metadata 不构成业务 API、closed command/snapshot、presenter、proof、consumer、executor、补偿账务、恢复或 activation。当前数据库尚无这些 Billing 命令的 owner-domain 持久 activation 行；有效的失败关闭事实是它们静态 `Executable=false` 且 GSA-01 在推进点拒绝，而不是已经持久化了 `INACTIVE`。GSA-03 才负责建立权威投影并把八类物化为 `INACTIVE`。Billing 当前共享 consumer 对它们返回 unsupported，不得因 executor identity 字符串相同而视为可执行。每类必须先完成独立领域设计并经 B 审核，再进入实现；此前无该审批命令的专用业务执行入口/handler、无审批中心 requestable 动作。注册表已明确分类的充值商品管理命令、`BILLING_NORMAL_REFUND`、对账 run/import/case update 等既有 `AUDIT_ONLY` 命令保持各自原合同；这里只保全这些已登记分类，不泛化为整个财务业务域，Provider 自动链等边界仍由各自合同管理。上述既有命令不等价于这八类财务修复审批命令，也不因本基线被改道或移除。

### 3.2 单类所有权矩阵

| policy-only 命令 | 当前领域/API事实 | 领域设计 | 实现与真实E2E | activation前置 |
|---|---|---|---|---|
| `ENTITLEMENT_COMPONENT_ADJUSTMENT` | 仅共同 policy metadata；无专用命令 API/handler。 | GSA-21 | GSA-22 | GSA-03/10～16及本类设计、实现、recovery、发布门禁全部通过后才可单独激活。 |
| `LEDGER_REPAIR` | 同上。 | GSA-23 | GSA-24 | 同上，不得改写历史账本。 |
| `RECEIVABLE_WRITE_OFF` | 同上。 | GSA-25 | GSA-26 | 同上，须先冻结应收不变量与补偿凭证。 |
| `PAYMENT_FINANCIAL_REPAIR` | 同上。 | GSA-27 | GSA-28 | 同上，外部支付证据和未知结果规则必须独立验收。 |
| `REFUND_EXCEPTION_REPAIR` | 同上。 | GSA-29 | GSA-30 | 同上，不开放普通退款产品入口。 |
| `CHARGEBACK_FINANCIAL_REPAIR` | 同上。 | GSA-31 | GSA-32 | 同上，拒付证据与补偿账务不得借用退款规则。 |
| `RECONCILIATION_FINANCIAL_REPAIR` | 同上。 | GSA-33 | GSA-34 | 同上，对账不变量和高水位独立冻结。 |
| `USAGE_SETTLEMENT_REPAIR` | 同上。 | GSA-35 | GSA-36 | 同上，绝不重发模型请求。 |

Portal 联调只由 GSA-37 在对应领域实现通过后逐类接入；生产 activation 只属于取得单独授权后的 GSA-40。T9 异常索引未获确认，不属于这八类的隐含入口；GSA-09不得被误作T9实现，T9另行确认并完整交付前始终不显示导航。

## 4. 后续 GSA 所有权与兼容门禁

| 当前已知缺口 | 唯一增量所有权 | 不得破坏的基线 |
|---|---|---|
| Billing owner-domain activation 与三/八命令状态投影 | GSA-03 | 三类既有 Billing 命令保持兼容 ACTIVE，八类物化为默认 INACTIVE；Token 三命令继续复用自身持久 activation，不进入 Billing 投影。 |
| Account control、双epoch与锁 | GSA-04 | Token activation仍由Token持久真相拥有；Account不新建第二真相。 |
| typed encoder、Outbox sidecar、Relay与consumer/DLQ | GSA-05～09 | 同一原Outbox的自动at-least-once恢复不等于人工replay；当前事件ID/业务payload不改写。 |
| directory/cursor/v2 minimal list/closed detail | GSA-10～12 | 当前v1 wire和历史读取保留；不可见与不存在等价。 |
| Account proof 与批准检查点 | GSA-13 | 15分钟检查必须在领域precheck/proof前；Wallet无新增recent auth；REJECT零领域调用。 |
| Token/Billing逐命令proof和执行前revision | GSA-14～15 | 六类p/s不原地修改，无法可信映射的历史失败关闭。 |
| 逐命令失败分类与恢复worker | GSA-16 | 未知默认只查证，不通用自动重试。 |
| Portal wire锁定、审批中心和业务页闭环 | GSA-17～20、37 | 中心不编辑复杂表单、不触发Provider探测，旧URL/history兼容。 |
| 迁移、N/N-1、生产等价证据与activation | GSA-38～40 | 代码或DESIGN通过不等于已部署；实际生产操作另行授权。 |

## 5. GSA-02 验收边界

GSA-02 只在以下条件全部满足后可由 B 审核为 DONE：

1. 六类既有命令与八类 policy-only 各且仅有一条事实记录，p/s、权限、当前接口、实际快照、recent-auth现状、consumer/executor和activation缺口均可追溯到仓库证据；
2. 明确 Billing 当前一个共享 consumer 与 Token 当前三个独立 durable，未把目标三Billing durable、proof、v2或恢复协议写成现状；
3. Wallet实际快照与registry metadata、Billing领域字符串policy_version与Account整数policy version均被区分；
4. 15分钟目标合同与当前Account只校验`AuthenticatedAt`非零的缺口同时记录，Wallet不被新增要求；
5. 八类当前由静态`Executable=false`失败关闭，并映射到独立GSA-21～36；GSA-03才物化持久`INACTIVE`，没有为缺少领域设计的命令猜测API、不变量或activation；现有普通`AUDIT_ONLY`领域操作不受影响；
6. 所有相对链接存在，任务/status交叉引用一致，文档检查与diff检查通过。本DESIGN通过不代表产品功能实现或部署。
