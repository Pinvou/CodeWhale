# 应收核销

Billing 前向迁移 `900015_receivable_activation_execution_lock` 为 RWO v1/s1 提供固定无参数 owner 锁能力。运行身份仅获私有 schema 的 USAGE 和函数 EXECUTE，不能修改激活表或继承 owner。新执行在原财务事务内锁定激活行并核对冻结版本；operator 停用必须等待已持锁的财务事务提交。已存在的不可变回执仍先只读返回，停用不会使历史回执失效。该迁移 Down 显式拒绝，运维停止使用应将激活置为 INACTIVE，不删除财务事实。

官方迁移收尾维持 `receivable_write_off_cases` 的 SELECT/INSERT 权限，撤销 UPDATE/DELETE；原不可变触发器仍保留。此前收尾授权退化的独立记录为 [#1706](https://github.com/Pinvou/pinvou3/issues/1706)，不表示已绕过触发器或发生资金损失。执行摘要沿用已有算法以兼容不可变回执；摘要领域拆分的后续边界见 [#1678](https://github.com/Pinvou/pinvou3/issues/1678)。

在当前已审生产叠加文件之后最后加入 `compose.receivable-write-off.yaml`；同一个 F11 源提供 Account、Billing、Portal、migrator、Token、relay 和 approval-resources。沿前序私有网络、外部 HTTPS、服务身份与资源发行合同，不添加独立消费者开关。安装完整公开迁移，原 Account072 和 Billing063/064 与前序所有校验和原样保留。

新增 `RECEIVABLE_WRITE_OFF` v1/s1 默认 INACTIVE；只读 precheck 可准备事实，create/update/submit/APPROVE/Begin、proof/执行仍要求实时 ACTIVE。消费集合为 Billing6 + Token3，Token19策略原样；按现有 operator cutover/readback 重新发行严格资源，不自动激活或创建消费者。

平台财务既有治理表单新增应收核销选择。请求人 `platform.billing.repairs.request`，独立审批人 `platform.billing.repairs.approve`；输入目标应收、正 int64 核销金额、原应收版本，固定原因 APPROVED_RECEIVABLE_WRITE_OFF。仅 USAGE_OVERAGE 的未结应收可处理，拒 CHARGEBACK/终态/超额/冻结事实漂移。前端金额与版本保持十进制原精度，typed EDIT 保留原目标、金额和 expected_receivable_version，重新只读获取当前事实。

效果为一个不可变 case、一笔平衡 journal 与 APPROVED_WRITE_OFF allocation；应收及钱包 receivable 投影推进，不改变 posted/reserved/credit lots/权益。结果先行与原审批查询/同键回执恢复继续生效，未知 ACK 不证明零效果。Native 管理入口沿原 Portal 页面，不新增 Native 管理 API。

本手册描述生产合同，不代表私有候选已完成实际相邻传播、审批全链、issuer/NATS资源发行、canonical资产构建、Native或部署 E2E。
