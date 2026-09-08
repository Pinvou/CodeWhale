# 正式 Token 运行角色准备

状态：C4C3b1 文档、执行器、正式现场及精确清理均经独立审核通过，任务完成。
前置为 [C4C3a 只读清点](production-token-release-window.md) 完成并清理全部临时资源。
本页不授权直接启动业务、开放管理员 TCP、跳过治理或将测试策略用于正式环境。

## 当前事实与分步范围

执行前须再次核验，不依赖历史结果盲目重跑：

- Token 迁移完整账本为 31，治理 PRE_ACTIVATION/epoch=0，无历史 transition、无运行角色候选。
- token_gateway、token_migrator、token_policy_operator 当前 LOGIN，Token 会话为零；
  token_schema_owner、token_cap_policy_deployer 为 NOLOGIN。五角色均非超级用户、不能创建角色/数据库、
  不能复制或绕过 RLS；migrator/policy-operator 为 NOINHERIT。
- migrator 仅有 gateway、schema_owner、cap_policy_deployer 三项成员关系，均无 admin_option，
  inherit_option=false、set_option=true；本项不额外授予 pg_read_all_stats。
- Token 库及 public schema 均由 token_schema_owner 所有。Account 已有 115 项审批策略，
  该数量不替代原生策略集一致性检查。Token owner-cap 尚无有效版本与 revision。

拆为连续独立任务，每项文档/代码/实证均由 B 审核后独立提交：

1. **C4C3b1：关闭初始化 writer 并准备 NOLOGIN 角色。** 本批先实现，仅变更下述明确角色状态与原生
   prepare 所定义的权限，不生成密码、不修改 HBA、不初始化 cap、不激活治理。
2. **C4C3b2：正式身份材料与受限运维接线。** 独立设计两运行角色的 SCRAM 消费密码、有限有效期、
   到期提醒/轮换、加密离线保管，及非超级用户、非 runtime 的 Account 策略运维身份。
   只允许目标数据库/准确角色/实际来源私网/TLS+SCRAM 的 HBA 更新，保持其他连接与拒绝规则。
3. **C4C3c：显式业务策略、治理检查/CAS 与最终 LOGIN。** 每个 owner 的正式 Token Account 上限
   必须由负责人明确选择 1～1000；不得从测试 fixture 推导默认值。该选择尚待确认，b1 不依赖此数值。
   Account 策略检查、cap bootstrap/readiness、治理检查/激活、enable 与运维身份退场仍须分开受审执行。

## C4C3b1 需求与准确角色集合

新的两个 release-scoped 角色为 `token_credential_release_20260909`、`token_gateway_release_20260909`，
按此排序交给原生工具；已有同名角色时停止核验，不接管未知角色或生成新名称绕过半完成状态。
两者仅继承 token_gateway 运行权限，NOLOGIN、无密码，不授予 schema_owner、migrator 或 policy-operator。
Credential 单独使用前者，Gateway 与其 Relay 后续使用后者；业务秘密的实际投递仍属于 b2。

原生工具明确保护 migrator/policy-operator，不能通过 retired-roles 对它们执行 fence。
因此 b1 先在准确正式 PG 内，以既有本地 DBA 身份对这两个明确控制角色执行一次受控事务，
只改变 LOGIN→NOLOGIN，保留其余属性、成员关系与既有密码材料；然后才调用原生 fence 隔离 token_gateway。
没有现存会话时不传 terminate-sessions，也不创建通用终止连接工具。

## 受控执行顺序与失败恢复

1. 核验源制品、C4C3a 完成/清理回执、准确集群与九个原依赖健康、完整迁移/治理/角色/成员关系基线。
   所有候选角色必须不存在，Token 会话为零。对受影响角色检查全部其他可连接业务库的所有权、ACL、
   默认 ACL、成员继承与会话等跨库依赖；无法完整核验即停止，不能仅凭角色命名认定属于 Token。
2. 复用已验本机加密备份任务做新的操作前检查点，验证真实成功回执与加密 manifest；不重建数据库、
   不读取备份明文，不将准备回执当作恢复演练完成。本批完成后再做操作后检查点。
3. 为本批创建新的排他私有窗口，复用 C4C3a 已验的 private 父/FD Socket 导出与短时容器隔离方式。
   旧 C4C3a 执行器不得重跑。新的 Socket/管理员消费副本同样前后绑定，结束后精确清理。
4. 在一个 DBA 事务内，按顺序获取 release advisory（`hashtextextended('pinvou_token_release_prepare',0)`）、
   migration advisory（`hashtext('pinvou_account_platform_migrations')`）、schema_migrations 的 SHARE 锁，
   再锁 activation singleton 行 FOR UPDATE。手工事务的 advisory 使用 try，表/行使用 NOWAIT，并保留
   明确的有界 lock_timeout/statement_timeout；冲突或出现其他 Token 会话立即停止。
   锁内再次验证 PRE_ACTIVATION/epoch=0、零其他会话及两控制角色与预期基线完全相符，
   仅执行 `ALTER ROLE token_migrator NOLOGIN` 与 `ALTER ROLE token_policy_operator NOLOGIN`。
   不改密码、成员关系、角色所有权或其他数据库。提交后再次核对会话为零：NOLOGIN 不能终止已经建立的会话。
