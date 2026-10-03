# Pinvou Agent Windows 双端远程控制需求文档

> 文档状态：需求确认稿
>
> 文档版本：1.10
>
> 更新日期：2026-10-01
>
> 适用范围：Pinvou Agent Windows 桌面端、Remote Control Relay、模型与任务执行链路
>
> 相关资料：[Windows 双端远程控制代码架构](./Windows双端远程控制代码架构.md)、[完整 WebUI v2 架构](./remote-control-phase1-architecture.md)、[WebUI Relay v2 协议](../remote-control-relay/PROTOCOL.md)、[远程知识库设计](./remote-knowledge.md)、[真实用户自动化测试矩阵](./真实用户自动化测试矩阵.md)

| 版本 | 日期 | 修订说明 |
|---|---|---|
| 1.0 | 2026-09-15 | 明确 Windows 双端远程控制、7 位一次性配对、Relay 公网接入、临时模型配置、完整任务能力和双端同步要求 |
| 1.1 | 2026-09-15 | 明确 Relay 信任边界、配对预留状态、Web/桌面互斥租约、A 唯一审批权威、临时模型包 v1 严格字段和 30 秒重连边界 |
| 1.2 | 2026-09-15 | 统一 Relay 发码、双 Finished/ACK、公网 endpoint enrollment、B generation/watchdog、旧 Web wire 兼容和双角色 resume token |
| 1.3 | 2026-09-15 | 改用随机 Finished receipt secret、provisional resume 激活门、码隔离 tombstone、30 秒总心跳预算并补齐 endpoint enrollment 迁移 |
| 1.4 | 2026-09-15 | 固化 Finished receipt 编码与测试向量、角色隔离 liveness、恢复后存活确认门、tombstone 多密钥轮换及安全回滚下限 |
| 1.5 | 2026-09-15 | 增加 A→B 端到端加密 liveness 主路径，并以保守跨时钟公式约束 Relay attestation 的 30 秒总预算 |
| 1.6 | 2026-09-22 | 明确 A、B 均可向同一个 B 权威 Session 输入消息，并统一任务接纳、执行状态、journal 投影和精确会话关联 |
| 1.7 | 2026-09-23 | 固化临时模型规范数字编码、视觉兜底注入、任务接纳与凭据安装原子性、HTTPS/DNS 预校验、客户端能力兼容及最终出站目标未固定的已知风险 |
| 1.8 | 2026-09-26 | 引入统一 ConversationRuntime，会话 UI 与应用层通过同一运行时端口选择本地或远程执行；明确远程运行时是加密代理、B 端保持 Session 与执行权威，并补充迁移、测试和完成定义 |
| 1.9 | 2026-09-26 | 取消 ConversationRuntime/LocalRuntime/RemoteRuntime 分层，改为薄 Tauri Command/用例编排直接协调并列的 SessionStore、EnginePool、AcpPool 与 RemoteControl；明确 A 以 SessionStore 保存 RemoteProxy 会话和已认证投影，B 保持唯一权威 Session |
| 1.10 | 2026-10-01 | 记录 Finished receipt 固定向量勘误，澄清 reservation 原子提升、模块落位与模型路由判别符，并登记生产容量定标门禁 |

## 1. 背景

Pinvou Agent 已具备桌面端作为执行权威、浏览器通过 Relay 访问桌面能力的 WebUI v2 架构，但尚不支持两个 Pinvou Windows 桌面客户端之间建立“主控端—被控端”关系。

本需求新增双端远程控制模式：用户在被控端 B 开启远控并获得 7 位连接码，在主控端 A 输入连接码后自动完成公网配对。此后，A、B 均可在各自的 Pinvou 消息窗口向同一个 B 权威 Session 发起任务，B 使用自身执行环境完成模型调用、工具调用、Skill、附件处理和成果生成，双方输入、任务状态与输出在 A、B 两端同步显示。

A 还可以选择将自己已经配置的 API-Key 模型作为单次任务的临时模型。该模式下，模型配置和凭证随任务通过端到端加密通道发送到 B，仅在 B 内存中短暂存在，不写入 B 的配置、会话或日志。这里的端到端加密以受信 Relay 完成配对认证和握手 transcript 绑定为前提，目标是避免被动窃听以及日志、存储和普通转发节点泄漏业务明文；首版不宣称抵御已恶意化的 Relay 主动替换双方握手密钥。

由于功能允许远程执行工具、访问 B 的工作区并临时使用 A 的模型凭证，本需求将身份认证、端到端加密、权限审批、单主控约束和敏感信息生命周期作为发布阻断项，而非后续优化项。

## 2. 产品目标与非目标

### 2.1 产品目标

1. 支持两台 Windows 设备上的 Pinvou Agent 通过现有 Relay 完成公网配对。
2. B 开启远控后向 Relay 申请并展示由 Relay 唯一生成的简短、易输入、短期有效且一次性使用的 7 位连接码。
3. A 输入有效连接码后自动连接 B，不要求 B 再次点击授权。
4. A 可以远程创建或继续任务；连接建立后，A、B 的用户输入必须进入同一个 B 权威 Session，并实际在 B 的模型、工具、Skill 和文件环境中执行。
5. A 可以选择使用 B 当前模型，或将 A 的 API-Key 模型配置作为单次任务临时发送给 B 使用。
6. A 负责审批远程任务产生的权限请求，B 保留随时终止连接的本地控制权。
7. A、B 任一端提交的用户消息，以及对应的模型流式输出、工具过程、权限请求、错误、取消和成果状态，均在 A、B 两端一致显示。
8. B 保存权威执行会话，A 保存独立、可离线查看的远程会话历史。
9. 同一时刻一台 B 只允许一个远程交互客户端控制，禁止静默顶替。
10. 在不破坏现有 WebUI v2 远控能力的前提下复用已有 Relay、RPC、事件流、附件和 Session 基础设施。
11. 普通本地、ACP 与远程会话必须复用同一套会话 UI 和可共享的薄用例编排；Rust 端不新增统一运行时层，而是由 Tauri Command/用例按会话类型协调并列的 `SessionStore`、`EnginePool`、`AcpPool` 与 `RemoteControl`。

### 2.2 非目标

- 首版不支持 macOS、Linux、移动端作为 A 或 B。
- 不实现局域网 IP 直连、连接码本地解码 IP、NAT 打洞或端口映射。
- 不允许 Relay 执行模型调用、工具调用或保存会话正文、附件、成果、模型配置及 API Key。
- 不把 A 的全局配置、全部模型列表、全部 Skill 源码或工具安装包同步到 B。
- 不传递 OAuth、网页登录态、订阅账号 Cookie、企业 SSO 会话或仅 A 本机可用的模型服务凭证。
- 不允许 A 绕过 B 的既有系统权限、工作区边界、工具安全策略和平台能力限制。
- 不支持多个 A 同时控制同一 B，也不支持多人协同编辑同一远程会话。
- 不承诺在 B 已关机、退出应用或主动停用远控后继续执行或连接。
- 不以“连接成功”隐含允许后台持久控制；每次 B 重新开启或刷新远控都必须生成新的配对关系。

## 3. 名词与角色

| 名称 | 定义 |
|---|---|
| 主控端 A | 输入连接码、向共享权威会话发送指令、选择模型来源、审批权限并保存独立历史的 Windows Pinvou 客户端 |
| 被控端 B | 开启远控、保存权威 Session、向同一会话发送本地指令、执行模型和工具任务、管理本机文件及成果的 Windows Pinvou 客户端 |
| Relay | 复用现有 `remote-control-relay`；它是受信的配对认证与租约控制面，负责端点在线状态、一次性配对、认证、单控制租约和密文转发，但按最小知情原则不持有正常会话解密密钥或业务明文 |
| 连接码 | Relay 使用 CSPRNG 唯一生成并原子占用的 7 位短期一次性字母数字码；B 只负责申请、解密展示和刷新，不参与随机码生成，也不直接编码 IP 或模型信息 |
| 配对记录 | Relay 内存中的短期记录，关联连接码摘要、B 端点、过期时间、协议能力和临时握手材料 |
| 配对预留 | 有效 A claim 原子取得的短期独占权，绑定 A 连接身份和本次握手；预留期间连接码不可被其他客户端使用，但尚未创建控制租约 |
| 交互租约 | Relay 对 B endpoint 维护的唯一远程交互槽位，租约类型为既有 Web lease 或新增桌面 controller lease，二者互斥 |
| 控制租约 | Relay 在 A、B 握手确认完成后为已认证 A 签发的桌面 controller lease；B 同时只能存在一个有效交互租约 |
| 连接会话 | A 与 B 从配对成功到 B 停止/刷新远控或租约终止之间的内存态安全连接 |
| 任务 | A 或 B 向 B 权威 Session 发起的一次用户任务，具有唯一 `task_id`、幂等请求 ID、明确来源和完整生命周期 |
| B 模型 | B 本机当前已配置并可用的模型，由 B 的安全存储和模型配置管理 |
| A 临时模型 | A 随单次任务端到端加密发送给 B 的 API-Key 模型配置，不成为 B 的持久模型 |
| 权威会话 | B 持久化的实际执行 Session，是任务状态、消息和成果的最终事实源 |
| A 独立历史 | A 根据 B 的权威快照与事件，在本机 `SessionStore` 中保存的已认证远程会话投影；它与 B 的权威 Session 分别管理 |
| 薄用例编排 | Tauri Command 边界内或从 Command 提取的普通 Rust 函数/应用服务，负责参数校验、准入、权限、调用顺序、失败收口和事件投影；不持有第二份会话、引擎或远控状态 |
| 远程代理会话 | A 在本机 `SessionStore` 中保存的 `RemoteProxy` 类型会话；拥有 A 本地会话 ID、UI 元数据、已认证历史投影及与 B opaque session reference 的后端绑定，但不拥有 B 的执行与终态权威 |
| 远程会话引用 | A 端可信 Rust `ControllingSessionManager` 生成的 opaque `remote_session_ref`；B 在认证请求后将其幂等映射到唯一权威 Session 并回传确认，A 再将它与本地 Session ID、connection/lease/generation 绑定；前端只能原样回传，不得解析、构造、改写或跨绑定复用 |
| RemoteControl | 与 `SessionStore`、`EnginePool`、`AcpPool` 并列的 Rust 远控能力组件，负责连接、租约、加密通道、请求关联、恢复和远端事件代理；不替代会话存储或执行引擎 |
| 桌面 endpoint enrollment | 公网 Relay 对 B endpoint 的正式注册权威；只有预先 enrolled/allowlisted 且持有不可导出设备凭证的桌面端才能首次注册，不能使用协议 v2 的调用方自带凭证进行公网 TOFU |
| `remote_control_generation` | B 的 RemoteControlManager 在每次开启或完成刷新时创建的进程内单调世代，所有配对、租约、任务、回包和自动换码均绑定该世代 |

## 4. 已确认产品决策

| 决策项 | 确认结果 |
|---|---|
| 网络范围 | 支持公网，使用现有 Relay，不要求 A、B 位于同一局域网 |
| 连接码 | Relay 使用 CSPRNG 和固定 31 字符表 `23456789ABCDEFGHJKMNPQRSTUVWXYZ` 唯一生成 7 位随机码，并一次性解析到 B 端点；不编码 IP、端口或模型信息 |
| 有效期 | 默认 5 分钟；B 停止或刷新远控时立即失效 |
| 配对次数 | claim 先原子预留；握手确认后以单个原子转换消费码并创建控制租约；只能有一个 A 成功 |
| B 端确认 | A 持有效连接码时自动连接，不要求 B 二次授权 |
| 设备记忆 | 不记住 A；B 重启、停用或刷新后必须重新配对 |
| 控制并发 | B 同时只允许一个远程交互客户端，不允许新客户端静默顶替 |
| 模型来源 | A 可选择 B 当前模型或 A 临时模型 |
| 模型传输 | A 临时模型的完整必要配置随每次任务发送，不写入 B 配置 |
| 权限审批 | A 是远程任务唯一审批权威；B 只能查看、停止任务或断开，不能 Allow；超时、断线或无法校验时默认拒绝 |
| Relay 信任 | Relay 是受信的配对认证控制面；端到端加密抵御被动窃听、转发日志和存储泄漏，不承诺抵御恶意 Relay 主动中间人攻击 |
| 重连宽限 | 同一进程内存身份可在相应角色最后有效认证 liveness 后的 30 秒总预算内恢复；B 的远程任务预算只以 A 存活证据为准。预算包含黑洞检测，超时、Relay 重启或任一端进程重启均须重新配对 |
| 公网注册 | 正式 desktop endpoint enrollment/allowlist 是 P0 发布门禁；生产 Relay 禁止未 enrolled endpoint 首次自注册 |
| 任务范围 | 支持模型、工具、Skill、附件和成果文件的完整任务能力 |
| 会话保存 | B 保存权威会话，A 保存独立历史 |
| 双端输入 | A、B 的输入统一提交到当前映射的 B 权威 Session；不得在 B 另开普通本地 Session 形成两条上下文 |
| 会话组件架构 | `SessionStore`、`EnginePool`、`AcpPool` 与 `RemoteControl` 作为并列 Rust 能力由薄 Command/用例编排协调；本地普通会话使用 `SessionStore + EnginePool`，本地 ACP 使用 `SessionStore + AcpPool`，A 远程会话使用 `SessionStore + RemoteControl` |
| 控制面与会话面 | 连接码、握手、租约、liveness、resume 和断开属于远程控制面；会话、任务、权限、附件、成果和历史属于统一会话面。连接成功后才允许创建 live remote binding；离线只读历史上下文不要求连接且不得发送业务帧 |
| 平台范围 | 首版仅 Windows |

## 5. 总体架构

```text
┌──────────────────────── 主控端 A（Windows） ────────────────────────┐
│ ChatView → JS/Tauri Bridge → 薄 Tauri Command/用例编排             │
│                         ├─ SessionStore（RemoteProxy/历史投影）   │
│                         └─ RemoteControl（连接/租约/加密代理） │
│ 本地普通/ACP 会话则分别编排 SessionStore + EnginePool/AcpPool │
└───────────────────────────┬─────────────────────────────────────────┘
                            │ RemoteControl: WSS + 应用层端到端密文
                            ▼
┌──────────────────────── 现有 Remote Relay ─────────────────────────┐
│ 受信配对认证 / 配对预留 / 单交互租约 / 在线状态 / 限流 / 密文转发  │
│ 正常运行不持有会话密钥和模型 Key，不解释或保存业务正文             │
└───────────────────────────┬─────────────────────────────────────────┘
                            │ WSS + 应用层端到端密文
                            ▼
┌──────────────────────── 被控端 B（Windows） ────────────────────────┐
│ RemoteControl 受控入口 → 解密/校验 → 薄本地用例编排         │
│                         → SessionStore + EnginePool/AcpPool          │
│ 权限 / 附件 / 成果 / 权威 Journal                                  │
│ A 临时模型配置仅驻留单次任务内存                                   │
└─────────────────────────────────────────────────────────────────────┘
```

### 5.1 复用边界

以下能力应优先复用现有实现：

- Relay 的 WebSocket 接入、端点注册、限流、连接容量和心跳机制；
- endpoint、lease、撤销、离线和错误状态模型；
- 桌面端 `RemoteControlManager`、RPC 白名单、幂等台账和响应大小边界；
- `stream_epoch + seq + after_seq` 事件游标、journal、快照和重连恢复；
- EnginePool、Session 权威提交、任务准入锁和消息投影；
- Web 附件分块上传、一次性句柄、Session 工作区落位和清理机制；
- Session 成果路径校验、分块预览和下载能力；
- 平台能力快照和旧版本降级机制。
- 现有 `ChatView`、本地 Session、EnginePool、ACP、权限、附件、成果和历史的应用层语义，作为共享 UI 与薄用例编排的行为基线。

以下属于新增能力：

- 7 位短期一次性配对记录、独占预留和握手成功后的原子消费；
- 正式 desktop endpoint enrollment/allowlist 与 B `remote_control_generation`；
- Windows 桌面主控客户端角色与远程设备 UI；
- A、B 之间的应用层端到端加密；
- Web lease 与桌面 controller lease 共享单一交互槽位的“不顶替、忙碌即拒绝”语义；
- A 临时模型配置的任务级封装、使用和清理；
- 从 B 转发到 A 的远程权限审批闭环；
- A 在 `SessionStore` 中的 `RemoteProxy` 会话、已认证历史投影及与 B 权威 Session 的对账关系。
- 按会话类型协调 `SessionStore + EnginePool`、`SessionStore + AcpPool` 或 `SessionStore + RemoteControl` 的薄 Command/用例编排；
- 将远程连接控制面与会话数据面分离，让有效 live binding 和无需联网的 `RemoteProxy` 只读历史投影分别接入统一 `ChatView`。

### 5.2 依赖方向

业务逻辑必须遵循项目既有模块边界。下图仅为 Tauri 宿主的模块落位与调用方向示意，不表示每个节点都是独立模块，也不替代跨模块的窄接口：

```text
React UI / ChatView
  → platform bridge（仅 invoke/listen 适配）
  → 薄 Tauri Command / 普通 Rust 用例函数
      ├─ 本地普通：SessionStore + EnginePool
      ├─ 本地 ACP：SessionStore + AcpPool
      └─ A 远程：SessionStore(RemoteProxy) + RemoteControl
           → controller session service / encrypted lane / Relay
           → B 端 RemoteControl 受控入口
           → B 薄本地用例 → SessionStore + EnginePool/AcpPool
```

图中的远控 UI、platform bridge、远控 Command 和 RemoteControl 领域实现分别落于 `pinvou3-app/src/features/remote-control/`、`pinvou3-app/src/platform/tauri/bridge/desktop-remote-control.js`、`pinvou3-app/src-tauri/src/app/commands/desktop_remote*.rs` 和 `pinvou3-app/src-tauri/src/features/remote_control/`；这些路径说明不改变图中的薄用例编排和 B 唯一执行权威。

- 远控业务状态不得继续堆入全局 `main.jsx`。
- Windows 专属网络或凭证内存保护放入远控功能内的 `pinvou3-app/src-tauri/src/features/remote_control/platform/windows.rs` 适配，不放入全局 `src-tauri/src/platform/`。
- Engine、Session、工具和 Skill 不得反向依赖远控 UI。
- Relay 只承载连接和密文，不引入 Pinvou 业务执行逻辑。
- Command 只负责 IPC 适配；复杂但可共享的校验、准入、附件准备、任务提交和失败收口应提取为普通 Rust 用例函数，不得形成新的有状态 Runtime。
- A 远程用例必须同时使用 `SessionStore` 和 `RemoteControl`：前者维护 A 本地 `RemoteProxy` 会话、UI 元数据和已认证投影，后者负责连接、加密发送、事件接收和恢复。
- A 远程用例不得调用 A 本机 `EnginePool`/`AcpPool`，也不得将 `RemoteProxy` 的投影、pending 记录或离线副本当作 B 的权威状态。
- B 端 RemoteControl 受控入口在解密和租约校验后，必须调用与本地 Command 共享的普通 Rust 用例函数，进入同一任务准入、Session、权限、附件、成果和执行服务；远控领域层不得反向调用 `#[tauri::command]` 函数或复制第二套底座。
- 连接码、握手、租约、liveness、resume 和显式断开保持独立控制面；不得因统一会话接口而把连接生命周期隐藏为普通 Session 状态。

### 5.3 信任边界与无确认认证

- A、B 必须信任所配置 Relay 的服务身份、配对判定和握手绑定结果；官方 Relay 通过客户端内置的服务身份信任链，自托管 Relay 通过管理员显式配置的 TLS/签名公钥建立信任。
- B 注册时提交经正式 enrolled desktop endpoint 凭证认证的握手临时公钥；A claim 时提交连接码和 A 握手临时公钥。A 身份不是用户账号或长期设备 ID，而是 Relay 为当前已认证 TLS/WebSocket claim socket 创建的不可猜测临时 `controller_connection_identity`；reservation owner 必须绑定该 socket 身份，socket 断开后不得迁移。Relay 每次进程启动生成不可预测的 `relay_epoch`，并生成规范化 transcript，至少包含 `relay_epoch`、`pairing_id`、`reservation_id`、B endpoint、`remote_control_generation`、A/B 角色和握手临时公钥、协议版本、能力摘要哈希、过期时间和随机 nonce，再用部署级签名密钥签名后把完全相同的 transcript 发给 A、B。
- B 的 `code_wrap_keypair` 只用于 Relay 密封和 B 解密展示码，必须与 A—B `handshake_keypair_b` 分别调用 CSPRNG 独立生成、使用不同类型/用途标签且不得互相派生或复用；`code_wrap_public_key` 不进入握手 transcript，握手公钥也不得用于包裹连接码。展示码确认接收且幂等注册窗口结束后销毁 code-wrap 私钥。
- A、B 都必须验证 Relay 服务身份、transcript 签名、角色、本端握手公钥、B `remote_control_generation` 和过期时间，再以标准临时密钥交换和绑定 transcript 哈希的 KDF 派生会话密钥、双方独立 Finished key和会话恢复密钥。Finished receipt 不从双方共享握手秘密派生，而使用每个发送方独立 CSPRNG 生成、仅放入其加密 Finished 的随机 receipt secret；必须按 9.3 节的 commitment→Finished→receipt ACK 顺序完成后才提交租约。该机制无需 B 二次点击，可阻止旁路攻击者或被动转发节点替换密钥。
- 建连后 A 存活证明默认复用 A→B 方向会话密钥形成端到端 AEAD liveness，Relay 只盲转发；Relay 签名的新鲜证明仅是直接心跳暂不可用时的受信控制面兜底，并按 6.7 节扣除观察年龄、完整 RTT 与安全裕量。
- 因 Relay 同时是双方信任的认证控制面，恶意 Relay 可向两端签发不同的伪造绑定并主动中间人转发。首版把这一情形列为剩余风险，不以“零信任 Relay”或“可抵御 Relay 主动 MITM”对外宣传；若未来需要抵御，必须另增独立设备身份验证或用户比较码流程，不得仅修改文案。

### 5.4 公网 desktop endpoint enrollment 门禁

