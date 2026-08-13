# 团队接力演示阶段实施计划

> 状态：批次 0—4 的实现已落盘并通过自动化验证；批次 5 的无凭据容器/Nginx 模板已完成本地构建与健康检查，但尚未部署，多设备真实验收门槛待执行
> 目标：用最小真实后端在两到三台 Pinvou 设备之间打通“派活 → Handoff → 回传 → 最终交付 → 要求补充/接收结束”
> 边界：本阶段只实现人到人的可靠接力，不接入 Agent 自动执行，不建设项目看板或 Agent-to-Agent

## 1. 结论

演示阶段采用一个独立、可自托管的 `Team Relay Hub`，由 47 演示计算节点承载；8 公网入口节点继续负责 HTTPS 和反向代理。Hub 不复用现有 Remote Relay，也不把现有 Draft PR #109 的 A2A 任务模型直接合入。

第一阶段以一条三人纵向链路为唯一完成标准：

```text
A 派活给 B
→ B Handoff 给 C，责任仍在 B
→ C 回传给 B
→ B 最终交付给 A
→ A 要求补充
→ B 再次交付
→ A 接收并结束
```

演示阶段完成后，三台设备重启仍应看到一致的线程、接力路径和最终交付；断网重试不能产生重复动作；未被用户明确选择的本地文件不能离开设备。

## 2. 已有基础与复用边界

### 2.1 已有前端

当前 `feat/team-handoff-tab` worktree 已从交互预览接通真实 Team Relay：

- 团队接力入口；
- 需要我处理、等待对方、全部接力、已结束、已归档，以及全部接力中的来源筛选；
- 工作线程与接力路径；
- 派活、当前持有人更新进展、Handoff、回传、最终交付、要求补充、接收结束；
- 明确选择文件和私人 Agent 工作区提示；
- 中、英、日三语和桌面/窄屏适配。

`team-relay-state.js` 保留共享纯函数：它从权威线程、动作日志和当前用户推导 `category`、`status` 和 `banner`，不把这些 UI 字段保存到服务端。生产入口不再读取示例任务。

### 2.2 现有 Remote Relay

Remote Relay 保持原职责：远程浏览器访问一台 Pinvou 桌面端。它不保存团队线程、工作包、成员或文件，不承担 Team Relay Hub 的任何业务状态。

### 2.3 Draft PR #109

PR #109 已验证以下能力，可以作为实现参考：

- Node + WebSocket 服务在现有公网入口下运行；
- 桌面端能够注册连接并收到实时消息；
- systemd、Nginx、健康检查和浏览器/客户端联调路径可行；
- 离线成员发现和简单待投递队列可以完成演示。

但不直接采用以下部分：

- 固定 project token；
- 以 `peer_id` 代替用户、设备和成员关系；
- JSON 状态文件和最多 100 条的内存待投递队列；
- `task_create/task_accept` 的 A2A 任务语义；
- 通过聊天输入框 `@成员` 创建任务；
- 直接访问 bridge、单语文案和 mock 工作台；
- 缺少幂等、revision、确认、可靠重连和可靠文件模型的协议。

PR #109 当前部署实例继续保持独立。新演示 Hub 使用新的服务目录、数据目录、端口和公网路径；迁移完成前不停止或覆盖旧服务。

2026-08-04 的只读现场回查显示：旧 `/pinvou3/collaboration/` 实例实际运行在 8 公网入口节点的 `127.0.0.1:8790`，47 节点没有对应 systemd 服务或 8790 监听。后续部署必须以现场回读为准，不能按 PR 描述推断目标主机。

## 3. 演示范围

### 3.1 必须实现

- 管理员创建一个演示团队的首次邀请；
- A 使用一次性邀请码加入并成为团队 owner；
- A 邀请 B、C 加入团队；
- 成员列表和基本在线/最近同步状态；
- 创建工作线程并派活；
- 单一当前行动人；
- 当前行动人可追加进展说明和明确选择的文件，动作进入历史但不移动接力棒；
- Handoff 给协作者并明确 `return_to`；
- 演示阶段只允许负责人把工作 Handoff 给一名协作者，协作者完成后必须回传，暂不支持嵌套 Handoff；
- 回传给原负责人；
- 负责人向发起人最终交付；
- 发起人要求补充或接收结束；
- 说明、外部链接和明确选择的小文件；
- 服务端稳定动作历史；
- 客户端启动和重连时从 Hub 重新取得权威状态；
- 基础桌面通知；
- 三设备、断网、重启和重复提交验证。

