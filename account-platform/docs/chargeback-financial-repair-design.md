# 拒付财务修复（`CHARGEBACK_FINANCIAL_REPAIR`）领域设计

状态：领域设计冻结；当前私有完整组合已实现 handler、closed proof、executor、durable consumer 与只读恢复，默认 activation 仍为 `INACTIVE`，未部署或激活。首版范围仅 **BASE 短缺补偿入账**，沿用 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`。本文后续章节保留原冻结业务契约；完整发行、审批跨域与生产激活验收仍须独立完成。

## 1. 设计阶段基线（历史）

- 原设计阶段只有策略元数据，Billing activation 为 `INACTIVE`，尚无本命令可执行路径；这是历史基线，不代表当前完整组合的实现状态。
- 当前 Account create/update/submit/APPROVE/Begin 执行五入口沿用实时 activation 与既有授权、冻结快照和租约守卫；只读预检可在 `INACTIVE` 下准备，不能授权财务变更。实际消费注册来自 `approvalconsumer.BillingCommands()` 的有限已安装集合。


## 2. 既有拒付链路与不变量（事实，本命令必须复用）

- **案件模型**：`chargeback_cases`（`payment_evidence_id` 唯一、`UNIQUE(payment_transaction_id,provider_chargeback_id)`、组合外键绑定交易/订单与 package/交易、`revoked_base_microcredits`/`revoked_bonus_microcredits>=0`、`receivable_id`、`resolution_code/reason`、`version`；状态经 `000029` 为 `REVIEW_REQUIRED/CONFIRMED/DISMISSED`）（`000025`、`000029`）。
- **确认路径**（`internal/billing/chargebacks.go`）：锁定证据/交易/package → 要求证据 `provider_status='CHARGEBACK_CONFIRMED'` 且交易 `PAID/LATE_PAID` → 写 `CHARGEBACK` journal（`business_reference='payment-transaction:<tx>:chargeback:<provider_chargeback_id>'`）→ 逐组件撤销 `available+active+suspended`（钱包负债账户负 posting 带 lot、`credit_lots` 减少、`entitlement_components.revoked_microcredits` 增加、成员配额 grant `REVOKED`）→ `baseShortfall = BASE.original − revocable` 时写 `RECEIVABLE_ASSET -baseShortfall` → `CHARGEBACK_CLEARING +(totalRevoked+baseShortfall)` → 钱包 `posted_microcredits-=totalRevoked` → `baseShortfall>0` 时创建 `source_type='CHARGEBACK'` 应收并同步钱包应收投影 → 案件置 `CONFIRMED` 并记录 revoked/receivable/`resolved_at`/`version`。
- **异常收口语义**：拒付异常为 `payment_anomaly_cases.anomaly_type='CHARGEBACK'`，合法收口为 `CONFIRM_CHARGEBACK`(`APPLIED`) 或 `DISMISS`(`NONE`)；`ESCALATE_FINANCIAL_REPAIR`(`PENDING`) 表示交由财务修复。与退款异常相同，模型没有通用的 `PENDING→APPLIED` 路径（详见 [退款异常修复设计](refund-exception-repair-design.md) §7.2）。
- **账户与枚举**：`platform:receivables`（`RECEIVABLE_ASSET`）、`platform:chargeback-clearing`（`CHARGEBACK_CLEARING`）启动时校验；`journal_entries.entry_type` 已含 `CHARGEBACK`；`receivables.source_type` 已含 `CHARGEBACK`（既有白名单，无需新迁移）。

## 3. 与相邻流程的职责边界（避免重复建设）

- **既有确认路径拥有“撤销仍可用额度 + 记录拒付”的完整语义**；本命令**不撤销可用额度**、不改 BONUS、不写钱包可用额，只补记无法收回的 BASE 短缺。
- 若案件仍存在可撤销额度（任一组件 `available/active/suspended>0`），说明应按既有 `CONFIRM_CHARGEBACK` 收口，本命令必须失败关闭并指明该路径，而不是部分执行。
- 退款与强制退款（GSA-29）、应收核销（GSA-25）、权益组成错配（GSA-21）各有路径；本命令不借用退款规则（基线硬约束）。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**允许的领域变化**（单次执行恰好产生，同一 Billing Serializable 事务内）：

1. 一条 `journal_entries`：`entry_type='CHARGEBACK'`、`business_reference='chargeback-repair:'||<chargeback_case_id>`，`description` 为固定模板；postings 恰好两条：`RECEIVABLE_ASSET -B`（无 lot）与 `CHARGEBACK_CLEARING +B`（无 lot），`B>0` 且合计 0。
2. 一条应收：`source_type='CHARGEBACK'`、`source_reference='chargeback-repair:'||<case_id>`、`original=B`、`outstanding=B`，按既有 `createReceivableWithinTransaction` 语义同步钱包 `receivable_microcredits += B`、`financial_status='RECEIVABLE_OPEN'`、`financial_freeze_reason='CHARGEBACK'`、`version+1`。
3. `chargeback_cases` 单行推进（`WHERE id=$1 AND version=$2`）：`status='CONFIRMED'`、`revoked_base_microcredits=0`、`revoked_bonus_microcredits=0`、`receivable_id=<新应收>`、`resolution_code='CONFIRM_CHARGEBACK_SHORTFALL'`、`resolution_reason=<审批摘要>`、`resolved_at=now`、`version+1`。
4. 同一事务把该案件的 `CHARGEBACK` 异常推进为 `financial_action_status='APPLIED'`（`version+1`、`updated_at`，`status/resolution_code/resolved_at` 不变），复用 §7 的受控 CAS 语句。
5. 一条 `chargeback_financial_repair_cases` 不可变审计行（见 §7.1）。

**明确禁止**：

- 禁止撤销/清零任何 `available/active/suspended` 额度，禁止 BONUS 侧的任何变化，禁止写钱包负债账户 posting、禁止改变 `wallets.posted_microcredits`。
- 禁止修改/删除既有 journal/posting、既有 `chargeback_cases` 历史字段（`revoked_*` 非零即拒绝）、既有 `payment_evidence`、既有应收；禁止删除或重开已 `CONFIRMED/DISMISSED` 的案件。
- 禁止把 `B` 记为可用额度、退款、强制退款或 `WALLET_ADJUSTMENT` 效果；不得核销（GSA-25）或减免既有应收。
- 禁止 `B<=0`、一次执行多条 journal/多条应收、或对同一案件/证据重复补记。

## 5. 外部证据与判据（冻结）

| 证据 | 要求 |
|---|---|
| Provider 拒付确认 | `payment_evidence.evidence_type='CHARGEBACK'` 且 `provider_status='CHARGEBACK_CONFIRMED'`，其 `payment_order_id`/`payment_transaction_id` 与案件一致，`provider_refund_id`/`evidence_key` 与案件 `provider_chargeback_id` 一致 |
| 案件事实 | `chargeback_cases.status='REVIEW_REQUIRED'`、`revoked_base_microcredits=0`、`revoked_bonus_microcredits=0`、`receivable_id IS NULL`，`version` 参与 CAS |
| 交易事实 | 交易状态 `PAID`/`LATE_PAID`，package 与交易/订单绑定一致，钱包存在且 `status='ACTIVE'` |
| 短缺事实 | 遍历 package 全部组件：`revocable=available+active+suspended` 必须**全部为 0**；`B=sum(BASE.effective_original)` 且 `B>0`（`effective_original=original−reclassified_out`，与既有 `effectiveComponentOriginal` 一致） |
| 零事实 | 不存在 `'chargeback-repair:'` 前缀的 journal/应收/案件行；不存在同 package 的进行中强制退款案或退款冲销案 |
| 异常事实 | 存在 `anomaly_type='CHARGEBACK'` 的异常行，`financial_action_status ∈ {APPLIED, PENDING}` 且 `version` 参与 CAS |

判据全部由 Billing 从持久事实重算；proof 不接受浏览器提供的金额、组件、账户或 Provider 文本，执行期不调用 Provider。

## 6. closed command / snapshot / proof / presenter（冻结）

### 6.1 normalized command（`additionalProperties:false`，RFC 8785，整数为 JSON number）

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `CHARGEBACK_FINANCIAL_REPAIR` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `chargeback_case_id` | uuid | 目标拒付案件，必须 `REVIEW_REQUIRED` 且零撤销 |
| `payment_evidence_id` | uuid | 冻结的 `CHARGEBACK` 确认证据行 |
| `expected_case_version` | int64 | `>0`，等于冻结的案件 `version` |
| `reason_code` | string | 唯一取值 `BASE_SHORTFALL_COMPENSATION` |

### 6.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.chargeback_case_version` | 冻结案件 `version` |
| `critical_resource_versions.facts_digest` | 下述事实集合的 closed RFC8785 摘要 |
| `critical_resource_versions.policy_version` | 1 |
| `financial_snapshot.base_shortfall_microcredits` | `B`（预检重算值） |
| `financial_snapshot.wallet_id` / `.wallet_version` | 目标钱包与版本 |
| `financial_snapshot.package_id` / `.component_digest` | 目标 package 与全部组件（类型/有效原始量/可撤销量）摘要 |
| `financial_snapshot.provider_chargeback_id` | 外部幂等引用 |

