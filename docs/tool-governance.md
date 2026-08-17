# 工具管理说明（Tool Governance）

本文档描述 pinvou3 在 **CodeWhale v0.9.5** 基线上的模型工具治理。v0.9.5 已改用 canonical action family 与 `allowed_tools` 正向白名单；v0.9.0 的 `PINVOU3_HIDDEN_TOOLS`、`hidden_tools` 注入和逐个旧工具名放出机制已经退役。

## 1. 当前治理链路

模型最终可见且可执行的工具由以下约束共同决定：

```text
CodeWhale 按 Plan / Agent / Yolo 注册 canonical 工具族
        ↓
Pinvou allowed_tools 正向白名单（硬边界）
        ↓
CodeWhale 首轮加载 / tool_search 延迟加载策略
        ↓
Pinvou disallowed_tools（模式、连接器、技能空态等动态拒绝）
        ↓
单轮 allowed_tools（例如 restrict_tools=true 的零工具请求）
```

- `allowed_tools` 同时约束首轮目录、`tool_search` 结果和实际执行，不在白名单中的工具不能通过搜索重新激活。
- `disallowed_tools` 只能继续收窄白名单；模式差量、连接器停用集和运行期空态均走这条通道，下轮请求生效。
- Plan / Agent / Yolo 的注册面和 sandbox 仍由 CodeWhale 决定。白名单允许一个工具族，不代表每种模式都注册其全部写操作。
- `request_user_input` 与 `image_analyze` 由 `PINVOU3_ALWAYS_LOADED_TOOLS` 要求首轮直接可见；其他允许工具可按底座策略延迟加载。

关键实现：

| 职责 | 文件 |
|---|---|
| Pinvou 正向白名单 | `pinvou3-app/src-tauri/src/features/assistant/tool_policy.rs` |
| 模式能力静态表 | `pinvou3-app/src-tauri/src/features/assistant/session_policy.rs` 的 `MODE_TABLE` |
| 模式与动态拒绝整形 | `pinvou3-app/src-tauri/src/features/assistant/session_policy.rs`、`platform/bridge.rs::shape_disallowed_tools` |
| 能力包与 scope 治理 | `docs/capability-governance.md` |
| 底座注册、过滤与执行 | `CodeWhale/crates/tui/src/core/engine.rs` 及其工具注册模块 |

## 2. Pinvou 正向白名单

`PINVOU3_ALLOWED_TOOLS` 当前允许：

| 类别 | 名称 | 说明 |
|---|---|---|
| canonical action family | `Bash`、`File`、`Git`、`Web` | v0.9.5 将同域动作收敛到一个工具名，通过 `action` 参数分派 |
| 协作与工作流 | `agent`、`workflow` | 子 agent 与工作流入口 |
| 交互与状态 | `request_user_input`、`revert_turn`、`todo_write` | `todo_write` 是模型可见的 canonical 进度工具 |
| 技能与发现 | `load_skill`、`tool_search` | 仍受技能组合目录与底座延迟加载规则约束 |
| 视觉与知识 | `image_analyze`、`kb_search`、`kb_open_source` | 知识工具还受索引/语义引擎就绪状态约束 |
| MCP | `mcp_*`、`list_mcp_resources`、`list_mcp_resource_templates`、`read_mcp_resource` | `mcp_*` 是前缀规则；具体连接器停用集走 `disallowed_tools` |

规则匹配大小写不敏感，尾部 `*` 表示前缀。新增白名单项必须同步更新 `tool_policy.rs` 的正反向测试和 fork guard。

## 3. canonical 工具族与旧名称

v0.9.5 的模型侧名称以 canonical family 为准：

| canonical 名称 | 典型 action | 取代的旧模型侧名称 |
|---|---|---|
| `File` | `read`、`write`、`edit`、`patch`、`list`、`search_content` | `read_file`、`write_file`、`edit_file`、`apply_patch`、`list_dir`、`grep_files` 等 |
| `Bash` | 前台、后台、等待、取消等 shell 动作 | `exec_shell`、`exec_shell_wait`、`exec_shell_cancel` 等 |
| `Git` | `status`、`diff`、`log`、`show`、`blame` 等 | `git_status`、`git_diff`、`git_log`、`git_show`、`git_blame` |
| `Web` | 搜索与 URL 获取 | `web_search`、`fetch_url` |
| `todo_write` | 进度项写入 | `work_update`、`update_plan`、`checklist_write` |

旧名称只在历史会话回放或兼容解析中保留，`model_visible()=false` 的 replay 别名不得写入产品白名单、提示词或新文档。前端渲染器可以同时识别旧名称和 canonical 名称，以保证历史会话可读。

