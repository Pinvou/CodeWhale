# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；四个服务端口为 `18101` 至 `18104`，账号 Portal 为 `http://127.0.0.1:18105/account.html`。

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
