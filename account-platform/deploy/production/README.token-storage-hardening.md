# Token 存储硬化升级

900001 只能向前迁移，保留历史 SQL 校验和与审计证据。升级前备份数据库，并在维护窗口验证角色和迁移账本。

新库由独立数据库操作员执行 `token-database-bootstrap.sql`；已有 F04 库执行 `token-storage-hardening-bootstrap.sql`。随后以 `token_migrator` 登录，切换 `token_gateway` 并使用 Token 专属迁移入口。服务和应用迁移不得使用超级用户。

操作员脚本核对已有五个角色及成员关系，不创建或重置角色、密码、数据或迁移历史。脚本仅建立 schema owner 到 runtime 的单向继承关系；runtime 不能切换 schema owner 或策略部署角色。缺少预置时迁移拒绝执行。

迁移后普通对象和迁移账本归不登录的 `token_schema_owner` 所有。运行时保留业务必需的读写、审计追加和账本只读权限，不拥有对象、不具有 schema CREATE，不允许改写账本或截断存储。策略对象及原有安全定义函数保持独立所有权。

后续迁移仅在事务内临时获得 DDL 权限，提交前恢复所有权并收回权限；账本写入切换 schema owner。新增普通表和策略表均安装 TRUNCATE 语句保护，不可变历史的 UPDATE/DELETE 权限不会重新授予运行时。
