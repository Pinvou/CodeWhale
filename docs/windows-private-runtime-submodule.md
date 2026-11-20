# Windows 独立运行时 submodule

> 相关 Windows 接手/验收史料（2026-11 回填留档）：[`Windows迁移与维护接手手册.md`](./Windows迁移与维护接手手册.md) · [`Windows端功能清单与测试用例.md`](./Windows端功能清单与测试用例.md)。

## 目标

Windows 分支只维护代码、安装器模板和 submodule gitlink。Poppler、Tesseract、Python、
Node.js、Pandoc、ONNX Runtime、ASR 引擎、7-Zip 和 VC Runtime 存放在公开仓库：

```text
https://github.com/Pinvou/pinvou3-windows-runtime.git
```

主仓库不保存这些大型二进制，也不在基础 `tauri.conf.json` 中硬编码平台资源路径。
`private-runtimes/windows` 是为保持 gitlink、缓存和构建脚本兼容而保留的历史路径名，
不代表当前仓库可见性。

## 目录和版本锁定

主仓库引用：

```text
private-runtimes/windows  -> 公开运行时仓库的确定 commit
```

运行时仓库的 `payload/` 按组件保存资源。Poppler、Tesseract、ASR、7-Zip 分别使用一个
确定性 ZIP；Python、Node.js、Pandoc、ONNX Runtime 继续使用各自的上游组件包；
VC Runtime 保持独立文件。运行时仓库历史中的二进制仍由 Git LFS 管理；主仓库初始化
不下载 LFS 对象，而从公开的固定 Release 附件物化同一 manifest 所锁定的文件。

`windows-runtime.manifest.json` 当前锁定 9 个仓库级资源，并额外记录四个组件 ZIP 内部
112 个 7-Zip/Poppler/Tesseract/ASR 文件（2026-11 核验：7-Zip 5 + ASR 4 + Poppler 41 + Tesseract 62，按 gitlink `dcde8f01` manifest 计）的路径、大小和 SHA-256。staging 会先校验 ZIP，
解压后再逐文件复核内部清单。Windows 安装包不映射 `sensevoice-small-q8.gguf`，应用在用户首次启用本地语音识别时
下载并校验到用户目录；resolver 在复核归档后会从 staging 删除主模型和已解包的 payload，只保留 ASR 引擎与 VAD 模型。

主仓库 `pinvou3-app/src-tauri/config/platforms/windows/runtime/x86_64.lock.json` 再锁定：

- submodule URL、路径和 commit；
- runtime manifest 路径及 SHA-256；
- 目标平台 `windows-x86_64`。

因此版本完整性由三层保证：主仓库 gitlink、主仓库 lock、运行时文件级 manifest。

## 初始化和构建

```powershell
cd pinvou3-app
npm run runtime:windows:init
npm run runtime:windows:validate
npm run runtime:windows:stage
npm run build:nsis
```

运行时 submodule 配置为 `update = none`，普通的递归 submodule 初始化只处理公共底座，
Windows 构建机通过 `runtime:windows:init` 显式准备固定 gitlink 的源码和 manifest，
禁用自动 LFS smudge，然后匿名下载该 commit 对应的 `runtime-<完整40位commit>` Release 附件。
附件名称取 manifest 中资源路径的文件名，下载、缓存复用和物化均校验清单大小及 SHA-256；
不使用 `latest`，也不回退到 `git lfs pull`。旧 `-ForceLfsPull` 调用作为 `-ForceDownload` 的别名保留。
更新运行时前，需先在公开运行时仓库发布对应固定 commit 的 Release，上传 manifest 中的全部载荷附件；
验证附件内容与清单一致后再更新主仓库 gitlink/lock。缺附件或校验不符会阻止初始化，不得通过修改缓存元数据绕过。

### Jenkins 子模块准备

Jenkins 不应执行无路径限制的 `git submodule sync` 或 `git submodule update --init --recursive`。构建阶段只准备实际需要的两个子模块：

```powershell
git submodule update --init --checkout -- CodeWhale
npm --prefix pinvou3-app run runtime:windows:init
```

`CodeWhale` 当前没有嵌套 submodule，因此不需要 `--recursive`。`runtime:windows:init` 会比较主仓库 gitlink 与本地 runtime
commit；更新 checkout 时显式禁用 LFS smudge，一致时复用源码 checkout。随后按固定 manifest
校验 `release-assets/` 的内容寻址缓存，缺失或损坏时重新下载固定附件，再原子物化到 runtime 工作树。
脚本输出的
`PINVOU3_WINDOWS_RUNTIME_CACHE_KEY=pinvou3-windows-runtime-v6-<commit>-<lock-sha-prefix>` 可作为 Jenkins 缓存键，
缓存 `pinvou3-app/src-tauri/target/windows-runtime` 中已验证的 Release 附件和 staging。缓存 schema、gitlink 或
主仓库 runtime lock 任一变化都会生成新键。lock 文件缺失时直接失败，不输出缓存键。

