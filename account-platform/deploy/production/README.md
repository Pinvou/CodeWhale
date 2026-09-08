# 生产依赖配置（尚未正式投递）

此目录只包含公开部署制品，不保存 `.env`、密码、证书或恢复包。独立使用
`compose.dependencies.yaml`，**不与开发 Compose 合并**。当前交付 PostgreSQL 配置及同目录
独立 NATS `compose.nats.yaml`；Redis、Keycloak、业务发布和数据库备份恢复
仍需各自验收。

## PostgreSQL 投递约束

- `POSTGRES_IMAGE` 使用独立核验的 PostgreSQL 17 不可变镜像摘要/本机 image ID，
  预先导入，禁止 latest/可变 tag 和运行时拉取。缓存 17.10 仅作为本批隔离配置验证制品；
  正式发布须核对当时维护补丁与安全公告，不能把缓存版本叫作最新版。
- `POSTGRES_DATA_DIRECTORY` 必须是本次全新且空的独立目录；不得使用旧测试数据目录，
  更不得指向 KMS 或其他团队数据。固定 UID/GID 999:999、0700。
- `POSTGRES_TLS_DIRECTORY` 为本次已核验证书包的 PostgreSQL 单独叶目录，
  999:999/0700；`server.key` 为 0600，证书 SAN 为 `postgres`。不挂根私钥或其他叶私钥。
- `POSTGRES_ADMIN_DIRECTORY/password` 与 `POSTGRES_BOOTSTRAP_DIRECTORY` 下的
  八个文件都为 999:999/0600、目录 0700。每个文件保存不同的 32 字节随机值的小写 hex
  （64 字符，可有一个结尾换行）。管理员密码不传入 Compose 环境或命令行。

八个引导文件名：`ACCOUNT_DB_PASSWORD`、`BILLING_DB_PASSWORD`、`TOKEN_DB_PASSWORD`、
`BILLING_MIGRATION_DB_PASSWORD`、`TOKEN_MIGRATION_DB_PASSWORD`、`TOKEN_POLICY_DB_PASSWORD`、
`NEWAPI_DB_PASSWORD`、`KEYCLOAK_DB_PASSWORD`。由既有受控秘密投递流程生成/备份，
此配置不生成秘密、不读取 KMS custody、不自动删除数据。

公开环境文件只设置上述镜像和目录位置。在核验来源、摘要及预置权限后使用独立项目：

```sh
docker compose --env-file /controlled/public/dependencies.env -f account-platform/deploy/production/compose.dependencies.yaml config --quiet
docker compose --env-file /controlled/public/dependencies.env -f account-platform/deploy/production/compose.dependencies.yaml up -d postgres
```

正式投递归 SECINF-04C2D，以上不是已执行记录。私有数据网络自动创建且不发布任何宿主端口。
后续业务只连接该私有网络，DSN 必须包含 `host=postgres`、`sslmode=verify-full` 以及
该客户端只读挂载的公开 `sslrootcert`；不同域使用不同数据库身份。

## 初始化与异常处理

复用原有五库/八登录角色/四 NOLOGIN 角色定义，禁用模拟器；业务进程无迁移身份。
只通过文件将密码引入初始化进程环境，psql 使用 `\getenv`，不把密码传为 argv。
官方入口仍会暂时在进程内使用管理员密码，宿主 root/数据库 OS 用户在信任边界内；
不得打印环境、执行 shell tracing 或启用记录 SQL/参数的调试日志。

初次建库全成功才写 `.pinvou-bootstrap-complete`，绑定集群 system identifier。
已有 PG_VERSION 却缺少或错配标记、部分 initdb 残留、已有业务角色/库均失败关闭。
禁止人为补写标记或删除数据来“修复”正式失败；保留现场，核对原因再受控恢复。
容器可能按 restart 策略重复拒绝，这不是就绪状态。

