# 模型管理首次部署

本叠加层交付模型目录、协议和公式、Token Account/凭据/路由管理、十八项治理策略以及三条审批执行链。目录同步、密钥轮换、默认路由配置和管理取消 worker 为实际生产进程。普通聊天、公开用量、签名 GO、组织和注销尚未交付；共享迁移中的必要对象不表示这些功能已启用。

按顺序组合 `compose.personal.yaml`、`compose.personal-workload.yaml`、`compose.finance.yaml`、`compose.reconciliation.yaml` 和 `compose.models.yaml`。使用同一已审核源码构造 migrate、Account、Billing、Token Gateway、Portal、独立 credential Portal、Token executor 和 approval-resources 镜像。所有 image 值须固定到审核过的制品，不在生产自动拉取或生成审批资源。数据库、OpenBao、NATS、身份服务、可验证的 HTTPS OTLP 和至少三个独立时间源均须先准备。此前个人/财务运行规则仍适用。

启动上述组合前显式配置 `ACCOUNT_PLATFORM_PROFILE=models`。`compose.models.yaml` 对 Account、Token Gateway 与 `approval-resources` 三者均明确选择 `models`；推荐保持模型叠加最后。即使再叠加 finance 文件，其 operator 也从这个必填变量读取 `models`，不能回落到 `personal`。这是业务运行模式，不是 Docker Compose profile；未配置的直接二进制仍默认 `models`，未知值拒绝，不能用缺失的 Token URL、digest、完整数据库、KMS 或 provider 输入触发个人模式。

## 数据库和治理

Token 单独数据库和五个部署角色使用 `token-release-prepare` 明确 prepare 操作建立；不能复用 Account/Billing runtime 凭据。该操作的数据库静默、角色和 membership 守卫失败时停止。人工部署须满足同一五角色和 membership 约束，不能借 superuser 给日常 migrator。独立数据库 operator 对空库执行 `token-database-bootstrap.sql`：仅预装原迁移需要的 pgcrypto、明确 public schema 所有权，并授予 token_migrator 统计可见性供离线治理切换查验；Gateway 不获得数据库 CREATE 或统计角色。

然后运行 `token-migrate`，由普通 token_migrator 执行当前发布目录的真实 Up。迁移 wrapper 验证当前稀疏 ledger 与原 SQL 校验和，拒绝超前、部分和降级集合；不能手工补业务表，也不能加载未来匿名化迁移。既有数据库必须走所交付 release-prepare/governance 的离线预检与发行流程，不将首次空库 bootstrap 用作升级或清历史。

本轮 Token 900001 前向硬化收回 Gateway 对审计、业务对象及迁移 ledger 的所有权，补齐不可变语句边界。已安装库在离线预检后，由独立数据库 operator 先执行 `token-storage-hardening-bootstrap.sql` 验证并建立单向 schema owner 到 Gateway 的角色关系，再由普通 migrator 执行 Up；初次空库使用更新后的 `token-database-bootstrap.sql`。该关系只允许 owner 执行原 runtime 所有对象的安全所有权迁移，Gateway 不获得 owner 或政策 deployer 身份；不修改业务资金数据、角色密码或历史 SQL 校验和。不能跳过 bootstrap 使用 runtime 或 superuser 代跑普通迁移，不能用数据库 Down 撤回审计保护。

使用 `token-policy-bootstrap` 提供明确上限、expected version、审计 reason 和稳定 idempotency key；默认不臆造生产配额。Account 和 Token 使用独立核验的当前 PolicySetDigest 与十八项 TokenCommandHandlerDigest。`token-governance` 的 prepare/activate/safe 必须遵循实际命令校验及冻结证据、quiescence 和 CAS；不能因进程 ready 自动激活。后续第十九项用量策略改变集合时必须重新发行并验证，不能把旧 ACTIVE digest 沿用为新集合。

## KMS、凭据和隔离入口

`deploy/openbao` 保留固定摘要镜像、独立 TLS/CA、隔离网络、Raft 持久化、审计和备份操作。首次 init/unseal/bootstrap 不由应用启动自动执行。按 `tools/kms-recovery/README.md` 的 custody、独立审批和审计约束执行 kms-operator bootstrap，再用 StageAgentFiles 将 Gateway、credential Portal、backup 三个 AppRole 角色的 role-id/secret-id 分别投影到只属于该 agent 的私有目录。操作角色不得投影到应用。

