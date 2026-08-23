# 账号平台部署与运维

> 状态：当前部署基线
>
> 校准日期：2026-08-23
>
> 安全要求：本文只列配置名称和占位符，不保存真实域名、IP、账号、密码、AccessKey、Token 或商城密钥。

## 1. 部署形态

### 1.1 当前实际形态

当前环境运行在单台 Linux 主机上：

- Docker Compose 管理账号平台应用服务和基础设施。
- 仓库基础定义为 `deploy/compose/compose.yaml`，远程公共 URL 覆盖为 `compose.remote.yaml`，生产环境还可叠加不入库的站点专用 override。
- 宿主机反向代理终止 HTTPS，并把允许公开的路径转发到回环端口。
- 独立 New API 为模型数据面，由 Token Gateway 通过私网/容器网络调用。
- 发布物放在带 Git Commit 和时间戳的版本目录，稳定符号链接指向当前版本。
- 数据库、Redis、NATS、New API 和内部管理端口不直接向公网开放。

该形态是可运维的单机部署，但不是高可用架构。主机、Docker Engine 或本地 PostgreSQL 故障会影响整个平台。

### 1.2 仓库提供的目标形态

`deploy/helm/account-platform/` 是 Kubernetes/Helm 生产目标模板，包含 Deployment、Service、健康检查、指标抓取和网络策略等材料。除非实际集群已经部署并验证，不得以 Chart 存在为由宣称当前环境具备多副本、高可用、自动故障转移或既定 RPO/RTO。

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

远程 Compose 覆盖已把 `payment-simulator` 主进程置于 `development-simulator` profile，但基础 Compose 的数据库初始化仍创建模拟器数据库和角色，`payment-simulator-migrate` 也尚未进入同一 profile。因此当前单机模板还没有完成“生产不保留测试数据库”的隔离，不能据此宣称生产清单已净化；应在部署前通过站点覆盖排除，并在后续代码变更中把初始化、迁移和主进程统一纳入非生产 profile。

生产必须显式选择正式 Payment Provider 并失败关闭，不能沿用 Compose 的默认模拟路径。当前 Billing 启动和 Helm 模板仍使用 `PAYMENT_SIMULATOR_URL`、`PAYMENT_SIM_HMAC_*` 等历史命名承载部分 Provider 配置；在这些配置迁移为供应商中立名称前，部署验收必须证明 `BILLING_PAYMENT_PROVIDER=h3c_mall`、正式 Adapter 配置和路由均已生效，且 Simulator 分支不可达。历史命名和仍待隔离的测试数据库属于实现债务，不表示 Simulator 是生产依赖。

## 4. 目录约定

建议的单机发布布局：

```text
/opt/
  pinvou-account-platform-<commit>-<timestamp>/
    account-platform/
    pinvou3-app/
    scripts/
    VERSION
    .env                         # 站点 Secret，不入仓库
    compose.site.yaml            # 站点覆盖，不入仓库
  pinvou-account-platform-current -> pinvou-account-platform-<commit>-<timestamp>/
  pinvou-account-platform-backups/
    <deployment-id>/
```

不要在原目录上直接覆盖源码。新版本在独立目录完成构建、迁移和验证后，再原子切换 `pinvou-account-platform-current`。

## 5. 前置条件

- Linux 主机、Docker Engine 和 Compose Plugin。
- 可解析且已配置 TLS 的 `<public-base-url>`。
- 反向代理能够访问本机回环端口。
- 独立 New API 已初始化管理员、供应商 Channel 和最小权限 API Token，并能按请求 ID 查询用量日志。
- 阿里云短信认证的 RAM AccessKey、签名和模板已审核可用。
- 新华三商城环境、App 凭据和四个商品 SKU 已可联调。
- Secret 通过主机 Secret 文件、Docker Secret、Vault/KMS 或平台 Secret 管理注入；不能进入 Git。
- 发布前有足够空间保存新镜像、数据库备份和上一个可回滚版本。
- 已明确发布负责人、数据库负责人、支付尾单负责人和回滚决策人。
- Account 数据库允许迁移用户安装 `pgcrypto` 扩展；若由 DBA 预装，发布前核验 `digest` 与 `gen_random_bytes` 可用。

