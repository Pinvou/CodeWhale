# 生产依赖配置（尚未正式投递）

此目录只包含公开部署制品，不保存 `.env`、密码、证书或恢复包。独立使用
`compose.dependencies.yaml`，**不与开发 Compose 合并**。当前交付 PostgreSQL 配置及同目录
独立 NATS `compose.nats.yaml`、隔离缓存 `compose.redis.yaml`；Keycloak、业务发布和数据库备份恢复
仍需各自验收。

## PostgreSQL 投递约束

- `POSTGRES_IMAGE` 使用独立核验的 PostgreSQL 17 不可变镜像摘要/本机 image ID，
  预先导入，禁止 latest/可变 tag 和运行时拉取。2026-09-08 已用 17.11 重跑隔离配置验证；
  各版本及独立摘要见安全设施文档 D3 前置验收记录，不能把历史缓存版本叫作最新版。
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

## Redis 投递约束

`compose.redis.yaml` 必须同时提供独立 `redis`（Portal、Identity）与 `newapi-redis`
（New API）实例，不能以不同数据库编号替代隔离。后者的缓存目录功能需要 SCAN，不能
与登录会话共库。使用预先独立核验的不可变 `REDIS_IMAGE`；2026-09-08 已用 8.2.9 重跑
完整双实例隔离验证，不使用缺少本次 TLS/ACL 安全补丁的历史 8.2.8 制品正式部署。

- `REDIS_DATA_DIRECTORY` / `NEWAPI_REDIS_DATA_DIRECTORY`：不同、全新空目录。
- `REDIS_TLS_DIRECTORY` / `NEWAPI_REDIS_TLS_DIRECTORY`：v2 PKI 包中各自独立叶，
  SAN 分别 `redis` / `newapi-redis`；只挂自身 `server.crt`、`server.key`。
- `REDIS_ACL_FILE` / `NEWAPI_REDIS_ACL_FILE`：只读 ACL，不是明文密码文件。以对应
  `.acl.template` 为唯一授权定义，受控投递用三个不同 32 字节随机值的小写 hex 作为密码，
  计算各密码文本的 SHA256 替换 `*_PASSWORD_SHA256`。未替换模板故意不能启动。
  不得临时改为 nopass、共享密码或全命令授权来通过健康检查。
- 以上目录为 65532:65532/0700，私钥及 ACL 为 0600。正式密码通过受控秘密投递/加密备份
  分别交给三个消费者，不能放在公开 env、Compose 或命令行；服务进程内使用是既有边界。

Portal 使用 `REDIS_URL=rediss://portal:…@redis:6379/0` 和专用 `REDIS_CA_FILE`；
Identity 使用 `AUTH_RISK_REDIS_URL=rediss://identity:…@redis:6379/0` 和
`AUTH_RISK_REDIS_CA_FILE`。这两份 CA 只影响 Redis 连接，不能使用全进程环境替换公网根。
指定 CA 时拒绝明文、跳过验证、错误 CA/主机名及不合规 CA 文件。旧开发构造入口不变。
New API 使用 `REDIS_CONN_STRING=rediss://newapi:…@newapi-redis:6379/0`；其单容器
`SSL_CERT_FILE` 由原镜像公开系统根加基础设施公开根组成，不包含 KMS 根或任何私钥。
该原二进制信任投递与真实业务验证在 C4 完成，本批不冒充 New API 已上线。

两实例 TLS1.2+、无明文端口、default 关闭、无宿主端口。每个容器 384 MiB，Redis 96 MiB
`noeviction`；满内存拒绝写入，不悄悄驱逐会话/限流状态。AOF everysec 不是零丢失承诺；
截断 AOF 拒绝自动截断继续启动，须保留现场受控恢复。本机 socket 精确 NOAUTH 仅证明进程
存活，不能替代真实认证 TLS/业务/持久化就绪。C2D 补容量/到期告警、备份恢复及轮换交接。
变更身份 ACL 后应受控关闭旧连接并验证新旧身份；仅删除密码文件不能撤销已有连接。

隔离验收（先构建同源 Linux Portal、Identity、redisconnection 测试二进制，命名
`redis-portal.test`、`redis-identity.test`、`redis-options.test`）：

```sh
python3 scripts/account-platform-redis-fixture.py --run --bin-directory /controlled/public/test-binaries
```

只生成随机测试材料，实际运行本目录双实例配置与两个业务客户端，验证登录会话、审批
分类、刷新锁、风控脚本、失败聚合、跨前缀/命令/脚本拒绝，New API 命令矩阵与跨实例
身份拒绝，错误 TLS/密码/明文拒绝，重启持久化及容器约束；最后精确清理自己的临时资源。
