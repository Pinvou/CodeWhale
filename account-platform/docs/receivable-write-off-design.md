# 应收核销（`RECEIVABLE_WRITE_OFF`）领域设计

状态：`IN_PROGRESS / FACTS_RECORDED / DECISION_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。本文只登记 GSA-25 的仓库事实、既有不变量与待用户决策的门禁，不冻结写入协议、不开始 GSA-26、不开启 activation；用户范围决策前不把本文算作设计完成。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies` 与 `approvalpolicy` 注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求权限 `platform.billing.repairs.request`、审批权限 `platform.billing.repairs.approve`、风险类 `FINANCIAL_FACT_CHANGE`、一名非申请人审批、critical 字段 `target_id/resource_version/financial_snapshot/policy_version`、24 小时申请有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`、`internal/platform/approvalpolicy/registry.go`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，create/update/submit/APPROVE/Begin 均在领域调用前失败关闭。
- **Billing 侧只有未激活行**：`('RECEIVABLE_WRITE_OFF',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`）；没有该命令的 handler、proof、executor、consumer durable 或恢复分类。
- **合同层登记为已知命令且恒为 INACTIVE**（`contracts/openapi/billing.yaml`、`test/contract/billing-approval-activation.test.mjs`）。基线文档要求先冻结应收不变量与补偿凭证（[命令基线](admin-governance-security-command-baseline.md)）。

## 2. 应收现状与不变量（事实）

- **应收主表**：`receivables.source_type ∈ {USAGE_OVERAGE,CHARGEBACK}`，`UNIQUE(source_type,source_reference)`，携带钱包、支付订单/交易、权益包与 `usage_request_id`；`original_microcredits>0`、`settled_microcredits>=0`，并有两条硬约束：`settled+outstanding=original` 与 `(status ∈ {SETTLED,WRITTEN_OFF}) = (outstanding=0)`；`status ∈ {OPEN,PARTIALLY_SETTLED,SETTLED,WRITTEN_OFF}`，带 `version` 与 `settled_at`（`migrations/billing/000025_receivables_and_context_freeze.up.sql`）。
- **核销分录用 immutable 明细承载**：`receivable_allocations.source_type ∈ {CASH_RECHARGE_BASE,REFUND_RECOVERY,APPROVED_MANUAL_REPAYMENT,APPROVED_WRITE_OFF}`，要求 `amount_microcredits>0`、必须引用已存在的 `journal_entry_id`，`UNIQUE(receivable_id,source_type,source_reference)`，并有 `receivable_allocations_immutable` 触发器拒绝 UPDATE/DELETE。
- **`APPROVED_WRITE_OFF` 目前没有任何写入者**：Go 代码只有创建应收与“用钱包入账抵扣”两条写入路径（`internal/billing/receivables.go` 的 `createReceivableWithinTransaction`、`settleOpenReceivablesWithinTransaction`），后者按 `CASH_RECHARGE_BASE`/`REFUND_RECOVERY` 等来源分配并同步 `settled/outstanding/status`；没有任何命令、审批、proof、executor 或 consumer 会写入 `APPROVED_WRITE_OFF`。也就是说 schema 已为该命令预留了唯一条目，但业务语义（谁、以何证据、核销多少、对应哪条 journal）尚未定义。
- **账本侧**：系统账户已含 `platform:receivables`（`RECEIVABLE_ASSET`）与 `platform:chargeback-clearing`（`CHARGEBACK_CLEARING`），启动时校验（`internal/billing/system_ledger_accounts.go`）；journal/posting 的不可变与平衡规则见[账本修复设计](ledger-repair-design.md) §2，任何核销都必须是新增的平衡 journal 及其 posting，不能改写历史。

## 3. 与相邻流程的职责边界（避免重复建设）

- **已实现**：应收创建来自用量超额与拒付（`chargebacks.go`、用量结算路径）；钱包入账会先抵扣未结应收（`settleOpenReceivablesWithinTransaction`）；退款的可行性检查会把“存在未结应收”作为阻塞条件（`refunds.go`）；支付订单与支付证据的 `attention_summary` 会因未结应收显示 `RECEIVABLE_OPEN`（`payment_evidence.go`、`payment_anomalies.go`、`chargebacks.go`）；管理员只读接口可列出应收与其分配（`service.go`、`admin_pagination_items.go`）。
- **未实现且不与上述重叠**：`APPROVED_WRITE_OFF` 语义、核销所需证据、部分核销还是全额核销、核销后 `status`/`settled_at` 的推进规则、以及核销的幂等与恢复协议。
- **需用户先定范围的关键歧义**：①`USAGE_OVERAGE` 与 `CHARGEBACK` 两类来源是否都允许核销（拒付应收常与拒付案件、追偿流程耦合）；②核销是否允许部分金额；③核销是否需要钱包/权益侧效果（按现有约束，核销只影响应收与账本，不产生新的可用余额）；④同一应收是否允许多次核销；⑤核销与既有 `WALLET_ADJUSTMENT`、`PAYMENT_FINANCIAL_REPAIR` 的边界。

## 4. 决策门禁（待用户确认，本文不做选择）

| 决策 | 选项 | 影响 |
|---|---|---|
| 首版来源范围 | (a) 仅 `USAGE_OVERAGE`；(b) 仅 `CHARGEBACK`；(c) 两类 | 决定是否需要拒付案件/追偿证据联动，以及是否需要新的事实读取 |
| 核销粒度 | 全额核销 / 允许部分核销（多次核销同一应收） | 决定 CAS 目标（`receivable.version`）、分配唯一键使用方式与并发门禁 |
| 证据要求 | 仅领域内既有持久事实 / 需要外部或人工证据记录 | 决定 proof 是否可离线重算、是否需要新的证据表与归档 |
| 账本形式 | 只写 `receivable_allocations` 与对应平衡 journal（应收账户对向） | 决定 journal `entry_type` 选择（沿用 `ADJUSTMENT`/`REVERSAL`）与是否需要新白名单 |
| 幂等与恢复 | `source_reference` 单次语义 + GSA-16 分类，结果未知只读查证 | 决定 executor/consumer 的 result-first 读取、幂等键与 DLQ 收口 |

无论选择哪一项：默认保持 `INACTIVE`，只有本类设计、实现、recovery 与发布门禁全部通过并取得独立激活授权后才可激活；不得改写 `receivables` 的既有不变量（`settled+outstanding=original`、`status` 与 `outstanding` 的等价关系）或历史账本。

## 5. 本切片不做的事

- 不新增或修改任何迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；不定义 error 分类与 closed schema。
- 不改动既有应收创建/抵扣、拒付、退款阻塞、`attention_summary` 或管理员只读接口的行为。

## 6. 自检与验证

- 每条事实均可回溯到上列迁移或源码路径；“未实现”结论以“无任何写入 `APPROVED_WRITE_OFF` 的代码路径”为依据。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check` 与统一 `account-platform verify`，不使用 SKIP 作为通过证据。

## 7. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定，等用户决策后再进入 GSA-25 设计冻结与 GSA-26 实现。
