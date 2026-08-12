# Team Relay 演示协议

## 1. 术语与权威边界

- `Principal`：同一团队接力服务内的一个邮箱账户；邮箱唯一，`display_name` 是可修改且允许重名的名字。同一账户加入多个 Team 时复用同一个 Principal。
- `Device`：一次 Pinvou 安装及其可撤销凭据，只用于底层认证，不参与派活、Handoff 或交付，也不要求用户命名。
- `Membership`：Principal 在 Team 中的有效成员关系。
- `Membership.role=owner`：团队管理员，可以管理团队加入码或移除成员；本文称为 team owner。
- `WorkThread`：一次从派活到接收结束的接力线程。
- `RelayAction`：线程中不可变、只追加的业务动作。
- `DeliveryPackage`：随 RelayAction 固化的一次交付快照，包含说明、文件角色、可选语义子类型、下一步、未决问题和 Git 引用；动作 ID 与 sequence 就是交付包标识。
- `owner`：对最终结果负责的人；Handoff 不改变 owner。
- `current_actor`：当前应该行动的人。
- `return_to`：当前行动完成后应该回给的人。
- `final_recipient`：最终交付和验收的人。

PostgreSQL 中的 WorkThread、RelayAction、成员和附件关系是唯一权威状态。SSE、界面分类、在线状态和页面文案都不是业务真相源。

team owner 和 WorkThread owner 是两个独立概念。例如演示链路中 A 可以是 team owner，而被派活的 B 是该 WorkThread 的 owner。

## 2. 身份与加入团队

1. 桌面端显示内测入口前不内置体验密码或其摘要；用户提交的体验密码通过 HTTPS 发送到 `POST /v1/beta-access/verify`，由 Hub 使用仅存在于服务器 Secret 中的 `TEAM_RELAY_BETA_ACCESS_PASSWORD_HASH` 校验。失败尝试按客户端地址限流，成功后客户端只在本机保存已开启标记。
2. 团队接力服务显式启用 `TEAM_RELAY_ALLOW_SELF_SERVICE_ENROLLMENT=true` 时，桌面端先调用 `POST /v1/enrollments/account`，只提交邮箱、名字和自动生成的设备名。新邮箱创建 Principal 和 Device；已有邮箱复用原 Principal、更新名字并为当前安装创建新的 Device。该能力默认关闭。
3. 当前无验证码的邮箱复用仅用于受控内测，并不证明邮箱所有权；扩大开放范围前必须替换为可验证的身份流程。邮箱一旦绑定不能通过个人资料修改。
4. 兼容部署仍可由服务器管理员通过本地命令创建 Team 和一次性 owner 初始化码，再由旧加入流程领取。
5. 邮箱在当前服务内唯一，比较前去除首尾空格并转为小写。不同账户可以使用相同 `display_name`；一个账户只有一个当前名字，调用 `PATCH /v1/me` 修改后对该账户加入的所有团队生效。成员接口不返回完整邮箱，只在重名时允许客户端使用脱敏 `email_hint` 区分。
6. 团队接力服务只在账号登记响应中返回明文 device token；数据库只保存 token 哈希，客户端把 token 存入系统 credential store。设备名由客户端或服务自动生成，不出现在正常登录表单和团队工作流中。
7. 已登记用户在团队页面调用 `POST /v1/teams` 创建团队，继续使用同一个 Principal，并自动成为新团队 owner。
8. `GET /v1/teams` 返回当前 Principal 的所有有效 Membership 及团队摘要；客户端只保存当前选中的 `team_id`，切换团队不更换 device token。重新登录已有邮箱时，客户端恢复最近加入的有效团队。
9. 任一有效成员可通过 `POST /v1/teams/{team_id}/invites` 生成 24 小时内有效的一次性成员邀请码。
10. 已登记用户在团队页面调用 `POST /v1/teams/join`，使用一次性邀请码或固定加入码把现有 Principal 加入另一个 Team。
11. `POST /v1/enrollments/create-team` 和 `POST /v1/enrollments/join` 继续服务旧客户端；新客户端不得在账号登录表单中要求团队名或加入码。
12. 迁移前已有 Principal 保持原 ID、Membership 和历史记录，邮箱暂为空；用户可在认证后通过 `PATCH /v1/me` 一次性绑定邮箱。当前版本不支持更换已绑定邮箱。
13. 桌面端“退出账号”只退出当前设备：停止 SSE 同步，删除系统 credential store 中的 device token 和本机接力连接配置，并清空当前进程中的账号缓存。它不删除 Principal、Membership、接力记录或本机任务材料，也不影响其他设备；用户再次登录后可恢复仍有效的团队。
14. Principal 可以通过 `PUT /v1/me/avatar` 上传独立接力头像，或通过 `DELETE /v1/me/avatar` 恢复名字首字头像。头像跨该 Principal 加入的所有团队生效，不复用专家池头像。未上传时，中文名显示第一个有效汉字，英文名显示大写首字母；客户端按 Principal ID 从连续色域派生稳定的个性化底色，同一成员在不同电脑上保持一致，不使用每台电脑独立的随机色。

