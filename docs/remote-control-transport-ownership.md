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

B 的两个平台无关回归分别保持 operation、generation、租约不变只替换 transport，
验证旧 owner 无权执行闭包；以及在后继握手已开始后交付旧安装，验证安装闭包从未
执行、后继仍可正常完成。Linux 完整 lib 测试和 Windows 回归均可收集这些用例：

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib same_operation_replaced_transport_rejects_the_old_owner -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib stale_connected_install_never_runs_its_shared_side_effect -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib keeper_without_original_registration_cannot_read_or_send -- --test-threads=1
```