### 3.2 明确不做

- Agent 自动接活、自动 Handoff 或自动交付；
- 共享 Agent 对话、Session、workspace 或本地目录；
- 多人并行接棒；
- 协作者继续 Handoff 给第四人等嵌套接力；
- 转交最终责任；
- 项目、目标、截止时间、优先级和看板；
- 邮箱、手机、企业 SSO 或完整账号找回；
- 在线共同编辑和 CRDT；
- 大文件、文件版本合并和在线预览转换；
- 多 Hub 集群、Redis、消息队列和 Kubernetes；
- 与 PR #109 的旧任务协议保持兼容。

以下可靠性能力明确放到演示通过之后，不进入第一次打通的关键路径：

- 发送方离线时的持久化 Outbox；
- 本地完整线程数据库；
- 基于 cursor 的增量重放；
- 多设备共用同一 Principal；
- 自动后台重试和复杂冲突合并。

演示仍支持“接收方离线”：动作先提交到 Hub，接收方下次启动或重连后通过完整刷新看到工作。发送方离线时则明确提示未发送，不显示为已送达。

## 4. 总体架构

```text
┌──────────────────┐
│ Pinvou A / B / C │
│                  │
│ React Team Relay │
│       ↓          │
│ Tauri team_relay │
│  ├─ REST Client  │
│  ├─ SSE Client   │
│  └─ Credential   │
└────────┬─────────┘
         │ HTTPS
         ▼
┌──────────────────────┐
│ 8 公网入口节点        │
│ Nginx + TLS           │
│ /pinvou3/team-relay/  │
└────────┬─────────────┘
         │ 受控反向隧道
         ▼
┌─────────────────────────────┐
│ 47 演示计算节点              │
│                             │
│ Team Relay Hub              │
│  ├─ Enrollment / Team       │
│  ├─ Thread / Action         │
│  ├─ Bootstrap / SSE         │
│  ├─ Attachment              │
│  └─ Authorization           │
│            │                │
│     PostgreSQL + File Store │
└─────────────────────────────┘
```

### 4.1 服务端落位

- 仓库目录：`team-relay-hub/`；
- 服务目录：`/opt/pinvou-team-relay/current`；
- 数据目录：`/var/lib/pinvou-team-relay/`；
- 默认仅监听：`127.0.0.1:8791`；
- 公网路径：`https://pinvou.com/pinvou3/team-relay/`；
- 数据库不监听公网；
- 服务名、数据目录和端口均与 Remote Relay、PR #109 演示服务隔离。

真实主机地址、SSH 凭据、数据库口令和 token 只进入部署环境，不写入仓库、示例、日志或 PR。

### 4.2 服务端技术基线

演示阶段沿用仓库已有 Node 20 和 WebSocket/HTTP 测试经验，Hub 保持单进程、无状态业务服务：

- Node 20 ESM；
- HTTP JSON API；
- PostgreSQL 作为权威数据源；
- SSE 作为服务端到客户端的实时提示；
- 文件通过流式 HTTP 上传和鉴权下载；
- Docker Compose 运行 Hub 与 PostgreSQL；
- 所有数据库迁移版本化并可重复执行。

Hub 不把线程或待投递动作仅保存在内存。SSE 断开不影响业务事实，客户端可始终通过同步游标补齐。

### 4.3 Pinvou 客户端落位

- React：`pinvou3-app/src/features/team-relay/`；
- Rust：`pinvou3-app/src-tauri/src/features/team_relay/`；
- Tauri 命令：`pinvou3-app/src-tauri/src/app/commands/team_relay.rs`；
- Tauri/Web 适配：`pinvou3-app/src/platform/{tauri,web}/`；
- 凭据：复用项目 `credential_store`，不把 access token 写入普通 JSON；
- 网络：复用已有 `reqwest`；
- Web 宿主在本阶段显式返回 unsupported，不伪装成功。