桌面端的日常邀请入口由 team owner 生成或轮换可重复使用的团队加入码；一次性成员邀请码接口保留为协议兼容能力，不作为当前界面的默认入口。

固定团队加入码由 Hub 从排除易混字符的字符表中随机生成 8 位，并按 `XXXX-XXXX` 显示；输入不区分大小写，横线和空白可省略。数据库同时保存规范化加入码的 SHA-256 摘要，以及由部署密钥使用 AES-256-GCM 加密的显示值；摘要用于加入校验和团队定位，加密值只用于向有权限的现有成员恢复显示，数据库不保存明文。部署密钥必须独立备份并保持稳定，丢失后只能由 owner 刷新加入码。

加入码默认对所有有效成员可见并持续有效，点击“团队加入码”只读取当前值，不产生新值。只有 team owner 可以刷新加入码、切换为仅 owner 可见或暂停加入；刷新和暂停都会立即使旧码失效，但已加入设备 token 不受影响。移除成员时，如果加入仍启用，Hub 在移除成员的同一事务内自动刷新加入码，避免被移除成员继续使用已知旧码。升级前只保存摘要的旧加入码无法恢复显示，owner 需要刷新一次后才进入可持续查看状态。一个 Hub 可承载多个团队，因此加入码摘要必须全局唯一且不能由客户端自行指定。

短加入码必须和服务端防猜测同时启用：未登记用户按客户端地址、已登记用户按设备凭据计数，10 分钟内连续 5 次提交错误加入码后暂停 15 分钟，Hub 返回 `429 rate_limited` 和 `Retry-After`。反向代理部署只有在代理覆盖可信 `X-Real-IP` 且 Hub 不直接暴露公网时才可启用 `TEAM_RELAY_TRUST_PROXY=true`。旧版长加入码继续按原值校验，以便滚动升级。演示版限流状态保存在单个 Hub 进程内且有容量上限；扩展到多副本前必须改为所有副本共享的限流存储。

一次性邀请码只能成功消费一次。团队列表、线程、附件、头像内容和 SSE 始终按 `team_id` 授权隔离；头像元数据可出现在成员快照中，但 `GET /v1/principals/{principal_id}/avatar?team_id=...` 只有双方都是该团队有效成员时才返回内容。设备被撤销或成员被移除后，旧 token 不得继续读取该团队数据、头像或提交动作，但不影响该 Principal 的其他有效团队。

头像上传最大 5 MiB，只接受经文件内容识别并与扩展名一致的 PNG、JPEG、WebP 和 GIF；SVG 与其他可执行格式不接受。Hub 原样保存通过校验的 GIF，以保留动画，并返回 SHA-256 元数据供桌面客户端下载后校验。头像字节保存在仅服务账号可读的持久化目录，HTTP 响应使用 `nosniff`、私有且不缓存的策略。

## 3. 线程建立