`compose.agents.yaml` 的三处 sink 使用 TOKEN_KMS_TOKEN_DIRECTORY、CREDENTIAL_KMS_TOKEN_DIRECTORY、BACKUP_KMS_TOKEN_DIRECTORY；分别只挂给对应消费者。bootstrap、sink、审计、备份和 workload JWT 目录相互独立，应用不得读取 role-id/secret-id。目录 0700、私密文件 0600，并明确运行 UID/GID 可读；POSIX/ACL 须在目标主机核验，Windows 单测不能证明实际权限。不要打印 env、Token、OIDC code、凭据或原始 HTTP 请求。

当前已交付的 KMS Agent 检查器和监控 timer 使用固定 custody 路径。运行该运维闭环时，三项目录须分别设为 `/run/pinvou-kms-agents/pinvou-token-gateway`、`/run/pinvou-kms-agents/pinvou-credential-portal`、`/run/pinvou-kms-agents/pinvou-kms-backup`。备份容器也读取同一 `BACKUP_KMS_TOKEN_DIRECTORY`（未指定时保留上述备份默认路径）。Compose 的目录参数化不代表现有监控器支持任意自定义 sink；不得将其他目录的文件存在检查作为本监控健康证明。

TOKEN_SECRET_PROVIDER 固定 vault-transit；TOKEN_SECRET_PROVIDER_URL 是已验证的 OpenBao HTTPS 根 URL，实际客户端再追加 `/v1/transit/{operation}/{key}`。TOKEN_SECRET_KEY_ID 须与 bootstrap 的实际 transit key 对齐（原计划为 pinvou-token-data），不能当作自选新 key。仅通过各自 TOKEN_SECRET_PROVIDER_TOKEN_FILE 读取轮换后的 sink；禁止普通 env 明文 token/local KEK。combined CA 必须包含 OpenBao、供应商、身份和公开入口的实际信任链。

CREDENTIAL_PORTAL_PUBLIC_URL 只能为已允许的独立 credentials HTTPS origin，与 PORTAL_PUBLIC_URL 分离，不含端口、path、query 或 fragment。为它准备独立 DNS/TLS 和 ingress 地址，渲染 `credential-edge.nginx.conf.template` 中准确的站点名后绑定 443；不将 Portal 自身 origin 当作隔离边界。Keycloak `provision-mobile-auth.sh` 的可选 CREDENTIAL_PORTAL_PUBLIC_URL 将创建独立 public PKCE S256 client、精确 `/oidc/callback` redirect 和 audience。设定对应 CREDENTIAL_OIDC_CLIENT_ID/AUDIENCE、用户 realm issuer/JWKS/auth/token URL；不能使用 workload realm 登录普通用户。

Gateway 私有 env file 除其 receiver rules 外须提供 Token DATABASE_URL、独立分页/管理代理 HMAC、实际 default provider/adapter/base URL/API key/models 配置；credential Portal 的 DSN、receiver rules、workload JWT 和 KMS token 分别独立。TOKEN_PROVIDER_ALLOWED_HOSTS 是准确的受控供应商 DNS allowlist，TOKEN_PROVIDER_CA_FILE 用完整可验证 CA；发现或验密不接受截断、多 JSON、缺失目录事实。默认供应商凭据只放受控私有 env file，不进入 Compose 公共模板或日志。

## 来源缺失与过期预约

来源缺失只更新来源观察并使模型不可用，不自动改写生命周期或新建、续用截止时间。再次发现不会撤销已批准的人工弃用；弃用继续使用既有审批计划和执行路径。

Billing 每分钟检查过期预约，先以短事务持久化绑定原预约身份的恢复记录，再调用 Token，财务行锁不跨服务调用。仅在 Token 对绑定的 usage、hold、wallet 和金额核验成功，并在真实派发行锁下持久取消、阻断未来派发后，复用既有取消释放闭环；Billing 重新读取本地 RELEASED 状态才记完成。释放后进程中断仍由已持久化记录按原身份继续确认，不重复派发。缺失 canonical 证据、已派发、超时或未知结果均保留资金，在独立恢复记录登记 MANUAL_REVIEW，不凭过期时间或未安装普通调用 producer 推断未派发。

