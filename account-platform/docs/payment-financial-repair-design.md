# 支付财务修复（`PAYMENT_FINANCIAL_REPAIR`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-27 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-28、不开启 activation；用户范围决策前不把本文算作设计完成。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与 `approvalpolicy` 注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求权限 `platform.billing.repairs.request`、审批权限 `platform.billing.repairs.approve`、风险类 `FINANCIAL_FACT_CHANGE`、一名非申请人审批、critical 字段 `target_id/resource_version/financial_snapshot/policy_version`、24 小时申请有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`、`internal/platform/approvalpolicy/registry.go`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，create/update/submit/APPROVE/Begin 在领域调用前失败关闭。
- **Billing 侧只有未激活行**：`('PAYMENT_FINANCIAL_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`）；没有该命令的 handler、proof、executor、consumer durable 或恢复分类。
- **基线要求**：外部支付证据与"结果未知"规则必须独立验收（[命令基线](admin-governance-security-command-baseline.md)）。

## 2. 支付/账本现状与既有修复能力（事实）

- **支付事实链**：`payment_orders`（含 `provider_id/provider_version`、`amount_fen`、`credit_microcredits`、`status`/`fulfillment_status`）、`payment_transactions`、`payment_evidence`、`payment_provider_failure_confirmations`、`payment_anomaly_cases`、`payment_risk_attempts`。
- **既有"已确认支付但未入账"修复路径**：`internal/billing/reconciliation.go` 的 `mergeSuccessfulPaymentWithinTransaction(..., "MANUAL_REPAIR", "repair:<order>:<txn>", ..., repair=true)` 会（a）在 `payments.go` 写 `REPAIR_CREDIT` journal 并给钱包入账、建立 credit lot/package 与组件，（b）向 `payment_order_repairs` 追加一行审计：`payment_order_id`、`payment_transaction_id`、`payment_evidence_id`、`actor_id`、`reason(1..500)`、`provider_snapshot`、`entitlement_package_id`（唯一）、`points_awarded>0`、`wallet_balance_before>=0`、`wallet_balance_after>=before`；唯一性在迁移 `000024` 由订单改为交易——`payment_order_repairs_payment_order_id_key` 被移除，改为 `UNIQUE(payment_transaction_id)` 并新增组合外键 `(payment_transaction_id,payment_order_id) → payment_transactions(id,payment_order_id)`，因此插入使用 `ON CONFLICT (payment_transaction_id) DO NOTHING`，语义是"同一支付交易只修复一次"（`migrations/billing/000014`、`000015`、`000024`）。
- **其它支付相关既有能力**：拒付（`chargebacks.go`）、退款/强制退款（`refunds.go`、`packages.go`）、延迟支付评审（`late_payment_reviews.go`）、对账（`reconciliation.go` 的日对账与案件）、层级止损（`000047`）。它们都不接受"任意金额修账"。
- **账本不变量**（与[账本修复设计](ledger-repair-design.md) §2 相同）：journal/posting 不可变、journal 必须平衡、`business_reference` 全局唯一、钱包与 credit lot 有上下界；任何修复只能是新增平衡 journal 与配对 posting，不能改写历史。
- **缺口**：不存在可申请、可审批、可证明（proof）的支付财务修复命令；"哪些支付差额属于本命令、需要什么外部证据、能否触碰钱包/包/额度、未知结果如何只查证"都未定义。

## 3. 与相邻流程的职责边界（避免重复建设）

- `MANUAL_REPAIR` 已覆盖"provider 已确认成功但系统未入账"的补记；本命令不应重复实现同一场景，也不应成为绕过 provider 证据的通用补记入口。
- 拒付、退款、强制退款、延迟支付评审与对账案件各自已有领域路径与状态机；本命令若涉及这些差额，必须显式定义与它们的先后与互斥。
- `payment_evidence`/`payment_provider_failure_confirmations` 是既有外部证据载体；是否足够作为本命令证据、是否需要额外的对账/人工确认记录，属于待决策项。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版范围 | (a) 仅"金额/归属错误"的等额纠正；(b) 仅"已确认支付但未入账"的补记（与 MANUAL_REPAIR 区分点需明确）；(c) 两者 | 决定 closed command/snapshot/proof 字段与外部证据要求 |
| 外部证据 | (a) 仅领域内既有持久事实；(b) 需要 provider 查询/对账证据；(c) 需要人工确认记录 | 决定 proof 是否可离线重算、是否需要新证据表与归档 |
| 写入边界 | (a) 只写 journal/posting；(b) 允许同时补钱包/credit lot/package 有效量 | 决定是否触碰余额守恒、退款/拒付归属与消费顺序 |
| 未知结果 | 沿用 GSA-16 分类，结果未知只读查证、绝不盲重放；是否需要"零效果"证明才允许重试 | 决定 executor/consumer 的 result-first 读取与 DLQ 收口 |
| 幂等与限额 | 单笔上限、同一订单/交易可修复次数、冷却窗口 | 决定是否新增限额表与并发门禁 |

无论选择哪一项：默认保持 `INACTIVE`，只有本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权后才可激活；不得改写历史账本或历史支付事实。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；不定义 closed schema 与 error 分类。
- 不改动既有 `MANUAL_REPAIR`、拒付、退款、对账与延迟支付评审的任何行为。

## 6. 自检与验证

- 每条事实均可回溯到上列迁移或源码路径。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`，不使用 SKIP 作为通过证据。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-27 设计冻结与 GSA-28 实现。
