# Windows 双端远程控制代码架构

> 本文描述当前桌面双端远程控制的代码结构、状态所有权和业务调用流程，并给出后续无行为变更的整理方向。
>
> 本文中的双端远程控制，是指 Windows 桌面 A 端控制 B 端的加密远程会话，不是浏览器 WebUI 远程访问。WebUI 架构见 `docs/remote-control-phase1-architecture.md`。

## 1. 角色与边界

| 名称 | 角色 | 核心职责 |
|---|---|---|
| A 端 / Controller / 主控端 | 发起控制的一端 | 输入连接码、建立租约、创建远程会话、发送任务、查看回传结果 |
| B 端 / Controlled Device / 被控端 | 执行任务的一端 | 生成连接码、授权工作区、执行任务、维护权威 Session 和 Journal |
| Relay | 不可信业务中继 | 注册和认领连接码、转发控制帧和加密业务帧，不执行任务、不持有业务明文 |
| Tauri WebView | UI 投影与用户交互 | 展示状态、收集有限 UI 输入，不持有密钥、租约或完整模型凭据 |

核心边界与目标决策：

- Rust 是连接、租约、会话、任务和历史记录的权威状态源。
- 前端 `remote-control-state.js` 是状态投影与迟到结果过滤层，不是业务真相源。
- Relay 只负责鉴权和转发；任务正文通过双端派生的会话密钥加密。
- B 端复用本地 Session、EnginePool、Codex ACP、权限、附件和产物体系，不重复实现执行引擎。
- 设置页负责连接管理；连接后的主要任务交互复用统一 `ChatView`。
- `SessionStore` 提供会话状态、持久化与投影能力；`EnginePool`、`AcpPool`、`RemoteControl` 分别参与本地 Engine、本地 ACP 和远程设备路径。各组件保持独立状态所有权，不在其上新增 `ConversationRuntime`、`LocalRuntime` 或 `RemoteRuntime`，也不要求三种后端能力实现统一接口。
- Tauri Command 是薄 IPC 入口；跨能力调用顺序、准入、权限和失败收口由无独立持久状态的用例编排完成。
- A 端远程会话使用 `SessionStore + RemoteControl`：`SessionStore` 保存 `RemoteProxy` 会话、A 本地 UI 元数据和已认证历史投影，`RemoteControl` 负责连接、加密通道、请求关联和恢复。
- B 端 Session、Journal、任务准入和执行终态始终是唯一业务权威；A 的 `RemoteProxy` 只是已认证投影，不得进入 A 本机 EnginePool/AcpPool。

## 2. 总体组件关系

```mermaid
flowchart LR
    subgraph A["A 端 · 主控设备"]
        ASettings["远程控制设置页"]
        AChat["ChatView"]
        AControlBridge["desktop-remote-control.js<br/>控制面 API"]
        ASessionBridge["desktop-remote-control.js<br/>会话面 API"]
        AControlCommands["连接控制 Commands"]
        ASessionCommands["会话 Commands"]
        AUseCase["A 远程会话用例编排"]
        AProjectionPort["RemoteSessionProjectionPort"]
        AStore["SessionStore<br/>RemoteProxy / 历史投影"]
        ARemote["RemoteControl<br/>连接 / 租约 / 加密通道"]
    end

    subgraph Relay["Relay 服务"]
        WS["认证 WebSocket"]
        Pairing["连接码注册与认领"]
        Forward["控制帧与加密业务帧转发"]
    end

    subgraph B["B 端 · 被控设备"]
        BSettings["远程控制设置页"]
        BChat["ChatView"]
        BControlBridge["desktop-remote-control.js<br/>控制面 API"]
        BSessionBridge["desktop-remote-control.js<br/>会话面 API"]
        BControlCommands["连接控制 Commands"]
        BSessionCommands["会话 Commands"]
        BState["ControlledDeviceManager"]
        BWorker["ControlledRelayWorker"]
        BLane["ControlledLaneState"]
        Ingress["RemoteControl 受控入口<br/>解密 / 认证 / 防重放"]
        Port["ControlledExecutionPort"]
        LocalUseCase["本地会话用例编排"]
        BStore["SessionStore"]
        Task["RemoteTaskService"]
        Engine["EnginePool / AcpPool"]
        Journal["Journal / History / Artifacts"]
    end

    ASettings -->|连接 / 断开| AControlBridge
    AControlBridge --> AControlCommands
    AControlCommands --> ARemote
    ASettings -. Connected 后导航 .-> AChat
    AChat -->|会话操作| ASessionBridge
    ASessionBridge --> ASessionCommands
    ASessionCommands --> AUseCase
    AUseCase --> AStore
    AUseCase --> ARemote
    ARemote -->|已认证远端事件| AProjectionPort
    AProjectionPort -. 组合根注入实现 .-> AUseCase

    ARemote <--> WS
    WS --> Pairing
    WS --> Forward

    Pairing <--> BWorker
    Forward <--> BWorker
    BWorker --> BState
    BWorker --> BLane
    BLane --> Ingress
    Ingress --> Port
    Port --> LocalUseCase
    LocalUseCase --> BStore
    LocalUseCase --> Task
    Task --> Engine
    Task --> Journal
    Journal --> BLane
    BLane --> BWorker

    BSettings -->|启停 / 刷新 / 授权| BControlBridge
    BControlBridge --> BControlCommands
    BControlCommands --> BState
    BChat -->|本地输入| BSessionBridge
    BSessionBridge --> BSessionCommands
    BSessionCommands --> LocalUseCase
    Journal -->|状态投影| BSessionBridge
```

