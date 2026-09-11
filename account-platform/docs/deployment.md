# 账号平台部署与运维

> 状态：当前单机 Compose 部署基线；生产提升仍以本文列出的 NO-GO 门禁为准
>
> 校准日期：2026-08-31
>
> 安全要求：本文只列配置名称和占位符，不保存真实域名、IP、账号、密码、AccessKey、Token 或商城密钥。

## 1. 部署形态

### 1.1 当前实际形态

当前环境运行在单台 Linux 主机上：

- Docker Compose 管理账号平台应用服务和基础设施。
- 仓库基础定义为 `deploy/compose/compose.yaml`，远程公共 URL 覆盖为 `compose.remote.yaml`，生产环境还可叠加不入库的站点专用 override。
- 边缘主机或应用宿主机上的反向代理终止 HTTPS，并把允许公开的路径转发到应用主机回环端口；边缘与应用分离时，转发链路、证书、防火墙和 MTU/MSS 持久化均属于恢复边界。
- 独立 New API 为模型数据面，由 Token Gateway 通过私网/容器网络调用。
- 发布物放在带 Git Commit 和时间戳的版本目录，稳定符号链接指向当前版本。
- 数据库、Redis、NATS、New API 和内部管理端口不直接向公网开放。

该形态是可运维的单机部署，但不是高可用架构。主机、Docker Engine 或本地 PostgreSQL 故障会影响整个平台。

“Compose 能启动”不等于“系统可完全重新部署”。恢复现有系统还必须具备同一恢复集内的四库、NATS、独立模型网关、站点 Secret、边缘代理/证书/网络配置和不可变发布制品。完整新主机重建统一执行 [`deploy/runbooks/full-redeployment.md`](../deploy/runbooks/full-redeployment.md)；缺少恢复集时只能创建空环境，不能宣称恢复了当前用户、资金和权限状态。

当前公网单机站点在同时满足以下条件前只能按 UAT/受控调试环境管理，不得仅因使用正式域名或真实 Provider 就宣称为生产：`APP_ENV` 为正式环境值；所有测试 Provider、测试证据和暂定价格开关关闭；镜像、配置与迁移均绑定不可变发布标识；备份恢复、容量、高可用、安全和客户端三层门禁均有同一候选版本的有效证据。当前环境状态必须写入每次发布的 `deployment-manifest.json`，不得从域名、Compose project 名或人工口头说明推断。

### 1.2 仓库提供的目标形态

`deploy/helm/account-platform/` 是 Kubernetes/Helm 生产目标模板，包含 Deployment、Service、健康检查、指标抓取和网络策略等材料。除非实际集群已经部署并验证，不得以 Chart 存在为由宣称当前环境具备多副本、高可用、自动故障转移或既定 RPO/RTO。

`production-new` 还会强制开启跨可用区/节点拓扑约束、PDB 与零不可用滚动策略，并禁止 Chart 内单副本 Redis/NATS，改用 `rediss://`、`tls://`/HTTPS 管理监控端点的外部高可用依赖。按 `deploy/runbooks/high-availability-failover.md` 完成 Pod、节点、区域、滚动升级和依赖主节点丢失场景后，运行 `npm --prefix account-platform run check:production-ha -- <evidence.json>`；没有同一候选版本的实际证据时仍标记 `NOT_DEPLOYED`。

性能发布按 `deploy/runbooks/performance-capacity.md` 冻结发布、数据集、资源和负载档位，并运行 `npm --prefix account-platform run check:production-performance -- <evidence.json> --expected-release-sha256=<候选镜像或制品 SHA-256> --page-trace=<逐页轨迹> --mutation-trace=<并发变更轨迹>`。发布流水线传入的可信候选摘要必须与证据中的 `release_sha256` 完全一致，命令会读取两份轨迹制品并重算摘要。测试报告必须保留全部慢请求/错误样本并分离平台、Provider、排队和客户端网络，不能用模板值或开发机冒烟替代生产等价容量结论。

人工案件 worker 由 `MANUAL_REVIEW_ON_CALL_USER_IDS` 接收按升级顺序排列的稳定用户 UUID。Compose 示例值只用于开发；正式 Helm 发布前必须把 `global.manualReviewOnCallUserIds` 替换为真实值班负责人链。`production`/`production-new` 缺失该配置时 Account、Billing、Token 均拒绝启动，避免 SLA 超时退化成只有通知而无人接手。更换值班人只影响后续升级，不重算已创建案件的认领、首次响应、止损或解决截止时间。

## 2. 服务与端口

Compose 的宿主机端口均绑定回环地址，供本机反向代理或运维访问：

| 组件 | 回环端口 | 生产公开策略 |
|---|---:|---|
| Keycloak | 18080 | 只公开 Realm 登录和静态资源路径；不公开 `/admin/` |
| Account Service | 18101 | 仅反代桌面注册/账号所需路径 |
| Identity Adapter | 18107 | 只公开密码预检路径；内部身份管理路径不公开 |
| Billing Service | 18102 | 不直接公开；通过 Portal/Gateway BFF |
| Token Gateway | 18103 | 反代 OIDC 保护的模型和用户业务入口 |
| Token Portal | 18105 | 公开个人中心、管理后台及同源 `/api/*` |
| Compose 内 New API profile | 18106 | 仅本机初始化/受限管理；模型调用走私网 |
| PostgreSQL / Redis / NATS | 不绑定公网 | 仅 backend 网络 |
| Event Relay ×3 | 不公开 | 指标与健康仅供内部监控 |

开发/测试 Compose 另将非生产 Payment Simulator 绑定到回环端口 `18104`，只供自动化验证和测试付款页面使用。该端口、工作负载和数据库不属于生产服务清单或生产公开面。

远程部署必须把所有用户可见 URL 统一到 `<public-base-url>`，避免 OIDC issuer、回调 URL、Cookie 域和桌面配置不一致。

## 3. 服务清单

生产业务服务：

- `account-service`
- `identity-service`
- `billing-service`
- `token-gateway`
- `token-portal`
- `account-event-relay`
- `billing-event-relay`
- `token-event-relay`
- `keycloak`
- `postgres`
- `redis`
- `nats`
- 独立 `newapi`

一次性迁移服务：

- `account-migrate`
- `billing-migrate`
- `token-migrate`

仅开发/测试支撑组件：

- `mock-newapi`
- `payment-simulator`
- `payment-simulator-migrate`

这些组件不属于产品服务边界。它们可以部署在与真实用户、真实钱包和真实 Provider 隔离的开发/测试环境；生产环境不得启动、路由或保留其数据库，生产 Helm 也不得包含对应工作负载。

远程 Compose 覆盖把 `payment-simulator` 主进程和迁移置于 `development-simulator` profile，并通过 `ENABLE_PAYMENT_SIMULATOR_DB=false` 跳过测试角色和数据库初始化。只有显式启用该 profile 的隔离开发/测试环境才可创建这些资源；生产站点不得覆盖该保护。

生产必须显式选择正式 Payment Provider 并失败关闭。Billing 对 `development_simulator` 只接受 `APP_ENV=development/test`；Helm 默认 `BILLING_PAYMENT_PROVIDER=h3c_mall`、`BILLING_ALLOW_TENTATIVE_PRICES=false`，使用供应商中立通知变量且无 Simulator 工作负载或配置。启动还必须找到与 Adapter 版本匹配的 `FROZEN` 商品映射，否则拒绝就绪，不能回退测试路径。没有回调和账单能力的 Provider 还必须声明长期历史查询、有限支付/晚到窗口、权威未支付终态和足够证据保留期；任一条件不满足时即使存在 `FROZEN` 映射也拒绝创建正式订单。

## 4. 目录约定

建议的单机发布布局：

```text
/opt/
  pinvou-account-platform-<commit>-<timestamp>/
    account-platform/
      deploy/compose/.env        # 站点 Secret 的运行副本，0600，不入仓库
      deploy/compose/compose.site.yaml  # 私有配置运行副本
    pinvou3-app/
    scripts/
    VERSION
    deployment-manifest.json     # 脱敏发布清单，不含 Secret
  pinvou-account-platform-current -> pinvou-account-platform-<commit>-<timestamp>/
  pinvou-account-platform-backups/
    <deployment-id>/
/etc/pinvou-account-platform/    # 0700；由 Secret/私有配置系统恢复，不属于发布目录
  account-platform.env           # 0600；站点 Secret 的受控来源
  compose.site.yaml              # 0600/0640；站点私有覆盖
/var/lib/pinvou-recovery/
  <deployment-id>/               # 0700；短期恢复工作区，不是长期备份唯一副本
```

不要在原目录上直接覆盖源码。新版本在独立目录完成构建、迁移和验证后，再原子切换 `pinvou-account-platform-current`。日常发布可以从 `/etc` 下的受控来源安装站点 `.env` 和 override；从当前版本复制只能作为短期发布便利，不能作为灾难恢复方案。Secret/override 必须另有加密、离机、可验证恢复的副本，复制后再次核对权限、摘要和目标路径；不得把它们移动到仓库根、发布清单或构建日志中。

`deployment-manifest.json` 至少记录 `deployment_id`、完整 `source_commit`、工作树/制品是否干净、构建时间、环境等级、Compose 文件 SHA-256、迁移清单 SHA-256、每个服务的镜像 ID 与不可变 digest、上一可回滚部署及其镜像 digest、迁移计划、完整恢复集 ID、四库/NATS/模型网关/边缘配置备份引用、Secret 版本引用、验证结果和操作者。该清单不得记录 Secret 值、Token、Cookie、个人信息或 Provider 原始报文。若健康响应、镜像标签和清单不能共同证明运行 Commit，发布失败关闭。

每次正式迁移必须先运行 `node scripts/account-platform-migration-manifest.mjs check`，创建24小时内的一致性快照，并把不含凭据的快照 ID、完成时间和 SHA-256 注入迁移 Job；缺项会在连接数据库前失败。Job 成功后还要归档 `migration.post_verification` 摘要及四库只读审计结果。新增迁移只能运行 `node scripts/account-platform-migration-manifest.mjs append` 追加更高版本，禁止编辑、删除或重命名清单内历史文件。

正式流量开放前运行 `npm --prefix account-platform run check:production-recovery -- <evidence.json>`。该证据必须来自实际备份系统和隔离恢复演练，覆盖资金60秒/配置300秒 RPO、核心60分钟/非关键4小时 RTO、连续WAL、日增量、周全量、TLS/AES-256、独立KMS、异地不可变副本、月/季/年演练以及三年财务证据/90天普通日志的到期清除收据；仓库默认值和模板不能替代证据。

## 5. 前置条件

- Linux 主机、Docker Engine 和 Compose Plugin。
- 可解析且已配置 TLS 的 `<public-base-url>`。
- 反向代理能够访问本机回环端口。
- 独立 New API 已初始化管理员、供应商 Channel 和最小权限 API Token，并能按请求 ID 查询用量日志。
- 阿里云短信认证的 RAM AccessKey、签名和模板已审核可用。
- 新华三商城环境和 App 凭据已可联调；运行时目录中至少存在一条经受信准入验证、与目标 Provider/Adapter 版本匹配的可售商品映射。部署资料不记录 SKU/BOM、金额、权益或商品数量。
- Secret 通过主机 Secret 文件、Docker Secret、Vault/KMS 或平台 Secret 管理注入；不能进入 Git。
- 发布前有足够空间保存新镜像、数据库备份和上一个可回滚版本。
- 已明确发布负责人、数据库负责人、支付尾单负责人和回滚决策人。
- Account 数据库允许迁移用户安装 `pgcrypto` 扩展；若由 DBA 预装，发布前核验 `digest` 与 `gen_random_bytes` 可用。
- 若目标包含整机/持久状态重建，已经从独立存储取得 `RESTORABLE` 的完整恢复集，覆盖四库、NATS、模型网关、站点 Secret、边缘配置和不可变制品；单独的数据库 dump 或发布目录副本不满足门禁。
- 已在隔离环境使用相同恢复格式完成演练，并记录实际 RPO/RTO、镜像/迁移版本、恢复工具版本和全链路验收；不能把当前线上主机作为首次恢复试验目标。

## 6. 配置清单

### 6.1 通用与数据库

| 类别 | 配置名 |
|---|---|
| Compose | `COMPOSE_PROJECT_NAME`、`PUBLIC_BASE_URL`、`PORTAL_PUBLIC_URL=https://account.pinvou.com` |
| PostgreSQL 管理 | `ACCOUNT_PLATFORM_POSTGRES_PASSWORD` |
| 分库密码 | `ACCOUNT_DB_PASSWORD`、`BILLING_DB_PASSWORD`、`TOKEN_DB_PASSWORD`、`KEYCLOAK_DB_PASSWORD` |
| 开发库密码 | `PAYMENT_SIM_DB_PASSWORD`、`NEWAPI_DB_PASSWORD` |
| 内部鉴权 | Kubernetes 投射 OIDC 工作负载令牌：`WORKLOAD_IDENTITY_TOKEN_FILE`、`WORKLOAD_IDENTITY_ISSUER`、`WORKLOAD_IDENTITY_JWKS_URL`、`WORKLOAD_IDENTITY_AUDIENCE`、`WORKLOAD_IDENTITY_RULES_JSON`；Account 另从 Secret 读取 `HUMAN_DELEGATION_SIGNING_KEY_ID` 与 `HUMAN_DELEGATION_SIGNING_KEY_BASE64URL` 签发最长 5 分钟的人类操作委托；仅 development/test 可使用 `INTERNAL_SERVICE_TOKEN` 兼容路径 |

