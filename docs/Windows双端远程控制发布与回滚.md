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
| 泄露扫描 | 下述三个真实 Rust 用例、`node --test remote-control-relay/test/controller-sensitive-scan.test.js remote-control-relay/test/controller-telemetry.test.js` | 真实错误投影、Debug 脱敏、受控枚举与哨兵扫描；零匹配视为失败 |

### 1.1 泄露扫描的非空执行证据

已删除的 Rust `controller::telemetry` 不再是验证入口。以下每条必须实际报告 `1 passed; 0 failed`，不能仅用 cargo 退出码 0 或 `0 tests` 作为通过证据：

```bash
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::relay_runtime::tests::log_allowlist_rejects_identifier_shaped_values_and_secrets -- --exact --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::relay_runtime::tests::real_remote_failure_projections_do_not_persist_any_sensitive_category -- --exact --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::resume::tests::resume_debug_redacts_current_and_retired_material -- --exact --test-threads=1
```

发布门槛测试校验这些入口与现有 Rust 源码一致；这项静态合同不能代替上述实际执行，也不能代替真实双机证据。

## 2. 灰度步骤

1. **Registry 覆盖率**：先统计已迁移 endpoint 比例（`scripts/enrollment-migrate.js` 的 dry-run 输出）。未达到 100% 之前不开启公网强制门禁。
2. **打开桌面控制能力开关**：只打开 `desktop_remote_control` 能力，不动 enrollment registry、pairing tombstone 与 digest keyring。
3. **自托管升级说明**：自托管 Relay 必须先升级到 registry-aware 版本，或在所有入口前挂 enrollment gateway（无旁路）。
4. **容量与限流**：确认连接码领取、claim、握手速率限制与告警阈值生效；观察爆破告警与容量水位。
5. **故障注入**：单边网络隔离、Relay 重启、A/B 单边断线，确认 30 秒预算、resume 与终态清理符合需求。

## 3. 真实双机 E2E 旅程

必须使用两台隔离 Windows 设备与生产等价 TLS/网关环境，逐条执行需求 18.3 的 21 个旅程；其中以下条目最容易假通过，需要保留可复现 trace（不含敏感正文）：

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

> 历史普通文本 pilot 登记，依据 `Windows双端远程控制端到端补齐任务.md` 第 4 节与第 11 节整理。以下“已隐藏”、实现限制和旧测试数量仅记录当时阶段，不代表当前完整双端闭环的功能范围或当前 head 验证；当前功能边界以《Windows双端远程控制开发任务拆分》为准。真实双 Windows、生产等价部署与 DEV-25 发布验收仍须实际取得证据，不因本次传播视为完成。

### 7.1 本轮 blocker（未取得证据前不得进入灰度）

| 项 | 现状 | 需要的证据 |
|---|---|---|
| 真实双机 E2E 旅程（本文件第 3 节 21 条） | 未执行（环境受限：需要两台隔离 Windows 设备与生产等价 TLS/网关） | 每条旅程的可复现 trace（不含敏感正文），重点 9～10/11～13/16/19～21 |
| 三个 UI 面浏览器冒烟（B 面板、A 连接壳、远程会话窗口） | 当时未执行；当时证据为源码契约（`remote_control_ui_closure` 等，历史计数 36 条）与 `vite build`，不代表当前测试数量 | 真实渲染/交互记录：键盘可达性、焦点、aria-live、倒计时节流 |
| 真机断线恢复演练（E2E-06 出口的 30 秒预算） | 未执行；当前证据为同进程双端恢复序列用例 | 单边断网/Relay 重启下 resume 完成或失败关闭的 trace，确认无半恢复状态 |

### 7.2 已隐藏（首版不暴露，开关之外无半通路）

| 项 | 处置 |
|---|---|
| A 端临时模型、代码模式、附件与成果导出 | UI 按首版开关隐藏；能力未声明时不暴露入口 |
| 远程普通会话的工具审批 | 未闭环前禁用工具，B 侧不出现 Allow 入口 |
| 历史登记：Relay WS 子协议与 `controller_capabilities` 首帧 | **子协议限制已被后续实现取代（superseded）**：当前 A 端通过 `relay_client::controller_websocket_request` 声明 `pinvou-desktop-controller-v1`，Relay 通过 `remote-control-relay/server.js` 的 `handleProtocols` 协商。独立 `controller_capabilities` handler 仍未接入，当前能力通过 register/claim 与 `controller_joined` 协商；不再以本历史行声称当前客户端不声明子协议，也不将其视为发布验收。 |

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

### 7.4 当前自动化恢复接线回归的证据边界

`app/commands/remote_control/reader_resume_tests.rs` 的独立子进程创建真实 Windows Wry 宿主，调用 feature 中的共享恢复夹具，经真实 RelayClient reader、RemoteControlManager 和 ControlledRelayWorker 执行恢复。前序 A08 的宿主分层、断线 prepared 屏障、错误 final liveness 拒绝、retained epoch 非空断言和旧 reader 围栏在 A13 中继续保留；不重新启用旧 feature 内宿主初始化，也不以非 Connected 投影的 `None == None` 声称覆盖内部 epoch。

此用例仅在 Windows 编译与执行，须取得当前提交适用 Windows 原生 CI 的真实执行结果；Linux / macOS 的条件排除或 Draft skip 不是该用例的通过证据。子进程整体 15 秒、父进程 25 秒和局部等待 3 秒均为失败退出的 watchdog，不是平台启动性能承诺；本轮未完成冷启动及高负载 Windows runner 的时限余量验收。

它使用真实 loopback WebSocket 与认证测试材料，没有真实 renderer、在线 provider、生产网关或双 Windows 设备，不能替代第 7.1 节的发布验收。测试数量以当前提交实际输出为准。
