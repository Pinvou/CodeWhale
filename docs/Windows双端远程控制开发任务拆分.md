# Pinvou Agent Windows 双端远程控制开发任务拆分

> 状态：历史交付计划；当前闭环与兼容边界见下，不代表所有编号仍待开发
>
> 需求基线：[`Windows双端远程控制需求文档.md`](./Windows双端远程控制需求文档.md) v1.13
>
> 适用范围：Windows 双端桌面远控；现有 WebUI v2 必须保持兼容
>
> 更新日期：2026-10-02

当前 A08 依赖前移已经落实 `SessionStore.RemoteProxy`、统一 `ChatView`、薄用例和注入端口。`features/sessions/application/mod.rs` 的精确导出是 `admission`、`dispatch_remote`、`ports`、`project_remote`、`submit_controlled` 五项；旧 `sync_remote_journal` 槽已删除，禁止以空槽或兼容模块复活。任务编号用于追溯原设计及交付依赖，不证明验收通过；当前实现以《代码架构》的当前契约及 PR 当前 head 为准。

本桌面功能未发布，不迁移或兼容旧独立 history、状态别名、旧 UI / wire 入口，不双写，不保留稳定版本兼容窗口。下文 DEV-26～DEV-31 原计划明确作为历史附录，旧迁移要求已经废弃，不作为待办或技术债；已发布 Legacy Web v2 的兼容和 enrollment 迁移要求不受影响。真实浏览器、双 Windows、公网部署与 DEV-25 发布证据仍须分别验证，不以历史计划或单元测试代替。

## 1. 目的与执行规则

本文把需求基线拆成可独立实现、独立测试、独立验收和独立提交的开发任务。任务编号是交付单元，不是需求优先级；同一任务内必须先补协议模型或失败测试，再完成实现，提交时不得留下预期失败的门禁。

执行时遵守以下规则：

1. `Windows双端远程控制需求文档.md` 是产品与安全语义的唯一真相源；本文不重新定义连接码、信任模型、30 秒预算或审批权威。
2. 现有 WebUI v2 不是桌面双端协议的试验接口。新增桌面消息族、状态和持久数据必须显式版本化，旧 Web 客户端只接收既有 wire shape。
3. 每个任务单独提交，建议提交信息见任务条目；需要同时修改 Relay、Rust 和前端的任务，也应以一个可验收的纵向行为为边界，不能按文件类型拆出半成品。
4. 每项实现都要运行该任务列出的定向测试，并在合入前运行第 8 节公共门禁；测试通过不能替代密钥生命周期、迟到回包、重复副作用和日志泄露自检。
5. 新增代码遵循 `app → features → platform/core` 依赖方向。远控连接、租约和加密通道归 `pinvou3-app/src-tauri/src/features/remote_control/`，远控前端业务归 `pinvou3-app/src/features/remote-control/`；需被 Tauri Command 与网络入口共用的业务流程，放入其职责所属 `features/<name>/application/` 并显式导出，不强制新增统一 `features/conversation`。远控功能专属的 Windows 原语放在 `pinvou3-app/src-tauri/src/features/remote_control/platform/windows.rs`，不是全局 `src-tauri/src/platform/`。
6. 首版仅 Windows 声明能力。macOS/Linux 不实现替代行为，只返回稳定的 `unsupported` 并隐藏入口。

## 2. 原设计启动时的仓库快照（历史）

以下结论来自原计划制定时的代码，仅用于解释拆分起点，不代表当前实现、未完成任务或验收结论。当前闭环、五个 application 导出与未发布旧链路直接退役规则以文首及《代码架构》的当前契约为准。

| 领域 | 当时实现 | 当时识别的缺口与边界 |
|---|---|---|
| Relay 协议 | `remote-control-relay/server.js` 和 `PROTOCOL.md` 实现严格 `v: 2` 的 `desktop_endpoint_register`、`web_client_join`、Web-to-Web 接管、离线 TTL、revoke tombstone 和消息白名单 | 当前首次注册会在内存创建 endpoint，没有生产 enrollment registry；没有连接码、desktop controller reservation、E2E handshake 或 controller resume |
| Relay 状态 | `endpoints`、`revokedEndpoints`、连接/创建限流桶是进程内 Map；持久状态版本为 `RELAY_STATE_VERSION = 1` | 必须显式升级持久 schema，增加 registry、码 tombstone/keyring、pairing、共享交互槽位和 controller lease；损坏状态失败关闭 |
| Relay 测试 | `remote-control-relay/test/relay.test.js` 已覆盖 Web 注册、加入、接管、断线、TTL、限流、revoke；`web-ui.smoke.cjs` 是真实旧 Web 旅程入口 | 新测试复用真实 WebSocket 服务和旧 Web fixture，不能只做源代码正则断言 |
| Rust 远控 | `features/remote_control/` 的 `RemoteControlManager`、`relay_client.rs`、`protocol.rs`、`manager/{persistence,rpc,transfer,workspace_grants}.rs` 完成 WebUI endpoint、RPC ledger、附件/Session 分块和工作区授权 | `WebAccessConfig`/`WebAccessStatusKind` 只描述 Web 分享；桌面 A/B 状态、generation、controller lease、E2E 密钥和 watchdog 必须独立建模，不能塞进 Web 状态枚举 |
| Tauri 命令 | `app/commands/remote_control.rs` 注册大量 `web_access_*` 命令并复用 `EnginePool`、`AcpPool`、`SessionStore`、`UserPrefs` | 新增 `desktop_remote_*`（最终名称由协议任务冻结）命令，但 Command 只做 IPC 适配；共享流程必须进入 Tauri 无关用例边界，不得把敏感包放入 Web `access-policy.json` 或通用前端事件 |
| 前端桥 | `src/platform/tauri/bridge/remote-control.js` 是 WebUI 桌面代理，带 intent sequence 防止 start/stop/rotate 迟到结果复活状态；`bridge.js` 只暴露 `webAccess` | 新建桌面控制域并复用“意图序号 + generation 丢弃迟到结果”的模式；不要改变 Web bridge 的职责 |
| 现有 UI | `SettingsView.jsx` 展示 Web 分享开关、二维码、Relay 设置和工作区授权；`main.jsx` 以 `webAccessAdmin` 控制入口 | B 的“桌面远程控制”面板和 A 的“连接远程设备/远程窗口”必须独立；不得把 7 位码放进 Web 分享卡片 |
| 模型 | `platform/prefs/mod.rs::SavedModel` 含路由字段、`vision_model_id`、`credential_ref`、`credential_state`、`has_secret` 和编辑态 `credential_action`；密钥由安全存储解析 | 临时包必须从 `SavedModel` 显式投影到 `temporary_model_package/v1`，不能序列化原对象，也不能沿用未知枚举兼容兜底 |
| Session/任务 | `SessionStore` 是本机权威存储；普通聊天由 `EnginePool` 驱动，代码任务由 `AcpPool` 驱动；现有 Web RPC 有持久幂等台账和 turn/session 范围检查 | B Session 保持唯一事实源；桌面远程任务需独立 request/task ledger、来源标记和 turn lock，不得复用 Web lease 身份 |
| 审批 | `CodexAcpPendingPermission`/`CodexAcpPendingElicitation` 与 `AcpPool::respond_permission` 支持本地和 Web 操作 | 桌面远程任务必须在适配层增加绑定和 CAS；A 是唯一 Allow 权威，B UI 只能查看、停止或断开 |
| 附件/成果 | Web 路径已有有界分块上传、摘要校验、Session 隔离、artifact chunk/preview；前端有 attachments/artifacts 组件和测试 | 复用限制与预览能力，但新增 E2E 密文、connection/lease/session/artifact ID 绑定；绝不传 B 绝对路径 |
| 同步/历史 | Web 事件代理有 stream sequence、snapshot/reset 和 Session 分块；桌面本地 Session/产物可持久化 | A 的离线副本由单一 SessionStore RemoteProxy 保存；只写入认证后的投影，删除与 B Session 解耦，不另建历史仓库 |
| i18n/平台 | 文案集中在 `src/shared/i18n/{zh,en,ja}.js`；平台能力由 `src/shared/platform.js`、Web bootstrap 和 Tauri 能力返回控制 | 新文案三语同批提交；只在 Windows 打开能力，键盘、读屏、焦点和倒计时不能仅靠颜色表达 |
| 遥测/部署 | Relay 复用 `telemetry-service.js` 和现有匿名行为遥测；部署有 nginx/hardening 与 deploy 测试 | 只增加受控枚举和聚合计数；enrollment gateway、registry 备份、迁移与安全回滚必须成为生产门禁 |

## 3. 目标模块边界

建议在现有 `remote_control` feature 内形成窄接口，而不是继续扩张单一 manager：

```text
remote-control-relay/
  protocol-controller.*       # 纯协议校验、规范编码、错误 envelope
  enrollment-registry.*       # endpoint enrollment 与迁移权威
  pairing-store.*             # pairing/tombstone/digest keyring
  controller-lease.*          # 共享交互槽位、握手、租约、resume

pinvou3-app/src-tauri/src/features/remote_control/
  web_access/                  # 现有 Web 行为，迁移时保持公开接口兼容
  controller/protocol.rs       # 桌面控制消息与严格反序列化
  controller/crypto.rs         # handshake、receipt、业务帧、liveness
  controller/relay_transport.rs
  controller/controlled.rs     # B 状态所有者
  controller/controlling.rs    # A 状态所有者
  controller/ports.rs          # ControlledExecutionPort / RemoteSessionProjectionPort
  controller/task.rs
  controller/journal.rs        # B 权威 journal，不建立第二套 A 历史
  controller/transfer/         # 附件与成果的安全传输

pinvou3-app/src-tauri/src/features/sessions/
  remote_proxy/               # SessionStore 持久投影、精确绑定与 outbox
  application/                # 唯一远控提交、执行与投影用例

pinvou3-app/src-tauri/src/features/<owner>/application/
  *.rs                         # 按职责归属的 Tauri 无关用例边界

pinvou3-app/src-tauri/src/features/sessions/
  remote_proxy/                # RemoteProxy schema、binding、pending/outbox 与投影

pinvou3-app/src/features/remote-control/
  ControlledDevicePanel.jsx
  RemoteConnectForm.jsx
  RemoteConnectionShell.jsx   # 仅连接控制，会话复用现有 ChatView
  remote-control-state.js
```

目录名是建议落点；实际拆分可随职责所属调整，但必须保持 Web access、A controller、B controlled endpoint 三个连控状态所有者彼此独立。`SessionStore` 是会话状态、持久化与投影能力，`EnginePool`、`AcpPool`、`RemoteControl` 分别是三种路径的后端能力；不新增统一有状态 Runtime，也不要求三种后端实现共同基类。

`features/remote_control` 只定义窄端口并消费组合根注入：B 网络入口经 `ControlledExecutionPort` 进入共享执行用例，A 已认证入站事件经 `RemoteSessionProjectionPort` 进入投影/对账用例。用例边界的签名和实现均不得导入或调用 Tauri API；事件、窗口和路径等宿主能力由组合根通过窄端口注入。

### A08 审核修复的依赖前移边界

A08 保留 DEV-21～DEV-24 的可用 UI 与单一遥测验收，不降为占位骨架。当前本地 Tauri `task_submit` 注册不能代替真实的 A→B 接线；为修复该闭环，选择性前移下游已经实现的必要依赖，原提交历史保持不变。

| 前移边界 | 必须一起形成的行为 |
|---|---|
| 连接与恢复 owner | Relay 注册、claim、握手、加密 lane；认证存活证据；首次断线冻结截止时间，恢复期间不延长 |
| 执行与权限 owner | 唯一 Session/任务入口、真实取消；B 本地 opaque 工作区授权；快照 revision 校验；不可复用审批 ID；任务级模型凭据隔离 |
| 历史与 UI owner | SessionStore RemoteProxy 的精确绑定、原子 outbox、typed projection；现有 ChatView；不保留临时会话视图或第二套历史 |
| 文件与模型使用者 | 附件按已认证只读 handle 使用，读 owner 持有至执行终态；成果与模型释放仍受精确任务、会话和 lease 约束 |
| 能力与遥测 | 平台能力未加载或非 Windows 时失败关闭；统一状态 DTO；复用 BehaviorTelemetry 与 Relay 聚合指标，不包含身份、正文、路径或凭据 |

不前移后续 OpenCode gateway、知识库下载、连接器 PATH 及发布工作流；不宣称该波次已完成 OpenCode Zen/Go 远端模型兼容。部署脚本仅同步 Relay 模块发布单元及完整回滚清单，不执行部署或启用生产服务。默认产品编译、定向行为测试、Legacy Web 回归、独立审核与当前 head CI 分别记录；静态契约和本地测试不能代替双机/公网发布证据。

## 4. 依赖 DAG 与交付批次

```text
DEV-00 协议模型与测试夹具
 ├─ DEV-01 Enrollment registry ──┬─ DEV-02 迁移/网关/回滚
 │                               └─ DEV-03 发码与 tombstone ── DEV-04 共享槽位与 reservation
 └─ DEV-05 客户端密码学 ─────────── DEV-07 controller transport 基座
DEV-02 + DEV-04 + DEV-05 ── DEV-06 Relay 组合、握手与 controller lease
DEV-06 + DEV-07 ── DEV-08 Liveness/watchdog ── DEV-09 Resume
DEV-06 + DEV-08 + DEV-09 ──┬─ DEV-10 B 被控端生命周期
                            └─ DEV-11 A 主控端生命周期
DEV-10 + DEV-11 ── DEV-12 能力快照与工作区
DEV-07 + DEV-12 ── DEV-26 Tauri 无关用例边界与守卫
DEV-11 + DEV-26 ── DEV-27 RemoteProxy schema、路由与 binding
DEV-12 + DEV-26 + DEV-27 ── DEV-13 Session/任务/幂等
DEV-13 + DEV-27 ── DEV-28 B 双端输入与统一准入
DEV-13 + DEV-15 + DEV-27 ── DEV-29 A prepare→persist→dispatch
DEV-28 + DEV-29 ── DEV-30 类型化事件与投影端口
DEV-07 + DEV-13 ── DEV-14 transfer core
DEV-13 ───────────┬─ DEV-15 临时模型包
                  └─ DEV-16 A 唯一审批
DEV-14 ───────────┬─ DEV-17 附件上传
                  └─ DEV-18 成果下载
DEV-13 + DEV-16 + DEV-17 + DEV-18 + DEV-27..DEV-30 ── DEV-19 journal 与 RemoteProxy 投影
DEV-10 + DEV-11 + DEV-12 + DEV-15 + DEV-19 + DEV-26..DEV-30 ── DEV-20 Tauri 注册、前端桥与状态域
DEV-19 + DEV-20 + DEV-27 + DEV-30 ── DEV-31 移除旧链路与 ChatView 收口
DEV-10 + DEV-15 + DEV-16 + DEV-20 + DEV-31 ── DEV-21 B 面板
DEV-11 + DEV-20 + DEV-21 ─────────── DEV-22 A 连接壳
DEV-13 + DEV-15..DEV-20 + DEV-22 + DEV-31 ── DEV-23 远程会话能力 UI
DEV-03..DEV-20 + DEV-23 ──────────── DEV-24 遥测与安全门禁
DEV-02 + DEV-21 + DEV-22 + DEV-23 + DEV-24 + DEV-26..DEV-31 ── DEV-25 公网交付 E2E
```

