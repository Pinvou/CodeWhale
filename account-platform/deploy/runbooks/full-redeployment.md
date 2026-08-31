# 账号平台完整重部署与现有状态恢复

本 Runbook 用于在应用容器、镜像或整台应用主机不可用时，使用已审核代码、不可变制品和站点私有恢复包重建账号平台。目标是恢复已有用户、身份、权限、订单、余额、权益、用量、支付收敛和模型路由，而不是只启动一组空容器。

执行本 Runbook 不代表可以删除原环境。只有隔离重建、全链路验收、恢复点核对和观察窗均通过，并获得事故、财务、安全和运维负责人批准后，才能另行清理旧实例。

## 1. 重部署模式与结论标准

开始前必须选择且只选择一种模式，并写入恢复记录：

| 模式 | 删除/故障范围 | 数据处理 | 完成标准 |
|---|---|---|---|
| `APPLICATION_REBUILD` | 仅应用容器和镜像 | 保留 PostgreSQL、NATS、模型网关数据、站点 Secret 和边缘配置 | 原状态无损，迁移校验、服务健康和领域烟测通过 |
| `STATEFUL_RESTORE` | 新主机或持久状态丢失 | 从同一恢复集恢复四库、NATS、模型网关和配置 | 已有业务状态恢复到声明时间点，并完成跨域核对 |
| `EMPTY_BOOTSTRAP` | 新建独立环境 | 不恢复历史数据 | 只得到全新空平台；不得称为现有系统恢复 |

`EMPTY_BOOTSTRAP` 不会自动恢复管理员业务授权、商品目录、Provider 控制、商城映射、模型渠道、定价、历史订单或用户。若任务目标是恢复当前系统，不得使用该模式替代 `STATEFUL_RESTORE`。

## 2. 恢复包是前置输入，不从故障主机临时拼装

完整重部署必须从独立 Secret/备份系统取得一个已经校验的 `<recovery-set-id>`。恢复集至少包含：

```text
<recovery-set-id>/
  recovery-manifest.json                 # 脱敏、签名或带独立校验和
  source/
    source.tar.gz                        # 目标 Commit 的干净源码或发布制品
    source.tar.gz.sha256
  images/
    images-manifest.json                 # 应用、基础镜像的不可变 digest 与平台
    images.tar.zst                       # 可选；Registry 不可用时的离线镜像包
    SHA256SUMS
  deployment/
    deployment-manifest.json             # Commit、镜像 digest、Compose 摘要、迁移版本
    compose.site.yaml                     # 站点私有覆盖，不含 Secret 值
    account-platform.env.enc              # 加密 Secret 文件，不与解密密钥同域
  postgres/
    globals.sql.gz
    account.dump
    billing.dump
    token.dump
    keycloak.dump
    SHA256SUMS
  nats/
    jetstream-backup.tar.gz               # 或受支持的 stream/consumer 导出集
    SHA256SUMS
  model-gateway/
    deployment-manifest.json
    database.dump
    data-backup.tar.gz
    config.enc
    SHA256SUMS
  edge/
    topology.json                         # 仅逻辑角色和公开主机名，不含凭据
    reverse-proxy.conf
    tunnel-firewall-mss.md
    certificate-inventory.json            # 证书引用、签发者、到期日，不保存私钥
    SHA256SUMS
  evidence/
    backup-verification.json
    last-restore-exercise.json
```

真实 Secret、证书私钥、Provider Token 和备份解密密钥必须由独立 Vault/KMS/Secret 管理系统交付；不得放入源码、恢复清单、命令参数、Shell 历史或聊天记录。发布目录中的 `.env` 只是运行副本，不能作为唯一恢复来源。

`recovery-manifest.json` 至少记录：

- 恢复集 ID、环境、创建时间、统一目标恢复时刻和时区。
- 源码完整 Commit、工作树/制品摘要、Compose 与站点 override 摘要。
- 四库备份 ID、WAL/等价日志位点、NATS 和模型网关备份 ID。
- Secret 版本引用和 key ID；只记录引用，不记录值。
- 边缘代理、应用主机、模型网关的逻辑所有者和配置版本。
- 每个备份文件的 SHA-256、加密方式、KMS key 引用和恢复验证结果。
- 预期数据库迁移最高版本、应用及基础镜像 digest/平台、上一可回滚部署和批准人。