`facts_digest` 覆盖：案件全字段（含 `version`/`status`/`revoked_*`/`receivable_id`/`resolution_*`）、证据行摘要（含 `payload_digest`/`provider_status`）、交易与订单标识、package/组件当前状态（含 `reclassified_out_microcredits`）、钱包 `status/posted_microcredits/receivable_microcredits/financial_status/version`、异常行（类型/`financial_action_status`/`version`）、以及零事实证据与 activation/policy/schema 版本。

### 6.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属身份，2 秒绝对期限、64 KiB 上限，按 §5 重读并重算 `B` 与"可撤销额度全为 0"的证明，返回闭合结果（`PROOF_VALID/PROOF_DENY`、案件、证据、`B`、钱包/包版本、`facts_digest`、policy/schema/activation 版本）。执行期与 proof 均禁止 Provider 调用。
- presenter（GSA-37）只显示案件号（脱敏）、订单/交易引用（脱敏）、Provider 拒付引用（脱敏）、package 有效原始量与可撤销量摘要、`B`、预计应收金额、异常状态与风险警示；不下发 Provider 原文、个人数据或内部 proof 字段。

## 7. 幂等、异常推进与未知结果

### 7.1 新增不可变审计行（GSA-32 前向迁移）

`chargeback_financial_repair_cases`（`billing_schema_owner` 持有，运行期仅最小 `INSERT/SELECT`，UPDATE/DELETE 由触发器与 ACL 双重拒绝）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`chargeback_case_id`（唯一）、`anomaly_case_id`、`wallet_id`、`package_id`、`payment_evidence_id`、`base_shortfall_microcredits`（`>0`）、`component_digest`、`facts_digest`、`journal_entry_id`（唯一）、`receivable_id`（唯一）、`state`、`created_at`。

