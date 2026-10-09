# 应收核销（`RECEIVABLE_WRITE_OFF`）领域设计

状态：`DESIGN_FROZEN / PRIVATE_CANDIDATE_IMPLEMENTED / INDEPENDENT_REVIEW_PENDING / NOT_DEPLOYED`。首版范围由用户在 2026-09-19 确认为**仅 `USAGE_OVERAGE` 来源、允许部分核销（同一应收可多次核销）**，权限与命令版本复用现有 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`（守卫见 §9）。本文冻结 GSA-26 的 closed command/snapshot/proof、不变量、幂等与恢复、失败分类、迁移与回滚及验收映射；当前私有候选已接通原 closed owner/proof/consumer/审批与专属表单，activation 行仍默认 `INACTIVE`；不代表实际前序承接、生产激活或整波审核通过。命令基线见 [十四命令事实与兼容基线](admin-governance-security-command-baseline.md)，治理与激活约束见 [治理与安全重设计](admin-governance-security-redesign.md) §11 与 §14，账本不可改写边界见 [账本修复设计](ledger-repair-design.md) §2。

## 1. 设计冻结时的命令基线（历史事实，非当前候选状态）

- **只有策略元数据**：Account `approval_command_policies` 与 `approvalpolicy` 注册表登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求权限 `platform.billing.repairs.request`、审批权限 `platform.billing.repairs.approve`、风险类 `FINANCIAL_FACT_CHANGE`、一名非申请人审批、critical 字段 `target_id/resource_version/financial_snapshot/policy_version`、24 小时申请有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`、`internal/platform/approvalpolicy/registry.go`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，create/update/submit/APPROVE/Begin 均在领域调用前失败关闭。
- **Billing 侧只有未激活行**：`('RECEIVABLE_WRITE_OFF',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`）；没有该命令的 handler、proof、executor、consumer durable 或恢复分类。
- **合同层登记为已知命令且恒为 INACTIVE**（`contracts/openapi/billing.yaml`、`test/contract/billing-approval-activation.test.mjs`）。

## 2. 应收现状与不变量（事实）

- **应收主表**：`receivables.source_type ∈ {USAGE_OVERAGE,CHARGEBACK}`，`UNIQUE(source_type,source_reference)`；`original_microcredits>0`、`settled_microcredits>=0`，两条硬约束 `settled+outstanding=original` 与 `(status ∈ {SETTLED,WRITTEN_OFF}) = (outstanding=0)`；`status ∈ {OPEN,PARTIALLY_SETTLED,SETTLED,WRITTEN_OFF}`，带 `version` 与 `settled_at`（`migrations/billing/000025_receivables_and_context_freeze.up.sql`）。
- **核销明细不可变**：`receivable_allocations.source_type ∈ {CASH_RECHARGE_BASE,REFUND_RECOVERY,APPROVED_MANUAL_REPAYMENT,APPROVED_WRITE_OFF}`，`amount_microcredits>0`，必须引用既有 `journal_entry_id`，`UNIQUE(receivable_id,source_type,source_reference)`，`receivable_allocations_immutable` 触发器拒绝 UPDATE/DELETE（`000025`）。
- **设计冻结时 `APPROVED_WRITE_OFF` 没有核销写入者**：Go 侧只有创建应收与“钱包入账抵扣”两条写入路径（`internal/billing/receivables.go` 的 `createReceivableWithinTransaction`、`settleOpenReceivablesWithinTransaction`）。抵扣路径按 `CASH_RECHARGE_BASE`/`REFUND_RECOVERY` 分配，并同步 `settled/outstanding/status/version/settled_at`、钱包 `receivable_microcredits/financial_status/financial_freeze_reason` 与 `payment_orders.attention_summary`。
- **钱包侧的应收投影**：创建应收时 `wallets.receivable_microcredits += amount`、`financial_status='RECEIVABLE_OPEN'`、`financial_freeze_reason=<source_type>`、`version+1`；抵扣时等量回落，清零则 `financial_status='CLEAR'` 且 `financial_freeze_reason=NULL`（`receivables.go`）。因此任何核销都必须同步该投影，否则钱包与应收事实不一致。
- **账本侧**：系统账户含 `platform:receivables`（`RECEIVABLE_ASSET`）、`platform:adjustment`（`ADJUSTMENT`），启动时校验（`internal/billing/system_ledger_accounts.go`）；journal/posting 只能追加、必须平衡（`000002`）。

