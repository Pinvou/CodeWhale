# Windows 双端远程控制端到端补齐任务

> 任务状态：执行中  
> 基线提交：`770a648ab4387f842dd53cee2787027c8949c31b`  
> 核心目标：优先打通 **A 远程控制 B 的普通文本会话**。临时模型、代码模式、附件与成果首版继续由能力开关隐藏，不阻塞普通会话闭环。

## 1. 执行规则

1. 每个任务先实现、后审核；审核未通过时修复并复审，通过后才能进入下一任务。
2. 每个任务独立提交，提交信息使用 `type(remote-control): 中文描述`，并保留 DCO 签名。
3. 任务审核必须包含：
   - 目标差异核对；
   - 关键失败路径检查；
   - 定向测试；
   - 架构守卫；
   - 敏感信息与跨generation/lease边界检查。
4. 本轮遇到产品取舍时采用保守默认值并登记，不中断执行：
   - 首版普通会话仅开放 B 模型与 `chat` 模式；
   - 远程普通会话工具审批未闭环前禁用工具；
   - A 临时模型、代码模式、附件与成果继续隐藏；
   - 不修改 legacy WebUI v2 wire。

## 2. 任务拆分

### E2E-00：任务拆分与审核门禁

- 内容：登记任务边界、顺序、审核规则与首版保守能力决策。
- 出口：本文档提交，审核确认任务覆盖当前端到端断点。

### E2E-01：B 端 Relay 通道与短期码注册

- 内容：为 B 端建立可复用 Relay 消息通道，将本地 `ControlledDeviceManager` 与 `desktop_pairing_register` / Relay 回包接线，真实展示短期码。
- 不做：A claim、任务发送与 UI 能力扩展。
- 出口：B 注册成功后进入 `waiting_for_pairing`，失败关闭并保留本地停止入口。

### E2E-02：A 端 claim、安全握手与租约状态

- 内容：实现 A 端真实 Relay claim、双端握手、lease 建立与状态事件投影。
- 不做：业务帧发送。
- 出口：A 只有在完整握手和 lease 安装后才进入 `connected`；失败统一映射为有限公共错误。

### E2E-03：A 端远程 Session 与合法任务请求构造

- 内容：新增 A 专用 session/capability/task intent 命令，由 Rust 构造冻结协议 payload 与输入摘要，前端不再拼 wire。
- 出口：UI 状态持有真实远程 `sessionId` 与 capability revision；非法上下文提交失败关闭。

### E2E-04：加密业务帧双向派发

- 内容：A 发送加密 `task_submit/task_cancel`，B 解密、校验并派发到本地 `RemoteTaskService`；B 的任务状态加密回传 A。
- 出口：同一进程模拟双端可完成普通文本任务 admission/execution/terminal status；旧 lease/generation 帧丢弃。

### E2E-05：普通会话正文 journal 与 A 渲染

- 内容：将远程 session 的用户消息、assistant 输出、错误与任务终态写入权威 journal，并以 `journal_event` 回传；A 写独立历史并渲染 transcript。
- 出口：A 能看到用户输入与 B 模型回复，而不是只有任务状态。

### E2E-06：liveness、恢复与清理闭环

- 内容：接通 A 心跳、B watchdog、断线恢复门、显式断开、B 停止/刷新时的任务与凭证清理。
- 出口：有效 A 存活证据不超过 30 秒；停止/断开产生明确终态并清理远端任务。

### E2E-07：UI 最小闭环与能力降级

- 内容：按能力开关收敛 A/B UI，仅展示 B 模型普通会话；修复连接状态、session 初始化、发送、取消、错误与正文渲染。
- 出口：三个 UI 面有可执行行为测试；不可用能力不暴露半通路。

### E2E-08：集成回归与最终审核

- 内容：补充双端集成测试、架构守卫、UI 契约与安全扫描，登记剩余发布缺口。
- 出口：普通文本会话端到端测试通过；所有剩余缺口明确非本轮 blocker 或已隐藏。

## 3. 审核记录

| 任务 | 审核轮次 | 结论 | 说明 |
|---|---:|---|---|
| E2E-00 | 1 | PASS | 任务覆盖当前已确认的连接、session、任务路由、事件回流、生命周期与 UI 断点；保守能力决策已登记。 |
| E2E-01 | 1 | PASS | B 端 Relay 通道与短期码注册接线完成；审核中修复两处：`controller_endpoint_id` 由散文错误改为有界码 `desktop_relay_inactive`，注册等待循环抽出可测端口并补齐超时/断线/忽略 ack/拒绝四条用例。验证：`cargo test remote_control::` 252 通过；`desktop_remote_state.test.mjs` 9 通过；architecture-guard 通过。 |

