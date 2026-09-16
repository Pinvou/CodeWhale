# GSA-21 权益组成调整：领域事实与决策门禁

状态：`IN_PROGRESS / DECISION_PENDING / REVIEW_PENDING`。本文是领域事实与待决策记录，不授权 GSA-22 实现、迁移或启用 `ENTITLEMENT_COMPONENT_ADJUSTMENT`。

## 已有事实

- Account 中该命令只有 policy version 1 / snapshot schema version 1 的通用元数据；Billing activation 为 `INACTIVE`，没有专属写入 API、proof、executor 或恢复协议。`AUDIT_ONLY` 的充值、退款等路径继续保持原合同。[命令基线](admin-governance-security-command-baseline.md)与 [GSA 总设计 §11](admin-governance-security-redesign.md)是门禁依据。
- `entitlement_components` 按 package 限定至多一条 `BASE` 和一条 `BONUS`；每条 component 唯一绑定 credit lot，`available + reserved + used + active_grant + suspended_grant + expired + revoked = original`。component 变更触发 package 投影；package 本身也有总量守恒检查（Billing 迁移 `000016`）。
- `member_quota_grants` 保存 component ID 与类型的复合外键；usage hold allocation、grant hold allocation 及 refund component reservation 也绑定 component。跨类型原地改写可能破坏既有引用或历史证据（Billing 迁移 `000021`、`000027`）。
- 支付入账从商品快照拆分 BASE/BONUS，分别创建 credit lot；BASE 被标记为可退款，BONUS 不可退款，且 BASE 可先用于应收清偿（`internal/billing/payments.go`）。退款冻结前按 component 分别计算 BASE/BONUS，并冻结相应权益及成员配额（`internal/billing/refunds.go`）。因此“只改显示分类”也可能改变退款和应收含义，不能当作普通资料编辑。
- 消费顺序按过期时间、类型（同到期时 BONUS 先于 BASE）、入账时间及 ID 排序（`internal/billing/packages.go`）。调整不能让已存在的预留、消费、过期、退款或配额引用改写为另一来源。

## 必须先决定的产品边界

1. **建议的首版窄范围**：只允许有原始来源证据且尚未消费、预留、配额分配、退款或过期的可用额度纠错。对存在这些历史效果的 package 失败关闭，另以专门补偿流程处理；不把原 component 的 `component_type`、原始 lot 或已发布订单/交易/退款记录原地改写。即使如此，BASE 可退款性和应收关联变化仍需显式财务授权与成对补偿凭证，不能仅搬移数值。
2. **完整历史范围**：若要调整已消费、预留、配额分配、退款或过期的额度，必须逐条定义用户、Provider、订单、应收、账本和权益历史的对应补偿；不能以一次组成变更概括。范围、外部证据和回滚语义需先由产品/财务决定，设计与实现成本及上线风险显著更高。

在用户选择前，不冻结 closed command/snapshot/proof、补偿分录或 migration 细节，GSA-21 不标记 `DESIGN_COMPLETE`；GSA-22 不开始写入路径。

## 两种范围共有的不可退让约束

- 只从 Billing 权威事实和原始来源证据建立预检与 proof；Account 只做闭合批准和引用，不解释金额或组件。新 policy/schema version 不借用通用 v1 假装可执行；单命令 activation 默认 `INACTIVE`。
- 冻结钱包、package、两 component、lot、订单/交易/退款/应收、授予/预留、消费与过期的版本或摘要；批准和执行前重查。差异即失效并要求新申请；近期认证、独立审批、权限和动作代次由共用审批合同执行。
- 所有实际财务变化必须有不可变的正反成对记录、同钱包同单位守恒、原请求幂等键、唯一领域效果及可追溯修复引用；不更新历史 journal/posting 的金额或来源。未知执行结果先按原领域引用只读查证，不能再发第二次副作用。
- 只读 presenter 脱敏，不向浏览器下发 proof 或 Provider 原文。N/N-1、迁移/回滚、撤权、并发、重复投递和故障注入的真实 Billing E2E 在 GSA-22 验收；GSA-38～40 前不部署或激活。
