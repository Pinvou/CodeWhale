# 管理后台治理与安全及审批中心整改设计

GSA-07 的身份投递、迁移、专项就绪及永久 replay 禁令操作约束见[审批授权 Relay 手册](../deploy/runbooks/approval-authorization-relay.md)；实现/环境状态以任务清单为准。

> 状态：需求已确认 / 未实现 / 未部署
>
> 版本日期：2026-09-14
>
> 正式需求编号：`UI-60`、`ADM-67`、`NFR-57`

## 0. 文档效力与现状边界

本文是“治理与安全”信息架构、审批中心展示、审批命令激活、批准 proof、失败恢复和财务高风险命令扩展的单一详细真相源。`requirements.md` 定义稳定目标和总体边界；本文细化页面、状态、接口、安全与验收合同。[十四命令事实与兼容基线](admin-governance-security-command-baseline.md)只记录GSA-02核对的当前仓库事实和缺口，不另定义目标协议。与通用后台设计冲突时，本专项优先；与已经实现的领域不变量、既有审批状态机或安全边界冲突时必须停止实现并修订文档，不能由 Portal 推断或覆盖服务端事实。

本次是对既有能力的补齐，不是重建审批系统。当前已经存在且必须保持兼容的能力包括：

- Approval request 的 create、update、submit、withdraw、decide、`supersedes_request_id`、`allowed_actions`、list/detail；
- 六类真实可执行命令：`WALLET_ADJUSTMENT`、`PAYMENT_PROVIDER_RESUME`、`PAYMENT_PRODUCT_RESUME`、`TOKEN_MODEL_LIFECYCLE_CHANGE`、`TOKEN_MODEL_CATALOG_ROLLBACK`、`TOKEN_CREDENTIAL_REVOKE`；
- Account 授权事件、领域执行确认、未知结果查询刷新、三语展示，以及 Token 创建/提交时的权威快照链；
- Token 已有 `PRE_ACTIVATION / ACTIVE / SAFE_ROLLBACK` 持久 activation、单调 epoch、consumer 和 executor guard；
- Billing、Token 已有 `admin_pagination` 有界快照分页基础。

当前缺口是安全可见目录、最小列表与闭合详情 presenter、逐命令 activation 投影、批准 proof、历史误批准收敛、逐命令失败恢复，以及八类仍只有 policy 的财务命令领域设计。上述增量未实现、未部署。

## 1. 目标、非目标与分期

### 1.1 目标

1. “治理与安全”只展示当前主体可见且已有真实接口的审批、审计、系统与偏好能力，不以角色名或前端常量扩权。
2. 审批中心只承担最小列表、闭合详情、决定和安全恢复安排；复杂业务草稿始终由所属业务页创建、编辑、重新读取事实并续提或替代。
3. 未完成领域 schema、executor、consumer、恢复、测试和发布门禁的命令即使已有 policy，也不能创建、更新、提交或批准。
4. 批准前按命令校验 closed-schema 权威 proof，执行失败按命令分类和窗口恢复；未知结果只查证，不盲重放。
5. 兼容 N/N-1、旧 URL 和已执行历史，不复制审批时间线，不改变普通 `AUDIT_ONLY` 命令。

### 1.2 非目标

- 不允许管理员、审批中心或普通浏览器触发 Payment Provider 主动探测；PAY-57 的探测只由专用受信工作负载以一次性意图执行。
- 不在 Account 内解释 Billing/Token 业务字段，不让 Approval service 成为领域事实源。
- 不为 Wallet 新增近期认证，也不改变其关键字段；如产品以后要求，必须追加新 policy version 后另行实施。
- 不新增 `DRAFT -> WITHDRAWN` 或 `DRAFT -> ABANDONED`；不通过前端隐藏制造新状态迁移。
- 不提供通用“重试全部失败申请”、通用事件重放、Provider 探测、跨域修复执行或浏览器传入 proof。
- 不在本次展示尚未实现的异常处置导航；不改变普通 `AUDIT_ONLY` 命令已有路径、权限、状态机和审计。

### 1.3 分期与上线边界

| 阶段 | 范围 | 激活边界 |
|---|---|---|
| P0 | 命令 activation/executable fail-closed、历史 APPROVED 收敛、三层执行 gate、目录与 presenter 安全底座 | 全部门禁完成前仅可读；不得开放新增命令 |
| P1 | 治理与安全导航、我的申请/待我审批、闭合列表详情、有界 cursor、业务页双向链接、Token 三命令和 Billing 既有三命令 proof、逐命令恢复 worker | 逐命令、逐 policy version、逐 activation epoch 放量 |
| P2 | 八类 policy-only 财务命令逐类领域设计、补偿、外部证据、执行与 E2E；异常索引另行确认 | 每个命令家族独立满足全部门禁后才可激活；不得整体打包激活 |

## 2. 信息架构、可见性与权限

### 2.1 治理与安全导航

固定保留“治理与安全”导航组，子项为：

1. **审批中心**：我的申请、待我审批、执行与失败、审批策略目录；
2. **审计与导出**：沿用既有审计权限和导出合同；
3. **系统与安全**：沿用系统状态和只读运行手册合同；
4. **偏好设置**：仅有真实本地偏好且当前管理页可访问时显示。

导航与旧 `section`、settings 入口和历史深链必须保持兼容；规范化只能使用一次 `replaceState`，不得产生循环跳转。双向链接只保存稳定 request/target 引用和返回上下文，不复制审批时间线或领域事件。

T9 异常索引在其服务端索引、权限、脱敏、Portal 路由和 E2E 均未实现前不显示导航、入口、空壳或占位卡。普通 audit-only 队列仍走原页面，不改道到异常索引。

### 2.2 审批目录与动作语义

Portal 不直接读取需要 `platform.roles.read` 的内部 policy API。Account 提供新的窄、只读、安全目录投影；它聚合既有 policy registry 与领域 activation，但不是可写 activation 真相源。

目录只返回以下两个布尔语义：

- `visible`：当前主体依据当前 requester/approver 权限与作用域可读取该命令版本的目录项，不等于可申请、可查看任意历史申请或可批准具体申请；
- `requestable`：命令已激活、当前主体具备冻结 requester permission、作用域和必要近期认证入口，可从所属业务页发起；

具体申请能否批准只由 detail 的服务端 `allowed_actions` 表达，Directory 不返回或推导第三种批准布尔值。`APPROVE` 仍在决定事务前重新校验冻结 approver permission、作用域、非自审、近期认证、policy、activation 和 proof，`allowed_actions` 不是永久授权。

“全部”只能表示以下当前主体可见集合的并集，不得暗示平台全量：① `requester_user_id` 等于当前稳定用户 ID 的全部历史申请；② 当前仍具备申请冻结 `approver_permission` 与作用域的非本人申请。权限撤销后，第二组立即从审批中心消失并中止请求/清缓存；主体自己的历史申请仍按稳定 requester ID 保持最小审计可读。曾审批但不属于自己的申请在审批权限撤销后不再因“曾经处理过”获得审批中心可见性，其历史决定只能由具备 `platform.audit.read` 的审计页面读取。若未来需要跨主体全量读取，必须另行定义并审核 `read.all` 类权限；现有读权限不能推导该能力。Directory 的 presenter/activation 可见性只控制当前目录和新动作，不得阻断上述历史最小 envelope 读取。authorization version 或 activation epoch 变化必须中止在途请求并清理当前目录、非本人申请和动作缓存；迟到响应不能恢复已撤销内容。

## 3. 命令 activation 的唯一权威

### 3.1 Token 三命令

`TOKEN_MODEL_LIFECYCLE_CHANGE`、`TOKEN_MODEL_CATALOG_ROLLBACK`、`TOKEN_CREDENTIAL_REVOKE` 必须复用 Token 已有持久 `PRE_ACTIVATION / ACTIVE / SAFE_ROLLBACK` 状态和单调 epoch。Account 不新建第二份 Token activation 表、不独立推进状态；Account 只在 create/update/submit/approve/Begin 时强读 Token 的受信兼容投影并绑定 state、epoch、policy version 与命令摘要。

- `PRE_ACTIVATION`：新申请及执行失败关闭；已有申请可按本文只读或安全收口。
- `ACTIVE`：只有 policy、schema、presenter、proof、consumer、executor、recovery、测试和部署 gate 均一致时允许相应动作。
- `SAFE_ROLLBACK`：停止新建、更新、提交、批准和领取新执行；旧读保持，执行中任务按已有 epoch/fencing 规则收敛，不恢复 direct 写旁路。

### 3.2 Billing 命令

当前仓库尚无 Billing owner-domain 持久 activation 行：三类既有命令由静态 `Executable=true` 维持现有能力，以下八类只有 policy 并由静态 `Executable=false` 有效失败关闭。GSA-03 的前向迁移必须建立兼容 activation 投影，把既有 `WALLET_ADJUSTMENT`、`PAYMENT_PROVIDER_RESUME`、`PAYMENT_PRODUCT_RESUME` 物化为 `ACTIVE`，把以下八类物化为默认 `INACTIVE`；升级不能使三类合法命令回退，也不能在八类各自门禁完成前使其可执行：

1. `ENTITLEMENT_COMPONENT_ADJUSTMENT`；
2. `LEDGER_REPAIR`；
3. `RECEIVABLE_WRITE_OFF`；
4. `PAYMENT_FINANCIAL_REPAIR`；
5. `REFUND_EXCEPTION_REPAIR`；
6. `CHARGEBACK_FINANCIAL_REPAIR`；
7. `RECONCILIATION_FINANCIAL_REPAIR`；
8. `USAGE_SETTLEMENT_REPAIR`。

Billing activation 的写入只属于受信前向迁移/运维边界；Directory 只读聚合。同名未知版本、缺 activation 行、policy/activation 混合版本、N/N-1 不可理解状态全部失败关闭。GSA-03 的兼容发布固定为 Billing activation 前向迁移 → Billing 闭合只读投影 → Account 强读消费者；回滚固定为 Account → Billing 服务，并保留已产生 current/revision 历史的 activation 迁移，不执行破坏性 down。回滚不得删除仍被 Approval request、Outbox、Inbox、execution 或领域效果引用的 policy/activation 历史。

### 3.3 服务端动作封锁

服务端必须在 create、update、submit 和 APPROVE 四个入口校验可执行 activation，不能只依赖 Portal 隐藏动作。现有 `CreateApprovalDraft` 已通过 `validateApprovalDraftRequest` 校验静态 `approvalpolicy.Executable`，并对 Token 命令调用现有 activation guard；该安全检查必须保留。增量缺口是把四个入口统一绑定逐领域持久 activation/epoch、policy version 与兼容投影，尤其不能让仅有 policy 的八类 Billing 命令因进入静态 allowlist 而提前开放。未激活命令的已有 request 仍可按权限读取：

| 当前状态 | 未激活时允许 | 未激活时禁止 |
|---|---|---|
| DRAFT | 只读、跳回业务页查看失效原因 | update、submit、withdraw、自动刷新快照 |
| SUBMITTED | REJECT、WITHDRAW 安全收口 | APPROVE、修改命令、执行 |
| APPROVED | 只读并进入第 9 节核对 | 新增/重放 Outbox、直接执行 |
| EXECUTING | 只查领域结果并按三层 gate 收敛 | 人工重发、重新批准 |
| 其他终态 | 只读与关联新业务操作 | 重开、改写历史 |

## 4. 申请状态、编辑与替代矩阵

### 4.1 DRAFT 与中心直接提交

DRAFT 不可撤回。只有审批中心的快捷 submit 受“自创建后完全未修改”限制：服务端重新取得最新 `allowed_actions`，客户端提交精确 `expected_version`，activation/policy 未变化，并在提交前重新执行 precheck。任何编辑入口、事实漂移、策略变化、版本冲突或 precheck 失败都必须跳回所属业务页；审批中心不得自动刷新领域快照、修改复杂表单或重建命令。

业务页可以读取并更新仍处于 DRAFT 且已激活的现有申请。业务页 Update 必须重读当前领域事实，生成新的命令与 snapshot 摘要，以 `expected_version` CAS 更新同一 DRAFT；Update 成功后可按返回的新版本和最新 `allowed_actions` 再执行服务端 precheck 并 submit，不因“已修改”被强制 fresh superseding。只有 SUBMITTED 之后或终态需要改变意图时，才按第 4.2 节创建 fresh superseding。

### 4.2 fresh superseding

业务页是复杂表单和领域意图的唯一所有者。进入业务页后必须重新读取当前事实、重新校验权限和近期认证，并创建新的命令摘要、snapshot、request ID 和幂等键；旧 request 仅作为 `supersedes_request_id` 关联，不继承批准、意见、快照或执行权。

| 原申请状态 | 用户下一步 |
|---|---|
| DRAFT | 未修改时可在审批中心快捷 submit；也可在业务页重读事实、Update 同一 DRAFT，并按最新 expected_version/precheck/CAS submit。未激活 DRAFT 只读 |
| SUBMITTED | 申请人可按 `allowed_actions` 撤回；需要改动时先撤回，再由业务页 fresh superseding |
| APPROVED / EXECUTING | 只读；等待执行或只查结果，不允许替代覆盖在途授权 |
| EXECUTION_FAILED：transient 且安全 | 由逐命令恢复 worker 在原执行 ID、次数和窗口内安排重试 |
| EXECUTION_FAILED：unknown | 只查询原执行/领域引用；不得创建替代或重放副作用 |
| EXECUTION_FAILED：permanent、事实/策略变化或窗口结束 | 先确定性 `INVALIDATED`，再由业务页 fresh superseding |
| REJECTED / WITHDRAWN / EXPIRED / INVALIDATED | 业务页基于当前事实创建 fresh superseding |
| EXECUTED | 可创建新的关联业务命令，但不是撤销、覆盖或重开旧执行 |

## 5. 列表、聚合、详情与脱敏

### 5.1 最小列表 envelope

`GET /admin/v2/approval-requests` 不得返回完整 `ApprovalRequest` 或原始 command JSON。v2 closed base envelope 仅含列表所需稳定字段：request ID、command type、policy version、snapshot schema version、风险级别、最小目标摘要、申请人最小摘要、状态、提交/到期/更新时间和最小关联 ID；v2 列表不返回 `allowed_actions`、`critical_resource_versions`或原始命令，也不提供直接决定入口。命令摘要采用 `command_type + policy_version + snapshot_schema_version` discriminated oneOf；family 只能复用字段 fragment，不能放宽为任意对象。无 presenter 的历史命令返回最小 base envelope 和稳定 `DETAIL_PRESENTATION_UNAVAILABLE`，不得回退原 JSON。v2 详情 closed base envelope 在上述最小字段之外增加服务端当前 `allowed_actions`，具体批准、拒绝或撤回只能从详情页发起，并在决定写入前重验。v1 列表在第 5.4 节兼容窗口内继续返回旧 `allowed_actions/critical_resource_versions/financial_snapshot`，不得偷换为 v2 线格式。

### 5.2 聚合统计

顶部统计若展示，必须来自独立、权限裁剪的聚合接口，不能扫描当前页或把本页条数冒充总量。未实现聚合接口时删除顶部统计。`处理中` 只统计 `APPROVED + EXECUTING`；DRAFT、SUBMITTED、EXECUTION_FAILED 和终态分别按闭合定义展示。无权与失败不能显示为 0。

### 5.3 closed detail presenter

详情响应使用 closed base envelope 与按 `command_type + policy_version + snapshot_schema_version` 区分的 oneOf；每个 presenter 明确 required、optional、脱敏、允许差异和错误字段，`additionalProperties:false`。v2 detail base envelope新增required正整数`action_control_version`，其值精确等于生成`allowed_actions`时锁定的`ADMIN_APPROVAL.version`；v1 body保持逐字兼容，但成功detail必须返回`X-Pinvou-Approval-Action-Control-Version: <positive-decimal>`。v1/v2 detail与capability均返回`Cache-Control: no-store`。执行信息只返回对人工判断必要的 attempt 状态、稳定错误分类、`retryable`、`next_retry_at`、窗口和脱敏领域引用；不返回工作负载凭据、内部网络、原始堆栈、任意服务 token、完整幂等材料或原始领域 JSON。

目录当前 `visible` 的命令版本必须存在同步 presenter；`requestable=true` 还要求 presenter 与 activation 同版本就绪，缺 presenter 时目录不得宣称可申请。历史申请的最小 envelope 读取独立于当前 Directory visible/presenter/activation；历史无 presenter 项仍显示最小 envelope 和 `DETAIL_PRESENTATION_UNAVAILABLE`，不影响审计留存。

对 approval request 的 detail、decision、events/timeline、execution/result 和 recovery 子资源，不存在与当前主体不可见必须字节级等价返回 404 `APPROVAL_REQUEST_NOT_FOUND`：HTTP status、closed error body、header 集、`Cache-Control: no-store`、响应大小分桶和时延分桶相同。除通用请求大小/CSRF/身份验证外，Account 必须先用当前主体和授权版本执行授权裁剪的存在性读，通过后才可调 presenter、解析命令/摘要、取 proof 或发布事件；不可见路径的 presenter/proof/领域查询/事件数均为 0。`platform.audit.read` 只能读取独立审计端点的脱敏审计投影，不能作为审批 detail/decision/event 的越权后门。

### 5.4 v1/v2 线协议与升降级

本增量选择版本化 path，不用模糊 Accept 协商：现有 `/admin/v1/approval-requests*` 为 v1 兼容线，新增 `/admin/v2/approval-directory`、`/admin/v2/approval-requests`、`/admin/v2/approval-requests/{id}` 及对应 create/update/submit/withdraw/decide 子资源为 v2 线。v1 列表/详情保持现有完整 `ApprovalRequest`、`allowed_actions`、`critical_resource_versions`、`financial_snapshot` 线格式，v1 decide 仍接受 `observed_versions`；Account N 在该字段后端已启用 proof 时忽略其授权语义，但不删字段。v2 列表只返回第 5.1 节最小 envelope，v2 detail 只返回第 5.3 节 closed presenter，v2 decide closed body 恰为 `{expected_version,action_control_version,decision,reason}` 且拒绝 `observed_versions/proof*`；动作版本必须与服务端控制版本在最终写事务内一致。两线都必须使用同一 request 和状态机，不允许复制数据或双写。