<details>
<summary>历史批次与并行 owner 计划（保留原任务依赖，不作为当前执行规则）</summary>

以下表格记录原开发方案，包括后来退役的第六 application 槽、旧历史迁移和 bridge 兼容切换；这些要求已废弃。当前执行与验收只采用文首五个 application 导出及单一 `SessionStore.RemoteProxy` owner，不新增旧槽、旧源迁移或兼容代理。任务编号与原依赖保留用于追溯，不代表功能已经交付或旧计划仍需执行。

| 批次 | 可并行任务 | 合入出口 |
|---|---|---|
| 0：协议先行 | DEV-00 | 协议常量、fixture、错误模型和兼容矩阵被冻结 |
| 1A：独立基座 | DEV-01、DEV-05 可并行（均只依赖 DEV-00） | registry 与密码学原语各自通过定向测试 |
| 1B：第一层依赖 | DEV-02、DEV-03、DEV-07 可并行（分别等待 DEV-01、DEV-01、DEV-00+DEV-05） | 迁移 handler、发码存储和 Rust transport 均有稳定接口且不修改中央入口 |
| 1C：槽位 | DEV-04（等待 DEV-03） | reservation 与 Web/controller 互斥接口冻结 |
| 1D：Relay 组合与握手 | DEV-06（等待 DEV-02、DEV-04、DEV-05） | 单一任务完成 `server.js` 组合，测试客户端可建立密文 controller lease，Web v2 回归不变 |
| 2A：存活安全 | DEV-08；完成后 DEV-09 | 先证明 30 秒 watchdog，再证明无业务 provisional resume |
| 2B：双端连接 | DEV-10、DEV-11 可并行（均等待 DEV-06、DEV-08、DEV-09） | 两个 Windows backend 能配对、断开和安全恢复 |
| 3A：用例与会话基座 | DEV-12 完成后依次执行 DEV-26、DEV-27、DEV-13；然后 DEV-15 与 DEV-28 可并行，DEV-15 完成后执行 DEV-29（可与尚未完成的 DEV-28 并行），最后 DEV-30 | 先冻结 Tauri 无关边界和 RemoteProxy schema，再在 DEV-26 预声明的互不重叠文件中接入双端输入、临时模型构包、原子发件箱和事件投影 |
| 3B：业务扩展 | DEV-14、DEV-16 与 3A 的 DEV-15 可并行（均等待 DEV-13）；DEV-14 完成后 DEV-17、DEV-18 可并行 | transfer core 先冻结，模型/审批/附件/成果各自闭环且不争用共享文件 |
| 3C：同步 | DEV-19（等待 DEV-13、DEV-16～DEV-18、DEV-27～DEV-30） | journal、快照和 `SessionStore.RemoteProxy` 已认证投影覆盖全部业务事件 |
| 4A：Tauri/前端基座 | DEV-20（等待 DEV-10、DEV-11、DEV-12、DEV-15、DEV-19、DEV-26～DEV-30） | 单一组合任务注册 commands，并在 DEV-26 trait 契约下实例化/注入 DEV-28 与 DEV-30 已实现的端口适配器；前端 bridge 只做宿主适配且 Web bridge 不变 |
| 4B：收口与分面 UI | DEV-31 完成单一供数收口；然后 DEV-21、DEV-22、DEV-23 严格串行 | 旧 API/历史源不双写，远程会话接入现有 ChatView，三个 UI 任务各自仍可独立验收 |
| 4C：生产门禁 | DEV-24（等待 DEV-23 与全部后端领域） | 最小化遥测、审计、泄露扫描和最终 UI 事件门禁就绪 |
| 5：公网交付 | DEV-25 | 迁移、灰度、回滚、真实双机 E2E 全部通过 |

### 4.1 中央入口与并行文件所有权

| 中央或共享文件 | 唯一整合 owner | 后续规则 |
|---|---|---|
| `remote-control-relay/server.js` / controller router | DEV-06 | DEV-01～04 只交付独立 handler/store；DEV-08/09 使用 DEV-06 预挂载 hook；DEV-24 依赖 DEV-20 且只串行追加审计，不改路由拓扑 |
| `remote-control-relay/PROTOCOL.md` | DEV-00，DEV-06 串行落实 | 其他并行任务只引用冻结 schema/fixture，不各自改协议中央文档 |
| Relay 持久化公共接口 | DEV-01 | DEV-03 仅通过 namespace API 写 tombstone；DEV-02 不改 store；接口不足先创建串行契约修订任务 |
| `pinvou3-app/src-tauri/src/app/commands/mod.rs` | DEV-07 | DEV-07 一次性声明九个 desktop command 子模块；DEV-10/11/12/15～19 及 DEV-29 只编辑各自预留文件，其中 `desktop_remote_task.rs` 的薄适配由 DEV-29 独占 |
| `pinvou3-app/src-tauri/src/lib.rs` 的 command/state/端口装配 | DEV-20 | 所有 backend command 先以模块单测独立验收；DEV-20 只在 `lib.rs` 一次性注册 command 并实例化/注入 DEV-28/DEV-30 已实现的端口适配器，不修改适配器实现或允许并行任务修改 invoke 列表 |
| `features/sessions/application/{mod.rs,ports.rs}`、预声明用例槽与 `controller/ports.rs` | DEV-26 | DEV-26 一次性预声明/导出必要空契约；后续独占映射为 DEV-13→`admission.rs`、DEV-19→`sync_remote_journal.rs`、DEV-28→`submit_controlled.rs`、DEV-29→`dispatch_remote.rs`、DEV-30→`project_remote.rs`，各任务只编辑本实现文件，不修改 `mod.rs`、`ports.rs`、中央 export 或回改端口语义 |
| `features/sessions/remote_proxy/` 的 schema/version migration | DEV-27 | DEV-29 只新增 `outbox` 实现，DEV-30 独占 `projection` 实现，DEV-19 只消费 DEV-30 公开投影端口而不编辑实现；共享 schema 不足时先串行修订 DEV-27 契约 |
| legacy `RemoteHistoryStore` 数据迁移文件 | DEV-31 | 作为中央所有权例外由 DEV-31 独占；只消费 DEV-27 冻结的 `RemoteProxy` schema/version API，不修改 DEV-27 的 schema migration 文件 |
| B 权威 Session 映射、ledger、turn lock、执行适配与准入核心原语/API | DEV-13 | DEV-13 独占 `features/sessions/application/admission.rs`；DEV-15～18 只实现 model/permission/upload/download 扩展；DEV-28 只基于冻结 API 接入 A/B 两种输入生命周期并在能力准备前创建/幂等复用映射，不回改准入核心 |
| `controller/transfer/{core,limits,mod}.rs` | DEV-14 | DEV-17/18 分别独占 `upload.rs`/`download.rs`；共享契约变化必须先串行修订 DEV-14 |
| `src/platform/tauri/bridge.js` 与 desktop bridge 安装 | DEV-20 | DEV-31 负责旧 bridge 兼容切换；DEV-21～23 只消费 domain API，不越过 bridge 直接调用 Tauri |
| `ChatView`、`src/app/main.jsx`、`SettingsView.jsx`、`src/shared/i18n/{zh,en,ja}.js` | DEV-31 → DEV-21 → DEV-22 → DEV-23 | DEV-31 先完成会话适配与供数切换；三个 UI 任务再严格串行，各自只追加本任务组件和 i18n 命名空间 |
| 现有 `app/commands/remote_control.rs`、Web `access-policy.json` | 无 desktop task 可修改 | 保持 WebUI v2 专属；若确需兼容修订，必须另建串行 Web 回归任务 |

并行任务只有在“预计文件”所有权不重叠时才能同时开发。测试 fixture 使用任务独立文件；不得让两个并行任务共同追加同一测试文件、`mod.rs`、中央 export 列表或协议枚举。DEV-26 预声明的 application 模块槽是 DEV-28/DEV-29 并行的前置条件；两任务不得为添加 export 而修改 owner 文件。

</details>

## 5. 任务明细

### DEV-00：桌面控制协议模型、兼容矩阵与测试夹具

- **依赖**：无。
- **目标**：先冻结 desktop controller 的 wire schema、状态机、错误 envelope、尺寸上限、规范序列化和真实新旧组合 fixture，使后续实现不靠字符串拼接或隐式字段猜测。
- **范围**：更新 `remote-control-relay/PROTOCOL.md`；新增纯解析/校验模块和固定 fixture；定义 capability 协商、`remote_control_generation`、pairing/reservation/lease、加密帧、receipt、liveness、resume、任务、审批、附件、成果和 snapshot/journal 消息；冻结 legacy Web 冲突映射。
- **不做**：不分配连接码，不建真实租约，不实现加密算法或业务执行。
- **预计文件**：`remote-control-relay/PROTOCOL.md`、`remote-control-relay/protocol-controller.js`（新）、`remote-control-relay/test/fixtures/`（新）、`remote-control-relay/test/controller-protocol.test.js`（新）、`pinvou3-app/src-tauri/src/features/remote_control/controller/protocol.rs`（新）。
- **协议/数据**：严格版本字段；未知字段/枚举失败关闭；旧 Web 无 capability 时仍只收到含 `message` 的 `error/endpoint_not_found` 与 1008；所有敏感消息注明禁止日志字段。
- **测试**：JSON/二进制边界、未知字段、超长字段、错误码统一、旧 Web fixture、新旧 A/B/Relay 能力矩阵、跨角色消息拒绝。
- **验收**：协议文档、JS/Rust fixture 对同一合法/非法样本结论一致；测试本身通过；没有启用未实现业务路径。
- **风险**：双语言 schema 漂移。以共享 fixture 哈希和 protocol contract 测试阻断。
- **需求映射**：RC-A-004、RC-P-002、RC-P-009、RC-P-013、RC-L-003～004、RC-T-003、RC-T-008、RC-C-012、RC-S-003、RC-M-005～006。
- **建议提交**：`feat(remote-control): 定义桌面控制协议契约`

### DEV-01：生产 desktop endpoint enrollment registry

- **依赖**：DEV-00。
- **目标**：建立公网 desktop endpoint 的正式注册权威，生产拒绝未 enrolled 首次注册，同时保留自托管配置能力。
- **范围**：版本化 registry schema、enrollment credential 摘要、状态/撤销/轮换、原子持久化、备份恢复、官方与自托管配置开关；提供供 Web v2 重注册和 `desktop_pairing_register` 调用的失败关闭校验 API；建立可按 namespace 原子持久化的 Relay state store，供后续 pairing tombstone 使用。
- **不做**：不做既有 endpoint 批量迁移 UI，不生成 pairing code，不实现 gateway 回滚。
- **预计文件**：`remote-control-relay/enrollment-registry.js`（新，本任务独占）、`remote-control-relay/relay-state-store.js`（新，本任务冻结公开接口）、`test/enrollment-registry.test.js`（新）、部署环境示例/说明；不修改 `server.js`，由 DEV-06 统一接线。
- **协议/数据**：Relay state schema 升级；registry 条目含 endpoint、credential digest、状态、revision、enrolled/rotated/revoked 时间和审计最小字段；损坏或降级读失败时生产关闭注册。
- **测试**：已 enrolled 注册、匿名首次注册拒绝、撤销/轮换、并发写 CAS、持久化失败、重启恢复、旧 state 迁移、生产禁用 TOFU、自托管显式开发模式。
- **验收**：registry API 对未 enrolled、已撤销和损坏状态全部失败关闭，已 enrolled Web 凭证可通过；本任务不单独开启生产入口，所有公网入口的无旁路验收在 DEV-06 接线后执行。
- **风险**：一次性切门禁造成存量离线。DEV-02 完成前不在生产强制启用。
- **需求映射**：RC-P-012、RC-P-019、RC-P-022～023。
- **建议提交**：`feat(relay): 增加桌面端注册门禁`

### DEV-02：既有 endpoint 迁移、enrollment gateway 与安全回滚

- **依赖**：DEV-01。
- **目标**：提供官方原 socket 认领、自托管 dry-run/批量 allowlist/re-enroll，以及真实旧 Relay 必须置于无旁路 gateway 后的可执行回滚路径。
- **范围**：独立 migration handler/CLI、稳定幂等请求、冲突处理、覆盖率审计、registry 备份、gateway 校验、直连旁路预检、旧 Web 链接保留验证、未迁移提示所需状态；handler 只调用 DEV-01 registry API。
- **不做**：不实现桌面控制 UI；不允许裸 v2 回滚；不删除已迁移 registry。
- **预计文件**：`remote-control-relay/enrollment-migration-handler.js`（新，本任务独占）、`scripts/` 下迁移工具（新）、nginx/gateway 配置与 `test/deploy.test.js`、`test/remote-test-deploy.test.js`、新增迁移测试；不修改 `server.js`、`enrollment-registry.js` 或 pairing 文件，DEV-06 统一挂载 handler。
- **协议/数据**：认领绑定原认证 socket；成功保持 endpoint ID/token；选择重新 enroll 时明确旧分享链接失效；回滚只关闭新功能，不回退 registry/tombstone schema。
- **测试**：原 socket 认领幂等/冲突、批量 dry-run、备份恢复、真实旧 Relay + gateway、已 enrolled 重注册、匿名首次注册/旁路拒绝、gateway 故障时回滚编排拒绝、旧 Web fixture。
- **验收**：部署脚本能输出待迁移数和安全预检结果；未满足门禁时自动失败；迁移成功样本的旧 Web 链接继续可用。
- **风险**：部署拓扑存在未纳管入口。用全入口探测清单和发布前自动预检阻断。
- **需求映射**：RC-P-019、RC-P-022～023。
- **建议提交**：`feat(relay): 完成endpoint迁移与安全回滚`