以下任一情况为 NO-GO：恢复集超过对应 RPO、校验和不符、缺少四库中任一库、Secret 版本不可取得、NATS/模型网关没有明确处理结论、边缘入口所有者未知、目标 Commit 或应用/基础镜像无法从不可变来源取得，或最近恢复演练不覆盖当前恢复格式。

## 3. 持久状态与恢复责任

| 状态 | 当前责任边界 | 完整重部署要求 |
|---|---|---|
| Account/Billing/Token/Keycloak | 账号平台 PostgreSQL | 使用同一恢复集恢复；Billing 和支付事实不得单库随意选点 |
| NATS JetStream | 账号平台 NATS 持久卷 | 恢复 stream、consumer 和投递位置；不能把空 NATS 当作成功 |
| Redis | 短期 Session、验证码/风险和缓存 | 不作为长期事实恢复；重建后强制全部会话重新登录，未完成挑战失效 |
| 独立模型网关 | 独立部署和数据库/挂载目录 | 由模型网关清单先行恢复；Token Gateway 不得回退到 Mock Provider |
| 站点 Secret | 独立 Vault/KMS/加密配置备份 | 按固定版本恢复，文件权限不宽于 `0600`，恢复后逐项轮换计划可追踪 |
| 公网边缘 | 反向代理、证书、转发链路、防火墙和 MSS | 使用边缘恢复包重建；管理路径、内部端口和模型网关不得暴露公网 |
| 商品、角色、Provider 状态和定价 | 业务数据库中的运行时事实 | 由数据库恢复，不从部署文档、环境变量或硬编码重新生成 |

若 NATS 备份不可用，必须在恢复记录中标记 `EVENT_STATE_RECONCILIATION_REQUIRED`，保持支付创建、模型调用和财务写入口关闭，先核对各库 Outbox/Inbox 与外部 Provider，再由受审计重放恢复投递。不得直接清空 Outbox/Inbox、修改游标或假定所有事件均可安全重复。

## 4. 重部署前置门禁

1. 明确 `<deployment-id>`、`<recovery-set-id>`、目标 Commit、恢复模式、目标恢复时刻、维护窗口和责任人。
2. 新主机与原实例隔离；在切换前不得同时接收同一支付回调、相同公网域名流量或相同定时任务租约。
3. 验证主机时间、磁盘、内存、Docker Engine、Compose Plugin、DNS、出站代理和备份/KMS访问。
4. 验证构建容器可以访问站点固定的 `GOPROXY`。中国区站点必须在受控 Secret 配置中使用经验证的可达代理；变量名必须精确为 `GOPROXY`，禁止使用拼写变体或只设置宿主机变量。
5. 校验源码、应用及基础镜像 digest/离线包、Compose、站点 override、迁移清单、四库、NATS、模型网关和边缘恢复文件的 SHA-256。
6. 执行 `node scripts/account-platform-migration-manifest.mjs check`，并把结果写入恢复记录。
7. 解密后的 Secret 只写入受限临时目录或目标运行目录，权限为 `0600`；操作完成后按 Secret 管理策略安全清理临时副本。
8. 确认最新 Provider 支付、回调、未结订单、Hold、Outbox、人工案件和模型执行的可验证边界。无法取得故障前快照时，恢复后必须扩大对账范围。

只读预检示例：

```sh
git rev-parse HEAD
git status --porcelain
node scripts/account-platform-migration-manifest.mjs check
docker version
docker compose version
df -h /opt /var/lib/docker
sha256sum -c <recovery-set>/source/source.tar.gz.sha256
sha256sum -c <recovery-set>/postgres/SHA256SUMS
sha256sum -c <recovery-set>/nats/SHA256SUMS
```

## 5. 新主机目录与私有配置恢复

建议目录：

```text
/opt/pinvou-account-platform-<commit>-<timestamp>/
/opt/pinvou-account-platform-current -> 上述目录
/var/lib/pinvou-recovery/<deployment-id>/      # 0700，临时恢复工作区
/etc/pinvou-account-platform/                  # 0700，站点配置运行副本
  account-platform.env                         # 0600
  compose.site.yaml                            # 0600 或 0640
```

操作顺序：

