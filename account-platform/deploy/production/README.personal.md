# F01 个人身份、钱包和会话装配

本目录只装配已实现的个人注册、SMS、资料、密码、真实钱包激活、个人余额、Portal、桌面登录与普通 Account 事件投递。F01 必须拥有这条运行链；F18 的全组合交付工具不能代替它。这里不部署模型、付款、组织、礼赠、注销或审批执行器，也不需要 Token 数据库、KMS 或未来治理策略。

`compose.personal.yaml` 运行五个业务进程、两个迁移作业、普通 Account relay 和 TLS 入口；`event-resources` 是显式运维 profile。它使用已独立配置的 Postgres、Redis、NATS 和 Keycloak，并不接管它们或创建网络、密钥与账号。站点依赖必须位于验证过的私有网络上。业务进程不发布宿主端口，只有入口的 TLS 端口对外开放。私有 origin、URL 中的凭证、数据库 DSN、短信凭证、Token、Cookie、HMAC 和密码不得写入仓库、例子、日志或渲染后的公开配置。

`ACCOUNT_PLATFORM_PROFILE` 是业务进程运行模式，与 Compose 的 `event-resources`、`approval-operations` profiles 不同。基础模板明确将 Account 与余额 Token facade 设为 `personal`：Account 保留 Billing 审批投影、证明及结果读取，Token 保留个人余额和已有个人金融接口，不构造模型数据库、KMS、provider、模型 worker 或模型管理入口。直接运行未配置该变量的二进制仍默认 `models`；未知值拒绝启动，缺少模型配置不会隐式降级。
个人 Account 不构造组织 Billing/Token lifecycle clients，组织、成员、单位与邀请入口在业务仓库或跨域写入前明确返回既有 `ORGANIZATION_UNAVAILABLE`（503）；通用个人角色查询和合法 `PERSONAL` 上下文切换仍可用，`ORGANIZATION` 切换不可用。个人余额 Token facade 不挂载组织金融或模型管理入口（404），models 则通过显式完整 management 装配提供。需要组织功能时必须改用完整模型与组织叠加，不得仅补一个 Token URL 将个人模式当作组织部署。

叠加 `compose.finance.yaml` 前须显式配置 `ACCOUNT_PLATFORM_PROFILE=personal`，其 `approval-resources` 要求该变量非空，保持三个服务一致。个人财务的 live activate/resume 仅核验原三条 Billing consumer，不执行 Token consumer 初始化；当前离线公共 manifest 仍是既有六 descriptor 的固定期望，不能将其误作模型消费者已安装或就绪的证明。模型部署必须改用 [完整模型叠加](README.models.md)，不得把个人余额服务宣称为模型运维部署。

## 同源镜像

通过 `account-platform/Dockerfile` 分别构建 `account-service`、`identity-service`、`billing-service`、`token-gateway`、`token-portal`、`event-relay`、`event-resources`、`migrate` 和 `workload-token-agent`。`SERVICE` 仅接受本阶段实现。镜像包含实际健康检查和本阶段迁移，不编译未来策略引导器。Portal 镜像必须包含已经构建、独立审核的 `internal/portal/static` 产物。版本参数使用仓库根 `VERSION` 的实际值，不另外定义客户端支持范围。站点独立核验并提供镜像引用；配置采用 `pull_policy: never`，不自动替换镜像或隐式获取部署依赖。

## 必需站点配置

`INFRA_CA_DIRECTORY/ca.crt` 必须为已验证完整信任 bundle：同时包含私有依赖 CA 与实际公开 OIDC、SMS 和 OTLP endpoint 所需的受信任根，不可只有私有 CA。scratch 镜像的 `SSL_CERT_FILE` 使用该 bundle；每个显式私有 CA 参数继续严格验证对应证书，不关闭 TLS。agent 独立配置目录内的 CA 同样必须覆盖其真实 issuer/JWKS。

全部挂载源必须预先存在，使用各进程专属目录，并满足运行 UID 的读取权限；Compose 不自动创建宿主目录。数据库 pgpass 不与其他进程共享，密钥和密码文件使用仅所属运行身份可读取的权限。`env_file` 使用 `format: raw`。禁止将这些文件传给会展开秘密的 `compose config` 或输出原环境。

