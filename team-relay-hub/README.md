# Pinvou Team Relay Hub

Team Relay Hub 是“人派活给人、Handoff、回传和最终交付”的独立服务。它不运行 Agent，不读取 Pinvou 私人 Session，也不复用 Remote Relay 的业务状态。

当前实现覆盖演示批次 1：

- Team、邮箱唯一的 Principal、底层 Device 凭据、Membership、初始化单次邀请和 owner 可轮换的 8 位团队加入码；
- A → B → C → B → A 接力状态机，以及派发/协作邀请的接受、拒绝、撤回和改派；Hub 按接收人的近期设备协议能力自动选择确认式或旧版直接生效；
- 不可变动作日志；
- 发起人可审计取消，以及按 Principal 隔离的置顶/隐藏偏好；
- revision 乐观并发与幂等重放；
- bootstrap 完整刷新与 SSE 提醒；
- team owner 移除成员的安全限制。
- 显式文件上传、类型识别、SHA-256、团队容量和动作原子绑定；
- 线程参与者鉴权下载，以及孤儿和已结束线程文件清理。
- 独立的 Principal 头像上传、完整解码重编码、受限 GIF 动图、48/96 px 显示变体和同团队鉴权读取。

服务尚未部署，也不应替代已有 collaboration 或 Remote Relay。

不含凭据的容器和 Nginx 模板位于 [`deploy/`](deploy/)；它们必须先在本地通过 `docker compose config/build`，再经独立授权部署。

协议基线见 [`../docs/team-relay/`](../docs/team-relay/)。

当前 Hub 支持协议 `0.2`。新客户端发送 `X-Pinvou-Team-Relay-Protocol: 0.2` 后，派发任务和邀请协作都必须由接收人确认才移动责任或接力棒；未发送该请求头的 `0.1` 客户端继续使用直接生效语义。`0009_confirmed_transfers.sql` 与 Hub 新代码必须在同一维护窗口切换：停止所有旧 Hub 写入，备份数据库，执行迁移，再启动新 Hub 并完成健康检查。不得在执行 `0009` 后重新启动旧 Hub，也不得在迁移前启动新 Hub；桌面端则可在 Hub 升级前后滚动更新。

## 本地运行

需要 Node.js 20+ 和 PostgreSQL 15+。参考 `environment.example` 把变量注入自己的进程环境，真实密码不得提交到仓库。

```bash
npm ci
TEAM_RELAY_DATABASE_URL=postgresql://... npm run db:migrate
TEAM_RELAY_DATABASE_URL=postgresql://... npm run team:create -- --name "Demo team"
TEAM_RELAY_DATABASE_URL=postgresql://... npm start
TEAM_RELAY_DATABASE_URL=postgresql://... npm run attachments:cleanup
```

内测入口密码只在 Hub 端校验。部署者把密码的 SHA-256 十六进制摘要通过
`TEAM_RELAY_BETA_ACCESS_PASSWORD_HASH` 注入服务器；真实密码及摘要都不得提交到仓库或打进客户端。
公网部署必须使用 HTTPS，并通过现有失败次数限制防止重复猜测。

`team:create` 会在终端中显示一次 owner 初始化邀请码。也可以显式设置 `TEAM_RELAY_ALLOW_SELF_SERVICE_ENROLLMENT=true`，允许尚无身份的 Pinvou 用户先用邮箱和名字登记账号，再在团队页面创建或加入团队；该开关默认关闭，公网演示服务器启用前必须确认入口访问范围和容量。已经登记的成员始终可以创建其他团队。数据库只保存邀请码和 device token 的 SHA-256 摘要；团队加入码另外保存 SHA-256 摘要与 AES-256-GCM 加密值，不保存明文。

