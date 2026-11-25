# CI 上传、缓存与下载网络策略

本策略处理共享网络层的失败，不改变产品测试覆盖、Runner 配额或既有 Job 超时。

## 上传与缓存入口

所有工作流中的 artifact 上传/下载、`actions/cache`、Rust 缓存及可能启用缓存的语言 setup Action，必须在紧邻的准备步骤中运行 `scripts/ci-network-node-options.cjs`，再仅给该 Action 设置生成的 `NODE_OPTIONS`。包括 Action 的 post-job 缓存保存；业务测试不继承这些选项。已有 Node 内存等选项保留，显式更高的 TLS 最低版本与兼容策略冲突时失败，不降低最低安全版本。

存储 Action 暂用经过证书和主机名验证的 TLS 1.2。此前同一 Azure Blob 主机和 IP 的对照出现 TCP 成功、TLS 1.3 握手停滞而 TLS 1.2 成功；其他 IP 也出现两者均失败，因此这不是对全部网络错误的统一归因。上游或出口修复后应通过对照恢复默认协商，而非永久扩散此兼容选项。Python artifact 客户端仅对 Azure Blob 设置这一上限，其他 HTTPS 服务保持默认现代协商。

`scripts/ci-network-policy.cjs` 扫描全部工作流，拒绝遗漏、条件不匹配、向业务进程泄漏选项及未审查的新上传/缓存 Action。Windows 缺少 PATH 中的归档工具时，仅给存储 Action 添加既有 Git 安装中的归档工具目录，不写入全局 `GITHUB_PATH`，不改变业务测试的工具选择。实际缓存保存可能仅警告而保持绿色，因此必须验证恢复命中和完整字节。

诊断仅记录有界的阶段、计时、状态码和安全分类，不记录签名 URL、授权头、载荷、代理凭据或本地路径。HTTP 核心客户端有阶段信息；其他客户端不保证每个底层事件可见，不把缺少事件解释成没有故障。

`CI Network Transport` 工作流在 Linux、Windows 实际保存及恢复小缓存，上传并下载小工件，核对被测提交及完整字节。每轮缓存使用独立精确 key。工件名称包含操作系统和运行尝试次数，只保留一天。下载只使用该作业本次上传返回的工件 ID。缺少 ID 或无法取得该工件时，探测会失败并保留各次尝试的运行记录。它是传输验证，不替代产品测试；无需新增服务或配置密钥。

## 下载与本地缓存

固定工具、交叉工具链、Deepin Packages/软件包和 PipeWire 源码经 `scripts/ci-download.py` 下载：严格 HTTPS、验证重定向协议、总预算内有限瞬态重试、临时文件后原子发布。证书、认证、404 或 SHA-256 不符直接失败，不关闭证书校验，也不通过延长 Job 超时掩盖问题。

下载器解析真实 curl 可执行文件，不依赖 PowerShell 的同名 alias。Windows 服务账号若未把 curl 加入 PATH，可从 PATH 中 Git 安装的相邻目录或系统目录解析已安装二进制（包含 32 位解释器的 Sysnative 情况）；不改写业务 PATH，也不下载未知替代客户端。工具确实不存在时显式失败。

代理 CONNECT 的状态与目标 HTTP 状态分别采集。代理返回 502/503 时，curl 可能以 22/56 退出而目标状态仍为 000；此时在原总预算和次数上限内重试，并记录 `proxy-connect` 阶段。代理认证/策略拒绝和证书错误仍立即失败；不将其当作瞬态错误，也不记录代理地址或原始 stderr。

只有调用方提供的独立预期 SHA-256 才能进入内容寻址缓存。缓存命中重新计算哈希；损坏对象不可使用；可变 Release 元数据每次联网刷新。Deepin 软件包仍使用原有 Release → Packages → 软件包哈希链；本变更没有新增 InRelease 的 GPG 验证能力，Release 的来源认证沿用严格 HTTPS。

优先使用 `PINVOU_CI_DOWNLOAD_CACHE`，否则使用 Runner 工具缓存下的专属目录。每目录默认上限 2 GiB、单对象上限 1 GiB，保留至少 2 GiB 空间；并发发布使用可随进程退出释放的系统锁。满容量或竞争时跳过缓存发布，已验证的本次下载仍可使用；不删除历史产物。发布、复制和网络请求仍受空间及大小约束。管理员可预热经过独立 SHA-256 验证的对象，不能把未知下载内容直接当可信缓存。

