# Windows 双端远程控制端到端补齐任务

> 任务状态：历史普通文本 pilot 计划（已被完整双端闭环取代）<br>
> 基线提交：`770a648ab4387f842dd53cee2787027c8949c31b`<br>
> 历史目标：优先打通 **A 远程控制 B 的普通文本会话**，当时未闭环能力默认隐藏。本文件保留旧任务记录，不作为当前 A08 功能范围或发布验收依据；当前边界以《Windows双端远程控制开发任务拆分》为准。A 侧离线副本统一由 SessionStore RemoteProxy 承载，不另建 history store。
>
> 历史进度：E2E-00、E2E-01、E2E-02 完成（见第 3 节与第 5 节），当时位于 E2E-03。以下实现限制和测试结果仅记录该历史阶段，不代表当前 head。

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

- 内容：将远程 session 的用户消息、assistant 输出、错误与任务终态写入权威 journal，并以 `journal_event` 回传；A 侧离线副本由 SessionStore RemoteProxy 保存，在现有 ChatView 渲染，journal 不作为第二历史仓库。
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
| E2E-02 | 1 | PASS | A 端 claim、安全握手、租约安装与 B 端对称接管全部接线完成（三步 6 个提交）；审核中每步各修复 1 处，正式审核再修复 1 处：测试构造入口收口、在途失败不覆盖已有状态（A/B 两侧）、claim 后立即清除连接码。验证：`cargo test remote_control::controller::` 193 通过 / 0 失败，`cargo test remote_control::` 282 通过 / 0 失败，architecture-guard 通过。细节见第 5.3 节。 |

## 4. 已登记决策与本轮已知限制

| 项 | 决策/状态 | 说明 |
|---|---|---|
| `desktop_remote_controlled_start` 阻塞等待注册结果 | 保守默认，登记 | 注册最长等待 30 秒（`REGISTRATION_TIMEOUT_MS`）；期间状态事件照常推送，UI 显示 starting。E2E-07 收敛 UI 状态时再评估是否改为「立即返回 starting + 事件收口」。 |
| `serve()` 暂只保活通道 | 预期 | 握手与加密业务帧派发分别在 E2E-02 / E2E-04 安装到该服务循环；当前对未知消息只做丢弃，绝不转发到 legacy WebUI 事件流。 |
| A 端 `device_display_name` 用有界占位 `remote-device` | 保守默认，登记 | 冻结协议 v1 的 `controller_joined` 不携带被控设备显示名（已核对 PROTOCOL.md 与 fixture）；首版用占位，真实来源需协议扩展或 E2E-03 的能力快照补充。 |
| B 端 `controller_device_name` 留空 | 保守默认，登记 | 同上：`controller_pairing_register` / `controller_joined` 都不含主控设备名；留空让 E2E-07 的 UI 用 i18n 兜底文案（`deviceUnknown`）显示，不为数据层硬编码单语名称。RC-B-006「展示主控设备名称」需协议扩展后才能真正满足。 |
| A 端不在 WebSocket 层声明 controller 子协议 | 保守默认，登记 | PROTOCOL.md:598 要求 controller-v1 客户端选择 `pinvou-desktop-controller-v1`，但 Relay（server.js:846 `noServer: true`，无 `handleProtocols`）不回投该头部，而客户端库（tungstenite 0.30 `handshake/client.rs:272`）在请求了子协议却未收到时必须失败关闭 —— 声明子协议会让 A 端完全无法连接。Relay 侧补齐子协议协商后再接入；当前 Relay 以 `v=1 + controller_*` 类型路由 controller lane（server.js:1182）。 |
| A 端不发送 `controller_capabilities` 首帧 | 保守默认，登记 | 同上：router 没有该类型的 handler（`controller-router.js` 只有 register/claim/handshake/disconnect 与 liveness/resume hooks），发送会收到 `controller_message_unsupported` 并打断握手；E2E-01 的 B 端同样未发送。Relay 补齐后两侧一并接入。 |
| 握手期间 transport 断线 | 预期 | A 端把断线统一映射为 `endpoint_offline` 并失败关闭；transport 在断线时会作废已排队帧（`transport_epoch` 递增），因此不会用旧 claim 重连，重新 claim 属 E2E-06 的恢复链路。 |
| 连接完成后断线只投影 `reconnecting` | 预期 | `serve_controlling_lane` 在断线时调用 `mark_transport_disconnected()` 保证界面不再声称 connected；自动 resume / liveness 属 E2E-06。 |
| B 端信任根缺失在接管点失败关闭 | 保守默认，登记 | 为保持 E2E-01 的注册行为不变，B 在收到 `controller_pairing_reserved` 时才要求信任根（错误码 `relay_trust_unconfigured`）；A 端在 connect 前即失败关闭。 |
| 命令层错误码未映射到 A 对话框文案 | 已知缺口（E2E-07） | `desktop_remote_controller_*` 现在返回有界错误码，但 `desktop-remote-control.js` 的 `controllerConnect` 未把 `Err(String)` 转成带 `code` 的错误，`connectionErrorKey` 只能拿到 undefined；B 面板与 A 壳层的状态名映射同样与后端枚举不一致（`ControlledDevicePanel.jsx:27-40` 期望 `registering`/`waiting_for_pairing`/`pairing_reserved`/`handshake_pending`，后端实际是 `reserving`/`waiting_connection`/`safety_handshake`），导致 B 面板回落到 statusIdle 且不显示连接码。E2E-07 统一修复并补行为测试。 |
| 能力协商按交集收敛 | 依赖 E2E-04/E2E-06 处理 | `negotiateControllerCapabilities` 取「A 请求 ∩ B 注册」；B 目前只注册 `[pairing_v1, encrypted_frames_v1, finished_receipt_v1]`，A 声明 `pairing_v1/encrypted_frames_v1/finished_receipt_v1/role_liveness_v1/resume_v1`，因此 `role_liveness_v1`、`resume_v1`、`task_v1` 都未协商成功。E2E-02 只用到 `pairing_v1`（`controller_joined` 能力绑定）所以可跑通；E2E-04 发送业务帧前必须先在两侧声明 `task_v1`（本地运行时校验会以 `unsupported_capability` 拒绝 `task_submit` 等载荷），E2E-06 同理需要 `role_liveness_v1`/`resume_v1`。 |

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