### DEV-03：Relay 发码、全局摘要占用与 tombstone/keyring

- **依赖**：DEV-00、DEV-01。
- **目标**：实现 Relay 唯一发码、严格 31 字符集、全局 CAS、幂等密封响应、先 tombstone 后发码和多 digest key 查重。
- **范围**：CSPRNG 7 位码、5 分钟 TTL、32 次碰撞重采样、规范化、摘要索引、10 分钟签发隔离/终态后不少于 2 分钟、keyring revision、密封给 B 的一次展示值、稳定 registration request ledger；实现独立 pairing registration handler。
- **不做**：不接受 A claim，不创建 reservation 或 controller lease；B 不包含任何本地码生成后备路径。
- **预计文件**：`remote-control-relay/pairing-store.js`、`pairing-registration-handler.js`（均新，本任务独占）、`test/pairing-code.test.js`（新）；只调用 DEV-01 `relay-state-store.js` 冻结接口，不修改 `server.js` 或 enrollment migration handler，DEV-06 统一挂载。
- **协议/数据**：`pairing_id` 永久终结，7 位字符串只在隔离期内禁重分配；活动记录仅内存，tombstone/keyring/幂等密封响应按需求持久化；持久化失败不返回码。
- **测试**：字符分布/字母表、强制碰撞与 32 次耗尽、并发 B 注册、同请求同参幂等/异参拒绝、结果丢失重试、各崩溃点、重启、多 key 强制碰撞、隔离后同字符串新 pairing 但旧 pairing 不恢复、日志扫描。
- **验收**：任何成功返回给 B 的码都已有 durable tombstone 和唯一活动摘要；代码库不存在 B/A 生成路径；码明文不进日志或遥测。
- **风险**：密钥轮换漏查旧摘要。旧 key 最后引用过期前不得卸载，以 keyring revision CAS 阻断竞态。
- **需求映射**：RC-B-002～003、RC-P-001～006、RC-P-008、RC-P-011、RC-P-015～017、RC-P-021、RC-P-024。
- **建议提交**：`feat(relay): 实现安全连接码分配`

### DEV-04：共享交互槽位、claim reservation 与竞态仲裁

- **依赖**：DEV-03。
- **目标**：将 Web lease、desktop reservation 和 controller lease 纳入同一 endpoint 交互槽位，使用 revision CAS 决出唯一赢家。
- **范围**：claim 规范化、来源/连接/全局限流、reservation owner 绑定、10 秒 reserved/20 秒 handshake 超时、断线清理、失败自动换码通知、统一无效码错误、Web-to-Web 既有接管保留。
- **不做**：不完成握手 receipt，不签发 controller lease，不改变无冲突 Web v2 wire。
- **预计文件**：`remote-control-relay/controller-reservation.js`（新，本任务独占）、`test/controller-reservation.test.js`（新）及任务专属 fixture；只调用 DEV-03 PairingStore CAS 接口，不修改 `pairing-store.js`、`server.js` 或 `controller-lease.js`。
- **协议/数据**：reservation 绑定 request ID、claim socket 临时身份、公钥哈希、endpoint、generation；跨类型冲突的旧 Web 映射为既有 `endpoint_not_found`，新 A 得到 `controller_busy` 或统一码无效语义。
- **测试**：两个 A 同码并发、owner 断线/超时、Web 已连接时 claim 不消费码、reservation 存在时 Web join、A/Web 同时竞争、Web-to-Web 接管、限流与爆破告警、过期/已用/撤销同文案。
- **验收**：每个 endpoint 任意时刻最多一个交互槽位所有者；失败 claim 不泄漏 endpoint 历史且不改变合法 owner。
- **风险**：旧 Web 高频重连放大负载。拒绝必须在昂贵密码学前完成并带现有限流/jitter 保护。
- **需求映射**：RC-B-008～009、RC-P-007、RC-P-009～010、RC-P-013～014、RC-L-002、RC-L-007。
- **建议提交**：`feat(relay): 仲裁桌面与Web控制槽位`

### DEV-05：A/B 密码学原语、规范 receipt 与敏感序列化

- **依赖**：DEV-00。
- **目标**：在 Rust 提供可互操作的独立 code-wrap/handshake keypair、Relay 签名 transcript 验证、Finished、receipt commitment、业务 AEAD 和防重放原语。
- **范围**：严格 LP 编码、32 字节随机 receipt secret、域分离、固定测试向量、密钥派生/清零、nonce/epoch/sequence、`SensitiveString`、加密帧尺寸前置检查；JS 仅实现 Relay 所需 commitment 校验和 fixture 验证。
- **不做**：不联网、不拥有连接状态、不把共同派生 key 当 receipt 证据。
- **预计文件**：`controller/crypto.rs`、`controller/protocol.rs`（新）、`remote-control-relay/protocol-controller.js`、双方固定向量 fixture、Rust/Node 单测。
- **协议/数据**：发送方只在加密 Finished 内给对端 secret；接收方验证后恰好一次经角色认证 socket 回传；Relay 只验证 commitment，不能导出业务会话密钥。
- **测试**：文档固定 preimage/commitment 向量、字段顺序/长度/编码、跨角色/跨用途替换、损坏 Finished、secret 重放、nonce/sequence 重放、未知 epoch、Debug/错误脱敏、内存清理钩子。
- **验收**：Rust/Node 对固定向量完全一致；任何 receipt secret 不出现在明文业务消息和日志；两套 keypair 类型上不能互换。
- **风险**：密码学组合错误。仅采用经过审计的库原语，安全评审在 DEV-24 设为发布门禁。
- **需求映射**：RC-P-018、RC-P-020、RC-L-003～004、RC-M-007、RC-M-012、RC-S-006～007。
- **建议提交**：`feat(remote-control): 增加端到端加密原语`

### DEV-06：Relay 中央组合、两阶段握手与 controller lease 原子建立

- **依赖**：DEV-02、DEV-04、DEV-05。
- **目标**：完成 claim 后的 transcript、commitment、加密 Finished 盲转发、双 receipt 校验，以及“消费 pairing + 创建 lease”的单一原子转换。
- **范围**：Relay epoch/pairing/reservation/generation/角色绑定；claim socket 临时身份；消息顺序和超时；租约随机 ID、lease epoch、A/B 独立初始 resume token 摘要；失败终结 pairing 并通知 B 自动申请新码；把 DEV-01～04 的独立 handler 组合进现有 Web v2 dispatcher，并为 DEV-08/09 冻结 liveness/resume hook 槽。
- **不做**：不开放业务 RPC，不实现断线 resume，不让 Relay 解密 Finished 或业务正文。
- **预计文件**：`remote-control-relay/controller-lease.js`、`controller-router.js`（新，本任务独占）、`server.js`（本阶段唯一 owner）、`PROTOCOL.md`、`controller-liveness.js`/`controller-resume.js` 的可编译 hook 槽、`test/controller-handshake.test.js`（新）、Rust 测试客户端 fixture；后续 DEV-08/09 不再修改 `server.js`/router。
- **协议/数据**：只有两个相反角色 receipt 都匹配各自 sender commitment 才提交；pairing 消费和 lease 插入必须同 revision 原子完成；租约绑定 endpoint/generation/claim identity/protocol/session keys。
- **测试**：真实三方 WebSocket、enrollment/migration/pairing/reservation handler 组合、commitment 顺序、提前/单方/损坏/错 socket/错角色/错 transcript/重放 receipt、任一方断线、超时、原子提交故障注入、busy、旧 pairing 不恢复、旧 Web fixture。
- **验收**：成功路径只有一个 controller lease；任何半握手不消费为可恢复 pairing、不产生半租约，Relay 抓包只见密文和最小元数据。
- **风险**：状态半提交。所有转换通过一个带 revision 的 store API，禁止 handler 跨多个 Map 手工更新。
- **需求映射**：RC-A-003、RC-P-007～008、RC-P-013～014、RC-P-018、RC-L-001～006、RC-L-009。
- **建议提交**：`feat(relay): 原子建立桌面控制租约`

### DEV-07：Rust 桌面控制 transport 与状态所有者基座

- **依赖**：DEV-00、DEV-05。
- **目标**：在不改变现有 WebAccess API 的前提下，先建立 A/B 共用的有界 Relay transport、controller frame dispatch 和独立状态所有者接口，成为 liveness 与 resume 的唯一网络承载层。
- **范围**：从当前 `relay_client.rs` 提取可复用连接原语；为 controller 增加有界收发队列、优先级/背压、敏感帧专用序列化、取消 token、事件 sink；明确 Web manager、B manager、A manager 生命周期；提供 transport clock/RTT 注入点；一次性声明 controller feature 与 desktop command 子模块槽。DEV-13 后续在职责所属 application 边界冻结准入核心与扩展 trait。
- **不做**：不实现具体配对、liveness、watchdog 或 resume；不改 Web `access-policy.json`；不把 controller 状态加入 `WebAccessStatusKind`。
- **预计文件**：`features/remote_control/mod.rs`、`relay_client.rs`、`controller/{mod,relay_transport}.rs`（新）、预留的 `controller/{liveness,resume,controlled,controlling,capabilities,task,temporary_model,permission,journal}.rs` 与 `controller/transfer/mod.rs` 可编译槽、`app/commands/mod.rs`、预留的 `desktop_remote_{controlled,controller,capabilities,task,model,permission,attachment,artifact,journal}.rs` command 模块和 transport 单测。A 历史统一归 DEV-19 的 `features/sessions/remote_proxy/`，不得预留第二历史实现。本任务拥有 transport 基座；后续模块声明由各 owner 串行追加，DEV-20 独占 `lib.rs` 最终组合与注册。
- **协议/数据**：业务帧只接受已认证 lease context；队列上限不得宽于现有 Relay 帧与字节预算；断线事件带 generation/lease epoch；控制帧和业务帧分队列且都受总字节预算。
- **测试**：队列满、敏感帧过大、取消、重连、消息优先级、RTT 采样、旧 generation/epoch frame 丢弃；现有 remote_control Rust 测试和 Web contract 不回归。
- **验收**：三个状态域可独立启动/停止；controller transport 未连接时不能调用 Web RPC 后门；现有 Web 分享行为兼容；后续模块无需编辑 transport 内部实现。
- **风险**：重构影响 Web。先加适配接口再迁移调用，保留当前 public methods 并跑完整 Web 回归。
- **需求映射**：RC-B-013、RC-L-003～004、RC-T-008、RC-S-003。
- **建议提交**：`refactor(remote-control): 拆分桌面控制传输域`

### DEV-08：角色隔离 liveness 与 B 本地 30 秒 watchdog

- **依赖**：DEV-06、DEV-07。
- **目标**：在已冻结的 transport 上实现 5 秒角色心跳、A→B E2E liveness 主路径、Relay attestation 兜底和总计不超过 30 秒的 B 本地 watchdog。
- **范围**：`last_liveness_a/b`、角色专属 epoch/sequence/challenge；15 秒 suspect；直接与 attestation 保守 deadline 公式；B 本地单调时钟 deadline；Relay 盲转发和签名新鲜证明；断线/黑洞清理 hook。
- **不做**：不实现 resume token/prepare/activation；不执行业务任务；普通 TCP 活动、B 心跳或重试不能延长 A 缺失 deadline。
- **预计文件**：`controller/liveness.rs`（新，本任务独占）、Relay `controller-liveness.js`（DEV-06 已挂载的 hook，本任务独占实现）、`test/controller-liveness.test.js`（新）、Rust deadline 单测；只调用 DEV-06 lease 与 DEV-07 transport 的公开接口，不修改 `server.js`、router 或中央 Rust 模块。
- **协议/数据**：新 A epoch/sequence 可安装新 candidate deadline；同序号只能收紧，旧序号拒绝；直接证据用 `30000 - RTT - 1000`，attestation 用 `min(remaining_ttl, 30000 - age) - RTT - 1000`。
- **测试**：A 断 B 活、B 断 A 活、黑洞、另一角色/无效/重放心跳；age=5000/RTT=5000 的 19000ms attestation 与直接 24000ms；新序号推进、同序号收紧、旧序号拒绝、Relay 不可达和本地 deadline 到期。
- **验收**：从任一最后有效 A liveness 观察点到 B 触发清理绝不超过 30000ms；B 心跳与普通流量永不推进该 deadline。
- **风险**：跨时钟计算延长窗口。B 只用本地单调时钟和 Relay 提供的 age/remaining TTL，完整扣除测得 RTT 与 1000ms 裕量。
- **需求映射**：RC-B-015～018、RC-L-008、RC-L-012、RC-L-015～016、RC-T-007。
- **建议提交**：`feat(remote-control): 实现角色隔离存活看门狗`

### DEV-09：双端 provisional/pending-liveness resume

