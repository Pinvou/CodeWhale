# F02 普通发布与三种审批授权发布

普通 Account relay 保留 F01 安全边界。F02 增加独立 Account 审批授权 relay、Billing 普通 outbox 和当前 Billing executor；没有 Token 发布/执行、未来治理命令或人工事件 replay。审批 durable 只有 `WALLET_ADJUSTMENT`（policy 1）、`PAYMENT_PROVIDER_RESUME`（policy 2）、`PAYMENT_PRODUCT_RESUME`（policy 1），此外消费普通退款事件。

`ACCOUNT_PLATFORM` stream 仍保留原共享规格的四个 subject：`account.>`、`billing.>`、`token.>`、`account-platform.dead-letter.>`。这是不可变共享资源元数据，不代表后续领域已交付。当前发行 ACL 只含上述五种身份（含资源操作员）；共享 public-key 元数据保留 Token 角色名称，不给它们发行 broker 权限或挂载运行凭据。

## 预置与运行

F02 构建 `cmd/approval-resources` 和 `cmd/event-relay`，先按下节生成并独立核验五身份配置，再挂载给 broker。原 `cmd/event-resources` 仅保留 F01 固定 stream 的只读检查/显式 `-create-only` 初始化能力，不能替代 F02 的 consumer receipt 和审批控制初始化。已有漂移不覆盖、更新、删除或 purge。创建响应未知时只回读。`NATS_FIXED_STREAM_VERIFIED` 只证明固定 stream，不是 F02 完整资源就绪或部署验收。

两入口均使用 `NATS_URL`（单个 `tls://` 地址）、可选 `NATS_CA_FILE`，以及严格互斥且恰有一个的 `NATS_NKEY_SEED_FILE` / `NATS_CREDENTIALS_FILE`。凭据和 CA 为受控绝对路径的普通非链接文件。CA 只含当前有效 CA，保持 TLS 域名验证；种子不能超过 4 KiB，POSIX 仅本角色访问且父目录私有，Windows 投递必须另保 ACL。不要把私密值、URL 用户信息、凭据内容写进参数或日志。

`deploy/production/nats.conf` 指向挂载的服务端证书和私钥。F02 broker 必须使用上述独立核验的五身份渲染配置；同目录原 `authorization.conf` 是 F01 双身份历史模板（只有 `NATS_EVENT_RESOURCE_OPERATOR_PUBLIC_KEY` 与 `NATS_ACCOUNT_OUTBOX_RELAY_PUBLIC_KEY`），不得直接拿它运行 F02。用已有离线 `nats-identities` 工具生成保管包，再经独立公开清单核验选取对应角色。只给 broker 投递公开 key，不能把私钥目录挂给 broker 或另一个运行角色。TLS 私钥、JetStream 数据目录和配置目录均由部署者控制；管理 HTTP 只绑定容器 loopback，不对外发布。

F01 基线操作员只获固定 stream INFO/CREATE 和自己的回复收件箱；F02 渲染配置另外仅给固定退款及当前三审批 consumer 的 INFO/CREATE 和 consumer 名称读取。运行 relay 只获对应 Account 或 Billing 发布、对应死信、固定 stream INFO 和自己的收件箱。不要扩展为 `$JS.API.>`、`_INBOX.>` 或给运行角色 CREATE 权限。先用操作员只读检查；首次初始化显式 create-only；确认成功后退出操作员部署窗口。只读或创建失败均不得自动改成宽权限重试。

运行进程使用 `DATABASE_URL` 指向对应已迁移的库；`EVENT_SOURCE` 为空或 `account` 时只发布 Account 普通事件，显式 `billing` 时使用独立 Billing 普通库/发布身份/`billing.>` subject 且不读取或启动 Account 授权 channel。Token 或其他来源拒绝。Billing 数据库必须先装好本阶段的 outbox publication-generation 迁移。`EVENT_RELAY_HTTP_ADDR` 默认 `:9090`，应只开放给受控服务网络。进程始终要求预置 stream 和显式凭据，开发环境也不自动初始化。`/health/live` 只表示进程存活；`/health/ready` 检查数据库与 broker 连接；`/metrics` 给出普通 pending/死信/投递周期指标，不暴露连接、身份、事件正文或服务端原始错误。

生产环境还必须配置既有观测库要求的 HTTPS `OTEL_EXPORTER_OTLP_ENDPOINT`；缺失或非法配置会拒绝启动。下面的 loopback 组件用例显式验证该拒启，正向链路使用测试环境并保持 NATS 预置和身份检查，不代表生产 OTLP 服务已验证。

每次发布由真实 JetStream ACK 后才标记 SQL published，message ID 是 `event UUID:publication_generation`。租约失效或未知 SQL 提交不能绕过重投栅栏；相同代次再次发布由 broker 去重，人工改变代次不是本阶段提供的操作。普通发布持续失败按原 12 次策略进入对应领域死信，仍须保留数据库事实。本阶段不提供死信人工重放能力。

## 验证与限制

