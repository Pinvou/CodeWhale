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

## 当前代显式查询与历史 UNKNOWN

`900048_late_payment_query_generations` 追加独立、不可变的 command→UUID 绑定。
首次 Prepare 在同一个短事务中写入原命令、绑定和订单的当前 UNKNOWN 代次，提交确认后
才给该调用一次 Provider 执行资格；提交与 Provider 调用之间没有未登记代次窗口。
同 key 的重放和结果读取只读原记录，旧 UNKNOWN 不补绑定、不换 key、不重新派发。
Provider I/O 不持有该事务锁。新的显式调用保留完整 Sync、兼容 Close 和强支付归并，
只有其绑定代次仍是当前代时才更新当前查询事实；迟到强 SUCCESS 的证据及原资金守卫不被丢弃。

历史 UNKNOWN 本身不再永久阻止一次新的合法归档：必须由同案另一次显式新命令完成，
该命令绑定恰为订单当前代，完整查询确认权威未支付，并且原完成回执和证据已经提交。
当前命令未完成、Complete 失败、查询失败、PENDING、非权威或旧代结果仍不能准入。
当前投影的 UPDATE 即使已提交，确认丢失也不能被当成完整查询成功：当该调用以错误
完成时，同一 Complete 事务只把该命令仍匹配的当前代权威事实置回 UNKNOWN（SQL NULL），
再提交原错误证据和错误完成回执；如果事务失败，原命令仍未完成而阻断。旧代错误不得
清除新代成功事实，已完成回执的只读重试也不改写当前状态。回归中的 ACK 错误是实际
生产 UPDATE 提交、独立连接读证后的受控确认边界模型，不宣称真实网络协议故障演练。
普通内部非 command Sync 调用保留原路径，但不能替历史 UNKNOWN 冒充一条完成的显式命令。
其他案件的绑定、时间先后或旧回执都不能替代上述身份。

界面按新建操作的 begin 身份保留同案最新条目，不按响应到达时间排序。旧 UNKNOWN
继续显示原 ID，其 retry 仍只读；“新建复核”经明确确认生成新 ID。只有当前权限代际下
同案最新显式操作已确认完成，界面才允许尝试归档，服务端仍做最终准入。该例外不解锁
其他资金操作或其他案件。归档自身响应失联时仍保留其独立原 key 和事实，只确认原归档回执。

## 900048 部署及旧写方边界

按已批准的强约束方案，先排空并停止受影响的旧 query 和 archive 写方/入口，再以既有
部署身份执行完整迁移包并验证 ledger，之后启用新版写方和新界面。不要混用新旧写方。
900048 的延迟约束在提交时要求新 query 命令同时具有绑定和当前 UNKNOWN 事实；旧二进制
仅插原命令的事务会以 SQLSTATE `23514` 拒绝，不是兼容接纳路径。已存在的 UNKNOWN
原行不被回填、重派或伪造完成，也不因新 schema 获得旧 writer 的安全保证。

已发布 900030 和 900047 SQL 字节保持；新增 900048 前向迁移的 Down 明确拒绝。
schema 升级后不可通过回滚旧 binary、恢复旧 archive 入口或删除代次事实恢复旧行为。
回退须保持受影响入口停用，采取与该前向 schema 兼容的修复版；部署前须核实目标实例的
实际迁移身份，不能把某次只读观察推广为所有实例状态。本文是部署合同，不表示已执行生产迁移。

现有 Helm `templates/migrations.yaml` 的 `pre-install,pre-upgrade` hook 只排序迁移，
`app-deployments.yaml` 仍采用滚动更新；它们不排空旧 Billing replica。Compose 的
`service_completed_successfully` 依赖同样不停止已经运行的旧容器。不能直接执行普通
`helm upgrade` 或 `compose up` 并宣称满足上述停写要求。沿用
[`full-redeployment.md`](../deploy/runbooks/full-redeployment.md) 的站点命令清单、真实
ledger/备份核验和人工维护窗口，由 operator 显式完成以下步骤；本 PR 不新增自动停机框架：

1. 关闭受影响 query/archive 入口，记录并排空在途请求/事务；盘点所有同库旧写方，包含
   非本编排管理的实例，暂停会恢复 replica 的站点控制器/计划任务。不能确认全集则停止迁移。
2. Helm 站点按已渲染的真实 `<namespace>`、`<fullname>` 使用
   `kubectl -n <namespace> scale deployment/<fullname>-billing-service --replicas=0`，
   并确认旧 Pod/进程和在途事务实际退出。保持站点控制器暂停，保留发布值中通过 chart 校验的
   副本数，离线渲染目标版本的迁移模板：

   ~~~sh
   helm template <release> <chart> --namespace <namespace> --is-upgrade \
     <已核验站点参数> --show-only templates/migrations.yaml > migration-jobs.yaml
   ~~~

   从渲染结果仅提取 `migrate-billing` Job，核对其 namespace、迁移镜像 digest、
   `/migrations/billing`、`MIGRATION_ROLE=billing_service`、ServiceAccount 和 Secret 引用。
   确认目标站点已有匹配的 ServiceAccount、镜像拉取和数据库迁移配置。为 Job 设置明确
   namespace 和本次维护专用的新名称，去掉 Helm hook 注解，保留原 Pod spec。
   在旧写方持续停用时，用 `kubectl -n <namespace> create -f <审核后的单个Job文件>`
   创建此一次性 Job。若任一依赖、身份、迁移执行或结果无法确认，停在维护窗口内。
   Job 到达 Complete 后记录迁移日志，核对 `migration.post_verification`、完整 ledger
   和权限。验证成功后使用相同制品与正常副本值执行受审计的 `helm upgrade`，
   保留原 hook 执行及其相同包的幂等核验。确认启动的是新版，完成下方烟测后恢复入口与控制器。
3. Compose 站点先核完整 `<compose>` 参数及 `config --services`，使用
   `docker compose <compose> stop billing-service` 并核实际旧容器退出，再由原
   `docker compose <compose> run --rm billing-migrate` 执行迁移及其已有依赖检查。
   完整 ledger/权限验证成功且目标镜像确认后，才通过原站点发布流程启用新版服务。
   开发 Compose 不是生产拓扑的默认替代；无同名服务的站点使用经审核的等价停写程序。
4. 恢复入口前验证新命令原子绑定、同 key 只读恢复、当前未完成拒绝及旧归档回执恢复，
   保存维护记录。任一步未知/失败保持受影响入口关闭，不回退旧 binary 强行恢复服务。

数据库约束仅 fence 旧 query INSERT，不 fence 旧 archive UPDATE；因此停止全部受影响旧
archive 写方是独立前置，不能用一次 `23514` 或新表存在代替。命令中的占位符必须来自
核验后的站点清单，不包含仓库内置连接、凭据或未验证的部署地址。
