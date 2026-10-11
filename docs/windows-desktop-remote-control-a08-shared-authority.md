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

## 受控提交的阻塞工作与回合预算

B 本地和认证 A 提交共用同一准备、映射和接纳实现；宿主将整段同步工作交给
阻塞工作线程。回合预算在入队前冻结，显式传到接纳票据，排队、扫描和恢复活动
均不得重新起算。工作线程开始及精确租约/能力门禁内再次检查，过期请求不接纳。

生产派发先在阻塞线程完成登记库 Running 写入，再等待真正的异步执行器，最后在
阻塞线程完成精确 sequence 终态写入与资源清理。资源锁不跨 await，收口取得锁后
再次检查同一截止时间和取消令牌；会话失败持久化、产物扫描及附件清理也不占用
异步轮询。阻塞工作无法被超时抢占，预算在下一检查点收口，不宣称硬实时中断磁盘 IO。

接纳后、移交后台前的异常只收口捕获的任务 sequence；资源安装异常先转为无回调的
持久失败，再于租约 owner 锁释放后执行清理。Chat 捕获拥有完整租约、会话、任务及
sequence 的清理保护，早退和异常不留下本轮捕获，也不删除替换回合。工作线程错误
仅返回固定错误码，不透出异常文本。客户端丢弃响应不会取消已归 B 所有的工作，
认证重试仍通过登记库去重；这不表示 CodeWhale #97 依赖项已经合入或 A08 整体验收通过。

## R15 启动降级与审核修复边界

A07 的 ready 标记只在 A08 的全部状态及宿主执行/投影端口安装完成后发布。
State 参数使用同一受保护提取器；只用 AppHandle 的能力与操作命令在获取域状态前
检查 ready。启动失败返回稳定 unavailable，能力广告关闭；非 Windows 不打开桌面
登记库、不启动域 reaper。原 Tauri State IPC 提取返回错误而非必然 panic；
此处修复真实降级合同，不创建假状态、不重置持久数据，也不改变 Legacy Web。
保留 A08 的旧持久历史退役决定；A07 新增的已退役 DTO 路径测试不复活成第二历史入口。

敏感值分类传入 A08 的有界流式 sanitizer，覆盖 password、client_secret、
refresh_token、裸 Basic 及敏感 HTTP header 的折行续值，包含 CRLF 和任意分片边界。
无约束审批输入仍采用已批准的固定遮挡，不改回启发式预览。审批与启动恢复复用唯一
单调时钟。ACP 的 task_cleanup_busy 只是发布窗口瞬时竞争：未生成审批时等待下一次
有界 poll，不退休运行时；真实任务世代或所有权错误仍失败关闭。

B 上传 admission 使用宿主的摄入预算（与本地文件摄入共用20MiB），预留与 staging
分配前拒绝超限；共享上传/下载协议核心32MiB不因此改写。模型列表的 keychain/设置
读取移至阻塞工作线程，无消费者的主动模型导出入口删除，准备阶段不迁移或写设置。
前端轮询依赖完整租约/会话身份及相关 lifecycle journal 修订，不以每个文本块更换的
对象身份重发；B 端审批读取依赖等待任务集合，并每2秒无重叠刷新，以获取同一等待
任务新增的审批；停止后抑制迟到结果。预览关闭按钮复用已有三语无障碍文案。

非阻塞容量项仍未修：RemoteProxy 全局记录/文件预算、4096 retired_requests 墓碑、
配对注册专用 endpoint/global 配额及持锁大文件IO。256记录恰满仅限制新增，并非
所有修改必然失败；墓碑满首先阻断退休/abandon，不是普通投影立即全部冻结；Relay
已有通用入站限速，不冒称完全无限制。正式开放前必须闭合相关P2，不能丢墓碑复用ID、
手改用户JSON或删除精确 owner 检查。每轮Chat引擎重建是一次性owner隔离设计，未
证明泄漏或随轮次延迟递增；宿主适配的维护性不以文件行数判定架构违规。