5. 从冻结原生 image 按顺序分别执行 fence 检查、fence apply、prepare 检查、prepare apply，
   每次使用准确新 CID，先 inspect 再启动一次；固定目标库、迁移目录、批准/维护引用、准确 retired 或
   candidate 集合，只有对应 apply 步显式传 `--apply`。fence 的唯一 retired-role 为 token_gateway。
   prepare 使用上面的两个候选，不改变既有原生跨库检查、全域锁、校验或角色保护。
   原生 prepare 还会重申既有 migrator GRANT、库/schema OWNER、USAGE/CREATE ACL 与 pgcrypto；
   前后核验要求这些重申没有计划外权限差异，不能把该命令描述为只执行两条 CREATE ROLE。
6. inventory 和前后 SQL 再核对：五控制角色与两候选均 NOLOGIN，无非 DBA 可登录 writer、无其他会话；
   两候选只具准确 gateway 成员关系、INHERIT/SET 且无 ADMIN，无治理/策略/schema CREATE 权限。
   原五角色除计划的三个 LOGIN 位外属性与成员关系未变；库/schema 原所有权保持。
   账本、业务表数据、cap 空状态、审批策略与治理记录未变，PRE_ACTIVATION/epoch=0 仍保持。
7. 独立验收后，只清理本批退出工具、临时挂载及 DBA 消费副本，保留正式原材料、两 NOLOGIN 角色、
   备份与完整回执。不可将 NOLOGIN 角色删除当作临时资源清理。

检查模式不传 apply，但原生 prepare 会取得必要锁，因此不能把它包装成只读事务而破坏锁语义。
原生 release advisory 和 activation FOR UPDATE 本身可能等待，由其既有 30 秒 context 约束；
不把手工维护事务的立即失败规则冒称为原生所有锁均 NOWAIT，也不在本项重构原生工具。
只读 inventory 保持自身只读事务与只读 DSN；写操作/锁检查使用本批明确受审连接，不开放 TCP DBA。
DBA 的两条控制角色 DDL 在原 PG 容器内通过既有本地 psql 执行；不在宿主运行新的 DBA 二进制。
原生工具镜像仍 network=none、UID999、两只读挂载、只读根、无 capabilities、资源与日志有界。

每个写入前记录唯一操作与阶段意图。事务提交/CLI 退出结果未知时，保留原操作标识与现场，
先用只读角色/账本/治理状态核验实际结果，不自动重试，不自动恢复 LOGIN，不换角色名称重来。
中途失败保留已安全隔离的 NOLOGIN 状态，禁止为“恢复服务”重开旧 writer；当前正式业务尚未启动。
任何超出准确角色集合的权限差异、跨库影响、已有业务会话或互斥行为选择须停止并请求决策。

## C4C3b1 正式执行证据

2026-09-09，源码 SQL 与冻结归档逐字节绑定的私有执行器经独立审核后只执行一次，正常退出：

- 手工事务的错误 epoch 无 DDL 负例被拒绝，正确 epoch 无 DDL 正例回滚成功，前后完整状态未变。
- 两控制角色 LOGIN 关闭，原生 fence 检查/apply 仅隔离旧 gateway；prepare 检查/apply 创建两个
  指定 NOLOGIN 候选，无密码，准确 gateway 成员关系，无超出预期权限。五个原生短时工具均正常退出。
- 最终原生 inventory 为 CHECKED / PRE_ACTIVATION / epoch=0 / migration_version=31，
  writer_logins=0、writer_sessions=0、applied=false、ENV_PENDING；不是业务已上线的证明。
- Token 全部 78 张业务/控制表的完整行集摘要、Account 审批策略行集摘要、完整迁移账本与治理记录未变；
  库/schema 所有者和既有 ACL、成员关系保持，仅有计划的三个 LOGIN 位及两个候选/成员关系新增。
- 操作前后真实本机加密增量备份回执和 manifest 已核验。九个原依赖健康，原 PostgreSQL Socket
  身份与可写源挂载保持；本批 Socket 导出只读、private、无秘密传播至其他命名空间。

独立审核者通过新鲜只读检查复核了全部五工具的真实隔离配置、退出/原生证据、完整预期差异、
两份备份 manifest、零会话和九项健康。随后受审清理器仅按五个准确 CID 非 force、无删除卷地移除
已退出工具，正常卸载本批准确子 mount、核空后删除 Socket 子目录，再正常卸载父 mount 并核验
全部命名空间均无残留，最后删除唯一 DBA 消费副本；原管理员文件、正式角色、数据、备份和回执
均保留。独立清理后复验再次通过。

未生成运行密码，未修改 HBA、cap 或治理激活，尚未开放正式业务登录；b2 与 c 不属于本项完成范围。
