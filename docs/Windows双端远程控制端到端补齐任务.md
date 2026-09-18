# Windows 双端远程控制端到端补齐任务

> 任务状态：执行中  
> 基线提交：`770a648ab4387f842dd53cee2787027c8949c31b`  
> 核心目标：优先打通 **A 远程控制 B 的普通文本会话**。临时模型、代码模式、附件与成果首版继续由能力开关隐藏，不阻塞普通会话闭环。
>
> 进度：E2E-00～E2E-04 完成并审核通过（见第 3 节与第 5～7 节）；E2E-05 正文链路（A 落库/投影 + B 用户消息/回复/终态回传）与 E2E-06 第 1 步（A 心跳 + `role_liveness_v1` 协商）已落地待审核；剩余 E2E-05 缺口恢复与 UI 渲染、E2E-06 的 B 侧 challenge/attestation 与 resume、E2E-07、E2E-08。

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
| E2E-02 | 1 | PASS | A 端 claim、安全握手、租约安装与 B 端对称接管全部接线完成（三步 6 个提交）；审核中每步各修复 1 处，正式审核再修复 1 处：测试构造入口收口、在途失败不覆盖已有状态（A/B 两侧）、claim 后立即清除连接码。验证：`cargo test remote_control::controller::` 193 通过 / 0 失败，`cargo test remote_control::` 282 通过 / 0 失败，architecture-guard 通过。细节见第 5.3 节。 |
| E2E-03 | 1 | PASS | A 端会话/能力/任务意图命令落地（2 个提交）：冻结 payload 与输入摘要由 Rust 构造并自检，前端只传 UI 字段；UI 状态持有 sessionId 与 capability revision。审核修复 2 处：会话重开复用引用、wire 快照映射进 UI 能力域。验证：`cargo test remote_control::` 289 通过 / 0 失败；4 个 node 契约文件 25 通过 / 0 失败；`npm run build:ui` 与 architecture-guard 通过。细节见第 6 节。 |
| E2E-04 | 1 | PASS | 加密业务帧双向派发完成（5 个提交）：共享加密通道、A 端收发接线、B 端解密派发与加密回传、bridge 发送入口；同进程双端用例覆盖 A 发 task_submit → B 解密派发 → B 回传 task_status → A 投影。审核修复 1 处：任务校验改用活租约的真实协商能力（删除本地常量表）。验证：`cargo test remote_control::` 298 通过 / 0 失败；architecture-guard 通过。细节见第 7 节。 |

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
| A 端会话 id 由本端生成 | E2E-03 决策 | 冻结协议没有「创建会话」帧：`task_submit.session_id` 必须由发起方提供。E2E-03 由 A 生成 `session-<uuid>` 作为该远程会话在 wire 上的 id（UI 持有的「远程 sessionId」即此值），`session_ref` 只用于本地索引；B 的派发层（E2E-04）需据此绑定/创建权威会话（B 侧 `RemoteSessionMap` 支持 ref→权威 id 映射），并能把权威 id 回传（`task_status`/`session_snapshot` 都带 `session_id`）。 |
| 任务意图重试必须复用 client_request_id | 依赖 E2E-04 | `desktop_remote_controller_task_intent` 每次调用生成新的 `task_id`/`client_request_id`（一次用户提交 = 一个新意图）；断线重试/恢复时必须复用已返回的 id（B 侧 `task_id + client_request_id` 幂等），E2E-04 接线时不要把重试当成新意图。 |
| A 会话视图发送路径仍走旧 JS 拼装 | 已知缺口（E2E-07） | `RemoteConversationView.jsx:55-62` 仍在 JS 拼一个 camelCase 伪 payload 并调用 B 侧 `desktop_remote_task_submit`（DEV-22 遗留，必然被冻结校验拒绝）。E2E-03 只提供了 A 专用构造路径与状态位；E2E-07 收敛 UI 时把发送/取消改为 `controllerTaskIntent`/`controllerTaskCancelIntent` 并接 E2E-04 的加密派发。 |
| A 端历史与远程会话尚未关联 | 依赖 E2E-05 | `desktop_remote_controller_create_history` 的 `b_session_reference` 目前由前端传入；E2E-03 的 `session_ref`/`session_id` 是它的自然来源，E2E-05 写 A 独立历史时统一关联。 |
| B 端取消不校验跨 lease 任务 | 依赖 E2E-06 | `cancel_decrypted_task` 只校验帧的租约与任务存在性；旧 lease 遗留任务的清理由 E2E-06（B stop/刷新的任务与凭证清理）负责，`ControllerTaskEventSink` 级别的权限不再扩张。 |
| 能力快照按 `known_revision` 复用 | E2E-04 决策 | B 收到 `capability_snapshot_request` 时，若本地已有同一 revision 且未过期的快照则直接复用（A 端同 revision 幂等接受），否则重建并把 revision 加一；避免每次请求都抬 revision。 |
| B 端 journal 回传的 session 身份 | E2E-05 决策 | journal 事件在 B 侧按权威 session id 记录；回传 wire 帧时与 `task_status` 一致改写回 A 的 ref（需要在 `RemoteSessionMap` 补反向查询），保证 A 端一份 wire 身份贯穿任务与正文。 |
| A 端未绑定历史时只投影不落库 | E2E-05 决策 | 收到 journal 事件时若没有匹配 `session_ref` 的本地历史，只广播 `desktop-remote-controller-journal`（含 `historyId: null`），由 E2E-07 决定先创建/绑定历史；避免在数据层臆造标题或文案。 |
| wire `event_kind` 只用冻结闭集 | E2E-05 决策 | 内部 `JournalKind`（task_status/attachment/snapshot 等）与冻结 `EVENT_KINDS` 不完全一致：回传时终态 `task_status` 映射为 `terminal`，进行中状态、附件与快照不回传（进行中状态由 `task_status` 帧负责，附件/快照属后续任务）；事件本体（base64）保留内部更细类型，A 端只核对 epoch/序号/会话。 |
| B 回复正文来源 | E2E-05 决策 | 首版在轮次结束后从 B 的 `SessionStore` 读最后一条 assistant 消息的文本块（按 journal 上限截断），不做流式增量；因此 A 端看到的是一轮结束后的完整回复。读取失败不阻塞终态，只少一条正文事件。 |

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

