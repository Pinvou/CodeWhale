# Account 021 的邀请回退边界

冻结迁移 `000021_membership_governance.down.sql` 最后恢复 `invitations.email_normalized NOT NULL`。LINK 邀请允许该字段为 NULL；PHONE、USERNAME 等同样不依赖邮箱的邀请也不能假设具有邮箱值。存在 NULL 时，原 Down 会以 SQLSTATE `23502` 拒绝。此前在同一迁移事务中的 DDL 会回滚，账本不应删除 021；这表示回退不受支持，不表示原邀请已经丢失。

先使用已有只读权限核对部署版本、完整迁移账本和邀请事实：

```sql
SELECT version,name,checksum FROM schema_migrations ORDER BY version;
SELECT target_type,count(*) FROM invitations
WHERE email_normalized IS NULL GROUP BY target_type;
```

不要删除邀请、补造邮箱、改写迁移 SQL/checksum 或跳删账本来绕过拒绝。保留数据库及原备份，按现有停写、备份和迁移角色流程，使用已审核的当前或后继版本执行公开 Up/Verify；应用回退也必须保留能够读取这些事实的版本。存在后续 forward-only 迁移时，先遵守它们各自的保护，不能越过高版本直接执行 021。

回归从原始完整 1–21 迁移真实安装存量库，使用 `account_migrator` LOGIN / `account_service` 非超级用户角色，保留一个符合原指纹合同的 LINK 邀请。实际公开 Account Down 两次均拒绝，并核对所有表、账本、列、索引、触发器及相关函数不变；随后公开 Up/Verify 完整前向升级，当前实际邀请写方按原键回放同一邀请，再接受合法新命令。存量身份/组织/邀请由受控 fixture 准备，真实写方回放验证其可消费性；不声称运行旧服务、外部 IdP 或生产恢复演练。
