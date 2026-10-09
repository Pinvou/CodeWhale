# F02 审批结果恢复进程

`account-approval-recovery` 只恢复已经开始的原审批执行。它没有提交、批准、Begin、重新派发或直接修改财务表的权限。可信域结果已经成功时，Account 在原查询租约内重新读取证据并确认原执行；明确的执行前失败、原窗口已结束、域结果未确认且可信效果为 ABSENT 同时成立时，Account 才按既有 CAS 失效原执行。ABSENT、404、超时或缺少结果本身不证明没有资金效果。

## 数据库身份

1. 停止恢复进程，保持 `account_approval_recovery NOLOGIN`。按现有 Account 迁移工具安装全部当前迁移并完成 `verify`，保留输出的实际校验凭据。000068 的原 SQL 和校验值不变；它要求这个角色初始禁用。
2. 在正确的 Account 数据库，由受控操作员执行 `deploy/postgres/account-approval-recovery-role.sql`，显式传入 `-v ACCOUNT_MIGRATIONS_VERIFIED=true`。这个参数是操作员对上一步真实结果的确认，脚本不能替代完整迁移校验。脚本只启用已存在且属性、成员关系及表权限符合隔离要求的专属角色，并允许连接当前数据库；不会创建角色、密码或继承业务角色。
3. 由既有私密凭据流程供应 SCRAM 密码或受控证书身份及数据库连接配置。禁止把密码、连接凭据、投射令牌或查询租约写入仓库、命令行记录和运行日志。生产数据库认证和网络访问须只允许此进程连接相应 Account 数据库；对外连接使用已验证的数据库 TLS 配置。
4. 私密环境文件配置 `APPROVAL_RECOVERY_DATABASE_URL`，认证用户必须是真正的 `account_approval_recovery`。进程验证实际 `session_user/current_user`、安全属性和两项租约函数权限；不执行 SET ROLE/SET SESSION AUTHORIZATION，也不回退到普通 Account 连接。

后续升级前先停进程并由操作员恢复 `NOLOGIN`，再迁移、完整校验和显式激活。不要在恢复进程内提升角色权限；禁用 LOGIN 不会终止已经建立的会话，因此必须先停止进程并确认连接已退出。

## 工作负载与部署

- 使用 `workload-approval-recovery.json.template` 单独注册并投射 `account-approval-recovery` 的 JWT。Account receiver 只授予 `internal.approval-recovery.plan/confirm/zero-effect`；Billing receiver 只授予 `internal.billing.approval-effect.read` 和 `internal.billing.approval-result.read`。普通 Account、Portal、审批人以及其它服务不能调用恢复确认端点。更新两个 receiver 的完整规则文件后按既有流程滚动加载规则。
- 从同一源码构建 `Dockerfile --build-arg SERVICE=account-approval-recovery`，使用经独立核验的本地镜像。合并 personal、workload、finance 和 `compose.approval-recovery.yaml`，仅在操作者明确启用 `approval-recovery` profile 时启动恢复进程和其令牌 agent。没有公网端口或新的审批入口。
- 模板保持既有 Account/Billing backend 私有 HTTP 契约，明文地址仅允许对应的 `account-service` 和 `billing-service` 服务名，要求独立、受控的内部网络，不能暴露到公共入口；其它地址须使用操作者验证的 HTTPS 源站。`APPROVAL_RECOVERY_CA_FILE` 使用独立根证书验证，传输不使用环境代理，不跟随重定向，不修改全局 HTTP transport。投射文件必须为绝对路径 JWT；不支持静态内部令牌回退。
- `APPROVAL_RECOVERY_INTERVAL` 默认 1 分钟，可配置 1 分钟至 1 小时；`APPROVAL_RECOVERY_BATCH_LIMIT` 默认 200，范围 1–200。每轮总预算 30 秒，每个查询继续受原 15 秒数据库租约约束。就绪状态要求最近一轮成功且数据库连接正常，退出时取消当前轮并有界等待租约释放后关闭连接。

## 未知结果与运维核实

每轮记录候选、确认、失效、待核实、不可用计数及是否仍有候选，不输出请求 ID、原幂等键、租约、域证据或原始错误。`pending` 表示原执行仍保持未知；`unavailable` 表示此次查询没有可信完成，不能据此认为原执行失败。持续异常必须通过既有审批详情和受控运维查询核实，而不是删除租约、改尝试次数、直接清资金冻结或重发执行请求。

规划器跳过活跃租约与未到冷却时间的请求；尚未调查的候选优先，之后按已到期冷却时间稳定轮转。一次候选错误不会阻止同轮其它已经选出的请求；预算耗尽后下一轮继续。`more_candidates` 只描述本轮可查询集合，不能作为所有历史请求已经恢复的证明。

已批准投递在 grant 后、Begin 前崩溃的历史状态没有执行记录，仍不属于该结果恢复规划器；它必须保留人工核实，不能以无 execution 或无域结果猜测零效果。该已知缺口按 F02 审核单独登记，不在本轮新增原子审批接口或自动重新执行。

## 验证范围

测试必须区分受控 HTTP 证据 fixture 和真实资金执行。真实 PG 专项使用完整 Account/Billing 迁移、原批准和投递流程、专属低权限 LOGIN、真实 TLS/JWT receiver，验证缺少域结果仍保持 EXECUTING/单次尝试、没有新财务动作、查询租约释放与持久冷却；它不宣称真实供应商或生产部署已验证。运行结果由本次 PR 审核记录提供。
