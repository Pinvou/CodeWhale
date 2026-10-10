# 当前实现与本轮修复

本波安装 `RECONCILIATION_FINANCIAL_REPAIR` v1/s1：独立三语申请表、原意编辑与新鲜预检、Account closed policy/encoding/presenter/recovery、Portal BFF、Billing 权威 proof/claim/执行、专属 consumer 与精确 NATS ACL。只处理 `PROVIDER_PAID_INTERNAL_MISSING` 的零账务效果结案；不推断内部订单、用户或钱包归属。激活默认 `INACTIVE`，消费者资源与实时 activation 门禁继续生效。

本轮修复在结案事务内推进关联手审单及案件 `version`，按渠道/业务日期跨 run 复核同商户单号的未结差异，校验退款 BILL 与 run 的 provider 身份及持久 payload。既有两条 BILL 导入路径仅在真实 REFUND 条目上保存其退款参考号，不改变支付/拒付参考语义。原 Billing `000072/000073`、Account `000076` 及历史迁移身份不改。

验证由本轮 PR 正文及固定提交证据记录说明；有限 owner PG、受控 React 与默认套件分别披露。完整 Account/outbox/relay/真实 issuer/资源/OS E2E、部署和生产激活未执行。下文“历史设计阶段”保留原阶段记录，当前行为以本节及修正后的事实描述为准。

# 对账财务修复（`RECONCILIATION_FINANCIAL_REPAIR`）领域设计

