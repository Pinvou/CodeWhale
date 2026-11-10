# 远控 transport 与生命周期归属

断线通知已离开 reader 或事件队列后，停止旧 socket 不能撤回该通知。共享业务 lane、
生命周期及出站 sender 的最终变更必须仍由接收该事件的原操作及 transport 注册持有。

## A：断线通知的身份

`ControllerTransportEvent::Disconnected` 将原 sender 的内部身份、generation 和完整
`LeaseContext` 经过实际 lane adapter 传给 port/keeper。内部身份复用 sender 已有 Arc，
只比较同一分配，不作为协议字段、密钥或另一个 socket epoch 算法。keeper 固定原 port
的 sender 和认证上下文；在 manager 锁内核对原 operation、注册及完整租约后，才冻结
剩余存活预算和挂起业务。恢复继续使用既有 reader epoch 与一次性恢复安装凭据。

原尝试自己的未认证断线会结束该尝试，返回既有 `endpoint_offline`；没有租约的事件
不获得任何已认证恢复权。其它 owner、generation 或五元租约不匹配时，不改变当前
连接。精确退休注册只允许原终态清理，不赋予 live lease 或重新恢复权限。

## B：已出队事件与原子提交

初次握手在生命周期锁内重新核对 operation 和当前阶段，并取得实际 reader 认证凭据，
将加密 lane、出站 sender 与 Connected 注册一并提交。worker 随后固定原 operation、
完整租约和 transport Arc；迟到旧断线、握手失败或恢复回调不得重新查询新 operation
来取得作用于后继连接的权力。失败 clear 及断线 suspend/预算冻结也在同一 owner
检查下进行；合法当前断线已经撤销 active 发送权，仍可进入既有有界恢复。

恢复最终 reader proof、生命周期转换和 lane.activate 共用同一最终提交区，避免旧
成功回调在换代后重新打开后继 lane。清理先在锁内捕获旧对象，解锁后才通知清理端。
锁顺序保持生命周期 manager → relay manager → transport authority；不把 recv 的
relay manager 锁持到业务 worker，不在 authority 的业务门禁里重入 transport。

B 轮询尾部登记存活挑战及失败/超时取消，也在生命周期锁内核原 worker 的
operation、完整租约和 transport。登记与返回用于构帧的租约属于同一原子检查；
不能借旧 tick 已通过的心跳检查给后继登记 phantom pending，暂时掩盖其存活可疑
状态。挑战本身不续期硬截止；只有真实有效证据才推进既有预算。

此处不改变 wire JSON、首次冻结截止或认证恢复协议，也不补充恢复中再次断线的
即时处置策略。成功恢复后的 A Connected 投影由独立问题 #1425 跟踪。

## 验证入口与边界

平台无关注册/adapter 回归：

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib registration_ownership -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib actual_adapter_ -- --test-threads=1
```

Windows 实际 Wry/RelayClient/reader/manager/worker 回归：

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib app::commands::remote_control::reader_resume_tests:: -- --test-threads=1
```

Windows parent driver 使用隔离子进程和临时运行目录，固定真实 recv 返回后的交错，
在后继完成签名握手后放行旧断线或 joined，核对状态、冻结预算、出站绑定及加密
会话仍属于后继，并继续一次合法断线。另以完整实际恢复确认最终激活仍持有生命周期
提交锁。CI 已有该 parent 模块的非零过滤器；Linux full lib 执行平台无关测试。
A adapter 测试使用真实事件生产/投递及控制队列，但认证 lane 状态由测试夹具安装；
同 operation 换 sender 是防御 API 场景，不声称当前 A 产品能复现跨新操作竞态。
这些回归不等价于双机公网、供应商任务执行或发布安全验收。

keeper 在所有编译模式都必须带原 transport 注册；缺失时在读取帧或发送之前退出。
孤立服务测试也登记真实内存 sender 的完整身份，不再跳过此入口检查。存活发送口由
生产组合入口绑定原 sender，测试可通过相同发送接口注入背压或连接替换；这类测试
证明注册检查及状态派发，不替代实际 socket writer。实际 adapter/reader 回归另验
事件生产与传输边界。

## Controller transport 的调度和异常退休

