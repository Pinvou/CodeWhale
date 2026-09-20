# GSA-16 六命令失败恢复实现合同

状态：实现中；本文不授权生产启用、人工重放或跨 epoch 补发。以 [治理与安全整改 §10](admin-governance-security-redesign.md) 为需求依据。

## 当前安全基线

- Account `000065` 将 proof-era Billing 确定失败保留为 `DOMAIN_INFLIGHT`；`000066` 栅栏化旧版本留下的 `READY + last_error_code`；`000067` 进一步把 `READY + 已有attempt`（即使错误码丢失）迁入只查证态，并使直接 grant 对 proof-era 仅允许第 0 次到第 1 次。Billing consumer 本地也拒绝第二次 Begin/Grant。legacy Billing 的既有 12 次上限不变。
- `EXECUTION_FAILED` 不由通用过期 worker 直接标记 INVALIDATED；已有执行记录的结果必须先经领域权威查证。未 Begin 的 APPROVED 仍按原五分钟执行窗口过期。
- Account `PlanProofApprovalRecovery` 已提供最多200条同一只读快照的原request/execution/event/durable、原幂等键、投递/执行状态、epoch、截止时间及分类；legacy不进入列表。它不取租约、不读取领域效果、不改变状态，也不暴露为Portal接口。生产恢复worker、独立身份和领域结果权威仍未接入，不可把这份计划当作完成恢复的证据。
- `InspectProofRecovery` 在30秒总预算内对每个closed候选仅调用原Billing/Token效果观测；`ABSENT`、`EFFECT_OR_ATTEMPT_OBSERVED`、不可用都输出 `UNKNOWN_QUERY_ONLY`。它不查询原命令的权威终态，也不授予零效果证明；只读效果观测不能替代逐命令领域结果查证或独立持久租约。
- Account `000068` 增加原执行/事件/epoch/请求版本绑定的15秒只读查询租约，重复抢占与错误token受CAS拒绝，释放按2～512秒退避。独立`account_approval_recovery`身份目前只预建为`NOLOGIN`，仅可执行租约函数，不能直读/写表。既有数据库升级前需要通过受控运维预建同样NOLOGIN角色；本阶段没有运行worker、颁发凭证或启用LOGIN，租约不授权领域重试或终态变更。
- 任何已提交 Billing proof claim、已进入 Provider 探测、Token 本地命令已开始、数据库提交结果不明、权威结果读超时或未知错误码，均按 UNKNOWN 处理。一次“未查到结果”不是“没有产生效果”的证明。
- 六命令 closed 分类已落入 `approvalconsumer.ClassifyProofFailure`。Billing 的 `DOMAIN_REVISION_CHANGED`、`COMMAND_SNAPSHOT_MISMATCH` 暂归事实/政策变化调查，`DOMAIN_RESOURCE_NOT_FOUND` 暂归永久失败调查；`DOMAIN_PRECONDITION_REJECTED` 混合余额、可用性和冲突，不具备单义分类，故与 Token 合并码 `DOMAIN_EXECUTION_FAILED`、缺码、未知版本一律归 UNKNOWN。分类本身不能证明零效果、不能直接 INVALIDATED；当前六命令均不允许自动第二次领域调用，`TRANSIENT_SAFE` 和 `WINDOW_ENDED` 必须待领域证据与绝对窗口核实后才可最终判定。

## 封闭命令矩阵

| 命令及 policy version | 工作负载与原结果依据 | 目前可自动再次执行 | 必须查证的领域事实 |
|---|---|---|---|
| WALLET_ADJUSTMENT / 1 | Billing；审批请求绑定的调整、钱包与账本 | 否 | 不可变proof claim、调整状态、ledger journal、原execution/idempotency |
| PAYMENT_PROVIDER_RESUME / 2 | Billing；恢复执行行与Provider observation | 否 | claim、恢复执行、探测订单和原观测证据；不得为查证再探测Provider |
| PAYMENT_PRODUCT_RESUME / 1 | Billing；恢复执行行与Provider observation | 否 | 同上，另核对商品、mapping和admission版本 |
| TOKEN_MODEL_LIFECYCLE_CHANGE / 2 | Token；原command reference及生命周期计划 | 否 | 命令结果、计划状态、catalog与price证据 |
| TOKEN_MODEL_CATALOG_ROLLBACK / 2 | Token；原command reference及目录版本 | 否 | 命令结果、目录写入、路由及价格证据 |
| TOKEN_CREDENTIAL_REVOKE / 1 | Token；原command reference及凭据隔离结果 | 否 | 命令结果、凭据/路由状态；不能重放外部探针 |

未知命令或版本不回退到相近命令。各项最大尝试次数、指数退避、绝对窗口和租约必须由封闭注册表显式定义；Billing 的12次是 legacy 传输上限，不能当作 proof-era 的可重试许可，Token 不使用 Billing 的 DLQ/12次计数。

## 后续实现顺序与验收

1. 六命令的保守错误码分类已落地；继续接入领域权威结果和零效果证据，最终判定 `TRANSIENT_SAFE / UNKNOWN / PERMANENT / FACT_OR_POLICY_CHANGED / WINDOW_ENDED`。只有领域返回可验证的“零效果且可按原 execution/idempotency/fencing 重试”证据，才允许 `TRANSIENT_SAFE`；缺码或未识别码一律 UNKNOWN。没有安全证明的命令继续零自动重试。
2. 独立最小权限恢复身份持有有界租约，读取 Account 原执行、delivery、epoch、deadline，再读取唯一领域结果和持久证据。UNKNOWN 只能重复只读查询；租约丢失、超时或并发抢占不得创建新执行、调用Provider或复用浏览器请求。
3. 只有原结果已权威确认才可幂等完成 Account 的同一执行和消息确认；确定永久/漂移失败必须先证明零领域效果，才可在控制行和请求行CAS下 INVALIDATED，供业务页fresh superseding。窗口结束仍有未知效果时留待查，不以时间流逝推定无效果。
4. 为每个命令验证首调成功/失败、首次响应丢失、提交不明、领域读不可用、broker重投、双worker抢租、租约到期、epoch切换、五分钟窗口前后、原结果晚到、无结果但已有claim/观测、12次界限和N/N-1。验证独立身份的负向权限、零第二execution和零重复领域效果。

