# 已发布账户平台历史的兼容升级

从既有生产数据库升级时，不能用新建空库的成功结果替代历史升级验证，也不能修改或删除 `schema_migrations` 中的原始身份来通过校验。

## Account 003 与出箱代次

原始 `000003_event_delivery` 不含 `publication_generation`，后续候选的同版本 SQL 含该列。两份已存在的 SQL 都保留原字节。Account 专用升级器仅在旧账本精确匹配原版名称及 LF/CRLF 校验和时，使用嵌入的原始 SQL 核验该条历史；通用迁移器的校验规则不变。

新增 `900012_outbox_generation_upgrade` 使用出箱表现有的 `account_approval_authorization_owner` 增加非空、默认零且非负的 BIGINT 代次，已存在的列必须符合该定义。该增量排在首个使用此列的 094 之前执行，之后恢复 `account_service` 角色。旧事件及原迁移记录均不重写。当前候选本身的 003 也不改动，空库沿原候选创建列，增量只核验定义。

Account migrator 完成后使用 `accountschema.VerifyApplied` 核验原始历史与完整新包。缺少新增桥接、未知旧名或 SQL 校验和、列定义不符均失败关闭。新增迁移为 forward-only。

## Token 018 与既有中继退役边界

原生产库的 `000018_event_replay_audit` 包含不可变重放审计。它作为嵌入的历史资源保留，只在已经安装该版本的数据库中加入期望集合，不给新库伪造 018。只有原始名称、原 SQL 的 LF/CRLF 校验和以及启用且原始函数体未改变的审计触发器均匹配时才接受。未知历史或缺失保护在新增迁移之前拒绝。

该函数体只抛出固定异常，不解析其他对象名称，因此允许原始未设置、仅 `pg_catalog` 和现网 RDS 恢复形成的 `public, public, pg_catalog` 三种已知 search_path；额外函数设置、条件触发器或不同函数体仍拒绝，不修改旧函数体或审计数据。

迁移器、Gateway 启动和发布准备工具共用这条专用历史规则；通用 `migrations.VerifyApplied` 仍拒绝额外版本。升级不删除审计行、原触发器或账本身份，原审计仍禁止 UPDATE/DELETE，硬化后也禁止 TRUNCATE。

当前版本按既有运行手册退役普通 Token Relay；兼容 018 只保留已安装的审计与账本，不重新启用已退役的进程、writer 或其数据库权限。上线前须按原发布准备与 writer inventory 流程停用/fence 现网残留的旧 Token Relay，并验证 Account/Billing 两个仍在使用的普通 Relay。不能为了迁移通过重新引入旧 Token Relay。

## 发布与验证

1. 记录当前不可变应用镜像、配置摘要和实际数据库版本，保留旧发布目录。
2. 以原始已发布 SQL 和独立临时 PostgreSQL 运行历史升级用例。测试使用空的专属临时实例，拒绝其他数据库/角色；原历史 SQL 通过显式 `PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY` 提供。Account 从 1..87、Token 从 1..34 的原始包升级，并核对重复 Up、完整 Verify、原行及账本不变、非法历史拒绝和审计不可变。
3. 生产迁移前使用独立备份身份完成近期备份并验证可读取，使用独立 migrator 及已配置 owner 角色，禁止将 runtime 临时提升为管理员。必要的旧库权限准备使用现有受控 operator 脚本，不能对已有数据库执行空库 bootstrap。
4. 检查生产 Compose，按实际服务依赖安排写入窗口，再运行三域迁移及专用后验验证。未验证的异常必须保留原始失败记录，不能自动改写账本或资金事实。
5. 更新固定摘要镜像后检查健康、错误日志、登录/会话、Token 与账户/计费关键路径及中继投递。镜像回退不撤销数据库迁移；涉及权限硬化时必须确认旧镜像仍能运行，不能恢复 runtime owner 来实现回退。

运行 `node scripts/account-platform-migration-manifest.mjs` 只证明当前包自身不可变清单一致，不证明它兼容生产已执行的 SQL。真实 PostgreSQL 用例缺少 opt-in 环境时会明确 skip，不能计为升级验收。