Account DB 必须新增唯一持久控制记录作为 wire/proof cluster gate 与授权Relay栅栏的共同真相，不能使用第二控制表、进程环境变量或 Portal 缓存代替。closed 字段固定为 `control_id="ADMIN_APPROVAL"/schema_version=1/mode/approval_wire_version/proof_required/authorization_relay_state/approval_epoch/authorization_relay_epoch/version/updated_at`；首次前向迁移只能初始化为 `LEGACY/wire=1/proof_required=false/RUNNING/approval_epoch=1/authorization_relay_epoch=1/version=1`。`mode` 仅 `LEGACY/QUIESCING/PROOF_REQUIRED/SAFE_ROLLBACK`，wire仅`1/2`，`authorization_relay_state`仅`RUNNING/FENCED`，两个epoch与version均为正整数。mode/wire/proof变化原子递增`approval_epoch + version`，Relay状态变化只递增`authorization_relay_epoch + version`；不得用恢复Relay的epoch变化使同一proof epoch中合法批准失效。合法mode组合冻结为：`LEGACY=(wire=1,proof_required=false)`、`QUIESCING=(wire=1,proof_required=false)`、`PROOF_REQUIRED=(wire=2,proof_required=true)`、`SAFE_ROLLBACK=(wire=1,proof_required=true)`；Relay状态作为同一行的正交受控字段。合法mode边仅为 proof 开启前的 `LEGACY -> QUIESCING`、尚未开启 proof 时经同样 CAS 的 `QUIESCING -> LEGACY`、`QUIESCING -> PROOF_REQUIRED`，以及开启后的 `PROOF_REQUIRED <-> SAFE_ROLLBACK`；禁止跳级、禁止 `proof_required:true -> false`，也禁止 PROOF_REQUIRED/SAFE_ROLLBACK 回到 LEGACY/QUIESCING。QUIESCING 回 LEGACY 只允许在授权Relay尚为RUNNING、尚未持久化Q、旧Billing durable与ACL仍完整时执行，并须先停扫描器、证明本轮没有已释放Begin/新execution；一旦同一行的Relay状态已FENCED并持久化Q或旧durable被停用，即越过不可逆切换点，只能留在QUIESCING修复后前进，不能恢复LEGACY。已经CAS INVALIDATED的历史必须保留，不能回写APPROVED。

mode 与 Relay 物理状态的完整动作矩阵冻结如下；未列为允许的动作均失败关闭，Directory 始终只返回 `visible/requestable`，不返回具体申请的 `approvable`：

| mode | Relay | create/update/submit | APPROVE | Account Begin | REJECT/WITHDRAW、历史读取与 Relay |
|---|---|---|---|---|---|
| LEGACY | RUNNING | 按逐命令activation与既有v1合同 | 仅既有active命令按v1批准并原子写Outbox | 已发布legacy事件按既有epoch/deadline允许 | REJECT/WITHDRAW仅SUBMITTED；读取可用；Relay可claim/publish |
| LEGACY | FENCED | 按逐命令activation与既有v1合同 | 禁止，使用下述稳定503，request保持SUBMITTED | 已发布事件仍按既有epoch/deadline判断，FENCE本身不授权也不撤销Begin | 安全收口/读取可用；Relay零claim/publish；仅受控maintenance或QUIESCING回退前暂态 |
| QUIESCING | RUNNING | 已激活命令仍可维护DRAFT/submit，不能据此批准 | 禁止，503 `APPROVAL_QUIESCING` | 仅栅栏前legacy原event在5分钟deadline内允许 | 安全收口/读取可用；Relay只完成原行首次发布与claim回收 |
| QUIESCING | FENCED | 同上 | 禁止，503 `APPROVAL_QUIESCING` | 新Begin关闭，既有Begin/execution只查证 | 安全收口/读取可用；Relay零claim/publish；只允许继续cutover，或在Q未持久化且旧durable/ACL完整时受控恢复RUNNING后再回LEGACY |
| PROOF_REQUIRED | RUNNING | 按逐命令activation与v1/v2服务端proof合同 | 允许；v1/v2均取得同一proof并原子写canonical Outbox | 仅匹配proof/approval epoch的已发布事件允许 | 安全收口/读取可用；Relay可claim/publish |
| PROOF_REQUIRED | FENCED | 按逐命令activation与v1/v2合同 | 禁止，使用下述稳定503，request保持SUBMITTED | 已发布且匹配proof/approval epoch的事件仍按deadline判断 | 安全收口/读取可用；Relay零claim/publish；仅受控maintenance或SAFE恢复暂态 |
| SAFE_ROLLBACK | RUNNING | 全部禁止 | 禁止，503 `APPROVAL_SAFE_ROLLBACK` | 全部禁止；已有Begin只查证 | 仅SUBMITTED安全收口和鉴权后读取；Relay可执行下述`SAFE_DRAIN` Claim并归一到FENCED |
| SAFE_ROLLBACK | FENCED | 全部禁止 | 禁止，503 `APPROVAL_SAFE_ROLLBACK` | 全部禁止；已有Begin只查证 | 仅SUBMITTED安全收口和鉴权后读取；Relay零claim/publish，按第8.7节恢复 |

LEGACY/PROOF_REQUIRED 任一入口在proof前强读到 `FENCED` 时，APPROVE固定返回503 `APPROVAL_RELAY_FENCED`、`Retry-After: 5`、`Cache-Control: no-store`，零proof/Outbox/request状态变化，request保持SUBMITTED，detail `allowed_actions`不含APPROVE。入口读到RUNNING后若与FENCE竞态，最终APPROVE短事务仍须FOR SHARE重验；FENCE先提交则返回同一503且零Outbox，已经发起的只读proof必须丢弃、不能绑定或改变request。APPROVE先取得最终共享锁则先提交Outbox，FENCE随后在计数复核中看见它。该竞态不得靠远程proof期间持DB锁解决。

Relay状态边也必须由DB受控CAS与同一行锁冻结：`RUNNING -> FENCED`仅允许具名`CUTOVER/MAINTENANCE/SAFE_RECOVERY`流程，且须满足可发布pending=0、active/reclaimable claim=0、未分类dead-letter=0；`FENCED -> RUNNING`仅允许同mode maintenance恢复、QUIESCING在Q前满足回退条件后的恢复，或第8.7节SAFE先切PROOF_REQUIRED并启动consumer后的最后一步。SAFE不得直接恢复RUNNING，QUIESCING越过Q/旧durable退役点不得恢复RUNNING或LEGACY。每次边转换都必须锁同一singleton、expected_version CAS并递增`authorization_relay_epoch`；非法purpose/mode组合、缺行、重复/回退epoch、计数非0、readiness失败及CAS/crash竞态均保持FENCED或原状态，零claim/publish/Outbox。

同一 `ADMIN_APPROVAL` singleton 的 `authorization_relay_state` 只约束 `account.approval-command.authorized.v1` 的 claim/publish，不能暂停或扩权其他事件。Relay每次claim/publish都强读同一行并绑定`authorization_relay_epoch`。通常新Claim必须重验mode属于LEGACY/QUIESCING/PROOF_REQUIRED；唯一例外是`SAFE_ROLLBACK + RUNNING`下受控`SAFE_DRAIN` Claim：只可选择SAFE CAS前已经同事务提交、`approval_control_epoch`绑定前一`approval_epoch`且Outbox DB状态尚未确定收敛为published/dead-letter/exclusion的原授权行，以原event ID作为相同Msg-Id、原持久payload和sidecar冻结的同语义delivery bytes执行自动at-least-once恢复；禁止选择SAFE epoch新行、修改payload/digest、创建第二Outbox或调用proof/APPROVE/Begin。PubAck或DB `published_at`/claim close结果不确定时允许对同一行、同一Msg-Id、同一语义bytes再次publish；即使已超过broker 24小时去重窗并产生重复流消息，也由第9.1节消费幂等链收敛，而不伪称流内最多一次。失败仍使用原行既有attempt/dead-letter状态；只有request已INVALIDATED且确认无Begin/execution/effect时才可按第9.2节封存exclusion。第9.2节已封存exclusion允许永久保留，不计入可发布pending或未分类dead-letter；因此本文所称授权链“排空/全空”一律且仅指：授权Outbox DB可发布pending=0、Relay active/reclaimable claim=0、未分类授权dead-letter=0、所有Q前consumer pending=0/ack-pending=0/handler=0，且每个原行已由DB确定为published、dead-letter或合法exclusion。该字段不是第二Outbox、消息队列或activation真相，不保存payload，也不能用于人工补发/重放。

数据库线性化冻结为所有参与方锁定同一 `ADMIN_APPROVAL` singleton，服务层预读不能替代：APPROVE最终短事务与subject-specific Relay Claim事务都必须先执行`SELECT ... FOR SHARE`，前者绑定`approval_epoch`，后者绑定`authorization_relay_epoch`。APPROVE重验mode/wire/proof_required/relay state/version/approval_epoch后，把request状态、proof binding、当前wire生成且冻结为不可变delivery bytes的authorization Outbox和`approval_control_epoch`同事务提交；proof-era bytes必须是第9.1节typed RFC8785 bytes，legacy bytes必须精确复用既有`events.Event.Marshal`格式，Relay不得解析后重序列化。普通Relay新Claim重验`authorization_relay_state=RUNNING`、`mode in (LEGACY,QUIESCING,PROOF_REQUIRED)`、version/authorization_relay_epoch；`SAFE_DRAIN` Claim同样FOR SHARE，但须额外重验mode=SAFE_ROLLBACK、Relay=RUNNING、候选Outbox的`approval_control_epoch`为SAFE切换前一approval epoch、DB尚无确定published/dead-letter/exclusion终态且delivery bytes/digest完整，并把`relay_control_epoch`与claim原子持久化。SAFE_DRAIN Claim先取共享锁时FENCE等待，claim提交后FENCE必须因active/reclaimable claim非0失败，直至同一claim由DB确定为published/dead-letter/exclusion或lease回收；FENCE先取排他锁时迟到SAFE_DRAIN看到FENCED并零claim失败。锁后claim提交前crash必须全回滚；claim提交后、publish前crash只按原lease恢复同一event/bytes；PubAck后、DB close前crash若不能证明`published_at`已提交，必须允许用相同Msg-Id/bytes恢复publish，不能凭进程内PubAck猜测终态。QUIESCING CAS与FENCE CAS都对这同一行`FOR UPDATE`，以expected_version重验合法边，并在各自事务的同一数据库快照复核授权subject的可发布pending、active/reclaimable claim、未分类/已封存dead-letter计数，并证明每个原行已由DB确定published/dead-letter/exclusion后才分别写mode+新approval_epoch或relay state+新authorization_relay_epoch。APPROVE先取共享锁时，其Outbox必在QUIESCING获锁前提交并被复核看见；QUIESCING先取排他锁时，迟到APPROVE只能看到QUIESCING并失败。普通Claim先取共享锁时，FENCE同样等待且复核不能通过；FENCE先取排他锁时，迟到Claim看到FENCED并零claim失败。FENCE事务提交后才允许在数据库外等待所有新Relay副本确认该authorization_relay_epoch且publish handler=0；外部确认不能倒置成CAS前提，也不能冒充数据库原子条件。APPROVE先锁/QUIESCING先锁、普通或SAFE_DRAIN Claim先锁/FENCE先锁，以及任一锁后、提交前/后的crash都必须由合同测试证明线性化结果唯一；已提交claim的进程crash只按原lease回收，未提交事务不得留下claim。

SAFE_ROLLBACK 的服务端动作矩阵固定为：create/update/submit/APPROVE/Account Begin 全部 503 `APPROVAL_SAFE_ROLLBACK`、`Retry-After: 5` 且零 proof/领域调用/Outbox/execution；REJECT/WITHDRAW 仍仅对 SUBMITTED 按当前 `allowed_actions` 和 CAS 安全收口；鉴权后的 Directory、历史 list/detail/events/result GET 保持只读，Directory 对受影响版本固定 `requestable=false`。Account 数据库 migration 必须同时以 singleton/check 约束冻结合法元组，并以锁定 singleton 行的受控 CAS 函数或等价 DB 约束冻结合法边、version及两个epoch单调递增；Account 所有审批写和 Begin 在同一事务内强读 control 并绑定 `approval_epoch`，Relay claim/publish绑定`authorization_relay_epoch`，服务层检查不能替代数据库约束。

proof_required 一旦为 true，任何不理解 control/proof 的 legacy N-1 Account 二进制都不得承接 approval create/update/submit/withdraw/decide、recovery 写或 Begin。回滚候选必须是已通过 control/proof 合同测试的兼容 N-1；若基础设施仍保留更老二进制，只能由路由层按方法/路径限制为历史 GET，并撤销其 Account DB 写凭据。proof-capable N 使用独立可轮换写角色/受控函数，legacy 共享角色只读；路由误配与旧凭据直连均须在 DB 层失败。运维写只接受 `expected_version` CAS，每次 mode、wire 或 proof_required 变化同时递增 version 与 `approval_epoch`，每次Relay状态变化同时递增version与`authorization_relay_epoch`；其他组合、非法边、缺行、未知 schema/mode 或回退任一epoch全部失败关闭。Account 提供只读、`additionalProperties:false`、`Cache-Control: no-store`的closed capability；required keys恰为`{schema_version,mode,approval_wire_version,proof_required,epoch,authorization_relay_state,authorization_relay_epoch,version,request_id}`，其中`epoch`精确等于`approval_epoch`，所有数值为正整数。Directory 只消费该真相，不可写。Portal每次加载detail先取得capability并记录version；v2比较body `action_control_version`，v1比较上述header，缺失/畸形/不相等一律清空`allowed_actions`并重取，不能使用旧动作。capability的mode/state/任一epoch/version变化必须abort在途detail/decision并清缓存；迟到RUNNING响应因version不匹配不得复活APPROVE，RUNNING→FENCED→RUNNING即使最终state相同也因version单调变化被识别。只有逐副本受信 readiness 已证明相同 build、proof schema、Begin/executor/recovery 与 v2 route 都就绪，运维才可 CAS 开启；探针失败或副本集合变化后不得自动开启。

唯一升级顺序只以第 14 节为准；特别是 execution safety 和 Portal compatibility 必须先于 QUIESCING，不能提前执行第8.6节的授权Outbox可发布pending=0、Relay active/reclaimable claim=0、未分类dead-letter=0与Q前consumer pending/ack-pending/handler=0收敛。Portal BFF 只读取服务端受信 capability，不采信浏览器自报，并在服务端 Session 第一次进入审批中心时原子写入不可变 `approval_wire_version:1|2`：已有 Redis Session 缺字段一律解释为 v1；新 Session 仅在 capability wire=2 时以 Redis CAS/SETNX 选择 v2。刷新、多 Portal 副本和同一 Session 多标签只能读到同一值，不能各自选择；Session 生命周期内不升级或降级。gate 回退后既有 v2 Session 固定 503 `APPROVAL_PROTOCOL_UNAVAILABLE` 并清在途写，绝不改走 v1；既有 v1 Session继续 v1。Portal 回滚只允许回到支持 control/proof 的兼容 N-1；Account 回滚前必须先关 v2 新流量、进入 SAFE_ROLLBACK 并证明无在途 v2 决定，legacy Account 只读边界按上段执行。

v1 仅能在全部受支持 Portal/桌面客户端的 N/N-1 都已转 v2、Account 回滚候选也支持 v2、v1 回滚窗口正式关闭、连续 30 天生产访问指标证明除具名合成探针外 v1 请求为 0，且 v2 列表/详情/决定在每个副本的合成探针全通过后才可停止。停止先拒绝新 v1 写并继续 v1 读的审计窗口，再在后续独立版本删除；本项实施不得提前删 v1。

## 6. 有界 cursor 与查询作用域

审批列表明确选择 NFR-56/Token `admin_pagination` 合同作为 Account 新实现基线；不回改 Billing NFR-55 或 Token NFR-56 的既有 envelope、错误码或运行参数。当前若没有可由 Account 直接复用的公共实现，后续实现必须先抽取窄公共抽象或按下列冻结合同接入，不能复制出参数和错误漂移的新变体。

请求只接受 `limit/cursor/command_type/status/risk_class/requester_user_id/target/submitted_from/submitted_to/expires_from/expires_to`；未知、重复、非法枚举/时间、`from>=to` 返回 400 `INVALID_FILTER`。`limit` 默认 50、最大 200，超界返回 400 `INVALID_LIMIT`。200 closed envelope 的 required keys 恰为 `{items,next_cursor,has_more,limit,as_of,snapshot_expires_at,request_id}`，`additionalProperties:false`；无下一页时 `next_cursor=null`。成员按 `updated_at DESC,id DESC` 冻结，snapshot `as_of` 内全量翻页在并发创建/更新时无重无漏，item 的当前状态可以变化但成员与顺序不变。

| 项目 | Account 审批分页冻结值 |
|---|---|
| snapshot/cursor TTL | 15 分钟 |
| tombstone | snapshot 过期/替换/回收后至少 20 分钟；清理每分钟幂等运行 |
| 单 snapshot | 最多 1,000,000 个成员 |
| 单主体 | 最多 2 个活动 snapshot；同资源最多 1 个，新建同资源事务替换旧 snapshot |
| 全局容量 | 最多 64 个活动 snapshot、20,000,000 个成员、8 GiB 成员存储 |
| 创建速率 | 单主体 6 次/分钟；全局 60 次/分钟；均使用数据库权威分钟窗口 |
| 常规读取速率 | 单主体/资源 60 次/分钟；来源 IP 120 次/分钟 |
| build/read deadline | 首屏构建唯一墙钟 10 秒；既有 snapshot 读取唯一墙钟 3 秒且禁止重建 |
| 水位 | snapshot/成员/存储任一达到 70% 告警，85% critical 并拒绝新的非替换 snapshot；清理积压超过 5 分钟告警 |