### R15 新审查的契约澄清

RC-B-011 要求开放代码模式前由 B 本地授权工作区，不要求普通聊天启动必须具有
工作区。空列表仍可登记与配对，但 workspace_count 为 0、code_mode_allowed 为 false，
任何代码工作区解析均返回 workspace_not_authorized。撤回此前“不以空工作区启动”的
过度宣称；真实 manager 生命周期与前端实际 Toggle 条件回归锁定该区别。

Chat 的 relay.finish(true) 仅安全收口 Reasoning/AssistantDelta 尾文本，不发布任务成功
终态。任务终态仍由任务服务在原截止时间、取消令牌与精确 sequence 下统一裁决。
引擎完成后，收尾或资源锁等待跨过截止仍可使任务超时；这不是已提交成功被改写。
既有 dispatch_settlement_rechecks_same_deadline_and_cancel_after_resource_wait 回归覆盖
成功 executor 后等待资源 gate 跨同一截止时间或取消的结算；Chat 私有模型清理耗时
跨截止的路径由源码确认，未单独动态注入。不得删除最后 checkpoint 或因收尾延长预算。

A 端逐帧投影仍在异步 lane 持 manager/RemoteProxy 锁同步全量落盘，慢 IO 可能拖住
其他命令与心跳；未实测“每帧 MB 级”或实际超时。登记 P2 非阻塞待办，优化须保持
精确所有权与持久化先成功后 ACK，不能只移除锁或提前确认。

Chat 工件目前登记活文件而非不可变副本，后续改写可使历史成果下载失败。B 的传输
核心会校验登记摘要并拒绝不符的最后一块；A 再校验完整长度与摘要后才保存 Blob。
因此“无摘要校验、错内容成功发布”的前提不成立，也不保证拒绝前没有非最终块传出。
快照稳定性另登记 P2 非阻塞待办，需在完成时受限复制、校验并精确清理，不能以下载
前 hash 后重新打开的 TOCTOU 方案代替。以上登记项未修，不代表正式开放验收通过。

## 第11轮复审的生命周期与线程边界

Web Access rotate/disable、端点设置刷新与替换，在同一 manager owner 锁内退休旧
socket 的 controller authority，并清除旧 generation、binding 与 inbound。首次启动
先安装 lane 再创建 endpoint，不清除这条新 lane；迟到旧 reader/sender/Drop 不得清新绑定。
Windows 真实 reader 回归同时执行实际 refresh/stop、重新 start 与新 socket 路由，
退休后的 recv 丢弃此前缓冲帧；EOF 以捕获 operation 收口等待配对及已连接 worker，
不会留下 busy，也不会清理后继 operation。业务同步突变持 B owner 与精确 transport
authority 门禁，退休/挂起以同一 authority 锁为线性化点，不能穿过已准入的突变。
门禁内不得 await 或回调重入生命周期/transport/Web manager；仅允许已审的窄同步
突变及非重入的遥测入队。响应构造只持 B owner；
逐帧发送仍由精确 transport 验权，不以响应构造扩大业务准入。独立 transport 的
停止以 lease writer 退休完成为边界，不宣称 cancellation 信号发出即零在途操作。
仅使用私有临时目录和 loopback；CI 的非零匹配执行清单必须包含其 app 宿主 driver。
源码 CI 契约只能验证接线，不能替代实际 Windows Wry 回归结果。

A 端 reserve/commit/cancel/abandon 的完整同步操作交给最多4个并发阻塞工作线程，
不得在 IPC 或异步执行器内联读取凭证及落盘。等待名额时取消尚未启动任何操作；
阻塞线程仍先校验 feature gate、启动 Ready 标记与受管状态，缺失时失败关闭；
四入口及纯元数据遥测 helper 泛型化仅用于真实 mock IPC 回归，不改变运行时策略。
已经启动的持久提交不因客户端丢弃响应中断。prepare 不持服务级 ephemeral 锁；
完成后重新核对当前映射、完整活跃连接、pending/retired ID 与容量，再由宿主在
认证 manager/lane 门禁中写入唯一登记库。attempt-before-send 与未知结果合同不变。