- **依赖**：DEV-08。
- **目标**：在同一 watchdog 预算内实现 A/B 独立单次 token 的双端恢复，完整激活前不承载业务。
- **范围**：A/B 角色绑定 token 摘要、双方 prepare、`resume_provisional`、B activation ACK、`resume_activated_pending_liveness`、首个 post-activation B liveness、Relay 确认、双方 ready、原子轮换 token/密钥和最终开放业务。
- **不做**：不创建新 liveness 计算公式；不因 prepare/ACK/重试重置 DEV-08 deadline；Relay 重启不恢复 lease。
- **预计文件**：`controller/resume.rs`（新，本任务独占）、Relay `controller-resume.js`（DEV-06 已挂载的 hook，本任务独占实现）、`test/controller-resume.test.js`（新）；通过 DEV-06 lease、DEV-07 transport 和 DEV-08 liveness 公开接口工作，不修改其内部文件、`server.js` 或 router。
- **协议/数据**：两个 pending 状态均拒绝业务帧；token 不相等、不可跨角色、成功后分别消费轮换；任一步缺失或超时终止原租约。
- **测试**：单边 prepare、token 交换/重放、B ACK 丢失、首个 liveness/Relay 确认/单边 ready/final 丢失、pending 状态业务拒绝、deadline 到期、Relay/A/B 重启和成功后旧 token/key 拒绝。
- **验收**：只有完整激活原子提交后才开放业务；B 收到最终 `controller_resumed` 才取消旧 watchdog；失败绝不形成 Relay active/B terminal 分裂。
- **风险**：多阶段状态遗漏收尾。每一阶段共享单一 transition table 和 deadline，终态统一销毁 lease/token/key。
- **需求映射**：RC-L-005～006、RC-L-009～014、RC-T-007。
- **建议提交**：`feat(remote-control): 实现双端安全恢复`

### DEV-10：B 被控端 lifecycle、generation、watchdog 与 Tauri 命令

- **依赖**：DEV-06、DEV-08、DEV-09。
- **目标**：实现 B 开启、展示码、刷新、停止、立即断开、自动换码、能力状态与本地安全收尾的唯一状态机。
- **范围**：每次开启/刷新新 generation；稳定 registration request ID；refresh 的 retire→Relay 撤销确认→本地清理→新 generation；完整状态枚举；设备信息只存内存；本地 30 秒 watchdog；任务/审批/密钥/临时模型清理 hook；预授权工作区入口；Tauri commands/events。
- **不做**：不实现 A UI，不在 B 提供 Allow，不持久记住 A，不在非 Windows 假装支持。
- **预计文件**：`controller/controlled.rs`、`manager/workspace_grants.rs`、`app/commands/desktop_remote_controlled.rs`（均由本任务独占）、Windows platform 适配和单测；协议类型使用 DEV-00，模块槽使用 DEV-07，不修改 `controller/protocol.rs`、`app/commands/mod.rs`、现有 `remote_control.rs` 或 `lib.rs`。
- **协议/数据**：B 持久化仅 enrollment 引用、用户开关/工作区授权和必要配置；pairing/lease/key/token/设备名不持久化；所有回包带 generation，retire 后丢弃。
- **测试**：start/refresh/stop 竞态、旧回包、撤销无法确认时失败关闭、自动换码、立即断开离线执行、Relay 黑洞、进程重启、设备名不落盘、工作区未授权、非 Windows unsupported。
- **验收**：B 无需 A/Relay 响应即可触发本地终止；刷新不会同时存在两代 pairing；停止后没有可用 lease、token、审批或敏感上下文。
- **风险**：异步结果复活旧状态。复用现有前端 intent sequence 思路，并在 Rust 以 generation 作为硬门。
- **需求映射**：RC-B-001～018、RC-L-006、RC-L-008、RC-T-011～012、RC-C-010、RC-C-014。
- **建议提交**：`feat(remote-control): 实现Windows被控端状态机`

### DEV-11：A 主控端 claim、连接生命周期与 RemoteProxy 绑定入口

- **依赖**：DEV-06、DEV-08、DEV-09。
- **目标**：实现 A 输入码、自动握手、后台连接、显式断开、短暂恢复和非敏感历史容器，为后续业务消息提供稳定 connection context。
- **范围**：码规范化；连接状态/错误映射；A 临时 claim 身份；后台 transport 与窗口可见性解耦；显式 disconnect；resume token 内存生命周期；远程连接元数据与 SessionStore RemoteProxy 的精确绑定入口；Tauri commands/events。
- **不做**：不持久化码/设备凭证，不实现具体任务/模型/附件，不让关闭窗口等于断开。
- **预计文件**：`controller/controlling.rs`、`app/commands/desktop_remote_controller.rs`（均由本任务独占）和 Rust 单测；历史只使用 SessionStore RemoteProxy 窄接口，持久投影实现归 DEV-19；不另建历史 manager/store，不修改现有 `remote_control.rs` 或 `lib.rs`。
- **协议/数据**：持久化只含远程历史标题、脱敏设备显示名、终态和 B Session 映射的非敏感引用；所有可操作 API 要求 live connection/lease/epoch。
- **测试**：粘贴/大小写/分隔符、统一无效码、busy/离线/版本不兼容、窗口隐藏后台继续、显式断开、旧码不自动重试、进程重启 token 不恢复、迟到连接结果丢弃。
- **验收**：输入有效码无需 B 二次确认进入 connected；断开后发送/审批 API 同步拒绝；RemoteProxy 离线投影可经 SessionStore 枚举但明确不是活动连接。
- **风险**：误把短期码当设备身份。存储 schema 不含 code，UI API 只返回短期凭证提示状态。
- **需求映射**：RC-A-001～010、RC-L-005、RC-L-009～014、RC-S-008～009。
- **建议提交**：`feat(remote-control): 实现Windows主控端连接`

### DEV-12：B 能力快照、模式可见性与代码工作区约束

- **依赖**：DEV-10、DEV-11。
- **目标**：让 A 只看到 B 当前允许的普通/代码模式、工具/Skill 摘要和预授权工作区能力，不暴露私有文件或凭证。
- **范围**：能力快照 schema/revision/过期；从现有 bundle readiness、AcpPool agent/mode、工具与 Skill 启用状态投影；工作区用 opaque ID 而不是路径；B 策略变化时撤销或刷新能力。
- **不做**：不传 A 的 Skill/安装包，不允许 A 修改 B 工具配置，不扩大现有 workspace grant。
- **预计文件**：DEV-07 预留的 `controller/capabilities.rs` 与 `app/commands/desktop_remote_capabilities.rs`（本任务独占实现）、`manager/workspace_grants.rs`、Codex ACP 只读接口和任务专属 fixture 测试；不修改 `features/remote_control/mod.rs`、`app/commands/mod.rs`、现有 `remote_control.rs` 或 `lib.rs`。
- **协议/数据**：快照绑定 connection/lease epoch/generation，字段 allowlist；不含绝对路径、Skill 正文、环境变量、工具凭证。
- **测试**：能力 command 的字段 allowlist 与序列化 contract、普通/代码模式显隐、未授权工作区、策略变更、快照 revision、过期/跨租约、凭证/路径扫描、缺失能力安全降级；中央 module/`lib.rs` 保持未改。
- **验收**：独立 command 模块可由 DEV-20 直接注册；A 不能构造未声明 mode 或路径；B 既有权限策略仍是执行上限；快照正文符合最小化清单且不含 Skill 私有内容。
- **风险**：把展示能力误当授权。任务接纳时 B 必须重新校验，不能只信快照。
- **需求映射**：RC-B-011、RC-T-009～011、RC-C-001～004、RC-C-011。
- **建议提交**：`feat(remote-control): 发布受控能力快照`

### DEV-13：B 权威 Session、远程任务 ledger 与共享准入核心

- **依赖**：DEV-12、DEV-26、DEV-27。
- **目标**：冻结可独立验证的 B 权威 Session 映射、ledger、turn lock、执行适配和准入核心原语/API，保持 B Session 唯一权威并防止重试重复副作用。
- **范围**：冻结 B Session 映射的创建/查找/切换原语；`task_id + client_request_id` ledger；per-session turn lock；普通 EnginePool 与代码 AcpPool 执行适配；Tauri 无关 admission/dispatch/cancel/terminal API；远程来源标记；transport 断线/stop 的真实终态和结果未知处理；冻结模型、审批、附件、成果扩展 trait。
- **不做**：不接入 A 网络输入或 B `ChatView` 本地输入的具体生命周期，不负责“能力准备前创建/幂等复用映射”的用例编排；不实现临时模型、审批 Allow、附件和成果下载；不暴露计划/系统任务。
- **预计文件**：DEV-26 预留且由本任务独占的 `features/sessions/application/admission.rs`、`features/sessions/` 的 B Session 映射窄接口、assistant EnginePool/Codex AcpPool 执行适配和核心单测；DEV-15～18 只能实现已冻结扩展 trait，DEV-28 只能消费已冻结准入 API；不修改 `features/sessions/application/mod.rs`、`ports.rs`、准入核心之外的预留用例槽或中央 command 文件。
- **协议/数据**：任务绑定 connection/lease/epoch/generation/session；ledger 原子记录 admission/dispatch/terminal；只通过 `SessionStore` 写权威 transcript，A 的请求不是第二真相源。
- **重启装配约束**：生产使用 sessions 根目录下独立的 `_remote_control_tasks.json` 原子完整快照（最多 256 条、64 条活动记录、文件最多 2 MiB），内存淘汰与磁盘快照同步。装配公开前校验字段、摘要、请求指纹及 task/client/sequence/session 活锁唯一性，并一次原子关闭全部重启遗留任务；已派发但不能证明结果的任务报告 `result_unknown`。读取、校验或恢复写入失败时明确禁用桌面远控，不回退内存台账、不覆盖坏文件，既有 Legacy Web 仍可恢复。持久的旧租约标识只用于终态和幂等证明，不恢复认证状态、密钥或令牌，也不跨进程比较单调时间。
- **测试**：映射原语的创建/查找/切换、ledger 重复请求与结果丢失、并发 turn、普通/代码执行适配、取消、stop/disconnect、session 切换、计划任务隔离、迟到终态丢弃、B 重启收尾；仅通过冻结 API 的单测假实现验证，不接入 Command、`ControlledExecutionPort` 或 `ChatView`。
- **验收**：准入核心可在无 Tauri/网络/UI 环境下独立验收；相同 client request 最多执行一次；所有已接纳任务最终有 completed/failed/cancelled/interrupted 之一；B Session 重载后仍能证明终态。
- **风险**：跨 Engine/ACP 两套执行器终态不一致。统一 adapter 和 ledger terminal CAS，不从 UI 事件反推终态。
- **需求映射**：RC-T-001～002、RC-T-004～012、RC-S-001～002、RC-C-004；协作支撑 RC-T-003。
- **建议提交**：`feat(remote-control): 打通幂等远程任务`

### DEV-14：共享 transfer core、绑定与资源预算

- **依赖**：DEV-07、DEV-13。
- **目标**：在附件上传和成果下载前冻结共用的分块、摘要、背压、超时、绑定和清理接口，消除两个方向并行修改同一 transfer core 的冲突。
- **范围**：定义 `TransferContext`、方向化 handle、块序号/摘要、连接总预算、取消/超时、清理注册表和错误模型；创建 upload/download 扩展槽与测试 fixture，但不开放业务命令。
- **不做**：不读写真实附件/成果，不实现预览，不注册前端 API；不修改现有 Web handle 的 wire 类型。
- **句柄安全**：upload/download 使用各自前缀加随机 UUID v4，不使用进程序号；锁内最多尝试 4 次，冲突不覆盖既有传输，分配失败不占用预算。随机句柄不替代业务适配层的实时 lease/task/session 绑定校验，不保留未发布的旧计数句柄生成逻辑。
- **预计文件**：实现 DEV-07 预留的 `controller/transfer/mod.rs`，新增 `core.rs`、`limits.rs`、`upload.rs`、`download.rs` 可编译扩展槽。本任务独占 `mod/core/limits`；DEV-17 只编辑 `upload.rs`，DEV-18 只编辑 `download.rs`，两者不得回改共享 core，接口不足须先单独修订 DEV-14 契约。
- **协议/数据**：所有 handle 绑定 connection/lease epoch/task/session 并区分 upload/download；块和总内存限制不宽于现有 Web transfer；每次传输最多接收 4096 个新块（相同块重试不计数），使用连续索引的 32 字节摘要保存重试证据，每次传输摘要缓冲最多 128 KiB、4 个并发传输最多 512 KiB；发送方须选择可在块数预算内完成的块长（32 MiB 文件至少 8 KiB，默认 256 KiB），若按配置最大块长仍无法在预算内完成，则在 begin 分配资源前拒绝。超出块数返回 `transfer_chunk_budget`，不改变进度或 TTL，业务适配层按其他拒绝块相同规则终止并清理；敏感文件元数据只进 E2E 帧。
- **测试**：预算竞争、方向混用、跨 lease/task/session、块重复/乱序、摘要、取消/超时、清理幂等、Web handle 与 controller handle 类型隔离。
- **验收**：upload/download 可仅新增各自模块实现而不修改共享 core；两方向并发共享连接预算且不能相互消费 handle。
- **风险**：过早冻结遗漏需求。用 DEV-17/18 的 fixture 作为 contract consumer；接口变更必须回到独立 core 提交后再继续。
- **需求映射**：RC-F-002～003、RC-F-006、RC-T-008。
- **建议提交**：`feat(remote-control): 冻结远程传输核心契约`

### DEV-15：`temporary_model_package/v1` 构包、校验与全终态清理

- **依赖**：DEV-13。
- **目标**：实现 A “我的模型”的严格导出和 B 内存临时路由，覆盖视觉兜底、SSRF 防护、绑定校验和所有终态清理。
- **范围**：SavedModel→v1 映射；本机安全存储解析 API key；可导出筛选；`SensitiveString`；B 独立复算 wire API；URL/DNS/重定向/私网校验；主/视觉双凭证；任务内存上下文；失败不回退 B 模型；A、Relay、B 三端一致拒绝 `mode=code + controller_temporary`。
- **不做**：不发送完整 SavedModel、credential_ref、OAuth/Cookie/订阅态；不持久化或加入 B 模型列表；不支持 A loopback 模型；模型 Command 只提供模型描述/构包能力，不生成任务 ID、不写 outbox、不 dispatch 且不编排任务提交。
- **预计文件**：`controller/temporary_model.rs`、`app/commands/desktop_remote_model.rs`（均由本任务独占）、`platform/prefs/mod.rs` 的只读投影接口、assistant runtime model/bridge 的临时注入接口、Rust/JS 选择器测试；只实现 DEV-13 冻结的模型扩展 trait，不修改 DEV-13 所属 application 准入核心/扩展 trait 定义、`app/commands/mod.rs`、现有 `remote_control.rs` 或 `lib.rs`。
- **协议/数据**：完全按需求 7.6.1 allowlist；unknown field/enum/组合全拒绝；绑定固定为 connection_id + controller_lease_id + lease_epoch + task_id；总尺寸和每字段上限为协议常量。
- **测试**：SavedModel 每个字段映射/排除、builtin/coding_plan/OAuth 隐藏、credential_ref 本地解析不传、主模型图片能力、视觉 fallback 循环/缺凭证、SSRF/rebind/redirect、跨任务重放、`mode=code + controller_temporary` 在 A/Relay/B 各层的拒绝且不回退 B 模型、completed/failed/cancelled/interrupted/timeout/disconnect/stop 清理、哨兵扫描；单独调用模型 Command 不产生任务 ID、outbox 记录或网络 dispatch。
- **验收**：B 的配置、Session、模型列表、Relay、日志和错误中均搜不到哨兵 key；临时模型失败不会调用 B 模型；所有终态释放敏感缓冲。
- **风险**：现有 SavedModel 兼容反序列化会兜底未知值。远程包使用独立严格类型，不直接 Deserialize 为 SavedModel。
- **需求映射**：RC-B-010、RC-M-001～015、RC-T-008。
- **建议提交**：`feat(remote-control): 支持任务级临时模型`

