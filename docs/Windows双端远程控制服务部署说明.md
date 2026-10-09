# Windows 双端远程控制：Relay 服务部署说明

> 适用范围：`remote-control-relay`（公网 Relay + enrollment gateway）。
> 关联：`remote-control-relay/ENROLLMENT.md`（enrollment 单一真相源）、`remote-control-relay/.env.enrollment.example`、`remote-control-relay/nginx-enrollment-gateway.conf`、`Windows双端远程控制发布与回滚.md`。
> 命令与脚本路径以仓库根目录为当前目录。历史部署记录不是当前发布验收或永久安全豁免。

## 1. 本机部署（自测用，已完成）

适用：单机联调、配合 [本地单机自测脚本](../scripts/remote-control-local-selftest.ps1) 与刚生成的安装包做双进程冒烟。

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

当前自测脚本直接启动并记录 Node 服务进程（PID + 启动时间），停止时还验证 Node 路径及绝对脚本命令行，不再启动包装进程或按端口强杀。旧 PID 文件、复用 PID、无法证明属主或被无关服务占用的端口均不会被停止；旧手工启动的服务需操作者自行确认后处理。标准错误单独记录到同一自测目录的 `relay.stderr.log`，上表旧 PID 仅为历史记录。

重复 Prepare/EnvOnly 会复用两组 HMAC 及签名身份；存在状态、Relay 环境或客户端公钥投影而原身份丢失时拒绝重建。既有签名 JSON 在覆盖环境前校验完整字段、标准 base64、Ed25519 私钥与派生公钥一致性；验证材料只走本地子进程 stdin，不进入命令行或输出。坏身份需恢复原文件，不能用新建密钥或删状态代替。

## 2. 生产部署（需目标主机与运维输入）

### 2.1 组件与顺序

1. **Relay 本体**：`node server.js`（`PORT` 默认 8787，`PINVOU_REMOTE_HOST` 决定监听地址）。
2. **enrollment 门禁**：Relay 监听必须位于 loopback（字面 IPv4/IPv6，`localhost` 不算），由 `remote-control-relay/scripts/enrollment-gateway-server.js` 作为唯一入口；公网路由使用 `remote-control-relay/nginx-enrollment-gateway.conf`。
3. **注册库**：`enrollment-registry.js` 读取 `PINVOU_REMOTE_CONTROLLER_STATE_PATH`（不得与 legacy `PINVOU_REMOTE_STATE_PATH` 同路径）；迁移与覆盖率用 `remote-control-relay/scripts/enrollment-migrate.js` dry-run 取证（要求 100%）。
4. **验收预检**：对每个公网入口运行 `remote-control-relay/scripts/enrollment-gateway-preflight.js`（校验健康标记、WebSocket upgrade 标记、匿名注册拒绝、Relay URL 为字面 loopback、OS 监听检查无通配绑定）。
5. **回滚检查**：`scripts/check-enrollment-gated-v2-rollback.mjs` 退出码为 0 才允许启动旧 v2 二进制；裸 v2 回滚一律拒绝。
6. **运行加固**：`10-hardening.conf` 作为服务单元加固基线；服务账号只读仓库、只写状态目录。

### 2.2 必需配置（来自部署 secret 管理器，禁止入库/入日志）

