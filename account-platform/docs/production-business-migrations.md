# 全新业务库迁移窗口

> GSA-04 兼容增量（尚未部署）：后续含 Account `000050` 的迁移窗口必须使用
> `account_migrator LOGIN NOINHERIT` 登录并保持 `MIGRATION_ROLE=account_service`；
> 该身份是 `account_service` 与 `account_approval_control_owner NOLOGIN NOINHERIT` 的成员，
> 但不具有 SUPERUSER/CREATEDB/CREATEROLE/REPLICATION/BYPASSRLS。DBA 只在窗口前临时
> 授予目标 Account 数据库 `CREATE`，成功迁移会撤销；失败事务会恢复外部前置授权，
> 因此失败 runbook 必须先显式撤销该授权再调查。下文 C4C1a/C4C1b 的
> `account_service` 登录和实测数字是 2026-09-09 已执行窗口的历史证据，不得回写成
> 本次尚未部署的结果。

状态：C4C1a 模板已交付；C4C1b 正式三库迁移、前后备份及独立现场验收已完成。
承接 `production-business-cutover.md`，不启动长期业务、不切换公网。
保留既有正式 PostgreSQL、Keycloak、KMS、身份与备份；不触碰旧测试环境、其他团队服务或恢复材料。
模板/合同先独立审核与提交，正式投递/执行脚本另外审核；完成迁移后再提交实际验收记录，连续推进两步。

## 目标与执行边界

本项只对已经正式初始化、尚无业务表的 Account、Billing、Token 三库应用已构建制品中的既有迁移。
New API 业务表由后续固定版本自身初始化，本项不向它应用账号平台迁移；Keycloak 库已正式使用，不重新迁移。
2026-09-09 只读基线已确认上述四个业务库非系统表均为零；该历史结果不能替代执行前再次检查。

复用 C4B1 的精确 `migrate` image ID 与镜像内 `/migrations`，不临时改 SQL、额外手工改角色授权、重建依赖或拉取镜像。
既有 Billing 000053、Token 000029/000031 SQL 中的 SET ROLE/GRANT/REVOKE 是迁移本身的预期效果，
按已构建原文执行；上述限制不是禁止这些既有语句，也不允许为消除错误而临时补授权。
只新增独立 `compose.business-migrations.yaml`，不和业务/开发 Compose 合并；三个工具均需显式
`business-migrations` profile、restart=no、禁用健康检查，没有 depends_on 自动拉起关系。
必须逐域点名执行，Account 成功后再 Billing，最后 Token，不使用一次 up 全部的方式。

| 实例 | 目标库 | 登录角色 | MIGRATION_ROLE | 明确命令 |
| --- | --- | --- | --- | --- |
| account-migrate | account_platform_account | account_service | account_service | `/service --dir /migrations/account --direction up` |
| billing-migrate | account_platform_billing | billing_migrator | billing_service | `/service --dir /migrations/billing --direction up` |
| token-migrate | account_platform_token | token_migrator | token_gateway | `/service --dir /migrations/token --direction up` |

这是现有初始化时已经定义的迁移角色关系，不增加 superuser、跨库授权或新运行账号。
Token release-scoped 运行角色的准备、旧 writer 隔离、HBA 更新和启用单列 C4C 后续任务，本项不绕过该门禁。

## 配置和秘密

三个工具只接已验收正式 data 内网，无公网出口、宿主端口、Docker socket、workload 或 NATS/KMS 身份。
UID/GID=65532:65532、read_only、cap_drop=ALL、no-new-privileges、core=0、256 MiB/0.5 CPU/96 PIDs、
有界私有 noexec tmpfs 与日志，pull_policy=never；不向现有 PostgreSQL 添加网络或更改 HBA。

每工具一个独立绝对目录（65532:65532/0700），仅含自己的 `pgpass`（0600），只读挂到
`/var/run/secrets/pinvou/database`；从既有正式对应域密码材料派生精确单条记录，禁止通配符。
公开基础设施 CA 单独只读挂到 `/var/run/secrets/pinvou/trust/ca.crt`，不含叶私钥。
无密码 DSN 精确固定角色、目标库、postgres:5432、sslmode=verify-full、sslrootcert；
PGPASSFILE 指向 `/var/run/secrets/pinvou/database/pgpass`，不把密码写入 env、参数、SQL 或日志。
这些是既有密码的独立消费副本，不生成或轮换数据库密码，不重复请求离线保管确认。

配置明确设置 `PINVOU_DEPLOYMENT_ENVIRONMENT=production-new`，保留现有迁移程序的备份门禁。
`MIGRATION_BACKUP_EVIDENCE_ID`、`MIGRATION_BACKUP_COMPLETED_AT`、`MIGRATION_BACKUP_SHA256`
必须来自本窗口真实完成的加密备份；不填写构造值或把普通文件摘要冒充备份。
现有 `productionBackupEvidence` 只验证格式与 24 小时时效，不自行证明备份来源；外层执行器必须核验
已验收驱动摘要、同一实际集群、真实 backup label/完成时间及该 label 的密文 manifest SHA-256。

## 顺序、回执与失败处理

1. 核对精确 PostgreSQL 容器/image ID、集群 system identifier、PGDATA 与只读基线，确认三库无业务表、
   无迁移账本；Keycloak/签发代理健康、磁盘至少 8 GiB、无其他本窗口迁移/未确认容器。
   同时只读核验现有 migrator NOINHERIT、所需 deployer/schema-owner membership、各库/public schema
   owner、Billing/Token runtime 不继承 owner/deployer 及初始 schema CREATE 权限；Account 保持现有
   account_service 本库所有者设计，不套用上述 Billing/Token 隔离断言。任何前置角色不符即停止，
   不自动补授权或扩大登录范围；Token pgcrypto 必须已由初始化脚本安装，Account 的对应扩展由其本库
   owner 按既有迁移安装，不让 Token runtime 临时成为库 owner。
