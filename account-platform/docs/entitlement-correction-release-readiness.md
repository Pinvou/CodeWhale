# 权益组成纠错（`ENTITLEMENT_COMPONENT_ADJUSTMENT`）资源清单与只读 readiness

状态：`IN_PROGRESS / READONLY_READY / NOT_ACTIVATED / NOT_DEPLOYED`。本文是 GSA-38 面向本命令的只读交付：冻结激活/切换所需的资源清单、只读 readiness 检查、切换顺序与回滚边界。**本文不含任何写入或激活动作**；真正激活仍按[受控激活手册](../deploy/runbooks/entitlement-correction-activation.md)由授权人执行。

## 1. 资源清单（全部为仓库内可核对的路径）

| 类别 | 资源 | 位置 |
|---|---|---|
| 迁移 | Billing 准备列/只读纠错表 | `migrations/billing/000056_entitlement_component_correction_prepare.up.sql` |
| 迁移 | Billing 激活 v2（追加 INACTIVE 行，保留 v1 历史） | `migrations/billing/000057_entitlement_component_correction_activation_v2.up.sql` |
| 迁移 | Billing 成对账本执行 | `migrations/billing/000058_entitlement_component_correction_effect.up.sql` |
| 迁移 | Billing proof claim 命令白名单放宽（前向） | `migrations/billing/000059_entitlement_component_adjustment_proof_claims.up.sql` |
| 迁移 | Account policy v2 元数据 | `migrations/account/000069_entitlement_component_adjustment_policy_v2.up.sql` |
| 迁移 | Account 专属 durable 路由（proof-era only） | `migrations/account/000070_entitlement_correction_consumer_route.up.sql` |
| 注册表 | Account 静态 executable 集合（实时 activation 门禁） | `internal/platform/approvalpolicy/registry.go` |
| 注册表 | 专属 durable 描述（Billing workload / policy v2 / Q+1） | `internal/platform/approvalconsumer/registry.go` |
| 入口 | Billing 消费者启动（显式开关 + 受信收据） | `cmd/billing-service/main.go`、`cmd/billing-service/approval_consumer.go` |
| 事件 | `account.approval-command.authorized.v1` 消费入口与权限 | `deploy/authorization-entrypoints.json` |
| 合同 | Billing activation/ proof 投影 | `contracts/openapi/billing.yaml`、`test/contract/billing-approval-activation.test.mjs` |
| 运行时 | proof/claim/执行/消费者映射 | `internal/billing/approval_proof*.go`、`entitlement_correction_*.go`、`approval_delivery*.go` |
| Presenter | 只读闭合详情 | `internal/account/approval_presenter.go`（`approvalEntitlementCorrectionSummary`） |
| Runbook | 受控激活与回滚 | `deploy/runbooks/entitlement-correction-activation.md` |
| 迁移清单 | 规范化 SHA-256 清单（330 项） | `migrations/manifest.json` |

## 2. 只读 readiness 检查（全部只读，输出可归档）

1. **迁移账本**：`node scripts/account-platform-migration-manifest.mjs check` 通过；目标库 `schema_migrations` 已应用到 000059（Billing）与 000070（Account）。
2. **Billing activation**：`billing_approval_command_activations` 中该命令存在 `(2,2,'INACTIVE')` 行且 v1 `(1,1,'INACTIVE')` 仍在；`GET /internal/v1/approval-command-activations` 返回该组合且 `state=INACTIVE`，其余命令状态不变。
3. **Account 入口关闭**：以服务身份对 create/update/submit/APPROVE/Begin 各调用一次，均应返回 `ErrBillingGovernanceInactive`（503 `BILLING_COMMAND_GOVERNANCE_INACTIVE`），且不产生 decision/outbox/execution 行。
4. **Proof 不可用**：以真实 Account 身份请求 Billing proof，返回 `BILLING_APPROVAL_PROOF_UNAVAILABLE`；`billing_approval_proof_claims` 为空。
5. **消费者未启动**：Billing 服务日志不含 `BILLING_ENTITLEMENT_CORRECTION_CONSUMER_NOT_PROVISIONED` 之外的本命令相关启动记录；受信 cutover 收据的 `billing_starts` 尚未包含该 durable 的 Q+1 起始序（GSA-38 收据/清单为单一真相源）。
6. **零效果基线**：`entitlement_component_corrections` 为空，`receivable_allocations` 无 `APPROVED_WRITE_OFF`，相关 package/component 未被改动。
7. **Presenter**：以一条该命令的历史/演练请求检查审批详情可用性；`DETAIL_PRESENTATION_UNAVAILABLE` 只在目标畸形或版本未知时出现。

以上每步都是读取或失败关闭调用；任何一步出现写入、状态推进或意外 200，都应视为 NO-GO 并停止。

## 3. 切换顺序（激活时使用，本文不执行）

1. Billing：应用 000059 并保持 activation 行 `INACTIVE`（本步只放宽 claim 白名单）。
2. Billing：在受信收据/清单中加入该 durable 的 Q+1 起始序，并以 `BILLING_ENTITLEMENT_CONSUMER_ENABLED=true` 启动消费者；缺失或不受信绑定必须启动失败关闭。
3. Account：确认 policy v2 行与注册表一致、四入口仍因 Billing `INACTIVE` 失败关闭。
4. Portal：仅当 Billing 报告 `ACTIVE` 时目录/业务入口才 requestable（目录真相以 Billing activation 为准）。
5. 按受控激活手册把 v2 行翻为 `ACTIVE`（version+1，history 追加），并完成手册第 4 节的五项验证。

## 4. 回滚边界

- 应用回滚只允许：停止消费者 → 把 v2 行翻回 `INACTIVE`（version+1）→ 验证四入口重新失败关闭、proof 重新不可用。
- **不做破坏性数据库回滚**：correction case、journal/posting、component/lot、receivable 与支付事实一律保留；000059 的 down 为 forward-only 55000。
- 仍在 5 分钟窗口内的 `APPROVED` 申请按 GSA-16 失败分类收敛，不得盲重放。

## 5. 本切片不做的事

- 不新增/修改迁移、Go/Node 代码、合同或 activation；不执行任何激活、切换或部署动作。
- 不替代受控激活手册中的授权、备份与证据归档要求。

## 6. 记录

- 2026-09-18 代理：登记资源清单、只读 readiness、切换顺序与回滚边界；未执行任何激活。
