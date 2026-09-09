# 正式 Token 策略初始化与治理激活

状态：C4C3c 需求、设计与四项任务拆分已独立审核通过；c1 已完成并提交；c2 因正式数据库编码前置不满足暂停，c3–c4 尚未执行。
前置为 [C4C3b2](production-release-credentials.md) 完成：正式凭据与保管、精确 HBA、真实 TLS/授权检查、
临时运维登录退场及测试资源清理均已独立验收。负责人已明确本次上限为 **100**，不使用测试默认值。

## 需求和边界

- 将现有 `max_per_owner` 正式策略初始化为 100。保持既有所有者计数口径：按 `owner_type + owner_id`
  计算 PLATFORM 管理账户的 ACTIVE/DISABLED/FROZEN 数量，并计入 PENDING/MANUAL_REVIEW 预占。
  USER、ORG、ORG_UNIT 使用各自所有者桶；不是 Token 用量、请求次数，也不新增组织与其全部节点合计配额。
- 沿用冻结并验证过的原生 policy-bootstrap、governance、release-prepare 制品与已安装迁移，禁止重建数据库、
  重跑迁移、直接 UPDATE 激活状态或手工插入策略版本。只有明确的本阶段状态差异允许写入。
- 所有临时权限按阶段开启、验证、关闭。Account 策略检查必须使用独立的受限运维身份，不能使用
  `account_service` 业务身份；不放开 DBA TCP、不新建共享密码，不把运维凭据挂载给业务容器。
- 保持旧 `token_gateway` 及全部受保护所有者 NOLOGIN；只有当前两个准确候选能够在治理 ACTIVE、
  正确 epoch、有限未过期 SCRAM 和正确权限均验证后，由原生 enable 开启登录。
- 此阶段不启动业务容器、不切换公网、不创建首个业务管理员，也不代表 New API 或商城对接已经完成。

## 共同执行约束

在每个实际写入阶段前保存新鲜真实加密备份及精确前置快照，完成后核对允许差异并保存后置备份。
完整保留其他领域表、权限、全局角色和密码摘要；Token 全部表均纳入检查，不能只比较本次目标表。
每次工具执行记录固定制品摘要、参数、身份、退出结果与回执；进程与容器禁止 core dump，错误日志不回显秘密。
沿用已验证的 URI 百分号编码、显式 PGAPPNAME、逐工具总超时以及精确退出资源清理，不复制已修复的测试缺陷。

DBA 原生检查需要本地 Socket 时，复用已验证的 FD/mount 身份绑定隔离方式，在本阶段独立目录创建临时
只读 Socket 导出和唯一消费副本；工具不挂载数据库数据、Docker Socket 或管理员材料父目录，不开放网络。
核对原数据库 PID、mount/inode、容器/集群身份与制品，使用新回执，不能重新执行已经完成的旧窗口脚本。
结束后精确清理本阶段工具与导出，并检查其他 namespace 和消费者，不删除正式数据或材料。

当前 HBA 和连接证据针对真实 data network 中的受控工具。生产 Credential 的 credential-control、治理
profile 的 governance-operations 是不同网络，本阶段不能把临时工具路径视作最终业务链路通过。
后续创建并核验这些网络真实 ID/CIDR 后，必须再按准确角色和目标库配置对应 hostssl 规则并从目标网络实测，
作为业务启动的必需门禁，不能仅留为说明或用 data 网结果替代。
不得为省略接线将 Credential 并回通用 backend、使用 all 地址规则或开放公网数据库端口。

未知提交结果先按原请求 ID、幂等键摘要及真实状态查询，不重新生成请求来覆盖结果。发生失败时优先关闭
本阶段临时运维 LOGIN、确认零会话并保留状态；业务事实已提交时不删除版本、历史或 outbox 来“还原测试”。
账号运维密码仍受原安装后 24 小时到期值约束；超期必须单独处理，不能静默延长。

## 任务拆分与验收

### C4C3c1：治理工具的专用 Account 身份接线

生产业务 Compose 的治理工具 Account DSN 从硬编码 `account_service` 改为必填
`ACCOUNT_POLICY_RELEASE_ROLE`；保持 Account 数据库、verify-full、CA、独立治理消费目录及运维 profile。
部署检查器校验该角色是专用 `account_policy_release_` 命名的合法 PostgreSQL 标识符，且与两个 Token
运行角色不同；不允许改用业务、迁移、内置或超级身份。运行时仍须核验真实角色权限，命名不能代替授权证明。
补充成功、缺失、非法/注入、业务身份、跨域复用等合同测试；同步接线文档。本任务不改变服务代码或镜像，
不启动治理工具，独立审核并提交后进入 c2。

验证记录：治理接线专项合同测试 15/15 通过；全量合同测试共 531 项，529 通过、0 失败、
2 项既有 opt-in 测试跳过；架构守卫与差异空白检查通过。独立代码审核通过。
本次仅修改部署接线、检查器、测试和文档，未执行正式策略写入、治理激活或业务启动。

### C4C3c2：初始化账户上限为 100 的正式策略

1. 精确确认当前只有一个未激活指针行，其 `active_policy_version` 为 NULL、pointer_version 为 0，
   revisions 为空；不是把“指针表有一行”误判为已有正式策略。确认无业务创建者、预占及待处理创建任务。
2. 全部非超级写身份 NOLOGIN 时，通过原生 DBA `quiescence` 检查维护屏障；检查工具退出后，屏障仍通过
   关闭的运行身份和逐阶段角色核验保持。不得为通过 quiescence 临时放宽检查器或开启运行角色。
3. 仅短暂开启现有 `token_policy_operator`，用其已有独立密码和 TLS 执行原生 bootstrap：max=100、
   expected-version=0（对应 NULL）、固定本次幂等键、规范原因；不授予 schema/表写或新成员关系。
