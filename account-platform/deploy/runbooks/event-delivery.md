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
SELECT event_id,event_type,aggregate_id,aggregate_version,publication_generation,attempts,last_error,
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

1. 确认失败根因已修复，核对目标环境和该事件当前业务状态。
2. 由第二名审批人复核事件 ID、影响范围和重放理由。
3. 用本批审核的构建生成工具镜像，在对应服务数据库凭据作用域下执行。
   Dockerfile 将所选命令安装为 `/service`，镜像内没有 `/event-replay`：

```sh
docker build -f account-platform/Dockerfile --build-arg SERVICE=event-replay \
  -t pinvou/account-platform-event-replay:reviewed account-platform
# DATABASE_URL 由当前环境的安全凭据注入，不在命令或日志中写其值。
docker run --rm --network <approved-service-network> -e DATABASE_URL \
  pinvou/account-platform-event-replay:reviewed \
  --event-id 01900000-0000-7000-8000-000000000001 \
  --reason "INC-1234 修复消费者后重放" --confirm
```

4. 观察 Outbox 变为已发布、Consumer pending 收敛，并核对领域状态及审计。

重放工具只把指定事件恢复为待投递，不直接调用领域处理器，不绕过 Inbox，
也不允许按条件批量重放。资金或用量事件重放后必须执行账本/用量对账。

每次批准的手动重放会原子递增该事件的 `publication_generation`，使主消息及
死信消息在原有 24 小时 JetStream 去重窗口内获得新的投递身份；普通失败、租约
过期或发布成功后数据库 ACK 丢失的重试保持同代身份。业务 `event_id`、事件
内容、聚合版本与付款事实不变，Inbox 继续按业务事件 ID 保证恰一次效应。
旧代或被替换、已过期租约的 Relay 不能回写新代状态，但已发出的旧代消息无法
撤回，仍由 Inbox 幂等处理。并发手动重放请求逐次换代，不会合并成一次批准；
因此操作人必须避免重复执行已成功的重放命令。
