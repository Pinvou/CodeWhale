# Token 审批消费者的执行入口

`cmd/token-command-executor` 通过 `RunTokenApprovalConsumers` 注册 `TokenAuthorizationCommandConsumer.Handle`。该入口核对路由、冻结投递与绑定，使用既有状态机执行、恢复和确认消息。`TokenApprovalConsumer` 保留共享的 HTTP 配置、请求、严格解码与事实比较能力，旧的未注册 `Handle(events.Event)` 及其专属告警回调已移除。

原旧路径测试的七类保护在当前消费者中覆盖：

| 原场景 | 当前回归 |
|---|---|
| 成功及重复投递 | `TestTokenAuthorizationRegisteredConsumerSafety/success_duplicate`：领域写入、complete、result、terminal 各一次，两次投递均 ACK |
| 领域响应丢失 | 原 `TestTokenAuthorizationRawConsumerCrashRecoveryAndUnlimitedNAK` 的 `execute-response-lost`、`execute-unknown`：查询原结果或保持未确认，不重发副作用 |
| Begin 响应丢失 | 同一原回归的 `begin-response-lost`：保持原领取事实，不再次派发领域操作 |
| SAFE | 新回归 `safe`：可进行只读路由/绑定/投递状态核查，不读取 Token 领域结果、不 Begin、不执行；未决消息 NAK |
| 非法 lease | 新回归 `invalid_lease`：不执行、不完成、不 ACK；补充核对稳定且脱敏的内部诊断 |
| JSONB 编码及冻结字节 | 新回归 `jsonb_whitespace`：受控格式变体与紧凑 HTTP 事实语义相同，消费过程保留所有实际交给消费者的原始字节副本 |
| 大整数版本差异 | 新回归 `large_integer_equal` / `large_integer_lease_mismatch`：精确保留相等或相邻的 int64 事实；原 JSON 比较及严格传输回归继续保留 |

这些用例经真实 codec、当前消费者和受控 HTTP 对端执行；格式变体用于解码与不可变输入的防御性合同验证，不代表生产 publisher 使用缩进格式。重投递保留原 stream sequence，仅增加投递次数。现代状态机的终态重放直接 ACK，不能沿用旧死路径重复 complete 的次数作为期望。生产认证、20 秒处理预算、治理代次、领取、ACK/NAK 与恢复分支未改变；本项没有新增数据库或 NATS 现场验收。
