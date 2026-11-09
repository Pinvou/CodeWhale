# 注册任务异常退出

首次启动和刷新保存各自收到的 operation，并通过同一等待边界读取注册结果。正常成功与正常拒绝沿用 worker 原有收尾；结果通道异常关闭时，先等待原注册任务退出、释放 worker 和通道，再关闭捕获 generation 的 relay lane，并用捕获 operation 收口 manager、发布有界失败投影。

旧 operation 已被 stop、重新 start 或 refresh 取代时，不发布旧失败，不查找并清理“当前”operation。relay 的 generation 检查及 channel 的绑定身份防止旧任务 Drop 清理后继通道。返回固定 `pairing_registration_failed`，不格式化或记录 JoinError/panic payload；不新增超时终态。成功注册后仍由原任务继续 serve，等待结果不会等待整个服务生命周期。

刷新完成换代时，在同一个生命周期锁内发布新代 Registering，再启动 worker。前端先接受这个已绑定的新代，才接受其 Error 终态；保留现有旧代拒绝规则。构造失败和任务异常的命令终态同样在精确 operation 围栏内发布，旧代资源通知仍在锁外。发布回调不得重新进入 manager。

验证包含真实 oneshot 在等待前和等待中关闭、首次与刷新操作、正常结果保全、下一次启动，以及进程隔离 Windows Wry 环境中实际 Relay/注册 worker 任务中止。受控本地 WebSocket 接收真实注册帧后中止任务，交错 stop/start/refresh 并保留后继通道核对其不被旧任务或旧命令清理。该测试不声称普通网络输入必然触发 panic，也不证明进程 abort 或运行时崩溃可在进程内恢复。