`WORKLOAD_IDENTITY_RULES_JSON` 的每个 Subject 只能列出接收路由实际声明的精确 `internal.*` 权限，不接受方法/宽路径前缀或 `internal.*` 通配符。Billing 从独立 Secret `HUMAN_DELEGATION_VERIFICATION_KEYS_JSON` 读取“key ID → Ed25519 公钥”集合；轮换时先发布新公钥并保留旧公钥，待最长 5 分钟旧委托自然失效后切换 Account 签名 key ID，再移除旧公钥。私钥只进入 Account，公钥消费者不能签发委托。

每个数据库用户必须使用不同随机密码。生产环境不得沿用 `.env.example` 中的占位值。

### 6.2 OIDC 与 Keycloak

- `KEYCLOAK_ADMIN_USERNAME`
- `KEYCLOAK_ADMIN_PASSWORD`
- `KEYCLOAK_PROVISIONER_CLIENT_ID`
- `KEYCLOAK_PROVISIONER_CLIENT_SECRET`
- 各服务的 `OIDC_ISSUER`、`OIDC_JWKS_URL`、`OIDC_AUDIENCE`
- Portal 的 `PORTAL_PUBLIC_URL`、`PORTAL_OIDC_AUTHORIZATION_URL`、`PORTAL_OIDC_TOKEN_URL`、`PORTAL_PRIVATE_CA_FILE`、`PORTAL_OIDC_CLIENT_ID`

`OIDC_ISSUER` 必须与外部 Token 中的 issuer 完全一致；容器访问 JWKS 可使用内部 URL，但不能改变验证的 issuer。
Portal 的浏览器授权地址使用公开 issuer；服务端授权码交换使用 Keycloak 内部 HTTPS 服务名，并由
`PORTAL_PRIVATE_CA_FILE` 在 Portal 专用 HTTP 客户端中扩展系统信任。不得把授权码交换发送到只允许密码和刷新令牌的公网风控入口。

### 6.3 手机认证

- `MOBILE_AUTH_ENABLED`
- `IDENTITY_HTTP_URL`
- `IDENTITY_PASSWORD_HMAC_KEY_BASE64`（Identity Adapter 专用、至少 32 随机字节的无填充标准 Base64）
- `REGISTRATION_QUERY_HMAC_KEY_BASE64`（Account 专用、至少 32 随机字节，与密码和短信 HMAC 密钥隔离）
- `WALLET_PROVISIONING_POLICY_VERSION`
- `WALLET_PROVISIONING_SLA_MINUTES`（默认 15 分钟，创建任务时快照）
- `SMS_PROVIDER=aliyun-managed|http-generated`
- `ALIYUN_SMS_ACCESS_KEY_ID`
- `ALIYUN_SMS_ACCESS_KEY_SECRET`
- `ALIYUN_SMS_SIGN_NAME`
- `ALIYUN_SMS_REGISTER_TEMPLATE`
- `ALIYUN_SMS_LOGIN_TEMPLATE`
- `ALIYUN_SMS_ENDPOINT`
- `SMS_VERIFICATION_HMAC_KEY_BASE64`（仅平台生成验证码模式，独立密钥）
- `SMS_HTTP_ENDPOINT`、`SMS_HTTP_BEARER_TOKEN`（仅平台生成验证码的通用发送 Adapter）

`aliyun-managed` 由供应商生成并核验验证码；`http-generated` 由 Adapter 使用密码学安全随机源生成六位验证码，Account 只持久化带服务端密钥的校验数据，再通过通用 HTTPS 发送端点投递。两种模式由启动配置和依赖注入选择，Account 挑战状态机不包含厂商判断。RAM 用户只授予所选模式所需的最小权限。AccessKey、Bearer Token 和 HMAC 密钥不写入命令行参数、Shell 历史或日志；泄露后立即轮换。

当前 Identity Adapter 的注册密码引用及操作 ID 到密码 HMAC 的短期绑定只存在于单进程内存，Helm 固定 `identityService: 1`；Identity 创建前滚动重启会使尚未完成的引用失效，持久注册 Saga 会安全补偿并要求客户端重新预检。Identity 已创建后，Account 可通过 Keycloak 中的稳定注册操作属性恢复，不依赖内存引用。不得通过普通 Redis/业务数据库复制原始密码来扩副本；Identity 高可用需要专门的受保护短期材料方案和安全评审。

Account 启动注册恢复、资料变更恢复、钱包初始化、角色绑定到期和审批期限 worker。发布前必须确认 `registration_operations`、`profile_change_requests`、`password_reset_operations`、`wallet_provisioning_jobs` 以及 Account `000016_role_binding_lifecycle`、`000018_approval_commands`、`000019_organization_lifecycle`、`000020_organization_unit_lifecycle`、`000021_membership_governance`、`000022_global_active_context`，Billing `000017_approval_execution`、`000018_organization_lifecycle`、`000019_organization_unit_blockers`、`000020_organization_member_lifecycle`、`000021_organization_entitlement_grants`、`000022_versioned_metering_pricing`、`000023_payment_provider_catalog`、`000024_payment_evidence_lifecycle`、`000025_receivables_and_context_freeze`、`000026_hold_operations_evidence`、`000027_targeted_refund_lifecycle` 和 Token `000008_organization_lifecycle`、`000009_organization_unit_blockers`、`000010_protocol_formula_versions` 迁移完成，注册查询 HMAC 密钥在同一部署周期内稳定，且 Billing/Token 内部组织、节点及成员生命周期接口可用。角色 worker 每分钟固化已到期的组织与平台授权；即使 worker 短暂延迟，授权查询也会按数据库 `now()` 立即拒绝过期绑定。运维应监控仍满足 `revoked_at IS NULL AND expires_at <= now()` 的绑定积压、`account.role-binding.changed.v1` Outbox 和 `role.binding.expire` 审计；不得通过直接改表延长临时/紧急授权。

三类权限发布必须先应用 Account `000034_three_scope_authorization`，再滚动 Account、Token 和各入口服务。迁移将现有内置人类角色权限全量替换为审定真值表、为非终态用户回填 `PERSONAL_USER`、使存量授权缓存版本失效，并把节点授权从组织权限数组中分离；发布后需抽样核对个人、平台、组织和节点四类版本及节点绑定返回。Account `000033_payment_provider_resume_policy` 曾在未部署版本中误用不存在的 `scope_type` 列，发布包只接受迁移清单冻结的新摘要；若任一数据库记录旧摘要，必须停止升级并人工核验，不得跳过校验或手工伪造迁移成功。

受控审计导出上线先执行 Account `000032_audit_export_protection`，再滚动 Account、Portal 和桌面管理制品。Account 启动必须从 Secret 读取 `AUDIT_EXPORT_MASTER_KEY_BASE64`，其值为32个独立随机字节的严格标准 Base64，且不得与注册、认证、支付或模型密钥复用；Helm 对应运行时 Secret 键为 `audit-export-master-key-base64`。轮换前必须等待所有未下载任务归零或显式使其失败并通知操作者，因为任务的随机文件密钥在成功下载、失败或过期前仍由该主密钥包裹；文件密钥只在创建响应或同一幂等请求的恢复响应中返回，普通状态查询不提供恢复。上线抽样验证无权限、认证超过15分钟、超过31天、100,000行、500MB、每用户2并发/10每日任务均失败关闭；默认文件不可逆化主体并脱敏引用，PII 模式只接受带10–500字符原因、最长24小时且未到期的 `platform.audit.pii.export` 临时绑定，任何角色不得常驻获得该权限。验证内容和文件名水印、AES-256-GCM/HMAC-SHA-256封装、15分钟票据、单次成功下载、重复/过期下载无泄露，以及创建、就绪、失败、下载和过期事件连续性。禁止浏览器拼接审计列表、直接读取 `file_payload`、延长票据或恢复已消费/过期文件；异常时暂停新任务并保留不可变事件，采用前向修复。

组织生命周期上线必须先检查现有活动组织名称在忽略大小写后无重复；若发现重复，须先由业务确认保留名称并完成审计化更名，不得绕过唯一索引。随后先迁移 Billing 与 Token，再迁移 Account，并先滚动 Billing/Token、再滚动 Account，确保 Account 开始调用两者的 `/internal/v1/organizations/{organization_id}/lifecycle` 时目标端点已经可用。上线后抽样验证：同一创建幂等键只产生一个组织；停用后组织钱包为 `FROZEN`、Token 组织投影为 `DISABLED`，且新充值/Hold/组织用量均被拒绝；余额、有效权益、未结算 Hold、支付/退款/差异、未终态用量或治理阻断任一存在时删除返回 `ORGANIZATION_LIFECYCLE_BLOCKED`；全部收敛后 Account、Billing、Token 分别为 `DELETED`、钱包 `CLOSED`、`DELETED`，历史账本、订单、用量和审计仍可按保留权限查询。远端超时或版本冲突只能保持或进入更严格状态，不得直接改表解冻或跳过预检。

全局活动上下文迁移会为每个用户从最近一条成功的 `session.active_context.set` 审计选择初值；没有可靠切换证据或目标组织/成员关系已失效时回退 PERSONAL，普通 Session 最近活动时间不能覆盖切换证据。迁移前应统计同一用户存在多个不同 Session 上下文的数量并保存只读报告，迁移后必须确认 `user_active_contexts` 与 `users` 一一对应、所有 ORGANIZATION 记录均指向有效组织及成员、`context_version >= 1`。滚动顺序为先完成 Account 数据库迁移并滚动 Account，再滚动 Token Gateway、Portal 和桌面制品；旧客户端在切换接口缺少 `expected_context_version` 时会收到明确请求错误，因此必须在兼容窗口内同步发布。上线抽样使用同一用户的 Web 与桌面：一端切换后另一端聚焦可见相同上下文，旧版本并发切换返回冲突，成员移除和组织停用均自动回到个人；不得直接更新 `user_active_contexts` 绕过单调版本触发器。

组织节点生命周期沿用同一上线顺序。迁移 Account 前先检查同一组织、同一父节点下的活动节点名称在忽略大小写后无重复。抽样验证创建同键重放不生成第二个节点，同键异请求冲突；移动以 `expected_structure_version` 拒绝旧树写入且闭包查询只看到完整旧树或新树；删除含子节点、成员、有效/未来角色、审批、Billing 授予登记、Token Account/路由或未终态用量的节点时返回 `ORGANIZATION_UNIT_LIFECYCLE_BLOCKED` 及逐项原因。删除尝试后即使远端超时，Billing/Token 的 `DELETION_PENDING` 也必须继续阻止新增关联事实；清零重试后各域进入 DELETED，Account 节点、闭包、命令和审计仍保留。禁止通过直接改表解除限制态或物理清除历史节点。

成员治理迁移前必须执行与迁移内相同的检查，确认每个未删除组织恰有一个当前有效 OWNER；发现零个或多个所有者时先通过审计化修复窗口收敛，迁移不得自动选择。上线仍按 Billing 先、Account 后：先确认 Billing 成员生命周期接口可用，再开放新版邀请和成员命令。抽样验证手机号、用户名和链接邀请均只消费一次，任意目标探测不返回用户是否存在；重复添加/移除及所有权转让同键返回原结果，同键异请求冲突；成员移除后所有 Session 立即回退个人上下文、旧授权失败且 Billing 投影为 REVOKED，重新加入以更高成员版本恢复 ACTIVE。不得物理删除邀请、角色绑定或命令历史，也不得绕过专用事务直接改 OWNER。

组织权益授予上线必须在新版 Token Gateway、Portal 和桌面开放入口前完成 Billing `000021_organization_entitlement_grants`，并确认组织生命周期、成员生命周期投影已经追平 Account。抽样选择一个同时含 BASE/BONUS 的组织权益包：并发使用同一组织幂等键的同请求只能得到同一授予，同键异请求必须冲突；两个管理员竞争同一组成余额时成功总额不得超过该组成可用额；批量中一个无效成员或不足额项目不得回滚其他成功项。成员移除后授予应进入 SUSPENDED 且组织组成不回池，旧成员版本的 Hold 必须失败；更高版本重新加入后仅恢复未到期授予。普通撤回只允许不存在 Hold 且仍有可撤回未消费额的授予，并把当前全部可撤回额返回原包原组成；`ACCOUNT_CLOSED` 必须拒绝并等待专用回收流程。发布后监控授予/组成守恒约束错误、幂等指纹冲突、批量逐项失败率和 `member_quota_grant_audits` 连续性；禁止直接修改授予或组成余额桶。

版本化计量与定价上线按 Billing、Token、Portal 的顺序滚动：先执行 Billing `000022_versioned_metering_pricing` 并启动新版 Billing，确认历史三元组价格已回填为发布版本且同一定价空间不存在重叠；再执行 Token `000010_protocol_formula_versions` 并启动新版 Gateway，确认 CHAT v1 协议公式已发布、Billing 中存在对应不可变公式绑定；最后发布后台管理页面。抽样验证模型覆盖优先、显式全局继承、固定零价和缺价/歧义错误；创建 Hold 后撤销、结束或替代价格，续租、追加预占与结算仍须使用原快照。长流式调用应在租约到期前续租，并在预计成本超过最大预占前追加额度，否则中止可控上游并进入复核。监控价格解析错误、发布冲突、Hold 续租/追加失败和公式同步失败。回滚应用时保留新增迁移和不可变历史；已有新版价格、公式或 Hold 快照后禁止执行 down migration。

