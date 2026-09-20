# 支付财务修复（`PAYMENT_FINANCIAL_REPAIR`）领域设计

状态：`DESIGN_FROZEN / SELF_REVIEW_PASS / INDEPENDENT_REVIEW_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。首版范围由用户在 2026-09-19 确认为**仅“已确认支付但未入账”的补记**，权限与命令版本复用现有 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`（守卫见 §9）。本文冻结 GSA-28 的 closed command/snapshot/proof、外部证据规则、幂等与恢复、失败分类、迁移与回滚及验收映射；不新增可执行路径，activation 行保持 `INACTIVE`。命令基线见 [十四命令事实与兼容基线](admin-governance-security-command-baseline.md)，治理与激活约束见 [治理与安全重设计](admin-governance-security-redesign.md) §11 与 §14。

## 1. 命令现状（事实）

- **只有策略元数据**：Account `approval_command_policies`/`approvalpolicy` 登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`，请求/审批权限 `platform.billing.repairs.request|approve`、`FINANCIAL_FACT_CHANGE`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`、`internal/platform/approvalpolicy/registry.go`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，create/update/submit/APPROVE/Begin 全部失败关闭。
- **Billing 侧只有未激活行**：`('PAYMENT_FINANCIAL_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`）；无 handler、proof、executor、consumer durable 或恢复分类。

## 2. 既有“已确认支付未入账”修复路径（事实，本命令必须复用）

- **入口与前置**：`PaymentCoordinator.RepairReviewedOrder`（`internal/billing/reconciliation.go`）要求能力 `ManualRepairSupported`+`OrderQuerySupported`、非空 `actorID` 与 1–500 字符原因、订单 `status='MANUAL_REVIEW'` 且 `fulfillment_status='MANUAL_REVIEW'`；随后调用 Provider 查询并要求 `status='SUCCESS'`、渠道交易号/支付时间非空，且商户单号、渠道、金额（分）、币种与订单逐项相等。
- **入账写入**：事务内重读订单（`FOR UPDATE`）→ 记录钱包 `posted_microcredits` 作为 `balanceBefore` → `mergeSuccessfulPaymentWithinTransaction(..., "MANUAL_REPAIR", "repair:"+order.ID+":"+channelTransactionID, snapshot, provider, repair=true, ...)`：写 `payment_transactions`（`status='SUCCEEDED'`）、推进订单为已支付/已履约、写 `REPAIR_CREDIT` journal（`business_reference='repair-payment-transaction:'+transactionID`）与成对 posting（BASE/BONUS lot 的可用额计入钱包负债账户、未结应收清偿`RECEIVABLE_ASSET`、`PAYMENT_CLEARING −amount`）、按 `splitProductComponents` 写 credit lot、entitlement package 与 BASE/BONUS component、更新钱包 `posted_microcredits += amount − appliedToReceivables`（`internal/billing/payments.go`）。
- **审计行**：同事务追加 `payment_order_repairs`（`migrations/billing/000014`、`000015`、`000024`）：订单、交易、`payment_evidence_id`、actor、原因、Provider 快照、唯一 entitlement package、`points_awarded>0`、`wallet_balance_before/after`，以 `ON CONFLICT (payment_transaction_id) DO NOTHING` 收口；`UNIQUE(payment_transaction_id)` 与 `UNIQUE(entitlement_package_id)` 使**每笔交易至多一次补记**。
- **支付事实**：`payment_orders`（状态含 `MANUAL_REVIEW`，无 `version`，有 `updated_at`/`attention_summary`/`summary_version`）、`payment_transactions`（`000004` 的单订单唯一索引已被 `000024` 改为普通索引 `(payment_order_id,created_at,id)`，当前唯一约束只有组合身份 `UNIQUE(id,payment_order_id)` 与 `provider_transaction_id` 唯一——**数据库不再保证"一订单一条交易"**）、`payment_evidence`（`evidence_type ∈ {CALLBACK,QUERY,BILL,MANUAL_REPAIR}`，含 `provider_id/provider_version/evidence_key/merchant_order_no/provider_transaction_id/amount_fen/currency/provider_status/payload_digest/payload_snapshot/merge_result`，唯一键 `(provider_id,provider_version,evidence_type,evidence_key,payload_digest)`）。
- **现状缺口**：该路径由运维/对账侧直接调用，没有审批、proof、activation、专属 consumer/durable、result-first 或恢复分类；它同时**主动调用 Provider**，不能直接作为审批命令的执行体。

## 3. 与相邻流程的职责边界（避免重复建设）