React 不直接访问 Tauri 全局对象或自行持有服务端 token。业务组件只消费稳定的 team-relay bridge/state 接口。

## 5. 身份与加入团队

### 5.1 默认方案

演示阶段不建设邮箱账号系统，采用“Principal + Device + 单次邀请”模型：

- `Principal` 表示一个真实成员；
- `Device` 表示该成员的一次 Pinvou 安装；
- 首次 owner 邀请由服务器管理命令生成；
- owner 可以在 Pinvou 中生成 B、C 的成员邀请码；
- 邀请码随机、单次使用、可过期、服务端只保存哈希；
- 成员输入邀请码后，服务端创建 Principal、Device 和 Membership；
- 服务端签发不透明 device token，客户端存入系统凭据存储；
- 成员退出或管理员撤销设备后，旧 token 不能继续读取或提交新内容。

### 5.2 演示阶段限制

- 一次成员邀请默认创建一个新 Principal；
- 同一成员的第二台设备和身份恢复不进入本轮；
- 显示名允许重复，界面同时保留短成员 ID；
- 不允许匿名公共注册；
- 不把邀请或 token 放在 URL 查询参数和日志中。

Hub 地址必须是用户可配置的公开设置。官方演示地址和自托管地址使用同一协议，客户端不能依赖私有 DNS、内部 IP 或仅官方部署才存在的接口。首次连接和首次上传前明确告知用户将访问团队服务并发送其选中的内容。

## 6. 权威数据模型

### 6.1 Team

| 字段 | 含义 |
|---|---|
| `id` | 随机团队 ID |
| `name` | 团队显示名 |
| `created_by` | 首位 owner Principal；服务器创建团队后、owner 领取前可暂为空 |
| `created_at` | 创建时间 |

### 6.2 Principal / Device / Membership

| 对象 | 核心字段 |
|---|---|
| `Principal` | `id`, `display_name`, `created_at`, `disabled_at` |
| `Device` | `id`, `principal_id`, `token_hash`, `last_seen_at`, `revoked_at` |
| `Membership` | `team_id`, `principal_id`, `role`, `joined_at`, `removed_at` |
| `Invite` | `id`, `team_id`, `token_hash`, `role`, `expires_at`, `claimed_at` |

角色只保留：

- `owner`：管理邀请和成员；
- `member`：参与接力。

### 6.3 WorkThread

| 字段 | 含义 |
|---|---|
| `id` | 工作线程 ID |
| `team_id` | 所属团队 |
| `title` | 工作标题 |
| `initiator_id` | 发起人 |
| `owner_id` | 对最终结果负责的人 |
| `current_actor_id` | 当前接棒人；结束后为空 |
| `return_to_id` | 当前行动完成后应回给谁 |
| `final_recipient_id` | 最终交付对象 |
| `phase` | `active`, `awaiting_acceptance`, `closed` |
| `revision` | 乐观并发版本号 |
| `created_at`, `updated_at` | 时间 |

服务端不保存中文状态、页面 banner 或收发箱 category。

### 6.4 RelayAction

动作只追加、不原地修改：

| 字段 | 含义 |
|---|---|
| `id` | 动作 ID |
| `thread_id` | 所属线程 |
| `sequence` | 线程内单调递增序号 |
| `kind` | 动作类型 |
| `actor_id` | 操作者 |
| `recipient_id` | 本次接收人 |
| `return_to_id` | 本次动作完成后的回传对象 |
| `message` | 用户确认发送的说明 |
| `base_revision` | 客户端提交时看到的线程版本 |
| `idempotency_key` | 客户端请求幂等键 |
| `request_hash` | 规范化业务请求的 SHA-256，用于识别同 key 不同请求 |
| `created_at` | 服务端时间 |

动作类型：

```text
assign
note
handoff
return
final_deliver
request_changes
accept
```

唯一约束至少包含：

```text
(actor_id, idempotency_key)
(thread_id, sequence)
```

### 6.5 ThreadParticipant