## 6. E2E-03 实施与审核记录（轮次 1，结论 PASS）

### 6.1 已落地提交

| 提交 | 内容 |
|---|---|
| `debf3aa1c` | A 端远程会话与任务请求构造：`controlling_session.rs`（会话意图 + 三个冻结 payload builder + 输入摘要 + 失败门）、7 个 `desktop_remote_controller_session_*` 命令、`ControllerLease` 记录协商能力与 `live_runtime_context`、A/B 两侧补 `task_v1` 能力声明、前端状态与 bridge 接线、node 契约测试 |
| `8858dfe48` | 审核修复：会话重开复用 `sessionRef`（幂等）、wire 快照映射进 UI 能力域（`capabilitySnapshotForUi`），并锁定「UI 路径不出现协议字段名」 |

### 6.2 审核要点

- 目标差异：A 专用 session/capability/task intent 命令齐备；payload 与输入摘要全部由 Rust 构造，且每个 builder 的输出都经 `validate_controller_runtime_payload_json`（角色 A + 活租约 + 协商能力）自检，前端只提交标题/模式/正文。
- 失败关闭：未连接（`controller_not_connected`）、lease 五元组变更（`session_context_stale`）、未取到能力快照（`capability_snapshot_required`）、能力未协商（`capability_not_negotiated`）、revision 回退（`capability_snapshot_stale`）、跨连接/跨 lease 快照、取消未构造过的任务（`task_not_found`）、非 chat 模式（`session_mode_unavailable`）、容量上限（`session_capacity`/`task_capacity`）均有用例。
- 导出标准：`validate_controller_runtime_payload_json` 输出即为 wire payload（租约五元组 + revision + 摘要 + input 结构），因此 E2E-04 只需加密与派发，不再重新拼装字段。
- 敏感信息：会话状态只在内存（无持久化调用）、投影只含 opaque 标识与计数、意图投影不含凭证；无新增日志/遥测。
- 跨 generation/lease：会话绑定活租约五元组，快照必须与同一 lease 匹配；断线/新连接清空全部会话意图。
- 审核发现（均 minor，已修复）：见 6.1 第二个提交。
- 验证：`cargo test remote_control::` 289 通过 / 0 失败；`node --test controller_session_intent_contract/desktop_remote_state/remote_conversation_contract/connect_remote_device_contract` 25 通过 / 0 失败；`npm run build:ui` 通过；`python scripts/architecture-guard.py` 通过；`npm run lint:ui` 0 error。

