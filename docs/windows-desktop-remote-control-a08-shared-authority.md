# A08 共享桌面通道的认证恢复边界

B 端使用既有桌面 Relay socket，不另建远控连接。初次握手完成后，必须将实际 reader
收到的 `controller_joined` 凭据、完整五元租约及协商能力登记到该 socket 的认证权限。
恢复最终帧仍须通过既有恢复状态机的加密确认、激活、存活证据和截止时间校验；
没有实际 reader 凭据时不得恢复 Connected，也不得退回“仅验证 wire JSON”。
入口须将 `encrypted_resume_confirm` 交给既有恢复状态机，不能因它没有
`controller_` 前缀而过滤；worker 仍负责解密、activation 和 request 校验。

内部凭据不是协议字段，不含 token 或密钥。它绑定 socket 实例、连接轮次、通道绑定
序号、代际、完整租约、activation 与 request，并在权限锁内检查和单次消费。
权限还绑定实际协商的 controller 子协议；Legacy v2 回退不能生成远控凭据。
断线、停止、通道退休或 endpoint 替换会撤销旧权限；同名代际重新绑定也不复用旧凭据。
非法 final 不占用后续合法 final 的接纳位置，重复或迟到 final 不得恢复业务发送权限。

manager 在同一锁中验证 reader 所属 endpoint 并投递到相应通道。发送者捕获的通道
绑定序号还须在实际入队前由权限锁再次验证，旧 reader、旧 sender 与迟到 Drop/Stop
均不能影响替换后的通道。锁内不调用业务生命周期、执行 await 或触发外部回调。

业务帧只能在当前认证连接上发送，旧队列不能跨 socket 重放。唯一允许在断线时提前
入队的是严格验证、匹配已登记租约的 `controller_resume_prepare`，它只定位到下一次
连接轮次。首次断线冻结的最后有效认证存活证据预算不会因重连或恢复消息延长。
Legacy WebUI v2 的共享连接及本地 Chat/ACP/Stop 不引入此远控权限门禁。

回归使用实际 loopback WebSocket reader，而非手工写入凭据：覆盖首轮登记、恢复成功、
错配/重复 final、关闭/停止/同代替换、断线 prepare，以及旧 reader 的投递隔离。
Windows 原生回归贯穿真实 RelayClient、manager 与 worker，并核验冻结预算和保留的
存活 epoch；不等价于真实双机部署、外部 provider 或发布验收。