图中的控制面/会话面 Bridge 与 Command 是逻辑职责，不要求拆分现有 `desktop-remote-control.js` 或 Command 文件。A 设置页通过控制面建立连接，进入 `Connected` 后只执行 UI 导航到统一 `ChatView`；导航不是后端调用，也不经过远程会话用例或 `SessionStore`。B 的 `ChatView` 本地输入和 `RemoteControl` 网络入站分别经会话 Command 与 `ControlledExecutionPort` 进入同一本地会话用例，再使用相同的 `SessionStore + EnginePool/AcpPool` 链路。

图中的用例编排是可复用、Tauri 无关的普通 Rust 边界，最终按职责位于所属 feature 的 `application` 子模块并显式导出；已发布能力的既有 Command 内 helper 只可作为迁移兼容 shim，未发布的 desktop controller 旧 helper 直接删除或停止注册，二者都不得继续承载新增业务。边界的签名和实现均不导入或调用 Tauri API，宿主事件通过注入的 `AppEventBus` 等窄端口发布，也不是新 Runtime。`ControlledExecutionPort` 与 `RemoteSessionProjectionPort` 由远控 feature 定义、由 app 组合根注入实现；图中 A 的虚线是经端口发生的运行时回调，不代表 `features/remote_control → app/commands` 或 feature 之间的反向模块依赖。本设计不要求新增固定的 `features/conversation` 目录。

## 3. 当前代码分层

### 3.1 页面与交互层

路径：`pinvou3-app/src/features/remote-control/` 和 `pinvou3-app/src/features/chat/ChatView.jsx`。

主要职责：

- `RemoteControlSettingsPage.jsx`：双角色页签、29 个 UI 状态汇总。
- `ControlledDevicePanel.jsx`：B 端启停、连接码、倒计时、刷新和工作区授权。
- `RemoteControllerPanel.jsx`：A 端连接状态、断开和进入会话。
- `RemoteConnectForm.jsx`：连接码格式化、本地校验和连接提交。
- `ChatView.jsx`：当前主要远程会话界面，同时支持 A 端远程会话和 B 端权威本地会话。
- `RemoteConnectionShell.jsx`：A 端连接摘要、断开与跳转壳；远程会话正文只在 `ChatView` 呈现。
- `main.jsx`：连接成功后的导航、B 端自动进入聊天页和权威 Session 切换。

页面层只应提交以下 UI 级输入：

- 连接码；
- 会话标题和模式；
- 消息正文；
- 模型来源；
- 用户对权限请求的决定；
- 用户显式选择的本地工作区或文件。

协议标识、租约字段、任务 ID、摘要和加密材料均由 Rust 构造。

### 3.2 前端领域投影层

路径：`pinvou3-app/src/features/remote-control/`。

| 文件 | 职责 |
|---|---|
| `remote-control-logic.mjs` | 连接码规范化、29 个 UI 状态派生、错误分类、异步操作防竞态 |
| `remote-control-state.js` | 将 Rust 事件归并为前端只读投影，并按 intent、generation、connection、lease 和 revision 丢弃旧结果 |
| `chat-transcript-projection.mjs` | 将远程 Journal、任务状态转换为聊天消息模型 |
| `capability-request-loop.mjs` | 能力协商重试和超时控制的纯逻辑 |
| `remote-model-source.mjs` | 被控端模型与主控端临时模型的选择约束 |

前端投影主要包含：

```text
controlledDevice
controlledSession
controllerConnection
remoteCapabilities
remoteModels
remoteProxySession / remoteCapabilities
controlledTasks / controlledTranscript
remotePermissions
```

### 3.3 Tauri 前端适配层

主要文件：

- `pinvou3-app/src/platform/tauri/bridge/desktop-remote-control.js`
- `pinvou3-app/src/platform/tauri/bridge.js`
- `pinvou3-app/src/index.html`

职责：

1. 暴露 `bridge.desktopRemoteControl` API；
2. 将前端调用转换为 Tauri `invoke`；
3. 订阅 Rust 事件；
4. 将事件交给纯状态投影函数；
5. 发布 `desktopRemoteControl` 状态域更新；
6. 协调能力快照的超时、重试和续期。

主要事件：

| 事件 | 消费方 | 用途 |
|---|---|---|
| `desktop-remote-controlled-status` | B 端投影 | 被控端连接生命周期 |
| `desktop-remote-controller-status` | A 端投影 | 主控端连接生命周期 |
| `desktop-remote-proxy-capability` | A 端投影 | 已认证 RemoteProxy 映射与能力协商结果 |
| `desktop-remote-controlled-session` | B 端投影 | A 端 session ref 到 B 权威 Session 的映射 |
| `desktop-remote-task-status` | 双端投影 | 任务状态 |
| `desktop-remote-task-event` | B 端投影 | 本地任务事件 |
| `desktop-remote-task-content` | B 端投影 | 本地聊天正文 |
| `desktop_remote_proxy` | A 端投影 | RemoteProxy 已持久 Journal/快照的有界失效通知，不携带正文 |
| `desktop-remote-controlled-journal` | B 端投影 | B 端权威 Journal |

事件身份必须按事件族定义，不得使用“所有事件必填 A Session ID”的单一规则：

- 接收端的会话事件始终携带该接收端本地 Session ID；
- A 的 `RemoteProxy` 会话事件额外携带 `remote_session_ref` 和当前 live connection/lease/generation；
- B 的受控会话事件携带 B 权威 Session ID 及适用的 lease/generation；
- 纯连接事件只携带其 connection/lease/generation 必填字段，不伪造 Session 引用；
- task ID、journal epoch/sequence 和 capability revision 按事件类型要求，用于去重和迟到过滤。

### 3.4 Tauri Command 应用入口层

路径：`pinvou3-app/src-tauri/src/app/commands/`。