`spawn_controller_transport` 在提交异步任务前创建本实例的生命周期 guard。任务正常
意外返回、panic 或取消/未轮询即丢弃时，guard 先竞争本 sender 的一次性终态，再取消
发送权、清除 active/saved lease 与最终恢复安装凭据。显式 Stop 已领取终态时不另发
异常断线；异常结束只投影一次捕获的 owner、generation 和完整租约，继续由原 keeper
核对，不能借全局当前实例清理替换后的 sender。外部 sink panic 不跳过先行的退休。

认证安装到 keeper 首次轮询之间也适用此规则：任务在清空 active/saved 之前固定一个
精确的终态注册元数据，尚未登记的 keeper 仅通过专用来源取得该身份，再经过原
manager 的 operation/owner/完整租约围栏消费已排队的断线或入站失败。元数据不包含
密钥、token 或能力列表，不能用于发送、安装或恢复；不把缺失租约改成通配。终态
通知先发生、旧 keeper 延迟启动且新实例已 Connected 时，原身份仍不能作用于新实例。

持续就绪的出站最多连续领取 8 个帧后，下一次选择优先检查已到期心跳及入站，再继续
出站；Stop 和入站绝对静默截止始终优先。过期出站也占用该轮询额度。单次挂起写仍
受既有写截止和更早的入站静默截止约束，不声称能抢占同步阻塞的 poll。控制/业务队列
内的既有控制优先语义保持；这不是共享 relay_client 的另一个公平调度修复。

合法但无安装租约的控制帧仍可交付握手/协商，不能重置重连退避。只有同一 socket、
同一次已认证租约安装的正向上下文匹配帧，至少两次跨越既有一个心跳窗口，才重置
退避；换 socket 或重新安装等值租约也重新积累证据。该 transport 证据不替代领域的
MAC、存活挑战或首次断线冻结预算。入站 Ping、合法当前帧与当前未消费探测的 Pong
可更新 transport 静默时间；非法/旧代帧、任意或重放的 Pong、二进制噪声和本地发送
不更新它。Ping 的 16 字节负载包含本地时间和本 socket 的探测序号，Pong 按 WebSocket
规则原样回显；序号不作为远控 wire JSON 或认证材料。

transport 只严格解析一次原始 JSON（含重复键和原始字节长度检查），随后在同一个
parsed/validated 视图上执行角色、协议、能力和上下文检查。已安装租约的不可变 Arc
快照避免热路径复制完整能力列表；队列和退休事件仍捕获需要保留的上下文。JSON
验证入口继续保留原有错误顺序、大小、能力和租约约束。

共享恢复夹具的 pending、confirmed 和 resumed 回执分别引用真实 B 发出的 ACK、
liveness 和 ready 请求，保持与生产 Relay 写方一致后再验证协议重构的完整恢复链路。

相关定向回归入口（需使用仓库共享进程/资源作用域）：

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib controller::relay_transport:: -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib controller::protocol:: -- --test-threads=1
```

实际 loopback WebSocket 验证裸合法控制与短命连接、无有效响应的噪声、真实任务 panic
及清理；可控时钟/队列另验轮询边界、安装替换、Stop 和原有建连/写总截止。这些
本地入口不等价于部署 Relay、完整界面、双机、负载性能或发布验收。

实际 connected-loop 的内存 wire 探针使用生产轮询、严格入站和真实 backoff 状态，
核对持续出站时入站处理上界、稳定正向的重置及仅 Pong/裸帧不重置，以及生产生成的
16 字节探测被当前响应恰好消费一次。FIFO 帧标记确认先前响应已处理，不使用 sleep
猜测交错。此处仅替代网络 I/O；loopback socket、真实 spawn、adapter 与实际 App
恢复夹具分别核对其边界，父子/重复执行不累计成独立场景。Windows 定向过滤器显式
包含新的 availability、parsed-message 与 adapter 回归，Linux 完整 lib 仍包含它们。

B 的两个平台无关回归分别保持 operation、generation、租约不变只替换 transport，
验证旧 owner 无权执行闭包；以及在后继握手已开始后交付旧安装，验证安装闭包从未
执行、后继仍可正常完成。Linux 完整 lib 测试和 Windows 回归均可收集这些用例：

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib same_operation_replaced_transport_rejects_the_old_owner -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib stale_connected_install_never_runs_its_shared_side_effect -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib keeper_without_original_registration_cannot_read_or_send -- --test-threads=1
```