| 对象 | 核心字段与用途 |
|---|---|
| `ThreadParticipant` | `thread_id`, `principal_id`, `first_action_sequence`；记录曾被明确加入线程的成员，用于线程和附件授权 |

成员必须同时保持有效 Team Membership 且属于 ThreadParticipant，才能读取线程和附件。被移出团队后不能继续同步新内容；历史保留和导出在正式阶段另行设计。

### 6.6 Attachment / ActionAttachment

| 字段 | 含义 |
|---|---|
| `id` | 附件 ID |
| `team_id` | 所属团队 |
| `uploaded_by` | 上传者 |
| `storage_key` | 服务端内部对象键 |
| `original_name` | 用户看到的文件名 |
| `content_type` | 检测后的 MIME |
| `byte_size` | 大小 |
| `sha256` | 内容校验 |
| `deleted_at` | 保留期结束并删除文件内容的时间；元数据仍保留 |
| `created_at` | 上传时间 |

`ActionAttachment` 将附件固定到一次已发送动作。发送后不能静默替换同一个文件 ID 的内容。

## 7. 状态迁移与权限

| 当前状态 | 动作 | 允许者 | 结果 |
|---|---|---|---|
| 新建 | `assign` | 发起人 | owner/current=B，return_to=A，phase=active |
| active | `handoff` | owner 且 current=owner | owner 不变，current=C，return_to=owner |
| active | `return` | 当前协作者且 return_to=owner | current=owner，return_to=final_recipient |
| active | `final_deliver` | owner 且 current=owner | current=final_recipient，phase=awaiting_acceptance |
| awaiting_acceptance | `request_changes` | final_recipient | current=owner，return_to=final_recipient，phase=active |
| awaiting_acceptance | `accept` | final_recipient | current/return_to 为空，phase=closed |
| active/awaiting_acceptance | `note` | 线程参与者 | 追加过程记录，不移动接力棒；桌面端“更新进展”入口仅在 active 时向 current_actor 展示 |

所有业务写入在同一个数据库事务中完成：

1. 验证 token 和有效团队成员；
2. 先检查 `(actor_id, idempotency_key)`：相同请求返回同一动作与当前线程，不同请求返回冲突；
3. 锁定线程并验证参与权限、动作权限和 `expected_revision`；
4. 写入 RelayAction；
5. 更新 WorkThread 和 revision；
6. 提交事务；
7. 向在线客户端发送 SSE 提示。

SSE 发送失败不回滚已提交动作；客户端重连后执行完整 bootstrap 恢复。由于演示团队和线程量很小，完整刷新优先于提前建设游标重放系统。

## 8. 工作箱推导规则

客户端先从 `phase`、`current_actor` 和个人归档偏好推导“待处理、待验收、对方处理中、已结束、已归档”互斥状态，再生成行动视图：

1. “需要我处理”包含待处理和待验收，待验收优先排列；
2. “等待对方”包含当前 Principal 发起或负责、但当前轮到其他人的未结束线程；
3. “全部接力”包含当前 Principal 参与的所有未归档线程；
4. 已结束和已归档分别提供历史与个人归档视图。

“全部接力”再按发起人和负责人提供“我派出的、派给我的、我协作的”来源筛选。来源筛选可以与行动状态重叠，不再伪装成互斥状态分类。

服务端返回结构化线程字段，客户端使用共享纯函数推导状态和视图。服务端列表 API 可以提供相同语义的筛选参数，但不能生成与语言相关的状态文案。

## 9. API 与实时同步契约

### 9.1 基础与身份

```text
GET    /v1/healthz
POST   /v1/enrollments/claim
GET    /v1/me
GET    /v1/teams/{team_id}/members
POST   /v1/teams/{team_id}/invites
DELETE /v1/teams/{team_id}/members/{principal_id}
```

### 9.2 工作线程

```text
GET  /v1/threads?team_id=...&view=...
POST /v1/threads
GET  /v1/threads/{thread_id}
POST /v1/threads/{thread_id}/actions
```

动作请求至少包含：

```json
{
  "kind": "handoff",
  "recipient_id": "principal_...",
  "message": "请补充两个案例，完成后回给我。",
  "attachment_ids": ["attachment_..."],
  "expected_revision": 3,
  "idempotency_key": "client-generated-random-id"
}
```

