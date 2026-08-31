# 账号平台全量备份、PITR 与恢复

本流程适用于生产 Account、Billing、Token、Keycloak。Payment Simulator 仅允许存在于隔离开发/测试环境，不是生产备份对象，也不能作为资金事实恢复来源。目标和分类以 [`../recovery-policy.json`](../recovery-policy.json) 为机器可读单一来源：资金事实 RPO 不超过 60 秒、账号与配置 RPO 不超过 300 秒；核心认证、Account、Billing、Gateway 和支付状态查询 RTO 不超过 60 分钟，报表、导出及非关键后台 RTO 不超过 4 小时。

数据库备份只是完整恢复集的一部分。能够重新启动 PostgreSQL 不等于账号平台已经恢复；NATS JetStream、独立模型网关、站点 Secret、边缘代理/证书/网络配置和目标 Commit 制品必须绑定同一个恢复集。完整新主机重建步骤见 [`full-redeployment.md`](full-redeployment.md)。

## 完整恢复集清单

每次全量备份或重大发布前快照必须生成唯一 `<recovery-set-id>`，并在独立不可变存储中保存以下内容及 SHA-256：

| 状态域 | 必备内容 | 恢复说明 |
|---|---|---|
| PostgreSQL | globals/角色清单、Account、Billing、Token、Keycloak 四库、自增/WAL 位点 | 四库使用统一目标恢复时刻；恢复后仍需外部支付和模型证据收敛 |
| NATS JetStream | stream、consumer、ack/pending 元数据，或经过演练的离线卷归档 | 恢复前 Relay 关闭；空 NATS 不能冒充成功恢复 |
| Redis | 无长期备份 | 恢复后 Session、验证码挑战和短期风险状态全部失效，用户重新登录 |
| 独立模型网关 | 数据库、数据挂载、不可变镜像、私有配置版本、渠道/模型/定价恢复证据 | 与账号平台 Secret 中的调用/核对凭据版本配套恢复 |
| 站点配置 | 加密 `.env`、站点 Compose override、Secret/KMS 版本引用 | 明文不进入恢复清单；发布目录不是唯一副本 |
| 公网边缘 | 反向代理、证书引用、隧道/转发、防火墙、MTU/MSS 和开机持久化说明 | 私钥由证书/Secret 系统交付，不打包进普通备份 |
| 发布制品 | 完整 Commit、源码/制品摘要、Compose 摘要、应用及基础镜像 digest/平台、可选离线镜像包、迁移清单 | 健康响应和镜像必须能证明同一 Commit；恢复不能依赖可变 tag |

恢复集清单记录各子备份的开始/结束时间和一致性边界。禁止把不同日期的四库、NATS 和模型网关备份随意拼成一个“最新备份”。若外部组件只能取得较旧恢复点，统一恢复点必须向较旧边界收敛，并记录之后需要通过 Provider 查询、Outbox 和人工案件补偿的范围。

## 创建恢复集

1. 冻结发布和配置变更，记录支付订单、模型执行、Outbox、NATS consumer 与模型网关的观察边界；正常业务可以继续时使用在线/PITR能力，不得为备份直接释放 Hold 或改终态。
2. 为 PostgreSQL 创建 globals/角色清单和四库自定义格式 dump，启动/完成时间、WAL 位点、工具版本和文件摘要写入恢复清单。备份命令只能从受控脚本或短生命周期运维 Job 运行，凭据使用 `PGPASSFILE`/工作负载身份，不写命令行。
3. 使用 NATS 官方管理工具导出所有受管 stream 和 consumer；无法在线导出时，在批准维护窗口停止三个 Relay 和 NATS，创建文件系统一致的 JetStream 卷归档，然后原样恢复服务。备份必须包含恢复工具版本和 stream/consumer 清单。
4. 调用模型网关自己的备份流程，保存数据库和挂载数据、镜像 digest、配置版本以及 `/api/status`、模型目录和日志查询验证结果。账号平台备份脚本不得假定模型网关由同一 Compose 管理。
5. 从 Secret 管理系统导出加密站点配置或记录可恢复的不可变 Secret 版本；解密密钥保存在独立 KMS/Vault，不能与密文同介质。
6. 由边缘所有者导出脱敏反向代理、证书清单和网络持久化配置；明确应用主机、边缘主机和公开域名之间的逻辑关系，不记录 SSH 私钥或真实 Secret。
7. 计算全部 SHA-256，验证备份可读性，把恢复集复制到独立账号/区域的不可变存储。只有隔离恢复演练通过后，状态才能标记为 `RESTORABLE`。