## 3. 与相邻流程的职责边界（避免重复建设）

- **已实现**：应收创建来自用量超额与拒付（`chargebacks.go`、用量结算路径）；钱包入账先抵扣未结应收；退款可行性检查把“存在未结应收”作为阻塞条件（`refunds.go`）；`attention_summary` 因未结应收显示 `RECEIVABLE_OPEN`；管理员只读接口可列出应收与分配（`service.go`、`admin_pagination_items.go`）。
- **本命令只定义 `APPROVED_WRITE_OFF`**：减免而不收现的审批核销。现金/退款回收`CASH_RECHARGE_BASE`/`REFUND_RECOVERY`、人工还款`APPROVED_MANUAL_REPAYMENT`保持原合同，不在本设计范围。
- **首版排除**：`CHARGEBACK` 来源应收（与拒付案件、追偿流程耦合，属 GSA-31）；钱包余额或权益侧补偿（属 `WALLET_ADJUSTMENT`/GSA-21/GSA-27）；应收创建、抵扣顺序与 `attention_summary` 的既有语义不改写。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**唯一允许的领域变化**（单次执行恰好产生，全部在同一 Billing Serializable 事务内）：

1. 一条 `receivable_allocations`：`source_type='APPROVED_WRITE_OFF'`、`source_reference='write-off:'||<approval_request_id>`、`amount_microcredits=A`（`0<A<=核销前 outstanding`）、`journal_entry_id` 指向本次新增 journal、`wallet_id` 等于应收钱包。
2. `receivables` 单行推进（`WHERE id=$1 AND version=$2 AND outstanding_microcredits>=$3`）：`settled_microcredits+=A`、`outstanding_microcredits-=A`、`version+=1`、`updated_at=now`、`settled_at=CASE WHEN outstanding=0 THEN now ELSE settled_at END`、`status=CASE WHEN outstanding=0 THEN 'WRITTEN_OFF' ELSE 'PARTIALLY_SETTLED' END`。
3. `wallets` 单行同步（`WHERE id=$1 AND receivable_microcredits>=$2`）：`receivable_microcredits-=A`、`version+=1`、`updated_at=now`，清零时 `financial_status='CLEAR'`、`financial_freeze_reason=NULL`，否则保持 `'RECEIVABLE_OPEN'` 与原冻结原因。
4. 一条平衡 `journal_entries`：`entry_type='ADJUSTMENT'`、`business_reference='write-off:'||<approval_request_id>`、两条 `ledger_postings`（`RECEIVABLE_ASSET +A`、`ADJUSTMENT −A`，均 `credit_lot_id IS NULL`）。
5. `payment_orders.attention_summary` 按既有抵扣路径的同一规则重算（该钱包应收全部结清则 `RECEIVABLE_OPEN→NORMAL`，否则保持 `RECEIVABLE_OPEN`），`summary_version+=1`。
6. 一条 `receivable_write_off_cases` 不可变审计行（见 §7.1）。

**明确禁止**：

