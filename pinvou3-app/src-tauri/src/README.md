# Rust 目录边界

Rust 后端按“功能优先、平台适配次之”组织：

- `app/`：Tauri 命令入口、启动装配、事件桥和应用级基础设施；命令实现按业务域放在 `app/commands/`。
- `features/`：业务功能；功能专属的平台代码放在该功能的 `platform/` 子目录。
- `platform/`：跨功能复用的操作系统原语与兼容门面，例如进程、凭证、通知和路径。
- `core/`：存放不依赖 Tauri、具体功能或操作系统的共享类型。
- `lib.rs`：唯一的 crate 组合根，只声明模块和装配应用。

Tauri 命令按 `app/commands/<domain>.rs` 组织，`app/mod.rs` 与 `app/commands/mod.rs` 只做模块声明；命令名、参数协议由各领域文件直接定义。新增命令应放入 `app/commands/<domain>.rs`，不再向门面文件或 `platform/os/*_system.rs` 追加完整业务实现。

平台代码规则：

1. 业务流程留在 feature 根目录。
2. Windows、Linux、macOS 差异放入 `features/<name>/platform/`。
3. 只有被多个 feature 共同使用的低层能力才能进入 `platform/`。
4. 操作系统选择使用 `cfg(target_os)`；Cargo feature 不用于模拟操作系统。
5. 未支持能力必须显式返回 unsupported，不得静默执行其他平台实现。
6. 禁止 spawn 外部 `kill` 可执行文件执行进程组或进程树终止。Unix 组杀统一
   使用 `platform::process::kill_process_tree` /
   `platform::os::kill_pid_tree` 直接调用 `kill(2)`；此约束由
   `scripts/architecture-guard.py` 的 `rust_external_group_kill_spawn` 规则检查。

上述依赖和平台边界由仓库根目录的 `scripts/architecture-guard.py` 检查。迁移期保留
`#[path]`、`include!` 可以用于兼容公共路径或生成代码，语法本身不作为架构违规；是否
合理取决于实际职责、依赖方向和生成代码边界。可稳定检测的结构性债务记录在
`scripts/architecture-baseline.json`，只能逐步减少，不能新增或扩大；规则、显式例外
和本地运行方式见 `docs/architecture-guard.md`。

当前 Rust 依赖方向为 `lib.rs/app → features → platform/core`。feature 不能引用
`app/commands`，feature 之间也不能形成依赖环。跨功能协作由组合根注入：Engine 工具和
工具门控通过 `EngineToolFactory` / `ToolPolicy` 注入，Tauri 事件通过 `AppEventBus`
转发，远控附件 staging 状态由 `RemoteControlManager` 内部
（`features/remote_control/manager/transfer.rs`）持有。连接器可见性协调
（`platform/connector_state.rs`、`platform/connector_skills.rs`）和路径安全策略
（`platform/path_policy.rs`）属于跨功能基础设施，位于 `platform/`；内置资源 bundle
的解包由 `features/runtime_bundle/` 承担。

## 市场授权与生命周期的窄接口

assistant 持有一次 manifest 快照并枚举技能来源，拒绝判定由
`marketplace::bundle::skill_gating_is_disabled_with` 负责；owner 未被拒绝时
才继续检查 preset ID 别名，不改变导入或 UI 的 owner 认领规则。

CLI 连接器通过 `ConnectorSkillGate::apply_with_consent` 复用
“登记默认关同意 → 物化”的顺序，并区分原始同意错误与物化错误。
命令的安装后同步和启动的 ledger-gated 刷新仍保持独立。
IMA 使用原安装登记语义，在任何凭据访问之前执行，不借用 CLI 可见性登记。

固定退役内嵌包的清理归 `marketplace::retirement` 所有；
runtime boot 只调用零参数入口，不传任意包 ID 或路径。
该迁移保留 landing lease、import mutex、Upload 保护和原删除顺序，
不声称已解决市场事务与 import 锁之间既存的全局锁序问题。

连接器、市场持久状态和凭据的真实组合回归位于
`app/connector_consent_tests.rs`；feature 保留自身单元测试。
IMA 测试入口仅在 `cfg(test)` 编译，不新增产品或 IPC 接口。

## 远程对话轮次的 Skills 与引擎所有权

远程选择在 assistant 适配层对照 B 的已启用集合解析，复制到每次尝试独占的
Skills 目录（空选择也保留空目录）。它不改写普通会话的共享目录；共享物化、
增量重写和自愈不能扩大该轮次的选择。这里的冻结是应用写入隔离，不是 OS
只读沙箱。目录缺失时发送失败，不回退到共享 Skills。