客户端通过 `POST /v1/threads` 提交：

- `team_id`；
- `title`；
- `assignee_id`；
- 可选说明和已上传附件；
- `idempotency_key`。

协议 `0.2` 客户端在 bootstrap 和写请求中发送 `X-Pinvou-Team-Relay-Protocol: 0.2`，Hub 以设备为单位自动登记最近声明的协议能力。只有发送请求声明 `0.2`，且接收人至少有一台未撤销设备在最近 30 天声明过 `0.2` 时，Hub 才选择 `transfer_mode=confirmed`。服务端创建线程并追加第一条 `assign` 动作，但在接收人确认前，发起人仍持有责任和接力棒，待接收人只获得任务摘要：

```text
initiator=A
owner=A
current_actor=A
return_to=A
final_recipient=A
pending_transfer_kind=assignment
pending_sender=A
pending_recipient=B
phase=pending_transfer
revision=1
```

B 提交 `accept_transfer` 后才变为 `owner/current_actor=B`、`return_to=A`、`phase=active`，并获得线程完整材料访问权限。B 拒绝，或 A 撤回、改派后，从未获得材料权限的旧候选人不再保留线程可见性。

接收人的能力未知、过期或仅有旧客户端时，Hub 选择 `transfer_mode=direct_legacy`，派发、Handoff 或改派立即生效。每条 `assign`、`handoff`、`reroute_transfer` 动作持久化实际模式，写响应也返回同名字段，客户端必须以 Hub 结果为准显示“等待确认”或“直接生效”。能力是设备自动上报的协议事实，不提供人工开关，也不按应用版本字符串推断。未发送协议头的旧客户端登记为 `0.1` 并保持直接转交语义，以支持 Hub 与桌面端滚动升级。

客户端不能直接提交或修改 owner、current_actor、return_to、final_recipient、phase、revision 和 action sequence。

## 4. 状态迁移

| 当前状态 | 动作 | 允许者 | 客户端可指定接收人 | 服务端结果 |
|---|---|---|---|---|
| 新建 | `assign` | 发起人 | `assignee_id=B` | confirmed：pending_transfer(assignment)，接力棒仍在 A；direct_legacy：owner/current=B，立即生效 |
| pending_transfer | `accept_transfer` | pending_recipient | 不可指定 | 初次派发：owner/current=B；协作：current=C、owner 不变 |
| pending_transfer | `decline_transfer` | pending_recipient | 不可指定 | 初次派发进入 awaiting_reassignment；协作邀请恢复 active |
| pending_transfer | `withdraw_transfer` | pending_sender | 不可指定 | 初次派发进入 awaiting_reassignment；协作邀请恢复 active |
| pending_transfer / awaiting_reassignment | `reroute_transfer` | pending_sender / initiator | `recipient_id` | 原请求失效；confirmed：新接收人进入 pending_transfer；direct_legacy：接力立即转移 |
| active，current=owner | `handoff` | owner | `recipient_id=C` | confirmed：pending_transfer(handoff)，owner/current 暂时不变；direct_legacy：current=C |
| active，current≠owner | `return` | current_actor | 不可指定 | current=owner，return_to=final_recipient |
| active，current=owner | `final_deliver` | owner | 不可指定 | current=final_recipient，return_to=null，awaiting_acceptance |
| awaiting_acceptance | `request_changes` | final_recipient | 不可指定 | current=owner，return_to=final_recipient，active |
| awaiting_acceptance | `accept` | final_recipient | 不可指定 | current/return_to=null，closed |
| 任意未结束状态 | `note` | 线程参与者 | 不可指定 | 不移动接力棒，revision 仍递增 |
| 任意未结束状态 | `cancel` | initiator | 不可指定 | 清除待确认转移，current/return_to=null，cancelled；必须填写原因 |

