# 正式环境安全基础设施补建

状态：实施中 / 整体 NOT_RELEASED；独立 KMS 已加密初始化，业务尚未切换。2026-09-07，部署负责人明确授权将缺失的 KMS 和服务身份签发设施纳入全新部署。

## 需求与范围

本变更补齐既有生产 Secret Provider、短期 workload 身份和 APT-137 环境前置，不另造业务授权体系。部署负责人已确认旧账号平台及 New API 是测试数据，不保留、不迁移；该豁免只适用于本次明确目标，不能删除其他系统数据，也不能豁免新环境的密钥保护、备份与恢复要求。正式商城配置仅通过受控秘密渠道注入。

1. KMS 使用 OpenBao Transit（Vault 兼容 API），不自研密码算法或伪装远端本地 AES。AES-256-GCM、随机 nonce、强制 AAD，密钥禁止导出与明文备份；业务仅持有密文及 key ID。
2. 复用 Keycloak，以独立 workload realm 和逐服务 confidential client 的 client-credentials 流程签发 JWT。最长 15 分钟，目标 5 分钟，后台自动续取；每个客户端只能代表自己的固定 subject，禁止用户登录、通配权限和共享静态业务 token。
3. 保留接收端既有 issuer/JWKS/audience/subject/精确 permissions 验证。实际 service-account subject 必须与部署授权表一致，不能用改写任意客户端 sub 的通配 mapper 绕过绑定。
4. KMS 和 workload 使用经验证的 TLS。KMS 只对指定服务及受控运维网络开放，无公网管理入口；凭据 Portal 继续使用独立 origin、OIDC client 和 audience。
5. 初始化主密钥前确定解封材料保管人及加密接收方式；不把解封份额、root token 或 CA 私钥留在业务容器、仓库、日志或聊天。初始化后撤销临时 root token。重启后需要人工解封的可用性限制必须明确交接。
6. 当前单服务器方案不承诺跨主机高可用或 HSM 保护。宿主机 root 仍在信任边界内。恢复演练、备份存放位置、密钥保管未落实前不能宣称正式可用。

## 设计

### Native Transit 适配

既有 `remote` 是自定义 `/v1/encrypt|decrypt` 协议，不等于 Vault API，保留其兼容行为。增加显式 `vault-transit` 模式，通过 `/v1/transit/encrypt/{key}`、`decrypt/{key}` 调用原生 API；不部署自制加密中转服务。

- 使用 `X-Vault-Token`，生产仅允许从绝对路径的短期 token 文件读取；每次调用重新读取，以支持 OpenBao Agent 原子轮换。Agent 与业务分别挂载单服务目录，业务只读，不可接触 AppRole bootstrap 身份。
- AAD 原样 Base64 编码为 `associated_data`，密文原样保存 OpenBao 返回的带版本封装，不自行解密。密钥名限定单路径段；轮换后按持久 key ID 解密，由服务端窄 ACL 决定历史 key 可用性。
- 严格 HTTPS、证书验证、禁止跳转、超时、请求/响应大小限制、结构与封装校验。错误只返回稳定信息，不透传 KMS 请求/响应内容、token 或明文。生产禁止静态 token 和 local fallback。
- Agent 使用逐服务窄授权，仅指定 key 的 encrypt/decrypt update；不得具备 create-key、export、rotate、policy、token 管理权限。密钥生命周期由独立运维身份操作。

### 服务令牌投递

轮换器只负责调用可信 Keycloak token endpoint 并原子写入该服务的 token 文件，不签名、不授予权限。客户端 bootstrap secret 独立保管；业务进程只读短期 JWT。签发结果须先通过 issuer/JWKS/audience/subject/签发时间/过期验证再投递。失败保留尚有效的旧 token，过期后业务认证拒绝；禁止把失败响应写为 token 或静默切换 development。

SECINF-02 按串行可验证交付细分为 02A（投递程序与安全负例）和 02B（独立 realm/客户端配置、真实 Keycloak 签发与接收端联验），两者完成前不标记 SECINF-02 完成。02A 使用 `cmd/workload-token-agent`，一个实例仅服务一个客户端，不接触其他服务秘密。唯一输入参数为非秘密配置文件路径，配置字段为 `issuer`、`audience`、`client_id`、`subject`、`client_secret_file`、`token_file`；后两者必须是分离目录中的绝对路径。客户端秘密每次从只读 bootstrap 文件读取，不接受明文参数/环境变量。

端点由固定 Keycloak realm issuer 派生为 `protocol/openid-connect/token` 和 `certs`，强制 HTTPS、可信证书、禁止跳转及有界响应；只调用 client-credentials，不请求 refresh token。投递前要求 RS256 签名、精确 issuer/audience/sub/azp、非未来 iat、有效 nbf（若存在）、正数且最多 15 分钟的生命周期，并至少剩余 30 秒有效期。不把 `expires_in` 或未验证 payload 当可信时间。正常在剩余有效期约一半时续取；失败按有界退避重试，只记录固定错误阶段，不输出 HTTP 响应或 JWT。保留旧文件不延长其有效期，接收端和 `-check` 始终按签名与实际 exp 拒绝过期身份。

02B 交付无秘密的 realm 配置生成器：固定账号、身份、计费、Token 网关、命令执行器、凭据 Portal、Keycloak 手机验证器 7 个客户端，沿用既有内部 audience；事件 relay 使用 NATS 身份，不为其增建无用途的 HTTP client。配置不预置 client secret、service-account UUID、管理员或测试用户；Keycloak 创建后通过受控管理接口取得真实账户 UUID 与逐客户端秘密，秘密只进入 agent bootstrap 文件，UUID 进入实际部署授权表。生成器不联网、不部署、不修改既有 realm。隔离联验使用全新专用 realm、随机临时管理员、loopback TLS、无业务数据和公网端口，结束后删除该测试容器；测试秘密与正式设施无关。

投递采用同目录临时文件、0600 权限、sync、原子 rename；运行目录必须预先创建且非符号链接，仅该服务的 agent 可写，业务以相同 UID 只读挂载。不能挂载单个 token 文件（会固定旧 inode），必须挂载单服务目录。正式运行宿主为 Linux；Windows 仅用于开发测试，不能将其 rename 的平台差异视为 Linux 持久化证明。`-check` 仅验证已有投递文件，不申请新令牌，供健康探针使用。02B 固定默认 RS256 与 300 秒有效期，关闭标准/隐式/密码授权/设备授权/Token Exchange 和 offline access，仅保留服务账户；读取服务账户真实 UUID，绑定授权表，禁止自定义 sub 或动态扩权。

### 部署顺序