| 进程 | 私有文件中必需的实际输入 |
| --- | --- |
| Account | `DATABASE_URL`（TLS verify-full）、`WORKLOAD_IDENTITY_RULES_JSON`、`REGISTRATION_QUERY_HMAC_KEY_BASE64`、`SMS_VERIFICATION_HMAC_KEY_BASE64`、`SMS_PROVIDER` 及对应 Aliyun/HTTP provider 的真实凭证和四种用途模板；F04 起还须提供独立的 `OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64`（标准 Base64，解码后恰好 32 字节）；钱包策略版本与 1–1440 分钟 SLA 可沿既有默认。 |
| Identity | `IDENTITY_PASSWORD_HMAC_KEY_BASE64`、`AUTH_RISK_REDIS_URL`（identity ACL 用户、TLS）、`AUTH_RISK_HMAC_KEY_BASE64`、`WORKLOAD_IDENTITY_RULES_JSON`；挂载 Keycloak provisioner client-secret。没有风险 Redis 就无法提供必需的 OIDC token proxy，不可当作可运行装配。 |
| Billing | `DATABASE_URL`（TLS verify-full）、仅本阶段钱包权限的 `WORKLOAD_IDENTITY_RULES_JSON`。 |
| Token | 出站 workload token；只有个人余额 facade，无数据库、模型路由或 readiness GO。 |
| Portal | `REDIS_URL`（portal ACL 用户、TLS）；挂载专属 handoff client-secret。公开 origin 和四个 OIDC endpoint 必须一致，公开及 OIDC 使用 HTTPS。 |
| 迁移作业 | 独立 `DATABASE_URL`、`MIGRATION_ROLE`、实际 `MIGRATION_BACKUP_EVIDENCE_ID`、`MIGRATION_BACKUP_COMPLETED_AT`、`MIGRATION_BACKUP_SHA256`，以及独立 pgpass。Account 必须走实际 `accountschema.Up`；不能用通用顺序绕过阶段桥。 |
| relay | 仅 Account DSN、pgpass 和 relay NKey seed；TLS NATS origin/CA。没有审批 relay、manual replay 或自动资源修复。 |

五个 HTTP 进程和普通 relay 的生产启动还必须设置 `OTEL_EXPORTER_OTLP_ENDPOINT` 为经验证、无 URI 凭证的 HTTPS OTLP endpoint，使用已挂载 CA；站点如需认证，仅在对应私有环境文件配置，禁止记录凭证或原 exporter 错误。这里不创建 collector。

`TRUSTED_TIME_SOURCES` 至少提供三项已验证独立时间源。就绪检查只覆盖当前实际实现：Account 检查仓库，Identity 联合 Keycloak/风险 Redis，Billing 检查数据库，Portal 检查 Redis，Token 仅宿主基础门禁；不能推定持续检查所有上游。Compose 的依赖健康与 agent 检查只约束初始启动，之后每次业务调用和 JWT 到期验证仍按真实失败返回 unavailable/拒绝，不伪造成功。迁移成功后才启动相应服务。数据库 schema/角色与阶段迁移证明由 Account/Billing 的独立审核记录约束，不能根据进程编译成功推定它们已正确部署。

## 服务身份与权限

使用当前 `scripts/account-platform-workload-realm.py` 生成仅五个现有身份的无秘密 realm 导入对象，交站点受控 Keycloak bootstrap 安装并逐项验证。它只生成配置，不调用服务器，不生成或输出 client-secret。实际 secret 与 service-account UUID subject 必须从已安装 realm 独立获得，分别置入私有 bootstrap 文件和配置；不能把 client 名称当 subject。

Account、Identity、Billing、Token 和 Keycloak authenticator 必须各自获得来自已验证 workload realm 的短期 Token。结合 `compose.personal.yaml` 与 `compose.personal-workload.yaml` 运行本阶段实际 `workload-token-agent` CLI。各 agent 用 `workload-personal.json.template` 的独立已验证 issuer/client/subject 配置；bootstrap secret 在只读 `/run/bootstrap`，仅本身份可写的投影目录在 `/run/projected`，业务仅只读挂载整个投影目录，原子替换才可被后续请求观察。配置、bootstrap、输出目录不得含 symlink 或组/其他可写权限；secret、配置和输出文件满足通用库实际私有文件检查。Keycloak agent 按 Keycloak UID 1000 运行，其余按业务 UID 65532，不共享 seed/secret/Token。

本模板使用一个明确 canonical origin：`PERSONAL_PUBLIC_HOST` 为无 scheme/port/path 的已验证 DNS 主机名，`PERSONAL_EDGE_BIND_PORT` 为 1024–65535 的非特权整数（建议 8443）。将两者分别渲染为 `__PUBLIC_HOST__` 和 `__PUBLIC_TLS_PORT__`，Keycloak `hostname`、`https-port`、公开 edge 绑定、用户/workload issuer、Portal public URL 及四个 OIDC URL 的 origin 必须一致。443 特权端口不属于本模板，不通过新增 capability 借用。配置检查必须验证范围、没有残留标记和 TLS 证书覆盖此主机。

