# Billing 普通死信的受控恢复

本手册延续原普通事件恢复合同，仅用于实际已安装并核验的 Billing 独立库。
先通过现有公共 migrator 安装原字节 `000032_event_replay_audit`，并运行当前
Billing 完整迁移校验。已安装原032的账本及不可变回执保持原身份，不重写历史；
当前缺032的库补装同一原迁移。Account 的同名表和普通 replay 能力属于另一个
物理库，不能代替 Billing 载体。不要手工补写 schema_migrations 记录。

此恢复使用现有 `event-replay` 工具及现有 `billing_service` 登录身份，数据库
凭据只能由受控私有环境提供到 `DATABASE_URL`，不得写入命令参数、工单或日志。
不能用超级用户、schema owner 或额外授权替代实际运行身份。原审计表经当前
hardening 由 `billing_schema_owner` 持有，运行身份不能取得 owner/DDL/TRUNCATE；
已有审计的 UPDATE/DELETE 由原不可变触发器拒绝。本增量不新建 operator 角色、
权限、HTTP 入口、调度或 relay，不更改当前 CLI 的事件分类和授权边界。

1. 确认真实根因已排除，并核对目标库、构建、普通事件原 ID 与当前业务状态。
2. 由第二名复核人确认影响范围、原事件及恢复理由；保留现有受控审批/事件记录。
   CLI 仅记录原 JOB 身份、运行 ID 和理由，不声称软件已证明第二人的实际复核。
3. 在私有受控环境使用原工具 inspect。只核对脱敏元数据、`replayable` 和完整摘要；
   普通事件的现有 `last_error` 仍可能包含历史诊断文本，不复制进公共输出/工单。

```sh
/event-replay --event-id "$EVENT_ID" --inspect
```

确认 `replayable=true`、原类型适用普通恢复并复核摘要后，用原事件 ID、稳定 key
和同一理由执行一次：

```sh
/event-replay --event-id "$EVENT_ID" \
  --expected-digest "$INSPECTED_DIGEST" \
  --idempotency-key "$ORIGINAL_REPLAY_KEY" \
  --reason "$REVIEWED_REASON" --confirm
```

错误摘要、缺确认、无效原 key 或不合法 JOB 上下文必须失败，不应清空死信字段或
写入审计。`account.approval-command.authorized.v1` 永久禁止人工重放，历史 key
也不能绕过；参见[授权投递恢复](approval-authorization-relay.md)。本手册不把
Token 普通事件的停写/历史保留决定扩到 Billing，也不为任何新命令扩权。

原工具在一个事务内写不可变 `event_replay_requests` 并按原 event_id 清死信、
增加 publication_generation、重置投递租约和 attempts。它只重新排期原事件，
不直接调用领域处理器、不绕过 Inbox、不允许条件批量重放。相同 key/事件/摘要/
理由重试返回原 replay_id、`committed=false`，不再次加 generation；改原事实必须
冲突。ACK/调用结果未知时先核原审计和事件状态，保留原 key，不换 key 猜重试。

人工确认会使普通事件成为后续既有 relay 可领取对象，因此必须处于已授权的维护
窗口；本手册不自动启动 relay、发布 NATS 或执行资金事件。确认后的实际投递、
Consumer pending/Inbox 收敛、领域终态及资金/用量对账仍须分别核验并登记。
不要直接更新 published_at、删 Outbox/Inbox/审计或回退迁移来“修复”事件。

`TestBillingOrdinaryReplayOriginalMigrationPostgres` 使用独占临时数据库、公共
Billing角色 bootstrap/migrator 和真实现 CLI 进程验证缺032补装、原LF/CRLF已安装
历史、inspect/确认/原 key/摘要和权限拒绝。它只写受控测试事件，不启动 NATS或
真实领域投递；测试通过不代表任何站点已恢复、真实资金事件已被重投或第二人已
完成现场审批。
