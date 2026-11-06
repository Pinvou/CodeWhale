# 显式组织套餐购买

组织购买使用当前商品目录与既有 `org.entitlements.purchase` 权限。Token 在目标组织鉴权后解析组织钱包，通过既有 `internal.billing.recharge-order.create` workload 权限提交实际付款用户、受益组织和钱包。Billing 严格接收该三者，按受益组织执行现有风控限额并核验精确 ACTIVE 组织钱包；个人下单继续只写个人钱包。付款人及 Commerce Mapping 快照、受益组织、商品/映射与金额快照冻结在原订单中，履约只记入该组织权益包。

丢失客户回执时保留原 Idempotency-Key 与原参数。若 Provider 创建结果未知，协调器保留原 merchant order 并只查询原结果，不另建资金请求；确认支付与重复查询不重复入账。组织订单读取仍须现有 Token 目标组织权限及订单受益组织/钱包检查。套餐期限、退款门禁和商品暂停/准入规则不因这一接线恢复改变。

旧 CNY_1、CNY_10、CNY_100、CNY_200 四档入口已按明确决定退休；产品及历史订单记录保留，使用当前受控商品目录。该决定不重新启用 simulator 四档映射，不改变既定 Billing pre016/F016 升级拒绝与保数据边界，也不引入无期限权益。

回归 `TestOrganizationPlanPurchaseActualClientPostgres` 使用新建 UUID 数据库、公开迁移、受限 Billing 与 simulator 角色、真实 Token HTTP client 和受控支付完成。它不是正式渠道收款、现场部署或完整人类授权服务验收；相邻 Token 权限测试和部署门禁应分别执行。

新组织订单写入前持有该钱包行锁，与组织生命周期串行：先建单时删除检查须见原未结单，先终止时拒绝新建。已持久化原键的回执/绑定恢复仍优先读取原事实，不以新建门禁拒绝原回执；已未知结果只查原订单，不新增外呼。
