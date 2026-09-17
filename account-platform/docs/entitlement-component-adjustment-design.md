# GSA-21 权益组成纠错领域设计

状态：`DESIGN_COMPLETE / REVIEW_PENDING / NOT_IMPLEMENTED / NOT_DEPLOYED`。用户选择首版仅纠正有原始来源证据且完全未动用的额度；审核统一延后。本文冻结 GSA-22 合同，不意味着命令已可执行或获准激活。

## 1. 权威事实与首版边界

`ENTITLEMENT_COMPONENT_ADJUSTMENT` 当前仅有 Account policy v1/schema v1 元数据，Billing activation 为 `INACTIVE`，没有专属 API、proof、executor 或恢复协议。既有充值、退款和人工调账 `AUDIT_ONLY` 路径不改道。[命令基线](admin-governance-security-command-baseline.md)与[治理设计 §11](admin-governance-security-redesign.md)继续约束激活。

首版只接受 `source_type=PAYMENT`、同一钱包与同一 package 的 BASE/BONUS 组成纠错，且 `payment_orders.product_snapshot` 中封闭的 BasePoints/BonusPoints、订单总微积分、成功 payment transaction 和原入账 journal 可交叉核对。按原入账的 `splitProductComponents` 整数算法重新计算应有 BASE/BONUS；命令必须一次性纠正全部差额，方向和金额由 Billing 计算，浏览器不能指定任意转移量。快照缺失/歧义、已与当前有效组成一致、来源为 SYSTEM/MANUAL_ADJUSTMENT、币种或单位不符时拒绝。商品现价或当前商品配置不是历史来源证据；如原订单快照本身错误，不能通过本命令修改，另立有证据的补偿任务。

申请和执行前均要求 package/wallet ACTIVE、订单已支付且已履约、交易成功且未退款；整包所有组件和 lot 的金额可守恒、可用且未过期。只要任一组件已有 used/reserved/active_grant/suspended_grant/expired/revoked、任一成员配额或用量预留、应收清偿、退款/强制退款/拒付申请及其保留证据、在途修复或无法判定的外部状态，就失败关闭。首版不触碰历史消费、配额、退款、到期和应收；不主动调用 Provider。多包/跨钱包转移、发放或扣减总额度、改支付价格、追溯改变已出具发票均不属于本命令。

这些限制是产品取舍，而不是把 `UNCONFIRMED` 视为零效果。需要扩大范围时新增 policy/schema 版本和对应独立设计，不放宽既有审批。

## 2. 不变量与补偿模型

- `entitlement_components` 现有唯一 `(package_id,component_type)`、一 component 对一 credit lot，以及 `available + reserved + used + active_grant + suspended_grant + expired + revoked = original` 均来自 Billing `000016`。member quota、usage hold、退款保留均按 component ID 引用；不得原地改 `component_type`、原 lot 来源、订单/交易、原 journal/posting 或原始发行量。
- GSA-22 用前向迁移引入专属不可变 correction case（审批ID、原 event/epoch、package、订单/交易、源/目标 component 与 lot、金额、原始事实摘要、journal ID、状态和唯一幂等引用）；为源 component 增加非负 `reclassified_out_microcredits`，其守恒式为原七项加转出等于 original。移除仅限同 package 同类型的唯一限制，但保留 component/lot 唯一身份及成员配额复合外键。新增目标类型的 correction component/lot 仍属于原 package，继承原到期边界，不覆写原行的类型和发行量；纠错记录唯一绑定本 package，首版不允许第二次/部分纠错。
- 单事务将源 available 和 lot.remaining 等量减小，源 `reclassified_out` 等量增大，插入等量的新目标 component/lot；目标 lot 的 `refundable` 仅由经证实的目标 BASE/BONUS 类型决定。钱包 `posted`、`reserved` 与整包有效总量不变。package 投影必须按 `sum(component.original - component.reclassified_out)` 计算 `points_total`，并保持 `remaining + reserved + used + expired + revoked = total`；新旧 lot 剩余和与组件可用和相等。金额只用 int64 微积分，溢出/负数拒绝，不用浮点。
- 在原钱包负债账户的同一不可变 `ADJUSTMENT` journal 写两条带 lot ID 的相反 posting：原 lot `-amount`、新 lot `+amount`；该 journal 合计零，不能制造新负债。journal `business_reference` 和 correction case 使用原审批/执行唯一引用，重复消费只能读同一 case/result，不能新增 journal、lot、component 或发第二个领域效果。财务修复只追加补偿事实，不改历史 `journal_entries` 或 `ledger_postings`。
- 普通退款应从原 package 的当前有效全部组件计算 BASE/BONUS，仍须冻结并预留；强制退款按每组件 `original - reclassified_out` 计算比例基数，拒付的短缺/归属也必须用有效基数而非历史 original。GSA-22 对退款、强制退款、拒付及包/钱包读取做真实回归；如果任一消费者仍把转出量当可退款/可追索额度，则此命令保持 `INACTIVE`。
- 消费排序沿过期时间、同到期 BONUS 优先、入账时间、ID 的现有规则。新组件在批准生效后才进入消费候选；入账时间与到期日需保留原权益期限语义，绝不延长有效期。执行前即使只发生一次并发预留/过期/退款，也整笔失败且零 posting。