桌面端把当前 `active` 线程，以及等待协作者确认且接力棒仍在 owner 手上的 `pending_transfer(handoff)` 线程的 `note` 暴露为“更新进展”。该入口把交付包的可选 `subtype` 固化为 `progress`，客户端仅对这一明确标记显示“进展更新”；字段缺失或为空的既有及通用 `note` 仍显示“补充说明”。Hub 只允许仍持有接力棒的 `current_actor` 在上述状态提交 `note + progress`，其他动作、其他参与者或其他阶段携带该值必须拒绝。提交后新增不可变动作和交付包、revision 递增，但责任关系和待确认转移均不改变。协议仍保留其他线程参与者提交通用 `note` 的能力，避免把 UI 入口约束误当成服务端权限收紧。

演示阶段禁止：

- current_actor 不是 owner 时再次 Handoff；
- 除接受初次派发外修改 owner，或修改 final_recipient；
- 多个 current_actor；
- closed/cancelled 后追加动作；
- 跳过 owner，由协作者直接最终交付；
- `assign` 通过通用动作接口追加。

## 5. 不变量

每次事务提交后必须满足：

1. `phase=pending_transfer` 时 current_actor、return_to、pending_kind、pending_sender 和 pending_recipient 均非空；发送人与接收人不同。
2. `phase=awaiting_reassignment` 时 owner/current_actor 等于 initiator，且没有 pending transfer。
3. `phase=active` 时 current_actor 和 return_to 均非空，且没有 pending transfer。
4. `phase=awaiting_acceptance` 时 current_actor 等于 final_recipient，return_to 为空。
5. `phase=closed/cancelled` 时 current_actor 和 return_to 均为空。
6. 未结束线程的 initiator、owner、current_actor、return_to、final_recipient、pending_sender 和 pending_recipient 都是有效团队成员；closed 历史可以保留已移除成员的归属记录。
7. 能读取线程的人必须同时是有效团队成员和 ThreadParticipant；待确认接收人可以读取摘要，但 `material_access_at` 为空时不能读取附件元数据或内容。拒绝、撤回或被改派后，尚未获得材料权限的候选人从 ThreadParticipant 移除。
8. `revision` 等于最后一条动作的 `sequence`，两者从 1 单调递增。
9. RelayAction 只追加，不修改、不删除；拒绝、撤回、改派和展示修订都通过新动作表达。
10. 附件只能绑定一次已成功提交的动作，绑定后内容不可替换。
11. 同一 `(actor_id, idempotency_key)` 只对应一个规范化请求和一个结果。
12. 收发箱分类由结构化线程字段和当前 Principal 的 ThreadPreference 推导，服务端不保存本地化状态文案或 UI category。

## 6. 写入事务与并发

通用写入按以下顺序执行：

1. 验证 device token、Device、Principal 和 Membership。
2. 按 `(actor_id, idempotency_key)` 查询历史结果。
   - request hash 相同：不再写入，返回同一动作、当前权威线程和 `replayed=true`，不再执行 revision 校验。
   - request hash 不同：返回 `409 idempotency_key_reused`。
3. `SELECT ... FOR UPDATE` 锁定线程。
4. 验证线程参与权限、动作权限和 `expected_revision`。
5. 验证附件归属、状态、数量和团队容量。
6. 追加 RelayAction，绑定附件，更新 WorkThread 和 revision。
7. 提交数据库事务。
8. 提交成功后发送 SSE 刷新提示。

SSE 失败不能回滚业务事务。客户端在 SSE 断开、应用启动和重新联网后执行 bootstrap。

## 7. 幂等规范化

- `idempotency_key` 使用客户端生成的随机 UUID，每次用户确认的业务提交生成一次。
- 网络失败后的手动重试必须复用原 key。
- Hub 对请求体做确定性规范化后计算 SHA-256 `request_hash`；忽略 JSON 对象键顺序，但不忽略业务字段。协议 `0.2` 的创建线程和动作请求把协议版本纳入哈希，避免同一幂等键跨直接转交与确认式转交重放。
- 创建线程的幂等重放返回同一个 WorkThread、第一条 assign 动作和 `replayed=true`。
- 动作提交的幂等重放返回同一条 RelayAction 和当前权威 WorkThread；线程可能已经被后续动作推进，客户端不能把旧动作当成当前状态。
- 上传附件单独使用 `upload_id`；相同 upload_id 与相同内容可返回原附件，不同内容返回冲突。