稳定错误固定为：篡改、主体/授权版本/资源/筛选/limit/位置/kid 不匹配返回 400 `INVALID_CURSOR`；过期、被替换/回收或 cursor 冻结 authorization version 已失效返回 410 `CURSOR_EXPIRED`；结果超过单 snapshot 容量返回 422 `PAGINATION_FILTER_REQUIRED`；创建速率耗尽返回 429 `PAGINATION_SNAPSHOT_LIMITED`，`Retry-After` 为距数据库下一分钟窗口向上取整且限制在 1～60 秒，非时间窗口型容量不足沿用 Token 基线 `Retry-After: 1`；常规读取限流返回 429 `RATE_LIMITED` 与距下一分钟窗口向上取整的 1～60 秒 `Retry-After`；build 超时、存储/签名配置不可用或 hard disable 返回 503 `PAGINATION_SNAPSHOT_UNAVAILABLE` 与 `Retry-After: 5`；read 超时返回 504 `QUERY_TIMEOUT`。任何失败不返回半页、截断页或静默首页。

cursor closed payload 固定 `v:"1"/kid/snapshot_id/subject_id/authorization_version/resource/scope_digest/filter_digest/sort/limit/position/position_key/as_of/expires_at`，其中 `sort` 只允许字符串 `updated_at_desc_id_desc`，`authorization_version` 必须为正整数，`scope_digest` 为对当前主体、资源、视图模式及规范化可见组织/节点/命令作用域 closed 投影做 JCS 后的 lowercase SHA-256 hex；筛选条件只进 `filter_digest`，不得与权限作用域混用。cursor 以至少 32 字节 HMAC key 签名。签发只使用 current key；验签只接受配置中 current 或 previous 的精确 `kid`，未知 kid 固定 `INVALID_CURSOR`。previous 只验签不签发，并自其最后一次签发 cursor 起至少保留 `15 分钟 TTL + 20 分钟 tombstone = 35 分钟`；完成该窗口且不存在引用后才可退役。正常轮换先配置 new current + old previous，再停止旧 key 签发；紧急密钥失陷允许 hard disable，此时统一 503，不以未验签 payload 猜测 410。

服务端必须在任何 snapshot 成员 SQL 之前完成 cursor 解码/验签、closed 字段与固定 sort 校验、正整数 authorization version 校验，并强读当前 authorization version 与重算 `scope_digest`。非正 authorization version、scope/sort/签名畸形返回 400 `INVALID_CURSOR` 且成员 SQL 计数为 0；已签发 cursor 与当前正整数 authorization version 或 scope 不同返回 410 `CURSOR_EXPIRED` 且成员 SQL 计数为 0。旧 cursor 每次读取都以当前授权重验第 2.2 节可见集合；撤权不因快照冻结而继续返回非本人申请。关闭“接受新 snapshot” gate 时，首屏固定 503/`Retry-After:5`，已签发 cursor 只读至原 TTL，不续期、不重建、不 live fallback；hard disable 或 current/previous key 配置不合法时全部失败关闭。v1/v2 兼容边界按第 5.4 节执行，不能把 v2 envelope/cursor 发给只能识别 v1 的客户端。

## 7. Provider 健康事实边界

Payment Provider 主动探测继续严格执行 PAY-57/PAY-58：只有 `internal.billing.provider-health.probe` 专用受信工作负载可以提交绑定 Provider、版本、分区、nonce、过期时间和 attempt ID 的一次性意图。管理员、审批中心、财务页面和浏览器均无触发、代理、重放或自定义探测请求的能力。

支付控制 UI 只读取 Billing 已持久化的可信观察、gap、案件、控制版本和 `resume_draft`。结果过期、缺失、不确定或不满足恢复条件时禁用申请并显示稳定原因；刷新只 GET 持久事实，不以重试按钮发起 Provider 请求。

## 8. 批准 proof 与版本协议

### 8.1 通用协议

批准才进行领域权威 proof；拒绝不依赖领域服务。对要求近期认证的命令，Account 必须在任何 precheck、proof 或领域调用前完成当前审批人鉴权、可见性、非自审、近期认证和请求版本检查；失败时零领域调用并返回逐命令稳定 step-up 错误。Proof 只允许 Account 以受信内部身份调用，浏览器不得提交、缓存或转发 proof 内容。

每个命令版本定义允许变化与禁止变化。Account 发往领域的 proof request 使用 `additionalProperties:false` closed body，required keys 恰为 `approval_request_id/command_type/command_digest/snapshot_digest/policy_version/proof_schema_version/proof_input`；其中 `proof_input` 使用第 8.2 节 closed oneOf。领域返回的权威 proof 也为 `additionalProperties:false`，required keys 恰为：`approval_request_id`、`command_type`、`command_digest`、`snapshot_digest`、`policy_version`、`proof_schema_version`、`domain_revision`、`verdict`、`reason_code`、`issued_at`、`expires_at`。`verdict` 只允许 `ALLOW/DENY`；`reason_code` 是逐 `command_type + policy_version + proof_schema_version` 冻结的有限 enum，ALLOW 与 DENY 都必须命中各自白名单。Account 不解释业务字段，只校验受信身份、closed schema/version、request/type、领域重算后的命令和 snapshot 摘要、policy version、返回的 domain revision、时间及稳定 verdict。

初始且尚未部署的 `proof_schema_version` 固定字符串 `"1"`；本次在任何 proof 实现或生产数据出现前即把该版本冻结为下述“领域 int64 → 规范十进制字符串”合同，因此不存在已发布的 numeric proof v1 可兼容或重算。request 为 lowercase hyphen UUID，两个 digest 为 64 位 lowercase SHA-256 hex，顶层 `policy_version` 是逐命令闭合的协议判别数字而不是任意 int64，domain revision 为 1～128 字符不透明稳定值，时间为 RFC3339 UTC。ALLOW 唯一 reason 为 `PROOF_VALID`；DENY 全局白名单固定为 `RESOURCE_NOT_FOUND`、`RESOURCE_STATE_CHANGED`、`RESOURCE_VERSION_CHANGED`、`SNAPSHOT_MISMATCH`、`POLICY_VERSION_CHANGED`、`ACTIVATION_CHANGED`、`DOMAIN_INVARIANT_FAILED`、`EXECUTION_WINDOW_EXPIRED`、`EVIDENCE_MISSING`、`EVIDENCE_EXPIRED`、`PROVIDER_STATE_CHANGED`，且每个命令版本必须从中冻结更窄子集。白名单外 reason、ALLOW 携带非 `PROOF_VALID` 或 DENY 携带 `PROOF_VALID` 均视为畸形 proof；Account 只校验枚举，不解释领域含义。

Proof 调用不得持有数据库事务锁。流程固定为：Account 短事务读取 request version/status、approver authorization checkpoint、policy/activation 和冻结输入 → 释放锁 → Account 在发起内部请求前创建不可重置的 2 秒绝对 deadline → 领域服务在同一 deadline 内完成鉴权、closed 解码、重算 digests、权威读取和响应，Account 在 deadline 内读完 body → Account 新短事务只以本域 request version/status、approver authorization checkpoint、policy/activation、proof request/response binding 和 TTL CAS 决定，同事务持久化脱敏 `domain_revision` 与 proof digest 作为后续执行关联证据。Account 不读写领域库、不维护 current domain revision 表，也不声称能对跨库 current domain revision 做 CAS。请求与响应 body **合计**最大 64 KiB，且任一方单体也不得超过 64 KiB；超限立即停止读取并失败关闭。`issued_at` 相对双方受信时钟最多允许 30 秒偏差，`expires_at` 必须精确等于 `issued_at + 60 秒`，APPROVE 提交事务时 proof 必须仍未过期。

批准后领域 executor 在任何领域副作用前必须重读当前领域事实，按同一 closed 规则重算 digests 并在领域本地事务中 CAS/验证 proof 绑定的 `domain_revision`。漂移时以稳定 `DOMAIN_REVISION_CHANGED` 零副作用失败，Account 按原 request/execution ID 确定性转 `INVALIDATED`；未知结果先查证，不得以新 proof 覆盖旧授权。

领域 proof endpoint 只接受 Account 专用受信内部 workload/mTLS 或等价服务身份；浏览器 Bearer、Portal workload、缺失或错误 audience 固定 403 且零领域查询。Portal BFF 必须剥离所有 `X-Pinvou-Approval-Proof-*` 及同义 proof-like 外部请求头；决定 body 按 closed schema 拒绝 `proof`、`proof_*`、`domain_revision`、`verdict`、`reason_code`、`issued_at`、`expires_at` 等未知字段，不能把浏览器值转为内部 proof。

Account 对管理端稳定映射固定为：权威 `DENY` 返回 409 `APPROVAL_PROOF_REJECTED`；proof 后 Account 本域 request/auth checkpoint/policy/activation/proof binding CAS 漂移返回 409 `APPROVAL_REQUEST_CHANGED`；closed schema、未知 reason、摘要/时间/请求绑定不符返回 502 `APPROVAL_PROOF_INVALID`；请求加响应合计超过 64 KiB、或任一单体超过 64 KiB，返回 502 `APPROVAL_PROOF_TOO_LARGE`；网络失败、非受信身份失败、领域 429/503 或无法取得权威结论返回 503 `APPROVAL_PROOF_UNAVAILABLE`；2 秒 deadline、context deadline 或领域 504 返回 504 `APPROVAL_PROOF_TIMEOUT`。所有错误保留 request ID 并写脱敏审计；未知、畸形或超限响应不得解释为允许或退回 `observed_versions`。

### 8.2 `proof_input` closed oneOf

本项明确选择“Account 从已持久 request/snapshot 构造脱敏 `proof_input`，领域现场重算事实”方案，不新增领域预存 proof record，因此不会形成创建审批前先写 proof 的循环依赖。`proof_input` 逐层 `additionalProperties:false`，共同 required keys 恰为 `{snapshot_schema_version,target,normalized_command,critical_resource_versions,financial_snapshot}`；`target` 恰为 `{resource_type,resource_id}`。Account 必须从已持久并冻结的 request/snapshot 生成，不接受浏览器补值。

`command_digest = lowercase_hex(SHA-256(JCS({command_type,policy_version,target,normalized_command})))`；`snapshot_digest = lowercase_hex(SHA-256(JCS({snapshot_schema_version,critical_resource_versions,financial_snapshot})))`。Account 与领域各自从 closed 输入重算并对比顶层 digest，领域再从自己的权威库重建同一结构并对比；三者任一不等即 `DENY`。JCS 固定为 RFC 8785 UTF-8 字节，不允许 duplicate/case-alias key、非有限数、未知 enum 或 Unicode 替代规则漂移。为完整保留数据库 `int64`，所有领域 int64 语义在 proof canonical 对象中一律先经 typed int64 解析，再编码为下表规定的 JSON **规范十进制字符串**；即使值为 0、1、2 或落在 binary64 安全范围也不得改用 JSON number。规范字符串只允许 `0` 或 `-?[1-9][0-9]*`，必须可无损解析为有符号 int64并继续满足逐字段正/非负/非0约束；拒绝前导零、`+`、`-0`、指数、小数、两端或内部空白及越界。禁止把任意文本“字符串化”后纳入摘要。proof request 连同 `proof_input` 与 response 的总字节数受第 8.1 节 64 KiB 总上限约束。

`proof_digest = lowercase_hex(SHA-256(JCS(closed_proof_response)))`。其唯一输入就是第8.1节领域返回的11个closed字段：`approval_request_id,command_type,command_digest,snapshot_digest,policy_version,proof_schema_version,domain_revision,verdict,reason_code,issued_at,expires_at`，不得加入HTTP header、响应顺序或任何展示字段；其中`policy_version`仍是逐命令常量JSON number，`proof_schema_version`是字符串`"1"`，时间是经closed校验的RFC3339 UTC字符串。`proof_digest`不是领域response的第12个传输字段：Account只在完整验证11字段后本地派生并持久绑定；领域与Account参考实现用同一golden各自计算一致，不存在对“传入proof_digest”的比较。缺失、额外、重复或格式错误的11字段响应仍失败关闭。`command_fingerprint`不属于上述proof response摘要，它在授权event中单独固定为64位lowercase SHA-256 hex并按既有申请事实校验；因此授权payload中的`command_fingerprint/command_digest/snapshot_digest/proof_digest`四项不得混称为“三个digest”。

初始 proof schema 1 的数值表示矩阵是闭合合同：仅 `command_digest` 输入及proof request/response的 `policy_version` 保留 JSON number，且必须等于对应命令已登记的常量 1 或 2；`proof_input.snapshot_schema_version` 保留 JSON number，且必须等于对应命令的常量 1 或 2；`proof_schema_version` 始终是字符串 `"1"`。除此之外，下列所有当前持久 int64 语义均编码为规范十进制字符串，任何未列出的 JSON number 均失败关闭：

| 变体 | 必须为规范十进制字符串的字段 |
|---|---|
| Wallet p1/s1 | normalized `amount_microcredits`；critical `adjustment_version`；financial `delta_microcredits` |
| Provider p2/s1 | normalized/critical `provider_control_version`；financial `provider_control_version`；`product_control_version` 仍必须为 JSON null；领域 `policy_version` 本来就是非空字符串，不得与 Account 顶层 p2 混用 |
| Product p1/s1 | normalized/critical `provider_control_version,product_control_version,catalog_version,admission_version,mapping_version`；financial `provider_control_version,product_control_version`；领域 `policy_version` 本来就是非空字符串 |
| Lifecycle p2/s2 | normalized `lifecycle_plan_version`（无计划的typed语义0固定编码为字符串 `"0"`）；critical `catalog_version,lifecycle_plan_version,billing_price_version,policy_version`（critical中的typed语义0同样只编码为`"0"`）；financial `billing_price_revision,models_checked,models_covered` |
| Catalog p2/s2 | normalized `source_catalog_version`；critical `current_catalog_version,source_catalog_version,billing_price_version,policy_version`；financial `billing_price_revision,models_checked,models_covered` |
| Credential p1/s1 | normalized `expected_account_version,credential_version`；critical `token_account_version,credential_version,replacement_credential_version,policy_version`，其中有替代凭据时typed integer编码为规范字符串、无替代凭据时仍为 JSON null |

