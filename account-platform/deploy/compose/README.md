# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；Account、Billing、Token Gateway、Payment Simulator、Token Portal、New API 和 Identity Adapter 分别绑定 `18101` 至 `18107` 的对应回环端口。
4. 真实 New API 通过 `real-newapi` profile 启动，仅绑定 `http://127.0.0.1:18106`；首次启动后需在本机完成管理员、上游 Channel 和最小权限 API Token 初始化，具体见 [`../runbooks/newapi-deployment.md`](../runbooks/newapi-deployment.md)。

站点专用覆盖示例与单机部署注意事项见 [部署规划](../../docs/deployment.md)。基础 Compose 及 remote 覆盖不构成高可用生产或真实供应商验收；Payment Simulator 不得承接生产流量。

`node scripts/account-platform.mjs dev down` 与启动命令对称启用 `real-newapi` profile，关闭开发项目的对应工作负载，不删除数据卷；不应用于生产或其他 Compose project。

## 平台管理台

`real-newapi` profile 的 New API 同时连接私有 `backend` 和可出站的 `frontend` 桥，仅在宿主机回环地址发布 `18106`，以支持本地初始化与上游模型连接。数据库、Redis、NATS 仍仅连接私有网络；这不构成真实模型或生产网络验收。

平台管理台位于 Token Portal 的 `/admin.html`，复用 Keycloak 登录；实际业务授权由 Account 平台角色和原子权限码决定，Keycloak Realm Role 不再授予后台权限。管理台提供 Account、Billing、Token Gateway、系统运行与审计等受保护的运营功能。供应商凭据只展示状态、指纹和更新时间等元数据，真实密钥不会返回浏览器。

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

开启 `MOBILE_AUTH_ENABLED=true` 前，明确选择 `SMS_PROVIDER=http`、`aliyun`/`aliyun-managed` 或 `http-generated`。阿里云适配器使用号码认证服务 AccessKey、签名及各用途的专属模板，由供应商生成验证码并通过 `CheckSmsVerifyCode` 核验；换绑手机号和重置密码分别配置 `ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE`、`ALIYUN_SMS_PASSWORD_RESET_TEMPLATE`，不能回退登录或注册模板。既有 `http` 适配器使用下文的 `/send` 和 `/verify` 契约。这些供应商托管模式下 Account 不生成验证码；`http-generated` 则临时生成六位验证码并发送，仅持久化带独立密钥的 HMAC 摘要。所有模式均不持久化或记录明文验证码，密钥仅放在部署环境的 Secret 中。

Commerce Customer 开通与手机认证相互独立。`COMMERCE_PROVIDER_ID` 和 `COMMERCE_ADAPTER_VERSION` 冻结任务所用 Provider/Adapter 身份；配置当前 H3C Adapter 的 `H3C_MALL_BASE_URL`、`H3C_MALL_APP_KEY` 和 `H3C_MALL_APP_SECRET` 后，Account Service 启动供应商中立同步 worker。未配置 Adapter 时注册、登录和模型调用仍可用，Mapping 保持待处理，只有依赖它的支付被关闭。Billing 从 `payment_product_mappings` 读取商品与 Provider 版本对应的 SKU、金额、币种和渠道，订单保存不可变映射快照；正式订单必须使用通过准入的 `FROZEN` 映射，不能把测试商品或旧套餐字段当作正式商品配置。

开发 Compose 默认 `BILLING_PAYMENT_PROVIDER=simulator`，其能力明确标记 `TestOnly`，不能用于正式收款；开发和测试环境允许测试商品映射。`BILLING_ALLOW_TENTATIVE_PRICES` 默认关闭，用于控制 `h3c_mall` 分支的测试映射准入，不控制模拟器分支。用户已确认 staging/生产可以启动 Billing，`TestOnly` Provider 仍禁止正式收款；正式收款必须通过供应商能力和商品映射门禁。仅选择 `h3c_mall` 才连接企业商城；默认配置不代表生产准入。

已有 Keycloak 数据卷升级后，先使用包含手机 Authenticator SPI 的当前 Keycloak 镜像，再执行可重复运行的初始化脚本安装手机直登 Flow、服务账号权限和手机号 Token Claim；初始化脚本不编译或安装扩展 JAR：

```sh
docker compose --env-file account-platform/deploy/compose/.env \
  -f account-platform/deploy/compose/compose.yaml exec keycloak \
  sh /opt/keycloak/data/import/provision-mobile-auth.sh
```