## 8. 附件生命周期

1. 用户明确选择文件后，客户端先本地检查类型和 20 MiB 上限。
2. Hub 流式写临时文件，同时计算大小和 SHA-256。
3. 检测 MIME、扩展名和类型策略后原子落位，创建未绑定 Attachment。
   文本白名单包含文档文本及 HTML/CSS/JavaScript/TypeScript/JSON/YAML 代码快照；代码类统一存为 `text/plain` 响应，不能作为 Hub 可执行页面或脚本提供。
4. 只有上传者可以把未绑定附件加入自己的下一次动作。
5. 动作事务成功后附件被永久绑定；失败时仍保持未绑定。
6. 未绑定附件 24 小时后清理；演示环境中 closed/cancelled 线程附件默认保留 30 天。

孤儿附件清理时删除元数据和文件。已结束线程超过保留期后只删除文件内容并标记 Attachment `deleted_at`，保留动作与附件名称、大小和 SHA-256 供历史审计；再次下载返回 `410 attachment_expired`。

文件名只作展示字段，不能参与磁盘路径解析。下载每次重新验证有效 Membership 和 ThreadParticipant。

## 8.1 交付包、过程文件与 Git 引用

每次 `assign`、`handoff`、`return`、`final_deliver` 等动作都是一个不可变交付包，而不是对共享工作区的持续编辑。交付包可以包含：

- 本次说明、接收人拿到后应做什么，以及仍未解决的问题；
- 用户明确上传并绑定到本动作的文件快照；
- 一个或多个 Git 引用：仓库 HTTP(S) 地址、固定 commit SHA、辅助性的 branch/PR/路径和说明。

附件角色按动作默认推导：派活和要求补充为 `input`，Handoff/回传为 `intermediate`，最终交付/接收为 `final`，补充说明/取消原因为 `evidence`。角色描述文件在本次交付中的用途，不改变附件本身的不可变性。

Git 仓库仍以 Git 服务为权威来源。Hub 只保存引用，不克隆仓库、不保存凭据、不拉取、不推送、不合并；branch 是可变辅助信息，固定 commit SHA 才能标识本次实际交付的版本。线程关闭后，以 `accept` 之前最近的一次 `final_deliver` 为已验收交付包；界面应明确展示其 sequence，而不是复制或覆盖历史文件。

客户端可以为每个线程建立本机私有任务目录，缓存该线程已获授权下载的附件，并记录一个本机 Agent Session 关联。一个线程的附件统一放在该任务目录的 `materials/` 下，文件名以首次引用它的 RelayAction sequence 作为 `P01-` 一类前缀；同一动作内的安全文件名仍冲突时追加附件短指纹，不得覆盖任一不可变快照。旧版 `inputs/<附件指纹>/` 缓存可在校验附件大小和 SHA-256 后迁入 `materials/`，清单成功写入后才清理已迁移的旧文件。关联可在本机附带 Agent/ACP 标识、已带入附件 ID 和最近同步的动作 sequence，以便继续工作时只建议新增上下文；这些字段不是 Hub 协议字段，不得进入 bootstrap、动作、SSE 或附件元数据。打开 Agent 只准备用户勾选的已共享附件，并把新增说明和 Git 引用预填到本机关联 Session，不自动发送消息，也不代表执行、回传或交付；任何新文件和 Git 引用仍须通过新的显式动作提交。

客户端可以把线程内所有文件快照和 Git 引用汇总为常驻材料面板，但必须按 RelayAction sequence 保留其交付包、提交人、动作和材料角色，不得把汇总结果当成新的服务端状态。面板中的“已准备”“已带入 Agent”和“有新 Agent 上下文”均为本机状态；附件同名不能证明版本替代关系，在协议没有显式逻辑成果 ID 或 supersedes 关系前不得推断“最新版”或覆盖旧材料。

