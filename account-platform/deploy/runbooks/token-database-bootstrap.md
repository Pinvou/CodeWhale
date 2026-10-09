# F04 Token 数据库首次准备

只对已经由数据库操作员建立的当前 Token 专用数据库运行 `../production/token-database-bootstrap.sql`。先核对目标数据库、备份、角色清单和公私 CA；操作员凭据与 `token_migrator`、`token_gateway` 分离。SQL 不创建其他数据库或未来功能资源，也拒绝重置已有或部分安装的迁移历史。

预置五个最小权限角色：`token_gateway` 为运行身份；`token_migrator` 与 `token_policy_operator` 为 LOGIN、NOINHERIT；`token_schema_owner` 与 `token_cap_policy_deployer` 为 NOLOGIN。所有角色均无 SUPERUSER、CREATEDB、CREATEROLE、REPLICATION、BYPASSRLS。仅迁移身份能 SET ROLE 到 gateway、schema owner 和 cap deployer；运行身份不能继承 schema/database owner。身份及密码通过现有受控数据库运维流程投递，不在本 SQL 或示例中提供凭据。

使用 PostgreSQL 的独立私密 service 文件和 passfile，准确绑定目标数据库并设置 `sslmode=verify-full` 和可信 CA。文件必须是操作员独占的绝对路径、普通文件且非符号链接；POSIX passfile 权限 0600，Windows 使用等价私有 ACL。禁止把完整连接串、密码、环境或展开后的配置打印到日志。

在受控 shell 先设置 `PGSERVICEFILE`、`PGPASSFILE` 和指向上述已核验配置的 `PGSERVICE`，然后运行：

```sh
psql -X --set=ON_ERROR_STOP=1 --file=account-platform/deploy/production/token-database-bootstrap.sql
```

该操作安装原迁移 011/014 必需的 `pgcrypto`、设置当前 public schema owner 和首次基础 DDL 权限，并仅向 `token_migrator` 授予 `pg_read_all_stats` 成员资格。离线治理切换用该统计可见性核对所有会话是否已退出；运行身份不获得该统计成员资格。不要为安装 extension 向 gateway 或日常 migrator 授予数据库 CREATE。随后用真实 `token_migrator`、`MIGRATION_ROLE=token_gateway` 执行当前 `migrate` 入口；SQL 029 撤回 runtime schema CREATE，公开 stage wrapper 只在原 033 的同一事务内临时授予建索引权限并撤回。启动前必须对当前固定的完整 Token 迁移集合运行 `VerifyApplied`，不得接受后继匿名化或其他阶段数据库。

安装后独立检查 runtime 不能创建 schema 对象、不能改治理激活及 cap policy，以及迁移账本、当前对象和固定输入完整一致。普通服务没有 operator 凭据；此准备不表示治理 ACTIVE、运行服务启动或部署验收通过。完整 E2E 按用户决定延期，现场 TLS、凭据 ACL 和数据库运维步骤仍需部署环境核验。