历史设计阶段状态：`DESIGN_FROZEN / SELF_REVIEW_PASS / INDEPENDENT_REVIEW_PENDING / 历史设计阶段：NOT_IMPLEMENTED / NOT_DEPLOYED`。首版范围由用户在 2026-09-19 确认为**仅 `PROVIDER_PAID_INTERNAL_MISSING` 差异**，权限与命令版本复用现有 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`（守卫见 §9）。本文冻结 GSA-34 的 closed command/snapshot/proof、归因与结案规则、零账务效果的守恒证明、幂等与恢复、失败分类、迁移与回滚及验收映射；不新增可执行路径，activation 行保持 `INACTIVE`。命令基线见 [十四命令事实与兼容基线](admin-governance-security-command-baseline.md)，治理与激活约束见 [治理与安全重设计](admin-governance-security-redesign.md) §11 与 §14。

## 1. 设计冻结阶段命令现状（历史）

- **只有策略元数据**：Account `approval_command_policies`/`approvalpolicy` 登记 `policy_version=1 / snapshot_schema_version=1 / APPROVAL_REQUIRED`、请求/审批权限 `platform.billing.repairs.request|approve`、`FINANCIAL_FACT_CHANGE`、一名非申请人审批、24 小时有效期、5 分钟执行窗口、执行身份 `billing-command-executor`（`migrations/account/000018_approval_commands.up.sql`）。
- **静态不可执行**：不在 `executableApprovalCommands` 内，四入口失败关闭；Billing 侧只有 `('RECONCILIATION_FINANCIAL_REPAIR',1,1,'INACTIVE',1)`（`migrations/billing/000054_approval_command_activations.up.sql`），无 handler、proof、executor、consumer durable 或恢复分类。

## 2. 对账现状与不变量（事实）

- **运行与案件模型**：`reconciliation_runs`（渠道、业务日期、`status ∈ {RUNNING,COMPLETED,FAILED}`、provider/case 计数；`000029` 已允许同渠道/业务日期多个手工/API/file run，仅定时运行保留条件唯一约束）与 `reconciliation_cases`（`reconciliation_run_id`、可空 `payment_order_id`、`merchant_order_no`、`case_type ∈ {PROVIDER_PAID_INTERNAL_MISSING,INTERNAL_PAID_PROVIDER_MISSING,AMOUNT_MISMATCH,DUPLICATE_TRANSACTION}`、`status ∈ {OPEN,IN_REVIEW,RESOLVED,IGNORED}`、`provider_snapshot`、`internal_snapshot`、`resolution`、`created_at`、`resolved_at`）（初始字段见 `000004`，当前状态和扩展模型见 `000029_payment_anomaly_reconciliation.up.sql`）。
- **差异分类**：`CompareReconciliation`（`internal/billing/reconciliation.go`）以商户单号对齐 Provider 账单行与内部订单；Provider 有行而内部无匹配订单即 `PROVIDER_PAID_INTERNAL_MISSING`（P0，`payment_order_id` 为空）。另有按条目类型的比对（`PAYMENT→PROVIDER_PAID_INTERNAL_MISSING`、`REFUND→PROVIDER_REFUND_INTERNAL_MISSING`、`CHARGEBACK→PROVIDER_CHARGEBACK_INTERNAL_MISSING`）。
- **关键事实**：`PROVIDER_PAID_INTERNAL_MISSING` 的语义是**内部完全没有可归属的订单/用户/钱包事实**——因此不存在可写入的钱包、package、component 或 lot，也不存在可创建应收所需的 `wallet_id`（`receivables.wallet_id NOT NULL`）。任何“补记入账”都会凭空选择归属，属于猜测。
- **可用账户**：`PAYMENT_CLEARING`/`ADJUSTMENT` 等系统账户存在，但没有“未识别付款人负债”账户类型；`ledger_accounts.account_type` 白名单固定（`000002`、`000025`），本命令不得新增账户类型。

## 3. 首版范围与精确允许/禁止的变化（冻结）

**允许的领域变化**（单次执行恰好产生，同一 Billing Serializable 事务内）：

1. `reconciliation_cases` 单行结案：`status='RESOLVED'`、`resolution='PROVIDER_REFUND_CONFIRMED'`、`resolved_at=now`、`updated_at=now`、`version=version+1`（`provider_snapshot`/`internal_snapshot`/`case_type`/`merchant_order_no`/`run` 引用逐字段不变）。
2. 若有 `manual_review_case_id`，同事务核对其 `BILLING/RECONCILIATION_CASE/resource_id` 身份并从未结状态推进为 `RESOLVED`，更新结案时间并追加事件；不修改其它手审单。
3. 一条 `reconciliation_repair_cases` 不可变审计行（见 §7.1），记录审批/授权引用、run/case、冻结证据摘要、外部处置证据引用、复核结果与 `facts_digest`。
4. 结案必须同时在冻结 run 渠道/业务日期上执行**修复后再核对**：重新比对 Provider 账单行与内部事实，证明该 `merchant_order_no` 不再产生 `OPEN` 的 `PROVIDER_PAID_INTERNAL_MISSING` 案件（差额已由 Provider 侧退款/冲正消化）。

**明确禁止（首版零账务效果）**：

- **不写任何账本事实**：不新增/修改 `journal_entries`、`ledger_postings`；`business_reference` 不新增。
- **不写任何金额事实**：不新增/修改 `wallets`（含 `posted_microcredits/receivable_microcredits/financial_status/version`）、`receivables`、`credit_lots`、`entitlement_packages`、`entitlement_components`、`payment_orders`、`payment_transactions`、`payment_evidence`、`refunds`、`chargeback_cases`、`forced_refund_cases`、`usage_holds`。
- 禁止在缺少**外部处置证据**时结案：仅当 Provider 账单/退款证据证明该笔款项已由 Provider 侧退回或冲正时才允许 `RESOLVED`；否则失败关闭，案件保持 `OPEN`。
- 禁止由申请人或审批人指定内部订单/钱包/用户来“归属”该笔款项（猜测归属）。
- 禁止把该差异结案为补记入账、应收、可用额度或 `ADJUSTMENT`；需要入账语义时必须新增账户类型、policy/schema 版本与独立设计。

## 4. 外部处置证据与判据（冻结）

| 证据 | 要求 |
|---|---|
| 冻结差异 | 案件 `case_type='PROVIDER_PAID_INTERNAL_MISSING'`、`status='OPEN'`，其 run `status='COMPLETED'`；同渠道/业务日期允许有多个 run，复核不可只查当前 run |
| Provider 侧处置 | 存在与案件 `provider_snapshot` 同一 `provider_id`/`provider_version`/`channel_transaction_id`/`merchant_order_no` 的**退款或冲正**账单事实（`payment_evidence.provider_refund_id` 或 Provider 账单行的同额退款/冲正记录），金额与币种一致且状态为成功 |
| 内部仍无归属 | 冻结时刻与结案时刻的内部比对均无该 `merchant_order_no` 的订单/交易/入账事实（`payment_order_id IS NULL` 保持成立） |
| 零账务证明 | 结案前后逐表复读：§3 禁止清单涉及的表逐行不变（含 `wallets.version` 与 `receivables.settled/outstanding`） |
| 幂等 | 该案件无既有 `reconciliation_repair_cases`，`reconciliation_cases.status='OPEN'` 且 `resolved_at IS NULL` |

判据全部由 Billing 从持久事实重算；proof 不接受浏览器提供的金额、Provider 文本或内部订单号，执行期不调用 Provider。

## 5. closed command / snapshot / proof / presenter（冻结）

### 5.1 normalized command（四字段 closed 对象，整数保留 JSON number）

`command_type` 与 policy/schema 位于审批 envelope，normalized 对象只含下表的案件、证据、日期与 resolution 四字段。预检只收 command、target、payment_evidence、reason_code、business_date 五字段；不增加 expected case version 参数。

| 字段 | 类型 | 规则 |
|---|---|---|
| `command_type` | string | 固定 `RECONCILIATION_FINANCIAL_REPAIR` |
| `policy_version` / `schema_version` | int | 均为 1 |
| `reconciliation_case_id` | uuid | 目标案件，必须 `PROVIDER_PAID_INTERNAL_MISSING` 且 `OPEN` |
| `payment_evidence_id` | uuid | Provider 侧处置证据行 |
| `business_date` | string | 冻结 run 的业务日期（`YYYY-MM-DD`），必须与案件 run 一致 |
| `resolution_code` | string | 唯一取值 `PROVIDER_REFUND_CONFIRMED` |

### 5.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.run_id` / `.case_id` | 冻结 run 与案件 |
| `critical_resource_versions.provider_snapshot_digest` | 持久 JSONB 返回的案件 `provider_snapshot` 字节的 SHA-256；不冒称 RFC8785 摘要 |
| `critical_resource_versions.policy_version` | 固定 1 |
| `critical_resource_versions.facts_digest` | 下述事实集合摘要 |
| `financial_snapshot.amount_fen` / `.currency` | 冻结 Provider 账单行金额与币种 |
| `financial_snapshot.provider_transaction_id` | 外部幂等引用 |
| `financial_snapshot.ledger_effect` | 固定 `"NONE"`（零账务效果的显式声明，非 `NONE` 即 `PROOF_DENY`） |

