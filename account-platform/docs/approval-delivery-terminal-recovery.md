# 已确认审批投递的终态扫尾

Token 和 Billing 消费者先确认领域结果、持久化 `RESULT_CONFIRMED`，再确认消息 ACK、登记 `TERMINAL_CONFIRMED`。ACK 成功而最后登记失败时，不能依赖消息再次投递来补元数据。

Account 服务的 `configureApprovals` 在 personal/models 两种模式启动 `approval-delivery-terminal-recovery`，启动立即运行，随后每分钟一轮，每轮最多 100 项、10 秒预算。应用前需先用原迁移入口安装 Account `900016_approval_delivery_terminal_recovery`。函数仅允许真实 `account_service` 登录调用，运行角色仍无私有投递表权限。

扫尾只选择当前注册映射及序列化版本允许的 `RESULT_CONFIRMED`，同事务锁定并推进 state、version、updated_at。行锁使用 SKIP LOCKED，其他实例或消费者已持锁的行留待后续轮次；批次 SQL 错误整体回滚，修复依赖后下一轮重试。它不领取领域执行权、不调用执行器、不改变审批或领域结果。READY、DOMAIN_INFLIGHT、DLQ_PUBLISHED_ACK_PENDING 均保留，未知结果不因等待时间自动收尾。

终态登记不构成 broker ACK 证据：扫尾也可能先于 ACK。消费者读到终态后仍只确认消息，不重走领域执行。消费者已持有旧 RESULT 版本时，原 terminal 严格 CAS 可能被扫尾抢先；Account 仅在该精确 CAS 拒绝后只读相同绑定、确认终态且版本前进，作为幂等成功返回。权限、取消、数据库故障不会因此变成成功。

失败通过固定安全事件 `approval_delivery_terminal_recovery_failed`、错误码 `APPROVAL_DELIVERY_TERMINAL_RECOVERY_FAILED` 可定位；日志不含原错误、事件身份、命令内容或凭据。整批持续存储故障会持续保留原状态并记录失败，不保证损坏行在故障未解除前仍能推进同批其他行。每批上限不代表生产吞吐或容量验收。

回滚先停止新版本 worker，再使用原 Account DownOne。回滚只删除本次函数及精确 RESULT 部分索引，保留已经登记的终态与全部冻结证据；原消费者终态接口继续存在。升级旧二进制后仍可读取已登记终态。

验证入口：原金融 PostgreSQL runner 必须执行 `TestApprovalDeliveryTerminalRecoveryPostgres` 及七个精确子例；原 Go Token 测试包含 `TestTokenAuthorizationACKSuccessTerminalWriteFailure`，覆盖两种兼容消息格式。独立 PostgreSQL fixture 使用公开迁移、真实审批 writer/冻结 relay/运行角色，外部 proof、Token 领域回执与 broker ACK 为受控边界；这些证据不等于生产 NATS、真实 Token 副作用或线上发布验收。DLQ 的排除由精确 SQL 状态谓词保证，不把现代单次执行夹具伪装成历史十二次重试 DLQ 生产者。
