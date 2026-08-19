# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；Account、Billing、Token Gateway、Payment Simulator 和 Token Portal 端口依次为 `18101` 至 `18105`。
4. 真实 New API 通过 `real-newapi` profile 启动，仅绑定 `http://127.0.0.1:18106`；首次启动后需在本机完成管理员、上游 Channel 和最小权限 API Token 初始化，具体见 [`../runbooks/newapi-deployment.md`](../runbooks/newapi-deployment.md)。

## 平台管理台

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