财务审批发布后，Billing Service 同时以 durable `billing-command-executor-v1` 消费 `account.approval-command.authorized.v1`。上线顺序必须先完成两库迁移，再启动 Account 与 Account Relay，最后滚动 Billing；Billing 必须能访问内部 NATS 和 Account 内部执行复核端点。监控应覆盖批准后五分钟仍未进入 `EXECUTING`、`EXECUTION_FAILED`、消费者重复投递和 `account-platform.dead-letter.account.approval-command.authorized.v1`。重放只能使用原事件和原指纹，禁止修改消息体、直接调用账本 SQL 或恢复旧 `/adjustments/:id/approve` 路径。正式 Helm 必须为每个 HTTP 服务、Keycloak 和三个 Relay 保留独立 ServiceAccount；OIDC 投射令牌最长 15 分钟并按次重读，接收服务按 subject、method、path 白名单失败关闭。三个 Relay 及 `billing-command-executor` 分别挂载外部 NATS operator 签发的 `.creds`，只授予所需 JetStream API、消费/确认 Subject 及本域发布 Subject；不得复用 Relay 凭据或授予 `>` 全局发布。实际集群上线前必须验证 issuer/JWKS 可达、audience 匹配、轮换不中断、跨服务/跨 Subject 拒绝和凭据撤销，因此当前仍为 `NOT_DEPLOYED`。

Keycloak provisioner 服务账号还必须具备读取/更新用户、重置密码和撤销用户会话的最小管理权限；缺少任一权限会使资料或密码操作进入稳定失败/修复状态，不得临时扩大为 realm-admin。轮换查询密钥会使尚未完成注册的旧查询令牌失效，必须采用覆盖最长 48 小时令牌窗口的双读或维护窗口方案；当前实现不提供双密钥读取，因此不得无计划直接轮换。运维应监控 `REPAIR_REQUIRED`、`COMPENSATION_PENDING`、钱包 SLA 超时和 `next_retry_at` 积压；人工补偿只能通过受权限保护的命令执行，禁止直接改表或直接删除 Identity。

Account 的 Commerce Customer worker 还要求显式冻结 `COMMERCE_PROVIDER_ID` 与 `COMMERCE_ADAPTER_VERSION`。两者会写入 Mapping、同步任务和证据，升级 Adapter 时不能用新版本解释旧任务；切换 Provider 后服务启动会为全部已验证手机号且尚无当前 Provider Mapping 的用户幂等补建任务。未配置对应 Adapter 连接时注册和模型能力保持可用，Mapping 留在待处理/重试状态，只有依赖该 Mapping 的支付被拒绝。发布前确认 `commerce_customer_mappings`、`commerce_customer_sync_jobs`、`commerce_customer_sync_evidence` 已迁移，并监控 `RETRY`、`REPAIR_REQUIRED` 及待执行任务积压。

支付 Provider 中立化上线先执行 Billing `000023_payment_provider_catalog`，再滚动 Billing、Token Gateway 和 Portal/桌面。迁移把通用商品的旧 SKU 清空并写入 `TEST_ONLY` 版本映射；正式环境若没有与 `BILLING_PAYMENT_PROVIDER` 及 Adapter 版本匹配的 `FROZEN` 映射，Billing 必须启动失败。发布后抽样核对 `/v1/payment-capabilities`、商品渠道交集和新订单的 Provider/映射/能力快照；没有回调和账单时还要核对正式收款准入字段，不能仅凭可查询能力放行。已有新版订单后禁止执行 down migration。

一单多流水与支付证据上线紧随其后执行 Billing `000024_payment_evidence_lifecycle`，并在滚动 Billing 前完成数据库备份。迁移解除订单到流水及权益包的一对一约束，回填 Provider/版本、付款人、关闭状态、摘要和既有流水来源，新增不可变 `payment_evidence` 及流水到权益包、通知和人工修复的外键。发布后必须抽样验证：同一 Provider 流水重复回调/查询只入账一次；不同成功流水不共享权益包；金额、币种或订单归属冲突只追加证据并进入复核；退款和关单不改写 `PAID/LATE_PAID` 流水；组织订单的付款用户与受益组织分别正确。运营后台订单明细应能看到全部流水、证据和四类摘要。监控证据归并冲突、人工复核、重复流水唯一约束及权益包来源约束。已有多流水订单或按流水创建的多个权益包后，旧模型无法无损恢复，禁止执行 down migration；故障时保留迁移并前向修复应用。

应收与上下文冻结随后执行 Billing `000025_receivables_and_context_freeze`。迁移新增钱包应收投影、独立应收、不可变清偿分配、拒付案件和应收/拒付账本科目；必须先完成迁移再滚动 Billing、Portal 和桌面运营制品。发布后抽样验证：实际费用超 Hold 时结算成功且只冻结目标钱包；现金充值只以 BASE 优先清偿最老应收，BONUS 不减少应收；同一确认拒付证据重复执行只生成一个案件，且只撤销目标支付流水权益包；其他个人或组织钱包仍可正常创建 Hold。运营后台应同时显示钱包财务状态、未清应收和分配明细。监控 `RECEIVABLE_OPEN` 钱包、长期部分清偿、拒付幂等冲突、账本不平和应收投影差异。存在任何新版应收、分配或拒付案件后禁止执行 down migration；故障时冻结相关写流量并前向修复，禁止直接改钱包或应收表清债。

Hold 运营证据继续执行 Billing `000026_hold_operations_evidence`。迁移会为既有 Hold 回填一条 CREATED 租约事件，并创建不可变租约/异常操作表；应先迁移 Billing，再滚动 Billing、Portal 和桌面运营制品。发布后抽样验证个人与组织 Hold 的创建、续租/追加预占都生成有效值历史，详情中的组成总额与 `usage_hold_allocations` 一致，组织 Hold 同时展示成员授予分配；未到期 ACTIVE 的安全释放必须被拒绝，REVIEW 或已过期 ACTIVE 释放要求原因和 Idempotency-Key，同键同请求返回同一结果、异请求冲突。监控 `ALLOCATION_MISMATCH`、`TERMINAL_ALLOCATION_REMAINS`、长期 `REVIEW_REQUIRED` 和幂等冲突。已有新版租约或操作证据后禁止 down migration；故障时禁用异常释放入口并保留只读查询，禁止直接修改 Hold 或分配表。

目标退款领域迁移继续执行 Billing `000027_targeted_refund_lifecycle`，但当前产品阶段不开放新建普通退款。迁移前必须备份数据库，并确认每条历史退款都能唯一映射到一笔成功支付流水和一个来源权益包；存在零个或多个候选时迁移应失败并先由运营完成受审计的数据归并，禁止任意挑选。同一目标可以保留多条历史已成功部分退款记录，但这些记录会永久阻止再发起普通退款，必须通过人工复核处理，不能把历史部分退款余额自动升级成新版整笔退款；同一目标存在多个未终结历史退款时迁移失败。迁移会把历史订单级 `PARTIALLY_REFUNDED/REFUNDED` 摘要归一为 `PAID/PAID_LATE + CREDITED`，新增退款目标、组成/授予预约、渠道强制退款案件和 `REVOKED` 权益终态；支付成功流水本身不被退款覆盖。应先迁移 Billing，再滚动 Billing、Portal 和桌面运营制品。发布后抽样验证：用户端和运营端均没有新建普通退款入口，直接调用创建端点也由独立产品门禁拒绝，Provider `refund_supported=true` 不能自动解除屏蔽；既有请求仍按原规则查询和收敛，Provider 主动强制退款仍建立证据和复核案件且不改写支付成功事实。保留的退款实现还须持续验证请求不接受金额、精确目标、消费/Hold/异常前置拒绝、明确失败解冻、不确定结果保持冻结、稳定幂等键和账本守恒，以防未来重新开放时发生语义漂移。持续监控既有 `REQUESTED/MANUAL_REVIEW`、`FORCED_REFUND_REVIEW` 冻结、退款 Outbox 死信、目标唯一约束和账本守恒。写入新版预约、撤销终态或强制退款案件后禁止 down migration；故障时保持新退款动作关闭并保留查询/复核，禁止回退到订单级按金额部分退款或直接改表解冻。

支付证据收敛上线执行 Billing `000033_payment_convergence_states`：把历史 `CLOSED/FAILED` 关闭投影迁移为 `PROVIDER_CLOSED/EXPIRED`，增加三个关单时间点，并允许无法匹配本地订单的 Provider 账单先以空订单引用保存不可变证据。发布顺序为先迁移 Billing、再滚动 Billing/Portal；升级后验证回调、查询和账单中的同一流水只履约一次，账单冲突仍保留摘要并产生对账案件，关单请求依次经过 `CLOSE_REQUESTED` 与 `LOCALLY_CLOSED_EXTERNAL_PENDING`，本地关闭后公开响应不再返回支付链接/二维码，晚到成功仍成为 `LATE_PAID`。若已存在空订单引用账单证据，down migration 会失败关闭，必须以前向迁移修复，禁止删除证据或把关单状态直接回写旧值。

本地付款窗口独立收敛继续执行 Billing `000049_local_payment_expiry`，再滚动 Billing、Portal 和桌面制品。迁移新增本地订单 `EXPIRED` 终态，并把已越过最后付款期限且已经处于 `CLOSE_REQUESTED/LOCALLY_CLOSED_EXTERNAL_PENDING` 的历史 `CREATED/PENDING` 订单前向归一为 `EXPIRED`，清除残留支付链接与二维码；Provider 关闭观察保持原值，不删除订单、支付证据或对账任务。发布后必须验证：到期任务先完成末次 Provider 查询，未付款时本地订单立即显示“已超时”并退出待付款集合；H3C 继续返回 `WAIT_PAY` 或不支持关单时不会把本地状态恢复为待付款；后续成功回调/查询仍按 `PAID_LATE` 幂等履约；本地到期后的主动查询间隔不少于5分钟，72小时仍未收敛时进入唯一复核案件。若新版运行期已写入 `EXPIRED`，旧版本不能识别该状态，禁止直接 down migration；故障时保留迁移并前向修复。

晚到支付复核上线继续执行 Billing `000028_late_payment_review_lifecycle`，再滚动 Billing、Token Gateway、Portal 和桌面运营制品。迁移为既有订单补齐支付期限、72小时边界和最终归档期限，并新增人工案件/不可变时间线、晚到证据、账单文件导入及 Provider 版本新单开关。发布前必须核对 Provider 能力的 `reconciliation_mode`、历史查询保留、支付/晚到窗口和权威未支付终态；H3C 当前声明支付回调但仍为 `QUERY_DEGRADED` 且没有账单，因此现有 `TEST_ONLY` 映射不能直接升级为正式映射。发布后抽样验证：72小时最终查询只创建一个 P2 案件；前7天每日、之后每周的复核计划不因认领或安排重置创建时钟；用户申诉只为本人订单追加不可变证据；严格归档要求期限后成功权威查询且无支付/冲突/未复核申诉；无法取得该事实时升级 `PROVIDER_BLIND` 并停用该 Provider 版本新单。恢复收款不得直接改 `payment_provider_controls`，应在修复和验证能力后发布新的 Provider/Adapter 版本与 `FROZEN` 映射。持续监控 `REVIEW_REQUIRED_LATE_PAYMENT`、`PROVIDER_BLIND`、人工 SLA 升级 Outbox、逾期未认领/未响应/未止损/未解决案件以及账单导入失败；文件导入仅对声明 `FILE_IMPORT` 的能力开放，单批最多 100,000 行。已有案件、证据或停单事实后禁止 down migration；故障时保留只读复核和证据，不得把订单直接改成未支付终态。

支付异常与四方对账随后依次执行 Account `000023_payment_anomaly_commands` 和 Billing `000029_payment_anomaly_reconciliation`，再滚动 Billing、Portal 与桌面运营制品。Billing 迁移把既有未结强制退款和历史对账差异补入通用人工案件，新增不可变异常/差异证据、命令审计、Provider/Adapter 版本化独立对账运行，并解除旧的“渠道+日期只能有一次运行”约束；定时任务仍以 Provider、版本、渠道、业务日唯一，只允许手工重跑形成新运行。发布前确认数据库支持 `gen_random_uuid()`，并核对所有开启账单能力的 Provider 将 PAYMENT/REFUND/CHARGEBACK 映射为正金额、稳定外部引用和上海自然日时间；不支持账单的 Provider 不启动日终任务。发布后抽样验证：拒付和强制退款只冻结并冲销原支付流水权益包，异常录入端点不直接记账；明确退款失败、重复/晚到冲突和非法状态进入统一队列；同一结论命令重试不重复记账；定时任务在上海零点边界按前一自然日取数；Provider、订单/流水、退款/拒付和账本任一缺失或金额不一致均生成带严重度的差异；同日手工重跑保留前次证据，认领、修复计划和结论只追加命令/事件。持续监控开放 P0/P1 案件、`financial_action_status=PENDING/FAILED`、未分配对账差异、定时运行缺口和账本缺失差异。产生任何新版异常、命令、独立运行或差异后禁止 down migration；故障时停止新结论与对账调度、保留只读证据并前向修复，禁止直接更新钱包、权益、异常或对账表。

### 6.4 新华三商城

