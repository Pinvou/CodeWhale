# 账本修复（`LEDGER_REPAIR`）领域设计

状态：`DESIGN_FROZEN / PRIVATE_IMPLEMENTATION_READY / FULL_PARENT_REVIEW_PENDING / DEFAULT_INACTIVE / NOT_DEPLOYED`。当前私有候选已实现本章冻结范围，默认 activation 为 `INACTIVE`；尚未部署，也不表示完整父组合已获独立审核通过。首版范围由用户在 2026-09-19 确认为**仅“归属错误”的等额冲正**，权限与命令版本复用现有 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`（理由与守卫见 §9）。本文冻结 GSA-24 的 closed command/snapshot/proof、账务守恒、幂等与恢复、失败分类、迁移与回滚及验收映射；下列历史基线解释设计冻结时的缺口；当前实现仍以实时 activation 为授权边界，不自动激活。命令基线见 [十四命令事实与兼容基线](admin-governance-security-command-baseline.md)，治理与激活约束见 [治理与安全重设计](admin-governance-security-redesign.md) §11 与 §14。

## 1. 设计冻结时的命令历史基线

- **只有策略元数据**：Account `approval_command_policies` 与 `approvalpolicy` 注册表为该命令登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求权限 `platform.billing.repairs.request`、审批权限 `platform.billing.repairs.approve`、风险类 `FINANCIAL_FACT_CHANGE`、一名非申请人审批、critical 字段 `target_id/resource_version/financial_snapshot/policy_version`、24 小时申请有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`、`internal/platform/approvalpolicy/registry.go`）。
- **静态不可执行**：该命令不在 `executableApprovalCommands` 内，因此 create/update/submit/APPROVE/Begin 均在领域调用前失败关闭，Portal 目录以 Billing activation 真相为准（`internal/account/approvals.go`、`internal/account/token_governance_activation.go`）。
- **Billing 侧只有未激活行**：`billing_approval_command_activations` 中为 `('LEDGER_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`）。不存在该命令的 HTTP handler、proof 投影、executor、consumer durable、恢复分类或结果载荷；`internal/platform/approvalconsumer` 的 durable 注册表只有既有六命令与 GSA-22 的权益纠错专属 durable。
- **合同层已登记为已知命令**：OpenAPI 与 Node 合同把该命令列入 11 项 activation 投影并要求 `state=INACTIVE`（`contracts/openapi/billing.yaml`、`test/contract/billing-approval-activation.test.mjs`）。

历史结论：设计冻结时不存在可执行路径或通过审批获得执行授权的可能，实现前保持关闭。

当前私有候选已接入 Account 只读预检、权威 proof、typed 审批投影与结果/恢复客户端，Billing proof/执行器与专属 consumer、幂等回放，以及 Portal/BFF/三语修复表单。Ledger 执行新增一条平衡 `REVERSAL` journal 与两条配对 posting，仍遵守 §4–§5 的等额归属修正与不可改写边界。create/update/submit/APPROVE/Begin 五个授权入口均核对实时 activation；默认 `INACTIVE` 下可准备只读预检，发行及执行仍失败关闭。实际生产部署、activation 与完整 E2E 未执行。

## 2. 设计冻结时的账本基线与不可改写边界

