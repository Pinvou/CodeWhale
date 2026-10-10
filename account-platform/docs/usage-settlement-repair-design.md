# 当前完整实现与审核边界

本波安装 `USAGE_SETTLEMENT_REPAIR` p1/schema1：当前第七类财务修复及独立三语表单、原身份 EDIT、新鲜预检、typed 冻结详情、Account closed policy/encoding/proof/claim/recovery、Billing 实际 owner dispatch、11 个 Billing 与 3 个 Token durable、Portal BFF 和精确 NATS ACL。默认 activation `INACTIVE`；未部署、生产激活或重发模型调用。

首版只处理 `LOSS_REVIEW`：hold 已 RELEASED，原释放原因 ENTITLEMENT_EXPIRED_UNCERTAIN，复核仍 REVIEW_REQUIRED。应结额由 hold 的冻结 pricing_snapshot 与 review 的真实顶层 input_tokens/output_tokens/cached_input_tokens 重算，核对 price_version_id、model、protocol、formula 身份及 claimed；不查询当前价格、不从人工金额构造事实。整数算法沿既有 Token Cost，支持原 legacy 三费率及已解析单位的分母/舍入，字段缺失、溢出或不一致失败关闭。

本轮保留 FROZEN/REVOKED/EXPIRED 的组件和包终态，锁后完整冻结事实、精确实时 activation 与非空 facts_digest 校验；钱包 posted 不足以承担分配计划时提前拒绝，保持原余额不足零效果行为，不增加新的不足余额应收拆分政策。匹配原 request/event/epoch/hold/usage/evidence/key 收据先只读恢复；只有新效果重检实时事实。

原 Account 000077、Billing 000074/000075/000076 up/down 与全部历史迁移身份保留。有限 PG、受控 React、默认套件和标准构建分别以 PR 实际证据为准；完整 issuer/NATS/Account/资源/OS E2E 仍暂缓，编译与条件跳过不是运行通过。下文历史设计中的 COMMITTED_SHORTFALL 不是当前执行能力，须未来独立 policy/schema 与持久修正事实。

# 用量结算修复（`USAGE_SETTLEMENT_REPAIR`）领域设计

状态：领域设计冻结；当前私有完整组合已实现 handler、closed proof、executor、durable consumer、typed presenter 与只读恢复，默认 activation 仍为 `INACTIVE`，未部署或激活。当前首版仅处理 `LOSS_REVIEW`：hold 已 `RELEASED` 且原释放原因是 `ENTITLEMENT_EXPIRED_UNCERTAIN`，复核仍为 `REVIEW_REQUIRED`。`COMMITTED_SHORTFALL` 所需持久修正事实未安装，当前一律拒绝；本文后续含该情形的原设计条款作为历史设计记录保留，不表示当前可执行能力。沿用 `platform.billing.repairs.*` 与 `policy_version=1 / snapshot_schema_version=1`，绝不重发模型请求。

## 1. 设计阶段基线（历史）

- 原设计阶段只有策略元数据，Billing activation 为 `INACTIVE`，尚无本命令可执行路径；这是历史基线，不代表当前完整组合的实现状态。
- 当前 Account create/update/submit/APPROVE/Begin 执行五入口沿用实时 activation 与既有授权、冻结快照和租约守卫；只读预检可在 `INACTIVE` 下准备，不能授权财务变更。实际消费注册来自 `approvalconsumer.BillingCommands()` 的有限已安装集合。


## 2. 用量结算现状与不变量（事实）

