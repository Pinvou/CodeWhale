# 受管 PostgreSQL 发布与既有治理升级

本文适用于已有 Token 库的独立运维控制面，不改变业务身份、历史 SQL 或迁移账本的兼容规则。生产执行须有明确部署授权、备份与可恢复证据、精确候选及独占维护窗口；本工具的 `ENV_PENDING` 输出不是上线许可。

## 原生与受管身份

`token-release-prepare` 默认 `--database-provider postgres`，继续要求原生超级用户和目录中的 SCRAM verifier。既有默认调用不接受受管参数。

阿里云 RDS 使用显式 `--database-provider aliyun-rds`，并分别提供 `--expected-dba` 与 `--expected-server-host`。连接仍从 `TOKEN_RELEASE_DBA_DATABASE_URL` 读取。登录名、实际连接主机和 TLS 证书名称必须精确匹配，拒绝备用主机或 TLS 降级配置；仅名称相似不够，还须核验 `pg_rds_superuser` 的受保护成员关系及实际 CREATEDB、CREATEROLE、BYPASSRLS 属性。业务和迁移身份不因此取得控制权限。

平台库 `rdsadmin` 不能按“连不上就忽略”处理。仅受管路径可以用集群目录核验其平台超级用户 owner、操作员及所有目标角色/可切换父角色均无 CONNECT、无该库 ownership/ACL 依赖、无会话。其余业务库仍逐一真实连接并核验共享边界；缺任何证据即拒绝。

所有 DBA 身份仍接受会话盘点：只有当前 PID 是例外，其他同名或受信管理员连接会阻断需要停写的步骤。受管管理员不能因为 `rolsuper=false` 被当成待退休业务身份，亦不能因受信而忽略其活动会话。

900001 存储硬化将普通领域表也移交 schema owner，enable 不能仅据此把正常业务 DML 判为控制面越权。启用检查按已发布硬化与后续迁移的 ACL 合同区分普通 DML、控制/投影只读、追加历史和 TRUNCATE 禁止；候选身份自身不能拥有数据库对象。原 018 的重放历史沿用已核验的原不可变触发器及原 ACL，不重写历史权限来伪造相同形态。

## 分开的准备、迁移和启用

继续依次独立执行 inventory、旧 CLI 的 SAFE、精确旧 writer fence、角色准备、前向迁移/cap 检查、治理 CAS、关闭短期运维登录、enable 和业务服务验收。不能把这些写操作作为服务启动的隐式前置。

若旧库向 PUBLIC 开放 Token schema CREATE，先在获批维护窗口由 DBA 收回，再重新盘点真实 writer。不能为通过清单检查而停用其他业务域身份，也不能恢复 runtime CREATE 来解决升级失败。schema owner 可单向继承业务组以处理既有对象；业务组不可反向继承 owner。

## RDS 的候选凭据

RDS 不允许读取 `pg_authid`。因此受管 `enable` 必须额外提供 `--candidate-credentials-file`，不能使用操作者填写的 `credential_ready` 或伪造目录结果。该文件：

- 为规范绝对路径的普通文件，不经过符号链接；POSIX owner 必须等于当前有效 UID，权限严格为 0600；最大 8192 字节。Windows 等非 POSIX 平台在具备可验证的等价 ACL 实现前明确返回 `PRIVATE_CREDENTIAL_PLATFORM_UNSUPPORTED`，不会按权限豁免读取文件。
- JSON 只接受 `version`、`host`、`database`、`expires_at`、`credentials`。version 为 1，host/database 与本次绑定精确相同。
- expires_at 是秒精度时间且不超过九十天；首次安装须至少剩余一分钟，已提交的只读重试仅要求尚未到期。到期前须通过既有受控凭据轮换流程更新，不能将其视为永久凭据。
- credentials 恰为按 role 排序的两项，每项只含 role/password；role 等于本次两候选集合。密码由受控设施生成，32～128 个可打印 ASCII 字符，不能放入 argv、仓库或日志。

文件应与运维身份一样置于受控载体；运行时只取得自己的凭据，不挂载该双角色控制文件。源文件的结构合法不证明服务器已认证成功。

默认检查不写密码或 LOGIN。显式 apply 在所有状态、权限、角色集合及会话检查通过后，为仍 NOLOGIN 的两候选生成随机盐的 SCRAM-SHA-256 verifier，在同一事务安装 verifier、有限有效期与 LOGIN。原服务器的 `password_encryption=md5` 不会降级该 verifier。

提交后使用本次事务已验证并安装的同一份内存凭据、TLS 主机和信任根分别真实登录，不重新读取准入文件或套用一分钟准入期限；要求完整 SCRAM 交互和强制只读身份查询，trust、明文和 MD5 认证方法均拒绝。同 epoch 两角色已 LOGIN 的重试只核验尚未到期的有效期及真实认证，不重新设置密码。混合 LOGIN 状态拒绝。

