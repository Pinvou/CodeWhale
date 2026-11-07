# 迁移账本的 schema 选择

通用迁移器保留当前可见 `schema_migrations` 所在的 schema；首次建库则使用操作开始时的 `current_schema()`。所有账本读写和首次建表使用安全引用的完整 schema 名称，不把已有账本移到 `public`。存量升级只需要原账本权限，不因 `CREATE TABLE IF NOT EXISTS` 再要求 schema CREATE。

每次 `Up` / `DownOne` 操作会在开始时固定账本位置，迁移 SQL 或受信 hook 修改 `search_path` 不会重定向该次操作的账本读写。需要跨多次操作保留同一位置的调用者，应在最初配置好角色与 `search_path` 后使用同一个派生上下文：

```go
ctx, err := migrations.PinLedger(ctx, connection)
if err != nil {
    return err
}
defer migrations.Unlock(context.Background(), connection)
if err := migrations.Up(ctx, connection, loaded); err != nil {
    return err
}
_, err = migrations.VerifyApplied(ctx, connection, loaded)
return err
```

CLI 在选择 `MIGRATION_ROLE` 后固定一次位置，域识别、执行与最终验证沿用同一上下文。`LedgerTable(ctx, connection)` 为需要查询同一账本的受信调用者提供安全引用的限定表名。绑定上下文只适用于原连接；另一连接必须建立自己的绑定。

独立工作流可以选择另一个 schema 中的独立账本。没有绑定时，新的操作依照其开始时可见的账本选择；不会扫描其他 schema 猜测目标。管理员应在绑定前配置正确的可信 `search_path`。没有可用 schema 时明确拒绝，不静默改用 `public`。

这只限定迁移器的账本访问。迁移 SQL、领域包装器和自定义 hooks 对业务对象的 schema/权限约束仍按各自合同执行；已发布的 Account、Billing、Token 等 SQL 中的 `public` 约束保持不变。没有修改冻结 SQL、历史 checksum、数据库权限或原 advisory lock/Unlock 合同。