- **预留与分配**：`usage_holds`（`usage_request_id` 唯一、`wallet_id`、`price_version_id`、`status ∈ {ACTIVE,COMMITTED,RELEASED,EXPIRED,REVIEW}`、`max_microcredits>=0`（`000022` 已由 `>0` 放宽为 `>=0`）、`actual_microcredits>=0`、`usage_snapshot JSONB`、`expires_at`、`terminal_at`）与 `usage_hold_allocations`（`component_id` 非空、`reserved_microcredits>0`、`settled/released>=0`、`CHECK settled+released<=reserved`；主键由 `000012` 的 `(hold_id,package_id)` 在 `000016` 改为 `(hold_id,component_id)`）（`000002`、`000012`、`000016`、`000022`）。
- **零 delta 腿一律省略**：`ledger_postings.delta_microcredits<>0`（`000002`）是不可违反的既有限制，因此补结算时 `settled=0` 的钱包腿与 `overage=0` 的应收腿必须整条省略（`max_microcredits=0` 的 hold 因此只产生 `USAGE_REVENUE +M` 与 `RECEIVABLE_ASSET −M` 两条平台腿）。
- **结算落账**（`internal/billing/usage_holds.go` 的 `Commit`）：同 `usage_request_id` 且 `actual` 相同即幂等返回；`status='COMMITTED'` 之外的终态返回 `ErrUsageAlreadyTerminal`；`RELEASED && actual>0` 且 `usage_snapshot->>'release_reason'='ENTITLEMENT_EXPIRED_UNCERTAIN'` 时写 `usage_loss_reviews`（`reason_code`、`claimed_actual_microcredits`、`usage_snapshot`、`status='REVIEW_REQUIRED'`）并返回 `ErrUsagePlatformLossReview`。ACTIVE 路径：`backedActual=min(actual,max)`、`overage=actual-backedActual`；写 `USAGE` journal（`business_reference='usage:'||usage_request_id`）；按冻结分配顺序（组件过期时间、BONUS 优先、入账时间、ID）扣减 lot 与组件预留/已用、回填 `usage_hold_allocations`、投影 package 状态；`overage>0` 写 `RECEIVABLE_ASSET -overage` 并创建 `source_type='USAGE_OVERAGE'` 且 `source_reference=usage_request_id` 的应收；`USAGE_REVENUE +actual`；钱包 `posted_microcredits-=backedActual`、`reserved_microcredits-=max`、`version+1`；hold 置 `COMMITTED` 并记录 `actual`/`usage_snapshot`。
- **损失复核模型**：`usage_loss_reviews`（`hold_id` 唯一、`reason_code` 仅 `ENTITLEMENT_EXPIRED_UNCERTAIN`、`claimed_actual_microcredits`、`usage_snapshot`、`status ∈ {REVIEW_REQUIRED,RESOLVED_PLATFORM_LOSS}`、`updated_at`）——既有终态只表达“平台吸收损失”，没有“已补结算”收口（`000016`）。
- **不变量**：journal/posting 不可改写且必须平衡、`business_reference` 全局唯一；钱包 `posted_microcredits>=0`、lot 上下界、组件守恒；`usage_hold_allocations` 的 `settled+released<=reserved`。

## 3. 与相邻流程的职责边界（避免重复建设）

- 正常预留在 `ACTIVE` 时由既有 `Commit` 结算；本命令只处理 `Commit` 已无法覆盖的两类**既有事实**：①hold 已 `RELEASED`（`ENTITLEMENT_EXPIRED_UNCERTAIN`）但存在已发生用量 → `usage_loss_reviews` `REVIEW_REQUIRED`（未结算）；②hold 已 `COMMITTED` 但存在冻结的修正用量证据表明少结算。
- **绝不重发模型请求**：不调用任何 Provider/adapter、不重放推理请求、不新增 usage request；只读既有持久用量与定价事实。
- **相邻既有路径不改道**：管理端 `BILLING_USAGE_HOLD_SAFE_RELEASE`（`AUDIT_ONLY`，`internal/billing/service.go`）保持既有“安全释放”语义，本命令不得借用它、不得把它升级为结算修复入口；组织生命周期所消费的 `usage_loss_reviews` 阻塞投影在本命令推进为 `RESOLVED_SETTLED` 后按既有读取规则自然解除，不改其读取合同。
- 组成/归属错配属 GSA-21；支付、退款、拒付、应收核销分别属 GSA-27/29/31/25；本命令只处理消费侧结算差异。

