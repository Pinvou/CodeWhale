# Legacy Relay 单次连接截止

WebAccess 与独立撤销通道的每次 DNS/TCP/TLS/WebSocket upgrade 等待共享一个 10 秒截止，与 controller transport 的默认单次连接预算一致。该截止从本次连接开始等待起计算，不随待发送消息到达而重置；成功连接后的心跳、撤销回执 12 秒预算和既有指数退避分别保持独立。

截止报告普通连接失败，继续现有退避及下一次实际连接。WebAccess 保留有界待发送队列；撤销通道保留同一 endpoint 与撤销秘密，管理器的持久化账、去重和 in-flight 归属不变。超时不表示撤销成功。WebAccess Stop 和撤销 receiver 关闭仍能立即取消连接等待。

截止约束异步连接等待，不承诺终止操作系统内部已经开始的阻塞 DNS 工作。本项不改变协议降级、认证、帧大小或队列准入。

`relay_client::tests` 使用独立 loopback TCP 端点接收真实 upgrade 请求并保持无响应，核对客户端截止关闭、退避、下一条真实 WebSocket 连接与原消息/撤销责任交付；另核对两个入口等待中的取消。生产模块同时由应用原生测试和 `tests/controller-crypto-harness` 编译，后者不证明 Tauri 启动或真实部署可用。测试应通过 `scripts/ci-fixture-process.py` 执行。