- **账户类型与系统账户**：`ledger_accounts.account_type` 的当前白名单为七个值——`WALLET_LIABILITY/PAYMENT_CLEARING/USAGE_REVENUE/REFUND_CLEARING/ADJUSTMENT`（`000002`）加 `RECEIVABLE_ASSET/CHARGEBACK_CLEARING`（`000025`）；六个平台系统账户使用固定 UUID 并在 Billing 启动时校验存在，且 `wallet_id IS NULL`：`platform:payment-clearing`、`platform:usage-revenue`、`platform:refund-clearing`、`platform:adjustment`、`platform:receivables`、`platform:chargeback-clearing`（`000045`、`internal/billing/system_ledger_accounts.go`、`internal/billing/postgres.go`）。每个钱包另有唯一 `WALLET_LIABILITY` 账户（`walletLedgerAccount`）。
- **journal 与 posting 必须新增、不可改写**：`journal_entries.business_reference` 全局唯一；`entry_type` 白名单经 `000025` 收窄并由前向 `000060`（提交 `d253eeb0a`）补齐实际写入值，当前为 `RECHARGE/USAGE/REFUND/REVERSAL/ADJUSTMENT/EXPIRY/RECEIVABLE/CHARGEBACK/REPAIR_CREDIT/FORCED_REFUND`；`journal_entries_immutable`、`ledger_postings_immutable` 触发器拒绝 UPDATE/DELETE；`ledger_postings.delta_microcredits<>0`；`ledger_postings_balanced`（`DEFERRABLE INITIALLY DEFERRED`）要求同一 journal 内 posting 求和为 0（`000002`）。
- **余额与额度护栏**：`wallets.posted_microcredits>=0` 且带 `version`；`credit_lots.remaining_microcredits ∈ [0,original_microcredits]`；`entitlement_packages`/`entitlement_components` 有各自守恒约束与只读投影。钱包与 lot 的余额由写入者在同一事务内显式维护，账本本身不推导余额（`postgres.go`、`adjustments.go`）。
- **既有账本写入者与方向（冻结契约的来源）**：`RECHARGE`（钱包 `+A`（带 lot）/`PAYMENT_CLEARING -A`，`postgres.go`）、`USAGE`（钱包 `-S`（带 lot）/`USAGE_REVENUE +A`/`RECEIVABLE_ASSET -O`，`postgres.go`）、`ADJUSTMENT`（钱包 `+A`（带 lot）/`ADJUSTMENT -A`，`adjustments.go`）、`REFUND`（钱包 `-A`（带 lot）/`REFUND_CLEARING +T`，`refunds.go`）、`CHARGEBACK`（钱包 `-R`（带 lot）/`RECEIVABLE_ASSET -B`/`CHARGEBACK_CLEARING +R+B`，`chargebacks.go`）、`EXPIRY`（钱包 `-E`（带 lot）/`ADJUSTMENT +remaining`，`packages.go`）、`REPAIR_CREDIT`（钱包 `+A`（带 lot）/`RECEIVABLE_ASSET +R`/`PAYMENT_CLEARING -A`，`payments.go`）、`FORCED_REFUND`（钱包 `-R`（带 lot）/`RECEIVABLE_ASSET -B`/`REFUND_CLEARING +R+B`，`forced_refunds.go`）。设计冻结时 `REVERSAL` 与 `RECEIVABLE` **没有任何写入者**。本候选新增的 Ledger 修复是受 §4 限制的平衡 `REVERSAL` 写入者，不改写原 journal/posting；此处其余历史写入方向仍是冻结判据的来源。
- **已修复的前置缺陷**：`entry_type` 白名单与写入者不一致（`EXPIRE/REPAIR_CREDIT/FORCED_REFUND`）与 `receivables.source_type` 缺少 `FORCED_REFUND` 已由前向 `000060` 修复，并新增真实 PostgreSQL 回归（提交 `d253eeb0a`，Billing 真库整包 297.290s PASS）。本文不再重复处理该缺陷。
- **可发现的结论**：唯一 `business_reference` 是天然幂等边界；任何修复只能是“新增一个平衡 journal 及其配对 posting”，不能修改、删除、重放既有 journal/posting。

## 3. 与相邻流程的职责边界（避免重复建设）

- “支付已确认但未入账”已有对账驱动的 `REPAIR_CREDIT` 路径（GSA-27 冻结证据与范围），本命令不重复实现。
- 钱包余额的人工增减已有 `WALLET_ADJUSTMENT`（审批 + `ADJUSTMENT` 账户对向 posting），本命令不得成为第二个任意金额入口。
- 权益组成与原支付拆分错配已有 GSA-22 `ENTITLEMENT_COMPONENT_ADJUSTMENT`（含 zero-history 门禁与成对账本）。
- `payment_order_repairs`、`reconciliation_cases` 记录支付/对账事实与观察，不直接改账本；退款、拒付、强制退款、用量结算的例外分别属 GSA-29/31/35。
- 本命令**不等于**通用账本编辑：缺失/重复记账（属 GSA-27/33）、金额与业务事实不符（属 GSA-27/35）、lot 或钱包归属错误、组件组成错配（属 GSA-21）都在范围之外。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**唯一允许的领域变化**（单次执行恰好产生，全部在同一 Billing Serializable 事务内）：

