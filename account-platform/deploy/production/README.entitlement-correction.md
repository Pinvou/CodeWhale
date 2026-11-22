# 权益组成修正首次交付

这是 `ENTITLEMENT_COMPONENT_ADJUSTMENT` policy v2 / snapshot v2 的现有部署增量。使用已审生产叠加文件，最后加入 `compose.entitlement-correction.yaml`。它复用现有镜像变量与 Compose 流程；全部镜像、迁移目录和 Portal 生成资产必须来自同一个已审 F09 源。不能以旧 Account/Billing/Portal 二进制配新表或新页面。

使用现有公共 migrator 安装原字节 Account069/070、Billing056–059，以及新增 Billing900013。原 v1 policy/snapshot 行和历史审批保留，v1 activation 保持 INACTIVE；新增 v2 行安装后也为 INACTIVE。Billing 的 v2 激活仍须满足现有 Account authority、policy digest、handler digest、proof claim、身份和发行前置。此叠加文件不激活命令，不生成身份、签名或 receipt。

升级顺序沿用原受控部署窗口：先按既有流程 quiesce 并 fence relay/consumer，保留历史与未知投递；不得让仍只识别旧 activation 集合的读者继续读取已经追加 v2 行的数据。部署能同时接受原 v1 历史和并存 v2 行的当前 Account/Billing activation、proof、effect/result 读者，并核验实际 owner 回读；新 v2 保持 INACTIVE，原 v1 历史不替换、不激活。Billing 进程还依赖当前七 consumer 的已审配置和回执，不能把旧三项资源或不存在的 reader-only 启动模式当成新读者就绪。迁移、同源二进制、七 consumer 初始化和外部回执完成后，再核验当前读者及 proof/claim/executor 全链实际就绪，最后由原受控 operator 单独执行 activate/resume。兼容读取不等于激活授权，也不承诺无停机滚动升级。

Billing900013 仅安装固定、无参数的 owner 锁能力：运行角色只有该私有 schema 的 USAGE 和函数 EXECUTE，没有 activation 表 UPDATE、schema CREATE 或 owner 身份继承。新执行在资金事务内锁定 ENTITLEMENT_COMPONENT_ADJUSTMENT policy2/snapshot2 的 activation 行；运维停用必须等待已持锁事务结束，之后的新执行失败关闭。已经提交的 immutable result 优先只读回放，停用不能使既有回执失效。

数据迁移不提供回滚删除：Account069 和 Billing900013 的 Down 明确返回 SQLSTATE55000，不删除 policy、触发器、账务或 migration ledger。运维回退应停止专属 consumer，并由既有受控 operator 将 v2 activation 置为 INACTIVE，保留旧事实及未确认投递用于恢复。Issue1664 的旧 Account027 项由前序单独处理，不能以本波069验证宣称整项完成。

Account 的 `BILLING_HTTP_URL` 指向实际 Billing owner；现有 workload 凭据以 `file:<path>` 配置并由请求时读取。原 workload-billing-rules 已授 Account service `internal.billing.approval-proof.read`，新 precheck 使用同一权限，并额外严格要求 Account service 身份。Portal 仅透传真实会话/CSRF 的 Account 管理 precheck 和既有 typed governed approvals；没有公开 Billing 内部接口。

静态 registry 从 Billing3 增至 Billing4，保留 Token3 消费者和 Token19 管理策略。必须按既有 approval-resources 的状态、quiesce、fence、Q+1 初始化、严格 readback、外部核验 receipt 和 activate/resume 流程重新发行闭合配置。旧三消费者 manifest/digest/receipt 不可冒用；不允许在有流量时修改旧 consumer，不放宽 NATS ACL，也不以启动自动创建代替该流程。公开身份和 digest 仍通过原 `compose.finance.yaml` 的受控只读挂载输入。

页面入口是现有 Desktop「打开门户」进入有管理权限的 `/admin.html`，在财务管理的权益修正专属表单选择包、原支付订单和理由。该流程只读取 owner v2 冻结投影，服务端 proof/claim 再核事实，并由真实 executor 按既有审批准则执行；表单不自行授权或修改财务事实。Decimal 字符串仅是浏览器 int64 的无损表示，发送 wire 保持原 JSON 整数。

旧审批请求、Token/model 管理入口及个人财务保留。查询优先恢复使用同请求的既有 immutable domain result；提交 ACK 未知时冻结相同意图和 key。编辑发生包、订单或 reason_code 变化须重新读取 owner，而不是替换旧 snapshot。没有新增 Native 管理域、登录方式或未来财务命令。