## 6. 配置清单

### 6.1 通用与数据库

| 类别 | 配置名 |
|---|---|
| Compose | `COMPOSE_PROJECT_NAME`、`PUBLIC_BASE_URL` |
| PostgreSQL 管理 | `ACCOUNT_PLATFORM_POSTGRES_PASSWORD` |
| 分库密码 | `ACCOUNT_DB_PASSWORD`、`BILLING_DB_PASSWORD`、`TOKEN_DB_PASSWORD`、`KEYCLOAK_DB_PASSWORD` |
| 开发库密码 | `PAYMENT_SIM_DB_PASSWORD`、`NEWAPI_DB_PASSWORD` |
| 内部鉴权 | `INTERNAL_SERVICE_TOKEN` |

每个数据库用户必须使用不同随机密码。生产环境不得沿用 `.env.example` 中的占位值。

### 6.2 OIDC 与 Keycloak

- `KEYCLOAK_ADMIN_USERNAME`
- `KEYCLOAK_ADMIN_PASSWORD`
- `KEYCLOAK_PROVISIONER_CLIENT_ID`
- `KEYCLOAK_PROVISIONER_CLIENT_SECRET`
- 各服务的 `OIDC_ISSUER`、`OIDC_JWKS_URL`、`OIDC_AUDIENCE`
- Portal 的 `PORTAL_PUBLIC_URL`、`PORTAL_OIDC_AUTHORIZATION_URL`、`PORTAL_OIDC_TOKEN_URL`、`PORTAL_OIDC_CLIENT_ID`

`OIDC_ISSUER` 必须与外部 Token 中的 issuer 完全一致；容器访问 JWKS 可使用内部 URL，但不能改变验证的 issuer。

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

Account 启动注册恢复、资料变更恢复、钱包初始化、角色绑定到期和审批期限 worker。发布前必须确认 `registration_operations`、`profile_change_requests`、`password_reset_operations`、`wallet_provisioning_jobs` 以及 Account `000016_role_binding_lifecycle`、`000018_approval_commands`、`000019_organization_lifecycle`、`000020_organization_unit_lifecycle`、`000021_membership_governance`，Billing `000017_approval_execution`、`000018_organization_lifecycle`、`000019_organization_unit_blockers`、`000020_organization_member_lifecycle` 和 Token `000008_organization_lifecycle`、`000009_organization_unit_blockers` 迁移完成，注册查询 HMAC 密钥在同一部署周期内稳定，且 Billing/Token 内部组织、节点及成员生命周期接口可用。角色 worker 每分钟固化已到期的组织与平台授权；即使 worker 短暂延迟，授权查询也会按数据库 `now()` 立即拒绝过期绑定。运维应监控仍满足 `revoked_at IS NULL AND expires_at <= now()` 的绑定积压、`account.role-binding.changed.v1` Outbox 和 `role.binding.expire` 审计；不得通过直接改表延长临时/紧急授权。

组织生命周期上线必须先检查现有活动组织名称在忽略大小写后无重复；若发现重复，须先由业务确认保留名称并完成审计化更名，不得绕过唯一索引。随后先迁移 Billing 与 Token，再迁移 Account，并先滚动 Billing/Token、再滚动 Account，确保 Account 开始调用两者的 `/internal/v1/organizations/{organization_id}/lifecycle` 时目标端点已经可用。上线后抽样验证：同一创建幂等键只产生一个组织；停用后组织钱包为 `FROZEN`、Token 组织投影为 `DISABLED`，且新充值/Hold/组织用量均被拒绝；余额、有效权益、未结算 Hold、支付/退款/差异、未终态用量或治理阻断任一存在时删除返回 `ORGANIZATION_LIFECYCLE_BLOCKED`；全部收敛后 Account、Billing、Token 分别为 `DELETED`、钱包 `CLOSED`、`DELETED`，历史账本、订单、用量和审计仍可按保留权限查询。远端超时或版本冲突只能保持或进入更严格状态，不得直接改表解冻或跳过预检。