### 6.3 下一步

E2E-04（加密业务帧双向派发）。开工前需先读第 4 节与本节的登记项，特别是：`client_request_id` 复用规则、B 端会话映射（`RemoteSessionMap`）、`task_v1` 已协商、A 端 `controllerTaskIntent` 返回的 payload 直接交给加密派发。

## 7. E2E-04 实施与审核记录（轮次 1，结论 PASS）

### 7.1 已落地提交

| 提交 | 内容 |
|---|---|
| `c0629c5c4` | 共享加密业务帧通道 `encrypted_lane.rs`：seal/open 两侧冻结校验（明文 → 加密 → 信封 / 信封 → 解密 → 明文）、有界失败分类、`ControllingLaneState` 单连接通道、`FrameDirection::wire` 提升可见性、test_support 增加 `handshake_binding`/`role_session_keys`/`lease_context`；5 条用例（双向往返无明文泄漏、重放窗口与篡改/方向/类型失败关闭、能力与跨 lease 失败关闭、通道未安装/清理失败关闭、异密钥对失败关闭） |
| `800a83f75` | A 端收发接线：握手 outcome 带 `SessionKeys`（driver 在 Joined 后 `take_session`）；`complete_connected` 在安装认证租约的同一处安装加密通道；`send_authenticated_business`；保活任务改为解密+派发（capability_snapshot → 会话状态、task_status → 任务事件；异 generation/重放/伪造帧静默丢弃），事件出口收敛为 `ControllingLaneSinks`；新增 `desktop_remote_controller_task_send` / `_task_cancel_send`；`apply_capability_snapshot_for_lease` / `session_ref_for_lease`；1 条端到端派发用例 |
| `10ead6648` | bridge 暴露 `controllerTaskSend` / `controllerTaskCancelSend`（只传会话引用与正文/任务 id），并补契约断言锁定发送路径不出现协议字段名 |
| `c15029029` | B 端解密/派发/回传：`ControlledBusinessPort` 端口 + 纯函数 `dispatch_decrypted_payload`（capability_snapshot_request / task_submit / task_cancel，回包 session_id 改写回 A 的 ref）、B 侧加密通道安装（先装通道再投影 connected）、`ControlledLease`/`ControlledDeviceManager::live_runtime_context` 暴露协商能力、命令层端口实现复用 `build_capability_snapshot` 与抽出的 `admit_and_dispatch`/`cancel_and_project`、wire 会话映射（首次见到 A 的 ref 即创建 B 权威会话）、能力快照 wire 投影 `wire_capability_snapshot` |
| `a899f4096` | 审核修复：任务校验改用活租约的真实协商能力（删除本地常量表 `NEGOTIATED_CAPABILITIES`） |
| `88832f0f3` | E2E-05 第 1 步：A 端解密 `journal_event`（明文字段与事件本体逐项一致才接受）经 `ControllingJournalSink` 落库到 A 独立历史并广播 `desktop-remote-controller-journal`；能力声明补 `snapshot_journal_v1` |

