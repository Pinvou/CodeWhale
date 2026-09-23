# Windows 双端远程控制：发布、灰度与回滚

> 对应任务：DEV-25。需求基线：[`Windows双端远程控制需求文档.md`](./Windows双端远程控制需求文档.md) 第 14～20 节。
>
> 适用范围：公网 Relay（`remote-control-relay/`）+ Windows 客户端（`pinvou3-app/`）。

## 1. 发布前置门禁

以下门禁全部通过才可以进入灰度；任一条失败即阻断，不允许用 mock 结果替代真实证据。

| 门禁 | 命令 / 证据 | 说明 |
|---|---|---|
| Relay 全量 | `npm --prefix remote-control-relay test` | 含 enrollment registry、迁移、发码、槽位、握手、liveness、resume、telemetry |
| 客户端全量 | `cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1` | A/B 状态机、任务台账、传输、审批、模型包、journal |
| 前端与桥 | `npm --prefix pinvou3-app test`、`npm --prefix pinvou3-app run build:ui`、`npm --prefix pinvou3-app run test:webui` | WebUI v2 行为与旧 fixture 不回归 |
| 架构守卫 | `python scripts/architecture-guard.py` | 依赖方向与平台边界 |
| 发布门槛清单 | `node --test remote-control-relay/test/controller-release-gates.test.js` | 本文件的必备章节、回滚约束与残余风险 |
| 泄露扫描 | `cargo test ... controller::telemetry`、`node --test remote-control-relay/test/controller-telemetry.test.js` | 受控枚举与哨兵扫描 |

## 2. 灰度步骤

1. **Registry 覆盖率**：先统计已迁移 endpoint 比例（`scripts/enrollment-migrate.js` 的 dry-run 输出）。未达到 100% 之前不开启公网强制门禁。
2. **打开桌面控制能力开关**：只打开 `desktop_remote_control` 能力，不动 enrollment registry、pairing tombstone 与 digest keyring。
3. **自托管升级说明**：自托管 Relay 必须先升级到 registry-aware 版本，或在所有入口前挂 enrollment gateway（无旁路）。
4. **容量与限流**：确认连接码领取、claim、握手速率限制与告警阈值生效；观察爆破告警与容量水位。
5. **故障注入**：单边网络隔离、Relay 重启、A/B 单边断线，确认 30 秒预算、resume 与终态清理符合需求。

## 3. 真实双机 E2E 旅程

必须使用两台隔离 Windows 设备与生产等价 TLS/网关环境，逐条执行需求 18.3 的 21 个旅程；其中以下条目最容易假通过，需要保留可复现 trace（不含敏感正文）：

> 逐条的前置、操作、预期观测点与 trace 字段见 `Windows双端远程控制双机演练清单.md`；无需部署的本地单机自测见 `scripts/remote-control-local-selftest.ps1`。

- 旅程 9～10、19、21：断线恢复、30 秒预算、RTT/attestation 余量（24000ms / 19000ms）。
- 旅程 11～12：第二个 A、Web/controller 交叉接入、Web-to-Web 既有接管。
- 旅程 13：A 决定 / 超时 / B stop 竞态的唯一终态，B 侧不得出现 Allow。
- 旅程 16：刷新 generation 与旧注册/握手迟到回包丢弃。
- 旅程 20：digest key 轮换后的碰撞判定，以及「真实旧 Relay + gateway」的安全回滚与裸 v2 回滚拒绝。

## 4. 监控与阈值

| 指标 | 用途 | 建议阈值 |
|---|---|---|
| `claim|busy|slot_conflict` 速率 | 设备被争抢/爆破 | 突增 5 分钟即告警 |
| `handshake|rejected|*` 比例 | 握手/收据异常 | 连续 10 分钟 > 5% |
| `liveness|timeout|watchdog_expired` | 30 秒预算触发 | 任一出现即排查 |
| `resume|rejected|resume_failed` | 恢复失败 | 10 分钟 > 2% |
| `task|rejected|task_conflict` | 任务幂等冲突 | 出现即确认是否 A 重试 |
| `attachment|rejected|transfer_budget` | 传输预算 | 持续出现需复核客户端行为 |

所有指标只允许 DEV-24 枚举内的聚合计数，禁止携带连接码、endpoint/lease/token、IP、设备名、prompt/回复、文件名与路径。

## 5. 回滚

- 灰度关闭只禁用 `desktop_remote_control` 能力：不删除 registry、不删除 pairing tombstone、不回退 state schema。
- 最低回滚版本必须是 registry-aware；否则必须保证所有入口都位于同一 registry gateway 之后且不可旁路。
- 裸 v2 回滚（绕过 enrollment gateway 的旧 Relay）一律拒绝；发布前用 `enrollment-gateway-preflight` 预检全入口。
- 回滚演练需记录：待迁移 endpoint 数、gateway 预检结果、旧 Web 分享链接可用性、桌面能力关闭后的行为。

