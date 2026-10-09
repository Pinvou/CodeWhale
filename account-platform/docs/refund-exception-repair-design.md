# 退款异常修复（`REFUND_EXCEPTION_REPAIR`）领域设计

当前实现：`REFUND_EXCEPTION_REPAIR v1/s1` 已接入 owner、proof、预检、审批详情/编辑、consumer 和专属表单，默认 activation 仍为 `INACTIVE`；不开放普通退款产品，不调用 Provider。本文件保留首版已冻结的财务契约。F12 已审修复已通过普通 merge 相邻传播；本轮有限验证不代表生产激活或完整 E2E 验收。

历史设计基线：2026-09-19 用户确认仅“Provider 已退款但本地未冲销”。下文明确标记的历史章节记录设计阶段，不能当作当前缺少实现的说明。

## 1. 命令现状（历史设计基线）

- **只有策略元数据**：Account `approval_command_policies`/`approvalpolicy` 登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求/审批权限 `platform.billing.repairs.request|approve`、`FINANCIAL_FACT_CHANGE`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，create/update/submit/APPROVE/Begin 全部失败关闭；Billing 侧只有 `('REFUND_EXCEPTION_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`），无 handler、proof、executor、consumer durable 或恢复分类。
- **产品约束**：本命令不得开放普通退款产品入口（[命令基线](admin-governance-security-command-baseline.md) §3.2 与[治理设计](admin-governance-security-redesign.md) §11）。

## 2. 既有退款链路与异常事实（事实，本命令必须复用）

- **退款模型**：`refunds`（`payment_order_id`、`merchant_refund_no` 唯一、`channel_refund_id` 唯一、`amount_fen>0`、`credit_microcredits>0`、`status ∈ {REQUESTED,SUCCEEDED,FAILED,MANUAL_REVIEW}`、`idempotency_key` 唯一、`reason`、`failure_reason`、`updated_at`），另有 `refund_component_reservations`/`refund_grant_reservations` 冻结退款前的组件与配额分配（`000004`、`000024`）。
- **成功路径**（`internal/billing/refunds.go`）：冻结预留 → Provider 退款 → 事务内写 `REFUND` journal（`business_reference='refund:'||refund_id`）、按预留逐组件写钱包负债账户负 posting 与 `credit_lots.remaining_microcredits` 减少、组件（`status='FROZEN'` 且 used/reserved/expired 均为 0）置 `REVOKED`、成员配额 grant 置 `REVOKED` 并写审计/outbox、`REFUND_CLEARING +total`、钱包 `posted_microcredits-=total`、entitlement package 置 `REVOKED`、退款置 `SUCCEEDED` 并发出 `billing.refund.succeeded.v1`。
- **本地冲突分支（本命令的触发事实）**：当预留组件缺失、合计与 `credit_microcredits` 不等或 `wallet.PostedMicrocredits<total` 时，同一事务把退款置 `MANUAL_REVIEW`、记录 `channel_refund_id`、发出 `billing.refund.review-required.v1`，并同事务追加由原 Provider 已验证 `SUCCESS` 结果产生的不可变 `QUERY` payment evidence，再创建 P0 `REFUND_FAILURE` 异常（`dedupe_key='refund:<refund_id>:local-conflict'`）——即"Provider 已退款、本地未冲销"的持久事实。
- **异常收口语义**：`payment_anomaly_cases`（`status ∈ {OPEN,RESOLVED}`、`resolution_code`、`financial_action_status ∈ {NONE,PENDING,APPLIED,FAILED}`、`version`）与 `ResolvePaymentAnomaly` 只允许 `REFUND_FAILURE` 的 `DISMISS`(`NONE`) 或 `ESCALATE_FINANCIAL_REPAIR`(`PENDING`)；**历史设计基线没有从 `PENDING` 推进到 `APPLIED` 的受控路径**，也没有任何命令会写 `APPROVED_WRITE_OFF` 之外的财务收口。
- **可用账户与枚举**：`platform:refund-clearing`（`REFUND_CLEARING`）、`platform:receivables`（`RECEIVABLE_ASSET`）、`platform:adjustment`（`ADJUSTMENT`）在启动时校验；`journal_entries.entry_type` 已含 `REFUND`/`REVERSAL`/`FORCED_REFUND`；`receivables.source_type` 经前向 `000060`（提交 `d253eeb0a`）已含 `FORCED_REFUND`。

## 3. 与相邻流程的职责边界（避免重复建设）

- 普通退款成功/失败/评审仍由 `refunds.go` 既有链路拥有；本命令不改其冻结预留、不动 `refund_component_reservations`/`refund_grant_reservations` 的历史行，也不重发 Provider 退款。
- 拒付触发的退款冲销属 GSA-31；支付未入账补记属 GSA-27；应收核销属 GSA-25；组成错配属 GSA-21。
- 本命令**不开放任何退款入口**：不创建退款单、不调用 Provider、不改变退款金额或可退基数。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**允许的领域变化**（单次执行恰好产生，同一 Billing Serializable 事务内）：