A 重连后的权威状态读取只在当前 connect intent、完整租约及未退休身份下替换
终态留下的旧 claim；普通事件不能借此绕过 claim 守卫。B 显式断开在单次持锁中
核对捕获的 operation 与完整 lease 后提交终态，解锁后通知捕获旧 generation 的
清理；加密 lane 与出站 sender 也只清捕获旧 lease/socket binding，不误伤后继连接。
运行时开关的非 Unicode 值失败关闭，不等同于变量未设置。

本轮将 RemoteProxy 256条/16MiB 无生命周期问题重新确认为 P1，不能再沿用R15的
非阻塞容量定级。用户已确认不可逆离线归档方案；本轮已实跑原生归档和隔离回归，
不能把 bank harness 或浏览器 mock 通过当作 A08 全部通过。原 #1423 的损坏恢复边界
继续跟踪，不因本轮提供离线容量出口而关闭整张 Issue。

归档只接纳无未决任务、且不属于当前连接/恢复租约的记录；由 manager 生命周期
owner 锁内取身份，并持锁完成 Store 封存。管理候选列表包含已隐藏的活动记录，
不复活普通历史；归档正文只在独立只读入口分页浏览，不能重新握手、派发或进入
本地 Engine/ACP。活动记录仍限256条，活动 JSON 仍限16MiB，永久 client/task 两域
反重放墓碑进入 SQLite 索引，不淘汰历史 ID。

独立 anchor 不占活动文件预算，包含 archive UUID、单调封存水位和至多一条有界
pending 封存记录。pending 原子持久化即不可撤销：启动先恢复精确记录与全部墓碑，
再接纳任何远控变更；数据库提交后中断活动 JSON 清理时，只剔除精确相同的残留。
缺库、缺 anchor、异库和同 UUID 旧备份低于水位均失败关闭，不重新建空库。Windows
先 pin 文件且拒绝 delete sharing，再打开 SQLite；协作进程由 fd-lock 序列化。POSIX
前后 inode 与当前路径 UUID/水位校验不宣称抵抗具备本地写权限的恶意 ABA 替换，
也不承诺抵抗整个用户 profile 连同 anchor 协同回滚；文件系统/断电持久化边界沿用
既有宿主原子写和 SQLite FULL，不把进程中断回归当作物理断电实验。

持久化用 compact JSON，与1MiB投影验证使用相同序列化；按实际候选字节准入，并
预留未决任务的投影、转义后的退休 epoch、cursor、fingerprint/终态墓碑增长。预留
按最大未来大小减当前字段占用，可随正常进展消耗。旧v3仅校验当前16MiB/schema；
超出新预留预算时拒绝新增，仍允许不增加预留总量的结算、放弃与离线归档。
归档或 IO 歧义失败不恢复执行权；未知任务必须先对账或明确放弃从未发送的任务。

### 同一旧 head 的追加评审：真实入口与旧测试接口

追加审核 `5483780511` 提到旧 `ControllingSessionManager::task_intent_with_model`
只增长的64项任务表。精确旧 head `8c4cae42` 与当前该文件相同；全仓调用核实及
独立审核均确认三个旧 task intent 方法只被 `cfg(test)` 模块调用，旧 renderer handler
不注册。真实 A 提交走 `RemoteDispatchService::reserve_new` → `commit_prepared`，
宿主 `DispatchPort::prepare` 直接构造 payload；取消走 `cancel_task` →
`send_bound_cancel` → 仍在生产使用的 `build_bound_task_cancel_payload`。
因此不能认定当前产品第65次任务会命中这张旧表。旧接口本身的累计容量与无消费者
维护问题仍存在，复用 #1743；不为死接口新增生产淘汰策略，也不混淆实际发件箱、
B任务台账与永久反重放身份的各自容量合同。