### DEV-16：A 唯一审批权威与 dispatch 竞态边界

- **依赖**：DEV-13。
- **目标**：把远程来源权限请求投影给 A，由 A 独占决定；B 只能查看、停止或断开，并以原子 dispatch 边界报告真实结果。
- **范围**：审批 binding/expiry、风险预览与脱敏、pending store、首个 A 决定 CAS、同值幂等、冲突/迟到错误、A 断线/lease 变化/任务取消默认拒绝、B stop 与 Allow 竞态、ACP permission/elicitation adapter。
- **不做**：不允许永久授权、远程超级权限或 B Allow；不削弱 B 原有策略/工作区上限。
- **预计文件**：`controller/permission.rs`、`app/commands/desktop_remote_permission.rs`（均由本任务独占）、`features/codex_acp/` 的窄适配接口、前端审批状态 fixture、Rust 并发测试；只实现 DEV-13 冻结的审批扩展 trait，不修改 DEV-13 所属 application 准入核心/扩展 trait 定义、`app/commands/mod.rs`、现有 `remote_control.rs` 或 `lib.rs`。
- **协议/数据**：绑定 connection/lease/epoch/task/permission request/expires；决定终态与 operation dispatch 在同一受控 CAS 流程中；敏感参数只返回安全预览。
- **测试**：allow/deny/timeout、同/异值并发、迟到/重放、跨 lease、B 伪造 Allow、A 断线、B stop 在 dispatch 前后、越界路径/策略拒绝、凭证脱敏。
- **验收**：没有来自 B UI 或 controller 之外的远程 Allow 入口；一次请求最多一次 dispatch；结果未知时不伪报拒绝或未执行。
- **风险**：现有 Web ACP respond 命令成为绕过。远程 task source 必须在 AcpPool adapter 层拒绝非 A-controller authority。
- **需求映射**：RC-B-012、RC-C-004～014、RC-S-002。
- **建议提交**：`feat(remote-control): 收口远程审批权威`

### DEV-17：A→B 端到端附件上传与 Session 隔离

- **依赖**：DEV-14。
- **目标**：复用现有附件类型/大小/摘要/清理约束，在 controller 密文通道内把 A 文件安全写入 B 当前 Session 隔离区。
- **范围**：upload ID、分块、摘要、断点状态、connection/lease/task/session 绑定、背压、超时/取消/断线清理、普通与 ACP 附件 staging。
- **不做**：不接受任意 B 路径，不放宽现有 Web 附件上限，不让 Relay 缓存明文，不将上传缓存持久化。
- **预计文件**：`controller/transfer/upload.rs`、`app/commands/desktop_remote_attachment.rs`（均由本任务独占）、`features/files/attachment_upload.rs`、assistant/Codex attachment adapter 和 fixture；只实现 DEV-13 冻结的附件扩展 trait，不修改 DEV-13 所属 application 准入核心/扩展 trait 定义、DEV-14 的 `core/limits/download`、中央 command/`lib.rs`。
- **协议/数据**：元数据和块均在 E2E 帧内；摘要/长度在接纳前校验；成功 handle 只能由同任务消费一次。
- **本地写入约束**：复用 `platform::filesystem::PrivateFileDirectory` 固定目录对象，先预留有界槽位再分配临时资源；暂存采用独占创建、删除随句柄关闭的匿名文件，分块只写持有的文件句柄，不重开可替换路径。提交从同一文件读取并复核长度和摘要，在同一锚定目录原子发布；提交缓冲最多 32 MiB。清理仅操作已知文件和空子目录，不递归跟随链接；清理失败保留明确错误和重试索引，不声称已清理。任务终态/超时和跨重启清扫的接线仍须分别验收。
- **测试**：文本/图片/允许文件、块乱序/重复/篡改、超限、连接内存上限、取消/超时/disconnect/stop 清理、跨 lease/task/session handle、普通/代码任务。
- **发布与消费约束**：Windows 发布凭据必须持有目录整链与只读文件租约；写句柄原子发布后，先在只读句柄下重新认证原对象 ID、单链接、长度和完整内容，再公开凭据。外部解析器及后续图片读取只能读取该受保护对象，租约贯穿执行；本地控制附件复用同一发布原语。撤销先移除凭据，再等待执行持有者释放并重试清理；删除句柄须重新核对原对象身份，只删除该对象与空目录。其他平台在资源分配前返回 unsupported，Legacy Web 通用摄入流程不变。
- **失败发布清理**：原子发布后任一步骤失败均不得授予读取凭据。优先用仍持有的删除句柄或锚定目录与原对象身份精确清理；失败则保留独立清理权交由上传回收器重试，清理成功后释放槽位，绝不删除替换对象。本地提前失败没有上传回收器时立即重试并记录固定非敏感诊断，跨重启残留按已登记的清扫事项处理。
- **执行生命周期**：Engine 提交成功不等于任务结束；外层 turn port 必须持续持有附件读取权，覆盖终态等待、取消/错误、技能恢复及模型释放，防止断线撤销上传凭据时提前释放大文本分页读取所需的路径。
- **验收**：Relay 无法读取文件名/内容；B 只在目标 Session 隔离目录得到完整文件；失败后有界时间内无孤儿缓存。
- **风险**：复用 Web cache 时身份域混淆。共享底层 chunk primitive，但 WebHandle 与 ControllerHandle 使用不同类型/前缀和校验入口。
- **需求映射**：RC-F-001～004、RC-T-008、RC-S-002。
- **建议提交**：`feat(remote-control): 支持端到端附件上传`

### DEV-18：B→A 成果索引、预览与下载副本

- **依赖**：DEV-14。
- **目标**：让 A 通过 opaque artifact ID 查看 B Session 成果列表、预览受支持类型并下载本地副本，不暴露 B 路径。
- **范围**：artifact index snapshot/delta、ID→Session 内安全解析、分块下载/摘要、预览能力和大小判定、下载目标选择、本地副本元数据、终态与 lease 校验。
- **不做**：不支持路径直读，不在 A 历史保存 B 绝对路径，不截断后宣称完整预览；首版不远程编辑 B 成果。
- **预计文件**：`controller/transfer/download.rs`、`app/commands/desktop_remote_artifact.rs`（均由本任务独占）、`file_access.rs` 的 opaque-ID 接口、`src/features/artifacts/` 的复用接口与测试；只实现 DEV-13 冻结的成果扩展 trait，不修改 DEV-13 所属 application 准入核心/扩展 trait 定义、DEV-14 的 `core/limits/upload`、中央 command/`lib.rs`。
- **协议/数据**：artifact ID 绑定 connection/lease epoch/B session；A 本地只存名称、类型、大小、摘要、终态和已下载副本路径。
- **测试**：列表、文本/图片/可视化预览、大文件 download-only、摘要失败、跨 session/lease ID、删除竞态、断线续传拒绝或明确重启、路径泄露扫描。
- **验收**：只凭 B 路径无法访问成果；下载副本摘要一致；不支持/过大类型显示明确下载提示。
- **风险**：basename 或旧路径 fallback 导致跨 Session 串读。controller 路径禁止 fallback，只接受权威 artifact ID 映射。
- **需求映射**：RC-F-005～008、RC-S-002、RC-S-007。
- **建议提交**：`feat(remote-control): 支持远程成果交付`

### DEV-19：权威事件 journal、快照恢复与 SessionStore RemoteProxy

- **依赖**：DEV-13、DEV-16、DEV-17、DEV-18。
- **目标**：同步消息、流式输出、思考、工具、审批、错误、取消、附件和成果状态，并让 A 建立可离线查看、与 B 删除语义解耦的独立副本。
- **范围**：per-lease/session event epoch+sequence、bounded journal、ACK/replay、gap→snapshot reset、事件去重、终态 CAS、认证后写入、SessionStore RemoteProxy 的版本化投影/精确绑定/原子 outbox、软删除、敏感字段清洗。
- **不做**：不让 A 离线副本成为执行真相源，不把 B Session JSON 原样复制，不同步 API key/绝对路径/工具秘密，不建立独立 history store。B journal 仅为传输/审计与同步游标，不作为第二历史。
- **预计文件**：`controller/journal.rs`、`app/commands/desktop_remote_journal.rs`、`features/sessions/remote_proxy/`、`features/sessions/application/project_remote.rs`、`app/commands/desktop_remote_proxy.rs`、host projection adapter、同步单测与故障注入 fixture；最终组合归 DEV-20。
- **协议/数据**：snapshot 有 base epoch/sequence 和内容摘要；journal 不覆盖 gap 时 A 原子丢弃不完整增量再换快照；历史记录明确 `RemoteProxy` kind 和最后已认证终态，live credential 永不保存。打开 live proxy 必须在完整认证绑定的原子检查内 prepare/commit，不以 ID 前缀或 JS state 判权威。
- **测试**：重复/乱序/丢失、journal 溢出、快照期间新事件、重连 replay、跨 epoch 迟到、各类事件、A/B 独立删除、离线重启、损坏 history、凭证/路径哨兵扫描。
- **验收**：A 与 B 在连接态最终收敛；重复包不重复消息/终态；离线历史明确为副本，且 A 删除不触发 B Session 操作。
- **风险**：快照与增量竞态。使用 snapshot base sequence + 原子 replace，再接收大于 base 的事件。
- **需求映射**：RC-A-008、RC-F-007、RC-S-001～009、RC-T-002、RC-T-005、RC-T-015、RC-T-024；协作支撑 RC-S-010～011。
- **建议提交**：`feat(remote-control): 同步远程会话并保存副本`

### DEV-20：前端桥、独立状态域与迟到结果防护

- **依赖**：DEV-10、DEV-11、DEV-12、DEV-15、DEV-19。
- **目标**：由单一整合任务注册全部 desktop Tauri commands，把 B、A、能力快照、临时模型和远程历史 API 接入前端，同时保持现有 `webAccess` bridge/状态/策略不变。
- **范围**：在 `lib.rs` 单一 composition root 组合并注册已经实现的 commands 与 execution/projection/dispatch ports；durable ledger 和必要 owner 初始化成功后才公开，失败仅禁用桌面远控，Legacy Web 在失败域外。建立 `controlledDevice`/`controllerConnection`/`remoteCapabilities`/`remoteModels` 展示状态及 `remoteProxyHistories`/`remoteProxyHistory` 的只读投影；冻结“按当前 connection/lease/revision 读取能力快照”的 bridge contract；监听权威 status/RemoteProxy 通知；intent sequence + generation/lease epoch 门；窗口隐藏与 transport 连接解耦。UI 只提交模式、模型选择及用户输入 intent；Rust 生成任务/request 身份、冻结快照、解析凭据并构包，通过 SessionStore 原子 outbox 完成 reserve/action/commit，后续重试和取消仅用权威 handle。
- **不做**：不在本任务完成视觉 UI；不让 JS 接触 API key、credential_ref 或完整 `SavedModel`；不在 bridge 中构造模型包；不把 desktop command 加进 Web `access-policy.json`；Web build 不获得桌面敏感 API。
- **预计文件**：`pinvou3-app/src-tauri/src/lib.rs`（本功能中央 command 注册的唯一 owner）、`app/desktop_remote_composition.rs`、`app/remote_projection_adapter.rs`、`app/commands/mod.rs` 及 desktop command 模块的串行整合、`src/platform/tauri/bridge/desktop-remote-control.js`、`src/platform/tauri/bridge.js`、`src/features/remote-control/remote-control-state.js`、`src/shared/platform.js`、任务专属 bridge contract/race 测试；各 owner 经窄接口整合，不新增第二套历史或执行状态。
- **协议/数据**：能力快照前端投影只含 DEV-12 allowlist，并绑定当前 connection/lease/revision，旧 revision 原子丢弃；前端模型列表只含 opaque model ID、显示名、能力和不可导出原因；UI 提交 model source/ID、模式与用户输入 intent，不生成 wire task/request ID；Rust 在当前认证绑定内生成身份、冻结快照、解析凭据、构包并经原子 outbox 发送，任一步失败都不发送任务且不回退 B 模型；secret/receipt/token/model key 永不出 Rust；所有异步完成写入校验当前 intent/generation/lease。
- **测试**：start/stop/refresh/claim/disconnect 交错、旧事件/旧快照、关闭窗口、平台 capability；能力快照读取、字段 allowlist、跨 lease/revision 丢弃、Skill/工具凭证不进 JS；可导出/不可导出模型投影、选择 model ID、构包失败不提交、构包与 Rust 生成身份原子绑定、重试使用同一权威 handle、JS state/event/error 无 key/credential_ref；实际 desktop command 清单各注册一次，durable 初始化失败时不公开半成品 owner；Web bridge 无新命令；bridge protocol hash/contract。
- **验收**：迟到回包不能复活旧码、旧连接、旧能力或旧任务；DEV-23 只消费稳定 bridge API 即可读取能力快照、完成模型选择和任务提交，不读取 prefs/Skill 私有文件、不构包、不调用底层 Tauri；WebUI v2 build/fixture 不变；非 Windows capability 为 false。
- **风险**：现有 `remoteControl` 名称已指 WebAccess。新域采用不歧义命名并提供一次内部迁移，不能偷换旧 API 语义。
- **需求映射**：RC-B-001、RC-B-005、RC-B-013～014、RC-A-004～006、RC-A-009、RC-T-005、RC-T-008、RC-M-001～006、RC-M-009、RC-S-003～006；协作支撑 RC-T-003。
- **建议提交**：`feat(remote-control): 接入桌面双端状态桥`

