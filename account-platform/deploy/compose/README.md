# 本地 Compose 环境

GSA-07 专用审批授权 Relay 的独立凭据、专项就绪与迁移顺序见[发布约束](../runbooks/approval-authorization-relay.md)；普通容器健康不能替代授权专项验收，当前扩展 NOT_DEPLOYED。

本目录的 Compose 基线用于开发、测试和单机部署编排；能够启动这些容器不代表能够从空白主机恢复当前账号平台。完整恢复还需要同一恢复集中的四个 PostgreSQL 数据库、NATS JetStream、独立模型网关、站点 Secret/override、边缘代理/证书/网络配置和不可变制品。生产或灾难恢复必须执行 [`../runbooks/full-redeployment.md`](../runbooks/full-redeployment.md)，不得把新建空卷后的健康检查当作恢复成功。

1. 将 `.env.example` 复制为 `.env`，按各字段契约填写：秘密使用独立随机值，身份/provider 等使用真实配置；两个 `TOKEN_GOVERNANCE_*_SET_DIGEST` 必须按下节取得同源 registry 摘要，不能随机生成。`.env` 已被仓库忽略。
2. 按[开发 NATS 材料与初始化说明](../runbooks/development-nats.md)准备独立 TLS/身份、当前 consumer 制品及真实 fresh 回执，并在进程环境声明 `PINVOU_DEV_NATS_CONFIG_FILE`；已有数据不得自动重新初始化。随后从仓库根执行 `node scripts/account-platform.mjs dev up`，入口固定追加 `compose.dev-nats.yaml`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；Account、Billing、Token Gateway、Payment Simulator、Token Portal、New API 和 Identity Adapter 分别绑定 `18101` 至 `18107` 的对应回环端口。
4. 真实 New API 通过 `real-newapi` profile 启动，仅绑定 `http://127.0.0.1:18106`；首次启动后需在本机完成管理员、上游 Channel 和最小权限 API Token 初始化，具体见 [`../runbooks/newapi-deployment.md`](../runbooks/newapi-deployment.md)。

`AUDIT_EXPORT_MASTER_KEY_BASE64` 必须使用32个独立随机字节的标准 Base64 编码，并与注册、认证、支付和模型密钥隔离。它只用于本地受控审计导出任务的文件密钥包裹；不得提交真实值，丢失或无计划轮换会使尚未生成或下载的任务无法恢复。文件解密和验签密钥只通过创建响应或同一幂等请求的恢复响应返回，不通过普通状态查询返回。

启用手机认证或远程密码登录前，必须同时配置 `MOBILE_AUTH_ENABLED=true`、独立的 `AUTH_RISK_HMAC_KEY_BASE64` 和 Redis 地址。远程 Nginx 将精确 Token 路径转发到 Identity 风险门禁；门禁未就绪时登录失败关闭，不得把该路径临时改回 Keycloak 直连。

## Account 治理摘要输入

开发基线保留默认 `models`；Account 在构造 Token 治理 reader 时即要求
`TOKEN_GOVERNANCE_POLICY_SET_DIGEST` 和 `TOKEN_GOVERNANCE_HANDLER_SET_DIGEST`
两个64位小写十六进制摘要，PRE_ACTIVATION 或关闭可选执行器也不免除此输入。
优先使用与本次 Account/Token 源码、镜像及构建输入逐项核验一致的既有发布计划
`candidate.policy_set_digest` / `candidate.handler_set_digest`，分别填入本机 `.env`。
已有 ACTIVE/SAFE 数据保留原卷和状态，按既有发布流程核对 installed 与 candidate，
不为启动重建数据或自动 apply。PRE 的空 installed 摘要不是 candidate 摘要。