## 4. 首版范围与精确允许/禁止的变化（冻结）

**允许的领域变化**（单次执行恰好产生，同一 Billing Serializable 事务内）：

1. 一条 `USAGE` journal：`business_reference='usage-settlement-repair:'||usage_request_id`，postings = 逐 lot 钱包负债账户 `-settled`（带 lot）、`USAGE_REVENUE +M`、`overage>0` 时 `RECEIVABLE_ASSET -overage`；`settled+overage=M` 且 journal 合计 0。
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
| 应结额 `A` | 由 hold 冻结的 `pricing_snapshot` 与 review.usage_snapshot 顶层三 token 数量按既有整数 Cost 算法重算；快照缺失、字段不全或与 `price_version_id` 不一致即 `PROOF_DENY` |
| 已结算额 `E` | case ② 取 `usage_holds.actual_microcredits`（`COMMITTED`）；case ① 取 `0`，并要求 hold 为 `RELEASED` 且 `usage_snapshot->>'release_reason'='ENTITLEMENT_EXPIRED_UNCERTAIN'` |
| 差额 `M` | `M=A-E`，必须 `>0`；若已有 `'usage-settlement-repair:'` 前缀的 journal/应收或既有 `usage_request_id` 应收，其未结部分计入 E，不得二次创建 |
| 修正用量证据（case ②） | 必须是与该 `usage_request_id` 绑定的持久核对事实（Token 用量记录/损失复核快照），不得是人工输入的数字 |
| 损失复核（case ①） | `usage_loss_reviews.status='REVIEW_REQUIRED'`，`claimed_actual_microcredits>=0`，review 的真实 token 用量按 hold 独立冻结的 pricing_snapshot 重算并与 claimed 一致；hold.usage_snapshot 仍保留释放原因，两个 JSON 不要求错误的逐字节相同 |
| 上限与余额 | `settled<=可用组件额度`、`overage=M-settled>=0`；钱包 `posted_microcredits>=settled`、`status='ACTIVE'` |

判据全部由 Billing 从持久事实重算；proof 不接受浏览器提供的用量、金额、组件或定价，执行期与 proof 均禁止任何外部调用。

## 6. closed command / snapshot / proof / presenter（冻结）

### 6.1 当前 normalized command（五字段 closed，整数为 JSON number）

`command_type`、policy/schema 位于审批 envelope；normalized 对象恰含 usage_request_id、hold_id、evidence_kind、loss_review_id、reason_code。预检恰含 command_type、target_resource_id（hold）、usage_request_id、loss_review_id、reason_code 五字段。无新增金额、定价或 expectedVersion 请求字段。

| 字段 | 类型 | 规则 |
|---|---|---|
| `usage_request_id` | uuid | 目标用量请求，必须 `usage_holds.usage_request_id` 命中 |
| `hold_id` | uuid | 必须与 `usage_request_id` 一致 |
| `evidence_kind` | string | 仅 `LOSS_REVIEW`；case ② 是后续设计 |
| `loss_review_id` | uuid | 当前首版必填 |
| `reason_code` | string | 唯一取值 `MISSING_USAGE_SETTLEMENT` |

### 6.2 critical / financial snapshot（复用 `snapshot_schema_version=1`）

| 键 | 内容 |
|---|---|
| `critical_resource_versions.hold_updated_at` / `.loss_review_updated_at` | 当前首版两个必填冻结时间戳（RFC3339Nano） |
| `critical_resource_versions.facts_digest` | 下述持久 JSONB（UseNumber 保留 bigint）转换后固定 encoding/json 的 SHA-256；审批 wire 的 RFC8785 摘要另算，不混称 |
| `critical_resource_versions.policy_version` | 1 |
| `financial_snapshot.expected_microcredits` | 本次应结额 `A` |
| `financial_snapshot.settled_microcredits` / `.settlement_microcredits` | 原 p1/schema1 owner 投影两个字段均为计划钱包结算 `S`，不是已经发生的扣款；理论已结额 `E=0` 不另加 wire 字段 |
| `financial_snapshot.overage_microcredits` | `A-S`，仅形成应收，不产生可用额度 |
| `financial_snapshot.wallet_id` / `.wallet_version` | 目标钱包与版本 |
| `financial_snapshot.allocations_digest` | 冻结的分配集合（package/component/lot/预留/已结算）摘要 |

