# F18 普通 Account 死信重放

本入口用于保留在 Account outbox 中、尚未发布的普通死信，包括历史记录；不恢复已停止的普通事件 writer。F02 的资源发行范围不变，参见 [NATS 资源与 relay](nats-resources.md)。审批授权事件 `account.approval-command.authorized.v1` 永久禁止重放，不能通过历史回执或直接调用数据库函数绕过。Billing、Token 不因此获得新的重放能力或权限。

## 前提与身份

使用现有交付的 `event-replay` 可执行文件。`DATABASE_URL` 通过既有受控环境提供，指向已经完整升级并核验的 Account 库，必须包含 `000094_ordinary_dead_letter_replay_runtime`。配置的登录用户、实际 `session_user` 和 `current_user` 均须为 `account_service`；不能用超级用户或 `SET ROLE` 代替真实运行身份。不要把连接串、密码、Token 或内部地址写进命令参数、工单或输出归档。

CLI 按解析后的 `DATABASE_URL` 用户选择 Account 专用构造，不新增来源参数；缺少新数据库能力时失败，不退回直接 `UPDATE`。确认操作沿用既有 JOB 上下文：服务 `event-replay`、类型 `EVENT_REPLAY`，每次执行生成新的运行 ID，不要求新增环境变量或审批策略。普通 `event-relay` 继续使用原有构造、事件分区和 broker 身份，不增加本入口的启动门禁。

## 检查与确认

以下变量仅表示操作人员已核对的事件 UUID、检查摘要、稳定操作键和原因，不包含连接凭据。先只读检查：

```sh
event-replay --event-id "$EVENT_ID" --inspect
```

核对返回的 `event_id`、事件类型和 `replayable`，保存本次 `event_digest`。检查输出还可能包含业务标识与历史 `last_error`，应受控保存，不能原样贴入公开日志。只有普通事件且 `replayable=true` 时才继续；本命令不会修改事件。检查失败不代表允许改库后继续。

为这次逻辑操作固定 `REPLAY_KEY`（去除首尾空白后16–128字节，且至少16字符）、原摘要和 `REPLAY_REASON`。原因须同时满足既有 Go 与数据库约束：去除首尾空白后至少20字符，不能只用少量多字节字符达到 Go 的20字节下限。确认后执行：

```sh
event-replay --event-id "$EVENT_ID" \
  --expected-digest "$EVENT_DIGEST" \
  --idempotency-key "$REPLAY_KEY" \
  --reason "$REPLAY_REASON" --confirm
```

新请求在同一事务内校验锁定事件、不可变 envelope、摘要与发布代际，创建持久回执和受控代际映射，再清除死信/旧租约并递增一次 `publication_generation`。重放不修改 payload、事件类型、业务标识或原发生时间；旧 claim 的确认不能覆盖新代际。它只重新排入原 outbox，由现有 relay 后续投递，不代表消息已经得到 JetStream ACK 或业务消费成功。

结果未知或进程中断时，复用原事件、摘要、键和原因重试同一命令，不能换键、改原因或手动增加代际。CLI 重启会产生新的 JOB 运行 ID，不改变同一请求的幂等事实。已存在的相同回执只返回原结果，不再次排队；日志中的 `committed=false` 对应返回既有回执，不能据此换键再发。异事实同键、摘要变化或事件已不满足死信条件时失败，保留原事实再核查，不使用 SQL 手动补写回执或重置事件。

## 数据库与回退边界

094 使用独立 owner 的窄数据库入口，将回执、不可变代际映射与排队更新绑定在同一事务；运行身份不获得 `publication_generation` 的直接更新权。不得扩大表级授权、改写已发布迁移、清除历史映射，或绕过函数修改 generation、receipt、租约字段来“修复”失败。授权事件仍由专用授权链处理。

094 的 Down 在存在代际映射记录时拒绝撤销，保留已使用的重放证据；不能删除记录强行回退。空映射条件下新迁移自身的撤销不代表整个冻结迁移链均可回滚。按现有迁移与恢复流程处理，不新增自动降级或清理策略。

实际测试结果由本轮原始日志另行记录；本说明不将本机 SQL/权限验证视为真实 NATS 发布、消费、生产部署或端到端验收通过。