## 4. 已登记决策与本轮已知限制

| 项 | 决策/状态 | 说明 |
|---|---|---|
| `desktop_remote_controlled_start` 阻塞等待注册结果 | 保守默认，登记 | 注册最长等待 30 秒（`REGISTRATION_TIMEOUT_MS`）；期间状态事件照常推送，UI 显示 starting。E2E-07 收敛 UI 状态时再评估是否改为「立即返回 starting + 事件收口」。 |
| `serve()` 暂只保活通道 | 预期 | 握手与加密业务帧派发分别在 E2E-02 / E2E-04 安装到该服务循环；当前对未知消息只做丢弃，绝不转发到 legacy WebUI 事件流。 |

## 5. E2E-02 实施状态与剩余步骤

### 5.1 已落地提交（协议与状态机层已可运行且测试齐全）

| 提交 | 内容 |
|---|---|
| `08143219` | `SessionKeys::finished_pair`：收据阶段不再需要 `LeaseBinding` |
| `7ad57a5f` | 受控信任根入口：`from_pinned_keys` / `load_relay_trust_config` / `authenticate_relay_transcript` |
| `a79e2d36` | `LeaseInstallValidationContext` 访问器 + transcript hash 解码 |
| `d2d1a31d` | `ReceiptSecret::to_base64`（receipt ack 信封） |
| `311b539e` | 双方握手公钥透出（A 端 ECDH 的对端公钥） |
| `75565b17` | A 端 claim 与握手序列模块 `controller/controlling_handshake.rs` |
| `0444a083` | A 端全序列测试；修正 ack 角色语义与 finished 收发器提前丢弃两处缺陷 |
| `c5a2ee3b` | B 端对称模块 `controller/controlled_handshake.rs` + A↔B 双端配对测试 |
| `0d6a405f` | A 端 worker 编排 `controller/controlling_runtime.rs`（端口化，2 条用例） |

验证基线：`cargo test controller::` 178 通过 / 0 失败；`python scripts/architecture-guard.py` 通过。

### 5.2 剩余三步（按序执行，每步独立提交并审核）

1. **transport 共享 generation 与一次性绑定**（`controller/relay_transport.rs`）
   - 现状：`install_authenticated_lease` 要求租约 generation 等于 transport 创建时的 generation，而 A 端在 claim 前不可能知道 B 的 generation（只能从验签 transcript / `controller_joined` 得到），因此 A 端无法安装租约。
   - 做法：把 generation 改为 sender 与 transport 任务共享的 `Arc<RwLock<String>>`（sender 字段 `:237-247`、任务内 4 处 `config.remote_control_generation.clone()` `:551/:564/:616/:669`、`install_authenticated_lease` `:250-260` 统一读同一来源），并新增受控 `bind_generation`：仅未安装租约时可调用、只允许绑定一次、空值/非法 id 失败关闭。
   - 用例：一次绑定成功、二次绑定拒绝、已装租约后拒绝、绑定后用新 generation 安装租约成功且旧 generation 仍被拒。
2. **A 端接线**（`app/commands/desktop_remote_controller.rs` + 端口实现）
   - `connect`：`HandshakeKeyPair::generate()` → `load_relay_trust_config`（未配置失败关闭）→ `spawn_controller_transport(ControllerTransportConfig::new(relay_url, ControllerRole::ControllerA, placeholder), sink)` → 收到验签 transcript 后 `bind_generation(transcript.remote_control_generation)` → `drive_controlling_handshake`；
   - 阶段投影：`ControllingStageEvent{ClaimSent, Reserved, FinishedSent, Joined}` → `ControllingDeviceManager::{connect, complete_reserved, complete_connected}` + 广播 `desktop-remote-controller-status`；失败/超时/断线统一映射为有限公共错误，不产生半租约；
   - `disconnect`：停止 worker 并清理 live 上下文；`ControllerLease` 需要 `device_display_name`（`controller_joined` 无该字段，先取有界占位并在 E2E-03 补真实来源）与 `transport_key_fingerprint`（需确定派生来源，建议由 transcript_hash + 角色派生）。
3. **B 端接线**（`controller/relay_runtime.rs`）
   - `ControlledRelayWorker::serve()` 收到 `controller_pairing_reserved` 时用 `ControlledHandshake::new(...)` 接管，按序处理 `finished_receipt_commit` / `encrypted_handshake_finished` / `finished_receipt_ack`，并把 `controller_joined` 应用到 `ControlledDeviceManager`。

完成后运行定向测试与架构守卫，做 E2E-02 正式审核（失败路径、敏感信息、跨 generation/lease 边界），通过后进入 E2E-03。