| 模块 | 职责 |
|---|---|
| `desktop_remote_controlled.rs` | B 端启停、刷新、立即断开、状态查询、工作区授权 |
| `desktop_remote_controller.rs` | A 端连接、断开、状态和连接历史 |
| `desktop_remote_capability.rs` | A 端能力请求/读取的窄 Command 适配 |
| `desktop_remote_proxy.rs` | A 端可信 live open、RemoteProxy 列表/读取/软隐藏 Command |
| `desktop_remote_controller_business.rs` | B 端解密业务帧到本地能力、任务和 Journal 服务的适配 |
| `desktop_remote_capabilities.rs` | 构造 B 端能力快照 |
| `desktop_remote_task.rs` | B 端权威 Session、任务准入、执行和状态投影 |
| `desktop_remote_permission.rs` | 权限请求与决策 |
| `desktop_remote_model.rs` | 可导出模型和任务级临时模型 |
| `desktop_remote_attachment.rs` | 附件上传和任务传输范围 |
| `desktop_remote_artifact.rs` | 产物索引、预览和下载 |
| `desktop_remote_journal.rs` | B 权威 Journal 运行时与 A 端认证投影适配；不提供独立 A 历史 Command |

Command 入口负责 Tauri 参数、返回值和事件适配，不应成为连接状态、会话状态或任务状态的第二所有者。目标架构下，需要复用的业务流程由 Command 委托给所属 feature `application` 子模块显式导出的 Tauri 无关用例边界，再按会话类型使用以下组合；不要求统一迁入 conversation feature，已发布能力的既有 Command helper 仅作为迁移 shim，未发布的 desktop controller 旧 helper 直接删除或停止注册：

| 会话类型 | 调用组合 | 约束 |
|---|---|---|
| 本地普通 | `SessionStore + EnginePool` | 保持现有 Session 和 turn 准入语义 |
| 本地 ACP | `SessionStore + AcpPool` | 保持现有工作区、进程和 ACP 事件语义 |
| A 远程代理 | `SessionStore(RemoteProxy) + RemoteControl` | 不得调用 A 本机 EnginePool/AcpPool |
| B 远程受控 | `RemoteControl 入口 + 本地用例 + SessionStore + EnginePool/AcpPool` | B 只产生一个权威 Session |

用例编排可负责参数校验、会话类型检查、turn 准入、附件准备、调用顺序、失败回滚/收口、错误转换和事件投影；持久化和长生命周期状态仍归各自的能力组件，其中会话状态、持久化与投影归 `SessionStore`，本地执行资源和远程控制状态分别归对应后端组件。

### 3.5 Rust 领域层

路径：`pinvou3-app/src-tauri/src/features/remote_control/controller/`。

| 模块 | 状态所有权或领域职责 |
|---|---|
| `controlled.rs` | B 端连接状态、generation、工作区授权、watchdog 和恢复材料 |
| `controlling.rs` | A 端连接状态、连接码、lease 和显式断开 |
| `controlling_session.rs` | A 端远程 Session 意图、能力 revision 和已构造任务 |
| `task.rs` | B 端任务账本、Session 映射、准入、执行器和清理 |
| `capabilities.rs` | B 端能力快照、有效期和工作区解析 |
| `permission.rs` | 权限请求状态机和唯一决策权 |
| `journal.rs` | B 权威事件日志、序号、快照和缺口恢复；A 端经投影端口写入 `SessionStore.RemoteProxy` |
| `temporary_model.rs` | 任务级临时模型生命周期 |
| `transfer/` | 附件与产物的有界分块传输 |

### 3.6 安全协议与运行时层

| 模块 | 职责 |
|---|---|
| `protocol.rs` | 冻结的 controller v1 消息、字段、能力和大小约束 |
| `crypto.rs` | 握手密钥、签名、会话密钥和方向密钥派生 |
| `controlled_handshake.rs` | B 端握手状态机 |
| `controlling_handshake.rs` | A 端握手状态机 |
| `encrypted_lane.rs` | 加密业务帧、租约绑定、方向校验、序号和防重放 |
| `relay_transport.rs` | A 端 Relay 传输、背压和连接事件 |
| `relay_runtime.rs` | B 端注册、握手、加密业务帧派发和恢复运行时 |
| `controlling_service.rs` | A 端连接编排、通道保活、入站业务帧派发和恢复 |
| `liveness.rs` | 存活检测和 watchdog |
| `resume.rs` | 断线恢复状态机 |

### 3.7 Relay 与平台基础设施层

路径：`pinvou3-app/src-tauri/src/features/remote_control/`。

- `manager/`：远程 endpoint、Relay 设置、WebUI RPC 以及桌面 controller lane 的共享管理器。
- `relay_client.rs`：WebSocket 连接、重连、心跳、背压和发送队列。
- `platform/`：Windows、Unix 和 fallback 平台实现。
- `file_access.rs`：受控文件访问边界。

当前桌面双端远控复用了 Web Access 的长连接和 endpoint。`RemoteControlManager` 是基础设施复用点，不应被视为桌面双端远控的领域状态机。

## 4. 当前与目标状态所有权

