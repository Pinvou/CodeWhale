# 财务事件契约

七个 schema 描述当前 `events.Event.Marshal` 七字段 envelope 和现有 emitters 的 payload；它们不激活消费者或改变资金写入、broker 权限、持久化协议和重投规则。运行 `npm --prefix account-platform run test:contracts` 会发现正式契约测试。测试专用 validator 不连接数据库或 NATS。

`account.wallet.provisioning.requested.v1` 保留现有 user/policy-only 与 registration_operation_id 两种分支；policy_version 不添加当前 ConfigureWalletProvisioning 没有的64字符上限；completed 仍只有 user_id。Billing recharge 现有 transaction/package/payer/beneficiary 字段必须保留，beneficiary 可为 null；order 总 credit_microcredits 仍大于0；零额历史 component 不放宽订单金额合同。SLA 总是带 severity 和可为 null 的 owner_id；provider-blind 既有按订单与全 provider 两种 payload，payment_order_id 因此可省略。现有领域 reason 以 UTF-8 字节校验10–1000，而 JSON Schema 长度按字符计，只作非空/最多1000字符的 wire 检查，不另造10字符策略；真实领域校验仍权威；payment-anomaly 的 payment_order_id 则总有该 key、值可为 null。金额仍为既有 int64 JSON number，不在此修改 wire 或公开输入标签。

Node 正式测试的七类样本是受控契约样本并绑定实际生产 subject/envelope 源；不能把它们称为真实领域或下游集成。实际 PG 测试额外经完整公开 Billing migrations/运行角色创建真实 outbox 行，经生产 Event.Marshal 后交同一 validator 验证。Account 两种事件的现存 worker 独立验证应另引用，不把这组契约样本当作真实注册/发钱包执行。现场 NATS 发布、consumer 和生产验收均未因此完成。