Rust 工具链使用官方源，避免继承故障镜像；既有每实例独立的 Cargo/Rustup home 保留。`scripts/ci-network-environment.py` 只撤销已知的 `rsproxy-ci` 替换片段，保留无关配置并备份原文件；自定义注册表不擅自改写，混合来源策略需人工检查。Linux 服务器在下一 Job 的预启动钩子执行，不改写运行中编译的配置。各工作流还在 Rust 初始化前经 `scripts/ci-rust-network.cjs` 调用同一迁移，包括继承了旧镜像环境的 Windows Listener；无需为修复下载源而中断运行中的 Listener。

Rustup 本身不继承存储 Action 的 Node TLS 策略，且 Runner 可能把官方源列在 `NO_PROXY` 中。CI 的 Rust 准备入口因此启动本 Job 所有的 loopback 分发缓存：后台下载器只访问固定官方 HTTPS origin，先认证并读取 SHA-256，再校验 manifest/组件内容；组件进入上述内容缓存，Rustup 自己的摘要校验也保留。每个 Job 首次重新认证 channel 的 SHA，不跨 Job 冻结 floating stable；同一 Job 固定其 channel 快照，避免 SHA 与 manifest 来自不同更新时刻。

官方 Rust 后台下载采用与现场对照验证一致的 TLS 1.2 兼容策略，保留证书和主机名验证；不改变业务 HTTP 客户端的协议。下载总预算不延长。新缓存应通过真实 Rustup 的全新 home 冷安装和第二个全新 home 的暖安装验证，不能只验证下载器自身：manifest、cargo、std、rustc 都须经过校验路径，暖安装须有相应内容命中。

此 listener 不接受任意 URL、凭据或写请求，只有官方 dist 路径；不改写 manifest、工具链版本、组件或安装目录。只有后台官方源下载器收窄其自身的 `NO_PROXY`，业务进程的公共源绕行策略不变。Rustup 到 loopback 的明文段只在本机，外网段仍严格验证 HTTPS。监听器保留 Runner tracking/Windows Job Object，随 Job 清理；最长两小时作为额外回收边界。安全的下载阶段、命中及失败类型保存在 `PINVOU_CI_RUST_DIST_STATE` 同目录的有界 `statistics.json`，不保存原始错误、签名 URL 或认证信息。

全部 Rust 初始化入口（包括手写的 Windows 安装步骤）随后以 `always()` 输出筛选后的诊断快照，使失败 Job 的临时目录被 Runner 回收前仍有阶段证据。报告只输出固定字段、数值错误码和受限枚举，不打印原始异常或环境；报告不可改变原安装步骤的判定。上传配置准备保留 main 的严格阻断和必须成功的复用证据，只有后续诊断上传可非阻断；产品测试和资源回收始终阻断。Job 级预算与短步骤预算分开校验。

已有 artifact 准备入口通过 `ci-artifact-node-options.cjs --network` 组合公共策略，保留旧 CLI 单值输出和原步骤身份；新增选项才输出公共 Action 参数和归档工具路径，不重复运行两套配置步骤。缓存及其他 Action 使用公共准备入口。原产品门禁和 main 的上传隔离负向测试保留；测试辅助投影先运行完整公共策略守卫，只把已校验的传输扩展映射为旧断言的词汇，任意业务命令、额外环境、条件或失败策略修改仍必须失败。

## APT 与代理部署

`scripts/ci-apt-prepare.py --apply` 只处理启用的 LLVM 源，验证仓库内官方公钥的 SHA-256、主指纹和签名子密钥，然后在原 Signed-By 范围内合并证书。它不改变源、关闭签名检查或把其他 keyring 的身份复制到新的范围。公钥更新必须重新从官方源取证、核对身份及摘要并运行回归；不能按错误信息下载任意密钥。

`scripts/ci-egress-proxy.py` 是现有本机 CONNECT 代理的可部署实现，不会由工作流自行安装。仅监听 loopback，允许 HTTPS 443，保持既有 GitHub 域名的验证型 DoH 路由；不解密 TLS。修复两向流的半关闭处理和 HTTP 头缓冲吞掉提前发送数据的问题，并合并同域名的并发 DNS 查询。连接、缓存、缓冲及诊断均有界。代理日志中的请求类别和字节数可验证是否转发完整，但不能证明每次历史 TLS/EOF 都来自同一个缺陷。

