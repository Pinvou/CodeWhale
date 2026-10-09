# 应收核销（`RECEIVABLE_WRITE_OFF`）受控激活手册

状态：F11 交付能力、**默认未激活**；本手册不是本轮已完成的生产验收记录。本手册只描述受控激活（把 Billing 逐命令 activation 行从 `INACTIVE` 翻到 `ACTIVE`）的操作与证据要求；是否执行必须另有授权，且 GSA-38～40 的切换、回滚与发布门禁全部通过后才能开始。

## 1. 前置条件（全部满足才可开始）

1. 目标 Commit 已完成 GSA-38（切换/回滚工具、资源清单与只读 readiness）、GSA-39（生产等价集成、性能、N/N-1 与故障演练）与 GSA-40（逐命令额度与生产证据归档）中适用于本命令的项，并有独立审核记录。
2. 用户/授权人已就该命令的生产激活给出明确授权（书面记录存档，含时间与范围）。
3. 专属 durable `billing-receivable-write-off-executor-p1-v1` 的受信 cutover 资源绑定已就绪，Billing 使用当前 `RunBillingApprovalConsumers` 注册集合与受信资源装配启动（六个受信收据输入缺项、摘要不符或读取失败仍硬失败关闭），不得添加不存在的逐命令启动开关。
4. 已验证 Account 侧五入口读取实时 Billing activation：行仍为 `INACTIVE` 时 create/update/submit/APPROVE/Begin 全部失败关闭（`ErrBillingGovernanceInactive`）。
5. 已完成一致性备份（含 Billing 库）并记录备份 ID、完成时间与 SHA-256；激活窗口内停止写入新审批。

## 2. 只读核验（记录输出作为证据）

以只读身份执行并归档：

```sql
-- Billing：确认 v1/s1 行仍在且尚未激活
SELECT command_type, policy_version, snapshot_schema_version, state, version, updated_at
FROM billing_approval_command_activations
WHERE command_type = 'RECEIVABLE_WRITE_OFF' AND policy_version = 1 AND snapshot_schema_version = 1;

-- Billing：确认没有该命令的核销效果
SELECT (SELECT count(*) FROM receivable_write_off_cases) AS cases,
       (SELECT count(*) FROM receivable_allocations WHERE source_type = 'APPROVED_WRITE_OFF') AS write_off_allocations,
       (SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%') AS write_off_journals,
       (SELECT count(*) FROM billing_approval_proof_claims) AS proof_claims;
```

Account 侧核验：`approval_requests` 中该命令不得存在 `APPROVED`/`EXECUTING`/`EXECUTION_FAILED` 的遗留行；`outbox_events` 中不得存在未消费的该命令授权事件。任一不满足即阻断激活，并按设计 §8 失败分类只读查证。

## 3. 激活操作（仅在授权与前置条件齐备后执行）

激活必须由持有 `billing_schema_owner` 的部署身份执行，且只允许对该命令的 **v1/s1 行**做一次状态推进；不得新增行、不得改写任何既有迁移脚本或 `migrations/manifest.json`：

```sql
SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations
SET state = 'ACTIVE', version = version + 1
WHERE command_type = 'RECEIVABLE_WRITE_OFF'
  AND policy_version = 1 AND snapshot_schema_version = 1 AND state = 'INACTIVE';
-- 必须恰好影响一行；触发器会写入不可变 history 行。
```

注意：冻结快照包含 activation 版本，因此**任何在激活前装配的请求都必须在激活后重新装配**；不得把激活施加到已冻结的请求上。Guard 会拒绝身份/版本之外的改写；`receivables`/`receivable_allocations`/`journal_entries`/`ledger_postings` 仍然只允许新增，激活本身不产生任何财务效果。

## 4. 激活后验证（全部通过才可宣告激活完成）

1. Billing 投影：`GET /internal/v1/approval-command-activations` 返回该命令 `policy_version=1 / snapshot_schema_version=1 / state=ACTIVE`，其余命令状态不变，`billing_approval_command_activation_history` 追加一行且旧行未被改写。
2. Account 读取：`billinggovernance` 客户端观测到本命令 v1/s1 的 `ACTIVE`，五入口不再返回 `ErrBillingGovernanceInactive`；整个投影中的未交付命令仍不能因本次激活而获得 ACTIVE 资格。
3. Proof：以真实 Account 身份调用 Billing proof endpoint，对一件真实存在的未结 `USAGE_OVERAGE` 应收返回 `PROOF_VALID`/`ALLOW`；对 `CHARGEBACK` 来源、终态应收、超出未结金额或钱包投影不足的候选项返回对应 DENY。
4. 消费者：durable `billing-receivable-write-off-executor-p1-v1` 处于消费状态（Q+1 起始序与收据一致），DLQ/重试计数为 0。
5. 受控演练：以一条一次性审批完成端到端执行，核对 `receivable_write_off_cases` 追加一行且不可变、`receivable_allocations` 新增一条 `APPROVED_WRITE_OFF`、新 journal 恰含两条相反 posting（`RECEIVABLE_ASSET +A`/`ADJUSTMENT −A`，均无 lot）、应收三元组与状态推进、钱包 `receivable_microcredits`/`financial_status` 同步、`posted_microcredits`/`credit_lots`/`entitlement_*` 零变化、result-first 幂等重放不产生第二条 allocation/journal，并在演练记录中登记真实 ID 与校验结果。

将上述输出归档到发布证据（脱敏，不含 Secret/Token/Cookie/个人信息）。

## 5. 回滚

1. 停止该 durable 消费者（或回滚到不消费该 durable 的镜像），使新事件不再执行。
2. 用 `billing_schema_owner` 把 v1/s1 行翻回 `INACTIVE`（`version = version + 1`）；history 追加而非改写。
3. 验证 Account 五入口重新失败关闭、proof 返回不可用，新领域执行重新核对实时 activation 状态及 policy/schema/version 后失败关闭；已提交 case 仍按 result-first 返回原收据且零新效果。已执行的核销事实**不回滚**（历史 allocation、journal 与 case 永不撤销）。
4. 仍在 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓，不得盲目重放。

## 6. 记录

- 2026-09-19 代理：新建本手册，落地“能力就绪但默认未激活”；激活必须由授权人按上表执行，未执行任何生产激活。