1. 对每个冻结预留组件：按成功路径的同一 SQL 语义冲销仍可冻结回收的部分（`credit_lots.remaining_microcredits` 减少、组件 `REVOKED`、成员配额 grant `REVOKED` 并写既有审计/outbox），记 `revoked`。
2. 一条 `REFUND` journal：`business_reference='refund-repair:'||refund_id`，postings = 逐 lot 钱包负债账户 `-amount`（带 lot）、`REFUND_CLEARING +total`、以及在 `shortfall>0` 时 `RECEIVABLE_ASSET -shortfall`（无 lot）；`total=revoked+shortfall` 必须等于 `refunds.credit_microcredits`，journal 合计 0。
3. `shortfall = refund.credit_microcredits - revoked`：对仍无法回收的部分创建应收（`source_type='FORCED_REFUND'`、`source_reference='refund-repair:'||refund_id`、`original=shortfall`），按既有 `createReceivableWithinTransaction` 语义同步钱包 `receivable_microcredits/financial_status/financial_freeze_reason`。
4. 钱包 `posted_microcredits-=revoked`、`version+1`、`updated_at`；`entitlement_packages` 按冲销结果推进（全部收回则 `REVOKED`）。
5. `refunds` 推进为 `SUCCEEDED`、`intent_version+1`、`updated_at`（`channel_refund_id` 不变），并发出 `billing.refund.succeeded.v1`（字段与既有成功路径一致）。
6. 同一事务把触发本命令的 `REFUND_FAILURE` 异常由 `PENDING` 推进为 `financial_action_status='APPLIED'`、`version+1`、`updated_at`（`status` 保持 `RESOLVED`、`resolution_code` 保持 `ESCALATE_FINANCIAL_REPAIR`）；这是本命令必须显式实现的唯一新增状态推进，见 §7。
7. 一条 `refund_exception_repair_cases` 不可变审计行（见 §7.1）。

**明确禁止**：

- 禁止修改/删除既有 journal/posting、`refund_component_reservations`/`refund_grant_reservations` 和 `payment_evidence`；禁止删除既有退款或改写其金额、流水与来源事实。退款和异常仅允许 §4.5/§4.6 明确列出的状态、版本与时间推进。
- 禁止在 `channel_refund_id` 为空或缺少 Provider 退款成功证据时执行；禁止执行期调用 Provider。
- 禁止把 `shortfall` 记为钱包可用额度、二次发放或 `WALLET_ADJUSTMENT`；只能形成应收。
- 禁止 `revoked+shortfall<>credit_microcredits`、`revoked<0`、`shortfall<0` 的任何形态；禁止一次执行产生多条 journal 或多条应收。
- 禁止借本命令开放退款入口、改变退款金额/可退基数或核销 GSA-25 的应收。

## 5. 外部证据与判据（冻结）

| 证据 | 要求 |
|---|---|
| Provider 退款成功 | `refunds.channel_refund_id` 非空，且存在 `payment_evidence` 行满足 `provider_refund_id=refunds.channel_refund_id`、`payment_order_id`/`payment_transaction_id` 与退款行一致、`amount_fen`/`currency` 与订单一致、`provider_status` 非失败 |
| 退款单事实 | `refunds.status='MANUAL_REVIEW'`、`credit_microcredits>0`、组件/配额预留行仍完整存在且合计等于 `credit_microcredits` |
| 异常事实 | 存在 `payment_anomaly_cases`：`anomaly_type='REFUND_FAILURE'`、`dedupe_key='refund:<refund_id>:local-conflict'`、`financial_action_status='PENDING'`，其 `version` 参与 CAS |
| 本地未冲销 | 不存在 `business_reference='refund-repair:'||refund_id` 的 journal、不存在 `source_reference='refund-repair:'||refund_id` 的应收、`refund_exception_repair_cases` 无该退款行 |
| 无冲突事实 | 同一 package 不存在强制退款案（`forced_refund_cases`）或拒付案（`chargeback_cases`）；现有判据不按已结案状态放行 |

判据全部由 Billing 从持久事实重算；proof 不接受浏览器提供的金额、`revoked/shortfall` 拆分、账户或 Provider 文本。

## 6. closed command / snapshot / proof / presenter（冻结）

### 6.1 normalized command（`additionalProperties:false`，RFC 8785，整数为 JSON number）

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `REFUND_EXCEPTION_REPAIR` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `refund_id` | uuid | 目标退款，必须 `MANUAL_REVIEW` 且 `channel_refund_id` 非空 |
| `payment_evidence_id` | uuid | 冻结的 Provider 退款成功证据行 |
| `anomaly_case_id` | uuid | 冻结的 `REFUND_FAILURE` 异常行 |
| `reason_code` | string | 唯一取值 `PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING` |

