# 财务修复家族切换工具、资源清单与只读 readiness（GSA-38／39／40 工作底稿）

状态：`IN_PROGRESS / READONLY_READY / NOT_ACTIVATED / NOT_DEPLOYED`。本文件是七个财务修复命令的**只读**切换前置清单与证据计划；它不执行激活、不部署、不写入任何事实。所有 activation 行当前均为 `INACTIVE`。

## 1. 家族范围与资源清单（单一真相源）

| 命令 | 专属 durable | 默认关闭开关 | Billing 迁移 | Account 迁移 | 激活手册 |
|---|---|---|---|---|---|
| `LEDGER_REPAIR` | `billing-ledger-repair-executor-p1-v1` | `BILLING_LEDGER_REPAIR_CONSUMER_ENABLED` | `000061` | `000071` | `deploy/runbooks/ledger-repair-activation.md` |
| `RECEIVABLE_WRITE_OFF` | `billing-receivable-write-off-executor-p1-v1` | `BILLING_RECEIVABLE_WRITE_OFF_CONSUMER_ENABLED` | `000063`/`000064` | `000072` | `deploy/runbooks/receivable-write-off-activation.md` |
| `PAYMENT_FINANCIAL_REPAIR` | `billing-payment-financial-repair-executor-p1-v1` | `BILLING_PAYMENT_FINANCIAL_REPAIR_CONSUMER_ENABLED` | `000065`/`000066` | `000073` | `deploy/runbooks/payment-financial-repair-activation.md` |
| `REFUND_EXCEPTION_REPAIR` | `billing-refund-exception-repair-executor-p1-v1` | `BILLING_REFUND_EXCEPTION_REPAIR_CONSUMER_ENABLED` | `000067`/`000068`/`000069` | `000074` | `deploy/runbooks/refund-exception-repair-activation.md` |
| `CHARGEBACK_FINANCIAL_REPAIR` | `billing-chargeback-financial-repair-executor-p1-v1` | `BILLING_CHARGEBACK_FINANCIAL_REPAIR_CONSUMER_ENABLED` | `000070`/`000071` | `000075` | `deploy/runbooks/chargeback-financial-repair-activation.md` |
| `RECONCILIATION_FINANCIAL_REPAIR` | `billing-reconciliation-financial-repair-executor-p1-v1` | `BILLING_RECONCILIATION_FINANCIAL_REPAIR_CONSUMER_ENABLED` | `000072`/`000073` | `000076` | `deploy/runbooks/reconciliation-financial-repair-activation.md` |
| `USAGE_SETTLEMENT_REPAIR` | `billing-usage-settlement-repair-executor-p1-v1` | `BILLING_USAGE_SETTLEMENT_REPAIR_CONSUMER_ENABLED` | `000074`/`000075`/`000076` | `000077` | `deploy/runbooks/usage-settlement-repair-activation.md` |

共同资源：

- 受信收据输入（六个一组，专属 durable 共用）：`BILLING_APPROVAL_CONSUMER_{PUBLIC_KEYS,MANIFEST,RECEIPT}_{FILE,SHA256}`；由 `cmd/billing-service/approval_consumer.go` 经 `events.ApprovalConsumerResourceFiles.Load` 校验。任一缺项、摘要不符或读取失败都以 `<SWITCH>_CONSUMER_RESOURCES_UNAVAILABLE` 启动失败关闭。
- 事件入口与权限：`deploy/authorization-entrypoints.json` 中每个 durable 一条 `account.approval-command.authorized.v1` 条目，权限 `internal.event.consume.account-approval-command-authorized`；授权覆盖门禁当前为 378 HTTP / 28 task / 15 event / 23 CLI（GSA-37 补充两个只读预检端点后由 376 增至 378，2026-09-20 复核 `node scripts/check-account-platform-authorization-coverage.mjs` exit 0）。
- 只读预检接口面（业务页构造草稿的唯一入口）：Billing `POST /internal/v1/approval-command-prechecks`（Account 服务身份、复用 `internal.billing.approval-proof.read`）与 Account 代理 `POST /admin/v2/approval-command-prechecks`（复用 `platform.billing.repairs.request`）。两者都只读、不缓存、不落库，且与 proof 输入共用同一闭合装配路径。
- 注册表闭合：`internal/platform/approvalconsumer/registry.go`（durable/命令/工作负载/起始序）、`internal/platform/approvalpolicy/registry.go`（Account 四入口可执行集合）、`contracts/openapi/{billing,account}.yaml`（激活投影与 `canonical_durable` 枚举）。
- 迁移清单：`migrations/manifest.json`（当前 378 项，`account-platform-migration-manifest.mjs check` 必须通过）。

