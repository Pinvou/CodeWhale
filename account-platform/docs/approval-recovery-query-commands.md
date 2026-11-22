# 已授权原执行的有界调查租约

`900009_approval_recovery_query_billing_commands` 是新的前向迁移。它保留原 `000068` 以及 `000069`–`000077` 的版本、校验和和既有记录，仅给 `claim_approval_recovery_query` 的闭集追加已交付的八个 Billing 命令/版本/执行者/专用 durable 四元组：

| 命令 | 冻结策略版本 | 专用 durable |
| --- | --- | --- |
| ENTITLEMENT_COMPONENT_ADJUSTMENT | 2 | billing-entitlement-component-adjustment-executor-p2-v1 |
| LEDGER_REPAIR | 1 | billing-ledger-repair-executor-p1-v1 |
| RECEIVABLE_WRITE_OFF | 1 | billing-receivable-write-off-executor-p1-v1 |
| PAYMENT_FINANCIAL_REPAIR | 1 | billing-payment-financial-repair-executor-p1-v1 |
| REFUND_EXCEPTION_REPAIR | 1 | billing-refund-exception-repair-executor-p1-v1 |
| CHARGEBACK_FINANCIAL_REPAIR | 1 | billing-chargeback-financial-repair-executor-p1-v1 |
| RECONCILIATION_FINANCIAL_REPAIR | 1 | billing-reconciliation-financial-repair-executor-p1-v1 |
| USAGE_SETTLEMENT_REPAIR | 1 | billing-usage-settlement-repair-executor-p1-v1 |

这八项的执行者均为 `billing-command-executor`。原有六项（包括三个 Token 命令）不变；旧共享 Billing durable、组件 v1 和未知的未来路由不会由此开放。

迁移通过项目公开 Account 迁移入口执行，要求实际 `session_user=account_migrator`、`current_user=account_service`，并已安装原 068 和 077。函数仍由 `account_service` 持有，以 `SECURITY DEFINER` 和固定 `search_path=pg_catalog` 执行，仅接受真实 `account_approval_recovery` 登录会话。该工作者不能切换角色、读取业务表、审批、开始财务执行或授予新的领域尝试。

调查仍必须绑定原申请及其当前版本、原执行、授权事件、专用 durable 和审批 epoch；只允许原 `EXECUTING`/`EXECUTING` 或 `EXECUTION_FAILED`/`FAILED` 组合。15 秒单租约、释放后的退避、次数与版本围栏保持不变。拿到调查租约不表示发现可信结果，更不表示可以重新扣款或自动确认零效果；后续结果、资金证据与终态 CAS 仍走原独立校验。

回滚该闭集会令已经交付的原执行再次无法调查，因此迁移拒绝不完整回滚；需要变更时追加新的受审前向迁移，不修改已部署迁移身份或资金事实。

CI 的真实 PostgreSQL 阶段显式要求 `TestApprovalRecoveryQueryAllRegisteredCommandsPostgres` 和 `TestApprovalRecoveryQueryForwardMigrationPostgres`，缺根、无具名子用例、跳过或失败均不算通过。测试使用完整公开迁移和真实受限登录角色，经过实际申请、提交、独立批准、冻结事件、投递绑定、原执行、恢复计划和租约函数；外部激活、领域证明及发布传输为受控契约夹具，不宣称真实资金执行、消息中间件或发布验收。