旧客户端省略 `delivery` 时，Hub 生成上述默认角色与空交付说明，因此这一扩展保持协议 `0.1` 的输入兼容性。

## 9. Bootstrap 与 SSE

- `GET /v1/bootstrap?team_id=...` 返回成员、演示保留期内的线程及动作摘要。
- 客户端的“我的工作”依次读取当前 Principal 已加入团队的 bootstrap，在本机按团队标签汇总；团队筛选只改变展示，不改变当前任务状态。
- `GET /v1/events?team_id=...` 只订阅指定团队；省略 `team_id` 时订阅当前 Principal 所有有效团队。`thread.changed` 供参与者刷新权威快照；`thread.attention` 只定向发送给下一行动人或被撤回、被改派的受影响成员，用于系统通知。
- 事件只包含资源 ID 和最新 revision，不包含 token、邀请码、文件内容或完整消息。
- 客户端收到事件后重新读取资源；SSE 重连后必须重新 bootstrap。
- 演示阶段不提供 cursor replay，也不承诺发送方离线时自动恢复未确认请求。

## 10. 工作箱推导

客户端先为每个线程推导一个互斥状态：

1. 当前 Principal 设置了 `hidden_at`：已归档；
2. `phase=closed/cancelled`：已结束；
3. `phase=pending_transfer && pending_recipient=me`：待我确认；
4. `phase=awaiting_reassignment && initiator=me`：待重新派发；
5. `phase=awaiting_acceptance && current_actor=me`：待我验收；
6. `phase=active && current_actor=me`：待我处理；
7. 其余未结束参与线程：等待对方。

侧栏按行动需求提供视图，而不是把来源与状态混成互斥分类：

- 需要我处理：待确认、待重新派发、待我处理和待我验收；打开任务只清除未读，不清除尚未完成的行动提醒；
- 等待对方：对方处理中，且当前 Principal 是发起人或负责人；
- 全部接力：当前 Principal 参与的所有未归档线程，包括已结束线程；
- 已结束：已结束且未归档；
- 已归档：当前 Principal 已归档的线程。

“全部接力”提供可重叠的来源筛选：`initiator=me` 为“我派出的”；`initiator!=me && owner=me` 为“派给我的”；当前 Principal 是参与者且既非发起人也非负责人时为“我协作的”。历史协作者不会因为仍是线程参与者而进入“等待对方”。

前端实现必须使用共享纯函数推导，不能信任预览数据中的 `category`、`status` 或 `banner`。

团队成员页只读取 bootstrap 权威快照，不新增组织架构或绩效状态。它展示负责人、普通成员、当前处理、等待回传、待验收和未结束责任项数量；工作任务仍在“我的工作”中处理。团队是可创建、加入和切换的临时工作群，不表达公司上下级关系。

### 10.1 个人管理与团队取消

- `PUT /v1/threads/{thread_id}/preference` 同时提交 `pinned` 和 `hidden`。偏好按 Principal 存储并跨该成员的设备同步，不影响其他成员。
- 置顶只改变同一分类内的排序，不改变线程状态或接力顺序。
- 只有 closed/cancelled 工作可以隐藏；隐藏是可恢复的个人视图状态，不删除线程、动作或材料。
- 团队层面没有物理删除。发起人可提交带原因的 `cancel` 动作停止未结束工作；动作进入不可变历史，所有参与者可见。

### 10.2 未读更新与已读游标