- 本命令不是第二个任意金额入口：金额、组件拆分、lot、package 与账本方向全部由订单冻结的 `credit_microcredits`/`product_snapshot` 与既有发放算法决定。
- 退款/强制退款、拒付、延迟支付评审、对账案件与对账差异修复各有路径（GSA-29/31/33）；本命令只在“Provider 已确认成功且内部完全未入账”时补记。
- GSA-21 已冻结“权益组成拆分错配”；本命令不改既有 package/component，也不纠正已入账的组成，只补记**完全缺失**的入账。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**允许的领域变化**（复用既有补记写入者，单次执行恰好产生；同一事务内）：

1. `payment_transactions` 一行（`SUCCEEDED`）与订单推进：`payment_orders.status='PAID'`、`fulfillment_status='CREDITED'`、`paid_at`/`channel_transaction_id` 按冻结证据写入、`updated_at`/`summary_version` 递增、`attention_summary` 按既有规则重算。
2. 一条 `REPAIR_CREDIT` journal（`business_reference='repair-payment-transaction:'||<payment_transaction_id>`）与其成对 posting：每个组件的可用额记入钱包负债账户（带 lot）、未结应收清偿记入 `RECEIVABLE_ASSET`、`PAYMENT_CLEARING −amount`；journal 合计 0。
3. credit lot（每组件一个）、entitlement package（`source_type='PAYMENT'`）与 BASE/BONUS component，字段与既有路径逐字段一致。
4. 钱包 `posted_microcredits += amount−appliedToReceivables`、`version+1`、`updated_at`；未结应收按既有 `CASH_RECHARGE_BASE` 顺序清偿（既有语义，不新造规则）。
5. `payment_order_repairs` 一行（审计）：`actor_id`=审批执行身份（冻结在 wire 上的审批人 `approver_user_id`，与既有 `payment_provider_resume_executions.approver_user_id` 同义；执行时缺失即拒绝，不得写入全零 `actor_id`）、`reason`=审批冻结的原因摘要、`provider_snapshot`=冻结证据快照摘要、`payment_evidence_id`=冻结证据行、`entitlement_package_id`、`points_awarded`、`wallet_balance_before/after`，以及 GSA-28 前向迁移追加的审批联动列。

**明确禁止**：

- 执行期禁止调用 Provider、禁止发起任何外部网络写；外部证据必须是**已持久化**的 `payment_evidence` 行（见 §5）。
- 禁止服务订单以外的场景：非 `MANUAL_REVIEW`、已有成功交易/package/补记行、存在退款或拒付事实、`credit_microcredits` 或 `product_snapshot` 与证据不一致的订单一律失败关闭。
- 禁止改写订单金额、商品快照、渠道交易号、既有 journal/posting、既有 package/component/lot 或既有 `payment_order_repairs` 行。
- 禁止一订单多于一条交易、一交易多于一次补记（DB 唯一约束）、一 package 用于第二笔交易。
- 禁止把本命令当作“重复入账的更正”或“金额不符的更正”（属 GSA-33/对账家族）。

## 5. 外部证据与判据（冻结）

| 证据 | 要求 |
|---|---|
| Provider 证据行 | 至少一条 `payment_evidence` 满足：`provider_status` 表示成功、`merchant_order_no`、`provider_transaction_id`、`amount_fen`、`currency`、`provider_id`/`provider_version` 与订单/订单商品快照一致，`merge_result ∈ {RECORDED,DUPLICATE,MERGED}`（`REVIEW_REQUIRED` 拒绝） |
| 订单状态 | `status='MANUAL_REVIEW'` 且 `fulfillment_status='MANUAL_REVIEW'`；`channel_transaction_id` 为空或等于冻结的 `provider_transaction_id` |
| 内部未入账 | 订单不存在 `status IN ('PAID','LATE_PAID')` 或 `fulfillment_status='CREDITED'` 的交易、无可引用 package、无 `payment_order_repairs` 行；且存在恰好一条 `fulfillment_status='REVIEW_REQUIRED'` 的交易，其 `provider_transaction_id` 等于冻结交易号（该交易即订单进入 `MANUAL_REVIEW` 的来源，补记复用既有 repair 分支并产出 `REPAIR_CREDIT`） |
| 无冲突事实 | 订单无 `refunds`/`chargebacks` 行，钱包存在且 `status='ACTIVE'`、`posted_microcredits>=0` |
| 外部幂等引用 | 冻结 `provider_transaction_id` 作为外部幂等引用；同一交易号只能补记一次 |

判据全部由 Billing 从持久事实重算；proof 不得从请求体读取 Provider 文本、不得接受浏览器提供的金额或交易号。

## 6. closed command / snapshot / proof / presenter（冻结）