`Pinvou3Bridge` / `AppEngine` 克隆持有快照资源；pool 的复用与远程终态清理
必须匹配同一个不可复用 owner。旧 owner 没有 entry 或遇到 replacement 时不操作
新轮次、共享 Skills 或新 reservation。所有远程模型来源遵循同一清理边界。
冷启动在生成引擎之前加载会话，先将 SyncSession 入队，再公开 entry；
未公开引擎由专用 guard 在取消/失败时停止 forwarder 并发送有界清理操作。
Shutdown 入队不等于底座/provider 已 join；崩溃遗留目录和 Windows 打开文件导致
的删除失败也不在自动清理保证内，不可将这些局限描述为已验证的完整释放。

ACP 远程 code 回合在 `codex_acp/remote_ownership.rs` 持有独占预留。真实 busy/config
接纳与本地发送共用门控；尚未结束的 SDK prompt 即使被 watchdog 本地收口，仍禁止
远程复用该连接。预检失败回滚 Reserved，只有绑定实际 turn 与连接身份后才允许
远程审批；Reserved、Closing、退休连接和迟到回调都明确取消，不回退成本地审批。
取消、卡片清理和事件转发匹配同一个 task/lease、不可复用 turn ID 与实际连接；
缺失权威终态明确失败。卡片响应后的清理还匹配单次 callback，避免复用 tool ID
时误删后继卡片。

远程结束会退休该 ACP 实例，持久会话、工作区及配置保留，后续调用按正常流程恢复；
因此后续回合可能冷启动。普通本地回合仅增加实际 prompt 存活记账，不改变既有
watchdog/Stop 策略。退休关闭宿主连接及其直接子进程，不等于完整 provider/后代
进程树已终止或 join；该底座局限仍按 Issue #254 跟进。

## 共享子进程的终止边界

`platform::process::output_with_timeout_and_kill_tree` 在 Windows 上先以挂起状态
启动根进程，加入独立 Job Object 后才恢复执行。超时或输出超限时显式终止 Job，
覆盖经正常进程创建 API 派生、即使中间父进程已经退出的后代；这不是阻止进程借助
系统服务或其他既有进程启动任务的安全沙箱。正常完成不启用 kill-on-close，保留
启动器完成后继续运行的后代。普通 `output_with_timeout` 保持超时只终止根进程的
既有契约；两者的输出超限均请求终止受管理进程树，输出上限仍为每路 4 MiB。

Linux/macOS 继续直接终止独立进程组。Windows 的兼容
`kill_process_tree(pid)` 仅对未受管理进程使用身份句柄和有界 `taskkill`，
不能保证回收已经失去父链的后代。上述同步输出路径不借用这一兼容退路：
Job 创建、加入或恢复失败均停止该次启动并返回错误，不静默降级。
普通父 Job 支持嵌套启动；宿主处于带 UI 限制等不允许嵌套的 Job 时，加入失败即
返回启动错误，该宿主环境不受此路径支持，不以绕过父 Job 或无托管启动作为回退。
ACP 安装的实际消费方通过异步 spawn_managed_tokio 同样在恢复前建立独立 Job，
创建、加入或恢复失败有界回收并返回错误，不回退裸 PID；Unix 所有安装阶段均为
独立进程组。共享托管 owner 可跨线程克隆，正常关闭不终止后代，取消/超时显式终止。
取消在安装操作锁内标记并捕获精确的当前 tree，随后锁外终止；唯一嵌套锁顺序为
operations → children。旧安装完成/新安装开始不能将迟到取消转向后继 tree。
取消发生于启动后、tree 登记前时仍由登记后的标记复检收口。
安装 guard 默认持有异常回收责任：失败、future 取消或 panic 都在注册表锁外终止
自身托管树；仅在完成输出收口并确认成功后解除，正常完成仍允许后代存活。
“精确捕获”指注册表中的同一 owner；Windows 使用内核 Job 身份，Unix 使用 PGID，
不将它表述为 Unix 进程组 ID 永不复用的保证。

该实现影响 PDF/Office/邮件转换、连接器探测、代码检查点、Marketplace 依赖检查
和监控命令等共享调用方；共享进程回归必须覆盖成功后代存活、超时孤儿后代退出、
输出超限和有界回收。完整 Rust 门禁仍须在原生 Windows 与 Linux 上通过。