- `remote-control-relay/PROTOCOL.md` 已明确指出 origin 校验和调用方自带的首次注册凭证不是生产 enrollment 系统。本功能不得把现有 `desktop_endpoint_register` 的“首次请求创建 endpoint”语义直接暴露到公网。
- 官方 Relay 必须由 Pinvou 的已认证设备开通流程预先签发 endpoint ID 和不可导出、可撤销的设备凭证，并在服务端 enrollment registry 中处于 enabled 状态；账户退出、设备撤销或风控禁用后不能注册配对。
- 自托管 Relay 必须由管理员通过部署配置、离线 enrollment token 或管理面显式 allowlist endpoint 并签发凭证；默认配置不得允许匿名 TOFU。开发/测试若启用临时自注册，必须是明确的非生产开关且启动日志持续警告，生产构建或生产配置检测到该开关必须拒绝启动。
- `desktop_pairing_register` 必须同时验证现有 PROTOCOL v2 的 endpoint/credential/Origin/限流约束和正式 enrollment 状态；未 enrolled、已撤销或凭证不匹配均失败关闭，不生成连接码。该门禁是公网开放、灰度和完成定义的 P0 条件。
- 官方 Relay 上线门禁前必须提供既有 endpoint 认领窗口：升级后的 B 在现有已认证 `desktop_endpoint_register` socket 上提交 `desktop_endpoint_claim`，同时证明当前 endpoint 的 access token/desktop secret 和 Pinvou 已登录设备身份；Relay 校验当前内存 credential hash 后，把原 `endpoint_id + access_token hash + desktop_secret hash` 原子迁入持久 enrollment registry，不改变现有 Web 分享 URL、fragment token 或 endpoint ID。
- 未在认领窗口完成迁移的官方用户，B 必须显示“此设备需要重新认领远控”并提供登录后恢复入口。恢复走独立、限流且可审计的 enrollment API，不开放通用 WebSocket TOFU；当 endpoint ID 尚未被其他设备认领且用户能证明原凭证时保留原 ID/token，已有 Web 分享链接在 B 恢复上线后自动续接。证明失败、归属冲突或凭证遗失时必须人工撤销旧 endpoint 并签发新 endpoint，UI 明确要求重新分享链接。
- 自托管升级必须提供管理员 dry-run 和批量迁移工具，将明确清单中的既有 endpoint/credential hash 导入 allowlist；无法导入的设备由管理员签发一次性 enrollment token 重新 enroll。保留原 endpoint ID/token 时旧 Web 链接继续使用；选择轮换凭证时必须明确列出会失效的分享链接并要求重新分享。
- enrollment registry 迁移前必须备份并支持 schema 回滚。首版应把 registry 和码隔离 journal 放在独立的版本化存储中，不直接改写旧 `PINVOU_REMOTE_STATE_PATH` 的严格 v2 revoke schema；若未来合并存储，必须先部署可双读的过渡版本。回滚只关闭桌面双端配对入口，不删除 registry、码隔离 tombstone 或既有 endpoint 凭证；旧 Relay/WebUI 路径继续使用未变化的 endpoint ID/token。不得以回滚为由重新开放匿名首次注册。
- 生产最低可回滚版本必须原生读取 enrollment registry 并对首次注册执行同等门禁；若只能回滚到不理解 registry 的旧二进制，必须先部署独立前置 enrollment gateway，由 gateway 读取同一权威 registry、验证 endpoint/credential/enabled 状态，并在 WebSocket upgrade 或 `desktop_endpoint_register` 到达旧 Relay 前拒绝未 enrolled 首次注册。旧 Relay 不得绕过 gateway 直连公网，gateway 不得改变已 enrolled 旧 endpoint 的 ID/token 或 Web 分享链路。
- 回滚编排必须先验证“registry 可读、gateway/旧 Relay 拓扑无旁路、拒绝审计和旧 Web fixture 通过”，再切换旧二进制；任一条件不满足即拒绝回滚并保持当前安全版本。禁止回滚到裸 PROTOCOL v2 Relay 后仍宣称满足生产 enrollment 门禁。

## 6. 核心用户流程

### 6.1 B 开启远程控制

1. B 用户打开“远程控制”面板。
2. B 点击“开启桌面远程控制”。
3. B 在本地状态锁内创建新的 `remote_control_generation` 和稳定 `registration_request_id`，分别生成仅驻留内存且不可互换的 `code_wrap_keypair` 与 A—B `handshake_keypair_b`，并以已 enrolled、已认证 endpoint 向 Relay 注册配对记录；B 不生成连接码。
4. Relay 幂等校验注册请求后，使用 CSPRNG 从固定 31 字符表采样 7 位码。对每个候选，Relay 必须在同一 keyring 快照中使用当前摘要密钥以及所有仍被活动 pairing 或未过期 tombstone 引用的旧摘要密钥分别计算摘要，并对各 key ID 的活动码与隔离集合执行原子查重/占用；任一摘要命中都视为碰撞。发生碰撞最多重新采样 32 次；仍未占用成功则返回 `pairing_code_generation_failed`，不得创建或返回半成品记录。
5. 原子占用成功后，Relay 在返回前持久写入 8.5 节的有界隔离 tombstone，只保存摘要，并保存以 B 的 `code_wrap_public_key` 密封的展示码密文以支持同一请求幂等重试；Relay 将该密文、`pairing_id` 和过期时间返回 B，B 解密后显示连接码、剩余有效时间、刷新和停止按钮。Relay 不持久化、记录或再次从摘要还原明文码，也不得用 A—B 握手公钥包裹展示码。
6. 连接码过期后不得继续兑换；B 可点击刷新并向 Relay 申请新码，旧 pairing 立即失效，其字符串进入 8.5 节隔离。
7. 刷新时，B 先把旧 generation 标记为 retiring 并停止接受其回包，再撤销该 generation 的配对记录、reservation、控制租约，取消远程来源的在途任务并清理密钥/凭证；只有收到 Relay 撤销确认，或通过新 `relay_epoch` 证明旧状态已不存在后，才能创建下一 `remote_control_generation`、新的稳定注册请求 ID 并申请由 Relay 生成的新码。撤销无法确认时失败关闭，不展示新码。
8. B 关闭远控时先使当前 generation 本地失效，再撤销其配对记录、控制租约和连接会话密钥，取消远程来源的在途任务，并清理所有 A 临时模型材料；stop 后到达的旧 generation 注册、自动换码、握手或任务回包必须丢弃，且不得自动开启新 generation。

### 6.2 A 使用连接码配对

1. A 打开“连接远程设备”。
2. A 输入 7 位连接码；输入框自动转为大写并忽略展示分隔符。
3. A 向 Relay 提交配对 claim、自身临时握手公钥和稳定 `claim_request_id`。
4. Relay 在确认 B 没有任何交互租约后，原子地把配对记录从 `waiting_for_pairing` 转为绑定该 A 的 `pairing_reserved`；此时连接码对其他客户端不可用，但尚未最终消费，也未创建控制租约。
5. Relay 生成并签名 5.3 节定义的同一份握手 transcript；A、B 验证后进入 `handshake_pending` 并协商端到端密钥。
6. 在发送 Finished 前，A、B 各自用 CSPRNG 独立生成恰好 32 字节的 `finished_receipt_secret_<sender>`，通过自己的角色认证 socket 向 Relay 提交 `finished_receipt_commit`。commitment 必须且只能按 9.3 节唯一的域分离、长度前缀算法计算；Relay 原子记录每个 sender role 唯一 commitment。
7. commitment 被 Relay 确认后，发送方才把原始 receipt secret 放入由本角色独立 Finished key 认证的 `encrypted_handshake_finished`，连同同一完整绑定发送给对端。发送方把 secret 交付对端的唯一协议路径是该加密 Finished；commit 消息、共享握手 KDF、日志和遥测都不得包含 secret 明文。
8. 接收方成功解密、认证并核对对端 Finished 后，恰好一次通过自己的角色认证 socket 提交 `finished_receipt_ack`，回传 `sender_role` 和收到的 32 字节 receipt secret。Relay 按 9.3 节复算 commitment，并验证接收 socket 必须是相反角色；A 的 ACK 只能证明收到 B Finished，B 的 ACK 只能证明收到 A Finished。ACK 是接收方在收到 Finished 之后对 Relay 的必要回传，不与上一条“发送方只通过加密 Finished 向对端发送 secret”冲突。
9. Relay 仅在两个发送方 commitment 和两个相反角色 receipt 均有效、未使用、未超时且所有绑定仍匹配时，原子地消费连接码并创建唯一 controller lease；B 自动接受，无需本地确认。双方可共同派生的任何 key、单方 Finished 或单方自述 ACK 都不得作为 receipt 证明。
10. A 进入远程消息窗口，B 显示“已被远程连接”和当前连接状态。
11. 若 B 已被 WebUI 或其他桌面主控占用，Relay 在预留前返回“设备正在被控制”，不消费连接码且不得顶替原租约。
12. 预留或握手超时、任一方断线、commitment/Finished/receipt 损坏、重放或验证失败时，Relay 撤销预留并使原 `pairing_id` 永久不可恢复或复用；对应 7 位字符串按 8.5 节进入隔离，但未来可分配给全新 pairing。B 当前 `remote_control_generation` 仍有效、远控仍开启且在线时，使用新的稳定注册请求 ID 自动申请并展示由 Relay 生成的新码，A 必须使用当前 B 新展示的码重试。

### 6.3 A 发送使用 B 模型的任务

1. A 在远程消息窗口选择“使用被控端模型”。
2. A 输入指令，可选择附件和 B 暴露为可用的 Skill/任务能力。
3. A 发送任务，使用稳定 `client_request_id` 防止重试重复创建任务。
4. B 先完成任务准入、附件消费和 Session 创建/恢复，再使用 B 当前模型执行。
5. B 将用户消息、任务状态、流式输出和终态写入权威 Session，并同步给 A。
6. A 将已确认的权威事件写入自己的独立历史。

### 6.4 A 发送使用 A 临时模型的任务

1. A 选择“使用我的模型”，并从本机可传输的 API-Key 模型中选择一项。
2. A 在本机安全边界内读取该模型本次调用所需的配置和凭证。
3. A 将模型配置绑定到当前 `connection_id + controller_lease_id + lease_epoch + task_id`，通过端到端加密信道发送。
4. B 校验配置结构、大小、协议和 endpoint 安全策略，仅在任务内存中创建临时模型上下文。
5. B 使用该临时上下文完成模型调用，不将其加入 B 的模型列表或持久配置。
6. 任务结束、取消、失败或连接终止时，B 清除临时模型上下文和凭证缓冲。
7. B 不得在错误、事件、会话、工具输入、诊断和崩溃信息中返回 API Key。

### 6.5 权限审批

1. B 的任务触发需要审批的工具、命令或文件操作。
2. B 暂停该操作，生成唯一 `permission_request_id`，向 A 发送结构化审批请求。
3. A 展示操作类型、风险等级、目标范围、理由和必要的安全预览；A 是该远程任务的唯一审批权威。
4. A 选择允许或拒绝；首版仅支持单次、当前任务范围内的决定。B 本地只能查看请求、停止任务或断开连接，不提供 Allow/Approve 操作。
5. B 以原子 compare-and-set 接受首个属于当前连接、controller lease、任务且未过期的 A 决定；同决定重试幂等，迟到或冲突决定只能返回终态错误，不能改变已提交结果。
6. A 断线、租约变化、请求超时或响应校验失败时，B 必须默认拒绝。
7. B 用户可随时在本机停止远控；停止后所有待审批请求立即拒绝。

审批 ID 由 B 的审批权威签发，采用进程随机标识和不回退的单调序号；调用方不能指定、重新注册或在缓存回收后复用旧 ID。A 的决定与重试只回传 B 返回的不透明 ID，不解析其内部格式。B 的本地注册输入不再接受 `permissionRequestId`，旧调用方必须改用注册结果中的 ID；传入旧字段明确拒绝，不做静默兼容。

容量只统计待决定或已允许但尚未派发的请求，上限为 8；已拒绝、已过期和已派发记录不占活动槽位。终态记录在 B 的同一单调时钟域中至少保留 10 分钟，且不早于原请求有效期，包含活动记录的缓存总上限为 256。保留窗口内满时暂时拒绝新请求，不提前淘汰幂等结果；窗口外回收后，旧 ID 的响应返回不可确认/未知，不得批准新请求或把已派发结果改成未执行。

### 6.6 附件与成果

1. A 从本机选择附件，经端到端加密后通过 Relay 分块传输到 B。
2. B 复用既有上传配额、类型识别、一次性句柄和 Session 工作区机制。
3. 附件正文只在 A、B 端解密，Relay 只能看到有界密文帧和必要元数据。
4. B 生成的成果归属于 B 的权威 Session 和本机工作区。
5. A 可查看成果元数据、请求预览或显式下载副本。
6. 下载到 A 后由 A 选择保存路径；B 的绝对路径不得写入 A 历史。
7. A 删除本地副本或独立历史不得删除 B 的成果；B 删除成果后 A 再次拉取应明确显示不可用。

### 6.7 断线与恢复

1. 当前 pairing 在握手成功并创建租约的原子提交后终结；失败的预留或握手也终结该 pairing。连接码不是 resume 凭证，即使相同字符串未来重新分配，也不能恢复旧租约。
2. Relay 分别向 A、B 签发不同的 `resume_token_a`、`resume_token_b`；令牌仅驻各自内存，绑定 `role + relay_epoch + connection_id + controller_lease_id + lease_epoch + remote_control_generation`，不得跨角色交换或共享。同一建租约事务还为 A、B 分别生成新的 128-bit 不透明 `liveness_epoch_a/b`，经 `controller_joined` 绑定到当前 E2E 会话；只有后续成功的 `controller_resumed` 才能替换它们。
3. 已连接时 A、B 各自每 5 秒向 Relay 发送一次绑定 `role + relay_epoch + connection_id + controller_lease_id + lease_epoch + remote_control_generation` 的 transport `controller_liveness`，A、B 使用互不共用的 `liveness_epoch_<role>` 与单调 `heartbeat_sequence_<role>`。A 在同一 5 秒逻辑周期的 transport 心跳和端到端响应必须使用同一 `(liveness_epoch_a, heartbeat_sequence_a)`，不同逻辑心跳不得复用序号。Relay 分别维护 `last_liveness_a` 与 `last_liveness_b`；任一角色的心跳只能推进该角色记录，B 的高序号或持续在线绝不能推进 `last_liveness_a`，A 同理不能推进 `last_liveness_b`。
4. B 每 5 秒生成一次 128-bit 随机 `liveness_challenge_a` 并以 B 本地单调时钟记录 `challenge_sent_at_b`，经 Relay 转发给 A。A 收到后使用当前 A→B 端到端方向密钥发送 `encrypted_controller_liveness_a`；AEAD 认证内容必须绑定 `connection_id + controller_lease_id + lease_epoch + relay_epoch + remote_control_generation + liveness_epoch_a + role=A + heartbeat_sequence_a + challenge`。Relay 只盲转发该密文，不能生成或验证其中 MAC。B 在本地单调时刻 `received_at_b` 验证 AEAD、当前绑定、未使用 challenge 和严格更新的 `(liveness_epoch_a, heartbeat_sequence_a)`，并计算 `measured_rtt_ms = received_at_b - challenge_sent_at_b`；RTT 超过 5000 ms 的响应不用于推进 deadline。直接路径的候选值固定为 `candidate_deadline = received_at_b + max(0, 30000 - measured_rtt_ms - SAFETY_MARGIN_MS)`，其中 `SAFETY_MARGIN_MS = 1000`。因为 A 只能在 challenge 发出后生成该 liveness，真实 A liveness 到 B 收件的时间不大于完整 RTT，故总预算严格不超过 29 秒并保守小于 30 秒。
5. 仅当端到端响应暂不可用时，B 才可请求 Relay attestation 兜底。Relay 用自身单调时钟记录 A transport 心跳的 `observed_at_relay`，签发时计算 `observed_age_ms = issued_at_relay - observed_at_relay` 和 `remaining_ttl_ms = max(0, 30000 - observed_age_ms)`；证明签名绑定 B 的一次性 challenge、当前连接/租约/epoch/generation、A 的 liveness epoch/sequence、Relay attestation sequence、`observed_age_ms`、`remaining_ttl_ms` 与仅供 Relay 审计的 `issued_at_relay`。B 不跨机器比较 `issued_at_relay`：只用自身单调时钟测量请求 RTT，并要求 `observed_age_ms <= 5000`、`measured_rtt_ms <= 5000`、challenge/attestation sequence 未使用。兜底候选值固定为 `candidate_deadline = received_at_b + max(0, min(remaining_ttl_ms, 30000 - observed_age_ms) - measured_rtt_ms - SAFETY_MARGIN_MS)`。在最大 5 秒 age 与 5 秒 RTT 下仅剩 19 秒，最坏总计不超过 29 秒。
6. B 按已认证 A liveness 的 `(liveness_epoch_a, heartbeat_sequence_a)` 更新 `local_safety_deadline_a`：只有已由当前 `controller_joined/controller_resumed` 安装的新 epoch 或当前 epoch 的更大 sequence，才能直接用该证据的 `candidate_deadline` 替换旧值，因此后续真正新鲜心跳可以推进 deadline；消息自行声明的未知 epoch 一律拒绝。相同 sequence 的直接/兜底重复证据只能取 `min(现有值, candidate_deadline)`，更旧序号、重放或乱序一律拒绝。禁止对新序号使用 `min(现有 deadline, candidate)`，也禁止无条件设置为 `received_at_b + 30000`。连续 3 个 A 心跳周期（15 秒）无新证据即 suspect，但清理仍不晚于最后接受的候选 deadline。
7. B 自己的心跳、B 与 Relay 的普通租约消息、未认证/TCP 活动、journal 活动以及 provisional/pending resume 消息均不得推进 `local_safety_deadline_a`。B transport 是否在线由 `last_liveness_b` 单独判断，供 Relay 停止向失联 B 路由及收尾使用。controller socket 显式关闭只可缩短相关 deadline，不得创建新的 30 秒预算；网络黑洞也必须在相应角色最后有效证据的保守 deadline 到期。Relay 保持 resume 的唯一权威。
8. A、B 必须各自在相关角色 deadline 与 B 的 `local_safety_deadline_a` 中较早者之前，通过角色绑定 socket 提交自己的令牌和稳定 resume request ID。Relay 只把两个令牌分别原子预留；双方 prepare 齐全后进入 `resume_provisional`，签发绑定双方新 transport challenge 的 `activation_id`，只允许 resume 控制消息和 `encrypted_resume_confirm`，禁止承载、缓存或转发任何业务 RPC、事件、审批、附件或成果帧。
9. A、B 在 provisional 状态以 `resume_secret` 验证新 transport；B 只有在原 `local_safety_deadline_a` 前验证 A 的恢复确认、Relay 的 `activation_id` 和所有租约绑定后，才能通过 enrolled B 角色 socket 提交 `controller_resume_activation_ack`。Relay 验证该 ACK 后，原子预留两枚新角色 token 和新传输密钥候选并进入 `resume_activated_pending_liveness`，但旧 token 尚不标记最终消费、watchdog 继续按原 deadline 运行，业务门保持关闭，也不得通知 A 已恢复。
10. 在 `resume_activated_pending_liveness` 中，B 必须在原 deadline 前发送绑定 `activation_id`、当前租约/generation 和新 `heartbeat_sequence_b` 的首个 `controller_post_activation_liveness`。Relay 验证并持久记录后，向 A、B 返回同一 `controller_post_activation_liveness_confirmed`；A、B 各自验证后通过角色认证 socket 恰好一次提交 `controller_resume_ready_ack`。一方 ACK 不得代表另一方。
11. 只有 Relay 在原 deadline 前收到双方 ready ACK，才原子消费两枚旧 token、启用各角色新 token/传输密钥、初始化新的 `liveness_epoch_a/b` 与 sequence 0、将租约转为 `connected`、开放业务帧，并向双方返回 `controller_resumed`。B 收到并验证该最终结果后才取消 watchdog 和接受业务；A 也必须验证同一 activation 结果后才能发送业务。activation ACK、首个 B liveness、Relay 确认、任一 ready ACK 或最终结果丢失/超时均终止 candidate、租约和全部新旧 token，所有暂存帧丢弃，不能出现 Relay active 而 B terminal。
12. B 使用 journal 重放缺失事件；游标过旧时发送快照并重置 epoch。
13. A 失联时，已被 B 接纳的任务仅在 `local_safety_deadline_a` 的剩余总预算内继续执行并写入 B 的权威 Session；B 自身持续心跳不能延长期限。所有待审批和新权限请求立即拒绝，恢复后不得复活。A 完成全部激活门后可恢复仍存在的任务状态和输出。
14. deadline 届满、只有一端 prepare/confirm、缺少 B activation ACK、首个 post-activation liveness 或双方 ready ACK 时，Relay 终止租约，预留的新旧令牌全部作废；B 必须按本地 watchdog 取消远程来源的在途任务、提交明确终态、清除临时模型与会话密钥，并在当前 generation 仍有效、远控仍开启且重新在线时申请由 Relay 生成的新连接码。
15. Relay 重启会生成新的 `relay_epoch` 并丢失全部内存配对、预留、租约和重连状态，现有连接不得恢复；A、B 检测到 epoch 改变或收到恢复状态不存在后立即丢弃旧密钥与各自令牌，B 取消远程来源的在途任务并申请新码，不再等待原 deadline。
16. B 重启、停止远控、刷新连接码、撤销租约或 A/B 任一端进程重启后，旧租约、密钥和两枚重连令牌永久失效；即使令牌字节相同或请求 ID 重放，也不得复用。
17. 仅关闭 A 的远程消息窗口但让后台 RemoteControlManager 和 controller transport 保持连接，不视为断线，已接纳任务可继续并写入 B 权威 Session；显式“断开”、A 进程退出或任何实际 controller transport 中断则进入上述总预算流程。UI 必须明确区分“关闭窗口”和“断开设备”。

## 7. 功能需求

