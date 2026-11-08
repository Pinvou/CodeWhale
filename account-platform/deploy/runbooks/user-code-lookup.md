# 完整用户编码精确查询

这是 ACC-58/NFR-50/APT-001 的既有能力补齐。用户目录的用户名、展示名或 UUID 搜索不代替独立编码查询。运营入口在用户管理中单独提交完整编码，BFF 只允许 GET 单一代码段；不把 HEAD、批量、前缀或模糊请求映射为查询。

用户表格为永久编码提供独立列，复用现有简中、英文、日文的完整编码文案。即使用户名存在，也同时显示服务端返回的编码；缺失编码显示占位符，不从用户名或其他字段推断。

Account 继续验证用户身份，仓储事务使用当前 `platform.users.read`。权限判断留在该事务，使无权、格式错误、无对应主体都返回相同404 `USER_NOT_FOUND`，并把可归属有效操作者的尝试与审计一起提交，避免通用拒绝日志保存完整编码路径。源IP沿用精确受信代理边界；BFF覆盖浏览器来源头，设备头只在该精确入口透传，缺失时沿用当前会话归属。数据库只保存来源、设备及规范化目标编码的SHA256摘要，审计只含目标摘要及结果枚举。匿名或无有效主体的认证拒绝不能伪造审计操作者。

精确查询失败后的重试继续调用当前完整编码的同一入口，不转成目录列表。编辑编码可以取消旧查询的显示资格，但不能解锁正在执行的用户状态写操作；写操作期间编码字段保持不可编辑，直到该次写及其既有刷新完成。客户端换代会退休旧写标记，旧回执不能解除新客户端写操作的busy围栏。

保留原每分钟actor30/source60/device30/target10阈值，所有结果计入对应维度。查询先按全局排序取得维度事务锁，再取操作者USER行锁；随后以数据库当前时钟和READ COMMITTED快照核验计数及权限，避免锁等待前的快照/过期权限放行。结果、尝试与审计在同一事务提交。原始永久编码与格式化规则不变；已注销主体仍可按完整编码定位，返回当前匿名化后的投影及DELETED状态，不重新暴露历史邮箱或提供注销主体模糊目录。

现有011已经建立`user_code_lookup_attempts`及四维索引，不新增表或配额策略。本次另原样恢复原O/F共同发布的005 `audit_search` up/down身份及字节，安装顺序仍按既有wrapper，不能改写旧迁移账本或把已安装5当成新版本。当前无5的库补装原三索引；当前合法schema已含原5身份时，重跑Up/Verify须原账本行保持不变。本项不证明原O整库升级：O的003等其他body与当前不同，其整库兼容另列未验证，不推断为已授权拒绝。

正式回归：Account `TestUserCodeLookupRuntimePostgres`、`TestAuditSearchOriginalMigrationRuntimePostgres`（严格专用loopback/admin数据库、公开迁移、非superuser runtime、UUID数据库及owned actors清理），Portal `TestUserCodeLookupActualSessionProxy`；客户端`account_platform_user_code_lookup.test.mjs`，实际React/client窄浏览器脚本`account_platform_user_code_lookup_smoke.mjs`使用独立profile与受控Account响应，不代表真实Keycloak/生产端到端。本机Edge启动未取得DevTools时必须记未验证，不能将脚本存在或客户端单元通过当实际浏览器通过。