归档 UI 的实际组件浏览器冒烟使用 mock bridge：覆盖隐藏候选、在线/未决拒绝、
永久完成、只读正文和附件名，以及封存后列表刷新失败不误报为封存失败。
同一 smoke 纳入前端 CI 全量执行清单；不等价于原生 Store、Tauri IPC、真实 socket
或双机发布验收。封存完成后的列表异常提示已完成、待刷新，不自动重新派发任务。

### 本轮原生执行与故障隔离

Windows 11 上以 GNU/LLVM 编译当前应用 lib test，测试产物单独嵌入与 tauri-build
相同的 Common-Controls v6 manifest，并在测试进程 PATH 加入生成的 WebView2 DLL
目录；不改产品构建、CI 门禁或仓库文件。未嵌入时进程启动失败，不能算测试执行。
实际使用生产 Store、SQLite、文件 pin、Tauri mock IPC 和真实 Wry/socket driver。
当前六个互不重叠过滤器共966 PASS / 0 FAIL / 0 IGNORE：sessions 268、desktop_remote
命令115、app reader driver 2、remote_control 572、behavior_telemetry 5、capabilities 4。
不是 MSVC、Linux/macOS、在线模型、双机公网或物理断电验收。

首次 Store 回归17项中5项失败：session_kind 对普通本地 ID 也查询归档权威，使坏
RemoteProxy 文件阻断本地会话分类。已把归档查询限定到 remote- 域，原五项重新通过；
新增缺库后本地 create/load/kind/ACP route/save/delete 仍正常、远控失败关闭且不建
空库的回归，正常封存后拒绝 forged SavedSession 保存、删除和远控 mutation。
已有坏缓存测试另锁定本地 Engine route。此隔离修复独立源码审核通过，不恢复旧
档案执行权，不删除坏缓存、anchor 或永久反重放墓碑。

前端最终全量 Node 复跑1422 PASS / 0 FAIL / 3平台条件 SKIP；指定本机真实 Python
与 Windows PowerShell 模块目录仅作用于该测试进程，不降低断言或持久修改环境。
实际归档组件 Chrome mock-bridge smoke、smoke selector 12项、架构守卫、CI策略
36项、版本一致性、fork-fast、Rust fmt 与 UI build 均通过；ESLint quiet 零错误，
不据此宣称既有 warning 已消失。独立 bank/core harness 474 PASS / 1既有 IGNORE，
使用实际生产 archive/SQLite/文件 pin，但最小 Session ID 校验 seam 不代表完整 Store。

追加事实复核不沿用宽泛指控：x25519-dalek 默认启用 zeroize；非 Windows 的真实
桌面 connect/start 在平台 gate 失败关闭，纯 capability builder 常量不证明生产广告
可达；ACP 常规取消/超时有精确 owner settle，未调用旧 release 不证明普通取消泄漏。
真实残余另以非阻塞 Issue 跟踪：并行审批/已决文案、下载末块重试及前端聚合预算、
部分 Rust 契约输入缺 frontend CI 路由、共享出站普通控制帧公平性，以及共享 B socket
v2 error 的登记诊断。Shutdown 独立 cancel，不泛化为无法停机；正常下载已有32MiB/
4096块与摘要校验，不泛化为整链无界或摘要绕过。

ChatView 重试提交跨 composer identity 退休后，transfer guard.finish 为 false 可以
跳过全组件 remoteDispatchAction 的清理；独立审核用实际函数、guard 与 case helper
作内存执行确认 UI busy 残留。按 P3 可用性登记，不再称不可达，不混为认证漏洞或
导航读取意图问题；后续需精确 owner 收尾与真实组件迟到 Promise 回归。