组织节点生命周期沿用同一上线顺序。迁移 Account 前先检查同一组织、同一父节点下的活动节点名称在忽略大小写后无重复。抽样验证创建同键重放不生成第二个节点，同键异请求冲突；移动以 `expected_structure_version` 拒绝旧树写入且闭包查询只看到完整旧树或新树；删除含子节点、成员、有效/未来角色、审批、Billing 授予登记、Token Account/路由或未终态用量的节点时返回 `ORGANIZATION_UNIT_LIFECYCLE_BLOCKED` 及逐项原因。删除尝试后即使远端超时，Billing/Token 的 `DELETION_PENDING` 也必须继续阻止新增关联事实；清零重试后各域进入 DELETED，Account 节点、闭包、命令和审计仍保留。禁止通过直接改表解除限制态或物理清除历史节点。

成员治理迁移前必须执行与迁移内相同的检查，确认每个未删除组织恰有一个当前有效 OWNER；发现零个或多个所有者时先通过审计化修复窗口收敛，迁移不得自动选择。上线仍按 Billing 先、Account 后：先确认 Billing 成员生命周期接口可用，再开放新版邀请和成员命令。抽样验证手机号、用户名和链接邀请均只消费一次，任意目标探测不返回用户是否存在；重复添加/移除及所有权转让同键返回原结果，同键异请求冲突；成员移除后所有 Session 立即回退个人上下文、旧授权失败且 Billing 投影为 REVOKED，重新加入以更高成员版本恢复 ACTIVE。不得物理删除邀请、角色绑定或命令历史，也不得绕过专用事务直接改 OWNER。

财务审批发布后，Billing Service 同时以 durable `billing-command-executor-v1` 消费 `account.approval-command.authorized.v1`。上线顺序必须先完成两库迁移，再启动 Account 与 Account Relay，最后滚动 Billing；Billing 必须能访问内部 NATS 和 Account 内部执行复核端点。监控应覆盖批准后五分钟仍未进入 `EXECUTING`、`EXECUTION_FAILED`、消费者重复投递和 `account-platform.dead-letter.account.approval-command.authorized.v1`。重放只能使用原事件和原指纹，禁止修改消息体、直接调用账本 SQL 或恢复旧 `/adjustments/:id/approve` 路径。当前内部调用仍依赖 `INTERNAL_SERVICE_TOKEN`；切换 APT-047 短期工作负载身份前必须保持令牌仅在 Secret 管理系统中分发且 Billing/Account/NATS 均不公开。

Keycloak provisioner 服务账号还必须具备读取/更新用户、重置密码和撤销用户会话的最小管理权限；缺少任一权限会使资料或密码操作进入稳定失败/修复状态，不得临时扩大为 realm-admin。轮换查询密钥会使尚未完成注册的旧查询令牌失效，必须采用覆盖最长 48 小时令牌窗口的双读或维护窗口方案；当前实现不提供双密钥读取，因此不得无计划直接轮换。运维应监控 `REPAIR_REQUIRED`、`COMPENSATION_PENDING`、钱包 SLA 超时和 `next_retry_at` 积压；人工补偿只能通过受权限保护的命令执行，禁止直接改表或直接删除 Identity。

Account 的 Commerce Customer worker 还要求显式冻结 `COMMERCE_PROVIDER_ID` 与 `COMMERCE_ADAPTER_VERSION`。两者会写入 Mapping、同步任务和证据，升级 Adapter 时不能用新版本解释旧任务；切换 Provider 后服务启动会为全部已验证手机号且尚无当前 Provider Mapping 的用户幂等补建任务。未配置对应 Adapter 连接时注册和模型能力保持可用，Mapping 留在待处理/重试状态，只有依赖该 Mapping 的支付被拒绝。发布前确认 `commerce_customer_mappings`、`commerce_customer_sync_jobs`、`commerce_customer_sync_evidence` 已迁移，并监控 `RETRY`、`REPAIR_REQUIRED` 及待执行任务积压。

### 6.4 新华三商城

- `COMMERCE_PROVIDER_ID=h3c_mall`
- `COMMERCE_ADAPTER_VERSION=h3c-mall-v1`
- `H3C_MALL_BASE_URL`
- `H3C_MALL_APP_KEY`
- `H3C_MALL_APP_SECRET`
- `BILLING_PAYMENT_PROVIDER=h3c_mall`
- `BILLING_ALLOW_TENTATIVE_PRICES`