五个 writer 的 `extra_hosts` 仅将同一 canonical issuer 主机映射到 `PERSONAL_KEYCLOAK_BACKEND_IP` 的准确私有 Keycloak 地址，且 Keycloak 真实 listener 使用同一端口；因此它们直接请求真实 `/realms/pinvou-workload` 的 token/certs，公开 edge 仍拒绝该 realm。不得映射到公开 edge、任意其他 issuer 或关闭证书检查。接收端的 `WORKLOAD_IDENTITY_ISSUER` 必须与真实 Token `iss` 完全一致，JWKS 使用独立已验证的私有 HTTPS origin/同 CA。站点实际发行和解析验证未运行，不能仅凭映射模板推定它们已成立。

agent 只接收既有 Keycloak 的 client_credentials 响应，并验证真实签名、issuer、audience、azp、subject 与最多 15 分钟寿命；失败保留旧文件而不延长其有效性，接收端逐请求验证到期，写入失败不伪造就绪。`-check` 验证已投影 Token，不请求新 Token。刷新进程仅 Linux 可运行；Windows 编译/检查不能证明 POSIX 文件模式或实际 Linux 刷新。`WORKLOAD_IDENTITY_ISSUER`、JWKS、audience 与 CA 必须匹配。三个 `workload-*-rules.json.template` 只记录当前真实接收端权限；将标记替换为验证后的实际 subject，并将相应数组作为该进程私有文件的 `WORKLOAD_IDENTITY_RULES_JSON`。不得保留标记、泛化到 `internal.*` 或加入未来命令。

Keycloak 用当前同源 SPI/theme 镜像和 `keycloak-personal.conf.template` 的经验证私有配置。只在受控首次 bootstrap 时使用管理员密钥，随后退役。使用已存在的 `provision-mobile-auth.sh` 完成并复核真实 provider、用户 realm、Desktop offline scope、Portal 公共 PKCE 与专属 confidential access-only handoff client。配置更新脚本不替代实际 realm 验证。Identity provisioner 有真实用户管理权限；Keycloak realm 的管理角色不能伪造 Account 平台管理员授权，首次平台管理员只能走本阶段实际安全 CLI。

`compose.keycloak-personal-auth.yaml` 仅向该站点 Keycloak 进程添加真实 Account/Identity/SMS origin 与 authenticator 专属 workload 投影。它不能独立创建 Keycloak 进程或 realm。user realm 的公开 issuer 必须与 Account、Token、Portal、桌面配置完全一致；私有 TLS origin 证书必须包含对应 DNS 名称，CA 验证不可关闭。

## 公开 TLS 入口和风险来源

本阶段用户 realm 固定为 `pinvou`，与 Identity 的实际 token proxy 路由一致；其他 realm 不经公开入口暴露。用验证后的 public host 和私有 Keycloak DNS origin 渲染 `nginx/personal.conf.template` 到专属文件。启动前运行 NGINX 的配置检查，拒绝任何未替换标记。模板不公开 Keycloak 管理 API、业务 `/internal`、Account/Token 的直接 `/admin` 或 Billing；个人管理仍只经 Portal 已实现的受控 BFF 与真实权限执行。实际 token grant 经 Identity 风险准入，其他 user realm/theme 请求才到 Keycloak。模板关闭请求日志，避免 callback query、授权凭证和手机号进入日志。

模板使用 Docker 内置 DNS `127.0.0.11` 每 10 秒更新业务上游地址，容器重建后无需重启入口。`__KEYCLOAK_ORIGIN__` 必须是私有 DNS 名称及可选端口，不包含 scheme、路径或 IP 字面量；模板将 DNS 名称用于 HTTPS SNI 和证书名称核验，并保留真实请求路径。发布验证应重建一个隔离业务容器，验证 DNS 更新后请求恢复且 TLS 错误仍失败关闭。

入口必须固定在 `PERSONAL_EDGE_BACKEND_IP`，该地址与私有网络的实际分配相符。NGINX 强制将 `X-Pinvou-Client-Source` 和 `X-Forwarded-For` 设置为实际连接地址，不追加或相信浏览器输入。Portal 的 `PORTAL_TRUSTED_PROXY_IPS` 只接受这一个显式入口身份；非受信任连接仍按实际 peer 处理。Identity 固定在 `PERSONAL_IDENTITY_BACKEND_IP`，两者都须在既有私有网络 IPAM 中独立校验且不能相同。Identity 的 `IDENTITY_TRUSTED_PROXY_IPS` 同样只配置这个准确 edge 地址，缺省为空时仅采用规范化的直连 peer；配置拒绝 CIDR、主机名、通配符和带 zone 的地址。Identity 公共 token grant 只能由入口访问，入口覆盖单值 XFF；只有来自该准确 peer 的单个合法 IP XFF 才被采用，重复头、链和非法值回退为直连 peer，非法连接地址返回 400 且不进入风险准入或上游。其上游仅转发已准入的规范化单值 source，不转发浏览器的 source、Cookie 或 Authorization。Keycloak 只信任准确 edge 与准确 Identity 两个 peer 的 forwarded headers，不加 CIDR 或全部信任；生产 token proxy 单值 XFF 接线须随这项配置独立验证。不能以任意 XFF、整个公开地址网段或可绕过的业务宿主端口代替这条信任边界。

