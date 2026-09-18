# 退款异常修复（`REFUND_EXCEPTION_REPAIR`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-29 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-30、不开启 activation；用户范围决策前不把本文算作设计完成。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求/审批权限 `platform.billing.repairs.request` / `.approve`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`。
- **静态不可执行**且不在 `executableApprovalCommands` 内；Billing 侧只有 `('REFUND_EXCEPTION_REPAIR',1,1,'INACTIVE',1)`，没有 handler、proof、executor、consumer 或恢复分类。
- **基线明确禁止**：本命令不得开放普通退款产品入口（[命令基线](admin-governance-security-command-baseline.md)）。

## 2. 退款现状与不变量（事实）

- **普通退款**：`refunds` 表以 `merchant_refund_no`、`channel_refund_id`、`idempotency_key` 三重唯一约束冻结一次退款；`amount_fen>0`、`credit_microcredits>0`，状态仅 `REQUESTED/SUCCEEDED/FAILED/MANUAL_REVIEW`；`request_hash` 与 `reason` 记录原始请求。退款金额与组成来自 package 当前有效组件（BASE 可退、BONUS 不可退），并受未结应收阻塞（`refunds.go`）。
- **强制退款**：`forced_refund_cases` 由外部证据驱动，携带 `payment_evidence_id`（唯一）、订单/交易/package/钱包、`provider_refund_id`、`amount_fen>0`、`entitlement_snapshot`，状态仅 `REVIEW_REQUIRED/RESOLVED`，并以 `UNIQUE(payment_transaction_id,provider_refund_id)` 与组合外键冻结"同一交易同一 provider 退款"。
- **相邻流程**：拒付（`chargebacks.go` + `chargeback_cases`）、Refund 消费者（`billing-refund-executor-v1`，`refunds.go`）、包过期与强制退款回收（`packages.go`）、对账与支付异常（`reconciliation.go`、`payment_anomaly_cases`）。
- **账本不变量**（同[账本修复设计](ledger-repair-design.md) §2）：journal/posting 不可变、必须平衡、`business_reference` 全局唯一、钱包与 credit lot 有上下界；退款相关历史调整只能是新增平衡 journal 与配对 posting。
- **缺口**：没有"退款异常"的可申请/可审批/可证明命令；"异常"指哪一类（provider 已退款但系统未冲销、重复退款、金额/归属错误、强制退款案件收口）以及外部证据、写入边界、未知结果规则都未定义。

## 3. 与相邻流程的职责边界（避免重复建设）

- 普通退款与强制退款各自已有领域入口与状态机；本命令不得成为第二个普通退款入口，也不得绕开 `forced_refund_cases` 的证据要求。
- 拒付与退款异常有重叠但语义不同（拒付由支付网络发起）；两者若需联动必须显式定义先后与互斥。
- 退款冲销的组成计算必须复用既有"有效原始量"口径（`original - reclassified_out`），否则会与 GSA-22 的权益纠错以及消费顺序冲突。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版范围 | (a) 仅"provider 已退款但系统未冲销"；(b) 仅重复/错误退款的等额纠正；(c) 含强制退款案件收口 | 决定 closed command/snapshot/proof 字段与外部证据要求 |
| 外部证据 | (a) 仅 `refunds`/`forced_refund_cases`/`payment_evidence` 既有事实；(b) 需要额外 provider 查询或对账证据 | 决定 proof 是否可离线重算、是否需要新证据表 |
| 写入边界 | (a) 只写 journal/posting；(b) 允许回滚钱包/credit lot/package 有效量 | 决定是否触碰余额守恒与退款归属 |
| 未知结果 | 沿用 GSA-16 分类，未知只读查证；是否需要"零效果"证据才允许重试 | 决定 executor/consumer 的 result-first 与 DLQ 收口 |
| 幂等与限额 | 单笔上限、同一退款/交易可修复次数、冷却窗口 | 决定是否新增限额表与并发门禁 |

无论选择哪一项：默认 `INACTIVE`，需本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权；不得改写历史账本、退款记录或支付事实。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；不定义 closed schema 与 error 分类。
- 不改动普通退款、强制退款、拒付、对账与包过期的任何行为。

## 6. 自检与验证

- 每条事实均可回溯到上列迁移或源码路径。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-29 设计冻结与 GSA-30 实现。
