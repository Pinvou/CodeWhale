# RemoteProxy 导航、后台与只读面板读取

桌面 sessions 的会话切换通过 bridge 注入的 `readRemoteProxyHistory(id,
{ navigation: true })` 验证 Rust 返回的历史记录。轮询、事件刷新和归档后的
刷新使用默认的后台读取。归档管理面板通过 `{ view: true }` 读取组件本地的
只读正文，不导航、不发布全局投影。三类读取分别保留同一会话的最新意图，
各类只替换本类请求，不使其他类的有效读取失效。

导航结果只交给 sessions，由其确认仍是最新会话切换后采用 ID 并发布投影。
后台读取只在该 ID 仍是当前会话时发布，不改变当前选择。同类读取仍丢弃较早
请求的迟到结果。删除或归档成功同时退休该 ID 的全部读取；bridge 停止则
退休全部读取。同名 ID 的后续读取不能使已退休的结果重新有效。

每个经校验的结果以弱引用关联其读取身份；sessions 必须在采用 ID 前同步复核
身份，缺少 checker 时失败关闭。该检查保留同步退休下的防御边界；受控 IPC
测试可以安排校验到采用之间的微任务交错，不代表生产中独立的删除/归档 IPC
完成回调可以插入同一微任务 drain。同步 stop 当前没有生产调用者。
Rust 阻塞读取的实际执行顺序可能不同于请求顺序；同会话、同 journal epoch 的
认证 cursor 可比较时保持 cursor 不回退。同一 ID 已发布认证 cursor 后，不再
接受初始无 cursor 的空窗覆盖；Rust 无 cursor 只代表该记录尚无认证 journal，
不会用它重置既有 journal。其他会话的初始空历史仍可正常打开。
cursor 相同或跨 epoch 不可比较时
按有效结果到达发布边界的顺序发布，不以请求序推导快照新旧，也不为不透明的
epoch 新增排序规则。相同 cursor 的 pending 等非 journal 元数据仍可更新；
这不承诺不可比较快照的全局新旧排序。`selectRemoteProxyHistory` 返回 true
表示当前记录已被接受，较旧的可比较 cursor 仍可能不触发发布。
弱引用不缓存每个会话的完整历史内容。

这只区分前端读取意图，不改变 Rust 命令、记录校验、持久化或授权契约，也不
以返回顺序推导 journal 快照的新旧。会话切换的最终选择仍由 sessions 拥有。

运行 `cd pinvou3-app` 后执行
`node --test tests/remote_proxy_navigation.test.mjs`：使用真实 sessions、桌面
remote bridge 和状态投影，固定导航/刷新两种完成顺序、等 cursor 元数据、跨
epoch、重复导航、双 pending 跨会话导航、只读面板隔离、停止/删除/归档后的
迟到结果及失败后继续读取，并检查生产 checker 接线。Rust IPC 在此测试中由受控
Promise 替代；该验证不代表真实双机部署或发布验收。
