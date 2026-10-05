# 真实 New API 部署与初始化

账号平台使用上游 `QuantumNous/new-api` 的官方容器作为真实模型数据面。开发 Compose 与 Helm 当前固定为 `calciumion/new-api:v1.0.0-rc.11`，并锁定多架构镜像摘要 `sha256:bd30213d808857bb569ef47d3c9209d061a66ea089c2472ef46ce51e75517f19`。升级版本前必须重新执行兼容、安全和流式结算测试，不得改用 `latest`。

New API 使用 AGPLv3，并带上游声明的附加归属要求。发布或修改前必须完成许可证、上游模型授权、内容安全、日志留存和适用监管要求复核。

## 本地 Compose

1. 从 `.env.example` 创建 `.env`，为 `NEWAPI_DB_PASSWORD`、`NEWAPI_SESSION_SECRET` 和 `NEWAPI_CRYPTO_SECRET` 分别生成不同的随机值。
2. 执行 `node scripts/account-platform.mjs dev up`。该命令显式启用 `real-newapi` profile；普通 Integration/Fault 测试仍只使用 Mock New API。
3. 首次使用新的 PostgreSQL 卷时，初始化脚本自动创建 `new_api` 角色和 `account_platform_newapi` 数据库。已有卷可在更新 Postgres 容器环境后执行 `docker compose --env-file account-platform/deploy/compose/.env -f account-platform/deploy/compose/compose.yaml exec postgres sh /docker-entrypoint-initdb.d/10-init-databases.sh`，脚本可重复执行。
4. 仅从本机打开 `http://127.0.0.1:18106` 完成管理员初始化、配置真实上游 Channel，并创建供 Token Gateway 使用的最小权限 API Token。
5. 创建一个仅允许平台代理模型的最小权限 New API Token，将其通过部署 Secret 注入 `TOKEN_DEFAULT_PROVIDER_API_KEY`；Provider Base URL 使用 `http://newapi:3000`。Token Gateway 会在用户第一次对话时创建平台托管的默认 Token Account 和路由，并将凭据二次加密后写入 Token 数据库。
6. 为自动用量核对创建独立的 New API 管理访问 Token，分别把 Token 和对应管理员用户数字 ID 注入 `TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN`、`TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID`。Token Gateway 会保存 New API 响应的 `X-Oneapi-Request-Id`，该凭据只用于按此 ID 精确读取 New API 日志，不用于模型调用。不得把任何实际 Token 写入 Compose、源码、日志、命令行参数或工单。

Mock New API 继续用于限流、断流和缺失 usage 故障注入；真实 New API 不用于制造这些异常。

## Kubernetes / Helm

部署前，`global.runtimeSecret` 指向的 Secret Provider 同步结果必须包含：

- `newapi-database-url`：指向独立 PostgreSQL 数据库；
- `newapi-session-secret`：所有 New API 副本一致；
- `newapi-crypto-secret`：所有 New API 副本一致，且不得与 Session Secret 相同。
- `token-default-provider-api-key`：供 Token Gateway 自动创建默认路由的最小权限 New API Token。
- `token-reconciliation-newapi-admin-token`：供 Token Gateway 按请求 ID 查询 New API 用量日志的独立管理访问 Token；
- `token-reconciliation-newapi-admin-user-id`：上述管理 Token 所属的 New API 数字用户 ID。

Helm 不创建内联 Secret。生产 New API 没有公网 Ingress，Service 名为 `newapi`，只有同部署 Token Gateway Pod 可以访问其 HTTPS 8443 数据面。New API 只能访问 DNS、内部 Redis、批准的 PostgreSQL CIDR 和模型供应商 CIDR。

首次初始化使用受审计的管理员终端：

```sh
kubectl -n <namespace> port-forward --address 127.0.0.1 deployment/<release>-account-platform-new-api 13000:8080
```

将 Deployment 名替换为实际渲染名称；这里使用获授权的 Kubernetes Pod 隧道访问同 Pod 内 HTTP 管理面，不能经只提供受限数据路由的 HTTPS Service 初始化。随后仅从本机访问 `http://127.0.0.1:13000` 创建强管理员账号，配置真实上游 Channel，并创建最小权限 API Token，完成后关闭隧道。不要在命令行参数、工单或聊天记录中传递管理员密码和 API Token。

Token Gateway 的 Provider Base URL 和用量补偿 Base URL 均使用 `https://newapi:8443`。固定版本的 New API 只提供 HTTP 入站，因此 chart 在同 Pod 中部署本项目 `newapi-tls` 前置，并固定代理到 `http://127.0.0.1:8080`。跨 Pod 的 Service 仅暴露 HTTPS 8443；NetworkPolicy 仅允许同部署 Token Gateway 进入该端口，不开放 New API 的 HTTP 8080 或公共 Ingress。

安装前建立独立 TLS Secret（`tls.crt`、`tls.key`）和 CA Secret（`ca.crt`），填入 `newApi.tls.serverSecretName`、`newApi.tls.caSecretName`。服务端证书须覆盖 `newApi.serviceName`（默认 `newapi`）的 DNS SAN，私钥仅挂到前置；Gateway 只挂 CA，并通过 `SSL_CERT_DIR` 保留系统 CA 同时信任该 CA。缺 Secret/键阻止 Pod 启动，缺失或无效证书令前置退出，错误 CA、SAN 或过期证书会被 Gateway 拒绝；不设置 `InsecureSkipVerify`。证书轮换后滚动重启前置和 Gateway，以载入新证书及 CA。

前置只代理精确的 `POST /v1/chat/completions`、`GET /v1/models` 和 `GET /api/log/`，保留补偿查询参数和 SSE flush/cancel；`/api/log/self`、管理面及其他路径不经该入口开放。管理面配置继续使用经授权的运维访问，不新增公共入口。`newapi-tls` 使用现有 Dockerfile 的 `SERVICE=newapi-tls` 构建，不依赖第三方 TLS 代理镜像。完成实际 CA/SAN 验证、`/v1/models`、JSON、SSE、usage 结算和客户端断开验证前，不得执行 DNS/API Gateway 切换。

## 验收与回滚

- `/api/status` 返回成功；
- Token Gateway 可以用加密凭据读取 `/v1/models`；
- JSON 与 SSE 请求只调用一次真实 New API；
- usage 明确时正确 Commit；断流或 usage 不确定时先进入 Review，后台任务随后按 New API 消费日志自动补结算；单渠道鉴权、模型不存在或额度错误日志不能证明整个逻辑请求最终未消费，因此不据此自动释放 Hold。代理已持久化的明确释放意图可以重试原释放操作或补齐已确认释放的本地终态；24 小时仍无确定证据的请求停止自动处理并保留人工复核；
- New API 管理面没有公网入口，其他账号平台 Pod 不能直接访问数据面；
- PostgreSQL、Redis 和 New API 镜像均有备份/回滚记录。

回滚时只切回旧模型入口，不把新账号、余额、Token 或 New API 数据写回旧系统。Billing 继续收敛已经发生的支付和用量结算。