本层要求 `TOKEN_HOLD_EXPIRY_URL` 为已验证的私有 HTTPS Token 根地址，并由 `WORKLOAD_IDENTITY_CA_FILE` 验证其证书与主机名；不得填入现有明文容器地址或公开 Portal 地址。模板不自行安装 TLS 入口，部署方须先准备可信内部入口并从 Billing 验证可达。请求使用 Billing 的投影 workload 身份，不跟随重定向；Token receiver 仅授予准确 Billing subject 的 `internal.token.hold-expiry.prove`，且另核对 service 身份。此前没有配置该端点的波次仍登记人工复核，不自动释放。Billing 的前向 900009 和 Token 的前向 900002 迁移必须先成功，不能修改已有迁移校验和或猜测旧记录的派发事实。Token 派发意图插入与取消在同一 usage 行锁下互斥；后续普通调用也受此数据库守卫约束。已过期预约不得通过延长到期时间重新取得派发期限；未来续租入口必须遵守该约束。

人工复核由授权数据库运维人员在 Billing 库使用只读连接查询；本轮未新增 Hold 管理页面，不以原有页面的计数代替该恢复队列：

```sql
BEGIN READ ONLY;
SELECT h.id AS hold_id, h.usage_request_id, h.wallet_id,
       h.max_microcredits, h.expires_at, h.status AS financial_status,
       r.state AS recovery_state, r.proof, r.next_attempt_at, r.updated_at
FROM usage_hold_expiry_recovery r
JOIN usage_holds h ON h.id = r.hold_id
WHERE r.state = 'MANUAL_REVIEW'
ORDER BY r.next_attempt_at, h.id
LIMIT 100;
SELECT e.hold_id, e.decision, e.proof, e.occurred_at
FROM usage_hold_expiry_evidence e
ORDER BY e.occurred_at DESC, e.id DESC
LIMIT 100;
COMMIT;
```

按原 hold/usage 追踪证明与恢复回执，不直接改数据库状态、删除证据或调整余额。仍需核实真实派发事实的记录继续保留资金；已有可信取消证明的丢回执恢复由 worker 按原身份重试闭环，不能重新发起模型调用。

## NewAPI 受控 TLS

首次使用 NewAPI 发现或验密时，按 [NewAPI 受控 TLS 入口](README.newapi.md) 投放 `newapi.yaml.template` 的同 Pod Deployment、仅 8443 Service 和最小 CIDR NetworkPolicy，使用同源码 `SERVICE=newapi-tls` 镜像及当前 infrastructure PKI 的 `newapi/server.crt`、`server.key`/combined CA。共享 Compose backend 的 HTTP 容器不能替代该隔离；普通外部 HTTPS origin 沿现 allowlist/CA 合同继续支持。

## 审批资源和 workload 身份

当前 workload realm emitter 发行原五个身份以及 credential Portal、Token executor 两个独立身份。七个 agent 的 bootstrap、sink 与应用挂载一一对应；receiver rules 使用所交付 Account/Billing/Token 三份有限模板。Credential Portal 只拥有 check，executor 只拥有 execution begin/complete 与 authorization consume，不能借 Portal/Gateway 私密身份执行审批。

离线 `approval-resources render` 读取独立批准的 public identity SHA 生成既有固定六 descriptor manifest/authorization.conf；v2 预留公钥及 renderer ACL 保持原合同。离线输出不联系 broker、不创建 consumer，不证明已安装、ready 或授权激活。它发行 Token executor NKey 和 resource operator 的精确三 durable CREATE/INFO 权限，不发行 Token 普通 outbox producer。`models` 的 live activate/resume 必须核验三条 Billing 加三条 Token；先核验并装载实际 NATS TLS 配置及共享 stream，保留原 Billing Q+1 初始化与独立回执。

使用 approval-operations profile 显式运行 `approval-resources token-consumers initialize`，将三条 Token DeliverAll durable 预置；检查用 `token-consumers check`。两操作都要求实际 public identities/manifest 的独立 SHA、operator 私密 NKey、准确 TLS CA 和已准备的 TOKEN_CONSUMER_PUBLIC_RECEIPT_DIRECTORY。每次回执路径须为空且独立于 Billing receipt/manifest/identity/私密目录；不覆盖已有文件，check 如需另一次证明也使用新的独立路径。运行前可用 Compose 单次环境覆盖 TOKEN_CONSUMER_RESOURCE_RECEIPT_FILE 的新文件名。operator 不更改 Account 控制行、Billing cutover 或金融历史。

拒绝错误配置和已有 durable 漂移，不在 executor 启动时 Create/Update/Delete 资源。Token executor 仅订阅自身三 durable 并 ACK，不能消费 Billing 队列或发布 dead-letter。将 operator 回执与实际 readback 保存为不可变发行证明，核验后才启动 ACTIVE 治理执行；回执存在本身不代替审批授权、真实 effect/result 或域事务成功。

