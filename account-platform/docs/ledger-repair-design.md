# 账本修复（`LEDGER_REPAIR`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-23 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-24、不开启 activation；用户范围决策前不把本文算作设计完成。命令基线见 [十四命令事实与兼容基线](admin-governance-security-command-baseline.md)，治理与激活约束见 [治理与安全重设计](admin-governance-security-redesign.md) §11 与 §14。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与 `approvalpolicy` 注册表为该命令登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求权限 `platform.billing.repairs.request`、审批权限 `platform.billing.repairs.approve`、风险类 `FINANCIAL_FACT_CHANGE`、一名非申请人审批、critical 字段 `target_id/resource_version/financial_snapshot/policy_version`、24 小时申请有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`、`internal/platform/approvalpolicy/registry.go`）。
- **静态不可执行**：该命令不在 `executableApprovalCommands` 内，因此 create/update/submit/APPROVE/Begin 均在领域调用前失败关闭，Portal 目录以 Billing activation 真相为准（`internal/account/approvals.go`、`internal/account/token_governance_activation.go`）。
- **Billing 侧只有未激活行**：`billing_approval_command_activations` 中为 `('LEDGER_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`）。不存在该命令的 HTTP handler、proof 投影、executor、consumer durable、恢复分类或结果载荷；`internal/platform/approvalconsumer` 的 durable 注册表只有既有六命令与 GSA-22 的权益纠错专属 durable。
- **合同层已登记为已知命令**：OpenAPI 与 Node 合同把该命令列入 11 项 activation 投影并要求 `state=INACTIVE`（`contracts/openapi/billing.yaml`、`test/contract/billing-approval-activation.test.mjs`）。

结论：当前不存在任何可执行路径，也不存在通过审批获得执行授权的可能；本命令要么整体设计后按 GSA-24 实现，要么保持关闭。

## 2. 账本现状与不可改写边界（事实）

- **账户类型与系统账户**：`ledger_accounts.account_type` 的**当前**白名单为七个值——`WALLET_LIABILITY/PAYMENT_CLEARING/USAGE_REVENUE/REFUND_CLEARING/ADJUSTMENT`（`000002`）加 `RECEIVABLE_ASSET/CHARGEBACK_CLEARING`（`migrations/billing/000025_receivables_and_context_freeze.up.sql`）；**六个**系统账户使用固定 UUID 并在 Billing 仓库启动时校验其存在：`platform:payment-clearing`、`platform:usage-revenue`、`platform:refund-clearing`、`platform:adjustment`、`platform:receivables`、`platform:chargeback-clearing`（`migrations/billing/000045_system_ledger_accounts_repair.up.sql`、`internal/billing/system_ledger_accounts.go`、`internal/billing/postgres.go`）。
- **journal 与 posting 必须新增、不可改写**：`journal_entries.business_reference` 全局唯一；`entry_type` 白名单的**当前**取值以迁移 `000025_receivables_and_context_freeze` 为准——`RECHARGE/USAGE/REFUND/REVERSAL/ADJUSTMENT/EXPIRY/RECEIVABLE/CHARGEBACK`（该迁移替换了 `000013_package_expiry` 的 `EXPIRE/REPAIR_CREDIT` 组合，`EXPIRE` 更名为 `EXPIRY` 且不再包含 `REPAIR_CREDIT`）。另有 `journal_entries_immutable`、`ledger_postings_immutable` 触发器拒绝 UPDATE/DELETE；`ledger_postings` 要求 `delta_microcredits<>0`，`ledger_postings_balanced` 是 `DEFERRABLE INITIALLY DEFERRED` 约束触发器，要求同一 journal 内 posting 求和为 0（`migrations/billing/000002_ledger.up.sql`）。
- **已登记缺陷（审核发现，尚未修复）**：`internal/billing/payments.go` 的既有支付修复路径在 `repair=true` 时写入 `journalType="REPAIR_CREDIT"`，而 `000025` 之后的 `entry_type` CHECK 已不含该值，因此该路径会在 journal 插入处违反约束而失败；仓库内没有任何测试覆盖该分支。修复需要在"放宽白名单（前向迁移加入 `REPAIR_CREDIT`）"与"改用既有取值"之间做出选择，属于 GSA-27 范围内的独立切片。
- **余额与额度护栏**：`wallets.posted_microcredits>=0` 且带 `version`；`credit_lots.remaining_microcredits` 限定在 `[0,original_microcredits]`；`entitlement_packages`/`entitlement_components` 有各自的守恒约束与只读投影，GSA-22 已把 `original-reclassified_out` 纳入包有效量。
- **已有账本写入者与白名单的一致性**（修复设计必须复用而不是绕开）：写入者包括普通充值 `RECHARGE`（`postgres.go`）、用量结算 `USAGE`（`postgres.go`）、人工调账 `ADJUSTMENT`（`adjustments.go`）、普通退款 `REFUND`（`refunds.go`）、拒付 `CHARGEBACK`（`chargebacks.go`）、包过期 `EXPIRE`（`packages.go`）、已验证支付补记 `REPAIR_CREDIT`（`payments.go`）、强制退款 `FORCED_REFUND`（`forced_refunds.go`）以及 GSA-22 权益组成纠错的成对 `ADJUSTMENT` posting（`entitlement_correction_execute.go`）。
- **已登记缺陷（审核发现，尚未修复）**：`entry_type` 白名单与写入者不一致，且不止一处——白名单（`000025`）**不含** `EXPIRE`、`REPAIR_CREDIT`、`FORCED_REFUND` 三个实际写入值（包过期、已验证支付补记、强制退款确认都会在 journal 插入处违反 CHECK），而白名单中的 `REVERSAL/EXPIRY/RECEIVABLE` 目前没有任何写入者；同时 `forced_refunds.go` 以 `source_type='FORCED_REFUND'` 调用 `createReceivableWithinTransaction`，而 `receivables.source_type` 的 CHECK 只接受 `USAGE_OVERAGE/CHARGEBACK`（`000025`），因此 BASE 短缺的 `CONFIRM_FORCED_REFUND` 必然失败。上述路径在仓库内均无真实 PostgreSQL 集成用例。修复需要在"前向放宽白名单/枚举"与"改用既有取值"之间做出取舍，属 GSA-27/29/31 的共同前置决策。
- **可发现的结论**：唯一 `business_reference` 天然是幂等边界；任何修复只能是“新增一个平衡 journal 及其配对 posting（必要时附带新 credit lot 或 component）”，不能修改、删除、重放既有 journal/posting，也不能让历史台账失去可核对性。

