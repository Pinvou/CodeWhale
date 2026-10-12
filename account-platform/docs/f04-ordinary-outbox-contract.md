# F04 普通通知与政策审计边界

本轮停止从 Token 治理命令完成、账户开通请求、完成、失败、人工复核和恢复动作生成普通通知。原有业务事务、命令回执、幂等重放、不可变审计及恢复队列继续承担既有闭环。没有安装普通 Token event relay，也不发行普通 Token 发布身份；F05 的 Account/Billing relay 不是这项能力的交付依赖。

这项修正只停止新的普通通知写入，不删除、修改、标记已投递或死信化历史 outbox 记录。历史积压不能作为资金释放、命令失败或需要重做业务的证据。

`apply_token_account_owner_cap_policy` 的政策审计事件保留。其真实事件 ID 是不可变政策版本及公开政策回执的一部分，同键重放仍返回原事实。这不是已交付的消息通知：当前没有投递入口，新的政策修订仍会增加政策审计和对应事件。不得声称 Token outbox 零增长、已经具备自动归档或硬性容量上限；现有迁移身份、校验和和历史关联均不改写。

部署前应由获授权的运维人员只读核对历史积压和政策审计规模，结合实际调用量与数据库容量评估；本轮本地测试不替代留存数据库的核查。以下查询只读取计数、时间和关系大小，不输出事件载荷：

```sql
SELECT event_type, count(*) AS total,
       count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL) AS pending,
       min(occurred_at) AS first_recorded_at
FROM public.outbox_events
GROUP BY event_type
ORDER BY event_type;

SELECT pg_total_relation_size('public.outbox_events'::regclass) AS bytes;
```

如果日后交付普通 Token 消息能力，须单独确定资源身份、主题、消费者、幂等交付、重放和留存契约并完成审核。不能自动发送现存历史事件、解除既有资源限制，或以删除审计记录代替这项交付。