该脚本还会将 Keycloak 用户资料规则调整为适配手机注册：邮箱、姓、名保持可选，手机号及其验证状态仅允许账号服务写入。脚本可重复执行，并会同步更新既有 Realm，避免新账号已创建却因资料不完整而无法在应用内登录。

设置 `MOBILE_AUTH_ENABLED=true` 后，可用 `SMS_PROVIDER=http` 接入供应商中立的短信服务（配置 HTTPS `SMS_HTTP_ENDPOINT` 和随机 `SMS_HTTP_SERVICE_TOKEN`），也可显式配置阿里云适配器。通用服务的 `/send` 接收手机号、用途和挑战 ID 并返回 `message_id`；`/verify` 接收手机号、挑战 ID 和验证码并返回布尔 `verified`。该通用协议没有权威的未接受响应，失败或发送结果不确定时保留短信冷却；阿里云仅完整返回官方明确的前置拒绝时解除冷却，网络中断、超时和无效响应均不代表未发送。验证码、手机号和服务凭据不得写入日志。社区注册、钱包激活和开发支付模拟器不依赖商城。企业商城支付要求已验证且同步的手机号；二维码只使用商城返回值，查询到权威 `PAID` 才记入权益。

当前商城合同没有关闭订单、退款、支付回调和对账单接口，因此这些能力不会伪造：订单关闭跟随商城查询到的 `PAY_CLOSED`，退款、回调和渠道账单明确返回“供应商能力不可用”，对账模式显示为 `QUERY_DEGRADED`。普通未支付订单在72小时最终查询后进入唯一 P2 晚到复核案件，前7天每日、之后每周人工查询；若超过最终归档期限仍无法取得成功的权威未支付查询，则升级 `PROVIDER_BLIND` 并停用该 Provider 版本新单。没有有限晚到窗口、长期历史查询、权威终态和足够保留期时不得把商城 `TEST_ONLY` 映射升级为正式 `FROZEN` 映射；查询快照不能冒充渠道账单。上线退款前仍需补充商城退款合同。

确定性集成烟测使用独立 project：`node scripts/account-platform.mjs test integration`。命令会启动完整栈、验证五个 Readiness、请求/Trace 关联头和 Portal 安全头，然后仅删除该测试 project 的卷。

数据库、Redis、NATS 和服务间网络不对公网开放。Compose 是开发/测试拓扑，不是生产部署模板。Payment Simulator 不得承接生产收款流量。

数据存储和迁移容器仅连接隔离的 `backend` 网络；Keycloak、四个 API 服务与 Portal 同时连接 `frontend` 桥接网络，入口仅绑定 `127.0.0.1`。这些服务可通过 `frontend` 出站联网；不要将测试密钥或开发环境用于生产。Keycloak 就绪检查通过后，统一启动命令才会成功返回。

## 身份接线

浏览器与服务严格使用同一公开 issuer：`http://127.0.0.1:18080/realms/pinvou`。Compose 的 Keycloak hostname 固定为该入口；Account 和 Token 通过 `OIDC_JWKS_URL` 在内部网络获取密钥，仍严格验证公开 issuer 和 audience。独立部署未设置 `OIDC_JWKS_URL` 时沿用 issuer 的标准 certs 地址；生产环境的 issuer 和 JWKS 地址均须使用 HTTPS。

Portal 使用 `PORTAL_OIDC_ISSUER` 验证登录 ID Token 的公开 issuer，以 `PORTAL_OIDC_JWKS_URL` 在内部取钥，并校验 audience、有效期、身份和登录 nonce。登录授权入口是公开 Keycloak 地址，服务端令牌交换使用内部地址；生产环境这些 OIDC 地址均须使用 HTTPS。

## 受控账号与结算验收

`test/integration/auth-business-acceptance.mjs` 使用真实 Keycloak 网页登录和
Authorization Code + PKCE S256，不启用 password grant。仅向本地回环测试入口
发送请求；测试用户、凭据由隔离环境准备并通过进程环境提供，不打印或保存令牌。
该用例保留账号、组织上下文、Token 管理、模拟充值回调及真实 Billing/Token
结算完整闭环，并核对最终 COMMITTED 状态和钱包精确扣款。

运行前必须在自有测试 Billing 数据库准备经审阅的 `pinvou-mock` 正价格 fixture，
并保留价格版本供账本追溯；产品没有默认模型价格。价格缺失或结算未收尾均为
失败，不作为跳过或验收通过。模型输出和价格属于明确测试 fixture，模拟支付
不证明真实商户、真实模型或生产定价通过；生产发布仍须完成对应真实外部验收。
