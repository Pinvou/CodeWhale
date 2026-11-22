# 强制退款中的成员授予终态

已确认的渠道强制退款仍沿用 PAY-32 的独立异常复核与原流水权益回收流程，不改变普通退款准入、现金预算、清账或应收边界。

回收恰好耗尽一条成员授予时，必须在同一条更新中将状态写为 REVOKED 并清空仅适用于 SUSPENDED 的 suspension_reason，以满足既有状态约束。部分回收后仍有余额的授予继续保留原暂停状态及原因。

原始 ACTIVE、MEMBER_REMOVED 或 ACCOUNT_CLOSED 等状态与暂停原因已由冻结前的完整授予快照写入异常 resource_snapshot；现有数据库触发器保护该证据不可变。清理终态投影字段不修改这份历史证据。回放继续使用同一强退案件的既有账本凭据，不重复回收或写分录。

真实 PostgreSQL 回归 `TestForcedRefundMemberGrantTerminalStatePostgres` 通过公开迁移、受限 Billing 角色、真实目录准入/上架、组织下单成功归并、成员授予及生命周期生产路径，覆盖全额耗尽、部分保留和部分强退耗尽单条授予。资金和证据不变量与普通退款拒绝暂停授予的既有测试共同验收；受控 Provider 输入不代表真实支付渠道验收。
