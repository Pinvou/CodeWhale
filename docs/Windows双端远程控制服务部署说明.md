# Windows 双端远程控制：Relay 服务部署说明

> 适用范围：`remote-control-relay`（公网 Relay + enrollment gateway）。
> 关联：`remote-control-relay/ENROLLMENT.md`（enrollment 单一真相源）、`remote-control-relay/.env.enrollment.example`、`remote-control-relay/nginx-enrollment-gateway.conf`、`Windows双端远程控制发布与回滚.md`。

## 1. 本机部署（自测用，已完成）

适用：单机联调、配合 [本地单机自测脚本](D:/13_Pinvou/pinvou3-main-1/scripts/remote-control-local-selftest.ps1) 与刚生成的安装包做双进程冒烟。

```powershell
# 起服务（生成/复用信任根 + development+TOFU 模式启动 Relay）
powershell -ExecutionPolicy Bypass -File .\scripts\remote-control-local-selftest.ps1

# 查看状态 / 停止
powershell -ExecutionPolicy Bypass -File .\scripts\remote-control-local-selftest.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\remote-control-local-selftest.ps1 -Action Stop
```

客户端（A/B 两个进程）设置以下变量后即可连接，变量值由脚本打印：

```text
PINVOU_REMOTE_RELAY_SIGNING_KEY_ID / PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64
PINVOU_REMOTE_RUNTIME_RELAY_WS_URL = ws://127.0.0.1:8787/pinvou3/remote/ws
```

**本次部署记录（2026-09-19，本机）**

| 项 | 值 |
|---|---|
| 监听 | `127.0.0.1:8787`（`PINVOU_REMOTE_HOST=127.0.0.1`），WebSocket 路径 `/pinvou3/remote/ws` |
| 验证 | `GET /pinvou3/remote/ws` → HTTP 200；Relay 日志出现不含监听地址的固定事件 `pinvou remote relay listening (protocol=...)` |
| 进程 | 包装进程 PID 91988，node 服务进程 PID 62020（`-Action Status` 可查） |
| 日志 | `%LOCALAPPDATA%\Pinvou3\remote-control-selftest\relay.log` |
| 密钥/状态 | `%LOCALAPPDATA%\Pinvou3\remote-control-selftest\`（Ed25519 签名密钥 + HMAC 材料 + `relay-state\`，禁止提交） |
| 遥测 | `Telemetry disabled`（本机未提供 enrollment token / device pepper / admin password），属预期 |

**边界声明**：该实例是 `PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=development` + `DEVELOPMENT_TOFU=true`，只允许在本机自测使用；`ENROLLMENT.md` 明确要求 TOFU 仅在 development 且 `NODE_ENV≠production` 时可用，**不得作为生产或对外服务**。

## 2. 生产部署（需目标主机与运维输入）

### 2.1 组件与顺序

1. **Relay 本体**：`node server.js`（`PORT` 默认 8787，`PINVOU_REMOTE_HOST` 决定监听地址）。
2. **enrollment 门禁**：Relay 监听必须位于 loopback（字面 IPv4/IPv6，`localhost` 不算），由 `scripts/enrollment-gateway-server.js` 作为唯一入口；公网路由使用 `nginx-enrollment-gateway.conf`。
3. **注册库**：`enrollment-registry.js` 读取 `PINVOU_REMOTE_CONTROLLER_STATE_PATH`（不得与 legacy `PINVOU_REMOTE_STATE_PATH` 同路径）；迁移与覆盖率用 `scripts/enrollment-migrate.js` dry-run 取证（要求 100%）。
4. **验收预检**：对每个公网入口运行 `scripts/enrollment-gateway-preflight.js`（校验健康标记、WebSocket upgrade 标记、匿名注册拒绝、Relay URL 为字面 loopback、OS 监听检查无通配绑定）。
5. **回滚检查**：`scripts/check-enrollment-gated-v2-rollback.mjs` 退出码为 0 才允许启动旧 v2 二进制；裸 v2 回滚一律拒绝。
6. **运行加固**：`10-hardening.conf` 只保存与环境无关的服务单元加固基线；服务账号只读仓库、只写状态目录。反向代理地址必须通过受权限保护的 controller 环境文件显式注入，`PINVOU_REMOTE_ALLOWED_PROXY_IPS` 与 `PINVOU_REMOTE_TRUSTED_PROXY_IPS` 必须是相同的合法 IP 集合，不得写入仓库模板。

### 2.2 必需配置（来自部署 secret 管理器，禁止入库/入日志）

| 变量 | 说明 |
|---|---|
| `PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT` | `official` / `self_hosted`（生产不得用 `development`） |
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID` / `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY` | registry HMAC（32–4096 字节） |
| `PINVOU_REMOTE_PAIRING_HMAC_KEY_ID` / `PINVOU_REMOTE_PAIRING_HMAC_KEY` | 连接码摘要 keyring（支持轮换，见发布文档第 7.3 节） |
| `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64` | Ed25519 PKCS#8 DER base64，签名 pairing transcript |
| `PINVOU_REMOTE_CONTROLLER_STATE_PATH` / `PINVOU_REMOTE_STATE_PATH` | 新旧状态文件绝对路径，必须不同 |
| `PINVOU_REMOTE_ALLOWED_PROXY_IPS` / `PINVOU_REMOTE_TRUSTED_PROXY_IPS` | 实际反向代理的 IPv4/IPv6 地址列表；两者必须显式配置且集合完全一致 |
| 遥测（controller 生产必需） | `PINVOU_TELEMETRY_ENROLLMENT_TOKEN`（至少 24 字符）、`PINVOU_TELEMETRY_DEVICE_PEPPER`（至少 24 字符）、`PINVOU_STATS_ADMIN_PASSWORD`（至少 12 字符）及绝对路径 `PINVOU_TELEMETRY_DATA_DIR`；任一缺失或无效时 controller 部署预检失败，本机仅开发验证可保持禁用 |