- Hub 为每个 Principal/线程保存单调递增的 `last_read_sequence`；同一账户的所有设备共享该游标，SSE 只用于提示客户端重新读取权威快照。
- `ThreadDetail.unread_count` 只统计游标之后、由其他成员产生的动作；本人提交的派活、进度、回传或验收不会形成自提醒。
- 新任务的接收人从 `assign` 动作得到 1 条未读，发起人的游标在创建时初始化到序号 1；后加入的协作者只从使其成为参与者的动作开始统计。
- `PUT /v1/threads/{thread_id}/read` 提交 `read_through_sequence`。服务端拒绝超过当前线程 revision 的游标，并使用 `greatest` 保证并发设备或迟到请求不能让已读位置倒退。
- 打开任务详情时客户端将当前 revision 标记为已读；侧栏分类数量仍表示任务数量，顶部“我的工作”角标表示有未读更新的任务数，任务卡片角标表示该任务的未读动作数。
- 上线迁移把既有参与者初始化到当时的线程 revision，避免把部署前的全部历史记录突然显示成未读。
- 滚动升级期间，若旧 Hub 的 `ThreadDetail` 尚未提供未读字段，桌面端以本机持久化游标兼容：首次只把每个线程最后一条他人动作提示为未读，点击后本机消除；Hub 升级后始终以服务端游标覆盖该兼容结果。

## 11. 错误响应

所有非 2xx JSON 响应使用统一结构：

```json
{
  "error": {
    "code": "revision_conflict",
    "message": "Thread changed; refresh before retrying.",
    "request_id": "req_01...",
    "details": {
      "current_revision": 4
    }
  }
}
```

`message` 是面向开发调试的稳定英文概述；最终用户文案由 Pinvou i18n 根据 `code` 生成。

| HTTP | code | 使用场景 | 是否可重试 |
|---|---|---|---|
| 400 | `invalid_request` | 字段、格式或组合不合法 | 修正后 |
| 401 | `authentication_required` | 缺少或无效 token | 否 |
| 401 | `join_password_invalid` | 团队加入码不正确或已经轮换 | 向管理员确认 |
| 401 | `device_revoked` | Device 已撤销 | 重新加入 |
| 403 | `membership_required` | 不是有效团队成员 | 否 |
| 403 | `thread_access_denied` | 不是线程参与者 | 否 |
| 403 | `action_not_allowed` | 当前身份或状态不允许该动作 | 刷新后判断 |
| 404 | `resource_not_found` | 资源不存在或不可见 | 否 |
| 409 | `revision_conflict` | expected_revision 不是当前版本 | 刷新后 |
| 409 | `idempotency_key_reused` | 同一 key 对应不同请求 | 生成新 key |
| 409 | `invite_already_claimed` | 邀请已被消费 | 否 |
| 409 | `email_already_registered` | 旧登记接口尝试重复创建已有邮箱账户 | 先通过账号登录入口复用账户，再加入或创建团队 |
| 409 | `email_change_not_supported` | 尝试更换已绑定邮箱 | 当前版本不支持 |
| 409 | `last_team_owner` | 试图移除最后一位 team owner | 先增加 owner |
| 409 | `member_has_open_work` | 成员仍承担未结束线程的关键角色 | 先结束工作 |
| 410 | `invite_expired` | 邀请过期或已撤销 | 重新邀请 |
| 410 | `attachment_expired` | 已结束线程的文件超过保留期 | 否 |
| 413 | `attachment_too_large` | 文件超过限制 | 否 |
| 413 | `team_attachment_capacity_exceeded` | 团队文件总量超过限制 | 清理后 |
| 415 | `attachment_type_not_allowed` | 类型不允许或 MIME 冲突 | 否 |
| 422 | `attachment_not_ready` | 附件不属于提交者、已绑定或不可用 | 修正后 |
| 429 | `rate_limited` | 请求频率过高 | 按 Retry-After |
| 503 | `service_unavailable` | 数据库或必要存储不可用 | 稍后重试 |

Hub 不得在数据库写入失败时返回业务成功。

## 12. 契约版本与变更规则

- HTTP API 使用 `/v1` 前缀。
- 演示阶段允许在合入首个实现前修订本文件；开始三设备验收后，破坏性变更必须同步迁移方案。
- 新增可选响应字段属于兼容变更；删除字段、改变状态语义、收紧既有输入或改变错误码属于破坏性变更。
- Hub、Rust 客户端、前端和契约测试必须在同一 PR 中声明其支持的协议版本。