1. 从不可变制品解压到新的版本目录，不从旧工作树复制源码。
2. 核对目标 Commit 和源制品摘要。
3. 从 Secret 管理系统恢复 `/etc/pinvou-account-platform/account-platform.env`，从私有配置备份恢复 `compose.site.yaml`。
4. 将运行副本安装到版本目录，或在所有部署命令中直接引用 `/etc` 下的受控文件；不得让发布目录成为唯一 Secret 来源。
5. 使用全部 Compose 文件渲染并检查：

```sh
export RELEASE_DIR=/opt/pinvou-account-platform-<commit>-<timestamp>
export SECRET_ENV=/etc/pinvou-account-platform/account-platform.env
export SITE_OVERRIDE=/etc/pinvou-account-platform/compose.site.yaml

cd "$RELEASE_DIR/account-platform/deploy/compose"
docker compose --env-file "$SECRET_ENV" \
  -f compose.yaml -f compose.remote.yaml -f "$SITE_OVERRIDE" config --quiet
docker compose --env-file "$SECRET_ENV" \
  -f compose.yaml -f compose.remote.yaml -f "$SITE_OVERRIDE" config --services
```

渲染结果中不得出现未替换占位符、Mock/Simulator 生产工作负载、意外公网端口、空必填 Secret、错误 Provider、`APP_ENV=development/test` 或未记录的站点覆盖。UAT 环境必须显式记录其测试开关，不能与正式环境混淆。

## 6. 基础设施和状态恢复顺序

以下命令中的 `<compose>` 表示同一组 `--env-file` 与 `-f` 参数。`--clean` 恢复会覆盖目标库，只允许在已确认的全新隔离 PostgreSQL 实例执行。

### 6.1 PostgreSQL 与四库

1. 只启动新 PostgreSQL，让初始化脚本建立固定角色和空数据库：

```sh
docker compose <compose> up -d --wait postgres
```

2. 把 `globals.sql.gz` 作为角色/属性核对证据，不要直接把原始 `pg_dumpall --globals-only` 输出管入已经完成初始化的实例；固定角色已由 `init-databases.sh` 使用当前 Secret 创建，直接重放 `CREATE ROLE` 会产生冲突。比较备份角色清单与新实例，存在固定角色以外的业务角色时，由 DBA 生成经过复核、可幂等执行的角色恢复脚本并单独留证。
3. 在恢复目标为空且恢复负责人再次确认后恢复四库。恢复顺序为 Keycloak、Billing、Account、Token；保留 dump 中的对象 owner 与 ACL，所引用角色必须已在上一步存在：

```sh
docker compose <compose> exec -T postgres \
  pg_restore -U account_platform_admin -d account_platform_keycloak \
    --clean --if-exists --exit-on-error \
  < <recovery-set>/postgres/keycloak.dump

docker compose <compose> exec -T postgres \
  pg_restore -U account_platform_admin -d account_platform_billing \
    --clean --if-exists --exit-on-error \
  < <recovery-set>/postgres/billing.dump

docker compose <compose> exec -T postgres \
  pg_restore -U account_platform_admin -d account_platform_account \
    --clean --if-exists --exit-on-error \
  < <recovery-set>/postgres/account.dump

docker compose <compose> exec -T postgres \
  pg_restore -U account_platform_admin -d account_platform_token \
    --clean --if-exists --exit-on-error \
  < <recovery-set>/postgres/token.dump
```

4. 查询四库 `schema_migrations`，与恢复清单核对数量、最高版本和校验和。任何差异先停止，不得修改历史迁移文件或数据库记录。
5. 按主部署文档第 8.5 节生成的无环迁移计划依次运行迁移服务；不能仅按 Account/Billing/Token 固定顺序猜测。
6. 运行 `backup-restore.md` 指定的四库恢复审计 SQL。Billing 账本、权益组成、支付流水和应收任一不平时保持资金写入口关闭。

### 6.2 NATS JetStream

优先使用受支持的 stream/consumer 备份恢复工具。若恢复集采用离线卷归档，必须先停止 NATS，确认目标卷为空，再按恢复清单记录的工具版本恢复。示例仅适用于全新隔离卷：

```sh
docker compose <compose> stop nats
docker run --rm \
  -v <nats-volume>:/restore \
  -v <absolute-nats-backup-dir>:/backup:ro \
  alpine:3.22 sh -eu -c \
  'test -z "$(ls -A /restore)"; tar -xzf /backup/jetstream-backup.tar.gz -C /restore'
docker compose <compose> up -d --wait nats
```

