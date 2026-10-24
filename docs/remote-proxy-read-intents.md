# RemoteProxy 导航与后台读取

桌面 sessions 的会话切换通过 bridge 注入的 `readRemoteProxyHistory(id,
{ navigation: true })` 验证 Rust 返回的历史记录。轮询、事件刷新和归档后的
刷新使用默认的后台读取。两类读取分别保留同一会话的最新意图；后台刷新不能
使正在等待的用户导航失效，导航也不能取消有效的后台刷新。

导航结果只交给 sessions，由其确认仍是最新会话切换后采用 ID 并发布投影。
后台读取只在该 ID 仍是当前会话时发布，不改变当前选择。同类读取仍丢弃较早
请求的迟到结果。删除或归档成功同时退休该 ID 的两类读取；bridge 停止则
退休全部读取。同名 ID 的后续读取不能使已退休的结果重新有效。

每个经校验的结果以弱引用关联其读取身份；sessions 在采用 ID 前同步复核身份，
以覆盖 bridge 校验完成到采用之间的异步窗口。两类请求共同记录发布顺序，
有效导航可以成功完成，同时保留已经发布的较新投影。
Rust 阻塞读取的实际执行顺序可能不同于请求顺序；同会话、同 journal epoch 的
认证 cursor 可比较时优先保持 cursor 前进。cursor 相同或跨 epoch 不可比较时
使用请求发布顺序，不为不透明的 epoch 新增排序规则。
弱引用不缓存每个会话的完整历史内容。

这只区分前端读取意图，不改变 Rust 命令、记录校验、持久化或授权契约，也不
以返回顺序推导 journal 快照的新旧。会话切换的最终选择仍由 sessions 拥有。

运行 `cd pinvou3-app` 后执行
`node --test tests/remote_proxy_navigation.test.mjs`：使用真实 sessions、桌面
remote bridge 和状态投影，固定导航/刷新两种完成顺序、重复导航、跨会话选择、
停止/删除/归档后的迟到结果及失败后继续读取。Rust IPC 在此测试中由受控
Promise 替代；该验证不代表真实双机部署或发布验收。