UAT 联调可显式允许暂定目录；正式环境必须冻结商品后关闭该开关。商城特殊状态映射由 H3C 适配层处理，不能通过通用环境变量绕过服务端商品目录。

### 6.5 Token Gateway 与 New API

- `TOKEN_SECRET_PROVIDER`
- `TOKEN_SECRET_KEY_ID`
- `TOKEN_LOCAL_KEK_BASE64`（仅开发/测试）
- `TOKEN_PROVIDER_ALLOWED_HOSTS`
- `TOKEN_DEFAULT_PROVIDER`
- `TOKEN_DEFAULT_PROVIDER_BASE_URL`
- `TOKEN_DEFAULT_PROVIDER_API_KEY`
- `TOKEN_DEFAULT_PROVIDER_MODELS`
- `TOKEN_DEFAULT_ROUTE_PRIORITY`
- `TOKEN_GATEWAY_MAX_INPUT_TOKENS`
- `TOKEN_RECONCILIATION_NEWAPI_BASE_URL`
- `TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN`
- `TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID`
- `TOKEN_RECONCILIATION_INTERVAL_SECONDS`

`production`/`production-new` 环境必须拒绝本地 KEK，使用 KMS/Vault 类 Provider。Provider URL 必须是 HTTPS 且主机精确命中白名单。

### 6.6 桌面端

- `PINVOU_ACCOUNT_PLATFORM_ENABLED`
- `PINVOU_ACCOUNT_OIDC_ISSUER` 或显式 Token URL
- `PINVOU_ACCOUNT_OIDC_CLIENT_ID`
- `PINVOU_ACCOUNT_SERVICE_URL`
- `PINVOU_IDENTITY_SERVICE_URL`
- `PINVOU_TOKEN_GATEWAY_URL`

开发态 `npm run dev` 不需要每次生成安装包。远程端点必须 HTTPS，只有 `127.0.0.1`/`localhost` 允许 HTTP。

## 7. 本地开发

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

## 8. 单机 Compose 发布流程

下面流程以 `<release-dir>`、`<backup-dir>` 和 `<compose-files>` 为占位符。站点专用配置和 Secret 不进入仓库。

### 8.1 发布前检查

1. 确认目标 Git Commit、当前运行 Commit、数据库迁移版本和当前符号链接。
2. 检查工作树/发布包来源，确保构建内容可追溯且不包含本地私人文件。
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

4. 检查磁盘空间、Docker 可用空间、数据库健康、待处理支付订单、有效 Hold、Outbox 积压和死信。
5. 验证 Secret 文件权限，确认不会被复制到公开日志或提交。

### 8.2 创建版本目录

1. 创建 `/opt/pinvou-account-platform-<commit>-<timestamp>`。
2. 将已审核源码或发布包复制到版本目录。
3. 从当前部署安全复制 `.env` 和站点 override，仅保留受限权限。
4. 在版本目录记录完整 Git Commit 和构建时间。
5. 不修改当前稳定符号链接，不停止现有服务。

### 8.3 数据库备份

迁移前至少备份：

- PostgreSQL globals/roles。
- Account 数据库。
- Billing 数据库。
- Token 数据库。
- Keycloak 数据库。
- 独立 New API 数据库（若本次变更涉及）。

备份写入 `<backup-dir>/<deployment-id>`，生成校验和并执行最小可读性检查。Billing 备份是资金恢复依据，未验证备份时不得迁移。

即使此前部署后人工删除过迁移备份，也不能把“无需备份”固化为流程。可按保留策略在稳定观察期和恢复演练完成后清理旧备份，但发布前备份仍是必需门禁。

### 8.4 构建镜像

在新版本目录使用仓库 Compose 与站点 override 构建业务和迁移镜像：

```text
docker compose --env-file <secret-env> <compose-files> build \
  account-service billing-service token-gateway token-portal \
  account-event-relay billing-event-relay token-event-relay \
  account-migrate billing-migrate token-migrate
```

如果站点需要 Go Module 代理，只通过受控构建参数临时配置，不在仓库硬编码区域性代理，也不把代理凭据写入镜像层。

### 8.5 执行迁移

迁移顺序：

