# 用量结算修复（`USAGE_SETTLEMENT_REPAIR`）领域设计

状态：`DESIGN_FROZEN / SELF_REVIEW_PASS / INDEPENDENT_REVIEW_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。首版范围由用户在 2026-09-19 确认为**仅“少结算/未结算”补记**，权限与命令版本复用现有 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`（守卫见 §9）。本文冻结 GSA-36 的 closed command/snapshot/proof、补结算账务、幂等与恢复、失败分类、迁移与回滚及验收映射；不新增可执行路径，activation 行保持 `INACTIVE`。命令基线见 [十四命令事实与兼容基线](admin-governance-security-command-baseline.md)（**绝不重发模型请求**），治理与激活约束见 [治理与安全重设计](admin-governance-security-redesign.md) §11 与 §14。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies`/`approvalpolicy` 登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`、请求/审批权限 `platform.billing.repairs.request|approve`、`FINANCIAL_FACT_CHANGE`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，四入口失败关闭；Billing 侧只有 `('USAGE_SETTLEMENT_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`），无 handler、proof、executor、consumer durable 或恢复分类。

## 2. 用量结算现状与不变量（事实）

- **预留与分配**：`usage_holds`（`usage_request_id` 唯一、`wallet_id`、`price_version_id`、`status ∈ {ACTIVE,COMMITTED,RELEASED,EXPIRED,REVIEW}`、`max_microcredits>0`、`actual_microcredits>=0`、`usage_snapshot JSONB`、`expires_at`、`terminal_at`）与 `usage_hold_allocations`（`component_id` 非空、`reserved_microcredits>0`、`settled/released>=0`、`CHECK settled+released<=reserved`；主键由 `000012` 的 `(hold_id,package_id)` 在 `000016` 改为 `(hold_id,component_id)`）（`000002`、`000012`、`000016`）。
- **结算落账**（`internal/billing/postgres.go` 的 `Commit`）：同 `usage_request_id` 且 `actual` 相同即幂等返回；`status='COMMITTED'` 之外的终态返回 `ErrUsageAlreadyTerminal`；`RELEASED && actual>0` 且 `usage_snapshot->>'release_reason'='ENTITLEMENT_EXPIRED_UNCERTAIN'` 时写 `usage_loss_reviews`（`reason_code`、`claimed_actual_microcredits`、`usage_snapshot`、`status='REVIEW_REQUIRED'`）并返回 `ErrUsagePlatformLossReview`。ACTIVE 路径：`backedActual=min(actual,max)`、`overage=actual-backedActual`；写 `USAGE` journal（`business_reference='usage:'||usage_request_id`）；按冻结分配顺序（组件过期时间、BONUS 优先、入账时间、ID）扣减 lot 与组件预留/已用、回填 `usage_hold_allocations`、投影 package 状态；`overage>0` 写 `RECEIVABLE_ASSET -overage` 并创建 `source_type='USAGE_OVERAGE'` 且 `source_reference=usage_request_id` 的应收；`USAGE_REVENUE +actual`；钱包 `posted_microcredits-=backedActual`、`reserved_microcredits-=max`、`version+1`；hold 置 `COMMITTED` 并记录 `actual`/`usage_snapshot`。
- **损失复核模型**：`usage_loss_reviews`（`hold_id` 唯一、`reason_code` 仅 `ENTITLEMENT_EXPIRED_UNCERTAIN`、`claimed_actual_microcredits`、`usage_snapshot`、`status ∈ {REVIEW_REQUIRED,RESOLVED_PLATFORM_LOSS}`、`updated_at`）——既有终态只表达“平台吸收损失”，没有“已补结算”收口（`000016`）。
- **不变量**：journal/posting 不可改写且必须平衡、`business_reference` 全局唯一；钱包 `posted_microcredits>=0`、lot 上下界、组件守恒；`usage_hold_allocations` 的 `settled+released<=reserved`。

## 3. 与相邻流程的职责边界（避免重复建设）

- 正常预留在 `ACTIVE` 时由既有 `Commit` 结算；本命令只处理 `Commit` 已无法覆盖的两类**既有事实**：①hold 已 `RELEASED`（`ENTITLEMENT_EXPIRED_UNCERTAIN`）但存在已发生用量 → `usage_loss_reviews` `REVIEW_REQUIRED`（未结算）；②hold 已 `COMMITTED` 但存在冻结的修正用量证据表明少结算。
- **绝不重发模型请求**：不调用任何 Provider/adapter、不重放推理请求、不新增 usage request；只读既有持久用量与定价事实。
- 组成/归属错配属 GSA-21；支付、退款、拒付、应收核销分别属 GSA-27/29/31/25；本命令只处理消费侧结算差异。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**允许的领域变化**（单次执行恰好产生，同一 Billing Serializable 事务内）：