SECINF-03 继续串行细分：03B 交付固定版本的 TLS/Raft/声明式审计/逐服务 AppRole 与 Agent 配置，并以独立实例验证持久化、封存、解封、权限、续租和审计；03C 才处理本机加密初始化响应、正式初始化、离线交接和备份恢复。03B 的随机测试主密钥不用于正式实例，不将其测试结果等同于保管人的正式备份确认。

03B 固定 OpenBao 2.6.2 及既有已验证摘要，采用专用 Raft 数据目录、独立审计目录、非 root UID 65532 和只读配置/TLS。业务只能连接私有 KMS 网络的 TLS API，正式部署不发布 KMS 公网端口；Agent 无 HTTP 代理或监听器。宿主禁用未加密 swap/转储，数据目录和审计目录不得被其他业务挂载。声明式 file audit 始终开启 HMAC 脱敏，不允许 API 新建审计设备；轮转必须重开文件并验证继续留痕，不能静默丢弃审计。

网关与凭据 Portal 使用不同 AppRole 和秘密文件，即使访问同一个业务 Transit key，也独立撤销和审计。业务策略仅允许指定 key 的 encrypt/decrypt（强制 AAD 参数，禁止 runtime 指定旧 key_version 或创建 key），以及本人 token 的 lookup/renew/revoke；不得创建 token、修改其他 token、密钥、策略、认证或审计配置。Token TTL 5 分钟、显式最长 15 分钟，Agent 达到最长周期后重新 AppRole 登录。bootstrap SecretID 为可撤销的持久机器秘密，不冒充短期身份：仅 Agent 可读取，需在交接中登记周期轮换及泄露撤销步骤。短期 token sink 放在单服务受控运行目录，业务只读整个目录，且无法读取 bootstrap。

隔离实例以新建私有 internal Docker 网络运行，不发布任何宿主端口，测试控制进程仅通过该独立网络的已验证容器地址连接，绝不使用既有业务端口/数据卷。初始化 root 与解封材料只在测试进程内存中使用，不输出，不落明文文件；叶证书、临时测试 Agent bootstrap 及 Raft 数据均在具名临时目录，结束精确清理。测试主动缩短租期仅用于观测，公开正式角色模板仍固定 300/900 秒。

2026-09-07 保管方式已获负责人同意：本机口令保护的离线恢复包，异地/离线另存备份，口令分开保管，不通过聊天交付明文。先完成 SECINF-03A 本机托管工具，再进行正式初始化；此子任务不代表 SECINF-03 整体完成。

恢复包采用标准 age 格式，独立工具模块固定 `filippo.io/age v1.3.2`，不改变业务服务依赖。`prepare` 仅在本机终端无回显读取及确认口令，生成 X25519 恢复身份、用 scrypt recipient 加密为 `identity.age`，只输出公钥 `recipient.txt`；`seal` 从标准输入接收材料，用该公钥加密，绝不写入明文文件。`verify` 在内存解密并验证完整性，不打印明文，不接受参数/环境口令。拒绝覆盖已有文件、短口令、损坏包和超限输入；文件写入后 sync。口令以及丢失后的不可恢复风险由保管人负责。加密不防护已被控制的本机；操作前应关闭会记录输入/内存的录屏或调试工具。

本机身份文件不含正式 KMS 密钥；正式材料只能在确认该身份可解密及备份安排后生成。仅初始化托管身份不操作服务器、不初始化 OpenBao、不删除任何数据。Windows 发布目录继承权限需显式收紧至当前用户与 SYSTEM。单人保管不冒充多人分权；长期多人 custody 为后续治理选项，不自行创建未知保管人。

03C 先交付独立的 Linux `kms-initializer`，不向已有离线 `kms-recovery` 程序添加网络能力。初始化器只持有保管人的公开 age recipient 和固定私有 TLS CA；公钥 SHA-256 必须与人工核对的配置一致。目标仅允许无路径/查询/用户信息的 HTTPS origin，无代理、跳转或自动重试。实例编号、目标 origin、CA 指纹、时间和原始响应一并封装在加密包内，用于后续受控解封核对；实例编号是部署关联信息，不代替 TLS 身份验证。

初始化前必须显式确认全新实例和保管人离线备份安排；程序验证目标确实未初始化且已封存。先独占创建私有输出目录、密文文件和同步的尝试标记，再发起一次 `sys/init`（当前单人 custody 为 1/1，不冒充多人分权）。响应只在内存读取并立即加密，文件和目录同步、密文读回一致后才创建成功标记；不自动解封、不配置策略、更不删除/重置服务。所有错误仅返回固定阶段，不透传服务响应、URL 或秘密。

结果不确定必须 fail closed：只要开始初始化请求，超时、断网、非预期状态、损坏/截断响应、写盘失败均不能自动重试。非空已收到响应也保存为加密诊断包，但没有成功标记不能认定恢复材料齐备。所有输出保留且拒绝覆盖。网络请求与服务持久化不具备跨进程/掉电原子性；若服务已初始化而一次性响应丢失，须人工核对这一尚未承载业务的实例并另行决定恢复或重新置备，程序不得自行清库。后续本机人工解封、运维身份、根 token 撤销及真正快照恢复仍是独立验收项。

公开模板及隔离测试 → OpenBao/Keycloak 固定版本与镜像摘要 → TLS/受限网络/保管安排 → 正式初始化和逐服务策略 → 真实 KMS/签发轮换负例 → 维护窗口复核精确旧目标 → 清理测试数据并从空库安装 → NATS TLS/ACL、治理角色及初始化 → 双域登录、模型与计费验收 → 放量。

本次空库安装无需旧测试密文迁移；现有生产安全门禁中依赖旧 key 的轮换证明仍需增加独立 fresh-install 分支及真实新环境轮换演练，不能伪造旧 key 或直接跳过整项 gate。

## 串行任务与验收

03C-2 增加单独的本机 `kms-operator unseal`：保管人在真实终端输入口令，解密 identity 和初始化包只在内存中完成；先校验包的版本、完整状态、实例编号、原始 HTTPS origin、专用 CA 和 recipient 指纹，再请求封存状态。只允许已初始化 Shamir 1/1 实例的单次解封，不发送 root token，不提供 init/reset/migrate 或任意 API 代理。已经解封时不重复提交份额。请求失败或状态不确定只报告固定阶段，不回显响应或重试写入。

本机访问私有 KMS 可经既有 SSH 隧道：可选连接覆盖仅允许 loopback IP 和明确端口，HTTP origin、Host 与 TLS 证书验证仍使用包内原始目标，不把 localhost 当成可信证书名，也不关闭验证。隧道由部署步骤建立，operator 不启动外部命令、不自动配置 SSH/DNS。提取共享的纯离线 age/身份读取能力供两个命令复用，默认 `kms-recovery` 保持无网络，不修改已有加密格式。此子项不代替后续策略/运维身份初始化、root 撤销或快照恢复。