| 状态 | 当前实现 | 目标权威所有者 | 前端表现/约束 |
|---|---|---|---|
| B 端连接生命周期 | `ControlledDeviceManager` | `ControlledDeviceManager` | `controlledDevice` |
| A 端连接生命周期 | `ControllingDeviceManager` | `ControllingDeviceManager` | `controllerConnection` |
| A 端远程 Session 意图和能力 revision | `ControllingSessionManager` | `RemoteControl` 领域状态 | 必须绑定当前 connection/lease/generation |
| A 本地远程会话身份和 UI 元数据 | `SessionStore.RemoteProxy` | `SessionStore` 中的 `RemoteProxy` | A 本地 Session ID 是 ChatView 会话身份 |
| A 端 pending 发送 | `SessionStore.RemoteProxy` 中的有界非权威投影 | `SessionStore.RemoteProxy` 中的有界非权威投影 | 只能经 B 接纳或 Journal 对账为权威内容 |
| B 端权威 Session | `RemoteTaskService` + `SessionStore` | `RemoteTaskService` + `SessionStore` | `controlledSession`；仍是唯一业务事实源 |
| 加密通道 | `ControllingLaneState` / `ControlledLaneState` | `RemoteControl` 内的双端 lane state | 不直接暴露 |
| 任务状态 | `RemoteTaskService` / `RemoteTaskLedger` | B 端 `RemoteTaskService` / `RemoteTaskLedger` | A 只显示已认证投影 |
| 正文和工具事件 | B 端 `RemoteJournal` | B 端 `RemoteJournal` | A 投影必须校验 epoch/sequence |
| 权限 | `PermissionAuthority` | `PermissionAuthority` | `remotePermissions`；A 仍是远程任务唯一审批权威 |
| A 端离线副本 | `SessionStore.RemoteProxy` 已认证投影 | `SessionStore` 中的 `RemoteProxy` 已认投影 | 断线后只读，不得发送业务帧 |
| 输入框、倒计时、按钮 pending | React 本地状态 | React 本地状态 | 不进入 Rust 领域状态机 |

`RemoteHistoryStore` 及其注册与调用方已在首次发布前直接移除；实验数据不迁移、不兼容读取，产品供数链路只有 `SessionStore.RemoteProxy`。

`RemoteProxy` 可持久化 A 本地 Session ID 与 opaque `remote_session_ref` 的历史来源映射，但这不是访问凭据。`remote_session_ref` 由 A 端可信 Rust 生成，B 在认证请求后幂等映射并确认。在原 resume 边界内恢复时仍需验证新的 transport 绑定；超出 resume 边界或重新配对后，A 必须重新打开会话并生成新引用，再绑定当前 connection/lease/generation，不得将磁盘上的旧引用直接用于变更操作。

## 5. 29 个 UI 子状态

29 个状态是设置页展示模型，由 B 端 12 个状态和 A 端 17 个状态组成。它们不是 29 个 Rust 协议状态。

### 5.1 B 端 12 个状态

Rust 状态机：

```text
Disabled
Reserving
WaitingConnection
SafetyHandshake
Connected
Reconnecting
Refreshing
Stopped
Error
```

UI 状态：

```text
off
reserving
waiting
expiring
expired
reserved
handshake
connected
reconnecting
refreshing
stopping
error
```

派生规则：

- `waiting`、`expiring`、`expired` 由 `WaitingConnection`、连接码和剩余时间共同决定；
- `reserved` 是连接码已被认领、后端即将进入安全握手时的短暂 UI 状态；
- `stopping` 是本地命令 pending 状态；
- `off` 统一覆盖 `Disabled`、`Stopped` 和初始空投影。

### 5.2 A 端 17 个状态

Rust 状态机：

```text
Idle
Connecting
SafetyHandshake
Connected
Reconnecting
Disconnected
Error
```

UI 状态：

```text
empty
typing
ready
format_invalid
invalid
connecting
safety_handshake
connected
reconnecting
disconnected
busy
offline
version
protocol
unsupported
local_busy
failed
```

派生规则：

- `empty`、`typing`、`ready`、`format_invalid` 来自本地连接码输入；
- `busy`、`offline`、`version`、`protocol`、`unsupported`、`local_busy`、`invalid`、`failed` 来自有界错误码分类；
- 其余状态直接来自 A 端 Rust 状态机。

## 6. 业务调用流程

### 6.1 B 端开启远程控制

```mermaid
sequenceDiagram
    participant UI as ControlledDevicePanel
    participant JS as Desktop Bridge
    participant CMD as desktop_remote_controlled_start
    participant SM as ControlledDeviceManager
    participant Worker as ControlledRelayWorker
    participant Relay as Relay
    participant Store as Frontend Projection

    UI->>JS: controlledStart()
    JS->>CMD: invoke
    CMD->>SM: start()
    SM-->>CMD: Reserving + generation
    CMD->>Worker: start(generation)
    Worker->>Relay: 注册被控端和连接码
    Relay-->>Worker: pairing code + expiresAt
    Worker->>SM: complete_registration()
    SM-->>Worker: WaitingConnection
    Worker-->>Store: desktop-remote-controlled-status
    Store-->>UI: waiting / expiring / expired
```

关键约束：

- 每次开启或刷新都由新的 `generation` 隔离；
- 旧 generation 的回包和事件不能覆盖新状态；
- 连接码和握手材料只保存在内存中；
- 停止或刷新时先关闭对应 Relay lane，再清理这一代状态。

### 6.2 A 端建立连接

```mermaid
sequenceDiagram
    participant UI as RemoteConnectForm
    participant JS as Desktop Bridge
    participant CMD as Controller Command
    participant A as ControllingDeviceManager
    participant Relay as Relay
    participant B as ControlledRelayWorker
    participant Crypto as Handshake / EncryptedLane

    UI->>JS: controllerConnect(code)
    JS->>CMD: desktop_remote_controller_connect
    CMD->>A: connect(code)
    A-->>UI: Connecting
    CMD->>Relay: claim pairing code
    Relay->>B: controller_pairing_reserved
    A<->>B: 双向安全握手
    A->>Crypto: 安装 A 端租约和会话密钥
    B->>Crypto: 安装 B 端租约和会话密钥
    A-->>UI: Connected
    B-->>UI: Connected
```

连接成功时同时形成：

- `connection_id`；
- `controller_lease_id`；
- `lease_epoch`；
- `relay_epoch`；
- `remote_control_generation`；
- 双端会话密钥；
- 协商后的协议能力集合。

只有租约和加密业务通道均安装成功后，状态机才进入 `Connected`。

### 6.3 打开远程会话并协商能力

