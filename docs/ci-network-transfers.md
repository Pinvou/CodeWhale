# CI 上传、缓存与下载网络策略

本策略处理共享网络层的失败，不改变产品测试覆盖、Runner 配额或既有 Job 超时。

## 上传与缓存入口

所有工作流中的 artifact 上传/下载、`actions/cache`、Rust 缓存及可能启用缓存的语言 setup Action，必须在紧邻的准备步骤中运行 `scripts/ci-network-node-options.cjs`，再仅给该 Action 设置生成的 `NODE_OPTIONS`。包括 Action 的 post-job 缓存保存；业务测试不继承这些选项。已有 Node 内存等选项保留，显式更高的 TLS 最低版本与兼容策略冲突时失败，不降低最低安全版本。

存储 Action 暂用经过证书和主机名验证的 TLS 1.2。此前同一 Azure Blob 主机和 IP 的对照出现 TCP 成功、TLS 1.3 握手停滞而 TLS 1.2 成功；其他 IP 也出现两者均失败，因此这不是对全部网络错误的统一归因。上游或出口修复后应通过对照恢复默认协商，而非永久扩散此兼容选项。Python artifact 客户端仅对 Azure Blob 设置这一上限，其他 HTTPS 服务保持默认现代协商。

`scripts/ci-network-policy.cjs` 扫描全部工作流，拒绝遗漏、条件不匹配、向业务进程泄漏选项及未审查的新上传/缓存 Action。诊断仅记录有界的阶段、计时、状态码和安全分类，不记录签名 URL、授权头、载荷、代理凭据或本地路径。HTTP 核心客户端有阶段信息；其他客户端不保证每个底层事件可见，不把缺少事件解释成没有故障。

`CI Network Transport` 工作流在 Linux、Windows 实际保存及恢复小缓存，上传并下载小工件，核对被测提交及完整字节。每轮缓存使用独立精确 key，工件只保留一天。它是传输验证，不替代产品测试；无需新增服务或配置密钥。

## 下载与本地缓存

固定工具、交叉工具链、Deepin Packages/软件包和 PipeWire 源码经 `scripts/ci-download.py` 下载：严格 HTTPS、验证重定向协议、总预算内有限瞬态重试、临时文件后原子发布。证书、认证、404 或 SHA-256 不符直接失败，不关闭证书校验，也不通过延长 Job 超时掩盖问题。

只有调用方提供的独立预期 SHA-256 才能进入内容寻址缓存。缓存命中重新计算哈希；损坏对象不可使用；可变 Release 元数据每次联网刷新。Deepin 软件包仍使用原有 Release → Packages → 软件包哈希链；本变更没有新增 InRelease 的 GPG 验证能力，Release 的来源认证沿用严格 HTTPS。

优先使用 `PINVOU_CI_DOWNLOAD_CACHE`，否则使用 Runner 工具缓存下的专属目录。每目录默认上限 2 GiB、单对象上限 1 GiB，保留至少 2 GiB 空间；并发发布使用可随进程退出释放的系统锁。满容量或竞争时跳过缓存发布，已验证的本次下载仍可使用；不删除历史产物。发布、复制和网络请求仍受空间及大小约束。管理员可预热经过独立 SHA-256 验证的对象，不能把未知下载内容直接当可信缓存。

Rust 工具链使用官方源，避免继承故障镜像；既有每实例独立的 Cargo/Rustup home 保留。`scripts/ci-network-environment.py` 只撤销已知的 `rsproxy-ci` 替换片段，保留无关配置并备份原文件；自定义注册表不擅自改写，混合来源策略需人工检查。Linux 服务器在下一 Job 的预启动钩子执行，不改写运行中编译的配置。各工作流还在 Rust 初始化前经 `scripts/ci-rust-network.cjs` 调用同一迁移，包括继承了旧镜像环境的 Windows Listener；无需为修复下载源而中断运行中的 Listener。

## APT 与代理部署

`scripts/ci-apt-prepare.py --apply` 只处理启用的 LLVM 源，验证仓库内官方公钥的 SHA-256、主指纹和签名子密钥，然后在原 Signed-By 范围内合并证书。它不改变源、关闭签名检查或把其他 keyring 的身份复制到新的范围。公钥更新必须重新从官方源取证、核对身份及摘要并运行回归；不能按错误信息下载任意密钥。

`scripts/ci-egress-proxy.py` 是现有本机 CONNECT 代理的可部署实现，不会由工作流自行安装。仅监听 loopback，允许 HTTPS 443，保持既有 GitHub 域名的验证型 DoH 路由；不解密 TLS。修复两向流的半关闭处理和 HTTP 头缓冲吞掉提前发送数据的问题，并合并同域名的并发 DNS 查询。连接、缓存、缓冲及诊断均有界。代理日志中的请求类别和字节数可验证是否转发完整，但不能证明每次历史 TLS/EOF 都来自同一个缺陷。

部署前保存旧脚本和服务属性，使用独立空闲 loopback 端口验证新实现；只对本次拥有的规则进行连接切换，不刷新宿主机/Docker 网络规则，不新建浏览器可见网卡。保留旧连接直至排空；若需要维护切换，记录准确时段，避免把由维护造成的断连计作原始故障。失败时恢复原脚本及本次规则，既有服务身份、Runner 数量和配额不变。

## 验证

运行网络策略守卫、`scripts/tests/ci-artifact-*.test.cjs` 和 `test_ci_{download,https,egress_proxy,apt_prepare,network_environment}.py`，以及 frontend bundle、结果复用、发布契约回归。Linux shell/符号链接契约在 Linux 验证；Windows 原生传输通过实际工作流验证。对照保留同一来源、TLS 策略、冷/暖下载耗时、哈希与服务器代理阶段日志，不能以一次重跑绿灯代替根因证据。