| 变量 | 说明 |
|---|---|
| `PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT` | `official` / `self_hosted`（生产不得用 `development`） |
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID` / `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY` | registry HMAC（32–4096 字节） |
| `PINVOU_REMOTE_PAIRING_HMAC_KEY_ID` / `PINVOU_REMOTE_PAIRING_HMAC_KEY` | 连接码摘要 keyring（支持轮换，见发布文档第 2 节步骤 2、第 3 节旅程 20） |
| `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64` | Ed25519 PKCS#8 DER base64，签名 pairing transcript |
| `PINVOU_REMOTE_CONTROLLER_STATE_PATH` / `PINVOU_REMOTE_STATE_PATH` | 新旧状态文件绝对路径，必须不同 |
| 遥测（可选） | enrollment token / device pepper / admin password；未配置即禁用遥测（本机部署即此状态） |

客户端侧必须 pin 同一份公钥（原始 32 字节 base64）。内置 production/test profile 随包提供 key id 与公钥；自建或轮换后的服务需成对设置 `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` + `PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64`。不能把未设置环境变量等同于无信任根；显式覆盖不完整或无效仍失败关闭，服务公钥与客户端 pin 不一致不能配对。

`run-remote-control-endpoint.ps1 -Mode Production` 清除进程级 profile、地址与信任根覆盖，但不修改持久配置。若用户持久选择了 test 或自定义 Relay，清除环境后仍采用该选择；请先在设置中确认端点。Test 模式显式选 test 并清除遗留 runtime 地址覆盖，避免同一进程切换后使用旧地址。

### 2.3 部署后的验收清单

- [ ] `enrollment-gateway-preflight.js` 对所有公网入口退出码 0
- [ ] registry 覆盖率 100%（`enrollment-migrate.js` dry-run 输出留档）
- [ ] Relay 仅 loopback 监听，且 OS 监听检查无通配/非 loopback 绑定
- [ ] 匿名 `web_client_join` / `desktop_endpoint_register` 被 gateway 拒绝
- [ ] 发布文档第 4 节的监控阈值已接入告警（claim/busy、handshake rejected、liveness timeout、resume failed、task conflict、transfer budget）
- [ ] 回滚演练证据：gateway 预检结果、旧 Web 分享链接可用性、关闭 `desktop_remote_control` 后的行为

## 3. 运维输入与发布执行边界

部署前须由运维在本地受控配置中提供 SSH 目标、独立登记目标、部署私钥路径、域名/TLS、部署目录和 secret 注入方式；禁止提交这些实例值。secret 材料仅由部署环境生成、验证和注入，Agent 不通过 PR 或日志传递私钥。

生产脚本必须同时配置 `PINVOU_CONFIRM_PRODUCTION_DEPLOY=1`、`PINVOU_REMOTE_DEPLOY_SERVER`、`PINVOU_REMOTE_EXPECTED_DEPLOY_SERVER`、`SSH_KEY`、`PINVOU_REMOTE_DIRECT_URL`。两份目标必须一致，不配置或不一致时，在联网、上传前退出。确认标志不是 enrollment、验收或回滚门禁豁免。

测试脚本必须同时配置 `RELAY_SERVER`、`EXPECTED_RELAY_SERVER`、`PROXY_SERVER`、`EXPECTED_PROXY_SERVER` 和 `SSH_KEY`；目标为 SSH 账号与字面 IPv4，登记值从运维清单独立取得，不能从实际目标自动赋值。有跳板时同时配置 `PROXY_SSH_JUMP` 与 `EXPECTED_PROXY_SSH_JUMP`。参见 [测试端点操作说明](remote-e2e-test-endpoint.md)。

## 4. 云端测试端点历史记录（2026-09-19，非当前验收）

本节保留当次部署结论，不公开具体 SSH 登录目标、个人密钥名、服务器布局或备份定位。脚本的隔离测试服务、持久身份目录、专用隧道账户、受限转发和局部失败回滚均须按当前源码与运维清单核对；未在本轮重新部署或修改线上配置。

### 4.1 历史 Relay 侧证据与当前身份规则

当次记录包括测试实例 active、loopback 健康检查成功及 controller-v1 启用。首次记录时公网路由未就绪；后续代理配置与公网探测成功，二者有时间先后，不代表当前服务状态。

身份与状态必须位于整体替换的代码目录之外。已有身份验证后复用，旧代码目录的身份仅作一次性只读迁移；已有状态或公钥痕迹而身份丢失时失败关闭，不重新铸钥。私钥/HMAC 环境文件为 0600，父目录为 0700；代码回滚、普通 teardown 不回收该持久身份。

内置测试 profile 的公开信任根为 `relay-test-key-1` / `otRocTSuXfkyfUKA6Gx71zvxII60UdxGK5h/dJ8aOIg=`。首次空安装生成的新公钥不会自动等于此 pin；必须取回实际公钥并成对显式配置同一 key ID/public key 才能演练。脚本尚未自动交叉校验内置 pin（#1507），不能用删身份或状态解决失配。

公开客户端入口继续使用 `https://pinvou.com/pinvou3/remote-test` 与 `wss://pinvou.com/pinvou3/remote-test/ws`；它们是随包发布的用户访问合同，不是 SSH 管理入口。

### 4.2 历史代理配置与公网探测

当次通过受限 SSH 跳板完成代理侧专用隧道账户、authorized_keys、sshd 转发限制、nginx 测试路由和反向隧道的配置。保留这些变更类型以便核查副作用，具体管理目标与配置布局仅在运维清单登记。

当次探测记录包含测试/生产健康接口成功、旧版本页面成功及不完整 controller claim 返回 `invalid_message`。旧页面含 bridge 的观测不是当前产物校验标准；现在使用 CLI 检查 index、base、startup 和模块资源，并检查公网当前 base 的 assets。

历史本机到公网不可达曾触发 Relay 侧自动回滚；不把其他位置的探测成功等同于本机发布脚本成功，也不声称这次审核重跑了公网验收。临时部署密钥已在当次操作后删除，文件名与目标不入库。

### 4.3 当前操作与验证要求

先在本地加载不入库的受控配置，再执行：

```bash
bash scripts/deploy-remote-relay-test.sh --validate-only
ENABLE_DESKTOP_CONTROLLER=1 bash scripts/deploy-remote-relay-test.sh
```

默认执行本地测试和 Web 构建；只有已有对应实际证据时才可显式跳过。测试端点使用 development + TOFU，只供测试，不是生产配置。首次部署、重部署与回滚都不得代替第 2.3 节和真实双 Windows 发布验收。

Windows 本机应执行 `npm --prefix remote-control-relay test`，保留 PowerShell 身份、属主和 launcher 行为测试的实际结果；Linux 的 win32 skip 和文件符号链接权限 skip 均不计通过。CI 的具体选择与结果以当前提交工作流为准，不以 required gate 绿色声称所有平台行为已执行。

非阻塞跟进：Windows 脚本行为 CI 覆盖见 #1512；本地自测秘密文件当前依赖 LOCALAPPDATA 继承权限，尚无显式 DACL 验证/收紧，后续实测与硬化见 #1675。未复现越权读取，不据此声称发生泄露；launcher 的健康 warning 或缺可执行文件退出也不是应用已成功启动的证据。

历史脚本曾遗漏运行期模块、错误放置 legacy 状态、随代码替换丢状态、用固定等待误判健康；目前分别由完整模块归档、独立状态/身份目录、回滚恢复单元与有界健康重试覆盖。具体实现、测试数量和是否通过以当前源码与当前执行输出为准。
