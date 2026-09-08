# 真实 New API 部署与初始化

账号平台使用上游 `QuantumNous/new-api` 的官方容器作为真实模型数据面。开发 Compose 与 Helm 当前固定为 `calciumion/new-api:v1.0.0-rc.26`，并锁定多架构镜像摘要 `sha256:7b82c866e9c0fb02faca7d5068e9d1261e47edf52462d63d9db7b614e7f1027d`。该 RC 经本次部署负责人明确选用，并非官方稳定版；固定候选镜像不代表生产验收通过。升级版本前必须重新执行兼容、安全和流式结算测试，不得改用 `latest`。

## rc.26 来源与验收边界（2026-09-08）

- [官方发布记录](https://github.com/QuantumNous/new-api/releases/tag/v1.0.0-rc.26)对应源码提交 `8f6961c675932f406260ff0c218bc2aa0603e9b2`；镜像 OCI revision/source/version 标签与之相符。
- Docker Hub 原始 OCI index 响应重新计算 SHA-256，与上述摘要一致；`linux/amd64` 子 manifest 为 `sha256:5576e4c2f16c6f8e094268a13c90eb0ee541b374bc9681292bd963a3b384497c`，config 为 `sha256:5c67339bb8c9ff26295f8518a738ce09ce5a9f2f2633c07efab038da11395633`，两者也按原始响应复算。
- `linux/arm64` 子 manifest 为 `sha256:3b0b727823d102f898e1170c31833ffe9a5330e0ba84ed252e0f565fb3d4e0f0`；本次仅验证 amd64 运行时，不将 `unknown/unknown` 的 attestation 条目当作运行平台，也不将存在 attestation 等同于独立签名验证。
- 官方镜像没有默认非 root 用户，部署必须保留 `65532:65532`、只读根文件系统、移除 capabilities 和受限可写目录。发布说明要求 64 位系统，并新增大额度字段检查；必须实测独立 PostgreSQL 空库启动及超过 32 位上限的额度，不对旧测试库做迁移。
- 若 Docker Hub 不可达，可使用缓存仓库传输**同一摘要**的镜像；拉取后核对平台 config ID 和 OCI revision/version，部署引用不得退化成可漂移标签。仓库模板仍以官方仓库为准，不把站点镜像网络配置写入公共模板。
- 完整安全公告复核、隔离管理/Token API、JSON/SSE 用量和核对兼容性、内部 TLS 与生产依赖门禁仍是切换条件。摘要核验只证明固定了同一镜像，不能证明镜像没有漏洞。正式切换前不得开放 New API 公网入口或导入旧测试数据。

### 空库基础验收

Linux Docker 操作者先下载固定摘要的 New API 和 PostgreSQL 17 镜像，再显式运行：

```sh
python3 scripts/account-platform-newapi-fixture.py --isolated-fixture \
  --newapi-image 'calciumion/new-api@sha256:7b82c866e9c0fb02faca7d5068e9d1261e47edf52462d63d9db7b614e7f1027d' \
  --postgres-image '<docker image inspect 得到的本地 sha256 image ID>'
```

fixture 不主动下载镜像、不读取站点配置，使用独立 internal 网络及 tmpfs 数据库，不发布端口；宿主机仅连接已核验归属的临时容器地址。随机凭据通过 stdin 投递，不输出响应正文、原始错误或环境变量。退出前只删除本次创建且所有权标签匹配的容器 ID 和网络 ID，不删除镜像或其他数据。异常退出未能清理时报告 `fixture_cleanup_failed`，不得改用全局 prune。数据库在内存中，结果不可用作备份或生产部署。

`NEWAPI_FRESH_FIXTURE_PASS` 仅表示 PostgreSQL 17 空库、非 root/只读启动、一次性初始化、管理员登录、四类匿名 API 拒绝、用户 quota bigint 的 50 亿值读写、应用进程重启后初始化/额度保留这 7 项基础检查通过。不包含 Redis、数据库重启恢复、真实上游、Token/Channel 配置、流式结算、漏洞扫描或生产网络验收。离线保护测试为 `python -m unittest discover -s scripts/tests -p test_newapi_fixture.py`，不能替代该真实 fixture。

New API 使用 AGPLv3，并带上游声明的附加归属要求。发布或修改前必须完成许可证、上游模型授权、内容安全、日志留存和适用监管要求复核。

## 本地 Compose

1. 从 `.env.example` 创建 `.env`，为 `NEWAPI_DB_PASSWORD`、`NEWAPI_SESSION_SECRET` 和 `NEWAPI_CRYPTO_SECRET` 分别生成不同的随机值。
2. 执行 `node scripts/account-platform.mjs dev up`。该命令显式启用 `real-newapi` profile；普通 Integration/Fault 测试仍只使用 Mock New API。
3. 首次使用新的 PostgreSQL 卷时，初始化脚本自动创建 `new_api` 角色和 `account_platform_newapi` 数据库。已有卷可在更新 Postgres 容器环境后执行 `docker compose --env-file account-platform/deploy/compose/.env -f account-platform/deploy/compose/compose.yaml exec postgres sh /docker-entrypoint-initdb.d/10-init-databases.sh`，脚本可重复执行。
4. 仅从本机打开 `http://127.0.0.1:18106` 完成管理员初始化、配置真实上游 Channel，并创建供 Token Gateway 使用的最小权限 API Token。
5. 将 `TOKEN_GATEWAY_ADAPTER` 固定为 `NEW_API`。创建一个仅允许平台代理模型的最小权限 New API Token，将其通过部署 Secret 注入 `TOKEN_DEFAULT_PROVIDER_API_KEY`；Provider Base URL 使用 `http://newapi:3000`。Token Gateway 启动后通过 Adapter 的 `/v1/models` 自动同步独立目录，并在用户第一次对话时创建平台托管的默认 Token Account 和显式 `CHAT` 协议路由；凭据二次加密后写入 Token 数据库。目录连续两次未发现某模型才会自动弃用，禁止把单次抖动解释为下架。
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

Helm 不创建内联 Secret。生产 New API 没有公网 Ingress，Service 名为 `newapi`，只有带 Token Gateway 组件标签的 Pod 可以访问其 8080 数据面。New API 只能访问 DNS、内部 Redis、批准的 PostgreSQL CIDR 和模型供应商 CIDR。

首次初始化使用受审计的管理员终端：

```sh
kubectl -n <namespace> port-forward service/newapi 13000:8080
```

随后访问 `http://127.0.0.1:13000` 创建强管理员账号，配置真实上游 Channel，并创建最小权限 API Token。不要在命令行参数、工单或聊天记录中传递管理员密码和 API Token。

Token Gateway 中的 Provider Base URL 使用 `http://newapi:8080`。完成 `/v1/models`、JSON、SSE、usage 结算和客户端断开验证前，不得执行 DNS/API Gateway 切换。

## 验收与回滚

- `/api/status` 返回成功；
- Token Gateway 可以用加密凭据读取 `/v1/models`；
- JSON 与 SSE 请求只调用一次真实 New API；
- usage 明确时正确 Commit；断流或 usage 不确定时先进入 Review，后台任务随后按 New API 请求日志自动补结算；只有明确的鉴权、模型不存在或额度拒绝证据才自动释放 Hold，24 小时仍无确定证据的请求停止自动处理并保留人工复核；
- New API 管理面没有公网入口，其他账号平台 Pod 不能直接访问数据面；
- PostgreSQL、Redis 和 New API 镜像均有备份/回滚记录。

回滚时只切回旧模型入口，不把新账号、余额、Token 或 New API 数据写回旧系统。Billing 继续收敛已经发生的支付和用量结算。
