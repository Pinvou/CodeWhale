# 正式环境安全基础设施补建

状态：实施中 / NOT_DEPLOYED。2026-09-07，部署负责人明确授权将缺失的 KMS 和服务身份签发设施纳入全新部署。

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

投递采用同目录临时文件、0600 权限、sync、原子 rename；运行目录必须预先创建且非符号链接，仅该服务的 agent 可写，业务以相同 UID 只读挂载。不能挂载单个 token 文件（会固定旧 inode），必须挂载单服务目录。正式运行宿主为 Linux；Windows 仅用于开发测试，不能将其 rename 的平台差异视为 Linux 持久化证明。`-check` 仅验证已有投递文件，不申请新令牌，供健康探针使用。02B 固定默认 RS256 与 300 秒有效期，关闭标准/隐式/密码授权/设备授权/Token Exchange 和 offline access，仅保留服务账户；读取服务账户真实 UUID，绑定授权表，禁止自定义 sub 或动态扩权。

### 部署顺序

2026-09-07 保管方式已获负责人同意：本机口令保护的离线恢复包，异地/离线另存备份，口令分开保管，不通过聊天交付明文。先完成 SECINF-03A 本机托管工具，再进行正式初始化；此子任务不代表 SECINF-03 整体完成。

恢复包采用标准 age 格式，独立工具模块固定 `filippo.io/age v1.3.2`，不改变业务服务依赖。`prepare` 仅在本机终端无回显读取及确认口令，生成 X25519 恢复身份、用 scrypt recipient 加密为 `identity.age`，只输出公钥 `recipient.txt`；`seal` 从标准输入接收材料，用该公钥加密，绝不写入明文文件。`verify` 在内存解密并验证完整性，不打印明文，不接受参数/环境口令。拒绝覆盖已有文件、短口令、损坏包和超限输入；文件写入后 sync。口令以及丢失后的不可恢复风险由保管人负责。加密不防护已被控制的本机；操作前应关闭会记录输入/内存的录屏或调试工具。

本机身份文件不含正式 KMS 密钥；正式材料只能在确认该身份可解密及备份安排后生成。仅初始化托管身份不操作服务器、不初始化 OpenBao、不删除任何数据。Windows 发布目录继承权限需显式收紧至当前用户与 SYSTEM。单人保管不冒充多人分权；长期多人 custody 为后续治理选项，不自行创建未知保管人。

公开模板及隔离测试 → OpenBao/Keycloak 固定版本与镜像摘要 → TLS/受限网络/保管安排 → 正式初始化和逐服务策略 → 真实 KMS/签发轮换负例 → 维护窗口复核精确旧目标 → 清理测试数据并从空库安装 → NATS TLS/ACL、治理角色及初始化 → 双域登录、模型与计费验收 → 放量。

本次空库安装无需旧测试密文迁移；现有生产安全门禁中依赖旧 key 的轮换证明仍需增加独立 fresh-install 分支及真实新环境轮换演练，不能伪造旧 key 或直接跳过整项 gate。

## 串行任务与验收

| 任务 | 交付与验收 | 状态 |
| --- | --- | --- |
| SECINF-01 | 原生 Transit provider、逐调用 token-file、两服务接线；HTTPS、AAD、轮换、权限拒绝、无泄漏、跳转及有界响应测试 | 代码已验证 / NOT_DEPLOYED |
| SECINF-02 | Keycloak 独立服务客户端配置与轮换器；真实签发/轮换/过期/错 subject、audience、权限拒绝 | 02A 已验证；02B 待实施，整体 NOT_DEPLOYED |
| SECINF-03A | 保管人已同意的本机离线托管工具；口令无回显、标准 age、加密身份读回、拒绝损坏/覆盖；在正式初始化前完成 | 代码与隔离测试已验证；本机托管身份设置成功（仅核对就绪标记与加密文件元数据），正式恢复包和离线备份尚待完成 |
| SECINF-03 | 固定版本 OpenBao/Agent 部署、逐服务策略、TLS/网络、初始化与解封交接、备份恢复；明确单节点边界 | 待实施 |
| SECINF-04 | Compose/Helm/生产安全门禁原生 KMS 与 fresh-install 适配、真实依赖验收及部署运行手册 | 待实施 |
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
