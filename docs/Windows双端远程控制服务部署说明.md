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
| 验证 | `GET /pinvou3/remote/ws` → HTTP 200；Relay 日志出现 `pinvou remote relay listening on http://127.0.0.1:8787 ...` |
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
6. **运行加固**：`10-hardening.conf` 作为服务单元加固基线；服务账号只读仓库、只写状态目录。

### 2.2 必需配置（来自部署 secret 管理器，禁止入库/入日志）

| 变量 | 说明 |
|---|---|
| `PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT` | `official` / `self_hosted`（生产不得用 `development`） |
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID` / `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY` | registry HMAC（32–4096 字节） |
| `PINVOU_REMOTE_PAIRING_HMAC_KEY_ID` / `PINVOU_REMOTE_PAIRING_HMAC_KEY` | 连接码摘要 keyring（支持轮换，见发布文档第 7.3 节） |
| `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64` | Ed25519 PKCS#8 DER base64，签名 pairing transcript |
| `PINVOU_REMOTE_CONTROLLER_STATE_PATH` / `PINVOU_REMOTE_STATE_PATH` | 新旧状态文件绝对路径，必须不同 |
| 遥测（可选） | enrollment token / device pepper / admin password；未配置即禁用遥测（本机部署即此状态） |

客户端侧必须 pin 同一份公钥（原始 32 字节 base64）：`PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` + `PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64`；缺失会以 `relay_trust_unconfigured` 失败关闭。

### 2.3 部署后的验收清单

- [ ] `enrollment-gateway-preflight.js` 对所有公网入口退出码 0
- [ ] registry 覆盖率 100%（`enrollment-migrate.js` dry-run 输出留档）
- [ ] Relay 仅 loopback 监听，且 OS 监听检查无通配/非 loopback 绑定
- [ ] 匿名 `web_client_join` / `desktop_endpoint_register` 被 gateway 拒绝
- [ ] 发布文档第 4 节的监控阈值已接入告警（claim/busy、handshake rejected、liveness timeout、resume failed、task conflict、transfer budget）
- [ ] 回滚演练证据：gateway 预检结果、旧 Web 分享链接可用性、关闭 `desktop_remote_control` 后的行为

## 3. 我需要哪些输入才能代为执行生产部署

当前环境只能完成本机部署。要真正部署到服务器，请提供：

1. 目标主机与访问方式（SSH/部署通道）与部署目录；
2. 域名与 TLS 证书（或 ACME 签发方式）以及 nginx 落点；
3. secret 注入方式（secret manager / 环境文件路径），以及上面 2.2 的密钥材料由谁生成（我可以生成候选并只把私钥交给运维）；
4. registry 状态文件路径与现有 v2 endpoint 清单（迁移输入）；
5. 是否启用遥测及其凭据。

拿到上述输入后，我会按 2.1 的顺序执行并逐项产出 2.3 的验收证据。
