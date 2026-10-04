# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；四个服务端口为 `18101` 至 `18104`，账号 Portal 为 `http://127.0.0.1:18105/account.html`。

数据库、Redis、NATS 和服务间网络不对公网开放。Compose 是开发/测试拓扑，不是生产部署模板。Payment Simulator 不得进入生产环境。

数据存储和迁移容器仅连接隔离的 `backend` 网络；Keycloak、四个 API 服务与 Portal 同时连接 `frontend` 桥接网络，入口仅绑定 `127.0.0.1`。这些服务可通过 `frontend` 出站联网；不要将测试密钥或开发环境用于生产。Keycloak 就绪检查通过后，统一启动命令才会成功返回。

## 身份接线

浏览器与服务严格使用同一公开 issuer：`http://127.0.0.1:18080/realms/pinvou`。Compose 的 Keycloak hostname 固定为该入口；Account 和 Token 通过 `OIDC_JWKS_URL` 在内部网络获取密钥，仍严格验证公开 issuer 和 audience。独立部署未设置 `OIDC_JWKS_URL` 时沿用 issuer 的标准 certs 地址；生产环境的 issuer 和 JWKS 地址均须使用 HTTPS。

Portal 使用 `PORTAL_OIDC_ISSUER` 验证登录 ID Token 的公开 issuer，以 `PORTAL_OIDC_JWKS_URL` 在内部取钥，并校验 audience、有效期、身份和登录 nonce。登录授权入口是公开 Keycloak 地址，服务端令牌交换使用内部地址；生产环境这些 OIDC 地址均须使用 HTTPS。