### 6.1 normalized command（`additionalProperties:false`，RFC 8785，整数为 JSON number）

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `PAYMENT_FINANCIAL_REPAIR` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `payment_order_id` | uuid | 目标订单，必须 `MANUAL_REVIEW` |
| `payment_evidence_id` | uuid | 冻结的成功证据行，必须属于该订单 |
| `reason_code` | string | 唯一取值 `CONFIRMED_PAYMENT_NOT_CREDITED` |

订单金额、交易号、组件拆分、lot/package/账本字段一律由领域推导；不接受任何金额、账户、lot、package 或 Provider 文本字段。

### 6.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.payment_order_updated_at` | 冻结订单 `updated_at`（RFC3339 UTC） |
| `critical_resource_versions.facts_digest` | 下述事实集合的 closed RFC8785 摘要 |
| `critical_resource_versions.policy_version` | 1 |
| `financial_snapshot.amount_fen` / `.credit_microcredits` | 订单冻结金额与额度 |
| `financial_snapshot.provider_transaction_id` | 冻结的外部幂等引用 |
| `financial_snapshot.wallet_id` / `.wallet_version` | 目标钱包与版本 |
| `financial_snapshot.applied_to_receivables` | 预检计算出的未结应收清偿额 |

`facts_digest` 覆盖：订单 `status/fulfillment_status/channel/amount_fen/currency/credit_microcredits/product_snapshot/channel_transaction_id/updated_at/summary_version`、冻结证据行全字段摘要（含 `payload_digest`）、钱包 `status/posted_microcredits/version`、未结应收三元组与分配汇总、以及"无交易/无 package/无补记/无退款/无拒付"的零事实证据、activation 版本与 policy/schema 版本。

### 6.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属身份，2 秒绝对期限、64 KiB 上限，按 §5 重读并重算，返回闭合结果（`PROOF_VALID/PROOF_DENY`、订单、证据、金额、组件拆分（BASE/BONUS）、`applied_to_receivables`、钱包版本、`facts_digest`、policy/schema/activation 版本）。禁止执行期 Provider 调用，proof 亦不得触发外部请求。
- presenter（GSA-37）只显示订单号（脱敏）、渠道、金额（分）、额度、冻结证据摘要（`evidence_type/received_at/payload_digest` 截断显示）、组件拆分、预计清偿应收额、钱包版本与风险警示；不下发 Provider 原文、支付凭证、个人数据或内部 proof 字段。

## 7. 守恒、幂等与未知结果

