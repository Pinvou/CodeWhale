# 当前完整实现与审核边界

## 本轮资格围栏与回放边界

新前向迁移 `900021_usage_activation_execution_lock` 安装固定 Usage p1/schema1 的私有 owner 能力。新效果在同一财务事务中锁定 activation 行并校验冻结版本和时间；操作员停用须等待已持锁事务提交。运行身份只有 schema USAGE/function EXECUTE，不能自行 UPDATE activation、成为 owner 或在私有 schema CREATE。注入的其他默认 grantee 权限仅从受控对象收回。默认仍为 INACTIVE；Down 明确拒绝，不删除能力、业务事实或账本。

`usage_settlement_repair_cases` 的运行权限收尾保留 SELECT/INSERT、撤销 UPDATE/DELETE，原 074 不可变 statement trigger 和 SQL 字节不改（#1723）。跨域测试通过官方自有 UUID 数据库、真实角色及完整 Up/Verify 装配，替代旧固定私人环境；原 inactive 历史测试使用实际 Usage hold 和完整冻结投影，保留权限、proof 和消费者判据（#1724）。独立审核与实际运行结果以 PR 为准，本段不宣称生产已激活或部署真实 issuer/NATS 全链通过。

执行键调用采用 `legacyFinancialExecutionKeyDigestV1`，仍保留历史 opaque `LEDGER_REPAIR` 标签、TrimSpace、JSON 编码与摘要算法。固定历史样本与持久回执在停用后的只读回放用于兼容验收；不是生产历史数据实测，也不增强 hash 命令隔离。隔离仍由 request/event/epoch/command/target/frozen facts 的正式 claim 绑定负责（#1678）。未知资金和旧模糊状态不自动处置；禁止旧代际任务重新派发，合法在途回调及历史证据继续保留。

本波安装 `USAGE_SETTLEMENT_REPAIR` p1/schema1：当前第七类财务修复及独立三语表单、原身份 EDIT、新鲜预检、typed 冻结详情、Account closed policy/encoding/proof/claim/recovery、Billing 实际 owner dispatch、11 个 Billing 与 3 个 Token durable、Portal BFF 和精确 NATS ACL。默认 activation `INACTIVE`；未部署、生产激活或重发模型调用。

首版只处理 `LOSS_REVIEW`：hold 已 RELEASED，原释放原因 ENTITLEMENT_EXPIRED_UNCERTAIN，复核仍 REVIEW_REQUIRED。应结额由 hold 的冻结 pricing_snapshot 与 review 的真实顶层 input_tokens/output_tokens/cached_input_tokens 重算，核对 price_version_id、model、protocol、formula 身份及 claimed；不查询当前价格、不从人工金额构造事实。整数算法沿既有 Token Cost，支持原 legacy 三费率及已解析单位的分母/舍入，字段缺失、溢出或不一致失败关闭。

本轮保留 FROZEN/REVOKED/EXPIRED 的组件和包终态，锁后完整冻结事实、精确实时 activation 与非空 facts_digest 校验；钱包 posted 不足以承担分配计划时提前拒绝，保持原余额不足零效果行为，不增加新的不足余额应收拆分政策。匹配原 request/event/epoch/hold/usage/evidence/key 收据先只读恢复；只有新效果重检实时事实。

原 Account 000077、Billing 000074/000075/000076 up/down 与全部历史迁移身份保留。有限 PG、受控 React、默认套件和标准构建分别以 PR 实际证据为准；完整 issuer/NATS/Account/资源/OS E2E 仍暂缓，编译与条件跳过不是运行通过。下文历史设计中的 COMMITTED_SHORTFALL 不是当前执行能力，须未来独立 policy/schema 与持久修正事实。

# 用量结算修复（`USAGE_SETTLEMENT_REPAIR`）受控激活手册

状态：能力已就绪，**默认未激活**。首版范围是**未结算补记**（`evidence_kind=LOSS_REVIEW`）：hold 因额度过期被释放、但损失复核仍为 `REVIEW_REQUIRED` 的用量。少结算补记（`COMMITTED_SHORTFALL`）需要 Billing 目前并不持有的持久修正用量事实，已显式推迟到后续 policy/schema 版本，当前一律失败关闭。是否执行激活必须另有授权，且 GSA-38～40 的切换、回滚与发布门禁全部通过后才能开始。

## 1. 前置条件（全部满足才可开始）

1. 目标 Commit 已完成 GSA-38/39/40 中适用于本命令的项并有独立审核记录。
2. 用户/授权人已就该命令的生产激活给出明确授权（书面存档，含时间与范围），且确认首版只补记 `LOSS_REVIEW` 存量。
3. 专属 durable `billing-usage-settlement-repair-executor-p1-v1` 的受信 cutover 资源绑定已就绪。当前 `cmd/billing-service` 通过 `approvalconsumer.BillingCommands()` 的有限已安装集合注册消费者；不存在本命令专属 `BILLING_USAGE_SETTLEMENT_REPAIR_CONSUMER_ENABLED` 开关。既有六个 `BILLING_APPROVAL_CONSUMER_*` 受信工件输入缺失、摘要不符或读取失败均失败关闭，不能以未校验资源启动消费者。
4. 已确认 Account 五入口在 `INACTIVE` 时全部失败关闭（`ErrBillingGovernanceInactive`）。
5. 一致性备份完成（含 Billing 库），记录备份 ID、时间与 SHA-256；激活窗口内停止写入新审批。

## 2. 只读核验（记录输出作为证据）