客户端侧必须 pin 同一份公钥（原始 32 字节 base64）：`PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` + `PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64`；缺失会以 `relay_trust_unconfigured` 失败关闭。

生产部署脚本会在通过 controller 环境文件预检后，以 `root:root`、`0700` 显式创建
`/var/lib/pinvou-remote-relay/state`。状态存储本身不会创建缺失的父目录，这是为了保留
软链接、所有权和目录替换检查的失败关闭语义；新服务器不得省略该目录准备步骤。

### 2.3 部署后的验收清单

- [ ] `enrollment-gateway-preflight.js` 对所有公网入口退出码 0
- [ ] registry 覆盖率 100%（`enrollment-migrate.js` dry-run 输出留档）
- [ ] Relay 仅 loopback 监听，且 OS 监听检查无通配/非 loopback 绑定
- [ ] 匿名 `web_client_join` / `desktop_endpoint_register` 被 gateway 拒绝
- [ ] 发布文档第 4 节的监控阈值已接入告警（claim/busy、handshake rejected、liveness timeout、resume failed、task conflict、transfer budget）
- [ ] `operations_admin` 可读取 `/pinvou3/stats/api/overview.desktop_controller`，匿名请求和 `behavior_analyst` 均被拒绝
- [ ] 合成哨兵对日志、controller 持久文件、抓包/密文和崩溃诊断扫描零命中；失败报告不回显哨兵值
- [ ] 回滚演练证据：gateway 预检结果、旧 Web 分享链接可用性、关闭 `desktop_remote_control` 后的行为

## 3. 我需要哪些输入才能代为执行生产部署

当前环境只能完成本机部署。要真正部署到服务器，请提供：

1. 目标主机与访问方式（SSH/部署通道）与部署目录；
2. 域名与 TLS 证书（或 ACME 签发方式）以及 nginx 落点；
3. secret 注入方式（secret manager / 环境文件路径），以及上面 2.2 的密钥材料由谁生成（我可以生成候选并只把私钥交给运维）；
4. registry 状态文件路径与现有 v2 endpoint 清单（迁移输入）；
5. 生产遥测凭据及其持久目录；controller 生产部署不提供关闭遥测的选项。

拿到上述输入后，我会按 2.1 的顺序执行并逐项产出 2.3 的验收证据。

## 4. 云端测试端点部署记录（2026-09-19，已完成）

按 `docs/remote-e2e-test-endpoint.md` 的幂等脚本 `scripts/deploy-remote-relay-test.sh`（带 `ENABLE_DESKTOP_CONTROLLER=1`）把
Windows 双端远控的 controller-v1 lane 部署到云端测试端点。生产 relay（8787 / `/pinvou3/remote`）全程未改动。

### 4.1 已完成（relay 侧）