2. 使用已验收的本机加密备份驱动执行一次增量备份，等待实际完成并复核该集群、WAL 与完整链；
   绑定最新 backup label、完成时间和该备份实际加密 manifest 摘要。备份失败或来源不符不运行迁移。
3. 独立审核私有投递/执行脚本后，创建新窗口目录和逐消费者 pgpass，核对父目录、软链、属主和权限，
   冻结公开 Compose/环境/image/migration 清单摘要。配置只在受控内存验证，不输出秘密文件内容。
4. 逐域先写 exclusive planned 回执，创建唯一命名、尚未启动的准确容器，记录 CID 并核对其实际配置；
   再启动该容器，等待真实退出。只允许接既有 data 网，不影响现有数据库或身份容器。
5. 每域必须退出 0，且读取现有程序输出 `migration.post_verification` 的准确域、版本、应用数量、
   最新校验和与同一备份引用。另以只读 SQL 核对完整 schema_migrations 行与镜像内源清单一致，
   同时核对 Billing/Token runtime 的 public CREATE 已被既有迁移撤回；不能只根据容器退出、表已建立
   或最后版本号宣布完成。审核成功后写该域 completed 回执，再继续下一域。
6. 三域完成后复核 Keycloak/签发代理和备份归档健康，执行新增量备份，保存迁移后检查点。
   只在回执完整、容器已经退出且独立复核后按准确 CID 移除本窗口迁移容器；保留配置、密码副本和回执，
   不自动删除正式数据、备份或已经完成的迁移账本。

出现非零退出、超时、输出不完整、状态未知或任一校验不一致时立即停止后续域并保留现场。
现有迁移每版本一个事务，不是三库原子事务；部分成功不能通过删除数据库、down 或重跑整套脚本掩盖。
后续恢复必须先查实际已提交账本及失败容器，再独立审核窄范围续跑；只确认已经完成的域不重复执行。
迁移程序现有总 context 为两分钟，外层等待必须将其非零退出/超时作为失败保留，不修改该期限，
也不能自动重试或把部分账本更新解释为成功。任何工具超时后的退出未被确认之前，不开始新迁移或移除该容器。

## 验证与后续

实施前先跑模板正/负合同，覆盖精确三实例/角色/库、profile、无隐式依赖、TLS/PGPASSFILE、
无秘密环境、固定镜像、隔离挂载/网络和备份元数据必填。公开 fixture 经真实 Compose 渲染，
静态检查与运行门禁分别记录；静态通过不证明数据库连接、备份可恢复或迁移已执行。
正式迁移使用实际 pgx 消费 PGPASSFILE，保留其原有校验与备份门禁，不更换迁移引擎。
本项完成后还需 NATS 固定资源、新 release 运行身份、治理激活、首次业务管理员和业务启动，不能称为完整上线。

C4C1a 实测：五组模板正/负合同与十三组业务配置回归通过，无跳过；另外核对原模板资源/网络值，
避免 fixture 的标准化过程掩盖原值被修改。真实 Compose 渲染包含三个工具，五项必填输入缺失均拒绝；
实际 CLI 返回 MIGRATION_CONFIG_VALID / ENV_PENDING / migrations_executed=false，无容器或数据库连接。
272 份迁移文件的不变性检查通过。正式集群的只读角色、所有权、初始 CREATE 和 Token pgcrypto 前置检查通过，
但执行器仍必须在窗口内重新检查，不能复用历史结果替代。此阶段当时尚未投递迁移密码副本或执行业务迁移。
最终全量合同 529 项中 527 通过、0 失败、2 项既有 opt-in KMS Compose/Helm 测试未启用；
架构与差异检查通过。该阶段结果不代表正式迁移已经执行。

### C4C1b 正式执行结果

私有准备脚本和执行器分别通过独立审核；准备阶段重新核对实际集群、十个相关角色、三库空账本、
固定镜像与全部迁移源摘要，完成新的真实加密增量备份后投递三个独立 pgpass 消费副本。
只下载不含秘密的实际 Compose 渲染与库存供已签名检查器验证，通过后将准确摘要绑定到一次性执行器。
回执和新目录执行 fsync；备份前后核对既有驱动与配置未变，保留容量 warning，不将其报告为无预警。

账号、计费、Token 三库依次一次执行，均退出 0：分别核对 49、52、31 条完整迁移账本，
最新版本分别为 49、53、31（计费版本存在既有编号间隔，不将最大版本号当成执行数量）。
每域均核对程序 post-verification 的版本、完整校验和与同一实际备份引用，以及准确正式 data 网络 ID；
计费与 Token 的 public CREATE 权限均已被既有迁移正常撤回，没有追加手工授权或更改既有 SQL。
执行器没有发生重试，三个工具容器保留为已退出状态供独立审核；未启动长期业务或切换公网。
最后再次通过既有身份/数据库健康检查，并完成迁移后的新增加密备份。New API 与 Keycloak 库未应用本项迁移。

独立审核者再次读取完整三库账本、精确容器/网络与两个备份 manifest，复核九个原有容器健康及无备份 inflight，
全部通过。随后仅按回执中的准确 CID 移除三个已退出工具容器，未使用 force 或删除卷；配置、密码副本、
回执、正式数据及前后备份全部保留。本项完成不代表 NATS 固定资源、release 运行身份、业务服务与公网接入已经完成。