验证基线：`cargo test remote_control::` 295 通过 / 0 失败；architecture-guard 通过。

### 7.2 审核要点（E2E-04 轮次 1）

- 目标差异：出口用例「同进程双端完成任务 admission/execution/terminal status」由 `dual_end_task_round_trip_lands_in_a_side_state` 覆盖（A 构造并加密 → B 解密派发 → B 加密回传 → A 解密投影为任务事件）；B 的真实接纳/执行由 DEV-13 的服务用例覆盖，本用例用端口桩产出冻结形状的 `task_status`。
- 失败路径：解密/校验失败、无活租约、异 generation/异租约帧、重放帧与未知 payload_kind 一律丢弃且无副作用（`encrypted_inbound_frames_dispatch_to_session_and_task_state`、`decrypted_payloads_dispatch_and_rewrite_the_session_reference`、`replay_wrong_direction_and_tampering_fail_closed`、`foreign_lease_and_missing_capabilities_fail_closed`）。
- 敏感信息：信封不含明文或摘要（用例断言）；通道/密钥类型 Debug 脱敏；无新增日志。
- 跨 generation/lease：通道与租约同时安装、断线即清空；解密与信封校验都绑定 lease 五元组与方向；`send_authenticated_business` 未装租约即拒绝。
- 审核发现（minor，已修复）：命令层任务校验使用本地能力常量表（与真实协商可能漂移）→ 改用 `live_runtime_context`（`a899f4096`）。
- 登记缺口：`task_cancel` 只校验帧的租约与任务存在性，跨 lease 遗留任务的清理属 E2E-06（B stop/刷新的任务清理）。

### 7.3 下一步注意

- 已按第 4 节决策实现「回传 frame 的 session_id 统一改写回 A 的 ref」；B 内部权威会话 id 只存在于 `RemoteSessionMap` 与台账中。
- `role_liveness_v1` / `resume_v1` 仍未在 B 注册能力中声明，E2E-06 处理心跳时一并补齐并补协商用例（`task_v1`、`snapshot_journal_v1` 已在 E2E-03/05 补齐）。

## 8. E2E-05 实施进度（进行中，未审核）

### 8.1 已落地

| 提交 | 内容 |
|---|---|
| `88832f0f3` | A 端：解密 `journal_event`（`event_b64` 与 `event_bytes` 一致、明文 `journal_epoch`/`event_sequence`/`event_kind`/`session_id` 与事件本体逐项一致），经 `ControllingJournalSink` 端口落库到 A 独立历史（`RemoteHistoryStore::find_by_session_reference`）并广播 `desktop-remote-controller-journal`；能力声明补 `snapshot_journal_v1`（A claim + B 注册）；双端用例扩展出正文事件（assistant_delta）往返 |

### 8.2 剩余步骤（按序执行）

1. ~~**B 端正文与终态产出并回传**~~ 已完成（`352997f15`、`9dc5f9e51`）
   - `RemoteJournal::record_task_event`（落库并返回权威事件）+ `ControllerRelaySender`/`ControlledOutboundState` 出站旁路 + `build_journal_event_payload`（内部类型 → 冻结 `EVENT_KINDS` 映射，终态 `task_status` → `terminal`）+ `RemoteSessionMap::remote_ref_for` 反向改写；
   - `ControllerTaskEventSink::emit_journal` 默认端口 + `ChatTurnExecutor` 产出 `user_message`/`assistant_delta` + `EnginePoolChatTurnPort::run_chat_turn` 返回回复文本（`StoreChatTranscript` 从 SessionStore 读最后一条 assistant 文本）；
   - 命令层 `TaskEventBridge::emit_journal` 与 `DesktopTaskEventSink::publish_task_event` 都先写权威 journal，再经加密通道回传 A。