## 2. 只读 readiness 检查（每个命令逐项执行，任一步出现写入或状态推进即 NO-GO）

1. **迁移账本**：`node scripts/account-platform-migration-manifest.mjs check` 通过，且目标环境 Billing/Account 迁移版本不低于 §1 表内版本。
2. **activation 行**：`SELECT ... FROM billing_approval_command_activations WHERE command_type=$(cmd)` 返回唯一 v1/s1 行且 `state='INACTIVE'`；投影端点返回全部 11 行、顺序与版本与合同一致。
3. **Account 四入口失败关闭**：以真实服务身份调用 create/update/submit/APPROVE，全部返回治理关闭错误且零副作用。
4. **proof 不可用**：真实 Account 身份调用 Billing proof 端点，命令未激活时返回不可用且**不产生** proof claim。
5. **消费者未启动**：durable 未注册、Q+1 受信收据未包含该 durable，DLQ/重试计数为 0；开关未开启。
6. **零效果基线**：按各手册 §2 的存量清单记录基线计数（该命令的 case 表、`billing_approval_proof_claims`、`journal_entries`/`ledger_postings`/`receivables` 计数）。
7. **presenter 可用性**：以真实冻结投影构造的请求在 `approvalV2Summary` 下 `AVAILABLE`，字段闭合、无 Provider 原文与个人信息。

## 3. 切换顺序（每个命令独立执行一次）

1. 先放宽 Billing 侧 claim 白名单（迁移，已随实现交付）。
2. 再配置受信收据并开启该命令的专属消费者开关（只影响该 durable）。
3. 再部署 Account（closed wire／门禁／proof 路由／presenter／recovery 与只读路由迁移已就位）。
4. 最后按各命令手册 §3 由持有 `billing_schema_owner` 的部署身份做一次单行 `version+1` 激活。

Portal 业务入口（GSA-37）在对应命令激活后逐命令开放，不得一次性放开全部命令。

## 4. 回滚边界

- 只允许「停止该 durable 消费者 → 把该命令 v1/s1 行翻回 `INACTIVE`（`version+1`）」；history 追加而非改写。
- 既有 case/journal/应收/回填与状态推进一律保留可读；禁止破坏性 down（本家族全部 down 迁移均为 forward-only），禁止把已结案的案件或复核退回旧状态。
- 5 分钟执行窗口内的 `APPROVED` 申请按 GSA-16 失败分类收殓；禁止通用人工 replay。

## 5. GSA-39 生产等价集成、性能、N/N-1 与故障演练（需生产环境访问）

本机已交付的证据：七个命令各自在**真实 PostgreSQL + 真实 HTTP**（真 Account 库 + 真 Billing 库 + 真实 proof/activation 端点 + 真实 relay + 专属 durable 消费者）上的跨域 E2E、真库整包回归与迁移链断言；全部 activation 行保持 `INACTIVE`、Provider 调用计数为 0、幂等重放零新增。

仍需在生产等价环境执行（未执行前不得宣称完成）：

1. **N/N-1**：先部署可读新表/新取值的 Billing，再部署 proof 端与执行器/消费者，最后业务页；上一次版本在遇到新状态取值（如 `usage_loss_reviews.RESOLVED_SETTLED`）时必须失败关闭而非误判。
2. **性能**：proof 端点 2 秒绝对期限下的 P99；消费者在单命令 5 分钟窗口内的端到端时延；批量授权事件下的 ACK 行为。
3. **故障演练**：消费者宕机/重启、relay 中断、数据库序列化冲突、12 次尝试耗尽后的 DLQ 与 PubAck/`ACK_PENDING`/DoubleAck 收口；每个命令至少一次。
4. **提权边界复核**：以最小权限身份复核 `billing_schema_owner` 仅用于激活与迁移，运行期不持有 CREATE/OWNER。

## 6. GSA-40 授权后的逐命令灰度与证据归档（需生产环境访问）

1. 逐命令取得明确授权（书面、含时间与范围），一次只开一个命令；首版范围按各设计文档（退款异常＝Provider 已退款未冲销；拒付＝BASE 短缺；对账＝零账务效果结案；用量结算＝仅 `LOSS_REVIEW` 未结算补记）。
2. 每命令一次受控演练并归档：真实 ID、审批/授权事件 ID、迁移版本、激活前后 activation 投影、proof 结论、journal/应收/案件/复核结果、幂等重放证据、Provider/adapter 调用计数为 0 的观测。
3. 证据脱敏（不含 Secret/Token/Cookie/个人信息与 Provider 原文），按命令独立归档；未通过前置门禁的命令不得开启。

## 7. 记录

- 2026-09-20 代理：建立家族资源清单与只读 readiness 工作底稿；未执行任何激活、部署或写入。
