# 数据库备份、PITR 与恢复

本流程适用于生产 Account、Billing、Token、Keycloak。Payment Simulator 仅允许存在于隔离开发/测试环境，不是生产备份对象，也不能作为资金事实恢复来源。目标和分类以 [`../recovery-policy.json`](../recovery-policy.json) 为机器可读单一来源：资金事实 RPO 不超过 60 秒、账号与配置 RPO 不超过 300 秒；核心认证、Account、Billing、Gateway 和支付状态查询 RTO 不超过 60 分钟，报表、导出及非关键后台 RTO 不超过 4 小时。

## 备份与迁移前门禁

- 所有数据库持续归档 WAL/等价日志；Billing、Token 的最近成功归档不得早于 60 秒，Account、Keycloak 不得早于 300 秒。
- 每 24 小时至少一次增量备份、每 7 天至少一次全量备份；数据库迁移、账本结构变更及重大发布前额外创建一致性快照。
- 传输最低 TLS 1.2，静态最低 AES-256 或等价强度；解密密钥由独立 KMS/Vault 保存，不与备份同域；至少一份副本位于独立账号/区域的不可变存储，生产服务身份无删除权限。
- 每日备份保留 35 天，月度恢复点保留 13 个月。业务三年保留通过分类归档实现，不无限延长普通备份。
- 凭据只通过受控 `PGPASSFILE`、工作负载身份或 Secret 注入；数据库 URL、密码、密钥、签名令牌不得进入命令历史、日志或证据文件。
- 生产迁移 Job 在连接数据库前强制校验 `MIGRATION_BACKUP_EVIDENCE_ID`、24 小时内的 `MIGRATION_BACKUP_COMPLETED_AT` 和 `MIGRATION_BACKUP_SHA256`。缺少任一项即失败关闭。

仓库 `migrations/manifest.json` 固化所有历史迁移的规范化 LF SHA-256。已有迁移不得改名、删除或修改；新增版本只能使用：

```sh
node scripts/account-platform-migration-manifest.mjs append
node scripts/account-platform-migration-manifest.mjs check
```

迁移器仍以数据库 `schema_migrations` 的实际校验和二次拒绝篡改。迁移完成后输出不含秘密的 `migration.post_verification` JSON，必须保存每库已应用数量、最高版本及校验和；不能用 Helm Job 成功状态代替该结果。

## 隔离恢复顺序

1. 建立全新隔离实例/namespace，关闭 Ingress、支付回调和模型出口；不得覆盖原实例，不得连接旧 New API。
2. 记录故障宣布、恢复开始、目标恢复时刻、构建 SHA、备份 ID/SHA-256、WAL 位点和各域最后一条可验证业务事实。
3. 依次恢复 Keycloak、Billing、Account、Token。多库不是分布式快照；恢复后必须以支付渠道证据和不可变账本收敛。
4. 对每库运行当前迁移器；已执行迁移校验和不同或文件缺失时立即停止，禁止修改历史脚本“适配”恢复库。
5. 运行 `sql/account-restore-audit.sql`、`billing-ledger-audit.sql`、`token-restore-audit.sql`、`outbox-inventory.sql`；任一非零异常先调查。
6. 在业务 Deployment 与 Ingress 仍关闭时启动依赖和服务，检查 Readiness、Outbox、Inbox 与死信。Inbox 依赖 `event_id` 去重，不得清空后重放。
7. 对过期 Hold 先审阅。`UPSTREAM_ACTIVE`、`SETTLEMENT_PENDING`、`REVIEW` 不得按超时直接释放；只运行仓库 `hold-recovery` 受限作业。
8. 主动查询恢复点后仍可能到达的支付订单，取得渠道证据并做四方对账；“渠道已付、恢复库未入账”只能通过验签通知/安全查询归并器补入。
9. 验证抽样登录、授权隔离、余额/权益、充值查询、Token 结算、支付状态只读查询和审计，再由事故、财务、安全负责人共同批准流量。
10. 以恢复点前最后业务事实与恢复后最后可验证事实计算实际 RPO；以故障检测/宣布较早者到各能力安全就绪计算 RTO。权限、密钥和介质取得时间都计入。

账本或支付未核对时，可以在目标 RTO 内仅恢复明确只读/禁资金写/禁模型调用的安全降级，不得声明完全恢复。

## 生产证据门禁

`npm --prefix account-platform run check:production-recovery -- <evidence.json>` 校验四库 WAL/增量/全量、加密、密钥分离、异地不可变副本、分级 RPO/RTO、月/季/年演练，以及分类删除扫描与收据。证据只能保存不可逆 ID、时间、计数和 SHA-256，不得保存备份 URL、凭据或业务正文。

## 禁止操作

不得直接修改钱包快照、账本 Posting、支付终态或 Usage 终态；不得删除 Outbox/Inbox“清理积压”；不得让恢复环境和原环境同时接收同一支付回调。回滚应用版本不等于回滚数据库；数据库恢复必须走新隔离实例和完整核验。