2. **缺口恢复**（剩余）：A 端发现 `event_sequence` 缺口时用 `journal_replay_request`/`session_snapshot_request`（DEV-19 已有 B 侧 API）补齐；当前只做增量事件，缺口时 A 端历史等待下一次 epoch 或 E2E-07 的显式重放。
3. **E2E-07 渲染**（剩余）：前端订阅 `desktop-remote-controller-journal`，按 `historyId` 落库并渲染 transcript；未绑定历史时先创建/绑定。

### 8.3 E2E-05 审核要点（轮次 1 待补）

- 目标差异：A 端可得到用户输入与 B 的回复——`journal_event` 解密 → 历史落库 → `desktop-remote-controller-journal`；B 端由 `user_message`/`assistant_delta`/`terminal` 三类事件产出，同进程双端用例已覆盖 assistant_delta 往返。
- 失败路径：`event_b64` 体积/声明不一致、epoch/序号/会话不符、未映射内部类型、非远程会话、通道未就位、未协商 `snapshot_journal_v1` 都静默丢弃（不产生半状态）。
- 敏感信息：正文只进权威 journal 与 A 独立历史（本地），wire 明文只有 epoch/序号/类型；无凭证、无路径、无主机信息。
- 待补证据：缺口恢复用例与前端渲染用例（分别在剩余步骤 2、3 完成）。

## 9. E2E-06 实施进度（进行中，未审核）

出口：有效 A 存活证据不超过 30 秒；停止/断开产生明确终态并清理远端任务。

### 9.1 已落地

| 提交 | 内容 |
|---|---|
| `50ed50244` | A 端心跳：`liveness::build_controller_liveness`（冻结形状 + 非法输入失败关闭）、`ControllingDeviceManager::{live_liveness_epoch, send_authenticated_control}`、保活任务按 10 秒间隔发送单调序号心跳（间隔可注入）、B 注册能力补 `role_liveness_v1`；2 条用例（帧通过冻结校验；保活任务按间隔产出心跳且不自行结束） |

### 9.2 剩余步骤

1. **B 侧存活证据链**：B 的 watchdog（DEV-08）目前只能被动收证据，需要接线——
   - 需要证据时（deadline 将尽）B 发 `controller_liveness_challenge_a` 挑战；
   - A 收到挑战后把 `controller_liveness_a` 明文（绑定连接/租约/epoch/角色/序号/挑战）
     用业务通道加密成 `encrypted_controller_liveness_a` 回传；B 解密后调用
     `ControlledDeviceManager::accept_direct_liveness` 续期；
   - 直接路径不可用时走 relay 签名兜底：B 发
     `controller_liveness_attestation_request_a` → Relay 回
     `controller_liveness_attestation_a` → `accept_liveness_attestation`。
   注意 `ControllerLivenessWatchdog` 目前只在 `accept_*` / `state` 上暴露能力，
   需要新增「何时该请求证据」的只读判断（例如 `needs_evidence(now_b)`）。
2. **显式断开**：A 的 `disconnect` 在停止 worker 前发送 `controller_disconnect`；
   B 收到后终结当前租约（清空租约/凭证/任务，保留用户工作区授权）并投影明确终态
   （保守默认：`stopped`，因为 A 需要重新刷新取码）。
3. **断线恢复门**：A/B 的 resume 序列（DEV-09 已有状态机与 Relay hooks）尚未接线；
   需要 A/B 客户端把 `controller_resume_prepare`/`encrypted_resume_confirm`/
   `controller_resume_activation_ack`/`controller_post_activation_liveness`/
   `controller_resume_ready_ack` 接到各自的 `ControllerResumeGate`，并在
   `controller_resumed` 后重新派生业务密钥（租约 epoch 变化）。
4. **B 停止/刷新清理闭环**：把 `ControlledDeviceManager::register_cleanup_sink`
   接到任务/权限/传输/临时模型注册表（`RemoteTaskHooks` 的清理入口），确保
   stop/refresh/远端断开都会清空远端任务与凭证；当前 `fail_operation` 已触发
   cleanup sinks，但 `stop`/`disconnect_now` 与远端断开路径需要逐一核对。
