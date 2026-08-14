# Team Relay 演示契约

本目录是团队接力演示阶段的协议基线，供 Team Relay Hub、Pinvou Rust 客户端和前端共同使用。

当前协议版本为 `0.1`。批次 1 开始后，任何状态语义变更都必须同步修改本目录中的三份契约并重新执行迁移与一致性检查。

## 文档入口

- [`protocol.md`](protocol.md)：身份、状态机、权限、不变量和错误码；
- [`openapi.yaml`](openapi.yaml)：OpenAPI 3.1 HTTP/SSE 契约，`components/schemas` 同时是核心 JSON Schema；
- [`migrations/0001_demo.sql`](migrations/0001_demo.sql)：PostgreSQL 首版 schema 草案；
- [`migrations/0002_delivery_packages.sql`](migrations/0002_delivery_packages.sql)：为已部署动作日志增量加入不可变交付包；
- [`migrations/0003_thread_management.sql`](migrations/0003_thread_management.sql)：加入个人置顶/隐藏偏好，以及可审计的工作取消状态；
- [`migrations/0004_fixed_join_password.sql`](migrations/0004_fixed_join_password.sql)：加入由 owner 生成和轮换、可重复使用的团队加入码；
- [`migrations/0005_short_join_code.sql`](migrations/0005_short_join_code.sql)：保留既有摘要字段并登记短加入码语义；
- [`migrations/0006_persistent_join_code.sql`](migrations/0006_persistent_join_code.sql)：加入可恢复的加密加入码与成员可见性策略；
- [`migrations/0007_principal_email.sql`](migrations/0007_principal_email.sql)：加入服务内唯一邮箱账户，并允许旧成员后续绑定；
- [`migrations/0008_thread_read_states.sql`](migrations/0008_thread_read_states.sql)：持久化跨设备已读游标，并提供任务未读更新计数；
- [`../team-relay-demo-implementation-plan.md`](../team-relay-demo-implementation-plan.md)：范围、部署、批次与验收计划。

发生冲突时，以 `protocol.md` 的业务不变量为准；实现修改契约时必须在同一变更中同步 OpenAPI、数据库迁移草案和契约测试。

## 演示基线

唯一必须跑通的业务链路是：

```text
A 派活给 B
→ B Handoff 给 C
→ C 回传给 B
→ B 最终交付给 A
→ A 要求补充
→ B 再次交付
→ A 接收结束
```

演示阶段的实时事件只是刷新提示，PostgreSQL 中的线程与动作日志才是权威状态。接收方离线后可在下次 bootstrap 恢复；发送方离线不提供持久化 Outbox，也不能显示为已经送达。

派活、Handoff、回传、最终交付和要求补充共用同一套直接提交表单，顺序为：接收人、关联 Session、要做什么、交付目标、上传附件、确认或取消。主路径不再调用 AI 整理、不创建草稿 Session，也没有二次预览；用户在一张表单里看清将要发送的信息后直接确认。

关联 Session 为单个下拉框，不再提供“是否引用产物与工作区摘要”开关。选择普通 Session 后，客户端默认选中该 Session 已登记的产物和可识别的工作区文件，用户可在提交前逐个移除。选择 Coding Session 后，界面展示经脱敏的远端仓库地址、当前 Commit SHA、分支、Git 工作区统计及相关变更路径；只有仓库地址和 Commit 都可靠时才生成历史固定的 Git 引用。不会自动 commit 或 push，未提交变更只能通过用户明确保留的文件快照传递。

关联 Session 不会把对话消息、工具调用、命令输出、Git diff、绝对路径或 Git 凭据发送给 Team Relay Hub。手工附件和 Session 材料在表单阶段只保留本地候选；只有用户点击确认后，客户端才以稳定 `upload_id` 上传所选文件快照并把它们绑定到当前动作；上传失败时接力动作不提交。代码类快照允许 HTML/CSS/JavaScript/TypeScript/JSON/YAML，并统一作为 `text/plain` 下载，Hub 不以可执行内容类型提供。

接到工作后，桌面端可把线程内已共享文件准备到 `~/.pinvou3/team-relay/workspaces/` 下按团队和线程哈希隔离的本机私有目录，并在本机 manifest 中记录该任务关联的普通或 Coding Session。首次“使用我的 Agent”可选择普通对话或 Pinvou 代码模式；代码模式默认使用 Pinvou 原生代码会话，也可主动切换到当前接入的其他 ACP Agent，并可绑定本机已有代码目录。再次进入复用同一 Session，并展示已带入材料、尚未带入的新材料、Git 固定引用和代码目录；新材料默认勾选，旧材料可手工再次选择。新的接力动作、材料或 Git 信息只会形成一段待用户确认发送的差量上下文，不会自动向 Agent 发消息。客户端也不自动 clone、checkout、pull、commit 或 push。上述本机路径、关联关系和 Agent 过程均不发送到 Hub；回传和最终交付仍由用户在提交表单中明确选择 Session 产物和文件。

任务材料中的 HTML/HTM 文件准备到本机后，复用 Pinvou 现有 HTML 产出物窗口打开；其他文件继续交给系统关联应用。“打开所在位置”始终只定位本线程已准备的准确材料文件。

## 与 Draft PR #109 的边界

PR #109 只作为部署与连接验证的参考，不直接复用其协议或 UI：

| 可参考 | 不复用 |
|---|---|
| Node 服务部署、健康检查、Nginx 和实时连接联调 | 固定 project token |
| 公网入口与桌面客户端之间的连接经验 | `peer_id` 身份模型 |
| 旧演示隔离和回归检查方法 | JSON 权威状态和内存待投递队列 |
| | `task_create/task_accept` A2A 语义 |
| | 聊天框 `@成员` 派活和单语 mock UI |

新 Hub 使用独立服务、数据库、附件目录、端口和公网路径；旧服务未明确停用前保持不变。