恢复后核对 stream、consumer、ack floor、pending 和 redelivery；随后才能启动三个 Relay。原 NATS 尚在运行时禁止把同一 consumer 身份连接到新旧两套环境。

### 6.3 Redis

Redis 按空实例启动：

```sh
docker compose <compose> up -d --wait redis
```

所有 Portal Session、未完成验证码/滑块挑战、短期风险状态和一次性登录事务视为失效。用户必须重新登录，不得从日志或浏览器 Cookie 反向恢复 Session。

### 6.4 独立模型网关

模型网关必须在 Token Gateway 前恢复，并继续由独立部署清单管理：

1. 使用模型网关不可变镜像和私有 Compose/Secret 恢复数据库、数据目录、渠道、模型、定价与最小权限 Token。
2. 验证管理面不公开，数据面只在批准网络可达。
3. 验证 `/api/status`、模型目录、JSON/SSE、usage 日志和请求 ID 查询。
4. 将稳定的内部健康地址、Provider Base URL、调用 Token 和核对 Token 通过账号平台 Secret 注入。
5. Token Gateway 不得使用 Mock New API 或空目录作为恢复成功条件。

详细边界见 [`newapi-deployment.md`](newapi-deployment.md)。

## 7. 导入或构建镜像、迁移与启动应用

优先从受控 Registry 按 digest 拉取，或从恢复集导入已验证的离线镜像包。只有恢复清单记录了所有应用和基础镜像 digest、依赖锁定文件齐全且构建源可取得时，才允许在恢复主机重新构建；不能只按可变 tag 下载基础镜像。重新构建必须显式写入版本和 Commit，不得让健康响应继续显示 `version=dev`、`commit=unknown`：

```sh
export SOURCE_COMMIT=<full-commit>
export RELEASE_VERSION=<version-or-deployment-id>

# Keycloak 扩展和 Portal 使用各自 Dockerfile；其最终镜像仍必须记录 digest
docker compose <compose> build keycloak token-portal

# Go 应用、Relay 和迁移镜像写入同一版本事实
docker compose <compose> build \
  --build-arg VERSION="$RELEASE_VERSION" \
  --build-arg COMMIT="$SOURCE_COMMIT" \
  account-service identity-service billing-service token-gateway \
  account-event-relay billing-event-relay token-event-relay \
  account-migrate billing-migrate token-migrate
```

导入、拉取或构建后都必须核对每个最终镜像的 digest/平台，并把镜像 ID、digest、完整 Commit、基础镜像 digest 和 Compose 摘要写入 `deployment-manifest.json`。若使用离线镜像包，导入后仍要核对 digest，不得按镜像名推断内容。

按迁移计划显式执行一次性迁移，保存每步退出码和后置校验。然后分阶段启动：

```sh
# 已恢复 PostgreSQL、NATS、Redis 后启动身份底座
docker compose <compose> up -d --wait keycloak

# 恢复模式下重复执行是幂等校准，不得传递明文密码参数
docker compose <compose> exec keycloak \
  sh /opt/keycloak/data/import/provision-mobile-auth.sh

# 再启动业务、事件和 Portal
docker compose <compose> up -d --no-deps --wait \
  identity-service account-service billing-service token-gateway \
  account-event-relay billing-event-relay token-event-relay token-portal
```

平台管理员登录身份和 Account 业务角色是两层事实。恢复 Keycloak 数据库后应保留原身份；`EMPTY_BOOTSTRAP` 才使用 `provision-platform-admin.sh` 建立首个登录身份，并继续按 `account-rbac-migration.md` 显式授予 Account 业务角色。禁止把 Keycloak Realm Role 自动提升为平台超级管理员。

## 8. 边缘代理、证书与网络恢复

公网切换前，由边缘主机所有者从私有恢复包恢复并复核：

- HTTPS 证书引用、自动续期、TLS 参数和 HTTP 到 HTTPS 跳转。
- Portal、OIDC、注册、回调和 Token Gateway 的精确路径路由。
- `/admin/`、Billing/Internal、数据库、NATS、模型管理面和指标端口拒绝规则。
- 应用主机到边缘主机的受控连接、开机自启动和健康监控。
- 防火墙、转发表、路径 MTU 与面向全部客户端连接的 MSS 规则；规则必须持久化并在重启后验证。
- SSE 禁止代理缓冲，连接/读取超时覆盖最长模型响应，且不以宽泛重试制造第二次模型调用。

