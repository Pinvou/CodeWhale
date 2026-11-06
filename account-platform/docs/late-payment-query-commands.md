# 人工支付复核查询的原操作身份

新的人工复核使用 `POST /admin/v1/late-payment-reviews/:case_id/query-commands`。
首次明确确认时冻结 protocol 1、案件、订单、Provider 身份与订单号，并生成独立请求 ID。
服务端先以短事务持久化当前 actor、请求 ID 和规范事实，只有明确提交该新记录的一次调用
获得执行资格。Provider 查询、原 Sync 的兼容 Close 分支及 PAY-12 归并保留既有实现和预算，
网络调用不持有查询命令或案件事务锁。原 key 的 POST 重放也不能再次获得执行资格。

原操作重试只使用 `POST /admin/v1/late-payment-reviews/:case_id/query-command-results`，
携带原 ID 和事实。该入口不新增命令、不访问 Provider；发起记录缺失、尚无完成回执、
响应失联或当前读取失败都不能证明“未执行”。只有原 Record 的证据、复核计划和完成
回执在同一事务中实际提交，才返回该次不可变结果；当前案件已支付、归档等快照不能
补造原回执。已记录的一次查询失败只证明那次失败观察已记录，不证明支付失败。

两个入口都经过当前 `platform.billing.orders.sync` 权限及原
`BILLING_LATE_PAYMENT_QUERY` / `AUDIT_ONLY` 策略，仓库重复校验当前 owner、actor、
案件和冻结事实。授权或 owner 改变不披露旧回执。合法的案件终态变化不阻止读取已完成
原回执，但不能被迟到完成事务覆盖。没有完成回执的原记录保持 UNKNOWN，没有 TTL、
自动重派、租约恢复或后台补跑。

界面保留每条 UNKNOWN 的原引用。原 retry 从不更换 ID 或重新派发；用户明确确认
“新建复核”才生成新 ID 并依当前准入重新查询。新结果不抹掉旧 UNKNOWN，也不把它
标为成功。换号、授权版本、会话代际或卸载会退休旧响应，其他资金操作已有 UNKNOWN
锁继续生效；Anomaly 操作保持独立。新文案通过 `shared/i18n.js` 的 financeCopy 提供
中、英、日三语。

旧 `/query` 公开入口在新 Portal 闭集拒绝，在新 Billing 明确返回 `QUERY_PROTOCOL_REQUIRED`，不派发；原内部
Record 方法仍可供合法内部 caller 使用。旧客户端的 keyless UNKNOWN 不补 protocol，
不因安装新客户端获得重放能力。旧 Billing 和旧 Portal 不认识两个新路径，拒绝时不
回退旧入口。滚动发布和缓存旧页因此保持关闭；部署应先升级服务再启用新界面。

900030 up/down 及 manifest 注册原身份表和一次完成限制，保留原迁移字节；回滚明确
拒绝，原调用身份和回执与既有财务审计事实共同保留。runtime 为 `billing_service`，
只获 SELECT、INSERT 和两列完成更新，不能更改原身份、删除或截断记录。

永久验收包括真实 client 合同、现有 F02 浏览器 smoke 中的 Query 场景、真实 Go
路由/Portal 闭集、PostgreSQL 原调用/故障/并发/PAY-12 验收，以及严格 runner 强选的
`TestLatePaymentQueryCommandPostgres`、`TestLatePaymentQueryClientHTTPPostgres`。
本地受控 Provider 接口实现不替代真实外部支付渠道或 IdP 验收。

Query 客户端在派发及只读恢复时绑定当前 session 与已知授权版本代际；首次观察版本仅建立基线。
既有真实授权 observer 一旦发现已知版本变化，响应自身的 header 也会立即退休旧操作，
正文迟到不得确认原 UNKNOWN。相同 actor、session、工作区的 journal 跨合法授权刷新保留，
版本变更只退休挂起操作，不创建另一份历史。当前 me 快照落后于已知版本时，首次挂载或重挂均
不能生成新 key 或请求；同用户合法授权刷新后仍展示原 UNKNOWN，仍须人工明确确认新建。

测试中的 Close 中断叶通过注入活动 late case + PENDING/到期订单覆盖原 Sync 兼容分支。
当前生产 Escalate 会把订单置为 MANUAL_REVIEW，未证明存在恢复到该 PENDING 组合的生产者；
该叶仅证明注入兼容态的防重派，不宣称普通 Query 的真实 Close 生产窗口。

fresh事实保存原actor和本地authority，真实client派发前核当前me不落后于
已知授权版本，首次挂载同样受检查。当前合法授权下，人工retry只读原key/业务facts；当前authority
是独立的本地读取门禁，不替换原事实，不进入原五字段wire。不同actor/client/session不共享journal。