- 禁止改写或删除任何 `receivables`/`receivable_allocations`/`journal_entries`/`ledger_postings` 历史行（不可变触发器与 ACL 双重拒绝）。
- 禁止核销 `CHARGEBACK` 来源、终态应收（`SETTLED`/`WRITTEN_OFF`）、`outstanding=0` 或金额大于 outstanding 的应收。
- 禁止产生任何可用余额、credit lot、entitlement component/package、退款、拒付或 `usage_holds` 变化：`wallets.posted_microcredits/reserved_microcredits`、`credit_lots.*`、`entitlement_*` 必须逐行不变。
- 禁止在钱包 `receivable_microcredits<A` 时执行，禁止跳过钱包投影或只改钱包不改应收。
- 禁止一次执行产生多条 allocation、多条 journal 或 >2 条 posting；禁止复用既有 allocation 的 `source_reference`。

## 5. closed command / snapshot / proof / presenter（冻结）

### 5.1 normalized command（`additionalProperties:false`，RFC 8785，整数为 JSON number）

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `RECEIVABLE_WRITE_OFF` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `receivable_id` | uuid | 目标应收，必须 `source_type='USAGE_OVERAGE'` |
| `write_off_microcredits` | int64 | `>0` 且 `<=` 冻结的 outstanding |
| `expected_receivable_version` | int64 | `>0`，必须等于读取时的 `receivables.version` |
| `reason_code` | string | 唯一取值 `APPROVED_RECEIVABLE_WRITE_OFF` |

自由文本原因单独审计、脱敏，不进入数值事实；不接受任意 SQL、账户 ID、钱包 ID 覆盖或未列字段（钱包与账户一律由领域从应收事实推导）。

### 5.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.receivable_version` | 目标应收 `version` |
| `critical_resource_versions.facts_digest` | 下述事实集合的 closed RFC8785 摘要 |
| `critical_resource_versions.policy_version` | 1 |
| `financial_snapshot.write_off_microcredits` | 本次核销金额 |
| `financial_snapshot.outstanding_microcredits` / `.settled_microcredits` / `.original_microcredits` | 核销前三元组 |
| `financial_snapshot.source_type` / `.source_reference` | 目标应收来源（必须 `USAGE_OVERAGE`） |
| `financial_snapshot.wallet_receivable_microcredits` | 钱包当前应收投影 |

`facts_digest` 覆盖：应收五行（id/钱包/来源/三元组/状态/版本/`settled_at`）、钱包 `receivable_microcredits/financial_status/financial_freeze_reason/version`、来源的 `usage_request_id`/`entitlement_package_id`/`payment_order_id`/`payment_transaction_id`（可空）、既有 allocation 汇总（按 `source_type` 聚合与条数），以及 activation 版本与 policy/schema 版本。`expected_receivable_version` 与 `facts_digest` 同时参与 CAS。

### 5.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属工作负载身份，2 秒绝对期限、64 KiB 上限，重读上述事实并按 §4/§9 判定，返回闭合结果（`PROOF_VALID/PROOF_DENY`、`receivable_id`、`write_off_microcredits`、三元组、`version`、`facts_digest`、`policy_version`、`schema_version`、`activation_version`）。proof 由 Billing 计算，浏览器/Portal/审批人不得生成或提交。
- presenter（GSA-37）只显示应收 id、来源类型/引用、原始/已解决/未结金额、状态与版本、钱包摘要（脱敏）、既有分配的来源构成、本次核销金额与风险警示；不下发支付原文、Provider 数据或内部 proof 字段。
- 免歧义：`settled_microcredits` 是**已解决金额**（现金、退款回收、人工还款、审批核销的合计），其构成只能由不可变 `receivable_allocations` 给出；展示层不得把它表述为“已收到现金”，核销构成必须单独显示。

## 6. 不变量与并发