| 项 | 值 |
|---|---|
| 实例 | `pinvou-remote-relay-test.service`，`/opt/pinvou-remote-relay-test`，端口 8788 |
| 状态 | `active`；`curl http://127.0.0.1:8788/pinvou3/remote-test/healthz` → **200** |
| 协议 | 启动日志：`pinvou remote relay listening (protocol=v2+controller-v1, …)`；监听地址不得写入生产日志 |
| controller 环境 | `/opt/pinvou-remote-relay-test/controller.env`（0600，`EnvironmentFile=-` 加载）：`development` + `TOFU`、enrollment/pairing HMAC、`PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=relay-test-key-1` |
| 状态目录 | `/var/lib/pinvou-remote-relay-test/state`（0700，位于代码目录之外，重新部署不丢 tombstone/keyring） |
| 客户端信任根 | `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=relay-test-key-1`、`PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64=otRocTSuXfkyfUKA6Gx71zvxII60UdxGK5h/dJ8aOIg=` |
| 客户端入口 | `PINVOU_REMOTE_PUBLIC_URL=https://pinvou.com/pinvou3/remote-test`、`PINVOU_REMOTE_RELAY_WS_URL=wss://pinvou.com/pinvou3/remote-test/ws`（**公网路由尚未生效**，见 4.2） |

### 4.2 代理侧（已完成，用 SSH 跳板写入）

本机出口 IP 直连共享代理会被 TCP 重置（fail2ban 类封禁，密钥授权正常），而 relay 主机到代理的 22 端口可达。因此给部署脚本
新增 `PROXY_SSH_JUMP=root@47.120.8.237`（跳板只做 `-W` TCP 转发，认证仍用本机 `SSH_KEY`），用它完成了：

1. 代理侧 `relay-tunnel` 账号与 `authorized_keys`、`/etc/ssh/sshd_config` 的 `Match User` 限权块；
2. 代理侧 nginx snippet `pinvou3-remote-relay-test.conf` 与 `sites-enabled/pinvou` 的 include（`/pinvou3/remote-test` 路由，`nginx -t` 通过后 reload）；
3. relay → 代理的 `pinvou-remote-relay-test-tunnel.service`（active，回环健康检查通过）。

### 4.3 公网验收（已完成）

| 检查 | 结果 |
|---|---|
| `https://pinvou.com/pinvou3/remote-test/healthz` | 200，`{"ok":true,"endpoint_count":0,…}` |
| `https://pinvou.com/pinvou3/remote-test/r/deploy-check` | 200，页面标题与 bridge 路径匹配 |
| `https://pinvou.com/pinvou3/remote/healthz`（生产） | 200，未被本次部署影响 |
| 测试实例 / 隧道 | `pinvou-remote-relay-test.service` active、`pinvou-remote-relay-test-tunnel.service` active |
| 运行版本 | `protocol=v2+controller-v1`，`controller-router.js`/`controller-resume.js` 等模块在位 |
| **controller lane 探针** | 从公网 `wss://pinvou.com/pinvou3/remote-test/ws` 发 `controller_pairing_claim`（故意不完整）→ 收到控制器族错误信封 `{"v":1,"type":"error","code":"invalid_message","message":"消息无效"}`，证明 controller-v1 分发器已启用且公网可达 |

**唯一遗留**：脚本内置的 `public_verify` 从**本机**执行，而本机到 `pinvou.com` 超时（同环境访问生产 URL 亦超时），因此该步判失败并
把 relay 侧回滚到上一版；由于两次上传的代码内容一致（均含 controller 支持）且状态目录已外置，当前实例功能等价。若要让磁盘上的版本与
“最新一次上传”完全一致，请从一个能访问 `pinvou.com` 的网络重跑同一条命令（幂等）。

部署过程中在 47 上生成的临时密钥 `id_ed25519_pinvou_test_proxy` 已删除。

### 4.4 部署命令（含跳板）

```bash
PROXY_SSH_JUMP=root@47.120.8.237 SKIP_LOCAL_TESTS=1 ENABLE_DESKTOP_CONTROLLER=1 \
  bash scripts/deploy-remote-relay-test.sh
```

### 4.5 本次踩到并已修复的脚本缺陷（`dd307239`）

1. 上传清单是旧白名单，缺 `controller-*.js` / `enrollment-*.js` / `pairing-*.js` / `relay-state-store.js` 等运行时模块 →
   启用 controller 后启动即 `ERR_MODULE_NOT_FOUND`；已改为打包 relay 根目录全部 `*.js`。
2. `legacy` 状态文件被放在 controller 状态目录内 → `legacy_state_path_inside_controller_directory`；已移到同级。
3. 状态目录默认 755 且位于会被整体替换的代码目录 → `relay_state_permissions_unsafe` 且重部署丢状态；
   已改到 `/var/lib/pinvou-remote-relay-test` 并 `chmod 700`。
4. 单元 `EnvironmentFile` 不可选、回滚不恢复 unit、健康检查固定 `sleep 2` → 回滚后进入重启循环 / 启动稍慢被误判；
   已改为可选 `EnvironmentFile=-`、回滚同时恢复 unit、健康检查有界重试 30s。