## 4. work / code 模式差量

模式差量由编译期静态表 `MODE_TABLE` 声明，不再维护运行时能力档案：

| 维度 | work（plain） | code |
|---|---|---|
| 原生家族 | 公共白名单（含 `Git`） | 公共白名单（含 `Git`） |
| `MODE_TABLE.unavailable_tools` | 空 | `mcp_pinvou3_present_artifact` |
| 能力包默认策略 | AllowAll | DenyAll |
| 连接器/技能禁用集 | `plain` scope | `code` scope |
| `load_skill` | 按正常技能状态 | 会话组合技能目录为空时隐藏 |
| 项目规则与代码层指令 | 不绑定 | 绑定项目并使用 code instructions |

因此，#238 在 v0.9.0 上“逐个 include 8 个旧工具名”的产品目标，在 v0.9.5 上由 canonical family 与公共正向白名单承接；不再存在 8 个 `hidden_tools` 注入项。底座能力是产品承诺，plain 与 code 均允许 `Git` family；两种模式的外部能力风险姿态则由各自 scope 的默认策略和用户选择控制。

code 会话默认 Plan 时，最终可用 action 仍受 Plan 注册面与只读 sandbox 限制；切换 Yolo 后才具备相应写入/执行面。产品层不得用白名单绕过底座模式安全边界。

## 5. 动态拒绝与优先级

`shape_disallowed_tools` 在持久禁用集上叠加：

1. 当前模式 `MODE_TABLE` 的 `unavailable_tools`；
2. 对应模式 scope 的连接器禁用工具；
3. 当前模式要求且技能组合目录为空时的 `load_skill`；
4. 知识库、会话或产品状态产生的其他动态拒绝项。

同一名称重复加入时会去重。允许与拒绝冲突时拒绝优先；`restrict_tools=true` 的单轮请求还可进一步收敛到零工具。

## 6. 修改流程

### 放出或新增工具族

1. 确认 CodeWhale 当前模式确实注册该 canonical 名称/action，并核对 sandbox 与审批语义。
2. 在 `PINVOU3_ALLOWED_TOOLS` 添加精确名称或最窄前缀；不要添加 replay 别名。
3. 若某模式架构上不提供该工具，在 `MODE_TABLE` 对应行的 `unavailable_tools` 中声明；用户可选的外部能力应走能力包 scope，不得混入模式身份。
4. 同步工具策略、模式静态表、前端渲染与提示词契约测试。
5. 更新本文和 `scripts/fork-guard.sh`，运行完整 fork guard 与 app 测试。

### 隐藏工具

1. 全产品禁止：从正向白名单移除。
2. 按模式禁止：仅架构缺席项写入 `MODE_TABLE.unavailable_tools`。
3. 按连接器、技能或运行状态禁止：通过 scope 状态和 `disallowed_tools` 动态计算，不修改模式静态表。
4. 不得仅隐藏 UI；执行层必须同步不可达。

## 7. 安全不变量

- 白名单是唯一产品级放行边界；不能引入第二套 fork-only 黑名单或恢复 `hidden_tools` 注入。
- `tool_search` 只能发现白名单内且未被拒绝的工具。
- `File` 写入仍受 64 KiB 单次内容上限、工作区边界和审批/claim 约束。
- `Bash` 的多行命令按 segment 做安全分析；破坏性命令不能被自动批准绕过。
- 宿主额外工具必须在 Plan、Agent、Yolo 等注册路径一致注入，并继续受白名单和拒绝集约束。
- 三省六部结构化/文件产出只能写声明过的安全路径，不得借 canonical `File` 扩大权限。

## 8. 迁移记录

| 日期 | 变更 |
|---|---|
| 2026-08-10 | v0.9.0 能力档案曾为 code 模式从底座隐藏集放出 5 个 Git 工具及 `apply_patch`、`run_verifiers`、`exec_shell_cancel`。 |
| 2026-08-11 | 升级 CodeWhale v0.9.5：切换到 canonical family、`allowed_tools` 正向白名单和单一 `disallowed_tools` 差量通道；旧 8 项 include 被等价迁移，不再作为模型侧工具名维护。 |
| 2026-08-14 | 退役无运行时写入者的能力档案，模式架构差量收敛为 `MODE_TABLE`；plain 放开 `Git` family，外部能力默认姿态改由按模式 scope 管理。 |

每次 CodeWhale 升级都必须重新核对 canonical 名称、各模式注册面、首轮/延迟加载策略、前端历史回放兼容和本文档；不能只凭旧工具名相同就判定行为等价。
