# 高可用与故障切换验收

本流程只适用于 `production-new` Helm 形态。单机 Compose 永远不是高可用环境。Chart 在生产模式强制 Account、Billing、Gateway、Portal、三类 Relay、Keycloak 和 New API 至少双副本，跨至少两个故障域调度，采用零不可用滚动更新和 PodDisruptionBudget；内置单副本 Redis/NATS 必须关闭并替换为 TLS 连接的外部高可用服务。

Identity Adapter 暂维持单副本，因为注册/改密的短期明文密码引用只属于受控进程内存，不能复制到普通缓存。Keycloak 承担核心登录高可用；Identity Adapter Pod 丢失时旧短期引用安全失效，由持久 Saga 补偿并要求重新预检。演练必须证明其在非关键能力时限内自动替换，不能通过复制明文密码实现多副本。

## 发布前容量与拓扑

1. 至少两个独立可用区/故障域，所有强制多副本工作负载的 Ready Pod 实际分布在两个域，不能只检查期望副本数。
2. 模拟失去容量最大的单个故障域后，剩余 CPU、内存、连接、数据库、Redis、NATS 和 Provider 配额仍至少保留 30% 余量。
3. PostgreSQL、Redis、NATS 的高可用、复制一致性、选主、备份责任和删除权限均有明确外部所有者；生产业务身份不能执行管理面故障切换或删除备份。
4. PDB 已生效，业务 Deployment 使用 `maxUnavailable=0`；节点维护前确认 PDB 不会与容量不足形成永久阻塞。

## 必做故障场景

在关闭真实支付收费/退款和真实模型调用出口的生产等价隔离环境依次执行：

- 删除每类业务 Pod 与 Relay Pod，验证 Service 不中断、租约/Outbox 可安全重领且不重复资金效果。
- 排空一个节点，再完全隔离一个可用区；验证核心能力不超过 300 秒恢复，非关键能力不超过 900 秒恢复。
- 执行一次完整滚动升级与中途暂停；确认 Ready 容量、旧/新版本兼容、迁移不可修改门禁和失败回滚边界。
- 分别切换 PostgreSQL writer、Redis primary、NATS leader/集群节点、Keycloak Pod 和 New API Pod；核对零已提交事实丢失、零重复账本/支付/退款/用量结算。
- 杀死 Identity Adapter 后验证旧密码引用不可被另一实例或日志恢复、Saga 安全补偿、Pod 在 900 秒内重建且新预检可用。

每个场景保存开始/恢复时间、容量水位、Ready 副本/故障域、Outbox/Inbox/死信、账本与支付核对、实际 Provider 副作用数和不含正文的 SHA-256 证据摘要。执行：

```sh
npm --prefix account-platform run check:production-ha -- ha-evidence.json
```

Chart、PDB 或门禁脚本存在不代表高可用已经完成；必须取得同一发布候选的真实集群证据。
