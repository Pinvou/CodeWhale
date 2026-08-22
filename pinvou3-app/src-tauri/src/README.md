# Rust 目录边界

Rust 后端按“功能优先、平台适配次之”组织：

- `app/`：Tauri 命令入口、启动装配、事件桥和应用级基础设施；`commands.rs` 仅维护稳定门面，命令实现按业务域放在 `app/commands/`。
- `features/`：业务功能；功能专属的平台代码放在该功能的 `platform/` 子目录。
- `platform/`：跨功能复用的操作系统原语与兼容门面，例如进程、凭证、通知和路径。
- `core/`：预留给不依赖 Tauri、具体功能或操作系统的共享类型。
- `lib.rs`：唯一的 crate 组合根，只声明模块和装配应用。

迁移期通过 `lib.rs` 的 `#[path]` 保持原有 `crate::<module>` 路径，避免目录调整同时改变公共 API。`app/commands.rs` 使用 `include!` 将领域文件保持在同一个 Rust 模块中，从而维持 Tauri 命令名、参数协议和 `crate::commands::*` 路径。新增命令应放入 `app/commands/<domain>.rs`，不再向门面文件或 `platform/os/*_system.rs` 追加完整业务实现。

平台代码规则：

1. 业务流程留在 feature 根目录。
2. Windows、Linux、macOS 差异放入 `features/<name>/platform/`。
3. 只有被多个 feature 共同使用的低层能力才能进入 `platform/`。
4. 操作系统选择使用 `cfg(target_os)`；Cargo feature 不用于模拟操作系统。
5. 未支持能力必须显式返回 unsupported，不得静默执行其他平台实现。

上述依赖和平台边界由仓库根目录的 `scripts/architecture-guard.py` 检查。迁移期保留
`#[path]`、`include!` 可以用于兼容公共路径或生成代码，语法本身不作为架构违规；是否
合理取决于实际职责、依赖方向和生成代码边界。可稳定检测的结构性债务记录在
`scripts/architecture-baseline.json`，只能逐步减少，不能新增或扩大；规则、显式例外
和本地运行方式见 `docs/architecture-guard.md`。

当前 Rust 依赖方向为 `lib.rs/app → features → platform/core`。feature 不能引用
`app/commands`，feature 之间也不能形成依赖环。跨功能协作由组合根注入：Engine 工具和
工具门控通过 `EngineToolFactory` / `ToolPolicy` 注入，Tauri 事件通过 `AppEventBus`
转发，远控附件 staging 通过 `AttachmentStager` 注入。资源 bundle、连接器可见性协调和
路径安全策略属于跨功能基础设施，统一位于 `platform/`。

## 内置模型账号平台切换

客户端内置模型只有一条平台托管路径：`AccountPlatformRuntimeModelProvider` 在每次请求前
从账号平台取得当前 OIDC 访问令牌和 Token Gateway 地址，再交给 Engine 发起调用。内置
模型持久化 ID 为 `builtin_account_platform`；本地模型和用户自行配置的第三方模型仍直接
使用各自配置，不经过账号平台。

应用启动时，`features/account_platform/client_migration.rs` 在会话存储初始化前执行一次
版本化迁移：

1. 将设置、历史会话模型映射和定时任务模型引用切换到新 ID，并保留旧设置与绑定文件备份。
2. 删除已发现的旧设备账号、旧会话及旧模型凭据，不在迁移报告中记录凭据值。
3. 写入 `~/.pinvou3/migrations/account-platform-model-v1.json`。只有状态为 `complete`
   才会在后续启动跳过；凭据清理失败时写入 `cleanup_pending`，下次启动继续清理。

客户端切换验收以以下不变量为准：内置模型运行时必须获得账号平台授权；源码注册链不得
包含旧模型命令或运行时回退；旧模型 ID 只能出现在迁移常量、兼容反序列化别名和迁移测试
数据中；迁移重复执行不得再次改写业务数据。对应自动化覆盖位于
`features/account_platform/client_migration.rs`、`platform/prefs/mod.rs` 和
`tests/account_platform_state.test.mjs`。