### 6.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.refund_updated_at` / `.anomaly_version` | 冻结退款 `updated_at` 与异常 `version` |
| `critical_resource_versions.facts_digest` | 下述 owner 事实结构经固定 Go JSON 编码后的 SHA-256；与审批 wire 的 RFC8785 摘要分开 |
| `financial_snapshot.amount_fen` / `.credit_microcredits` | 退款冻结金额与额度 |
| `financial_snapshot.provider_refund_id` | 外部幂等引用（`channel_refund_id`） |
| `financial_snapshot.revoked_microcredits` / `.shortfall_microcredits` | 预检计算的回收额与短缺额（`revoked+shortfall=credit_microcredits`） |
| `financial_snapshot.wallet_id` / `.wallet_version` / `.package_id` | 目标钱包、钱包版本与权益包 |

`facts_digest` 必须非空并与执行期重算值完全一致；不提供跳过比较的旧开发契约。它覆盖：退款行全字段（含 `intent_version`）、预留组件与配额行（ID/类型/lot/冻结值/当前值）、钱包 `posted_microcredits/version`、package 与组件当前状态、Provider 证据摘要（`payload_digest`）、异常行（类型/状态/`financial_action_status`/`version`）、以及"无 journal/无应收/无 case"的零事实与 activation/policy/schema 版本。

### 6.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属身份，2 秒绝对期限、64 KiB 上限，按 §5 重读并重算 `revoked/shortfall`，返回闭合结果（`PROOF_VALID/PROOF_DENY`、退款、证据、异常、`revoked`、`shortfall`、钱包/包版本、`facts_digest`、policy/schema/activation 版本）。执行期与 proof 均禁止 Provider 调用。
- 当前 typed presenter 仅从严格校验的冻结 proof artifact 提取退款、证据、异常、钱包与包 ID、十进制字符串金额/额度/回收/短缺、facts_digest 和固定 reason_code，供原始意图 EDIT 恢复；不从实时事实另选证据，不下发 Provider 原文、支付凭证或个人数据。

## 7. 执行、幂等与状态推进

### 7.1 新增不可变审计行（GSA-30 前向迁移）

`refund_exception_repair_cases`（`billing_schema_owner` 持有，运行期仅最小 `INSERT/SELECT`，UPDATE/DELETE 由触发器与 ACL 双重拒绝）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`refund_id`（唯一）、`anomaly_case_id`、`wallet_id`、`package_id`、`revoked_microcredits`（`>=0`）、`shortfall_microcredits`（`>=0`）、`payment_evidence_id`、`facts_digest`、`journal_entry_id`（唯一）、`receivable_id`（可空、唯一）、`state`、`created_at`，并检查 `revoked_microcredits+shortfall_microcredits>0`。

### 7.2 异常状态推进（本命令引入的唯一新状态边）

- 既有 `ResolvePaymentAnomaly` 只允许 `REFUND_FAILURE` 的 `DISMISS(NONE)` 或 `ESCALATE_FINANCIAL_REPAIR(PENDING)`，历史设计时没有 `PENDING→APPLIED` 的路径。GSA-30 引入一个受控、可测试的推进（同一事务内 `UPDATE payment_anomaly_cases SET financial_action_status='APPLIED',version=version+1,updated_at=$now WHERE id=$1 AND status='RESOLVED' AND resolution_code='ESCALATE_FINANCIAL_REPAIR' AND financial_action_status='PENDING' AND version=$2`），并保持 `status/resolution_code/resolved_at` 不变。
- 该推进必须幂等：已为 `APPLIED` 且存在同 `refund_id` 的 case 时按 result-first 读回，不重复推进、不再写账。
- 若异常仍是 `OPEN`/`NONE`（未升级）或 `version` 漂移，整笔失败关闭，禁止自行升级或替审批人决定。

### 7.3 幂等与未知结果

