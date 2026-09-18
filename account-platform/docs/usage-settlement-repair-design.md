# 用量结算修复（`USAGE_SETTLEMENT_REPAIR`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-35 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-36、不开启 activation；用户范围决策前不把本文算作设计完成。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`、请求/审批权限 `platform.billing.repairs.request` / `.approve`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`。
- **静态不可执行**且不在 `executableApprovalCommands` 内；Billing 侧只有 `('USAGE_SETTLEMENT_REPAIR',1,1,'INACTIVE',1)`。
- **基线硬约束**：本命令**绝不重发模型请求**（[命令基线](admin-governance-security-command-baseline.md)）。

## 2. 用量结算现状与不变量（事实）

- **预留与结算**：`usage_holds`（`status ∈ {ACTIVE,COMMITTED,RELEASED,EXPIRED,REVIEW}`、`max_microcredits>0`、可选 `actual_microcredits>=0`）与 `usage_hold_allocations`（`PRIMARY KEY(hold_id,package_id)`、`reserved_microcredits>0`、`settled/released>=0`、`CHECK settled+released<=reserved`）共同承载一次用量在多个 package 上的预留与最终结算。；出处：`migrations/billing/000002_ledger.up.sql`、`000012_package_hold_allocations.up.sql`；`usage_request_id` 唯一，影响幂等键设计
- **结算落账**：结算写入 `USAGE` journal（业务引用 `usage:<usage_request_id>`）并按信用 lot 扣减钱包/组件（`internal/billing/postgres.go`），全程要求 journal 平衡且不可改写。
- **既有相关路径**：Token 侧 `USAGE_RECONCILE` 任务与用量对账（`cmd/token-gateway` 的 `usage-reconciliation`）、`usage_hold_allocations` 的释放/提交、以及支付异常/对账案件；它们只做观察、重放安全收敛与既有结算，不提供可申请、可审批、可证明的结算修复命令。；另有 Billing 侧管理员安全释放 `BILLING_USAGE_HOLD_SAFE_RELEASE`（AUDIT_ONLY，`internal/billing/service.go`）与用量损失复核 `usage_loss_reviews`（`internal/billing/postgres.go` 的 `ENTITLEMENT_EXPIRED_UNCERTAIN`，并入 `organization_lifecycle.go` 的组织阻塞）
- **账本与余额不变量**：journal/posting 不可变且必须平衡、`business_reference` 全局唯一；钱包 `posted_microcredits>=0`、credit lot 有上下界；`usage_hold_allocations` 的守恒关系不得被破坏。
- **缺口**：没有定义"哪些结算差异允许修复"（少结算/多结算、重复结算、hold 已 RELEASED 却存在已结算量、package 归属错误）、证据来源（用量记录与 hold 事实）、写入边界（只写 journal/posting 还是允许回补钱包/package）与未知结果规则；也明确禁止以"重发模型请求"作为修复手段。

## 3. 与相邻流程的职责边界（避免重复建设）

- 用量对账/重放已有独立路径（Token `USAGE_RECONCILE` 与结算服务）；本命令不得成为第二结算入口，也不得触发任何模型调用或重放原推理请求。
- 与[账本修复设计](ledger-repair-design.md)的成对 posting 规则、以及支付/拒付修复的资金口径必须显式区分：用量结算修复只处理消费侧差异。
- 若差异源于 package 归属或权益纠错，应与 GSA-22 权益组成纠错先/互斥，避免同一差额被修两次。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版范围 | (a) 仅"少结算/未结算"补记；(b) 仅"多结算/重复结算"冲销；(c) 两者 | 决定 closed command/snapshot/proof 字段与证据要求 |
| 事实证据 | (a) 仅 `usage_holds`/`usage_hold_allocations`/`USAGE` journal 既有事实；(b) 需要 Token 侧用量记录交叉证据 | 决定 proof 是否可离线重算、是否需要新证据表 |
| 写入边界 | (a) 只写 journal/posting；(b) 允许回补钱包/credit lot/package 有效量 | 决定是否触碰余额守恒与消费顺序 |
| 未知结果 | 沿用 GSA-16 分类，未知只读查证；是否需要"零效果"证据才允许重试 | 决定 executor/consumer 的 result-first 与 DLQ 收口 |
| 幂等与限额 | 单笔上限、同一 hold/用量可修复次数、冷却窗口 | 决定是否新增限额表与并发门禁 |
| 权限与版本 | (a) 复用现有 permission/policy 版本；(b) 新增专用 permission 或 policy 版本 | 决定是否需要 Account/Billing 迁移、合同与灰度顺序 |

无论选择哪一项：默认 `INACTIVE`，需本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权；**任何情况下不得重发模型请求**，不得改写历史账本、用量/结算事实。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；不定义 closed schema 与 error 分类。
- 不改动用量预留/释放/结算、Token 用量对账、支付/拒付/退款与应收的任何行为。

## 6. 自检与验证

- 每条事实均可回溯到上列迁移或源码路径。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-35 设计冻结与 GSA-36 实现。
