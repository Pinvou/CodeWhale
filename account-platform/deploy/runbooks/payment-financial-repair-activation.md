# 支付财务修复（`PAYMENT_FINANCIAL_REPAIR`）受控激活手册

状态：能力已就绪、**默认未激活**。本手册只描述受控激活（把 Billing 逐命令 activation 行从 `INACTIVE` 翻到 `ACTIVE`）的操作与证据要求；是否执行必须另有授权，且 GSA-38～40 的切换、回滚与发布门禁全部通过后才能开始。

## 1. 前置条件（全部满足才可开始）

1. 目标 Commit 已完成 GSA-38（切换/回滚工具、资源清单与只读 readiness）、GSA-39（生产等价集成、性能、N/N-1 与故障演练）与 GSA-40（逐命令额度与生产证据归档）中适用于本命令的项，并有独立审核记录。
2. 用户/授权人已就该命令的生产激活给出明确授权（书面记录存档，含时间与范围）。
3. 专属 durable `billing-payment-financial-repair-executor-p1-v1` 的受信 cutover 资源绑定已就绪，Billing 以 `BILLING_PAYMENT_FINANCIAL_REPAIR_CONSUMER_ENABLED=true` 启动，且启动时不出现 `BILLING_PAYMENT_FINANCIAL_REPAIR_CONSUMER_RESOURCES_UNAVAILABLE`（六个受信收据输入缺项、摘要不符或读取失败都会硬失败关闭）。
4. 已验证 Account 侧四入口读取实时 Billing activation：行仍为 `INACTIVE` 时 create/update/submit/APPROVE/Begin 全部失败关闭（`ErrBillingGovernanceInactive`）。
5. 已完成一致性备份（含 Billing 库）并记录备份 ID、完成时间与 SHA-256；激活窗口内停止写入新审批。

## 2. 只读核验（记录输出作为证据）

以只读身份执行并归档：

```sql
-- Billing：确认 v1/s1 行仍在且尚未激活
SELECT command_type, policy_version, snapshot_schema_version, state, version, updated_at
FROM billing_approval_command_activations
WHERE command_type = 'PAYMENT_FINANCIAL_REPAIR' AND policy_version = 1 AND snapshot_schema_version = 1;

-- Billing：确认没有该命令的审批联动补记
SELECT (SELECT count(*) FROM payment_order_repairs WHERE approval_request_id IS NOT NULL) AS approval_repairs,
       (SELECT count(*) FROM billing_approval_proof_claims) AS proof_claims;

-- Billing：存量清单——"MANUAL_REVIEW 且已有匹配成功证据但内部未入账"的订单
-- （本命令只接受其中已有一条 REVIEW_REQUIRED 交易、其 provider_transaction_id 与证据一致者；
--  其余必须走既有 RepairReviewedOrder 运维路径，或在将来另开 policy/schema 版本处理）
SELECT payment_order.id,
       EXISTS(SELECT 1 FROM payment_transactions t WHERE t.payment_order_id=payment_order.id
              AND t.fulfillment_status='REVIEW_REQUIRED'
              AND t.provider_transaction_id=evidence.provider_transaction_id) AS in_scope
FROM payment_orders payment_order
JOIN payment_evidence evidence ON evidence.payment_order_id=payment_order.id
WHERE payment_order.status='MANUAL_REVIEW' AND payment_order.fulfillment_status='MANUAL_REVIEW'
  AND evidence.provider_transaction_id IS NOT NULL AND evidence.amount_fen=payment_order.amount_fen
  AND evidence.currency=payment_order.currency AND evidence.merge_result IN ('RECORDED','DUPLICATE','MERGED')
  AND NOT EXISTS(SELECT 1 FROM payment_transactions t WHERE t.payment_order_id=payment_order.id
                 AND (t.status IN ('PAID','LATE_PAID') OR t.fulfillment_status='CREDITED'))
  AND NOT EXISTS(SELECT 1 FROM entitlement_packages p WHERE p.payment_order_id=payment_order.id);
```

