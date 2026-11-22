# 开发与隔离测试的 NATS 材料和首次初始化

`node scripts/account-platform.mjs dev up/down` 和 `test integration/e2e/fault` 固定组合 `compose.yaml`、`compose.dev-nats.yaml`、`real-newapi` profile。基础拓扑单独运行不能满足当前 daemon 的 provisioned、TLS 和显式身份要求。此开发组合不能替代生产、恢复或 model-token 发布组合；不自动开启正式审批、Token activation、外部支付或模型调用。

## 外部材料

先按 [NATS 身份工具说明](../../tools/kms-recovery/cmd/nats-identities/README.md) 生成并独立核验开发专用 v1/v2 包。保管人验证闭合文件集合、READY/manifest、公钥与 seed 对应和权限，再分别投递；不要把完整保管包挂进容器。使用当前 `approval-resources render` 生成、独立核验 authorization 和 consumer manifest，步骤见 [资源手册](nats-resources.md)。该手册 F02 的历史 consumer 数量不代表当前 registry；初始化和 Billing 校验使用可执行程序的当前固定集合。普通 Token outbox 不因此获得发行权限。

准备开发专用 CA、SAN 包含 `nats` 的服务端证书 `server.crt` 和对应 `server.key`。不关闭主机名验证。公开 current v2 清单、consumer manifest、authorization 各自单独目录；receipt 另用可写的受控目录；五个 seed 各自单独私有目录且文件名为 `nkey.seed`。TLS 私钥不与任何 seed 或公开投影共目录。目录不得重叠、含链接或由其他用户写入。运行身份只得到自己的 seed；操作员 seed 只进入显式 operations 容器。

在仓库外写开发配置 JSON，并通过**当前进程环境**设置 `PINVOU_DEV_NATS_CONFIG_FILE` 为其规范绝对路径；不能仅在 Compose `.env` 写此变量。配置本身也为私有文件。字段闭合如下；路径须替换为本机真实规范绝对路径，摘要从独立核验的工具制品/投递记录取得，不能把任意现场文件自算摘要当作信任来源。

```json
{
  "version": 1,
  "uid": 1000,
  "gid": 1000,
  "caFile": "/private/dev-nats/tls/ca.crt",
  "serverCertificateFile": "/private/dev-nats/tls/server.crt",
  "serverKeyFile": "/private/dev-nats/tls/server.key",
  "authorizationFile": "/private/dev-nats/authorization/authorization.conf",
  "authorizationSHA256": "<独立核验的 authorization SHA256>",
  "currentPublicKeysFile": "/private/dev-nats/public/current.json",
  "currentPublicKeysSHA256": "<独立核验的 current v2 SHA256>",
  "manifestFile": "/private/dev-nats/manifest/manifest.json",
  "manifestSHA256": "<独立核验的 consumer manifest SHA256>",
  "seeds": {
    "account-outbox-relay": "/private/dev-nats/account-outbox-relay/nkey.seed",
    "billing-outbox-relay": "/private/dev-nats/billing-outbox-relay/nkey.seed",
    "billing-command-executor": "/private/dev-nats/billing-command-executor/nkey.seed",
    "account-approval-relay": "/private/dev-nats/account-approval-relay/nkey.seed",
    "event-resource-operator": "/private/dev-nats/event-resource-operator/nkey.seed"
  },
  "seedSHA256": {
    "account-outbox-relay": "<该角色投递 seed SHA256>",
    "billing-outbox-relay": "<该角色投递 seed SHA256>",
    "billing-command-executor": "<该角色投递 seed SHA256>",
    "account-approval-relay": "<该角色投递 seed SHA256>",
    "event-resource-operator": "<该角色投递 seed SHA256>"
  },
  "dataDirectory": "/private/dev-nats/data",
  "receiptFile": "/private/dev-nats/receipt/receipt.json",
  "receiptSHA256": ""
}
```

`uid/gid` 必须为非 root；POSIX 上与执行入口的用户/组一致，私有目录 0700、私有文件 0600，且容器中的同一身份可读。运行 broker 和相关 Go 服务均用该非 root UID。Windows 宿主必须另外限制为保管人/SYSTEM ACL；Docker Desktop 的 bind 投递还必须在实际 Linux 容器中满足同样的私有目录/文件权限和可读性。宿主 ACL 或本地 Windows Go 测试不能证明容器 POSIX mode。无法满足时停止并修正受控文件投递，不能改 root、chmod 放宽或跳过 daemon 检查。不要使用生产材料。

## 新的开发环境：逐步显式操作

先补齐原 `.env` 的所有独立数据库/服务密钥、JWT/委托签名和真实开发 New API 输入；仍须按 [New API 手册](newapi-deployment.md) 完成既有初始化。创建受控空 data/receipt 目录，`receiptSHA256` 暂空。以下命令从仓库根执行：