1. 一条 `journal_entries`：`entry_type='REVERSAL'`，`business_reference='ledger-repair:'||<目标 posting 的 UUID>`，`description` 为固定模板 `ledger repair: <目标 journal 的 business_reference>`，`created_at` 取执行时刻。
2. 恰好两条 `ledger_postings`，两者 `credit_lot_id IS NULL`：`-D` 记在**被判定归属错误的平台账户**，`+D` 记在**正确平台账户**；`D` 等于目标 posting 原带符号的 `delta_microcredits`，公开命令金额仍为 `abs(D)>0`；两条求和为 0。
3. 一条 `ledger_repair_cases` 不可变审计行（新增表，见 §7.1），记录审批/授权引用、目标 journal/posting、两账户、金额、事实摘要与 journal ID。

**明确禁止**：

- 禁止 UPDATE/DELETE 任何 `journal_entries`/`ledger_postings`（不可变触发器亦拒绝），禁止复用或重放既有 journal。
- 禁止改写或新增 `wallets`、`credit_lots`、`entitlement_packages`、`entitlement_components`、`receivables`、`payment_*`、`refund_*`、`chargeback_*`、`usage_holds`、`reconciliation_*` 的任何行或字段：`wallets.posted_microcredits/version`、`credit_lots.remaining_microcredits` 必须逐行不变。
- 禁止把本命令用于金额不符、缺失/重复记账、lot/钱包归属错误、组成错配、应收核销或 Provider 侧事实；这些属于其它家族或需要新设计。
- 禁止一次执行产生多条 journal、多于两条 posting，或任何“部分冲正”“多次冲正同一 posting”的形态。
- 禁止 `REVERSAL` 之外的新 `entry_type`，也禁止新增/放宽任何数据库枚举（本命令复用既有白名单）。

## 5. 归属判据：`entry_type` 账户类型契约（冻结）

“归属错误”不由人工判断，而由**条目类型的账户类型契约**判定。契约 = 该 `entry_type` 既有写入者写出的账户类型与净方向签名（§2 的事实来源）：

| `entry_type` | 必须出现的账户类型签名（净 delta 符号） |
|---|---|
| `RECHARGE` | `PAYMENT_CLEARING −A` 必需；`WALLET_LIABILITY +A` 当 `available>0` 时必需；`RECEIVABLE_ASSET +R` 当 `appliedToReceivables>0` 时可出现（全额清偿未结应收时无钱包腿） |
| `USAGE` | `USAGE_REVENUE +A` 必需；`WALLET_LIABILITY −S` 当 `settled>0` 时必需；`RECEIVABLE_ASSET −O` 当 `overage>0` 时可出现 |
| `ADJUSTMENT` | `WALLET_LIABILITY ±A` 与 `ADJUSTMENT ∓A` 成对（`adjustments.go` 两方向）或 `ADJUSTMENT −A` + `RECEIVABLE_ASSET +A`（GSA-25 核销形状，无钱包腿） |
| `REFUND` | `REFUND_CLEARING +T` 必需；`WALLET_LIABILITY −A` 当 `revoked>0` 时必需；`RECEIVABLE_ASSET −B` 当缺口回收时出现 |
| `EXPIRY` | `ADJUSTMENT +E` 必需；`WALLET_LIABILITY −E` 当 `expiring>0` 时必需 |
| `CHARGEBACK` | `CHARGEBACK_CLEARING +R+B` 必需；`WALLET_LIABILITY −R` 当 `revocable>0` 时必需；`RECEIVABLE_ASSET −B` 当 `baseShortfall>0` 时必需（仅剩短缺时只有两条平台腿） |
| `REPAIR_CREDIT` | `PAYMENT_CLEARING −A` 必需；`WALLET_LIABILITY +A` 当 `available>0` 时必需；`RECEIVABLE_ASSET +R` 当 `appliedToReceivables>0` 时可出现 |
| `FORCED_REFUND` | `REFUND_CLEARING +R+B` 必需；`WALLET_LIABILITY −R` 当 `revocable>0` 时必需；`RECEIVABLE_ASSET −B` 当 `shortfall>0` 时必需 |