1. `account-migrate`
2. `billing-migrate`
3. `token-migrate`

每个迁移任务必须退出码为 0，再继续下一个。迁移器记录版本和校验和；已应用迁移文件不得修改，只能新增更高版本。迁移器可兼容仅换行符不同的旧校验和，但不接受语义内容变化。

Account `000011_permanent_user_codes` 会为历史用户回填永久编码并建立不可变触发器。迁移后必须核验：所有用户均有格式与校验码有效的唯一编码、`user_code_registry` 与用户一一对应、更新 `users.id/user_code` 和物理删除用户均被拒绝。不得通过禁用触发器、清空登记表或回滚迁移来“释放”已发放编码。

不要默认执行 down migration。涉及破坏性 Schema 的发布必须先提供前向兼容阶段和单独数据迁移方案。

### 8.6 替换应用服务

迁移完成后只更新应用服务，不主动重建正在运行且无需变更的 PostgreSQL、Redis、NATS 和独立 New API：

```text
docker compose --env-file <secret-env> <compose-files> up -d --no-deps \
  account-service billing-service token-gateway token-portal \
  account-event-relay billing-event-relay token-event-relay
```

使用 `--no-deps` 的前提是基础设施已经健康、网络和服务名没有变化。若发布涉及基础设施，必须使用单独审批和维护窗口。

### 8.7 切换稳定链接

应用和公共烟测通过后，原子更新：

```text
/opt/pinvou-account-platform-current -> /opt/pinvou-account-platform-<commit>-<timestamp>
```

符号链接只表示当前发布源码/配置位置；正在运行容器的镜像 Commit 仍需通过健康响应或镜像标签独立验证。

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

### 11.2 公共烟测

使用测试账号在 `<public-base-url>` 验证：

1. Keycloak issuer、JWKS、Token audience 和 HTTPS 正确。
2. 密码登录及自动登录恢复。
3. 滑块、注册短信或短信登录的最小路径（不得在日志输出验证码）。
4. Account bootstrap、个人信息、无组织时隐藏组织页签。
5. 余额、权益包和用量列表。
6. 使用 TOK-37 非计费 readiness 验证内置模型路径；如必须验证非流式和流式协议，只使用专用合成验收身份及隔离测试/运营路由，确认其记录和成本不进入任何真实用户或组织钱包、权益、Hold、用量及账单。
7. 管理后台按 Account 原子平台权限访问，旧 `platform-admin` Claim 不能绕过拒绝；凭据只显示元数据，且只有独立模型运营权限可以访问 Token Account、路由和凭据操作。
8. 系统运行页的服务、NATS、Relay 和 New API 状态可信。

### 11.3 支付烟测

UAT 只使用明确的测试商品和测试商城账号：

1. 创建订单前确认商品金额、SKU、积分和有效期。
2. 返回二维码和应付金额必须与商城事实一致。
3. 客户端退出后，Billing 继续轮询。
4. 商城 PAID 后仅入账一次，权益包与平衡账本一致。
5. 重复查询、服务重启和晚到支付不重复入账。
6. 退款、主动关单、回调和账单对账入口保持禁用/不支持。

### 11.4 客户端三层发布门禁

1. **发布门禁**：发布流水线通过 TOK-37 并确认服务端兼容后，才能把新客户端提升到正式更新渠道；失败时渠道继续指向上一版本。
2. **安装门禁**：更新器验证包签名、TOK-37 生成且绑定制品/环境/服务版本的短时签名准入证明，并在暂存副本完成兼容性和迁移预检后，才原子提交应用与配置；失败保持或恢复旧二进制和旧配置的匹配组合。桌面端不得持有内部 readiness 工作负载凭据。
3. **运行门禁**：已经安装的新版本只运行账号平台和 Token Gateway 路径；平台不可用时显示原因与修复动作，不调用旧模型后台。
4. 三层门禁分别保存构建版本、服务版本、配置 schema、检查结果和脱敏证据。不得以开发机手工调用真实用户付费模型代替任何一层门禁。

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
  --workload-identity <incident-workload> \
  --target-username <existing-user> \
  --reason "<至少20字符的事故原因>" \
  --evidence-file <controlled-incident-evidence> \
  --confirm ALL_SUPER_ADMINS_UNREACHABLE \
  --duration 30m
