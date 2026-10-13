# 付款财务纠偏

在前序生产叠加文件后加入 compose.payment-financial-repair.yaml。Account、Billing、Portal、migrator、Token、relay 和 approval-resources 必须同一 F12 源，继承前序身份、私有网络和外部 HTTPS 合同。公开安装原 Account073 与 Billing065/066/077/900016/900017，以及全部前序迁移，不改变原迁移字节。

新增唯一 PAYMENT_FINANCIAL_REPAIR v1/s1，默认 INACTIVE。注册集合为 Billing7 + Token3；当前 Token19 原样。使用现有 operator cutover/readback 与 RunBillingApprovalConsumers 装配，不添加独立命令环境开关，不自动激活、发行资源或创建消费者。

请求人 platform.billing.repairs.request，独立审批人 platform.billing.repairs.approve。输入付款订单和不可变付款证据 ID；固定原因 CONFIRMED_PAYMENT_NOT_CREDITED。仅 MANUAL_REVIEW 且匹配唯一 REVIEW_REQUIRED 交易、证据为成功、内部尚未入账、无退款拒付的当前订单；证据的真实 customer/SKU 必须匹配，不能从订单反填。完整数字由既有财务 codec 保真，typed EDIT 使用原 normalized payment_evidence_id 重新只读预检。

执行复用已审付款合并写入者，追加单次 case/平衡 journal/package/权益及双身份审计；BASE 按既有顺序清偿应收。结果先行查询与未知 ACK 同键恢复沿原审批工作流；不新增 PAYMENT_REPAIR_CREDIT，不放宽自审、窗口、权限或 TestOnly 收款守卫。Native 沿原管理页面进入，无新 Native API。

本手册是当前生产合同；私有候选验证不代表实际相邻传播、完整审批/issuer/NATS 发行、canonical、Native、部署或 E2E 验收。

## 激活、审计权限与回滚边界

新资金事务先调用固定 PAYMENT_FINANCIAL_REPAIR v1/s1 owner 锁能力，并核对审批冻结的 activation version；停用更新与资金提交在同一行锁边界串行化。默认仍为 INACTIVE，必须由既有 operator 流程明确激活。已持久提交的原回执先只读回放，停用不删除或改写历史资金事实。runtime 仅有私有能力 EXECUTE/USAGE，不能更新激活状态或继承 owner；payment_order_repairs 保留 SELECT/INSERT，收尾禁止 UPDATE/DELETE，后继迁移仍保持该 ACL。

900016 与 900017 数据 Down 明确返回 55000。运维回退是停止消费者、将 activation 置 INACTIVE 并保留历史，不能删除审批、审计或已入账事实。900017 为 payment_order_repairs(payment_order_id) 增加非唯一索引，保留一订单多笔维修记录。

本轮作者隔离 PostgreSQL 计划样本为 10001 条维修记录、2001 个订单：干净 900016 阶段默认 planner 顺序扫描，官方升级 900017 后使用该索引。首轮与暖访问均记录实际 EXPLAIN/BUFFERS；操作系统缓存未受控，不代表生产负载或延迟保证。索引问题跟踪 #1671，审计写 ACL 跟踪 #1709；独立验收和正常发布前不以作者结果关闭。#1678 的 execution digest 兼容议题仍保留，未改旧 receipt/claim 的算法。


## 有限可移植验证

`TestPrivateF12PublicOwnerIntegration` 使用显式 `PINVOU_TEST_SCHEMA_DATABASE_URL` 的隔离 current/session 同一 superuser，要求其它部署角色全 absent；复用现有 UUID 数据库、原 actor、权限和正常清理守卫，不安装到输入数据库。仅未提供 URL 才 SKIP，已提供但不安全则失败。命令：`go test -mod=readonly -race -count=1 ./internal/billingschema -run '^TestPrivateF12PublicOwnerIntegration$'`。子进程原三个 named owner 测试必须实际通过，零例、任何 SKIP/FAIL 都不能通过。`F12_GO_BINARY` 可选覆盖 PATH Go，`F12_CHILD_LOG` 可选，默认日志位于测试临时目录；不再依赖私人端口、用户名或实例目录。

补充 BASE 饱和用例使用真实 owner plan/execute 和原付款证据夹具：未结应收100、BASE40时最多清偿40；BONUS60保留为余额，不替 BASE 扩大抵扣。closed wire 的 0..total credit 是粗校验，执行仍由冻结 owner BASE 预算与精确事实约束；不增加公共字段或新钱包状态策略。批准的完整原审计、账本、allocation、组件和回执断言保留。此有限验证不代表真实 Provider、审批发行/NATS、Native 或部署 E2E。

浏览器正式入口读取 `CHROME`（也保留 `EDGE_PATH`/常见跨平台候选），缺浏览器明确失败而非 SKIP。`F12_REACT_RESULT` 仅为显式诊断输出，不覆盖已有私人结果。管理门户近期认证与未知审批操作保持现有 Approval EDIT 兜底，不新增跨刷新持久化或 Native 管理恢复政策。