Account 的 `ACCOUNT_TRUSTED_SOURCE_PEER_IPS` 只包含固定入口、Portal 和 Keycloak 的准确 peer IP；未配置时忽略所有来源头，采用直连地址。生产模板新增 `PERSONAL_PORTAL_BACKEND_IP` 与 `PERSONAL_KEYCLOAK_BACKEND_IP`，应与 edge、Identity 地址一并在既有 IPAM 核验且互不相同。将 Keycloak overlay 与站点配置组合时保持其既有必要网络并接入同一 `PERSONAL_BACKEND_NETWORK`，不要用动态 DNS 解析结果或整个 CIDR 作为可信来源。可信 peer 也只能传递一个合法 IP 来源头；部署验证须确认任意非可信 peer 伪造此头不改变风险计数来源。

## Redis 与普通事件资源

为单独的账户 Redis 使用 `redis.conf` 和渲染后的 `redis-personal.acl.template`，保持 TLS、default user disabled 与 noeviction。Portal 和 Identity 使用不同 ACL 用户；模板只包含实际 session/lineage/handoff/refresh 及风险计数命名空间和实际 Lua 所需命令。`redis-health.sh` 仅证明匿名连接无法 PING，并不证明业务权限正确；上线前必须实际验证 Portal 的刷新、logout/lineage 与 Identity 风险写入。

NATS 使用现有 `nats.conf`、`authorization.conf` 和 `runbooks/nats-resources.md`。独立 operator 首先显式创建缺失固定 stream，然后严格检查。普通 relay 只使用专属 publisher NKey；已有 stream drift 必须拒绝，不自动覆盖或借 operator 身份执行。relay 与 operator 不共享 seed，不添加未来 Billing/Token consumer 或审批 subject。

## 运行验证边界

部署前依次核验镜像/私有挂载、真实 TLS/证书与身份、迁移 schema/角色、固定 NATS 资源，再启动业务和公开入口。实际注册必须直到 Billing wallet READY；密码/资料变更须完成 Keycloak fencing 和会话撤销；验证 Portal PKCE/refresh/logout/handoff、Desktop 登录恢复与个人余额。未知 ACK 按原请求恢复，不强制激活，不替换主体。

本文件和模板不表示任何环境已经运行、realm 已安装或部署通过。源码、静态配置和有限单测结论只能覆盖各自实际输入；完整 Keycloak、TLS、Redis ACL、服务身份和生产部署验证另登记实际结果，未运行项如实披露。
# F01 密码预检与修复审计边界

Identity 的匿名密码预检保持原公开路径与 15 分钟 handle 有效期。`IDENTITY_PASSWORD_REFERENCE_CAPACITY` 限制当前未过期 handle 数量：默认 4096，可配置 1–65536；非法、零或无界配置拒绝启动。达到容量只拒绝新预检（503 `PASSWORD_PRECHECK_CAPACITY_REACHED`），不驱逐已接受 handle，不改变已有 48 小时 operation 绑定或正常回调。此上限是资源安全默认，不是并发用户服务承诺；容量和源准入应按实际流量继续评估。4096 个最大长度密码的原始文本最多 512 KiB；完整填充 benchmark 的累计分配约 5.10 MB，不能据此宣称进程驻留总内存。现有 handle 仍是原内存存储，不新增跨进程恢复承诺。

注册修复在产生效果前持久记录 `registration.repair.retry.requested` 或 `registration.repair.compensate.requested`；其 `SUCCEEDED` 仅表示请求审计写入成功，不表示身份或钱包业务完成。写入失败时不派发修复效果。原 wallet/compensation 事务内强审计保持。结束审计写入失败时，保留已记录请求和原业务结果，返回 `X-Registration-Repair-Audit: pending` 并记录稳定诊断；该信号不触发资金动作或旧代际重派。修复后读不到权威 operation 时返回503 `REGISTRATION_REPAIR_RESULT_UNKNOWN`，不可把空对象当成功回执。自动可靠补齐结束审计仍未实现，继续由 #1624 跟踪；不得把 requested 事实或当前列表读取推断为未收到的业务完成回执。
