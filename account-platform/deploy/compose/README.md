# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；Account、Billing、Token Gateway、Payment Simulator 和 Token Portal 端口依次为 `18101` 至 `18105`。
4. 真实 New API 通过 `real-newapi` profile 启动，仅绑定 `http://127.0.0.1:18106`；首次启动后需在本机完成管理员、上游 Channel 和最小权限 API Token 初始化，具体见 [`../runbooks/newapi-deployment.md`](../runbooks/newapi-deployment.md)。

## 平台管理台

`real-newapi` profile 的 New API 同时连接私有 `backend` 和可出站的 `frontend` 桥，仅在宿主机回环地址发布 `18106`，以支持本地初始化与上游模型连接。数据库、Redis、NATS 仍仅连接私有网络；这不构成真实模型或生产网络验收。

平台管理台位于 Token Portal 的 `/admin.html`，复用 Keycloak 登录，并只接受带有 `platform-admin` Realm 角色的用户。管理台提供 Account、Billing、Token Gateway、系统运行与审计等受保护的运营功能。供应商凭据只展示状态、指纹和更新时间等元数据，真实密钥不会返回浏览器。

远程部署只向公网开放 Keycloak 的 `/realms/` 与 `/resources/` 登录所需路径，管理控制台 `/admin/` 必须通过受限网络或本地隧道访问，具体见 [`../runbooks/keycloak-admin-access.md`](../runbooks/keycloak-admin-access.md)。

首次部署或已有 Keycloak 数据卷升级后，在本机通过环境变量提供平台管理员账号和密码，再运行初始化脚本。密码不得写入仓库中的 `.env.example`、命令历史或部署日志：

```sh
docker compose --env-file account-platform/deploy/compose/.env \
  -f account-platform/deploy/compose/compose.yaml exec \
  -e PINVOU_ADMIN_USERNAME -e PINVOU_ADMIN_PASSWORD keycloak \
  sh /opt/keycloak/data/import/provision-platform-admin.sh
```

部署时应将 `provision-platform-admin.sh` 以只读方式挂载到容器，或复制脚本内容到受控的运维环境执行。脚本可重复运行，并会确保用户拥有 `platform-admin` 角色。

确定性集成烟测使用独立 project：`node scripts/account-platform.mjs test integration`。命令会启动完整栈、验证五个 Readiness、请求/Trace 关联头和 Portal 安全头，然后仅删除该测试 project 的卷。

数据库、Redis、NATS 和服务间网络不对公网开放。Compose 是开发/测试拓扑，不是生产部署模板。Payment Simulator 不得进入生产环境。

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