### DEV-21：Windows B 被控端面板、三语与可访问性

- **依赖**：DEV-10、DEV-15、DEV-16、DEV-20、DEV-31。
- **目标**：独立交付 B 的桌面远程控制面板，不与现有 Web 分享设置混淆。
- **范围**：开关、7 位码、秒级倒计时、刷新、全状态、主控设备/连接时间、模型来源、预授权工作区、立即断开、只读审批与停止任务；中文/英文/日文；键盘、焦点、读屏、对比度和 reduced-motion。
- **不做**：不提供 Allow/Approve，不显示 key，不改 A 连接/会话 UI，不替换 `SettingsView` 的 Web 分享卡片。
- **预计文件**：`src/features/remote-control/ControlledDevicePanel.jsx`（新）、B 面板子组件/状态测试、`src/shared/i18n/{zh,en,ja}.js` 的 `controlledDevice` 命名空间、`src/app/main.jsx`/`SettingsView.jsx` 入口挂载。本任务是三个 UI 任务中首个中央 UI/i18n owner；DEV-22/23 通过依赖串行追加各自命名空间，不与本任务并行编辑。
- **协议/数据**：码和倒计时只存在内存展示状态；状态由后端显式枚举映射；stop/refresh 交互保留 generation 防迟到写入。
- **测试**：三语 key、无单语硬编码、倒计时 ARIA 节流、键盘操作、焦点恢复、高对比、B 无 Allow、刷新/停止竞态、非 Windows 隐藏。
- **验收**：B 用户可完成开启、刷新、查看、停止和立即断开；Web 分享功能仍是独立入口；安全状态不只依赖颜色。
- **风险**：Settings 页概念混淆。使用独立标题、说明和状态域，不复用 Web 二维码/分享文案。
- **需求映射**：RC-B-001～012、RC-B-013～014、RC-C-010、RC-M-001～002。
- **建议提交**：`feat(remote-control): 完成Windows被控端面板`

### DEV-22：Windows A 连接壳、短期码提示与后台窗口语义

- **依赖**：DEV-11、DEV-20、DEV-21。
- **目标**：独立交付 A 的“连接远程设备”入口、连接状态壳和后台连接/显式断开语义。
- **范围**：7 位码输入、粘贴/大写/分隔符规范化、连接中/connected/offline/busy/统一无效码/版本错误、短期 bearer secret 提示、远程设备标识、关闭窗口与断开设备分离；三语和可访问性。
- **不做**：不实现消息、模型、审批、附件或成果能力；不保存连接码或长期设备凭证。
- **预计文件**：`src/features/remote-control/RemoteConnectForm.jsx`、`RemoteConnectionShell.jsx`、`src/shared/i18n/{zh,en,ja}.js` 的 `controllerConnection` 命名空间、`src/app/main.jsx` 路由和 smoke/a11y 测试；只在 DEV-21 合入后串行编辑中央 UI/i18n 文件。
- **协议/数据**：仅本地格式错误给具体提示；所有服务端失效原因统一文案；关闭窗口只改变可见性，显式断开才调用 transport disconnect。
- **测试**：输入法/粘贴/键盘、错误映射、统一码文案、窗口关闭后台存活、显式断开、焦点陷阱/恢复、读屏状态、三语、非 Windows 隐藏。
- **验收**：A 可无 B 二次确认建立连接；关闭窗口不终止任务；断开后壳层不可发送/审批且明确安全收尾。
- **风险**：用户长期保存短码。入口和错误恢复都提示向 B 获取当前码，不展示最近码历史。
- **需求映射**：RC-A-001～007、RC-A-009～010。
- **建议提交**：`feat(remote-control): 完成Windows主控连接界面`

### DEV-23：Windows 远程会话 ChatView 能力、三语与可访问性

- **依赖**：DEV-13、DEV-15、DEV-16、DEV-17、DEV-18、DEV-19、DEV-20、DEV-22。
- **目标**：A 连接壳只控制连接；在现有 ChatView 内交付远程消息、模型、能力、审批、附件、成果与 SessionStore RemoteProxy 离线副本的完整 UI。
- **范围**：远程 Session 创建/切换、显著远程标识、通过 DEV-20 bridge contract 完成 B/我的模型选择与原子任务提交、模式/能力快照、流式/工具/错误/取消、A 审批、附件上传、成果预览/下载、离线副本；三语和键盘/读屏/焦点/高对比测试。
- **不做**：不复制整套 ChatView 状态机，不显示 B 路径/Key，不允许 B Allow，不把 A 历史误标为在线 Session。
- **预计文件**：现有 `src/features/chat/ChatView.jsx` 与 `src/features/remote-control/` 内的 model/permission/transfer/RemoteProxy adapter、`src/shared/i18n/{zh,en,ja}.js` 的 `remoteConversation` 命名空间、必要的 `src/app/main.jsx` 会话路由、UI smoke/a11y 测试；不复制 ChatView 状态机，依赖 DEV-22 后串行编辑中央 UI/i18n 文件。
- **协议/数据**：所有动作经 DEV-20 bridge；渲染事件需匹配当前 connection/lease/session；download-only、result unknown、offline copy 等状态使用明确文案。
- **测试**：三语全旅程、仅通过 DEV-20 API 完成模型列表/选择/提交且组件源码不读取 prefs/credential/构包字段、审批 allow/deny/timeout、附件/成果、journal reset、重复终态、离线历史、窗口隐藏、键盘与读屏、缩放/高对比。
- **验收**：用户无需开发工具完成基础与完整能力旅程；远程/本地上下文始终可辨；所有安全失败均有可恢复提示。
- **风险**：复用现有组件时耦合本地 bridge。抽取纯展示组件，业务调用保持 remote-control domain API。
- **需求映射**：RC-A-005、RC-A-008、RC-M-001～003、RC-C-002、RC-C-005～010、RC-F-001、RC-F-005、RC-F-008、RC-S-002、RC-S-009。
- **建议提交**：`feat(remote-control): 完成远程会话能力界面`

### DEV-24：遥测、审计、安全扫描与运维指标

- **依赖**：DEV-03、DEV-04、DEV-06、DEV-08、DEV-09、DEV-10、DEV-11、DEV-13、DEV-15、DEV-16、DEV-17、DEV-18、DEV-19、DEV-20、DEV-23（可在前置任务中增量埋点，最终由本任务统一 schema 与门禁）。
- **目标**：复用现有行为遥测和 Relay 运维面，增加最小化远控漏斗、爆破/容量/恢复告警与凭证泄露门禁。
- **范围**：受控事件枚举、客户端去重、Relay 聚合计数、配对/租约/恢复/任务/审批/传输成功率和延迟桶、限流告警、结构化 audit、日志 redaction、持久文件/抓包/崩溃信息哨兵扫描。
- **不做**：不记录码、endpoint/lease/token/key、IP、设备名、prompt/回复/工具参数、文件名/路径/内容；不新建第二套 telemetry 管线。
- **预计文件**：`behavior-telemetry-schema.js`、`telemetry-service.js`、Relay audit helper、`server.js` 的串行审计接线（在 DEV-06 后）、app behavior telemetry registry/schema、对应测试和运维说明；不改 Relay handler/command 注册拓扑。
- **协议/数据**：只允许需求第 16 节事件和受控原因枚举；身份沿用现有匿名主体；高基数字段在 schema 层拒绝而非调用方自觉省略。
- **测试**：schema 正反例、去重、未知枚举、敏感 key/路径/prompt 注入、日志/持久文件扫描、爆破/容量/恢复告警、stats 权限与现有指标回归、抓包确认正文密文。
- **验收**：能定位配对、恢复和任务失败阶段而不能还原用户/设备/内容；哨兵值在允许的内存敏感路径之外零命中。
- **风险**：调试字段意外形成标识符。生产审计使用固定 allowlist，禁止任意 `patch` 携带用户值。
- **需求映射**：RC-P-006、RC-P-010～011、RC-M-007、RC-M-012、RC-C-008、RC-F-007、RC-S-007，以及需求第 16 节全部边界。
- **建议提交**：`feat(remote-control): 增加安全遥测与审计`

### DEV-25：公网部署迁移、灰度、真实双机 E2E 与发布门禁

- **依赖**：DEV-02、DEV-21、DEV-22、DEV-23、DEV-24、DEV-26～DEV-31，以及其余 P0 任务完成。
- **目标**：在两台隔离 Windows 设备和生产等价 Relay 拓扑上完成需求第 18.3 节全部旅程，并把 enrollment、回滚、安全和兼容检查固化到 CI/发布流程。
- **范围**：feature flag；registry 覆盖率灰度；自托管升级说明；容量/限流/故障注入；真实旧 Relay + gateway；旧 Web fixture；双机安装包 E2E；密钥/凭证扫描；监控阈值、回滚 runbook 和证据归档。
- **不做**：不绕过失败用例上线；不把 mock Relay 结果替代真实双机/真实旧二进制；不宣称抵御恶意 Relay 主动 MITM。
- **预计文件**：部署/迁移文档、CI workflow/测试脚本、`remote-control-relay/test/` E2E harness、`pinvou3-app/tests/` Windows journey、feature flag/config 和发布 runbook。
- **协议/数据**：灰度关闭只禁用 desktop controller 能力，不删除 registry/tombstone；最低回滚版本必须 registry-aware，或所有入口均在同 registry gateway 后且不可旁路。
- **测试**：逐条执行需求 18.2/18.3；特别覆盖两 A、Web/controller 交叉、A/B 单边网络隔离、30 秒预算、Relay 重启、refresh generation、A/B 模型、审批竞态、附件/成果、历史、digest key 轮换碰撞、旧 Web 重连 jitter、裸 v2 回滚拒绝。
- **验收**：所有 P0 验收、公共门禁和安全扫描通过；监控可见且回滚演练成功；风险台账明确保留“受信 Relay，不抵御恶意主动 MITM”。
- **风险**：测试环境时钟/网络不稳定造成假通过。使用单调时钟、可控网络故障注入和服务端时间断言，并保留可复现 trace（不含敏感正文）。
- **需求映射**：全部 RC-B/RC-A/RC-P/RC-L/RC-T/RC-M/RC-C/RC-F/RC-S，需求第 14～20 节。
- **建议提交**：`test(remote-control): 固化公网双机发布门禁`

<details>
<summary>DEV-26～DEV-31 原始设计任务（历史快照，非当前兼容、导出或迁移要求）</summary>

以下保留原设计追溯；旧六槽、独立历史迁移及稳定版本保留窗口不再执行。当前五导出、无兼容债和已落实的单一供数边界以文档顶部及《代码架构》的当前契约为准。

### DEV-26：Tauri 无关用例边界、远控窄端口与架构守卫

- **依赖**：DEV-07、DEV-12。
- **目标**：冻结 Command、共享用例、会话状态与远控领域的依赖方向，为后续任务提供可编译、可枚举的边界。
- **范围**：定义 `features/remote_control` 所有的 `ControlledExecutionPort` 和 `RemoteSessionProjectionPort`；在 `features/sessions/application/` 一次性创建并导出 `admission`、`sync_remote_journal`、`submit_controlled`、`dispatch_remote`、`project_remote` 可编译模块槽和必要空契约，冻结用例错误与宿主窄端口形状；扩展架构守卫，禁止后续任务修改 application 中央 export。
- **不做**：不实现具体任务提交、投影或 Tauri 注册；不新增 `ConversationRuntime`、统一 `features/conversation` 或三后端共同基类；不迁移无关 Command。
- **预计文件**：`features/remote_control/controller/ports.rs`、`features/sessions/application/{mod.rs,ports.rs,admission.rs,sync_remote_journal.rs,submit_controlled.rs,dispatch_remote.rs,project_remote.rs}`、`scripts/architecture-guard.py`、`docs/architecture-guard.md` 及架构契约测试；DEV-26 独占 `mod.rs`/`ports.rs` 和中央 export，其余文件仅提交可编译槽/空契约供后续 owner 填充。
- **测试**：守卫可枚举 `features/*/application/**/*.rs`；五个预声明用例模块可编译且从 `application/mod.rs` 导出；模拟 DEV-28/DEV-29 分别只编辑 `submit_controlled.rs`/`dispatch_remote.rs` 时无共享文件 diff；拒绝用例签名或实现引入/调用 Tauri；拒绝 `remote_control → app/commands`、Tauri 事件 API 或 feature 环；允许组合根注入窄端口。
- **验收**：两个端口 trait 可由假实现独立测试；不含 Tauri 的 Rust 测试可直接调用 application 边界；DEV-13/19/28/29/30 可各自只填充预留实现文件而不修改 `mod.rs`、`ports.rs` 或中央 export，其中 DEV-28 与 DEV-29 可无文件争用地并行；既有 Command 行为未变。
- **风险**：用例边界被误做成新的中央 Runtime。边界保持无状态且按职责归属 feature，由架构守卫禁止第二状态所有者。
- **需求映射**：RC-T-018、RC-T-023。
- **建议提交**：`refactor(remote-control): 建立Tauri无关用例边界`

### DEV-27：SessionStore RemoteProxy schema、可信路由与 live binding

- **依赖**：DEV-11、DEV-26。
- **目标**：把 A 远程会话建模为 `SessionStore` 中的版本化 `RemoteProxy`，并以可信 Rust 绑定决定三种会话路由。
- **范围**：`RemoteProxy` schema/migration；A 本地 Session ID、opaque `remote_session_ref`、B 映射确认、connection/lease/generation 绑定；本地 UI 元数据、已认证投影游标和有界 pending 槽位；基于 session kind/backend binding 的路由。
- **不做**：不实现业务事件投影或网络 dispatch；不将磁盘历史映射当作访问凭据；`RemoteProxy` 不进入 A 本机 `EnginePool`/`AcpPool`。
- **预计文件**：`features/sessions/remote_proxy/{schema,binding,routing,migration}.rs`、`SessionStore` 类型扩展和 migration/contract 测试。
- **测试**：创建/幂等复用、引用伪造/篡改、跨 session/lease/generation 重放、断线只读、重新配对不复用旧 ref、三种 session kind 精确路由、`RemoteProxy` 本机 Engine/ACP 零调用。
- **验收**：前端无法指定后端路径；离线可打开投影但所有变更操作失败关闭；`SessionStore` 之外不产生第二份会话状态。
- **风险**：持久的 B Session 映射被误当成 live 访问凭据。变更操作始终重新校验当前 connection/lease/generation 绑定。
- **需求映射**：RC-T-019～022；协作支撑 RC-S-010。
- **建议提交**：`feat(sessions): 增加RemoteProxy会话与可信路由`