单独的数据库 dump、宿主机磁盘快照或发布目录副本只能标记为 `PARTIAL`，不能满足完整重部署门禁。

### PostgreSQL 四库备份示例

以下示例在受限恢复集目录中生成自定义格式 dump。`<compose>` 表示与目标站点完全相同的 `--env-file` 和全部 `-f` 参数；执行前设置 `umask 077`，并确认目标目录不是发布目录或 Docker 活跃卷。`globals.sql.gz` 含角色属性和密码摘要，必须按 Secret 级别加密、传输和保留：

```sh
umask 077
export RECOVERY_POSTGRES_DIR=<absolute-recovery-set>/postgres
mkdir -p "$RECOVERY_POSTGRES_DIR"

docker compose <compose> exec -T postgres \
  pg_dumpall -U account_platform_admin --globals-only \
  | gzip -9 > "$RECOVERY_POSTGRES_DIR/globals.sql.gz"

docker compose <compose> exec -T postgres \
  pg_dump -U account_platform_admin -Fc account_platform_keycloak \
  > "$RECOVERY_POSTGRES_DIR/keycloak.dump"
docker compose <compose> exec -T postgres \
  pg_dump -U account_platform_admin -Fc account_platform_billing \
  > "$RECOVERY_POSTGRES_DIR/billing.dump"
docker compose <compose> exec -T postgres \
  pg_dump -U account_platform_admin -Fc account_platform_account \
  > "$RECOVERY_POSTGRES_DIR/account.dump"
docker compose <compose> exec -T postgres \
  pg_dump -U account_platform_admin -Fc account_platform_token \
  > "$RECOVERY_POSTGRES_DIR/token.dump"

for dump in keycloak billing account token; do
  docker compose <compose> exec -T postgres pg_restore -l \
    < "$RECOVERY_POSTGRES_DIR/$dump.dump" >/dev/null
done

(cd "$RECOVERY_POSTGRES_DIR" && \
  sha256sum globals.sql.gz keycloak.dump billing.dump account.dump token.dump \
    > SHA256SUMS && sha256sum -c SHA256SUMS)
```

命令成功只证明文件可列目录和摘要一致，不等于已验证可恢复。必须在隔离 PostgreSQL 实例执行完整 restore、迁移校验和、三份领域审计 SQL 与跨域账实核对，才能把该子备份标记为 `VERIFIED`；只有其余恢复集组件也通过后，整个恢复集才能标记为 `RESTORABLE`。

## 备份与迁移前门禁

- 所有数据库持续归档 WAL/等价日志；Billing、Token 的最近成功归档不得早于 60 秒，Account、Keycloak 不得早于 300 秒。
- 每 24 小时至少一次增量备份、每 7 天至少一次全量备份；数据库迁移、账本结构变更及重大发布前额外创建一致性快照。
- 传输最低 TLS 1.2，静态最低 AES-256 或等价强度；解密密钥由独立 KMS/Vault 保存，不与备份同域；至少一份副本位于独立账号/区域的不可变存储，生产服务身份无删除权限。
- 每日备份保留 35 天，月度恢复点保留 13 个月。业务三年保留通过分类归档实现，不无限延长普通备份。
- 凭据只通过受控 `PGPASSFILE`、工作负载身份或 Secret 注入；数据库 URL、密码、密钥、签名令牌不得进入命令历史、日志或证据文件。
- 生产迁移 Job 在连接数据库前强制校验 `MIGRATION_BACKUP_EVIDENCE_ID`、24 小时内的 `MIGRATION_BACKUP_COMPLETED_AT` 和 `MIGRATION_BACKUP_SHA256`。缺少任一项即失败关闭。

