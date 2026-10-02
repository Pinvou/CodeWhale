# 架构门禁

`scripts/architecture-guard.py` 检查 Rust、前端和平台适配边界。它只使用 Python
标准库，可在本地和 CI 中运行：

```bash
python scripts/architecture-guard.py
```

## 强制规则

- 前端 feature 不得 import `src/app`。
- `window.__TAURI__` / `globalThis.__TAURI__` 只能出现在 `src/platform`。
- 禁止通过 `navigator.userAgent`、`navigator.userAgentData` 或
  `navigator.platform` 判断平台。
- Rust feature 不得反向依赖 `app`。
- Rust `platform` 不得反向依赖 `app` 或 `features`。
- Rust feature 不得新增或扩大循环依赖，也不得增加现有循环内部的依赖边和引用。
  `use crate::app as host`、`use crate::features as domains` 等根模块别名
  （含分组、`self` / `super` 别名链和 `extern crate self`）按原始层级解析；
  crate 根真实顶层的私有或公开 `use`（含无 `crate::` 前缀）及
  `extern crate self as ...` 导出的别名也会跨文件追踪；函数、内联模块和
  未调用的宏定义体不作为根别名，无法静态展开的根宏调用会明确报不确定项。
  raw identifier 也归一到同一模块名；`self` / `super` 按真实模块路径
  （`mod.rs`、普通 `.rs`、`#[path] mod` 与内联模块）解析，`as _` 仍计入
  导入路径的依赖。仅导入 `app` / `features` 等根模块本身也算依赖；
  feature 根模块导入和解析到 crate 层级根的 glob 导入因无法静态确定具体依赖而拒绝，
  普通测试子模块的 `use super::*` 不在此列。`cfg_attr` 中所有可能的 `path`
  分支均检查；无法唯一定位的内联 `#[path]`、动态或扫描树外的 `include!` / `path`
  会按不确定模块路径报错，不会退回到磁盘目录猜测。经 `include!` / `#[path]`
  映射的文件按声明所在的模块归属计算依赖边，而非按其磁盘父目录。
  修正导入解析后识别出的既有环按引用次数登记；测试源码依赖也纳入图，
  增加同一依赖的引用或扩大环会失败，等价移动导入不改变架构债计数。
- `features/*/application/**/*.rs` 会被递归枚举；其中的用例签名和实现不得
  导入或调用 Tauri API，也不得直接依赖具体 `platform::app_events` 实现。
  宿主通知必须通过所属 feature 定义的窄端口由组合根注入。
- `features/sessions/application/mod.rs` 的五个公开模块导出按 Rust token 检查固定为
  `admission`、`dispatch_remote`、`ports`、`project_remote`、`submit_controlled`；
  旧 `sync_remote_journal` 槽已由 RemoteProxy / `project_remote` 的单一投影 owner 取代，
  不保留兼容模块；声明必须无条件可用，顶层宏展开与
  `include!` 不得增删导出；跨行声明和注释不能绕过检查。后续任务只填充各自实现文件。
- 整个 `features/remote_control/**/*.rs` 禁止新增直接 Tauri API 或宿主事件调用。
  `scripts/architecture-baseline.json` 按文件、调用模式和数量记录已有依赖，
  新文件与现有文件的增长均失败；宿主根路径别名、glob 导入及泛型 `emit` 调用
  也计入守卫。清除旧调用时须同步下调基线。
  新端口 `controller/ports.rs` 没有例外；远控入口通过注入的
  `ControlledExecutionPort` / `RemoteSessionProjectionPort` 调用用例。
- 平台 `target_os` 分支应位于 platform adapter 或组合根；PowerShell、注册表、
  `xdg-open` 等实现细节应位于 platform adapter。合理例外按下文显式登记。
- `#[tauri::command]` 必须位于 `app/commands/` 路径下，`generate_handler!` 条目以
  `commands::`（或 `crate::app::commands::`）前缀引用。
- `resources/common` 不得混入 PE、ELF 等平台专属二进制。
- 禁止 spawn 外部 `kill` 可执行文件执行进程组或进程树终止
  （`Command::new("kill")`、`Path::new("kill")`、
  `connector_cli_command(_, "kill")` 等形态）。Unix 组杀必须通过
  `platform::process::kill_process_tree` / `platform::os::kill_pid_tree`
  直接调用 `kill(2)`；该规则对所有 Rust 文件生效且没有文件级例外。