## 6. 残余风险与声明

- 信任模型以「受信 Relay」为前提：本功能不声称抵御恶意 Relay 的主动 MITM；该风险在发布说明中保留并明示。
- 连接码是 5 分钟短期凭证，不承诺 7 位字符串永久作废；UI 文案不得使用「永久作废」表述。
- 首版仅 Windows 声明能力；macOS/Linux 返回 `unsupported` 并隐藏入口。

## 7. 端到端补齐（E2E-00～E2E-08）登记项分类

> 依据 `Windows双端远程控制端到端补齐任务.md` 第 4 节与第 11 节整理；发布评审必须逐条确认分类未变化。

### 7.1 本轮 blocker（未取得证据前不得进入灰度）

| 项 | 现状 | 需要的证据 |
|---|---|---|
| 真实双机 E2E 旅程（本文件第 3 节 21 条） | 未执行（环境受限：需要两台隔离 Windows 设备与生产等价 TLS/网关） | 每条旅程的可复现 trace（不含敏感正文），重点 9～10/11～13/16/19～21 |
| 三个 UI 面浏览器冒烟（B 面板、A 连接壳、远程会话窗口） | 未执行；当前证据为源码契约（`remote_control_ui_closure` 等 36 条）与 `vite build` | 真实渲染/交互记录：键盘可达性、焦点、aria-live、倒计时节流 |
| 真机断线恢复演练（E2E-06 出口的 30 秒预算） | 未执行；当前证据为同进程双端恢复序列用例 | 单边断网/Relay 重启下 resume 完成或失败关闭的 trace，确认无半恢复状态 |

### 7.2 已隐藏（首版不暴露，开关之外无半通路）

| 项 | 处置 |
|---|---|
| A 端临时模型、代码模式、附件与成果导出 | UI 按首版开关隐藏；能力未声明时不暴露入口 |
| 远程普通会话的工具审批 | 未闭环前禁用工具，B 侧不出现 Allow 入口 |
| Relay 未协商 WS 子协议、无 `controller_capabilities` handler | A 端不声明 `pinvou-desktop-controller-v1` 子协议、不发该首帧（否则握手会被 `controller_message_unsupported` 打断）；Relay 补齐后两侧一并接入 |

### 7.3 后续排期（不阻断首版灰度）

| 项 | 说明 |
|---|---|
| 设备显示名 | 冻结协议 v1 的 `controller_joined`/`pairing_register` 不携带对端显示名，A 端用占位 `remote-device`、B 端留空由 UI 兜底；需协议扩展 |
| B 回复非流式 | 首版在轮次结束后从 B 的 `SessionStore` 取文本，A 端看到的是整轮完成后的回复 |
| A 端快照引导 | 首次收到 journal 事件时直接从该事件起算；完整快照引导属可选优化 |
| 附件/成果事件回传 | wire `event_kind` 只用冻结闭集，进行中状态、附件与快照不回传（属后续任务） |
| 跨 lease 任务清理语义 | B 端 `cancel` 只校验帧租约与任务存在性；旧 lease 遗留任务由 stop/refresh/远端断开的清理端口负责 |
| `DesktopControlledCleanup` 适配器测试 | 仓库无 `mock_app` 先例，当前由 manager 侧三条路径用例覆盖触发面，Tauri 级测试列入后续 |
| remote_control 之外的既有失败 | 全量 `cargo test --lib` 有 6～8 条与本功能无关的既有失败（进程树终止超时、marketplace 工具缺失等），属环境限制，已在任务文档登记 |

## 8. 生产 controller lane 启用记录（2026-09-19，含已接受的安全降级）

### 8.1 变更内容