1. 一条 `USAGE` journal：`business_reference='usage-settlement-repair:'||usage_request_id`，postings = 逐 lot 钱包负债账户 `-settled`（带 lot）、`USAGE_REVENUE +settled`、`overage>0` 时 `RECEIVABLE_ASSET -overage`；`settled+overage=M` 且 journal 合计 0。
2. 按冻结分配顺序在当前可用额度内扣减：`credit_lots.remaining_microcredits`、`entitlement_components`（`available`/`used`/`status` 按既有结算语义推进）、成员配额按既有 `settleMemberQuotaComponent` 语义。
3. 钱包 `posted_microcredits-=settled`、`version+1`、`updated_at`；`overage>0` 时按既有 `createReceivableWithinTransaction` 语义创建 `source_type='USAGE_OVERAGE'`、`source_reference='usage-settlement-repair:'||usage_request_id` 的应收并同步钱包应收投影。
4. `usage_hold_allocations` 的 `settled_microcredits`/`released_microcredits` 与 `updated_at` 按既有语义回填；`usage_holds` 追加 `usage_snapshot->'settlement_repair'` 子对象（含审批引用、`M`、`settled`、`overage`、前后 `actual`），`actual_microcredits` 更新为 `max(原值, 应结额)`，`terminal_at`/`updated_at` 更新；不得改写 `usage_snapshot` 既有键。
5. case ① 的 `usage_loss_reviews` 单行推进（`WHERE status='REVIEW_REQUIRED' AND updated_at=$frozen`）：`status='RESOLVED_SETTLED'`、`updated_at=now`；case ② 无该行。
6. 一条 `usage_settlement_repair_cases` 不可变审计行（见 §7.1）。

**明确禁止**：

- 禁止重发/重放模型请求、禁止调用任何外部 adapter、禁止新增 `usage_request_id`、禁止改写既有 `USAGE` journal。
- 禁止处理“多结算/重复结算/冲销”差异（需新增版本与独立设计）；禁止修改定价快照、`price_version_id` 或用量事实。
- 禁止超过 `usage_holds.max_microcredits` 的“超额结算”（`overage` 只能形成应收，不能变成可用额度）；禁止把 `M<=0` 的执行当作有效修复。
- 禁止改写 `wallets.reserved_microcredits`（预留已在释放时清零）、既有 `credit_lots.original`、既有组件 `original/reclassified_out`、既有应收的 `settled/outstanding`。
- 禁止一次执行产生多条 journal、多条应收或对同一 hold 重复补结算。

## 5. 事实证据与判据（冻结）

| 证据 | 要求 |
|---|---|
| 应结额 `A` | 由冻结的 `usage_snapshot`（含定价/公式快照与 token 事实）按与既有结算同一整数算法重算；快照缺失、字段不全或与 `price_version_id` 不一致即 `PROOF_DENY` |
| 已结算额 `E` | case ② 取 `usage_holds.actual_microcredits`（`COMMITTED`）；case ① 取 `0`，并要求 hold 为 `RELEASED` 且 `usage_snapshot->>'release_reason'='ENTITLEMENT_EXPIRED_UNCERTAIN'` |
| 差额 `M` | `M=A-E`，必须 `>0`；若已有 `'usage-settlement-repair:'` 前缀的 journal/应收或既有 `usage_request_id` 应收，其未结部分计入 E，不得二次创建 |
| 修正用量证据（case ②） | 必须是与该 `usage_request_id` 绑定的持久核对事实（Token 用量记录/损失复核快照），不得是人工输入的数字 |
| 损失复核（case ①） | `usage_loss_reviews.status='REVIEW_REQUIRED'`，`claimed_actual_microcredits>=0`，其 `usage_snapshot` 与 hold 快照一致 |
| 上限与余额 | `settled<=可用组件额度`、`overage=M-settled>=0`；钱包 `posted_microcredits>=settled`、`status='ACTIVE'` |

判据全部由 Billing 从持久事实重算；proof 不接受浏览器提供的用量、金额、组件或定价，执行期与 proof 均禁止任何外部调用。

## 6. closed command / snapshot / proof / presenter（冻结）

### 6.1 normalized command（`additionalProperties:false`，RFC 8785，整数为 JSON number）

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `USAGE_SETTLEMENT_REPAIR` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `usage_request_id` | uuid | 目标用量请求，必须 `usage_holds.usage_request_id` 命中 |
| `hold_id` | uuid | 必须与 `usage_request_id` 一致 |
| `evidence_kind` | string | `LOSS_REVIEW`（case ①）或 `COMMITTED_SHORTFALL`（case ②） |
| `loss_review_id` | uuid（可空） | 仅 `LOSS_REVIEW` 必填 |
| `reason_code` | string | 唯一取值 `MISSING_USAGE_SETTLEMENT` |