`facts_digest` 覆盖 hold/loss_review/wallet 全 JSONB、全部 allocation/component/package/credit_lot 有序组成及状态/额度、冻结定价与真实用量、时间戳和 policy/schema/activation。既有 journal/应收/receipt 是否存在由原身份与重检门禁核对；不把仅有 SQL 判据说成已进入摘要。

### 6.3 proof 与 presenter

- Billing 内部证明端点只接受 Account 专属身份，2 秒绝对期限、64 KiB 上限，按 §5 重读并重算计划，通过既有 closed proof 响应返回 verdict、绑定及摘要。只读 owner 预检返回 normalized5/critical4/financial7 三个闭合对象，不能把计划字段误称为额外 proof 响应合同。执行期与 proof 均禁止任何外部调用。
- presenter 使用当前 11 字段 typed owner 摘要：`loss_review_id`、`hold_id`、`usage_request_id`、`wallet_id`、`evidence_kind`、`expected_microcredits`、`settled_microcredits`、`overage_microcredits`、`allocations_digest`、`facts_digest`、`reason_code`；这是本次冻结计划的摘要，不是完整七字段 financial snapshot，不下发 pricing/request 原始 JSON、提示词、Provider 原文或个人数据。

## 7. 幂等、状态推进与未知结果

### 7.1 新增不可变审计行（GSA-36 前向迁移）

`usage_settlement_repair_cases`（`billing_schema_owner` 持有，不可变触发器拒绝 UPDATE/DELETE；900003 最终 runtime 对该后加表实际有 UPDATE/DELETE privilege，有限 PG 已验证两项权限及写入拒绝，不能声称 ACL 双重拒绝。保留原迁移身份，不扩大权限）：`id`、`approval_request_id`（唯一）、`authorization_event_id`（唯一）、`approval_epoch`、`execution_key_digest`（唯一、`^[0-9a-f]{64}$`）、`usage_request_id`（唯一）、`hold_id`、`loss_review_id`（可空、唯一）、`wallet_id`、`evidence_kind`、`expected_microcredits`、`settled_microcredits`、`overage_microcredits`（`>=0`）、`allocations_digest`、`facts_digest`、`journal_entry_id`（唯一）、`receivable_id`（可空、唯一）、`state`、`created_at`。

### 7.2 损失复核状态推进（本命令引入的唯一新状态边）

- `usage_loss_reviews.status` 原两值 `REVIEW_REQUIRED`/`RESOLVED_PLATFORM_LOSS` 保留；本波既有 000075 前向迁移加入 `RESOLVED_SETTLED`，并以 `WHERE id=$1 AND status='REVIEW_REQUIRED' AND updated_at=$2` 的 CAS 推进（`updated_at=now`）。
- 既有 `RESOLVED_PLATFORM_LOSS` 语义（平台吸收损失）与既有写入者保持不变；本命令不改已吸收行。

### 7.3 幂等与未知结果