```

命令从终端无回显读取并确认正式密码；非终端场景使用 `--password-fd`。持续时间必须在 5–60 分钟内。执行后必须立即核对 `BREAK_GLASS_ACTIVATED` CRITICAL 告警、`security.break-glass.activated` 不可变审计、工作负载身份、目标、原因、证据 SHA-256 和到期时间，并在 Identity 恢复后通过正常流程完成长期管理员交接。仓库没有 Break-glass HTTP API。

当前仍缺生产等价环境的并发兑换、Keycloak/数据库提交故障补偿、告警投递和完整事故演练证据，因此 ADM-01/ADM-02 保持 `NOT_DEPLOYED`，APT-007 标记 `BLOCKED_EXTERNAL`。

## 12. 日志与可观测性

### 12.1 日志位置

当前 Compose 服务以结构化 JSON 写标准输出/错误，首选通过 Docker 查看：

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
- 业务资源 ID（只在受控系统中记录订单 ID、Usage Request ID 等）
- 服务名、环境、版本和 Commit

禁止记录密码、短信验证码、Access/Refresh Token、Cookie、商城 App Secret、阿里云 AccessKey Secret、内部服务 Token、New API Admin Token 和真实供应商密钥。

### 12.3 关键告警

- Readiness 连续失败或 5xx/延迟升高。
- Billing 账本不平或余额投影不一致。
- PENDING/本地过期支付订单持续增长。
- `REVIEW`/`SETTLEMENT_PENDING` 用量请求超出自动核对窗口。
- 有效 Hold 过期、无法解释或长期未收敛。
- Outbox 积压、Relay 重试、NATS Consumer Lag 和死信增长。
- 商城用户同步任务持续失败。
- PostgreSQL/NATS 卷和 Docker 数据目录空间不足。

## 13. 回滚

### 13.1 应用回滚

只有当数据库 Schema 与上一版本镜像向后兼容时，才能：

1. 停止新增发布动作，保留支付轮询、用量核对和事件投递。
2. 用上一版本镜像对应用服务执行 `up -d --no-deps`。
3. 恢复稳定符号链接。
4. 重新执行健康、身份、聊天、账本和支付尾单验证。

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

必须备份的持久状态：PostgreSQL 各数据库、NATS JetStream、独立 New API 数据和部署 Secret 的受控副本。Redis Portal 会话可丢失并要求用户重新登录，不应把 Redis 当长期事实源。

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

清理前先用只读命令核对 Docker 容器、镜像引用、卷挂载、目录绝对路径和磁盘占用；不要对未解析变量、通配符、`/opt` 根目录或 Docker 根目录执行递归删除。

## 15. Kubernetes/Helm 部署摘要

Chart 位于 `deploy/helm/account-platform/`。使用前：

1. 为所有镜像固定不可变 tag/digest。
2. 使用外部 Secret 管理，不把 Secret 值写进 values 文件或渲染产物。
3. 将 PostgreSQL、Redis、NATS、Keycloak 和 New API 的持久性、高可用和备份责任明确分配给平台组件或托管服务。
4. 配置 Ingress 只公开允许路径，NetworkPolicy 默认拒绝并只放行必要服务流向。
5. 迁移使用一次性 Job，并在应用滚动前完成。
6. 为应用配置 readiness/liveness、资源请求限制、PodDisruptionBudget 和指标采集。
7. Keycloak Admin 使用 port-forward/VPN/受限入口，不建公网 Ingress。
8. 先在非生产命名空间执行 Helm 渲染、合同测试、故障测试和恢复演练。

示例只使用占位符：

```text
helm upgrade --install <release> account-platform/deploy/helm/account-platform \
  --namespace <namespace> --create-namespace \
  -f <non-secret-values.yaml>
```

## 16. 相关 Runbook

- `deploy/runbooks/backup-restore.md`
- `deploy/runbooks/cutover-rollback.md`
- `deploy/runbooks/newapi-deployment.md`
- `deploy/runbooks/keycloak-admin-access.md`
- `deploy/runbooks/event-delivery.md`
- `deploy/runbooks/observability.md`
- `deploy/runbooks/rpo-rto-exercise-template.md`
