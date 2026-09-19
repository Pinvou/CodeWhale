# 财务修复只读预检接口面提案（GSA-37 剩余工作的决策材料）

状态：`PROPOSAL / AWAITING_SCOPE_DECISION / NOT_IMPLEMENTED`。本文件不是已冻结设计，也未被实现；它把 GSA-37 目前唯一缺失的跨域接口面写成可评审的方案，供授权人决定是否开这条路线。

## 1. 为什么缺它

- Portal 侧闭合草稿构造器已交付（`pinvou3-app/src/features/account-platform/admin-financial-repair-drafts.js`）：它只接受「服务端只读预检投影」，并据此重建闭合 `normalized_command`、逐字复制 critical/financial。
- 但七个财务修复命令的闭合投影目前**只在 Billing 进程内**由 proof 输入装配（`LedgerRepairProofInput`／`ReceivableWriteOffProofInput`／`PaymentFinancialRepairProofInput`／`RefundExceptionRepairProofInput`／`ChargebackFinancialRepairProofInput`／`ReconciliationFinancialRepairProofInput`／`UsageSettlementRepairProofInput`），**没有任何 HTTP 端点**把它交给 Portal。
- 同类只读投影在 Token 域已有先例（`/token/admin/v1/... /impact/...` 由 Portal 用于构造高风险草稿），Billing 域亦有 `/internal/v1/pricing/impact-coverage`。因此这条路线与既有架构一致，但对本家族是**新增接口面**。

## 2. 方案 A（推荐）：Billing 只读预检 + Account 代理

1. **Billing**：新增 `POST /internal/v1/approval-command-prechecks`（只读，2 秒绝对期限、64 KiB 上限）。请求体为封闭对象：`command_type`、`target_resource_id` 及该命令要求的证据引用（与 §3 表一致）；响应体就是现有 `LegacyProofInput` 的三段投影（normalized/critical/financial）加 `command_type`／`policy_version`／`snapshot_schema_version`／`target_resource_type`／`target_resource_id`。**绝不返回** Provider 原文、提示词、个人数据；不写任何事实、不产生 proof claim。
2. **权限**：需要一条新的内部工作负载权限（例如 `internal.billing.approval-precheck.read`），只授予 Account 服务身份；不得复用 proof 或 activation 的权限字符串，避免把「读预检」与「出证明」的授权面混在一起。
3. **Account**：新增 `POST /admin/v2/approval-command-prechecks` 代理端点，权限复用各命令既有的 `platform.billing.repairs.request`，只做转发与响应闭合校验，不缓存、不落库。
4. **门禁**：`deploy/authorization-entrypoints.json` 需新增两条路由条目（Billing 内部 + Account 管理端），授权覆盖门禁计数随之上升；`contracts/openapi/{billing,account}.yaml` 需新增 path 与 schema；Node 合同需覆盖「未知命令／版本不符／目标不符／证据缺失一律拒绝且零写入」。

## 3. 各命令的预检输入（与现有 proof 输入一一对应）

| 命令 | 预检输入 | 目标资源 |
|---|---|---|
| `LEDGER_REPAIR` | `target_journal_entry_id`＋`target_posting_id` | `LEDGER_POSTING` |
| `RECEIVABLE_WRITE_OFF` | `receivable_id` | `RECEIVABLE` |
| `PAYMENT_FINANCIAL_REPAIR` | `payment_order_id`＋`payment_evidence_id` | `PAYMENT_ORDER` |
| `REFUND_EXCEPTION_REPAIR` | `refund_id`＋`payment_evidence_id`＋`anomaly_case_id` | `REFUND` |
| `CHARGEBACK_FINANCIAL_REPAIR` | `chargeback_case_id`＋`payment_evidence_id` | `CHARGEBACK_CASE` |
| `RECONCILIATION_FINANCIAL_REPAIR` | `reconciliation_case_id`＋`payment_evidence_id`＋`business_date` | `RECONCILIATION_CASE` |
| `USAGE_SETTLEMENT_REPAIR` | `usage_request_id`＋`hold_id`＋`loss_review_id`（`evidence_kind=LOSS_REVIEW`） | `USAGE_HOLD` |

预检与 proof／执行必须使用**同一** owner 读路径与同一闭合校验，任何不一致都要在预检阶段就返回「不可用」，而不是让 Portal 拿到一个稍后必然被拒绝的投影。

## 4. 方案 B（不新增接口面）：联调环境手工装配

