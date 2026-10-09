# 拒付财务修复（`CHARGEBACK_FINANCIAL_REPAIR`）受控激活手册

状态：能力已就绪，**默认未激活**。本手册只描述受控激活（把 Billing 逐命令 activation 行从 `INACTIVE` 翻到 `ACTIVE`）的操作与证据要求；是否执行必须另有授权，且 GSA-38～40 的切换、回滚与发布门禁全部通过后才能开始。

## 1. 前置条件（全部满足才可开始）

1. 目标 Commit 已完成 GSA-38（切换/回滚工具、资源清单与只读 readiness）、GSA-39（生产等价集成、性能、N/N-1 与故障演练）与 GSA-40（逐命令额度与生产证据归档）中适用于本命令的项，并有独立审核记录。
2. 用户/授权人已就该命令的生产激活给出明确授权（书面记录存档，含时间与范围）。首版范围只含 **BASE 短缺补偿**；任何放宽（BONUS 缺口、部分撤销、借用退款规则）都必须另开 policy/schema 版本与独立设计。
3. 专属 durable `billing-chargeback-financial-repair-executor-p1-v1` 的受信 cutover 资源绑定已就绪。当前 `cmd/billing-service` 通过 `approvalconsumer.BillingCommands()` 的有限已安装集合注册消费者；不存在本命令专属 `BILLING_CHARGEBACK_FINANCIAL_REPAIR_CONSUMER_ENABLED` 开关。既有六个 `BILLING_APPROVAL_CONSUMER_*` 受信工件输入缺失、摘要不符或读取失败均失败关闭，不能以未校验资源启动消费者。
4. 已验证 Account 侧五入口读取实时 Billing activation：行仍为 `INACTIVE` 时 create/update/submit/APPROVE/Begin 全部失败关闭（`ErrBillingGovernanceInactive`）。
5. 已完成一致性备份（含 Billing 库）并记录备份 ID、完成时间与 SHA-256；激活窗口内停止写入新审批。

## 2. 只读核验（记录输出作为证据）

以只读身份执行并归档：

```sql
-- Billing：确认 v1/s1 行仍在且尚未激活
SELECT command_type, policy_version, snapshot_schema_version, state, version, updated_at
FROM billing_approval_command_activations
WHERE command_type = 'CHARGEBACK_FINANCIAL_REPAIR' AND policy_version = 1 AND snapshot_schema_version = 1;

-- Billing：确认没有该命令的审批联动事实
SELECT (SELECT count(*) FROM chargeback_financial_repair_cases WHERE approval_request_id IS NOT NULL) AS approval_cases,
       (SELECT count(*) FROM billing_approval_proof_claims) AS proof_claims;

-- Billing：存量清单——“已确认拒付、但仍留有可撤销额度”的候选会走既有 CONFIRM_CHARGEBACK 路径，
-- 只有 in_scope=true（全部组件可撤销额度为 0）的案件才在首版范围内。
SELECT chargeback.id,
       chargeback.entitlement_package_id,
       chargeback.provider_chargeback_id,
       (SELECT COALESCE(sum(component.available_microcredits + component.active_grant_microcredits + component.suspended_grant_microcredits), 0)
          FROM entitlement_components component WHERE component.package_id = chargeback.entitlement_package_id) AS revocable_microcredits,
       (SELECT COALESCE(sum(component.original_microcredits - component.reclassified_out_microcredits), 0)
          FROM entitlement_components component
         WHERE component.package_id = chargeback.entitlement_package_id AND component.component_type = 'BASE') AS base_shortfall_microcredits,
       EXISTS(SELECT 1 FROM payment_evidence evidence
              WHERE evidence.id = chargeback.payment_evidence_id
                AND evidence.evidence_type = 'CHARGEBACK'
                AND evidence.provider_status = 'CHARGEBACK_CONFIRMED'
                AND evidence.evidence_key = chargeback.provider_chargeback_id) AS evidence_ok,
       EXISTS(SELECT 1 FROM payment_transactions transaction
              WHERE transaction.id = chargeback.payment_transaction_id
                AND transaction.status IN ('PAID','LATE_PAID')) AS transaction_ok,
       EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly
              WHERE anomaly.chargeback_case_id = chargeback.id
                AND anomaly.anomaly_type = 'CHARGEBACK'
                AND anomaly.financial_action_status IN ('PENDING','APPLIED')) AS anomaly_ok,
       NOT EXISTS(SELECT 1 FROM journal_entries journal
                  WHERE journal.business_reference = 'chargeback-repair:' || chargeback.id::text) AS journal_clear,
       NOT EXISTS(SELECT 1 FROM receivables receivable
                  WHERE receivable.source_type = 'CHARGEBACK'
                    AND receivable.source_reference = 'chargeback-repair:' || chargeback.id::text) AS receivable_clear,
       NOT EXISTS(SELECT 1 FROM refunds refund
                  WHERE refund.entitlement_package_id = chargeback.entitlement_package_id) AS refund_clear,
       NOT EXISTS(SELECT 1 FROM forced_refund_cases forced
                  WHERE forced.entitlement_package_id = chargeback.entitlement_package_id) AS forced_refund_clear
FROM chargeback_cases chargeback
JOIN wallets wallet ON wallet.id = chargeback.wallet_id
WHERE chargeback.status = 'REVIEW_REQUIRED'
  AND chargeback.revoked_base_microcredits = 0 AND chargeback.revoked_bonus_microcredits = 0
  AND chargeback.receivable_id IS NULL
  AND wallet.status = 'ACTIVE'
  AND NOT EXISTS(SELECT 1 FROM chargeback_financial_repair_cases repair WHERE repair.chargeback_case_id = chargeback.id);
```

