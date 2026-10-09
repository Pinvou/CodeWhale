# 退款异常修复（`REFUND_EXCEPTION_REPAIR`）受控激活手册

本轮前向迁移 `900018_refund_activation_execution_lock` 提供固定 Refund v1/s1 的私有 owner 锁能力；先按正式 Billing Up/Verify 安装，再由已授权 operator 明确激活。默认仍为 INACTIVE。新写在同一财务事务内锁定 activation 并核对冻结版本与更新时间，停用必须等待该事务结束；已有不可变成功回执先只读回放，不因后续停用失效。不得给 runtime 表 UPDATE 或 owner 身份来绕过该接口。

900018 Down 显式拒绝，保留能力、迁移账本和财务事实。运维回退通过停止 consumer 并受控设置 activation 为 INACTIVE；不删除已执行 case、journal 或回执。原 067 Up 与不可变触发器保持原字节，runtime 仅保 SELECT/INSERT，收尾及未来安装不得重新授予 UPDATE/DELETE。执行键摘要采用历史兼容域（见 `docs/financial-execution-key-compatibility.md`），没有更换历史 hash，也不声称增强 hash 隔离。

状态：本轮候选实现已完成本机隔离角色和受控跨服务验收，**默认未激活**；这不是部署 NATS 或生产激活验证。本手册只描述受控激活（把 Billing 逐命令 activation 行从 `INACTIVE` 翻到 `ACTIVE`）的操作与证据要求；是否执行必须另有授权，且 GSA-38～40 的切换、回滚与发布门禁全部通过后才能开始。

本轮还修正了三项独立记录：#1712 收尾权限保留 SELECT/INSERT 并撤销 UPDATE/DELETE；#1713 将退款跨服务测试装配改为官方自有 UUID 数据库和真实角色，不再依赖固定开发环境；#1714 用已发布 F12 的 84 项迁移成员和校验和固定索引测试历史阶段，再正式升级当前完整迁移集合。索引观察仅适用于隔离测试数据，不是生产性能结论。原资格反例、权限断言失败及测试装配失败保留在验证记录中，修后测试成功不等于这些问题已在生产发生或生产已修复。

## 1. 前置条件（全部满足才可开始）

1. 目标 Commit 已完成 GSA-38（切换/回滚工具、资源清单与只读 readiness）、GSA-39（生产等价集成、性能、N/N-1 与故障演练）与 GSA-40（逐命令额度与生产证据归档）中适用于本命令的项，并有独立审核记录。
2. 用户/授权人已就该命令的生产激活给出明确授权（书面记录存档，含时间与范围）。
3. 专属 durable `billing-refund-exception-repair-executor-p1-v1` 的受信 cutover 资源绑定已就绪；当前统一 Billing approval consumer 按已安装的闭合 descriptor 注册。资源摘要、身份和收据输入必须通过现有启动校验；不存在本命令专属的旧布尔启用环境开关。
4. 已验证 Account 侧create/update/submit/APPROVE/Begin 五个门禁读取实时 Billing activation：行仍为 `INACTIVE` 时 create/update/submit/APPROVE 全部失败关闭（`ErrBillingGovernanceInactive`）。
5. 已完成一致性备份（含 Billing 库）并记录备份 ID、完成时间与 SHA-256；激活窗口内停止写入新审批。

## 2. 只读核验（记录输出作为证据）

以只读身份执行并归档：

