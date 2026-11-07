# Outbox 存储接口合同

接口职责适用于替换或新增存储实现。本文明确现有保护，未增加 subject、权限或重新投递入口；外部 broker 成功与本地持久化成功须分别确认。

## 普通事件

`events.OutboxStore.Claim` 在领取前过滤 relay 明确允许的 subject，同时保留存储分区排除规则。`Relay.RunOnce` 会在发布整个批次前复核范围，但该检查不能撤销错误实现已经领取的租约。

三个 `Mark*` 方法必须在原子写入中比较 event ID、worker、publication generation 和精确且尚未过期的 lease。过期、旧 generation、错误 owner 或同 worker 的旧 lease 都必须报错并保持 attempts、调度和终态事实不变。只有匹配的当前 claim 可以递增 attempt 并写对应结果。`PostgresStore` 通过完整条件 UPDATE 和受影响行数检查实现这一义务。

传输重试保留原 generation、事件内容及发布幂等 ID；人工重放的授权与 generation 管理由各自领域流程承担，不能由 Mark 方法自行创造。

## 冻结审批授权事件

`events.FrozenOutboxStore` 的 subject 范围由具体实现明确固定。当前 `account.ApprovalRelayStore` 只处理 `account.approval-command.authorized.v1`，经专属数据库函数领取，不能把普通事件或其他 subject 当作冻结授权事件。

`ProcessFrozen` 必须先在同一事务内锁定并复验存储中的 worker、有效 lease、claim token、relay control epoch、event type、trace identity 和冻结内容完整性，再将持久字节交给 publisher。保持相关锁至发布及结果登记；旧 claim、越域或过期租约不能发布，也不能推进 attempt 或终态。

主发布和必要的 dead-letter 发布共用原绝对 lease 边界。broker 结果未知时保留未知，不凭超时推断成功、递增重试或进入 dead-letter。数据库提交结果未知时仅以原 claim 的精确持久结果恢复，不创建新授权或推断资金完成。

## 验证边界

普通租约与 generation 的真实 PostgreSQL 载体为 `TestPostgresOrdinaryDeliveryLeaseAndGeneration`；冻结交付的实际角色、数据库函数及租约时序由 Account 审批集成测试覆盖。纯 `FrozenRelay` mock 测试只能证明调用与结果传递，不能替代上述数据库义务。真实 broker、部署 ACL、跨服务恢复和生产验收分别核验。
