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
既有安装取消路径没有改为 Job Object，也没有新增长期闲置的 Tokio 启动接口。

该实现影响 PDF/Office/邮件转换、连接器探测、代码检查点、Marketplace 依赖检查
和监控命令等共享调用方；共享进程回归必须覆盖成功后代存活、超时孤儿后代退出、
输出超限和有界回收。完整 Rust 门禁仍须在原生 Windows 与 Linux 上通过。