```sql
-- Billing：确认 v1/s1 行仍在且尚未激活
SELECT command_type, policy_version, snapshot_schema_version, state, version, updated_at
FROM billing_approval_command_activations
WHERE command_type = 'REFUND_EXCEPTION_REPAIR' AND policy_version = 1 AND snapshot_schema_version = 1;

-- Billing：确认没有该命令的审批联动事实
SELECT (SELECT count(*) FROM refund_exception_repair_cases WHERE approval_request_id IS NOT NULL) AS approval_cases,
       (SELECT count(*) FROM billing_approval_proof_claims) AS proof_claims;

-- Billing：存量清单——“Provider 已退款、本地冲销缺失”的退款
-- in_scope 只表示候选证据匹配；不代表已通过全部 owner 判据。
-- 必须使用当前只读预检确认完整冻结事实，不能直接依据本查询审批或激活。
SELECT refund.id,
       refund.payment_order_id,
       refund.entitlement_package_id,
       refund.amount_fen,
       refund.credit_microcredits,
       EXISTS(SELECT 1 FROM payment_evidence evidence
              WHERE evidence.payment_order_id = refund.payment_order_id
                AND evidence.payment_transaction_id = refund.payment_transaction_id
                AND evidence.provider_refund_id = btrim(refund.channel_refund_id)
                AND evidence.amount_fen = refund.amount_fen
                AND evidence.currency = payment_order.currency
                AND evidence.provider_status IS NOT NULL AND btrim(evidence.provider_status) <> ''
                AND upper(btrim(evidence.provider_status)) NOT IN
                    ('FAILED','FAIL','REJECTED','CLOSED','PAY_CLOSED','CANCELLED','CANCELED','REFUND_FAILED','REFUND_CLOSED')) AS in_scope
FROM refunds refund
JOIN payment_orders payment_order ON payment_order.id = refund.payment_order_id
JOIN payment_anomaly_cases anomaly ON anomaly.dedupe_key = 'refund:' || refund.id::text || ':local-conflict'
WHERE refund.status = 'MANUAL_REVIEW'
  AND refund.channel_refund_id IS NOT NULL AND btrim(refund.channel_refund_id) <> ''
  AND anomaly.anomaly_type = 'REFUND_FAILURE' AND anomaly.status = 'RESOLVED'
  AND anomaly.resolution_code = 'ESCALATE_FINANCIAL_REPAIR' AND anomaly.financial_action_status = 'PENDING'
  AND NOT EXISTS(SELECT 1 FROM journal_entries journal
                 WHERE journal.business_reference = 'refund-repair:' || refund.id::text)
  AND NOT EXISTS(SELECT 1 FROM receivables receivable
                 WHERE receivable.source_type = 'FORCED_REFUND'
                   AND receivable.source_reference = 'refund-repair:' || refund.id::text)
  AND NOT EXISTS(SELECT 1 FROM refund_exception_repair_cases repair WHERE repair.refund_id = refund.id)
  AND NOT EXISTS(SELECT 1 FROM forced_refund_cases forced
                 WHERE forced.entitlement_package_id = refund.entitlement_package_id)
  AND NOT EXISTS(SELECT 1 FROM chargeback_cases chargeback
                 WHERE chargeback.entitlement_package_id = refund.entitlement_package_id);
```

对每条候选证据匹配（`in_scope=true`）的退款，还必须逐条确认（本命令的边界，任一不满足即不得纳入）：

- 退款包状态为 `FROZEN`，钱包为 `ACTIVE`；
- `refund_component_reservations` 覆盖的组件金额之和恰好等于 `refunds.credit_microcredits`，且
  `refund_grant_reservations` 的身份、组件关联、冻结金额和当前状态完整，不能只比较数量；
- 冻结预检必须展示 `revoked_microcredits` 与 `shortfall_microcredits`，其和等于退款额度。部分回收按既有同一次独立审批确认短缺与 `FORCED_REFUND` 应收，不允许静默结案，也不额外引入第二套审批政策；
- facts_digest 必须覆盖预留、组件、配额、证据与零事实，并在执行时完全匹配；上述 SQL 是筛选工具，owner 的当前预检和执行判据才是权威。

Account 侧核验：`approval_requests` 中该命令不得存在 `APPROVED`/`EXECUTING`/`EXECUTION_FAILED` 的遗留行；`outbox_events` 中不得存在未消费的该命令授权事件。任一不满足即阻断激活，并按设计 §8 失败分类只读查证。

## 3. 激活操作（仅在授权与前置条件齐备后执行）