- 线性化点：`refund_exception_repair_cases.refund_id` 唯一 + `refunds` 行的 `status` 校验 + 异常 `version` CAS + journal `business_reference` 唯一；同一退款至多一次补记。
- result-first：先按 `refund_id`/`approval_request_id`/`execution_key_digest` 只读读回 case、journal、应收与异常状态；命中即返回同一结果。
- 未知结果：提交结果不明时只读查证 case、journal、应收、退款状态与异常 `financial_action_status` 是否同时一致；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`，禁止重放、禁止假定零效果。
- 执行顺序：认领 execution → 校验 activation/policy/schema 与 `facts_digest` → 锁退款行与预留行 → 复读证据与零事实 → 回收可用额度 → 写应收（短缺）→ 写 journal → 推进退款与异常 → 写 case → 提交后复读确认。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、policy/schema/版本不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态 |
| 缺 `channel_refund_id`、缺 Provider 退款证据、证据与退款不一致、退款非 `MANUAL_REVIEW`、异常未升级或 `version` 漂移、存在同包拒付/强制退款案 | `PROOF_DENY` | 失败关闭、零写入、终态 |
| 已有同退款 case/journal/应收 | 幂等命中 | 读回既有结果，零新写入 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 沿用现有 consumer 的 proof-era 单次认领与恢复分类；需要恢复时只读查询 owner 收据，禁止未知结果重新执行。历史通用最多 12 次重试不适用于本命令的财务认领 |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证，不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用 `platform.billing.repairs.request|approve`、一名非申请人审批、24 小时有效期、5 分钟窗口，不新增权限与 policy/snapshot 版本；不开放任何退款入口。
- 依据：该命令的 v1/s1 行从未是可执行合同（静态不可执行、四入口失败关闭），不存在由该版本产生并已执行的领域事实；首次冻结的 closed 形状即该版本合同。
- 激活前只读守卫：目标库中该命令必须不存在未终态申请（`APPROVED/EXECUTING/EXECUTION_FAILED`）；守卫同时只读列出"`MANUAL_REVIEW` 退款 + `REFUND_FAILURE` 异常 PENDING"的存量清单，存量必须在激活后逐笔走本命令收口，不得在激活时批量改写。
- activation 行保持 `INACTIVE`；GSA-30 交付默认 INACTIVE 的专属 consumer/durable 资源绑定；生产激活只属于取得独立授权的 GSA-40。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `refund_exception_repair_cases`（Billing `000067`，proof allowlist 为 `000068`，Account consumer route 为 `000074`）与异常 `PENDING→APPLIED` 的受控写入所需的最小权限；不改 `000004/000024/000025/000029/000054/000060` 的既有列、CHECK 与历史行。
- N/N-1：先部署可读新表的 Billing，再部署 proof 端与执行器/消费者，最后业务页；旧客户端与既有退款链路行为不变。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；已产生的 case/应收/journal 与异常 `APPLIED` 状态一律保留可读，禁止破坏性 down。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-30 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 全额回收 | `shortfall=0`、全部预留组件/配额被 revoke、journal 两腿（钱包各 lot + `REFUND_CLEARING`）平衡、退款 `SUCCEEDED`、异常 `APPLIED` |
| 部分回收（存在消费/过期） | `shortfall>0` 时应收一行 `FORCED_REFUND` 且金额等于 `credit_microcredits-revoked`，钱包应收投影与 `financial_status` 正确，journal 含 `RECEIVABLE_ASSET` 腿 |
| 证据门禁 | 缺 `channel_refund_id`、无 Provider 证据、证据金额/币种不符、退款非 `MANUAL_REVIEW`、异常非 PENDING `ESCALATE` 全部 `PROOF_DENY` 且零写入 |
| 异常状态边 | 只有 `version` CAS 成功才推进 `APPLIED`；并为重复推进、错误前置状态、`version` 漂移各写负例 |
| 幂等与并发 | 同一退款二次授权/重复投递只命中既有 case；与钱包消费/充值并发不破坏不平衡约束、无死锁 |
| 崩溃/未知 | 提交后中断可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 入口边界 | 命令不创建退款单、不调用 Provider、不放宽普通退款入口（真实断言零外部请求、零新退款行） |
| 权限与 presenter | 自审/越权/撤权/窗口过期零副作用；presenter 脱敏且字段闭合 |
| N/N-1 与回滚 | 缺行/未知版本/翻回 `INACTIVE` 失败关闭；case/应收/journal 与异常状态保留可读 |
| 真实领域 E2E | Account 批准 → outbox → relay → 专属 durable → 回收/应收/journal/异常收口 → 终态 → 重放幂等，真实 PostgreSQL + 真实 HTTP，无 SKIP 替代 |

## 12. 设计冻结阶段范围（历史）

以下仅记录原文档切片范围，不代表当前功能实现或验收。

- 不新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不改动既有退款成功/失败/评审链路、强制退款、拒付、应收抵扣或异常收口 helper 的既有行为。
- 设计冻结不等于实现或激活；GSA-30 未交付前该命令保持不可执行。

## 13. 设计冻结阶段验证（历史）

- 每条事实可回溯到 §1/§2 的迁移与源码路径；`revoked/shortfall` 拆分对应既有两个写入者的实际账户方向。
- 首版范围与版本复用决策与用户 2026-09-19 确认一致；`PENDING→APPLIED` 是显式登记的新增状态边，不假装既有能力已具备。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、回收/短缺补偿模型、异常状态推进、幂等与未知结果、失败分类、迁移回滚与验收映射。