### 6.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.hold_updated_at` / `.loss_review_updated_at` | 冻结时间戳（损失复核可空） |
| `critical_resource_versions.facts_digest` | 下述事实集合的 closed RFC8785 摘要 |
| `critical_resource_versions.policy_version` | 1 |
| `financial_snapshot.expected_microcredits` / `.settled_microcredits` | `A` 与 `E` |
| `financial_snapshot.settlement_microcredits` / `.overage_microcredits` | `settled` 与 `overage` |
| `financial_snapshot.wallet_id` / `.wallet_version` | 目标钱包与版本 |
| `financial_snapshot.allocations_digest` | 冻结的分配集合（package/component/lot/预留/已结算）摘要 |

`facts_digest` 覆盖：hold 全字段（含 `usage_snapshot` 的关键子键摘要）、全部 `usage_hold_allocations`、相关组件与 lot 的当前额度/状态、钱包 `posted/reserved/version`、既有应收（含 `usage_request_id` 与 `'usage-settlement-repair:'` 引用）、损失复核行、既有 `USAGE` journal 引用、以及 activation/policy/schema 版本。

### 6.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属身份，2 秒绝对期限、64 KiB 上限，按 §5 重读并重算 `A/E/M/settled/overage`，返回闭合结果（`PROOF_VALID/PROOF_DENY`、`usage_request_id`、`hold_id`、`M`、`settled`、`overage`、钱包/包版本、`facts_digest`、policy/schema/activation 版本）。执行期与 proof 均禁止任何外部调用。
- presenter（GSA-37）只显示用量请求摘要（脱敏）、模型/渠道（脱敏）、冻结定价版本与用量数量、`A/E/M` 与拆分、预计应收金额、证据类型（损失复核/少结算）、风险警示；不下发原始请求体、提示词、Provider 原文或个人数据。

## 7. 幂等、状态推进与未知结果

### 7.1 新增不可变审计行（GSA-36 前向迁移）

`usage_settlement_repair_cases`（`billing_schema_owner` 持有，运行期仅最小 `INSERT/SELECT`，UPDATE/DELETE 由触发器与 ACL 双重拒绝）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`usage_request_id`（唯一）、`hold_id`、`loss_review_id`（可空、唯一）、`wallet_id`、`evidence_kind`、`expected_microcredits`、`settled_microcredits`、`overage_microcredits`（`>=0`）、`allocations_digest`、`facts_digest`、`journal_entry_id`（唯一）、`receivable_id`（可空、唯一）、`state`、`created_at`。

### 7.2 损失复核状态推进（本命令引入的唯一新状态边）

- `usage_loss_reviews.status` 现有取值只有 `REVIEW_REQUIRED`/`RESOLVED_PLATFORM_LOSS`；GSA-36 的前向迁移必须放宽 CHECK 以加入 `RESOLVED_SETTLED`（guard 校验迁移身份、schema owner 分离与既有取值不变），并以 `WHERE id=$1 AND status='REVIEW_REQUIRED' AND updated_at=$2` 的 CAS 推进（`updated_at=now`）。
- 既有 `RESOLVED_PLATFORM_LOSS` 语义（平台吸收损失）与既有写入者保持不变；本命令不改已吸收行。

### 7.3 幂等与未知结果