### 步骤 1～4 现状（2026-09-20）

步骤 1（分类）与步骤 2（独立身份、有界租约、领域结果权威）已交付；步骤 3 的零效果终态路径亦已交付：`68748d4bf` 只读零效果判定、`0954baeea` Serializable 终态 CAS（复用既有 `INVALIDATED` 边，新封闭码 `RECOVERY_ZERO_EFFECT_PROVEN`）、`4ee73a0a9` 由 Account 自己重读结果与效果两个 owner 权威的专用端点（`internal.approval-recovery.zero-effect`，授权目录 377→378；其 handler 级失败关闭矩阵由 `a8620cf66` 的 20 个子用例锁定）、`03e4d6df8` worker 接线（`InspectProofRecoveryLeasedWithRecovery`；CLI 自身不发 HTTP，两条受治理 POST 由 Account 客户端在存活租约内发起）、`162e56720` 触发边界单测。真实 PostgreSQL 证据：`d9195dbe1` 证明已收敛为 `EXECUTED` 的终端请求即使重放原候选与租约也被拒绝且状态事件数不变；`726843cfd` 补齐正向场景——真实批准 → relay 发布 → 专属 durable → 执行 `FAILED(DOMAIN_RESOURCE_NOT_FOUND)` → 窗口强制过期，在同一个存活租约下依次断言「效果 OBSERVED → 409 且请求零变更」「结果 SUCCEEDED → 409 且请求零变更」「ABSENT + UNCONFIRMED → 204 且落 `INVALIDATED` + `RECOVERY_ZERO_EFFECT_PROVEN` + 证据 + 一条状态事件」，并断言原执行行与投递状态行逐字节不变、仍只有一条执行、同证据重放冲突零新增、被失效的请求不再出现在恢复计划中；同一用例还在窗口仍开启时以「候选谎称窗口已过期」的伪造输入验证行级复检拒绝且零写入。

步骤 4 的验收矩阵现状（除最后两行外均由 `go test ./internal/account -run TestProofRecoveryPlanReadsOriginalExecutionWithoutReplayIntegration` 在真实 PostgreSQL 上执行，PASS 42.63s）：

| 验收项 | 现状 |
|---|---|
| 首调失败（确定性前置失败且零领域效果） | 真实 PG 已覆盖 |
| 原结果晚到（正向结果优先，绝不失效） | 真实 PG 已覆盖 |
| 领域读不可用 / 分类未知（一律留待查，拒绝且零写入） | 真实 PG 覆盖分类未知、终态、过期版本、错误令牌；端点单测覆盖结果权威不可用、效果权威不可用、未接线（结果/效果/仓储任一缺失）与非法租约 |
| 双 worker 抢租、租约到期、错误 token、释放退避 | 真实 PG 已覆盖（过期租约由单测覆盖） |
| 独立身份负向权限（NOLOGIN、无表 DML、无 `SECURITY DEFINER` 继承） | 真实 PG 已覆盖 |
| 零第二 execution 与零重复领域效果 | 真实 PG 已覆盖（执行行/投递行逐字节不变、仍一条执行） |
| 五分钟窗口前后 | 真实 PG 已覆盖：窗口过期后收敛，以及候选谎称窗口已过期（`WindowElapsed=true` 但行内 `execution_deadline` 仍在未来）时由事务内行级复检拒绝且零写入；候选自认窗口未过时由 CAS 守卫矩阵单测拒绝；"窗口结束仍有未知效果则留待查"由拒绝路径覆盖 |
| broker 重投、epoch 切换、Billing 12 次界限、N/N-1 | 未覆盖，需要 NATS 与多版本环境，归 GSA-39 生产等价演练 |
| 六命令领域结果权威的端到端查证链路 | 部分覆盖：权威读与闭集校验已有真实 PG/跨域证据（GSA-21～37），但恢复 worker 的调度与生产凭证仍未建立 |

因此 GSA-16 的**代码与实现合同项已全部交付**，独立审核已于 2026-09-20 完成（见下），仍未完成的是环境门禁（GSA-04、GSA-38～40）。

独立审核已于 2026-09-20 完成（`INDEPENDENT_REVIEW_PASS`）：审核方对零效果终态路径给出 `CHANGES_REQUESTED`，阻断项为「CAS 信任 worker 自报的失败分类，未在事务内从已锁定事实重算」，另含主机时钟与库内时钟不一致、本文件 `zero-effect` 端点的 OpenAPI 合同漏项、一处过时注释与两处 `gofmt` 未对齐；四项已由 `5cca2681d` 修复（含「伪造 `PERMANENT` 不得收敛 UNKNOWN 执行」的真实 PostgreSQL 负例），并在修复后的 HEAD 复核通过。

在步骤 4 的剩余验收项（broker 重投、epoch 切换、12 次界限、N/N-1 及生产等价演练）与 GSA-04/GSA-38～40 环境门禁完成之前，GSA-16 只能保持 `IN_PROGRESS / PARTIAL / ZERO_EFFECT_CAS_GATED / INDEPENDENT_REVIEW_PASS / DEPENDENCY_ENV_PENDING / NOT_DEPLOYED`；本状态不授权生产启用、调度恢复 worker、人工重放或跨 epoch 补发。
