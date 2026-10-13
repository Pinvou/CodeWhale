# A05 回滚网关空闲传输合同

网关分别终止公网和上游 WebSocket。Relay 对网关连接发送 ping，网关上游客户端自动 pong，不能证明公网客户端仍在线，也不会自动给公网代理产生流量。

网关在 Enrollment 授权成功且上游连接建立后，独立向公网客户端发送 transport ping。默认间隔为 15 秒；上一轮没有收到 pong，下一轮终止连接，关闭路径清理该连接的计时器。未认证连接不启动这项心跳，原认证超时（默认 10 秒）保留。业务消息、认证状态、任务权限与代际、应用层 native heartbeat/liveness 的原规则保持独立；transport pong 不刷新它们，也不重新派发旧任务。

| 链路 | 合同 |
| --- | --- |
| nginx → Gateway 公网连接 | 样例显式 `proxy_read_timeout 75s` 和 `proxy_send_timeout 75s`，大于默认 15 秒 ping 间隔；健康空闲客户端继续自动 pong。 |
| Gateway → Relay 上游连接 | 保留 Relay 自身心跳配置（`HEARTBEAT_INTERVAL_MS` 默认 15000）；上游自动 pong 与公网心跳分别发生。 |
| Enrollment 认证与任务 | 保留既有认证期限、凭据校验及业务 liveness，不能将代理超时或 pong 当成授权存活证据。 |

`createEnrollmentGateway` 的 `heartbeatIntervalMs` 参数支持 100–60000 的整数毫秒，默认 15000，非法值在监听前拒绝；生产 CLI 使用该默认值。库调用方若改变间隔，须保证代理读写预算大于其实际心跳间隔并留出调度余量，不能通过增加代理预算延长任务或认证期限。不回投上游 ping 作为公网权限证明。

`enrollment-gateway-heartbeat.test.js` 用真实 Gateway 和 WebSocket 传输覆盖健康空闲、上游/公网 ping 分离、缺失公网 pong、未认证连接原期限、关闭与配置拒绝。Registry 和上游为明确测试适配器，不代表持久 Enrollment 或真实 Relay E2E。

`enrollment-gateway-nginx-idle.test.js` 可通过显式测试环境变量 `PINVOU_TEST_NGINX_BIN` 指定已验证的本地 nginx 工具，使用自有回环监听、独立 prefix 和缩放的 1 秒代理预算，验证超过两个空闲预算后连接仍开放。没有该测试工具时仅此项条件跳过，不能算实际通过。测试不部署 nginx，不操作其他服务，也不证明 TLS/DNS/公网部署、存量数据恢复或真实 Relay 空闲验收；结果以当前提交日志为准。
