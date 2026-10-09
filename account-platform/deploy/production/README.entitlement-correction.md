# 权益组成修正首次交付

这是 `ENTITLEMENT_COMPONENT_ADJUSTMENT` policy v2 / snapshot v2 的现有部署增量。使用已审生产叠加文件，最后加入 `compose.entitlement-correction.yaml`。它复用现有镜像变量与 Compose 流程；全部镜像、迁移目录和 Portal 生成资产必须来自同一个已审 F09 源。不能以旧 Account/Billing/Portal 二进制配新表或新页面。

使用现有公共 migrator 安装原字节 Account069/070、Billing056–059；原 v1 activation 不修改，保持 INACTIVE。Billing 的 v2 激活仍须满足现有 Account authority、policy digest、handler digest、proof claim、身份和发行前置。此叠加文件不激活命令，不生成身份、签名或 receipt。

Account 的 `BILLING_HTTP_URL` 指向实际 Billing owner；现有 workload 凭据以 `file:<path>` 配置并由请求时读取。原 workload-billing-rules 已授 Account service `internal.billing.approval-proof.read`，新 precheck 使用同一权限，并额外严格要求 Account service 身份。Portal 仅透传真实会话/CSRF 的 Account 管理 precheck 和既有 typed governed approvals；没有公开 Billing 内部接口。

静态 registry 从 Billing3 增至 Billing4，保留 Token3 消费者和 Token19 管理策略。必须按既有 approval-resources 的状态、quiesce、fence、Q+1 初始化、严格 readback、外部核验 receipt 和 activate/resume 流程重新发行闭合配置。旧三消费者 manifest/digest/receipt 不可冒用；不允许在有流量时修改旧 consumer，不放宽 NATS ACL，也不以启动自动创建代替该流程。公开身份和 digest 仍通过原 `compose.finance.yaml` 的受控只读挂载输入。

页面入口是现有 Desktop「打开门户」进入有管理权限的 `/admin.html`，在财务管理的权益修正专属表单选择包、原支付订单和理由。该流程只读取 owner v2 冻结投影，服务端 proof/claim 再核事实，并由真实 executor 按既有审批准则执行；表单不自行授权或修改财务事实。Decimal 字符串仅是浏览器 int64 的无损表示，发送 wire 保持原 JSON 整数。

旧审批请求、Token/model 管理入口及个人财务保留。查询优先恢复使用同请求的既有 immutable domain result；提交 ACK 未知时冻结相同意图和 key。编辑发生包、订单或 reason_code 变化须重新读取 owner，而不是替换旧 snapshot。没有新增 Native 管理域、登录方式或未来财务命令。