### DEV-28：B 双端输入、唯一权威 Session 与统一准入

- **依赖**：DEV-13、DEV-27。
- **目标**：仅基于 DEV-13 冻结的准入 API，把 A 远程输入和 B `ChatView` 本地输入接入同一 B 权威 Session、turn lock、ledger、执行器和 journal。
- **范围**：在任一来源进入能力准备前，通过 DEV-13 冻结原语幂等创建或复用 B Session 映射；接入 A 网络输入和 B `ChatView` 本地受控输入的完整用例生命周期；仅为 B 本地输入由 B 可信后端生成 `task_id`、`client_request_id` 和输入摘要；A 网络输入必须验证并原样绑定 DEV-29/协议提供的稳定 ID，禁止重写，然后调用 DEV-13 ledger/准入 API；两种来源调用同一准入/取消/终态 API；忙碌时保留可重试输入。
- **不做**：不修改 DEV-13 准入核心、ledger、turn lock、执行适配或扩展 trait；不为 A 网络输入重新生成或改写 `task_id`/`client_request_id`，不用 B 本地 ID 覆盖 A 稳定 ID；不让 B 受控窗口退回普通本地聊天链路；不另建 Session、第二份 ledger 或远程专用执行器。
- **预计文件**：DEV-26 预留且由本任务独占的 `features/sessions/application/submit_controlled.rs`、B `ChatView` 后端输入适配、A 网络入口的 `ControlledExecutionPort` 适配器、映射生命周期与双来源接入测试；不编辑 DEV-13 拥有的 `admission.rs`、DEV-29 拥有的 `dispatch_remote.rs`、`application/mod.rs`、`ports.rs` 或中央 export。
- **测试**：A/B 分别首先发言且均在能力准备前创建/幂等复用映射、交替输入、同时输入、`task_session_busy`、取消/终态、同一 `task_id + B session_id` 关联、输入临时投影被 journal 去重替换；显式断言“A 的 `task_id`/`client_request_id` 验证后原样进入 DEV-13 ledger，B 的 ID/摘要由 B 可信后端生成”，伪造/缺失/改写 A ID 均失败关闭；假 DEV-13 API 断言两来源只调用冻结边界。
- **验收**：A/B 任意一端可首发且能力准备始终拿到同一幂等 B Session 映射；A 稳定 ID 未被改写地进入 DEV-13 ledger，B 本地 ID/摘要均由 B 可信后端生成；同一会话同时只有一个活动 turn；错误不依赖当前页面或最近会话猜测关联；DEV-13 准入核心文件无变更。
- **风险**：B UI 保留旧的普通本地发送入口导致双 Session。受控会话下由 Rust session kind 强制进入共享准入用例。
- **需求映射**：RC-T-013～017；协作支撑 RC-T-003。
- **建议提交**：`feat(remote-control): 统一双端输入与任务准入`

### DEV-29：A 任务 prepare→persist→dispatch 原子发件箱

- **依赖**：DEV-13、DEV-15、DEV-27。
- **目标**：在 A 上建立可证明顺序的远程提交用例，防止未持久先发送、同 ID 异参重放和进程重启后误自动补发。
- **范围**：生成稳定 task/request ID、输入摘要和覆盖全部非敏感语义参数的规范指纹；task 薄 Command/远程提交用例调用 DEV-15 构包能力完成内存 prepare，但 DEV-15 模型 Command 不编排提交；先原子写入有界、不含密钥的 pending/outbox，持久成功后再经 `RemoteControl` 加密 dispatch；持久可证明的 dispatch-attempt 状态；不可持久的临时凭证/负载丢失时，仅当后端可证明从未尝试 dispatch 才原子废弃原 pending 并持久化原 ID 不可复用标记，之后只接受用户以全新 `task_id`/`client_request_id` 发起的新请求；是否尝试发送不确定时保持 `outcome_unknown`；原进程/同 live binding 内的同参重试和对账。
- **不做**：不持久化 API Key 或不可恢复负载；不在 A 重启或重新配对后自动发送、查询或对账旧请求；废弃原 pending 后不复用其 ID，`outcome_unknown` 不标记成功/失败、不自动重发且不通过换用户可见文案隐式重试。
- **预计文件**：DEV-26 预留且由本任务独占的 `features/sessions/application/dispatch_remote.rs`、`app/commands/desktop_remote_task.rs` 的薄适配、`features/sessions/remote_proxy/outbox.rs`、请求指纹/原 ID 不可复用标记编码和故障注入测试；不编辑 DEV-28 拥有的 `submit_controlled.rs`、`application/mod.rs`、`ports.rs` 或中央 export。
- **测试**：DEV-29 调用 DEV-15 构包能力且模型 Command 零提交编排；prepare 后持久失败零发送、持久后 dispatch 前失败、发送后 ACK 丢失、同 ID 同/异参、投影更新前崩溃、A 重启只恢复 `outcome_unknown`；临时凭证/负载丢失且后端证明未尝试 dispatch 时废弃 pending、原 ID 重试拒绝、全新双 ID 的用户新请求可接纳；dispatch-attempt 为 attempted/unknown 时均保持 `outcome_unknown`，断言不写 completed/failed、不自动重发；DEV-28/DEV-29 并行 diff 零共享 application/export 文件。
- **验收**：任一故障点都不会产生未登记发送或重复副作用；只有可证明从未尝试 dispatch 才可废弃原 pending，原 ID 永不复用且用户必须以全新双 ID 发起新请求；不能证明未发送时始终保持 `outcome_unknown`，不标记成功/失败且不自动重发。
- **风险**：为了重试而把临时模型凭证写入 outbox。持久字段使用严格 allowlist 和哨兵扫描；不可恢复负载丢失后以可证明 dispatch-attempt 状态分支，任何不确定都保持 `outcome_unknown`。
- **需求映射**：RC-T-003、RC-T-026、RC-S-011。
- **建议提交**：`feat(remote-control): 实现远程任务原子发件箱`

### DEV-30：类型化事件 envelope、RemoteSessionProjectionPort 与宿主事件适配

- **依赖**：DEV-28、DEV-29。
- **目标**：让纯连控事件、B 权威会话事件和 A `RemoteProxy` 投影事件各自携带正确必填绑定，并以无 Tauri 反向依赖的方式通知 UI。
- **范围**：按事件类型定义 envelope；接收端本地 Session ID、remote ref、B Session ID、connection/lease/generation、task/journal/capability 字段规则；迟到过滤；`RemoteSessionProjectionPort` 实现；组合根注入的 `AppEventBus` 窄端口。
- **不做**：纯连控事件不伪造 Session 引用；远控 feature 不直接调用 Tauri emit 或 `app/commands`；不根据当前页面猜测事件归属。
- **预计文件**：`controller/event_envelope.rs`、`features/sessions/remote_proxy/projection.rs`、DEV-26 预留且由本任务独占的 `features/sessions/application/project_remote.rs`、`AppEventBus` 端口及契约测试；不修改 `application/mod.rs`、`ports.rs` 或中央 export，`lib.rs` 实际注入归 DEV-20。
- **测试**：每类 envelope 必填/禁止字段、伪造/篡改/跨绑定/迟到拒绝、已认证事件只投影一次、pending 对账、宿主通知失败不回滚权威投影、无 `remote_control → app/Tauri` 依赖。
- **验收**：必填绑定缺失或不匹配时不更新状态/UI；投影成功后才通知 UI；同一权威事件不产生双写。
- **风险**：使用包含大量可选字段的通用 envelope 弱化绑定校验。事件使用带类型变体和逐类型构造器，不允许调用方自由填组合字段。
- **需求映射**：RC-T-015、RC-T-023～024；协作支撑 RC-S-010～011。
- **建议提交**：`feat(remote-control): 收口类型化事件投影`

### DEV-31：旧 Command/Bridge/历史原子迁移与统一 ChatView 收口

- **依赖**：DEV-19、DEV-20、DEV-27、DEV-30。
- **目标**：在不改变已验证 Command 公开契约的前提下，按需求第 15 节完成旧远程 API、独立历史与 UI 供数的可恢复原子迁移，收敛到唯一用例链路和 `SessionStore.RemoteProxy`。
- **范围**：为每条旧会话/事件保存稳定源记录 ID 与去重键，持久化 schema/migration revision；先在 `SessionStore` 构建并校验完整 `RemoteProxy` 基线，记录稳定 high-water cursor；切换时在单一写入门下冻结旧写入，补齐并校验最终 cursor，再以一个原子事务同时切换写入目标、供数指针和 migration revision；旧 Command helper 改为单次代理且停止新增业务，旧 Command/Bridge 适配器也代理读取 `SessionStore`；现有 `ChatView` 的远程 session adapter；旧源至少保留一个稳定版本的只读验证/回滚窗口。
- **不做**：不修改 DEV-27 拥有的 `RemoteProxy` schema/version migration；不复制 `RemoteConversationView` 状态机；不在新旧链路同时写入或并行供数；不在本任务或只读窗口完成前删除旧源，删除必须由后续显式、可审计任务完成；不借迁移改名或重写无关 Command。
- **预计文件**：旧 desktop remote Command/Bridge 适配器、DEV-31 独占的 `features/sessions/remote_proxy/legacy_remote_history_migration.rs`、`src/features/remote-control/remote-session-adapter.js`、现有 `ChatView` 接入点、单一写入门/供数指针与迁移/回滚测试；仅通过 DEV-27 公开 API 写入 `RemoteProxy`。
- **协议/数据**：同去重键同内容幂等复用，同键不同内容失败关闭并保留源数据；切换前只写/只从旧源供数，切换后只写/只从 `SessionStore` 供数；中断后按 revision 幂等继续，原子切换未发生时可安全废弃未完成基线；切换后功能开关回退只切换已验证适配器，不恢复旧源供数或写入。
- **测试**：稳定源记录 ID/去重键、schema/migration revision、high-water 与最终 cursor；同键同内容重复和同键异内容失败关闭；基线构建、冻结旧写入、最终补齐和单事务三指针切换的每个故障点/崩溃重启；迁移前/中/切换后/回退始终单一写入与供数源；旧源跨一个稳定版本保持只读；旧适配器只调用一次新用例；本地/远程切换不串 Session、事件、附件或权限状态。
- **验收**：原子事务同时切换写入目标、供数指针和 migration revision，任一故障不会形成双写或双供数；工作区只剩一条远程正文业务链路和一个 UI 会话状态机；旧源在至少一个稳定版本及验证/回滚窗口内持续只读可恢复，本任务不删除。
- **风险**：回滚功能开关同时恢复旧写入而形成双权威。回滚只切换读适配与新入口可见性，旧源一旦迁移即保持只读。
- **需求映射**：RC-T-018、RC-T-022、RC-T-025、RC-S-010。
- **建议提交**：`refactor(remote-control): 迁移远程会话到统一ChatView`

</details>

## 6. 需求 ID 覆盖矩阵

下表逐项指定唯一的主要实现 owner；括号内任务是该需求不可缺少、但不取代 owner 的前置实现。DEV-25 负责所有项目的跨域/公网回归，涉及敏感数据的条目同时由 DEV-24 做泄露门禁。

