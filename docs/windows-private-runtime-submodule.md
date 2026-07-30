# Windows 私有运行时 submodule

## 目标

Windows 分支只维护代码、安装器模板和 submodule gitlink。Poppler、Tesseract、Python、
Node.js、Pandoc、ONNX Runtime、ASR、7-Zip 和 VC Runtime 存放在私有仓库：

```text
https://github.com/Pinvou/pinvou3-windows-runtime.git
```

主仓库不保存这些二进制，也不在基础 `tauri.conf.json` 中硬编码私有资源路径。

## 目录和版本锁定

主仓库引用：

```text
private-runtimes/windows  -> 私有仓库的确定 commit
```

私有仓库的 `payload/` 按组件保存资源。Poppler、Tesseract、ASR、7-Zip 分别使用一个
确定性 ZIP；Python、Node.js、Pandoc、ONNX Runtime 继续使用各自的上游组件包；
VC Runtime 保持独立文件。二进制由 Git LFS 管理。

`windows-runtime.manifest.json` 当前锁定 9 个仓库级资源、四个组件 ZIP 内部
112 个 7-Zip/Poppler/Tesseract/ASR 文件，以及 2145 个最终 staging 文件的路径、大小和 SHA-256。
staging 会先校验 ZIP，解压后再按该 manifest 中的可信最终清单逐文件复核。ASR 安装资源只内置
`fsmn-vad.gguf`；SenseVoice 识别模型由应用按需下载，不进入 Windows 安装包。

主仓库 `pinvou3-app/src-tauri/config/platforms/windows/runtime/x86_64.lock.json` 再锁定：

- submodule URL、路径和 commit；
- 私有 manifest 路径及 SHA-256；
- 目标平台 `windows-x86_64`。

因此版本完整性由三层保证：主仓库 gitlink、主仓库 lock、私有文件级 manifest。缓存中的
`.stage-manifest.json` 和 `.verified-lock` 只用于快速拒绝和诊断；缓存能否复用最终由主仓库 lock
锁定的私有 `stagedFiles` 清单决定，缓存元数据不能自行构造信任。

## 初始化和构建

```powershell
cd pinvou3-app
npm run runtime:windows:init
npm run runtime:windows:validate
npm run runtime:windows:stage
npm run build:msi
```

私有 submodule 配置为 `update = none`，普通的递归 submodule 初始化只处理公共底座，
Windows 构建机通过 `runtime:windows:init` 显式覆盖该策略并拉取 Git LFS 对象。

### Jenkins 子模块准备

Jenkins 不应执行无路径限制的 `git submodule sync` 或 `git submodule update --init --recursive`。构建阶段只准备实际需要的两个子模块：

```powershell
git submodule update --init --checkout -- CodeWhale
npm --prefix pinvou3-app run runtime:windows:init
npm --prefix pinvou3-app run runtime:windows:validate
```

`CodeWhale` 当前没有嵌套 submodule，因此不需要 `--recursive`。`runtime:windows:init` 会比较主仓库 gitlink 与本地 runtime
commit；更新 checkout 时显式禁用 LFS smudge，一致且工作树中不存在 LFS pointer 时，跳过 submodule update 和
`git lfs pull`。脚本输出的
`PINVOU3_WINDOWS_RUNTIME_CACHE_KEY=pinvou3-windows-runtime-v5-<commit>-<lock-sha-prefix>` 可作为 Jenkins 缓存键，
缓存外置 Git LFS objects 和 `pinvou3-app/src-tauri/target/windows-runtime` 中已验证的 staging。缓存 schema、gitlink 或
主仓库 runtime lock 任一变化都会生成新键。

恢复缓存前可直接从主仓库 gitlink 取得缓存键，不会访问 runtime 远端：

```powershell
npm --prefix pinvou3-app run runtime:windows:cache-key
```

取得缓存键后，Jenkins 只恢复 `pinvou3-app/src-tauri/target/windows-runtime`，再执行本节开头的 submodule 准备命令。
其中 `lfs-storage/` 是脚本通过命令级 `lfs.storage` 使用的外置 Git LFS 对象库，其余目录保存已验证、已展开的安装资源。
不得把 `.git/modules/private-runtimes/windows/lfs/objects` 作为独立缓存路径恢复到全新 workspace；这会提前创建不完整的
submodule gitdir，导致后续 `git submodule update --init` 无法初始化私有 runtime。

恢复的 staging 会按私有 manifest 中由主仓库 lock 锁定的完整文件集合和 SHA-256 清单重新核验；缓存损坏时会先重新校验
私有源文件，再原子重建 staging，不能把缓存本身或同缓存内的元数据视为可信发布输入。

仓库脚本只负责生成缓存键和验证恢复结果，不负责 Jenkins Job 的 restore/save 配置；构建平台必须显式配置上述缓存路径。
GitHub Actions 当前也未跨 workflow run 持久化私有 runtime，只能复用单次构建工作区内的验证与 staging 结果。

主仓库 checkout 的 refspec、tag 获取和 shallow clone 由 Jenkins SCM 插件控制，不在仓库脚本内。发布 Job 应启用
`Honor refspec on initial clone`、`No tags`，并把 refspec 限制为实际构建分支或 MR ref；不要在每次构建中抓取全部分支和 tag。

GitHub Actions 构建 Windows NSIS 时，需要配置仓库或组织 Secret
`PINVOU3_WINDOWS_RUNTIME_TOKEN`。该凭据只需对主仓库和
`Pinvou/pinvou3-windows-runtime` 具有 `Contents: read` 权限。未配置时，
`release-packages` 只运行 Windows 打包契约测试，不会声称或上传 NSIS 产物。

`scripts/tauri/build.js` 是项目内 `tauri build` / `tauri bundle` 的统一入口：Windows
构建前自动执行 staging，所有平台都会加载对应 config overlay，并在调用 Tauri CLI 前
生成有效合并配置和安装包资源清单。不要直接运行 `npx tauri build/bundle`。
基础 Tauri 配置的构建/打包钩子会拒绝未经过包装器的调用。

迁移验证阶段可设置 `PINVOU3_WINDOWS_RUNTIME_ROOT` 指向相同 commit 的本地私有仓库；
正式发布不得用它绕过主仓库锁定的 submodule。

普通开发和检查不需要私有仓库：

```powershell
cd pinvou3-app/src-tauri
cargo check
```

## 增量更新资源

1. 在临时目录展开并修改对应组件；
2. 使用 `scripts/pack-component.ps1` 重新生成对应的确定性 ZIP；
3. 执行 `scripts/update-manifest.ps1`；
4. 提交并推送私有仓库，只有变更组件产生新的 LFS 对象；
5. 主仓库更新 submodule gitlink；
6. 更新主仓库 lock 中的 commit 和 manifest SHA-256；
7. 从干净 checkout 验证 staging 和安装包。

不得使用 `git submodule update --remote` 参与发布构建，也不得只更新 gitlink 而不更新
lock。若 Git LFS 没有拉取成功，resolver 会识别 pointer 文件并在打包前失败。

## 多平台扩展

macOS 私有运行时应使用独立路径和仓库，例如：

```text
private-runtimes/windows
private-runtimes/macos
```

这样 Windows 与 macOS 更新不会争用同一个 gitlink，主线仍只处理平台无关代码和少量
平台适配配置。