- `COMMERCE_PROVIDER_ID=h3c_mall`
- `COMMERCE_ADAPTER_VERSION=h3c-mall-v1`
- `H3C_MALL_BASE_URL`
- `H3C_MALL_APP_KEY`
- `H3C_MALL_APP_SECRET`
- `BILLING_PAYMENT_PROVIDER=h3c_mall`
- `BILLING_ALLOW_TENTATIVE_PRICES`
- `BILLING_TEST_ADMISSION_EVIDENCE_TTL`：仅控制 development/test 环境中 `TEST_ONLY` 现场验证证据的有效期，默认 `720h`，允许 `1h` 至 `2160h`；正式 `FROZEN` 证据仍由外部受信验证结果决定，不使用该参数。
- `BILLING_PAYMENT_EXPIRY_INTERVAL_SECONDS`（不得小于 Adapter 声明的最小查询间隔）
- `PAYMENT_MERCHANT_ID`（商城回调订单归属校验）
- `PAYMENT_CALLBACK_CLIENT_ID`（当前凭据绑定 H3C Provider Profile）
- `PAYMENT_CALLBACK_CLIENT_SECRET`（至少32字符，独立 Secret）
- `PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64`（至少256 bit 随机密钥的标准 Base64）
- `PAYMENT_CALLBACK_TOKEN_KEY_ID`
- `PAYMENT_CALLBACK_TOKEN_TTL_SECONDS`（60至900秒，默认300秒）

UAT 联调可显式允许 `TEST_ONLY` 暂定目录，并通过 `BILLING_TEST_ADMISSION_EVIDENCE_TTL` 设定受控测试证据有效期；默认 30 天，避免固定 24 小时导致联调商品每日被准入到期任务暂停。该设置只影响新签发证据，不延长或改写既有证据；证据已失效时仍须重新验证并完成商品恢复审批，禁止自动续证或自动开放新单。正式环境必须由业务确认 SKU、金额、BASE/BONUS 权益、有效期和 Provider 映射，发布新的 `FROZEN` 映射后关闭该开关。已有映射不可原地修改，只能退休并发布更高版本；商城特殊状态映射由 H3C Profile 处理，不能通过通用环境变量绕过服务端商品目录。通用 Provider 回调与 Web 个人中心统一使用独立账号子域名，公网边界只允许把 `POST https://account.pinvou.com/api/v1/payment-callback/token` 和 `POST https://account.pinvou.com/api/v1/payment-callbacks` 两个精确路径转发到 Billing，其余同源流量进入 Portal；`pinvou.com` 不再承载相关接口。客户端凭据由服务端绑定 Provider、版本和商户，回调请求体不得携带 Provider 身份；Token 仅含 `payment.callback.write` 权限，回调收到 `202` 只表示通知已持久化。后端主机变化时只更新 Nginx/网关 upstream，Provider 配置不得改为实例 IP 或临时地址。

新华三应用授权 Token 按供应商建议缓存 1 小时；收到 `401/403` 才强制刷新一次，禁止每单登录或超过每 AppKey 每小时 3 次的授权限流。创建订单必须同时发送 `orderKey`、大陆手机号、`skuBom` 和 `merchantCustomerKey`，最后一项固定为付款人的永久 `user_id`；商城异步受理后由客户端/后台按 `orderKey` 查询，Adapter 声明的最小真实查询间隔为 2 秒。`pageQueryOrder` 以同一 `merchantCustomerKey` 为必填条件，手机号只是可选过滤条件。

新华三回调请求体必须直接使用其 `queryOrder.content` 原生订单对象，不再发送品悟旧版规范字段。至少核验 `orderKey`、`merchantCustomerKey`、`mobile`、`skuBom`、`needPayAmt`、`skuAmt`、`status`、`updatedAt`；支付成功还要核验 `paidAmt`、`payTime` 和 `payUniqueKey/paymentOrderId`。金额按十进制元精确换算为分并与本地订单快照比较；客户、SKU、金额或归属不一致只留证并进入复核。发布后必须使用一笔隔离测试订单验证 `Token 200 → 回调 202 → Inbox PROCESSED → 支付流水/权益各一份`，并确认重复原生快照不重复履约；不能只以 `202` 作为成功证据。

历史商品迁移仅用于把部署前已有事实导入运行时目录，不再代表当前可售清单。部署后由具备商品权限的运营人员在后台创建草稿、完成准入验证并发布；验收应从运行时目录读取实际商品和映射，核对订单冻结快照、商城查询/回调金额及单次履约，不得在部署文档或发布脚本中固化 SKU/BOM、金额、权益或商品数量。正式准入证据不足时保持测试准入状态，不得直接升级为正式状态。

### 6.5 Token Gateway 与 New API

- `TOKEN_SECRET_PROVIDER`
- `TOKEN_SECRET_KEY_ID`
- `TOKEN_LOCAL_KEK_BASE64`（仅开发/测试）
- `TOKEN_SECRET_ROTATION_INTERVAL_SECONDS`（旧密文批量重加密周期，60 至 86400 秒）
- `TOKEN_SECRET_ROTATION_BATCH_SIZE`（每批 1 至 1000 条）
- `TOKEN_PROVIDER_ALLOWED_HOSTS`
- `TOKEN_PROVIDER_CA_FILE`（可选，仅加载到模型供应商 HTTP 客户端；内部 HTTPS Provider 使用私有 CA 时必须配置为容器内只读 CA 文件，禁止用全进程 `SSL_CERT_FILE` 扩大无关客户端的信任边界）
- `TOKEN_DEFAULT_PROVIDER`
- `TOKEN_GATEWAY_ADAPTER`（`NEW_API` 或 `OPENAI_COMPATIBLE`；决定请求构造、上游引用提取与用量证据 Adapter，不改变通用数据库/API 字段）
- `TOKEN_MODEL_CATALOG_DEFAULT_CONTEXT_TOKENS` / `TOKEN_MODEL_CATALOG_DEFAULT_MAX_OUTPUT_TOKENS`（Adapter 目录发现只返回模型标识时采用的保守能力上限；后续只能通过新能力版本调整）
- `TOKEN_MODEL_CATALOG_SYNC_MINUTES`（模型目录自动发现周期；一次缺失只记录证据，连续两次缺失才自动弃用并停止新请求）
- `TOKEN_DEFAULT_PROVIDER_BASE_URL`
- `TOKEN_DEFAULT_PROVIDER_API_KEY`
- `TOKEN_DEFAULT_PROVIDER_MODELS`
- `TOKEN_DEFAULT_ROUTE_PRIORITY`
- `TOKEN_GATEWAY_MAX_INPUT_TOKENS`
- `TOKEN_RECONCILIATION_NEWAPI_BASE_URL`
- `TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN`
- `TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID`
- `TOKEN_RECONCILIATION_INTERVAL_SECONDS`

`production`/`production-new` 环境必须拒绝本地 KEK，使用 KMS/Vault 类 Provider。Provider URL 必须是 HTTPS 且主机精确命中白名单。Helm 通过 External Secrets Operator 从已审核的 `SecretStore`/`ClusterSecretStore` 同步运行时 Secret，正式环境禁止关闭该资源或把 Secret 明文写入 values、渲染产物、日志和验收证据。

密钥轮换采用“旧 key 可解密、新 key 负责加密”的双读窗口：先在 KMS/Vault 中将旧 key 保持 `DECRYPT_ONLY`、新 key 置为 `ENCRYPT_DECRYPT`，再更新 `TOKEN_SECRET_KEY_ID` 并滚动发布。Token Gateway 会按批重加密 Provider 凭据、派发准备/意图、流事件和重放结果；任一旧 key 密文尚未收敛时生产 readiness 返回失败，因此新 Pod 不接流量。全部 Pod 就绪且旧密文计数为零后，才可将旧 key 置为 `DISABLED`。失败时保留旧 Pod 与旧 key 解密能力，不得强制提升或直接改表。

### 6.6 桌面端

桌面端公开连接配置是随安装包交付的 `config/account-platform.json`，字段包括：

- `oidc_issuer` 或显式 `oidc_token_url`；
- `oidc_client_id`；
- `account_service_url`；
- `identity_service_url`；
- `token_gateway_url`；
- `account_portal_url`。

官方正式资源的 OIDC、Account、Identity、Token Gateway 与 Portal 均使用 `https://account.pinvou.com` 的对应路径，OIDC 客户端为 `pinvou-desktop`。release 运行时只读取安装资源，不接受操作系统环境变量覆盖；客户端升级时由安装包或 OTA 一并更新该文件。私有发行版和 UAT 必须在构建时打包各自的同路径配置，不得在已发布客户端上依赖全局环境变量切换环境。登录只依赖身份与 Account 服务；Token Gateway 缺失时仅模型调用等 Token 专属操作失败，不得阻断登录。

开发态 `npm run dev` 不需要每次生成安装包；debug 构建在存在账号平台环境变量时仍优先读取它们，项目标准入口由 `.env.development.local` 注入。远程端点必须 HTTPS，只有 `127.0.0.1`/`localhost` 允许 HTTP。

Web 个人中心的用户名/手机号修改依赖 Portal OIDC Authorization Code + PKCE 重新认证；Identity Client 必须接受 `prompt=login` 与 `max_age=0`，回调地址固定为 `https://account.pinvou.com/auth/callback`。桌面端只保留未登录忘记密码，不承载资料修改。

Web 首次登录同样使用该 Authorization Code + PKCE 流程，但登录、短信登录与手机注册表单由 Keycloak 的 `pinvou` 主题和 `pinvou-browser-authenticator` 承载。Keycloak 镜像必须由 `deploy/keycloak/Dockerfile` 构建并包含 Authenticator JAR 与主题，不能把 Helm 的 `images.keycloak` 指向未包含扩展的官方裸镜像。运行时必须配置 `PINVOU_ACCOUNT_AUTH_URL`、与 Account `OIDC_ISSUER` 完全一致的 `PINVOU_OIDC_ISSUER`、`PINVOU_IDENTITY_AUTH_URL`、`PINVOU_SMS_VERIFY_URL` 及既有工作负载身份；Account 的工作负载规则只向 Keycloak 开放 `POST /internal/v1/auth/sms/verify` 和 `POST /internal/v1/authorization/check`，缺失或超出路径时认证失败关闭。发布后需分别验证密码登录、手机号密码登录、短信滑块与验证码登录、注册滑块与验证码注册、三语和手机号/来源/设备限流；再撤销 `PERSONAL_USER` 的测试副本、停用测试主体并阻断 Account，逐项确认密码及短信路径都不会签发新客户端/Portal 会话。既有 Cookie SSO 不替代凭据路径的 ID-19 检查，其撤权收敛仍按会话撤销和授权版本失效策略执行。

工作负载令牌 Agent 与业务服务连接同一个内部依赖网络时，Compose 服务名和显式网络别名必须与全部业务服务发现名完全不相交。Agent 使用 `<业务服务名>-workload-agent`，但其 OIDC `client_id`、subject 和投射目录仍使用原业务服务名；不得为了目录或身份一致而复用业务 DNS 名。发布前后必须分别检查业务名只解析到对应业务容器、Agent 名只解析到对应 Agent 容器，并连续验证认证依赖健康，避免 Docker DNS 轮询把认证请求发送到不监听业务端口的 Agent。

### 6.7 配置完整性与环境分级

`account-platform/deploy/compose/.env.example` 只用于本地开发起步，不是生产配置的完整真相源。每次发布必须以目标版本实际参与渲染的全部 Compose 文件为准执行 `docker compose --env-file <secret-env> <compose-files> config`，归档脱敏后的配置键名、Compose 文件摘要和渲染结果摘要；存在未解析变量、空的必填值、开发默认值或站点 override 未纳入摘要时均为 NO-GO。

除前述领域配置外，发布配置清单还必须显式覆盖：

| 类别 | 配置名与约束 |
|---|---|
| 环境与契约 | `APP_ENV`、`PINVOU_API_CONTRACT_VERSION`、`ACCOUNT_SERVICE_VERSION`、`ACCOUNT_CLIENT_CONTRACT_VERSION`、`TOKEN_SERVICE_VERSION`、`TOKEN_CLIENT_CONTRACT_VERSION`；正式环境不得使用 `development`、`dev` 或未知 Commit |
| Keycloak 管理 | `KEYCLOAK_ADMIN_BASE_URL`、`KEYCLOAK_ADMIN_USERNAME`、`KEYCLOAK_ADMIN_PASSWORD`；管理地址不得使用公网 Admin 路径 |
| 认证风控 | `AUTH_RISK_REDIS_URL`、`AUTH_RISK_HMAC_KEY_BASE64`；密钥须独立生成并与注册、短信、支付密钥隔离 |
| 客户端 Readiness | `CLIENT_READINESS_ATTESTATION_KEY_ID`、`CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64`、`CLIENT_READINESS_ATTESTATION_TTL_SECONDS`、`TOKEN_MINIMUM_CLIENT_VERSION`、`TOKEN_MAXIMUM_CLIENT_VERSION`、`PINVOU_CLIENT_RELEASE_MANIFEST`、`PINVOU_CLIENT_UPGRADE_URL` |
| 模型目录 | `TOKEN_GATEWAY_ADAPTER`、`TOKEN_MODEL_CATALOG_DEFAULT_CONTEXT_TOKENS`、`TOKEN_MODEL_CATALOG_DEFAULT_MAX_OUTPUT_TOKENS`、`TOKEN_MODEL_CATALOG_SYNC_MINUTES`、`MODEL_GATEWAY_STATUS_URL` |
| 支付产品门禁 | `BILLING_ALLOW_TENTATIVE_PRICES`、`BILLING_NORMAL_REFUND_CREATION_ENABLED`；正式环境前者必须为 `false`，当前退款入口关闭阶段后者必须为 `false` |
| New API | `NEWAPI_CRYPTO_SECRET`、`NEWAPI_SESSION_SECRET` 及真实数据面的最小权限管理凭据；`MOCK_NEWAPI_KEY` 仅允许隔离开发/测试 |
| Payment Simulator | `PAYMENT_SIM_HMAC_ALIPAY`、`PAYMENT_SIM_HMAC_UNIONPAY`、`PAYMENT_SIM_HMAC_WECHAT`、`PAYMENT_SIM_PAY_TOKEN_SECRET`；只允许隔离开发/测试，正式发布清单不得出现 |