### 7.1 被控端管理

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-B-001 | B 必须提供独立的“桌面远程控制”开关，不与 WebUI 分享链接混淆 | P0 |
| RC-B-002 | 开启成功后必须展示 7 位连接码和精确到秒的剩余有效时间 | P0 |
| RC-B-003 | B 可以刷新连接码；刷新必须先终结旧 pairing 并隔离其码字符串，再展示新码 | P0 |
| RC-B-004 | B 可以停止远控；停止必须撤销配对、租约、会话密钥、待审批和临时模型 | P0 |
| RC-B-005 | B 必须展示等待连接、正在预留、正在安全握手、已连接、30 秒总预算内的断线重连、已停止和错误状态 | P0 |
| RC-B-006 | 已连接时 B 必须展示本机可理解的主控设备名称和连接时间，但不得持久记住 A | P1 |
| RC-B-007 | B 本地必须始终保留“立即断开”入口，且不依赖 A 或 Relay 响应 | P0 |
| RC-B-008 | B 同时只能持有一个远程交互控制租约 | P0 |
| RC-B-009 | B 不得因新 A 或 WebUI 客户端接入而静默替换当前控制者 | P0 |
| RC-B-010 | B 应允许用户查看远程任务正在使用“B 模型”还是“A 临时模型”，但不得展示 Key | P1 |
| RC-B-011 | B 在开放代码模式前必须由本地用户预先授权允许的工作区；未授权时 A 不得看到或使用代码模式 | P0 |
| RC-B-012 | B 本地权限卡只能查看详情、停止任务或断开连接，不得提供允许远程受保护操作的 Allow/Approve 入口 | P0 |
| RC-B-013 | B 每次开启或刷新必须使用新的 `remote_control_generation`；全部注册、配对、租约、任务、自动换码和回包都须绑定 generation，stop/retire 后的旧 generation 回包一律丢弃 | P0 |
| RC-B-014 | 刷新必须先在本地 retire 旧 generation，并确认 Relay 已撤销其配对/租约、完成任务和凭证清理，之后才能创建新 generation；无法确认时失败关闭 | P0 |
| RC-B-015 | B 必须只以最后一条有效 A 端到端认证心跳或 Relay 签名的 A 心跳新鲜证明维护总计 30 秒的不可延长 `local_safety_deadline_a`；黑洞、显式断开或 Relay 不可达时均须最迟在该 deadline 取消远程任务并清理凭证/密钥 | P0 |
| RC-B-016 | controller liveness 间隔固定为 5 秒；连续丢失 3 个 A 心跳周期时启动恢复但不重置 deadline，只有已连接态的新序号 A 存活证据或完整恢复激活成功才能建立新的任务存活周期 | P0 |
| RC-B-017 | A/B 必须使用角色专属单调心跳序号，Relay 独立维护 `last_liveness_a`/`last_liveness_b`；一方心跳、普通租约消息或 TCP 活动不得推进另一方存活记录或 B 的远程任务 deadline | P0 |
| RC-B-018 | A 存活主路径必须是 Relay 盲转发、由 A→B 方向 AEAD 认证并绑定当前连接/租约/epoch/角色/序号/challenge 的 `encrypted_controller_liveness_a`；Relay attestation 只能作为兜底 | P0 |

### 7.2 主控端连接

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-A-001 | A 必须提供“连接远程设备”入口和 7 位码输入框 | P0 |
| RC-A-002 | 输入必须支持粘贴、自动大写和忽略空格/连字符，不接受其他字符 | P1 |
| RC-A-003 | A 输入有效码后自动完成兑换、握手和进入远程会话，不要求 B 确认 | P0 |
| RC-A-004 | A 必须显示连接中、已连接、B 离线、设备忙、统一的“连接码无效或已失效”和版本不兼容；不得向用户区分码不存在、过期、已使用或已撤销 | P0 |
| RC-A-005 | A 必须清晰标记当前为远程会话及被控设备，防止用户误认为任务在本机执行 | P0 |
| RC-A-006 | A 可主动断开当前连接；断开后不能继续发送任务或审批 | P0 |
| RC-A-007 | A 不得保存可跨 B 重启或远控刷新继续使用的设备凭证 | P0 |
| RC-A-008 | A 应保存非敏感的远程会话标题、消息和终态，支持离线查看 | P1 |
| RC-A-009 | 关闭远程消息窗口默认只隐藏 UI、保持后台连接；“断开设备”必须是独立显式动作，并清晰提示其会触发任务的最多 30 秒安全收尾 | P0 |
| RC-A-010 | A 必须提示连接码是短期 bearer secret，不长期保存或反复重试旧字符串；原 pairing 失效后应向 B 获取当前码，因为同一 7 位字符串在隔离期结束后可能分配给另一 pairing | P0 |

### 7.3 连接码与配对

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-P-001 | 连接码严格使用 31 字符表 `23456789ABCDEFGHJKMNPQRSTUVWXYZ`，由 7 个大写字符组成；排除 `0/O`、`1/I/L`，不得由实现自行增删字符 | P0 |
| RC-P-002 | 展示可使用 `XXX-XXXX`，协议层使用无分隔符的 7 位规范值 | P1 |
| RC-P-003 | Relay 是连接码唯一生成方，必须使用 CSPRNG；B/A 不得生成、选择或建议码，码不得从 IP、设备 ID、时间戳或序号推导 | P0 |
| RC-P-004 | 默认有效期为 5 分钟，服务端时间为唯一过期依据 | P0 |
| RC-P-005 | 活动 pairing 记录只在 Relay 内存保存；Relay 重启后未兑换 pairing 全部失效，但码摘要隔离 tombstone 必须按 8.5 节持久保留并在重启后恢复 | P0 |
| RC-P-006 | Relay 不得明文记录连接码，查询键应使用带服务端密钥的摘要或等价防护 | P0 |
| RC-P-007 | claim 必须以 compare-and-set 原子取得唯一预留；多个 A 并发提交时最多一个成为 reservation owner，其他请求不得观察或改变该记录 | P0 |
| RC-P-008 | 握手成功后的“消费当前 pairing + 创建 controller lease”必须是一个原子转换；过期、预留/握手失败、B 刷新或停止后原 `pairing_id` 永久不可恢复或复用，对应 7 位字符串仅在隔离期内禁止重分配 | P0 |
| RC-P-009 | 除 A 本地格式错误外，服务端对码不存在、过期、已使用、已撤销、失效 generation 及并发 claim 失败必须使用同一 `error/pairing_code_invalid` 和用户文案“连接码无效或已失效”，不得泄露 B 地址、端点 ID 或历史状态 | P0 |
| RC-P-010 | Relay 必须按来源、连接和全局维度限制兑换频率，并对异常爆破告警 | P0 |
| RC-P-011 | 连接码必须作为可自动取得控制权的临时 bearer secret 对待，不得进入 URL、日志、埋点或崩溃报告 | P0 |
| RC-P-012 | 只有通过 Relay 桌面端注册策略认证且在线的 B endpoint 才能创建配对记录 | P0 |
| RC-P-013 | 配对预留必须绑定 `reservation_id + claim_request_id + A 临时 claim socket 身份 + A 临时公钥哈希 + B endpoint + remote_control_generation`，不得转让给另一连接 | P0 |
| RC-P-014 | `pairing_reserved` 最长 10 秒，`handshake_pending` 最长 20 秒；失败、超时或任一方断线均撤销预留、终结原 pairing 并隔离码字符串，由仍在线且保持开启的 B 自动申请新码 | P0 |
| RC-P-015 | Relay 必须对全局活动码摘要执行原子 `insert-if-absent`；碰撞最多 CSPRNG 重采样 32 次，耗尽后返回 `pairing_code_generation_failed` 且不得产生半记录 | P0 |
| RC-P-016 | 注册必须绑定 `remote_control_generation + registration_request_id`；同 ID 同参数重试返回同一 `pairing_id` 和密封展示码，同 ID 异参数拒绝 | P0 |
| RC-P-017 | Relay 只可在生成瞬间处理码明文，随后仅保存带服务端密钥的摘要及密封给 B 的幂等响应；禁止持久化、日志记录或从摘要还原明文 | P0 |
| RC-P-018 | Relay 仅在各发送方先按 9.3 节唯一规范提交 32 字节随机 receipt secret 的 commitment、接收方验证相应加密 Finished 后恰好一次从相反角色 socket 回传该 secret，且两个 receipt 都通过时才能完成配对；损坏、单方、跨角色、重放、错绑定或超时均不得消费 pairing 或创建租约 | P0 |
| RC-P-019 | 生产 `desktop_pairing_register` 必须要求 endpoint 已正式 enrolled/allowlisted 且凭证有效；协议 v2 的首次自注册能力不能满足该门禁 | P0 |
| RC-P-020 | B 的 `code_wrap_keypair` 与 A—B `handshake_keypair_b` 必须独立随机生成、类型/用途隔离且不可复用；code-wrap 公钥不得进入握手 transcript | P0 |
| RC-P-021 | Relay 对每个拟签发码摘要必须先持久写入 8.5 节隔离 tombstone，再提交活动 pairing 并返回 B；持久化失败不得发码。隔离结束后允许同一字符串服务于新的 `pairing_id`，但绝不能恢复旧 pairing | P0 |
| RC-P-022 | 官方既有 endpoint 必须支持在原认证 socket 上认领并原子迁入 registry，成功时保持 endpoint ID/token/Web 分享链接；未迁移用户须有明确提示和受控恢复路径 | P0 |
| RC-P-023 | 自托管必须提供 dry-run、批量 allowlist 或逐台 re-enroll；生产回滚目标须原生支持 registry 门禁，或由无旁路的前置 gateway 强制同等门禁，回滚到裸 v2 必须拒绝 | P0 |
| RC-P-024 | 摘要密钥轮换期间，候选码必须用当前密钥及所有仍被活动 pairing/未过期 tombstone 引用的旧密钥逐一计算并原子查重；旧 key 在最后引用到期前不得卸载或遗漏 | P0 |

### 7.4 控制租约

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-L-001 | 配对成功后 Relay 必须签发不可猜测的控制租约 | P0 |
| RC-L-002 | B 已有有效控制租约时，新兑换请求返回 `controller_busy`，不得接管 | P0 |
| RC-L-003 | 控制租约绑定 Relay epoch、B endpoint、`remote_control_generation`、A 临时 claim socket 身份、协议版本和加密会话 | P0 |
| RC-L-004 | 租约终止后到达的响应、事件和审批决定必须丢弃 | P0 |
| RC-L-005 | A 短暂断线只允许同一内存重连身份恢复，不允许其他 A 利用已终结 pairing 或其码字符串接管现有租约 | P0 |
| RC-L-006 | B 停止或刷新远控必须使当前租约及 A/B 两枚 resume token 立即失效 | P0 |
| RC-L-007 | Web lease 与桌面 controller lease 必须竞争同一个 endpoint 交互槽位；任一租约或桌面配对预留存在时，另一类型不得加入或顶替 | P0 |
| RC-L-008 | B 远程任务断线恢复总预算固定为最后有效 A 存活证据后 30 秒，包含 15 秒黑洞检测和后续恢复；B 自身心跳不得延长，超时、Relay 重启或任一端进程重启后必须重新配对 | P0 |
| RC-L-009 | A、B 必须分别持有角色绑定的独立单次 resume token；两个 token 不相等、不可互换，Relay 只保存验证所需摘要 | P0 |
| RC-L-010 | 恢复必须由 A、B 在同一 deadline 内分别 prepare，进入不可承载业务的 provisional 状态并完成端到端恢复确认；B activation ACK 后只能进入同样不承载业务的 `resume_activated_pending_liveness`，不得直接恢复租约 | P0 |
| RC-L-011 | 单边 prepare、另一端超时、跨角色 token 或旧 token 重放均不得恢复；超时后单边预留令牌与租约一起作废 | P0 |
| RC-L-012 | Relay 是 resume 的唯一权威；B 本地 30 秒 watchdog 不得因重试或 Relay 不可达而延长，到期必须先本地失败关闭 | P0 |
| RC-L-013 | `resume_provisional` 和 `resume_activated_pending_liveness` 均禁止业务帧且保持原 watchdog；B activation ACK、首个激活后 liveness、Relay 对该 liveness 的确认或双方 ready ACK 任一缺失时必须终止 | P0 |
| RC-L-014 | Relay 仅在首个 B post-activation liveness 已确认且 A/B 均确认同一 activation 后，才能原子轮换 token/密钥、进入 connected 并开放业务；B 收到最终 `controller_resumed` 后才取消 watchdog | P0 |
| RC-L-015 | 直接心跳和 Relay attestation 必须按 6.7 节保守扣除完整 RTT、已观察 age 与固定 1000 ms 安全裕量，任何有效证据对应的 deadline 均不得晚于该次 A liveness 后 30 秒 | P0 |
| RC-L-016 | `local_safety_deadline_a` 以 A liveness epoch/sequence 为更新单元：新序号可用新候选值推进，相同序号只能收紧，旧序号拒绝；不得对新序号与旧 deadline 取最小值导致存活周期无法前移 | P0 |

### 7.5 远程会话与任务

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-T-001 | A 可以创建新的远程会话并在连接有效期内切换自己已打开的远程会话 | P0 |
| RC-T-002 | 每个远程会话必须映射到 B 的一个权威 Session | P0 |
| RC-T-003 | A 的每次发送必须携带唯一 `task_id` 和稳定 `client_request_id` | P0 |
| RC-T-004 | 同一远程 Session 同时只允许一个已接纳任务，复用既有 turn lock | P0 |
| RC-T-005 | RPC 重试不得造成消息、工具调用或任务重复执行 | P0 |
| RC-T-006 | A 可取消进行中的任务，B 必须返回可验证的取消终态 | P0 |
| RC-T-007 | A 仅关闭远程窗口且后台 transport 存活时任务继续；实际 transport 断开时，B 保留已产生的权威 Session 提交，任务只可在“最后一条有效 A 存活证据后 30 秒”总 deadline 的剩余时间内继续，届满则取消并提交明确终态 | P0 |
| RC-T-008 | B 必须在接受模型凭证和附件前完成请求大小、协议版本和租约校验 | P0 |
| RC-T-009 | 计划任务和系统任务不自动暴露给 A，也不复用 A 的临时模型凭证 | P1 |
| RC-T-010 | A 可以选择 B 能力快照声明的普通工作模式或代码模式，未声明模式必须隐藏 | P0 |
| RC-T-011 | 代码任务只能绑定到 B 本地用户在开启远控前授权的工作区，不允许 A 通过路径参数扩大范围 | P0 |
| RC-T-012 | B 主动停止、刷新或立即断开远控时，所有远程来源的在途任务必须取消并产生明确终态 | P0 |
| RC-T-013 | 能力准备成功前，B 必须为 A 的远程会话引用创建或幂等复用唯一的 B 权威 Session，并把该精确映射投影给 B；A、B 任一端均可成为首个发言方 | P0 |
| RC-T-014 | B 消息窗口的发送入口必须构造本地受控任务，并复用 A 任务相同的准入、turn lock、执行器、取消、终态和权威 journal；不得退回普通本地聊天发送链路 | P0 |
| RC-T-015 | A、B 看到的用户消息、任务状态与回复必须来自同一 `task_id + B session_id` 权威链路；向 A 传输时仅将 B session ID 改写为对应 A session reference，不得按当前页面或最近会话猜测关联 | P0 |
| RC-T-016 | B 的本地任务 ID、请求 ID 和输入摘要必须由可信后端生成；任务输入文本只在接纳成功后作为有界 UI 投影显示，最终由权威 journal 去重替代 | P0 |
| RC-T-017 | 同一权威 Session 继续保持单任务 turn lock；任一端在已有活动任务时再次提交必须得到明确 `task_session_busy`，保留可重试输入，且不得静默丢弃、另开 Session、重排或重复执行 | P0 |
| RC-T-018 | `SessionStore`、`EnginePool`、`AcpPool` 与 `RemoteControl` 必须作为并列 Rust 能力被薄 Tauri Command/用例编排调用；不得在其上方新增持有会话或任务状态的统一 Runtime，UI 也不得按本地/远程复制第二套聊天业务流程 | P0 |
| RC-T-019 | 可信 Rust 后端必须从 `SessionStore` 的会话类型与后端绑定决定调用链路：本地普通会话调用 `SessionStore + EnginePool`，本地 ACP 会话调用 `SessionStore + AcpPool`，A 的 `RemoteProxy` 会话调用 `SessionStore + RemoteControl`；不得根据当前页面、最近连接、全局 active session 或前端声明猜测路由 | P0 |
| RC-T-020 | A 打开远程会话时，必须在本机 `SessionStore` 创建或幂等复用 `RemoteProxy` 记录，由 A 端可信 Rust 生成 opaque `remote_session_ref`，B 在认证请求后幂等映射到唯一权威 Session 并确认，A 再将本地 Session ID、`remote_session_ref`、connection/lease/generation 绑定。持久化的历史来源映射不是访问凭据；超出原 resume 边界或重新配对后，A 必须重新打开会话并生成新引用，不得直接复用磁盘上的旧引用 | P0 |
| RC-T-021 | A 的 `RemoteProxy` 可权威保存 A 本地标题、置顶、隐藏、最近访问等 UI 元数据，并保存已认证历史投影、有界 pending 关联和对账游标；但不得拥有或改写 B 的权威 Session、任务账本、Journal 序号和执行终态 | P0 |
| RC-T-022 | 远程连接控制面必须与会话投影分离。只有连接与加密业务通道均为 `Connected` 时，`RemoteProxy` 才允许提交、取消、审批、上传等变更操作；离线时只能查看 `SessionStore` 中已认证的只读投影，不得发送业务帧。关闭 ChatView 不得隐式断开租约 | P0 |
| RC-T-023 | Tauri Command 必须是薄 IPC 适配器；可共享的校验、准入、附件准备、任务提交与失败收口必须作为普通 Rust 用例函数供本地 Command 和 B 受控入口复用。`features/remote_control` 必须定义窄 `ControlledExecutionPort`，由 `lib.rs` 组合根注入无持久状态的 conversation 用例实现；禁止 `features/remote_control → features/conversation` 或 `features/remote_control → app/commands` 反向依赖，也不得内部调用 `#[tauri::command]`。任务来源、权限权威、协议/租约字段、`task_id`、输入摘要、临时模型包和加密材料均由可信 Rust 后端生成或派生 | P0 |
| RC-T-024 | 事件必须按类型使用带类型的公共 envelope：所有会话事件携带接收端本地 Session ID；A 的 `RemoteProxy` 会话事件额外携带可校验的 `remote_session_ref` 和 live connection/lease/generation；B 的受控会话事件携带 B 权威 Session ID 及适用的 lease/generation；纯连接事件只要求该类型的 connection/lease/generation，不伪造 Session 引用。task ID、journal epoch/sequence 和 capability revision 按类型定义必填字段与迟到过滤规则；必填绑定缺失、不匹配或迟到事件不得更新状态或当前 UI | P0 |
| RC-T-025 | 迁移期间旧远程 Command、Bridge API 和独立历史存储只能作为兼容代理，收敛到唯一薄用例链路和 `SessionStore` 中的 `RemoteProxy` 记录；同一操作不得同时进入新旧链路、产生双写、重复执行或两个状态所有者 | P0 |
| RC-T-026 | A 发送远程任务必须执行 `prepare → persist → dispatch`：可信 Rust 先生成稳定 `client_request_id`、`task_id`、输入摘要和覆盖所有非敏感语义参数的规范请求指纹，再将同一组标识与不含凭证的有界 pending/outbox 原子写入 `SessionStore.RemoteProxy`，持久化成功后才允许 `RemoteControl` 发送。只有 A 原进程存活、仍在原 resume 边界且恢复同一 live binding 时，才允许使用原 ID 和完全相同的规范请求向 B 幂等账本重试或通过 Journal/快照对账；同 ID 任一语义参数不同必须明确冲突。A 进程重启后只能从本地恢复 `outcome_unknown`，在 controller v1 未定义“新配对重新附着旧 B Session/账本”协议前，禁止自动发送、查询或对账旧请求。临时模型 Key 等不可持久负载在 dispatch 前丢失时，只有后端可证明从未尝试 dispatch 才能放弃原 pending，并由用户以新 ID 发起新请求；若是否发送不可确定，则必须保持 `outcome_unknown` | P0 |

任务幂等与终态证明使用独立、有界、原子持久化的 B 台账，不代替 `SessionStore` 会话正文。B 启动必须先验证台账并原子关闭全部重启遗留任务，之后才能公开远控运行时；已派发且无法确认结果时只报告 `result_unknown`，不得重派。坏文件或恢复写入失败必须禁用该远控域并明确诊断，不得以空内存台账继续执行，也不得阻止已发布的 Legacy Web 恢复。旧租约标识只用于终态审计，不恢复配对、有效租约、密钥或令牌。

#### 普通远程回合的执行边界

- 普通 chat 必须先获得 B 的真实回合预留，再冻结本次经认证的 `skill_ids`；空选择对应空技能目录。冷启动、模型重建、技能开关/安装/卸载刷新及目录自愈均不得扩大在途选择。本地发送在冻结至收尾期间明确拒绝，不借用远程技能集。
- 完成证明来自对应 Session 和回合 generation 的权威 `chat:done`；引擎不忙本身不证明成功，失败/取消不得投影为成功。监听须在提交前绑定，缺失终态只能报告有界错误。
- 正常成功、失败和取消收尾须先证明引擎已停止，完成任务临时模型引擎回收，再按精确技能 owner 恢复 B 最新启用集合。旧 worker、旧 owner 或重复收尾不得修改新回合。
- A 远程附件由绑定完整租约、Session、任务的 B 上传 receipt 消费，不能混同 B 本地附件描述符；取消映射校验遵循会话意图锁在连接 owner 锁之前的顺序。B 的远控只读视图和凭据门控只作用于当前受控 Session，不作用于其他本地会话。

当前实现边界：已物化回合的异步 future 非正常丢弃时，技能冻结保持失败关闭，不能自动恢复更广权限；该 Session 当前需重启进程恢复。捕获 panic、证明停止及精确 owner 有界收尾仍由 #1400 跟进，此段不宣称异常恢复或真实双 Windows 发布验收已完成。

