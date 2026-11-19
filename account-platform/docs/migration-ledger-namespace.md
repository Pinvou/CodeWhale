# 迁移账本的 schema 选择

通用迁移器按操作开始时的有效 `search_path` 顺序选择第一个已有的永久 `schema_migrations`；没有永久账本时，才使用显式创建路径中第一个有效的非临时 schema。所有账本读写和首次建表使用安全引用的完整 schema 名称，不把已有账本移到 `public`。存量升级只需要原账本权限，不因 `CREATE TABLE IF NOT EXISTS` 再要求 schema CREATE。

同名临时表不能提供迁移历史，即使 `pg_temp` 显式排在路径前方也不会被采纳。前方 schema 没有账本时，仍会读取路径后方已有的永久账本，不在前方另建一份历史；较早永久账本的读权限不足则报错，不跳过它猜测另一个可读账本。创建回退不会选择隐式 `pg_catalog`，只有临时 schema 或没有有效创建 schema 时在准备、写入前拒绝。

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

CLI 在选择 `MIGRATION_ROLE` 后固定一次位置，域识别、执行与最终验证沿用同一上下文。直接调用 Account、Billing、Token 的公开 `Up` / `VerifyApplied` 也会在各自工作流内保留绑定上下文，前置历史检查和事务钩子的工作流账本读点通过 `LedgerTable(ctx, connection)` 使用同一安全限定名。绑定上下文只适用于原连接；另一连接必须建立自己的绑定。

独立工作流可以选择另一个 schema 中的独立账本。没有绑定时，新的操作依照其开始时可见的账本选择；不会扫描其他 schema 猜测目标。管理员应在绑定前配置正确的可信 `search_path`。没有可用 schema 时明确拒绝，不静默改用 `public`。

这只固定工作流账本，不将业务对象迁移到任意 schema。迁移 SQL、领域包装器和自定义 hooks 对业务对象的 schema/权限约束仍按各自合同执行；为已发布 `public` 业务资产的表形状、触发器和权限背书的显式 `public.schema_migrations` 历史校验也保持原边界，不能用另一个 schema 的历史替它授权。没有修改冻结 SQL、历史 checksum、数据库权限或原 advisory lock/Unlock 合同。