| discriminant | `target` | `normalized_command` required keys | `critical_resource_versions` required keys | `financial_snapshot` |
|---|---|---|---|---|
| `WALLET_ADJUSTMENT` / policy 1 / snapshot 1 | `MANUAL_ADJUSTMENT` + lowercase UUID | `adjustment_id, wallet_id, amount_microcredits, requested_by`；ID 为 lowercase UUID，金额先解析为非 0 int64再编码为规范十进制字符串 | 恰为 `adjustment_version`，解析为正 int64的规范十进制字符串 | 恰为 `wallet_id,delta_microcredits`，delta为规范十进制字符串，并与 normalized wallet/amount 相等 |
| `PAYMENT_PROVIDER_RESUME` / policy 2 / snapshot 1 | `PAYMENT_PROVIDER_CONTROL` + 1～160 字符 provider ID | `provider_id, provider_version, provider_control_version, probe_order_id, policy_version, observation_high_watermark, evaluation_high_watermark, active_mapping_set_fingerprint`；两个 high watermark 可为 null；`active_mapping_set_fingerprint`精确保留既有 Billing p2 PostgreSQL `md5(string_agg(...))` 产生的32位lowercase hex，不重算或冒充SHA-256 | 与 normalized command 字段及值完全相同 | 恰为 `new_orders_enabled,provider_control_version,product_control_version`；值分别为 false、与normalized相等、null |
| `PAYMENT_PRODUCT_RESUME` / policy 1 / snapshot 1 | `PAYMENT_PRODUCT_CONTROL` + 1～160 字符 product ID | `provider_id, provider_version, product_id, provider_control_version, product_control_version, catalog_item_id, catalog_version, admission_version, mapping_id, mapping_version, probe_order_id, policy_version, observation_high_watermark, evaluation_high_watermark`；两个 high watermark 可为 null | 与 normalized command 字段及值完全相同 | 恰为 `new_orders_enabled,provider_control_version,product_control_version`；值分别为 false、与normalized两版本相等 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` / policy 2 / snapshot 2 | `MODEL` + lowercase UUID | `model_id, lifecycle_plan_id, lifecycle_plan_version, target_lifecycle, effective_at, existing_session_cutoff_at, replacement_model_id`；optional 值也必须显式 null，plan ID/version 成对；无 plan 时 ID=null 且typed version=0、wire恰为字符串`"0"` | `model_id, catalog_version, current_lifecycle, target_lifecycle, lifecycle_plan_version, effective_at, existing_session_cutoff_at, replacement_model_id, billing_price_version, route_set_fingerprint, provisioning_set_fingerprint, inflight_request_set_fingerprint, policy_version`；无 plan 时typed `lifecycle_plan_version=0`且wire恰为`"0"` | 恰为 `billing_price_revision, price_set_fingerprint, models_checked, models_covered, fully_covered, billing_as_of` |
| `TOKEN_MODEL_CATALOG_ROLLBACK` / policy 2 / snapshot 2 | `MODEL_CATALOG` + 与 source version 相同的十进制正整数字符串 | 恰为 `source_catalog_version` | `current_catalog_version, source_catalog_version, catalog_diff_fingerprint, billing_price_version, route_set_fingerprint, provisioning_set_fingerprint, inflight_request_set_fingerprint, policy_version` | 上述同一 6 字段财务投影 |
| `TOKEN_CREDENTIAL_REVOKE` / policy 1 / snapshot 1 | `PROVIDER_CREDENTIAL` + lowercase UUID | `token_account_id, expected_account_version, credential_id, credential_version` | `token_account_id, token_account_version, credential_id, credential_version, credential_status, credential_fingerprint, active_route_set_fingerprint, provisioning_lease_set_fingerprint, inflight_request_set_fingerprint, replacement_credential_version, policy_version`；无替代凭据时 `replacement_credential_version=null` | 恰为 `{}` |

字段类型同样闭合：Wallet/Token 的所有 `*_id` 为 lowercase UUID；Wallet `amount_microcredits/delta_microcredits` 解码为相等的非 0 int64但在proof中均为规范十进制字符串。Token/Billing领域 `*_version`、catalog数字、金额、revision与计数都按上述矩阵使用规范十进制字符串，且仅表内明示项的typed值可为0/null；旧业务存储、v1接口与 `events.Event.Marshal` 中的JSON number不因本合同改变。Provider/Product 的 `provider_id/product_id/provider_version/policy_version` 是 1～160 字符非空字符串，control/catalog/admission/mapping version 先按int64校验再规范编码，probe/catalog/mapping ID 与非空 high watermark 为 lowercase UUID；除 Provider p2 的 `active_mapping_set_fingerprint` 精确保留上述既有32位lowercase MD5事实外，其余表列fingerprint与所有新生成digest均为64位lowercase SHA-256 hex。该32位值仅作为不透明既有事实比较，不升级算法、不重写历史、不改变policy版本。`TOKEN_MODEL_LIFECYCLE_CHANGE` 的 `target_lifecycle` 仅允许 `DEPRECATED/REMOVED`，`current_lifecycle` 仅允许 `PUBLISHED/DEPRECATED`，并进一步只允许 `PUBLISHED -> DEPRECATED|REMOVED` 或 `DEPRECATED -> REMOVED`；target 为 `DRAFT/PUBLISHED`、current 为 `DRAFT/REMOVED` 及其他组合均在领域 SQL 前拒绝并进入 golden 反例。凭据状态仅 `PENDING/ACTIVE/RETIRED/REVOKED`；时间为 UTC RFC3339Nano且业务必填时间不得为零值。冻结proof合同允许`models_checked` typed值为0并以`"0"`进入golden，`models_covered`为非负int64且covered≤checked，`fully_covered`为boolean并与计数一致；当前`approvalpolicy/token_model_financial.go`的旧precheck仍要求`models_checked>=1`，GSA-05不得借表示修订改变该业务资格，差异由GSA-14在领域proof接线前按权威事实闭合。revision仍为正int64，`billing_as_of`为UTC RFC3339Nano。Provider/Product financial 的 `new_orders_enabled` 必须为 false。Provider 两类仅读持久化 probe order/观察/评估/映射事实，proof 不触发 Provider。所有变体禁止 password、secret、credential material、API key/token、endpoint/private URL、Provider 原文、个人敏感字段和原始证据；深层未知或敏感键在领域 SQL 前拒绝。每个变体必须有固定正/反样例、跨 Go/JSON 的 digest golden vector、最大/超限字节、缺失/重复/未知/敏感键、N/N-1 与生命周期回归。新字段只能追加 `proof_schema_version`；改变业务语义必须追加 policy version，不得放宽已发布 oneOf。

#### 8.2.1 既有持久字段到 `proof_input` 的精确映射

映射输入只来自 Account 已持久化的 `approval_requests.target_resource_type/target_resource_id/normalized_command/policy_version` 与 `approval_request_snapshots.snapshot_schema_version/critical_resource_versions/financial_snapshot`；`reason/redacted_evidence/expected_impact/command_fingerprint` 不进入 proof_input。所有 legacy JSON number 必须先由本节逐命令 typed closed decoder无损解析为int64并验证业务约束，再由公共encoder输出规范十进制字符串；这项确定性typed转换不是接受浏览器任意字符串或普通格式化。禁止 map 合并、字段猜测、binary64转换、任意数字字符串化或从展示 presenter 反推。领域 proof 使用映射后的输入重读权威行并逐字段核对，客户端持久值本身从不被当作权威事实。

| 命令与现有版本 | 当前持久事实 → proof 映射 | 派生/null/0/丢弃 | 领域校验来源 |
|---|---|---|---|
| `WALLET_ADJUSTMENT` p1/s1 | target 原样复制并要求 `MANUAL_ADJUSTMENT`；normalized 四字段原样复制；当前业务写路径的 critical 恰为 `{adjustment_version}`，financial 恰为 `{wallet_id,delta_microcredits}`，二者闭合复制 | `adjustment_id` 必须等于 target，financial 两值必须等于 normalized 的 wallet/amount；不把 registry 的通用 `target_id/resource_version/financial_snapshot/policy_version` 猜填进既有行；其他申请展示字段全部丢弃 | Billing `manual_adjustments` 不可变 ID、wallet、amount、requested_by、version 与状态；任一不符 DENY |
| `PAYMENT_PROVIDER_RESUME` p2/s1 | target 原样复制并要求 `PAYMENT_PROVIDER_CONTROL`；normalized 与 critical 都必须是同一八字段 `ProviderResumeVersions` 且逐字段相等；当前 Portal financial 恰为 `{new_orders_enabled:false,provider_control_version,product_control_version:null}` | 两个 watermark 只按持久 null/UUID 复制；`product_control_version` 必须显式 null；financial control version 必须等于 normalized；expected impact 丢弃 | Billing provider control、冻结 mapping fingerprint、持久 observation/evaluation watermark、probe order存在性/可信标记与 gap；proof 只读，禁止 Provider 调用 |
| `PAYMENT_PRODUCT_RESUME` p1/s1 | target 原样复制并要求 `PAYMENT_PRODUCT_CONTROL`；normalized 与 critical 都必须是同一十四字段 `ProductResumeVersions` 且逐字段相等；current Portal financial 恰为 `{new_orders_enabled:false,provider_control_version,product_control_version}` | watermark 只按持久 null/UUID 复制；两个 financial version 必须分别等于 normalized；其他展示/expected impact 丢弃 | Billing provider/product control、ON_SALE catalog/admission/frozen mapping、持久 watermark、probe order与 gap；proof 只读，禁止 Provider 调用 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` p2/s2 | target 要求 `MODEL` 且等于 normalized/critical model_id；normalized 映射为固定七字段；critical 十三字段、financial 六字段按typed语义闭合复制后把所有int64转为规范字符串 | 当前计划行复制 plan ID及typed version；历史即时行仅在 normalized 缺这两键且 critical typed `lifecycle_plan_version=0` 时确定性派生 `lifecycle_plan_id:null,lifecycle_plan_version:"0"`；既有显式 null 时间/替代模型保持 null | Token model/catalog/plan、路由/Provisioning/inflight fingerprints；Billing price closed projection；两域读取受同一 proof deadline，不宣称跨域事务 |
| `TOKEN_MODEL_CATALOG_ROLLBACK` p2/s2 | target 要求 `MODEL_CATALOG` 且十进制值等于 normalized `source_catalog_version`；normalized 一字段、critical 八字段、financial 六字段闭合复制 | 不从 target 猜出缺失 source version；仅在两处都存在且数值相等时接受；其他字段丢弃 | Token current/source catalog与diff、route/provisioning/inflight fingerprints；Billing price projection |
| `TOKEN_CREDENTIAL_REVOKE` p1/s1 | target 要求 `PROVIDER_CREDENTIAL` 且等于 normalized/critical credential_id；normalized 四字段、critical 十一字段按typed语义闭合复制后把所有int64转为规范字符串；financial 必须是 `{}` | `replacement_credential_version` 的typed integer编码为规范字符串，或原值为null时保持null；禁止从当前 active credential 补写缺键；所有秘密与隔离会话字段丢弃 | Token account/credential version/status/fingerprint、route/lease/inflight sets与替代凭据 version；零 Billing 调用 |

上述映射不修改六命令现有业务存储、v1接口或 policy/snapshot version；初始 proof transport 只使用独立 `proof_schema_version="1"`，其中领域int64全部按本节规范字符串编码。未来若只改变transport表示且业务语义不变，必须追加新的proof schema并保留current/immediately-previous reader；不得在同一proof schema内混用number/string。若资格、业务约束或字段语义变化则仍须追加policy version。如果迁移扫描发现某一历史行缺少表中必要持久字段、同字段互相矛盾或不能由所列权威来源重建，禁止静默填充：DRAFT 只能回业务页权威 Update，SUBMITTED 在切换后批准时重新取得 proof，切换前遗留的 APPROVED 按第 8.6、9 节执行或失效，绝不补 proof。只有产品确需继续执行且领域能以前向兼容迁移形成不可伪造新快照时，才为该命令追加 policy/snapshot version并保留旧 reader；不得原地改 p1/p2 或 s1/s2。

实现任务必须新增通用 typed closed RFC 8785 编解码模块（归属 `internal/platform` 的窄包），以逐变体 struct/union、拒绝 unknown/duplicate/trailing/depth/非有限数并输出 JCS bytes；普通 `encoding/json.Marshal` 只能传输，不能冒充 JCS，Token 专用 authority/canonicalizer（其Go字符串编码即便关闭HTML escaping仍会转义U+2028/U+2029）也不能冒充六命令公共 encoder，且本任务不修改该Token专用合同。合同测试固定两套 golden：legacy vector 从上述当前数据库三列读取、typed解析数字后映射为规范字符串；所谓new vector在GSA-12/17交付真实v2写入口前只能是同语义**合成目标向量**，不得冒称已有v2请求；两者的 proof_input JCS 与两个 digest 必须相等。另含生命周期 plan/即时`"0"`、watermark null/UUID、credential replacement null/规范整数字符串、Wallet 当前实际 snapshot、Provider/Product current financial 三字段，以及全部不可映射负例。Go与JavaScript必须读取同一不可变raw UTF-8/escaped-fragment golden文件，分别验证逐字节JCS和digest；JavaScript测试对int64字符串只能做词法校验或显式`BigInt`边界验证，禁止先`JSON.parse`成Number再重建摘要。边界至少覆盖int64最小值、最大值、0、1、-1及最小值以下/最大值以上、前导零、`+`、`-0`、指数、小数、空白拒绝，并覆盖U+2028/U+2029、非BMP字符、控制字符、组合Unicode原样保留（不做NFC/NFD归一化）和非法surrogate拒绝。

性能门禁固定为同一实现的两条benchmark：最大允许的closed有效向量执行typed decode+validate+JCS+双digest，以及恰64KiB恶意/畸形输入执行有界失败关闭。`go test -bench`使用固定`-benchtime=100x -count=5 -benchmem`，每轮完整记录`ns/op,B/op,allocs/op`与runner/Go版本；GSA-05首次实现以五轮最差值形成待B审核并随提交冻结的基线，后续同runner/Go版本任一指标回退超过20%必须阻断并说明，runner或Go版本变化必须重建并审核基线，不能拿不可比结果宣称无回归。单次输入不得按字段值创建无界递归/缓存；64KiB+1须在分配typed对象前拒绝。不得为了满足未经测量的alloc目标移除duplicate/Unicode/int64/closed校验；性能结果是GSA-05本地/CI证据，不代替第8.1节端到端2秒deadline或GSA-39生产等价性能证据。

### 8.3 policy 与 proof schema 版本

仅传输字段变化且语义不变时，可以追加独立 `proof_schema_version`，不修改 policy version。资格、关键字段、允许/禁止变化、近期认证、审批人数、执行窗口或恢复语义变化时必须追加 policy version，禁止原地修改。

每个 `command_type + policy_version` 的 registry 必须显式映射一个 proof schema；新申请只冻结当前映射，领域只为该映射签发，Account/consumer/executor 只接受 request 已冻结的同一版本。滚动升级期间 reader 最多同时接受 current 与 immediately-previous，且 previous 仅服务已冻结旧申请、不能签发新申请；N-1 任一组件不能 closed 解析 current 时不得推进该命令的 current activation。未知 future、早于 immediately-previous、请求冻结值与 proof/领域 endpoint 不同或同一次决定混合两个版本全部失败关闭，不做字段翻译或降级重试。唯一发布与激活顺序以第 14 节为准，本节不定义第二套顺序。旧申请继续使用冻结的旧 proof 协议，无法安全支持则确定性 INVALIDATED；回滚不能删除被历史申请引用的 policy/proof schema。

### 8.4 legacy observed_versions 迁移

proof-required gate 启用后，领域 closed proof 是 v1 与 v2 APPROVE 资格和事实变化的同一唯一权威；浏览器 `observed_versions` 永远不参与 proof、digest、Account 本域 CAS、verdict 或 `allowed_actions`。v2 decide 对该字段按 unknown 返回 400 `INVALID_APPROVAL_DECISION`。v1 wire 为了兼容旧 Portal 继续接受该字段，但缺失、空、伪造或修改都只按 legacy transport 解析后忽略，不写入权威 proof 且不能改变 APPROVE 结果；REJECT 在 v1/v2 都零 proof/领域调用。Account N-1 的现有 legacy 比对语义仅在 proof-required cluster gate 启用前保持，不得与已启 proof 的 Account N 混合接收批准写流量。

Portal N 在第 5.4 节会话被锁定 v1 时继续发送由旧 UI 已有数据形成的 `observed_versions`，并使用旧列表/详情；会话锁定 v2 时从第一次 decide 起就不发送，也不从 v2 detail 推导。Portal 不会在同一申请的写失败后尝试另一版本。v1 停发/停服时点完全按第 5.4 节的集群探针、流量和回滚证据；不再另建一套 observed_versions 退役门槛。

### 8.5 逐命令近期认证矩阵

| 命令 | 申请/提交 | 批准 | 说明 |
|---|---|---|---|
| Token 三命令 | 目标合同固定为 15 分钟 | 目标合同固定为 15 分钟 | recent auth 必须在 precheck/proof 前，失败零领域调用并返回稳定 step-up 错误；GSA-02基线确认当前Account审批链尚未落实年龄校验 |
| `PAYMENT_PROVIDER_RESUME` | 目标合同固定为 15 分钟 | 目标合同固定为 15 分钟 | proof 只读已持久化 PAY-57/PAY-58 事实，不触发探测；当前Account审批链缺口由后续任务补齐 |
| `PAYMENT_PRODUCT_RESUME` | 目标合同固定为 15 分钟 | 目标合同固定为 15 分钟 | Provider ACTIVE 后仍需商品独立申请和 proof；Billing商品管理的现有recent-auth中间件不等于本审批链已落实 |
| `WALLET_ADJUSTMENT` | 沿用当前合同，不新增 | 沿用当前合同，不新增 | 如新增 recent auth 或改变关键字段/变化语义，必须追加 policy version |

### 8.6 proof-required 切换前的历史处置

Billing 既有三命令与 Token 三命令始终保持各自既有 ACTIVE/Token activation 语义；切换不是把六命令整体置为 inactive。execution safety 与 Portal compatibility 全部就绪后，运维才以控制记录 CAS 进入 `QUIESCING`。该模式继续允许 create/update/submit、REJECT/WITHDRAW 和只读，但停止六命令新的 APPROVE；切换前已处于 APPROVED 的请求不得补 proof，也不得跨 control epoch 继承为 proof-ready。只有控制记录切到 `PROOF_REQUIRED` 以后发生的新 APPROVE 才现场取得并绑定新 proof。

`execution_deadline` 是旧授权能否 Begin/恢复的唯一时间资格：六命令既有“批准后5分钟”窗口与 Account expiry worker 均保持不变，QUIESCING、Relay drain、20分钟健康观测预算或人工操作都不得暂停、重置或延长它。deadline 到达时，现有 worker 立即按既有 `EXECUTION_WINDOW_EXPIRED` 把 APPROVED/EXECUTION_FAILED 失效；cutover reconciler 只修复 `execution_deadline <= Account DB now()` 却仍残留的行，沿用同一稳定 invalidation reason，并在锁内重验 status/version、无 Begin/execution/effect。deadline 未到的 APPROVED 只能等待旧 consumer 用原 event Begin或自然到期，不能因已等待20分钟之外的任何运维预算提前失效。

Q 前后的唯一可执行切换步骤冻结为：

1. CAS 进入 QUIESCING 并停止新 APPROVE；Account 决定事务必须先读到该状态，因而此后不再产生新的授权 Outbox。
2. 保持 Account 授权 Relay 与现有 consumer 运行；等待当前 Relay in-flight claim 完成，对过期 claim 由原 Relay 同行锁/lease 规则回收，并让所有既有授权 Outbox 以原event ID作为相同Msg-Id、使用sidecar同语义delivery bytes自动at-least-once恢复，直至每行在DB确定为published、dead-letter或合法exclusion。随后会因status/deadline被Begin拒绝的历史请求也必须先完成这种确定收敛。PubAck/DB close不确定时允许相同原行再次publish；24小时broker去重窗内外出现重复/乱序流消息都不是人工补发或replay。任何授权Outbox只进入Relay DLQ，或无法以原行/原event ID/同语义bytes安全恢复，默认硬阻断切换；唯一例外是该request已INVALIDATED、Account同库和领域权威查询确认无Begin/execution/effect，且已按第9.2节不可变封存为永不可发布的cutover exclusion。未分类、effect不明或仍非终态的授权DLQ始终阻断。
3. 连续核对授权 Outbox `pending=0`、Relay active/reclaimable claim=0，所有当前授权 durable 对截至栅栏前已发布消息（读取 Q 后即 Q 前消息）的 pending/ack-pending=0、进程内 handler=0；健康 broker 下最多观察20分钟，超时只判 readiness 失败并退出/恢复运行态，不改变任何 request deadline/status。
4. 以第5.4节锁协议对同一 `ADMIN_APPROVAL` singleton `FOR UPDATE`，在同一事务复核授权 subject 可发布 pending=0、active/reclaimable claim=0、未分类 dead-letter=0 后，用 expected_version CAS 将该行 `authorization_relay_state` 置为 FENCED并递增`authorization_relay_epoch`；事务提交后才在数据库外等待全部新 Relay 副本确认该Relay epoch、授权 publish handler=0，再次核对计数。栅栏后不得再向授权 subject 发布。
5. 从受信 JetStream server读取并持久化当前 stream high-watermark `Q`，以 `DeliverByStartSequence(Q+1)` 创建三个新 Billing durable并完成配置/ACL/readiness校验；此时保持新consumer未启动，三个durable的pending/ack-pending必须为0。
6. 停止旧共享Billing consumer；确认旧durable pending=0、ack-pending=0、进程内handler=0、Account旧epoch Begin in-flight=0后，删除旧durable，或永久pause并撤销其consume ACL。随后以旧workload身份对旧durable执行只读consumer-info及bind/fetch负向探针，必须分别得到not-found/paused或authorization-denied且零消息、零handler；探针不得向授权subject发布消息，也不得污染Q+1新durable。不得以重新启用旧durable作为回滚。
7. 将六命令全部legacy APPROVED、EXECUTING、EXECUTION_FAILED按下表与第9节收敛。gate前同一一致性快照必须证明这三种状态各自为0、旧epoch非终态execution为0、未知effect为0；APPROVED=0只是必要条件之一，不能单独放行。
8. 完成逐副本与资源readiness后 CAS 为 PROOF_REQUIRED/wire2并递增`approval_epoch`；CAS成功才按该approval epoch启动三个新Billing consumer，确认其Q+1配置和handler ready，最后把同一控制行的授权Relay状态CAS回RUNNING并递增`authorization_relay_epoch`。恢复Relay后只有gate后新批准产生的新授权事件能进入新durable，Relay epoch变化不得使其approval epoch失效。CAS前失败时保持QUIESCING/FENCED并修复，不能在Q已持久化或旧durable停用后退回LEGACY；gate CAS后但consumer启动失败或Relay恢复CAS未提交时进入零新批准的SAFE_ROLLBACK+FENCED，Relay恢复CAS已经提交后才开放批准，其后故障进入可能含已有/并发批准的SAFE_ROLLBACK+RUNNING。两类失败都不得降级旧durable。