1. 生产 Relay（`pinvou-remote-relay.service`，`/opt/pinvou-remote-relay`，端口 8787）升级到含 controller-v1 的版本（原先为 v2-only 旧代码）。
2. 新增 drop-in `/etc/systemd/system/pinvou-remote-relay.service.d/40-controller.conf`，`EnvironmentFile=/etc/pinvou-remote-relay-controller.env`（**0600**，私钥不出主机）。
3. 环境文件要点：`PINVOU_DESKTOP_CONTROLLER_ENABLED=1`、`NODE_ENV=development`、`PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=development`、`PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=true`、enrollment/pairing HMAC 各一组、`PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=prod-relay-key-1`、`PINVOU_REMOTE_CONTROLLER_STATE_PATH=/var/lib/pinvou-remote-relay/state/controller-state.json`（目录 0700）、`PINVOU_REMOTE_STATE_PATH=/opt/pinvou-remote-relay/data/relay-state.json`（沿用既有状态，未迁移）。
4. 生产部署脚本必须拒绝环境文件符号链接，校验其为 `root:root`、`0600`，并严格验证上述 development + TOFU 配置及两条状态路径；脚本原子维护 `40-controller.conf`，在本机执行无状态变更的 controller-v1 路由烟测，并在本机与公网健康检查中确认 `desktop_controller_enabled=true`。任一检查失败都必须整体恢复旧版 JS 模块和 systemd drop-in，禁止以 Web v2 正常但桌面控制不可用或新旧模块混装的状态通过发布。

### 8.2 为什么启用 TOFU（已接受的安全降级）

- 生产状态文件只有 `version` 与 `revoked_endpoints`，**没有端点清单**：存量端点只存在于内存，无法做「registry 覆盖率 100%」的迁移。
- 一旦启用 controller runtime，v2 的 `desktop_endpoint_register` 也会先过 registry（`server.js:886`）；在 `official` 且无 TOFU 的模式下，全部存量端点重连会被 `enrollment_required` 拒绝。
- 因此经用户确认，**生产采用 `development + TOFU`**：未登记客户端可自助注册。这是明确的安全降级，必须在发布说明中披露；正式发布前应补齐端点清单与 registry 迁移，然后关闭 TOFU。

### 8.3 信任根（公钥随包内置，私钥只在部署环境）

| 项 | 值 |
|---|---|
| key id | `prod-relay-key-1` |
| 公钥（原始 32 字节 ed25519，base64） | `nGlRK79H+YdNVhmZOwkI2T83uhiEBqfZeFbMrJ28LQM=` |
| 客户端内置位置 | `manager::PRODUCTION_PROFILE`（提交 `225fa4cd`），用户无需再配置 `PINVOU_REMOTE_RELAY_SIGNING_*` |
| 私钥 | 仅 `/etc/pinvou-remote-relay-controller.env`（0600），不入库、不入日志；轮换时需同步更新内置公钥并随客户端发版 |

### 8.4 备份与回滚

- 全量备份：`/root/pinvou-prod-backups/pinvou-remote-relay-20260919T134907Z.tgz`（代码 + unit + drop-ins，314 MB）。
- 部署时快照：`/opt/pinvou-remote-relay/backups/20260919-224624`（旧代码/旧 web/dist/旧 package 文件，脚本自动写入）。
- **最小回滚（关闭 controller lane，恢复 v2-only 行为）**：
  ```bash
  sudo rm -f /etc/systemd/system/pinvou-remote-relay.service.d/40-controller.conf
  sudo systemctl daemon-reload && sudo systemctl restart pinvou-remote-relay.service
  curl -fsS https://pinvou.com/pinvou3/remote/healthz   # 期望 200
  ```
- **彻底回滚代码**：用 `/opt/pinvou-remote-relay/backups/20260919-224624` 覆盖 `server.js`/`web/dist`/package 文件后重启（或解压全量备份 tarball）。
- 关闭 controller lane 后，客户端配对会失败关闭为 `invalid_message`/`endpoint_offline`，属预期。

### 8.5 验收证据（2026-09-19）

| 检查 | 结果 |
|---|---|
| 服务 | `pinvou-remote-relay.service` active；启动日志 `protocol=v2+controller-v1` |
| 公网 healthz | `https://pinvou.com/pinvou3/remote/healthz` → 200 `{ok:true}` |
| 存量 v2 用户 | 重启后桌面端重连，`desktop_open` 由 3 回升到 8，`web_client_open=0` 与变更前一致，无注册被拒 |
| controller lane（公网） | 从 `wss://pinvou.com/pinvou3/remote/ws` 发 `controller_pairing_claim` → 收到控制器族错误信封 `{"v":1,"type":"error","code":"invalid_message"}` |
| 回滚路径 | 部署过程中真实触发过一次自动回滚（1s 健康检查误判），成功恢复旧版本；之后脚本已改为 30s 有界重试 |

### 8.6 仍未完成（发布门禁）

- 真实双机 21 条旅程、三面 UI 浏览器冒烟（见 7.1）——在取得证据前不得发布。
- 端点清单与 registry 迁移、关闭 TOFU（见 8.2）。
- 设置页「服务端点」下拉（生产/测试切换）尚未实现，当前切换靠 `PINVOU_REMOTE_ENDPOINT_PROFILE` 或 `~/.pinvou3/web-relay.json` 的 `profile` 字段。
