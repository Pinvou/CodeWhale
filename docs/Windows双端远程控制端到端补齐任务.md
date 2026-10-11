# Windows 双端远程控制端到端补齐任务

> 任务状态：历史普通文本 pilot 计划（已被完整双端闭环取代）<br>
> 基线提交：`770a648ab4387f842dd53cee2787027c8949c31b`<br>
> 历史目标：优先打通 **A 远程控制 B 的普通文本会话**，当时未闭环能力默认隐藏。本文件保留旧任务记录，不作为当前 A08 功能范围或发布验收依据；当前边界以《Windows双端远程控制开发任务拆分》为准。A 侧离线副本统一由 SessionStore RemoteProxy 承载，不另建 history store。
>
> 历史进度：E2E-00～E2E-04 完成并审核通过；E2E-05 完成（正文链路 + 缺口恢复 + A 渲染，见第 8 节）；E2E-06 已落地 A 心跳、B 存活证据（直接应答 + Relay 兜底）与双端显式断开，剩余 resume 序列与 B 停止/刷新清理核对（见第 9 节）；E2E-07 已落地双端 UI 闭环与错误码回传，剩余浏览器冒烟与文案细化；E2E-08 待做。以下实现限制和测试结果仅记录该历史阶段，不代表当前 head。

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
| E2E-03 | 1 | PASS | A 端会话/能力/任务意图命令落地（2 个提交）：冻结 payload 与输入摘要由 Rust 构造并自检，前端只传 UI 字段；UI 状态持有 sessionId 与 capability revision。审核修复 2 处：会话重开复用引用、wire 快照映射进 UI 能力域。验证：`cargo test remote_control::` 289 通过 / 0 失败；4 个 node 契约文件 25 通过 / 0 失败；`npm run build:ui` 与 architecture-guard 通过。细节见第 6 节。 |
| E2E-04 | 1 | PASS | 加密业务帧双向派发完成（5 个提交）：共享加密通道、A 端收发接线、B 端解密派发与加密回传、bridge 发送入口；同进程双端用例覆盖 A 发 task_submit → B 解密派发 → B 回传 task_status → A 投影。审核修复 1 处：任务校验改用活租约的真实协商能力（删除本地常量表）。验证：`cargo test remote_control::` 298 通过 / 0 失败；architecture-guard 通过。细节见第 7 节。 |
| E2E-05 | 1 | PASS | 普通会话正文 journal 与 A 端 transcript 渲染完成（A 落库/投影、B 终态与正文产出、缺口回放与快照恢复、A 视图渲染）。细节见第 8.3 节。 |
| E2E-06 | 1 | PASS | liveness / 恢复 / 清理闭环完成：A 心跳与 B watchdog（直接应答优先 + Relay 签名兜底）、双端显式断开、双端 resume 序列接线（A `22929522`，B `5b42e1d8`/`4bfa63e3`/`a345b6a0`）、B 停止/刷新/远端断开的任务与凭证清理端口（`1987dd2e`）。审核发现 2 处 minor 已修复（恢复帧租约绑定、清理 scope 语义，`6e84a097`）。验证：`cargo test remote_control::` 322 通过 / 0 失败；`cargo test remote_control::controller::` 229 通过 / 0 失败；architecture-guard 通过。细节见第 9.5 节。 |
| E2E-07 | 1 | PASS | 双端 UI 闭环与能力降级完成：B 面板状态映射/连接码/主控设备名三语回落、A 会话窗口自动开会话与加密请求能力快照、发送取消走加密派发、transcript 渲染、A 端命令错误码回传、错误码与刷新状态三语文案（`8510525b`）。验证：6 个前端契约 36 通过 / 0 失败；`build:ui` 通过；`lint:ui` 0 error。发布 blocker：三面 UI 浏览器冒烟未执行（环境受限，已登记）。细节见第 10.3 节。 |
| E2E-08 | 1 | PASS | 集成回归与发布审核完成：完整旅程集成测试（`5a246985`）、门禁全套复跑与敏感信息扫描、发布缺口按「本轮 blocker / 已隐藏 / 后续排期」分类落进 `Windows双端远程控制发布与回滚.md` 第 7 节（`be913049`）。验证：Relay 58+25、前端 36、Rust `remote_control::` 323 / `controller::` 229、`build:ui`、`lint:ui`、architecture-guard、release-gates 6 条全部通过。发布 blocker：真实双机 21 条旅程与真机断线恢复演练未执行（环境受限，已登记）。细节见第 11.1 节。 |

## 4. 历史阶段已登记决策与当时已知限制

