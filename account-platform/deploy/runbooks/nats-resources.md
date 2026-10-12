# F01 普通 Account outbox 发布

本阶段只有普通 Account relay 和独立资源操作员两个运行身份，没有 Billing、Token、审批授权 relay、consumer、executor 或人工 replay 入口。

`ACCOUNT_PLATFORM` stream 仍保留原共享规格的四个 subject：`account.>`、`billing.>`、`token.>`、`account-platform.dead-letter.>`。这是不可变共享资源元数据，不代表后续领域已交付。当前 broker ACL、进程及私密挂载只允许 Account 普通发布及其死信；没有给未来领域创建运行凭据。

## 预置与运行

分别构建 `cmd/event-resources` 和 `cmd/event-relay`。资源工具默认只读检查，显式 `-create-only` 才创建缺失的固定 stream；已有漂移不覆盖、更新、删除或 purge。创建响应未知时只回读。成功只输出 `NATS_FIXED_STREAM_VERIFIED`，不是部署验收。

两入口均使用 `NATS_URL`（单个 `tls://` 地址）、可选 `NATS_CA_FILE`，以及严格互斥且恰有一个的 `NATS_NKEY_SEED_FILE` / `NATS_CREDENTIALS_FILE`。凭据和 CA 为受控绝对路径的普通非链接文件。CA 只含当前有效 CA，保持 TLS 域名验证；种子不能超过 4 KiB，POSIX 仅本角色访问且父目录私有，Windows 投递必须另保 ACL。不要把私密值、URL 用户信息、凭据内容写进参数或日志。

`deploy/production/nats.conf` 指向挂载的服务端证书和私钥；同目录 `authorization.conf` 仅引用两个**公开**用户 NKey 环境变量 `NATS_EVENT_RESOURCE_OPERATOR_PUBLIC_KEY` 与 `NATS_ACCOUNT_OUTBOX_RELAY_PUBLIC_KEY`。用已有离线 `nats-identities` 工具生成保管包，再经独立公开清单核验选取对应角色。只给 broker 投递公开 key，不能把私钥目录挂给 broker 或另一个运行角色。TLS 私钥、JetStream 数据目录和配置目录均由部署者控制；管理 HTTP 只绑定容器 loopback，不对外发布。

操作员只获固定 stream INFO/CREATE 和自己的回复收件箱；运行 relay 只获 Account 发布、Account 死信、固定 stream INFO 和自己的收件箱。不要扩展为 `$JS.API.>`、`_INBOX.>` 或给运行角色 CREATE 权限。先用操作员只读检查；首次初始化显式 create-only；确认成功后退出操作员部署窗口。只读或创建失败均不得自动改成宽权限重试。

运行进程使用 `DATABASE_URL` 指向已迁移的 Account 库；`EVENT_SOURCE` 为空或 `account`，其他来源拒绝。`EVENT_RELAY_HTTP_ADDR` 默认 `:9090`，应只开放给受控服务网络。进程始终要求预置 stream 和显式凭据，开发环境也不自动初始化。`/health/live` 只表示进程存活；`/health/ready` 检查数据库与 broker 连接；`/metrics` 给出普通 pending/死信/投递周期指标，不暴露连接、身份、事件正文或服务端原始错误。

生产环境还必须配置既有观测库要求的 HTTPS `OTEL_EXPORTER_OTLP_ENDPOINT`；缺失或非法配置会拒绝启动。下面的 loopback 组件用例显式验证该拒启，正向链路使用测试环境并保持 NATS 预置和身份检查，不代表生产 OTLP 服务已验证。

每次发布由真实 JetStream ACK 后才标记 SQL published，message ID 是 `event UUID:publication_generation`。租约失效或未知 SQL 提交不能绕过重投栅栏；相同代次再次发布由 broker 去重，人工改变代次不是本阶段提供的操作。持续失败按原 12 次策略进入 Account 死信，仍须保留数据库事实。本阶段不提供死信人工重放能力。

## 验证与限制

`PINVOU_TEST_NATS_SERVER_BINARY` 指向已核验的本机 NATS server，可执行相关具名测试。测试必须新建独有目录、证书、NKey、loopback 端口和进程；只停自己启动的 PID，不能连接或清理共享 NATS。真实 PG 用例通过 `PINVOU_TEST_EVENTS_DATABASE_URL` 创建独有 UUID schema，退出核验清理。条件跳过不能计为真实集成通过。

loopback TLS/PG 实测不等于生产 ACL、证书投递、无停机轮换或部署验收。完整 E2E 按用户决定延期，现场密钥保管和 Windows/POSIX 权限限制如实保留。
