# 当前实现与本轮修复

本轮追加 Billing `900020_reconciliation_activation_execution_lock`，固定锁定本命令 v1/s1 激活行。新结案在同一事务内取得 owner 提供的 `FOR SHARE` 锁并核对冻结激活版本；operator 停用须等待已取得该锁的事务结束。原不可变回执优先只读重放，不因后续停用失效。运行身份只获私有 schema USAGE 和无参数函数 EXECUTE，不获 activation UPDATE、schema CREATE 或 owner 身份；新迁移明确拒绝 Down。运维停止通过 activation `INACTIVE`，不删除既有案件、回执或账务事实。

`reconciliation_repair_cases` 收尾保留 SELECT/INSERT 并撤销 UPDATE/DELETE（#1720）；原 `000072` 不可变触发器和已发布 Up 字节保持。执行键使用明确的通用 legacy 兼容 helper，保留历史 `LEDGER_REPAIR` JSON wire 与摘要；命令、案件和审批上下文隔离依靠实际绑定校验，不宣称摘要算法增强隔离（#1678）。

跨域装配改用官方 owned UUID Account/Billing 安装夹具（#1721），保持原角色、安装与清理保护。受控 HTTP proof/relay/registered consumer 验证采用种子后完整财务行 JSONB 对照，覆盖三类付款表并保留案件、手审、版本和同 event 回放判据。原 actor 前置失败及钱包/包基线假设失败均保留；这些装配验证不等同部署真实 issuer/NATS 全链验收，#1683 仍待后续证据。前序 F14 readiness 首次失败原因仍未知，#1719 不因本轮通过而关闭。

本波安装 `RECONCILIATION_FINANCIAL_REPAIR` v1/s1：独立三语申请表、原意编辑与新鲜预检、Account closed policy/encoding/presenter/recovery、Portal BFF、Billing 权威 proof/claim/执行、专属 consumer 与精确 NATS ACL。只处理 `PROVIDER_PAID_INTERNAL_MISSING` 的零账务效果结案；不推断内部订单、用户或钱包归属。激活默认 `INACTIVE`，消费者资源与实时 activation 门禁继续生效。

本轮修复在结案事务内推进关联手审单及案件 `version`，按渠道/业务日期跨 run 复核同商户单号的未结差异，校验退款 BILL 与 run 的 provider 身份及持久 payload。既有两条 BILL 导入路径仅在真实 REFUND 条目上保存其退款参考号，不改变支付/拒付参考语义。原 Billing `000072/000073`、Account `000076` 及历史迁移身份不改。

验证由本轮 PR 正文及固定提交证据记录说明；有限 owner PG、受控 React 与默认套件分别披露。完整 Account/outbox/relay/真实 issuer/资源/OS E2E、部署和生产激活未执行。下文“历史设计阶段”保留原阶段记录，当前行为以本节及修正后的事实描述为准。

# 对账财务修复（`RECONCILIATION_FINANCIAL_REPAIR`）受控激活手册

状态：能力已就绪，**默认未激活**。本手册只描述受控激活（把 Billing 逐命令 activation 行从 `INACTIVE` 翻到 `ACTIVE`）的操作与证据要求；是否执行必须另有授权，且 GSA-38～40 的切换、回滚与发布门禁全部通过后才能开始。

## 1. 前置条件（全部满足才可开始）

1. 目标 Commit 已完成 GSA-38（切换/回滚工具、资源清单与只读 readiness）、GSA-39（生产等价集成、性能、N/N-1 与故障演练）与 GSA-40（逐命令额度与生产证据归档）中适用于本命令的项，并有独立审核记录。
2. 用户/授权人已就该命令的生产激活给出明确授权（书面记录存档，含时间与范围）。首版范围只含 **`PROVIDER_PAID_INTERNAL_MISSING` 差异的零账务效果结案**；任何需要账务效果的解释（补记入账、新账户类型、人工指定归属）都必须新增账户类型、policy/schema 版本与独立设计。
3. 专属 durable `billing-reconciliation-financial-repair-executor-p1-v1` 的受信 cutover 资源绑定已就绪。当前 Billing 启动从 `approvalconsumer.BillingCommands()` 的已安装目录注册本命令，按通用 `BILLING_APPROVAL_CONSUMER_RESOURCES_UNAVAILABLE` 资源守卫失败关闭；不使用历史开发开关 `BILLING_RECONCILIATION_FINANCIAL_REPAIR_CONSUMER_ENABLED`，该环境变量不能作为激活或执行依据。
4. 已验证 Account 侧四入口读取实时 Billing activation：行仍为 `INACTIVE` 时 create/update/submit/APPROVE 全部失败关闭（`ErrBillingGovernanceInactive`）。
5. 已完成一致性备份（含 Billing 库）并记录备份 ID、完成时间与 SHA-256；激活窗口内停止写入新审批。