- 线性化点：`usage_settlement_repair_cases.usage_request_id` 唯一 + hold/loss-review 行锁与时间戳 CAS + journal `business_reference` 唯一 + 应收 `UNIQUE(source_type,source_reference)`；同一用量请求至多一次补结算。
- result-first：先严格比对原 request/event/epoch、hold/usage/loss-review 身份与 execution key 摘要，只读恢复已提交 case、journal/应收；匹配原收据即返回同一结果，不重新以可变 live facts 授权已提交事实。
- 未知结果：提交结果不明时只读查证 case、journal、应收、`usage_hold_allocations` 回填与损失复核状态是否同时一致；缺失或部分事实一律 `UNKNOWN_QUERY_ONLY`，禁止重放、禁止假定零效果。
- 新效果顺序：hold advisory → wallet 行锁 → hold/loss-review 行锁 → 有序 allocation/component/package/lot 行锁 → 重新校验精确 live activation/policy/schema/version及非空完整冻结事实、定价/用量 → 写同一事务的 journal/postings、lot/组件/配额、钱包/package、可选应收、allocations/hold、损失复核与不可变 case → 提交。不能以锁前快照代替锁后重检。

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
- activation 行保持 `INACTIVE`；消费者来自已安装命令目录和受信 cutover 工件，不存在本命令特有环境开关；生产激活只属于取得独立授权的 GSA-40。
- 若将来处理“多结算/重复结算冲销”或引入新的用量证据来源，必须新增 policy/schema 版本与独立设计。

## 10. 迁移、N/N-1 与回滚

- 迁移只前向追加：新增 `usage_settlement_repair_cases` 与 `usage_loss_reviews.status` 的 `RESOLVED_SETTLED` 取值（本波 Billing `000074/000075`；proof-claim allowlist 为 `000076`，Account durable 路由为 `000077`），guard 校验迁移身份与 schema owner 分离；不改 `000002/000012/000016/000054/000060` 既有列、CHECK 语义与历史行。
- N/N-1：先部署可读新表/新取值的 Billing，再部署 proof 端与执行器/消费者，最后业务页；既有 `Commit` 与损失复核写入者行为不变（不认识新取值时仍按既有两值读取，失败关闭而非误判）。
- 回滚：只允许停止消费者、把 activation 行翻回 `INACTIVE`；已产生的 case/journal/应收/回填与损失复核状态一律保留可读，禁止破坏性 down。
- 未通过 GSA-38～40 且未取得独立生产授权前，不部署、不激活。

## 11. 验收与证据映射（GSA-36 必须逐项交付）

| 验收点 | 期望证据 |
|---|---|
| 未结算补记（case ①） | 由 `usage_loss_reviews` + `RELEASED(ENTITLEMENT_EXPIRED_UNCERTAIN)` hold 重算 `A`，写 `USAGE` journal、扣减与回填、损失复核推进 `RESOLVED_SETTLED` |
| 少结算补记（历史 case ②） | 当前一律拒绝；后续版本才可验收，不以当前LOSS_REVIEW测试冒称覆盖 |
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

- 当前交付原 first-release LOSS_REVIEW 闭环及范围内正确性修复；不重写迁移、定价/用量事实、当前价格或已有 journal，不增加 COMMITTED_SHORTFALL。
- 不改动既有预留/释放/结算、Token 用量对账、`usage_loss_reviews` 既有吸收口径或支付/退款/拒付/应收的任何行为。
- 本波源码实现不等于外部审核、真实跨域验收或生产激活；默认仍 INACTIVE。

## 13. 自检与验证

- 每条事实可回溯到 §1/§2 的迁移与源码路径；`A/E/M` 的定义与既有 `Commit` 的 `backedActual/overage` 口径一致。
- 首版范围与版本复用决策与用户 2026-09-19 确认一致；`RESOLVED_SETTLED` 是显式登记并前向新增的取值，不假装既有能力已具备。
- 验证使用实际存在的项目统一构建、架构/版本/授权 guard、Go race/真实有限PG及受控React；命令与终态由当前PR记录，不引用不存在的历史 manifest/task 脚本，不使用 SKIP 或零用例编译作为通过。

## 14. 记录

- 2026-09-18 代理：登记事实与决策门禁首切片；范围未定。
- 2026-09-19 代理：按用户确认的首版范围冻结 closed command/snapshot/proof、补结算账务与守恒、损失复核新状态边、幂等与未知结果、失败分类、迁移回滚与验收映射。