新客户端通过 `POST /v1/enrollments/account` 提交邮箱和名字，不在登录时创建或加入团队。邮箱在当前服务内规范化后唯一；内测期间再次提交已有邮箱会复用原 Principal 并为当前安装创建 Device。这个无验证码流程不证明邮箱所有权，只适用于受控内测。名字允许重名并可通过认证后的 `PATCH /v1/me` 修改，邮箱绑定后不可更换；成员接口只在重名区分场景返回脱敏邮箱提示，不返回完整邮箱。迁移前创建的 Principal 保持可用并可在个人资料中首次绑定邮箱。Device 仍为每次安装保存独立的可撤销凭据，但不要求用户填写或在团队工作流中理解设备名称。

团队加入码显示为 `XXXX-XXXX`，不区分大小写且横线可省略。`TEAM_RELAY_JOIN_CODE_ENCRYPTION_KEY` 必须是 32 个随机字节的 base64url 编码，生产环境需独立备份并保持稳定；缺失或错误时 Hub 会拒绝读取、刷新及安全移除启用加入码的团队成员。加入码默认对所有有效成员可见；只有 owner 可以刷新、隐藏或暂停，移除成员会自动刷新仍启用的加入码。

Hub 默认对未登记用户按客户端地址、已登记用户按设备凭据计数，10 分钟内连续 5 次错误加入尝试执行 15 分钟限流；参数可通过 `TEAM_RELAY_JOIN_RATE_LIMIT_*` 调整。反向代理覆盖可信 `X-Real-IP` 且 Hub 仅监听回环地址时，部署环境应设置 `TEAM_RELAY_TRUST_PROXY=true`。当前限流状态保存在单个 Hub 进程内且有容量上限；多副本部署前需迁移到共享限流存储。

服务默认只监听 `127.0.0.1:8791`，默认基础路径为 `/pinvou3/team-relay`。正式演示部署必须由 HTTPS 入口代理，且不能把数据库端口暴露到公网。

附件和头像默认保存在仅服务账号可读的 `data/attachments`；服务器部署必须通过 `TEAM_RELAY_ATTACHMENT_DIR` 指向独立持久化目录。普通附件默认单文件 20 MiB、单团队 2 GiB、未绑定文件 24 小时清理、已结束线程文件 30 天后删除内容并保留元数据。头像独立存入 `avatars/` 子目录，只接受 PNG、JPEG、WebP 和 GIF，输入与处理后主图的硬上限均为 1.5 MiB（`TEAM_RELAY_MAX_AVATAR_BYTES` 只能继续收紧）。Hub 完整解码并重新编码头像以剥离元数据：静态图最长边 512 px，GIF 最长边 384 px、最多 60 帧且总动画像素不超过 400 万；同时生成持续保留动画的 48/96 px 显示变体。处理采用 Principal 单并发与全局并发上限（`TEAM_RELAY_AVATAR_PROCESSING_CONCURRENCY`）；默认每个 Principal 每 60 秒最多尝试 6 次，可通过 `TEAM_RELAY_AVATAR_UPLOAD_RATE_LIMIT_ATTEMPTS` 和 `TEAM_RELAY_AVATAR_UPLOAD_RATE_LIMIT_WINDOW_SECONDS` 调整。清理任务会回收超时临时文件及数据库未引用的头像文件。部署时应定时执行 `attachments:cleanup`。

`0009_avatar_hardening` 会使此前 Draft 版本上传、但未经完整解码重编码的头像安全失效；升级后用户需重新上传，旧文件会在宽限期后由孤儿清理任务回收。正式发布前采用这一策略，避免把无法证明安全且没有显示变体的历史文件继续分发给客户端。

## 测试

普通测试不连接外部数据库：

```bash
npm test
```

完整纵向测试只在显式提供隔离测试库时运行，并拒绝数据库名不以 `_test` 结尾的连接：

```bash
TEAM_RELAY_TEST_DATABASE_URL=postgresql://.../team_relay_test npm test
```

集成测试会清空该测试数据库的 `public` schema，不能指向开发库或生产库。