首次开发尚无已核验计划时，可在该候选的独立构建副本中执行以下**临时离线片段**。
它不是已有产品 CLI；使用本机匹配的 Go 1.26.6 和已备齐的 module cache，从仓库根进入
`account-platform` 后在 Bash 执行。只调用实际导出函数，不连接 DB、不激活治理；缺工具或
离线依赖时应停止补齐构建前提，不填随机值或从旧版本复制值。

```sh
(
set -eu
cd account-platform
module_root="$(pwd -P)"
digest_dir="$(mktemp -d "$module_root/j12-digest.XXXXXXXX")"
cleanup_digest() {
  case "$digest_dir" in "$module_root"/j12-digest.*) ;; *) return 1 ;; esac
  # Refuse to delete unknown files; remove only this snippet and its empty directory.
  [ "$(ls -A "$digest_dir")" = main.go ] || return 1
  rm -- "$digest_dir/main.go" && rmdir -- "$digest_dir"
}
trap cleanup_digest EXIT
cat > "$digest_dir/main.go" <<'GO'
package main
import (
  "encoding/json"
  "os"
  "github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
  "github.com/Pinvou/pinvou3/account-platform/internal/token"
)
func main() {
  p, err := approvalpolicy.PolicySetDigest(); if err != nil { panic(err) }
  h, err := token.TokenCommandHandlerDigest(); if err != nil { panic(err) }
  if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
    "policy_set_digest": p, "handler_set_digest": h,
  }); err != nil { panic(err) }
}
GO
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go run -tags=nomsgpack "./${digest_dir##*/}"
)
```

保留实际源码 commit、Go版本、构建标签和输出；将两字符串分别填入 `.env` 同名变量。
它们只是当前源码集合的确定事实，不是发布批准、已安装状态或现场就绪证明。
若已有发布工件，须与 candidate 两字段逐字比对，并核源码/镜像身份；不一致走原发布核对流程。
fresh 本地开发可以使用此次真实输出，不伪造只支持 CLOUD_UAT/PRODUCTION 的发布计划。
现有 `token-governance` 没有摘要导出 flag，不要为取得输入调用 `--apply`。

按原 NATS TLS/独立身份和真实 fresh receipt 步骤准备其余依赖后，先用本机 `.env` 和
`compose.yaml` + `compose.dev-nats.yaml` 执行既有 Compose `config`，再运行原 `dev up`。
`config` 输出可能含本机秘密，只在受控目录核查，不上传或写入公开日志。
集成/e2e/fault 测试也要求操作者通过进程环境提供这两个同源值；现有输入 helper 仅检查
格式并透传，不随机生成摘要。公开 `compose-render.env` 只供 config，标记不能用于运行。
PRE_ACTIVATION 仍关闭 governed 写操作；配置可构造不表示完整栈或模型调用已就绪。

## 平台管理台

### APT-137 显式治理部署 overlay

`compose.model-token.yaml` 是受控发布 overlay，不由开发 `dev up` 自动加载。新 credential/executor 仅在 `model-token` profile 中；`token-governance-check` 独立 `governance-operations` profile、默认空参数只检查、无自动 apply。[发布说明](../runbooks/model-token-release.md) 已交付离线计划/配置校验与 G3 显式运维程序；版本/角色/迁移须按其第6节受控步骤和独立现场授权执行，不得直接创建实例；不能直接拿开发基线与新 overlay 启动来冒充 CLOUD_UAT。继承的开发 Keycloak/NATS、支付模拟器、迁移自动启动规则与站点覆盖仍须现场逐项审查，工具已交付不等于具备现场切换证据，不得未经审批用该 overlay 执行正式切换。

公开解析检查（**只 config，不 up/build/run**）：在本目录执行：

受控挂载检查只接受渲染结果中明确的布尔值 `bind.create_host_path: false`，缺字段或 `bind: {}` 均拒绝；不能手工给输出补值绕过检查。Compose 2.39.4 会省略安全的 false，5.4.0 则会省略不安全的 true，两者的空对象语义相反。请使用能够保留显式 false 的 renderer（已用 Compose 5.4.0 真实 config 验证），保持源配置的长语法 `read_only: true` 与 `bind.create_host_path: false`；旧 renderer 的歧义输出须更换 renderer 后从源文件重新渲染。配置检查仍不代表实际部署验收。