Windows Schannel 的证书链补全、OCSP 和 CRL 查询有独立的系统/用户代理路径，不能只检查包管理器的 `HTTPS_PROXY`。既有受限证书 HTTP 桥接应复用同一 relay、`PKI_HOSTS` 和提前校验的 `PKIRedirect` 策略，覆盖实际证书链使用的 GlobalSign 等端点；仅中转有界签名对象，客户端仍验证签名、信任链和有效期，不关闭吊销检查。验证时使用实际 Runner 身份；管理员交互账号的直连结果不能替代它。历史错误缓存可能是返回 HTTP 200 的拦截 HTML，需核对 DER/签名和对应 URL，不能把状态码当成功或清空所有缓存。

部署前保存旧脚本和服务属性，使用独立空闲 loopback 端口验证新实现；只对本次拥有的规则进行连接切换，不刷新宿主机/Docker 网络规则，不新建浏览器可见网卡。保留旧连接直至排空；若需要维护切换，记录准确时段，避免把由维护造成的断连计作原始故障。失败时恢复原脚本及本次规则，既有服务身份、Runner 数量和配额不变。

切换前检查 systemd 的双向依赖。Runner 若通过 `Requires=` 依赖代理，显式 `systemctl restart` 会传播停止事务并重启 Runner；即使当前代理连接只剩 Listener，也不能据此认定切换无影响。支持 `RestartMode=direct` 的主机应先用独立测试服务验证自动恢复不会停掉依赖者，再配合原有自动重启策略执行进程级切换；此模式不保护显式的服务停止/重启事务。前后核对全部 Runner 容器的启动时间、数量及配额，不能只看服务已恢复或 PID 变化。已中断的任务应通过实际 Runner 断联记录单独登记及补跑，不能归因给原网络问题。

## 描述符容量与 Action 初始化

Action 源码下载发生在工作流准备步骤之前；仅让上传/缓存探针通过，不能证明 Runner 的初始化链路稳定。代理每条 CONNECT 至少使用客户端、上游和 selector 三个描述符。必须同时核对 `LimitNOFILESoft`、硬上限以及 `/proc/<pid>/limits`；硬上限很高不代表软上限足够。生产默认软上限 1024 会在并发时提前耗尽，造成 DNS/连接在 0–1 毫秒内失败并返回 502，而不是外网连接超时。

1024 隧道部署预算使用 `LimitNOFILE=8192:524288`，并保证代理服务的 `TasksMax` 高于隧道和控制线程总数，例如 2048；这些是代理进程预算，不是 Runner CPU/内存配额。保存持久服务配置后，可用 `prlimit` 在保留 PID 和已有连接的情况下修正运行进程的软上限。核对全部容器启动时间，避免为调整软上限显式重启代理及其依赖者。

代理启动按实际软上限保守计算容量，在解析 HTTP 头、创建线程前准入；低预算下受控返回 503，并给 accept/DNS/日志留出描述符，而不是耗尽后使所有上游一起 502。诊断保留安全的 DNS/TCP/本地容量阶段和数值 errno，不打印异常原文。隔离复现应包括低软上限的失败、低预算保护及正确预算下的并发完整传输，不能只在空闲时验证一个连接。

## 验证

复用的 Actions 工作区可能残留只有 Git 元数据、但 HEAD 不能解析成 commit 的 CodeWhale 子模块；`git submodule sync/update` 会在有机会获取目标对象之前失败。这是本地准备状态问题，不是更改被测 gitlink 的理由。`ci-prepare-codewhale.sh` 在确认工作区身份、全部可变元数据归属（包括 HEAD）、公开源 URL 及唯一 indexed gitlink 后，仅对该状态获取原 indexed commit 并做 detached checkout，再继续原有递归准备和清理。只需代码树时保持浅获取；审计要求历史时仍完整验证历史。目标不可达就失败，不替换为 origin/main/HEAD，不删除共享对象缓存，不修复外部元数据或普通开发工作区。

运行网络策略守卫、`scripts/tests/ci-artifact-*.test.cjs` 和 `test_ci_{download,https,egress_proxy,apt_prepare,network_environment}.py`，以及 frontend bundle、结果复用、发布契约回归。Linux shell/符号链接契约在 Linux 验证；Windows 原生传输通过实际工作流验证。对照保留同一来源、TLS 策略、冷/暖下载耗时、哈希与服务器代理阶段日志，不能以一次重跑绿灯代替根因证据。
