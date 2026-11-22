# Commerce 客户同步的代际边界

新派发租约只授予尚未建立人工案件、与当前 mapping 的 subject、provider、adapter、version 全部匹配且 mapping 未 DETACHED 的任务。候选查询先排除旧代际，锁定 USER、mapping、job 后再次核验；租约仍按锁等待结束后的数据库时钟建立。手机号或适配器替换后，旧任务不能重新派发，也不会阻塞当前任务。

旧任务、失败和 STALE 证据不会删除。已取得有效租约的旧请求仍可按原 Complete/Retry 接收回执；旧来源的成功回执记录为 STALE，不更新当前 mapping。已经失去原租约的请求不能借旧回执再次完成。过期与结果未知转入人工案件，迟到回调只追加最小证据；人工核验、已注销主体和恢复合同见 [Commerce 人工收尾](commerce-manual-completion.md)。

入队保留既有注册 PROVISIONING 及已确认 MOBILE 在 ACTIVE、DISABLED、REPAIR_REQUIRED、PROVISIONING 的合同；只有 ACTIVE 用户可取得派发租约。尚未转人工、仍有合法 RETRY 状态的同代际临时 DISABLED 用户在恢复 ACTIVE 后可继续重试。新人工收尾通过前向迁移安装，不处置资金，也不声明供应商在途请求已撤销。