```text
ChatView
  → bridge.openLiveRemoteProxy
  → desktop_remote_proxy_open_live
  → 薄 A 端会话用例编排
  → SessionStore 创建或幂等复用 RemoteProxy Session
  → ControllingSessionManager.open
  → 将 A 本地 Session ID、remoteSessionRef 与当前 connection/lease/generation 绑定
  → bridge.controllerCapabilitySend
  → 构造 capability_snapshot_request
  → ControllingLaneState.seal
  → Relay
  → B 端 ControlledRelayWorker 解密
  → DesktopControlledBusiness.capability_snapshot
  → CapabilitySnapshotManager 构造快照
  → B 端加密返回
  → A 端校验 lease、requestId 和 revision
  → 更新 SessionStore 中对应 RemoteProxy 的能力投影
  → desktop-remote-proxy-capability
  → 前端进入 capability ready
```

能力快照只包含经过裁剪的描述信息：

- 可用模式；
- 工具和 Skill 描述；
- 已授权工作区；
- 能力 revision 和有效期；
- 是否允许主控端临时模型。

完整模型配置和凭据不会投影到 WebView。`RemoteProxy` 可以保存能力摘要和 revision，但该投影不能替代 `RemoteControl` 对当前租约和新鲜度的校验。

### 6.4 A 端发送任务，B 端执行

```mermaid
sequenceDiagram
    participant ChatA as A ChatView
    participant CmdA as A 薄 Session Command
    participant UseA as A 远程用例编排
    participant StoreA as A SessionStore / RemoteProxy
    participant RemoteA as A RemoteControl
    participant Relay as Relay
    participant WorkerB as B Relay Worker
    participant IngressB as B RemoteControl 受控入口
    participant UseB as B 本地用例编排
    participant StoreB as B SessionStore
    participant TaskB as RemoteTaskService
    participant Engine as EnginePool / AcpPool
    participant Journal as Authoritative Journal
    participant ChatB as B ChatView

    ChatA->>CmdA: controllerTaskSend(localSessionId, text, modelSource)
    CmdA->>UseA: 校验参数并调用用例
    UseA->>UseA: prepare 生成稳定 clientRequestId/taskId/digest
    UseA->>StoreA: 校验 RemoteProxy，原子持久化有界 pending/outbox
    UseA->>RemoteA: 使用已持久化的原 ID，校验 remoteSessionRef/lease
    RemoteA->>Relay: seal(task_submit) / encrypted_frame
    Relay->>WorkerB: encrypted_frame
    WorkerB->>WorkerB: 校验 lease、generation、方向和序号
    WorkerB->>IngressB: submit_task
    IngressB->>UseB: ControlledExecutionPort.submit
    UseB->>StoreB: 建立或复用 B 权威 Session
    UseB->>TaskB: submit_decrypted_task
    TaskB->>Engine: 执行 chat/code turn
    Engine-->>Journal: delta/tool/permission/result
    Journal-->>ChatB: controlled journal events
    Journal-->>Relay: 加密 journal/task_status
    Relay-->>RemoteA: encrypted_frame
    RemoteA-->>UseA: 校验后交付 journal/task event
    UseA->>StoreA: 对账 pending，写入已认证投影
    StoreA-->>ChatA: Session 投影更新
```

任务实际执行复用：

- Chat：`EnginePoolChatEngine`；
- Code：`AcpPoolCodeTurnPort`；
- 会话：`SessionStore`；
- 任务准入和账本：`RemoteTaskService`、`RemoteTaskLedger`；
- 权限：`PermissionAuthority`；
- 附件和产物：transfer、attachment、artifact runtime；
- 正文恢复：`RemoteJournal` 和 `JournalSnapshot`。

B 端用户也可以在同一个权威 Session 中继续输入。`desktop_remote_controlled_task_send` 会构造同一协议形状的任务，再进入相同的任务准入、执行器和 Journal，不会形成第二套执行系统。

A 端必须遵循 `prepare → persist → dispatch`：pending/outbox 持久化失败时禁止发送；持久化内容包含稳定 `client_request_id/task_id`、输入摘要和所有非敏感语义参数的规范指纹，但不含临时凭证。只有 A 原进程存活、处于原 resume 边界并恢复同一 live binding 时，才能使用原 ID 和完全相同指纹向 B 幂等账本重试或经 Journal/快照对账；同 ID 异参必须拒绝。A 进程重启后旧 live binding 已失效，只恢复本地 `outcome_unknown`；在 controller v1 未定义新配对重新附着旧 B Session/账本前，不得自动发送、查询或对账旧请求。临时凭证在 dispatch 前丢失时，只有后端可证明从未尝试发送才能废弃原 pending 并创建新请求；否则继续保持 `outcome_unknown`。A 在 B 接纳前只能显示未确认 `pending/outcome_unknown`；只有当前有效绑定内 B 的已认证接纳结果、Journal 事件或快照才能将该投影对账为权威内容。

### 6.5 任务结果和 Journal 回流

B 端执行器产生事件后：

1. 先更新任务账本；
2. 再写入 B 端权威 Journal；
3. 向 B 端前端投影本地任务和正文；
4. 将 Session ID 改写回 A 端持有的 `sessionRef`；
5. 通过 B 端加密通道发送 `task_status` 或 `journal_event`；
6. A 端校验和解密；
7. A 端 `RemoteControl` 校验当前 connection/lease/generation、`remote_session_ref` 与 Journal 游标；
8. 在 `SessionStore` 中将对应 `RemoteProxy` 的 pending 投影对账为已认证副本；
9. 广播 Session 投影更新并渲染聊天正文。

Journal 发生缺口时，A 端请求 replay；如果缺口已经超出保留窗口，B 端返回权威 Session snapshot，A 在完成认证、序号和会话绑定校验后原子替换 `SessionStore.RemoteProxy` 的投影。A 不得用本地 UI 状态或 pending 记录反向覆盖 B。

### 6.6 断线恢复与清理