客户端只在 `handoff` 时指定接棒人。`return_to_id`、owner、current actor、phase 和动作 sequence 均由 Hub 根据状态机生成，客户端不能覆盖。

冲突返回 `409 revision_conflict`，并携带当前 revision；幂等重放不产生新动作，返回第一次成功的同一动作、当前权威线程和 `replayed=true`。

### 9.3 文件

```text
POST /v1/attachments
GET  /v1/attachments/{attachment_id}
GET  /v1/attachments/{attachment_id}/content
```

演示阶段默认限制：

- 单文件最大 20 MiB；
- 单动作最多 10 个文件；
- 单团队总文件上限 2 GiB；
- 默认允许 `md/txt/pdf/docx/xlsx/pptx/csv/png/jpg/jpeg/webp/zip`，压缩包只作为不透明文件保存，不在服务端自动解压；
- 拒绝可执行文件、脚本和 MIME/扩展名明显冲突的上传；
- 流式写入临时文件，完成 SHA-256 和大小校验后原子落位；
- 文件名不能作为磁盘路径；
- 下载必须重新校验当前成员和线程参与权限；
- 不允许任意路径读取、符号链接逃逸和可执行文件自动运行；
- 上传失败、动作未发送的孤儿附件由定时任务清理。

演示环境中，未绑定动作的附件 24 小时后清理；已结束线程的附件默认保留 30 天，线程元数据和动作历史在演示期内不自动删除。界面和自托管文档必须展示这一策略，正式阶段再设计归档、导出和合规删除。

### 9.4 Bootstrap 与 SSE

```text
GET /v1/bootstrap?team_id=...
GET /v1/events?team_id=...
```

- `/bootstrap` 返回成员、所有未结束线程和演示保留期内的已结束线程；
- `/events` 使用 SSE，只发送“有新数据”的轻量事件、thread ID 和 revision；
- 客户端收到 SSE 后读取对应线程，断线重连后重新执行 `/bootstrap`；
- SSE 使用 Authorization header，不把 access token 放在 URL；
- SSE 不是业务真相源，事件遗漏不会造成线程丢失。

## 10. 客户端演示状态

客户端只持有当前运行期状态：

- 当前身份和团队公开信息；
- bootstrap 返回的成员、线程和动作；
- 当前表单草稿；
- 正在提交的 `idempotency_key`；
- 上传进度和最近错误。

发送状态：

```text
draft → sending → acknowledged
             └──→ retryable_failed
             └──→ blocked_conflict
```

网络失败后保留当前页面草稿并允许用户用同一个 `idempotency_key` 手动重试。应用退出时尚未确认的发送不保证自动恢复；重新打开后从 Hub 获取权威状态。持久化本地 Outbox 在演示通过后的可靠性阶段实现。

界面必须区分：

- 尚未发送；
- 正在发送；
- 已被服务器确认；
- 对方已同步；
- 冲突或权限失败。

不能因为本地写入成功就显示“对方已收到”。

## 11. 部署计划

### 11.1 47 演示节点

使用独立 Docker Compose 项目：

```text
team-relay-hub
postgres
```

约束：

- Hub 和 PostgreSQL 设置资源上限；
- PostgreSQL 仅在内部网络监听；
- Hub 仅映射到 `127.0.0.1:8791`；
- 数据卷固定在独立数据目录；
- 服务使用非 root 用户；
- 日志不记录 Authorization、邀请码、完整消息正文和文件内容；
- 健康检查区分进程存活与数据库可用；
- 每日 `pg_dump` 和附件清单备份到节点外；
- 首次部署前增加必要的紧急 Swap 或明确 OOM 恢复策略。

### 11.2 8 公网入口

- 新增独立反向隧道到 47 的 `8791`；
- Nginx 新增 `/pinvou3/team-relay/`；
- 保留 Authorization header；
- SSE 关闭代理缓冲并设置合理 idle timeout；
- 上传请求限制与 Hub 保持一致；
- 不修改 `/pinvou3/remote/` 和 `/pinvou3/collaboration/`；
- 部署前后分别验证 Nginx 配置、TLS、健康检查和现有入口回归。

