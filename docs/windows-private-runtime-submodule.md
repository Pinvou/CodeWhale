# Windows 独立运行时 submodule

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
VC Runtime 保持独立文件。二进制由 Git LFS 管理。

`windows-runtime.manifest.json` 当前锁定 9 个仓库级资源、四个组件 ZIP 内部
112 个 7-Zip/Poppler/Tesseract/ASR 文件，以及 2145 个最终 staging 文件的路径、大小和 SHA-256。
staging 会先校验 ZIP，解压后再按该 manifest 中的可信最终清单逐文件复核。ASR 安装资源只内置
`fsmn-vad.gguf`；SenseVoice 识别模型由应用按需下载，不进入 Windows 安装包。

主仓库 `pinvou3-app/src-tauri/config/platforms/windows/runtime/x86_64.lock.json` 再锁定：

- submodule URL、路径和 commit；
- runtime manifest 路径及 SHA-256；
- 目标平台 `windows-x86_64`。

因此版本完整性由三层保证：主仓库 gitlink、主仓库 lock、运行时文件级 manifest。缓存中的
`.stage-manifest.json` 和 `.verified-lock` 只用于快速拒绝和诊断；缓存能否复用最终由主仓库 lock
锁定的 `stagedFiles` 清单决定，缓存元数据不能自行构造信任。

## 初始化和构建

```powershell
cd pinvou3-app
npm run runtime:windows:init
npm run runtime:windows:validate
npm run runtime:windows:stage
npm run build:nsis
```

运行时 submodule 配置为 `update = none`，普通的递归 submodule 初始化只处理公共底座，
Windows 构建机通过 `runtime:windows:init` 显式覆盖该策略并拉取 Git LFS 对象。

### Jenkins 子模块准备

Jenkins 不应执行无路径限制的 `git submodule sync` 或 `git submodule update --init --recursive`。构建阶段只准备实际需要的两个子模块：

```powershell
git submodule update --init --checkout -- CodeWhale
npm --prefix pinvou3-app run runtime:windows:init
```

`CodeWhale` 当前没有嵌套 submodule，因此不需要 `--recursive`。`runtime:windows:init` 会比较主仓库 gitlink 与本地 runtime
commit；更新 checkout 时显式禁用 LFS smudge，一致且工作树中不存在 LFS pointer 时，跳过 submodule update 和
`git lfs pull`。脚本输出的
`PINVOU3_WINDOWS_RUNTIME_CACHE_KEY=pinvou3-windows-runtime-v5-<commit>-<lock-sha-prefix>` 可作为 Jenkins 缓存键，
缓存外置 Git LFS objects 和 `pinvou3-app/src-tauri/target/windows-runtime` 中已验证的 staging。缓存 schema、gitlink 或
主仓库 runtime lock 任一变化都会生成新键。

Jenkins 的 `CheckoutSubmodule` 阶段只负责准备 submodule，不再单独执行 `runtime:windows:validate`。后续直接运行
`npm --prefix pinvou3-app run build:nsis`：统一的 `build.js` 入口只执行一次 runtime 校验和 staging，读取
`runtime-descriptor.json`，再由 Windows installer adapter 为 NSIS 原子准备经过校验的 VC++ 引导程序。

恢复缓存前可直接从主仓库 gitlink 取得缓存键，不会访问 runtime 远端：

```powershell
npm --prefix pinvou3-app run runtime:windows:cache-key
```

取得缓存键后，Jenkins 只恢复 `pinvou3-app/src-tauri/target/windows-runtime`，再执行本节开头的 submodule 准备命令。
其中 `lfs-storage/` 是脚本通过命令级 `lfs.storage` 使用的外置 Git LFS 对象库，其余目录保存已验证、已展开的安装资源。
不得把 `.git/modules/private-runtimes/windows/lfs/objects` 作为独立缓存路径恢复到全新 workspace；这会提前创建不完整的
submodule gitdir，导致后续 `git submodule update --init` 无法初始化 runtime。

恢复的 staging 会按 runtime manifest 中由主仓库 lock 锁定的完整文件集合和 SHA-256 清单重新核验；缓存损坏时会先重新校验
运行时源文件，再原子重建 staging，不能把缓存本身或同缓存内的元数据视为可信发布输入。

仓库脚本只负责生成缓存键和验证恢复结果，不负责 Jenkins Job 的 restore/save 配置；构建平台必须显式配置上述缓存路径。
GitHub Actions 当前也未跨 workflow run 持久化 runtime，只能复用单次构建工作区内的验证与 staging 结果。

主仓库 checkout 的 refspec、tag 获取和 shallow clone 由 Jenkins SCM 插件控制，不在仓库脚本内。发布 Job 应启用
`Honor refspec on initial clone`、`No tags`，并把 refspec 限制为实际构建分支或 MR ref；不要在每次构建中抓取全部分支和 tag。