同一工作区上的并发初始化与 staging 调用通过 `target/windows-runtime/.resolver.lock` 串行化；等待方最多阻塞 120 秒，
超时即失败退出，稍后重试即可（元数据缓存与 staging 复用让重试代价很低）。

Jenkins 的 `CheckoutSubmodule` 阶段只负责准备 submodule，不再单独执行 `runtime:windows:validate`。后续直接运行
`npm --prefix pinvou3-app run build:nsis`：统一的 `build.js` 入口负责 runtime 校验和 staging，读取
`runtime-descriptor.json`，再由 Windows installer adapter 为 NSIS 原子准备经过校验的 VC++ 引导程序。
`build:nsis` 的 stage 与 bundle 两个 npm 阶段各自会进入一次 runtime 校验（第二次命中已验证缓存，代价很小）。

恢复缓存前可直接从主仓库 gitlink 取得缓存键，不会访问 runtime 远端：

```powershell
npm --prefix pinvou3-app run runtime:windows:cache-key
```

取得缓存键后，Jenkins 只恢复 `pinvou3-app/src-tauri/target/windows-runtime`，再执行本节开头的 submodule 准备命令。
其中 `release-assets/` 按内容哈希保存经过大小与 SHA-256 复验的固定 Release 附件，其余目录保存已验证、已展开的安装资源。
旧 `lfs-storage/` 不是 v6 初始化的输入，无需恢复；缓存命中仍须按锁定 manifest 复验，不能自行信任缓存元数据。
不得把 `.git/modules/private-runtimes/windows/lfs/objects` 作为独立缓存路径恢复到全新 workspace；这会提前创建不完整的
submodule gitdir，导致后续 `git submodule update --init` 无法初始化 runtime。

完整源校验后，`.verified-lock` 记录源校验信息。Stage 命中时复核源文件大小，Validate 仍逐文件重算 SHA-256。
恢复的 staging 以主仓库 lock 锁定的 runtime manifest 为依据，核验完整文件集合、大小和 SHA-256。
若 staging 不能复用，先完整复验源文件，再原子重建。缓存内的元数据不能替代锁定清单。

仓库脚本只负责生成缓存键和验证恢复结果，不负责 Jenkins Job 的 restore/save 配置；构建平台必须显式配置上述缓存路径。
GitHub Actions 当前也未跨 workflow run 持久化 runtime，只能复用单次构建工作区内的验证与 staging 结果。

主仓库 checkout 的 refspec、tag 获取和 shallow clone 由 Jenkins SCM 插件控制，不在仓库脚本内。发布 Job 应启用
`Honor refspec on initial clone`、`No tags`，并把 refspec 限制为实际构建分支或 MR ref；不要在每次构建中抓取全部分支和 tag。

GitHub Actions 构建 Windows NSIS 时仍进入 `windows-release` Environment，作为正式发布边界；
job 自身只允许 `main`，Environment 可按发布策略进一步配置 protected branch 和 required reviewers。
运行时仓库已公开，不需要仓库级、组织级或 Environment Secret。checkout 设置
`persist-credentials: false`，初始化脚本按锁定 URL、gitlink 和 manifest 匿名准备源码并下载固定 Release 附件。

本地可运行 `npm --prefix pinvou3-app run test:windows-runtime-cache` 验证缓存及固定 Release 物化行为（下载由本地夹具替代，不代表真实 HTTP 验证）。两个直接 Windows PowerShell 5.1 测试入口会在使用内置命令前按宿主恢复进程内的模块路径，支持从 PowerShell 7 终端直接调用；无需预先清空 `PSModulePath`，不修改用户或机器级配置。PowerShell 7 入口及正常 5.1 默认路径保持不变。

所有 PR（包括同仓 PR）只运行不拉取大型运行时的 Windows 打包契约测试。只有 `main` 的发布链路
push 或在 `main` 上执行的 `workflow_dispatch` 才能进入 `windows-release` 构建正式 NSIS；
运行时缺失、gitlink 不一致，或 Release 附件无法取得或未通过大小与 SHA-256 校验时仍会明确失败，避免产生不完整发布。

`scripts/tauri/build.js` 是项目内 `tauri build` / `tauri bundle` 的统一入口：Windows
构建前自动执行 staging，所有平台都会加载对应 config overlay，并在调用 Tauri CLI 前
生成有效合并配置和安装包资源清单。不要直接运行 `npx tauri build/bundle`。
基础 Tauri 配置的构建/打包钩子会拒绝未经过包装器的调用。

### Rust toolchain check (Windows)