回滚时只撤销新 Nginx location、停止新反向隧道和回退 Hub `current` 版本；不删除 PostgreSQL 和附件数据卷，不修改旧 Remote Relay 或 collaboration 服务。数据库迁移必须在部署前备份，并说明是否支持向下回退。

### 11.3 配置和密钥

仓库只提供 `environment.example` 和不含真实地址/凭据的部署模板。真实配置通过服务器环境或受控密钥文件注入，权限限制为服务账号可读。

## 12. 实施批次

### 批次 0：设计与契约

产出：

- 本计划评审通过；
- 状态迁移表冻结；
- OpenAPI/JSON Schema 契约；
- 数据库迁移草案；
- 线程和动作错误码；
- PR #109 语义复用/废弃清单。

完成门槛：A、B、C 三方对每一步“谁负责、轮到谁、回给谁”理解一致。

### 批次 1：Hub 纵向核心

产出：

- `team-relay-hub/`；
- PostgreSQL schema 和 migration；
- 一次性邀请和 device token；
- Team、Member、Thread、Action API；
- 幂等和 revision 冲突；
- bootstrap 完整刷新和 SSE 提醒；
- 三个模拟客户端的集成测试。

完成门槛：不依赖 Pinvou UI，API 测试能跑通 A → B → C → B → A。

### 批次 2：文件工作包

产出：

- 流式上传和鉴权下载；
- 文件限制、SHA-256、原子落位和孤儿清理；
- 动作与附件稳定关联；
- 恶意路径、越权下载和超限测试。

完成门槛：B 只能看到 A 明确发送的文件，不能构造路径读取其他文件。

### 批次 3：Pinvou Rust 客户端

产出：

- `features/team_relay/`；
- 凭据存储；
- REST、SSE、bootstrap 与重连；
- Tauri 命令和事件；
- 平台能力与 unsupported 状态；
- Rust 单元和集成测试。

完成门槛：两个无 UI 的客户端实例可重连并从 Hub 恢复线程；接收方离线期间产生的工作在其下次启动时可见。

### 批次 4：前端接入

产出：

- 删除开发预览数据源；
- 使用真实成员、收发箱和线程；
- 接通派活、Handoff、回传、交付、补充和接收；
- 接通文件选择、上传进度和失败恢复；
- 接通加入团队和邀请；
- 保持中、英、日文案；
- 正式功能未配置时显示引导，而不是假数据。

完成门槛：生产构建可显示真实 Team Relay，且没有 demo 假任务。

### 批次 5：部署与真实演练

产出：

- 47 独立 Hub/PostgreSQL 环境；
- 8 HTTPS 路由和反向隧道；
- 健康检查、备份、日志和恢复步骤；
- 三台真实设备演练记录；
- 断网、重启、重复点击、权限撤销和文件失败记录。

完成门槛：固定验收旅程全部通过，且 Remote Relay、OTA 和旧 collaboration demo 无回归。

## 13. PR 拆分建议

1. `docs(team-relay): 固定演示阶段架构与协议契约`
2. `feat(team-relay-hub): 实现团队身份与接力线程核心`
3. `feat(team-relay-hub): 实现文件工作包与重连刷新`
4. `feat(team-relay): 实现 REST/SSE 客户端与凭据存储`
5. `feat(team-relay): 接通真实团队收发箱和线程`
6. `ops(team-relay): 增加自托管和演示部署模板`

服务端、Rust 客户端和前端不在一个超大 PR 中一次完成。每个 PR 都必须保持已有产品可构建，并包含对应层级的测试。

关键路径为：

```text
协议契约
→ Hub 身份与线程核心
→ Rust REST/SSE 客户端
→ 真实前端接入
→ 47/8 部署
→ 三设备验收
```

文件工作包可以在 Hub 线程核心稳定后，与 Rust 客户端基础工作并行；生产 UI 不应在身份、线程和同步契约稳定前直接对接临时接口。

## 14. 验收矩阵

### 14.1 正常链路