```sh
node scripts/account-platform.mjs dev nats prepare
node scripts/account-platform.mjs dev nats create-only
node scripts/account-platform.mjs dev nats status --expected-version 1
node scripts/account-platform.mjs dev nats quiesce --expected-version 1
node scripts/account-platform.mjs dev nats fence --expected-version 2
node scripts/account-platform.mjs dev nats initialize-consumers --expected-version 3
```

`prepare` 仅执行原凭据检查、启动本开发 PostgreSQL/NATS 并执行原 Account migrator；不改变审批控制。操作者每步核对现场状态/版本，不能把上述版本当已有数据的通用重置参数。其余命令直接调用既有固定资源工具：create-only 不更新/删除资源；fresh 操作拒绝历史审批/授权事实，初始化在真实 Account 控制行锁内读取 NATS 高水位 Q、安装当前 Billing/普通消费者并写不可覆盖的真实回执。资源部分成功或回执失败时保留现场证据、保持 FENCED，不能覆盖文件、删除旧事实或在原目录假装重新 fresh。

独立核验实际 receipt 的绑定、Q+1、控制 version/epochs 和 SHA，再把该可信摘要填入配置的 `receiptSHA256`，执行 `node scripts/account-platform.mjs dev up`。默认 up 仅验证已有材料并启动，不自动 quiesce/fence/初始化旧数据库或总线。入口不提供 activate/resume；停在 QUIESCING/FENCED 不等于正式审批可执行或交付成功。已有环境沿用其原材料和控制事实，不重新执行 fresh 序列。up 与显式资源操作仅接受本机 Unix socket/Windows named-pipe Docker context 且不接受 DOCKER_HOST override，先只读核查当前 project 的 NATS /data mount 及历史 nats-data 命名卷；发现旧命名卷、不同 bind、无法检查或无法证明与声明目录一致时，在启动/资源写入前拒绝。不会自动将旧卷换成空目录、迁移、chown 或清理旧数据。原命名卷环境因此需要另行受控保全/处理后才能使用新组合；daemon 显示的跨平台 mount 路径若无法严格证明一致，也必须停止，不猜测其身份。`dev down` 使用同一组合，不因证书过期或材料文件暂缺而拒绝清理，但仍需原配置声明和 `.env`。

## 随机隔离测试

原 workload JWT 文件、委托签名和真实开发 New API 输入要求保持；额外提供上述开发配置。e2e/fault 的真实支付测试只接受 development_simulator，显式其他 provider 在启动前拒绝。只有 e2e 的一次性 acceptance fixture 将既有 BILLING_NORMAL_REFUND_CREATION_ENABLED 设为 true，以满足普通 LEGACY 退款断言；若调用者显式 false/空值/无效值则在启动前报 DEV_NATS_TEST_NORMAL_REFUND_REQUIRED，不覆盖其选择或跳过退款。integration/fault 不因此启用普通退款；dev up/production 默认 false 保持。测试只读取配置中的 TLS/公开制品/五个 seed，**不使用其 data/receipt**，而是创建随机 project 和自有临时目录。只接受本机 Unix socket/Windows named-pipe Docker context，拒绝 `DOCKER_HOST` override，并在写入前确认同名 project 的容器/卷/网络均不存在。

顺序为：凭据检查 → 独占 PostgreSQL/TLS NATS → 原 Account 迁移 → 真实 stream create-only/只读核查 → Account CLI status/quiesce/fence/initialize → 验证刚产生的 receipt/摘要 → 原完整业务启动和 smoke/acceptance/fault。该摘要的信任来源是测试编排者对**本次独占 fresh CLI** 的执行及受保护输出路径；不改变开发/生产已有回执的独立信任要求。没有调用 activate/resume，没有跳过原 acceptance/fault 断言。fault stop/start 始终携带同一 overlay 与 receipt 环境。

finally 只 down 本次 project/卷；Docker 清理失败时保留正在使用的私有工作目录。公开初始化 receipt 和结果保存在单独的 `pinvou-account-nats-evidence-*` 临时目录并打印路径，私密输入从不复制/删除。清理仅针对经绝对路径、前缀和 owner marker 核对的自有目录，不清理其他 project、原开发数据或外部材料。

## 监控和验证范围

`nats.dev.conf` 与 `production/nats.conf` 仅有管理 HTTP bind 一处差异：开发沿用原 `0.0.0.0:8222`，只在原 `backend internal:true` 网络可达，未新增 host port；Portal 原 `event-bus` HTTP 行及 service 分类保持。生产配置仍仅容器 loopback。TLS、固定 ACL include、JetStream 持久化/上限及其他安全参数逐字相同。普通健康、初始化回执和 Portal 状态均不能代替授权专项 readiness 或正式审批验收。

Node 合同随原 `account-platform` 的 `npm test` glob 执行，不新增依赖。真实 Compose config 仅证明解析/挂载装配，不证明 daemon、Linux 文件投递或业务流程成功；缺 Docker daemon、真实 New API/IdP 时必须另列未验收项。真实组件验证只能用自有 loopback NATS、开发合成身份及独占 PG，结束后核对自身监听器、数据库/角色/连接清零，不能访问部署总线或正式数据。
