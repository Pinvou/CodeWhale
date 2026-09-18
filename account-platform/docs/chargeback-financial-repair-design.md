# 拒付财务修复（`CHARGEBACK_FINANCIAL_REPAIR`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-31 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-32、不开启 activation；用户范围决策前不把本文算作设计完成。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`、请求/审批权限 `platform.billing.repairs.request` / `.approve`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`。
- **静态不可执行**且不在 `executableApprovalCommands` 内；Billing 侧只有 `('CHARGEBACK_FINANCIAL_REPAIR',1,1,'INACTIVE',1)`，没有 handler、proof、executor、consumer 或恢复分类。
- **基线明确约束**：拒付证据与补偿账务不得借用退款规则（[命令基线](admin-governance-security-command-baseline.md)）。

## 2. 拒付现状与不变量（事实）

- **拒付案件**：`chargeback_cases` 以 `payment_evidence_id` 唯一、`UNIQUE(payment_transaction_id,provider_chargeback_id)` 与组合外键（交易/订单、package/交易）冻结"同一 provider 拒付"，状态仅 `CONFIRMED/RESOLVED`，并以 `revoked_base_microcredits`/`revoked_bonus_microcredits>=0` 记录已撤销额度、以 `receivable_id` 关联应收。
- **既有确认路径**（`internal/billing/chargebacks.go`）：记录 `payment_evidence`（拒付类型、provider 状态 `..._CONFIRMED`）、暂停成员配额授予（`SUSPENDED` + 拒付原因）、创建 P1 支付异常、按组件撤销额度（撤销基数使用 `effectiveComponentOriginal(original, reclassified_out)`，与 GSA-22 的有效量口径一致）、写拒付 journal，并在 BASE 不足时创建 `receivables(source_type='CHARGEBACK')`。
- **应收不变量**（与[应收核销设计](receivable-write-off-design.md) §2 相同）：`settled+outstanding=original`、`(status∈{SETTLED,WRITTEN_OFF})=(outstanding=0)`；`receivable_allocations` 已预留 `APPROVED_WRITE_OFF` 但无写入者。
- **账本不变量**：journal/posting 不可变、必须平衡、`business_reference` 全局唯一，钱包与 credit lot 有上下界。
- **缺口**：没有可申请/可审批/可证明的"拒付财务修复"命令；"拒付场景下要修复什么"（撤销后 BASE 缺口、拒付后又发生退款、重复拒付、provider 金额与本地不一致）、外部证据来源、写入边界与未知结果规则都未定义。

## 3. 与相邻流程的职责边界（避免重复建设）

- 拒付确认与撤销额度已有领域路径；本命令不得成为第二个拒付确认入口，也不得改写已确认的 `chargeback_cases`。
- 补偿账务不得借用退款语义：退款按 package 当前有效组件、且受未结应收阻塞；拒付补偿若允许入账必须另行定义（并说明与应收核销、`MANUAL_REPAIR` 的边界）。
- 与 GSA-25 应收核销存在天然衔接（BASE 短缺形成应收、后续入账先抵扣），两者若同批实现必须显式定义先后与互斥。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版范围 | (a) 仅 BASE 短缺的补偿入账；(b) 仅 provider 金额/归属不一致的等额纠正；(c) 两者 | 决定 closed command/snapshot/proof 字段与外部证据要求 |
| 外部证据 | (a) 仅 `chargeback_cases`/`payment_evidence`/`receivables` 既有事实；(b) 需要额外 provider 或对账证据 | 决定 proof 是否可离线重算、是否需要新证据表 |
| 写入边界 | (a) 只写 journal/posting 与应收核销；(b) 允许回补钱包/credit lot/package 有效量 | 决定是否触碰余额守恒与消费顺序 |
| 未知结果 | 沿用 GSA-16 分类，未知只读查证；是否需要"零效果"证据才允许重试 | 决定 executor/consumer 的 result-first 与 DLQ 收口 |
| 幂等与限额 | 单笔上限、同一拒付案件可修复次数、冷却窗口 | 决定是否新增限额表与并发门禁 |
| 权限与版本 | (a) 复用现有 permission/policy 版本；(b) 新增专用 permission 或 policy 版本 | 决定是否需要 Account/Billing 迁移、合同与灰度顺序 |

无论选择哪一项：默认 `INACTIVE`，需本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权；不得改写历史账本、拒付案件或支付事实。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；不定义 closed schema 与 error 分类。
- 不改动拒付确认、退款、应收创建/抵扣与对账的任何行为。

## 6. 自检与验证

- 每条事实均可回溯到上列迁移或源码路径。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-31 设计冻结与 GSA-32 实现。