```sh
docker compose --env-file .env.example --env-file model-token.env.example \
  --env-file ../../test/fixtures/compose-render.env \
  -f compose.yaml -f compose.model-token.yaml \
  --profile model-token --profile governance-operations --profile real-newapi \
  config --format json > fixture-compose.json
```

三个 env 文件均为公开夹具输入。`.env.example` 的必填值刻意留空，单独与 `model-token.env.example` 组合仍会失败关闭；`compose-render.env` 仅补齐明显不可用的解析标记，编码密钥也刻意无效，不能用来启动服务。原生 KMS 解析在上述三个 env 文件之后追加 `--env-file kms-native.env.example`，在两个 `-f` 文件之后追加 `-f compose.kms-native.yaml`。不要加载站点 `.env` 或让同名站点环境变量覆盖这些夹具；Windows 本地 Compose 使用 `COMPOSE_CONVERT_WINDOWS_PATHS=1` 输出供 Linux 部署 checker 核验的挂载路径。把输出留在受控临时目录，运行仓库根目录的 `node scripts/account-platform-model-token-deployment-check.mjs --compose-render <文件>`，只得到 DEPLOYMENT_CONFIG_VALID/ENV_PENDING，不代表网络、凭据或权限实测。正式 URL、令牌目录、数据库凭据及独立可信摘要不得写回任何公开夹具。

`NATS_PUBLIC_AUTHORIZATION_DIRECTORY` 指向独立的只读公开授权制品目录，不能与 `NATS_CREDENTIAL_DIRECTORY` 的秘密身份目录共用；`APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256` 必须来自独立可信渠道，绑定 current-v2 制品，而不是对任意本地文件自算后当作信任依据。`model-token.env.example` 中的不存在目录和全零摘要仅让 config 解析完整，不提供授权就绪证据；正式配置与投递要求见[审批授权 Relay 手册](../runbooks/approval-authorization-relay.md)。

从仓库根执行 `PINVOU_TEST_KMS_COMPOSE=1 node --test account-platform/test/contract/kms-compose.test.mjs` 可真实解析 remote/native 两种组合，并验证缺必填值仍被拒绝；仅运行 config，不需要 Docker daemon，也不启动容器。默认使用 `docker compose`；可用 `PINVOU_TEST_KMS_COMPOSE_BINARY` 指定已审核的独立 Compose 二进制。测试主动隔离站点环境变量；未显式启用时该用例是跳过，不算真实解析通过。

外部可信签发器必须为每个 `pinvou:service:<service>` 主体提供独立、短期、可轮换的 `token` 文件；每服务只挂自己的目录且禁止 Docker 自动创建不存在的目录。Account/Identity/Billing/Token 接收端使用 overlay 的精确 rules，Keycloak 使用既有 `PINVOU_WORKLOAD_IDENTITY_TOKEN_FILE`；清空共享开发静态 Token，不自建或伪造签发器。三服务、Account/Billing 两个普通 Relay 和 Token executor 共用唯一 `MODEL_TOKEN_NATS_URL`，各服务从 `NATS_CREDENTIAL_DIRECTORY/<service>/client.creds` 取最小 NATS 凭据；现场必须证明同一集群/JetStream 历史与 ACL/轮换，不能仅启动一个新总线替代旧审批事件。