| 项 | 决策/状态 | 说明 |
|---|---|---|
| `desktop_remote_controlled_start` 阻塞等待注册结果 | 保守默认，登记 | 注册最长等待 30 秒（`REGISTRATION_TIMEOUT_MS`）；期间状态事件照常推送，UI 显示 starting。E2E-07 收敛 UI 状态时再评估是否改为「立即返回 starting + 事件收口」。 |
| `serve()` 暂只保活通道 | 预期 | 握手与加密业务帧派发分别在 E2E-02 / E2E-04 安装到该服务循环；当前对未知消息只做丢弃，绝不转发到 legacy WebUI 事件流。 |
| A 端 `device_display_name` 用有界占位 `remote-device` | 保守默认，登记 | 冻结协议 v1 的 `controller_joined` 不携带被控设备显示名（已核对 PROTOCOL.md 与 fixture）；首版用占位，真实来源需协议扩展或 E2E-03 的能力快照补充。 |
| B 端 `controller_device_name` 留空 | 保守默认，登记 | 同上：`controller_pairing_register` / `controller_joined` 都不含主控设备名；留空让 E2E-07 的 UI 用 i18n 兜底文案（`deviceUnknown`）显示，不为数据层硬编码单语名称。RC-B-006「展示主控设备名称」需协议扩展后才能真正满足。 |
| A 端不在 WebSocket 层声明 controller 子协议 | 历史限制，已修复 | **已被后续波次修复**：当前客户端通过 `relay_client::controller_websocket_request` 声明 `pinvou-desktop-controller-v1`，Relay 通过 `remote-control-relay/server.js` 的 `handleProtocols` 显式协商；以下保留当时决策。[PROTOCOL.md 的 controller-v1 子协议说明](../remote-control-relay/PROTOCOL.md#desktop-controller-protocol-v1-feature-gated) 要求 controller-v1 客户端选择 `pinvou-desktop-controller-v1`，但当时 Relay（remote-control-relay/server.js 的历史配置 `noServer: true`，无 `handleProtocols`）不回投该头部，而客户端库（tungstenite 0.30 `handshake/client.rs 的历史子协议校验`）在请求了子协议却未收到时必须失败关闭 —— 声明子协议会让 A 端完全无法连接。当时决定待 Relay 侧补齐子协议协商后再接入；当时 Relay 以 `v=1 + controller_*` 类型路由 controller lane（remote-control-relay/server.js 的历史 controller 路由）。 |
| A 端不发送 `controller_capabilities` 首帧 | 保守默认，登记 | 同上：router 没有该类型的 handler（`controller-router.js` 只有 register/claim/handshake/disconnect 与 liveness/resume hooks），发送会收到 `controller_message_unsupported` 并打断握手；E2E-01 的 B 端同样未发送。Relay 补齐后两侧一并接入。 |
| 握手期间 transport 断线 | 预期 | A 端把断线统一映射为 `endpoint_offline` 并失败关闭；transport 在断线时会作废已排队帧（`transport_epoch` 递增），因此不会用旧 claim 重连，重新 claim 属 E2E-06 的恢复链路。 |
| 连接完成后断线只投影 `reconnecting` | 预期 | `serve_controlling_lane` 在断线时调用 `mark_transport_disconnected()` 保证界面不再声称 connected；自动 resume / liveness 属 E2E-06。 |
| B 端信任根缺失在接管点失败关闭 | 保守默认，登记 | 为保持 E2E-01 的注册行为不变，B 在收到 `controller_pairing_reserved` 时才要求信任根（错误码 `relay_trust_unconfigured`）；A 端在 connect 前即失败关闭。 |
| 命令层错误码未映射到 A 对话框文案 | 历史缺口，已修复 | **已被后续波次修复**：当前 `controllerConnect` 用 `asBoundedError` 恢复 `error.code`；B 的 `ControlledStatusKind` 序列化名称与 `controlledDeviceUiState`/`ControlledDevicePanel` 的状态闭集一致。以下保留当时问题：`desktop_remote_controller_*` 当时返回有界错误码，但 `desktop-remote-control.js` 的 `controllerConnect` 未把 `Err(String)` 转成带 `code` 的错误，`connectionErrorKey` 只能拿到 undefined；B 面板与 A 壳层的状态名映射同样与后端枚举不一致（`ControlledDevicePanel.jsx 的历史状态映射` 期望 `registering`/`waiting_for_pairing`/`pairing_reserved`/`handshake_pending`，后端实际是 `reserving`/`waiting_connection`/`safety_handshake`），导致 B 面板回落到 statusIdle 且不显示连接码。当时计划由 E2E-07 统一修复并补行为测试。 |
| 能力协商按交集收敛 | 依赖 E2E-04/E2E-06 处理 | `negotiateControllerCapabilities` 取「A 请求 ∩ B 注册」；B 目前只注册 `[pairing_v1, encrypted_frames_v1, finished_receipt_v1]`，A 声明 `pairing_v1/encrypted_frames_v1/finished_receipt_v1/role_liveness_v1/resume_v1`，因此 `role_liveness_v1`、`resume_v1`、`task_v1` 都未协商成功。E2E-02 只用到 `pairing_v1`（`controller_joined` 能力绑定）所以可跑通；E2E-04 发送业务帧前必须先在两侧声明 `task_v1`（本地运行时校验会以 `unsupported_capability` 拒绝 `task_submit` 等载荷），E2E-06 同理需要 `role_liveness_v1`/`resume_v1`。 |
| A 端会话 id 由本端生成 | E2E-03 决策 | 冻结协议没有「创建会话」帧：`task_submit.session_id` 必须由发起方提供。E2E-03 由 A 生成 `session-<uuid>` 作为该远程会话在 wire 上的 id（UI 持有的「远程 sessionId」即此值），`session_ref` 只用于本地索引；B 的派发层（E2E-04）需据此绑定/创建权威会话（B 侧 `RemoteSessionMap` 支持 ref→权威 id 映射），并能把权威 id 回传（`task_status`/`session_snapshot` 都带 `session_id`）。 |
| 任务意图重试必须复用 client_request_id | 依赖 E2E-04 | `desktop_remote_controller_task_intent` 每次调用生成新的 `task_id`/`client_request_id`（一次用户提交 = 一个新意图）；断线重试/恢复时必须复用已返回的 id（B 侧 `task_id + client_request_id` 幂等），E2E-04 接线时不要把重试当成新意图。 |
| A 会话视图发送路径仍走旧 JS 拼装 | 已知缺口（E2E-07） | `历史 RemoteConversationView.jsx 的发送路径` 仍在 JS 拼一个 camelCase 伪 payload 并调用 B 侧 `desktop_remote_task_submit`（DEV-22 遗留，必然被冻结校验拒绝）。E2E-03 只提供了 A 专用构造路径与状态位；E2E-07 收敛 UI 时把发送/取消改为 `controllerTaskIntent`/`controllerTaskCancelIntent` 并接 E2E-04 的加密派发。 |
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
| `debf3aa1c` | A 端远程会话与任务请求构造：`controlling_session.rs`（会话意图 + 三个冻结 payload builder + 输入摘要 + 失败门）、7 个会话/能力/任务意图命令（历史名称见下）、`ControllerLease` 记录协商能力与 `live_runtime_context`、A/B 两侧补 `task_v1` 能力声明、前端状态与 bridge 接线、node 契约测试 |
| `8858dfe48` | 审核修复：会话重开复用 `sessionRef`（幂等）、wire 快照映射进 UI 能力域（`capabilitySnapshotForUi`），并锁定「UI 路径不出现协议字段名」 |

上述 7 条命令是 `debf3aa1c` 阶段的历史 IPC 名称：`desktop_remote_controller_session_open`、`desktop_remote_controller_session_state`、`desktop_remote_controller_session_close`、`desktop_remote_controller_capability_request`、`desktop_remote_controller_apply_capability_snapshot`、`desktop_remote_controller_task_intent`、`desktop_remote_controller_task_cancel_intent`；仅前三条属于 `session_*`。当前命令面已更名并收敛，实际注册列表以 `pinvou3-app/src-tauri/src/lib.rs` 为准；旧名称不作为调用当前 head 的依据。

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

历史口径注记：第 3 节 E2E-04 摘要的 298 与本节的 295 存在计数差异，本文件没有两次完整输出及精确采样提交，原因尚未核实；不把“期间传入其他测试”的猜测当作事实。两项原记录保留，均不作为当前验收证据；非阻塞 P3 由 Issue #1673 跟踪可追溯口径。

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

## 8. E2E-05 实施进度（历史阶段快照：当时进行中，未审核）

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

### 8.3 E2E-05 审核要点（轮次 1，结论 PASS）

- 已落地提交：`88832f0f3`（A 落库/投影）、`352997f15`（B 终态回传）、`9dc5f9e51`（B 正文产出）、`4b0b0283e`（缺口回放/快照双向）、`a8a56a490`（A 端快照恢复）、`a416ad41e`（A 视图渲染 transcript）。
- 目标差异：A 端可得到用户输入与 B 的回复——`journal_event` 解密 → A 独立历史落库 → `desktop-remote-controller-journal` → transcript 渲染（本机/远程徽标）；B 端由 `user_message`/`assistant_delta`/`terminal` 三类事件产出，同进程双端用例覆盖 assistant_delta 往返，缺口恢复另有 3 条用例（判定、回放请求冻结形状、快照替换）。
- 失败路径：`event_b64` 体积/声明不一致、epoch/序号/会话不符、未映射内部类型（进行中状态/附件/快照）、非远程会话、通道未就位、未协商 `snapshot_journal_v1` 都静默丢弃；缺口/换 epoch 时丢弃当前事件并请求回放（同会话只保留一个在途请求），快照摘要或基准不符同样丢弃。
- 敏感信息：正文只进权威 journal 与 A 独立历史（本地），wire 明文只有 epoch/序号/类型；无凭证、无路径、无主机信息；transcript 只保留最近 200 条且按 session+epoch+sequence 去重。
- 登记缺口：B 回复取轮次结束后的 SessionStore 文本（非流式）；A 端首次收到事件时直接从该事件起算（完整快照引导属可选优化）。

## 9. E2E-06 实施进度（历史阶段快照：当时进行中，未审核）

出口：有效 A 存活证据不超过 30 秒；停止/断开产生明确终态并清理远端任务。

### 9.1 已落地

| 提交 | 内容 |
|---|---|
| `50ed50244` | A 端心跳：`liveness::build_controller_liveness`（冻结形状 + 非法输入失败关闭）、`ControllingDeviceManager::{live_liveness_epoch, send_authenticated_control}`、保活任务按 10 秒间隔发送单调序号心跳（间隔可注入）、B 注册能力补 `role_liveness_v1`；2 条用例（帧通过冻结校验；保活任务按间隔产出心跳且不自行结束） |
| `8d5ae57f2` | B 端证据需求与挑战构造：`ControllerLivenessWatchdog::needs_evidence`（无证据/suspect/余量不足为真）、`build_liveness_challenge` / `build_liveness_attestation_request`（冻结校验 + 畸形输入失败关闭）、`new_liveness_challenge`（16 字节随机挑战值）、`ControlledDeviceManager::{liveness_evidence_needed, begin_liveness_challenge}`；2 条用例 |
| `e8fea59db` | B 端证据轮询与兜底：保活循环改为 `select`（2 秒轮询 + 收包），按 `liveness_evidence_action` 发起挑战 / 升级为 Relay 签名 attestation（同一挑战只升级一次）/ 超时作废；收到 attestation 后用本端记录的请求时间续期 watchdog；A 心跳间隔收紧到 3 秒以满足 Relay attestation 新鲜度；1 条策略用例 |
| `8d372e8a9` | 双端显式断开：A 端 `build_controller_disconnect`（冻结形状 + 有界 reason）与 `send_disconnect`（仅 connected 且有 transport 时尽力发送，本地断开始终权威）；B 端 `accept_remote_disconnect`（只接受已连接/恢复中的代际，随后 `terminate_local(Stopped, 保留工作区)`）与 worker 的冻结校验 + 清理 + 广播；2 条用例 |
| `4b0b0283e` | journal 缺口恢复双向：B 端 `replay_journal`/`session_snapshot` 与列表化派发（worker 逐帧加密回传、单次 ≤256 帧）；A 端 `journal_admission`/`JournalExpectations`（连续/重复/缺口/换 epoch）与 `journal_replay_request` 冻结构造 + 加密发送；4 条用例 |
| `a8a56a490` | A 端权威快照恢复：解密校验快照（体积/摘要/epoch/基准）→ 推进期望值 → 经 `ControllingJournalSink::publish_snapshot` 交命令层 `apply_snapshot`；1 条用例 |
| `94141a2e8` | A 端存活直接应答（liveness AEAD）：crypto 新增方向无关的 `liveness_root` 与 `LivenessCipher`（A 角色域 + 租约五元组派生，AAD 为冻结 `aad_binding`，nonce 前置）；EncryptedLane `seal_liveness`/`open_liveness`；A 保活任务应答 `controller_liveness_challenge_a`（共享单调序号、同一挑战只答一次）；B 解封后 `accept_direct_liveness` 续期并停止升级兜底；2 条用例 |

### 9.2 剩余步骤（历史快照，已被 9.5 及后续实现取代）

以下“尚未接线”“恢复无法完成”仅描述当时阶段，不代表当前代码。后续双端恢复接线、认证租约重装、B 心跳及首次断线冻结预算已落地；现有验证包含状态机/进程内端口与源码契约，不等于真实双 Windows、完整 B worker/Tauri/TCP 恢复旅程或发布验收。历史测试数量与审核结论保留原样，当前证据以对应 PR 的固定提交记录为准。

1. ~~**B 侧存活证据链**~~ 已完成：兜底路径（`8d5ae57f2`、`e8fea59db`）+ 直接应答路径（`94141a2e8`）；A 心跳 3 秒一次，B 优先用直接证明续期，未及时到达才升级 Relay 签名兜底。
2. **显式断开**：A 的 `disconnect` 在停止 worker 前发送 `controller_disconnect`；
   B 收到后终结当前租约（清空租约/凭证/任务，保留用户工作区授权）并投影明确终态
   （保守默认：`stopped`，因为 A 需要重新刷新取码）。
3. **断线恢复门**：A/B 的 resume 序列（DEV-09 已有状态机与 Relay hooks）尚未接线；
   需要 A/B 客户端把 `controller_resume_prepare`/`encrypted_resume_confirm`/
   `controller_resume_activation_ack`/`controller_post_activation_liveness`/
   `controller_resume_ready_ack` 接到各自的 `ControllerResumeGate`，并在
   `controller_resumed` 后重新派生业务密钥（租约 epoch 变化）。
   - 本轮已落地的前置（见 9.4）：B 注册补 `resume_v1`、恢复确认 AEAD、通道
     挂起/激活、A 端相位推进、出站帧构造、**A 端运行时循环接线**（断线即挂起
     通道并按序发出 prepare/confirm/ready_ack、接受 resumed 后重开业务闸门）。
     **剩余**：B 端运行时循环接线——B 目前仍只在断线时投影 `reconnecting`，
     未驱动本端 `ControllerResumeGate` 的 prepare/provisional/confirm/
     activation_ack/post_activation_liveness/ready_ack/resumed 序列，也未把
     `controller_resume_activation_ack`/`controller_post_activation_liveness`
     帧发到 Relay；因此当前双端断线后恢复无法完成（A 端会在预算内失败关闭并
     投影 `endpoint_offline`，属保守行为，不会留下半恢复状态）。
   - 已核对 Relay `controller-lease.js:completeResume`：恢复**不改变**
     `lease_epoch`/`connection_id` 等业务密钥派生输入，只轮换 liveness epoch、
     恢复令牌与 deadline。因此 `controller_resumed` 后无需重新派生业务密钥，
     复用同一 lease 上下文与其业务通道即可；任务文档原先「租约 epoch 变化」
     的假设不成立，此处按实测行为登记更正。
4. **B 停止/刷新清理闭环**：把 `ControlledDeviceManager::register_cleanup_sink`
   接到任务/权限/传输/临时模型注册表（`RemoteTaskHooks` 的清理入口），确保
   stop/refresh/远端断开都会清空远端任务与凭证；当前 `fail_operation` 已触发
   cleanup sinks，但 `stop`/`disconnect_now` 与远端断开路径需要逐一核对。

### 9.3 已落地（补充）

| 提交 | 内容 |
|---|---|
| `8d372e8a9` | 显式断开闭环（见 9.1 说明） |

### 9.4 本轮 resume 接线进展（E2E-06 第 3 步）

| 提交 | 内容 |
|---|---|
| `f4c3ea9b` | B 端注册声明 `resume_v1`；新增「A claim 与 B 注册能力族一致」用例，锁定七族交集不再静默缺项 |
| `c686550d` | 恢复原语：`ResumeConfirmCipher`（resume 秘密 AEAD，AAD 绑租约五元组 + activation_id，明文带域标签/activation_id/发送角色，并派生恢复后轮换指纹）；`EncryptedLane` 保留密钥材料并新增 `suspend`/`activate`（恢复期业务与存活帧失败关闭，确认帧不受影响）；敏感恢复队列放行 `controller_resume_ready_ack`；`ControllerResumeGate` 补 A 角色相位推进（Relay confirmed → PostActivationLiveness，B 侧幂等） |
| `75b30972` | 恢复出站帧纯构造：`new_resume_challenge`/`build_controller_resume_prepare`/`build_controller_resume_ready_ack`，逐帧过冻结 schema，畸形输入失败关闭 |
| `22929522` | A 端运行时接线：断线改为「挂起通道 + 启动恢复」（不再 `clear`）；`ControllingLane::send_resume_control`（生产走敏感恢复队列）；manager 新增 `begin_resume`/`advance_resume`/`current_operation`/`send_resume_control` 与带 `activation_id` 的 `complete_resumed`；恢复预算 30 秒内未完成、相位错乱、对端确认失败或收到 Relay `error` 都会清空通道并投影 `endpoint_offline`；新增整条恢复序列用例（prepare/confirm/ready_ack 顺序 + resumed 后重开业务）与断线即发 prepare 的断言 |

验证：`cargo test remote_control::` 320 通过 / 0 失败；`cargo test remote_control::controller::` 229 通过 / 0 失败；`python scripts/architecture-guard.py` 通过。

### 9.5 E2E-06 正式审核记录（轮次 1，结论 PASS）

审核提交范围：`50ed50244`、`8d5ae57f2`、`e8fea59db`、`8d372e8a9`、`4b0b0283e`、`a8a56a490`、`94141a2e8`、`f4c3ea9b`、`c686550d`、`75b30972`、`22929522`、`5b42e1d8`、`4bfa63e3`、`a345b6a0`、`1987dd2e`、`6e84a097`。

- 目标差异核对：出口「有效 A 存活证据不超过 30 秒」由 A 端 3 秒心跳（`94141a2e8`）+ B watchdog 的直接应答优先 / Relay 签名兜底（`8d5ae57f2`、`e8fea59db`）覆盖；「停止/断开产生明确终态并清理远端任务」由双端显式断开（`8d372e8a9`，B 终结为 `stopped` 并保留用户工作区）与清理端口（`1987dd2e`：`RemoteTaskService::interrupt_generation` → 任务生命周期钩子回收临时模型凭证 / 待决权限 / 传输暂存）覆盖。断线恢复门（本任务此前唯一未实现的协议族）已双端接通：A 端 `22929522`，B 端 `5b42e1d8`/`4bfa63e3`/`a345b6a0`；两端都按 `disconnect → prepare → provisional → confirm → activation_ack → post_activation_liveness → ready_ack → resumed` 驱动各自 `ControllerResumeGate`。
- 关键失败路径：本地 30 秒预算耗尽、相位错乱、对端 CONFIRM AEAD 验签失败、`controller_resumed` 材料复用（新令牌/指纹与旧值相同）、Relay `error` 帧——A/B 各有真实用例断言「清空通道 + 投影有界错误（A `endpoint_offline` / B `desktop_remote_resume_invalid`）+ 不留半恢复状态」。B 的 watchdog 仍是恢复期间的时间权威（恢复不得延长 A 的存活截止时间）；恢复期间业务帧一律不派发（A `business.suspend()`，B 的 `serve` 只走恢复处理函数）。
- 敏感信息：恢复令牌只出现在内存与冻结帧内（`controller_resume_prepare.resume_token_b64` 是协议要求）；`ControllingResumeMaterial`/`ControlledResumeMaterial`/`ControlledResumeAttempt` 的 Debug 全部脱敏且不含令牌字段；CONFIRM 帧用握手 `resume` 秘密做 AEAD（AAD 绑租约五元组 + activation_id）；无新增日志/遥测调用点。
- 跨 generation / lease 边界：恢复帧必须绑定当前租约五元组（见下方修复 1）；两端 gate 都绑定租约与原始 deadline，恢复材料一次性轮换；已核对 Relay `controller-lease.js:completeResume`——恢复**不改变**业务密钥派生输入，因此复用同一业务通道而不重新派生密钥（原假设已在 9.2 更正）。
- 审核发现与处置（均为 minor，已在 `6e84a097` 修复）：
  1. 入站恢复帧只按类型与 activation_id 处理，未校验租约/代际绑定 → 新增 `resume_frame_matches_lease`：旧代际/旧租约残留帧一律丢弃（不推进相位、也不终止候选，避免被被动 DoS），并补 B 端回归断言。
  2. B 端清理端口忽略 `ControlledCleanupScope` → 只有涉及远端任务/待决权限/临时模型的清理才中断任务，纯凭证清理不再顺带打断任务。
- 定向测试与守卫：`cargo test remote_control::` 322 通过 / 0 失败（含 A 端整条恢复序列 + 断线即发 prepare + B 端五帧顺序与旧代际丢弃 + B 端相位序列与顺序错乱失败关闭 + 三条清理路径）；`cargo test remote_control::controller::` 229 通过 / 0 失败；`python scripts/architecture-guard.py` 通过。
- 登记缺口（非本轮 blocker）：① 真机双端断线恢复演练未执行（环境限制，随 E2E-08 登记为发布缺口）；② `DesktopControlledCleanup` 适配器无 Tauri 级测试（本仓库无 `mock_app` 先例），其触发面由 manager 侧 stop/refresh/远端断开三条路径用例覆盖，适配器逻辑为 `try_state` + `interrupt_generation` 直连。
- 结论：**REVIEW: PASS**。下一步进入 E2E-07 剩余项（浏览器冒烟与正式审核）。

## 10. E2E-07 实施进度（历史阶段快照：当时进行中，未审核）

出口：三个 UI 面有可执行行为测试；不可用能力不暴露半通路。

### 10.1 已落地

| 提交 | 内容 |
|---|---|
| `a416ad41e` | 双端 UI 最小闭环与能力降级：B 面板状态映射补齐后端枚举（连接码按状态显示、disabled 不再算 active、主控设备行改用 `controllerDeviceName` + 三语回落）；状态域新增 `controllerDeviceName` 与 `transcript`（去重/只收正文/快照替换/上限 200）；A 会话窗口连接后自动开远程会话并加密请求能力快照，发送/取消改走 `controllerTaskSend`/`controllerTaskCancelSend`，渲染 transcript，A 临时模型与模型导出列表按首版开关隐藏；新增 `desktop_remote_controller_capability_send` 命令与 bridge 入口；新增 `remote_control_ui_closure` 契约（4 条）并更新 `remote_conversation_contract`（禁止旧的自建 payload 路径） |
| `582cba938` | A 端命令错误码回传：bridge `asBoundedError` 把 Tauri `Err(String)` 恢复成带 `.code` 的错误，连接/断开/会话/能力/任务/取消/快照路径统一套用；契约新增逐条断言 |
| `8510525b` | 文案细化（10.2 的第 2、3 项）：`connectionErrorKey` 补 `endpoint_offline` → `errorOffline`、`invalid_message` → `errorProtocol`（三语齐全，不再回落通用文案）；B 面板 `Refreshing` 改用面板既有的 `refreshing` 词条，不再借用 `statusStopping`；契约补「新码三语齐全」与「刷新文案回归」断言 |

### 10.2 剩余步骤

1. **浏览器冒烟**（DEV-21 登记的缺口）：三个 UI 面（B 面板、A 连接壳、远程会话窗口）需要一次真实渲染/交互冒烟（键盘、焦点、aria-live、倒计时节流），当前证据仍为源码契约 + `vite build`。
2. ~~**文案细化**~~ 已完成（`8510525b`）：`endpoint_offline` → `errorOffline`、`invalid_message` → `errorProtocol`，均补齐 zh/en/ja，并由契约锁定。
3. ~~**`ControllerStatusKind::Refreshing` 的 UI 文案**~~ 已完成（`8510525b`）：显式复用面板既有的 `refreshing` 词条（不再映射 `statusStopping`），契约新增回归断言。

### 10.3 E2E-07 审核记录（轮次 1，结论 PASS，含 1 项发布 blocker）

- 出口核对：「三个 UI 面有可执行行为测试」由 6 个前端契约文件 36 条用例覆盖（`desktop_remote_state`、`connect_remote_device_contract`、`controlled_device_panel_contract`、`remote_control_ui_closure`、`controller_session_intent_contract`、`remote_conversation_contract`），`npm run build:ui` 与 `npm run lint:ui`（0 error）通过；「不可用能力不暴露半通路」由 A 临时模型/代码模式/附件与成果导出按首版开关隐藏、工具审批未闭环前禁用体现，且契约禁止 UI 出现协议字段名。
- 本轮修复：`8510525b`（A 端有界错误码与刷新状态的三语文案）。
- 发布 blocker（环境受限，未执行）：三个 UI 面的**浏览器冒烟**（真实渲染/交互、键盘与焦点、aria-live、倒计时节流）。当前证据是源码契约 + vite 构建；已登记到 `Windows双端远程控制发布与回滚.md` 第 7.1 节，未取得冒烟证据前不得进入灰度。
- 结论：**REVIEW: PASS**（代码与契约侧）；浏览器冒烟作为发布 blocker 单独登记。

## 11. E2E-08 待做清单（集成回归与发布审核）

本节测试数量与待做项是该历史轮次的快照，不是当前提交的验收结果；当前覆盖率以对应提交的实际测试输出与 CI 为准。

- 较早历史快照（A12 留存）：Relay 契约套件 `node --test test/controller-protocol|controller-handshake|controller-reservation|controller-liveness|controller-resume|controller-router` 58 通过 / 0 失败，`test/relay.test.js` 25 通过 / 0 失败；Rust `cargo test remote_control::` 312 通过 / 0 失败；前端 6 个契约文件 36 通过 / 0 失败、`npm run build:ui` 通过、`npm run lint:ui|lint:node` 0 error；`python scripts/architecture-guard.py` 通过；敏感信息扫描（`sk-*` / `vault://` / `Bearer`）在新增的 controller 与前端文件中零命中。

- 历史复跑证据（E2E-06 收口后）：Relay 契约套件 `node --test test/controller-protocol|controller-handshake|controller-reservation|controller-liveness|controller-resume|controller-router` **58 通过 / 0 失败**，`test/relay.test.js` **25 通过 / 0 失败**；Rust `cargo test remote_control::` **322 通过 / 0 失败**、`cargo test remote_control::controller::` **229 通过 / 0 失败**；前端 6 个契约文件 **36 通过 / 0 失败**、`npm run build:ui` 通过、`npm run lint:ui` 0 error；`python scripts/architecture-guard.py` 通过；敏感信息扫描（`sk-*` / `vault://` / `Bearer` / `-----BEGIN`）在 controller 与前端文件中仅命中**刻意的脱敏测试夹具/哨兵**（`journal.rs` 断言投影剥离 `vault://secret`、`telemetry.rs` 哨兵表、`temporary_model.rs` 反向用例），无真实凭证。
- 发布缺口登记（已完成）：`docs/Windows双端远程控制发布与回滚.md` 新增第 7 节，把第 4 节全部登记项与本节缺口按「本轮 blocker（真实双机旅程、三面 UI 冒烟、真机断线恢复演练）/ 已隐藏（临时模型·代码模式·附件·成果、工具审批、Relay 子协议与 `controller_capabilities`）/ 后续排期（设备显示名、非流式回复、快照引导、附件事件、跨 lease 清理、清理适配器测试、无关既有失败）」分类。

### 11.1 E2E-08 审核记录（轮次 1，结论 PASS，发布 blocker 已登记）

本节 `controller-release-gates` 的 6 条是 `be913049` 历史提交中的实际 `test()` 数量；当前文件有 7 条，不用历史计数代替当前执行证据。

- 已落地提交：`be913049`（门禁证据复跑 + 发布缺口分类）、`5a246985`（完整旅程集成测试）。
- 目标差异核对：出口「普通文本会话端到端测试通过」由 `full_journey_connect_snapshot_task_text_and_disconnect_drops_stale_frames` 覆盖——连接 → 能力快照（revision 4 落进会话状态）→ A 构造并封帧的普通文本任务（B 解封校验 payload 与正文 `hello from A`）→ B 的 `task_status` 与 `journal_event` 回流并投影为任务/正文事件（`succeeded` + `hello from B`）→ 显式断开（`disconnected` + 业务通道清空）→ 断开后旧 generation 的残留帧不再产生任何投影。分段用例另覆盖 A↔B 真实端口派发（`dual_end_task_round_trip_lands_in_a_side_state`）、缺口回放与快照替换、恢复序列与显式断开。
- 门禁与安全：Relay 契约 58 + `relay.test.js` 25、前端契约 36、Rust `remote_control::` 323 / `controller::` 229、`build:ui`、`lint:ui`（0 error）、`architecture-guard`、`controller-release-gates` 6 条全部通过；敏感信息扫描仅命中刻意脱敏的测试夹具/哨兵，无真实凭证。
- 出口「所有剩余缺口明确非本轮 blocker 或已隐藏」：已按 `Windows双端远程控制发布与回滚.md` 第 7 节分类——3 项发布 blocker（真实双机 21 条旅程、三面 UI 浏览器冒烟、真机断线恢复演练，均环境受限未执行）、3 类已隐藏能力、7 项后续排期。
- 审核发现：无 blocker；本轮新增测试未发现缺陷。唯一非阻塞观察：旅程测试中 B 侧的接纳/执行用冻结形状的直接产出替代（真实路径由 DEV-13 用例覆盖），已在测试文档注释中写明。
- 结论：**REVIEW: PASS**（自动化门禁与集成回归侧）；发布 blocker 以 `Windows双端远程控制发布与回滚.md` 第 7.1 节为准，未取得证据前不得进入灰度。
- 双端集成测试：在现有同进程双端用例（task_submit/task_status、journal_event、快照、心跳证据）基础上补一条完整旅程（连接 → 能力快照 → 普通文本任务 → 正文回传 → 显式断开），并断言旧 lease/generation 帧在各阶段都被丢弃。
- 架构守卫与 UI 契约：`python scripts/architecture-guard.py`、`npm --prefix pinvou3-app run build:ui`、`npm --prefix pinvou3-app run lint:ui`、前端契约测试全套。
- 安全扫描：仓库已有的 `controller-telemetry`/日志脱敏用例 + `remote-control-relay` 契约测试；再跑一次敏感信息扫描（密钥/路径/内部地址不得进入 wire 与投影）。
- 发布缺口登记：把第 4 节的全部登记项按「本轮 blocker / 已隐藏 / 后续排期」分类，并更新 `docs/Windows双端远程控制发布与回滚.md` 的门禁清单。

## 12. 恢复点与实现地图（跨回合交接）

> 本节的唯一用途：让任何新的执行回合无需重新侦察即可原样接上；每完成一步就更新「恢复点」。

### 12.1 恢复点（NEXT）

- **当前恢复点**：E2E-06 第 4 项「B 停止/刷新清理闭环」——第 2 项（B 端恢复循环）已由 `5b42e1d8`、`4bfa63e3`、`a345b6a0` 完成：`serve` 的 joined 分支接
  `desktop_relay_connection{connected:false}` → 挂起通道 + `mark_transport_disconnected` + prepare，恢复期间所有帧只走
  `handle_controlled_resume_frame`，成功后 `activate` 并恢复存活性轮询。
- **下一步动作**：把 `ControlledCleanupSink` 接到生产注册表（任务/权限/传输/临时模型），确保
  `stop`/`refresh`/远端断开三条路径都清理远端任务与凭证；核对 `terminate_local`、`disconnect_now`、`accept_remote_disconnect`
  与 `fail_operation` 各自触发的清理范围，并补一条三种路径都触发清理的用例。之后进入 E2E-06 正式审核。
- **（历史记录）B 端恢复循环当时的接线要点**：在 `ControlledRelayWorker::serve`（`controller/relay_runtime.rs` 中的 `ControlledRelayWorker::serve`）的 joined 分支接恢复：
  用 `desktop_relay_connection{connected:false}` 触发 `mark_transport_disconnected(now_b)` + `begin_resume` + 发
  `controller_resume_prepare`；按 §12.1 的相位映射处理 provisional / encrypted_resume_confirm /
  activation_pending / post_activation_liveness_confirmed / controller_resumed，完成后 `activate` 业务通道；
  B 的三个出站帧用 `build_controller_resume_activation_ack` / `build_controller_post_activation_liveness` /
  `build_controller_resume_ready_ack`（B 的 liveness epoch 用 `live-b-<uuid>`，必须与旧值不同）。
  测试建议：把这段逻辑抽成 `relay_runtime` 内的纯函数（输入端口 + manager + lanes + 帧字符串），用 `FakePort` 驱动整条
  序列，再让 `serve` 调它。
- **B 端断线信号（已确认，无需再侦察）**：`RemoteControlManager::handle_connection`
  （`manager/mod.rs` 中的 `handle_connection`）在 socket 状态变化时先 `forward_controller_relay_message` 投递
  `{"type":"desktop_relay_connection","endpoint_id":…,"connected":false,"error":…}`，再更新 endpoint 状态；
  worker 的 joined 分支目前忽略该帧。B 的通道本身不会关闭（持久 endpoint socket），因此**必须**用这个事件进入恢复，
  不能等 `recv()` 返回 `None`。
- **B 端出站**：worker 已有 `self.channel.try_send(Value)`（`manager/mod.rs` 中的 `send_controller_message`），
  恢复帧直接走它即可；`build_controller_resume_prepare`/`build_controller_resume_ready_ack`（`resume.rs`）自带冻结校验，
  确认帧由 `ControlledLaneState::seal_resume_confirm`（`encrypted_lane.rs`）产出，因此 B 端**不需要**新增敏感队列入口
  （A 端当初需要是因为它走 WS transport 的通用队列）。
- **B 端相位映射**（Relay `controller-resume.js` 实测顺序）：`disconnect` → `prepare` → 收到 provisional 后
  `accept_provisional` 并回 `encrypted_resume_confirm` → 收到对端 confirm 后 `confirm` → 发
  `controller_resume_activation_ack` 时 `acknowledge_activation` → 收到 `controller_resume_activation_pending` 后发
  `controller_post_activation_liveness` 时 `send_post_activation_liveness(epoch_b)`（epoch 必须与旧值不同）→ 收到
  `controller_post_activation_liveness_confirmed` 后发 `controller_resume_ready_ack` 时 `acknowledge_ready` →
  收到 `controller_resumed` 后 `accept_resumed` 并重建 gate/watchdog（`controller/controlled.rs` 中的 `complete_resumed` 已有目标状态形状）。

### 12.2 实现地图

| 关注点 | 文件与入口 |
|---|---|
| A 端状态机 | `controller/controlling.rs`（`ControllingDeviceManager`、`ControllerLease`、`ResumeStep`） |
| A 端运行时 | `controller/controlling_service.rs`（`connect_controlling_device`、`serve_controlling_lane*`、`handle_resume_frame`） |
| B 端状态机 | `controller/controlled.rs`（`ControlledDeviceManager`、`ControlledLease`、`mark_transport_disconnected`、`complete_resumed`） |
| B 端运行时 | `controller/relay_runtime.rs`（`ControlledRelayWorker::{start,register_pairing,serve}`、`dispatch_business_frame`、`build_journal_event_payload`） |
| 加密通道 | `controller/encrypted_lane.rs`（`EncryptedLane`、`*LaneState::{install,clear,suspend,activate,seal,open,seal_resume_confirm}`） |
| 恢复状态机/构造 | `controller/resume.rs`（`ControllerResumeGate`、`build_controller_resume_*`、`new_resume_challenge`） |
| 冻结协议 | `remote-control-relay/PROTOCOL.md`、`protocol-controller.js`、`fixture/controller-protocol-v1.json`、`controller/resume.rs` 的 schema 自检 |
| Relay 恢复驱动 | `remote-control-relay/controller-resume.js`、`controller-router.js`（hooks）、`controller-lease.js:completeResume` |
| B 端断线来源 | `manager/mod.rs` 中的 `handle_connection` → `desktop_relay_connection` 帧 |
| 测试夹具 | `controller/test_support.rs`（`pairing_fixture`/`paired_session_keys`/`lease_context`/`signed_reservation`） |

### 12.3 每步固定验证命令（改动范围决定取哪几条）

```text
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib remote_control::
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib remote_control::controller::
python scripts/architecture-guard.py
npm --prefix pinvou3-app run build:ui
npm --prefix pinvou3-app run lint:ui
node --test pinvou3-app/tests/<相关契约>.test.mjs
```

**UI 改动的强制项（2026-09-20 补充）**：任何改动设置页/面板的提交，除源码契约测试外必须跑
`npm --prefix pinvou3-app run build:ui && npm --prefix pinvou3-app run test:settings-ui`
（当前为 Puppeteer 加载真实 Vite dist、mock Tauri 的浏览器渲染及错误边界断言，不是 jsdom，也不是双机或真实宿主验收）。历史原因：`ControlledDevicePanel` 里新增的
`bridge.desktopRemoteControl.endpointProfiles()` 在缺少该方法的宿主实现上是**同步抛错**，会让整页被
错误边界接住（用户侧表现为「设置页面加载失败」），而源码契约测试与 `build:ui` 都发现不了——只有
运行时渲染冒烟才能覆盖此类问题。该记录描述当时故障；当前 B 面板不暴露端点切换，不要求恢复旧下拉。执行时须确认浏览器用例实际运行；浏览器缺失、启动失败或任何 SKIP 都不得记为验证通过。

## 13. 生产打通与端上排障记录（2026-09-20）

> 历史排障快照：下述线上状态、临时诊断接口、旧包与 328～331 条测试数字仅记录当时阶段，不是当前 head 验证或最新部署证据。当前恢复链路、B 面板边界与正式发布要求以现行拆分文档及后续审核修复为准；未发布桌面协议随 A/B/Relay 一起升级，不保留旧开发接口兼容路径。production 的 development+TOFU 迁移与关闭须遵守现行发布与回滚文档的硬截止：下一次新部署、重部署或发布前，取得 registry 100% 覆盖与全部公共入口关闭 TOFU 的实际证据。

### 13.1 生产 controller lane 已启用

生产 relay（8787）已部署含 controller-v1 的版本并启用 `PINVOU_DESKTOP_CONTROLLER_ENABLED=1`（`development + TOFU`，安全降级经用户确认），
配置、密钥、验收与回滚见 `Windows双端远程控制发布与回滚.md` 第 8 节。

### 13.2 排障中修复的 6 个端上缺陷（全部为客户端，均有提交与回归证据）

| 现象 | 根因 | 修复 |
|---|---|---|
| 点「开启远程控制」报 `desktop_remote_control_unsupported` | 面板可见性用 `shared/platform.js` 的 `can`（桌面端“非 false 即允许”），bridge 注入的 `can` 却要求显式 `true` | 统一 bridge 的能力语义（`33c6a6515`） |
| 日志出现 `registration_sent` 后 30 秒超时 | `controller_relay_message` 白名单漏了 `desktop_pairing_registered`，relay 生成的连接码回包从未进入控制通道（relay 侧 registrations 已增长） | 放行该结果帧并补回归用例（`f952553bd`） |
| 端点通道偶发 20 秒未就绪即失败 | 注册前未等待端点 socket 认证为 B，且就绪等待无重试 | `wait_endpoint_ready` + 注册有界重试（幂等复用 `registration_request_id`）（`ac421eb90`、`eac888b3f`） |
| 面板只显示 `(error)` / `(desktop_remote_control_failed)` | Tauri 对 `Err(String)` 直接抛字符串，bridge 只读 `code/message` 导致真实有界码被兜底吞掉 | `asBoundedError` 支持字符串并面向前端透出（`84d2ec1b6`、`eac888b3f`） |
| 后端已 `waiting_connection` 但界面无码 | 状态代际门只认旧拼写 `registering/waiting_for_pairing`，新 generation 的 `waiting_connection` 被丢弃 | `starting` 接受 `reserving/waiting_connection`（`ddc376767`） |
| 同上（更深一层） | 域切片 `snapshotStateSlice` 读顶层键，而 feature 只把状态挂在嵌套键 `desktopRemoteControl` → `bs.controlledDevice` 恒为 `undefined` | `publish` 时把 7 个切片镜像到顶层，并在安装时镜像初始状态（`2edc456cf`；注意必须在 `STATE_SLICES` 初始化之后调用，见 `0fba53786` 的回归修复） |

### 13.3 当时的端上排障能力（历史记录）

当前已移除临时 `desktop_remote_client_log` / `clientLog` 自由文本接口；前端仅经 `desktop_remote_client_event` / `clientEvent` 提交闭集 event/detail，Rust 日志 sink 再以字段白名单收敛未知值为 `other`。旧 `profile/source/path`、`origin` 等拼接详情不保证留存，不能把下列历史日志描述当成当前日志合同。Rust 双端日志仍在，但轮转、容量、开关、异步写入及跨进程/文件权限验收另行跟进，不因本记录视为完成。

- `~/.pinvou3/logs/remote-control.log`：B 端（`[controlled]`）起停/注册/握手/端点就绪/relay 拒绝码，以及前端 `event`/`result`/`ui` 三类投影
  （`desktop_remote_client_log` 命令 + bridge `clientLog`）。只写阶段与有界值，不含 endpoint/lease/token。
- A 端（`[controller]`）：`connect_requested`（含 `profile/source/path` 端点标记）、`connect_rejected`、`connect_started`、
  每次状态投影 `status`（与界面同源的 `state`/`error`）、`connected` / `connect_failed`（有界结果码）。
- 验收记录（2026-09-20 16:29）：`registered=waiting_connection` 与前端 `event/result/ui` 四层一致 `code=yes`，被控端成功显示 7 位连接码。

### 13.4 剩余

- A 端用该连接码连接并完成普通文本任务（真机或第二实例；同机受单实例插件限制）。
- E2E-07 浏览器冒烟、E2E-08 完整旅程与发布缺口登记、以及关闭 TOFU 的前置（端点清单 + registry 迁移）。

### 13.5 A 端「连接码无效或已过期」根因与 A 端日志补齐（2026-09-20）

- **现象**：使用另一台设备（B 端，连接码注册在**生产** Relay）的连接码，在**未过期**的情况下仍报
  「连接码无效或已过期」（映射自 Relay 的 `pairing_code_invalid`）。
- **根因**：本机 A 端残留 **User 级**环境变量 `PINVOU_REMOTE_PUBLIC_URL` / `PINVOU_REMOTE_RELAY_WS_URL` /
  `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64`（指向测试端点
  `/pinvou3/remote-test`），且运行中的 dev 实例继承了这份环境。于是 A 端把 claim 发到测试 Relay，而 B 的码
  注册在生产 Relay，两端不在同一实例 → claim 必然 `pairing_code_invalid`。
  生产 Relay 的健康检查（`endpoint_count=29 / desktop_open_count=16`）与测试 Relay（`endpoint_count=0 /
  ws_connection_count=1（未认证）`）互相对照即是证据。
- **修复**：清除本机 User 级覆盖变量并以干净环境重启；同时补 A 端有界排障日志（此前 A 端完全没有落盘痕迹，
  失败只能从界面文案反推，这是本次排查最耗时的原因）。日志写入器泛化为 `log_relay_event(scope, …)`，
  `controlled` / `controller` 两个 scope 共用同一份有界规则与同一文件。
- **保守默认（登记）**：桌面生产客户端不得遗留未经确认的测试地址、profile 或 pin 覆盖；自建 Relay 的显式地址与信任根配置仍允许，需成对正确配置并核对持久自定义地址。该约束不禁止 Relay 服务端必需的部署密钥环境。生产客户端无覆盖或自定义设置时默认用内置 `production`；当前日志闭集行为见 13.3，不能依赖历史拼接详情排障。
- **验证**：`cargo test remote_control::` **328 通过 / 0 失败**（新增 3 条：A/B 日志行格式、有界洗白、端点路径脱敏）；
  `cargo test remote_control::controller::relay_runtime` **17 通过 / 0 失败**；
  `cargo test remote_control::manager::persistence` **1 通过 / 0 失败**；`python scripts/architecture-guard.py` 通过。

### 13.6 开发入口不再单独覆盖 Relay 地址（同类根因收口）

- **同类隐患**：`pinvou3-app/run-dev.sh` 原先只 `export PINVOU_REMOTE_PUBLIC_URL` /
  `PINVOU_REMOTE_RELAY_WS_URL`（指向 remote-test），**不设置**信任根，于是 controller 握手会用内置
  production 信任根去验 remote-test 的 transcript，必然失败关闭；同时这与
  `docs/PINVOU-开源执行代办.md` 中「移除生产远控默认地址」的登记项同源。
- **修复**：改为选中内置 `test` profile（`PINVOU_REMOTE_ENDPOINT_PROFILE=test`），地址与信任根同源；
  在无更高优先级 runtime/URL/pin 环境覆盖或持久自定义地址时，可用 `PINVOU_REMOTE_ENDPOINT_PROFILE=production` 切回生产；仅切 profile 不清除这些来源。地址级覆盖仍保留给自建 Relay，
  并在脚本注释与 `docs/remote-e2e-test-endpoint.md` 中写明「只覆盖地址不会同步换信任根」。
- **验证**：`node --test pinvou3-app/tests/tauri_platform_layout.test.js` 通过；
  `node --test pinvou3-app/tests/knowledge_host_packaging.test.mjs` 通过；`python scripts/architecture-guard.py` 通过。

### 13.7 真机首次 A↔B 握手失败：Relay 自帧回投未被两端过滤（已修复）

- **现象**：A 端拿到 `controller_pairing_reserved`（日志 `status state:safety_handshake`）后约 40 秒以
  `endpoint_offline` 收尾；B 端界面报错，B 端日志为
  `[controlled] stage=handshake_failed detail=controller_handshake_rejected`。
- **根因**：Relay router 契约会把**发送方自己的** `finished_receipt_commit` **原样回投**给发送方
  （`remote-control-relay/test/controller-router.test.js` 的 `finished_receipt_commit` 回投断言明确要求 socket-A 与 socket-B 都收到该帧；
  `remote-control-relay/controller-router.js` 的 `handle()` 先投发送方再投对端）。两端此前都把该类型的入站帧当作对端 commitment 处理，
  于是 `sender_role` 与本端不符 → B 端在发出自己的 commit 后立刻收到回投并 `controller_handshake_rejected`
  结束整条通道，A 端随后因对端离线收到 `endpoint_offline`。E2E-02 此后一直只有单测（无 Relay 回投），
  真机首跑才暴露。
- **修复**：`protocol.rs` 新增 `is_self_echoed_handshake_frame(message, own_role)`（只按本端角色匹配
  `finished_receipt_commit` / `encrypted_handshake_finished`）；A 端 `next_of_type` 与 B 端预握手分派
  都跳过自帧回投并继续等待，绝不吞掉对端帧，其余类型的分派与失败关闭语义不变。
- **回归**：A 端单测夹具（`ControlledPeerPort`）改为按真实链路注入自帧回投再推进握手；B 端新增自帧判定用例
  （对端帧/预留/joined/加密业务帧均不受影响）。
- **验证**：`cargo test remote_control::` **330 通过 / 0 失败**；`python scripts/architecture-guard.py` 通过。
- **端上影响（登记）**：B 端必须安装含本修复的包才能通过握手；对方设备上的旧包会稳定停在这一步。

### 13.8 真机 A↔B 第二处失败：`finished_receipt_ack` 误传本端 receipt（已修复）

- **现象**：自帧回投修复后 A 端推进到 `safety_handshake`，随即失败，界面提示
  「对方返回的数据无法识别，请重试；若持续出现请检查双方版本」（= `<code>invalid_message</code>`）。
- **根因**：Relay 的收据校验要求 `finished_receipt_ack` 携带 **原 Finished 发送方**（帧中的
  `sender_role`）生成的 receipt secret；ACK 提交方是 `receiver_role`，即收到 Finished 并解开对端 receipt 的一侧。
  `remote-control-relay/controller-lease.js` 的 `acceptReceipt()`
  先按 `sender_role` 取对端先前的 commitment，再用帧里的 secret 重算比对；契约样例见
  `remote-control-relay/test/controller-handshake.test.js` 的 `receiptMessage()` 及其调用用例（A 的 receipt 由 B 提交、B 的
  receipt 由 A 提交）。两端实现却都回传了**本端** receipt，于是 Relay 报 `finished_receipt_invalid`；
  该码不在端上公共错误闭集内，被兜底成 `invalid_message`，界面文案因此指向「数据无法识别」。
- **修复**：两端 `accept_peer_finished` 改为回传刚从对端 Finished 解开的 `peer_receipt`；同步修正
  `crypto.rs` 里 `to_base64` 的误导性注释（原注释写成「回传本端 receipt」，正是缺陷来源）。
- **回归**：新增与 Relay 同构的校验用例（`receipt_ack_carries_the_peer_receipt_matching_its_commitment`）：
  用 ack 里的 secret 按 `sender_role` 重算 commitment，必须等于对端先前提交的 commitment；反向用例
  确认「拿本端 receipt 当对端 receipt」必然对不上（即修复前的行为）。
- **验证**：`cargo test remote_control::` **331 通过 / 0 失败**；`python scripts/architecture-guard.py` 通过。
- **端上影响（登记）**：两端都需安装含本修复的包；只有一端更新时仍会在收据阶段失败。

### 13.9 A 端失败归因日志（Relay 原始码 + 本地握手码）

- **动机**：13.7/13.8 两轮都出现了「公共码掩盖真实原因」：闭集外的 Relay 错误码会被
  `public_failure_code` 兜底成 `invalid_message`，单看界面文案分不清是 Relay 拒绝还是本地校验拒绝。
- **改动**：A 端 transport 收到 `error` 帧时把**原始有界码**写入排障日志（`stage=relay_error`），
  并记录是否收到过 Relay 错误帧；失败时补两行：
  `stage=handshake_failed detail=<本地握手码>` 与
  `stage=connect_failed detail=<公共码>/origin:relay|lane|local`。
  所有值仍是协议枚举名，不含文案、连接码或凭据。
- **验证**：`cargo test remote_control::` **331 通过 / 0 失败**；`python scripts/architecture-guard.py` 通过。

### 13.10 当前非阻塞跟进（不算已完成）

早期断线合成帧的重试分类与 B 面板错误投影已由后续修复覆盖；跨 Relay epoch 的注册复用/迟到响应合同仍由 [#1405](https://github.com/Pinvou/pinvou3/issues/1405#issuecomment-5972681019) 跟踪，不能接受旧 epoch 来恢复注册。[#1431](https://github.com/Pinvou/pinvou3/issues/1431)（P2）保留日志容量、同步 IO、跨进程与权限验收；不恢复自由文本 clientLog。[#1430](https://github.com/Pinvou/pinvou3/issues/1430#issuecomment-6089621468)（P3）补充空 URL 环境变量的选择/来源标记不一致，连接仍失败关闭，不是认证绕过；未知 profile 合同仍开放。这些跟进不代表正式发布或双机验收通过。

全量 `cargo test` 一次编译约 1.5–5 分钟：同一模块的多处改动应攒成一次编译，任务边界才跑全量回归。