Before any Windows `tauri dev`, `build` or `bundle`, `build.js` runs
`scripts/ci/ensure-rust-toolchain.ps1 -CheckOnly` against the build account's
rustup. When the toolchain channel configured in `src-tauri/rust-toolchain.toml`
(currently the floating `stable`) provides `cargo`, `rustc`, `clippy-driver`,
`cargo-clippy`, `rustfmt`, `cargo-fmt` and a populated rust-std target
libdir, it is reused read-only. Otherwise the build switches to an isolated
`RUSTUP_HOME` (`pinvou3-app/.cache/rustup/<channel>-<host-triple>`, overridable
with `PINVOU3_RUSTUP_HOME`) and repairs the toolchain there, trying a
configured `RUSTUP_DIST_SERVER` first, then the rsproxy and USTC mirrors,
and the official Rust source last, with a bounded timeout per install attempt
and an overall repair budget (`-RepairTimeoutSeconds`, 1500 s by default).
The mirror-first ordering serves the population #619 targets (machines where
the official source stalls); set `RUSTUP_DIST_SERVER` to the official URL to
get official-first with mirror fallback. Note the integrity model differs
from #619's runtime downloads: those carry pinned SHA-256 sums, while rustup
verifies components only against the channel manifest served by the same
mirror, so mirror trust rests on HTTPS to the mirror operator. The script
refuses to modify the account's shared `~/.rustup`, a filesystem root, any
non-empty directory that lacks its `.pinvou3-managed-rustup` marker, or a run
without an isolated `RUSTUP_HOME`. If a repair, reset, or the budget runs out,
the error names the isolated `RUSTUP_HOME`, which is always safe to delete and
re-run. The probes only check presence and health, so under the floating
`stable` channel a reused isolated toolchain stays at the stable it was
installed with until a probe fails or the home is deleted; account-home
`rustup update` and CI installs do not refresh it. `npm run
test:windows-rustup-repair`
exercises this path natively on Windows, including the incomplete `-CheckOnly`
classifications and a removed rust-std. While Tauri runs, `build.js` logs the
CLI PID, phase and a heartbeat every minute.

For every Windows `dev`, `build` and `bundle`, `build.js` also compiles (or
reuses, while it is not older than its source) `src-tauri/scripts/rustc-stack-wrapper.exe`
and passes it to the Tauri CLI through `RUSTC_WRAPPER`, so `npm run dev` from
PowerShell gets the same 16 MiB compiler stack as `run-dev.sh` and CI without
leaking `RUST_MIN_STACK` into the app. A recompilation writes a temporary
output and renames it into place (the shell selector compiles the same way),
so an interrupted build cannot poison the cache. An explicit caller
`RUSTC_WRAPPER` is kept (the log states whether it is this stack wrapper or a
foreign one such as sccache that forgoes the stack fix); a missing wrapper
source or a failed compile stops the build.

resolver 只负责验证、展开运行时并生成 `target/windows-runtime/runtime-descriptor.json`；
`scripts/tauri/windows-installer.js` 只在目标包含 NSIS 时消费 descriptor 中的 VC Runtime。
Codex Bridge 同样从 descriptor 取得已锁定 Node，不反向解析 Tauri 资源映射。

迁移验证阶段可设置 `PINVOU3_WINDOWS_RUNTIME_ROOT` 指向相同 commit 的本地运行时仓库；
正式发布不得用它绕过主仓库锁定的 submodule。

普通开发和检查不需要初始化运行时仓库：

```powershell
cd pinvou3-app/src-tauri
cargo check
```

## 增量更新资源

1. 在临时目录展开并修改对应组件；
2. 使用 `scripts/pack-component.ps1` 重新生成对应的确定性 ZIP；
3. 执行 `scripts/update-manifest.ps1`；
4. 提交并推送运行时仓库，只有变更组件产生新的 LFS 对象；
5. 为该 commit 发布 `runtime-<完整 commit>` Release，上传 manifest 中的全部原名附件，并逐项核验大小与 SHA-256；
6. 主仓库更新 submodule gitlink；
7. 更新主仓库 lock 中的 commit 和 manifest SHA-256；
8. 从干净 checkout 验证 staging 和安装包。

上述 `scripts/pack-component.ps1` 与 `scripts/update-manifest.ps1` 位于
`private-runtimes/windows` 运行时子模块内，父仓库中没有这两个文件，不要在父仓查找。

不得使用 `git submodule update --remote` 参与发布构建，也不得只更新 gitlink 而不更新
lock。若缓存和下载都不能提供符合 manifest 大小与 SHA-256 的固定 Release 附件，初始化会失败；残留的 LFS pointer 仍会被 resolver 在打包前拒绝。

## 多平台扩展

macOS 平台运行时应使用独立路径和仓库，例如：

```text
private-runtimes/windows
private-runtimes/macos
```

这样 Windows 与 macOS 更新不会争用同一个 gitlink，主线仍只处理平台无关代码和少量
平台适配配置。
