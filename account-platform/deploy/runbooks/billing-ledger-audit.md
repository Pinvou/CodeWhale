# Billing 全账本只读核查

手动运行 `psql --no-psqlrc --set=ON_ERROR_STOP=1 --file=account-platform/deploy/runbooks/sql/billing-ledger-audit.sql`。连接通过站点既有受控服务文件/环境提供，不将密码、完整 DSN 或业务行内容写入命令记录。生产应使用既有审批授予的只读运维身份；脚本不会创建、授予角色或自动修复。

恢复验收另传 `--set=audit_database="$BILLING_AUDIT_DATABASE"`。在同一只读事务中检查目标库名、Billing 迁移标记和迁移账本可见性，并记录库名及 UTC 采集时间。该模式需要读取 `public.schema_migrations` 的 `version` 和 `name`。日常核查省略该参数时沿用原输出形状。

脚本对当前完整公开 Billing schema 执行 REPEATABLE READ READ ONLY，限定 search_path/超时并最终 ROLLBACK。显式把 pg_temp 放在 public 后，复用 psql 会话已有临时同名表也不能替代被核查的 public 表。会先检查17张必要表的 SELECT 权限和当前身份 RLS 可见性；权限不足、RLS 过滤、缺表、SQL 错误都非零失败，不能把未见行当健康。只有完整输出18个 `check_name|violations`（psql可加 `--no-align --tuples-only --field-separator='|'`）才是一次完整核查。输出只含计数，不含 owner、request、凭据或业务行。

保留原15类核查：journal 平衡、wallet posted/reserved/receivable 快照、应收金额/分配、chargeback 来源、component 守恒/lot/package 投影、hold/component 分配、过期 ACTIVE hold、最新 lease 和已付款待入账。没有任何 component 的 package 也参与投影比较，空集合按0比较；非零原包缺全部 component 必须提示差异，不另造“零包必须有 component”的业务政策。增加3类现有成员来源的投影/预留/身份一致性核查。成员预留已经计入 usage_hold_allocations，不能再叠加一次；ACTIVE/REVIEW 均保留预留，旧 LEGACY_ORDER_REFUND 仍计 wallet reservation，现代 targeted refund 冻结来源而不重复计 wallet 预留。

计数并不自动证明可以改账：`expired_active_holds` 表示缺少可信无 dispatch 证据的到期 hold，可能是正确的保守状态；`paid_orders_not_credited` 可是需人工复核的真实支付事实。非零必须结合现有审计/证据、来源 owner 和独立复核查明原因，不运行自动 release、补扣、退款或重放。其他守恒非零也只产生调查信号。读失败或核查不完整时保持验收未完成。

原迁移身份、权限边界、资金赢家和 pre016/F016 拒绝政策不变；不接受该脚本为旧数据自动升级工具。受控测试通过不代表站点、灾备或真实渠道验收。
