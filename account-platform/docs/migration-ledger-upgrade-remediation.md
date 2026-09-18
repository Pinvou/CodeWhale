# APT-138-P1：受限迁移身份的既有账本重入修复

## 需求与根因

APT-138 真实非超级用户 PostgreSQL 激活迁移验收发现：Token 29 已正确收回业务角色的 schema CREATE 权限；通用迁移执行器 `prepare` 却每次执行 `CREATE TABLE IF NOT EXISTS schema_migrations`。PostgreSQL 对该语句仍检查 CREATE 权限，因此已有完整迁移账本、具备读取/记录权限的受限迁移身份也会以 SQLSTATE 42501 失败。该基础设施前置单独审核和提交，不与未完成的激活接线混合。

必须允许合法升级及 down 重入既有账本；不授予 runtime CREATE、不新增账本 ACL、不变更领域迁移或校验和、不借超级用户替代生产身份。

## 设计

保持原 session advisory lock。持锁后在同一连接、同一 search_path 下执行 `pg_catalog.to_regclass('schema_migrations') IS NOT NULL`；账本存在则跳过 CREATE，否则执行原 CREATE TABLE IF NOT EXISTS。后续读取、校验和、INSERT、DELETE 和 VerifyApplied 全部不变，仍由 PostgreSQL 权限检查失败关闭。表不存在且无 CREATE 权限仍失败，不能伪造空账本或默默跳过迁移。

不修改 search_path 解析语义；同一 advisory lock 防止合作执行器并发初始化。非合作外部 DDL 造成对象消失或无权限时由既有错误处理拒绝，不重建既有历史。

## 单任务与验收

- `APT-138-P1`：仅本说明、`internal/platform/migrations/migrations.go`、专属 `restricted_upgrade_integration_test.go`。文档 B 审通过后编码；代码 B 审与 HEAD+本批隔离候选真实 PostgreSQL 验证通过后独立签署提交。当前 `DONE / REMEDIATION_REVIEW_PASS`，由根代理执行独立签署提交；不代表 APT-138 总装通过或已推送、部署。
- 真实独立临时 schema 与可认证的非超级用户临时角色，断言数据库/session_user/current_user；只临时 fixture 具备 bootstrap 管理身份。
- 有 CREATE 的首次 bootstrap 能创建账本和领域表；撤回 CREATE 后，既有账本重入/追加 DML 迁移/down 保持成功。
- 未持有账本权限拒绝读取或记录；账本不存在且无 CREATE 拒绝；篡改已应用 SQL 的校验和仍拒绝且原历史不变。
- 使用实际 pg_advisory 锁竞争证明 prepare 锁未绕过，测试结束关闭连接并清理精确临时 schema/角色，不影响其他服务的实际角色。
- 运行原 migrations 单元测试及本批真实 PG；不得将缺环境 SKIP 计为通过。APT-138 激活迁移随后再独立复跑。

## 独立审核记录

- 2026-09-06 A：完成根因、最小设计和任务边界，提交 B 审核；尚未编码或宣称验证通过。
- 2026-09-06 B：文档及代码静态审核 PASS。A 已写好三文件最小实现及具名 PostgreSQL 回归测试，等待隔离候选实际执行结果；尚未宣称真实测试通过，不标记完成。
- 2026-09-06 根代理：从已提交 HEAD 加本批三文件导出隔离候选 `fd445c2e68e3373fdd144065e582bc0c865dbe22`。`TestRestrictedMigrationLedgerPostgresIntegration` 真实 PostgreSQL 五个子用例全 PASS，用例 0.97 秒；原四项迁移单元测试亦 PASS，包总计 2.909 秒，退出码 0，无 SKIP。真实证据覆盖受限升级/down、历史校验和、无账本权限、无 CREATE 初始化和 advisory 锁竞争；不依赖未提交激活代码。
- 2026-09-06 B：最终独立审核 PASS，三文件范围、真实角色权限和完整测试证据均通过。A 仅更新本记录，交根代理独立签署；后续按既定顺序恢复其余前置修复及 APT-138。

# GSA-22-P1：迁移切换可信 owner 后的账本写入身份

## 需求与根因

GSA-22 跨域领域 E2E 首次按生产同构拓扑创建独立 Billing 数据库（`CREATE DATABASE ... OWNER billing_schema_owner`、`public` 归 `billing_schema_owner`、`schema_migrations` 归迁移角色 `billing_service`、迁移以 `billing_migrator` 加 `SET ROLE billing_service` 执行）应用迁移 1→58 时在 `000056_entitlement_component_correction_prepare` 失败：`permission denied for table schema_migrations (SQLSTATE 42501)`。

根因有两层：`000056` 内部 `SET ROLE billing_schema_owner` 建表后没有切回迁移角色，通用执行器 `Up` 随后在同一事务内以该 owner 记录账本；提交后连接角色仍是 schema owner，下一次迭代的账本 `SELECT` 同样会失败。`000053/000054/000055/000057/000058` 均在结尾回到 `SET ROLE billing_service`，因此这是 `000056` 的遗漏；而仓库 `migrations/manifest.json` 明确禁止编辑已登记历史脚本，修复只能落在通用执行器。

## 设计

`internal/platform/migrations` 在执行每个迁移前读取一次 `current_user`（迁移角色，即部署 `MIGRATION_ROLE`，也是账本在部署库中的所有者），并在事务内于迁移 SQL 执行前后各 `SET ROLE` 回该角色：迁移仍可合法切到可信 owner（例如 `billing_schema_owner` 建表），但账本 `INSERT` 以及下一次账本读取始终由迁移角色执行。角色恢复失败保持既有失败关闭；不新增账本 ACL、不改写已登记迁移、不引入超级用户替代身份。`DownOne` 采用同一约定，删除账本行同样回到迁移角色。

## 单任务与验收

- 文件范围：本说明、`internal/platform/migrations/migrations.go`、专属真实 PostgreSQL 回归 `internal/platform/migrations/restricted_upgrade_integration_test.go`。
- 回归用例 `trusted_owner_switch_keeps_ledger_identity`：迁移 3 在事务内 `SET ROLE` 到受信 owner 后建表；断言升级成功、账本记录版本 3、连接当前角色仍是迁移角色、DDL 对象归属该受信 owner、`down` 后对象消失且账本回到 2。
- 修复前同一用例必须以 `permission denied for table schema_migrations (SQLSTATE 42501)` 失败；修复后真实 PostgreSQL 六个子用例全部 PASS，无 SKIP。
- 运行 `go test ./internal/platform/migrations`、`go vet ./internal/platform/migrations`、架构守卫与统一 `account-platform verify`；不使用 SKIP 作为通过证据。
- 本切片只修正账本写入身份，不激活任何财务命令、不部署、不改动 `migrations/manifest.json`。

## 独立审核记录

- 2026-09-18 代理：定位根因、完成最小修复，并以真实 PostgreSQL 证明修复前失败、修复后 `TestRestrictedMigrationLedgerPostgresIntegration` 六个子用例全 PASS；待独立审核与本任务后续提交。