## 2. 只读核验（记录输出作为证据）

```sql
-- Billing：确认 v1/s1 行仍在且尚未激活
SELECT command_type, policy_version, snapshot_schema_version, state, version, updated_at
FROM billing_approval_command_activations
WHERE command_type = 'RECONCILIATION_FINANCIAL_REPAIR' AND policy_version = 1 AND snapshot_schema_version = 1;

-- Billing：确认没有该命令的审批联动事实
SELECT (SELECT count(*) FROM reconciliation_repair_cases WHERE approval_request_id IS NOT NULL) AS approval_cases,
       (SELECT count(*) FROM billing_approval_proof_claims) AS proof_claims;

-- Billing：存量清单——“Provider 已收款、内部无归属”且已有 Provider 侧退款/冲正证据的差异
-- （in_scope=true 的差异才在首版范围内；复核门禁要求同一商户单号在同渠道/业务日期的全部 run 不再有其它 OPEN/IN_REVIEW 差异。）
SELECT reconciliation_case.id,
       reconciliation_case.reconciliation_run_id,
       reconciliation_run.channel,
       reconciliation_run.business_date,
       reconciliation_case.merchant_order_no,
       (reconciliation_case.provider_snapshot->>'amount_fen')::bigint AS amount_fen,
       reconciliation_case.provider_snapshot->>'currency' AS currency,
       (SELECT count(*) FROM payment_orders payment_order
         WHERE payment_order.merchant_order_no = reconciliation_case.merchant_order_no) AS internal_orders,
       (SELECT count(*) FROM payment_transactions payment_transaction
         WHERE payment_transaction.provider_transaction_id = reconciliation_case.provider_snapshot->>'channel_transaction_id') AS internal_transactions,
       (SELECT count(*) FROM reconciliation_cases other
         JOIN reconciliation_runs other_run ON other_run.id = other.reconciliation_run_id
         WHERE other_run.channel = reconciliation_run.channel
           AND other_run.business_date = reconciliation_run.business_date
           AND other.merchant_order_no = reconciliation_case.merchant_order_no
           AND other.case_type = 'PROVIDER_PAID_INTERNAL_MISSING'
           AND other.status IN ('OPEN','IN_REVIEW') AND other.id <> reconciliation_case.id) AS recheck_open_cases,
       EXISTS(SELECT 1 FROM payment_evidence evidence
              WHERE evidence.evidence_type = 'BILL'
                AND evidence.provider_id = reconciliation_run.provider_id
                AND evidence.provider_version = reconciliation_run.provider_version
                AND evidence.merchant_order_no = reconciliation_case.merchant_order_no
                AND evidence.provider_transaction_id = reconciliation_case.provider_snapshot->>'channel_transaction_id'
                AND NULLIF(trim(evidence.provider_refund_id),'') IS NOT NULL
                AND upper(trim(evidence.provider_status)) IN ('SUCCESS','SUCCEEDED','REFUNDED','COMPLETED')
                AND upper(trim(evidence.payload_snapshot->>'entry_type')) = 'REFUND'
                AND evidence.amount_fen = (reconciliation_case.provider_snapshot->>'amount_fen')::bigint
                AND evidence.currency = reconciliation_case.provider_snapshot->>'currency') AS disposal_evidence
FROM reconciliation_cases reconciliation_case
JOIN reconciliation_runs reconciliation_run ON reconciliation_run.id = reconciliation_case.reconciliation_run_id
WHERE reconciliation_case.case_type = 'PROVIDER_PAID_INTERNAL_MISSING'
  AND reconciliation_case.status = 'OPEN'
  AND reconciliation_case.payment_order_id IS NULL
  AND reconciliation_run.status = 'COMPLETED'
  AND NOT EXISTS(SELECT 1 FROM reconciliation_repair_cases repair
                 WHERE repair.reconciliation_case_id = reconciliation_case.id);
```