恢复期间先使用临时受控主机名或本地 Host 映射验收。只有新环境全部检查通过后，才能把正式域名/入口原子切到新环境；旧环境随后停止接收公网流量，但继续保留取证和尾单处理，直到负责人批准下线。

## 9. 完整验收矩阵

### 9.1 基础与版本

- 所有预期服务运行且健康，未启动 Mock/Simulator。
- 健康响应、镜像标签和部署清单返回同一完整 Commit/版本。
- 四库迁移数量、最高版本和校验和与清单一致。
- PostgreSQL、NATS、模型网关和 Secret 版本均对应同一恢复集。
- 公网证书、OIDC issuer、Cookie 域和回调域一致。

### 9.2 身份与权限

- 已有密码登录、手机验证码登录、注册预检和忘记密码正常。
- 已有用户编码、用户名、手机号、组织成员和角色授权与恢复点一致。
- 普通用户、组织管理员、平台运营和超级管理员的允许/拒绝矩阵正确。
- Keycloak 管理面仍只可通过受控隧道或内网访问。

### 9.3 资金与支付

- Billing 六个系统总账账户存在且账本平衡。
- 抽样余额、权益包、订单、流水和履约状态与恢复点/Provider 证据一致。
- 恢复点之后可能成功的订单完成主动查询与回调收敛，不重复入账。
- 支付回调 Token 与通用回调接口可用，重复通知只处理一次。
- 商品和 Provider 控制保持数据库中的实际状态，不因部署默认值自动恢复上架。

### 9.4 模型与用量

- 模型目录、价格、默认 Token Account、凭据和 READY 路由完整。
- 非流式与 SSE 各执行一次隔离测试；思考内容不进入正文。
- 用量、Hold、结算和 New API 请求 ID 能一一核对，断流不造成第二次调用。
- 恢复中的 REVIEW/SETTLEMENT_PENDING 按原证据继续收敛，不被重建过程释放。

### 9.5 事件与可观测性

- 三个 Relay 健康，Outbox、Inbox、NATS consumer 和死信数量有解释。
- 没有因空 NATS 或错误 consumer 位置造成全量重复投递。
- 日志不含密码、验证码、Token、Cookie、Provider Secret 或完整模型正文。
- 告警覆盖健康、账本、支付尾单、Hold、Outbox、磁盘、证书和模型 Provider。

只有上述矩阵全部通过，并记录实际 RPO/RTO、未解决差异和批准人后，才能把恢复结果标记为 `FULLY_RESTORED`。只通过健康检查时最多标记 `SERVICES_STARTED`。

## 10. 失败处理与回退

- 新环境未接流量：停止新环境，保留日志和恢复目录；原环境不变。
- 已切流量但无新写事实：按部署清单切回原入口和原不可变镜像。
- 已产生支付、用量或身份新事实：停止新增高风险入口，保留回调、查询、Outbox 和核对，禁止直接回滚数据库或让新旧系统双写。
- 任一账本、权限隔离、重复模型调用或 Provider 事实不确定：标记事故，进入前向修复/隔离恢复，不执行 down migration。

禁止执行未经逐目标确认的 `docker compose down -v`、`docker volume prune`、递归删除 `/opt`、覆盖当前数据库恢复或同时删除当前与上一恢复集。销毁旧环境必须是独立审批动作，并满足主部署文档的保留和安全清理要求。

## 11. 每次演练必须留下的证据

- 使用的完整 Commit、恢复集和部署 ID。
- 每个阶段开始/结束时间、操作者、复核人和退出码。
- 四库/NATS/模型网关/边缘配置的备份 ID、摘要与恢复结果。
- 实际数据库迁移版本、镜像 digest、服务健康和公网探针结果。
- 身份、权限、支付、余额、模型、用量和事件验收摘要。
- 实际 RPO/RTO、偏差、未解决问题、是否允许删除旧环境的明确结论。

证据只保存不可逆 ID、摘要、计数和稳定错误码，不保存 Secret、个人信息、支付报文或模型正文。
