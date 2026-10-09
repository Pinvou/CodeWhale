# 拒付财务修复（`CHARGEBACK_FINANCIAL_REPAIR`）受控激活手册

## 本轮实现与验证边界

独立复验曾在 Account relay 就绪阶段失败，尚未进入 Billing 装配、proof、consumer 或财务执行，具体原因没有被原固定错误消息保留下来。后续带安全诊断的隔离运行通过，仅说明该次未重现，不能证明偶发根因已修。F14 专属测试现在可输出闭合阶段、SQLSTATE 和固定错误分类；共享默认装配及公开错误仍脱敏，原 5 秒就绪预算与全部权限判据不变。此未定位风险由 Issue #1719 保留，即使随后正式独立测试通过也不自动视为根因收尾。

前向迁移 `900019_chargeback_activation_execution_lock` 提供无参数、固定 v1/s1 的 owner 锁接口。新财务事务在已有不可变回执只读核对之后，通过该接口锁住 activation 行并核对 ACTIVE、版本与时间；部署身份的停用更新必须等待正在执行的财务事务提交。运行身份只持有私有 schema USAGE 和函数 EXECUTE，不能修改 activation 或成为 owner。900019 的 Down 显式返回 55000；停止消费者及切换 INACTIVE 是操作回退，不能删除已执行事实。

本轮隔离 PostgreSQL 验证包含未激活及旧版本零写、真实停用等待、停用后原回执只读重放、默认 ACL 注入回收、错误身份拒绝、未来迁移成功及失败后的权限保持。原 070 的不可变触发器保留；收尾撤回纠正事实表的 UPDATE/DELETE，保留 SELECT/INSERT，关联 Issue #1716。固定历史 execution-key 摘要保持原 wire 字节，通用 helper 命名不增强 hash 隔离；跨命令隔离依赖原审批、目标和 proof claim 绑定，Issue #1678 尚待后继波次完成。

跨域验收使用自有 UUID 数据库、正式 Account/Billing 角色、真实 HTTP proof 和注册消费者，外部依赖受控；这不代表部署 NATS、生产激活或生产端到端演练已经通过。旧环境夹具的角色前置失败、旧钱包占位快照导致的 proof 拒绝，以及新增测试接口名和冻结投影不完整的失败均保留在私有验收记录，不作为生产缺陷或通过证据。跨域装配的可复验性修复关联 Issue #1717；其收尾仍须独立验证和正常推送。

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
-- 以下只读 SQL 仅生成候选排查清单，不是权威 owner precheck 或激活准入证明。
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

候选筛选：只有 `revocable_microcredits = 0`、`base_shortfall_microcredits > 0` 且 `evidence_ok`/`transaction_ok`/`anomaly_ok`/四个 `*_clear` 全部为真时，该案件才通过此初筛；它仍须通过正式 owner 预检和 proof 的完整身份、关联、版本、零事实与可撤销量检查。**存在任何可撤销额度（`revocable_microcredits > 0`）的案件必须走既有 `CONFIRM_CHARGEBACK` 收口**，不得用本命令部分补偿；BONUS 侧在任何情况下都不参与本命令。

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
3. Proof：以真实 Account 身份调用 Billing proof endpoint，对一件候选清单中且经完整 owner 预检确认的案件返回 `PROOF_VALID`/`ALLOW`；对仍可撤销额度、证据非 `CHARGEBACK_CONFIRMED`、交易非 `PAID/LATE_PAID`、案件已 `CONFIRMED/DISMISSED`、`B<=0`、钱包非 `ACTIVE` 或存在 `chargeback-repair:` journal / 应收 / 修复 case 的候选返回对应 DENY。proof 本身不得触发任何 Provider 请求。
4. 消费者：durable `billing-chargeback-financial-repair-executor-p1-v1` 处于消费状态（Q+1 起始序与收据一致），DLQ/重试计数为 0。
5. 受控演练：以一条一次性审批完成端到端执行，核对：
   - `chargeback_cases` 由 `REVIEW_REQUIRED` 推进为 `CONFIRMED`，`revoked_base/bonus_microcredits` 保持 0、`resolution_code='CONFIRM_CHARGEBACK_SHORTFALL'`、`version+1`；
   - 生成 `CHARGEBACK` 应收（`original=outstanding=B`）与钱包 `receivable_microcredits/financial_status/financial_freeze_reason` 投影；
   - 一笔两腿平衡的 `CHARGEBACK` journal（引用 `chargeback-repair:<case_id>`：`RECEIVABLE_ASSET -B`、`CHARGEBACK_CLEARING +B`）；
   - 组件、lot、`wallets.posted_microcredits` 与 BONUS 侧**逐行零变化**；
   - `payment_anomaly_cases` 由 `PENDING` 推进为 `APPLIED`（version CAS），并写入不可改写的 `chargeback_financial_repair_cases` 行（含审批联动列）；
   - 同事务重算订单 `chargeback_summary/attention_summary` 并推进 `summary_version`，与案件 CONFIRMED 与新应收一致；
   - result-first 按原审批、授权事件、epoch、execution key 和目标案件核对收据，幂等重放不产生第二条 case/journal/应收；
   - 在演练记录中登记真实 ID、审批/授权事件 ID 与 Provider 调用计数为 0 的观测。

将上述输出归档到发布证据（脱敏，不含 Secret/Token/Cookie/个人信息与 Provider 原文）。

## 5. 回滚

1. 停止该 durable 消费者（或回滚到不消费该 durable 的镜像），使新事件不再执行。
2. 用 `billing_schema_owner` 把 v1/s1 行翻回 `INACTIVE`（`version = version + 1`）；history 追加而非改写。
3. 验证 Account 五入口重新失败关闭、proof 返回不可用；已执行的补偿事实**不回滚**（案件、journal、应收与审计行一律保留可读）。
4. 仍在 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓，不得盲目重放。

## 6. 记录

- 2026-09-20 代理：新建本手册，落地“能力就绪但默认未激活”；激活必须由授权人按上表执行，未执行任何生产激活。