激活必须由持有 `billing_schema_owner` 的部署身份执行，且只允许对该命令的 **v1/s1 行**做一次状态推进；不得新增行、不得改写任何既有迁移脚本或 `migrations/manifest.json`：

```sql
SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations
SET state = 'ACTIVE', version = version + 1
WHERE command_type = 'REFUND_EXCEPTION_REPAIR'
  AND policy_version = 1 AND snapshot_schema_version = 1 AND state = 'INACTIVE';
-- 必须恰好影响一行；触发器会写入不可改写 history 行。
```

注意：冻结快照包含 activation 版本，因此**任何在激活前装配的请求都必须在激活后重新装配**；不得把激活施加到已冻结的请求上。Guard 会拒绝身份/版本之外的改写；激活本身不产生任何财务效果，也不会调用 Provider。

## 4. 激活后验证（全部通过才可宣告激活完成）

1. Billing 投影：`GET /internal/v1/approval-command-activations` 返回该命令 `policy_version=1 / snapshot_schema_version=1 / state=ACTIVE`，其余命令状态不变，`billing_approval_command_activation_history` 追加一行且旧行未被改写。
2. Account 读取：`billinggovernance` 客户端观测到 `ACTIVE`，create/update/submit/APPROVE/Begin 五个门禁不再返回 `ErrBillingGovernanceInactive`。
3. Proof：以真实 Account 身份调用 Billing proof endpoint，对一件存量清单内（`in_scope=true`）的退款返回 `PROOF_VALID`/`ALLOW`；对非 `MANUAL_REVIEW`、证据不匹配或 `provider_status` 属失败闭集的候选、包非 `FROZEN`、钱包非 `ACTIVE`、已存在 `refund-repair:` journal / `FORCED_REFUND` 应收 / 修复 case 的候选返回对应 DENY。proof 本身不得触发任何 Provider 请求。
4. 消费者：durable `billing-refund-exception-repair-executor-p1-v1` 处于消费状态（Q+1 起始序与收据一致），DLQ/重试计数为 0。
5. 受控演练：以一条一次性审批完成端到端执行，核对：
   - `refunds.status` 由 `MANUAL_REVIEW` 推进为 `SUCCEEDED`，并写 `billing.refund.succeeded.v1` outbox；
   - 可回收组件按冻结值 CAS 全额收回，`credit_lots.remaining` 与组件 `available/active/suspended` 同步；
   - 钱包 `posted_microcredits` 减少 `revoked_microcredits`；
   - `REFUND` journal（引用 `refund-repair:<refund_id>`）平衡且含 `REFUND_CLEARING`；
   - 若 `shortfall > 0`，生成 `FORCED_REFUND` 应收（`RECEIVABLE_ASSET` 借项）且 `receivables` 行与钱包应收投影一致；
   - `payment_anomaly_cases` 由 `PENDING` 推进为 `APPLIED`（version CAS），并写入不可改写的 `refund_exception_repair_cases` 行（含审批联动列）；
   - result-first 幂等重放不产生第二条 case/journal/应收；
   - 在演练记录中登记真实 ID、审批/授权事件 ID 与 Provider 调用计数为 0 的观测。

将上述输出归档到发布证据（脱敏，不含 Secret/Token/Cookie/个人信息与 Provider 原文）。

## 5. 回滚

1. 停止该 durable 消费者（或回滚到不消费该 durable 的镜像），使新事件不再执行。
2. 用 `billing_schema_owner` 把 v1/s1 行翻回 `INACTIVE`（`version = version + 1`）；history 追加而非改写。
3. 验证 Account create/update/submit/APPROVE/Begin 五个门禁重新失败关闭、proof 返回不可用；已执行的修复事实**不回滚**（退款、journal、receivable、case 与审计行一律保留可读）。
4. 仍在 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓，不得盲目重放。

## 6. 记录

- 2026-09-20 代理：新建本手册，落地“能力就绪但默认未激活”；激活必须由授权人按上表执行，未执行任何生产激活。