仓库 Compose 基线将 `APP_ENV` 和部分测试准入开关固定为开发/UAT语义；仅在 `.env` 中写同名变量不会覆盖 YAML 中的固定值。需要生产提升时必须通过受审计的站点 override 显式改写这些服务环境，并以最终 `docker compose config` 结果验收，不能以宿主机环境变量或未参与渲染的 `.env` 行作为证据。

`H3C_MALL_PRODUCTS_JSON` 不再是运行时商品真相源，不得用于上架、下架或变更商品；商品事实只来自 Billing 运行时目录和受审计命令。`.env.example`、Compose 和本文三者的配置键必须在发布前对账，任一新增 Compose 变量未被示例或本文分类说明时阻止提升，废弃变量应从示例和站点配置中删除而不是静默保留。

站点配置对账必须比较“精确键名”，不能只凭人工目视或相似前缀判断已配置。例如构建代理键必须是 `GOPROXY`，带额外前缀、后缀、转义字符或不可见字符的变体均视为缺失；同理，废弃键、重复键和未被最终 Compose 消费的键必须报告并清理。发布前至少归档以下脱敏检查结果：示例键集合、站点键集合、最终 Compose 引用键集合、缺失/多余/重复键和 `docker compose config` 退出码，绝不归档值。

发布目录中的 `.env` 和站点 override 只是运行副本，不得成为唯一恢复来源。它们必须在独立 Vault/KMS/Secret 系统或加密离机恢复包中具有可取得的版本引用，且解密密钥与密文不在同一故障域。没有独立 Secret 版本、站点 override 摘要或恢复验证证据时，只允许重建空环境，不得宣称能够恢复当前平台。

### 6.8 死信检查与受审计重放

先按目标领域选择只读数据库凭据，执行 `event-replay --event-id <uuid> --inspect` 并留存返回的事件类型、聚合版本和摘要。根因修复后，使用受控 `event-replay` 工作负载及新的稳定幂等键执行 `--event-id <uuid> --expected-digest <sha256> --idempotency-key <key> --reason <不少于20字符> --confirm`；命令会以 `actor_type=SERVICE`、`EVENT_REPLAY`、`job_run_id` 和原因写入不可变审计，再原子重新排队。

不得修改 Payload、跳过摘要、复用其他服务数据库身份或直接更新 `outbox_events`；同一命令重试返回原审计结果，同键异请求拒绝。上线须先完成 Account `000030_event_replay_audit`、Billing `000032_event_replay_audit`、Token `000018_event_replay_audit`。

## 7. 本地开发与专项升级约束

### 7.1 本地开发

1. 将 `account-platform/deploy/compose/.env.example` 复制为被 Git 忽略的 `.env`。
2. 为每个占位 Secret 生成不同随机值。
3. 从仓库根运行：

```text
node scripts/account-platform.mjs dev up
```

4. 首次需要真实 New API 时，按 `deploy/runbooks/newapi-deployment.md` 初始化最小权限 Token 和 Channel。
5. 启动桌面应用使用项目既有开发命令；不要为 UI/接口联调生成安装包。
6. 停止环境：

```text
node scripts/account-platform.mjs dev down
```

`dev down` 不应被用于生产。集成测试使用独立 Compose project，并只清理自己的卷。

### 7.2 专项升级约束

账号注销资格与静默期上线执行 Account `000027_account_closure_saga`。滚动顺序必须先发布提供只读资格评估端点的 Billing 和 Token，再迁移并发布 Account，最后发布 Portal 与桌面制品；Account 继续通过现有 `BILLING_SERVICE_URL`、`TOKEN_SERVICE_URL` 和内部服务身份访问评估端点。上线后验证：最后组织所有者或最后有效超级管理员只会停留在静默期前且仍可完成交接；进入 `CLOSING` 后新支付、新权益与新模型调用统一返回 `ACCOUNT_CLOSING`，注销进度、历史查询及必要修复入口仍可用；任一领域不可用或存在阻断时请求保持持久化并按租约重试，运营只能提交带原因的幂等重试命令，不能跳过阻断。监控请求阶段、租约超时、阻断码、重试次数和 30 分钟 SLA。禁止恢复直接删除用户接口或通过 SQL 推进阶段。库中出现 `CLOSING` 用户、注销请求或不可变审计后禁止执行 down migration；故障时暂停 worker、保留查询与修复能力，待依赖恢复后从原阶段续跑。

注销后半段清理与组织授予隔离上线执行 Account `000028_account_closure_cleanup` 和 Billing `000030_account_closed_grants`。先完成两库迁移，再发布具备 Account 注销状态校验和内部隔离/回收端点的 Billing，随后发布 Account，最后发布 Token、Portal 与桌面制品；这样 Account Worker 首次进入 `IDENTITY_CLEANUP` 前，Billing 已能幂等接收隔离命令。Account 必须配置可用的 `IDENTITY_SERVICE_URL` 和服务凭据，即使没有开启手机号登录，也不能把 Identity 删除失败当作成功。上线后至少验证：注销期间角色、成员、Session、邀请和非资金 Commerce 任务自动收敛；资料 Saga 在 Identity 更新前取消、更新后向前归并；旧生命周期迟到结果不能重绑 Mapping；全部远端 Identity 删除和全部组织授予隔离均有完成证明后才进入 `ANONYMIZED`；永久 `user_id/user_code` 不变，原用户名/手机号可被全新用户使用；匿名化事件发布后才显示 `COMPLETED`。

授予回收验收必须使用具有 `organization.grant.reclaim_closed` 的组织管理员，通过 Token Gateway 的组织接口发起；请求不接受金额，只能一次性回收单条授予的全部当前可回收剩余量。验证普通撤回、恢复、消费、创建 Hold 仍拒绝 `ACCOUNT_CLOSED`，并分别注入关联 Hold、待核对用量、退款/拒付、支付异常和资金性对账差异，确认回收失败且余额守恒；批量请求中的失败项不得回滚成功项。监控 `IDENTITY_CLEANUP/ANONYMIZED` 停留时长、外部证明、主体生命周期不匹配的 Commerce `STALE` 证据、授予隔离/回收审计、Outbox 延迟和三年保留/法律保全任务。任一库产生清理、匿名化、隔离或回收事实后禁止对这两项迁移执行 down；故障时暂停注销 Worker 或关闭回收入口，保留已提交证明并从原阶段重试。

客户端切换 Readiness 上线执行 Token `000016_client_readiness`。为 Token Gateway 配置当前服务/契约版本、允许的最小和最大桌面版本，并从运行时 Secret 注入独立 Ed25519 私钥及稳定 `key_id`；私钥不得进入 values、镜像、日志或桌面安装包，生产环境缺失私钥时 Gateway 必须拒绝启动。先发布 Account 的服务身份 `/internal/v1/client-compatibility`，再迁移并发布 Token Gateway，最后由发布系统调用 `/internal/v1/client-readiness` 和 `/internal/v1/client-readiness/attestations`。反向代理只公开 `/v1/client-compatibility`，禁止公开两个内部入口。

验收连续执行只读 Readiness，并在调用前后核对用户/组织 Hold、权益桶、用量请求、账本和上游推理日志均无新增；分别注入 OIDC 不可达、Account 契约不可达、空目录、无 READY 路由和价格/公式不完整，结果必须返回稳定失败检查且不得签发证明。对有效证明逐项篡改环境、制品摘要、客户端版本、服务/契约/目录版本、签名和有效期，更新器均须拒绝；轮换密钥时先公开新公钥并保持旧 key 的验证窗口不短于所有已签证明寿命，再停止旧 key 签发。监控各检查失败率、证明签发量、过期拒绝和 `client_readiness_attestations` 连续性；一旦签发记录存在，禁止 down migration。生产等价证据完成前 APT-052 与 APT-053 保持 `BLOCKED_EXTERNAL`，不得提升正式客户端更新渠道。

## 8. 单机 Compose 发布流程

下面流程以 `<release-dir>`、`<backup-dir>` 和 `<compose-files>` 为占位符。站点专用配置和 Secret 不进入仓库。

发布不是“在新目录执行一次 `docker compose up`”。每次发布必须形成一个不可变部署单元：源码/制品、Compose 与站点 override 摘要、迁移计划、备份证据、服务镜像 digest、验证结果和上一回滚目标必须共同绑定同一个 `<deployment-id>`。缺少其中任一项时不得替换运行容器或切换稳定链接。

### 8.1 发布前检查

1. 确认目标 Git Commit、当前运行 Commit、数据库迁移版本、当前符号链接和上一可回滚部署清单。当前服务若仍报告 `commit=unknown`、`version=dev` 或镜像只有可变 `latest` 标签，先补齐制品标识，不得把目录名当作运行 Commit 证明。
2. 检查工作树/发布包来源，确保构建内容可追溯且不包含本地私人文件；使用 `git archive <commit>` 或等价的受审计制品生成方式，禁止从脏工作树直接复制。
3. 运行与变更范围相符的测试：

```text
node scripts/account-platform.mjs verify
node scripts/account-platform.mjs test integration
```

支付、故障和安全变更按需要增加：

```text
node scripts/account-platform.mjs test e2e
node scripts/account-platform.mjs test fault
node scripts/account-platform.mjs test security
```

4. 运行 `node scripts/account-platform-migration-manifest.mjs check`，比较目标 Commit 与当前部署之间新增的 Account/Billing/Token 迁移，生成逐步迁移计划；不能只按目录名称或最高版本号猜测跨库顺序。
5. 使用目标 `.env` 和全部 Compose 文件执行 `docker compose ... config`，确认无未解析变量、空必填项、测试工作负载、开发开关、意外公开端口或未摘要的站点 override。
6. 检查磁盘空间、Docker 可用空间、数据库健康、待处理支付订单、有效 Hold、Outbox 积压和死信。至少保留新制品、新镜像、当前版本、上一回滚版本和本次备份所需空间。
7. 验证 Secret 文件为受限所有者且权限不宽于 `0600`，确认不会被复制到发布清单、公开日志或提交。

### 8.2 创建版本目录

1. 创建 `/opt/pinvou-account-platform-<commit>-<timestamp>`。
2. 将已审核源码或发布包复制到版本目录。
3. 从当前部署安全复制 `.env` 和站点 override，仅保留受限权限。
4. 创建 `deployment-manifest.json`，写入完整 Git Commit、构建时间、环境等级、输入文件摘要、当前/上一部署标识和待填充的镜像/迁移/验证字段。
5. 不修改当前稳定符号链接，不停止现有服务。

### 8.3 完整恢复集与数据库备份

包含新增迁移、数据回填或人工修复的发布，迁移前至少备份：

- PostgreSQL globals/roles。
- Account 数据库。
- Billing 数据库。
- Token 数据库。
- Keycloak 数据库。
- 独立 New API 数据库（若本次变更涉及）。

备份写入 `<backup-dir>/<deployment-id>`，生成校验和并执行最小可读性检查，把备份 ID、时间点、校验结果和恢复演练引用写入发布清单。Billing 备份是资金恢复依据，未验证备份时不得迁移。

上述数据库文件只是完整恢复集的一部分。能够从新主机恢复当前系统的恢复集还必须绑定同一目标恢复时刻下的 NATS JetStream、独立模型网关数据库/数据目录/私有配置、站点 Secret 版本、站点 override、边缘代理/证书引用/网络配置以及目标 Commit 或镜像 digest。Redis 仅承载短期会话、验证码/风险和缓存，不作为长期事实恢复；恢复后所有会话和未完成挑战失效。恢复集结构、校验和 NO-GO 条件见 [`../deploy/runbooks/full-redeployment.md`](../deploy/runbooks/full-redeployment.md)，备份和 PITR 细则见 [`../deploy/runbooks/backup-restore.md`](../deploy/runbooks/backup-restore.md)。

纯应用发布只有在迁移清单差异为空、没有数据修复且 Schema 向前/向后兼容时，才可引用仍在 RPO 范围内并已验证可恢复的最近备份；该判断、清单差异摘要和备份 ID 必须写入发布清单。不能证明为纯应用发布时按迁移发布处理。即使此前部署后人工删除过临时迁移备份，也不能把“无需备份”固化为流程；可按保留策略在稳定观察期和恢复演练完成后清理旧备份。

### 8.4 构建镜像

在新版本目录使用仓库 Compose 与站点 override 构建业务和迁移镜像：

```text
docker compose --env-file <secret-env> <compose-files> build \
  keycloak token-portal

docker compose --env-file <secret-env> <compose-files> build \
  --build-arg VERSION=<release-version> \
  --build-arg COMMIT=<full-commit> \
  account-service identity-service billing-service token-gateway \
  account-event-relay billing-event-relay token-event-relay \
  account-migrate billing-migrate token-migrate
```