## 3. 与相邻流程的职责边界（避免重复建设）

- “支付已确认但未入账”已有对账驱动的 `REPAIR_CREDIT` 路径，本命令不应重复实现同一场景。
- 钱包余额的人工增减已有 `WALLET_ADJUSTMENT`（审批 + 执行 + `ADJUSTMENT` 账户对向 posting），本命令不应成为第二个任意金额入口。
- 权益组成与原支付拆分错配已有 GSA-22 `ENTITLEMENT_COMPONENT_ADJUSTMENT`（含 zero-history 门禁与成对账本）。
- `payment_order_repairs`、`reconciliation_cases` 目前记录支付/对账事实与观察，不直接改账本。
- **仍无归属的账本级缺口**（需要用户先定范围）：①journal 平衡但账户或 credit lot 归属错误；②钱包/包派生余额与账本事实不一致（未入账或重复入账）；③业务已发生但缺少 journal；④分录金额与业务事实不符。每类的证据来源、允许写入的字段集合与失败关闭点都不同，不能用一个通用 schema 一次性覆盖。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版范围 | (a) 仅“归属错误”的等额冲正；(b) 含“缺失/重复记账”的补记；(c) 仅只读核对并交由人工 `WALLET_ADJUSTMENT` | 直接决定 closed command/snapshot/proof 字段、需要的外部证据与恢复分类 |
| 事实证据 | (a) 仅领域内既有持久事实；(b) 需要外部对账/支付证据；(c) 需要运维人工确认记录 | 决定 proof 是否可离线重算、是否需要新的证据表与归档 |
| 写入边界 | (a) 只允许 journal/posting 层纠正；(b) 允许同时调整 wallet/credit lot/package 有效量 | 决定是否触碰余额守恒、退款/拒付/应收归属与消费顺序 |
| 幂等与恢复 | 沿用 `business_reference` 单次语义 + GSA-16 分类，是否允许“结果未知时只读查证” | 决定 executor/consumer 的 result-first 读取与 DLQ 收口 |
| 权限与版本 | (a) 复用现有 permission/policy 版本；(b) 新增专用 permission 或 policy 版本 | 决定是否需要 Account/Billing 迁移、合同与灰度顺序 |
| journal `entry_type` | (a) 前向迁移放宽白名单（加入 `EXPIRE`/`REPAIR_CREDIT`/`FORCED_REFUND`）；(b) 改用既有取值；(c) 先修 `receivables` 来源枚举再定 | 决定既有包过期/补记/强制退款路径能否恢复（见 §2 缺陷） |
| 金额与次数 | 单笔上限、同一目标可修复次数、是否需要冷却窗口 | 决定是否新增限额表与并发门禁 |

无论选择哪一项：默认保持 `INACTIVE`，只有本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权后才可激活，且不得改写历史账本。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不冻结 command/snapshot/proof schema，不定义 error 分类，不把本文当作 GSA-24 的开工依据。
- 不改动既有六命令、GSA-22 权益纠错、`REPAIR_CREDIT`、`WALLET_ADJUSTMENT` 或对账流程的任何行为。

## 6. 自检与验证

- 本文每条事实都能回溯到上列迁移或源码路径；结论只写“当前不存在可执行路径”，不推测未来实现。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`，不使用 SKIP 作为通过证据。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-23 设计冻结与 GSA-24 实现。
