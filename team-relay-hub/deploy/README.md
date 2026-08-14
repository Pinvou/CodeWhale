# Team Relay 演示部署模板

本目录只提供不含真实主机、域名和凭据的模板。执行任何远端部署前，必须重新回读目标主机、现有 Nginx 配置、端口占用、磁盘和可用内存，并单独取得部署授权。

## 本地验证模板

从仓库根目录执行：

```bash
cp team-relay-hub/deploy/environment.example team-relay-hub/deploy/.env
# 替换数据库密码，并为 TEAM_RELAY_JOIN_CODE_ENCRYPTION_KEY 生成一次 32 字节 base64url 密钥：
openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'
# 把输出安全写入 .env，单独备份；后续升级不得重新生成。
docker compose --env-file team-relay-hub/deploy/.env \
  -f team-relay-hub/deploy/compose.yaml config
docker compose --env-file team-relay-hub/deploy/.env \
  -f team-relay-hub/deploy/compose.yaml build
```

启动后，Hub 只暴露到宿主机回环地址：

```bash
docker compose --env-file team-relay-hub/deploy/.env \
  -f team-relay-hub/deploy/compose.yaml up -d
curl --fail http://127.0.0.1:8791/pinvou3/team-relay/v1/healthz
```

创建首次 owner 邀请时，不把命令输出写入日志或工单：

```bash
docker compose --env-file team-relay-hub/deploy/.env \
  -f team-relay-hub/deploy/compose.yaml exec hub \
  npm run team:create -- --name "Team Relay Demo"
```

## HTTPS 入口

`nginx-location.conf` 需要放入已有 TLS `server` 块。上线前先执行 `nginx -t`，再 reload，并分别验证：

- 公网 `/pinvou3/team-relay/v1/healthz`；
- SSE 长连接不被缓冲；
- 现有 Remote Relay、OTA 和 collaboration 入口；
- Hub 端口未监听公网地址。

模板会用 Nginx 的 `$remote_addr` 覆盖 `X-Real-IP`，供 Hub 的加入码限流使用。只有 Hub 无法被绕过代理直接访问时才设置 `TEAM_RELAY_TRUST_PROXY=true`。

## 数据与回滚

- PostgreSQL 与附件使用独立命名卷；回滚应用时不删除数据卷。
- 升级前用 `pg_dump --format=custom` 备份数据库，并单独备份附件卷。
- 升级到协议 `0.2` 时，`0009_confirmed_transfers.sql` 与新 Hub 必须在同一维护窗口切换：先停止所有旧 Hub 实例和写入，完成数据库备份，使用待发布版本执行 `npm run db:migrate`，再启动同一版本的 Hub 并检查内网、公网健康入口。旧 Hub 在 `0009` 后继续写入会违反新动作约束，新 Hub 在 `0009` 前启动则会访问尚不存在的字段，因此 Hub 不支持跨 `0009` 的滚动升级。
- 回滚只撤销新 Nginx location、停止新 Hub 和恢复上一镜像；不要修改其他 Pinvou 服务。
- `0009` 已成功应用后，不得只回滚 Hub 镜像到旧版本；需要回滚时保持新 Hub 停止，并按已演练的数据恢复方案还原升级前备份。
- 定时执行 `npm run attachments:cleanup`，并监控数据库、附件卷和容器日志大小。