Portal 表单只做渲染与提交，草稿投影由运维/联调人员用 Billing 侧脚本或测试夹具生成后粘贴进表单。优点是零新增权限与路由；缺点是业务页无法自助使用，且「人工粘贴投影」本身是新的操作风险面（需要额外的校验与审计），不满足「用户可使用的完整闭环」的社区版要求。因此仅作为过渡，不建议作为终态。

## 5. 方案 C（不推荐）：Portal 直连 Billing 内部端点

让 Portal 直接携带服务身份调用 Billing 会绕过 Account 的权限与审计边界，且 Portal 不应持有内部工作负载凭据。拒绝。

## 6. 决策后的实施清单（方案 A）

1. Billing：只读预检 handler + 权限校验 + 与 proof 输入共享的闭合装配函数 + 真实 PG 定向用例（含「预检不写入任何事实」断言）。
2. 合同与门禁：OpenAPI（billing/account）、authorization-entrypoints 两条、授权覆盖门禁计数、Node 合同（拒绝矩阵）。
3. Account：代理端点 + 既有 repairs 权限校验 + 只读响应校验（复用 `approvalProofReasonAllowed` 风格的闭集）。
4. Portal：`admin-financial-repair-drafts.js` 的调用方（表单与草稿编辑 UI）＋三语文案（`pinvou3-app/src/shared/i18n.js`）＋ Node 合同。
5. 真实浏览器联调：需要至少一个命令在生产等价环境激活后执行；在此之前只能做静态与合同级验证。

## 7. 记录

- 2026-09-20 代理：登记本提案，等待范围决策；未实现、未新增权限、未新增路由。

## 8. 方案 A 的落地勘测（2026-09-20 补充，已核实路径）

### 8.1 Account 代理端点的确切登记模板（已核实，下一片可直接照抄）

`deploy/authorization-entrypoints.json` 的 `http_policies` 每条形如
（示例取自既有 `POST /approval-requests`）：

```json
{ "source": "account-platform/internal/account/service.go", "receiver": "admin", "method": "POST",
  "path": "/approval-command-prechecks", "scope": "platform", "strategy": "domain.approval-policy-v1" }
```

要点：`source` 必须是路由实际注册所在的 Go 文件；`receiver` 为 `admin`（管理端 group）；`path` 只写 group
之后的相对路径；`scope: platform` 时覆盖门禁要求 `permissions` 中每个权限以 `platform.` 开头（本命令复用
`platform.billing.repairs.request`）；`strategy` 必须匹配 `^(protocol|authenticated|domain|workload)\.[a-z0-9][a-z0-9.-]*$`。
新增该条目后，`node scripts/check-account-platform-authorization-coverage.mjs` 的 HTTP 路由数从 **375 增至 376**，
`test/contract/authorization-coverage-release-gate.test.mjs` 里的精确断言必须同步为 376，否则合同必红。

- **可复用的装配入口已就绪**：七个命令的 proof 输入方法签名都很窄且无需新参数，
  `LedgerRepairProofInput(target, reasonCode)`、`ReceivableWriteOffProofInput(target)`、
  `PaymentFinancialRepairProofInput(target)`、`RefundExceptionRepairProofInput(target)`、
  `ChargebackFinancialRepairProofInput(target)`、`ReconciliationFinancialRepairProofInput(target)`、
  `UsageSettlementRepairProofInput(target)`（`internal/billing/*_repair*.go`）。端点只需把这些输入原样透传。
- **新增权限的代价比预估更大**：内部权限不是集中在单个常量表，而是同时出现在
  `deploy/helm/account-platform/templates/app-deployments.yaml`（`WORKLOAD_IDENTITY_RULES_JSON` 的 JSON-in-JSON）、
  `deploy/compose/compose.model-token.yaml`（同一 JSON 的字符串形式）、
  `internal/billing/approval_proof_test.go` 与 `internal/account/*_cross_domain_integration_test.go` 的环境夹具，
  以及 `test/contract/model-token-deployment.test.mjs` 的**精确权限列表断言**。新增 `internal.billing.approval-precheck.read`
  必须一次性同步这五处，否则对应合同/门禁必然红。
- **因此的落地建议（二选一，需授权人确认）**：
  (A1) 严格按 §2 新增独立只读权限，代价为上述五处部署清单与精确列表同步；
  (A2) 预检端点在 Billing 侧复用既有 `internal.billing.approval-proof.read`（Account 服务身份本就持有、且同为只读投影），
  仅新增路由与 `deploy/authorization-entrypoints.json` 条目，避免改部署清单与权限列表——治理上稍弱（读预检与出证明共用一个权限），
  但改动面小得多。两者都需要再补 Account 代理端点与 Portal 表单/三语文案才能形成用户可用的闭环。
