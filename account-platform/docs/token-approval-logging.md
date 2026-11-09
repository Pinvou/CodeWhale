# Token 审批消费者日志

`token-command-executor` 配置进程安全日志后，以下失败信号以固定 JSON `event` 与 `error_code` 输出：

| event | error_code | 含义 |
|---|---|---|
| `token_approval_consumer_stopped` | `CONSUMER_STOPPED` | 消费者意外返回错误；主程序取消应用上下文，结束时以非零状态退出 |
| `token_approval_authorization_delivery_pending` | `DELIVERY_PENDING` | 本次授权投递处理没有完成；可能包含 ACK 已确认而终态登记尚未完成的情况 |
| `token_approval_authorization_nak_failed` | `NAK_FAILED` | 请求延迟重投递的 NAK 调用失败 |

这些是脱敏的处理阶段类别，不包含原始错误、消息正文、凭证或资源标识。普通标准日志仍折叠为 `legacy_standard_log`，未知事件仍折叠为 `application_event`。不能从这些信号推断资金失败或成功，也不能据此释放资金、重做领域操作或手工修改投递终态。

本次不改变 ACK/NAK 选择、重投递延迟、20 秒处理超时、消费者治理及现有恢复边界；ACK 后终态扫尾等其他 #1644 待办仍需单独处理。日志回归使用真实安全归一器、真实消费者和受控 HTTP 对端；停止回归执行主程序使用的退出函数及进程日志配置，不代表完整 NATS、数据库或供应商现场验收。