- `pinvou-cli` crate 不得在代码中直接引用 `deepseek_tui::` 或 `tauri::`（注释文档
  除外；`use tauri as x`/glob 导入等别名形态同样被 `use` 语句规则拦截）：CLI 一律
  经由 `pinvou3_lib`（`features`/`platform` 公共面）触达 app，防止无头构建悄悄耦合
  GUI 栈或绕过 app 自身的分层（2026-09-13 随 PR #478 增补）。扫描范围是
  `pinvou-cli/crates/`（源码面；这是 tripwire 级规则，极端混淆形态不在其覆盖内）。

门禁只约束可以稳定、客观检测的架构边界，不以文件行数或仓库总代码量作为模块化
合规条件。模块是否需要拆分，应依据职责、依赖、状态耦合和独立测试能力，遵循根目录
`AGENTS.md` 的模块化开发公约进行评审。

## 显式例外

平台条件编译和平台实现细节规则允许少量、可评审的文件级例外。确有合理场景时，在
文件前 20 行内分别使用以下注释，并在 `--` 后写明具体理由：

```rust
// architecture-guard: allow-target-cfg -- 第三方类型仅在 Windows 目标存在
// architecture-guard: allow-platform-detail -- 此处只负责解析外部平台标识
```

例外必须保持最小范围并补充覆盖该场景的测试；缺少理由、位于文件头之外或仅为绕过
模块迁移而添加的标记不会被接受。`include!` 和 `#[path]` 本身不再作为架构违规；是否
合理由实际职责、依赖方向和生成代码边界判断。

## 历史基线

`scripts/architecture-baseline.json` 记录启用门禁时已经存在的架构债务。门禁不允许：

- 在新文件或新依赖边中出现同类违规；
- 增加现有违规数量；
- 新建或扩大 feature 依赖环。

修复历史债务后，必须在同一变更中下调或删除对应基线；否则门禁会报告基线过期并
失败。这使基线成为严格棘轮，已经删除的违规不能在后续变更中恢复。

PR CI 还会通过 `--base-ref` 对比目标分支中的基线。当前分支不能增加目标分支已有的
额度、增加新条目或扩大依赖环，因此同时修改当前基线不能绕过门禁。首次引入基线时，
目标分支尚无该文件，脚本会明确报告初始化并允许通过。

远控宿主 API 规则仅在目标基线缺少该规则键时可按已核定的历史调用额度首次登记；
已存在但为空的规则表示债务清零，不能重新放宽。Rust 依赖解析升级使用一次性
`rust_dependency_resolution_v1` 标记和固定迁移锚点
`5bedd1ba3c4eb6534821e293bea408470155f69a`（DEV-26 开工前、已审任务之后的提交）。
`--base-ref` 检查锚点确属候选分支历史，以当前解析器只读扫描目标分支 T、
锚点 A 和两者的共同祖先 M。首次迁移每条依赖引用的上限为
`max(0, T + (A - M))`：保留本任务之前已审分支提交的净变化，同时计入目标分支
后续增减；feature 环必须由合成后的完整依赖边图重新形成，不能凭旧环清单放行。
例如 M 有 24 次引用、A 有 28 次、T 仍有 24 次，则可登记 28 次；若 T 后来清理
2 次，上限降为 26 次。目标分支的无关变更不影响额度。锚点不是候选 HEAD 的祖先时明确失败，
须重新审计并更新迁移锚点，不能静默放行。迁移标记落地后，清零债务不能恢复，
也不能改动锚点重放迁移。无 `--base-ref` 不授予迁移额度。

本任务的独立评审以任务起点锚点为基准。当前 `origin/main` 已将
`connectors->marketplace` 的旧引用从 14 次减少到 10 次；本分支锚点该边为 14 次，
并无本任务之前的净新增。与该主线集成时须继承其清理、把本分支实际引用及基线
一同降至不超过 10 次，再重跑 `--base-ref origin/main`；不能放宽迁移门禁以保留 14 次。

查看当前扫描结果：

```bash
python scripts/architecture-guard.py --print-current
```

不要直接用该输出覆盖基线来绕过失败。清债时只能下调相关条目。若确需改变门禁政策，
必须作为独立架构决策修改规则和文档；单纯提高基线会被 CI 拒绝。
