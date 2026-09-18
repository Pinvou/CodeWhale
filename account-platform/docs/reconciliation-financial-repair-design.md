# 对账财务修复（`RECONCILIATION_FINANCIAL_REPAIR`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-33 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-34、不开启 activation；用户范围决策前不把本文算作设计完成。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`、请求/审批权限 `platform.billing.repairs.request` / `.approve`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`。
- **静态不可执行**且不在 `executableApprovalCommands` 内；Billing 侧只有 `('RECONCILIATION_FINANCIAL_REPAIR',1,1,'INACTIVE',1)`。
- **基线要求**：对账不变量与高水位必须独立冻结（[命令基线](admin-governance-security-command-baseline.md)）。

## 2. 对账现状与不变量（事实）

- **对账运行与案件**：`reconciliation_runs`（运行批次）与 `reconciliation_cases`（`reconciliation_run_id`、可选 `payment_order_id`、`merchant_order_no`、`case_type ∈ {PROVIDER_PAID_INTERNAL_MISSING, INTERNAL_PAID_PROVIDER_MISSING, AMOUNT_MISMATCH, DUPLICATE_TRANSACTION}`、`status ∈ {OPEN, RESOLVED, IGNORED}`、`provider_snapshot`/`internal_snapshot`、`resolution`、创建/解决时间）。
- **既有对账路径**（`internal/billing/reconciliation.go`）：过期支付对账器、日对账 worker、`MANUAL_REPAIR` 修复路径（写 `REPAIR_CREDIT`、入账并追加 `payment_order_repairs` 审计行）、以及支付异常（`payment_anomaly_cases`）与 `payment_evidence`。这些路径目前都以"案件/审计"为主，不存在可申请、可审批、可证明的财务修复命令。
- **账本与应收不变量**：journal/posting 不可变且必须平衡、`business_reference` 全局唯一；应收满足 `settled+outstanding=original` 与状态/余额等价关系（见[应收核销设计](receivable-write-off-design.md) §2）；钱包与 credit lot 有上下界。
- **缺口**：没有为四类对账差异定义"允许修复什么"（仅补齐缺失入账、仅纠正金额、是否允许冲销重复交易）、外部证据要求（是否仅用案件里的 provider/internal 快照）、写入边界（是否补钱包/包）、未知结果规则，以及 proof 需要冻结的对账高水位。

## 3. 与相邻流程的职责边界（避免重复建设）

- `MANUAL_REPAIR` 已覆盖"provider 已确认成功但未入账"的补记；本命令不应重复该场景，也不应成为绕过案件证据的通用补记入口。
- 重复交易（`DUPLICATE_TRANSACTION`）与内部多记的纠正涉及冲销，必须与[账本修复设计](ledger-repair-design.md)的成对 posting 规则、以及拒付/退款路径显式互斥。
- 与 GSA-25 应收核销、GSA-27 支付财务修复、GSA-31 拒付财务修复存在天然衔接，若同批实现必须定义先后与互斥。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版案件类型 | (a) 仅 `PROVIDER_PAID_INTERNAL_MISSING`；(b) 仅 `AMOUNT_MISMATCH`；(c) 全部四类 | 决定 closed command/snapshot/proof 字段与外部证据要求 |
| 高水位 | 以哪个对账运行/时间水位冻结 proof 与并发视角 | 决定 proof 是否可离线重算、是否需要新水位表 |
| 写入边界 | (a) 只写 journal/posting 与案件解决；(b) 允许补钱包/credit lot/package 有效量 | 决定是否触碰余额守恒与消费顺序 |
| 未知结果 | 沿用 GSA-16 分类，未知只读查证；是否需要"零效果"证据才允许重试 | 决定 executor/consumer 的 result-first 与 DLQ 收口 |
| 幂等与限额 | 单笔上限、同一案件可修复次数、冷却窗口 | 决定是否新增限额表与并发门禁 |
| 权限与版本 | (a) 复用现有 permission/policy 版本；(b) 新增专用 permission 或 policy 版本 | 决定是否需要 Account/Billing 迁移、合同与灰度顺序 |

无论选择哪一项：默认 `INACTIVE`，需本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权；不得改写历史账本、对账案件或支付事实。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；不定义 closed schema 与 error 分类。
- 不改动对账运行、案件生命周期、`MANUAL_REPAIR`、拒付/退款与应收的任何行为。

## 6. 自检与验证

- 每条事实均可回溯到上列迁移或源码路径。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-33 设计冻结与 GSA-34 实现。
