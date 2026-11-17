# 财务 JSON 对象约束

#1632 的 Account 046 / Billing 028 历史字段由现有生产者按 JSON 对象写入，但原数据库未登记结构 CHECK。本次新增 Account `900024_financial_json_object_contracts` 与 Billing `900042_financial_json_object_contracts`，不修改历史 SQL、摘要、JSON 内容、业务状态或资金事实。

| 数据库 | 表 | 受约束字段 |
|---|---|---|
| Account | approval_executions | critical_resource_versions、financial_snapshot |
| Billing | manual_review_case_events | details |
| Billing | late_payment_review_cases | provider_capability_snapshot、policy_snapshot、evidence_summary |
| Billing | late_payment_review_evidence | payload_snapshot |

要求字段顶层为对象；允许空对象、未知对象成员及对象内部的数组、数字、布尔和 null，不新增字段级业务规则。顶层数组、字符串、数字、布尔或 JSON null 均拒绝；原 NOT NULL 约束继续拒绝 SQL NULL。审批请求/授权、财务金额、摘要和不可变证据的既有约束继续生效。

使用现有 `cmd/migrate` Account / Billing 公开迁移入口，以及原各域迁移登录与角色。新增迁移双方向的版本、名称及原文字节摘要在公共 Up、Down、Verify 路径钉定；不能通过改写 down 文件撤除约束。它们不依赖其他补充迁移的新增对象；安装顺序仍以各域公开迁移入口为准。

迁移在原事务内添加 NOT VALID 约束后，逐项 VALIDATE 全部存量。Billing `900042` SQL 中的 RESET ROLE / SET ROLE 仅切换执行身份；公开迁移包装器仍沿用既有的事务内临时权限/所有权借用与恢复。失败会回滚本迁移的约束、台账及该事务内的临时权限/所有权变化，原坏记录保留，重复安装仍明确失败；不会把坏值替换为 `{}`、删记录或丢弃支付证据。发现坏历史时应停止依赖本约束的发布，记录表名、列名、类型及数量，原件留在受控系统，依据原不可变证据合同另行处置。本补丁不提供证据清洗或资金修复授权。

检查存量可使用只读聚合，例如在各自数据库内将目标表/列替换到下列查询；不要导出快照正文：

```sql
SELECT jsonb_typeof(financial_snapshot) AS top_level_type, count(*)
FROM public.approval_executions
GROUP BY jsonb_typeof(financial_snapshot);
```

该检查及 VALIDATE 都可能扫描历史数据。VALIDATE 自身的 SHARE UPDATE EXCLUSIVE 锁不直接阻塞普通 DML，但本迁移先行 ADD CONSTRAINT 取得的 ACCESS EXCLUSIVE 锁会保留至事务结束，仍可能阻塞并发访问；应在核验空间、积压、备份和站点规模后安排维护窗口。两域安装各自原子，不承诺跨两个数据库的分布式事务；一域失败时保留已完成另一域的前向保护，修复原因后续装失败域。down 明确返回 55000，恢复采用前向修复；应用回退应保留已安装迁移及约束。

本地验收使用公开迁移、真实受限登录、原审批/迟到付款生产者、非空升级及坏历史回滚。低层错误形状通过受控运行角色 SQL 验证，不代表匿名或公共 API 能构造签名坏快照。隔离验收不等于生产部署或真实供应商验收。