`TOKEN_RELEASE_ENABLE_COMMITTED_AUTHENTICATION_UNCONFIRMED` 明确表示已有启用提交或正在核对该提交，但认证未确认。不得据此启动服务、换一份密码盲重写或声称零副作用。先查询真实 epoch、角色、有效期和会话，保留原凭据；修复网络/HBA/挂载后只读确认同一提交。需要撤回时先按匹配当前摘要的治理 CLI 完成 SAFE，再显式 fence 该代角色并清退会话。数据库提交不等于客户端收到或业务上线成功。

## 精确策略扩展

`token-governance --policy-upgrade billing-policy-set-v1` 只适用于明确选择的 SAFE_ROLLBACK→ACTIVE。它不为运行时增加摘要别名，不允许 ACTIVE 直接改摘要，也不修改旧转换审计。

该合同同时固定原二十二项策略摘要、新三十项完整策略摘要及原集合在当前源码中的精确等价性；新增仅为八项计费策略。handler 摘要仍须与旧持久证据完全一致。未知摘要、原策略变化、缺项或未来扩展均拒绝。没有该显式选项时继续执行原有严格摘要校验。

操作仍由专用 token_migrator 执行：锁定并核验 Account 的完整新策略集合，保持该 SHARE 锁至 Token 操作结束；Token 侧锁 activation、核对精确 epoch、锁定领域表并盘点 writer，然后通过既有 CAS 和真实触发器产生新 epoch/审计。旧 CLI 应先以匹配旧摘要的制品完成 SAFE。原 request-id 查询用于确认未知结果，不能盲重 apply。

model-token operations 包装器仅在 `governance-active-check` / `governance-activate` 且 expected_state 为 SAFE_ROLLBACK 时接受可选 `policy_upgrade: billing-policy-set-v1` 并透传该 flag；旧六字段请求不变。未知契约、首次 PRE 激活、SAFE 与结果查询均拒绝该字段。默认仅计划检查，写入仍须显式 execute，绑定既有批准引用和精确可执行制品摘要。受管 provider 参数由独立发布准备 CLI 显式提供，不混入治理包装器的凭据。

## 验证边界

Linux 本地 Docker 的强制验收入口为 `python3 scripts/account-platform-deployment-contracts.py --artifacts <新的私有绝对目录>`。它不接受已有数据库 URL，也不开放宿主端口：两个带本次随机标签的 tmpfs PostgreSQL 与测试进程共享各自独占的无外网网络。仅工具构建/锁定依赖准备联网；需要 Docker、Python 和拉取固定镜像的网络。工具镜像由仓库 `test/Dockerfile.deployment-control` 构建。执行后只清理本次创建且标签匹配的容器。

默认 PostgreSQL 17；使用 `--postgres-version 18.4` 验证已固定镜像的 18.4 场景。参数仅接受这两个已登记配置，分别核对实例实际主版本或精确 `server_version_num=180004`，并将两实例的真实版本写入回执。required CI 对两个配置都执行完整载体，共享当前 job 的校验过的 Go 模块/构建缓存。现场目标版本改变时须另行登记、固定镜像并重新验证，不能把某个大版本的结果冒充另一版本或供应商内核的结果。

验收载体从当前源树创建隔离副本，封存源码、真实治理 CLI、镜像及原始 JSON 输出摘要；从冻结的原发布 up SQL 清单重建 87/34 两包，原 003/018 使用保留原字节，其余文件逐项验 SHA，不依赖私有历史目录或网络 Git ref。Go 验证必须命中指定 roots、父子结果全 pass 且无 skip，包装器→真实 CLI 的 check/apply 也实际执行。required Account Platform CI 调用同一入口；只有现场收到对应提交的实际结果才能声明远端 CI 通过。

历史场景包括原 Account 3/50/51/87 阶段及 Token 1..34；RDS 场景同时覆盖新库与原 Token 历史、显式身份/主机、跨库隔离、managed fence/prepare、禁止的控制/审计/截断/owner 权限、凭据准入、提交后认证失败与同提交恢复、策略锁/CAS/原结果和实际运行身份消费。原发布摘要状态通过真实触发器构造夹具，不冒称执行了旧生产 CLI；治理迁移账户的 loopback trust 不证明现场 HBA，候选凭据的 SCRAM/TLS 则由真实协议探针分别证明。

隔离真实 PostgreSQL 验证公开 Account/Token 迁移、非超级用户操作、真实 TLS/SCRAM、跨库 ACL、提交后认证失败/恢复以及治理锁/CAS/审计。原生 PostgreSQL 无法创建保留的 `pg_` 角色；专属空实例只模拟供应商预装角色的目录身份，实际权限、成员链及 SQL 行为仍由 PostgreSQL 执行。这不等于在真实 RDS 上验证其内核特有能力。

真实 RDS 的平台属性、控制权限、现有配置与服务验收须分别以现场证据闭合。远端 CI、合并资格、生产迁移和发布资格分开记录，不能继承旧候选或仅凭测试总数宣布通过。