- 应收三元组：写入后仍满足 `settled+outstanding=original`；`outstanding=0` 时 `status='WRITTEN_OFF'`（本命令路径）并有 `settled_at`；否则 `status='PARTIALLY_SETTLED'`。不新增列、不替换既有 CHECK，避免第二份真相。
- 钱包投影一致：`wallets.receivable_microcredits` 必须等于该钱包所有 `outstanding_microcredits>0` 应收之和（本命令只按 A 等量回落）；`financial_status`/`financial_freeze_reason` 与清零状态一致。
- 分配与账本一致：`sum(receivable_allocations.amount_microcredits)=settled_microcredits`；每条 allocation 的 `journal_entry_id` 指向的 journal 必须平衡且 `sum(ledger_postings.delta)=0`。
- CAS 与锁顺序固定：`SELECT ... FROM receivables WHERE id=$1 FOR UPDATE` → `SELECT ... FROM wallets WHERE id=$2 FOR UPDATE`（先应收后钱包，与抵扣路径的 `ORDER BY created_at,id FOR UPDATE` 顺序兼容，避免死锁）→ 校验 `version`/`outstanding`/`receivable_microcredits` → 写 journal/posting → 写 allocation → 更新应收与钱包 → 更新 `attention_summary`。
- 并发悲观证明：同一应收的两次并发核销至多一次成功（第二次 `version`/`outstanding` 校验失败或 allocation 唯一冲突）；钱包并发入账抵扣与核销都不得使 `receivable_microcredits` 与应收事实偏离。

## 7. 幂等、result-first 与未知结果

### 7.1 新增不可变审计行（GSA-26 前向迁移）

`receivable_write_off_cases`（由 `billing_schema_owner` 持有，运行期仅最小 `INSERT/SELECT`，UPDATE/DELETE 由触发器与 ACL 双重拒绝）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`receivable_id`、`wallet_id`、`amount_microcredits`（`>0`）、`receivable_version_before`、`receivable_version_after`、`status_after`、`facts_digest`、`journal_entry_id`（唯一）、`allocation_id`（唯一）、`state`、`created_at`。

### 7.2 result-first 与唯一性线性化点