## 启动与限制

按数据库迁移成功、独立 agent sink 可用、各服务真实 health/readiness 的依赖启动。Billing 模型定价投影通过其实际本地定价库读取路径启用，未添加虚构的跨服务目录查询。Portal 使用 Token 管理 API；Account 用实际 Token snapshot/proof/effect/result，并保留现有 Billing 三命令。public edge 不直接开放 Token 管理或内部 workload 路径。

当前源码、受控 TLS/HTTP、真实公开 PG schema 与独有 loopback NATS 证明分别记录其输入及限制。未运行真实 Compose/NGINX/OpenBao/Keycloak 发行或完整 E2E；目标系统的 ACL、TLS/DNS、端口隔离、部署审批、required CI 和生产恢复验证仍须实际完成，不能从单测或静态模板推断已通过。

### F04 model and Token page overviews

The model deployment overlay enables the two existing Portal overview flags and
requires a reviewed Portal image built from the same F04 source and generated
assets. Model catalog and route-health pages read the installed page projections;
the Provisioning page reads the provisioning block. Token-only authority uses
`TOKEN_ONLY` without a window; dual Token/usage authority uses the existing
`window=24h` and strictly validates `TOKEN_AND_USAGE`, without presenting usage
or reconciliation trends in this phase. Account/Token permission checks remain
authoritative. Disabled flags do not mount these readers. Invalid, unavailable,
or unsafe-integer counts do not become zero or rounded displayed counts.

The legacy aggregate operations overview and usage/reconciliation trends belong
to the first usage phase F05 and are not forwarded by this F04 Portal whitelist.


Account also requires `OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64` in its existing
private environment file (`ACCOUNT_SERVICE_PRIVATE_ENV_FILE`). Use an independently
generated random key encoded with standard Base64, decoding to exactly 32 bytes.
Do not reuse the Token pagination/proxy key or write secret values into Compose
templates, examples, logs, or expanded `docker compose config` output. Missing,
malformed, or incorrectly sized keys prevent Account startup with
`ACCOUNT_OWNER_CURSOR_KEY_INVALID`; verify private secret injection before rollout.

Metering-unit deactivation keeps the existing empty-body API. Send a stable
`Idempotency-Key` to recover the original actor-scoped receipt after a lost
response. Legacy callers without the header still work, with a generated key
for that request only; this does not provide cross-request receipt recovery.
The ACTIVE-to-INACTIVE update, before/after audit and receipt commit together.
Repeated deactivation of an inactive unit preserves its deactivation time and
does not append another mutation audit. Apply the new forward migration
`900010_metering_deactivation_receipts` before running the updated Billing service;
do not rewrite already applied migration checksums.

Portal page queries use the existing Token HMAC-SHA256 source protocol. Set
`TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64` in both the existing private
Portal environment file (`TOKEN_PORTAL_PRIVATE_ENV_FILE`) and Token environment
file (`TOKEN_GATEWAY_PRIVATE_ENV_FILE`) to the same externally managed key,
with at least 32 decoded bytes. Never put its value into this template, browser
configuration or logs. The Portal loader rejects malformed/short configured
keys and an enabled page-overview flag without a key; disabled flags retain the
existing key-absent startup behavior. Token keeps its current paginationKey,
`WithAdminProxyHMACKey` verifier and private/loopback immediate-peer requirement.
The Portal derives the source using its existing exact trusted-edge policy and
signs the final method, upstream RequestURI (including query) and canonical IP;
browser source/signature headers are not forwarded as authority. Actual remote
deployment and secret injection are not exercised by the finite HTTP proof.

### Original Account command-policy rollback limits

The public Account rollback wrapper rejects original migrations 024, 025, 026
and the previously guarded 033 before executing their Down SQL. Original 018's
statement-level immutable-policy trigger already rejects those DELETE statements,
even when no policy matches. The invariant check runs in the same transaction
as the original rollback and preserves policy rows, triggers and migration history.
Historical SQL versions, filenames and checksums remain unchanged.

This guard does not prohibit all policy rollbacks. Original 041 and 044 retain
their existing behavior; 044 also checks references from approval requests.
The isolated numeric-history tests are not deployment acceptance or permission
to erase retained history. They include an empty-history original 031 positive
control, and do not bypass the current high-numbered forward-only migrations.