- A 邀请 B、C；
- A 派活给 B；
- B Handoff 给 C，owner 仍是 B；
- C 回传，current 回到 B；
- B 最终交付，A 进入待接收；
- A 要求补充，current 回到 B；
- B 再交付；
- A 接收结束；
- 所有设备重启后线程路径一致。

### 14.2 可靠性

- B 离线时 A 派活，B 上线后同步到同一线程；
- 客户端对同一个 `idempotency_key` 重试不会重复动作；
- 两个设备基于同一 revision 提交，只有一个成功，另一个收到冲突；
- SSE 中断后重新 bootstrap，不丢业务线程；
- Hub 重启后线程、动作和邀请状态仍存在；
- PostgreSQL 暂时不可用时 Hub 不返回假成功。

### 14.3 权限与隐私

- 非团队成员不能读取团队线程；
- 非线程参与者不能读取附件；
- 非当前行动人不能 Handoff 或回传；
- 非 owner 不能最终交付；
- 非最终接收人不能接受或要求补充；
- 被撤销设备不能继续同步；
- 未选择的本地文件、Session 和 Agent 对话不上传；
- 日志不出现 token、邀请码和文件内容。

### 14.4 文件

- 正常小文件上传、校验、下载；
- 超限文件在上传前和服务端均被拒绝；
- 重名文件不互相覆盖；
- 上传中断可安全失败或重试；
- 未绑定动作的附件按保留策略清理；
- 路径穿越、符号链接和越权下载被拒绝。

### 14.5 既有功能回归

- Remote Relay 健康与远程 WebUI；
- OTA 健康入口；
- PR #109 旧 collaboration 演示入口在明确停用前保持可用；
- Pinvou UI lint/build、三语测试和架构守卫；
- Rust fmt/check/test；
- Hub test、数据库 migration 和端到端旅程。

## 15. 扩容兼容性

演示阶段不实现集群，但从第一天保留以下边界：

- Hub 无内存权威状态，可启动多个实例；
- PostgreSQL 是线程、动作和成员的单一真相源；
- 附件通过 `Storage` 接口访问，演示实现是本地磁盘，后续可切换 OSS/S3；
- 实时 SSE 只是提示，不能成为唯一交付通道；
- 通知入口保持独立边界，后续可增加服务端 Outbox、队列和 Worker；
- token 和邀请按服务端哈希保存，支持撤销和轮换；
- API 全部带版本前缀。

演示通过后的第一项可靠性增强是增加本地 SQLite cache、持久化 Outbox 和服务端增量 cursor；这些能力不改变现有 Thread/Action/API 语义。

后续扩容顺序：

1. 文件迁移到 OSS/S3；
2. PostgreSQL 迁移到独立或托管实例；
3. Hub 多实例和负载均衡；
4. Redis/队列承担跨实例通知与异步 Worker；
5. 数据库高可用、读副本、事件表归档和灾备。

## 16. 开工前门槛

进入批次 1 前必须确认：

- 本计划和状态迁移表获得产品确认；
- 新 Hub 与 PR #109 的关系确定为“参考并独立实现”，不直接合并旧 UI/协议；
- 公网路径和演示数据保留期确认；
- 演示成员加入方式确认使用一次性邀请码；
- 文件限制和允许类型采用本计划默认值；
- 47 与 8 的部署变更分别具备备份、回滚和独立授权；
- 服务端新增依赖完成安全和维护状态检查。

## 17. 演示完成定义

只有同时满足以下条件，才能称为“演示阶段打通”：

1. 三个真实成员和三台设备跑通完整接力；
2. 服务端重启、客户端重启和接收方离线均不丢线程；发送方离线时明确显示未发送；
3. 重试不会生成重复动作；
4. 当前行动人、负责人和回传对象在三端一致；
5. 文件只在用户明确选择并确认后上传；
6. 要求补充能回到原负责人，再次交付后才能结束；
7. 生产构建不再展示 mock 数据；
8. 47 Hub、8 公网入口、备份和恢复步骤可重复执行；
9. Remote Relay、OTA 和其他 Pinvou 功能没有回归；
10. 所有未验证项、容量边界和已知风险在演示说明中如实记录。