```sql
SELECT command_type, policy_version, snapshot_schema_version, state, version, updated_at
FROM billing_approval_command_activations
WHERE command_type = 'USAGE_SETTLEMENT_REPAIR' AND policy_version = 1 AND snapshot_schema_version = 1;

SELECT (SELECT count(*) FROM usage_settlement_repair_cases WHERE approval_request_id IS NOT NULL) AS approval_cases,
       (SELECT count(*) FROM billing_approval_proof_claims) AS proof_claims;

-- 存量：只读候选列表，不是in_scope或授权结论；必须通过实际owner冻结定价/用量/钱包和完整摘要检查
SELECT review.id,
       review.usage_request_id,
       hold.id AS hold_id,
       review.claimed_actual_microcredits,
       hold.max_microcredits,
       (SELECT COALESCE(sum(LEAST(allocation.reserved_microcredits, component.available_microcredits)),0)
          FROM usage_hold_allocations allocation
          JOIN entitlement_components component ON component.id = allocation.component_id
         WHERE allocation.hold_id = hold.id) AS settleable_microcredits,
       (SELECT COALESCE(sum(component.available_microcredits),0)
          FROM usage_hold_allocations allocation
          JOIN entitlement_components component ON component.id = allocation.component_id
         WHERE allocation.hold_id = hold.id) >= 0 AS components_readable
FROM usage_loss_reviews review
JOIN usage_holds hold ON hold.id = review.hold_id
WHERE review.status = 'REVIEW_REQUIRED'
  AND review.claimed_actual_microcredits > 0
  AND hold.status = 'RELEASED'
  AND hold.usage_snapshot->>'release_reason' = 'ENTITLEMENT_EXPIRED_UNCERTAIN'
  AND NOT EXISTS(SELECT 1 FROM usage_settlement_repair_cases repair WHERE repair.usage_request_id = review.usage_request_id);
```

上述 SQL 仅列候选，不负责重算或授权。权威 owner 必须由冻结定价及真实 token 用量重算 `A` 并与 claimed 一致，按原分配顺序计算 `S`；posted 不足 `S` 时失败关闭，不能用钱包 cap 改造分项或新增财务政策。`overage=A-S` 仅在大于零时形成 `USAGE_OVERAGE` 应收，**绝不能变成可用额度**。钱包非 ACTIVE、快照缺失/不符、claimed 非正、hold 非 RELEASED、复核非 REVIEW_REQUIRED 均拒绝。

Account 侧核验：`approval_requests` 中该命令不得存在 `APPROVED`/`EXECUTING`/`EXECUTION_FAILED` 遗留行；`outbox_events` 中不得存在未消费的该命令授权事件。

## 3. 激活操作（仅在授权与前置条件齐备后执行）

```sql
SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations
SET state = 'ACTIVE', version = version + 1
WHERE command_type = 'USAGE_SETTLEMENT_REPAIR'
  AND policy_version = 1 AND snapshot_schema_version = 1 AND state = 'INACTIVE';
-- 必须恰好影响一行。
```

冻结快照包含 activation 版本，激活前装配的请求必须在激活后重新装配。激活不产生任何财务效果，也不调用任何 Provider/adapter，更不重发模型请求。

## 4. 激活后验证（全部通过才可宣告完成）

1. Billing 投影返回该命令 v1/s1 `state=ACTIVE`，其余命令不变，history 追加一行。
2. Account 读取观测到 `ACTIVE`，五入口不再返回 `ErrBillingGovernanceInactive`。
3. 上述SQL不核定价/身份/完整事实，不能当作proof或直接执行依据。Proof：对owner检查合格的损失复核返回 `PROOF_VALID`/`ALLOW`；对 hold 非 `RELEASED`、复核非 `REVIEW_REQUIRED`、`claimed<=0`、钱包非 `ACTIVE` 或已有 `usage-settlement-repair:` 引用的候选返回对应 DENY。proof 不产生任何外部请求。
4. 消费者：durable `billing-usage-settlement-repair-executor-p1-v1` 处于消费状态，DLQ/重试计数为 0。
5. 受控演练（一次性审批端到端）：
   - 一条 `USAGE` journal，引用 `usage-settlement-repair:<usage_request_id>`，逐 lot 非零钱包腿 + USAGE_REVENUE 全额 A + 可选非零应收腿平衡；零 delta 腿省略，多 lot 不误称固定两/三腿，合计为 0；
   - `credit_lots.remaining` 与组件 `available/used` 按冻结分配顺序扣减，`usage_hold_allocations.settled + released = reserved` 仍成立；
   - 钱包 `posted_microcredits` 减少 `settled`（不得透支），`overage>0` 时形成 `USAGE_OVERAGE` 应收并同步钱包应收投影；
   - `usage_holds` 追加 `usage_snapshot->'settlement_repair'`（含审批引用、`expected/settled/overage`）且既有键不变；
   - `usage_loss_reviews` 由 `REVIEW_REQUIRED` 以 CAS 推进为 `RESOLVED_SETTLED`；
   - 幂等重放不新增第二条 case/journal；
   - 登记真实 ID 与「未调用任何 Provider/adapter、未新增 usage request」的观测。

## 5. 回滚

1. 停止该 durable 消费者。
2. 把 v1/s1 行翻回 `INACTIVE`（`version+1`）；history 追加而非改写。
3. 已产生的 case/journal/应收/回填与 `RESOLVED_SETTLED` **保留可读**；禁止破坏性 down，也禁止把复核退回 `REVIEW_REQUIRED`。
4. 仍在 5 分钟窗口内的 `APPROVED` 申请按 GSA-16 收殓，不得盲目重放。

## 6. 记录

- 2026-09-20 代理：新建本手册；首版只覆盖 `LOSS_REVIEW` 未结算补记，少结算补记待后续版本；未执行任何生产激活。
