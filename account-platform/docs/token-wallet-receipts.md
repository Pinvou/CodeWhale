# Token 开户钱包回执恢复

Token `TOKEN_ACCOUNT_CREATE` 的开户 worker 与人工恢复使用原来的 POST `/internal/v1/token-account-wallet-provisioning-receipts` 和 GET `/:command_reference`。供方回执绑定原 command reference、owner 类型/ID 和 SHA-256 request digest；相同 reference 不能更换 owner 或 digest。生产 Token workload 的 execute/read 两个已有权限分别保护写入和查询，普通钱包 ensure 权限不能使用此协议。

Billing 在一笔事务内使用现有 owner 创建锁及钱包行锁，复用当前组织生命周期与已删除节点保护，创建零余额钱包、唯一负债科目和不可变成功回执。它不创建资金分录、授予或账户身份。新命令遇到冻结/关闭钱包或待删除节点时失败并回滚，不能恢复钱包；已提交原成功回执则按历史结果重放，不改历史回执或当前终态。Token 仍需执行自己的 owner authority 与命令围栏校验。

POST 回执丢失时应先用同一 reference 查询，404 只表示还没有已提交回执。查询不能新建钱包或替换 reference。现有 Token 客户端严格读取供方 DTO；PENDING/FAILED/UNKNOWN 等原历史状态仍可读取，供方不为数据库/网络异常编造确定失败回执。

新增 Billing `900023_token_account_wallet_receipts`，保留全部既有迁移字节。新库由 schema owner 创建表并给 runtime 仅 SELECT/INSERT；已有原053回执必须精确匹配原结构、约束、不可变触发器与隔离的 owner/ACL，保留原 owner 和全部事实，不 DROP、不覆盖。真正原053的 LF/CRLF checksum 是明确兼容历史；专用 Billing VerifyApplied 将其纳入该库的期望集合，新库不写伪053。未知053在任何新增迁移之前拒绝；通用迁移验证仍要求精确集合，其他额外版本不因这条兼容规则获准。

900023 forward-only，回执存在与否都不允许自动回滚删除历史。部署前需按实际旧账本与权限验证升级；受限角色、旧回执兼容、实际 Token HTTP 客户端、未知 ACK、并发与零资金副作用的本地验证不能代替生产激活或真实用户开户验收。