```text
transport disconnected
  → 双端进入 reconnecting
  → 暂停业务加密通道
  → 使用旧租约中的 resume token 发起恢复
  → Relay、B、A 多阶段确认
  → 更新 transport fingerprint
  → 恢复业务帧、心跳与 Journal
```

恢复失败或显式断开后清理：

- 本 generation 的活动任务；
- 待决权限；
- 会话密钥和加密通道；
- 临时模型；
- 工作区授权；
- 远程 Session 映射；
- 能力快照和前端任务投影。

## 7. 当前主要架构问题

### 7.1 A 端存在两套会话 UI

DEV-31 已删除未发布的 `RemoteConversationView`；`ChatView.jsx` 是唯一远程会话正文界面，`RemoteConnectionShell` 只保留连接摘要、断开和跳转。不得恢复第二套 session/capability/task 编排或正文状态机。

### 7.2 前端 Bridge 职责过多

`desktop-remote-control.js` 当前同时承担：

- Tauri Client；
- 事件订阅；
- 状态 Store；
- 能力协商重试与续期；
- 错误标准化；
- 乐观任务投影；
- 实验期旧命令兼容。

这使平台适配层承载了领域编排逻辑，不利于单独测试和复用。DEV-31 必须删除实验期兼容分支，而不是把它保留为代理。

### 7.3 Rust 目录无法从路径直接区分双端角色

`features/remote_control/controller/` 同时包含：

- A 端 `controlling*`；
- B 端 `controlled*`；
- 公共协议、任务、权限、历史、加密和传输。

“controller” 既像领域总称，又像 A 端角色名称，增加理解成本。

### 7.4 Relay 共享管理器边界较宽

`RemoteControlManager` 同时承载 Web Access 和桌面双端 controller lane。共享 endpoint 和 WebSocket 是合理复用，但领域层直接感知共享管理器会模糊边界。

### 7.5 Command 面存在重叠入口

当前 A 端只保留 `openLiveRemoteProxy` 与 RemoteProxy 远程 action；其中 `submitRemoteTask(localSessionId, remoteRef, intent)` 是现行 `prepare → persist → dispatch` 入口，不是旧直发 handler。B 端保留本地权威会话入口 `controlledTaskSend`。未发布的 `controllerSessionOpen`、`openRemoteSession` 以及旧签名任务直发入口已直接删除，不设置别名或兼容代理。

### 7.6 A 端远程历史与 SessionStore 分裂

DEV-31 已删除实验期 `RemoteHistoryStore` 和前端 `remoteHistories`；A 端远程正文、对账游标和离线副本只由 `SessionStore.RemoteProxy` 供数。未发布的实验数据不迁移、不兼容读取，也不允许再引入第二个持久化数据源。

### 7.7 B 受控入口与本地用例复用边界不明

B 端网络入站和 Tauri Command 都需要进入相同的 Session、turn 准入、附件、执行器和 Journal 链路，但 feature 不能反向依赖 `app/commands`。目标架构需要在职责所属 feature 的 `application` 子模块为共享流程暴露不导入或调用 Tauri API 的普通 Rust 用例边界，并由组合根向 `RemoteControl` 注入窄 `ControlledExecutionPort` 适配器；不强制新增统一 conversation feature，也不搬迁无关 Command。已发布能力的既有 Command helper 只能作为代理到该边界的迁移 shim；未发布的 desktop controller 旧 helper 直接删除或停止注册；所有入口都必须避免内部 invoke、跨 Command 调用和第二套执行逻辑。

## 8. 推荐目标分层

以下是目标结构建议，不代表当前目录已经完成迁移。

### 8.1 前端

```text
src/features/remote-control/
├─ api/
│  └─ remote-control-client.js       # 仅封装 invoke/listen
├─ model/
│  ├─ state.js                       # 投影状态
│  ├─ reducer.js                     # 事件归并与迟到结果过滤
│  ├─ selectors.mjs                  # 29 状态及聊天投影
│  └─ contracts.mjs                  # 状态和错误码常量
├─ services/
│  ├─ capability-coordinator.js      # 能力请求、重试、续期
│  └─ remote-session-service.js      # 会话打开和任务发送编排
├─ views/
│  ├─ RemoteControlSettingsPage.jsx
│  ├─ ControlledDevicePanel.jsx
│  ├─ RemoteControllerPanel.jsx
│  └─ RemoteConnectForm.jsx
└─ integration/
   └─ remote-session-actions.js      # 为 RemoteProxy 提供远程操作，不持有会话副本
```

原则：

- `platform/tauri` 只做平台调用适配；
- 连接和会话编排回到 `features/remote-control`；
- `ChatView` 统一从会话列表/Session 投影获取正文，不再从 `remoteHistories` 第二份数据源获取远程正文；
- `RemoteProxy` 的发送、取消、审批和上传通过 remote session actions 进入 Rust，但路由决定仍由 Rust 的 Session 类型与绑定完成；
- 删除第二套远程会话正文 UI。

### 8.2 Rust

```text
src/features/remote_control/
├─ domain/
│  ├─ connection/
│  │  ├─ controlled.rs
│  │  ├─ controller.rs
│  │  ├─ lease.rs
│  │  ├─ liveness.rs
│  │  └─ resume.rs
│  ├─ session/
│  ├─ task/
│  ├─ capability/
│  ├─ permission/
│  ├─ journal/
│  └─ ports/
│     ├─ controlled_execution.rs       # B 受控请求进入本地用例的窄端口
│     └─ remote_session_projection.rs  # A 已认证事件进入投影/对账用例的窄端口
├─ application/
│  ├─ controlled_service.rs
│  ├─ controller_service.rs
│  ├─ session_service.rs
│  └─ task_service.rs
├─ infrastructure/
│  ├─ relay/
│  ├─ crypto/
│  ├─ persistence/
│  └─ transfer/
└─ platform/

src/features/sessions/
├─ ...
└─ remote_proxy.rs                  # RemoteProxy 格式、字段级写权限和 pending 对账

src/features/<owner>/application/         # 可枚举的 Tauri 无关共享用例边界
├─ mod.rs                            # 显式导出供 Command/注入端口复用的入口
└─ <use_case>.rs                     # 按职责归属到 assistant/codex_acp/remote_control 等 feature

src/app/commands/remote_control/
├─ connection.rs
├─ session.rs
├─ task.rs
├─ permission.rs
├─ transfer.rs
└─ history.rs
```

