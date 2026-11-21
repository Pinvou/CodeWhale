# 高可用与故障切换验收

本流程只适用于 `production-new` Helm 形态。单机 Compose 永远不是高可用环境。Chart 在生产模式强制 Account、Billing、Gateway、Portal、Account/Billing 两个普通 Relay、Keycloak 和 New API 至少双副本，跨至少两个故障域调度，采用零不可用滚动更新和 PodDisruptionBudget；内置单副本 Redis/NATS 必须关闭并替换为 TLS 连接的外部高可用服务。

Identity Adapter 暂维持单副本，因为注册/改密的短期明文密码引用只属于受控进程内存，不能复制到普通缓存。Keycloak 承担核心登录高可用；Identity Adapter Pod 丢失时旧短期引用安全失效，由持久 Saga 补偿并要求重新预检。演练必须证明其在非关键能力时限内自动替换，不能通过复制明文密码实现多副本。

## 发布前容量与拓扑

1. 至少两个独立可用区/故障域，所有强制多副本工作负载的 Ready Pod 实际分布在两个域，不能只检查期望副本数。
2. 模拟失去容量最大的单个故障域后，剩余 CPU、内存、连接、数据库、Redis、NATS 和 Provider 配额仍至少保留 30% 余量。
3. PostgreSQL、Redis、NATS 的高可用、复制一致性、选主、备份责任和删除权限均有明确外部所有者；生产业务身份不能执行管理面故障切换或删除备份。
4. PDB 已生效，业务 Deployment 使用 `maxUnavailable=0`；节点维护前确认 PDB 不会与容量不足形成永久阻塞。

## Billing 900044 的维护窗口

首次安装 [Billing 900044 拒付本金迁移](../../docs/chargeback-financial-repair-design.md) 时，为 Billing 安排停写窗口。旧版仍会按原付款总额更新摘要，默认的 Helm 迁移前置钩子与滚动替换顺序不足以隔离旧写入。

1. 停止向旧版 Billing 送入新请求和事件。排空已接收的请求、消费者和后台任务，再停止所有旧版 Billing 实例。确认没有旧写入会话，并冻结旧实例的自动恢复。
2. 在停写窗口内执行并验证完整的 Billing 前向迁移。确认 900044 已记账，且新版运行账号能读取本金视图、读取及插入证据表。检查失败时维持停写。
3. 只启动新版 Billing。确认各副本 `/health/ready` 通过，核对拒付摘要，再恢复流量与事件投递。恢复旧备份时，保持新版停机，完成前向迁移后再启动。

首次由新版重新观察同一渠道和上海业务日的历史对账数据时，事实摘要加入现金本金字段。
原有付款、退款或拒付差异可能以新摘要产生新的 `OPEN` 案件，既有 `IGNORED`、
`RESOLVED` 记录及已执行回执仍保留。用原渠道和业务日关联新旧案件，核对实际账本和回执后按现有流程处置。

## 必做故障场景

在关闭真实支付收费/退款和真实模型调用出口的生产等价隔离环境依次执行：

- 删除每类业务 Pod 与 Relay Pod，验证 Service 不中断、租约/Outbox 可安全重领且不重复资金效果。
- 排空一个节点，再完全隔离一个可用区；验证核心能力不超过 300 秒恢复，非关键能力不超过 900 秒恢复。
- 对支持混合版本的发布执行一次完整滚动升级与中途暂停，确认 Ready 容量、旧版和新版的兼容性、迁移不可修改门禁及失败回滚边界。首次安装 Billing 900044 时，按上文维护窗口演练。
- 分别切换 PostgreSQL writer、Redis primary、NATS leader/集群节点、Keycloak Pod 和 New API Pod；核对零已提交事实丢失、零重复账本/支付/退款/用量结算。
- 杀死 Identity Adapter 后验证旧密码引用不可被另一实例或日志恢复、Saga 安全补偿、Pod 在 900 秒内重建且新预检可用。

每个场景保存开始/恢复时间、容量水位、Ready 副本/故障域、Outbox/Inbox/死信、账本与支付核对、实际 Provider 副作用数和不含正文的 SHA-256 证据摘要。执行：

```sh
node scripts/account-platform-ha-gate.mjs "$EVIDENCE_EXPORT_ROOT/ha-evidence.json" \
  --trust-file="$CONTROLLED_TRUST_FILE" --artifact-root="$EVIDENCE_EXPORT_ROOT" \
  --deployment-id="$EXPECTED_DEPLOYMENT_ID" --expected-release-sha256="$CANDIDATE_RELEASE_SHA256"
```

Chart、PDB 或门禁脚本存在不代表高可用已经完成；必须取得同一发布候选的真实集群证据。

## 工件验证限制

命令从仓库根目录执行；策略或字段格式通过、工件摘要绑定和真实演练/发布审批是不同结论。现有可信来源、全部原始工件和独立审批不可省略，详见 [运维证据](operational-evidence.md)。