`facts_digest` 覆盖：案件全字段、run 全字段、Provider 处置证据行摘要（含 `payload_digest`）、内部无匹配事实（订单/交易/入账查询结果为空）、关联手审单全字段、冻结的零账务基线（禁止清单各表的关键计数、钱包 `version` 与应收余额摘要）、以及 activation/policy/schema 版本。

### 5.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属身份，2 秒绝对期限、64 KiB 上限，按 §4 重读并重算，返回闭合结果（`PROOF_VALID/PROOF_DENY`、案件、run、金额/币种、外部证据引用、`ledger_effect='NONE'`、`facts_digest`、policy/schema/activation 版本）。执行期与 proof 均禁止 Provider 调用。
- presenter（GSA-37）只显示差异类型、商户单号（脱敏）、渠道与业务日期、Provider 金额/币种与处置引用（脱敏）、内部无归属结论、`ledger_effect=NONE` 的显式提示、复核结果与风险警示；不下发 Provider 原文、账单明细或个人数据。

## 6. 守恒、幂等与未知结果

- **守恒（零效果证明）**：禁止清单涉及的全部表在结案前后逐行复读完全一致；`journal_entries`/`ledger_postings` 计数与最大 `created_at` 不变；钱包 `version` 与 `receivables` 三元组不变。
- **线性化点**：`reconciliation_repair_cases.case_id` 唯一 + 案件 `status='OPEN' AND resolved_at IS NULL` 的 `UPDATE ... WHERE` 条件 CAS（行锁）；并发两次执行至多一次成功，第二次读回既有结果。
- **result-first**：先严格比对审批/授权/epoch、案件及执行 key 摘要的不可变收据；仅原身份重试命中，已有结果不再用可变当前事实重新授权。随后按 `reconciliation_case_id`/`approval_request_id`/`execution_key_digest` 只读读回 case 与案件终态；命中即返回同一结果，不重复结案、不重复核对。
- **未知结果**：提交结果不明时只读查证 case 与案件 `RESOLVED` 是否同时成立；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`，禁止重放、禁止假定零效果。
- **执行顺序**：认领 execution → 锁来源渠道/日期/商户单号及案件、关联手审单 → 校验 live activation/policy/schema 与非空完整 frozen facts → 复读外部处置证据与内部无归属 → 执行修复后再核对 → 案件与手审单同事务结案 → 写 immutable case → 提交并读回收据；禁止清单前后零效果由有限 PG 验证单独证明。
- **修复后再核对**：核对以冻结 run 的渠道/业务日期跨全部 run 重算未结差异集合；若同一 `merchant_order_no` 仍产生 `OPEN` 案件，整笔失败关闭（禁止用结案掩盖仍存在的差异）。

## 7. 审计行与状态推进

### 7.1 新增不可变审计行（GSA-34 前向迁移）

`reconciliation_repair_cases`（`billing_schema_owner` 持有，运行期仅最小 `INSERT/SELECT`，UPDATE/DELETE 由触发器与 ACL 双重拒绝）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`reconciliation_run_id`、`reconciliation_case_id`（唯一）、`channel`、`business_date`、`merchant_order_no`、`amount_fen`、`currency`、`payment_evidence_id`、`provider_snapshot_digest`、`facts_digest`、`resolution_code`（固定 `PROVIDER_REFUND_CONFIRMED`）、`recheck_case_count`（`=0`）、`state`、`created_at`。

### 7.2 状态推进

- 只允许 `reconciliation_cases.status: OPEN → RESOLVED` 并推进 `version`、更新 `updated_at`；关联手审单同事务结案（`resolution='PROVIDER_REFUND_CONFIRMED'`、`resolved_at=now`）；`IGNORED`、重复结案、`resolution` 复用其它取值一律拒绝。
- 既有对账运行/比对链路（`RunReconciliation`）与 `reconciliation_cases` 的其它写入者保持原语义；本命令不改 run 计数、不改其它案件。

## 8. 失败分类与 DLQ 收口（GSA-16 合同）

| 情形 | 分类 | 行为 |
|---|---|---|
| activation 非 `ACTIVE`、policy/schema/版本不匹配 | `POLICY_OR_SCHEMA_MISMATCH` | 失败关闭、零写入、终态 |
| 案件非 `PROVIDER_PAID_INTERNAL_MISSING`/非 `OPEN`、run 非 `COMPLETED`、业务日期不符、缺外部处置证据、证据与案件不一致、内部出现可归属事实、复核仍有 `OPEN` 案件 | `PROOF_DENY` | 失败关闭、零写入、终态 |
| 原审批/授权身份及冻结事实完全匹配的已执行收据 | 幂等命中 | 读回既有结果，零新写入；案件已结案本身不能授权另一笔请求 |
| 5 分钟执行窗口过期 | `EXECUTION_WINDOW_EXPIRED` | 终态，零写入 |
| 可重试领域故障（序列化失败、死锁、连接失败） | 按 GSA-16 分类 | 有界重试；应用层最多 12 次尝试，第 12 次仍失败发布 DLQ 并按 PubAck/`ACK_PENDING`/DoubleAck 收口；禁止通用人工 replay |
| 提交结果未知 | `UNKNOWN_QUERY_ONLY` | 只读查证，不重放、不改判 |

## 9. 权限、版本与 activation（复用现有 v1/s1）

- 复用 `platform.billing.repairs.request|approve`、一名非申请人审批、24 小时有效期、5 分钟窗口，不新增权限与 policy/snapshot 版本。
- 依据：该命令 v1/s1 行从未是可执行合同（静态不可执行、四入口失败关闭），不存在由该版本产生并已执行的领域事实。
- 激活前只读守卫：该命令必须不存在未终态申请；守卫同时只读统计目标库中 `OPEN` 的 `PROVIDER_PAID_INTERNAL_MISSING` 案件（含 run 与业务日期分布），存量必须在激活后逐笔走本命令，不得在激活时批量改写。
- activation 行保持 `INACTIVE`；专属 consumer/durable 由已安装目录与受信 cutover 资源统一配置，历史命令特有环境开关不再是执行依据；生产激活只属于取得独立授权的 GSA-40。
- 若将来需要账务效果（例如入未识别负债账户、按业务规则补记归属），必须新增账户类型/表、policy/schema 版本与独立设计；不得在本版本内扩大解释。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `reconciliation_repair_cases`（原设计拟编号为 `000061`；当前公开累计迁移为 Billing `000072`）；不改 `000004/000025/000029/000054/000060`，不放宽任何枚举，不新增账户类型。
- N/N-1：先部署可读新表的 Billing，再部署 proof 端与执行器/消费者，最后业务页；既有对账运行与结案链路行为不变。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；已结案案件与 case 行保留可读，禁止破坏性 down 或重开案件。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-34 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 正例结案 | 案件 `RESOLVED` + `resolution=PROVIDER_REFUND_CONFIRMED` + `resolved_at`；case 一行含复核 `recheck_case_count=0` |
| 零账务效果 | 结案前后禁止清单各表逐行一致（含钱包 `version`、`journal/postings` 计数、`receivables` 三元组） |
| 复核门禁 | 人为保留同单号 `OPEN` 差异时必须失败关闭且案件保持 `OPEN`；复核成功后结案 |
| 证据门禁 | 缺外部处置证据、证据金额/币种不符、证据渠道交易号不符、案件类型不符、run 非完成、业务日期不符全部 `PROOF_DENY` |
| 归属门禁 | 命令不接受内部订单/钱包/用户/金额参数（闭合 schema 校验拒绝额外字段）；内部若出现可归属事实则失败关闭 |
| 幂等与并发 | 同案件二次授权/重复投递只命中既有结果；与对账运行并发时 CAS 生效、无死锁 |
| 崩溃/未知 | 提交后中断可只读确认 `SUCCEEDED`；未提交保持 `UNKNOWN_QUERY_ONLY` 且零写入 |
| 入口边界 | 不调用 Provider、不写入任何金额事实（真实断言零外部请求） |
| 权限与 presenter | 自审/越权/撤权/窗口过期零副作用；presenter 明确显示 `ledger_effect=NONE` 与外部处置引用 |
| N/N-1 与回滚 | 缺行/未知版本/翻回 `INACTIVE` 失败关闭；已结案案件与 case 保留可读 |
| 真实领域 E2E | Account 批准 → outbox → relay → 专属 durable → 案件结案与复核 → 终态 → 重放幂等，真实 PostgreSQL + 真实 HTTP，无 SKIP 替代 |

## 12. 被拒绝的替代方案（记录取舍）

| 方案 | 拒绝理由 |
|---|---|
| 按冻结金额补记钱包/package 额度 | 内部无可归属订单/用户/钱包；凭空选归属违反“不得猜测”与守恒要求 |
| 记入新建“未识别付款人负债”账户 | 需要新增账户类型、账本契约与 policy/schema 版本；首版不做，另行设计 |
| 由申请人指定内部订单完成归属 | 归属由人工指定等于把对账差异变成任意入账入口，且可能重复入账 |
| 直接 `IGNORED` 结案 | 会让未处置款项离开经营视图；只有外部退款/冲正证据才允许结案 |

## 13. 本切片不做的事

- 历史设计冻结切片未新增或修改迁移、Go/Node 代码、OpenAPI 合同、activation 行或执行路径；当前实现范围见本文开头说明。
- 修复两条既有 BILL 导入路径遗漏的 REFUND 参考列；不更改正常支付、退款、拒付、应收与钱包账务语义。
- 设计冻结不等于实现或激活；GSA-34 未交付前该命令保持不可执行。

## 14. 自检与验证

- 每条事实可回溯到 §1/§2 的迁移与源码路径；“内部无可归属事实”由 `payment_order_id IS NULL` 与比对语义共同证明。
- 首版范围与版本复用决策与用户 2026-09-19 确认一致；零账务效果是显式冻结的结论，并在 §12 记录被拒方案。
- 变更范围仅本文件与任务登记；运行 `node scripts/check-account-platform-tasks.mjs`、`node scripts/account-platform-migration-manifest.mjs check`、`git diff --check`，不使用 SKIP 作为通过证据。

## 15. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、外部处置证据与复核规则、零账务效果守恒证明、幂等与未知结果、失败分类、迁移回滚、验收映射与被拒方案。