上述 SQL 只作候选清单，不是权威 proof：最终还须由 owner 校验持久 REFUND payload 与列身份、状态、渠道、退款参考及金额逐项一致，绑定完整 frozen facts，不能用该清单代替 proof。

判据：只有 `internal_orders = 0`、`internal_transactions = 0`、`recheck_open_cases = 0` 且 `disposal_evidence` 为真时，该差异才是首版范围（`in_scope`）。`recheck_open_cases > 0` 说明同一商户单号仍有未处置差异，必须先按既有对账流程收敛；不得用本命令掩盖差异。

Account 侧核验：`approval_requests` 中该命令不得存在 `APPROVED`/`EXECUTING`/`EXECUTION_FAILED` 的遗留行；`outbox_events` 中不得存在未消费的该命令授权事件。

## 3. 激活操作（仅在授权与前置条件齐备后执行）

```sql
SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations
SET state = 'ACTIVE', version = version + 1
WHERE command_type = 'RECONCILIATION_FINANCIAL_REPAIR'
  AND policy_version = 1 AND snapshot_schema_version = 1 AND state = 'INACTIVE';
-- 必须恰好影响一行；触发器会写入不可改写 history 行。
```

冻结快照包含 activation 版本，因此**任何在激活前装配的请求都必须在激活后重新装配**。激活本身不产生任何财务效果，也不会调用 Provider。

## 4. 激活后验证（全部通过才可宣告激活完成）

1. Billing 投影：`GET /internal/v1/approval-command-activations` 返回该命令 `policy_version=1 / snapshot_schema_version=1 / state=ACTIVE`，其余命令状态不变，history 追加一行。
2. Account 读取：`billinggovernance` 客户端观测到 `ACTIVE`，四入口不再返回 `ErrBillingGovernanceInactive`。
3. Proof：以真实 Account 身份调用 Billing proof endpoint，对一件 `in_scope=true` 的差异返回 `PROOF_VALID`/`ALLOW` 且投影含 `ledger_effect=NONE`；对仍有内部归属、缺处置证据、run 非 `COMPLETED`、案件已结案或复核仍有 OPEN 差异的候选返回对应 DENY。
4. 消费者：durable `billing-reconciliation-financial-repair-executor-p1-v1` 处于消费状态，DLQ/重试计数为 0。
5. 受控演练：以一条一次性审批完成端到端执行，核对：
   - `reconciliation_cases` 由 `OPEN` 推进为 `RESOLVED`、`resolution='PROVIDER_REFUND_CONFIRMED'`、`resolved_at` 非空，`updated_at`、`version` 正常推进，其余来源字段逐字段不变；关联手审单同事务 `RESOLVED` 并追加结案事件；
   - 写入一条不可改写的 `reconciliation_repair_cases` 行（含审批联动列与 `recheck_case_count=0`）；
   - **零账务效果**：`journal_entries`/`ledger_postings`/`receivables`/`wallets`/`entitlement_packages`/`entitlement_components`/`credit_lots`/`payment_orders`/`payment_transactions`/`payment_evidence` 逐行一致（演练前后计数与钱包 `version` 均不变）；
   - result-first 幂等重放不产生第二条 case；
   - 登记真实 ID、审批/授权事件 ID 与 Provider 调用计数为 0 的观测。

## 5. 回滚

1. 停止该 durable 消费者。
2. 用 `billing_schema_owner` 把 v1/s1 行翻回 `INACTIVE`（`version = version + 1`）；history 追加而非改写。
3. 验证 Account 四入口重新失败关闭；已结案的差异与 case 行**保留可读**，禁止破坏性 down，也禁止把案件重开为 `OPEN`。
4. 仍在 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓，不得盲目重放。

## 6. 记录

- 2026-09-20 代理：新建本手册，落地“能力就绪但默认未激活”；激活必须由授权人按上表执行，未执行任何生产激活。