### 7.2 异常 `PENDING→APPLIED` 推进

与 [退款异常修复设计](refund-exception-repair-design.md) §7.2 完全同形：允许 `financial_action_status` 为 `APPLIED`（幂等命中）或 `PENDING`（由本命令在同一事务内以 `WHERE ... AND financial_action_status='PENDING' AND version=$2` 的 CAS 推进为 `APPLIED`、`version+1`）；`NONE`/`FAILED`/`OPEN` 一律失败关闭。

### 7.3 幂等与未知结果

- 线性化点：`chargeback_financial_repair_cases.chargeback_case_id` 唯一 + 案件 `version` CAS + journal `business_reference` 唯一 + 应收 `UNIQUE(source_type,source_reference)`；同一案件至多一次补记。
- result-first：先按 `chargeback_case_id`/`approval_request_id`/`execution_key_digest` 只读读回 case、journal、应收与案件状态；命中即返回同一结果。
- 未知结果：提交结果不明时只读查证 case、journal、应收、案件 `CONFIRMED` 与异常 `APPLIED` 是否同时一致；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`，禁止重放、禁止假定零效果。
- 执行顺序：认领 execution → 校验 activation/policy/schema 与 `facts_digest` → 锁证据/交易/package/组件/钱包（沿用既有确认路径的锁顺序）→ 复读零事实与可撤销量为 0 → 写应收与钱包应收投影 → 写 journal → 推进案件与异常 → 写 case → 提交后复读确认。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、policy/schema/版本不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态 |
| 证据非 `CHARGEBACK_CONFIRMED`、交易非 `PAID/LATE_PAID`、案件已 `CONFIRMED/DISMISSED`、存在可撤销额度、`B<=0`、钱包非 ACTIVE、`version` 漂移 | `PROOF_DENY` | 失败关闭、零写入、终态，并提示走既有确认路径 |
| 已有同案件 case/journal/应收 | 幂等命中 | 读回既有结果，零新写入 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 有界重试；应用层最多 12 次尝试，第 12 次仍失败发布 DLQ 并按 PubAck/`ACK_PENDING`/DoubleAck 收口；禁止通用人工 replay |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证，不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用 `platform.billing.repairs.request|approve`、一名非申请人审批、24 小时有效期、5 分钟窗口，不新增权限与 policy/snapshot 版本。
- 依据：该命令 v1/s1 行从未是可执行合同（静态不可执行、四入口失败关闭），不存在由该版本产生并已执行的领域事实。
- 激活前只读守卫：该命令必须不存在未终态申请；守卫同时只读列出"`REVIEW_REQUIRED` 拒付且可撤销额度为 0 的存量案件"清单，存量在激活后逐笔走本命令或既有 `CONFIRM_CHARGEBACK`，不得在激活时批量改写。
- activation 行保持 `INACTIVE`；GSA-32 交付默认关闭的专属 consumer/durable 开关；生产激活只属于取得独立授权的 GSA-40。
- 放宽范围（含 BONUS 缺口、部分撤销、借用退款规则）必须新增 policy/schema 版本与独立设计。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `chargeback_financial_repair_cases`（当前为 `000061`）与异常 `PENDING→APPLIED` 的最小权限；不改 `000025/000029/000054/000060` 与既有案件列，不放宽任何枚举。
- N/N-1：先部署可读新表的 Billing，再部署 proof 端与执行器/消费者，最后业务页；既有拒付确认路径行为不变。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；case/应收/journal 与异常状态保留可读，禁止破坏性 down。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-32 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 正例（BASE 全部已消费） | `B=sum(BASE effective_original)`，journal 两腿平衡、应收一行、钱包应收投影与 `financial_status` 正确、`posted_microcredits` 逐行不变 |
| 负例（存在可撤销额度） | `PROOF_DENY` 且零写入，错误码指向既有 `CONFIRM_CHARGEBACK` 路径 |
| 负例（BONUS 缺口/退避） | BONUS 侧不发生任何变化；含 BONUS 缺口的输入被拒绝或按既有确认路径处理，二者不得混用 |
| 证据门禁 | 证据状态不符、交易非成功、案件终态、`version` 漂移、`B<=0` 全部失败关闭 |
| 异常状态边 | `APPLIED` 幂等命中；`PENDING` 由 CAS 推进；`NONE`/`FAILED`/`OPEN` 拒绝并各写负例 |
| 幂等与并发 | 同案件二次授权/重复投递只命中既有结果；与既有确认路径并发时唯一约束与 CAS 生效、无死锁 |
| 崩溃/未知 | 提交后中断可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 入口边界 | 不调用 Provider、不撤销额度、不产生可用额度（真实断言零外部请求） |
| 权限与 presenter | 自审/越权/撤权/窗口过期零副作用；presenter 脱敏且字段闭合 |
| N/N-1 与回滚 | 缺行/未知版本/翻回 `INACTIVE` 失败关闭；case/应收/journal 与异常状态保留可读 |
| 真实领域 E2E | Account 批准 → outbox → relay → 专属 durable → 应收/journal/案件与异常收口 → 终态 → 重放幂等，真实 PostgreSQL + 真实 HTTP，无 SKIP 替代 |

## 12. 本切片不做的事

- 不新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不改动既有拒付确认/驳回、强制退款、退款、应收抵扣或异常收口 helper 的既有行为。
- 设计冻结不等于实现或激活；GSA-32 未交付前该命令保持不可执行。

## 13. 自检与验证

- 每条事实可回溯到 §1/§2 的迁移与源码路径；`B` 的定义与既有确认路径的 `baseShortfall` 完全一致（含 `reclassified_out_microcredits`）。
- 首版范围与版本复用决策与用户 2026-09-19 确认一致；"存在可撤销额度即拒绝"这一边界防止与既有路径重叠执行。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、BASE 短缺补偿模型、异常推进、幂等与未知结果、失败分类、迁移回滚与验收映射。