逐条判据：只有 `revocable_microcredits = 0`、`base_shortfall_microcredits > 0` 且 `evidence_ok`/`transaction_ok`/`anomaly_ok`/四个 `*_clear` 全部为真时，该案件才是首版范围（`in_scope`）。**存在任何可撤销额度（`revocable_microcredits > 0`）的案件必须走既有 `CONFIRM_CHARGEBACK` 收口**，不得用本命令部分补偿；BONUS 侧在任何情况下都不参与本命令。

Account 侧核验：`approval_requests` 中该命令不得存在 `APPROVED`/`EXECUTING`/`EXECUTION_FAILED` 的遗留行；`outbox_events` 中不得存在未消费的该命令授权事件。任一不满足即阻断激活，并按设计 §8 失败分类只读查证。

## 3. 激活操作（仅在授权与前置条件齐备后执行）

激活必须由持有 `billing_schema_owner` 的部署身份执行，且只允许对该命令的 **v1/s1 行**做一次状态推进；不得新增行、不得改写任何既有迁移脚本或 `migrations/manifest.json`：

```sql
SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations
SET state = 'ACTIVE', version = version + 1
WHERE command_type = 'CHARGEBACK_FINANCIAL_REPAIR'
  AND policy_version = 1 AND snapshot_schema_version = 1 AND state = 'INACTIVE';
-- 必须恰好影响一行；触发器会写入不可改写 history 行。
```

注意：冻结快照包含 activation 版本，因此**任何在激活前装配的请求都必须在激活后重新装配**；不得把激活施加到已冻结的请求上。Guard 会拒绝身份/版本之外的改写；激活本身不产生任何财务效果，也不会调用 Provider。

## 4. 激活后验证（全部通过才可宣告激活完成）

1. Billing 投影：`GET /internal/v1/approval-command-activations` 返回该命令 `policy_version=1 / snapshot_schema_version=1 / state=ACTIVE`，其余命令状态不变，`billing_approval_command_activation_history` 追加一行且旧行未被改写。
2. Account 读取：`billinggovernance` 客户端观测到 `ACTIVE`，五入口不再返回 `ErrBillingGovernanceInactive`。
3. Proof：以真实 Account 身份调用 Billing proof endpoint，对一件存量清单内（`in_scope=true`）的案件返回 `PROOF_VALID`/`ALLOW`；对仍可撤销额度、证据非 `CHARGEBACK_CONFIRMED`、交易非 `PAID/LATE_PAID`、案件已 `CONFIRMED/DISMISSED`、`B<=0`、钱包非 `ACTIVE` 或存在 `chargeback-repair:` journal / 应收 / 修复 case 的候选返回对应 DENY。proof 本身不得触发任何 Provider 请求。
4. 消费者：durable `billing-chargeback-financial-repair-executor-p1-v1` 处于消费状态（Q+1 起始序与收据一致），DLQ/重试计数为 0。
5. 受控演练：以一条一次性审批完成端到端执行，核对：
   - `chargeback_cases` 由 `REVIEW_REQUIRED` 推进为 `CONFIRMED`，`revoked_base/bonus_microcredits` 保持 0、`resolution_code='CONFIRM_CHARGEBACK_SHORTFALL'`、`version+1`；
   - 生成 `CHARGEBACK` 应收（`original=outstanding=B`）与钱包 `receivable_microcredits/financial_status/financial_freeze_reason` 投影；
   - 一笔两腿平衡的 `CHARGEBACK` journal（引用 `chargeback-repair:<case_id>`：`RECEIVABLE_ASSET -B`、`CHARGEBACK_CLEARING +B`）；
   - 组件、lot、`wallets.posted_microcredits` 与 BONUS 侧**逐行零变化**；
   - `payment_anomaly_cases` 由 `PENDING` 推进为 `APPLIED`（version CAS），并写入不可改写的 `chargeback_financial_repair_cases` 行（含审批联动列）；
   - result-first 幂等重放不产生第二条 case/journal/应收；
   - 在演练记录中登记真实 ID、审批/授权事件 ID 与 Provider 调用计数为 0 的观测。

将上述输出归档到发布证据（脱敏，不含 Secret/Token/Cookie/个人信息与 Provider 原文）。

## 5. 回滚

1. 停止该 durable 消费者（或回滚到不消费该 durable 的镜像），使新事件不再执行。
2. 用 `billing_schema_owner` 把 v1/s1 行翻回 `INACTIVE`（`version = version + 1`）；history 追加而非改写。
3. 验证 Account 五入口重新失败关闭、proof 返回不可用；已执行的补偿事实**不回滚**（案件、journal、应收与审计行一律保留可读）。
4. 仍在 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓，不得盲目重放。

## 6. 记录

- 2026-09-20 代理：新建本手册，落地“能力就绪但默认未激活”；激活必须由授权人按上表执行，未执行任何生产激活。