后台扫描按 request ID 分片、幂等、可中断，所有写都带 request version/control epoch CAS：

| 切换时状态 | 必须处置 |
|---|---|
| DRAFT | 保持可读。映射可验证者切换后仍可从业务页 Update/submit；不可映射者禁止中心快捷 submit，只能回业务页权威重读并 Update 同一 DRAFT。不得自动改快照或撤回 |
| SUBMITTED | 保持 SUBMITTED；切换后 v1/v2 APPROVE 都现场取得新 proof。不可映射或权威 DENY、策略/事实已确定漂移者只能在批准尝试时确定性拒绝或 INVALIDATED，业务页 fresh superseding；领域暂时不可用/超时保持 SUBMITTED，不得因暂态故障失效或批准；仍可 REJECT/WITHDRAW，且二者零 proof |
| APPROVED、尚无 Begin/execution/effect | 保留原 Outbox。已升级 consumer 仍可在 QUIESCING 且自身5分钟execution_deadline未到时，用原event和旧epoch调Account Begin；Begin只校验历史request、command/policy、既有activation、旧epoch、状态和原事件摘要，不要求或补写proof，并以request-status CAS唯一取得执行权。deadline已到的残留仅由expiry worker或cutover reconciler以`EXECUTION_WINDOW_EXPIRED`锁内失效；任何effect是否存在不能得到权威结论时保持阻断。不得创建第二Outbox或人工requeue/replay；同一原行在DB未确定published/dead-letter/exclusion前仍按第9.2节自动at-least-once恢复 |
| EXECUTING 或已有 Begin/execution/effect | 不回退、不补造第二授权，只按原execution/fencing查询并沿既有状态机收敛到EXECUTED或EXECUTION_FAILED；后者再按下一行收敛到EXECUTED或INVALIDATED。未知结果持续查证，不能为赶切换而重放或从EXECUTING直接失效。gate前EXECUTING必须为0，effect仍不明即阻断 |
| EXECUTION_FAILED | deadline内的TRANSIENT_SAFE只能沿原execution/idempotency/fencing和旧epoch按既有恢复合同收敛；UNKNOWN只查证；PERMANENT/漂移或deadline结束按既有稳定reason INVALIDATED。gate前EXECUTION_FAILED必须为0，不补proof、不跨epoch、不得legacy豁免 |

切换查询必须以 Account DB 同一一致性快照断言六命令 `APPROVED=0`、`EXECUTING=0`、`EXECUTION_FAILED=0`、旧epoch非终态execution=0、未知effect=0；还必须证明授权Outbox可发布pending=0、Relay active/reclaimable claim=0、未分类授权dead-letter=0、Relay已FENCED、所有Q前durable pending/ack-pending/handler=0、旧Billing durable已删除或永久pause且ACL撤销、新Billing durable从Q+1创建但未启动。已验证exclusion允许永久保留，不得因其存在把这些零值误判失败，也不得把它计入可发布pending/未分类dead-letter。不允许proof binding、投递状态或任何其他旧epoch中间态继承。逐副本readiness再确认consumer全部“先Begin后副作用”、新Begin能识别proof_required后，运维才按上述第8步原子CAS并有序恢复。自该线性化点起，v1与v2的新APPROVE都走同一服务端proof；只有v1 wire接受并忽略`observed_versions`，v2 closed body拒绝该字段。不存在按创建时间、客户端版本或command type绕过proof的legacy exemption。CAS冲突、进程crash或gate epoch变化只重扫同一request，不产生第二proof binding、Outbox、execution或领域效果；混合Account/Portal/Relay/consumer N/N-1、QUIESCING崩溃恢复、APPROVE与gate CAS、历史Begin与INVALIDATED竞态必须证明线性化结果唯一。

### 8.7 SAFE_ROLLBACK 物理状态与恢复

PROOF_REQUIRED gate提交时Relay仍为FENCED，因此新consumer启动/handler readiness失败，或Relay `FENCED -> RUNNING` CAS本身失败并保持FENCED，均发生在任何新批准开放之前：FENCED矩阵已移除APPROVE，故该分支必须证明新approval epoch没有APPROVED/新授权Outbox，再以expected_version CAS进入`SAFE_ROLLBACK + FENCED`。Relay恢复CAS一旦成功，物理状态就是RUNNING且可能立即产生新批准；其后的readiness/健康故障必须按`SAFE_ROLLBACK + RUNNING`处理该approval epoch已有Outbox，不能冒充FENCED零批准分支。两类分支的SAFE CAS都递增`approval_epoch`；从CAS起create/update/submit/APPROVE/Begin立即按第5.4节关闭。SAFE具有两种必须显式观测的物理状态，不能只记录一个逻辑布尔值：

| SAFE 物理状态 | 进入后的固定动作 |
|---|---|
| `SAFE_ROLLBACK + Relay FENCED` | Relay 保持 FENCED，禁止 claim/publish；停止新 consumer handler，只保留只读 readiness、历史 execution 查询和受控收敛 worker。不得为了“唤醒”已批准请求解除栅栏或 replay Outbox |
| `SAFE_ROLLBACK + Relay RUNNING` | 保持SAFE写/Begin封锁，仅允许`SAFE_DRAIN`对SAFE CAS前已提交、绑定前一approval epoch且DB未确定published/dead-letter/exclusion的原授权Outbox创建受控Claim，以原event ID/Msg-Id和sidecar同语义delivery bytes自动at-least-once恢复；既有claim同样完成或按lease回收，PubAck-close不确定允许相同消息再次publish。失败进入原dead-letter，request失效且无Begin/execution/effect后才可exclusion；其余未知DLQ阻断。每行DB终态确定、可发布pending、active/reclaimable claim、未分类dead-letter均为0后，按第5.4节同一行锁协议CAS Relay为FENCED，再在事务外确认全部新Relay副本handler=0；不得在Relay仍RUNNING时尝试SAFE→PROOF_REQUIRED |

进入 SAFE 的前一 proof/approval epoch 中，`APPROVED` 且无 Begin/execution/effect 的请求必须以 request version/approval_epoch CAS 为 INVALIDATED；`EXECUTION_FAILED` 仅在无活动 Begin/lease、原 execution 已确定无领域 effect 且不再具备安全恢复资格时同样失效。若其既有 `execution_deadline` 已到，现有expiry worker/cutover reconciler仍优先使用`EXECUTION_WINDOW_EXPIRED`；仅deadline内由SAFE提前收敛者使用稳定码`APPROVAL_SAFE_ROLLBACK_INVALIDATED`，SAFE不得重置或延长deadline。已有 Begin、EXECUTING、已证明 effect 或结果未知者只能按原 execution ID/fencing 查证到终态，不能因 SAFE 失效、补 proof、创建第二 Outbox 或重放授权事件。迟到的旧 approval epoch event 必须在任何副作用前被 Begin 拒绝；对应消息只按第9.1节服务器确认 ACK/DLQ 状态收口。

SAFE→PROOF_REQUIRED 的唯一恢复顺序为：先保持所有 approval 写/Begin 关闭；按上表把 Relay 规范到 FENCED 并完成数据库计数和外部 handler 双重验证；把前一 approval epoch 的 APPROVED/EXECUTING/EXECUTION_FAILED、旧 approval epoch 非终态 execution 与 unknown effect 全部清零；验证三个新 Billing consumer 的代码、ACL、Q+1 durable 与 handler 可启动但仍不消费；再以 expected_version CAS 把 control 切回 PROOF_REQUIRED并递增`approval_epoch`；CAS 成功后按新approval epoch启动 consumer，最后才把同一行Relay状态CAS回RUNNING并递增`authorization_relay_epoch`。任一步失败都停留或重新进入 SAFE，Relay 按其真实物理状态重新归一，禁止降级旧durable、复用旧proof epoch或人工/管理面requeue/replay；同一原Outbox行在DB终态不确定时仍允许自动at-least-once恢复。必须分别覆盖：FENCED下consumer启动失败或Relay恢复CAS未提交且新epoch零APPROVED/Outbox；Relay恢复CAS已提交为RUNNING后已有/并发批准时consumer故障、Relay readiness/健康故障；SAFE_DRAIN Claim先锁与FENCE先锁；Claim锁后提交前crash、提交后publish前crash、PubAck后close前crash；旧 approval epoch Begin 与 INVALIDATED CAS、恢复CAS与迟到handler/claim竞态。SAFE生效后均零新Outbox/Begin，SAFE_DRAIN允许同一原event在PubAck-close不确定或24小时去重窗外形成重复流消息，但Inbox/Begin/execution/领域幂等必须保证最多一个execution/effect。

## 9. 历史 APPROVED 收敛与三层 gate

对于切换前进入 `APPROVED` 的历史请求，禁止新建、补发或重放 Account 授权 Outbox。批准事务已写入的 Account Outbox 行只表示“授权事件待投递/曾投递”，本身不算领域执行证据，必须保留且不得删除或改写。本项不新增第二个 Account claim 真相；Account Begin 与 reconciler 对同库同一 `approval_requests` 行的 request-status CAS 是执行授权所有权的唯一线性化点。reconciler 可在单个短事务内以 `FOR UPDATE SKIP LOCKED` 或等价行锁选取并 CAS，但不持久化独立 claim/lease。

- 每个 consumer 无论是新旧事件，都必须在任何领域副作用前调用 Account Begin。QUIESCING 中历史 APPROVED 只允许携原 command/policy/activation、旧 epoch 与原事件摘要执行一次旧协议 Begin，不要求也不生成 proof；PROOF_REQUIRED 后的 Begin 必须绑定批准时服务端 proof。两种路径都在同一 Account 事务中将 `APPROVED -> EXECUTING`、创建/幂等取得 execution ID 和 fencing token。该 CAS 先成功则 reconciler 必须转查证；reconciler 的 `APPROVED -> INVALIDATED` CAS 先成功则 Begin 必须失败且 consumer 零领域副作用。
- Outbox delivery/published 位、仅写入 consumer Inbox、consumer 本地 claim（包括尚有 TTL 或已 stale）、以及 Begin 前的只读 precheck 都不是 Account 执行授权证据，不阻止 reconciler 失效。只读 precheck 与失效并发时，precheck 可完成但后续 Begin 必须按最新 status 失败；precheck 不得写领域事实或预留执行权。
- cutover reconciler只处理`execution_deadline <= Account DB now()`却未被既有expiry worker收敛的残留；它在锁内重验无Account Begin/execution、无经领域幂等引用证明的effect与无并发Begin后，按request version+`APPROVED/EXECUTION_FAILED` status+activation/epoch CAS `INVALIDATED`，稳定reason仍为`EXECUTION_WINDOW_EXPIRED`。20分钟只限健康broker的drain/readiness观测，绝不延长、暂停或取代5分钟execution_deadline。Outbox/Inbox与本地claim原样保留用于审计/去重。已有Begin/execution或已证明领域effect时，只能按原execution ID/fencing查证；effect存在性无法权威确认时必须报告硬阻断并保持QUIESCING，不能创建第二事件、第二效果或冒险失效。
- 所有切换前授权Outbox无论请求稍后是否失效，都必须在Relay栅栏和Q之前以原event ID完成首次原subject发布；未发布行、仅进入Relay DLQ或仍有claim者阻断切换。首次迟到发布不是补发/重放，但PubAck后禁止另建事件再次发布。已发布未消费、仅Inbox/本地claim的旧事件在deadline到期且无Begin/execution/effect时可失效；迟到consumer仍须调用Begin并被status/activation/fencing拒绝，然后服务器确认ACK，不能进入领域副作用。`EXECUTING`只查结果，不因超时自动重放，且必须在gate前终态化。

### 9.1 durable 映射、尝试上限与 ACK/NAK/DLQ

不再为历史 APPROVED 建立投递可达租约、可重投中间态或跨 epoch 等待协议；Outbox published、Relay、Inbox与JetStream delivery metadata都不是领域执行证据。只有第8.6节冻结的可发布pending=0、active/reclaimable claim=0、未分类dead-letter=0、Q前consumer pending/ack-pending/handler=0、FENCE、Q及资源配置作为控制面readiness参与切换；已封存exclusion可永久保留，不能据此推断Begin/effect。Account DB观测时间仅用于审计、deadline判定和readiness预算，不构成消息可达租约。新proof/v2只服务切换后发生的新批准。服务端registry必须把每个`command_type + policy_version + executor_identity`映射到唯一durable，consumer不接受浏览器或event payload自选durable/executor；NATS consumer ACL和workload权限绑定到该registry：

