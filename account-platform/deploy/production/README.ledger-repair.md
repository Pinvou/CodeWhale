# 账本归属修复

在当前已审生产叠加文件之后最后加入 `compose.ledger-repair.yaml`。它复用所有现有镜像变量和 Compose 投放流程；Account、Billing、Portal、Token、migrator、relay 和 approval-resources 必须来自同一个 F10 源，包含当前管理表单资产。外部 HTTPS、KMS、身份、工作负载规则和财务服务隔离仍沿前序配置。

安装完整公开迁移目录，保留所有既有 SQL 校验和；本波新增原 Account071、Billing061/062，原 Billing060 在前序存储闭包中。`LEDGER_REPAIR` 原 policy v1 / snapshot v1 默认 INACTIVE，部署或启动不会自动激活。按 `deploy/runbooks/ledger-repair-activation.md` 的既有离线 quiesce/fence/核验/独立审批发行合同激活，不可借用旧 digest 或 receipt。

Billing 前向迁移 900014 为固定 Ledger v1/s1 增加独立 owner 锁能力；900013 的 ENT v2/s2 能力和已发布 SQL 保持不变。新修复事务先读取同审批的不可变回执；没有回执时，必须在写账事务内锁住当前激活行，确认 ACTIVE 和授权执行调用冻结的 activation version 一致，再生成修复分录。停用与新写账在该行锁上串行，运行角色只有私有 schema 的 USAGE 和固定无参函数的 EXECUTE，不能修改激活表或继承 owner。已提交的同事实回执在停用后仍可只读回放。

900014 的 Down 显式拒绝数据回滚。运维回滚通过停 consumer 和将激活置为 INACTIVE，保留已执行事实和历史回执；不得删除修复分录或重写旧 migration ledger。

当前集合是 Billing5 独立消费者和 Token3 消费者，Token19 管理策略保持。通过现有 approval-resources 状态、Q+1 初始化和严格 readback 流程重新发行；不得在流量中修改 consumer，不放宽 ACL，不用启动自动创建代替 operator 流程。

管理入口为现有平台财务页的账本修复表单。请求人有 `platform.billing.repairs.request`，审批人有 `platform.billing.repairs.approve`，不得自批。表单显式选择目标过账、原分录、错误与正确平台账户及正 int64 金额；原因代码固定为 ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT。Account 只读预检查获取 Billing 权威冻结事实，数字 JSON 保持精度，创建草稿后转入原审批列表完成提交、审批及恢复。金额正值表达修复规模，最终冲正方向由原不可变目标过账符号决定；钱包/客户字段不作为平台归属修复输入。

当前 typed detail 包含 frozen 原分录、过账、错误/正确平台账户和十进制金额，EDIT 保持原意图并重读权威事实。错误、未知 ACK 沿现有严格同键回读和独立查询恢复合同，不把 HTTP 状态当作无 effect 证明。

当前私有完整候选已通过统一 Portal 构建，并在生成之后编译 token-portal 以绑定当前嵌入资产；这不代表实际相邻传播、issuer 发行或线上部署。Native 管理入口沿现有 Portal 管理页打开方式；未增加新的 Native admin API。