### 7.6 模型选择与临时凭证

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-M-001 | A 的远程消息窗口必须提供“被控端模型”和“我的模型”两种来源 | P0 |
| RC-M-002 | 选择 B 模型时，B 使用当前 Session 可用的本机模型配置 | P0 |
| RC-M-003 | 选择 A 模型时，只展示允许导出的 API-Key 模型 | P0 |
| RC-M-004 | 不允许传输 OAuth、Cookie、网页登录态、订阅授权和设备绑定凭证 | P0 |
| RC-M-005 | A 临时模型包必须严格使用 7.6.1 节的 `temporary_model_package/v1` allowlist、字段类型和 `SavedModel` 映射；不得透传整个 `SavedModel` JSON | P0 |
| RC-M-006 | 模型包必须绑定当前 `connection_id + controller_lease_id + lease_epoch + task_id`，不得被其他连接、租约世代或任务复用 | P0 |
| RC-M-007 | Relay、B 配置文件、B 模型列表、Session 正文和日志均不得出现 API Key | P0 |
| RC-M-008 | B 必须在内存中使用临时模型配置，并在任务终态后主动释放，连接终止时兜底清理 | P0 |
| RC-M-009 | A 模型调用失败时不得自动回退到 B 模型，必须向两端返回明确错误 | P0 |
| RC-M-010 | A 本机 loopback、Unix socket 或 B 不可访问的本地模型不得作为可传输模型 | P0 |
| RC-M-011 | B 必须在临时模型执行准备阶段复用模型 endpoint 安全校验；显式本机/内网地址在模型包安装前拒绝，预解析结果包含非公网地址、无结果或超时时，已接纳任务必须在首次模型调用前以明确 `failed` 终态结束；最终请求目标漂移按 10.5 节作为已知风险保留 | P0 |
| RC-M-012 | 错误信息和诊断必须经过凭证脱敏后才能同步给 A 或写日志 | P0 |
| RC-M-013 | v1 模型包出现未知字段、未知枚举、缺少必填字段、无效字段组合或无法解析的视觉兜底引用时，B 必须拒绝整个任务，不得忽略或猜测 | P0 |
| RC-M-014 | `credential_ref`、OAuth、Cookie、订阅登录、设备绑定令牌和网页登录态不得进入模型包；只能发送本次任务使用的 API Key secret | P0 |
| RC-M-015 | `controller_temporary` 首版只允许普通对话模式；代码模式仍由 ACP 执行器选择模型，在临时模型尚未接入 ACP 前，A、Relay 和 B 都必须拒绝 `mode=code + controller_temporary`，不得静默改用 B 模型 | P0 |

#### 7.6.1 `temporary_model_package/v1` 精确模型

模型包顶层只允许以下字段；`?` 表示可选，除此之外的字段一律拒绝：

```text
{
  schema: "temporary_model_package/v1",              // 必填常量
  package_id: string,                                  // 必填随机 ID，限本次任务
  binding: {                                           // 必填
    connection_id: string,
    controller_lease_id: string,
    lease_epoch: canonical decimal string,             // 十进制、无前导零
    task_id: string
  },
  primary: RouteV1,                                    // 必填
  primary_credential: { kind: "api_key", secret: SensitiveString },
  vision_fallback?: { route: RouteV1, credential: { kind: "api_key", secret: SensitiveString } }
}

RouteV1 = {
  display_name: string,                                // 必填，仅展示，不参与路由
  preset: "local_vllm" | "deepseek" | "kimi" | "openai_compatible" |
          "qwen" | "doubao" | "minimax" | "glm" | "mimo" | "openai" |
          "anthropic" | "gemini" | "xai",
  wire_api: "openai_chat_completions" | "anthropic_messages", // 必填，且须与 preset 匹配
  base_url: string,
  model: string,
  provider_kind?: "custom" | "official_api",         // `coding_plan` 禁止导出
  vendor?: "anthropic" | "deepseek" | "doubao" | "gemini" | "glm" | "kimi" |
           "mimo" | "minimax" | "openai" | "qwen" | "tencent" | "xai",
  endpoint_mode?: "full_chat_completions",
  context_window_tokens?: canonical decimal string,    // uint32 范围、无前导零
  max_output_tokens?: canonical decimal string,        // uint32 范围、无前导零
  reasoning_effort?: "off" | "low" | "medium" | "high" | "max" | "auto",
  image_capability_override: "pinvou" | "enabled" | "disabled"
}
```

`SensitiveString` 是非空 UTF-8 API Key，只能在已认证加密帧的敏感序列化路径出现，默认 `Debug`、普通日志和错误序列化必须脱敏。`RouteV1` 的字符串长度、URL 长度、数值上限和模型包总字节数必须由协议常量限定；`vision_fallback` 只允许一层，不得再次引用视觉模型。即使 `preset` 枚举中存在 `local_vllm`，RC-M-010/011 的可传输判定仍会拒绝 loopback、本机 socket、私网或 B 无法安全访问的 endpoint。所有枚举都按这里列出的 v1 值闭集校验，新增 vendor、endpoint mode 或 reasoning 档位必须提升包版本或完成显式能力协商，不能按任意字符串透传。

规范数字一律按上面的 canonical decimal string 编码；发送 JSON number 或接收时猜测两种编码均不属于 v1。A 只有在双方能力快照都包含 `temporary_model_package_v1` 时才可展示并发送“我的模型”；不具备该能力的旧客户端必须失败关闭或隐藏入口。该能力首次随桌面端 0.10.1 的同批 A/B 升级交付，不能把仅包含旧数字编码实现的中间构建与新版混用。

`SavedModel` 到 v1 包的映射固定如下：

| `SavedModel` 字段 | v1 映射/策略 |
|---|---|
| `id` | 不传输；仅由 A 在构包前定位模型 |
| `name`、`alias` | 规范化为一个受长度限制的 `display_name`；不作为路由或认证依据 |
| `preset` | 原值映射到 `RouteV1.preset`；无法映射则禁止导出 |
| `model`、`base_url`、`provider_kind`、`vendor`、`endpoint_mode` | 映射到同名路由字段；`wire_api` 由 A 的既有路由解析器导出，B 必须独立复算并校验一致 |
| `context_window_tokens`、`max_output_tokens`、`reasoning_effort` | 映射到同名可选字段；B 同时执行既有进程上限和枚举校验，不接受扩大本机安全上限的值 |
| `image_capability_override` | 映射到同名字段；未知值拒绝，不沿用持久配置读取时的兼容兜底 |
| `vision_model_id` | 不传 ID；A 必须解析到另一条可导出的 `SavedModel`，将其路由和单独 API Key 展开为一层 `vision_fallback`；引用不存在、循环、不可导出或无凭证均拒绝 |
| `api_key` | 只把本次任务需要的值放入对应 `credential.secret` 敏感缓冲，不出现在普通序列化、日志或错误中 |
| `credential_ref` | 引用本身不传；A 仅可在本机安全存储解析它，并把解析出的 API Key 放入 `credential.secret` |
| `credential_state`、`has_secret` | 只用于 A 本地判断是否可导出，不传输 |
| `credential_action` | 编辑态字段，禁止传输；非空时不得构包 |

`wire_api` 是 `temporary_model_package/v1` 导出与校验使用的规范化判别符，不是直接传给 CodeWhale 的内部端点模式。B 必须先根据 `preset` 独立推导期望的 `wire_api` 并校验；通过后再重建 `SavedModel`，并由既有路由解析接口独立选择底座协议；不得将包内枚举字面量直接透传给底座。底座的 `chat`、`chat_completions`、`chat-completions` 等内部别名不扩展本包的合法枚举。

`builtin_llmapi`、`provider_kind=coding_plan` 以及任何依赖 OAuth、Cookie、订阅账号、设备绑定或企业登录态的模型必须从“我的模型”隐藏；不得把这些凭证包装成 `api_key` 绕过限制。使用 B 模型时不发送该包。主模型原生支持图片时不发送 `vision_fallback`；需要视觉兜底时，主模型与兜底模型各自携带独立的 API Key，均遵守同一任务绑定和清理周期。

### 7.7 工具、Skill 与权限

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-C-001 | 任务执行使用 B 已安装并启用的工具和 Skill | P0 |
| RC-C-002 | A 可以查看 B 返回的受控能力清单，但不得直接获取 Skill 私有文件或工具凭证 | P0 |
| RC-C-003 | 首版不自动把 A 的 Skill 源码或工具安装包发送到 B | P0 |
| RC-C-004 | B 的工具、文件和命令权限策略继续生效，远控不得创建绕过入口 | P0 |
| RC-C-005 | 需要审批的操作必须在执行前发送给 A，未经有效批准不得执行 | P0 |
| RC-C-006 | 审批决定只对当前请求生效，不提供“永久允许”或远程超级权限 | P0 |
| RC-C-007 | 审批请求必须包含受控操作类型、风险等级、目标范围和安全预览 | P0 |
| RC-C-008 | 审批内容中出现凭证、Token、Cookie 或敏感环境变量时必须脱敏 | P0 |
| RC-C-009 | 审批超时、A 断线、租约改变、任务取消或 B 停止远控时默认拒绝 | P0 |
| RC-C-010 | A 是远程任务唯一审批权威；B 用户不能 Allow/Approve，但可以随时查看请求、停止任务、断开连接和终止后续远程操作 | P0 |
| RC-C-011 | A 的批准只能在 B 预授权工作区和既有工具策略内生效，不能批准越界路径或远程超级权限 | P0 |
| RC-C-012 | 审批请求必须绑定 `connection_id + controller_lease_id + lease_epoch + task_id + permission_request_id + expires_at`；任一绑定不匹配即拒绝 | P0 |
| RC-C-013 | 首个有效 A 决定以原子 compare-and-set 成为唯一终态；同值重试幂等，迟到、重复冲突或终态后的决定返回 `permission_already_decided`/`permission_expired` 且不得执行操作 | P0 |
| RC-C-014 | B 本地“停止任务/立即断开”与 A Allow 竞态时，以 B 是否已原子提交操作 dispatch 为边界：dispatch 前停止已提交则拒绝操作；dispatch 已提交则不得伪报未执行，并返回已执行或结果未知的真实状态 | P0 |

### 7.8 附件与成果

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-F-001 | A 支持上传文本、图片及既有附件管线允许的文件类型 | P0 |
| RC-F-002 | 单文件、单块、连接总内存和超时限制不得宽于既有远控附件上限 | P0 |
| RC-F-003 | 未完成、取消、超时和断线遗留的附件必须在有界时间内清理 | P0 |
| RC-F-004 | 附件必须落入 B 当前 Session 的隔离工作区，不能写入任意路径 | P0 |
| RC-F-005 | A 可以查看 B 生成的成果列表、预览支持的成果并下载副本 | P0 |
| RC-F-006 | 成果访问必须绑定当前连接、B Session 和成果 ID，禁止仅凭路径读取 | P0 |
| RC-F-007 | A 的本地历史只保存成果元数据和已下载副本位置，不保存 B 绝对路径 | P1 |
| RC-F-008 | 大文件或不支持预览的成果必须明确提示下载，不得截断后伪装完整预览 | P1 |

### 7.9 双端同步与独立历史

| 编号 | 需求 | 优先级 |
|---|---|---|
| RC-S-001 | B 的权威 Session 更新是远程消息和任务状态的唯一事实源 | P0 |
| RC-S-002 | 两端同步用户消息、流式文本、思考状态、工具状态、审批、错误、取消和成果状态 | P0 |
| RC-S-003 | 事件必须携带 epoch 和单调序号，支持断线后精确重放 | P0 |
| RC-S-004 | 事件重复到达不得在 A 历史中生成重复消息或重复终态 | P0 |
| RC-S-005 | journal 无法覆盖缺口时，A 必须丢弃不完整增量并重新获取权威快照 | P0 |
| RC-S-006 | A 只在事件或快照经过认证、解密和完整性校验后写入独立历史 | P0 |
| RC-S-007 | A 历史不得保存 API Key、访问 Token、会话密钥和未脱敏工具凭证 | P0 |
| RC-S-008 | A 删除独立历史不得删除 B Session；B 删除 Session 不得删除 A 已保存的历史 | P1 |
| RC-S-009 | A 离线查看历史时必须明确标记为副本，不得显示为仍连接 B | P1 |
| RC-S-010 | A 的远程历史必须作为 `SessionStore` 中 `RemoteProxy` 会话的已认证投影持久化；不得长期保留与 `SessionStore` 并列且同时向 UI 供数的第二份远程历史权威 | P0 |
| RC-S-011 | A 的 `RemoteProxy` 可保存符合 RC-T-026 的有界 `pending/outcome_unknown` 投影，但只能在 B 返回经认证的接纳结果、Journal 事件或快照后对账为权威内容；超时、断线或恢复后无法确认时必须显示未确认/结果未知，不得自行生成远程成功终态 | P0 |

## 8. 状态模型

### 8.1 被控端连接状态

```text
idle
  → registering
  → waiting_for_pairing
  → pairing_reserved
  → handshake_pending
  → connected
  → reconnecting
  → resume_provisional
  → resume_activated_pending_liveness
  → connected

等待、预留、握手、连接或重连中刷新
  → retiring_generation
  → registering（新 generation）

任意活动状态
  → stopping
  → stopped

不可恢复的注册或协议错误
  → error

预留、握手失败或超时
  → registering
  → waiting_for_pairing（自动向 Relay 申请新码）
```

状态约束：

- `waiting_for_pairing` 必须存在未过期连接码。
- `pairing_reserved` 由 Relay 配对记录唯一持有，必须绑定一个 reservation owner，最长 10 秒；码已被独占但尚未最终消费，任务能力不可用。
- `handshake_pending` 由同一 Relay 配对记录和 reservation owner 持有，最长 20 秒；只允许 A、B 对同一签名 transcript 完成加密 Finished 验证与双 receipt ACK，任务能力仍不可用。
- `connected` 必须同时存在有效租约、端到端会话密钥和已协商能力。
- `connected` 中 B 必须优先用 `encrypted_controller_liveness_a` 建立 A 存活证据；每个证据以 `liveness_epoch_a + heartbeat_sequence_a` 为身份并产生自己的保守候选 deadline。只有序号更新的有效证据可开启后继预算，重复证据不能延长。
- `reconnecting`、`resume_provisional` 与 `resume_activated_pending_liveness` 共享最后有效 A 存活证据后的 30 秒总 deadline；两个恢复中间态都不得承载业务或推进 watchdog，且不恢复已失效、已刷新、Relay 重启后或跨进程的重连身份。
- `stopped` 和 `error` 终态必须清理临时模型凭证；停止还必须撤销租约。
- 预留/握手失败或超时不得退回原 `pairing_id` 的 `waiting_for_pairing`；Relay 永久终结该 pairing 并隔离码字符串，B 在远控仍开启且在线时以新的 `pairing_id` 自动申请新码。
- `retiring_generation` 必须先使旧 generation 的本地回包门失效，再等待 Relay 撤销确认并完成任务/凭证清理；此前不得创建、显示或接纳新 generation。`stopping` 不创建后继 generation。
- B activation ACK 只允许把 `resume_provisional` 推进为 `resume_activated_pending_liveness`；只有首个 B post-activation liveness 经 Relay 确认、A/B 分别提交 ready ACK，且 B 收到最终 `controller_resumed` 后才能进入 `connected` 并取消 watchdog。任一步骤或回包丢失一律在原 deadline 进入终止清理。

`remote_control_generation` 的 wire 值为不透明字符串，由 B 进程启动时的 128-bit CSPRNG `boot_nonce` 与本进程严格递增的 64-bit counter 组成；不持久化，也不允许用户输入。创建、刷新、自动换码和所有回包都必须携带完整值并做精确相等比较。自动换码沿用当前 generation；只有开启或完成旧 generation 撤销后的刷新才递增 counter。B stop/retire 在本地先关闭该 generation 的接收门，因此迟到响应即使密码学有效也不能改变 UI 或任务状态。

### 8.2 任务状态

```text
created → submitting → admitted → running
                         ├─ waiting_permission → running
                         ├─ succeeded
                         ├─ failed
                         ├─ cancelled
                         └─ interrupted
```

- 只有 B 返回权威 `admitted` 后，A 才能将任务显示为正在执行。
- A 的“发送中”不等同于 B 已接纳。
- 任务只能产生一个终态；重复终态按 `task_id` 和事件序号去重。
- 临时模型凭证必须在所有终态路径和异常中清理。

### 8.3 配对记录所有权与原子转换

Relay 是配对记录、reservation 和交互租约状态的唯一权威写入者；A、B 只提交带前置条件的请求并投影 Relay 状态，不能各自在本地宣布预留或建租约成功。

| 转换 | 唯一写入者 | 原子前置条件 | 成功效果 | 失败/超时效果 |
|---|---|---|---|---|
| `registering → waiting_for_pairing` | Relay | B endpoint 已正式 enrolled、认证且在线，generation/稳定注册请求 ID 匹配，且无未撤销配对记录 | 最多 32 次 CSPRNG 采样；用当前及所有被引用旧 digest key 原子查重并占用，创建内存记录，向 B 返回密封展示码与 5 分钟有效期 | 不展示码；碰撞耗尽或 keyring 不完整时失败关闭，未 enrolled 返回受控认证错误 |
| `waiting_for_pairing → pairing_reserved` | Relay | 码有效、B 在线、无 Web/controller lease 或其他 reservation，claim ID 幂等参数一致 | CAS 写入唯一 reservation owner，启动 10 秒计时 | claim 统一失败；原记录不被非 owner 修改 |
| `pairing_reserved → handshake_pending` | Relay | owner、`remote_control_generation` 和签名 transcript 均匹配 | 冻结 transcript 哈希，启动 20 秒计时并转发双方加密 Finished | 废弃原码和预留；通知 B 以同 generation、新注册请求 ID 自动申请新码 |
| `handshake_pending → connected` | Relay | 两个 sender commitment 已先提交，A/B 各自验证对端 Finished，并从相反角色 socket 恰好一次回传通过 9.3 节校验的 receipt secret；当前 reservation 是交互槽位的唯一占有者，owner 与本次握手一致，且不存在其他 Web/controller lease 或竞争 reservation | 在一次事务/CAS 中终结 pairing、将当前 reservation 原子提升为 controller lease 并提升 lease epoch，不先释放槽位 | 单方/损坏/重放/超时 receipt 均不得提交；不得出现“pairing 已消费但无租约”或“有租约但 pairing 可复用” |
| 任意活动态 → `retiring_generation` | B 为发起权威，Relay 执行撤销 | B 先在本地关闭旧 generation 回包门 | 撤销旧配对、reservation、租约和令牌，取消任务并清理密钥/凭证；确认后才创建新 generation | 撤销无法确认则失败关闭，不申请或展示新码 |
| 任意活动态 → `stopping/stopped` | B 为发起权威，Relay 执行撤销 | B 先使当前 generation 本地失效 | 撤销配对、reservation、租约、令牌和密钥，取消远程任务 | 本地清理不依赖 A/Relay；迟到回包丢弃，绝不自动生成新 generation |

同一 `registration_request_id` 或 `claim_request_id` 且参数完全一致的重试只返回既有结果；ID 相同而 generation、身份、公钥或参数不同必须拒绝。B 断线、A owner socket 断线、`remote_control_generation` 改变、码到期或 transcript/commitment/Finished/receipt 验证失败都走失败清理，不允许转移 reservation owner。自动换码只能在当前 generation 仍 active 时使用新的稳定 `registration_request_id`；stop/retire 后的迟到成功响应必须丢弃。

### 8.4 Web lease 与桌面 controller lease 交叉矩阵

Relay 必须以 B endpoint 的 `interactive_slot_revision` 串行化所有 Web join、桌面 claim、握手提交、断开和接管动作：

| 当前槽位/状态 | 新动作 | 结果 | 连接码是否消费 |
|---|---|---|---|
| 空闲、无桌面 reservation | A `controller_pairing_claim` | A 原子取得 `pairing_reserved` | 否；仅成功握手提交时消费 |
| 已有 Web lease | A `controller_pairing_claim` | 返回 `controller_busy`，不得影响 Web lease | 否，码保持可用至原过期时间 |
| 已有 controller lease、reconnecting、`resume_provisional` 或 `resume_activated_pending_liveness` | A claim | 返回 `controller_busy`，不得顶替 | 否 |
| 已有桌面 `pairing_reserved`/`handshake_pending` | `web_client_join` | 返回 `desktop_controller_pending` | 不适用 |
| 已有 controller lease、reconnecting、`resume_provisional` 或 `resume_activated_pending_liveness` | `web_client_join` | 新 Web 返回 `desktop_controller_active`；旧 Web 按 9.4 节兼容 envelope | 不适用 |
| 仅有 Web lease，无桌面 reservation/controller lease | 第二个 `web_client_join` | 保留现有 WebUI v2 的 Web-to-Web 接管语义，新 Web 客户端按既有协议替换旧 Web lease | 不适用 |
| 空闲时 A claim 与 Web join 并发 | 两者竞争同一 revision CAS | 只有一个成功；Web 先提交则 A busy 且码不消费，A 预留先提交则 Web pending | 仅 A 后续握手成功时消费 |
| 同一码两个 A 并发 claim | 两者竞争配对记录与槽位 CAS | 仅一个成为 reservation owner；失败方收到统一 `pairing_code_invalid` | 否，直至 owner 握手提交 |

桌面 reservation 不是控制租约，但为了避免 Web 在握手中途夺取槽位，必须作为短期互斥占位。它超时/失败后槽位释放、原 pairing 终结且码字符串进入隔离。Web-to-Web 接管只在不存在桌面 reservation/controller lease 时保留；桌面与 Web 之间一律禁止接管。

### 8.5 配对实例终结与码字符串隔离