03C-3 独立恢复证据：两个真实 Raft 实例使用不同私有网络和数据/审计目录，恢复目标不能访问源实例。源创建 v1 密文并轮换至 v2，在撤销初始 root 后，以仅具 snapshot read/sudo 的限时独立测试身份取快照，外层 age 加密并落盘；恢复前完整解密验证，错误身份与损坏包必须在恢复 API 调用前拒绝。目标为明确全新的隔离实例，仅此测试流程可使用 `snapshot-force` 替换其数据，不向正式 operator 暴露此操作。恢复后使用源份额解封、验证旧密文与 v2 状态、两端旧 root 均拒绝。该证明不等于正式定时备份、保留策略、保管人恢复交接和生产灾备可用性。

03C-4 私有证书初始化使用独立离线 `kms-pki`：在保管人受控目录中生成 ECDSA P-256 专用根 CA（5 年、禁止中间 CA）与 KMS 叶证书（90 天、仅 serverAuth、固定 DNS `openbao` 和已核对的私有 IPv4 地址）。根 CA PKCS#8 私钥只在内存中产生，直接封装为保管人 age 公钥加密包，禁止明文 root key 文件；服务器只接收独立叶证书/私钥及公开 CA。新输出目录不可覆盖，私有文件写入同步并验证证书链与密钥对应关系后才写就绪标记。正式 Windows 输出父目录 ACL 必须先核对，受控加密包与身份、口令分别保管；叶证书到期前轮换及告警仍纳入 SECINF-04 运行交接。

| 任务 | 交付与验收 | 状态 |
| --- | --- | --- |
| SECINF-01 | 原生 Transit provider、逐调用 token-file、两服务接线；HTTPS、AAD、轮换、权限拒绝、无泄漏、跳转及有界响应测试 | 代码已验证 / NOT_DEPLOYED |
| SECINF-02 | Keycloak 独立服务客户端配置与轮换器；真实签发/轮换/过期/错 subject、audience、权限拒绝 | 02A/02B 隔离验证通过，整体 NOT_DEPLOYED |
| SECINF-03A | 保管人已同意的本机离线托管工具；口令无回显、标准 age、加密身份读回、拒绝损坏/覆盖；在正式初始化前完成 | 代码与隔离测试已验证；custody 备份已确认，正式 CA/初始化加密包已生成并由保管人完成解封 |
| SECINF-03 | 固定版本 OpenBao/Agent 部署、逐服务策略、TLS/网络、初始化与解封交接、备份恢复；明确单节点边界 | 独立 KMS/TLS、四类正式 AppRole 与初始 root 撤销已完成；新包离线备份已确认，正式备份调度与业务接入未完成 |
| SECINF-04 | Compose/Helm/生产安全门禁原生 KMS 与 fresh-install 适配、真实依赖验收及部署运行手册 | 04A 私有 CA 接线已通过真实隔离联验；其余待实施 |
| SECINF-05 | New API 候选兼容与安全验证、精确清理旧测试数据、空库部署及商城生产联调 | 待实施 |

每项独立自检、验证、提交，不能将模板解析/单元测试当作现场上线证据。New API 版本仍须单独通过验证，未将 rc.26 认定为生产稳定版本。主密钥保管、真实上游模型凭据及任何需新资源采购的选择应由负责人提供或批准。

## 依据