构建完成后必须为每个应用和迁移镜像赋予包含完整或唯一 Commit 的不可变 tag，优先推送到支持 digest 的受控 Registry；站点 override 的 `image:` 必须引用该 tag 或 digest。应用和基础镜像的 digest/目标平台必须进入发布清单或离线恢复包；不得让发布、回滚或灾难恢复依赖 Compose 自动生成的无版本镜像名、`latest` 或未来可能变化的基础镜像 tag。使用 `docker image inspect` 取得镜像 ID/digest，写入 `deployment-manifest.json`，并确认镜像 OCI label 或服务健康响应至少有一处能返回同一完整 Commit；二者均缺失时为 NO-GO。

只构建和替换受影响服务可以降低风险，但必须由目标 Commit 的实际差异和依赖图确定，并在发布清单记录服务集合。共享库、迁移、协议、环境变量或基础镜像发生变化时，不得误判为单服务发布。

Compose 会把站点 `.env` 中的 `GOPROXY` 作为 BuildKit 构建参数传给全部 Go 服务；未配置时使用仓库的官方默认值 `https://proxy.golang.org,direct`。无法稳定访问官方代理的站点必须在受控 `.env` 中持久配置可达代理，例如中国区站点使用 `GOPROXY=https://goproxy.cn,direct`，然后仍执行上述统一构建命令。不得只设置未被 Compose 读取的宿主机环境变量，也不得每次临时手写 `--build-arg`；发布前先从构建容器验证代理可达。代理凭据不得写入仓库、构建命令、日志或镜像层。

### 8.5 执行迁移

不存在适用于所有版本的固定跨库顺序。每个数据库内部严格按版本号递增；跨库顺序以目标版本专项升级约束和发布清单中的迁移依赖图为准。例如组织生命周期要求 Billing/Token 先于 Account，而部分 Account 授权或命令策略必须先于消费它们的应用。若同一发布同时命中多组约束，先合并为无环依赖图；存在环、缺少所有者或无法证明兼容时停止发布并拆分为多个兼容阶段。

每一步应使用一次性迁移服务显式执行并记录起止版本、退出码和日志摘要，例如 `docker compose ... run --rm <domain>-migrate`。当前步骤退出码为 0、迁移表版本/校验和正确且专项后置检查通过后，才能执行下一步。已应用迁移文件不得修改，只能新增更高版本；迁移器可兼容仅换行符不同的旧校验和，但不接受语义内容变化。

当前基线还必须覆盖以下近期升级组；组内各数据库仍按自身版本号递增，全部所需 Schema/策略就绪后才滚动对应应用：

1. Account `000037_recharge_product_permissions` 与 Billing `000043_recharge_product_catalog`、`000044_recharge_product_commands`：先完成目录、命令和权限迁移，再开放后台商品管理；旧的商品导入迁移只保留历史事实，不代表当前可售清单。
2. Billing `000038_provider_failure_confirmation`、`000039_payment_product_controls`、`000040_payment_provider_resume_audit`、`000047_payment_tiered_stop_loss` 与 Account `000038_payment_product_resume_policy`、Billing `000048_payment_product_resume_audit`：在启用分级止损和商品/Provider 恢复前完成策略、控制、审批与不可变审计迁移；恢复入口在两库均就绪前失败关闭。
3. Billing `000041_human_delegation_audit`：在商品和 Provider 人工控制命令上线前确认人类委托审计可用，禁止回退到调用方自报操作者。
4. Billing `000045_system_ledger_accounts_repair`：在 Billing 承接流量前核验六个系统账户和回调重试字段。
5. Billing `000046_clear_terminal_payment_instructions`：迁移后核验所有非可支付或已关闭订单均不再保存/返回 `payment_url`、`qr_data`，再发布会主动关闭到期订单的 Billing。

Billing 目录相关迁移中的历史 SKU/BOM、金额或权益仅是一次性数据事实，不得复制到部署资料、站点环境变量或发布脚本，也不得作为当前上架状态判断。当前可售性只能从运行时目录、准入证据及商品/Provider 控制读取。

Account `000011_permanent_user_codes` 会为历史用户回填永久编码并建立不可变触发器。迁移后必须核验：所有用户均有格式与校验码有效的唯一编码、`user_code_registry` 与用户一一对应、更新 `users.id/user_code` 和物理删除用户均被拒绝。不得通过禁用触发器、清空登记表或回滚迁移来“释放”已发放编码。

Token `000015_usage_cancellation_convergence` 增加执行/取消/交付/结算四类独立状态、幂等取消与人工核对命令、不可变脱敏证据、客户端确认序号、版本化收敛时限和平台损失复核。滚动升级前必须确认旧 Gateway 不会在新迁移后写入未知状态；应先完成 Token 数据库迁移，再同时升级 Token Gateway、Portal 和桌面制品。发布后至少验证：原稳定用户在 Session/组织上下文变化后仍可取消自己的请求；派发意图前取消不调用 Provider 且唯一 Hold 全释放；意图形成后停止仅写 `USER_STOPPED`，当前 `UNSUPPORTED` Adapter 继续后台读取并按可信实际用量结算；断线不会释放 Hold 或创建第二次模型调用；人工核对重试要求幂等键且不重发模型；期限后无可信用量会释放 Hold、转 REVIEW 并永久禁止晚到证据再次扣用户。迁移会建立不可变命令/证据触发器，产生新版记录后禁止回滚到不能表达取消与损失边界的旧 Schema。

Token `000019_usage_convergence_evidence` 增加模型外部事实的只追加收敛时间线，并为完整重放结果保存明文载荷的 SHA-256（正文仍只加密存储）。先迁移 Token 数据库再滚动 Gateway；发布后注入派发后连接中断、相同结果重放、冲突完整结果、Provider 查询暂无结论/明确拒绝/返回用量及 Billing 提交不确定，核验每次外部观察与复核、结算或客户 Hold 释放均有对应脱敏证据，冲突结果不能覆盖首个完整结果，非幂等模型请求不会自动二次派发。监控 `UNCERTAINTY/REVIEW` 和 `RECONCILIATION_STOPPED` 证据增长及无终态案件；禁止通过删除证据、覆盖结果密文或直接改状态结束复核。

Account `000026_token_account_operations` 注册平台 Token Account 创建为只审计管理命令，`000042_super_admin_platform_permissions` 将全部常驻平台管理权限补齐到超级管理员。先执行 Account 迁移，再滚动 Token Gateway 与 Portal；否则新版创建路由会因未知命令策略失败关闭。发布后分别使用平台模型运营员、超级管理员、组织所有者和组织模型管理员验证：前两者可按原子权限列出、创建、启停和检查 PLATFORM 账号；组织身份统一返回 403 且相同的不存在/存在资源响应不泄露元数据。抽样确认后台永远不列出 `management_mode=USER` 账号，凭据检查响应和审计仅含统一状态、稳定错误码、指纹等元数据，不含 Secret、密文、Key ID、Provider 地址或上游错误原文。

不要默认执行 down migration。涉及破坏性 Schema 的发布必须先提供前向兼容阶段和单独数据迁移方案。

### 8.6 替换应用服务

迁移完成后只更新发布清单列出的受影响应用服务，不主动重建正在运行且无需变更的 PostgreSQL、Redis、NATS 和独立 New API。下面是全量示例；单服务发布必须显式替换为经审核的服务集合：

```text
docker compose --env-file <secret-env> <compose-files> up -d --no-deps --wait \
  account-service billing-service token-gateway token-portal \
  account-event-relay billing-event-relay token-event-relay
```

使用 `--no-deps` 的前提是基础设施已经健康、网络、服务名、共享配置和依赖契约没有变化。`--wait` 超时、容器反复重启、镜像 ID 与发布清单不一致或任一 readiness 失败时，停止继续切换并按 13.1 使用上一清单回滚；若发布涉及基础设施，必须使用单独审批和维护窗口。

### 8.7 切换稳定链接

应用、公共烟测、领域烟测和短观察窗通过后，先把实际镜像 digest、迁移结果和验证结果补齐到发布清单，再原子更新：

```text
/opt/pinvou-account-platform-current -> /opt/pinvou-account-platform-<commit>-<timestamp>
```

符号链接只表示当前发布源码/配置位置；切换后再次读取运行容器镜像 ID/digest、健康响应 Commit 和 Compose 工作目录，三者必须与新发布清单一致。保留上一版本目录、其站点配置摘要和全部回滚镜像，至少经过稳定观察窗及下一次可恢复备份验证后才能清理。

## 9. 首次或升级后初始化 Keycloak

### 9.1 平台管理员

在受控终端临时注入管理员用户名和密码，执行可重复运行的初始化脚本：

```text
docker compose --env-file <secret-env> <compose-files> exec \
  -e PINVOU_ADMIN_USERNAME -e PINVOU_ADMIN_PASSWORD keycloak \
  sh /opt/keycloak/data/import/provision-platform-admin.sh
```

脚本确保登录用户和迁移期 `platform-admin` Realm 角色存在，但 Realm Role 不再授予任何 Account 业务权限。密码不能写入仓库、共享聊天、部署日志或永久 Shell 配置。

完成数据库迁移并让目标用户至少 bootstrap 一次后，必须按 [`deploy/runbooks/account-rbac-migration.md`](../deploy/runbooks/account-rbac-migration.md) 使用经审批清单把 `issuer + subject` 明确映射到具体 Account 平台角色。不得把全部旧 `platform-admin` 自动升级为超级管理员；开放后台流量前至少核验一名 `PLATFORM_SUPER_ADMIN`，并验证职责分离的允许/拒绝矩阵。

### 9.2 手机认证 Flow

已有 Keycloak 数据卷启用手机登录后执行：

```text
docker compose --env-file <secret-env> <compose-files> exec keycloak \
  sh /opt/keycloak/data/import/provision-mobile-auth.sh
```

该脚本安装手机 Direct Grant Flow、服务账号权限和手机号 Claim，并调整注册用户资料要求。脚本可重复执行；执行后验证密码登录、短信登录和新用户 bootstrap。

## 10. 反向代理与公网路径

建议只公开：

- Portal 页面与其同源 `/api/*`。
- Keycloak Realm 认证和静态资源路径。
- Account 的注册、短信、bootstrap 和桌面白名单业务路径。
- Token Gateway 的聊天、余额、权益、充值和个人用量路径。

不得公开：

- Keycloak `/admin/`。
- Billing Service 原始端口和 `/internal/*`。
- Account/Gateway 的 `/internal/*`。
- PostgreSQL、Redis、NATS 和 Relay 端口。
- New API 管理面及 Provider Token。
- 任何非生产 Payment Simulator、测试支付页面和 Mock New API。
- 服务独立 Metrics 端口。

反向代理必须覆盖并传递可信的 `X-Forwarded-Proto`、Host、`X-Real-IP`/`X-Forwarded-For`，限制请求体大小，并为 SSE 禁用代理缓冲和配置合理的长连接超时。Portal 会把经过该边界确认的客户端来源传给内部 Account 查询限流；不得让客户端覆盖转发头、内部身份头或服务凭据头。

## 11. 发布后验证

### 11.1 服务健康

业务服务提供：

- `/health/live`：进程存活。
- `/health/ready`：关键依赖可用，失败返回 503。
- 独立 Metrics 监听地址上的 `/metrics`。

至少验证 Account、Billing、Gateway、Portal 和三个 Relay；再检查 Keycloak Realm discovery、NATS health 和 New API status。

普通 `/health/ready` 只证明单个进程及其关键依赖达到服务级就绪，不能替代 TOK-37 的客户端切换 readiness。后者还必须验证客户端/服务端兼容版本、OIDC、跨服务契约、模型目录、READY 路由及计价配置，并保证不创建用户 Hold 或费用。

生产环境必须通过 `global.trustedTimeSources` 配置至少三个独立 NTP 源，并在 `external.timeSourceCidrs` 只放行这些源的 UDP/123；正式环境建议配置五个独立来源，以允许两个来源短暂超时而仍取得三份可信样本。示例地址必须替换为部署方实际治理的独立授时源。服务取全部成功样本的中位偏差，100ms 内为正常目标，超过250ms记录告警，超过1秒使 `/health/ready` 失败并以 `CLOCK_UNSAFE` 拒绝新的 POST/PUT/PATCH/DELETE 时间敏感操作。验收需分别注入100ms、250ms和1秒以上偏差，保存至少三个成功来源的UTC偏差、告警和拒绝证据；未完成真实授时与偏差注入前不得宣称 NFR-46 已部署。

支付和模型 Provider 熔断按 `provider_id + capability/action + route` 隔离：60秒至少20个非业务样本且失败率达到50%，或连续5次连接/超时/不确定失败时打开；30秒后最多三个安全半开探测，连续三次成功关闭，失败按30秒至15分钟指数重开。余额不足、输入错误、明确拒绝和Provider业务限流不计基础设施失败。支付创建、退款和模型派发属于高风险操作，结果不确定后不能充当半开探测或由熔断器自动重发，只能走只读查询/核对或人工复核。生产验收必须覆盖每个已启用Provider能力和路由，并接通告警。

### 11.2 公共烟测

使用测试账号在 `<public-base-url>` 验证：