| 需求 ID | 主要实现 owner | 必要协作或回归 |
|---|---|---|
| RC-B-001 | DEV-21 | DEV-10、DEV-25 |
| RC-B-002 | DEV-21 | DEV-03、DEV-10、DEV-25 |
| RC-B-003 | DEV-10 | DEV-03、DEV-21、DEV-25 |
| RC-B-004 | DEV-10 | DEV-09、DEV-16、DEV-21、DEV-25 |
| RC-B-005 | DEV-10 | DEV-08、DEV-09、DEV-21、DEV-25 |
| RC-B-006 | DEV-10 | DEV-21、DEV-24 |
| RC-B-007 | DEV-10 | DEV-21、DEV-25 |
| RC-B-008 | DEV-04 | DEV-10、DEV-25 |
| RC-B-009 | DEV-04 | DEV-10、DEV-25 |
| RC-B-010 | DEV-21 | DEV-15、DEV-24、DEV-25 |
| RC-B-011 | DEV-12 | DEV-10、DEV-21、DEV-25 |
| RC-B-012 | DEV-16 | DEV-10、DEV-21、DEV-25 |
| RC-B-013 | DEV-10 | DEV-07、DEV-20、DEV-25 |
| RC-B-014 | DEV-10 | DEV-03、DEV-06、DEV-20、DEV-25 |
| RC-B-015 | DEV-08 | DEV-10、DEV-25 |
| RC-B-016 | DEV-08 | DEV-10、DEV-25 |
| RC-B-017 | DEV-08 | DEV-07、DEV-25 |
| RC-B-018 | DEV-08 | DEV-05、DEV-07、DEV-25 |
| RC-A-001 | DEV-22 | DEV-11、DEV-25 |
| RC-A-002 | DEV-22 | DEV-00、DEV-25 |
| RC-A-003 | DEV-11 | DEV-06、DEV-22、DEV-25 |
| RC-A-004 | DEV-22 | DEV-00、DEV-11、DEV-25 |
| RC-A-005 | DEV-23 | DEV-20、DEV-25 |
| RC-A-006 | DEV-11 | DEV-22、DEV-25 |
| RC-A-007 | DEV-11 | DEV-19、DEV-24、DEV-25 |
| RC-A-008 | DEV-19 | DEV-23、DEV-25 |
| RC-A-009 | DEV-22 | DEV-11、DEV-13、DEV-25 |
| RC-A-010 | DEV-22 | DEV-03、DEV-25 |
| RC-P-001 | DEV-03 | DEV-00、DEV-25 |
| RC-P-002 | DEV-00 | DEV-03、DEV-21、DEV-22 |
| RC-P-003 | DEV-03 | DEV-10、DEV-11、DEV-25 |
| RC-P-004 | DEV-03 | DEV-21、DEV-25 |
| RC-P-005 | DEV-03 | DEV-24、DEV-25 |
| RC-P-006 | DEV-03 | DEV-24、DEV-25 |
| RC-P-007 | DEV-04 | DEV-25 |
| RC-P-008 | DEV-06 | DEV-03、DEV-04、DEV-25 |
| RC-P-009 | DEV-04 | DEV-00、DEV-22、DEV-25 |
| RC-P-010 | DEV-04 | DEV-24、DEV-25 |
| RC-P-011 | DEV-03 | DEV-21、DEV-22、DEV-24 |
| RC-P-012 | DEV-01 | DEV-03、DEV-25 |
| RC-P-013 | DEV-04 | DEV-06、DEV-25 |
| RC-P-014 | DEV-04 | DEV-03、DEV-06、DEV-10、DEV-25 |
| RC-P-015 | DEV-03 | DEV-25 |
| RC-P-016 | DEV-03 | DEV-10、DEV-25 |
| RC-P-017 | DEV-03 | DEV-24、DEV-25 |
| RC-P-018 | DEV-06 | DEV-05、DEV-25 |
| RC-P-019 | DEV-01 | DEV-02、DEV-25 |
| RC-P-020 | DEV-05 | DEV-06、DEV-25 |
| RC-P-021 | DEV-03 | DEV-25 |
| RC-P-022 | DEV-02 | DEV-01、DEV-25 |
| RC-P-023 | DEV-02 | DEV-01、DEV-25 |
| RC-P-024 | DEV-03 | DEV-25 |
| RC-L-001 | DEV-06 | DEV-25 |
| RC-L-002 | DEV-04 | DEV-06、DEV-25 |
| RC-L-003 | DEV-06 | DEV-05、DEV-07、DEV-25 |
| RC-L-004 | DEV-06 | DEV-07、DEV-13、DEV-16、DEV-19 |
| RC-L-005 | DEV-09 | DEV-11、DEV-25 |
| RC-L-006 | DEV-09 | DEV-10、DEV-25 |
| RC-L-007 | DEV-04 | DEV-25 |
| RC-L-008 | DEV-08 | DEV-09、DEV-10、DEV-25 |
| RC-L-009 | DEV-09 | DEV-05、DEV-25 |
| RC-L-010 | DEV-09 | DEV-08、DEV-25 |
| RC-L-011 | DEV-09 | DEV-25 |
| RC-L-012 | DEV-08 | DEV-09、DEV-10、DEV-25 |
| RC-L-013 | DEV-09 | DEV-07、DEV-08、DEV-25 |
| RC-L-014 | DEV-09 | DEV-08、DEV-25 |
| RC-L-015 | DEV-08 | DEV-07、DEV-25 |
| RC-L-016 | DEV-08 | DEV-25 |
| RC-T-001 | DEV-13 | DEV-11、DEV-23、DEV-25 |
| RC-T-002 | DEV-13 | DEV-19、DEV-25 |
| RC-T-003 | DEV-29 | DEV-00、DEV-13、DEV-20、DEV-28、DEV-25 |
| RC-T-004 | DEV-13 | DEV-25 |
| RC-T-005 | DEV-13 | DEV-19、DEV-25 |
| RC-T-006 | DEV-13 | DEV-23、DEV-25 |
| RC-T-007 | DEV-13 | DEV-08、DEV-09、DEV-22、DEV-25 |
| RC-T-008 | DEV-13 | DEV-00、DEV-14、DEV-15、DEV-17、DEV-25 |
| RC-T-009 | DEV-13 | DEV-12、DEV-25 |
| RC-T-010 | DEV-12 | DEV-13、DEV-23、DEV-25 |
| RC-T-011 | DEV-12 | DEV-13、DEV-25 |
| RC-T-012 | DEV-13 | DEV-10、DEV-25 |
| RC-T-013 | DEV-28 | DEV-13、DEV-27、DEV-30、DEV-25 |
| RC-T-014 | DEV-28 | DEV-13、DEV-23、DEV-25 |
| RC-T-015 | DEV-28 | DEV-19、DEV-30、DEV-25 |
| RC-T-016 | DEV-28 | DEV-13、DEV-19、DEV-25 |
| RC-T-017 | DEV-28 | DEV-13、DEV-23、DEV-25 |
| RC-T-018 | DEV-26 | DEV-13、DEV-20、DEV-31、DEV-25 |
| RC-T-019 | DEV-27 | DEV-13、DEV-23、DEV-25 |
| RC-T-020 | DEV-27 | DEV-11、DEV-13、DEV-25 |
| RC-T-021 | DEV-27 | DEV-19、DEV-29、DEV-30、DEV-25 |
| RC-T-022 | DEV-27 | DEV-11、DEV-23、DEV-31、DEV-25 |
| RC-T-023 | DEV-26 | DEV-13、DEV-20、DEV-28、DEV-30、DEV-25 |
| RC-T-024 | DEV-30 | DEV-19、DEV-20、DEV-25 |
| RC-T-025 | DEV-31 | DEV-19、DEV-20、DEV-25 |
| RC-T-026 | DEV-29 | DEV-13、DEV-15、DEV-19、DEV-30、DEV-25 |
| RC-M-001 | DEV-15 | DEV-21、DEV-23、DEV-25 |
| RC-M-002 | DEV-15 | DEV-13、DEV-25 |
| RC-M-003 | DEV-15 | DEV-23、DEV-25 |
| RC-M-004 | DEV-15 | DEV-24、DEV-25 |
| RC-M-005 | DEV-15 | DEV-00、DEV-25 |
| RC-M-006 | DEV-15 | DEV-00、DEV-13、DEV-25 |
| RC-M-007 | DEV-15 | DEV-19、DEV-24、DEV-25 |
| RC-M-008 | DEV-15 | DEV-10、DEV-13、DEV-25 |
| RC-M-009 | DEV-15 | DEV-13、DEV-23、DEV-25 |
| RC-M-010 | DEV-15 | DEV-25 |
| RC-M-011 | DEV-15 | DEV-25 |
| RC-M-012 | DEV-15 | DEV-19、DEV-24、DEV-25 |
| RC-M-013 | DEV-15 | DEV-00、DEV-25 |
| RC-M-014 | DEV-15 | DEV-24、DEV-25 |
| RC-M-015 | DEV-15 | DEV-00、DEV-13、DEV-20、DEV-25 |
| RC-C-001 | DEV-12 | DEV-13、DEV-25 |
| RC-C-002 | DEV-12 | DEV-23、DEV-24、DEV-25 |
| RC-C-003 | DEV-12 | DEV-24、DEV-25 |
| RC-C-004 | DEV-12 | DEV-13、DEV-16、DEV-25 |
| RC-C-005 | DEV-16 | DEV-13、DEV-23、DEV-25 |
| RC-C-006 | DEV-16 | DEV-23、DEV-25 |
| RC-C-007 | DEV-16 | DEV-23、DEV-25 |
| RC-C-008 | DEV-16 | DEV-24、DEV-25 |
| RC-C-009 | DEV-16 | DEV-08、DEV-09、DEV-10、DEV-25 |
| RC-C-010 | DEV-16 | DEV-21、DEV-23、DEV-25 |
| RC-C-011 | DEV-16 | DEV-12、DEV-25 |
| RC-C-012 | DEV-16 | DEV-00、DEV-25 |
| RC-C-013 | DEV-16 | DEV-25 |
| RC-C-014 | DEV-16 | DEV-10、DEV-13、DEV-25 |
| RC-F-001 | DEV-17 | DEV-23、DEV-25 |
| RC-F-002 | DEV-14 | DEV-17、DEV-25 |
| RC-F-003 | DEV-14 | DEV-17、DEV-25 |
| RC-F-004 | DEV-17 | DEV-13、DEV-25 |
| RC-F-005 | DEV-18 | DEV-23、DEV-25 |
| RC-F-006 | DEV-18 | DEV-14、DEV-25 |
| RC-F-007 | DEV-18 | DEV-19、DEV-24、DEV-25 |
| RC-F-008 | DEV-18 | DEV-23、DEV-25 |
| RC-S-001 | DEV-19 | DEV-13、DEV-25 |
| RC-S-002 | DEV-19 | DEV-13、DEV-16～DEV-18、DEV-23、DEV-25 |
| RC-S-003 | DEV-19 | DEV-00、DEV-07、DEV-25 |
| RC-S-004 | DEV-19 | DEV-25 |
| RC-S-005 | DEV-19 | DEV-25 |
| RC-S-006 | DEV-19 | DEV-05、DEV-24、DEV-25 |
| RC-S-007 | DEV-19 | DEV-15、DEV-16、DEV-18、DEV-24 |
| RC-S-008 | DEV-19 | DEV-23、DEV-25 |
| RC-S-009 | DEV-19 | DEV-22、DEV-23、DEV-25 |
| RC-S-010 | DEV-31 | DEV-19、DEV-27、DEV-30、DEV-25 |
| RC-S-011 | DEV-29 | DEV-19、DEV-30、DEV-31、DEV-25 |

## 7. 每个任务的提交前检查

每个任务在独立提交前至少完成：

- 需求 ID 与实现/测试名称可追踪，没有把后续任务的关键安全校验留成 TODO；
- 新状态有唯一所有者、进入/退出条件、timeout、失败关闭和迟到消息处理；
- 新消息有版本、角色、connection/lease/epoch/generation/request 绑定及大小限制；
- 幂等路径区分“未执行”“已执行”“结果未知”，不因重试重复副作用；
- 可复用业务流程位于职责所属 `features/<name>/application/`，签名与实现均不引入或调用 Tauri API，Command 和网络入口只做单次委托；
- `SessionStore`、后端能力与连控组件的状态所有权未混淆，没有统一有状态 Runtime、第二历史仓库或远程专用聊天状态机；
- 所有 secret 的创建、传输、使用、终态清理和日志/持久化禁区均有测试；
- WebUI v2 行为、旧 fixture、Web-to-Web 接管和既有分享链接在适用范围内回归；
- 新 UI 文案同时提供中文、英文、日文，且通过键盘/读屏基本检查；
- Windows 能力声明正确，macOS/Linux 入口隐藏且调用返回 `unsupported`；
- `git diff --check` 无错误，提交只包含本任务范围。

## 8. 测试与门禁矩阵

### 8.1 任务内定向反馈

每个任务在开发循环中先运行其“测试”字段列出的最小定向集合，并至少追加：

```bash
git diff --check
python scripts/architecture-guard.py
```

只改 Relay 时运行对应 `node --test remote-control-relay/test/<target>.test.js`；只改前端逻辑时运行对应 `node --test pinvou3-app/tests/<target>.test.mjs`；只改 Rust 时运行最窄模块过滤的 `cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml <target> -- --test-threads=1`。定向反馈用于快速定位，不能作为阶段出口或 PR 完整验证。

### 8.2 阶段/PR 完整出口门禁

任何第 9 节出口、发布候选或覆盖多个实现域的 PR，必须运行以下完整集合：

```bash
npm --prefix remote-control-relay test
npm --prefix pinvou3-app test
npm --prefix pinvou3-app run build:ui
npm --prefix pinvou3-app run test:webui
cargo fmt --manifest-path pinvou3-app/src-tauri/Cargo.toml --all -- --check
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib -- --test-threads=1
python scripts/architecture-guard.py
git diff --check
```

涉及前端桥/UI 时追加：

```bash
npm --prefix pinvou3-app run test:bridge-protocol
npm --prefix pinvou3-app run test:web-access-contract
npm --prefix pinvou3-app run test:ui-language
npm --prefix pinvou3-app run lint:ui
```

完整出口不得因为定向测试已经覆盖相同行为而省略 `pinvou3-app test`、`build:ui` 或 `cargo test --lib`。涉及 Windows 私有运行时、安装包、部署/gateway 或系统网络故障注入时，还须运行对应 Windows 专属测试与 DEV-25 旅程。

### 8.3 分层验收责任

| 层级 | 最低证据 | 不能替代的证据 |
|---|---|---|
| 纯协议/密码学 | 固定向量、双实现 fixture、非法输入表 | 真实三方 WebSocket 握手 |
| Relay 状态机 | 可控时钟、并发 CAS、崩溃点/重启、真实旧 Web fixture | Windows 本地 watchdog |
| Rust 状态机 | 单调时钟、generation/epoch、内存清理、进程重启 | 公网部署与旧 Relay 回滚 |
| 业务适配 | Engine/ACP/SessionStore 真实 adapter、重复副作用测试 | 双机 UI 旅程 |
| 前端 | bridge race、三语、键盘/读屏、窗口生命周期 | backend 权限与密钥边界 |
| 发布 | 两台隔离 Windows、生产等价 TLS/网关、网络故障、抓包/日志扫描 | 单元测试或 mock-only E2E |

## 9. 合入与灰度出口

1. **协议出口**：DEV-00～DEV-09 完成，测试客户端能建立密文租约、证明 30 秒 watchdog 并完成无业务 provisional resume；旧 Web v2 全绿。
2. **基础功能出口**：DEV-10～DEV-13、DEV-20、DEV-22 完成，两台 Windows 可用 B 模型完成普通任务、安全断开和后台窗口旅程。
3. **完整能力出口**：DEV-14～DEV-19、DEV-21、DEV-23 完成，模型、审批、附件、成果、认证同步与单一 SessionStore RemoteProxy/ChatView 形成闭环；不存在第二历史或临时会话状态机。
4. **生产出口**：DEV-24～DEV-25 完成，enrollment 覆盖率、限流、告警、迁移、回滚和真实双机证据齐全。

任何出口存在以下情况时必须阻断：未 enrolled 公网首次注册可成功；Web/controller 可互相顶替；Relay 可见业务明文/API key；B 可 Allow；30 秒预算可被 B 心跳、重试或网络检测额外延长；旧 token/generation/lease 可复活；任务重试产生重复副作用；网络入口调用 Tauri Command；`RemoteProxy` 进入 A 本机 Engine/ACP；新旧历史双写或并行供数；裸 v2 可被回滚上线。
