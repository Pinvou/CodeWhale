# 已确认预留与派发持久化的恢复边界

Billing 按原 usage_request_id 预留成功后，Token 先在独立事务中保存准确 hold_id、price_version_id 及 HOLD_RESERVED，再在第二事务中把已冻结 preparation 提升为 dispatch intent。两个事务之间不持有数据库锁跨外部请求。提升失败或调用 context 取消不会回滚已提交的预留身份，也不会据此调用 Release 或写入 RELEASED。

第一事务本身失败或提交结果未知时，原 usage key 与 HOLD_RESERVING／HOLD_RESERVED 保留；原 key 重试使用原冻结价格和请求向 Billing 取回同一 hold，不能生成新逻辑请求或重派发已取得租约的请求。HOLD_RESERVED 的原 key 重试可继续提升，取消仍须在最终提升锁内检查。数据库不可用时不能保证当前进程已经保存了 Billing 的 ACK，不能把内存观察当成持久化证据。

没有自动派发。已保存身份而未使用的 hold 仍由既有 Billing expiry proof／取消流程处理，沿用原资金截止和主体权限；只有 Billing Release 已确认成功后才写终态。Release 超时、失败或成功 ACK 丢失保留准确身份与原恢复围栏，由同一证明重试。自动 expiry 恢复仍以现有 TOKEN_HOLD_EXPIRY_URL、可信 workload 与所需 grant 配置为前提，未配置时沿用人工路径，不能据此宣称默认部署自动收敛。普通 missing-reference 对账与取消所属恢复边界保持不变。本项不新增状态、迁移、资金政策、释放授权或外部提供方调用。

永久 PostgreSQL 场景覆盖真实 intent 事务回滚、首次身份事务回滚、context 取消、同 key 一次派发、错误身份拒绝、取消交错，以及原 expiry worker 对释放失败／ACK 丢失的恢复；严格 runner 要求新根和子例命中，禁止以 skip 或零用例通过。