上表的"必需/可出现"是**逐腿条件**：同一 `entry_type` 允许多个合法形状（条件腿缺席即该形状），判据必须按形状集合做成员判定，不能把条件腿当必需腿。写入者除上表列出的路径外，`RECHARGE` 还由支付合并路径写入（`payment_evidence.go` → `payments.go`，`repair=false`），`REPAIR_CREDIT` 由同一段代码以 `repair=true` 写入。

判定与放行规则（全部必须成立，任一不成立即 `PROOF_DENY`）：

1. 目标 journal 的 `entry_type` 在上表内且 `entry_type <> 'REVERSAL'`；目标 posting 属于该 journal，其 `ledger_account_id` 是六个平台系统账户之一（`wallet_id IS NULL`），且 `credit_lot_id IS NULL`。
2. 目标 journal 的当前账户类型签名**不属于**该 `entry_type` 的合法形状集合（即已违反契约）；若属于任一合法形状则拒绝（无错误可修）。
3. 把该 posting 的账户替换为闭合命令给出的平台账户后，该 journal 的账户类型签名**必须恰好等于**该 `entry_type` 的某个合法形状（按 1 的逐腿条件判定，含 GSA-25 核销与 GSA-31 补偿这两类平台双腿形状）；`D` 等于该 posting 的 `abs(delta)`。
4. 被修账户与替换账户都是平台系统账户、都存在、且不相同；不允许指向钱包账户。
5. 目标 posting 尚未被本命令冲正过（`ledger_repair_cases` 无同 posting 记录，且不存在 `business_reference='ledger-repair:'||<posting>` 的 journal）。

## 6. closed command / snapshot / proof / presenter（冻结）

### 6.1 normalized command

`additionalProperties:false`、RFC 8785 规范化、整数为 JSON number：

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `LEDGER_REPAIR` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `target_journal_entry_id` / `target_posting_id` | uuid | 必须互相归属；目标条目类型在 §5 表内 |
| `wrong_ledger_account_id` | uuid | 必须是六个平台账户之一且等于目标 posting 的账户 |
| `correct_ledger_account_id` | uuid | 必须是六个平台账户之一、不同于被修账户、且满足 §5 规则 3 |
| `amount_microcredits` | int64 | `>0`，必须等于 `abs(目标 posting.delta)` |
| `reason_code` | string | 唯一取值 `ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT` |

不接受自由文本原因进入数值事实（原因单独审计并脱敏）、不接受任意 SQL、Provider 原文、账户类型字符串或未列字段。

### 6.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.target_posting_id` | 目标 posting UUID |
| `critical_resource_versions.posting_set_digest` | 目标 journal 的 `{entry_type,business_reference,created_at}` 与全部 posting `{id,ledger_account_id,delta_microcredits,credit_lot_id}` 按 ID 排序后的 closed RFC8785 摘要 |
| `critical_resource_versions.entry_type` / `.policy_version` | 目标条目类型 / 1 |
| `financial_snapshot.amount_microcredits` | 等于命令金额 |
| `financial_snapshot.wrong_ledger_account_id` / `.correct_ledger_account_id` | 两平台账户 UUID |
| `financial_snapshot.wrong_account_type` / `.correct_account_type` | 重读得到的账户类型 |
| `financial_snapshot.wallet_id` | 固定 `null`；非 null 即 `PROOF_DENY` |

目标 journal/posting 由不可变触发器保护，`posting_set_digest` 即本命令的 CAS 观察值：任何证据漂移只能表现为摘要不一致并失败关闭，不存在可原地更新的 `updated_at` 语义。

### 6.3 proof 与 presenter

- Billing 内部证明端点（GSA-15 交付）只接受 Account 专属工作负载身份，2 秒绝对期限、64 KiB 合计限制，重读目标 journal/posting/两账户/activation 行并按 §5 重算，返回闭合结果（`PROOF_VALID/PROOF_DENY`、`entry_type`、金额、两账户与类型、`posting_set_digest`、`policy_version`、`schema_version`、`activation_version`）。浏览器、Portal 与审批人不得生成或提交 proof。
- 审批详情 presenter（GSA-37）只显示目标 journal 的 `entry_type/business_reference/created_at`、被修账户与正确账户的代码与类型、金额、判据结论、`posting_set_digest` 摘要、风险警示与稳定错误码；不下发 Provider 原文、原始命令 JSON、个人数据或内部 proof 字段。