- 安全不变量是原 `pairing_id` 一旦消费、过期、撤销或失败便永久终结，不能恢复、重放或挂接到新租约；有限的 7 位字符串不承诺永不再次出现。
- Relay 每次签发码时必须在返回 B 前持久写入不含 endpoint 映射的隔离记录：`{ keyed_code_digest, digest_key_id, issued_at, quarantine_until }`。`PAIRING_CODE_QUARANTINE_MS` 固定为自签发起 10 分钟，并且终态发生后至少再保留 2 分钟，实际 `quarantine_until = max(issued_at + 10min, terminal_at + 2min)`。该值覆盖 5 分钟有效期、最多 30 秒时钟/传输偏差和 2 分钟幂等查询窗口。
- Relay 采用多 key 查重而不是后台改写 tombstone：维护只读 `digest_keyring`，包含当前写入 key 和所有仍被活动 pairing 或未到期 tombstone 的 `digest_key_id`。对每个候选字符串，Relay 必须在一次分配事务使用同一 keyring revision，以每把 key 计算摘要并查询对应 key ID 的活动记录和 tombstone；任一命中即碰撞，只有全部未命中才用当前 key 原子占用并写 tombstone。不得只用当前 key 查询旧摘要。
- key 轮换先添加新 current key，不删除旧 key；只有该旧 key 引用的活动 pairing 全部终结且最后一个 tombstone 到期并清理后才可退役。轮换与发码并发时通过 keyring revision/CAS 重试保证没有窗口；若任何未过期记录引用的 key 缺失，发码失败关闭并暂停至该记录最晚 `quarantine_until`，同时至少执行 10 分钟 cold-start quarantine。
- 隔离期结束并安全清理 tombstone 后，相同 7 位字符串可以由 CSPRNG 偶然分配给完全不同的新 `pairing_id`。任何客户端都必须以当前 pairing/transcript 为准，不能以字符串相同推断是旧设备。
- Relay 重启会终结全部内存活动 pairing，但必须从持久状态恢复未到期隔离 tombstone 和其完整 digest keyring 后才能发新码。状态文件丢失、损坏或所需 digest key 不可用时必须告警并按上一条失败关闭，不能立即以空集合或仅当前 key 继续。
- A、B UI 只能称连接码为 5 分钟短期凭证，提示用户不要长期保存或在失败后盲目重试旧字符串；需要重新连接时应查看 B 当前展示码。不得使用“该 7 位字符串永久作废”的文案。

## 9. 配对与传输协议要求

### 9.1 协议兼容原则

- 现有 WebUI v2 协议必须继续可用，旧客户端不得因 Relay 上线配对功能而失效。
- 桌面双端控制必须使用显式协议版本和客户端角色，不得伪装成浏览器绕过能力策略。
- 未知协议版本、消息类型、角色或字段默认拒绝，不做静默兼容或字段猜测。
- 配对控制消息与业务密文消息必须有独立白名单和大小限制。

### 9.2 建议消息族

以下名称用于固定需求语义，详细字段由技术设计确定：

| 方向 | 消息 | 用途 |
|---|---|---|
| B → Relay | `desktop_endpoint_claim` | 在原认证 socket 上将既有 endpoint 认领/迁入 enrollment registry，保持原 ID/token |
| B → Relay | `desktop_pairing_register` | 携带 enrollment 身份、generation、稳定注册请求 ID 及独立 code-wrap/握手公钥，申请 Relay 生成短期码 |
| Relay → B | `desktop_pairing_registered` | 返回密封给 B 的展示码、过期时间、配对记录 ID 和 generation |
| B → Relay | `desktop_pairing_revoke` | 刷新或停止时按 generation 撤销配对、reservation 及关联租约 |
| A → Relay | `controller_pairing_claim` | 使用 7 位码、claim ID 和 A 临时握手材料请求原子预留 |
| Relay → A | `error/pairing_code_invalid` | 对不存在、过期、已使用、已撤销、失效 generation 或已被其他 claim 占用的码返回同一错误 |
| Relay → A/B | `controller_pairing_reserved` | 返回 reservation、Relay 签名 transcript、过期时间和对端握手材料 |
| A/B → Relay | `finished_receipt_commit` | 发送 Finished 前，从 sender 角色 socket 提交按 9.3 节唯一算法计算的随机 receipt secret commitment；不含 secret 明文 |
| A ↔ B | `encrypted_handshake_finished` | Relay 只转发密文；发送方用角色独立 Finished key 认证 transcript 并携带自己的随机 receipt secret |
| A/B → Relay | `finished_receipt_ack` | 接收方验证对端 Finished 后，恰好一次从 receiver 角色 socket 回传收到的 sender secret |
| Relay → A/B | `controller_joined` | 返回租约、在线状态、恢复边界、初始 `liveness_epoch_a/b` 及只交付给本角色的独立 resume token |
| A/B → Relay | `controller_liveness` | transport 存活消息，携带角色专属单调序号；Relay 分别更新 `last_liveness_a`/`last_liveness_b`，不能替代 A→B E2E 心跳 |
| B → Relay → A | `controller_liveness_challenge_a` | Relay 盲转发 B 的一次性 128-bit challenge，供 B 测量端到端 RTT |
| A → Relay → B | `encrypted_controller_liveness_a` | Relay 盲转发；A→B 方向 AEAD 绑定连接、租约、epoch、role=A、A liveness epoch/sequence 与 challenge |
| B → Relay | `controller_liveness_attestation_request_a` | 直接 E2E 响应暂不可用时，携带新的一次性 challenge 请求兜底证明 |
| Relay → B | `controller_liveness_attestation_a` | Relay 签名并绑定 challenge、A/attestation 新序号、`observed_age_ms`、`remaining_ttl_ms`；B 按 6.7/9.3 节保守扣除 RTT 和裕量 |
| A/B → Relay | `controller_resume_prepare` | 在 30 秒内由各角色分别提交独立 resume token、请求 ID 和新 transport challenge |
| Relay → A/B | `controller_resume_provisional` | 双方 prepare 后进入不可承载业务的暂定恢复并返回 `activation_id` |
| A ↔ B | `encrypted_resume_confirm` | 双方确认新 transport 和原 E2E 恢复密钥，Relay 只转发密文 |
| B → Relay | `controller_resume_activation_ack` | B 在本地 deadline 前验证恢复确认后提交 activation ACK；该消息只进入 pending-liveness，不代表最终恢复 |
| Relay → A/B | `controller_resume_activation_pending` | activation ACK 后进入不承载业务的 `resume_activated_pending_liveness`，返回同一 activation 绑定 |
| B → Relay | `controller_post_activation_liveness` | B 发送绑定 activation 的首个新序号认证 liveness |
| Relay → A/B | `controller_post_activation_liveness_confirmed` | Relay 确认首个 B liveness，等待双方 ready ACK |
| A/B → Relay | `controller_resume_ready_ack` | A、B 各自确认同一 liveness/activation，不能互相代交 |
| Relay → A/B | `controller_resumed` | 双方 ready 后返回同一最终恢复结果、各角色新 token 密文和新的 `liveness_epoch_a/b` |
| A ↔ B | `encrypted_frame` | 承载加密后的 RPC、事件、附件块和审批 |
| A/B → Relay | `controller_disconnect` | 主动结束控制租约 |

### 9.3 幂等与原子性

- 配对注册、兑换、连接、任务发送、取消和审批响应必须具有稳定请求 ID。
- 同一请求 ID 与相同参数重试应返回相同结果；相同 ID 与不同参数必须拒绝。
- claim 只原子取得短期 reservation；只有双方 receipt 验证成功后，终结当前 pairing 和创建 controller lease 才在 Relay 内形成不可分割的状态转换。
- 握手 KDF 只按角色和用途域分离地产生 `session_key`、`finished_key_a/b` 与 `resume_secret`。每个发送方另用 CSPRNG 独立生成恰好 32 字节的 `receipt_secret_sender`；它不是 KDF 输出，另一方在收到 Finished 前不能从共享秘密预测或自行构造发送方 receipt。
- commitment 只有一个规范：`commit = SHA-256(domain || LP(relay_epoch) || LP(pairing_id) || LP(reservation_id) || LP(transcript_hash) || LP(sender_role) || LP(receipt_secret_sender))`。`domain` 是不带 NUL、未经长度前缀的 ASCII 字节串 `pinvou-finished-receipt-commit-v1`；`LP(x)` 是 4 字节无符号大端长度后紧跟内容。前四个标识字段和 `sender_role` 先规范为 UTF-8，其中 `transcript_hash` 必须是 64 字节小写十六进制 ASCII，`sender_role` 只能是单字节 `A` 或 `B`；secret 按原始 32 字节编码。输出是 32 字节 SHA-256，并在 JSON wire 中编码为 64 字符小写十六进制。禁止对 secret 先单独哈希、使用分隔字符串、调整字段顺序或省略长度前缀。
- 固定测试向量：`relay_epoch="epoch-test-1"`、`pairing_id="pairing-0001"`、`reservation_id="reservation-0001"`、`transcript_hash` 为 64 个字符 `0`、`sender_role="A"`、secret 十六进制为 `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`。规范 preimage 共 194 字节，其十六进制为：

```text
70696e766f752d66696e69736865642d726563656970742d636f6d6d69742d76310000000c65706f63682d746573742d310000000c70616972696e672d30303031000000107265736572766174696f6e2d303030310000004030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030000000014100000020000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
```

对应 commitment 必须为 `ed19ab422bae98c4f52b2e18665df08b9109eb916c85812678cfde57448f3a53`。实现应由共享规范脚本生成该向量，并把上述固定值作为跨语言兼容断言。
- 消息顺序固定为：sender 从角色认证 socket 提交 commitment；Relay 确认；sender 才通过对应 Finished key 保护的 `encrypted_handshake_finished` 把原始 secret 发送给对端；receiver 成功认证 Finished 后，恰好一次从相反角色认证 socket 用 `finished_receipt_ack` 把原始 secret 回传 Relay。Relay 按唯一算法复算 commitment，并要求 `receiver_role != sender_role`；一个 secret 对同一 reservation/sender 只能消费一次。提前 ACK、未确认 commitment 就发送 Finished、sender 自收、角色互换、重复 ACK或顺序跳跃均拒绝。
- Relay 只有在同一 `relay_epoch + pairing_id + reservation_id + transcript_hash + remote_control_generation` 下，分别收到 B 对 A Finished 和 A 对 B Finished 的有效 receipt 后，才能原子终结 pairing 并创建租约。旧 reservation、错 transcript、错 socket、损坏或 20 秒握手超时 receipt 均拒绝并进入 pairing 失败清理。
- receipt secret 在 Relay 完成校验后立即清理；它是随机收件证明，不与 `session_key`、Finished key 或 `resume_secret` 存在派生关系。Relay 知道 receiver 回传的 receipt secret 只能验证既有 commitment，不能由此导出或解密业务会话密钥，也不能把自己伪造的 ACK 当作来自另一个角色认证 socket。
- A→B 存活主路径必须使用当前会话的 A→B 方向 AEAD 保护 `encrypted_controller_liveness_a`。明文路由 envelope 只允许暴露有界 ciphertext 长度、连接路由 ID 和消息类型；所有安全绑定、A 的 liveness epoch/sequence 与 B challenge 都在 AEAD authenticated data/密文内，Relay 不持有验证密钥。篡改、跨租约转发、旧 epoch/sequence、未知 challenge 或 RTT 超限均不得产生候选 deadline。
- 直接路径与 attestation 兜底必须使用 6.7 节唯一的保守公式和 `SAFETY_MARGIN_MS=1000`。Relay 的 `observed_at_relay`/`issued_at_relay` 只在 Relay 单调时钟域相减；B 的 challenge RTT、`received_at_b` 和 `candidate_deadline` 只在 B 单调时钟域计算，绝不直接比较两台机器的单调值。Relay 签名 `remaining_ttl_ms` 是从 A 心跳观察点剩余预算的上界，B 必须再取 `min` 并扣除完整本地 RTT 与安全裕量。
- Deadline 更新键固定为 `(liveness_epoch_a, heartbeat_sequence_a)`。只有当前 `controller_joined/controller_resumed` 认证安装的 epoch 可用；其新 sequence 按自己的 candidate 替换旧 deadline，同 sequence 的多路径/重复响应只能取更早值，未知 epoch 或旧 sequence 拒绝。该规则既允许真正后续 A 心跳推进存活周期，又确保任何一次证据从对应 A liveness 起都不超过 30 秒；B transport 心跳及 Relay 普通控制消息不属于该键。
- `pairing_code_invalid` 的 envelope、关闭行为、响应大小和可观察时间桶必须对码不存在、过期、已使用、已撤销、失效 generation 及并发 claim 失败保持一致；仅 Relay 内部聚合指标可使用不含标识的受控原因，不能回传 A。
- resume 的两枚 token 仅在 prepare 阶段独立预留；双方 prepare 齐全后只能进入 `resume_provisional`，其消息白名单不含任何业务类型。B 验证 `encrypted_resume_confirm` 后提交绑定 `activation_id + lease + generation + original_deadline` 的 activation ACK；Relay 收到前不得预留新 token/密钥、通知 A 已恢复或开放业务。
- Relay 验证 B activation ACK 后只进入 `resume_activated_pending_liveness`：原子预留角色独立的新 token/传输密钥候选并返回 pending 结果，但不最终消费旧 token、不恢复租约、不取消 B watchdog、不开放或缓存业务。B 必须发送首个绑定 activation 的新序号 liveness；Relay 确认后通知双方，A/B 再各自从角色 socket 提交 ready ACK。
- Relay 仅在原 deadline 前确认首个 B liveness 并收到双方 ready ACK 后执行最终原子激活：消费旧 token、启用新 token/密钥、提交租约 connected、开放业务门并返回同一 `controller_resumed`。B 只有验证该结果才取消 watchdog；任一消息/ACK/结果丢失、单边确认或 deadline 到期均终止 candidate 和租约、作废所有新旧 token。结果未知只能用原稳定 resume request ID 查询，不能开始另一恢复。
- Relay 或客户端在结果未知时不得自动重复执行有副作用的业务命令。
- 附件提交、成果下载和审批决定必须能够区分“未执行、已执行、结果未知”。

### 9.4 `PROTOCOL.md` 变更与兼容约束

实现本需求时必须同步修改 `remote-control-relay/PROTOCOL.md`，明确：

- 增加显式 `client_role=desktop_controller`、协议能力位、正式 desktop endpoint enrollment 门禁、既有 endpoint 的 `desktop_endpoint_claim`/恢复错误、9.2 节桌面消息族、签名 transcript 规范化与验证规则；
- 增加 `relay_epoch`、`remote_control_generation`、独立 `code_wrap_public_key`、码摘要隔离 tombstone 与多 digest key 查重、`pairing_reserved`、`handshake_pending`、reservation owner、10/20 秒超时、9.3 节唯一编码的随机 receipt commitment→Finished→receipt ACK 完成条件，以及最终终结 pairing/建租约原子提交；
- 增加 5 秒角色专属 transport liveness、`last_liveness_a/b`、B challenge、Relay 盲转发的 `encrypted_controller_liveness_a` 主路径、Relay 签名 attestation 兜底、跨时钟保守 deadline 公式，以及双角色 token、禁止业务的 `resume_provisional`/`resume_activated_pending_liveness`、B activation ACK、首个 post-activation liveness、双方 ready ACK 和最终恢复门；
- 定义 endpoint 共享交互槽位、`interactive_slot_revision` 仲裁、`controller_busy`、`desktop_controller_pending`、`desktop_controller_active` 和不泄露配对记录的统一失败；
- 保留旧 WebUI v2 消息 wire shape 和“仅 Web-to-Web”的既有接管语义；旧客户端没有桌面能力位时不得收到或猜测新状态；
- 新桌面字段只在桌面能力协商后发送。未知角色、状态或消息继续失败关闭，不把桌面 controller 降级成 Web lease。
- enrollment registry 与码隔离 journal 必须采用独立版本化存储或经过双读过渡，不能令旧 Relay 因现有严格 revoke state schema 出错；协议文档需列出滚动升级、旧链接保留、摘要 keyring 轮换和回滚顺序。回滚目标必须原生执行 registry 门禁，或只能位于读取同一 registry 且无公网旁路的 enrollment gateway 后方；裸 v2 回滚不受支持。

旧 WebUI v2 没有能力协商字段，兼容行为必须按 wire shape 确定：完全符合现有 PROTOCOL v2 的 `web_client_join` 且不携带新 capability 时视为 legacy Web。无桌面冲突时请求与响应字节形状、Web-to-Web 接管和事件流保持原样；有桌面 reservation/controller lease 冲突时，只发送旧客户端已识别且字段完整的通用错误 envelope `{ "v": 2, "type": "error", "code": "endpoint_not_found", "message": "endpoint not found" }` 并以既有策略关闭码 1008 关闭，不得泄漏实际是桌面占用，也不得向其发送 `desktop_controller_pending`、`desktop_controller_active` 或其他未知状态。现有旧 Web 会把该错误映射为 `desktop_offline`，保持页面可恢复，并按 500ms 起步、指数增长、10 秒封顶且 ±20% jitter 的既有策略继续重连；这是明确接受的兼容行为，不得再声称它不会重试。Relay 必须利用客户端既有 jitter 分散重试，并对这种冲突流量执行每 IP/连接/endpoint 的速率、并发和总量保护，在进入昂贵握手前拒绝；不得新增旧客户端无法识别的重试提示，所有响应保持相同原因隐藏。声明新 capability 的 Web 客户端才可接收细分冲突码；即使新 code 被意外送到旧 UI，UI 的未知错误兜底也必须显示通用失败而非崩溃。

协议兼容测试必须固定以下事实：用发布版旧 WebUI v2 的真实 join/event fixture 验证无冲突 wire 输出逐字段兼容、Web-to-Web 接管和已预先 enrolled 的旧 endpoint 重注册全量通过；桌面冲突时真实旧客户端 fixture 收到含 `message` 的上述 envelope/关闭码，状态变为 `desktop_offline`，按精确指数退避与 jitter 边界重连且不永久关闭；高并发旧客户端重试触发 Relay 限频/负载保护且响应不泄漏桌面占用；Web lease 与桌面 claim 并发只有一个成功；任一桌面 reservation/controller lease 存在时新 Web join 被拒；旧客户端不能创建桌面租约；按 5.4 节安全下限回滚后旧 WebUI 仍可用，裸 v2 目标必须拒绝。匿名旧 endpoint 首次注册不属于生产兼容承诺。

## 10. 端到端安全要求

### 10.1 威胁模型

至少考虑以下威胁：

- 攻击者在线猜测 7 位连接码；
- 被动网络观察者、Relay 普通转发日志或存储快照读取模型 Key 和任务正文；
- Relay 之外的中间人或旁路攻击者替换 A 或 B 的握手密钥；
- 旧连接重放任务、审批或附件块；
- 新客户端利用现有 endpoint 语义顶替当前 A；
- B 的错误、工具输出或日志意外泄露 A 的 API Key；
- 恶意 base URL 利用 B 发起 SSRF 或访问 B 的本机/内网服务；
- A 通过远控绕过 B 的文件、命令和工具安全边界；
- Relay 延迟、重放或混淆 A/B 心跳，使 B 的远程任务超过 A 最后有效存活后的 30 秒预算；
- 断线或进程崩溃导致凭证、附件或审批残留。

首版明确把 Relay 服务身份、配对结果和签名 transcript 视为受信认证控制面。应用层端到端加密必须抵御被动窃听、普通代理/负载均衡日志、Relay 业务存储泄漏和 Relay 之外的主动替换；它不抵御掌握 Relay 服务身份或签名密钥的恶意 Relay 主动 MITM。A→B 加密 liveness 主路径可阻止正常 Relay 或旁路节点在不知道会话密钥时伪造 A 心跳；attestation 兜底仍依赖受信 Relay 如实报告其观察年龄，不能提升为抵御恶意 Relay 的声明。Relay 主机或签名密钥被完全攻陷属于剩余风险，处置依赖服务密钥保护、最小权限、审计、轮换、吊销和客户端更新信任根，不能以密码协议已经解决来关闭风险。

### 10.2 加密要求

- A、B 到 Relay 必须使用 TLS；生产环境禁止明文 WebSocket。
- 任务正文、模型配置、API Key、附件、输出、审批和成果内容必须在应用层端到端加密。
- 正常、遵循协议的 Relay 只能看到路由所需的最小元数据和密文大小/时间，不得持有会话解密密钥；产品文案不得据此宣称能抵御恶意 Relay 主动中间人攻击。
- 必须使用经过审计的标准密码库和协议原语，不得自创加密算法。
- 会话密钥必须由 A、B 临时密钥协商产生，具有前向保密性，并绑定 Relay 签名的配对记录、reservation、B endpoint/`remote_control_generation`、双方角色/公钥、协议能力和完整 transcript 哈希。
- A、B 必须验证同一 Relay 服务身份和签名 transcript，并按 9.3 节交换角色认证的加密 Finished 与随机 secret receipt；签名只证明受信 Relay 的配对判定，不构成 Relay 自身恶意时的独立设备身份证明。
- 完成握手必须使用发送方预提交随机 commitment、发送方仅经自己的加密 Finished 将 secret 交给对端、相反角色验证后再恰好一次回传 Relay 的顺序。Receipt secret 独立于共享握手 KDF，Relay 获知它不能导出业务密钥；双方可共同派生的 key 或单方“我已完成”布尔值都不构成收件证明。
- 连接码密封和 A—B 握手必须使用独立 keypair；任何公钥复用、跨用途派生、把 code-wrap 公钥放入 transcript 或以握手公钥解密展示码都必须拒绝。
- 每个加密帧必须具备完整性校验、方向隔离、唯一 nonce 和单调序号，重复或乱序越界帧必须拒绝。
- `encrypted_controller_liveness_a` 必须使用 A→B 专属 AEAD key/nonce 空间并绑定 9.2、9.3 节字段；Relay 只能盲转发。它不得与 transport `controller_liveness`、Relay attestation 或 B→Relay 心跳共用认证 key、序号或用途标签。
- 密钥协商失败、证明不一致或降级尝试必须终止连接，不得回退到仅 TLS 明文业务帧。

### 10.3 凭证生命周期

- A 的持久 API Key 只从 A 本机安全存储读取。
- A 只构造当前任务需要的临时模型包，不发送无关模型或凭证。
- B 不得调用持久模型配置写入接口保存临时模型包。
- B 不得把临时 Key 克隆到长生命周期全局状态；确需跨异步任务持有时必须绑定取消令牌并有统一清理出口。
- 任务终态后应立即清理临时凭证；连接停止、B 退出或异常收尾必须再次兜底清理。
- 日志、事件、错误、埋点、崩溃报告和测试夹具禁止包含真实 Key。
- code-wrap 私钥、Finished receipt secret、resume token 以及 provisional/pending-liveness activation 材料同样禁止持久化或记录；发送方/接收方和 Relay 在 receipt 校验后立即清理对应 secret，失败/超时走统一清理。
- 内存清零应在语言和平台能力允许范围内采用专用敏感缓冲；无法保证物理清零的边界必须在安全设计中说明。