- 线性化点：`usage_settlement_repair_cases.usage_request_id` 唯一 + hold/loss-review 行锁与时间戳 CAS + journal `business_reference` 唯一 + 应收 `UNIQUE(source_type,source_reference)`；同一用量请求至多一次补结算。
- result-first：先按 `usage_request_id`/`hold_id`/`approval_request_id`/`execution_key_digest` 只读读回 case、journal、应收与损失复核状态；命中即返回同一结果。
- 未知结果：提交结果不明时只读查证 case、journal、应收、`usage_hold_allocations` 回填与损失复核状态是否同时一致；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`，禁止重放、禁止假定零效果。
- 执行顺序：认领 execution → 校验 activation/policy/schema 与 `facts_digest` → 锁 hold/allocations/组件/lot/钱包 → 复读定价与用量证据 → 计算 `M` 与拆分 → 写 journal 与 posting → 扣减 lot/组件/配额 → 钱包与 package 投影 → 应收（overage）→ 回填 allocations 与 hold 快照 → 推进损失复核 → 写 case → 提交后复读校验。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、policy/schema/版本不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态 |
| 定价/用量快照缺失或不一致、`M<=0`、钱包非 ACTIVE、余额不足、hold 状态与 `evidence_kind` 不符、损失复核非 `REVIEW_REQUIRED`、时间戳/时间漂移、已有修复引用 | `PROOF_DENY` | 失败关闭、零写入、终态 |
| 已有同 `usage_request_id` 的 case/journal | 幂等命中 | 读回既有结果，零新写入 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 有界重试；应用层最多 12 次尝试，第 12 次仍失败发布 DLQ 并按 PubAck/`ACK_PENDING`/DoubleAck 收口；禁止通用人工 replay |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证，不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用 `platform.billing.repairs.request|approve`、一名非申请人审批、24 小时有效期、5 分钟窗口，不新增权限与 policy/snapshot 版本。
- 依据：该命令 v1/s1 行从未是可执行合同（静态不可执行、四入口失败关闭），不存在由该版本产生并已执行的领域事实。
- 激活前只读守卫：该命令必须不存在未终态申请；守卫同时只读列出目标库中 `usage_loss_reviews.status='REVIEW_REQUIRED'` 的存量与 `COMMITTED` hold 的少结算候选统计，存量在激活后逐笔走本命令或既有吸收口径，不得在激活时批量改写。
- activation 行保持 `INACTIVE`；GSA-36 交付默认关闭的专属 consumer/durable 开关；生产激活只属于取得独立授权的 GSA-40。
- 若将来处理“多结算/重复结算冲销”或引入新的用量证据来源，必须新增 policy/schema 版本与独立设计。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `usage_settlement_repair_cases` 与 `usage_loss_reviews.status` 的 `RESOLVED_SETTLED` 取值（当前为 `000061`），guard 校验迁移身份与 schema owner 分离；不改 `000002/000012/000016/000054/000060` 既有列、CHECK 语义与历史行。
- N/N-1：先部署可读新表/新取值的 Billing，再部署 proof 端与执行器/消费者，最后业务页；既有 `Commit` 与损失复核写入者行为不变（不认识新取值时仍按既有两值读取，失败关闭而非误判）。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；已产生的 case/journal/应收/回填与损失复核状态一律保留可读，禁止破坏性 down。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-36 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 未结算补记（case ①） | 由 `usage_loss_reviews` + `RELEASED(ENTITLEMENT_EXPIRED_UNCERTAIN)` hold 重算 `A`，写 `USAGE` journal、扣减与回填、损失复核推进 `RESOLVED_SETTLED` |
| 少结算补记（case ②） | `COMMITTED` hold 且存在冻结修正用量证据时只补差额 `M`，不重复结算已有 `E` |
| 超上限与余额不足 | `overage=M-settled>0` 时形成 `USAGE_OVERAGE` 应收而非可用额度；钱包余额不足时按既有语义失败关闭（不得透支） |
| 证据门禁 | 定价/用量快照缺失、`price_version_id` 不符、人工输入数字、`M<=0`、hold 状态不符、损失复核非 `REVIEW_REQUIRED` 全部 `PROOF_DENY` |
| 幂等与并发 | 同一 `usage_request_id` 二次授权/重复投递只命中既有 case；与 Token 用量对账/既有 `Commit` 并发时唯一约束与 CAS 生效、无死锁 |
| 崩溃/未知 | 提交后中断可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 账本守恒 | journal 平衡、`settled+overage=M`、组件守恒式成立、lot 上下界成立、既有 `USAGE` journal 逐字节不变 |
| 入口边界 | 不重发模型请求、不调用 adapter、不新增 usage request（真实断言零外部请求） |
| 权限与 presenter | 自审/越权/撤权/窗口过期零副作用；presenter 脱敏且不下发提示词/原始请求体 |
| N/N-1 与回滚 | 旧读取端遇到 `RESOLVED_SETTLED` 失败关闭而非误判；翻回 `INACTIVE` 后新动作失败关闭、历史保留 |
| 真实领域 E2E | Account 批准 → outbox → relay → 专属 durable → 补结算/应收/损失复核收口 → 终态 → 重放幂等，真实 PostgreSQL + 真实 HTTP，无 SKIP 替代 |

## 12. 本切片不做的事

- 不新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不改动既有预留/释放/结算、Token 用量对账、`usage_loss_reviews` 既有吸收口径或支付/退款/拒付/应收的任何行为。
- 设计冻结不等于实现或激活；GSA-36 未交付前该命令保持不可执行。

## 13. 自检与验证

- 每条事实可回溯到 §1/§2 的迁移与源码路径；`A/E/M` 的定义与既有 `Commit` 的 `backedActual/overage` 口径一致。
- 首版范围与版本复用决策与用户 2026-09-19 确认一致；`RESOLVED_SETTLED` 是显式登记并前向新增的取值，不假装既有能力已具备。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、补结算账务与守恒、损失复核新状态边、幂等与未知结果、失败分类、迁移回滚与验收映射。