### 审批首次初始化

构建 `cmd/approval-resources`，使用其离线 `render` 操作从独立核验的 v2 公开身份清单生成固定三 consumer manifest 和五身份 NATS authorization 配置。配置该窗口的 `APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE` / `SHA256`，可选的 `PREVIOUS_PUBLIC_KEYS_FILE` / `SHA256`，以及 `APPROVAL_RESOURCE_MANIFEST_FILE`、`APPROVAL_RESOURCE_CONFIGURATION_FILE`。完整变量前缀均为 `APPROVAL_RESOURCE_`。输出路径必须尚不存在、父目录受控且无链接；身份清单目录仅存身份文件，manifest、configuration 和 receipt 必须分目录并远离任何私密凭据目录。生成后的 SHA 只描述字节，必须另由部署审核者核验，不能在服务启动时将自算 SHA 当授权。F01 的双身份 authorization 模板不能替代这个五身份发行配置；broker 应挂载已审核的渲染文件，不挂载任何 seed。

首次初始化要求 Account 控制初始为 `LEGACY/RUNNING/version=1`，且没有任何历史审批请求、执行、授权事件、待投递、排除或 delivery state。NATS 不得已有历史 shared approval consumer、其他未知 consumer、授权消息或授权死信。即使历史审批已经终结，也拒绝此 fresh-only 路径；历史 drain/cutover 不在本次首次初始化支持范围。工具不删除、purge、覆盖已有金融事实或 consumer，不给 legacy shared durable 发行执行 ACL。

使用专有 `account_migrator` 登录配置 `APPROVAL_INITIALIZER_DATABASE_URL`（私有环境，不放到命令参数/输出），并分别显式执行 `-action status -expected-version 1`、`quiesce/1`、`fence/2`、`initialize-consumers/3`、`activate/3`、`resume/4`。这里斜线后的数是对应的 `-expected-version`，不是省略该参数的简写命令。每一步都有真实 CAS；模式与 relay transition 分别提交。四次模式/relay推进后的控制版本依次为 2、3、4、5；资源初始化只增加独立审计而不推进版本。先 quiesce，再 fence，不能并行或绕过版本检查。未知提交结果先读 `status` 和审计核对，不自动重复效果。

资源操作使用专有 `event-resource-operator` 的显式 NATS 凭据和 CA。`initialize-consumers` 在数据库控制行的独占锁内读取真实 stream 高水位 Q，只创建普通 refund consumer 和三个起点严格为 Q+1 的 durable；缺失创建 ACK 时仅 INFO 回读，并核对全部配置。receipt 使用 `APPROVAL_RESOURCE_RECEIPT_FILE` 创建为不可覆盖的受控文件，绑定控制 version/epoch、真实 Q、manifest 和 identity SHA。写失败不激活，已产生证据保留。独立核验后设置 `APPROVAL_RESOURCE_MANIFEST_SHA256` 与 `APPROVAL_RESOURCE_RECEIPT_SHA256`，再执行 activate/resume；两步重新读取 hash-bound receipt 和真实 NATS 资源，不以离线声明代替资源就绪。创建部分成功但缺回执或之后出现资源漂移时保持 FENCED，不能删除证据或用宽权限修复来伪装 fresh。

审批 publisher 使用独立 `APPROVAL_RELAY_DATABASE_URL`（`account_approval_relay`）与 `APPROVAL_RELAY_NATS_NKEY_SEED_FILE` / `APPROVAL_RELAY_NATS_CREDENTIALS_FILE`，身份文件和 SHA 使用 `APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_FILE` / `SHA256` 及可选 previous 对。普通 `DATABASE_URL` 和普通 relay NKey 不能替代该身份。`/health/ready/approval-authorization` 独立检查控制状态、实际发布身份和公开清单；授权 channel 不可用时不能把普通 relay ready 当审批 ready。严格 PROOF_REQUIRED/control RUNNING 和 Billing activation/执行 proof 的就绪门禁仍需分别满足，fresh consumer 初始化本身不代表三种审批已可执行。

`PINVOU_TEST_NATS_SERVER_BINARY` 指向已核验的本机 NATS server，可执行相关具名测试。测试必须新建独有目录、证书、NKey、loopback 端口和进程；只停自己启动的 PID，不能连接或清理共享 NATS。普通 Account PG 用例通过 `PINVOU_TEST_EVENTS_DATABASE_URL` 创建独有 UUID schema；首次审批控制正式用例使用 `PINVOU_TEST_SCHEMA_DATABASE_URL`，在独占的角色 absence/cleanup 夹具内创建 UUID 数据库；退款实际用例另用 `PINVOU_TEST_BILLING_DATABASE_URL` 的同一角色夹具。退出均核验仅自有资源清理。条件跳过不能计为真实集成通过。

loopback TLS/PG 实测不等于生产 ACL、证书投递、无停机轮换或部署验收。完整 E2E 按用户决定延期，现场密钥保管和 Windows/POSIX 权限限制如实保留。
