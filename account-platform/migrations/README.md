# 数据库迁移

Account、Billing、Token 和 Payment Simulator 分别拥有独立目录和数据库。文件名格式：

```text
000001_description.up.sql
000001_description.down.sql
```

迁移执行器对已应用 Up 文件保存 SHA-256，文件应用后不得修改。变更必须增加新的版本。新迁移计算校验和时将 CRLF 规范为 LF；已应用记录兼容相同 SQL 的 LF/CRLF 原始校验和，不改写历史记录，真实 SQL 内容修改仍会被拒绝。迁移通过 PostgreSQL Advisory Lock 串行执行，每个版本在独立事务中提交。

示例：

```text
DATABASE_URL=postgresql://... go run ./cmd/migrate --dir ./migrations/account
DATABASE_URL=postgresql://... go run ./cmd/migrate --dir ./migrations/account --direction down
```
