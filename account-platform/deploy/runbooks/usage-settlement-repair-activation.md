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

-- 存量：未结算的损失复核（in_scope 表示当前可由本命令补记）
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

判据：`claimable = claimed_actual_microcredits`；`settled = min(claimable, settleable_microcredits, wallets.posted_microcredits)`；`overage = claimable - settled` 只能在 `overage>0` 时形成 `USAGE_OVERAGE` 应收，**绝不能变成可用额度**。钱包非 `ACTIVE`、`claimed<=0`、hold 非 `RELEASED`、复核非 `REVIEW_REQUIRED` 的案件不得纳入。

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
3. Proof：对 `in_scope` 的损失复核返回 `PROOF_VALID`/`ALLOW`；对 hold 非 `RELEASED`、复核非 `REVIEW_REQUIRED`、`claimed<=0`、钱包非 `ACTIVE` 或已有 `usage-settlement-repair:` 引用的候选返回对应 DENY。proof 不产生任何外部请求。
4. 消费者：durable `billing-usage-settlement-repair-executor-p1-v1` 处于消费状态，DLQ/重试计数为 0。
5. 受控演练（一次性审批端到端）：
   - 一条 `USAGE` journal，引用 `usage-settlement-repair:<usage_request_id>`，两腿（或 `overage>0` 时三腿）平衡，合计为 0；
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