GitHub Actions 构建 Windows NSIS 时仍进入 `windows-release` Environment，作为正式发布边界；
job 自身只允许 `main`，Environment 可按发布策略进一步配置 protected branch 和 required reviewers。
运行时仓库已公开，不需要仓库级、组织级或 Environment Secret。checkout 设置
`persist-credentials: false`，初始化脚本按锁定 URL、gitlink 和 manifest 匿名拉取运行时及
Git LFS 对象。

所有 PR（包括同仓 PR）只运行不拉取大型运行时的 Windows 打包契约测试。只有 `main` 的发布链路
push 或在 `main` 上执行的 `workflow_dispatch` 才能进入 `windows-release` 构建正式 NSIS；
运行时缺失、gitlink 不一致或 LFS 对象未物化时仍会明确失败，避免产生不完整发布。

`scripts/tauri/build.js` 是项目内 `tauri build` / `tauri bundle` 的统一入口：Windows
构建前自动执行 staging，所有平台都会加载对应 config overlay，并在调用 Tauri CLI 前
生成有效合并配置和安装包资源清单。不要直接运行 `npx tauri build/bundle`。
基础 Tauri 配置的构建/打包钩子会拒绝未经过包装器的调用。

### Rust 工具链检查（Windows）

在任何 Windows `tauri dev`、`build`、`bundle` 之前，`build.js` 都会对构建账号的
rustup 执行 `scripts/ci/ensure-rust-toolchain.ps1 -CheckOnly`。当
`src-tauri/rust-toolchain.toml` 配置的 channel（当前是浮动的 `stable`）能提供
`cargo`、`rustc`、`clippy-driver`、`cargo-clippy`、`rustfmt`、`cargo-fmt` 以及
非空的 rust-std target libdir 时，只读复用；否则构建切换到隔离的
`RUSTUP_HOME`（`pinvou3-app/.cache/rustup/<channel>-<host-triple>`，可用
`PINVOU3_RUSTUP_HOME` 覆盖）并在其中修复工具链：先尝试已配置的
`RUSTUP_DIST_SERVER`，再依次 rsproxy、USTC 镜像，官方 Rust 源兜底，每次安装
尝试有界超时，整体修复有预算（`-RepairTimeoutSeconds`，默认 1500s）。
镜像优先的顺序服务于 #619 面向的人群（官方源会僵死的机器）；把
`RUSTUP_DIST_SERVER` 设为官方地址即可回到官方优先、镜像兜底。注意完整性模型
与 #619 的运行时下载不同：后者携带固定的 SHA-256 校验，而 rustup 只用同一镜像
提供的 channel manifest 校验组件，因此镜像信任建立在到镜像运营方的 HTTPS 上。
脚本拒绝修改构建账号共享的 `~/.rustup`、文件系统根目录、缺少
`.pinvou3-managed-rustup` 标记的非空目录，以及没有隔离 `RUSTUP_HOME` 的运行。
修复、重置或预算耗尽时，报错会点名隔离的 `RUSTUP_HOME`——它永远可以安全删除
后重跑。探针只检查存在与健康，因此在浮动 `stable` channel 下，被复用的隔离
工具链会停留在安装时的 stable，直到某次探针失败或 home 被删除；账号 home 的
`rustup update` 与 CI 安装都不会刷新它。`npm run test:windows-rustup-repair`
在 Windows 原生验证这条路径，包括不完整工具链的 `-CheckOnly` 分类与被删除的
rust-std。Tauri 运行期间，`build.js` 每分钟记录一次 CLI PID、阶段和心跳。

对每次 Windows `dev`、`build`、`bundle`，`build.js` 还会编译（或在不超过其
源码 mtime 期间复用）`src-tauri/scripts/rustc-stack-wrapper.exe`，并经
`RUSTC_WRAPPER` 传给 Tauri CLI，使 PowerShell 里直接运行的 `npm run dev` 与
run-dev.sh、CI 一样拿到 16 MiB 编译器栈，且不会把 `RUST_MIN_STACK` 泄漏进
应用。重新编译会先写临时输出再原子改名（shell selector 以同样方式编译），
被中断的构建不会污染缓存。调用方显式指定的 `RUSTC_WRAPPER` 会被保留（日志会
说明它是本 stack wrapper 还是 sccache 之类放弃了栈修复的外来 wrapper）；
缺少 wrapper 源码或编译失败会终止构建。

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
5. 主仓库更新 submodule gitlink；
6. 更新主仓库 lock 中的 commit 和 manifest SHA-256；
7. 从干净 checkout 验证 staging 和安装包。

上述 `scripts/pack-component.ps1` 与 `scripts/update-manifest.ps1` 位于
`private-runtimes/windows` 运行时子模块内，父仓库中没有这两个文件，不要在父仓查找。

不得使用 `git submodule update --remote` 参与发布构建，也不得只更新 gitlink 而不更新
lock。若 Git LFS 没有拉取成功，resolver 会识别 pointer 文件并在打包前失败。

## 多平台扩展

macOS 平台运行时应使用独立路径和仓库，例如：

```text
private-runtimes/windows
private-runtimes/macos
```

这样 Windows 与 macOS 更新不会争用同一个 gitlink，主线仍只处理平台无关代码和少量
平台适配配置。
