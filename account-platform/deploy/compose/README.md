# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；Account、Billing、Token Gateway、Payment Simulator 和 Token Portal 端口依次为 `18101` 至 `18105`。
4. 真实 New API 通过 `real-newapi` profile 启动，仅绑定 `http://127.0.0.1:18106`；首次启动后需在本机完成管理员、上游 Channel 和最小权限 API Token 初始化，具体见 [`../runbooks/newapi-deployment.md`](../runbooks/newapi-deployment.md)。

确定性集成烟测使用独立 project：`node scripts/account-platform.mjs test integration`。命令会启动完整栈、验证五个 Readiness、请求/Trace 关联头和 Portal 安全头，然后仅删除该测试 project 的卷。

数据库、Redis、NATS 和服务间网络不对公网开放。Compose 是开发/测试拓扑，不是生产部署模板。Payment Simulator 不得进入生产环境。