本代 gateway 使用 `TOKEN_GATEWAY_RELEASE_DATABASE_URL`，credential 使用不同本代 `TOKEN_CREDENTIAL_RELEASE_DATABASE_URL`；executor 不获得数据库 URL。当前仅部署 Account/Billing 两个普通 Relay，不配置或重启普通 Token Relay。历史 Token Relay 若曾复用旧 gateway 写身份，仍属于退休 writer 盘点；旧角色 NOLOGIN/fence 和存量会话清退必须覆盖这些旧身份及后台实例，不得因当前清单移除而遗漏或复活它们。credential 默认只绑定回环 `18108`（`CREDENTIAL_PORTAL_BIND_PORT` 可覆盖，发布前检测占用），外部冻结 origin 不变。专用控制网络隔离新服务与旧 backend；显式 egress 网络不是防火墙，OIDC/KMS/Provider/NATS/观测出站须由站点网络/主机规则限制并留证。内部 New API 通过单独 credential-provider 网络到达，不能因此开放任意 provider host。

Account control/授权Outbox 与 Token 账户配额迁移分别需要独立的 `ACCOUNT_MIGRATION_DB_PASSWORD`、`ACCOUNT_APPROVAL_WRITER_DB_PASSWORD`、`ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD`、`ACCOUNT_APPROVAL_RELAY_DB_PASSWORD`、`BILLING_MIGRATION_DB_PASSWORD`、`TOKEN_MIGRATION_DB_PASSWORD` 和 `TOKEN_POLICY_DB_PASSWORD`；不得复用任何服务运行密码，也不得互相复用。Account长期服务仅注入writer DSN，backfill凭据只交给独立受控命令，relay秘密只用于Account event-relay的专用DSN，不得交给Account service、writer或backfill。具体探针、顺序和失败关闭见[授权writer/backfill运行手册](../runbooks/approval-authorization-writer.md)及[授权Relay手册](../runbooks/approval-authorization-relay.md)。部署前置检查在已有数据卷上同样执行，发现复用即阻止迁移；迁移和backfill秘密不得注入业务服务。新卷上 Account 与 Billing 迁移在凭据检查及 PostgreSQL 健康后可并行，Token 迁移明确等待 Billing 完成，配额 bootstrap 再等待 Token 完成；Account 不是该 Billing → Token 链的前置。已有数据库不会重新执行初始化脚本，升级前必须由 DBA 通过受控 Secret 渠道创建 `account_migrator`、`account_approval_control_owner`、`account_approval_authorization_owner`、`account_approval_writer`、`account_approval_backfill`与`account_approval_relay`，校验成员关系、低权属性和独立密码，并仅按迁移合同向 Account 迁移身份开放受控权限；不能仅修改 `.env`。

数据库和 `public` schema 分别交由 `token_schema_owner NOLOGIN`、`billing_schema_owner NOLOGIN` 持有，只有对应 migrator 拥有其成员资格；运行账户和策略操作员不得持有数据库/schema 或继承这些所有者。既有业务表所有权保持不变。首次安装由数据库管理员预装 Token 的 `pgcrypto`，旧迁移阶段临时授予运行账户 schema CREATE，再由 Token `000029` 和 Billing `900003` runtime ownership hardening 收回；Billing 迁移钩子会在后续受保护迁移后重新加固。当前钱包回执由完整 Billing 迁移链中的 `900023_token_account_wallet_receipts` 安装，新库不补写原 `000053`；只有精确匹配已保留原053历史的旧库才可保留其对象与 owner。`900023` 即使空表也只允许向前修复，不能自动 down，详见[钱包回执合同](../../docs/token-wallet-receipts.md)。已有数据卷必须在停写窗口由 DBA 完成同样的所有权调整与权限核验，不能仅更新初始化脚本。完整停写、迁移和模式身份说明见 [Helm 部署说明](../helm/account-platform/README.md)。

平台管理台位于 Token Portal 的 `/admin.html`，复用 Keycloak 登录；实际业务授权由 Account 平台角色和原子权限码决定，Keycloak Realm Role 不再授予后台权限。管理台提供 Account、Billing、Token Gateway、系统运行与审计等受保护的运营功能。审计导出只创建服务端任务，不在浏览器拼接当前列表；文件密钥只在创建响应展示一次，PII 导出必须使用带原因和到期时间的临时授权。供应商凭据只展示状态、指纹和更新时间等元数据，真实密钥不会返回浏览器。

