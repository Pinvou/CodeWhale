# 注销长期阻塞告警

在 `CLOSURE_REQUESTED`（包括最后一位组织所有者或平台超级管理员阻塞）、`QUIESCING` 或 `FINANCIAL_CLEARANCE` 阶段保存非空阻塞快照时，若已经超过请求既有 `sla_expires_at`，Account 会在保存事务内追加 `BLOCKED_SLA_ALERT` 审计事件。`redacted_details` 仅含阻塞快照摘要和既有 SLA 时间；审计行仍保留请求 ID、前后阶段和发生时间等定位元数据，不包含原始账务或凭据。

同一请求和同一阻塞快照只产生一次事件。告警写入失败会回滚本次保存，既有 worker 按原规则重试；事件本身不改变注销状态或执行资金操作。现有评估和人工处理流程保持不变。事件当前持久化于 `account_closure_audit`，本变更不新增 HTTP 读取端点或通知消费者；运维责任与通知接入仍由 #1910 跟踪，不能因事件写入完成就关闭该 issue。
