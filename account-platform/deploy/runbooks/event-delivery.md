# 事件投递、死信与安全重放

Account、Billing 和 Token 各自的业务事务只写本库 `outbox_events`。独立
`event-relay` 通过短租约批量领取事件并发布到 `ACCOUNT_PLATFORM`
JetStream；NATS 故障不得回滚或阻塞已经安全提交的业务事务。

## 告警与诊断

以下任一条件应触发 P1：最老待投递事件超过 5 分钟、单库待投递超过
1000、出现 `dead_lettered_at`、Consumer pending 持续增长。先记录环境、
构建 SHA、事件 ID、事件类型、聚合 ID、尝试次数和 `trace_id`，不得复制
Payload 中的私人数据到工单或聊天。

```sql
SELECT event_id,event_type,aggregate_id,aggregate_version,attempts,last_error,
       occurred_at,next_attempt_at,dead_lettered_at
FROM outbox_events
WHERE published_at IS NULL
ORDER BY occurred_at
LIMIT 100;
```

确认 NATS、数据库和网络恢复后，Relay 会自动重试普通积压。不要手工更新
`published_at`，也不要删除 Outbox/Inbox 记录。Consumer 依靠 `event_id`
幂等，并且只接受连续增长的聚合版本；版本缺口应先恢复缺失事件。

## 死信重放

本节仅适用于可重放的普通事件。`account.approval-command.authorized.v1` 永久禁止人工重放，包含历史幂等记录；其 inspect 返回 `replayable=false`。授权投递恢复、身份和回滚边界见[审批授权 Relay 手册](approval-authorization-relay.md)。

1. 确认失败根因已修复，核对目标环境和该事件当前业务状态。
2. 由第二名审批人复核事件 ID、影响范围和重放理由。
3. 由受控环境投递对应服务数据库凭据，先 inspect；核验 `replayable=true` 和摘要后，用稳定幂等 key 确认：

```sh
/event-replay --event-id 01900000-0000-7000-8000-000000000001 --inspect
/event-replay \
  --event-id 01900000-0000-7000-8000-000000000001 \
  --expected-digest '<inspect返回的完整SHA-256>' \
  --idempotency-key 'INC-1234-event-recovery-01' \
  --reason "INC-1234 已修复消费者并经复核允许恢复此普通事件" --confirm
```

4. 观察 Outbox 变为已发布、Consumer pending 收敛，并核对领域状态及审计。

重放工具只把指定事件恢复为待投递，不直接调用领域处理器，不绕过 Inbox，
也不允许按条件批量重放。资金或用量事件重放后必须执行账本/用量对账。
