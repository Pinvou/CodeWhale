# A 端恢复成功的状态投影

完整认证恢复经过原 operation、完整五元租约、恢复相位、B 激活后存活终闸和
当前 reader/socket 的一次性安装证明后，manager 返回真实 Connected 快照。
该快照用于重新激活业务，再由 keeper 送入生产状态消费者。查询和订阅使用
同一完整 DTO；不能靠查询追平来替代成功事件。

业务激活和最终发布分别在 manager 锁内核对原 operation 与当前完整状态。
显式断开保留 operation，故只核 owner 不足；相位、claim、完整租约也必须匹配。
状态消费者拥有唯一的 previous_status，成功后会记录 Resume/Ok，下一次断线
重新记录 Disconnection/Ok。失败或旧状态被拒时，不改变该消费者状态。

与 A 端入站背压组合时，完整恢复快照先通过业务激活围栏，再释放本租约的
已接纳入站尾帧；只有 `resume_business` 成功后才能发布该 Connected 快照。
入站恢复失败沿原 owner 的失败路径清理业务并发布错误，不发布恢复成功。
组合回归同时覆盖入站释放前无成功事件，以及释放失败后不产生成功投影。
入站测试模块与恢复投影模块均只在 `cfg(test)` 下编译。恢复夹具显式转交
`ControllerLaneEvent::Failed`，并逐一覆盖队列满、字节预算耗尽和通道关闭：
恢复中的原租约进入错误状态、清理业务，剩余恢复帧不得再发布 Connected。

两个 guard 之间不持锁调用状态消费者。消费者的同步遥测、日志和事件端口不得
重入 manager；遥测未启用或不能记录时，状态仍正常发布。当前一个 keeper
同步处理恢复及回调，再消费下一个事件；状态相等性是当前值围栏，不是通用的
历史周期去重器。重复 final 由恢复相位、一次性 reader proof 和 keeper 收口。

本修复不改 40 秒握手上限、首次断线冻结的 30 秒预算、恢复认证、线协议或
Legacy Web v2。完整成功才开启下轮有限预算；中间阶段和重复断线不能续期。

正常 Rust 活动状态始终携带 claimRequestId 和完整 nested lease。前端原有的
已绑定同租约补丁保留 claim，完整身份不匹配或终态后的迟到成功仍被拒绝。
任意空状态下缺 claim 的人工输入不是本次认证合同，也没有作为后端生产缺陷。
本轮不改变既有前端兼容行为。

回归使用真实产品 lib 的恢复处理、manager、加密 lane、reader dispatcher 和
生产状态消费者；Relay 投递、Tauri emit 与遥测外部 I/O 为受控端口。测试先核
未经修改的随机 claim/完整租约和事件查询一致性，再仅对副本固定 claim 比较
共享 JSON 形状，JS bridge/state 消费同一共享形状。它不代表双机、WebView、
遥测上传或部署端到端验证。测试同步点只存在于 cfg(test)，不增加产品接口。
正常脚本恢复帧均经过实际 reader 的 schema/context 校验及事件出口后再交 keeper，
重复 final 按 reader 拒绝结果丢弃；无 reader 权限的 final 是明确的拒绝性注入。
第二轮恢复的预算失败仍按首次截止收口，发布时直接核原租约业务已激活。异租约中断置于
受控时钟的第 5、10 秒，并直接核交付时刻。计时器和 gate 输入使用同源时钟，
从预算建立后同一同步 poll 的首次断线交付计时，final 绝对计划在第 30 或 50 秒；
仍按首次 30 秒收口，断言仅容纳 Tokio 定时器一次至多 1 毫秒的向上取整。

恢复中的当前通道再次断线会立即终止候选，不等冻结预算耗尽，也不重启恢复。
A 在完整原始 transport registration（owner、generation、租约）仍匹配后清理
业务并提交一次恢复失败；旧 owner、旧租约和无租约的迟到通知不能终止新候选。
B 的本地 `desktop_relay_connection` 通知由持有当前 reader 的管理器产生，
恢复处理器先核捕获的 transport owner，再沿既有终态清理链结束本代。
终态提交在同一 owner 锁内退休实际业务密钥，随后释放锁再通知生产清理适配器；
旧 worker 或重复终态不能执行新通道的清钥动作。
该通知不带租约，不能按普通恢复帧的完整租约筛选丢弃。来自 socket JSON 的
同名输入被管理器拒绝，不能冒充本地断线；`connected:true` 不终止候选。

当前 controller-v1 线协议没有独立的 peer-offline 消息，Relay 的 socket-close
钩子也不向另一端生成这种帧。本修复不新增未发布消息或桌面兼容分支：只有
可信本地 transport 事件走即时路径；本地连接尚在而对端失联时，仍由既有认证
存活证据和有界 watchdog 处理。未知或无法绑定的帧不能续期首次预算。

定向回归在 Prepared、Provisional、pending 阶段分别断线。A 使用真实 keeper、
manager、密码通道和生产事件适配器，投递与时钟受控；B 使用 Windows 进程私有
真实 Wry host、loopback WebSocket、RelayClient、管理器、worker 和生产清理适配器，
核验即时退出、仅一次本代清理及下一次可重新登记。伪造 wire 通知经过真实 reader
入口后不得改变状态或冻结截止；这些测试不代表双机现场或发布验收。

Windows linked exe 的独立非零筛选及 Linux 全 lib 门禁须实际运行；本地 GNU
验证不能替代正式 MSVC/Linux 和 Merge Queue。发布仍受 #1428 独立安全前置约束。

本修复采用严格的逻辑 owner 口径：候选已进入恢复后，匹配归属的下一次可信本地
断线即终止。重连中 prepare 尚未被接受的写入失败、或回退 Legacy socket 后的
关闭也包括在内；这两类失败不再在同一候选内继续尝试第三个 socket，需重新发起
连接。此处 Legacy 指恢复连接尝试的 socket 模式，不改变 Legacy Web v2 线协议。
健康的 controller socket connected:true 仍按原 request/challenge 和冻结截止重发
prepare，重复通知不能增加发送次数或续期。A 端自动重连快速失败产生的匹配本地
Disconnected 同样立即收口；A 端连接超时本身不产生该事件，仍受原冻结预算约束。
B 端连接尝试失败（包括超时）产生当前 reader 的可信 false 时，按严格策略终止。
实际业务密钥退休的声明仅限本次匹配归属的再次断线终态路径，不代表所有既有
watchdog 或其它终态路径都由本次重构。真实回归分别验证两类失败的即时 Error、
密钥和出站退休、一次清理，以及新意图能重新登记并使用实际通道。