## 7. 账务守恒、执行顺序与幂等

### 7.1 新增不可变审计行（GSA-24 前向迁移）

`ledger_repair_cases`（由 `billing_schema_owner` 持有并只向运行期授予最小 `INSERT/SELECT`，UPDATE/DELETE 由触发器与 ACL 双重拒绝）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`target_journal_entry_id`、`target_posting_id`（唯一）、`wrong_ledger_account_id`、`correct_ledger_account_id`、`entry_type`、`amount_microcredits`（`>0`）、`posting_set_digest`（`^[0-9a-f]{64}$`）、`facts_digest`（同）、`journal_entry_id`（唯一，指向 `REVERSAL` 条目）、`state`、`created_at`。

### 7.2 守恒

- 新 journal 内两条 posting 互为相反数、求和为 0（`ledger_postings_balanced` 在提交时校验）。
- `wallets.posted_microcredits/version`、`credit_lots.remaining_microcredits`、`entitlement_*`、`receivables`、`payment_*` 在事务提交前后逐行复读必须完全一致（零变化证明）。
- 既有 journal/posting 逐字节不变：执行前复读的目标集合摘要必须等于 `posting_set_digest`，否则整笔失败、零写入。
- 单次执行只产生一条 journal、恰好两条 posting、一条 case；溢出或非正金额拒绝，不使用浮点。

### 7.3 目标唯一性线性化点与 result-first

- `business_reference='ledger-repair:'||target_posting_id` 的全局唯一性即**目标唯一性线性化点**：同一 posting 至多冲正一次；第二次尝试得到唯一冲突并按 result-first 读回既有 case 与 journal，不新增任何行。
- 执行顺序固定：认领 execution → 校验 activation/policy/schema 与摘要 → 复读证据并判据 → 插入 `REVERSAL` journal → 两条 posting → `ledger_repair_cases` → 提交后复读校验 → 确认 `SUCCEEDED`。
- 结果未知（连接中断、超时、进程崩溃）时只允许只读查证：按 `approval_request_id`/`execution_key_digest`/`business_reference` 读取 case 与 journal；只有 case、journal、两条相反 posting、零变化证明同时存在才确认成功，缺失即 `UNKNOWN_QUERY_ONLY`，不得盲重放、不得假定零效果。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、版本/策略/schema 不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态，不重试 |
| 摘要漂移、目标无错误、非平台账户、lot 非空、钱包非空、已满足契约 | `PROOF_DENY` | 失败关闭、零写入、终态，不自动改判 |
| 目标 posting 已冲正 | 幂等命中 | 读回既有 case/result，`SUCCEEDED`，零新写入 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试的领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 有界重试；应用层最多 12 次尝试，第 12 次仍失败则发布 DLQ 并按 PubAck/`ACK_PENDING`/DoubleAck 收口，禁止通用人工 replay |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证；不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用请求权限 `platform.billing.repairs.request`、审批权限 `platform.billing.repairs.approve`、一名非申请人审批、24 小时有效期、5 分钟执行窗口，不新增权限或 policy 版本。
- 依据：该命令 `policy_version=1 / snapshot_schema_version=1` 行**从未是可执行合同**——它不在静态 `executableApprovalCommands` 内，create/update/submit/APPROVE/Begin 自 GSA-01 起全部失败关闭，因此不存在任何由本版本产生并已被执行的领域事实；首次冻结的 closed 形状即该版本合同，不构成对已发布合同的原位修改。
- 守卫：激活前必须以只读 readiness 证明目标库中该命令不存在 `APPROVED/EXECUTING/EXECUTION_FAILED` 或其它未终态申请，且不存在任何 `ledger-repair:` 前缀 journal；任一存在即阻断激活并按 §8 分类只读查证，不得解释或改写历史。
- 若将来需要放宽首版范围（例如缺失/重复记账、金额不符、lot/钱包归属），必须新增 policy/schema 版本与独立领域设计，不得在本版本内扩大解释。
- `billing_approval_command_activations` 行保持 `INACTIVE`；专属 durable 经 `approvalconsumer.BillingCommands()` 的既有受信资源循环注册，运行期按实时 activation 拒绝未激活命令；生产激活只属于取得独立授权的 GSA-40。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `ledger_repair_cases`（实现时落位 `000061`，见提交 `c481b2644`）与 `billing_approval_proof_claims` 的命令白名单扩项 `LEDGER_REPAIR`（落位 `000062`，与 `000059` 对权益纠错的同类扩项同形）；后续家族的编号按实现时的下一个可用编号顺延。不改写 `000002/000025/000045/000054/000059/000060/000061`，不放宽任何枚举，不新增账户或系统账户。
- N/N-1：先部署可读新表/新字段的 Billing，再部署 proof 端与执行器/消费者，最后业务页；旧 writer 在新表缺省时不改变行为，旧客户端继续使用 v1 线格式。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；`ledger_repair_cases`、`REVERSAL` journal 与 posting 一律保留可读，禁止破坏性 down 撤销已执行的补偿事实。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活本命令。

