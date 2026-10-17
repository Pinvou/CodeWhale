# Reservation 自动到期清理退避

Relay heartbeat 仍按原 wall clock 判断 reservation 是否到期。首次到期清理立即进入原 `invalidating` 状态；持久化或通知失败时原 owner/slot 保持关闭，认证和恢复期限不延长。

仅自动 expiry 发起的失败尝试记录 cooldown。名义延迟依次为 1、2、4、8、16、32 秒，之后封顶 60 秒；每次乘以 0.8–1.0 的随机因子。因此实际延迟有固定 800ms 下界和 60s 上界，没有永久停止重试。真正尝试时间还取决于现有 heartbeat 下一次检查，退避不是新的业务期限。直接 release/disconnect 清理不被 cooldown 延迟；直接失败后由下一次 expiry 首次重试的既有行为也保留。

retry clock 默认使用 `performance.now()`，与认证 wall clock 分离。构造器的 `retryClock`、`retryRandom` 可供定向测试注入；它们必须返回有限、非递减的单调毫秒数和 0–1 随机值。无效源失败关闭，不释放 owner；新接口没有环境变量或配置文件字段。持久化/通知本来的错误继续传播，不用计时错误替代它。

若失败完成时不能获取有效单调时间，该 owner 保持待建立 cooldown，不用 I/O 开始时间代替。首次恢复有效时钟的 expiry 只建立此前已确定的完整延迟并跳过重试；达到该 deadline 后才再次尝试。失败计数不重置，后续无效读取不刷新 deadline，直接 release 仍不受此 gate 延迟。

计时只依附当前不可变 reservation 对象，失败次数饱和到 7，成功清理删除。WeakMap 不保留独立历史表；后来的 owner 即使复用相同 reservation ID，也不继承旧 cooldown。原同 owner 在途 Promise 去重、slot revision、pairing/generation 绑定和历史 terminal tombstone 均保留。通知失败后的 retry 不重写已落盘 terminal，也不会成为旧业务任务重新派发入口。

原匿名 `reservation_invalidation_failed` 和 `reservation_released` telemetry 保持原闭合集映射；不新增 endpoint、socket、pairing、原始错误或凭据日志。

这只是 #1470 的一个独立问题点。全量同步到期的并发上限、CAS 即时冲突重试及进一步运营告警仍未由本点解决。测试使用真实 reservation/pairing 生产模块及故障持久化端口；不声称真实磁盘、Windows DACL、双机服务或全 Relay 发布资格。
