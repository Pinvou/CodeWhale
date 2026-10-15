# Chat capture 登记预算

Chat capture 的内存登记表最多保留 64 条。此预算独立于非终态任务的 64 条限制：任务已提交终态后，成果 completion worker 仍可能保留扫描基线。

同 lease/session/task/sequence 的重复 begin 不新增登记，满额时仍保持幂等。新增第 65 条在同一 captures mutex 的检查与插入临界区被拒绝，返回固定 `artifact_capture_capacity`；不会淘汰旧 owner、取消运行任务或删除 partial、历史文件及回执。匹配 owner 正常释放后，新合法任务可获得容量，旧任务不会被重新派发。

这是 capture 登记表的条目界，不是所有 `spawn_blocking` 队列、扫描中间结果或后台资源的总预算。既有每次扫描的文件数/字节限制、权限、任务事实、连接 fence 和精确清理仍独立适用。本点不新增环境变量或可配置接口。

真实模块回归覆盖 literal 64/65、满额幂等、拒绝不改变其他登记或文件、旧 sequence 清理不释放新 owner、精确释放后恢复。验证状态以当前 PR 的实际日志为准；源码中有测试不代表已执行。

生产 ready 处理通过窄的 `finish_admitted_task_ready` 边界：准备失败时先调用原有可失败的 `fail_admitted`，成功后才移交 admission cleanup，随后读取权威状态。落盘或状态读取错误继续返回错误，不伪造成功。容量拒绝沿原来的 `artifact_capture_failed` 任务终态原因返回，不进入后续 dispatch。

新增回归把真实 capture 满额拒绝送入该生产边界，使用实际持久化任务服务，检查失败回执重开后仍保留、live count 恢复为零、原 64 个 capture owner 和 partial 文件不变，以及新的合法 admission 可继续。它是生产模块边界测试，不是完整 AppHandle、模型调用或双机端到端验收；不会据此宣称所有后台队列有界。
