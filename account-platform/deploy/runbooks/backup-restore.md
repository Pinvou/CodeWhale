# 数据库备份、PITR 与恢复

适用数据库：Account、Billing、Token、Keycloak。Payment Simulator 仅用于
dev/test，可按相同方法恢复，但不得作为生产资金事实。生产目标为同可用区
RPO 0、区域灾难初始 RPO 不超过 5 分钟、RTO 60 分钟；最终数值必须由实际
云资源演练认证。

## 备份要求

- PostgreSQL 开启连续 WAL 归档和加密存储，备份账号只有读取/归档权限。
- 每日逻辑备份用于可移植恢复，物理基础备份与 WAL 用于 PITR。
- 凭据通过受控 `PGPASSFILE` 或云身份注入，不把数据库 URL、密码写入脚本、
  命令历史、日志或演练记录。
- 记录每库开始/完成时间、WAL 位点、Schema 版本、构建 SHA、校验和和保留期。

示例（每个数据库分别执行并由备份系统加密）：

```sh
pg_dump --format=custom --no-owner --no-privileges \
  --file account.dump --dbname account_platform_account
sha256sum account.dump > account.dump.sha256
```

## 隔离恢复顺序

1. 建立全新隔离实例/namespace，关闭 Ingress、支付回调和模型出口；不得覆盖
   原实例，也不得连接旧 New API。
2. 选择事故前目标时间并记录证据。通过云 PostgreSQL/PITR 工具分别恢复各库；
   多库无法形成分布式快照，恢复后必须以支付渠道和不可变账本对账收敛。
3. 逻辑恢复使用新建空库和最小权限角色：

```sh
pg_restore --exit-on-error --single-transaction --no-owner \
  --dbname account_platform_billing billing.dump
```

4. 依次恢复 Keycloak、Billing、Account、Token；启动 PostgreSQL、NATS、Redis，
   仍保持业务 Deployment 和 Ingress 关闭。
5. 对每库运行迁移到当前版本；迁移失败立即停止，不修改已应用迁移文件。
6. 运行本目录 `sql/account-restore-audit.sql`、`billing-ledger-audit.sql`、
   `token-restore-audit.sql` 和 `outbox-inventory.sql`。任何非零异常先调查。
7. 在隔离环境启动服务但保持支付/模型出口关闭；检查 Readiness、Outbox 和
   Inbox。事件依赖 `event_id` 去重，禁止清空 Inbox 后重放。
8. 对过期 Hold 先审阅清单，再小批执行：

先用 `usage_request_id` 与 Token 数据库交叉核对。Token 侧仍为
`UPSTREAM_ACTIVE`、`SETTLEMENT_PENDING` 或 `REVIEW` 的 Hold 不得按“超时未消费”
直接释放，必须等待自动核对完成或取得人工复核证据。

Build or select the approved operations image from this repository with
`SERVICE=hold-recovery`, then run its `/service --limit 100 --confirm` entrypoint
as a short-lived Job whose Secret only grants Billing database access.

该命令仅将过期 ACTIVE Hold 标记为 REVIEW，保留钱包预留金额；租约过期不证明
上游没有产生用量。必须核实上游事实后再通过既有结算或释放接口收尾，不能把
命令返回的数量理解为释放资金数量。

9. 主动查询恢复点之后仍可能到达的支付订单，下载渠道账单并运行三方对账；
   “渠道已付、恢复库未入账”只能通过已验签通知/安全查询流程补入账。
10. 完成抽样登录、余额、充值查询、Token 结算和审计复盘后，由事故负责人和
    财务/安全复核人共同批准开放流量。

## 禁止操作

不得直接修改钱包快照、账本 Posting、支付终态或 Usage 终态；不得删除
Outbox/Inbox 来“清理积压”；不得让恢复环境和原环境同时接收同一支付回调。
回滚应用版本不等于回滚数据库，数据库恢复必须走新的隔离实例和上述核验。