上述目录只规定共享业务边界位于其职责所属 feature 的 `application` 子模块，不要求新增统一的 `features/conversation`。这一约定使架构守卫能够枚举 `features/*/application/**/*.rs`，检查其不导入或调用 Tauri API；已发布能力的既有 Command helper 仅能作为有退出条件的兼容代理，不属于最终边界，未发布的 desktop controller 旧 helper 则直接删除或停止注册。用例不持有第二份 Session、Engine、ACP、任务或远控状态，也不形成 Runtime 抽象。具体装配由 `lib.rs` 组合根完成，并保持下述依赖方向：

```text
Tauri Command（IPC 入口） → 可复用、Tauri 无关的用例边界
                           ├→ SessionStore（会话状态/持久化/投影）
                           └→ 按会话类型选择 EnginePool / AcpPool / RemoteControl

B RemoteControl 入站 → ControlledExecutionPort
                         ↑ 由 lib.rs 组合根注入适配器实现
                         └→ 同一 Tauri 无关用例边界 → SessionStore + EnginePool/AcpPool

A RemoteControl 已认证事件 → RemoteSessionProjectionPort
                              ↑ 由 lib.rs 组合根注入实现
                              └→ 投影/对账用例 → SessionStore.RemoteProxy → AppEventBus
```

领域层不得依赖 Tauri、WebView 或具体 Relay Manager；`features/remote_control` 不得依赖 `app/commands`。能力组件之间需要跨 feature 协作时，通过组合根注入窄端口，而不是形成 Runtime 包装层或反向调用 Command。

### 8.3 Relay 窄接口

建议由应用层依赖窄端口，而不是直接依赖共享 `RemoteControlManager`：

```rust
trait DesktopRelayPort {
    async fn open_controlled_lane(&self, generation: &str) -> Result<ControlledChannel, RelayError>;
    async fn open_controller_lane(&self, request: ControllerClaim) -> Result<ControllerChannel, RelayError>;
    fn close_generation(&self, generation: &str);
}
```

现有 `RemoteControlManager` 可以继续作为该端口的基础设施实现，从而保留 endpoint 和 WebSocket 复用，同时收窄领域依赖。

## 9. 推荐迁移顺序

### 阶段一：冻结首次发布目标

- 列出要保留的已发布本地/WebUI Command 与应删除的未发布 desktop Command，不冻结后者名称和参数；
- 冻结首次发布所需的 desktop 事件最小集合，删除实验期重复事件；
- 按最终安全语义冻结 A/B 后端状态机，不为实验期中间状态保留 fallback；
- 冻结首次发布实际使用的 UI 状态映射，删除第二套正文状态；
- 补充连接、能力协商、B 权威 Session 映射、任务和断线恢复的契约测试。

### 阶段二：引入 SessionStore RemoteProxy 并移除实验历史源

- 为 `SessionStore` 增加版本化 `RemoteProxy` 类型和字段级写入权限：本地 UI 元数据与非权威 pending/outbox 可写，远程业务投影只能由已认证 B 事件/快照更新，并禁止本机 Engine/ACP；
- 定义 A 本地 Session ID、opaque `remote_session_ref` 和 connection/lease/generation 绑定；
- 实现 `prepare → persist → dispatch` pending/outbox 与 B 已认证 Journal/快照的对账；
- 删除或停止注册未发布的 `RemoteHistoryStore` 及其 desktop Command/Bridge 调用方，不迁移实验数据，也不扩展 `RemoteProxy` schema 承载旧 DTO；
- 从首次发布路径的第一帧开始，远程正文只写入和读取 `SessionStore.RemoteProxy`；功能开关回退只关闭入口，不恢复旧数据源；
- 通过静态调用方扫描、单数据源契约测试和既有 WebUI v2 回归证明清理范围没有越界。

### 阶段三：收敛 Rust 用例编排

- 保持已发布的本地与 WebUI Command 名称、参数和已验证行为兼容，只为确需复用的流程在所属 feature 的 `application` 子模块暴露 Tauri 无关用例边界；未发布的 desktop controller 旧 Command 直接删除或停止注册；
- 保持 `SessionStore + EnginePool`、`SessionStore + AcpPool`、`SessionStore + RemoteControl` 三类显式编排，不引入统一 Runtime；
- 最小化提取本地与 B 受控入口共享的普通 Rust 逻辑，不新增统一 conversation feature；已发布能力的既有 Command helper 只可作为有退出条件的迁移 shim，未发布的 desktop controller 旧 helper 不保留；
- 定义并由组合根同时注入 `ControlledExecutionPort` 与 `RemoteSessionProjectionPort` 实现：前者承接 B 受控请求，后者承接 A 已认证入站事件和投影/对账；禁止 remote-control feature 反向依赖 Tauri Command、`app` 或 Tauri 事件 API；
- 将 A 已认证入站事件直接接入 `RemoteSessionProjectionPort`；删除绕过端口的旧直连回调或直接 Tauri 事件调用，并用静态调用方扫描和端口契约测试证明没有残余路径；
- 直接修订未发布的 controller v1 实验 wire 以补齐 B 权威 Session 映射确认，不保留旧 desktop wire fallback；最终形状冻结后仍由 B 端唯一权威 Session。

### 阶段四：统一会话界面与拆分前端 Bridge