### 10.4 连接码防爆破

- Relay 是唯一生成方，使用 CSPRNG 和固定 31 字符表 `23456789ABCDEFGHJKMNPQRSTUVWXYZ`；7 位码共有 `31^7 = 27,512,614,111` 种可能，熵约 `34.68 bit`，不得写作或宣传为 35 bit。
- Relay 必须按 8.5 节使用当前及所有被未过期记录引用的 digest key，在全局活动码与 tombstone 索引上原子查重/占用；碰撞时最多重新采样 32 次。不能占用或 keyring 不完整时明确失败，不得把碰撞码返回 B，也不得由 B 降级为本地生成。
- 连接码对应 pairing 默认 5 分钟失效；有效 claim 先独占预留，只有握手成功的原子提交才终结该 pairing 并创建租约。预留/握手失败也永久终结 pairing，但字符串仅按 8.5 节隔离，之后可服务于新的 pairing。
- Relay 必须实施每来源 IP、每网络前缀、每 endpoint、全局并发和总请求速率限制。
- 连续异常兑换应进入有界安全计数和运维告警，但不得记录完整连接码。
- 响应时间和错误文本不应明显区分“不存在、已过期、已使用”等状态。
- 安全限流不得允许攻击者通过少量错误请求永久锁死一个真实连接码；需要兼顾防爆破和抗拒绝服务。
- B 必须把连接码视作临时控制凭证并提示用户只通过可信渠道分享；复制到剪贴板后，应用无法保证外部副本被清除，界面文案不得作虚假承诺。

### 10.5 SSRF 与远程执行边界

- B 对 A 临时模型的 base URL 必须执行与本机模型配置一致或更严格的 scheme 和 host 校验；v1 仅允许 HTTPS。
- 默认拒绝 file、ftp、data 等非批准协议及带内嵌用户凭证的 URL。
- 公网模型配置不得借 B 访问 loopback、link-local、云元数据地址或未经允许的私有网络目标。
- B 在临时模型第一次实际调用前解析主模型和视觉模型的全部 DNS 地址；任一结果不是公网地址即拒绝，解析超时或无结果也必须失败关闭。该地址快照只用于 App 层执行准备校验，不注入 CodeWhale 的最终 HTTP 客户端。由于 DNS 解析是异步执行准备的一部分，任务可以先发布 `admitted`、`running`，随后在调用模型前以 endpoint 错误进入 `failed`；该失败必须正常写入权威 Session、同步到双端并清理临时凭据。
- **已知风险（首版接受）**：最终模型请求由 CodeWhale 自行创建的 HTTP 客户端发送，可能再次解析 DNS、使用进程/系统代理或跟随服务端重定向。因此 App 层预校验不能彻底阻断 DNS rebinding、代理绕行和重定向后的目标变化。首版不为远程控制修改 CodeWhale 底座；发布说明、安全评审和运维风险清单必须保留此项，不得宣称已消除该类 SSRF 风险。
- A 的审批不能扩大 B 进程本身没有的 OS 权限。
- B 本地停止远控优先级高于 A 的任务、审批和重连请求。

## 11. 数据与持久化

### 11.1 Relay 可持有数据

Relay 只允许持有：

- B endpoint 的既有注册与在线状态；
- 活动 pairing 的码安全摘要、密封给 B 的幂等展示码响应、过期时间、reservation owner 和状态；独立持久存储中不关联 endpoint 的有界码隔离 tombstone；
- 独立的 code-wrap/握手临时公钥、签名 transcript、Finished receipt commitment/短期校验 secret、A/B resume token 摘要和不含业务内容的协议能力摘要；
- 当前租约的角色 transport liveness epoch/sequence、A 心跳 Relay 单调观察时刻、短期一次性 liveness challenge/attestation 序号和加密 liveness 的有界路由 envelope；Relay 不得持有 A→B AEAD 验证密钥或解密心跳正文；
- 当前控制租约、连接状态、限流计数及聚合运维指标；
- 为撤销和防重放所需的最小有界状态。

Relay 禁止持有：

- 连接码明文（仅允许在 CSPRNG 生成并密封给 B 的瞬时缓冲中出现，随后立即清理，禁止存储和日志）；
- A 或 B 的会话解密密钥；
- API Key、OAuth Token、Cookie 或模型完整配置；
- 用户 Prompt、模型输出、工具参数与输出；
- 附件和成果明文；
- B 的本地文件路径和 A 的保存路径。

### 11.2 B 持久化数据

B 可以持久化：

- 权威 Session 消息、任务状态和成果引用；
- 经过既有规则允许的工具与 Skill 执行结果；
- 不含 A 凭证的远控来源标记和必要审计状态。

B 禁止持久化：

- A 临时模型 API Key 和认证头；
- 可在 B 重启后恢复控制 A 的令牌；
- 连接码明文和端到端会话密钥；
- 与业务无关的 A 设备指纹。

### 11.3 A 持久化数据

A 可以持久化：

- `SessionStore` 中标记为 `RemoteProxy` 的会话记录及其格式版本；
- A 本地权威的会话标题、置顶、隐藏、最近访问等 UI 元数据；
- 经认证的用户消息、模型输出、工具状态、终态、Journal 游标和最后同步时间；
- A 本地 Session ID 与 B opaque `remote_session_ref` 的历史来源映射标识；该持久数据不是访问凭据，不得脱离后端当前 live binding 用于发送、取消、审批或上传；
- B 返回的成果元数据和用户主动下载的本地副本位置；
- B 的非敏感显示名称、会话来源和最后同步时间。

A 禁止持久化：

- B endpoint 的长期访问凭证；
- 已过期连接码、控制租约、会话密钥和重连令牌；
- 从 B 获得的工具或连接器凭证；
- 写入历史的 A 模型 API Key 副本。
- 写入 `RemoteProxy` pending/outbox 的临时模型 Key、认证头、会话密钥或其他不可持久负载。
- 将 `RemoteProxy` 的 pending 记录、本地投影或离线副本标记为 B 已接纳、已执行或已完成的权威事实。

## 12. 界面需求

### 12.1 B 端远控面板

面板至少包含：

- “开启/停止桌面远程控制”主操作；
- 7 位连接码及复制按钮；
- 秒级倒计时与过期状态；
- “刷新连接码”按钮；
- “设备空闲时，任何获得连接码的人都可以在有效期内自动连接，无需本机再次确认”的安全提示；
- “内容以端到端加密传输；配对认证依赖当前 Pinvou Relay，请只使用可信官方或自托管 Relay”的准确提示，不得宣称可抵御恶意 Relay；
- Relay 连接状态；
- 当前是否已连接 A；
- 正在预留、正在安全握手和 30 秒重连倒计时；
- 连接时间和非敏感设备名称；
- “立即断开”按钮；
- 当前远程任务及模型来源提示；
- 当前共享权威会话内 A、B 双方的用户消息、执行中状态和模型回复；B 的输入框必须发送到该会话，而不是本机普通会话；
- 代码模式工作区授权状态和允许范围；
- 不泄露内部地址、endpoint、Token 和原始协议错误的用户提示。

### 12.2 A 端连接入口

连接入口至少包含：

- 7 位码输入与粘贴；
- 输入校验和规范化；
- 连接进度；
- “配对认证依赖当前 Pinvou Relay”的信任边界提示；
- 统一的连接码失效、B 离线、B 忙、版本不兼容和网络错误状态；
- 对服务端返回的码不存在、过期、已使用、已撤销或 generation 失效，只显示完全一致的“连接码无效或已失效”；仅本地字符/长度格式错误可在提交前单独提示；
- “连接码是短期凭证；失效后请查看 B 当前码，不要保存或反复尝试旧字符串”的提示；
- 最近一次非敏感错误的重试入口；
- 不保存连接码的明确行为。

### 12.3 A 端远程消息窗口

远程消息正文界面必须复用普通本机会话的统一 `ChatView`，并以 A 本机 `SessionStore` 中的 `RemoteProxy` Session ID 作为 UI 会话身份；需要联网的操作再由 Rust 校验其与 `remote_session_ref` 及当前 connection/lease/generation 的绑定。不得长期保留另一套承担会话创建、能力协商、任务发送、权限、附件、成果或历史编排的远程聊天正文 UI；连接弹窗或壳层只可展示连接摘要与进入统一 ChatView 的导航。

远程消息窗口至少包含：

- 持续可见的“远程执行于 B”标识；
- 当前连接与同步状态；
- “被控端模型/我的模型”选择；
- 选择 A 模型时的模型名称和供应商提示，不显示 API Key；
- B 可用 Skill/能力选择；
- 附件入口；
- 发送、停止和重试；
- 相互独立的“关闭窗口”和“断开设备”：前者保留后台连接与任务，后者终止 transport 并提示 30 秒断线收尾；
- 流式消息、工具调用和审批卡片；
- 成果列表、预览和下载；
- A 独立历史状态和最后同步时间；断线后可通过只读历史投影上下文离线查看，所有发送、审批、取消、上传等变更操作保持禁用。

### 12.3.1 双端消息窗口的一致行为

- 本地与远程会话必须复用同一套消息列表、输入、停止、重试、权限、附件、成果和历史交互组件；差异由 `SessionStore` 中的会话类型、后端能力快照和远程连接状态驱动。
- 能力准备完成后，A、B 的消息窗口绑定同一条 B 权威 Session；任一端先发送都不得创建第二条上下文。
- 任一端接纳成功的用户消息必须立即在本端可见，并通过同一权威 journal 在对端可见；后续运行、终态和回复按相同 `task_id` 更新。
- 连接 generation、session 映射或租约变化时，B 必须清空旧投影并拒绝迟到的旧 session 事件，避免跨连接串消息。
- 同一 Session 有任务运行时，后续发送必须显示可观测的忙碌失败并保留输入供用户重试，不得伪装成已发送。

### 12.4 多语言与可访问性

- 新增应用文案必须进入共享 i18n，提供简体中文、英文和日文。
- 连接状态、倒计时、错误和审批结果必须可被屏幕阅读器感知。
- 连接码不得只依赖颜色区分字符和状态。
- 键盘必须能够完成输入连接码、发送、审批、取消和断开。
- 遵守系统减少动态效果设置，倒计时和状态变化不得使用不可关闭的高频动画。

## 13. 异常与降级处理

| 场景 | 预期行为 |
|---|---|
| 连接码格式错误 | A 本地阻止提交并提示 7 位格式 |
| 连接码不存在、过期、已使用、已撤销或 generation 失效 | 除本地格式错误外，wire 响应和 A 文案统一为“连接码无效或已失效”，不泄露 B 信息 |
| Relay 连续 32 次发码碰撞 | 返回 `pairing_code_generation_failed`，不创建配对记录；B 显示“暂时无法生成连接码”并允许有界退避重试，不得本地生成 |
| digest key 轮换或旧 key 缺失 | 对候选使用全部仍被引用的 key 查重；缺少任一未过期记录所需 key 时停止发码至最晚隔离到期且至少 10 分钟，不得仅用新 key 继续 |
| B endpoint 未 enrolled/已撤销 | 生产 Relay 拒绝注册且不发码；B 显示设备尚未获准远控的受控错误 |
| 配对预留/握手超时或任一方中断 | 不创建租约，原 `pairing_id` 永久终结且码字符串进入隔离；B 仍开启且在线时自动展示新码 |
| B 已停止或离线 | A 显示被控端不可用，不创建控制租约 |
| B 已被控制 | 返回设备忙，不顶替当前控制者 |
| WebUI 已连接时 A claim | 返回设备忙且不消费连接码；不得顶替 Web lease |
| A 已预留/连接时 WebUI 加入 | 返回桌面控制 pending/active；不得顶替 reservation 或 controller lease |
| Relay 重启 | 未兑换 pairing、reservation、租约、密钥路由和 A/B resume token 全部失效；持久码隔离 tombstone 及其完整 digest keyring 恢复后才能发码，B 取消远程任务并申请新码，不允许原连接恢复 |
| A→B 加密 liveness 被篡改、重放或 RTT > 5 秒 | 主路径证据拒绝；可请求 Relay attestation 兜底，但仍须扣除 observed age、完整 RTT 和 1000 ms 裕量，不得重新获得 30 秒 |
| attestation 的 age/RTT 达 5 秒上限 | 候选剩余时间最多 19 秒；更大 age/RTT、重复 nonce/序号或签名错误直接拒绝，保证从 Relay 观察 A 心跳起总计不超过 30 秒 |
| A 网络黑洞但 B 仍持续心跳 | 最后有效 A 存活证据 15 秒后进入 suspect/恢复，B 心跳不得推进 `local_safety_deadline_a`；最迟在该 A 证据对应的保守 candidate deadline 取消任务并清理凭证 |
| B 网络黑洞但 A 仍持续心跳 | Relay 只推进 `last_liveness_a`，不得伪造 `last_liveness_b`；停止向失联 B 路由，B 按本地已有 A deadline 独立收尾，A 心跳不能证明 B 在线 |
| resume 双方 prepare 后 | 仅进入禁止业务的 provisional；B activation ACK 前不得预留新 token，ACK 后也只能进入禁止业务的 pending-liveness，不得通知 A 成功 |
| B activation ACK 或 `controller_resumed` 丢失 | B 与 Relay 均在原 deadline 终止 candidate/租约并清理，禁止 Relay active/B terminal 分裂或投递业务 |
| `resume_activated_pending_liveness` 中消息丢失 | 保持原 watchdog 且业务门关闭；首个 B liveness、Relay 确认、任一 ready ACK 或最终结果未在 deadline 前完成即双端终止 |
| A/B 协议不兼容 | 拒绝连接并提示升级，不做不安全降级 |
| 端到端握手失败 | 立即关闭连接，不发送模型、消息或附件 |
| A 模型不允许传输 | 从“我的模型”列表隐藏或明确禁用 |
| A 模型 endpoint 不安全 | B 在模型调用前拒绝并返回受控错误 |
| A 模型 Key 无效 | 任务失败，不回退 B 模型，不回显 Key |
| B 模型不可用 | 明确提示 B 模型配置不可用，可由 A 重新选择 A 模型后发新任务 |
| 附件超限 | 传输前拒绝；已上传分块进入清理流程 |
| A 在附件上传中断线 | B 不启动任务并清理未提交附件 |
| A 在任务运行中断线 | 已接纳任务仅在“最后一条有效 A 存活证据后 30 秒”总 deadline 的剩余时间内继续；B 自身心跳不得延长；待审批和新权限请求立即拒绝，超时则取消任务并清理临时模型 |
| A 仅关闭远程窗口 | 后台 manager/transport 保持时不视为断线，任务继续；用户显式断开或 transport 实际中断才启动 30 秒流程 |
| 用户长期保存并重试旧 7 位字符串 | UI 提示该字符串在隔离期后可能属于新的 pairing，拒绝把它当作旧设备身份；要求查看 B 当前码 |
| 权限审批超时 | B 拒绝操作，任务按工具策略继续或失败 |
| B 查看权限请求 | 只提供查看、停止任务和断开，不提供 Allow；A 是唯一审批权威 |
| A 的审批决定重复/冲突/迟到 | 首个有效决定保持不变；同值重试幂等，冲突或迟到决定返回终态错误且不执行 |
| B 用户立即断开、停止或刷新 | 终止租约、取消远程来源的在途任务、拒绝待审批并清理凭证 |
| 事件缺口超出 journal | A 丢弃增量并通过权威快照恢复 |
| 成果已被 B 删除 | A 保留历史元数据并标记远端成果不可用 |
| A 本地历史写入失败 | 不影响 B 权威任务，A 显示历史未保存警告 |

## 14. 非功能要求

### 14.1 安全与隐私

- 安全 P0 项未完成不得通过功能开关向正式用户开放。
- 所有敏感字段必须有集中脱敏规则和自动化泄露测试。
- 新协议需完成威胁建模、密钥生命周期评审和至少一次独立安全评审。
- Relay 不得通过调试开关记录解密后的业务数据；生产包不得包含此类能力。
- 安全评审和对外文案必须使用 5.3、10.1 节的同一信任模型：Relay 是受信认证控制面，E2E 不承诺抵御恶意 Relay 主动 MITM。
- 不新增账号、密码、密钥、客户数据或内部地址到代码、示例、测试和文档。

### 14.2 可靠性

- 配对成功率、连接恢复率、任务幂等和事件完整性必须可通过确定性测试验证。
- 任何断线、重试或客户端重载不得静默重复执行有副作用的任务。
- B 的权威 Session 提交不能依赖 A 在线。
- transport 与 A→B E2E liveness 固定每 5 秒一次并使用角色专属 epoch/序号；Relay 分别维护 `last_liveness_a/b`。B 的远程任务 deadline 必须按 6.7 节从完整 RTT、Relay observed age 和 1000 ms 裕量保守换算，不能从 B 收件时重新获得 30 秒；B 自身心跳、重试以及 provisional/pending 消息均不得延长。
- 临时状态、队列、journal、附件和缓存必须有数量、字节和时间上限。
- 进程退出和异常取消路径必须执行统一资源清理。

### 14.3 性能目标

在 Relay、A、B 网络正常且不含模型自身响应时间时：

| 指标 | 目标 |
|---|---|
| 输入连接码到连接成功 | P50 ≤ 2 秒，P95 ≤ 5 秒 |
| A 发送到 B 显示用户消息 | P95 ≤ 500 毫秒 |
| B 发送后在 A、B 显示用户消息 | P95 ≤ 500 毫秒 |
| B 产生流式片段到 A 展示 | P95 附加延迟 ≤ 500 毫秒 |
| 权限请求从 B 到 A 展示 | P95 ≤ 1 秒 |
| A 审批到 B 收到决定 | P95 ≤ 1 秒 |
| A→B 加密 liveness challenge/response | 正常网络 P95 ≤ 500 毫秒；每个活动租约每 5 秒双向控制开销合计 ≤ 2 KiB |
| 短暂断线后的状态恢复 | P95 ≤ 5 秒，journal 可覆盖时不得全量重复下载 |

性能目标不允许通过降低加密、审批、幂等或持久化正确性实现；即使 liveness RTT 接近上限，也必须按保守公式缩短剩余 budget，不能为满足恢复率放宽 30 秒上界。

### 14.4 可运维性

- Relay 必须提供不含标识和业务内容的配对注册、兑换成功、兑换拒绝、设备忙、连接和异常聚合指标。
- A、B 日志使用稳定错误码，不记录连接码、Key、Token、Prompt、输出和文件路径。
- 功能必须可通过服务端和客户端功能开关逐步开放，并可独立关闭新配对入口。
- 生产 Relay 必须可审计 endpoint enrollment 的签发、禁用和拒绝聚合结果，但日志不得包含设备凭证或可识别用户数据；未配置正式 enrollment backend 时桌面配对入口保持关闭。
- Relay 新版本必须支持快速回滚；最低回滚版本须原生读取 registry 并执行 enrollment 门禁，或部署在读取同一 registry 且无公网旁路的强制 gateway 后。回滚后旧 WebUI v2 endpoint 仍可使用；门禁/gateway/registry 预检失败时必须拒绝回滚。
- 配对协议故障时，不得影响设备运营统计、现有 WebUI 页面和其他 Relay 路径。

### 14.5 架构与可维护性

- `SessionStore`、`EnginePool`、`AcpPool` 与 `RemoteControl` 保持并列职责与独立状态所有权；其上只允许无独立持久状态的薄 Command/用例编排。
- Tauri Command 仅做 IPC 参数和错误适配；本地 UI 入口与 B 远程受控入口需要共享行为时，必须复用普通 Rust 用例函数，不得内部调用 Tauri Command。
- B 受控入口必须通过 `features/remote_control` 定义的窄 `ControlledExecutionPort` 进入 conversation 用例，并由 `lib.rs` 组合根注入实现；架构守卫必须禁止 `features/remote_control` 反向依赖 `features/conversation`、`app/commands` 或 Tauri 入口。
- `RemoteProxy` 必须实施字段级写入权限：A 可写本地 UI 元数据和 RC-T-026 定义的非权威 pending/outbox；远程业务正文、任务状态和终态只能由已认证 B 事件/快照更新；离线时所有远程业务操作只读。投影持久化、pending 对账、跨租约拒绝和禁止本机 Engine/ACP 执行必须有可执行契约测试。
- 本地与远程可以声明不同能力，但不支持项必须通过能力快照显式隐藏并由后端返回稳定 `unsupported`，不得在 UI 静默降级。
- 新增普通会话能力时必须明确本地普通、本地 ACP 和远程 `RemoteProxy` 的支持矩阵；若远程暂不支持，应提供显式产品状态和测试，不得复制一套仅远程使用的 Chat UI 绕过编排边界。
- 本次架构收口不得改变 controller v1 wire contract、加密语义、B 权威 Session、A 独立历史或现有 WebUI v2 行为，除非另行完成协议版本设计和兼容评审。

## 15. 兼容与迁移要求

