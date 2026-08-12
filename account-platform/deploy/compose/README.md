# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；四个服务端口为 `18101` 至 `18104`。

数据库、Redis、NATS 和服务间网络不对公网开放。Compose 是开发/测试拓扑，不是生产部署模板。Payment Simulator 不得进入生产环境。
