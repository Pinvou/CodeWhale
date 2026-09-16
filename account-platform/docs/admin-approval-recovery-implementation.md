# GSA-16 六命令失败恢复实现合同

状态：实现中；本文不授权生产启用、人工重放或跨 epoch 补发。以 [治理与安全整改 §10](admin-governance-security-redesign.md) 为需求依据。

## 当前安全基线

- Account `000065` 将 proof-era Billing 确定失败保留为 `DOMAIN_INFLIGHT`；`000066` 栅栏化旧版本留下的 `READY + last_error_code`；`000067` 进一步把 `READY + 已有attempt`（即使错误码丢失）迁入只查证态，并使直接 grant 对 proof-era 仅允许第 0 次到第 1 次。Billing consumer 本地也拒绝第二次 Begin/Grant。legacy Billing 的既有 12 次上限不变。
- `EXECUTION_FAILED` 不由通用过期 worker 直接标记 INVALIDATED；已有执行记录的结果必须先经领域权威查证。未 Begin 的 APPROVED 仍按原五分钟执行窗口过期。
- Account `PlanProofApprovalRecovery` 已提供最多200条同一只读快照的原request/execution/event/durable、原幂等键、投递/执行状态、epoch、截止时间及分类；legacy不进入列表。它不取租约、不读取领域效果、不改变状态，也不暴露为Portal接口。生产恢复worker、独立身份和领域结果权威仍未接入，不可把这份计划当作完成恢复的证据。
- `InspectProofRecovery` 在30秒总预算内对每个closed候选仅调用原Billing/Token效果观测；`ABSENT`、`EFFECT_OR_ATTEMPT_OBSERVED`、不可用都输出 `UNKNOWN_QUERY_ONLY`。它不查询原命令的权威终态，也不授予零效果证明；只读效果观测不能替代逐命令领域结果查证或独立持久租约。
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

在步骤1～4全部完成及GSA-04/GSA-38～40环境门禁之前，GSA-16只能保持 `IN_PROGRESS / PARTIAL / NOT_DEPLOYED`。