1. 新功能必须通过能力协商判断 A、B 和 Relay 是否均支持桌面双端控制。
2. 任一端不支持时，连接入口应明确提示升级，不发送未知消息。
3. 现有 `web-access.json` 的 endpoint、access token 和 desktop secret 不直接作为 A 的桌面控制凭证，也不能把 PROTOCOL v2 的调用方自带首次注册升级为生产 desktop enrollment；生产 B 必须先满足 5.4 节。
4. 现有 WebUI v2 分享链接、二维码和浏览器访问功能保持兼容。
5. 桌面控制与 WebUI 必须共享“单远程交互控制者”安全约束；桌面与 Web 之间当前已有租约或 reservation 时新连接返回忙碌，不静默接管；仅 Web-to-Web 保留既有接管语义。
6. 新增协议状态不得从旧字段猜测；持久数据升级必须有显式版本和失败关闭行为。
7. 首版只在 Windows 平台能力中声明支持；其他平台入口隐藏并返回 `unsupported`。
8. `remote-control-relay/PROTOCOL.md` 必须按 9.4 节更新并通过旧 Web wire-shape、Web-to-Web 接管和新旧能力组合测试后才可发布。
9. 旧 WebUI v2 无 capability 字段时必须走 9.4 节确定的 legacy 分支；新桌面冲突状态不得直接进入旧 wire。旧 desktop endpoint 只有已迁入 enrollment registry 或由管理员 allowlist 后才能在生产重新注册，这一安全门禁优先于匿名首次注册兼容。
10. 官方迁移按“客户端升级并在原 socket 认领 → registry 覆盖率审计 → 开启生产强制门禁”执行；认领成功保持 endpoint ID、access token 和现有 Web 分享链接不变，未认领客户端不得静默失败，必须进入 5.4 节恢复入口。
11. 自托管升级前必须运行 allowlist dry-run、备份 registry 并输出待迁移 endpoint 数；管理员可批量导入或逐台重新 enroll，选择凭证轮换时必须提前提示旧 Web 链接失效。
12. enrollment 发布回滚只关闭桌面双端配对能力和强制入口，不回退/删除已迁移 registry、码隔离 tombstone 或旧 endpoint 凭证；最低回滚版本必须原生支持 registry 门禁。若目标旧二进制不支持，必须先部署读取同一 registry、覆盖所有公网入口且不可旁路的 enrollment gateway，再允许切流。
13. 回滚演练必须使用真实旧 Relay 二进制和旧 Web fixture，证明 gateway 拒绝未 enrolled 首次注册、允许已 enrolled endpoint 重注册、保留原 Web 分享链接和审计结果；gateway/registry 不可用、存在直连旁路或旧二进制无法置于门禁后方时，自动化必须拒绝回滚。禁止裸 v2 回滚。
14. 架构迁移必须先固化现有本地普通与 ACP 行为，再为 `SessionStore` 增加版本化 `RemoteProxy` 记录和字段级写入权限，最后将 A 远程 Command 收敛为 `SessionStore + RemoteControl` 编排；不得以一次性替换同时改变本地会话、远控协议和 UI。
15. 迁移期间现有远程 Command、事件名和 `bridge.desktopRemoteControl` 可保留为兼容代理，但代理必须收敛到唯一薄用例链路；消费者迁移完成并有调用方扫描和回归证据后才能删除。
16. A 端旧远程历史迁移必须持久化 schema/migration revision，并为每条会话与事件保存稳定源记录 ID 及去重键（优先使用 journal epoch/sequence 或已有稳定事件 ID）。重复执行必须幂等；同去重键同内容复用，同键不同内容必须失败关闭并保留源数据，不得静默覆盖。
17. 迁移必须先在 `SessionStore` 中构建并校验完整 `RemoteProxy` 基线，记录旧数据源的稳定 high-water cursor；切换时在单一写入门下短暂冻结旧写入，补齐到最终 cursor 并校验后，以单个原子事务同时切换写入目标、供数指针和 migration revision，再释放写入门。切换前新事件仍按旧路径单写，切换后只写 `SessionStore`，任何时刻禁止双写或两个数据源同时供数。迁移中断或崩溃后必须根据 revision 幂等继续，或在原子切换未发生时安全废弃未完成基线。
18. 一旦供数指针切换到 `SessionStore.RemoteProxy`，功能开关回退只能切换已验证的 UI/Command 适配器，旧适配器也必须代理读取 `SessionStore`，不得恢复从已陈旧 `RemoteHistoryStore` 供数或双写。旧源数据至少在一个稳定版本和迁移验证/回滚窗口内保留为只读恢复源；删除必须是后续显式、可审计步骤。
19. 统一 `ChatView` 上线前必须覆盖本地普通对话、本地 ACP 代码会话、远程 B 模型、远程 A 临时模型以及连接中断恢复；清理第二套远程正文 UI、旧入口和旧数据源之前，必须通过 telemetry/静态调用方扫描证明无实际消费者，并保留一个版本的稳定兼容窗口或明确的同版本原子迁移证据。

## 16. 埋点与审计边界

允许匿名聚合以下受控事件：

- 远控开启、停止、连接码刷新；
- 配对成功/失败及受控失败原因；
- 连接、断线、恢复和设备忙；
- 远程任务提交、接纳和终态；
- 模型来源为 B 或 A 临时模型；
- 权限请求、允许、拒绝和超时；
- 附件/成果传输成功、失败和受控大小桶。

禁止进入埋点：

- 连接码、endpoint、租约、重连令牌和密钥；
- 模型名称之外的自定义 endpoint 明文、API Key 和认证头；
- Prompt、回复、工具参数、工具输出、审批正文；
- 文件名、路径、内容和成果正文；
- A、B 的公网 IP 或可识别设备名称。

埋点事件需遵循现有行为遥测的主体、受控枚举、去重、版本和数据最小化规范；本需求不自行创建另一套遥测管线。

## 17. 验收标准

### 17.1 配对与连接

- 已 enrolled 的 B 开启后，由 Relay 生成并在 5 分钟内返回可显示的 7 位连接码；B 代码路径不存在随机码生成器。
- 所有有效码只包含固定 31 字符表，7 位空间与约 34.68 bit 表述在 UI、协议和测试中一致。
- 全局活动码不会重复；强制连续 32 次碰撞时注册明确失败且无半记录，同一稳定注册请求重试返回同一配对结果。
- A 输入有效码可自动连接；B 无需二次确认。
- 同一码并发 claim 只有一个 reservation owner；每个发送方先按 9.3 节唯一长度前缀算法提交 32 字节随机 secret commitment，接收方验证对端加密 Finished 后恰好一次回传对应 secret。固定测试向量跨语言一致；提前、单方、损坏、角色互换、重放或超时 receipt 不能建租约，成功后 pairing 终结与 controller lease 创建不存在半提交状态。
- 已签发码摘要在 `max(issued_at + 10min, terminal_at + 2min)` 前不会再次分配；Relay 重启恢复隔离记录与 keyring。轮换期间用当前及所有被未过期记录引用的旧 key 查重，强制让新 key 候选碰撞旧 key tombstone 时必须拒绝；缺 key 时失败关闭。同一字符串隔离后可用于新 pairing，不能恢复旧 pairing。
- B 刷新或停止后旧 pairing 立即终结，码字符串进入隔离。
- 刷新时先 retire/撤销旧 `remote_control_generation`、租约和任务，再注册新 generation；stop/refresh 竞态中的旧回包不能重新显示码或恢复连接。
- Web lease 已连接时 A claim 收到设备忙且码不消费；A 已预留/连接时 `web_client_join` 被拒；Web-to-Web 仍按既有规则接管，并发结果只有一个赢家。
- B 重启后不记住 A，旧 pairing、租约和重连令牌均不可用；历史字符串不能作为旧设备身份。
- 官方和自托管生产模式均拒绝未 enrolled endpoint 首次注册；撤销 enrollment 后无法新建配对，开发自注册开关不能在生产启用。
- 官方既有 endpoint 可在原认证 socket 上认领并保留 endpoint ID/token/Web 分享链接；未迁移用户能看到明确提示并通过受控 enrollment API 恢复。自托管批量 allowlist/re-enroll、凭证轮换提示均有真实迁移证据；回滚目标原生执行 registry 门禁，或真实旧 Relay 只能在无旁路 enrollment gateway 后运行，裸 v2 回滚被自动拒绝。

### 17.2 模型与任务

- 同一个 `ChatView` 通过薄 Command/用例编排完成三类会话：本机普通会话使用 `SessionStore + EnginePool`，本机 ACP 使用 `SessionStore + AcpPool`，A 远程会话使用 `SessionStore(RemoteProxy) + RemoteControl`；切换会话时不得串用 Session、事件、附件或权限状态。
- A 端远程任务不会进入 A 本机 `EnginePool`，B 端解密后的任务必须进入 B 与本机会话相同的 Session、任务准入、执行器和 Journal 链路。
- A 可分别使用 B 模型和一个可传输的 A API-Key 模型完成真实任务。
- `temporary_model_package/v1` 的 primary、原生图片、视觉兜底和无图片能力模式均按 allowlist 往返；未知字段/枚举、无效组合、循环或缺失视觉引用都被拒绝。
- `SavedModel` 的每个字段均按 7.6.1 映射或排除；视觉兜底使用独立展开的路由和 API Key，不传 A 本地 `vision_model_id`/`credential_ref`。
- B 使用 A 模型时不新增或修改本地模型配置。
- A 模型调用失败不会自动切换 B 模型。
- 在 B 配置、Session、日志、错误、事件和 Relay 内存转储中扫描不到测试 Key 明文。
- 普通对话、工具调用、Skill、附件和成果任务均能在 B 完成并在两端同步。
- 分别覆盖 A 首发与 B 首发两种场景，并在同一远程会话中由另一端继续发送；所有消息均落入对应的同一个 B 权威 Session、按同一 turn lock 执行，并在两端显示用户消息、运行状态和回复。
- B 输入不得出现在另一个普通本地 Session；断开、刷新 generation 或切换远程会话后，旧 session 的迟到任务/journal 事件不得进入当前消息窗口。
- 同一 Session 任务运行时从另一端发送，必须明确返回 `task_session_busy` 且保留待重试输入；任务结束后重试只执行一次。
- B 预授权代码工作区后，A 可完成代码任务；未授权时入口隐藏且后端拒绝越界请求。
- 同一 `client_request_id` 重放不会重复执行任务或工具副作用。

### 17.3 权限与安全

- B 的受保护操作在 A 允许前不会执行。
- A 是唯一能允许远程受保护操作的端；B 权限卡只能查看/停止/断开，且任何协议级 B Allow 均被拒绝。
- A 拒绝、超时、断线、任务取消和租约变化均导致 B 拒绝该权限请求。
- 首个有效决定唯一生效；同值重试幂等，迟到、冲突、伪造、重放、篡改或跨租约审批响应不能生效。
- 在受信 Relay 遵循协议的假设下，Relay 转发日志、存储快照和被动网络抓包无法解密测试 Prompt、API Key、附件和输出；验收报告必须同时记录恶意 Relay 主动 MITM 不在首版保证内。
- 非 HTTPS、URL 中内嵌凭据和显式本机/内网 base URL 在安装前被 B 拒绝；DNS 预解析到非公网地址、无结果或超时时，已接纳任务在首次模型调用前按 `admitted → running → failed` 收口；最终请求阶段的 DNS 重绑定、系统代理和重定向目标变化按 10.5 节作为已知风险验收。
- B 本地“立即断开”在 Relay 或 A 无响应时仍能生效。

### 17.4 同步与恢复

- 带类型的公共事件 envelope 必须按事件族通过验收：普通本地和 A `RemoteProxy` 会话事件携带接收端本地 Session ID，A 远程会话事件额外校验 `remote_session_ref` 和 live connection/lease/generation，B 受控会话事件校验 B 权威 Session ID 及适用的 lease/generation，纯连接事件不要求虚构 Session 引用；task、journal epoch/sequence 和 capability revision 按类型校验。缺少该类型必填上下文、绑定不匹配或迟到的事件均被丢弃。
- A、B 的用户消息、流式文本、工具状态、审批和终态顺序一致。
- A、B 任一端提交的消息均按精确 session 映射进入同一个 B 权威 Session；A 使用 session reference，B 使用对应权威 session ID，双端投影不得把两者误当作两个会话。
- 重复事件不会生成重复 UI 项或重复历史。
- 在 journal 覆盖范围内断线重连后不丢事件。
- A/B 各自的角色 token 不同且不可互换；双端 prepare 后先进入不承载业务的 provisional，B activation ACK 后只进入 `resume_activated_pending_liveness`。首个 B post-activation liveness 经 Relay 确认且 A/B 各自 ready 后才原子轮换令牌/密钥、恢复租约和开放业务；B 验证最终结果前 watchdog 不取消。任一步骤丢失、单边超时、Relay 重启或进程重启均终止。
- 正常连接以 Relay 盲转发的 `encrypted_controller_liveness_a` 为 A 存活主路径；B 能验证 A→B AEAD、当前租约绑定、challenge、liveness epoch/sequence，并按完整 RTT 与 1000 ms 裕量计算候选 deadline。Relay 不能读取、篡改或自行生成有效主路径心跳。
- Attestation 仅作兜底，签名覆盖一次性 challenge、A/attestation 序号、`observed_age_ms` 和 `remaining_ttl_ms`；Relay 与 B 各自在自己的单调时钟域计算 age/RTT。在 age=5000 ms、RTT=5000 ms 时，B 候选剩余时间必须恰为 19000 ms，且从 Relay 最后观察 A 心跳到清理总计不超过 30000 ms。
- A、B 角色心跳序号及 `last_liveness_a/b` 相互独立。每个新 `(liveness_epoch_a, sequence_a)` 可用自己的候选值推进 deadline，相同序号只能收紧，旧序号拒绝。A 断而 B 活时，B 心跳不能延长远程任务 deadline；B 断而 A 活时，A 心跳不能让 Relay 继续向 B 路由或宣称 B 在线。
- 断线时待审批立即拒绝；任务仅在最后一条有效 A 存活证据后 30 秒总 deadline 的剩余时间内继续，超时后由 B 取消并提交明确终态、清理临时模型。
- 超出 journal 后可以通过快照恢复一致状态。
- A 仅关闭远程窗口且后台 transport 存活时不影响 B 已接纳任务完成和权威 Session 持久化；显式断开、进程退出或实际 transport 中断则只允许在最后一条有效 A 存活证据后 30 秒总 deadline 的剩余时间内继续，超时取消，检测耗时不得另加 30 秒。
- A 历史可离线查看，并明确显示其为远程会话副本。

### 17.5 兼容性

- 原 WebUI v2 协议测试全部通过。
- 发布版旧 WebUI v2 真实 fixture 在无冲突时保持原 wire；桌面冲突时只收到含 `message: "endpoint not found"` 的既有 `error/endpoint_not_found` envelope 和关闭码 1008，进入 `desktop_offline` 并按 500ms 起步、10 秒封顶、±20% jitter 的指数退避重连，不接收未知桌面状态。Relay 限频且不泄漏占用原因。
- 真实旧 Relay 二进制只有在原生支持 registry 或位于无旁路 enrollment gateway 后方时才允许回滚；旧 Web fixture 与既有分享链接仍可用，未 enrolled 首次注册被 gateway 拒绝，缺少门禁时回滚编排失败关闭。
- Windows 新功能关闭时，现有桌面和 Relay 行为不变。
- 旧 A、旧 B 或旧 Relay 组合明确提示不支持，不崩溃、不降级到明文。
- macOS、Linux 不显示可操作入口，底层调用返回明确 unsupported。

## 18. 测试要求

### 18.1 单元测试

- `SessionStore` 的本地普通、本地 ACP、`RemoteProxy` 会话类型路由，以及禁止 `RemoteProxy` 进入 A 本机 `EnginePool`/`AcpPool` 的不变式；
- 新建远程会话、绑定 opaque `remote_session_ref`、离线打开 `RemoteProxy` 投影、pending 对账及伪造、篡改、跨会话、跨租约和跨 generation 引用的拒绝；
- `prepare → persist → dispatch` 的全部故障点：prepare 后未持久化不发送，原进程内持久化后未 dispatch 可使用原 ID 恢复，同一 live binding 中发送后未 ACK 保持 outcome_unknown 并以完整规范指纹校验同参重试，ACK/Journal 后未更新投影可重放对账；A 进程重启只恢复本地 outcome_unknown 且不产生任何网络帧，重新配对后旧 ID/ref 不得自动附着或重发；
- `ControlledExecutionPort` 的注入、错误映射和单次调用；架构守卫必须拒绝 `features/remote_control` 引用 `features/conversation`、`app/commands` 或 Tauri Command 符号；
- 历史迁移的 schema/migration revision、稳定去重键、同键冲突失败关闭、部分记录、重复执行、切换前/后崩溃重启、原子供数指针和旧源只读保留；
- 普通本地任务、B 本地受控任务和 A 远程任务的可信来源派生与权限权威隔离；公共事件 envelope 各事件类型的必填字段、无占位字段和迟到过滤；
- 旧远程 API 兼容代理只调用一次新应用服务，不产生双写、重复任务或第二状态所有者；
- Relay CSPRNG 连接码生成、规范化、精确 31 字符表、`31^7` 空间、全局摘要原子占用、32 次强制碰撞耗尽和过期；
- 注册请求同 ID 同参数幂等、同 ID 异参数拒绝、密封展示码重放，以及 B 侧不存在生成路径；
- 配对记录摘要、reservation owner、10/20 秒超时、失败自动申请新码、原子消费/建租约和撤销；
- 单交互槽位状态机及 Web/controller/reservation 互斥；
- 密钥派生、帧加解密、nonce、序号和防重放；
- Relay 签名 transcript 的规范化、双方同值验证、角色/公钥/`remote_control_generation` 绑定、32 字节随机 receipt secret、唯一 LP 编码算法、固定 preimage/commitment 测试向量、commitment 顺序和加密 Finished；
- 提前 receipt、损坏 Finished、仅一方 receipt、sender 自收、错 role/socket、错 pairing/reservation/transcript/generation、secret 重放和握手超时均不建租约；共同派生 key 不能替代 receipt；
- `code_wrap_keypair` 与 `handshake_keypair_b` 独立生成、用途/类型隔离、公钥替换和跨用途复用拒绝；
- `temporary_model_package/v1` 精确 allowlist、全部 `SavedModel` 字段映射/排除、主模型/视觉兜底凭证绑定、未知字段拒绝和全终态清理；
- base URL、DNS 预校验、内网目标拒绝、执行准备失败的 `admitted → running → failed` 状态序列，以及未固定最终出站目标的已知风险记录；
- 权限请求租约绑定、A 唯一授权、首个决定、同值幂等、迟到/冲突决定、B stop 与 A Allow 竞态、超时、拒绝和重放；
- `encrypted_controller_liveness_a` 的 A→B AEAD 绑定、challenge 一次性、Relay 盲转发、篡改/跨租约/重放拒绝，以及与 transport liveness 的 key/epoch/sequence 域隔离；
- 直接路径 `30000 - RTT - 1000` 与 attestation `min(remaining_ttl, 30000 - age) - RTT - 1000` 公式边界；覆盖 age=5000/RTT=5000 得 19000 ms、超界拒绝、已安装新 epoch/sequence 替换旧 deadline、未知 epoch 拒绝、同序号只收紧和旧序号拒绝；
- 5 秒角色 transport liveness、A/B 独立单调序号与 `last_liveness_a/b`、15 秒 suspect，以及 A 断 B 活/B 断 A 活、黑洞、丢失和重放均不交叉推进；
- A/B 独立 resume token、跨角色拒绝、无业务 provisional/pending-liveness、B activation ACK、首个 B post-activation liveness、Relay 确认、双方 ready ACK、最终结果丢失、单边超时、原子激活、分别轮换及旧令牌拒绝；
- pairing tombstone 的先持久化后发码、各崩溃点无半签发、10 分钟签发隔离、终态后 2 分钟下限、重启恢复、损坏状态 cold-start 停发、多 key 查重、keyring revision 竞态、旧 key 强制碰撞和隔离后同字符串新 pairing/旧 pairing 不恢复；
- `remote_control_generation` 的开启、刷新 retire→撤销→新建、自动换码、stop 后迟到回包和稳定注册请求 ID；
- A 独立历史的去重、快照和增量合并。

### 18.2 协议与集成测试

- 三类编排分别覆盖 `SessionStore + EnginePool`、`SessionStore + AcpPool` 和 `SessionStore(RemoteProxy) + RemoteControl` 的打开会话、能力、提交、取消、权限、附件、成果、历史和事件；A 远程分支不得触发本机 EnginePool/AcpPool，B 端只产生一个权威 Session；
- 本地 Tauri Command 与 B 网络入口在同等输入下均通过同一 conversation 用例完成校验、准入和提交，每个请求只调用一次；网络入口不得经 Tauri invoke/Command 绕行；
- 迁移前、迁移中、原子切换后和 UI/Command 功能开关回退均只有一个供数源和一条写路径；切换后的旧适配器必须读 `SessionStore`，不得回读陈旧 `RemoteHistoryStore`；
- A 本地 Session ID、`remote_session_ref` 与任务来源绑定覆盖伪造、篡改、跨 session kind、跨 session、跨 connection/lease/generation 重放；B 的普通本地任务与受控任务选择正确权限权威，任何前端声明或切换来源均被拒绝；
- 断线状态可打开只读远程历史投影且不要求恢复 live binding；在该上下文提交、取消、审批、上传或发送任何业务帧均被拒绝；
- 会话类型切换、连接 generation 变化、旧兼容入口重试和新旧事件并发时，验证无串会话、无双写、无重复副作用；
- A、B、Relay 三方真实 WebSocket 配对；
- 官方 enrollment 的既有 endpoint 原 socket 认领、未迁移提示/恢复、ID/token/Web 链接保留、冲突时重新分享；自托管 dry-run/批量 allowlist/re-enroll；未 enrolled/已撤销 endpoint 拒绝、生产禁用 TOFU；使用真实旧 Relay 二进制加前置 gateway 演练允许已 enrolled 重注册、拒绝匿名首次注册和所有直连旁路，预检失败时回滚被拒绝；
- 两个 A 并发 claim 同一码、owner 断线、预留超时、唯一 commitment 测试向量、顺序错误、Finished 损坏/单方/重放、receipt secret 损坏/单方/角色互换/重放、握手超时及 B 自动申请新码；
- 两个 B 并发注册、强制 CSPRNG 碰撞、32 次耗尽、注册结果丢失后的幂等重试，以及全局活动码唯一性；
- Web 已连接时 A claim 不消费码、A reservation/controller 存在时 Web join 被拒、Web-to-Web 接管保留，以及 A/Web 同时竞争的单赢家；
- 控制租约忙碌、A/B 角色 token 交换拒绝、双方 prepare 仅得 provisional、provisional/pending-liveness 业务帧拒绝、B activation ACK 只进入 pending、首个 B liveness 及 Relay 确认、A/B ready 单边或丢失、最终结果丢失终止，以及恢复后各自令牌/密钥轮换；
- 使用真实 A→Relay→B 路径验证 Relay 盲转发 `encrypted_controller_liveness_a`、无法验证/伪造 AEAD，正常主路径推进 deadline；丢弃直接响应后才走签名 attestation，并验证 nonce、age、remaining TTL、RTT 与跨时钟字段；
- 分别模拟 A 断 B 活、B 断 A 活和 B 完全黑洞且无 close；持续注入另一角色心跳、TCP 流量、无效/重放心跳和重连请求，验证 `last_liveness_a/b` 不交叉推进、B 任务只按每个新 A epoch/sequence 的保守 candidate deadline 收尾且绝不超过 30 秒；
- Relay 重启、B 重启和 A 重载均不能复用旧租约、密钥或令牌，且任务/审批按 6.7 节收尾；
- 业务帧只以密文经过 Relay；
- 被动抓包、Relay 日志/存储扫描和 Relay 外密钥替换被阻断；测试名称和报告不得误称覆盖恶意 Relay 主动 MITM；
- RPC 结果未知与幂等台账恢复；
- 流式输出、权限、附件和成果完整链路；
- B 伪造 Allow、同/异决定并发、迟到审批、跨 lease epoch 审批及 B stop/Allow dispatch 边界；
- B 模型（无包）、A 主模型原生图片、A 无图片、A 视觉兜底双凭证、不可导出模型和模型包未知字段/枚举的集成路径；
- 模型 Key 哨兵字符串的日志与持久文件扫描；
- `remote-control-relay/PROTOCOL.md` 新消息 contract，以及发布版旧 WebUI v2 真实 fixture 的 wire shape、桌面冲突完整 `endpoint_not_found` envelope/关闭码、`desktop_offline` 状态、精确退避/jitter、Relay 限频、旧客户端组合、Web-to-Web 接管和浏览器旅程回归。