远程部署只向公网开放 Keycloak 的 `/realms/` 与 `/resources/` 登录所需路径，管理控制台 `/admin/` 必须通过受限网络或本地隧道访问，具体见 [`../runbooks/keycloak-admin-access.md`](../runbooks/keycloak-admin-access.md)。

首次部署或已有 Keycloak 数据卷升级后，在本机通过环境变量提供平台管理员账号和密码，再运行初始化脚本。密码不得写入仓库中的 `.env.example`、命令历史或部署日志：

```sh
docker compose --env-file account-platform/deploy/compose/.env \
  -f account-platform/deploy/compose/compose.yaml exec \
  -e PINVOU_ADMIN_USERNAME -e PINVOU_ADMIN_PASSWORD keycloak \
  sh /opt/keycloak/data/import/provision-platform-admin.sh
```

部署时应将 `provision-platform-admin.sh` 以只读方式挂载到容器，或复制脚本内容到受控的运维环境执行。该脚本只创建登录身份和迁移期 Realm Role，不授予 Account 业务权限；升级时必须按 [`../runbooks/account-rbac-migration.md`](../runbooks/account-rbac-migration.md) 将经审批主体明确映射到具体平台角色。

## 手机注册、登录与商城支付

开启 `MOBILE_AUTH_ENABLED=true` 前，需要配置阿里云号码认证服务 AccessKey、系统赠送签名和系统赠送模板（登录/注册模板 CODE 为 `100001`）。验证码由阿里云动态生成，并由 Account Service 调用 `CheckSmsVerifyCode` 核验；服务端不生成、保存或记录明文验证码。所有密钥仅放在部署环境的 Secret 中。

Commerce Customer 开通与手机认证相互独立。`COMMERCE_PROVIDER_ID` 和 `COMMERCE_ADAPTER_VERSION` 冻结任务所用 Provider/Adapter 身份；配置当前 H3C Adapter 的 `H3C_MALL_BASE_URL`、`H3C_MALL_APP_KEY` 和 `H3C_MALL_APP_SECRET` 后，Account Service 启动供应商中立同步 worker。未配置 Adapter 时注册、登录和模型调用仍可用，Mapping 保持待处理，只有依赖它的支付被关闭。启用商城支付时，Billing Service 直接读取数据库中已启用套餐的 SKU 和暂定 UAT 价格，避免额外环境变量形成第二份商品配置。

已有 Keycloak 数据卷升级后，还需要执行一次可重复运行的初始化脚本，以安装手机直登 Flow、服务账号权限和手机号 Token Claim：

```sh
docker compose --env-file account-platform/deploy/compose/.env \
  -f account-platform/deploy/compose/compose.yaml exec keycloak \
  sh /opt/keycloak/data/import/provision-mobile-auth.sh
```

该脚本还会将 Keycloak 用户资料规则调整为适配手机注册：邮箱、姓、名保持可选，手机号及其验证状态仅允许账号服务写入。脚本可重复执行，并会同步更新既有 Realm，避免新账号已创建却因资料不完整而无法在应用内登录。

注册短信由阿里云发送；注册成功后 Account Service 会在后台为同一手机号创建或确认商城用户。商城用户同步完成前，充值接口保持关闭。支付订单和二维码完全以商城返回值为准，客户端不会自行生成二维码；商城返回 `PAID` 后 Billing 才会把积分记入钱包。

