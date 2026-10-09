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

## A07 本轮修复传入 A08 的保全边界

以普通双父 merge 保留 A07 与 A08 原提交，不恢复已批准退役的旧持久 history 模块、
renderer history 命令或启动实例；A07 的 v2 恢复修复仍保留在祖先历史中。
A08 已有非空 unrestricted 审批预览固定脱敏及 scope 防护不降级为正文过滤。

正文继续使用 A08 Markdown/CJK 与代际 journal 实现；采用 A07 敏感字段分类，
识别引号、多词、分离 ':'/'='、无空格 Header/Cookie，并关闭已识别敏感值的整行。
用户选择方案①：所有非空白文本块（包括普通 CJK）在可完整分类的边界后才输出，
不保证逐字显示或跨推理/正文/工具通道的原始到达顺序；本地显示不变。
待判定缓存最多 4 KiB UTF-8 字节，超限只发固定脱敏标记、消费剩余块并保守遮蔽当前行，
换行后恢复；不得按时间/容量强制刷新原始前缀。普通 token budget 等句子不误触发。
工具事件和推理/正文交错不完成 pending block、不重置敏感 Header 状态。
只有已接受的成功 Completed 能分类最后未决块；失败/取消/超时/缺终态及 Drop 使用 abort，
未决块只输出固定遮蔽并清零，不原样冲刷。buffer Drop 清零；既有输出预算继续适用。
Chat 正文/工具及 ACP 投递与关闭使用同一回合状态锁：准备到投递之间不能被关闭超越，
关闭后不接纳迟到回调；Chat 已接受终态后也不接纳工具回调。真实投递 helper 的屏障
测试覆盖该锁边界和迟到事件，不能据此宣称已经验证动态 Tauri IPC 或真实 Drop 调度。
回归遍历各个字符边界的双分片及逐字符多分片，将每次 push 返回值分别送入真实
journal→回放/快照，不能先拼接后声称已验证逐 delta 发布；不是旧 history 落盘验收。

core、真实 staging 及 B 本地附件发布共同执行相同 Windows 安全原文件名合同，
分配前拒绝非法名并原名发布，不保留旧截断/静默改名 helper。
实际平台 unsupported 门禁、私有发布/read owner 及重试清理逻辑保留；只统一附件回收时基。
journal 保留 A08 会话隔离、事件指纹/容量、终态不重复、验证成功后才推进状态的合同，
并适配 A07 最大游标与耗尽测试；不得搬入 A07 旧单流数据结构或先写终态 CAS。
Chat/ACP 仅收口远控投影的流式收尾边界，不改变基础本地引擎、供应商调用或取消退休语义；
另传入 ACP CRLF/LF 测试探针增强，共享认证恢复修复不变。

本次传播不表示 A08 新审核阻塞项全部完成，也不提前请求 A09 以后复审或代为合并。