### 18.3 Windows 端到端测试

至少覆盖两台隔离 Windows 环境：

1. 已 enrolled 的 B 开启，Relay 生成连接码并由 B 展示；
2. A 配对并进入远程窗口；
3. 使用 B 模型完成普通任务；
4. 由 B 在同一权威 Session 输入下一条消息，验证 A、B 同时显示该消息、执行中状态和回复，且 B 本地不存在第二条普通 Session；
5. 使用 A 临时模型完成任务；
6. 触发工具权限并由 A 允许；
7. 触发第二项权限并由 A 拒绝；
8. 上传附件并由 B 处理；
9. B 生成成果，A 预览并下载；
10. 任务运行中断网并由 A/B 独立 token 双端恢复，验证 pending-liveness 前无业务、首个 B liveness 被双方确认后才 connected；
11. 隔离 B 网络超过 30 秒并持续制造重连流量，验证本地 watchdog 不被延长、B 取消任务、拒绝审批并清理临时模型，旧令牌无法恢复；
12. 第二个 A 尝试接入并收到设备忙；
13. 分别以 WebUI 已连接和桌面已连接状态尝试交叉接入，验证不顶替；另验证 Web-to-Web 既有接管；
14. 在 B 查看权限请求并确认没有 Allow，制造 A 决定/超时/B stop 竞态并验证唯一终态；
15. B 立即断开并验证所有凭证和待审批失效；
16. Relay 重启后验证旧连接不恢复，B 自动申请由 Relay 生成的新码；
17. B 刷新时验证旧 generation 完整撤销后才显示新码，并注入旧注册/握手迟到回包验证其被丢弃；
18. A 仅关闭窗口时验证后台任务继续，再显式断开并验证进入 30 秒收尾；
19. B 重新开启后验证旧 pairing 和旧连接不可恢复；隔离后模拟同字符串新 pairing 时不能关联到旧设备/租约。
20. 分别隔离 A 与 B：A 断 B 活时 B 心跳不延长任务，B 断 A 活时 Relay 不把 A 心跳当作 B 在线；恢复直连后验证新 A sequence 能推进 deadline，同 sequence 重放不能推进。
21. 轮换 digest key 后强制 CSPRNG 产生一个只在旧 key tombstone 中命中的码，验证新 key 下仍判碰撞；再演练真实旧 Relay 置于 enrollment gateway 后的安全回滚和裸 v2 回滚拒绝。
22. 注入直接 E2E liveness RTT=5000 ms，以及 attestation `observed_age_ms=5000`、请求 RTT=5000 ms，验证候选剩余分别为 24000/19000 ms，并从对应 A liveness 观察点断言总存活均不超过 30000 ms。
23. 在同一安装包中依次创建本机普通会话、本机 ACP 会话和远程 `RemoteProxy` 会话，验证它们共用 ChatView，但分别路由到 `SessionStore + EnginePool`、`SessionStore + AcpPool` 和 `SessionStore + RemoteControl`；切换、关闭和恢复后消息、权限、附件、成果及历史均不串线。
24. 对旧远程正文入口、旧 Command 和旧远程历史存储执行兼容窗口验收，验证它们只代理到唯一薄用例链路和 `SessionStore` 的 `RemoteProxy` 投影；移除阶段通过调用方扫描和真实用户旅程证明不存在残余入口或双写。
25. A 断线并结束恢复窗口后，在统一 ChatView 中打开 `SessionStore` 中已认证的 `RemoteProxy` 投影；验证历史可读、界面明确为离线副本，发送、取消、审批和上传均失败关闭且不会产生网络业务帧。
26. 分别注入组件故障与进程重启：在 A 原进程、原 resume 边界和同一 live binding 内制造 persist/dispatch/ACK 结果丢失，验证只用原 ID 与完整规范指纹幂等重试/对账，同 ID 异参拒绝且副作用只执行一次；强制终止并重启 A 后，验证只恢复本地 `outcome_unknown`、不发送或查询旧请求，重新配对也不使用旧 ID/ref 自动附着 B。
27. 使用包含部分会话、重复事件和人工同键冲突的旧 `RemoteHistoryStore` fixture，分别在基线构建、校验、原子切换前后中断并重启；验证幂等继续、冲突失败关闭、零双写，以及切换后回退旧 UI/Command 仍从 `SessionStore` 供数。

### 18.4 必跑回归入口

实现阶段至少运行：

```bash
npm --prefix remote-control-relay test
npm --prefix pinvou3-app run test:webui
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1
python scripts/architecture-guard.py
```

涉及 Windows 私有运行时、安装包或系统能力时，继续执行对应 Windows 专属测试；测试通过不能替代模型凭证泄露自检、协议边界自检和真实双设备验收。

## 19. 分阶段交付

### 阶段 0：协议与安全基座

- 完成威胁模型和详细技术设计；
- 正式 desktop endpoint enrollment/allowlist、既有 endpoint 认领/批量迁移、最低安全回滚版本或强制 gateway 方案，生产禁用首次自注册与裸 v2 回滚；
- Relay CSPRNG 发码、多摘要 key 查重、全局原子占用、配对预留、带固定向量的规范 receipt commitment/Finished/receipt ACK 和握手成功后的原子终结 pairing/建租约；
- Web/controller 互斥的单交互租约及竞态仲裁；
- Relay 签名 transcript、A/B 临时密钥握手与加密帧；
- `remote_control_generation`、角色隔离的 5 秒 transport liveness、A→B E2E liveness 主路径、保守 attestation 兜底公式、B 总预算 watchdog、A/B 独立 resume token、provisional/pending-liveness/final activation 和 Relay 重启收尾；
- 功能开关、能力协商和敏感信息扫描；
- 保持 WebUI v2 全量回归通过。

完成标准：可以在测试客户端之间安全建立单一密文通道；在第 10 节的受信 Relay 假设内，被动抓包、Relay 日志和存储无法读取测试业务正文，并明确记录不抵御恶意 Relay 主动 MITM 的剩余风险。

### 阶段 1：Windows 双端连接与基础任务

- B 远控面板和 7 位码；
- A 连接入口和远程会话壳层；
- 使用 B 模型的普通任务；
- 消息、流式输出、终态和断线恢复；
- B 立即断开与不记住 A。

完成标准：两台 Windows 设备可通过公网 Relay 配对并完成稳定的基础对话任务。

### 阶段 2：临时模型与完整任务能力

- A 临时模型包；
- 工具和 Skill 能力快照；
- A 远程权限审批；
- 附件传输；
- 成果预览与下载；
- A 独立历史。

完成标准：普通任务、工具、Skill、附件和成果全部满足双端同步、安全和异常验收。

### 阶段 3：并列能力与会话投影收口

- 固化 `SessionStore`、`EnginePool`、`AcpPool` 与 `RemoteControl` 并列责任，不引入 `ConversationRuntime` 或其本地/远程实现；
- 将 Tauri Command 收敛为薄 IPC 适配，抽取供本地 Command 和 B 受控入口共享的普通 Rust 用例函数；
- 为 `SessionStore` 增加版本化 `RemoteProxy` 会话，完成 A 本地 Session ID、opaque `remote_session_ref` 和 connection/lease/generation 的可信绑定；
- 将 A 已认证远程历史迁移到 `SessionStore` 投影，实现 pending 对账、离线只读和禁止本机 Engine/ACP 执行的不变式；
- 统一 `ChatView`、权限、附件、成果和历史交互，但由 Rust 按会话类型选择 `SessionStore + EnginePool`、`SessionStore + AcpPool` 或 `SessionStore + RemoteControl`；
- 将旧 Bridge/Command/历史存储收敛为单路兼容代理，完成消费者迁移后删除重复远程正文 UI、无调用方入口和双写存储；
- 完成并列能力编排、双设备、断线恢复和既有 WebUI v2 回归。

完成标准：普通本地、ACP 和远程会话复用同一 ChatView 与应用层用例；A 远程任务不进入 A 本机执行底座，B 仍是唯一执行与 Session 权威；新旧入口无双写，工作区不存在第二套远程聊天正文业务链路。

### 阶段 4：生产加固与灰度

- 容量、限流、爆破和异常网络测试；
- 灰度前根据目标部署实例内存、业务负载与安全余量开展压测，记录 pairing/reservation/lease 并发目标、持久记录与 RSS 预算、告警阈值及测量条件，并由部署负责人确认；现有硬上限不等同于容量承诺，未完成定标不得进入生产灰度；
- 两台真实 Windows 设备的公网端到端验收；
- 运维指标、告警、部署和回滚演练；
- 官方 endpoint 认领覆盖率灰度、自托管 dry-run/批量 allowlist、未迁移恢复入口和既有 Web 分享链接回归；
- 独立代码与安全评审；
- 小范围功能开关灰度。

完成标准：阶段 0～3 均已完成，第 17 节所有 P0 验收项通过，不存在真实凭证泄露、未授权执行、重复副作用或旧协议回归。

## 20. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| 7 位码被在线猜测 | 未授权控制 B | 固定 31 字符表、约 34.68 bit、短有效期、一次性消费、多维限流、异常告警和端到端握手 |
| Relay 发码碰撞或结果丢失 | 两台 B 展示同码或重试产生多个配对 | 全局摘要 CAS、最多 32 次 CSPRNG 重采样、密封幂等响应和稳定注册请求 ID |
| digest key 轮换漏查旧摘要 | 隔离中的同字符串被再次签发 | 对当前及全部被引用旧 key 逐一计算、keyring revision CAS、旧 key 到期后才退役及强制碰撞测试 |
| 用户把旧 7 位字符串当作永久设备身份 | 隔离期后字符串重新分配导致连接到另一新 pairing | pairing ID 永久终结、10 分钟持久隔离、UI 禁止长期保存并要求核对 B 当前码 |
| Relay 日志/存储或被动链路可见敏感业务 | API Key 和用户内容泄露 | 应用层端到端加密，正常 Relay 仅转发密文和最小元数据 |
| 恶意 Relay 主动替换握手密钥 | 可对业务内容实施 MITM | 明确 Relay 为受信认证控制面，使用服务身份与签名 transcript 阻断旁路替换；保护签名密钥、审计和轮换；首版不宣称消除此风险，未来需独立设备认证或用户比较码 |
| 配对预留或握手半提交 | pairing 被多方使用、租约与 pairing 状态不一致 | 唯一 reservation owner、10/20 秒超时、失败终结 pairing/隔离字符串并自动向 Relay 申请新码、终结 pairing 和建租约原子提交 |
| 未 enrolled endpoint 公网自注册 | 任意设备建立可配对 endpoint | 官方设备开通或自托管管理员 allowlist；生产禁用 TOFU，注册同时校验现有 PROTOCOL v2 约束 |
| 单方或伪造握手完成 | 未确认对端即创建租约 | 发送方独立 32 字节 receipt secret、唯一长度前缀 commitment、角色 Finished key、相反角色认证 socket 回传及原子建租约 |
| code-wrap 与握手密钥复用 | 跨协议攻击扩大密钥暴露影响 | 两套 CSPRNG keypair、类型/用途隔离、互不派生及跨用途替换测试 |
| A 临时 Key 落盘 | 凭证长期泄露 | 任务级内存上下文、统一清理、日志脱敏和哨兵扫描 |
| 恶意模型 endpoint | SSRF、访问 B 内网 | App 层限制 HTTPS、拒绝内嵌凭据并预解析全部 DNS 地址，发现非公网地址即拒绝；CodeWhale 最终请求仍可能二次解析、使用代理或跟随重定向，此残余风险由首版明确接受并持续登记 |
| Web 与桌面主控互相顶替 | 会话泄露或控制权劫持 | 共享交互槽位与 revision CAS，跨类型忙碌即拒绝；仅 Web-to-Web 保留既有接管语义 |
| 旧 Web 在桌面占用时持续重连 | Relay 负载上升 | 接受既有 `desktop_offline` 指数退避，利用 ±20% jitter，并实施每 IP/endpoint/全局限频和廉价前置拒绝 |
| 断线重试重复任务 | 重复调用模型或工具副作用 | 稳定请求 ID、持久幂等台账和结果未知状态 |
| A 审批被重放 | 未授权工具执行 | 审批绑定连接、租约、任务、请求和过期时间 |
| B 本地误成为第二审批者 | 冲突决定或绕过 A | A 为唯一审批权威；B 仅查看/停止/断开；首个 A 决定 CAS 与 dispatch 边界测试 |
| A 断线时 B 等待审批 | 任务永久挂起 | 审批超时默认拒绝，任务进入明确终态或继续策略 |
| 一方心跳错误推进另一方 deadline | A 已失联但 B 自身心跳让远程任务长期存活 | 角色专属序号、`last_liveness_a/b` 分离，B 任务只接受 A E2E 心跳或 Relay 签名 A 新鲜证明 |
| 网络时延被错误加到 A 的 30 秒预算之后 | 远程任务超过安全窗口继续运行 | challenge RTT、Relay observed age/remaining TTL、1000 ms 裕量和逐 A epoch/sequence 候选 deadline；5 秒 age/RTT 边界测试 |
| 宽限或 Relay 重启保留旧控制能力 | 旧令牌复用、任务或凭证残留 | 最后有效 A 存活证据后 30 秒总预算、完整激活才轮换、超时/重启取消远程任务并清理密钥凭证、旧令牌永久拒绝 |
| provisional resume 与 B 本地终态分裂 | Relay 向已终止 B 投递任务 | provisional/pending-liveness 均禁止业务，首个 B liveness、Relay 确认、双方 ready 和最终结果全部受原 deadline 约束 |
| B 网络隔离使 Relay 无法收尾 | 任务与临时凭证超过宽限存活 | B 本地不可延长 30 秒 watchdog 独立取消和清理；Relay 保持 resume 唯一权威 |
| 刷新/停止迟到回包复活旧状态 | 旧 pairing、租约或任务回归 UI | `remote_control_generation`、retire 后回包门、稳定请求 ID 和先撤销后新建 |
| enrollment 迁移或不安全回滚 | 旧 Web 分享失效，或裸 v2 重新开放匿名注册 | 原 socket 认领保留 ID/token、最低 registry-aware 回滚版本；旧二进制仅可置于无旁路 gateway 后，预检失败拒绝回滚 |
| 双端历史不一致 | 用户误判任务状态 | B 权威 Session、事件序号、journal 和快照恢复 |
| `RemoteProxy` 与 B 权威 Session 边界模糊 | A 的 pending/离线投影被误当成执行终态，或远程请求误入 A 本机底座 | `SessionKind::RemoteProxy` 不变式、禁止本机 Engine/ACP、B 权威 Journal 对账和离线只读测试 |
| 前端伪造远程引用或任务来源 | 跨会话/租约路由、审批权威绕过 | Rust 校验 A 本地 Session ID 与 opaque `remote_session_ref`/connection/lease/generation 绑定，后端派生任务来源与权限权威，拒绝篡改和跨绑定重放 |
| 新旧远程链路迁移期间双写 | 重复任务、重复工具副作用或两个状态所有者 | 旧入口只做代理、单路应用服务、幂等请求 ID、调用方扫描和分阶段功能开关 |
| 统一 ChatView 导致本地会话回归 | 普通对话或 ACP 功能受损 | 先固定 `SessionStore + EnginePool/AcpPool` 行为基线，再接入 `RemoteProxy + RemoteControl` 编排并执行全量本地回归 |
| 完整能力范围过大 | 首次交付周期和回归风险 | 按安全基座、基础任务、完整能力、生产加固分阶段交付 |

## 21. 需求追踪矩阵

| 原始需求 | 文档落点 |
|---|---|
| A 为主控端、B 为被控端 | 第 3、5、6 节 |
| B 开启后由 Relay 生成并返回 7 位连接码 | 第 4、6.1、7.3、10.4 节 |
| A 获取连接码后公网连接 B | 第 5、6.2、7.2 节；由 Relay 解析端点，不再本地解析 IP |
| A 指令转发到 B 执行 | 第 6.3、7.5 节 |
| A 可发送本地模型配置给 B | 第 6.4、7.6 节 |
| 模型凭证仅在 B 内存 | 第 6.4、10.3、11.2 节 |
| A 可选择 A 模型或 B 模型 | 第 7.6、12.3 节 |
| 支持工具、Skill、附件和成果 | 第 6.5、6.6、7.7、7.8 节 |
| 权限由 A 独占审批，B 仅查看/停止 | 第 4、6.5、7.1、7.7、17.3 节 |
| 指令与输出在 A、B 同步 | 第 6、7.9、8 节 |
| A 保存独立历史 | 第 7.9、11.3 节 |
| B 仅允许一个 A | 第 7.1、7.4 节 |
| 连接码短期一次性、不记住 A | 第 4、6.1、6.7、7.3 节 |
| B 无需二次授权 | 第 4、6.2、7.2 节 |
| Relay 信任与端到端加密边界 | 第 5.3、10.1、10.2、14.1、20 节 |
| Web/桌面租约互斥与 Web-to-Web 兼容 | 第 8.4、9.4、15、17.1 节 |
| 临时模型包严格字段和视觉兜底 | 第 7.6.1、17.2、18 节 |
| A→B 加密 liveness、30 秒保守预算、角色隔离、Relay 重启与旧令牌失效 | 第 5.3、6.7、7.1、7.4、8.1、9.2、9.3、10、13、14、17.4、18 节 |
| 正式 desktop endpoint enrollment | 第 5.4、7.3、15、17.1、19 节 |
| B generation 与本地 watchdog | 第 6.1、6.7、7.1、8、13、18 节 |
| Finished receipt 固定编码与 provisional/pending-liveness 激活 | 第 6.2、6.7、7.3、7.4、9.2、9.3 节 |
| 连接码 tombstone、digest key 轮换与重启 | 第 6.1、7.3、8.5、10.4、13、17.1 节 |
| 既有 endpoint 迁移、Web 链接与安全回滚下限 | 第 5.4、9.4、15、17.1、18、19 节 |
| 独立 code-wrap 与握手 keypair | 第 5.3、6.1、7.3、10.2、18 节 |
| 并列 SessionStore/EnginePool/AcpPool/RemoteControl 与薄用例编排 | 第 2.1、3、4、5.1、5.2、7.5、12.3、14.5、15、17、18、19 节 |
| A 使用 SessionStore + RemoteControl，B 保持执行与 Session 权威 | 第 3、4、5、7.5、11、17.2、19 节 |
| 控制面与会话面分离、统一 ChatView | 第 4、5.2、7.5、12.3、15、17、19 节 |
| 首版仅 Windows | 第 2.2、4、15 节 |

## 22. 完成定义

只有同时满足以下条件，功能才可认定完成：

1. 第 17 节所有 P0 验收项通过并留有可复现证据。
2. 第 18 节协议、集成、Windows 双设备和既有 WebUI 回归测试通过。
3. 独立安全评审确认不存在未认证连接、Relay 明文、Key 落盘和审批绕过；SSRF 评审必须明确记录 10.5 节已接受的最终出站目标漂移风险，除此之外不得遗留未登记的高危问题。
4. A、B、Relay 的异常和回滚路径经过演练。
5. 新增 UI 完成中文、英文和日文文案及可访问性验收。
6. 生产部署文档、配置、监控、告警和回滚步骤齐备。
7. 功能开关可以在不升级客户端的情况下关闭新的配对入口。
8. 文档、协议、测试矩阵和实现保持一致，不以占位代码、假数据或仅 happy path 作为完成状态。
9. 官方与自托管生产环境均启用正式 desktop endpoint enrollment/allowlist，未 enrolled 首次注册和生产 TOFU 被验证为失败关闭。
10. 既有 endpoint 迁移、旧 Web 链接、安全回滚下限、tombstone 多 key 轮换、Finished receipt 固定向量、A→B 加密 liveness、保守 30 秒公式、角色隔离、provisional/pending-liveness resume 和 B 黑洞 watchdog 均通过确定性故障注入与真实客户端 fixture 验收。
11. 普通本地、ACP 与远程会话通过统一 ChatView 和薄用例编排完成验收，分别使用 `SessionStore + EnginePool`、`SessionStore + AcpPool` 和 `SessionStore(RemoteProxy) + RemoteControl`；A 远程任务不进入 A 本机 EnginePool/AcpPool，B 保持唯一权威 Session，新旧入口与历史存储不存在双写，重复远程正文 UI 和无调用方旧入口均已清理。兼容代理只允许存在于阶段 3 的迁移窗口，不得进入最终完成状态。