4. 立即关闭 policy operator 登录并确认无会话，再用原生 DBA readiness 读取策略；不能假定仅有专用函数
   EXECUTE 权限的 operator 可以直接 SELECT 策略表。核验版本 1、上限 100、真实配置摘要与唯一变更事件。
5. 只允许新增一条 policy revision、一条对应 outbox event、更新唯一 active pointer；其他表、Account 策略、
   治理 PRE_ACTIVATION/epoch=0、角色和 ACL 保持。完成前后备份、临时资源清理、独立审核及单独提交。

执行记录：专项后置差异测试 7 项通过，新组合只读预检与独立审核通过；正式前置加密备份完成，
原生 quiescence 通过。bootstrap 返回 SQLSTATE `42601`：Unicode normalization 要求 server encoding
为 UTF8。现场独立检查确认所有新建数据库实际为 `SQL_ASCII`；初始化脚本导出的 `LC_ALL=C` 与
未显式指定 `--encoding=UTF8` 的初始化参数共同形成此缺口，既有健康检查未覆盖编码要求。

本次策略未提交：revision/outbox 均为 0，唯一指针仍为 NULL/version 0；临时 operator 已关闭、无残留
领域会话，全部角色、密码摘要、78 张 Token 表、59 张 Account 表及权限、函数与序列状态与前置一致。
失败窗口已按独立审核的退场流程移除两个退出工具、本窗口 Socket 导出及三份临时消费副本，
保留全部正式材料、数据库、备份和原始回执；退场后全量数据与前置一致，清理独立现场终验通过。
本任务尚未完成，不以重跑原脚本恢复。
不得绕过 NFC 校验、修改冻结函数或直接重建现有正式数据库。负责人已批准按
[正式 PostgreSQL UTF8 保全迁移与切换](production-postgres-utf8-cutover.md) 执行并行影子集群方案；
完成初始化合同、流式保全迁移、等价验证、备份恢复与受控切换后，再恢复本阶段。

### C4C3c3：原生治理 CAS 激活

1. 当前 cap 已 ready、候选仍 NOLOGIN，原生治理仍为 PRE_ACTIVATION/epoch=0；核验准确两个候选从未用于
   历史 ACTIVE，并确认原处理器/审批策略摘要对应同一冻结制品。
2. 在 `token_migrator` 仍 NOLOGIN 时，仅临时授予其 pg_read_all_stats 成员关系，明确 ADMIN FALSE、
   INHERIT FALSE、SET TRUE；保持角色本身 NOINHERIT 和原三项受保护成员关系。该权限只用于本治理窗口，
   不授予 runtime。然后仅开启 migrator 与既有受限 Account 运维身份，使用各自独立密码及真实 TLS。
3. 使用原生 governance 执行只检查与目标 ACTIVE 的 dry-run，绑定准确 expected-state/epoch、排序候选名单、
   稳定 request ID、可追溯操作人与原因；检查通过才执行同一语义的 apply。
4. 由原生工具持 Account 策略 SHARE 锁并冻结 Token 领域写入后完成 CAS。核验 ACTIVE/epoch=1、准确摘要、
   两候选名单与唯一不可变 transition；这不是跨库原子业务事务。未知结果先查询同一 request ID。
5. 无论成功或失败均关闭临时 migrator/Account 登录、确认零会话，撤销本窗口新加 pg_read_all_stats。
   只允许 activation 一行及对应 transition 新增；cap 与其他全部业务事实不变。备份、精确清理、审核及提交。

### C4C3c4：开启新运行身份并交接业务启动

1. 先确认 cap ready、治理 ACTIVE/epoch=1、精确两运行身份及其原密码/有效期、专属消费目录和已验 HBA；
   migrator、policy operator、Account 运维、旧 gateway 与受保护所有者均保持 NOLOGIN。
2. 通过冻结原生 DBA release-prepare 对准确两角色执行 enable check，再执行 enable apply；不能用直接
   ALTER ROLE LOGIN 代替原生权限/治理校验。仅两角色 LOGIN 允许变化，保持其他属性、成员及有效期。
3. 从隔离内网工具使用各自实际消费文件实测 verify-full 登录及身份；核验不能获得 Account/Billing 跨库访问、
   schema CREATE、受保护策略/激活表写权限；错误密码和旧 gateway 仍拒绝。拒绝测试不得产生业务数据。
4. 若 enable 已确认提交，而后续 TLS/权限验收失败，在本阶段尚无业务容器的前提下，使用独立受审的
   DBA 紧急 NOLOGIN 事务收口：绑定准确两个候选及其 OID、ACTIVE epoch、零非本工具会话，仅关闭其 LOGIN，
   再核验零残留连接。保留 cap、ACTIVE、transition 和全部证据，不删除业务事实，也不自动转为 SAFE。
   原生 fence 在 ACTIVE 会拒绝，不能冒称该收口由原生 fence 支持；上述只关登录的例外需执行器独立审核。
   enable 提交结果未知时先查两角色及原回执，不重复 apply、猜测退场或忽略混合状态。
5. 前后备份与完整不变量核验，精确退出 DBA/连接测试工具和全部临时导出/消费副本。交接真实运行角色、
   epoch、cap 版本与现有有效期监控，不重新生成凭据、不放开运维身份。
6. 独立审核并单独提交后，进入业务实例配置、New API/商城、安全凭据服务与公网入口联调。治理或数据库
   LOGIN 成功不等于业务健康、公网可用或部署已全部完成。

每个任务均由原实施者修改、独立审核者复核，发现问题修复后再审核，通过后独立提交。
继续沿用全部任务完成后统一推送的安排。
