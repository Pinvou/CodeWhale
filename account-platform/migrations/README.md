# 数据库迁移

Account、Billing、Token 和 Payment Simulator 分别拥有独立目录和数据库。文件名格式：

```text
000001_description.up.sql
000001_description.down.sql
```

迁移执行器对已应用 Up 文件保存 SHA-256，文件应用后不得修改。变更必须增加新的版本。迁移通过 PostgreSQL Advisory Lock 串行执行，每个版本在独立事务中提交。

## 发布前更正记录

- Account `000033_payment_provider_resume_policy.up.sql` 在尚未部署时误用了不存在的 `permissions.scope_type` 列，因此该原始文件不可能成功提交事务。三类权限迁移发布前已将其更正为真实列 `permissions.scope`，并同步冻结新摘要。任何环境若声称已经应用旧摘要 `760c79205a4832d3c1b750d328a05389f5d8ded47b166c923dc4bf0d09c5807f`，都不是可直接升级的受支持状态，必须先人工核验迁移表和权限数据；不得跳过摘要校验。
- Account `000035_break_glass_trusted_operator.up.sql` 在首次云端部署前发现会被 `break_glass_operations` 的语句级不可变触发器拒绝，即使历史表为空也无法提交。该迁移尚未在目标环境应用，现已更正为在同一事务内仅为兼容回填短暂移除触发器并立即恢复，同时冻结新摘要。任何环境若声称已经应用旧摘要 `869336713029646d8fa1e7fbe2d1ced8d5bee76a1ce324b79276c5e89ca74f18`，都不是可直接升级的受支持状态，必须先核验迁移表和安全证据完整性。

示例：

```text
DATABASE_URL=postgresql://... go run ./cmd/migrate --dir ./migrations/account
DATABASE_URL=postgresql://... go run ./cmd/migrate --dir ./migrations/account --direction down
```