1. Keycloak issuer、JWKS、Token audience 和 HTTPS 正确。
2. 密码登录及自动登录恢复。
3. 滑块、注册短信或短信登录的最小路径（不得在日志输出验证码）。
4. Account bootstrap、个人信息、Web 三个合并 Tab，以及无组织时“账号设置”隐藏组织管理区。
5. 余额、权益包和用量列表。
6. 使用 TOK-37 非计费 readiness 验证内置模型路径；如必须验证非流式和流式协议，只使用专用合成验收身份及隔离测试/运营路由，确认其记录和成本不进入任何真实用户或组织钱包、权益、Hold、用量及账单。
7. 管理后台按 Account 原子平台权限访问，旧 `platform-admin` Claim 不能绕过拒绝；凭据只显示元数据，且只有独立模型运营权限可以访问 Token Account、路由和凭据操作。
8. 系统运行页的服务、NATS、Relay 和 New API 状态可信。
9. `SYSTEM_PROBES_JSON` 与实际部署拓扑一致，key 唯一；远端探针 URL 不含凭据或 fragment。Compose 远端部署通过站点私有 `.env` 的 `MODEL_GATEWAY_STATUS_URL` 指向模型网关在共享容器网络中的真实健康地址，不得假设站点一定提供 `newapi` DNS 别名。通用 JSON 健康接口只通过 `json_boolean_field` 声明成功字段，Relay 才使用 backlog 指标 Adapter；替换 Identity Provider、模型网关或事件组件时修改配置而非 Portal 代码。

### 11.3 支付烟测

UAT 只使用明确的测试商品和测试商城账号：

1. 创建订单前确认 `/v1/payment-capabilities` 的 Provider/版本、渠道和动作与正式合同一致，并从运行时目录核对所选商品的准入、可售性、金额、映射、权益和有效期；不得从部署脚本或文档读取商品事实。
2. 返回二维码和应付金额必须与商城事实一致。
3. 客户端退出后，Billing 继续轮询。
4. 商城 PAID 后仅入账一次，权益包与平衡账本一致。
5. 重复查询、服务重启和晚到支付不重复入账。
6. 普通退款的新建入口无论能力声明如何均保持关闭；H3C 当前支持支付回调但不支持退款、主动关单和账单，回调固定声明 `notification_supported=true`，其他动作继续按各自能力声明启用，并明确显示 `QUERY_DEGRADED`，不得把查询快照描述为渠道账单。既有退款查询/收敛和强制退款异常处置不因入口关闭而停用。
7. 新订单必须保存 Provider/版本、商品映射 ID、映射快照和能力快照；历史订单不随 Adapter 或映射升级变化。
8. 订单越过72小时边界后只产生一个晚到支付案件；认领、复核查询、安排、用户申诉、归档和 blind 升级均保留不可变时间线。
9. 无回调无账单 Provider 只有在有限窗口、长期历史查询、权威未支付终态和证据保留均满足时才允许正式新单；blind 后同版本新单立即失败关闭。
10. 创建订单返回的 Provider 最后付款期限必须原样冻结为本地 `expires_at/provider_payable_until`；轮询、重启或再次打开二维码不得延长期限。
11. 到达最后付款期限且仍未收到成功事实时，Worker 必须先执行一次最终权威查询；查询仍未支付后进入 `LOCALLY_CLOSED_EXTERNAL_PENDING`（Provider 不支持主动关单时），清除公开响应中的二维码和支付链接，并拒绝把该订单重新激活为可支付。
12. 已支付、已超时、本地已关闭或其他终态订单不得继续出现在“待支付订单”及二维码弹窗；支付成功后当前弹窗应自动收敛并消失。期限后晚到的可信成功仍按 `LATE_PAID` 幂等履约，不能因本地关闭丢失资金事实。
13. 普通用户未付款和正常到期关闭不得计入 Provider/商品基础设施异常。注入单订单异常时只隔离订单；同商品达到规则时只暂停该商品；只有连续异常、跨商品异常、Provider 不可查询或失败率/样本多样性达到已发布策略时才关闭 Provider。
14. 商品或 Provider 恢复必须核验最新受信探测、观察缺口清零、控制/策略/目录/准入版本未漂移、申请人与审批人不同且审计已落库。Provider 仍暂停时商品恢复失败关闭；Provider 恢复后商品必须重新走双人恢复，不得自动可售。

### 11.4 客户端三层发布门禁

正式发布还必须更新 `compatibility.releases`（Compose 对应 `PINVOU_CLIENT_RELEASE_MANIFEST`），按当前版本、前一小版本、前两小版本从新到旧排列。Chart schema 和服务启动校验会拒绝少于三个版本、后继发布后支持不足90天、正常通知不足60天，以及缺少审批号的7天安全紧急淘汰。`PINVOU_CLIENT_UPGRADE_URL` 必须是发布方控制的 HTTPS 地址。发布前分别以三个版本执行 Account bootstrap、Billing 下单、Gateway 下单和平台模型合同；在通知时间点确认 `Deprecation`、`X-Client-Sunset-At` 与升级 `Link`，在淘汰时间后确认远程写返回稳定426且处理器、支付 Provider 和模型 Provider均未被调用，同时验证只读请求、本地数据和本地模型保持可用。

1. **发布门禁**：发布流水线设置 `PINVOU_RELEASE_ARTIFACT`、`PINVOU_RELEASE_ATTESTATION`、`PINVOU_CLIENT_COMPATIBILITY_URL`、`PINVOU_RELEASE_CLIENT_VERSION`、目标环境、公钥和 key ID，执行 `npm run release:account-platform-gate`。命令先执行三语源码/制品门禁，缺键、多键、空词条、单语硬编码、原始后端错误回退或 Portal 制品缺少任一静态词条均直接失败；随后核验 HTTPS 公开兼容范围、OIDC discovery、制品 SHA-256、版本区间以及 TOK-37 Ed25519 证明，才能把新客户端提升到正式更新渠道；它不接收内部 readiness 工作负载凭据，失败时渠道继续指向上一版本。
2. **安装门禁**：更新清单或 Windows OTA `UpdatePackInfo.json` 必须携带 `readinessAttestation`。更新器使用 `PINVOU_ACCOUNT_ENVIRONMENT`、`PINVOU_CLIENT_READINESS_KEY_ID`、`PINVOU_CLIENT_READINESS_PUBLIC_KEY` 验证证明绑定的环境、制品、目标版本、CHAT 目录、六项 PASS 检查和 15 分钟有效期，再用 `PINVOU_UPDATE_SIGNER_IDENTITY` 验证 Windows Authenticode、macOS codesign 或 Linux dpkg-sig 身份，并执行只读迁移预检。通过后才写入安装门禁事务证据并调用平台安装器；旧清单、缺字段、签名不符或预检失败一律不进入安装。平台安装器对二进制/配置的原子替换与失败回滚仍须用正式制品做环境验收，桌面端不得持有内部 readiness 工作负载凭据。
3. **运行门禁**：已经安装的新版本只运行账号平台和 Token Gateway 路径；平台不可用时显示原因与修复动作，不调用旧模型后台。
4. Portal 入口配置 `PINVOU_ACCOUNT_PORTAL_URL=https://account.pinvou.com/`，只允许 HTTPS（或 loopback HTTP）。客户端打开该完整固定地址，不得把 Token、查询串或任意 WebView URL 传给系统浏览器。Compose 的 `PORTAL_PUBLIC_URL` 配置为 `https://account.pinvou.com`，Keycloak 客户端必须允许 `https://account.pinvou.com/auth/callback`。上线前先为 `account.pinvou.com` 配置 DNS，使用 `deploy/nginx-pinvou-account-domain-bootstrap.conf` 完成 ACME 校验和证书签发，再启用 `deploy/nginx-pinvou-account-domain.conf`；HTTP 仅跳转 HTTPS，两个 `/api/v1/payment-callback*` 精确路径进入 Billing，其余同源请求进入 Portal。不得在 `pinvou.com` 保留旧个人中心、回调、BFF 或重定向路由。
5. 三层门禁分别保存构建版本、服务版本、配置 schema、检查结果和脱敏证据。客户端范围门禁执行 `npm run check:account-platform-client-scope`；不得以开发机手工调用真实用户付费模型代替任何一层门禁。生产签名身份、正式安装包原子提交/回滚及真实环境连续无副作用证据完成前，APT-053 保持 `BLOCKED_EXTERNAL`。

### 11.5 首个超级管理员安全激活

数据库迁移和 Identity 管理私网准备完成后，只能从受控主机或集群运维工作负载运行 `account-security`；该命令不是常驻服务，不得配置 Ingress、Service 或公网路由。先提供 `DATABASE_URL`、`OIDC_ISSUER` 及 Keycloak Provisioner 的受控连接配置，再显式选择一种模式：

```text
# 终端直接设置正式密码；命令无回显读取两次，不接受密码参数或密码环境变量
account-security bootstrap --mode INTERACTIVE_SET_PASSWORD \
  --deployment-identity <deployment-workload>

# 一次性引导；凭据只写入受控描述符 3，不写 stdout/stderr
account-security bootstrap --mode ONE_TIME_BOOTSTRAP \
  --deployment-identity <deployment-workload> --ttl 15m --delivery-fd 3

# 兑换时，引导凭据由描述符 3 输入，正式密码由终端无回显输入两次
account-security redeem --deployment-identity <deployment-workload> --credential-fd 3
```

非终端自动化必须通过 `--password-fd` 传入两行完全一致的正式密码；禁止把正式密码或引导凭据放入参数、环境变量、Compose/Helm values、普通标准输出、日志或部署制品。一次性凭据默认 256 bit、43 字符无填充 Base64URL、15 分钟有效，最长 60 分钟；第 5 次失败锁定并产生 CRITICAL 告警。重新签发会先使同一部署身份的旧代次失效。

激活验收必须确认：`admin` 业务状态从 `BOOTSTRAP_PENDING` 变为 `ACTIVE`；Identity 用户只在正式密码设置成功后启用；只有一个有效 `PLATFORM_SUPER_ADMIN` 绑定；旧 Session 已撤销；服务端校验值已删除；安全审计存在且不能更新、删除或截断。任何一步失败都不得开放后台流量；Identity 已激活但 Account 提交失败时命令会补偿禁用 Identity。

### 11.6 Break-glass 异常失联恢复

Break-glass 只用于凭据丢失、意外停用或 Identity 故障导致全部超级管理员均无法有效登录的事故。计划内交接必须走正常超级管理员授权/审批，不得使用本命令；该命令也不能让最后一个有效超级管理员主动注销。

```text
account-security break-glass \
  --operator-credential-fd 3 \
  --ticket <incident-ticket> \
  --reason "<至少20字符的事故原因>" \
  --evidence-file <controlled-incident-evidence> \
  --confirm ALL_SUPER_ADMINS_UNREACHABLE \
  --duration 30m
```

执行环境必须以 `BREAK_GLASS_OPERATOR_PUBLIC_KEY_BASE64URL` 注入受控 Ed25519 公钥；描述符 3 输入签名凭据包，载荷绑定唯一凭据 ID、运维主体、恢复目标、固定受众、签发时间和最长 15 分钟有效期。CLI 不接受操作者或目标身份参数，签名无效、过期、目标被篡改或凭据 ID 重放均失败。命令从终端无回显读取并确认正式密码；非终端场景使用 `--password-fd`。持续时间必须在 5–60 分钟内。执行后必须立即核对 `BREAK_GLASS_ACTIVATED` CRITICAL 告警、`security.break-glass.activated` 不可变审计、受信运维主体、凭据 ID、工单、目标、原因、证据 SHA-256 和到期时间，并在 Identity 恢复后通过正常流程完成长期管理员交接。仓库没有 Break-glass HTTP API。

当前仍缺生产等价环境的并发兑换、Keycloak/数据库提交故障补偿、告警投递和完整事故演练证据，因此 ADM-01/ADM-02 保持 `NOT_DEPLOYED`，APT-007 继续保持 `PENDING`，待生产等价环境执行。

## 12. 日志与可观测性

### 12.1 日志位置

当前 Compose 服务以统一结构化 JSON 写标准输出/错误，固定字段为 `time/level/service/environment/version/event/result`，按请求或任务补充 `request_id/trace_id`；首选通过 Docker 查看：

```text
docker compose --env-file <secret-env> <compose-files> logs --since 30m <service>
docker logs --since 30m <container>
```

Docker 的底层 JSON 日志通常由 Engine 保存在其数据目录，但运维不应直接编辑或删除正在使用的日志文件，应配置 Docker 日志轮转或集中日志采集。

New API 自身日志位置由其独立部署定义决定；核对用量优先调用受控日志查询 API，不通过应用日志推算资金事实。

### 12.2 关联字段

- `X-Request-ID`
- `X-Trace-ID`
- `traceparent`
- 服务名、环境、版本和 Commit

外部 `X-Request-ID` 仅允许64字符内的字母数字及 `._:-`，否则服务生成新的128-bit值；共享 HTTP Client 自动传播受信值。Trace 通过 W3C `traceparent`、Outbox `trace_id` 和 NATS 消费上下文延续，Trace ID 只写日志和 Prometheus Exemplar，不作为普通标签。业务资源 ID 只进入受权限控制的领域证据/审计，不进入通用日志或指标。

日志字段采用失败关闭白名单。禁止记录密码、短信验证码、Access/Refresh Token、Cookie、商城 App Secret、阿里云 AccessKey Secret、内部服务 Token、New API Admin Token、真实供应商密钥、请求/响应正文、事件 Payload 或完整模型正文；原始 error 只映射为稳定事件/错误码。发布前必须运行 `npm --prefix account-platform run check:observability-release`，发现绕过共享 Handler 或附加受保护字段即阻断。

### 12.3 关键告警