- [OpenBao Transit API](https://openbao.org/docs/api/secret/transit/)
- [OpenBao Agent AppRole](https://openbao.org/docs/agent-and-proxy/autoauth/methods/approle/)
- [OpenBao Seal/Unseal](https://openbao.org/docs/concepts/seal/)
- [Keycloak Service Accounts](https://www.keycloak.org/docs/latest/server_admin/index.html#_service_accounts)

当前仅为实施约束与任务拆分，不是生产验收报告。GitHub Issue/PR 检索因本机 gh 未认证未完成；本地未发现现成 Transit 适配或 workload 签发器。

## SECINF-01 实测记录

- 原生 provider、既有 remote 回归及生产 local 拒绝：Go 1.26.5，`go test -tags=nomsgpack ./internal/token -run 'TestTransitProvider(Native|Rejects)|TestRemoteEnvelopeProvider|TestProduction.*SecretProvider' -count=1`，PASS（6.219 秒）。
- 两服务入口：`go test -tags=nomsgpack ./cmd/token-gateway ./cmd/token-credential-portal`，gateway PASS（3.284 秒），credential 仅编译通过（无该包测试）。对应 `go vet` 通过。
- 真实 OpenBao 2.6.2 镜像锁定 `quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641`。独立容器 `--network none`，无业务卷/端口，受限 CPU/内存，测试 root token 随机生成、无日志输出；完成后精确移除测试容器及其内存数据。
- `TestTransitProviderOpenBaoIntegration` 实际执行 PASS（0.01 秒），无 SKIP：真实 native API、随机密文、AAD 拒绝、跨 key/ACL 拒绝、v1→v2 轮换后旧密文读取、撤销后拒绝。测试显式 opt-in `PINVOU_TEST_OPENBAO_ISOLATED=1`，只接受该隔离网络命名空间中的 `http://127.0.0.1:8200`；测试 bootstrap token 从环境注入，不能指向正式服务。
- 首次 fixture 启动因只读根目录未配置 `-dev-no-store-token` 失败，第二次只读 rootfs 阻止 docker cp；修正为不保存 dev token、只读挂载测试二进制后上述具名测试通过。未降低业务 TLS 或认证要求。
- 架构守卫通过；`workload-identity.test.mjs` 三项通过。自检确认现有 remote 协议未变、两入口接线齐备、生产静态 token 拒绝、token 文件缺失不复用缓存、错误不回显外部内容。
- 此处真实 OpenBao 仍为明确的**隔离开发模式 fixture**，不代表生产 TLS、持久化、解封、Agent、正式身份/网络或恢复验收。部署覆盖与生产安全门禁仍归 SECINF-03/04，不能直接用现有强制 remote 的 overlay 上线 native provider。

## SECINF-02A 实测记录

- [轮换器运行手册](../deploy/runbooks/workload-token-agent.md)说明单服务文件隔离、启动与健康检查、失败/过期语义及平台限制。新增程序未改变现有业务配置，不自动启用签发设施。
- Go 1.26.5：轮换器 TLS fixture 验证真实 RSA 签名/JWKS、双 token 与 bootstrap secret 轮换、错误 issuer/audience/sub/azp、未来 iat/nbf、缺失/异常/超长/过期生命周期、错误签名、HTTP 拒绝/跳转/超限/损坏响应、不可信 TLS、续期失败保留及过期拒绝、取消与固定日志。
- 接收端同类问题修复：新增严格签发/生效/失效时间检查，8 个边界子用例；不改变 subject→服务→精确权限规则，不扩大既有权限。
- Linux 独立容器、非 root UID、network none、只读根目录、无业务挂载，7 个顶层测试全部 PASS（无 SKIP），其中包含 40 次替换下并发读取完整性、0600 权限、公开秘密文件/符号链接/可写目录拒绝。容器执行结束已自动移除；只使用随机隔离测试签名密钥及假客户端秘密。
- Windows 首次并发替换测试实际暴露普通读句柄阻止 rename 的平台差异；未增加不安全的删除重建或宽松回退。正式 agent 明确仅支持 Linux，Windows 对 POSIX 权限和并发替换 2 项显式跳过，不能将 Windows 结果冒充生产原子性证明。
- 对应 Go 测试及 vet、架构守卫、既有 workload 身份契约 3 项已通过。上述签发端是隔离 TLS fixture，不是 Keycloak 联验；真实 realm、服务账户 UUID 与权限拒绝仍需 SECINF-02B。

## SECINF-02B 实测记录

- 无秘密 realm 生成器 4 项 Python 测试通过；显式固定 7 个 confidential client、300 秒 RS256、无交互式授权/离线/刷新/交换/设备授权，不预置 secret、用户 UUID 或角色。
- 真实官方 Keycloak 26.7.3 固定摘要 `sha256:ff4257d0d64efbe99ed1ddfaf07765cc3c36dc7518bf8324d41961327f441c54`；隔离容器 network none、无公开端口/业务挂载、独立 H2 测试数据，TLS 证书真实验证。具名 `TestKeycloakWorkloadIntegration` 实际 PASS（114.64 秒），无 SKIP；退出后测试容器、数据、临时秘密、启动日志均清理。
- 通过真实管理 API 核对 7 个客户端实际导入后的授权模式及 scope，读取 Keycloak 生成的服务账户 UUID 和随机客户端秘密；实际签发并验证 300 秒令牌，未使用测试进程自签 JWT 代替 Keycloak。
- 错 subject/audience 拒绝；另一个真实服务账户对未授权接收端 403；同一主体未授予的接口 403；服务 token 访问 Keycloak 管理 API 403。客户端秘密真正轮换后旧秘密无法再签发，新秘密自动读回并成功续取。
- 实际验证不返回 refresh token，密码授权、offline access、Token Exchange 和设备授权被拒绝。仅隔离客户端临时缩短为 40 秒后签发真实令牌、禁用客户端，再验证续期失败保留文件；等待真实 exp 后，健康检查失败且接收端返回 401。正式生成器仍固定 300 秒，未缩短生产策略。
- 过程发现并修复两项 fixture/配置问题：导入文件名必须与 realm 一致；省略 realm 层 scope 声明会自动附加内置 scope，已显式置空并补回归断言。早期 Go 测试进程 137 实际与 Keycloak 启动退出 1 关联，不能认定为 OOM；最终容器 memory.events 的 max/oom/oom_kill 均为 0。调试日志使用受限本地驱动，修复 max-file=1 与压缩冲突，并保证先 create 再 start、失败清理准确目标。
- 这些证明是独立签发器与接收端链路的真实隔离验证，不是手机认证扩展镜像回归、正式域名/realm 部署、KMS 初始化、恢复演练或全系统上线验收。正式设施创建、精确 subject 授权表和 secret-file 投递配置仍由 SECINF-03/04 部署。

## SECINF-03B 实测记录

- 固定 OpenBao 2.6.2 摘要的真实非 dev 实例，TLS、Raft、声明式 HMAC 审计及两个独立 Agent。专用 internal 网络，无宿主发布端口，非 root、只读根目录、受限能力/内存/CPU。测试 root 与解封材料只在控制进程内存中使用；结束清理自身容器、网络和测试数据。
- 最终完整运行输出 `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`，无跳过。可信 TLS 正例、不可信 CA/错主机名负例、双服务实际不同 token 且仅各自策略、AES-256-GCM 禁止导出/明文备份/删除配置均核实。
- 运行身份不能创建/轮换/导出/备份密钥、创建 token、撤销其他 token、修改策略或访问其他 key；缺失/空 AAD、错 AAD 与指定旧 key_version 均拒绝。真实 v1→v2 后默认写入 v2，旧 v1 密文仍可解密。
- 真实 Agent 产生续租审计并重新认证，投递文件为 0600；等待前一 token 实际过期后验证 403，不把新 token 提前投递误认为旧 token 已失效。正式模板为 300/900 秒，隔离 fixture 为 12/24 秒。
- 审计中未出现明文、Base64 明文、AAD、密文、root/运行 token 或解封 canary；rename/create 后 SIGHUP 确认新审计继续写入。重启后真实封存拒绝 503，原材料解封后通过重新投递身份读取历史密文，最终撤销测试 root 并确认 403。
- 实测发现并修正 fixture 可执行文件路径、internal 网络无宿主映射和 Agent sink mode 类型：该固定版本要求整数 `384`（0600），不能使用字符串 `"0600"`。早期启动诊断曾出现 Raft initialization lock 报错，未据此宣称原因已修复；最终完整实例就绪及上述验证通过。
- 四项静态配置测试、架构守卫、差异检查通过。尚未执行独立 Raft 快照恢复、加密初始化包与本机解封流程；重启持久化不能代替完整恢复验收。正式设施与业务部署均未改变。

## SECINF-03C-1 实测记录

- [单次加密初始化器](../tools/kms-recovery/cmd/kms-initializer/README.md)与离线托管程序分别构建，未改变后者的网络边界或业务服务依赖。Linux 命令必须显式确认新实例和保管人备份；新目录独占、目录/文件同步、密文读回，所有失败保留现场。
- Windows Go 1.26.5 全模块测试通过：原离线托管 13.330 秒、初始化模块 2.880 秒，命令编译通过（无该包测试）；`go vet` 通过。真实实例测试默认显式跳过，POSIX 权限断言只在 Linux 执行，不把 Windows 结果视作目录同步证明。
- TLS 单元 fixture 验证正确包解密与元数据绑定、错误身份/损坏包拒绝；HTTP/路径/查询/用户信息/错误公钥指纹/无确认拒绝；有效但不可信 CA 拒绝；已初始化和缺失状态拒绝；500/307/截断/超限/缺失材料/连接断开均不重试、不生成成功标记，非空响应保留为加密包；已有包不覆盖，关闭文件写入失败报告不确定。
- 同源 Linux 测试二进制在新建 OpenBao 2.6.2 非 dev、TLS、Raft 隔离实例上执行整个初始化模块测试集，输出 `OPENBAO_INITIALIZER_FIXTURE_PASS`，未跳过；真实用加密包恢复的材料解封、使用包内 root 认证、撤销 root 后 403，再次初始化及覆盖拒绝；确认所有输出没有实际 root/解封明文，Linux 私有文件和目录检查实际执行。该实例与其测试材料已精确清理。
- 四项 OpenBao 配置测试与架构守卫通过。此子项仅完成加密初始化捕获：用户的正式主密钥、CA、正式恢复包均未生成；本机人工解封/配置运维身份、独立快照恢复、业务部署仍未完成。初始化响应丢失的跨系统非原子性风险保留，未增加自动重置回退。

## SECINF-03C-2 实测记录

- 独立 [kms-operator](../tools/kms-recovery/cmd/kms-operator/README.md) 只提供人工解封；共享纯离线 custody 包，原有 `kms-recovery` 格式、输入和网络边界保持兼容。保管人口令不经工具或聊天输入，本轮只使用随机测试身份和固定测试口令。
- Windows Go 全模块通过：既有离线测试 8.580 秒、初始化 3.030 秒、operator 2.429 秒；`go vet` 通过。两个 CLI 编译通过，无 CLI 包测试；真实实例测试默认跳过，真实本机终端交接尚待部署时执行。
- 单元 TLS fixture 覆盖解封及已解封不重复、12 类包/绑定/身份/损坏拒绝且零网络请求、原目标 Host 与 TLS 名称通过 loopback 保留、错误 TLS 名称拒绝、5 类写失败不自动重试及不回显、未知/未初始化/错误 Shamir 阈值拒绝、HTTP/路径/查询/用户信息/远程或非数字 loopback 覆盖拒绝。
- Linux 同源 operator 模块全部测试在独立 OpenBao 2.6.2 TLS/Raft 实例真实执行，无跳过，输出 `OPENBAO_OPERATOR_FIXTURE_PASS`。使用标准 scrypt 加密测试身份及初始化加密包，错误口令拒绝；经透明 loopback TLS 转发实际解封，直连已解封不重复提交；重新封存后再次实际解封，最终撤销测试 root 并验证 403。只在测试控制进程中使用测试 root 完成封存/撤销，正式 operator 解封从不发送 root。
- 配置测试、架构守卫与差异检查通过。测试容器及临时材料精确清理；未生成正式主密钥、没有更改旧业务服务或数据，独立快照恢复及生产配置接线仍未完成。

## SECINF-03C-3 实测记录

- 同源 Linux operator 测试二进制配合 `--restore-fixture`，在两个新建 internal 网络、独立 Raft 与审计目录的 OpenBao 2.6.2 实例实际运行，输出 `OPENBAO_SNAPSHOT_RESTORE_FIXTURE_PASS` 和 `OPENBAO_OPERATOR_FIXTURE_PASS`，未跳过。没有挂载业务数据或发布宿主端口，结束精确清理两个实例与网络。
- 源实例产生 v1 密文并轮换 v2；限时 180 秒的 snapshot-only 与 readback 测试身份分别授权，后者不能取快照。撤销源 root 并确认 403 后，再由 snapshot-only 身份取得真实 Raft 快照、age 外层加密落盘，错误解密身份和损坏包拒绝，读回并完整认证后才提交给新恢复目标。
- 空目标独立初始化和解封后实际执行 `snapshot-force`；随后真实重新封存，使用源恢复包、原逻辑 TLS 身份及受控 loopback 路径再次解封。确认旧 v1 密文读回、密钥版本仍为 v2，源已撤销 root 和恢复目标被替换的 root 都返回 403，不因恢复重新生效。
- 两个隔离实例仅在本测试中共享临时测试叶证书（包含两个测试 IP），用于模拟同一逻辑 KMS 的替换；不涉及正式 CA/证书复用。快照与 root/份额仅用于此临时证明；正式 operator 没有新增 `snapshot-force` 入口。
- Go operator 单元 2.624 秒及 vet、4 项配置测试、架构守卫通过。该 fixture 将小型快照限制为 1 MiB，不能据此宣称正式容量/定时备份/留存/介质恢复已验证；正式备份工具、真实介质和容量验收仍需部署交接补齐。

## SECINF-03C-4 证书工具实测记录

- 独立离线 `kms-pki` 不联网、不接触 KMS 主密钥，只生成专用 CA 与叶证书；CA 私钥直接 age 封装，输出目录拒绝覆盖。Windows 全模块测试通过（PKI 6.294 秒），`go vet`、架构守卫与差异检查通过。
- 对应 Linux 测试二进制 3 项全部通过，无跳过：解密加密 CA 包并核对实际私钥与证书对应、叶/根密钥分离与用途/期限/身份约束、使用生成的叶证书完成真实 TLS 握手、错误身份/损坏包拒绝、现有目录保护和 9 类错误输入拒绝。Linux 0700/0600 与可写公钥拒绝实际执行。
- 正式生成前已核对本机恢复父目录继承关闭，仅当前保管人与 SYSTEM 有权；仅检查原 custody 的加密文件元数据，不读取口令或解密私钥。首次生成工具不提供续签/告警，正式证书轮换与运维自动化仍需后续交接，不能据此标记安全设施全部完成。

## SECINF-03C-5 设施启动记录

- 独立 OpenBao Compose 模板不发布端口、不自动初始化或解封；只接入新建 internal 网络，配置/证书只读，唯一持久化写目录为登记的 data/audit。生产数据不与测试 fixture 或旧业务目录共享。
- 实际 Docker Compose 解析和新建容器检查发现两项序列化/镜像细节：`core=0` 在 config JSON 中省略数值，但真实 HostConfig 已核实 soft/hard 均为 0；能力名规范化为 `CAP_IPC_LOCK`。未把表示差异当作运行配置缺失。
- 固定镜像另有两个隐式匿名卷，启动前拦截并改为明确 tmpfs。仅移除本次新建、从未启动的容器及其两个已核实为空、无其他使用者的匿名卷，保留新证书和绑定目录，未清理旧业务。最终实际容器只有 5 个受控 bind，无额外持久卷，禁用转储、只读根目录、限额和单网络均通过。
- 对隔离 helper 同步加上 core=0、noexec 与镜像路径 tmpfs 后，真实 TLS/Raft/双 Agent/AAD/ACL/续租/重认证/审计轮转/重启解封回归再次完整通过 `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`；5 项配置测试及架构守卫通过。
- 正式专用 CA/叶证书已在保管人受控本机生成，CA 私钥仅以 age 包保存在本机，未上传服务器；服务器已启动新 KMS 并通过实际 HTTPS 证书校验，启动核对时 `initialized=false`。这仅为独立设施启动，不代表主密钥交接、root 撤销、定时备份或业务切换完成。

### 正式初始化与保管人交接进度

- 保管人确认 custody 已存储后，正式新实例通过一次性初始化器生成 1/1 Shamir 材料，输出 `INIT_CAPTURED`；响应仅内存处理并直接 age 加密。未把 root、份额或 CA 私钥写入聊天/环境变量/业务镜像/明文文件。此后不得再次运行 init 或把该设施当成可丢弃测试实例。
- 初始化加密包已传输到本机受控恢复目录，服务端与本机 SHA-256 一致；目录 ACL 仅保管人与 SYSTEM。新 CA 加密包与初始化包都需加入离线备份，原 custody 备份不能代替新材料备份。
- 保管人通过本机交互窗口完成操作并确认“已解封”；随后独立只读核对 initialized=true、sealed=false、n=t=1，真实 TLS 健康检查返回 200。没有通过 Agent 读取口令或解密其身份代办。
- 本机暂存叶私钥在与受控服务端副本核对哈希一致后精确清理；服务器叶私钥、CA 加密包及初始化包未删除。此初始化阶段尚未创建正式 AppRole 或撤销 root；后续完成状态见 03C-6。旧账号平台、New API 和其他业务服务/数据尚未切换，也未完成全平台生产验收。

### SECINF-03C-6 有界身份引导与 root 退场（已正式执行）

需求：普通解封仍不解析或发送 root。另设显式本机 `bootstrap` 操作，使用同一恢复包绑定校验及真实终端口令，root 仅内存使用。仅面向已解封、尚无 transit/approle 的新实例，不修改已有挂载、角色或密钥；不提供任意 API、策略输入、删除或重置。

固定计划：创建不可导出/不可明文备份/不可删除的 AES-256-GCM `pinvou-token-data`；分别创建网关、凭据服务、快照备份、离线运维四个 AppRole。前两者沿用强制 AAD 的 runtime 策略；备份仅快照 read/sudo 及自身租约；运维只读/轮换该 key、管理这四个既定角色的 SecretID，不得修改角色策略、创建任意 token、管理挂载或直接加解密。四角色统一 300 秒 token / 900 秒显式上限，不附加 default policy。持久 SecretID 是单独的机器引导秘密，不冒充短期 token；运维 SecretID 只能留在离线加密包，不部署进业务服务器。

持久化与不确定状态：先独占创建受控输出目录并同步尝试标记，每个一次性 SecretID 响应立即 age 加密、同步、读回；最终生成绑定实例/origin/CA/recipient 的完整身份包。所有输出拒绝覆盖；失败不自动重试创建、不回滚或销毁已生成身份，保留初始 root 以便受控恢复。runtime/backup 的明文文件投递另行受控完成，本步骤只生成加密包。

退场分为独立 `finalize-bootstrap` 操作：完整解密身份包、验证四个身份的实际登录与限定权限、加解密及快照读取，再撤销初始 root 并确认 403，重新验证四角色不依赖 root 存活。root 撤销结果不确定时只能用同一包重新核对/完成退场，不能重新 bootstrap。就绪标记只在撤销及后验均成功后创建；加密包需加入既定离线备份。本步骤不等于正式备份调度、身份投递或业务上线。

验收：错误包/目标在联网前拒绝；已有实例配置与已有输出在写入前拒绝；策略和角色参数回读；四身份正向及越权负例、AAD 缺失/不符、错误 TLS、错误凭据、响应超限/断线/跳转、部分失败及重复退场；真实隔离 OpenBao 上验证初始 root 403 与四角色仍可登录。按该边界实现、验证并独立提交后，才交付本机正式执行入口。

实测与自检（2026-09-07）：

- Windows 独立工具模块全量测试 PASS：离线工具 10.527 秒、初始化 4.271 秒、operator 9.894 秒、PKI 4.288 秒，vet PASS。默认真实环境测试仍显式 opt-in，不能把默认 SKIP 当真实环境验收。
- 最新同源 Linux operator 测试二进制在全新独立 TLS/Raft 实例完整执行，无 SKIP，输出 `OPENBAO_BOOTSTRAP_FIXTURE_PASS` 和 `OPENBAO_OPERATOR_FIXTURE_PASS`。真实证明四角色创建、加密包读回、权限/AAD、备份读取、初始 root 403、重复退场、四角色存活、运维轮换 key、跨角色 SecretID 拒绝及轮换/销毁后拒绝。
- 联验修复了两处 API 假设：策略目录不是 GET 列表，改为逐个固定策略 GET/404；OpenBao 2.6.2 创建及轮换 key 返回带元数据的 200，而非 204。未放宽为“任意 2xx”，并对策略/角色/key 安全属性做实际回读。失败均发生在隔离实例，未操作正式配置。
- 本地负例覆盖创建响应断线/截断/超限/跳转、加密诊断保存、已有挂载与输出拒绝、完整包探测失败后的无重建续办、角色包九类绑定/重复身份拒绝、额外权限和错误凭据阻止 root 撤销，以及撤销响应丢失后的同包收敛。普通 unseal 仍不发送 root；默认离线程序无网络新增。
- 自检边界：只有固定 key/四角色，无任意策略或代理；运维可签发四角色 SecretID，属于离线特权身份，不宣称它完全无法间接获得运行身份。销毁 SecretID 不立即终止旧 token，最长 15 分钟窗口与紧急隔离已写入手册。快照读取不是持久备份；部分创建失败不伪称可自动恢复。正式业务/Agent 投递、备份调度及整个平台部署仍未完成。

正式执行：保管人完成 `BOOTSTRAP_PREPARED`，明确确认新身份包、CA 包及初始化包离线备份，再完成 `BOOTSTRAP_FINALIZED`。本机完成标记与 `roles.age` 摘要独立核对一致，真实 TLS 健康检查 200。正式 key、四类 AppRole 和初始化 root 撤销已由受控操作器逐项验证；Agent 未读取口令或解密身份包。旧业务服务/数据未切换，不再重复初始化或保留有效初始 root。

### SECINF-04A 业务侧专用 CA 与原生 TLS 接线（代码及隔离联验通过）

需求：网关与凭据服务支持 `TOKEN_SECRET_PROVIDER_CA_FILE`，仅对 `vault-transit` 生效；显式配置无效时启动拒绝，不回退系统 CA。未配置时保持系统信任库兼容。专用信任只属于该 KMS HTTP client，不修改 `SSL_CERT_FILE`、全局默认 Transport 或 OIDC/商城等其他 HTTPS client。

设计：CA 为受控只读挂载中的绝对路径 PEM 证书包，大小有界、只允许有效 CA 证书，拒绝缺失、非普通文件、组/全局可写和混入私钥/垃圾内容。允许 Kubernetes 只读投影文件的符号链接，经解析、文件身份核对及权限检查读取；挂载本身必须防业务写入。每实例创建独立 Transport，TLS 至少 1.2，关闭环境代理和透明写重试，保留主机名验证、禁止跳转与有界超时/响应。CA 在启动时加载，轮换信任包需受控滚动重启，不能误称自动热更新。

任务验收：两服务入口接线；专用 CA 正向、错误 CA/主机名、损坏/危险路径与权限负例、没有全局信任污染、不会继承代理；现有本地/remote 模式兼容（非 native 模式显式配置 CA 时拒绝，避免静默忽略）。真实隔离 TLS/Raft/Agent 下由业务原生 provider 使用投递 token 文件完成加解密与 AAD/ACL 拒绝，不借用测试 client 的 TLS Transport。完成后独立提交；本子项不提前修改仍要求 remote 的 Compose/Helm/门禁或宣称业务已接入。

实测：原生及既有 remote/生产拒绝回归 PASS（2.243 秒）；网关入口测试 PASS（1.764 秒），凭据入口编译通过（无该包测试）；三包 vet、6 项设施配置测试、架构守卫通过。默认两项真实 Transit 测试保留 opt-in，未将 SKIP 当作实测。

固定真实 OpenBao TLS/Raft/双 Agent fixture 在初始 root 已撤销后运行同源 Linux 业务测试二进制，专用 CA 两组测试与真实 provider 测试无 SKIP，输出 `OPENBAO_TRANSIT_TLS_FIXTURE_PASS` / `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`。包括真实 CA、文件 token、随机密文、AAD/跨 key 拒绝与错误主机名；Linux 文件权限和只读投影 CA 链接均实际覆盖。该 provider 测试由隔离控制进程执行，不冒充正式业务容器挂载或非 root 端到端验收。

首次新增 TLS 联验被 CA 校验拦截：旧 fixture 根证书依赖宿主 OpenSSL 默认用途。模板已明确指定 CA:TRUE 和 keyCertSign/cRLSign，与正式专用 CA 的限制一致；未放宽应用信任校验，完整重跑通过。自检确认未改全局 TLS/代理、未改 legacy remote 协议、未更改密文格式或移除 token 轮换读取；正式 Compose/Helm、安全门禁和业务接入仍待后续任务。

### SECINF-03C-7 Agent 引导文件受控投递（暂存工具通过，正式投递待执行）

需求：只把网关、凭据服务、快照备份三个 Agent 所需的 role-id/secret-id 投递给对应 Agent，离线运维身份、初始化 root、解封份额、恢复私钥和口令均不得进入投递文件。业务容器只接触短期 token，不读取这些长期机器引导秘密。

实现任务：独立本机 `kms-operator stage-agents` 通过真实终端口令解开既有 `roles.age`，完整验证实例/origin/CA/recipient/身份唯一性及 FINALIZED 摘要标记，不读取初始化包、不联网、不创建身份或撤销权限。只允许输出前三个固定角色的 role-id/secret-id，各文件 0600，逐文件同步与读回，写入受控的新 staging 目录并生成无秘密的摘要清单；既有输出拒绝覆盖。staging 与 custody/恢复包所在目录分离，不能误作为离线恢复备份长期留存。Windows 父目录先核对仅保管人和 SYSTEM；失败保留部分输出、不得宣称交付完成。

部署任务：经既有验证的 SSH 传输到专用 Agent bootstrap 目录，逐文件摘要及 UID/权限核对，Agent 以只读目录挂载，单独的短期 token sink 放在受控 /run 内存目录；仅确认投递和 Agent 登录后精确移除本机明文暂存文件。长期 bootstrap 在 Agent 侧按既定轮换制度保护，不冒充短期凭据。此任务不允许用聊天/工具读取口令或输出文件内容，不生成或恢复 root，不把运维角色捆绑上传。正式备份任务与业务 Compose 接线仍分别验收。

验收：错误身份/绑定/完成标记在写出前拒绝，缺失/重复角色拒绝，只产生六个固定文件与摘要清单、不包含运维或 root 数据，路径重叠/覆盖拒绝；真实隔离 bootstrap 完成后的同一包可正确投递，三身份可登录，离线步骤不发起任何网络请求；实际 Agent 只读 bootstrap、独立 sink 和租约须另行核对后记录现场完成。

03C-7A 实测（2026-09-08）：Windows 工具模块全量测试及 vet 通过（离线 12.750 秒、初始化 6.768 秒、operator 16.269 秒、PKI 5.285 秒）。离线测试先关闭 TLS 服务器再暂存仍成功，网络计数不变；仅六个身份文件和一个摘要清单，运维/root 排除、摘要读回、未退场/错误包/目录重叠与覆盖拒绝均通过。

最新同源 Linux operator 全包在真实隔离实例无 SKIP 通过，输出 `OPENBAO_AGENT_STAGING_FIXTURE_PASS`、`OPENBAO_BOOTSTRAP_FIXTURE_PASS` 和 `OPENBAO_OPERATOR_FIXTURE_PASS`。真实暂存文件中的三个身份再次登录并完成各自权限正负例，探测 token 撤销；四类身份及 root 退场回归也通过。初次 Linux 暂存被父目录权限拦截：Go 测试临时子目录使用 0777 再受 umask 限制，不等于受控 0700。测试现明确创建 0700 staging 父目录，未放宽工具权限要求。

此处仅为受控暂存工具验收；正式暂存需保管人本机输入既有口令，随后仍需验证 SSH 传输、服务端文件隔离、Agent 登录及本机明文副本清理。没有提前声称正式 Agent 已启动或业务已部署。

### SECINF-03C-7B 独立 Agent 部署与现场验收（通过）

保管人已完成 `AGENT_FILES_STAGED`。本任务只部署三个 Agent，不修改正式 KMS、策略、root 或旧业务。使用独立 Compose 项目，固定已验证镜像、非 root UID、只读根目录、全部移除 capabilities、禁止 core dump、限制资源及日志；只连接既有私有 KMS 网络，不发布端口、不提供代理。每个服务单独挂载只读 bootstrap 和可写 sink；配置与 CA 只读，运维身份不在部署清单内。

sink 使用宿主 `/run` 的 tmpfs，systemd-tmpfiles 在系统启动时先建立受限目录；父目录 root 0700、各服务子目录 UID 65532/0700，文件由 Agent 原子生成 0600。不能用文件存在代替令牌有效性，验收程序必须通过专用 CA/TLS 实际查询三枚令牌，验证角色唯一、有效 TTL 不超过 300 秒、显式最长 900 秒、可续租及权限拒绝，并检查真实 Docker 的隔离挂载与资源限制。验收程序只输出固定状态码，不打印请求、响应或令牌；只做自身查询、随机加解密验证和有界快照读取，不修改策略、轮换密钥或撤销 Agent token。

顺序：文档与部署模板/验收测试 → SSH 投递及摘要核对 → 启动三个 Agent → 实际挂载、身份、权限和续租核验 → 精确清除本机六份明文机器引导文件 → 独立提交。加密恢复包保留；远端 Agent bootstrap 仍为需周期轮换的长期秘密。快照读取仅为权限探测，不等于正式备份调度；主机重启后的 KMS 解封仍需保管人参与，不以 Agent 自动重启宣称自动解封或高可用。

现场结果：六文件通过本机身份包绑定、摘要、完整清单和 ACL 检查，SSH 传输后再次摘要核验，远端 UID/GID 65532、0700/0600。独立三个 Agent 启动，`KMS_AGENTS_VERIFIED` 通过；真实 TLS 查询唯一角色、活跃 300/900 秒租约、网关/凭据随机加解密及 AAD 拒绝、备份读取和越权拒绝全部通过。随后 `KMS_AGENTS_RENEWAL_VERIFIED` 确认三个身份已实际续租；并非只验证 renewable 标志。校验器最初误将 `last_renewal` 当数字，实测它为 RFC3339Nano，且宿主 Python 3.10 不能直接解析九位小数；已修正为严格格式解析并补纳秒回归测试，不调整 KMS 或角色租期来掩盖问题。

实际 Docker 校验：无主机端口/额外网络/匿名卷，全部 capabilities 移除、非 root、只读根目录、core=0、资源受限；每角色只读 bootstrap 与独立可写 tmpfs sink。系统 tmpfiles 已安装并创建目录，配置的启动顺序在 sysinit 前；未为验证而重启正式主机，不能冒充正式断电恢复验收。验证后精确删除本机六个明文暂存文件，既有加密恢复包及无秘密的摘要清单保留，服务端长期机器引导文件保留供 Agent 重新登录。

验证：Windows 14 项测试中 13 通过，1 项真实 POSIX UID/文件权限用例明确跳过；同源 Linux 六项校验器测试全部通过无 SKIP（包含真实权限/符号链接/有界读取），8 项配置测试通过；架构守卫及差异检查通过。KMS 保持健康，旧账号平台、New API、其他团队服务均未改动。正式业务接入、持续告警和定时备份尚未完成。

### SECINF-03C-8 定时加密快照（实施中）

需求：备份任务只能读取备份 Agent 的短期 token、公开 CA 和已核对的恢复 recipient，不挂载 AppRole 引导文件、运维身份或恢复私钥。通过可信 TLS 查询自身身份，确认唯一备份策略及有界有效期后读取真实 Raft 快照；不创建身份、不修改密钥、不解封、不执行恢复。快照未经外层 age 加密不得落盘。

03C-8A：交付独立 Linux `kms-snapshot` 单次命令。输入为公开配置文件，固定 API 与标准 age；有界响应及 gzip 完整性检查、实例/origin/CA/recipient/时间/快照摘要绑定。每次新建私有唯一目录，仅加密包落盘，sync 后读回密文摘要，再独占写完成清单；失败保留不完整尝试，不能覆盖或把它视为可恢复备份。当前压缩快照上限 64 MiB、展开上限 512 MiB，超限报错，后续随容量评审调整而非截断数据。离线验证入口通过保管人本机既有口令完整认证解密并验证绑定，不输出快照内容；真正 restore-force 仍只在隔离测试使用。

03C-8A 验收：真实 TLS/错误 CA、错角色/过期 token、重定向、损坏/截断/超限快照、损坏包/错误恢复身份、输出覆盖/不安全路径/权限均拒绝；既有隔离双实例恢复演练改用生产备份函数产物，源 root 已撤销后取得快照，恢复到新实例后能读回轮换前密文，旧 root 不能复活。单元测试不代替实际恢复证据。

03C-8B：通过后部署非 root、只读根目录且仅备份 Agent sink 可见的单次容器，由 systemd 每小时触发（随机延迟最多 60 秒，错过时补跑），每次运行有超时/内存/CPU 限制，失败必须表现为失败 unit。落盘目录只存加密包与无秘密完成清单。首次现场执行核对角色、密文、权限、完成标记和调度状态，并把首份加密包复制到保管人本机供离线验证。负责人已选择“由保管人定期下载并离线保存”，不配置向其他服务器或对象存储上传；每个工作日至少下载最新成功包，身份/密钥变更后立即补存。离线恢复点取决于最后实际下载的包，不能把服务器每小时备份频率当作异机 RPO。业务尚未放量，暂不自动删除任何正式快照；保留策略、容量/失败告警和正式恢复交接须单独记载实际验收，不能把同机文件数量或首次备份成功等同于灾备可用。

03C-8A 实际恢复验证通过：固定 OpenBao TLS/Raft 的双独立实例中，源 root 撤销后由仅含快照读取及自身查询策略的限时备份身份调用生产 `CaptureSnapshot`，生成带完整性清单的 age 包。原始错误身份与损坏包被拒绝，本机离线口令验证入口也执行通过；新实例恢复后可解密 v1 历史密文，key 最新版本仍为 v2，两枚过期 root 均拒绝。输出 `OPENBAO_SCHEDULED_SNAPSHOT_FIXTURE_PASS`、`OPENBAO_SNAPSHOT_RESTORE_FIXTURE_PASS`、`OPENBAO_OPERATOR_FIXTURE_PASS`，operator 全包无 SKIP。此证据使用随机隔离测试密钥，不读取正式恢复私钥，尚不代表正式快照已下载离线保存。

03C-8B 现场调度通过、离线交接待确认：受限单次容器的实际六个绑定挂载、非 root/只读根目录、仅备份 sink、无公开端口/额外网络/匿名卷、512 MiB/0.5 CPU/32 PID 等核对通过；验证用未运行空容器按明确 ID 删除，没有删除任何业务数据。正式 unit 首次运行 `Result=success`、`ExecMainStatus=0`，每小时 timer enabled/active，单次工作容器正常退出无残留，KMS 仍健康。

首份正式 `snapshot.age` 和完成清单已经由 SSH 下载到保管人本机受限目录，服务器与本机密文长度/摘要核对一致。提供本机下载与离线验证窗口，可重复下载最新成功包但不覆盖不一致文件；用户仅在本机输入既有口令，完整验证后自行复制到离线介质并确认。当前仍未收到本机完整验证/离线保存的完成反馈，不提前宣称这两项完成。加密包不包含恢复私钥；口令与原有恢复材料不通过聊天/工具读取。

验证：独立工具全模块 Windows 测试与 vet 通过；实际 Linux 双实例全包恢复验证通过；部署配置及既有 Agent 校验共 16 项 Python 测试中 15 通过、1 个 Windows 不适用 POSIX 用例跳过（其 Linux 实测已通过）。架构与差异检查通过。未配置异机自动上传、未删除正式备份；持续失败/容量告警、保留策略及完整业务上线仍未完成。