- 以 `ChatView` 为唯一远程会话正文界面，从 Session 投影读取 `RemoteProxy` 内容；
- 将权限、产物和历史入口整合进统一聊天上下文；
- 删除 `RemoteConversationView` 中重复的 session/capability/task 编排；

- 抽出纯 Tauri Client；
- 抽出 event reducer；
- 抽出 capability coordinator；
- 将高层业务 API 放回 remote-control feature；
- 删除未发布的旧 `bridge.desktopRemoteControl` 正文/历史子接口；保留的连接控制 API 必须是当前唯一调用面，不设置旧桌面 API 兼容代理。

### 阶段五：领域与基础设施整理

- 先抽 Relay port；若现有实验协议缺少完成目标架构所需的认证绑定，则直接修订并同步契约测试，不建立兼容分支；
- 再迁移 connection/session/task 领域模块；
- 清理跨 command 模块调用和无消费者旧入口；
- 删除旧历史双路径和无调用方的 desktop controller 实验入口，不保留兼容代理；
- 未发布的 desktop 事件名与 controller v1 实验 wire 可直接收口到唯一目标形状；必须保持已验证的 WebUI v2 行为。

## 10. 关键约束

- `SessionStore` 负责会话状态、持久化与投影；`EnginePool`、`AcpPool`、`RemoteControl` 分别参与本地 Engine、本地 ACP 和远程设备路径。各组件保持独立状态所有权，不引入持有会话或任务状态的统一 Conversation Runtime，也不要求三种后端能力实现统一接口。
- A 端远程会话必须经 `SessionStore.RemoteProxy + RemoteControl` 编排，不得进入 A 本机 EnginePool/AcpPool。
- A 只权威拥有本地 UI 元数据；远程正文、任务状态和终态只是经 B 已认证 Journal/快照对账的投影。
- B 端网络入站通过组合根注入的 `ControlledExecutionPort` 进入本地用例，不得反向调用 Tauri Command。
- A 端已认证入站事件只能通过组合根注入的 `RemoteSessionProjectionPort` 进入投影/对账用例，再经注入的 `AppEventBus` 等宿主端口通知 UI；不得直接依赖 `app`、Tauri 事件 API 或形成 feature 反向依赖。
- 不因纯目录重构改变加密语义；未发布的 controller v1 实验字段仅在补齐可信映射等目标架构缺口时直接修订，且不保留旧字段 fallback。
- 不允许 React 构造 lease、task ID、摘要或临时模型包。
- 不允许凭据、连接码、resume token、会话密钥进入日志或持久化。
- 不允许 Relay 获得任务和 Journal 明文。
- B 端权威 Session 与 A 端历史副本必须保持明确区分。
- UI 关闭不等于断开连接；只有显式断开或安全状态机终止才能清理租约。
- 重连期间暂停业务帧；完成完整 resume 后才能恢复发送。
- 所有异步结果必须受 intent、generation、connection、lease 或 revision 约束。
- 新增界面文案必须复用 `src/shared/i18n.js` 对应的中、英、日文资源。

## 11. 关键代码索引

### 前端

- `pinvou3-app/src/features/remote-control/RemoteControlSettingsPage.jsx`
- `pinvou3-app/src/features/remote-control/ControlledDevicePanel.jsx`
- `pinvou3-app/src/features/remote-control/RemoteControllerPanel.jsx`
- `pinvou3-app/src/features/remote-control/RemoteConnectForm.jsx`
- `pinvou3-app/src/features/remote-control/remote-control-logic.mjs`
- `pinvou3-app/src/features/remote-control/remote-control-state.js`
- `pinvou3-app/src/features/chat/ChatView.jsx`
- `pinvou3-app/src/platform/tauri/bridge/desktop-remote-control.js`
- `pinvou3-app/src/app/main.jsx`

### Rust 应用入口

- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_controlled.rs`
- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_controller.rs`
- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_capability.rs`
- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_proxy.rs`
- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_controller_business.rs`
- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_task.rs`
- `pinvou3-app/src-tauri/src/app/commands/desktop_remote_journal.rs`

### Rust 领域与基础设施

- `pinvou3-app/src-tauri/src/features/remote_control/domain/ports/controlled_execution.rs`（目标）
- `pinvou3-app/src-tauri/src/features/remote_control/domain/ports/remote_session_projection.rs`（目标）
- `pinvou3-app/src-tauri/src/features/remote_control/controller/controlled.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/controlling.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/controlling_session.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/controlling_service.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/relay_runtime.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/encrypted_lane.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/task.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/journal.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/controller/protocol.rs`
- `pinvou3-app/src-tauri/src/features/remote_control/manager/mod.rs`

## 12. 验证入口

前端远程控制契约测试位于 `pinvou3-app/tests/`，重点包括：

- `desktop_remote_state.test.mjs`
- `remote_control_ui_closure.test.mjs`
- `remote_control_race.test.mjs`
- `connect_remote_device_contract.test.mjs`
- `controlled_device_panel_contract.test.mjs`
- `controller_session_intent_contract.test.mjs`
- `remote_chat_projection.test.mjs`
- `remote_capability_request_loop.test.mjs`

Rust 领域测试集中在 `features/remote_control/controller/` 各模块及 controller crypto harness。目标实现还必须分别为 `ControlledExecutionPort` 和 `RemoteSessionProjectionPort` 增加注入、错误映射、单次调用、重复/迟到事件过滤和无绕行路径的契约测试；`scripts/architecture-guard.py` 必须枚举 `features/*/application/**/*.rs`，拒绝其中的 Tauri API 引用，并拒绝 `features/remote_control → app/commands`、直接 Tauri 事件调用和新增 feature 依赖环。

常用验证命令：

```bash
npm --prefix pinvou3-app test
python3 scripts/architecture-guard.py
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1
```