仓库 `migrations/manifest.json` 固化所有历史迁移的规范化 LF SHA-256。已有迁移不得改名、删除或修改；新增版本只能使用：

```sh
node scripts/account-platform-migration-manifest.mjs append
node scripts/account-platform-migration-manifest.mjs check
```

迁移器仍以数据库 `schema_migrations` 的实际校验和二次拒绝篡改。迁移完成后输出不含秘密的 `migration.post_verification` JSON，必须保存每库已应用数量、最高版本及校验和；不能用 Helm Job 成功状态代替该结果。

## 隔离恢复顺序

1. 建立全新隔离实例/namespace，关闭 Ingress、支付回调、定时任务租约和模型出口；不得覆盖原实例，不得让新旧环境同时连接相同 NATS consumer 或接收相同公网流量。
2. 记录故障宣布、恢复开始、目标恢复时刻、构建 SHA、完整恢复集 ID/SHA-256、WAL 位点、NATS/模型网关备份 ID 和各域最后一条可验证业务事实。
3. 恢复站点 Secret/override 的受控副本，核对版本引用和文件权限；恢复独立模型网关，但在 Token Gateway 就绪前不开放真实模型流量。
4. 依次恢复 Keycloak、Billing、Account、Token。多库不是分布式快照；恢复后必须以支付渠道证据和不可变账本收敛。
5. 对每库运行当前迁移器；已执行迁移校验和不同或文件缺失时立即停止，禁止修改历史脚本“适配”恢复库。
6. 运行 `sql/account-restore-audit.sql`、`billing-ledger-audit.sql`、`token-restore-audit.sql`、`outbox-inventory.sql`；任一非零异常先调查。
7. 恢复并核对 NATS stream、consumer、ack floor、pending 和 redelivery 后才启动三个 Relay。NATS 不可恢复时进入受审计事件核对流程，不得用空实例直接承接写流量。
8. Redis 使用空实例启动，明确撤销全部旧 Session、验证码挑战和短期登录事务；不得尝试从日志恢复它们。
9. 在业务 Deployment 与 Ingress 仍关闭时启动依赖和服务，检查 Readiness、Outbox、Inbox 与死信。Inbox 依赖 `event_id` 去重，不得清空后重放。
10. 对过期 Hold 先审阅。`UPSTREAM_ACTIVE`、`SETTLEMENT_PENDING`、`REVIEW` 不得按超时直接释放；只运行仓库 `hold-recovery` 受限作业。
11. 主动查询恢复点后仍可能到达的支付订单，取得渠道证据并做四方对账；“渠道已付、恢复库未入账”只能通过验签通知/安全查询归并器补入。
12. 恢复边缘代理和证书到临时受控入口，验证精确路由、管理路径拒绝、SSE、转发链路以及 MTU/MSS 持久化，再执行正式入口切换。
13. 验证抽样登录、授权隔离、余额/权益、充值查询、Token 结算、支付状态只读查询和审计，再由事故、财务、安全负责人共同批准流量。
14. 以恢复点前最后业务事实与恢复后最后可验证事实计算实际 RPO；以故障检测/宣布较早者到各能力安全就绪计算 RTO。权限、密钥、边缘恢复和介质取得时间都计入。

账本或支付未核对时，可以在目标 RTO 内仅恢复明确只读/禁资金写/禁模型调用的安全降级，不得声明完全恢复。

## 生产证据门禁

`npm --prefix account-platform run check:production-recovery -- <evidence.json>` 校验四库 WAL/增量/全量、加密、密钥分离、异地不可变副本、分级 RPO/RTO、月/季/年演练，以及分类删除扫描与收据。证据只能保存不可逆 ID、时间、计数和 SHA-256，不得保存备份 URL、凭据或业务正文。

## 禁止操作

不得直接修改钱包快照、账本 Posting、支付终态或 Usage 终态；不得删除 Outbox/Inbox“清理积压”；不得让恢复环境和原环境同时接收同一支付回调。回滚应用版本不等于回滚数据库；数据库恢复必须走新隔离实例和完整核验。
