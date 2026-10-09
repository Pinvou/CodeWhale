# 付款财务纠偏

在前序生产叠加文件后加入 compose.payment-financial-repair.yaml。Account、Billing、Portal、migrator、Token、relay 和 approval-resources 必须同一 F12 源，继承前序身份、私有网络和外部 HTTPS 合同。公开安装原 Account073 与 Billing065/066/077，以及全部前序迁移，不改变原迁移字节。

新增唯一 PAYMENT_FINANCIAL_REPAIR v1/s1，默认 INACTIVE。注册集合为 Billing7 + Token3；当前 Token19 原样。使用现有 operator cutover/readback 与 RunBillingApprovalConsumers 装配，不添加独立命令环境开关，不自动激活、发行资源或创建消费者。

请求人 platform.billing.repairs.request，独立审批人 platform.billing.repairs.approve。输入付款订单和不可变付款证据 ID；固定原因 CONFIRMED_PAYMENT_NOT_CREDITED。仅 MANUAL_REVIEW 且匹配唯一 REVIEW_REQUIRED 交易、证据为成功、内部尚未入账、无退款拒付的当前订单；证据的真实 customer/SKU 必须匹配，不能从订单反填。完整数字由既有财务 codec 保真，typed EDIT 使用原 normalized payment_evidence_id 重新只读预检。

执行复用已审付款合并写入者，追加单次 case/平衡 journal/package/权益及双身份审计；BASE 按既有顺序清偿应收。结果先行查询与未知 ACK 同键恢复沿原审批工作流；不新增 PAYMENT_REPAIR_CREDIT，不放宽自审、窗口、权限或 TestOnly 收款守卫。Native 沿原管理页面进入，无新 Native API。

本手册是当前生产合同；私有候选验证不代表实际相邻传播、完整审批/issuer/NATS 发行、canonical、Native、部署或 E2E 验收。
