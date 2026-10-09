# 应收核销

在当前已审生产叠加文件之后最后加入 `compose.receivable-write-off.yaml`；同一个 F11 源提供 Account、Billing、Portal、migrator、Token、relay 和 approval-resources。沿前序私有网络、外部 HTTPS、服务身份与资源发行合同，不添加独立消费者开关。安装完整公开迁移，原 Account072 和 Billing063/064 与前序所有校验和原样保留。

新增 `RECEIVABLE_WRITE_OFF` v1/s1 默认 INACTIVE；只读 precheck 可准备事实，create/update/submit/APPROVE/Begin、proof/执行仍要求实时 ACTIVE。消费集合为 Billing6 + Token3，Token19策略原样；按现有 operator cutover/readback 重新发行严格资源，不自动激活或创建消费者。

平台财务既有治理表单新增应收核销选择。请求人 `platform.billing.repairs.request`，独立审批人 `platform.billing.repairs.approve`；输入目标应收、正 int64 核销金额、原应收版本，固定原因 APPROVED_RECEIVABLE_WRITE_OFF。仅 USAGE_OVERAGE 的未结应收可处理，拒 CHARGEBACK/终态/超额/冻结事实漂移。前端金额与版本保持十进制原精度，typed EDIT 保留原目标、金额和 expected_receivable_version，重新只读获取当前事实。

效果为一个不可变 case、一笔平衡 journal 与 APPROVED_WRITE_OFF allocation；应收及钱包 receivable 投影推进，不改变 posted/reserved/credit lots/权益。结果先行与原审批查询/同键回执恢复继续生效，未知 ACK 不证明零效果。Native 管理入口沿原 Portal 页面，不新增 Native 管理 API。

本手册描述生产合同，不代表私有候选已完成实际相邻传播、审批全链、issuer/NATS资源发行、canonical资产构建、Native或部署 E2E。
