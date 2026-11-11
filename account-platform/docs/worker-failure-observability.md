# Worker 失败观测

Commerce 客户同步及 Account、Billing、Token 人工审核 SLA worker 通过服务默认安全日志输出下列有限事件。`event`、`error_code`、服务身份和时间用于识别失败操作；错误结果为 `failure`。不记录供应商/数据库原始错误、手机号、主体、任务内容或凭证。

| 事件 | 错误码 | 运行位置 |
| --- | --- | --- |
| `commerce_customer_sync_claim_failed` | `COMMERCE_SYNC_CLAIM_FAILED` | Account：领取同步任务失败 |
| `commerce_customer_sync_retry_failed` | `COMMERCE_SYNC_RETRY_FAILED` | Account：供应商调用失败后的重试持久化失败 |
| `commerce_customer_sync_complete_failed` | `COMMERCE_SYNC_COMPLETE_FAILED` | Account：供应商成功后的完成持久化失败 |
| `manual_review_sync_failed` | `MANUAL_REVIEW_SYNC_FAILED` | Account、Token：同步人工审核案件失败 |
| `manual_review_escalation_failed` | `MANUAL_REVIEW_ESCALATION_FAILED` | 三域：人工案件 SLA 升级失败 |
| `payment_stop_loss_evaluation_failed` | `PAYMENT_STOP_LOSS_EVALUATION_FAILED` | Billing：付款止损评估失败 |
| `provider_stop_loss_evaluation_failed` | `PROVIDER_STOP_LOSS_EVALUATION_FAILED` | Billing：供应商止损评估失败 |
| `archive_blind_review_escalation_failed` | `ARCHIVE_BLIND_REVIEW_ESCALATION_FAILED` | Billing：归档盲区案件升级失败 |

正常停机时，只有操作返回与父上下文取消相符的错误（包括包装错误）才不新增失败日志；同时发生的其他存储错误，以及父上下文仍有效时依赖自身返回的取消错误，继续记录。已领取 Commerce 任务的 Retry/Complete 仍用原 `WithoutCancel` 上下文；即使父上下文已取消，真实持久化失败仍会被观察。日志写入失败不改变原独立步骤及下一轮继续执行。

事件不是业务成功或失败终态凭证。Commerce 原 Provider/主体标识、30 秒租约、退避及失败次数策略保持；人工审核原启动调度、100 条批量、值班安排、止损和升级次序保持。供应商成功但本地完成未确认时，保留原幂等身份及已有恢复路径。

本叶只补充运行失败日志。供应商成功后未知完成的权威人工处置、12 次后 `REPAIR_REQUIRED` 的权限与审计合同，以及容量/留存等其他条目，继续由 [#1632](https://github.com/Pinvou/pinvou3/issues/1632) 跟踪，未据本叶宣称异常恢复或真实站点验收完成。
