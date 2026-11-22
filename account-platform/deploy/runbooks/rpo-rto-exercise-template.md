# RPO/RTO 恢复演练记录

演练类型必须是：每月全部数据库/关键对象抽样恢复、每季度核心服务端到端恢复、每年整机/集群/可用区完全丢失恢复。失败演练 24 小时内指定负责人和止损，30 天内整改复测。

## 身份与时间线

- 环境 / 隔离 namespace：
- 演练类型与故障场景：
- 负责人 / 财务复核 / 安全复核：
- 源构建 SHA、目标构建 SHA、Helm revision：
- 故障检测时间、人工宣布时间（RTO 取较早者）：
- 恢复开始、核心安全就绪、非关键能力就绪、验证完成时间：
- Account/Billing/Token/Keycloak 备份 ID、SHA-256、WAL/PITR 位点：
- 资金域故障前最后事实、恢复后最后事实及实际 RPO（目标 ≤60 秒）：
- 配置域故障前最后事实、恢复后最后事实及实际 RPO（目标 ≤300 秒）：
- 核心实际 RTO（目标 ≤3600 秒）：
- 非关键实际 RTO（目标 ≤14400 秒）：

## 必填验证

- [ ] 所有恢复均在新实例完成，原实例未被覆盖，恢复环境没有真实 Provider 收费、退款或模型调用出口。
- [ ] 四库 Schema 与 `migrations/manifest.json` 一致，迁移后验证摘要齐全，领域只读审计实际程序与工件齐全且无未解释异常；四个历史固定 SQL 文件当前未提供，不得填写虚假通过。
- [ ] 账本每个 Journal Entry 平衡；钱包、权益、Hold 可重算；恢复点前后支付均已主动查询和渠道对账且无重复入账。
- [ ] Outbox 自动投递、Inbox 去重、聚合版本连续；死信有结论，待结算 Usage 未被错误释放。
- [ ] 登录、组织隔离、授权撤销、余额、充值查询、支付状态和 Token 调用抽样通过。
- [ ] 报告记录 WAL、每日增量、每周全量、迁移前快照、TLS/静态加密、KMS 分离和异地不可变副本证据。
- [ ] 分类留存扫描覆盖支付证据、账本、审计、计费用量及普通日志；法律保全独立计数，到期可删记录有不含正文的清除收据。
- [ ] Trace、日志和指标不含凭据、业务正文或高基数私人标签。

## 偏差、整改与复测

逐条记录时间、操作者、动作、结果和证据摘要。每个偏差必须有等级、负责人、24 小时止损、30 天内整改期限和复测结果；禁止只填写“演练通过”。将最终结构化事实汇总为门禁证据，并运行：

```sh
node scripts/account-platform-recovery-gate.mjs "$EVIDENCE_EXPORT_ROOT/recovery-evidence.json" \
  --trust-file="$CONTROLLED_TRUST_FILE" --artifact-root="$EVIDENCE_EXPORT_ROOT" \
  --deployment-id="$EXPECTED_DEPLOYMENT_ID" --expected-release-sha256="$CANDIDATE_RELEASE_SHA256"
```

## 工件验证限制

命令从仓库根目录执行；策略或字段格式通过、工件摘要绑定和真实演练/发布审批是不同结论。现有可信来源、全部原始工件和独立审批不可省略，详见 [运维证据](operational-evidence.md)。