完整验证并确认密码已投递给各自服务且有受控恢复副本后，精确移除引导目录中的八份副本，
保留空目录、管理员文件和恢复包；成功集群此后重启无需八份引导副本。删除副本不撤销数据库权限。
角色/密码后续变更走独立受控流程，不重跑初始化脚本。健康检查验证标记与实际本机 SQL，
但不替代客户端 TLS、权限、业务迁移及备份恢复证据。

## 隔离验收

Linux 测试服务器已缓存固定测试镜像且安装 Docker Compose/OpenSSL 后，明确执行：

```sh
python3 scripts/account-platform-postgres-fixture.py --run
```

只创建随机名称的独立项目/私有网络/临时数据与测试秘密，不接受现有数据库连接信息。
验证真实生产配置、八身份 SCRAM/TLS、错误 CA/主机名/明文/跨库/错密码/身份提升拒绝，
初始化失败重启拒绝、错绑定拒绝、删除引导副本后重启持久化及日志不含密码。
结束仅清理该 fixture 的确切容器 ID、私有网络和临时目录；不能将其通过报告当作正式部署。

## NATS 投递约束

`compose.nats.yaml` 可独立验证，也可与 `compose.dependencies.yaml` 合并到同一个
`pinvou-production-dependencies` 项目；禁止混入开发文件。`NATS_IMAGE` 同样必须为
独立核验的不可变制品，预先导入且禁止运行时拉取。

- `NATS_DATA_DIRECTORY`：全新独立数据目录，65532:65532/0700，不能复用旧测试 JetStream。
- `NATS_TLS_DIRECTORY`：只含 NATS 单独叶证书/私钥，DNS SAN 为 `nats`，目录 0700、
  文件 0600、属主 65532:65532；不挂其他服务私钥、根包或 KMS 材料。
- `NATS_AUTHORIZATION_FILE`：既有 `event-resources -render-authorization` 的固定六公钥
  ACL 产物，逐制品可信摘要和完整身份包核验后才投递，65532:65532/0600，只读挂载。
  NATS 不持有任何客户端种子或恢复包，不能手工添加无身份用户/通配管理身份。

客户端使用 `tls://nats:4222`、专用 `NATS_CA_FILE` 和自身 `NATS_NKEY_SEED_FILE`，
不能用全域共享凭据。受控初始化窗口单独投递 `event-resource-operator` 种子，执行已有
`event-resources -create-only` 后再执行默认只读检查；失败保留资源，禁止 delete/update 重试。
完成后关闭初始化连接并精确撤除临时种子副本，不能宣称删除文件即可撤销已有连接。

监测 HTTP 仅监听容器自身 `127.0.0.1:8222`；健康检查证明本机 JetStream 响应，不代表
业务身份/资源就绪。Portal 不再直接访问 `http://nats:8222`；其运行状态证据在业务接线时
改为受控检测结果，未接通时报告未就绪/未知，不开放无认证监测来恢复绿色状态。
JetStream 本批最大文件存储 2GB、内存存储 128MB；满容量会拒绝新写入，不因容量不足自动
purge/delete；原有 30 天保留策略不变。
业务上线前必须完成 C2D 容量、证书到期、持久数据备份/恢复检查。

隔离联验命令（目录只放已核验的 Linux `age-keygen`、`nats-identities`、`event-resources`
公开二进制；前者从既有工具模块依赖的官方 age 构建，后两者使用已审核代码）：

```sh
python3 scripts/account-platform-nats-fixture.py --run --bin-directory /controlled/public/test-binaries
```

该测试只生成临时恢复身份和 NKey 包，启动真实生产 Compose 模板。验证精确 TLS/无身份/明文
拒绝、独立错误 CA、非本机监测拒绝、受限实际初始化和重启后只读复查；不会连接正式 KMS
或使用正式 seed。已有固定 ACL 的业务收发/撤权矩阵沿用 C1/B1/B2 已通过证据。