### 5.2 三步接线（已完成，每步独立提交并审核）

1. **transport 共享 generation 与一次性绑定**（`controller/relay_transport.rs`）—— 提交 `206695064`、审核修复 `39781d0aa`
   - generation 改为 sender 与 transport 任务共享的 `Arc<RwLock<String>>`（事件、入站 generation 门、`install_authenticated_lease` 同源）；新增受控 `bind_generation`（非法/空值/重复绑定/已装租约失败关闭），未绑定时入站只按类型准入、绑定后按 generation 硬门过滤；
   - 用例：一次绑定成功、二次绑定拒绝、已装租约后拒绝、绑定后同 generation 安装租约成功而旧 generation 仍被拒、共享单元格可见性、未绑定放行/绑定后过滤。
2. **A 端接线**（`controller/controlling_service.rs` + `app/commands/desktop_remote_controller.rs`）—— 提交 `c2faa5e25`、审核修复 `1c75ca6d9`
   - `connect`：生成握手密钥 → `load_relay_trust_config`（缺失失败关闭）→ 空 generation 启动 transport → Reserved 阶段 `bind_generation(验签 generation)` → 驱动 `claim→reserved→commit→finished→ack→joined` → 安装认证租约 → 进入 connected（租约安装先于状态投影）；
   - 阶段投影经 `ControllingStageEvent` 广播 `desktop-remote-controller-status`；失败/超时/断线统一映射为有限公共错误（Relay 闭集码优先，断线/40s 上限→`endpoint_offline`，其余→`invalid_message`），失败路径停止 transport 并清空连接码，不产生半租约；
   - 握手完成后由后台任务保活通道：断线投影 `reconnecting`，业务帧留给 E2E-04；`disconnect` 复用既有状态所有者清理路径停止 transport。
3. **B 端接线**（`controller/relay_runtime.rs`）—— 提交 `da654115b`、审核修复 `0d1409d48`
   - `serve()` 收到 `controller_pairing_reserved` 后由 `ControlledHandshakeDriver` 接管，按序处理 commit / Finished / ack，并把 `controller_joined` 经 `controlled_lease_from_outcome` 应用到 `ControlledDeviceManager`；
   - 握手密钥在接管时一次性移交；信任根缺失在接管点失败关闭；失败投影有界错误并结束本代通道。

