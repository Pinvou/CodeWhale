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

### 部署顺序

公开模板及隔离测试 → OpenBao/Keycloak 固定版本与镜像摘要 → TLS/受限网络/保管安排 → 正式初始化和逐服务策略 → 真实 KMS/签发轮换负例 → 维护窗口复核精确旧目标 → 清理测试数据并从空库安装 → NATS TLS/ACL、治理角色及初始化 → 双域登录、模型与计费验收 → 放量。

本次空库安装无需旧测试密文迁移；现有生产安全门禁中依赖旧 key 的轮换证明仍需增加独立 fresh-install 分支及真实新环境轮换演练，不能伪造旧 key 或直接跳过整项 gate。

## 串行任务与验收

| 任务 | 交付与验收 | 状态 |
| --- | --- | --- |
| SECINF-01 | 原生 Transit provider、逐调用 token-file、两服务接线；HTTPS、AAD、轮换、权限拒绝、无泄漏、跳转及有界响应测试 | 代码已验证 / NOT_DEPLOYED |
| SECINF-02 | Keycloak 独立服务客户端配置与轮换器；真实签发/轮换/过期/错 subject、audience、权限拒绝 | 待实施 |
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