支付 Provider 回调按原生能力启用，生产公开地址固定为 `POST https://account.pinvou.com/api/v1/payment-callback/token` 与 `POST https://account.pinvou.com/api/v1/payment-callbacks`。每个 Provider 的独立 HTTP Basic 客户端凭据在服务端绑定 Provider、Adapter 版本和商户，换取 1–15 分钟的最小权限 Bearer Token；回调请求体不能提交 Provider 身份，Billing 按 Token 选择 Profile，先把通知写入 Inbox 再返回 `202`。当前注册新华三为首个 Profile。Web 个人中心使用 `PORTAL_PUBLIC_URL=https://account.pinvou.com`，浏览器入口为 `https://account.pinvou.com/`；身份服务仍由 `PUBLIC_BASE_URL` 独立配置。`account.pinvou.com` 使用独立 HTTPS 站点，HTTP 只允许跳转 HTTPS；`pinvou.com` 不再承载个人中心或支付回调路由。部署时必须为 Billing 配置 `PAYMENT_CALLBACK_CLIENT_ID`、至少32字符的 `PAYMENT_CALLBACK_CLIENT_SECRET`、至少256 bit 的 `PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64`、`PAYMENT_CALLBACK_TOKEN_KEY_ID` 和 `PAYMENT_CALLBACK_TOKEN_TTL_SECONDS`，不得把真实值写入仓库或日志。

当前商城合同仍没有主动关闭订单、退款和对账单接口，因此这些能力不会伪造：订单关闭跟随商城查询到的 `PAY_CLOSED`，退款和渠道账单明确返回“供应商能力不可用”，对账模式显示为 `QUERY_DEGRADED`。普通未支付订单在72小时最终查询后进入唯一 P2 晚到复核案件，回调漏送时继续由查询补偿；Provider 明确声明不提供权威未支付终态时，案件继续保留并接收晚到支付，但不能仅因订单超过最终归档期限就升级 `PROVIDER_BLIND` 或停用该 Provider 版本新单。只有查询持续不可用、历史订单不可访问、能力声明与实际行为不符等真实事实盲区才适用 `PROVIDER_BLIND`。没有有限晚到窗口、长期历史查询、权威终态和足够保留期时不得把商城 `TEST_ONLY` 映射升级为正式 `FROZEN` 映射；查询快照不能冒充渠道账单。UAT 的新 `TEST_ONLY` 验证证据默认有效 `720h`，可用 `BILLING_TEST_ADMISSION_EVIDENCE_TTL` 在 `1h` 至 `2160h` 范围内调整；该参数不续期既有证据，也不绕过失效后的验证和恢复审批。上线退款前仍需补充商城退款合同。

确定性集成烟测使用独立 project：`node scripts/account-platform.mjs test integration`。命令还需要上述独立 NATS 材料；先在随机新 project 通过真实 CLI/DB/NATS 生成 fresh 回执，再启动原完整栈、验证五个 Readiness、请求/Trace 关联头和 Portal 安全头，最后仅删除本次测试 project 的卷。`e2e/fault` 使用同一隔离初始化和 overlay；既有开发数据库/总线不进入自动 fresh 路径。资源初始化停止在 QUIESCING/FENCED，不代表正式审批激活。

数据库、Redis、NATS 和服务间网络不对公网开放。`compose.yaml` 是可重复的开发/测试基础拓扑；单机远程访问使用 `compose.remote.yaml` 及由部署负责人提供的不入库站点专用覆盖；恢复前置、状态核对与操作顺序见 [`../runbooks/full-redeployment.md`](../runbooks/full-redeployment.md)，站点实际配置和操作记录仍须独立取得。Kubernetes 生产目标仍以 Helm Chart 为准，不能把基础 Compose 文件单独当作高可用生产模板。Payment Simulator 不得承接生产流量。

G6 持续验证对以上公开配置执行真实 config/checker，并分别构建三个新增服务制品；只读结构检查不代替现场启停。专属 PG/原 CLI、Chrome、nginx 测试依赖变体与正式缺项见 [发布手册第9节](../runbooks/model-token-release.md#9-g6-持续门禁与环境交接)。
