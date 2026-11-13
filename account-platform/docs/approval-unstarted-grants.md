# 未建立执行记录的已授权尝试：只读调查

Billing proof 审批在 grant 后、Begin 前中断时，投递可能停留在
`DOMAIN_INFLIGHT`，但没有 Account 执行行。普通 execution 恢复计划保持
既有合同；本工具提供独立的定位入口，不改变该计划或 consumer 协议。

先安装 Account 前向迁移 `900022_approval_unstarted_grants`。运维人员在
受控运行环境中通过 `APPROVAL_GRANT_INVENTORY_DATABASE_URL` 注入真正的
`account_service` LOGIN 连接（不要使用迁移身份或把连接串写入日志）。
本工具使用该既有运行身份，但其数据库事务为只读；它不是新的最小权限身份。

```sh
go run ./cmd/approval-grant-inventory --limit 25
# 从 account-platform 目录执行；下一页原样使用上一页 next 的两项：
go run ./cmd/approval-grant-inventory --limit 25 --after-event EVENT_UUID --after-durable DURABLE
```

每页最多 200 条，数据库调查操作最多 5 秒，父上下文更短时以父上下文为准。
输出只包含原授权/请求引用、版本、请求状态、执行期限和尝试计数；不包含
命令正文、付款资料或凭据。输出本身仍应按受控审批记录保存。
`next` 只表示当前快照还有候选。多页不是单一快照；一次巡检结束后应从首
页重新巡检，覆盖分页期间新出现或恢复的授权。工具没有后台调度或自动部署。

所有条目固定为 `effect_conclusion=UNKNOWN`、
`next_action=QUERY_ONLY_NO_REEXECUTION`。请求到期或变为 INVALIDATED 后仍可见；
真正 Begin 建立执行记录后，下一次查询不再列入本清单，应转普通执行恢复。
工具不保证查询返回后状态不变化，也不取得执行权、创建执行行、重派发、
改变财务状态或写入终态。没有执行记录、期限已过或领域查询返回 ABSENT
均不能单独证明零资金效果；晚到 Begin 仍可能合法。后续人工案件和权威
完成证据的结案合同另行确定，本工具不代表该崩溃窗口已经完整收尾。

迁移仅新增只读函数和部分索引，不改历史 SQL 或业务事实；down 明确拒绝，
恢复采用向前修复，避免移除未知结果的调查能力。历史保留期间，索引构建
需要按站点规模安排发布窗口；本地隔离验证不代表生产建索引演练。
