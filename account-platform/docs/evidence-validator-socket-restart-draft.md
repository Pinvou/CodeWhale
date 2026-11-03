# 图片校验器 socket 重启设计草案

状态：**重启恢复仅设计、未实现和验证；校验器默认关闭**。本文不授权清理现有目录、启动工作负载或调整凭证保全策略。独立校验器候选由 [PR #2213](https://github.com/Pinvou/pinvou3/pull/2213) 跟踪，源码位于 `account-platform/cmd/evidence-image-validator/`，尚未接入 Billing 上传、Helm 或 Compose。

## 已观察的现状

候选使用专用 Unix 域 socket 和双向 UID 预期；socket 目录由 worker 所有，Billing 不应有目录写权限。当前 worker 在启动时要求 socket 路径不存在，监听后显式关闭 Go `UnixListener` 的自动 unlink，不会自动删除路径。历史 head `2fad692934c601f43a185662acd6e9ce36c06044` 的 [Linux amd64 CI job 122720332200](https://github.com/Pinvou/pinvou3/actions/runs/40659993285/job/122720332200) 已构建 PDF 镜像并完成基础 UID、样本、cgroup 配置和遗留 socket 拒绝验证；该结果不覆盖后续修改、arm64、下述重启恢复方案或生产隔离条件。

Go 1.26.6 `net/unixsock_posix.go` 中，`net.Listen` 创建的 UnixListener 默认在 Close 时按路径 unlink；源码本身注明该路径可能在关闭前被替换。候选通过 `SetUnlinkOnClose(false)` 避免该自动删除。`net.Listen` 最终调用系统 bind；当前在路径检查与 bind/`os.Chmod` 之间仍存在竞态。同一 worker UID 或特权主体能够干扰可写目录时，常规路径检查无法证明安全。

Go 默认收到 SIGTERM 时退出，当前候选不安装优雅退出处理，不能依赖 defer 执行。`SysProcAttr.Pdeathsig` 的文档明确其触发条件是父线程终止，可能早于父进程退出；不能仅凭它证明所有子进程在下一实例启动前已退出。`exec.CommandContext` 对在运行中的请求提供取消，但 SIGKILL、OOM、主机故障及容器终止仍需真实运行时测试。

## 首选的失败关闭恢复

优先在整个 Pod 或专属 worker 实例重建时提供新的专用 Memory emptyDir / 经验证的独立 tmpfs 目录，使新实例使用全新目录，不对不明内容执行删除。Billing 和该 worker 仅挂载各自同一个实例的目录；不能通过目录名、临时文件或套接字从旧实例推断授权。目录仅供 worker 写，Billing 仅有遍历和连接权限；双方 UID 不同、非 root，且这些 UID 不被其他容器复用。原旧目录的生命周期交给已经核验的 Pod / 专用挂载控制器，不能递归清理其他路径。

Kubernetes emptyDir 在 **容器重启时保留**，init container 也不因单个 worker 容器重启重新执行。因此，在当前候选不自动 unlink 的设计下，同一 Pod 内 worker 重启可能被旧 socket 阻塞；直接重启容器不等于得到新目录。应在设计中明确由受控流程重建整个 Pod，而不是声称会自动恢复。Pod 重建期间 Billing 对校验器不可用必须失败关闭，不能绕过图片校验。多个 Billing 副本各用本地专用目录；滚动更新或并行 Pod 可能使总 worker 数和资源总量增加，需单独限额与验证。

Compose 不能把任意 host bind 路径当作 tmpfs。需要外部受控流程先验证专属挂载的类型、路径、owner、组和权限；关闭旧实例并确认其容器及全部子进程已退出后，安全替换或销毁该专属目录，再启动新实例。若身份、挂载、进程或清理边界不明，则保留原状并拒绝启动。Compose profile 不能条件化已合并的 Billing 挂载；默认不加载该 overlay，候选没有部署接线。

## 可研究的同目录恢复方案

以下是候选设计方向，不是现有实现或已证明的原子清理协议：

1. 使用固定可信根与目录描述符逐级解析，限制符号链接和路径逃逸，并验证目录的设备、owner、类型和权限。可研究 Linux `openat2` 的 `RESOLVE_BENEATH` / `RESOLVE_NO_SYMLINKS`；若系统、内核或运行策略不支持，应失败关闭。部署必须确保 Billing 无目录写权限，目录与祖先不可由不可信主体替换。
2. 在固定目录内以 no-follow 和 close-on-exec 打开单链接、worker 拥有的固定 lock 文件，取得非阻塞排他 `flock`，直到 supervisor 退出保持锁；不删除或替换锁文件。子进程不得继承锁描述符。所有合法实例必须使用同一规则；锁争用或 lock 身份异常时拒绝启动。
3. 只有本实例成功 bind 后，在持锁状态记录固定、有限大小的 socket 身份标记（至少设备、inode、owner、类型和协议版本）。标记本身须是经 no-follow 校验的 worker 私有普通文件，并使用受控原子创建/替换；它不得包含凭证、原件或请求值。标记和 socket 应关联同一个经验证的目录。
4. 重启持锁后，仅检查该目录中的固定 socket 和标记；仅在二者身份完全匹配且能证明前实例及其子进程已停止时，才考虑相对 dirfd 删除。不存在标记、文件类型不符、字段损坏、身份不符、锁或进程状态不明时，保留原样并失败关闭。bind 成功但标记持久化前崩溃会留下无法证明的 socket，此时走专属目录重建，不能猜测清理。
5. Linux 没有常规的 `bindat`；可研究通过 `/proc/self/fd/<dirfd>/<fixed-name>` 绑定到已打开目录，但这是待验证方案，依赖 procfs 与内核/容器权限，须证明客户端按挂载路径访问到同一 socket，且不能用未经核实的 pathname 删除。`fstatat` 与 `unlinkat` 之间没有按 inode 比较并删除的原子条件，目录受保护是必要假设。

**单实例锁不能替代 inode 标记。** 锁只表示遵循相同锁协议的其他进程当前未持锁，不能证明目录内一个已有 socket 是本服务曾创建、不是其他文件，也不能证明其他不遵守协议的进程没有在使用它。失败的 connect 探测也不能证明 socket 已失效：例如监听队列、权限和运行时故障都可能导致连接失败。标记可以缩小误删范围，但 inode 重用、标记/路径替换以及检查到删除之间的竞态仍受信任模型限制；同 UID 进程、具备相关特权或 host root 的主体可破坏这些假设。若不能证明目录唯一、标记与退出身份，优先重建专属目录，不应把标记称为独立的强身份认证。

锁只约束 supervisor，并不自动证明前一个 decode child 已经退出。异常退出时即使锁被内核释放，旧 child 的信号处理和资源释放也可能尚未完成；新的 supervisor 不应据此声称解码并发一定仍为一。部署应使 supervisor 成为独立容器的受控主进程，并证明容器重启前旧容器 cgroup 的进程均已停止；否则拒绝新的 work 或重建完整隔离单元。仅依靠 Pdeathsig、PID 字符串或进程名不能避免 PID 复用，也不能证明跨容器资源已回收。

## 必须完成的 Linux / 容器验证

- 固定真实内核、容器运行时和镜像；验证专属 tmpfs/Memory emptyDir、subPath、owner、UID/GID 映射、只读 Billing 挂载，以及双向 SO_PEERCRED 的正确与错误 UID、同 UID、伪服务和无权限目录负例。
- 验证 lock 获取与 close-on-exec、并发启动、锁文件替换/符号链接、非 socket、owner/inode/设备不符、损坏或缺失 marker，确认未知对象不被删除；验证 openat2 和 `/proc/self/fd` 绑定在目标运行时有效及不支持时失败关闭。
- 分别注入 bind 后标记前崩溃、marker 完成后 SIGTERM、SIGKILL、worker OOM、子进程运行中 supervisor 死亡、节点重启和非协作同 UID 尝试；确认不误删、不出现额外解码进程，验证新实例无法在旧子进程仍运行时放行。
- 在 Kubernetes 验证单容器重启会保留 emptyDir、重建 Pod 后目录隔离、滚动更新与副本资源上限；在 Compose 验证独立 tmpfs、容器及子进程退出和受控目录重建。验证重建失败时 Billing 仍拒绝上传。
- 验证独立 cgroup 的 memory/cpu/pids、worker 无业务凭据、网络与 core/swap 限制，以及 4,000 万像素极端 PNG/JPEG 的峰值、超时和 OOM；2 GiB 仅为测试起点。渲染、交叉编译和 Windows Job 数据不能证明上述限制。

在上述证据及独立审核完成前，既不接入用户上传，也不声称可恢复或可交付。