## 3. 闭合请求、快照与 proof

实现时新增独立 policy v2 / snapshot schema v2；沿用 `platform.billing.repairs.request` 与 `.approve`，`FINANCIAL_FACT_CHANGE`、一名非申请人审批、24 小时申请有效期及 5 分钟执行窗口，不借用 v1 通用 `financial_snapshot` 冒充领域事实。Billing 的逐命令 owner activation 必须随版本迁移仍保持 `INACTIVE`；Account 的 executable 注册仅在本任务全部领域门禁实现后开放，Portal 目录仍以 activation 真相为准。

业务页只能提交 `package_id`、来源 `payment_order_id`、原因及客户端幂等键；金额、方向、source/target component、版本和 proof 均由 Billing 预检产生。闭合命令至少冻结 `command_type/policy_version/schema_version/package_id/wallet_id/payment_order_id/payment_transaction_id/source_component_id/source_lot_id/source_type/target_type/amount_microcredits/reason_code/source_evidence_digest/facts_digest`，不接收额外字段、任意 SQL 或 Provider 文本。用户自由文本原因单独审计并脱敏，不进入可用于执行的数值事实。

`facts_digest` 使用共享 typed closed RFC8785 编码，覆盖订单快照及交易/原 journal、钱包版本、package 状态与期限、全部 component/lot ID/类型/原始及有效余额、授予/预留/应收/退款/拒付的零状态证据、按快照重算的差额和 Billing activation version/epoch。必要时 GSA-22 添加单调 revision 或锁定高水位；不能仅把可被并发改写的 `updated_at` 当 CAS。Billing 内部 proof endpoint 只接受 Account 专属身份，2 秒绝对期限及 64 KiB 合计限制，返回闭合 11 字段结果、稳定错误与短期事实绑定；浏览器、Portal、审批人均不得生成或提交 proof。Account 批准前按现有 GSA-13/15 合同重读，执行器在副作用前于同一 Billing Serializable 事务重读、按固定顺序锁 wallet→package→components/lots→相关证据并比对摘要；任何漂移零写入并按 GSA-16 分类。

审批详情 presenter 只显示受控的来源、原/目标组成、差额、关键版本、原因摘要与风险警示；不下发原订单个人数据、Provider 原文、内部 proof 或秘钥。业务页拥有草稿编辑与 fresh superseding；审批中心仅对最新未修改 DRAFT 快捷提交，终态不复活旧授权。

## 4. 执行、未知结果与回滚

新增唯一 Billing durable/consumer/executor 映射与专属领域 attempt，固定原授权 event ID、approval epoch、执行幂等键及版本；Account Begin、Billing owner activation、proof/revision 与 5 分钟窗口三层 gate 仍生效。Billing 事务先持久认领原 execution 和 correction case，再写成对 posting/组件变化；事务结果不明时按原 case/journal/lot 引用只读查证，不能重新生成 IDs 或盲重放。`SUCCEEDED` 仅在 correction case、两条平衡 posting、源/目标组件和 wallet/package 守恒全部持久一致时确认；缺行或 `UNCONFIRMED` 不是零效果。安全重试必须另有可证明零效果及仍在窗口内的独立证据，否则保持 UNKNOWN_QUERY_ONLY。历史普通审批和六个既有命令合同不变。

迁移仅前向追加 schema/reader，旧 writer 在新字段默认零时保持原行为；N/N-1 先部署可读新字段的 Billing，再部署 Account proof 与 executor/consumer，最后业务页。回退应用时保留 correction case、lot、component 和 ledger 历史，禁止用破坏性 down 撤销已执行的财务事实。GSA-38～40 通过且另有生产授权前，不部署、不激活本命令。

## 5. GSA-22 完成判据

真实隔离 PostgreSQL 的领域 E2E 至少覆盖：原快照与组件不一致的 BASE→BONUS、BONUS→BASE；一致时 no-op；一侧组件原本不存在/双方存在；所有未动用约束的逐项负例；原快照缺失/畸形、错订单/钱包/交易、既有退款/拒付、应收先清偿；并发 Hold/Grant/过期/退款/两次审批；重复授权、事务提交结果不明、崩溃后原 case 读取；金额边界和整数溢出。逐次核对 package/lot/钱包守恒、两条 posting 零和、可退款归属、普通退款/强制退款/拒付与消费顺序，且失败分支零副作用。另验 closed schema/proof/presenter、权限/非自审/撤权、N/N-1、默认 INACTIVE、迁移前后读取、回滚保全及真实 Account→Billing 批准/消费/查证。静态断言或 SKIP 不算领域 E2E；未通过任一项时保持 `PARTIAL / REVIEW_PENDING / NOT_DEPLOYED`。