## 11. 验收与证据映射（GSA-24 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 正例：`USAGE` 把 `ADJUSTMENT` 误记为收入账户 | 冲正后签名满足契约；两条相反 posting；钱包/lot/其它表零变化；case 一行 |
| 逐条目类型正例：八类契约类型各一例 | 每类都能判定并冲正，`REVERSAL` 之外不新增取值 |
| 负例：目标已满足契约 | `PROOF_DENY`，零写入，不自动改判 |
| 负例：目标账户是钱包账户或 `credit_lot_id` 非空 | `PROOF_DENY`，零写入 |
| 负例：`wallet_id` 非 null、金额不等、账户相同/不存在 | `PROOF_DENY`，零写入 |
| 幂等：同一 posting 第二次授权/重复投递 | 只读命中同一 case/result；唯一冲突不产生第二 journal |
| 崩溃/未知：提交后中断、执行前中断 | 提交后可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 并发：同目标两次审批并发、同钱包其它写入并发 | 至多一次冲正；其它写入不受影响；无死锁泄漏 |
| 撤权/窗口：批准后撤权、5 分钟窗口过期 | 失败关闭、零写入、终态分类正确 |
| N/N-1 与回滚：缺行、未知版本、翻回 `INACTIVE` | 失败关闭；历史 case/journal 保留可读 |
| 权限与 presenter：非申请人审批、自审、越权、脱敏 | 拒绝路径零副作用；presenter 只有闭合字段 |
| 真实领域 E2E：Account 批准 → outbox → relay → 专属 durable → 成对账本 → 终态 → 重放幂等 | 真实 PostgreSQL + 真实 HTTP 证据，不使用 SKIP 或静态断言替代 |

## 12. 本切片不做的事

- 不新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不改动既有六命令、GSA-22 权益纠错、`REPAIR_CREDIT`、`WALLET_ADJUSTMENT`、对账流程或 `000060` 的任何行为。
- 本文冻结设计不等于已实现或已激活；GSA-24 未交付前该命令保持不可执行。

## 13. 自检与验证

- 每条事实可回溯到 §1/§2 列出的迁移或源码路径；§5 契约表逐行对应 §2 的写入者方向事实。
- 范围、权限与版本决策与用户 2026-09-19 确认一致（仅归属错误等额冲正、复用现有 permission/policy 版本），未猜测填入其它选项。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof/presenter、账户类型契约、守恒与幂等、失败分类、版本复用守卫、迁移回滚与验收映射；同时登记 `000060` 已修复的前置缺陷。
- 2026-09-19 独立审核（`/root/review_frozen_designs`，只读，基线 `c481b2644`）：结论 CHANGES_REQUESTED，共 4 项。本文件已处理第 1 项（§5 契约表逐腿条件化 + 规则 2/3 改为形状集合成员判定 + 补 `payments.go` 写入者）与第 3 项的编号表述待办；其余（`000061` 编号在 GSA-25 文档中的重复表述、GSA-35 `max_microcredits>=0`、管理端 `entry_type` 过滤器闭集）在对应文件继续处理。审核同时确认七份文档结构齐备、`000060`/`packages.go`/activation 投影等关键事实成立。