### 5.3 E2E-02 正式审核记录（轮次 1，结论 PASS）

审核提交范围：`206695064`、`39781d0aa`、`c2faa5e25`、`1c75ca6d9`、`da654115b`、`0d1409d48`、`b6bfddf0c`。

- 目标差异核对：第 5.2 节三步全部落地；E2E-02 出口「A 只有在完整握手和 lease 安装后才进入 connected」由 `happy_path_installs_the_lease_and_reaches_connected` 断言（阶段顺序 connecting→safety_handshake→connected、transport generation 已绑定、租约上下文与协商能力一致、`live_lease()` 可用）。
- 关键失败路径：Relay 拒绝（`relay_rejection_is_surfaced_as_a_bounded_public_error`）、断线与本地 40 秒上限（`disconnect_and_timeout_map_to_peer_unavailable`）、伪造签名 transcript 与二次 generation 绑定（`untrusted_transcript_and_generation_bind_failure_fail_closed`）、阶段回调失败立即中止（`stage_callback_failure_aborts_before_any_lease_material`）、B 端错序/重复接管/绑定不符（`driver_*` 三例）均有真实用例；失败路径统一断言「无租约 + transport 已停止 + 状态为 error」。
- 敏感信息：连接码只在 claim 中使用并在提交后立刻 zeroize（`claim_submits_before...`/`claim_is_protocol_valid_and_carries_the_local_handshake_key` 断言握手对象内码已清空、二次 claim 失败关闭）；恢复凭证只以 `[u8;32]` 存在且 `JoinedLease`/`ControllerLease` 的 Debug 都是 `[REDACTED]`（A/B 两侧各有用例）；出站手握手帧走敏感（Zeroizing）队列；未新增日志或遥测调用点。
- 跨 generation / lease 边界：A 端 generation 必须来自验签 transcript 且只能绑定一次；未绑定或绑定不符时 `install_authenticated_lease` 拒绝；入站帧在绑定后按 generation 硬门过滤；B 端租约只在双收据完成且 `controller_joined` 与验签 transcript 绑定一致时产生；`is_connecting` / `is_pending_handshake` 门保证在途失败不覆盖用户显式断开或已被取代的状态（A/B 两侧各有用例）。
- 审核发现与处置（均为 minor，已修复）：
  1. `transport_channels` 与生产入口同名且会新建独立 generation 单元格，生产误用会失去共享绑定 → 加 `#[cfg(test)]` 并注明生产唯一入口（`39781d0aa`）。
  2. A 端在途失败收尾会覆盖用户的显式断开（状态被改成 error）→ 新增 `is_connecting` 门（`1c75ca6d9`）。
  3. B 端注册/握手失败收尾同源问题 → 新增 `is_pending_handshake` 门（`0d1409d48`）。
  4. 连接码在握手对象内保留到连接结束 → claim 成功后立即 zeroize（`b6bfddf0c`）。
- 定向测试与守卫：`cargo test remote_control::controller::relay_transport` 16 通过；`remote_control::controller::controlling_runtime` 3 通过；`controlling_service` 6 通过；`relay_runtime` 9 通过；`cargo test remote_control::controller::` 193 通过 / 0 失败；`cargo test remote_control::` 282 通过 / 0 失败；`python scripts/architecture-guard.py` 通过。
- 全量回归观察（非本任务范围）：`cargo test --lib` 2487 通过 / 6 失败 / 12 忽略。6 条失败全部在 remote_control 之外的既有模块，且与本次改动无调用关系：`platform::process::tests::timeout_kills_self_contained_process_tree`（隔离运行仍失败，进程树终止超时）与 `features::marketplace::tests::reinstall_upload_with_unlocked_pip_dependencies_is_rejected`（工具 `up-pip-reinstall` 不存在）为环境限制；`platform::process::{completed_parent_does_not_wait_for_descendant_capture_handles, output_capture_limit_terminates_process_tree}`、`features::assistant::multiagent_regression_tests::code_session_real_spawn_refresh_resolves_config_expert_without_project_writes`、`features::assistant::platform::bridge::tests::api_key_requirement_allows_only_vllm_or_loopback_without_key` 在隔离运行时通过，属并行负载下的抖动。
- 结论：**REVIEW: PASS**。下一步进入 E2E-03（A 端远程 Session 与合法任务请求构造）。