Account 侧核验：`approval_requests` 中该命令不得存在 `APPROVED`/`EXECUTING`/`EXECUTION_FAILED` 的遗留行；`outbox_events` 中不得存在未消费的该命令授权事件。任一不满足即阻断激活，并按设计 §8 失败分类只读查证。

## 3. 激活操作（仅在授权与前置条件齐备后执行）

激活必须由持有 `billing_schema_owner` 的部署身份执行，且只允许对该命令的 **v1/s1 行**做一次状态推进；不得新增行、不得改写任何既有迁移脚本或 `migrations/manifest.json`：

```sql
SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations
SET state = 'ACTIVE', version = version + 1
WHERE command_type = 'PAYMENT_FINANCIAL_REPAIR'
  AND policy_version = 1 AND snapshot_schema_version = 1 AND state = 'INACTIVE';
-- 必须恰好影响一行；触发器会写入不可变 history 行。
```

注意：冻结快照包含 activation 版本，因此**任何在激活前装配的请求都必须在激活后重新装配**；不得把激活施加到已冻结的请求上。Guard 会拒绝身份/版本之外的改写；激活本身不产生任何财务效果，也不会调用 Provider。

## 4. 激活后验证（全部通过才可宣告激活完成）

1. Billing 投影：`GET /internal/v1/approval-command-activations` 返回该命令 `policy_version=1 / snapshot_schema_version=1 / state=ACTIVE`，其余命令状态不变，`billing_approval_command_activation_history` 追加一行且旧行未被改写。
2. Account 读取：`billinggovernance` 客户端观测到 `ACTIVE`，四入口不再返回 `ErrBillingGovernanceInactive`。
3. Proof：以真实 Account 身份调用 Billing proof endpoint，对一件存量清单内（`in_scope=true`）的订单返回 `PROOF_VALID`/`ALLOW`；对非 `MANUAL_REVIEW`、已入账、证据不匹配、`merge_result='REVIEW_REQUIRED'`、存在退款/拒付或钱包非 ACTIVE 的候选项返回对应 DENY。proof 本身不得触发任何 Provider 请求。
4. 消费者：durable `billing-payment-financial-repair-executor-p1-v1` 处于消费状态（Q+1 起始序与收据一致），DLQ/重试计数为 0。
5. 受控演练：以一条一次性审批完成端到端执行，核对既有 `REVIEW_REQUIRED` 交易推进为已入账、订单 `PAID`/`CREDITED`、package/lot/component 与订单冻结额度一致、journal 为 `REPAIR_CREDIT`（引用 `repair-payment-transaction:`）且平衡、`payment_order_repairs` 追加一行含审批联动列与 `wallet_balance_before/after`、钱包 `posted_microcredits` 增量等于 `amount−appliedToReceivables`、未结应收按既有顺序清偿并写 `CASH_RECHARGE_BASE` 分配、result-first 幂等重放不产生第二条交易/package/补记；同时在演练记录中登记真实 ID、Provider 调用计数为 0 的观测与校验结果。

将上述输出归档到发布证据（脱敏，不含 Secret/Token/Cookie/个人信息与 Provider 原文）。

## 5. 回滚

1. 停止该 durable 消费者（或回滚到不消费该 durable 的镜像），使新事件不再执行。
2. 用 `billing_schema_owner` 把 v1/s1 行翻回 `INACTIVE`（`version = version + 1`）；history 追加而非改写。
3. 验证 Account 四入口重新失败关闭、proof 返回不可用；已执行的补记事实**不回滚**（交易、package、lot、journal 与审计行一律保留可读）。
4. 仍在 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓，不得盲目重放。

## 6. 记录

- 2026-09-19 代理：新建本手册，落地“能力就绪但默认未激活”；激活必须由授权人按上表执行，未执行任何生产激活。
