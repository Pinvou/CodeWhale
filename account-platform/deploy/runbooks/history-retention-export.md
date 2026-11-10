# 历史记录保留、只读盘点与校验导出

适用于 Account、Billing、Token 的 outbox 历史，以及 Billing 的支付供应商健康观测。
在另行批准明确的对象、期限及关联证据处置方案前，不自动删除事件、回执、案件或业务引用。
本工具不安装清理任务、不改变投递状态，也不授予恢复、重放或资金操作权限。

## 前提与保管

使用 PowerShell 7.4 或更新版本，以及不低于服务端主版本的 PostgreSQL 客户端
（`psql`、`pg_dump`、`pg_restore`）。脚本提供 Windows 和 Linux 权限适配；
支持状态以目标环境实际验收为准。macOS 及其他平台显式拒绝，避免将独立 ACL
误当作 Unix mode 已覆盖。不要使用 Windows PowerShell 5.1。

由 DBA 提供已经批准的只读身份：对 `public.schema_migrations`、`public.outbox_events`
有 USAGE/SELECT 权限；Billing 还需要 `public.payment_provider_health_observations`。
不要为导出扩大为超级用户或绕过行级权限。工具发现 RLS 会影响可见性时拒绝导出。
该身份、连接目标和凭据继续从原受控系统提供；用标准 libpq 环境变量和受保护的
passfile，不要把密码或连接 URL 写入命令行、工单、终端转录或 Git。

预先在加密或受控磁盘上准备一个只有当前操作人和 SYSTEM（Windows），或仅所有者
（Unix，0700）可访问的父目录。Windows 父目录 DACL 必须关闭继承且不向其他主体
授予访问。父目录与输出目录均不能是符号链接或重解析点。工具不会放宽权限、
覆盖已有目录，或自动清理失败产物。输出可含身份、业务引用和原始事件载荷，
必须按原始数据同等权限保管，不要上传到普通 CI 日志或公开附件。

## 只读盘点

在仓库根目录，连接环境由受控入口预先设置。以下命令不会输出事件载荷：

```powershell
psql -X -v ON_ERROR_STOP=1 -v audit_domain=billing -f account-platform/deploy/runbooks/sql/history-export-metadata.sql
```

该 SQL 使用只读事务，验证域标识、表可见性及最多 2048 条迁移身份；每条语句
最多 30 秒、锁等待最多 5 秒。存储字节包含表、索引及 TOAST；`estimated_rows`
来自数据库统计，可能过时或为 -1，不能作为确切记录数或删除依据。
需要投递状态的精确盘点时，另用 [outbox-inventory.sql](sql/outbox-inventory.sql)，
同时提供 `audit_database` 和 `audit_domain`；其全量计数有独立的 60 秒语句预算。

## 导出与交接

在受控入口设置 `PGHOST`、`PGPORT`、`PGDATABASE`（纯数据库名）、`PGUSER`、
`PGSSLMODE` 及受保护的认证材料；保持生产要求的 TLS 校验。按实际域替换 `billing`，
输出路径必须尚不存在。`PgToolsDirectory` 可省略以使用受控 PATH。

```powershell
pwsh -NoProfile -File scripts/export-account-history.ps1 -Domain billing -OutputDirectory '<受控父目录>/billing-history-001' -PgToolsDirectory '<PostgreSQL客户端目录>' -TimeoutSeconds 1200
pwsh -NoProfile -File scripts/export-account-history.ps1 -Domain billing -OutputDirectory '<受控父目录>/billing-history-001' -PgToolsDirectory '<PostgreSQL客户端目录>' -Verify
```

导出成功仅输出 `ACCOUNT_HISTORY_EXPORT_COMPLETE`；校验成功仅输出
`ACCOUNT_HISTORY_EXPORT_VERIFIED`。失败退出非零并输出固定失败标记，数据库错误细节
只保存在受保护目录中。失败目录不能当作交接完成；查明后使用新的目录重试。
超时终止的是本次拥有的客户端进程树；数据库只读事务随连接退出而释放。

`history.dump` 是 pg_dump 自身一致快照中的选定表数据；`manifest.json` 保存域、
数据库/角色身份、前后观察时间、迁移身份、表清单、归档大小及 SHA-256。
元数据在导出前后分别读取，**不是归档快照中的行数或时间点证明**。
迁移身份、数据库 OID、服务版本、角色或服务启动时间变化会拒绝生成完成回执。
长导出期间 DDL 仍需由发布流程冻结；客户端语句预算可能使大型导出失败，
此时安排受控维护窗口，不应取消证据校验或自动删除历史。

保管人应通过已有受控渠道单独登记 manifest 的摘要、工单编号、来源和交接人。
随归档一起存放的 SHA-256 只检测损坏或不一致，不能证明来源真实性，
也不能防止攻击者同时替换归档和 manifest。校验会核对数据表目录及原始字节摘要。

导出只包含 schema_migrations、outbox_events，Billing 再包含健康观测表。
Account 的 outbox 同时包含普通和授权事件，但授权 sidecar、inbox 回执、财务案件、
供应商尝试等关联表仍保留在原库；**这是选表证据导出，不是完整备份或可独立恢复包**。
不得直接执行恢复到线上、重新投递或据此删除原表记录。灾难恢复继续使用现有
完整备份与恢复流程；任何清理必须另行批准对象、期限、引用关系和验证方法。

有界统计、统计失效告警及容量告警属于后续独立修复阶段；本工具不提供自动容量准入。