- **守恒**：`sum(ledger_postings)=0`；`wallet.posted_microcredits` 增量必须等于 `amount−appliedToReceivables`；`sum(component.original)=order.credit_microcredits`；package/lot/component 的额度与订单冻结额度一致；`payment_order_repairs.points_awarded=order.credit_microcredits` 且 `wallet_balance_after=before+points_awarded`（既有 CHECK）。
- **线性化点**：`payment_order_repairs_payment_transaction_id_key`（同一 provider 交易至多一次补记，且既有运维路径同样受此约束）、`payment_order_repairs.entitlement_package_id` 唯一、`entitlement_packages` 的 `(id,source_payment_transaction_id)` 身份唯一与 `payment_transactions.provider_transaction_id` 唯一，辅以订单行锁；由于 `000024` 之后数据库**不再**保证一订单一条交易，执行前必须显式按"该订单是否已有绑定冻结 `provider_transaction_id` 的交易/package/补记行"判定已入账，不得依赖"每订单至多一条交易"这一已失效不变量。并发重复执行至多一次成功，第二次读回既有交易/package/审计行。
- **result-first**：先按 `payment_order_id`/`payment_transaction_id`/`approval_request_id` 只读读回交易、package、`payment_order_repairs` 与 journal；命中即返回同一结果，不生成新 ID、不再补记。
- **未知结果**：提交结果不明时只读查证上述四类事实是否同时存在；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`。禁止在未知状态下重放补记、禁止把"未查到"当作零效果。
- **执行顺序**：认领 execution → 校验 activation/policy/schema 与 `facts_digest` → `SELECT payment_orders ... FOR UPDATE` → 复读证据与零事实 → 复用既有补记写入者 → 追加审批审计行 → 提交后复读确认。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、policy/schema/版本不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态 |
| 订单非 `MANUAL_REVIEW`、证据不足/不一致、已有交易或 package、存在退款/拒付、钱包非 ACTIVE | `PROOF_DENY` | 失败关闭、零写入、终态 |
| 已有 `payment_order_repairs`（含既有运维路径产生） | 幂等命中 | 读回既有 package/交易/审计行，`SUCCEEDED`，零新写入 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 有界重试；应用层最多 12 次尝试，第 12 次仍失败则发布 DLQ 并按 PubAck/`ACK_PENDING`/DoubleAck 收口；禁止通用人工 replay |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证，不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用 `platform.billing.repairs.request|approve`、一名非申请人审批、24 小时有效期、5 分钟窗口，不新增权限或 policy/snapshot 版本。
- 依据：该命令 `policy_version=1 / snapshot_schema_version=1` 行从未是可执行合同（静态不可执行，四入口失败关闭），不存在由该版本产生并已执行的领域事实；首次冻结的 closed 形状即该版本合同。
- 激活前只读守卫：目标库中该命令必须不存在 `APPROVED/EXECUTING/EXECUTION_FAILED` 或其它未终态申请；守卫同时统计"`MANUAL_REVIEW` 订单且已有 Provider 成功证据但无内部入账"的存量清单，存量必须由运维走既有 `RepairReviewedOrder` 收口或在激活后逐笔走本命令，不得在激活时批量改写。
- `billing_approval_command_activations` 行保持 `INACTIVE`；GSA-28 交付默认关闭的专属 consumer/durable 开关；生产激活只属于取得独立授权的 GSA-40。
- 放宽首版范围（例如金额不符、重复入账更正、主动 Provider 复核）必须新增 policy/schema 版本与独立设计。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：为 `payment_order_repairs` 增加可空审批联动列（`approval_request_id`、`authorization_event_id`、`approval_epoch`、`execution_key_digest`、`facts_digest`）与相应唯一索引（非空时唯一），由 `billing_service` 持有；不改既有列、CHECK、唯一约束，不改 `000014/000015/000024/000054`。
- N/N-1：先部署可读新列的 Billing，再部署 proof 端与执行器/消费者，最后业务页；旧运维路径写入时新列为 NULL，语义为"运维补记"，读取端必须区分而不误判为审批执行。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；已产生的交易/package/lot/journal/审计行一律保留可读，禁止破坏性 down 撤销已入账事实。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-28 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 正例补记 | 既有 `REVIEW_REQUIRED` 交易推进为已入账、package/lot/component/钱包与既有 `RepairReviewedOrder` 在同一状态下的结果逐字段一致；journal 为 `REPAIR_CREDIT`（引用 `repair-payment-transaction:`）；审计行含审批联动列与 `wallet_balance_before/after` |
| 应收联动 | 钱包存在未结 `USAGE_OVERAGE` 应收时按既有顺序清偿并写 `CASH_RECHARGE_BASE` 分配；无应收时不产生分配 |
| 证据门禁 | 无证据、证据与订单不一致、`merge_result='REVIEW_REQUIRED'`、证据缺 `provider_transaction_id` 全部 `PROOF_DENY` 且零写入 |
| 状态门禁 | 非 `MANUAL_REVIEW`、已履约、已退款、存在拒付、钱包非 ACTIVE 全部失败关闭 |
| 重复入账门禁 | 已有交易/package/审计行时幂等读回，不产生第二条交易、第二个 package、第二次补记 |
| 并发 | 同订单两次并发授权/重复投递至多一次成功；与回调/查询合并并发时唯一约束生效且无死锁 |
| 崩溃/未知 | 提交后中断可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 守恒 | journal 平衡、金额等于 `amount−appliedToReceivables`、组件合计等于订单额度、`points_awarded` 与余额上下界 CHECK 通过 |
| 外部证据边界 | 执行与 proof 路径完全不发起 Provider 调用（用真实断言证明零外部请求） |
| 权限与 presenter | 自审/越权/撤权/窗口过期零副作用；presenter 脱敏且字段闭合 |
| N/N-1 与回滚 | 旧运维路径行（新列为 NULL）可读且不被误判；翻回 `INACTIVE` 后失败关闭，历史保留 |
| 真实领域 E2E | Account 批准 → outbox → relay → 专属 durable → 补记入账 → 终态 → 重放幂等，真实 PostgreSQL + 真实 HTTP，无 SKIP 替代 |

## 12. 本切片不做的事

- 不新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径。
- 不改动既有 `RepairReviewedOrder`、回调/查询合并、退款、拒付、对账或 `attention_summary` 的既有行为。
- 设计冻结不等于实现或激活；GSA-28 未交付前该命令保持不可执行。

## 13. 自检与验证

- 每条事实可回溯到 §1/§2 的迁移与源码路径；§4 的允许变化逐项对应既有补记写入者实际写入的表与列。
- 首版范围与版本复用决策与用户 2026-09-19 确认一致，未猜测填入其它选项。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、外部证据（仅持久化证据、执行期零外部调用）、允许/禁止变化、守恒与线性化点、未知结果、失败分类、迁移回滚与验收映射。