| 命令 / policy | executor identity | 唯一 durable | 物理投递 |
|---|---|---|---|
| `WALLET_ADJUSTMENT` / p1 | `billing-command-executor` | `billing-wallet-adjustment-executor-p1-v1` | `MaxDeliver=-1` |
| `PAYMENT_PROVIDER_RESUME` / p2 | `billing-command-executor` | `billing-payment-provider-resume-executor-p2-v1` | `MaxDeliver=-1` |
| `PAYMENT_PRODUCT_RESUME` / p1 | `billing-command-executor` | `billing-payment-product-resume-executor-p1-v1` | `MaxDeliver=-1` |
| `ENTITLEMENT_COMPONENT_ADJUSTMENT` / p2 | `billing-command-executor` | `billing-entitlement-component-adjustment-executor-p2-v1` | `MaxDeliver=-1` |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` / p2 | `token-command-executor` | 既有 `token-model-lifecycle-executor-v1` | 保持 `MaxDeliver=-1` |
| `TOKEN_MODEL_CATALOG_ROLLBACK` / p2 | `token-command-executor` | 既有 `token-catalog-rollback-executor-v1` | 保持 `MaxDeliver=-1` |
| `TOKEN_CREDENTIAL_REVOKE` / p1 | `token-command-executor` | 既有 `token-credential-revoke-executor-v1` | 保持 `MaxDeliver=-1` |

同一个三元组只能由一个 durable 处理；policy version 或 executor identity 变化必须注册新 durable，不能原地改旧映射。Billing 既有共享 `billing-command-executor-v1` 先原地迁移为 `MaxDeliver=-1`，只负责栅栏前已首次发布的历史并按上述精确零值条件收敛；只有第8.6节证明授权Outbox可发布pending=0、Relay active/reclaimable claim=0、未分类授权dead-letter=0、所有Q前consumer pending/ack-pending/handler=0并把授权Relay置为FENCED后，受控资源操作才能读取、持久化stream high-watermark `Q`，再以 `DeliverByStartSequence(Q+1)` 创建三个新Billing durable。已封存exclusion永久保留，不影响该判定。新durable在PROOF_REQUIRED前不得消费，禁止使用DeliverAll或从stream起点重放历史授权事件。旧Billing consumer停止且pending/ack-pending/handler/Begin均为0后，旧durable必须删除或永久pause并撤ACL；不得留作回滚。Token保持既有三个逐命令无限投递durable，但其Q前pending/ack-pending/handler也必须在gate前为0。资源manifest、readiness与NATS ACL必须逐durable固定stream/subject、filter、deliver policy、AckWait=30秒、最大显式NakWithDelay=512秒、MaxAckPending、`MaxDeliver=-1`、owner workload和唯一映射；manifest将Billing新durable的start sequence来源冻结为`quiescing_high_watermark_plus_one`，受控操作receipt记录Q与实际Q+1，readiness逐项比对而不接受手填任意sequence。Billing workload只能消费三个Billing新durable，QUIESCING收敛期旧ACL在旧durable停用后必须撤销；Token workload只能消费三个Token durable，并各自只持现有Begin/complete与领域最小权限。非本durable目标即使command合法也不得调用领域，只在完整验证event envelope后做服务器确认ACK。

#### 9.1.1 授权Outbox epoch与delivery-bytes sidecar前向迁移

Account Outbox以前向追加方式新增nullable `approval_control_epoch BIGINT`；另建一对一不可变sidecar `approval_authorization_event_bytes`，主键/外键为原`event_id`。sidecar是按`provenance`区分的closed oneOf，base required固定为`schema_version,event_id,serialization_version,provenance,created_at`：`FROZEN_AT_APPROVAL/RECONSTRUCTED_FOR_DELIVERY`变体另required `delivery_bytes,event_digest`且禁止`reconstructed_semantic_digest`；`RECONSTRUCTED_AFTER_PUBLISH`变体只另required `reconstructed_semantic_digest`，禁止`delivery_bytes/event_digest`，不能成为Relay输入。稳态约束为：仅`event_type='account.approval-command.authorized.v1'`必须具有正整数`approval_control_epoch`和恰一条sidecar，其他event的epoch必须为null且不得创建该sidecar。原Outbox `event_id/payload`及所有已有发布/dead-letter字段永不改写；sidecar不创建新事件、不改变published状态。

既有授权行统一确定性backfill `approval_control_epoch=1`。`serialization_version='LEGACY_TYPED_V1'`不得使用JCS或新公共encoder，而必须精确复用当前 `internal/platform/events.Event.Marshal`：从原Outbox列构造并Validate既有`events.Event`，再由现有固定struct顺序`event_id,event_type,aggregate_id,aggregate_version,occurred_at,trace_id,payload`和当前生产Go工具链的`encoding/json`行为生成bytes；其中UUID、`OccurredAt.UTC()`、`time.Time`、`json.RawMessage`、JSON number及HTML escaping等行为全部以该函数和跨版本golden为准，不能另写“等价”序列化器。既有 `events.Unmarshal`、业务存储、v1接口、历史摘要与非授权事件不改；不得拿新proof的十进制字符串规则重算或覆盖任何legacy bytes/digest。若Go依赖或工具链使golden变化，必须追加serialization version和迁移，不能原地漂移。对`published_at IS NULL`且仍待正常/SAFE_DRAIN恢复的行，sidecar保存从持久行确定性重构并成为唯一后续publish输入的`delivery_bytes`及其SHA-256 `event_digest`，`provenance=RECONSTRUCTED_FOR_DELIVERY`；对迁移前已published历史行，只保存`provenance=RECONSTRUCTED_AFTER_PUBLISH`与`reconstructed_semantic_digest=lowercase_hex(SHA-256(events.Event.Marshal重构结果))`，不保存或声称delivery bytes/event digest，明确不能证明当时broker byte identity、PubAck、Inbox、execution或effect，也不得用于重新投递。新writer在control仍为LEGACY时使用`serialization_version='LEGACY_TYPED_V1'`、`provenance=FROZEN_AT_APPROVAL`并在批准事务内冻结当前`events.Event.Marshal` bytes/digest；proof-era新批准才使用`serialization_version='APPROVAL_RFC8785_DECIMAL_V1'`、`provenance=FROZEN_AT_APPROVAL`和第8.2节typed closed `delivery_bytes/event_digest`。Relay只逐字节发布允许投递provenance的sidecar bytes，不得从payload重序列化。

`APPROVAL_RFC8785_DECIMAL_V1` 是仅适用于授权event的全新closed wire，不替代通用`events.Event`。envelope required keys恰为 `serialization_version,event_id,event_type,aggregate_id,aggregate_version,occurred_at,trace_id,payload`，`additionalProperties:false`；内嵌判别字段 `serialization_version` 必须恰为该常量，`event_type`仍恰为`account.approval-command.authorized.v1`，`aggregate_version`是解析为正int64的规范十进制字符串。payload required keys恰为 `authorization_schema_version,approval_request_id,command_type,command_fingerprint,policy_version,executor_identity,execution_deadline,normalized_command,target_resource_type,target_resource_id,critical_resource_versions,financial_snapshot,proof_schema_version,command_digest,snapshot_digest,domain_revision,proof_digest,approval_control_epoch`，`additionalProperties:false`；`authorization_schema_version="1"`、`proof_schema_version="1"`，`policy_version`仅保留逐命令常量JSON number，`approval_control_epoch`是正int64的规范十进制字符串；`command_fingerprint/command_digest/snapshot_digest/proof_digest`分别为64位lowercase hex，其他嵌套int64完全按第8.2节矩阵编码。该closed payload由后续GSA-13批准proof合同提供绑定事实，GSA-05只交付typed编码能力，不冒称批准接线已完成。

该新wire的基础类型同样闭合：`event_id/aggregate_id/approval_request_id`均为lowercase hyphen UUID，且`aggregate_id=approval_request_id`并按既有授权事件语义与`event_id`所绑定的同一批准事务核对（不要求三个UUID字面相同）；`occurred_at/execution_deadline`均为UTC RFC3339Nano规范字符串，前者来自批准事务DB时间、后者等于已冻结request执行截止。`trace_id`仅沿用当前`events.Event.Validate`的真实约束：原字符串非空且`strings.TrimSpace(trace_id)`后仍非空；本版本不虚构或新增长度/字符集限制。安全合同仍要求`aggregate_version`的typed值恰等于同一批准事务CAS后的`APPROVED request.version`及Outbox行`aggregate_version`，payload typed `approval_control_epoch`恰等于APPROVE短事务锁定的`ADMIN_APPROVAL.approval_epoch`、Outbox `approval_control_epoch`及冻结sidecar bytes。GSA-05无数据库所有权，只提供纯typed `ExpectedAuthorizationBinding{ApprovedRequestVersion,ApprovalEpoch}`（或等价命名）校验接口，golden以外部expected binding注入前一/未来request version和错epoch并证明拒绝，不把wire内单值与自身比较冒充事实重验；GSA-06才在APPROVE同事务从锁定request/control取得expected值、调用该接口并原子写Outbox列/sidecar，使用真实PG证明四者一致，GSA-08/Begin再按后续合同重验。`command_type/policy_version/executor_identity/target_resource_type/target_resource_id`必须命中六个既有可执行命令的closed映射，target ID与对应`proof_input.target`一致。任一基础类型、expected binding或命令映射不符均在摘要/业务handler前失败关闭。

现有`ConsumeUntilResolved`在进入handler前固定调用仅支持legacy number型`events.Unmarshal`，因此GSA-05必须同时交付只面向授权event的raw-bytes dual decoder窄接口；它先从原始对象读取并严格校验显式`serialization_version`，缺字段只走`LEGACY_TYPED_V1`的既有七字段decoder，有新判别则只走匹配的新closed decoder，未知/重复/畸形判别直接失败，禁止parse失败后fallback或按number/string猜测。GSA-08须新增或选用仅绑定授权subject的raw-message consume seam，在通用`events.Unmarshal`之前把原始bytes交给该dual decoder；不得改变非授权subject和通用事件路径。GSA-08接线完成并排空旧consumer前，GSA-06/07不得让新wire进入broker。

GSA-06负责新writer/sidecar原子生成并使sidecar `serialization_version` 与envelope内嵌判别字段逐字一致；GSA-07的Relay只发布sidecar bytes；GSA-08的新consumer交付显式双decoder：envelope存在上述判别字段时只能走新closed decoder，字段缺失时只能按sidecar/切换合同走`LEGACY_TYPED_V1`并复用现有`events.Unmarshal`，未知值或二者不符失败关闭。不得依据某个数值大小、number|string形状或解析失败后fallback猜版本。旧consumer只能读取legacy numeric wire；新consumer与sidecar reader、Relay全部就绪且旧consumer排空/停用前，control不得进入会产生新wire的状态，新wire不得投递给旧durable。非授权event始终沿现有`events.Event.Marshal/Unmarshal`，不增加该判别字段、不改变字节或摘要。

迁移分两阶段保持N/N-1。第一阶段以前向nullable列、sidecar自身closed oneOf/FK/唯一/不可变约束及授权行身份不可变守卫交付；**不**启用无条件“授权行必须已有sidecar”的约束，也不得以`NOT VALID`冒充兼容手段，因为PostgreSQL的`NOT VALID`约束仍会检查创建后的新行。授权event的DB `BEFORE INSERT`兼容trigger必须在旧writer所属的同一`SERIALIZABLE`事务中先`SELECT ADMIN_APPROVAL ... FOR SHARE`，并仅在control逐字段精确为`schema_version=1/mode=LEGACY/approval_wire_version=1/proof_required=false/authorization_relay_state=RUNNING/approval_epoch=1`且输入epoch缺失时填`1`；锁后读取的`version`与`authorization_relay_epoch`只要求为当前正整数并用于该事务观测，不要求等于初值1。其他schema/mode/wire/proof/state/approval epoch、非授权event或显式错误值均不得代填。trigger自身不得提交、开新事务或做语句级重试。旧writer不能生成sidecar，因此受信backfill worker使用同一`events.Event.Marshal`规则按event id幂等补齐；新fence-aware Relay对缺sidecar/digest或不允许投递provenance的未发布行硬阻断且不claim，旧Relay在LEGACY兼容窗按原行为运行。新writer必须显式写当前`approval_control_epoch`并在同一审批事务通过延迟到事务末的跨表核验或等价原子受控函数写sidecar，避免Outbox先插、sidecar后插被即时约束错误阻断；DB核对其等于已FOR SHARE锁定control epoch和event id，不能依赖trigger修正。第二阶段只能在旧writer停流、连接回收、权限负向探针和最终backfill完成后，由QUIESCING同一事务扫描零缺口并启用/验证稳态约束；该两阶段状态只来自既有`ADMIN_APPROVAL` mode/epoch，不新增第二布尔gate。

进入QUIESCING前必须先滚动完成所有新append writer与新Relay，停止旧writer流量、回收连接并撤销旧身份的授权Outbox写能力；随后重复backfill到稳定高水位。GSA-06把`outbox_events`所有权从`account_service`迁给`account_approval_authorization_owner NOLOGIN NOINHERIT`，并由该owner持有固定`account_approval_authorization`保护schema、sidecar、守卫及受控函数。DBA仅授该owner给`account_migrator WITH SET TRUE, INHERIT FALSE, ADMIN TRUE`；migrator是唯一最终member/admin，runtime/writer/backfill及反向成员关系均拒绝。迁移先复核`session_user=account_migrator/current_user=account_service`、现有Outbox/control owner与ACL；同一事务由migrator凭ADMIN临时GRANT service成为owner成员，并由public schema管理者临时GRANT owner以public CREATE，仅为执行`ALTER TABLE public.outbox_events OWNER TO ...`，随后立即REVOKE两者并复核；再用migrator数据库CREATE前置创建新schema并撤销数据库CREATE。跨control owner的deferred trigger显式SET ROLE GSA-04 owner创建。中途失败必须回滚owner、临时成员/schema ACL、schema和ledger；fresh/existing/reentry/down实证Outbox/sidecar/functions owner、migrator唯一最终member/admin、service不能SET ROLE及两项临时权限不可见。`account_service`只获回旧append的`INSERT(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)`、所需SELECT与Relay `UPDATE(attempts,next_attempt_at,locked_by,locked_until,published_at,dead_lettered_at,last_error)`，不获approval epoch、ALTER/TRIGGER/DROP/DELETE/TRUNCATE，所以N/N-1非授权append/Relay保持而runtime不能禁用授权守卫。另新增`account_approval_writer LOGIN NOINHERIT`与`account_approval_backfill LOGIN NOINHERIT`；全部高权属性关闭、互不继承service/migrator/control owner，分别使用独立Secret/DSN。writer从专用pool开启唯一APPROVE事务，持有精确审批读/状态写及版本化授权append函数权限，不持历史backfill、通用Outbox、Relay delivery或DDL权限；backfill只可调用有界候选/readiness与NULL→epoch1+sidecar函数。所有writer/backfill/validator/trigger函数必须`SECURITY DEFINER SET search_path=pg_catalog`、schema全限定引用、REVOKE PUBLIC EXECUTE、按精确signature单向授权并核对`session_user`；control owner deferred trigger只获调用authorization validator，不获Outbox/sidecar DML，authorization owner也不获control UPDATE。writer配置/连接/权限已知不可用时不得fallback到`account_service`，APPROVE在request锁和副作用前返回503 `APPROVAL_WRITER_UNAVAILABLE`、`Retry-After: 5`、`no-store`并移除APPROVE动作；Account仍服务REJECT/WITHDRAW/历史GET。若事务中途撤权、断连或权限失败则整个事务rollback并返回失败关闭类别、零提交副作用，不能承诺锁前，commit结果未知只查证不重试。旧writer仍用`account_service`，故不得把“停止旧Pod”冒充撤权：旧身份只有在exact LEGACY/epoch1 trigger条件下可写授权subject，QUIESCING后旧形状由owner守卫拒绝而不影响非授权append；cutover须逐副本证明旧连接为零并以三身份负向探针证明权限。`account_service`作为数据库owner仍能破坏所属数据库/schema造成既有灾难性可用性风险，但因不再拥有Outbox表不能静默禁trigger或合法篡改授权行；GSA-04 Linux环境欠账仍传递，未补证前不能进入生产QUIESCING。QUIESCING转换固定使用`SERIALIZABLE`短事务，先锁定`ADMIN_APPROVAL FOR UPDATE`，再在同一事务、同一可串行化快照完成最终授权行扫描，验证epoch非null且为可识别历史/当前值、sidecar恰一、serialization/provenance与oneOf字段合法；对可投递变体重算delivery bytes/digest，对已published历史变体只重算semantic digest。GSA-06安装owner持有、默认不授予runtime/backfill执行权的最终验证函数与deferred control-transition约束：未来GSA-09在同一caller-owned事务调用验证函数并按合法边CAS mode，事务末trigger再次扫描通过后才提交；无需在兼容迁移时创建会约束N/N-1新行的无条件`NOT VALID`约束，也不新增第二gate。自QUIESCING起trigger绝不填默认值，缺失/错误epoch、sidecar或各变体required/forbidden字段固定失败关闭。旧writer trigger先取得FOR SHARE时，QUIESCING等待并在最终扫描纳入其提交；QUIESCING先取得FOR UPDATE时，迟到trigger等待后看到mode已非LEGACY并零Outbox失败；撤权先完成时旧writer在trigger/INSERT前失败。授权append与QUIESCING事务若遇SQLSTATE `40001/40P01`，只允许调用方对**已证明回滚的整个事务**做最多3次总尝试的context-aware有界全抖动重试（第2次0～25ms、第3次0～50ms）；decision/state-event/authorization-event IDs在闭包外冻结，每次重试复用同一业务idempotency/expected version并重新取锁/重验control及DB时间。context取消/超时、commit结果未知、约束/权限/永久错误，或重试中发现schema/mode/wire/proof/state/approval_epoch不再满足兼容条件时均立即停止；仅`version/authorization_relay_epoch`因RUNNING→FENCED→RUNNING维护往返变为其他正整数时，重新取锁并确认其余条件仍满足后允许继续。禁止trigger内部、单条INSERT或最终扫描/VALIDATE局部重试。

GSA-06实现接缝冻结为Account自有窄层，不能让`internal/platform/events`反向依赖`approvalencoding`。APPROVE在开启`SERIALIZABLE`事务后、取得request `FOR UPDATE`前先取`ADMIN_APPROVAL FOR SHARE`；随后才执行request/version、权限、activation及既有批准事实重验。授权专用helper从实际持久Outbox `RETURNING event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at`结果构造`events.Event`，LEGACY模式只调用GSA-05 `EncodeLegacyAuthorizationEvent`，再以同一事务写`FROZEN_AT_APPROVAL` sidecar；不得用插入前`map`的Go JSON顺序替代JSONB持久事实。APPROVE的request CAS后version、Outbox aggregate version、锁定control approval epoch、Outbox epoch、sidecar event id/serialization/bytes/digest必须逐项相等，任一失败整事务回滚。REJECT不取control、不写Outbox/sidecar，维持当前安全收口。proof-era编码虽已存在，但真实批准路径在GSA-13 proof与gate完成前只能产`LEGACY_TYPED_V1`。

迁移结构必须为后续proof-era写入保留同一条原子路径，但GSA-06不得以SQL重写JCS或把request中的legacy numeric JSONB与新wire中的decimal-string JSONB直接等值比较。GSA-06即交付并以合成锁定request/control事实测试未接真实APPROVE的proof-era typed writer接口；GSA-13只在领域proof与gate完成后接入实际批准分支。受信Go窄writer必须从锁定request/snapshot调用GSA-05 typed转换得到closed artifacts，生成authorization event bytes，并把与event payload同语义的decimal-string JSONB写入Outbox、原bytes及其SHA-256写入sidecar；原request事实仍保持legacy numeric。真实PostgreSQL回读后须用dual decoder和`ExpectedAuthorizationBinding`逐命令核对`ConvertLegacyProofInput` artifacts，以及request CAS后version/Outbox aggregate version、control/Outbox/wire epoch和event id。数据库只负责一对一/nullability/provenance/不可变、`SHA-256(delivery_bytes)=event_digest`及event/version/epoch/discriminator关联，不自行序列化或只凭调用方digest放行。GSA-06的生产APPROVE仍只选择LEGACY writer，不得因该未接线接口产生v2。

GSA-06 backfill/readiness按`(occurred_at,event_id)`固定keyset和启动时有界高水位工作，每行在调用者事务锁定后从持久列构造`events.Event`并使用当前`Event.Marshal`；已存在sidecar必须全字段、逐字节核等，禁止`ON CONFLICT DO NOTHING`掩盖漂移。迁移前未发布与DLQ写`RECONSTRUCTED_FOR_DELIVERY`，已published只写`RECONSTRUCTED_AFTER_PUBLISH`；失败行整行零修改，报告不得包含payload/delivery bytes。授权Outbox守卫仅白名单允许Relay更新`attempts/next_attempt_at/locked_by/locked_until/published_at/dead_lettered_at/last_error`；身份、payload、trace、occurred_at和epoch更新、授权DELETE及存在授权行时TRUNCATE均拒绝。唯一受控epoch例外是owner函数在同一事务锁定原行与control、确认epoch仍NULL且sidecar不存在后，把既有授权行推进到epoch1并写入逐字段核等sidecar；runtime writer不能改历史。GSA-06只提供只读最终扫描/约束验证接缝，不执行mode CAS；新Relay、claim/read seam和逐字节发布属于GSA-07。新APPROVE从`account_approval_writer`专用连接开启唯一SERIALIZABLE事务，在同一事务完成control/request锁、decision/request/state、Outbox与sidecar；禁止service pool先提交后由writer pool补写。REJECT仍走既有事务且不依赖该新凭据。现有GSA-04随机schema历史fixture只允许精确截取迁移`<=000050`，另用独立数据库对`000051`及完整生产链取证，避免让不消费control/sidecar的历史fixture误连固定保护schema。

任何历史payload（包括迁移前DLQ）若无法由当前`events.Event.Marshal`路径完整重构typed legacy delivery bytes，或重算digest/字段类型/原event id不一致，必须生成只读迁移阻断报告并保持LEGACY/RUNNING；报告只含event id、稳定reason、schema/version和脱敏摘要，不回显payload。该行不得猜填、改写payload/event id、人工replay、创建替代Outbox或封存为exclusion。runbook只允许核对原数据库备份、迁移审计与已有PubAck/领域effect证据；无法证明时持续硬阻断并另立数据修复需求。

新 Billing consumer 另以 Account DB 中的持久应用状态限制副作用尝试，而不是依赖 JetStream `MaxDeliver`；Billing workload 只能通过绑定三元组和execution fencing的窄内部CAS操作更新，记录本身不授予Begin或领域执行权。状态主键为 `original_event_id + durable`，closed 字段至少含 `schema_version,event_digest,stream,stream_sequence,command_type,policy_version,executor_identity,domain_attempt_count,state,dlq_message_id,dlq_stream_sequence,last_error_code,observed_at,updated_at,version`；`state` 仅 `READY/DOMAIN_INFLIGHT/RESULT_CONFIRMED/DLQ_PUBLISHED_ACK_PENDING/TERMINAL_CONFIRMED`。`delivery_bytes` 精确定义为第9.1.1节sidecar中的不可变bytes；proof-era由第8.2节typed closed RFC 8785 encoder生成，legacy由当前`events.Event.Marshal`确定性生成。`event_digest = lowercase_hex(SHA-256(delivery_bytes))`，consumer直接对收到的原始bytes取摘要，禁止解析后重新序列化；对迁移后可投递行必须与sidecar digest一致，对`RECONSTRUCTED_AFTER_PUBLISH`历史摘要不得用来证明broker byte identity、ACK或effect。缺失、错误或不可重构的legacy sidecar阻断QUIESCING，不能进入新路径。所有观测时间由Account DB语句取得，不能采信consumer时钟；它们仅用于审计、attempt恢复和告警，不生成消息可达TTL，也不参与PROOF_REQUIRED gate。

`domain_attempt_count` 只统计可能产生领域副作用的调用，并在调用前以 CAS 持久递增；进程在递增后、调用前 crash 会消耗一次额度而不会扩大上限。最多允许 12 次领域调用：第 12 次失败后，任何 delivery、重启或恢复 worker 都只能查询原幂等结果或进入 DLQ 收口，绝不能再次调用领域。UNKNOWN 只查询，不通过重建命令消耗或重置次数。每次 Begin、领域调用、结果确认和 complete 都沿用原 execution ID/idempotency/fencing；重复 delivery 不得产生第二 execution 或效果。

目标事件的物理消息处置固定为：Account已权威拒绝Begin的终态（例如request已INVALIDATED）或原领域结果已确认且Account complete成功时，才可用有deadline的`DoubleAck`完成；Begin前失败、SAFE_ROLLBACK、TRANSIENT或仍需查证的UNKNOWN均对同一原message执行`NakWithDelay(2^min(num_delivered,9)秒)`，最大512秒，不得ACK后另建消息。Billing在应用层第12次领域失败后转下述DLQ收口；Token不设应用层12次上限并持续原message NAK/查证。所有DoubleAck返回都必须检查，超时或不确定时依赖同一原message的broker重投与持久状态幂等收敛，不能本地标记成功或主动补发事件。

DLQ 收口固定为：以原 canonical event bytes、原 event digest 和稳定 `message_id=<original_event_id>:<durable>:dead-letter` 发布既有 consumer dead-letter subject；这不是 Account 授权 Outbox、不得被执行 consumer 订阅或 replay。只有收到 DLQ PubAck 并持久化其 stream/sequence 后，状态才 CAS 为 `DLQ_PUBLISHED_ACK_PENDING`；该状态已永久禁止领域调用。随后必须对原 JetStream message 使用有 deadline 的 `DoubleAck`，且实现门禁必须证明所用客户端/服务端组合中 `DoubleAck` 返回 nil 表示服务器已确认原 ACK；不能用 `TermWithReason` 返回 nil、单向 Ack 或本地函数成功冒充确认。DoubleAck 得到服务器确认并成功持久化后才成为 `TERMINAL_CONFIRMED`。

crash 恢复保持失败关闭：DLQ PubAck 前重复 publish 只用同一 message ID 并由 broker 去重；PubAck 后、DoubleAck 前 crash 时，原 message 重投后只读取 `DLQ_PUBLISHED_ACK_PENDING` 并再次 DoubleAck，绝不调用领域；DoubleAck 已获服务器确认但 DB 终态落盘前 crash 时，行可以保留在 `DLQ_PUBLISHED_ACK_PENDING`，恢复器只能凭原 message 再投递或 broker 提供的逐 stream-sequence 权威 ACK 证据补记 `TERMINAL_CONFIRMED`。若当前 JetStream API 不能提供逐消息权威查询，则保留中间态并告警，禁止猜测终态、重放消息或再调领域；发布门禁必须先用生产版本证明 DoubleAck 语义和上述 crash 窗口。Token 继续无限投递并只查证未知结果，不进入 Billing 的 12 次/DLQ 终止协议。SAFE_ROLLBACK 只 NAK 同一原 delivery，不新建授权事件。

测试矩阵必须覆盖：授权Outbox未发布、发布后DB close确定/不确定、Relay claim正常完成/过期回收/crash、仅进Relay DLQ未确定published、pending/claim/handler非0时FENCE失败、FENCE后零publish、可信Q与伪造Q、三个新Billing durable恰从Q+1且不接Q前历史、旧Billing durable `MaxDeliver=-1`排空后删除或永久pause/撤ACL，并由旧workload对旧durable执行不发布新消息的consumer-info与bind/fetch负向探针，证明not-found/paused或authorization-denied、零消息、零handler且Q+1新durable仍为空；六个三元组唯一durable、错误executor/workload/ACL零领域调用；delivery bytes/digest/provenance篡改；同一Msg-Id自动at-least-once恢复在broker去重窗24小时内、恰跨窗和窗外产生重复/乱序delivery；consumer Inbox、Account Begin、execution与领域幂等逐层拒绝重复效果；Billing第1～12次领域失败、在第12次前后每个crash点、次数CAS竞争、UNKNOWN查询、DLQ PubAck成功/失败/重复、`DLQ_PUBLISHED_ACK_PENDING`重投、DoubleAck服务器确认/超时/进程crash及不能证明确认的保守中间态；Token `MaxDeliver=-1`且无DLQ；5分钟deadline前后、expiry worker与cutover reconciler竞态、20分钟readiness超时不改deadline/status；只读precheck与失效并发；Begin CAS先与INVALIDATED CAS先；execution lease过期/fencing接管；领域effect已提交但Account未确认；重复reconciler/重复delivery。全矩阵必须保留原Outbox、永久禁止人工/管理面requeue/replay但允许同一原行自动恢复，并保证最多一个execution、最多一次领域效果；PROOF_REQUIRED切换快照逐项证明APPROVED/EXECUTING/EXECUTION_FAILED/旧epoch非终态execution/未知effect均为0。

Outbox迁移测试另须覆盖：迁移前未发布/dead-letter授权行由当前`events.Event.Marshal`确定性backfill为legacy epoch 1、`RECONSTRUCTED_FOR_DELIVERY`且golden逐字节锁定七字段顺序、UUID、UTC/小数秒、RawMessage对象键序、legacy JSON number/null、HTML escaping及digest；迁移前已published行只生成`RECONSTRUCTED_AFTER_PUBLISH`语义摘要并证明它不能充当broker byte/PubAck/effect证据；proof-era新行固定`APPROVAL_RFC8785_DECIMAL_V1/FROZEN_AT_APPROVAL`。非授权event保持epoch null/无sidecar。真实PostgreSQL并发屏障覆盖trigger先取得FOR SHARE、QUIESCING先取得FOR UPDATE、撤权与旧writer并发；trigger只在`schema1/LEGACY/wire1/proof=false/RUNNING/approval epoch1`为缺值填1且永不改显式错误值；control初值version1/relay epoch1后执行RUNNING→FENCED→RUNNING维护往返，即使当前正整数version=3且relay epoch递增，旧writer仍成功，approval epoch非1则固定拒绝。另覆盖SERIALIZABLE `40001/40P01`整个事务最多3次context-aware全抖动重试、重试时control变化、最终取消/超时与永久错误不重试；新writer显式epoch/sidecar事务原子；null/0/future/错误epoch、缺/重复sidecar、serialization/provenance错误、payload类型漂移、bytes/digest篡改全部在claim/QUIESCING前失败。迁移前DLQ可重构时保持原event id/payload/dead-letter并允许后续合法exclusion，不可重构时只读阻断且禁止exclusion。所有正反用例逐字节断言原Outbox payload/event id、published/dead-letter状态和既有event digest（如有）不被迁移改写，并覆盖migration/backfill/Relay任一crash后幂等恢复。

### 9.2 授权事件永久禁止人工/管理面通用 replay

`account.approval-command.authorized.v1` 永久排除在三领域**人工/管理面**通用 Outbox replay 合同之外；`platform.events.retry`、`EVENT_REPLAY` workload、超级管理员或任何未来 Portal 路由都不能改变该结论。Account 事件 GET 只可返回脱敏审计摘要和 `replayable=false`，不得返回可重放动作；任何 `POST /outbox-events/{event_id}/replays` 命中该 event type，必须在创建 replay request、清 dead-letter、改尝试次数或写 next-attempt 之前固定返回 409 `AUTHORIZATION_EVENT_REPLAY_FORBIDDEN`。Account DB 受控 requeue 函数/触发器或等价约束必须按持久 event type 再次拒绝，不能只靠 HTTP/Portal 隐藏；拒绝只记录脱敏审计，不产生 `event_replay_requests`、Outbox mutation、claim 或 publish。

上述禁止不包括 Relay 对**同一不可变Outbox行**的自动 at-least-once 投递恢复：只要DB尚未确定收敛为published/dead-letter/exclusion，lease恢复器就可继续使用原event ID作为相同`Nats-Msg-Id`和sidecar同一语义delivery bytes发布。broker PubAck与DB `published_at`/claim close之间发生超时、连接断开或crash时，Relay不得凭进程内结果猜测成功，也不得修改event/payload/bytes；允许再次发布同一消息。NATS 24小时去重窗只是一项优化，超过该窗口产生重复、乱序流消息仍属同一原行的自动恢复，不是人工requeue/replay。consumer必须以`event_id`写Inbox去重，Account Begin以request-status CAS取得唯一execution所有权，execution ID、领域幂等键和领域副作用fencing共同保证最多一个execution、最多一次effect；重复或乱序delivery只ACK/查证，不再次Begin或执行。FENCE必须等待每个授权Outbox在DB中明确成为published、dead-letter或合法exclusion，不能用“曾收到PubAck”、broker去重命中或重构历史digest替代该事实。

QUIESCING/SAFE 中，只有 request 已 CAS 为 INVALIDATED，且 Account Begin/execution 查询与领域幂等/effect 权威查询均明确为不存在的历史授权 dead-letter，才可追加唯一、不可变的 `approval_authorization_delivery_exclusions` 记录。closed 字段固定为 `schema_version,event_id,event_digest,event_approval_epoch,request_id,request_version,request_status,sealed_at_control_approval_epoch,reason_code,verified_at,audit_ref`；`event_approval_epoch` 必须逐字复制原Outbox不可变`approval_control_epoch`，`sealed_at_control_approval_epoch`只记录封存时当前control的`approval_epoch`用于审计，绝不参与后续FENCE相等门禁；`reason_code` 仅 `CUTOVER_INVALIDATED_NO_EXECUTION` 或 `SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION`。插入短事务必须对同一`ADMIN_APPROVAL`行FOR SHARE并记录`sealed_at_control_approval_epoch`，再锁request/outbox并重验INVALIDATED、request version、event id/type/digest、`event_approval_epoch == outbox.approval_control_epoch`、dead-letter及Account无Begin/execution；control epoch后来变化不使合法exclusion失效。FENCE的FOR UPDATE因而只能看见完整提交的exclusion或仍把该DLQ判为未分类，不能读到半封存。该记录只把原 dead-letter 分类为“永不可发布的终态 cutover exclusion”，原 Outbox/dead-letter/payload 保持不变且永久留审计；它不创建第二 Outbox、不清 dead-letter、不重排 next-attempt，也不证明曾执行。FENCE 同事务只可从未分类 dead-letter 中扣除event id/digest/request id/request version/event approval epoch均与原Outbox一致的已验证exclusion；不得要求其`sealed_at_control_approval_epoch`等于FENCE时control epoch。缺记录、上述任一绑定不一致、request仍非终态、仅Inbox/claim、查询超时或effect未知都继续阻断。FENCE/Q 后 exclusion 永远不能删除、撤销或改回可发布，恢复、回滚和人工/管理面通用 replay 也不能绕过。

验收必须覆盖：HTTP/DB/工作负载/Portal 四层均拒绝授权事件 replay；GET `replayable=false` 且无动作；伪造 event type、跨域 ID、同 ID 改摘要、直接调用 DB requeue、授权事件与普通未发布死信混合批次均失败关闭。另覆盖 INVALIDATED+无Begin/execution/effect 的合法 exclusion、INVALIDATED但已有/并发Begin、仅Inbox、仅claim、effect未知、exclusion写入与Begin CAS竞态、FENCE/Q前后重排尝试和审计保留；修改/伪造`event_approval_epoch`或使其不等于原Outbox`approval_control_epoch`必须阻断，封存后current control `approval_epoch`正常变化不得阻断。合法 exclusion 可永久保留且不阻断FENCE，其他授权DLQ必须阻断。

## 10. EXECUTION_FAILED 恢复协议

不得统一自动重试 `EXECUTION_FAILED`。每个 executable command/version 都必须在领域注册表声明：稳定错误分类、`retryable`、最大次数、退避、租约/fencing、`next_retry_at`、绝对执行窗口、查询/确认方式及失效条件。

1. **TRANSIENT_SAFE**：仅在领域证明幂等、原执行 ID/键不变、CAS/policy/activation 未漂移且窗口未结束时，由独立 worker 按有界次数与退避重试；人类只能安排这条既定安全重试，不能改参数或直接执行。
2. **UNKNOWN**：只查原领域引用、Inbox/result/账本或 Provider 证据；不得重发副作用。
3. **PERMANENT**：确定性 INVALIDATED，业务页可 fresh superseding。
4. **FACT_OR_POLICY_CHANGED / WINDOW_ENDED**：先 INVALIDATED，再由业务页重新读取事实创建替代申请。
5. **未知错误码或缺分类**：按 UNKNOWN 只查证，绝不默认 retryable。

Worker 使用独立最小工作负载身份，不由 Portal、审批人或通用事件重放触发。租约过期、进程崩溃、迟到响应和并发调度不得重复计费、重复记账或生成第二领域效果。

## 11. 八类 policy-only 财务命令的 P2 设计门禁

八类命令必须逐类独立定义并验收，不能以一份通用修复 schema 或一个实现批次整体激活：

| 命令家族 | 必须先冻结的领域不变量与恢复证据 |
|---|---|
| 权益组成调整 | BASE/BONUS/来源组成守恒、消费顺序、过期与退款关联、成对补偿分录 |
| 账本修复 | 双边平衡、币种/积分隔离、不可变 posting、反向/补偿而非原地改写 |
| 应收核销 | 本金、清偿分配、减免授权、余额与账本关联、补偿账务 |
| 支付财务修复 | Provider/订单/交易/履约/账本证据，未知结果只查证，外部幂等引用 |
| 退款异常修复 | 原支付、可退基数、权益冻结、退款/账本证据与 Provider 最终态 |
| 拒付财务修复 | 拒付证据、订单/退款/权益/应收联动、外部争议引用与补偿账务 |
| 对账财务修复 | 冻结 run/case 差异、四方证据、责任归因、修复后再核对 |
| 用量结算修复 | 原 usage request、Hold、定价快照、adapter/核对证据；绝不重发模型请求 |

每类都必须独立追加 policy version、closed request/snapshot/proof/detail schema、允许/禁止变化、补偿账务、外部证据、跨域未知恢复、executor/consumer/recovery 注册、权限、审计、N/N-1、回滚和真实领域 E2E。八类全局 gate 不能替代逐类 activation。

## 12. 错误、缓存与可观测性

- 状态文案逐值精确映射，不把 `APPROVED` 显示为执行中、不把 `EXECUTION_FAILED` 显示为可重试、不把 `INVALIDATED` 显示为拒绝。
- 稳定错误至少区分未激活、policy/proof/schema 不兼容、近期认证不足、无权/不可见、自审、版本冲突、proof 拒绝/超时/不可用、cursor 篡改/过期/容量、恢复窗口结束、Relay栅栏、SAFE失效、授权事件禁止重放和详情 presenter 不可用；其中LEGACY/PROOF_REQUIRED在Relay FENCED时批准固定503 `APPROVAL_RELAY_FENCED`、`Retry-After: 5`、`Cache-Control: no-store`，授权事件通用重放固定409 `AUTHORIZATION_EVENT_REPLAY_FORBIDDEN`，SAFE旧epoch无执行失效固定`APPROVAL_SAFE_ROLLBACK_INVALIDATED`。
- Token `PRE_ACTIVATION / ACTIVE / SAFE_ROLLBACK` 与 epoch、Billing activation 版本、authorization version、policy version，以及Account `mode/approval_epoch/authorization_relay_state/authorization_relay_epoch` 都进入请求/缓存键；任一变化时先 abort，再清撤销内容与旧`allowed_actions`，迟到响应不回写。Relay切至FENCED后任何缓存详情都不得继续显示APPROVE。
- 日志和审计仅记录 request ID、命令/策略/schema 版本、稳定错误码、activation/epoch、主体/作用域摘要和脱敏 proof/result 引用，不记录原始命令 JSON、Provider 原文、secret 或个人敏感字段。

## 13. 验收合同

### 13.1 P0

1. Token 三命令分别验证 PRE、ACTIVE、SAFE、epoch 漂移、缺行、未知版本、N/N-1 与 rollback；Account 与 Token 对同一权威结果一致，Account 无可写 Token activation。
2. Billing 前向迁移后既有三命令保持 ACTIVE；八类 policy-only 均 INACTIVE。N/N-1、缺行、未知版本、混合版本和 rollback 失败关闭，六类既有 executable command 不回退。
3. 未激活命令的 create/update/submit/APPROVE 均在领域调用和副作用前拒绝；DRAFT 只读且无 withdraw，SUBMITTED 可 REJECT/WITHDRAW 安全收口。
4. 历史授权Outbox行不算领域执行证据且始终保留，Relay发布、仅Inbox、本地claim和只读precheck同样不算执行授权。QUIESCING前fence/delivery-bytes-aware新Relay必须滚动就绪，旧Relay claim/handler归0后逐副本撤claim/publish ACL与DB权限，并由负向探针证明旧副本不能再发布。APPROVE/QUIESCING与新Relay Claim/FENCE分别以同一singleton的FOR SHARE/FOR UPDATE锁线性化，锁顺序双方和crash矩阵结果唯一；FENCE数据库事务提交后才外部确认全副本handler=0。QUIESCING停止新APPROVE后，所有既有授权Outbox必须以原event ID/Msg-Id和同语义bytes自动at-least-once恢复至DB确定published/dead-letter/exclusion；PubAck-close不确定及24小时去重窗内外可产生重复乱序delivery；仅“request已INVALIDATED且权威确认无Begin/execution/effect”的历史授权DLQ可不可变封存为cutover exclusion，其余未发布/未知DLQ阻断。授权Outbox可发布pending=0、Relay active/reclaimable claim=0、未分类dead-letter=0、每行DB终态确定、全部Q前consumer pending/ack-pending/handler=0后持久FENCE授权Relay、读取可信Q并以Q+1创建未启动的新Billing durable；已验证exclusion永久保留且不计入这些零值。再停旧consumer、删除或永久pause旧durable并撤ACL。既有5分钟execution_deadline/expiry worker不变，20分钟只作健康drain/readiness预算；cutover reconciler仅以`EXECUTION_WINDOW_EXPIRED`处理deadline已过残留。Account Begin与reconciler CAS仍是唯一所有权线性化点，未知effect不得失效。gate同一快照逐项证明APPROVED/EXECUTING/EXECUTION_FAILED/旧epoch非终态execution/未知effect均为0后才CAS，随后启动新consumer并恢复Relay。Billing应用状态精确限制12次领域尝试并按PubAck/ACK_PENDING/DoubleAck收口，Token保持无限投递；授权event type在HTTP、DB、workload和Portal层永久禁止人工/管理面通用replay，同一原行自动恢复不在禁止范围，全矩阵无第二Outbox，并由Inbox/Begin/execution/领域幂等保证最多一个execution和一次领域效果。
5. Account control初值逐字段为LEGACY/1/false/RUNNING/approval epoch 1/relay epoch 1/version 1；授权Outbox epoch与delivery-bytes sidecar迁移按第9.1.1节通过。既有未发布/dead-letter授权行（含pre-migration DLQ）可重构者精确复用当前`events.Event.Marshal`，backfill legacy epoch1与可投递bytes/digest；已published历史仅产生带provenance的重构语义摘要，不证明原broker bytes、ACK或effect。原event id/payload/发布与dead-letter状态逐字节不变，非授权event保持null/无sidecar。兼容trigger在旧writer同一SERIALIZABLE事务先取control FOR SHARE，仅schema1/LEGACY/wire1/proof=false/RUNNING/approval epoch1填缺失epoch，并接受维护往返后的任意当前正整数version/relay epoch；新writer显式写并核对。trigger/QUIESCING两种先锁、撤权并发和整个事务有界重试通过；旧writer撤权、最终同事务扫描和约束VALIDATE先于QUIESCING。不可重构payload、null/错epoch、缺/错sidecar均硬阻断且不能exclusion、重写或人工replay。

### 13.2 P1

1. 导航、旧 section/settings/history、我的申请/待我审批、普通 audit-only 回归通过；异常索引无入口、无请求。
2. 安全目录不要求 `platform.roles.read`，且只返回 visible/requestable；具体申请动作只来自 detail.allowed_actions。“全部”严格为本人历史与当前可审批非本人集合的并集，撤权后非本人申请立即消失但本人历史最小 envelope 保留，版本变化中止请求并清缓存。
3. v2 列表只返回最小 closed envelope；v2 详情 oneOf、脱敏、无 presenter fallback、最小执行信息通过 schema 与越权测试，v1 在兼容窗口维持既有完整线格式。detail/decision/events/timeline/execution/result/recovery 的不存在与不可见路径必须字节级等价 404，且不可见路径为零 presenter/proof/领域/事件查询；`platform.audit.read` 只读独立脱敏审计投影。顶部统计要么来自独立聚合，要么不存在。
4. Account approval pagination 精确覆盖冻结 envelope、50/200 limit、`updated_at DESC,id DESC`、15/20 分钟 TTL/tombstone、1,000,000/64/20,000,000/8GiB 容量、6/60 创建速率、10/3 秒 deadline、全部 HTTP code/Retry-After、70%/85% 水位、v1+kid current/previous 35 分钟轮换和 gate/hard-disable；签名内容包含固定 sort 与按 closed scope 投影计算的 `scope_digest`，authorization version 必须为正整数，签名/schema/sort/scope/authorization 校验在成员 SQL 前完成。并发更新无重漏，旧 cursor 每页重验当前权限，且 Billing/Token 既有合同字节级不变。
5. 审批中心快捷 submit 只接受未修改 DRAFT；业务页可 Update 同一已激活 DRAFT 并按新 expected_version/precheck/CAS submit。resume_draft、fresh superseding、双向链接和浏览器历史通过；中心不重建表单、不刷新快照、不复制时间线。
6. 浏览器零 proof、零 Provider probe；BFF 剥离 proof-like header/body。六个既有 executable command/version 的持久 target/normalized/critical/financial 到 `proof_input` 精确映射、typed closed RFC 8785 公共 encoder、legacy/new digest golden、Account 内部身份、2秒绝对deadline、请求响应合计64KiB、30秒skew、60秒TTL与closed 11字段响应全部通过；普通json.Marshal、展示对象和Token专用canonicalizer负例必须失败。生命周期proof只接受current PUBLISHED/DEPRECATED、target DEPRECATED/REMOVED和三条真实迁移，target DRAFT/PUBLISHED等非法向量在领域SQL前失败。无权/不可见/自审/recent-auth失败发生在领域调用前；Account只CAS本域并持久化关联revision，领域executor副作用前重读验证，漂移零副作用。六命令覆盖allow/deny/稳定错误/超限/malformed/secret/不可映射历史/TOCTOU/N/N-1。
7. 每个已激活命令有独立失败分类与恢复合同；transient、unknown、permanent、漂移、窗口结束、租约接管和 crash 用例证明零盲重放、最多一次领域效果。
8. proof切换覆盖第5.4节全部8种mode×Relay状态动作、mode边、具名Relay边及DRAFT/SUBMITTED/APPROVED/EXECUTING/EXECUTION_FAILED矩阵；LEGACY/PROOF_REQUIRED在FENCED时APPROVE必须返回503 `APPROVAL_RELAY_FENCED`并保持SUBMITTED、零权威proof绑定/Outbox，detail无APPROVE动作，入口已见FENCED时零proof调用。另覆盖RUNNING/FENCED与APPROVE、proof、普通Claim、SAFE_DRAIN Claim、FENCE的竞态及任一CAS/crash；SAFE_DRAIN只处理SAFE前一approval epoch已提交的同一原行，以原Msg-Id/同语义bytes自动at-least-once恢复，失败才进入dead-letter/exclusion，绝无新Outbox/Begin。PubAck/DB close不确定及24小时broker去重窗内外允许重复流消息，但Inbox/Begin/execution/领域幂等保证最多一个execution/effect；人工requeue/replay始终拒绝。FENCED下consumer启动失败或Relay恢复CAS未提交必须证明该新epoch零APPROVED/Outbox；Relay恢复CAS已提交后即按RUNNING故障分支处理已有/并发批准。历史授权Outbox按DB确定published/dead-letter/exclusion、可发布pending=0、active/reclaimable claim=0、未分类dead-letter=0、Q前consumer pending/ack-pending/handler=0收敛；合法exclusion永久留存不阻断。Q+1新durable、旧durable停用撤ACL、5分钟deadline与expiry worker不变均有故障测试。SAFE分别覆盖Relay FENCED/RUNNING、`APPROVAL_SAFE_ROLLBACK_INVALIDATED`、已Begin只查证、旧epoch状态/unknown归零，以及“先FENCE并验证→epoch递增→启动consumer→恢复Relay”的唯一恢复顺序。六命令不被置为inactive，历史APPROVED不补proof、不跨epoch，APPROVED/EXECUTING/EXECUTION_FAILED或旧epoch非终态execution任一非0、effect不明或任一精确零值未满足时都不得切gate。非法purpose/mode/Relay边、非法mode跳转/true→false、混合版本/crash/gate/Begin竞态均失败关闭，无legacy豁免、无第二Outbox/execution/effect。
9. v1/v2 versioned path、同库同状态机、Account DB control合法元组/边/version/epoch的DB与服务双约束，以及required keys恰为`schema_version/mode/approval_wire_version/proof_required/epoch/authorization_relay_state/authorization_relay_epoch/version/request_id`的closed capability通过N/N-1。v2 detail required正整数`action_control_version`，v1 detail body不变且返回同义稳定header；capability/detail全部no-store。Portal对version缺失、畸形、不一致、RUNNING→FENCED→RUNNING及迟到RUNNING响应执行abort/清缓存/不复活APPROVE。Redis Session CAS/SETNX不可变协议选择覆盖混合副本、多Portal、多标签、刷新和回滚；proof开启后legacy N-1的所有approval写与Begin在路由和DB双层拒绝，回滚候选必须支持control/proof，旧Session缺字段=v1，gate回退时v2失败不降级，v1停写/停读只在探针、真实流量和回滚证据满足后执行。
10. 三语、键盘、焦点、屏幕阅读器状态播报、320px、200% 缩放、离线、局部失败和 request ID 展示通过。

### 13.3 P2

1. 八类财务命令分别具备独立的领域不变量、补偿账务、外部证据、proof、版本、恢复和真实 E2E；单类缺口不得由其他类测试覆盖。
2. 每类独立验证 permission、recent-auth 决策、非自审、create/update/submit/approve gate、N/N-1、混合版本、rollback、重复事件、未知结果、并发和失败注入。
3. 只有该类 schema、presenter、consumer、executor、recovery、测试和部署 gate 全部就绪，才允许单独从 INACTIVE 切换；其余命令保持不变。
4. T9 异常索引只有在单独确认并完整实现权限、索引、脱敏、导航、详情下穿和 E2E 后才显示；未完成时继续隐藏。

## 14. 发布与回滚门禁

唯一发布顺序固定如下，其他章节只能引用本顺序，不得另列变体：

1. **Schema/reader**：先以前向兼容迁移交付proof/presenter/pagination/recovery、Billing持久attempt/DLQ收口状态，以及唯一Account `ADMIN_APPROVAL` control行的wire/proof/subject专属授权Relay字段与只读reader；control初值逐字段固定为LEGACY/wire1/proof_required=false/RUNNING/approval_epoch1/authorization_relay_epoch1/version1，并在DB冻结合法元组/状态边/单行锁/CAS。按第9.1.1节为Outbox追加nullable approval epoch与带serialization/provenance的delivery-bytes sidecar；兼容期只启用局部/FK/唯一/不可变及按LEGACY mode延迟的一对一约束，不创建会立即约束N/N-1新行的无条件`NOT VALID` sidecar-presence约束。按当前`events.Event.Marshal` backfill既有授权行和pre-migration DLQ；不得改原event id/payload/published/dead-letter事实，已published历史摘要不得冒充broker byte/ACK/effect证据。不得另建第二个Relay控制真相。Token只读取既有activation/epoch，Billing迁移为既有三命令写入兼容ACTIVE投影、八类policy-only写入INACTIVE。此时不改变任何实际executable行为。
2. **Typed encoder 与 Domain proof**：先交付通用typed closed RFC8785模块和legacy/new golden，再部署逐命令proof endpoint、domain revision与closed schema，同时保持只允许Account内部身份；旧执行路径仍不依赖新proof。
3. **Account compatibility**：部署Directory、v1/v2 list/detail/action、分页、proof client、四入口activation校验、历史reconciler、含mode/Relay state/两epoch/version的control closed capability、v2 body/v1 header `action_control_version`、proof-capable独立写角色/受控函数和observed_versions兼容；全部capability/detail no-store，所有新命令仍inactive，旧客户端body合同可用。新append writer显式写并核对approval epoch/delivery-bytes sidecar；兼容trigger必须在旧writer同一SERIALIZABLE事务先取control FOR SHARE，且仅在schema1/LEGACY/wire1/proof=false/RUNNING/approval epoch1替缺失epoch填1，当前version/relay epoch只须为正整数，backfill持续补旧writer sidecar。40001/40P01只按第9.1.1节重试完整事务。
4. **Execution safety 与 Relay N/N-1**：部署并验证 consumer、Account Begin、executor、recovery worker 的 status/activation/epoch/policy/lease-fencing 三层 gate；资源manifest与操作器必须先把Billing旧durable迁移为无限投递，声明三个新唯一durable的`quiescing_high_watermark_plus_one`起点规则，把六个command/policy/executor三元组与workload权限逐项锁定，同时验证12次应用层尝试、DLQ PubAck/DoubleAck、Token无限投递和legacy Account路由/DB只读隔离。QUIESCING前必须滚动部署全部显式写epoch/sidecar的新append writer，以及支持第5.4节singleton锁/FENCE/SAFE_DRAIN、sidecar delivery bytes原样发布、自动at-least-once恢复与人工授权event replay永久拒绝、epoch绑定claim的新Account Relay；停止旧writer流量、回收连接并撤销其Outbox写权限，最终backfill/扫描验证零缺失、零错误epoch/sidecar且在QUIESCING同一SERIALIZABLE事务内完成约束VALIDATE。新Relay只可先做只读/readiness，不能与旧Relay共用可绕过栅栏的写权限。等待旧Relay claim/handler归0后撤销每个旧副本的授权subject claim/publish ACL与DB claim权限，以负向探针证明其不能再claim/publish；只有新Relay副本集逐一readiness通过且成为唯一有权集合才可进入下一步。此步只在隔离环境验证新durable，不得在生产以DeliverAll预创建并积压历史事件，worker 或新Relay未 ready 时不得推进；任一legacy payload不可重构则留在LEGACY/RUNNING并执行只读调查。
5. **Portal compatibility**：部署治理导航、业务页编辑/续提/替代、Session协议CAS和BFF proof-like清理；wire仍为1时Portal N继续v1并按8.4保留legacy observed_versions，不可展示inactive动作。
6. **QUIESCING、Relay栅栏与readiness**：严格执行第8.6节唯一顺序：用与APPROVE/Claim共同锁定的唯一`ADMIN_APPROVAL`行FOR UPDATE CAS进入QUIESCING并停止新APPROVE；保持已升级的新Relay链运行，以同一原event ID/Msg-Id和同语义bytes自动at-least-once恢复既有授权Outbox直至DB确定published/dead-letter，或将唯一允许的INVALIDATED且无Begin/execution/effect授权DLQ封存为永不可发布cutover exclusion；确认授权Outbox可发布pending=0、Relay active/reclaimable claim=0、未分类授权dead-letter=0、每行DB确定published/dead-letter/exclusion与全部Q前consumer pending/ack-pending/handler=0，已验证exclusion允许永久留存且不计入前三项；再对同一行FOR UPDATE事务CAS Relay FENCED，提交后才在数据库外取得全副本Relay publish handler=0确认；读取可信Q，以Q+1创建但不启动三个新Billing durable；停止旧Billing consumer，删除或永久pause旧durable并撤ACL/探针验证；按既有5分钟execution_deadline与expiry worker把所有legacy APPROVED/EXECUTING/EXECUTION_FAILED收敛到终态，cutover reconciler只修复deadline已过残留，20分钟只作健康readiness预算。生产等价环境完成权限、closed schema、proof、资源/ACL/Q receipt、APPROVE/QUIESCING与Claim/FENCE锁竞态、历史Outbox/Relay/Begin竞态、人工授权replay四层拒绝、PubAck-close不确定与24小时去重窗内外重复乱序仍最多一个execution/effect、DLQ/DoubleAck crash恢复、失败恢复、N/N-1、rollback、三语/无障碍和领域E2E；任一未知effect或任一精确零值未满足均阻断。
7. **原子 proof/v2 gate、SAFE与恢复Relay**：仅在第6步全绿、Account一致性快照中六命令APPROVED=0、EXECUTING=0、EXECUTION_FAILED=0、旧approval epoch非终态execution=0、未知effect=0，且授权Relay仍FENCED、旧durable已不可接收、新durable仍空闲时，以expected_version CAS同一control为PROOF_REQUIRED/wire2/proof_required=true并递增`approval_epoch`；CAS成功后按该epoch启动Q+1新Billing consumer并确认handler ready，最后CAS授权Relay回RUNNING并递增`authorization_relay_epoch`。FENCED阶段detail无APPROVE，因此consumer启动失败或Relay恢复CAS未提交时必须证明新epoch零APPROVED/Outbox，再进入SAFE+FENCED；不能套用“已有批准”处置。Relay恢复CAS一旦提交就是RUNNING，随后的readiness/健康故障可能已与批准并发，必须进入SAFE+RUNNING：前一proof epoch中无Begin/effect的APPROVED及无活动Begin/lease且确认无effect的EXECUTION_FAILED，在deadline内以`APPROVAL_SAFE_ROLLBACK_INVALIDATED`收敛，SAFE_DRAIN只让其原Outbox以原event ID/Msg-Id和同语义bytes自动恢复至DB确定published/dead-letter/exclusion；PubAck-close不确定允许重复流消息；已Begin/effect/unknown只查证，deadline已到仍用`EXECUTION_WINDOW_EXPIRED`，Outbox永不接受人工/管理面通用replay。SAFE恢复严格按第8.7节把真实Relay状态归一为FENCED、清零旧approval epoch非终态/unknown、验证consumer ready、CAS递增approval_epoch、启动consumer、最后恢复Relay并递增authorization_relay_epoch；不得降级或切回旧共享durable，gate回退时v2失败关闭不降级。
8. **最后 activation**：上述七步全部完成后，才由Token既有受信activation流程按epoch切换适用三命令，或由Billing受信迁移/运维边界逐个把已完备的policy-only命令从INACTIVE切ACTIVE。Billing既有三命令全程保持ACTIVE；任何一项readiness退化先停止新动作/租约，再按既有SAFE/领域失活流程收口。

每一步均验证 N/N-1，旧客户端不能识别新字段时继续使用旧合同，不得收到部分新响应。Schema/reader 部署、Directory `visible` 或 presenter 可读都不等于 activation，不能提前开放 create/update/submit/APPROVE。

回滚先停止新建/更新/提交/批准和新执行租约，再回 Portal/Account/领域服务；历史 policy、activation、proof schema、Outbox/Inbox、execution 和领域引用保留可读。任何被引用对象都禁止 down 删除。未通过 P0/P1/P2 相应门禁的能力保持隐藏和 inactive。

截至本文日期，本专项需求已确认；GSA-01推进门禁增量已实现并通过B审核，GSA-02事实基线及必要文档门禁修复已通过B整批审核，不新增产品运行能力；其余目标尚未实现，整体仍未部署。