- 幂等边界：`receivable_allocations` 的 `UNIQUE(receivable_id,'APPROVED_WRITE_OFF',source_reference)` 与 `business_reference='write-off:'||approval_request_id` 的全局唯一性共同保证**同一审批至多一次效果**；不同审批可对同一应收多次部分核销，每次金额与理由独立冻结。
- result-first：执行器/消费者先按 `approval_request_id`/`execution_key_digest` 只读读回 case 与 allocation，命中即返回同一结果，不生成新 ID、不新增 journal。
- 未知结果：提交结果不明时只读查证 case、allocation、journal、应收三元组与钱包投影是否同时成立；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`，禁止盲重放、禁止假定零效果。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、policy/schema/version 不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态 |
| `source_type='CHARGEBACK'`、终态应收、`outstanding=0`、`A>outstanding`、`expected_receivable_version` 不符、钱包投影不足 | `PROOF_DENY` | 失败关闭、零写入、终态 |
| 同一审批重复投递/重复授权 | 幂等命中 | 读回既有 case/allocation/result，零新写入 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 有界重试；应用层最多 12 次尝试，第 12 次仍失败则发布 DLQ 并按 PubAck/`ACK_PENDING`/DoubleAck 收口；禁止通用人工 replay |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证，不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用 `platform.billing.repairs.request`/`platform.billing.repairs.approve`、一名非申请人审批、24 小时有效期、5 分钟窗口，不新增权限或 policy/snapshot 版本。
- 依据：该命令 `policy_version=1 / snapshot_schema_version=1` 行从未是可执行合同（静态不可执行，create/update/submit/APPROVE/Begin 全部失败关闭），不存在由该版本产生并已执行的领域事实；首次冻结的 closed 形状即该版本合同。
- 激活前只读守卫：目标库中该命令必须不存在 `APPROVED/EXECUTING/EXECUTION_FAILED` 或其它未终态申请，且不存在 `'APPROVED_WRITE_OFF'` allocation 与 `'write-off:'` 前缀 journal；任一存在即阻断激活并按 §8 只读查证。
- `billing_approval_command_activations` 行保持 `INACTIVE`；GSA-26 当前交付默认未激活的专属 durable，使用现有 Billing 注册循环和严格受信资源装配；不存在逐命令 consumer 环境开关；生产激活只属于取得独立授权的 GSA-40。
- 若将来放开 `CHARGEBACK` 来源、允许核销产生余额效果或引入新权限，必须新增 policy/schema 版本与独立设计，不得在本版本内扩大解释。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `receivable_write_off_cases`（编号取**实现时**的下一个可用 Billing 前向编号，冻结基线为 `000061`，而 `000061` 已被 `ledger_repair_cases` 占用，故实现时应顺延），**不改** `receivables`/`receivable_allocations`/`wallets` 的列与 CHECK，不改 `000025/000045/000054/000060`。
- N/N-1：先部署可读新表的 Billing，再部署 proof 端与执行器/消费者，最后业务页；旧客户端与旧 writer 行为不变（本命令未激活时不产生任何新行）。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；已产生的 case/allocation/journal 一律保留可读，禁止破坏性 down。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-26 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 全额核销 | `outstanding→0`、`status='WRITTEN_OFF'`、`settled_at` 落值、allocation 一行、journal 两条相反 posting、钱包投影清零且 `financial_status='CLEAR'` |
| 部分核销（同一应收两次） | 第一次 `PARTIALLY_SETTLED`，第二次清零为 `WRITTEN_OFF`；两次 allocation 引用各自的审批与 journal；`sum(allocation)=settled` |
| 金额边界 | `A=outstanding` 成功；`A=outstanding+1`、`A=0`、负数、溢出全部 `PROOF_DENY` 且零写入 |
| 来源与状态负例 | `CHARGEBACK` 来源、`SETTLED`/`WRITTEN_OFF`、`outstanding=0`、`expected_receivable_version` 漂移全部失败关闭 |
| 钱包投影负例 | 人为使 `receivable_microcredits<A` 时必须拒绝且零写入（应收与钱包投影不得脱钩） |
| 幂等与重复 | 同一审批重复投递/重复授权只命中同一 case；第二 journal 与第二 allocation 均不产生 |
| 并发 | 同应收两次并发核销至多一次成功；与钱包抵扣/充值并发时应收三元组与钱包投影保持一致，无死锁 |
| 崩溃/未知 | 提交后中断可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 账本守恒 | 新 journal 平衡、`RECEIVABLE_ASSET +A`/`ADJUSTMENT −A`、无 lot；既有 journal/posting 与其它表逐行不变 |
| 权限与 presenter | 自审、越权、撤权、窗口过期零副作用；presenter 只输出闭合字段并区分核销与现金回收 |
| N/N-1 与回滚 | 缺行/未知版本/翻回 `INACTIVE` 失败关闭；case/allocation/journal 保留可读 |
| 真实领域 E2E | Account 批准 → outbox → relay → 专属 durable → 应收/钱包/账本推进 → 终态 → 重放幂等，真实 PostgreSQL + 真实 HTTP，无 SKIP 替代 |

## 12. 设计冻结阶段的切片边界（历史）

以下范围与验证记录属于原设计文档切片，不代表当前私有实现的范围或验收结论；当前状态以本文开头及本轮审核记录为准。

- 不新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不改动既有应收创建/抵扣、拒付、退款阻塞、`attention_summary`、管理员只读接口或 GSA-21/22 的任何行为。
- 设计冻结不等于实现或激活；GSA-26 未交付前该命令保持不可执行。

## 13. 设计冻结阶段的自检与验证（历史）

- 每条事实可回溯到 §1/§2 的迁移与源码路径；§4 的写入集合与 §6 的不变量逐一对应既有 CHECK 与投影规则。
- 首版范围、部分核销与版本复用决策与用户 2026-09-19 确认一致，未猜测填入其它选项。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、允许/禁止变化、不变量与并发、幂等与未知结果、失败分类、迁移回滚与验收映射。