- Readiness 连续失败或 5xx/延迟升高。
- Billing 账本不平或余额投影不一致。
- PENDING 超过 Provider 最后付款期限、最终查询缺口、`LOCALLY_CLOSED_EXTERNAL_PENDING` 长期不收敛或终态订单仍残留支付指令。
- 单订单隔离、商品暂停和 Provider 暂停数量突增；连续异常、跨商品、不可查询和失败率阈值必须分别带策略版本与原因告警，正常未付款不得混入。
- `REVIEW`/`SETTLEMENT_PENDING` 用量请求超出自动核对窗口。
- 有效 Hold 过期、无法解释或长期未收敛。
- Outbox 积压、Relay 重试、NATS Consumer Lag 和死信增长。
- 商城用户同步任务持续失败。
- PostgreSQL/NATS 卷和 Docker 数据目录空间不足。

## 13. 回滚

### 13.1 应用回滚

只有当数据库 Schema 与上一版本应用向后兼容，且上一部署清单、版本目录和不可变镜像 digest 均仍可用时，才能执行应用回滚：

1. 停止新增发布动作，保留支付轮询、到期最终查询、用量核对和事件投递；冻结会制造更多不确定事实的入口。
2. 读取当前与上一 `deployment-manifest.json`，核对数据库版本、Compose 摘要、目标服务、上一镜像 digest 和站点配置兼容性；不允许使用 `latest`、重新构建旧源码或仅凭目录名猜测镜像。
3. 使用上一版本的站点 override（其中 `image:` 指向清单记录的不可变 tag/digest）执行 `docker compose ... up -d --no-deps --wait <affected-services>`。
4. 核验每个运行容器的镜像 ID/digest、健康响应 Commit 和 Compose 工作目录均与上一清单一致，再原子恢复稳定符号链接。
5. 重新执行健康、身份、聊天、账本、支付尾单、支付到期关闭、Outbox/Relay 和本次变更领域烟测，并记录回滚观察窗。

若新版本尚未切换稳定链接但已经替换了同一 Compose project 的容器，仍必须按上述镜像清单回滚；稳定链接未切换不代表运行服务仍是旧版本。若 Schema 不兼容或已产生旧应用不能表达的新事实，停止普通应用回滚，按专项约束保留 Schema 并采用前向修复。

不要因为应用回滚而停止 Billing 对已创建订单的查询，也不要释放结果仍不确定的模型 Hold。

### 13.2 数据库恢复

数据库恢复只用于确认的数据事故，必须先冻结相关写流量并保全故障现场。跨库恢复需要统一恢复点，否则 Account、Billing、Token 和 Keycloak 可能失去引用一致性。

优先采用前向修复；只有经过演练并获得事故负责人批准才恢复备份。Billing 恢复后必须运行账本平衡、订单、权益包、Hold 和支付交易审计；Token 恢复后必须核对 Usage 与 New API 日志；Account 恢复后必须核对身份和组织所有者。

具体 SQL 和检查见：

- `deploy/runbooks/backup-restore.md`
- `deploy/runbooks/sql/account-restore-audit.sql`
- `deploy/runbooks/sql/billing-ledger-audit.sql`
- `deploy/runbooks/sql/token-restore-audit.sql`

## 14. 备份、保留和安全清理

必须备份的持久状态：PostgreSQL 各数据库、NATS JetStream、独立模型网关数据库与挂载目录、站点 override、部署 Secret 的受控版本引用，以及边缘代理、证书引用、转发链路、防火墙和 MTU/MSS 配置。Redis Portal 会话可丢失并要求用户重新登录，不应把 Redis 当长期事实源。上述资产必须组成带统一恢复时刻、摘要和验证结果的完整恢复集，不能分别保留一批互不对应的“最新文件”。

当前发布目录、当前应用主机和当前边缘主机都不能是自身恢复材料的唯一保存位置。站点 `.env`、override、模型网关私有清单和边缘配置必须有加密离机副本或受控配置源；证书私钥和备份解密密钥仍由独立 Secret 系统交付，恢复包只保存引用。删除旧部署、旧卷或旧主机前，必须完成隔离的新主机全量恢复演练和观察窗，并取得明确批准。

可以在确认当前版本稳定、备份保留策略满足且不影响恢复目标后清理：

- 已被新版本替代的旧源码发布目录。
- 不再引用的旧应用镜像和构建缓存。
- 超出保留期且已有更近可验证备份的迁移临时备份。
- 已轮转且不被容器使用的归档日志。

不得直接删除：

- 当前符号链接目标和上一可回滚版本。
- 正在运行容器引用的镜像、挂载目录或日志文件。
- PostgreSQL、NATS 或 New API 的活跃数据卷。
- 尚未收敛支付、Hold、人工修复和审计所需的数据。
- 当前有效的 `.env`、站点 override 和 Secret，除非已有受控替代且服务验证完成。
- 当前和上一部署清单引用的镜像 digest、Compose 文件、迁移计划及备份证据。

清理前先用只读命令核对 Docker 容器、镜像引用、卷挂载、目录绝对路径和磁盘占用；不要对未解析变量、通配符、`/opt` 根目录或 Docker 根目录执行递归删除。

## 15. Kubernetes/Helm 部署摘要

Chart 位于 `deploy/helm/account-platform/`。使用前：

1. 为所有镜像固定不可变 tag/digest。
2. 预先安装 External Secrets Operator，并配置最小权限的 `SecretStore`/`ClusterSecretStore`；Chart 的 `externalSecrets` 只引用远端对象，不把 Secret 值写进 values 文件或渲染产物。
3. 将 PostgreSQL、Redis、NATS、Keycloak 和 New API 的持久性、高可用和备份责任明确分配给平台组件或托管服务。
4. 配置 Ingress 只公开允许路径，NetworkPolicy 默认拒绝并只放行必要服务流向。
5. 迁移使用一次性 Job，并在应用滚动前完成。
6. 为应用配置 readiness/liveness、资源请求限制、PodDisruptionBudget 和指标采集。
7. Keycloak Admin 使用 port-forward/VPN/受限入口，不建公网 Ingress。
8. 先在非生产命名空间执行 Helm 渲染、合同测试、故障测试和恢复演练。
9. 为 Portal、Identity、OIDC 三个公网入口配置受信任 TLS 证书，并确保发布验收时剩余有效期不少于 14 天。

示例只使用占位符：

```text
helm upgrade --install <release> account-platform/deploy/helm/account-platform \
  --namespace <namespace> --create-namespace \
  -f <non-secret-values.yaml>
```

### 15.1 生产 TLS 与密钥轮换验收

发布系统必须在站点专用 Secret 中提供三个公网 HTTPS 探针、Token Gateway readiness、KMS/Vault 健康接口、当前/上一 key ID 和短期工作负载令牌，然后执行：

```text
npm --prefix account-platform run security:production-readiness
```

Token Gateway 不应建立公网入口；可通过受控网络访问其 HTTPS 内部端点，或先 `port-forward` 后使用 `http://127.0.0.1:<port>/health/ready`。验收会验证公网端点不经重定向返回预期状态、证书链可信且未进入 14 天续期窗口、所有旧密文已重加密、当前 key 为 `ENCRYPT_DECRYPT` 且上一 key 已 `DISABLED`。只归档命令输出的脱敏 JSON、制品版本与部署标识；不得归档工作负载令牌或 Secret 响应。任一检查失败均为 NO-GO，继续保留旧 key 的解密能力并停止提升。

## 16. 相关 Runbook

Token Gateway 同时运行托管路由 Provisioning Worker。上线迁移 `000013_route_provisioning` 后，应监控 `PENDING` 最老年龄、FAILED 重试量、REPAIR_REQUIRED 数量和 Worker 租约过期；滚动发布可安全重领过期租约，但不得通过 SQL 或管理接口直接把聚合改为 READY。模型目录回滚会同步检查 Billing 当前价格及 Token 当前 READY 账号、凭据、路由代次，任一依赖不可确认时失败关闭。生产验收至少应覆盖 Worker 崩溃重领、重复请求不重复建资源、动态目录新增模型、凭据失效后新代重建、SLA 转人工修复及修复命令幂等。

模型执行恢复继续执行 Token `000014_usage_execution_replay`，并先确认生产 Secret Provider 可同时解密旧凭据和新重放载荷。迁移会把既有原始模型幂等键不可逆替换为 SHA-256 摘要，并建立稳定用户级唯一索引；因此必须先滚动 Gateway 客户端，确保每个逻辑模型请求都发送16至128字符且不会复用的 `Idempotency-Key`。发布后验证：跨 OIDC Session 使用同键同请求只恢复同一执行，同键异请求在 Hold 前返回409；分别在 Hold 请求、Hold 成功、派发意图、取得租约及流事件阶段杀进程，只有能够证明未派发的准备阶段继续原执行，已取得派发租约但结果未知且 Adapter 不保证幂等的请求进入 REVIEW；断开 SSE 后服务端继续读取并结算，使用原键和 `Last-Event-ID` 只补发更大序号。监控 `HOLD_RESERVING`、`UPSTREAM_DISPATCHING` 租约过期、`REVIEW`、事件/结果清理失败和 KMS 解密失败；不得通过延长普通表 TTL、复制正文到日志或人工清除墓碑来恢复请求。流事件、结果和墓碑从执行/财务终态起算，硬上限分别为24小时、30天和90天；法律保全/未结案件只能迁入独立受限案件表并在解除后30天内清理。备份必须在30天内生命周期过期或使用可销毁独立数据密钥，三年用量/账本/审计证据禁止包含模型正文。写入新版执行状态、清除派发正文或释放过期墓碑键后不得回滚旧 Gateway；紧急故障应停止接收新模型请求并前向修复。

- `deploy/runbooks/backup-restore.md`
- `deploy/runbooks/full-redeployment.md`
- `deploy/runbooks/cutover-rollback.md`
- `deploy/runbooks/newapi-deployment.md`
- `deploy/runbooks/keycloak-admin-access.md`
- `deploy/runbooks/event-delivery.md`
- `deploy/runbooks/observability.md`
- `deploy/runbooks/rpo-rto-exercise-template.md`

## 17. Billing 系统总账账户与调试清理

Billing `000045_system_ledger_accounts_repair` 会幂等恢复支付清算、用量收入、退款清算、人工调整、应收和拒付清算六个系统级总账账户，并为支付回调入箱增加失败次数、稳定错误码和退避时间。迁移必须先于 Billing 应用发布；应用启动时会再次校验六个账户的固定 ID、代码、类型及无钱包归属，缺失或被改写时拒绝承接流量。

用户调试数据清理只能删除明确归属于目标用户或钱包的数据，不得清空 `ledger_accounts`，也不得删除 `wallet_id IS NULL` 的平台级账户。清理后至少校验：六个系统账户完整、待处理回调不会以高频率反复失败、同一支付流水仅产生一个权益包和一组平衡分录。回调失败只记录稳定错误码，不记录原始请求、回调载荷或用户标识；Worker 按指数退避重试，基础依赖恢复后继续走原有幂等归并流程。

## 18. 完全重新部署判定与执行入口

完全重新部署不是 `docker compose up` 的同义词。执行前必须在恢复记录中明确以下三种模式之一：

| 模式 | 适用范围 | 可宣称的结果 |
|---|---|---|
| `APPLICATION_REBUILD` | 仅重建应用容器/镜像，原四库、NATS、模型网关、Secret 和边缘配置均保留 | 应用重建完成，原业务状态未丢失 |
| `STATEFUL_RESTORE` | 新主机或持久状态丢失，从完整恢复集恢复 | 当前系统恢复到声明的恢复点 |
| `EMPTY_BOOTSTRAP` | 全新独立环境，不恢复历史状态 | 仅初始化空平台，不是现有系统恢复 |

`STATEFUL_RESTORE` 的最小输入是：目标 Commit、应用及基础镜像的不可变 digest/离线包、最终 Compose 与站点 override、四个 PostgreSQL 数据库和角色证据、NATS JetStream、独立模型网关数据与私有配置、站点 Secret 版本、边缘代理/证书引用/网络配置，以及同一恢复集的摘要、RPO/RTO 和最近演练证据。缺少任一资金、身份、事件、模型或入口状态时均为 NO-GO；不得用重新创建管理员、商品、Provider、角色、定价或模型渠道来冒充历史恢复。

标准执行顺序为：验证恢复集与目标 Commit → 在隔离主机恢复私有配置 → 恢复 PostgreSQL 四库 → 恢复 NATS → 使 Redis 会话失效并启动空实例 → 恢复独立模型网关 → 按 digest 导入/拉取镜像，或在锁定输入下构建带版本/Commit 的不可变镜像 → 执行无环迁移计划 → 启动身份、业务、Relay 与 Portal → 恢复边缘入口 → 完成身份、权限、资金、支付、模型、用量、事件和公网验收 → 原子切流。详细命令和失败处理统一见 [`../deploy/runbooks/full-redeployment.md`](../deploy/runbooks/full-redeployment.md)。

恢复状态必须逐级记录：`ARTIFACTS_VERIFIED`、`INFRA_RESTORED`、`SERVICES_STARTED`、`DOMAIN_VERIFIED`、`FULLY_RESTORED`。健康检查全绿最多只能证明 `SERVICES_STARTED`；只有跨域账实核对、回调/模型/权限烟测、实际 RPO/RTO 和批准均完成，才可标记 `FULLY_RESTORED`。旧环境和回滚材料的清理必须是之后的独立审批动作。
