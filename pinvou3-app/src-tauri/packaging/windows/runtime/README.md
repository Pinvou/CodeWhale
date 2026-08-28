# Windows 独立运行时

Windows 大型运行时存放在独立的公开仓库，不直接提交到主仓库的 `resources/` 或 `packaging/`。主仓库只保留：

- `../../../config/platforms/windows/runtime/x86_64.lock.json`：锁定 runtime submodule commit 和 manifest SHA-256；
- `scripts/resolve-runtime.ps1`：校验 submodule、manifest、ZIP 内部清单，原子 staging 并生成 runtime descriptor；
- `scripts/stage-runtime.ps1`：供构建入口调用的轻量包装；
- `scripts/init-submodule.ps1`：初始化 runtime submodule checkout；
- `scripts/stage-onnx-runtime.ps1`：为开发启动展开 ONNX Runtime 组件；
- `scripts/test-onnx-runtime.ps1`：验证 ONNX Runtime staging 产物。

发布机构建前执行：

```powershell
npm run runtime:windows:init
npm run runtime:windows:validate
npm run runtime:windows:stage
npm run test:windows-runtime-manifest
```

Windows desktop development requires a one-time `npm run runtime:windows:init:onnx`.
It initializes the pinned runtime checkout but downloads only the ONNX Runtime
Git LFS object. `npm run dev` then verifies and expands only that component into
`target/windows-runtime/<commit>-<manifest-sha>-onnx-dev/` and reuses the
fingerprinted result on later starts.

`runtime:windows:init` 只在当前 checkout 与主仓库 gitlink 不一致时更新 runtime submodule；gitlink 未变化时直接复用。
submodule checkout 会显式禁用 LFS smudge，随后检查受 LFS 管理的实际文件，只有仍存在 pointer 时才按路径执行
`git lfs pull`。LFS 对象通过命令级 `lfs.storage` 保存在 `target/windows-runtime/lfs-storage`，不得把
`.git/modules/private-runtimes/windows/lfs/objects` 独立恢复到全新 workspace。脚本输出
`pinvou3-windows-runtime-v5-<commit>-<lock-sha-prefix>` 形式的 Jenkins 缓存键；lock 或缓存 schema 变化时不会复用旧缓存。

校验内容包括 submodule commit、gitlink、origin URL、工作树状态、manifest SHA-256、文件大小与 SHA-256，以及受管理 ZIP 解压后的逐文件清单。`stagedFiles` 对应 payload 已复制、各组件已展开且按需分发的 ASR 主模型已移除，派生文件尚未生成且 payload 尚未清理的生命周期快照；缺失或多余文件都会导致校验失败。完整 staging 还会自动枚举所有声明 Windows Python 依赖的内置 MCP，核对暂存基础 Python ABI、target 绑定的锁 hash 及每个 wheel 的 PEP 425 标签（不预装第三方包），避免升级 Python 后沿用不兼容 wheel 锁。

首次完整校验后，脚本会在 `target/windows-runtime/.verified-lock` 记录 runtime commit、manifest SHA-256、lock 文件
SHA-256、目标平台和 resolver schema。staging 同时生成包含全部暂存文件路径、大小和 SHA-256 的 `.stage-manifest.json`，
其摘要绑定到 staging marker，但这两份缓存元数据只用于快速拒绝和诊断。后续构建必须按主仓库 lock 锁定的 runtime manifest
`stagedFiles` 重新校验完整文件集合与内容哈希，并校验 runtime descriptor 和 overlay；全部一致时才跳过重复展开与复制。
任一输入或暂存产物变化、使用 `-Force`，都会重新执行源文件 SHA-256 校验并原子重建 staging。同一工作区的 resolver
调用通过 `target/windows-runtime/.resolver.lock` 串行化，避免并发覆盖缓存。

ASR 安装资源只内置 `fsmn-vad.gguf`、wrapper 和 SenseVoice backend；`sensevoice-small-*` 识别模型由应用按需下载，不进入安装包。

验证后的运行时写入：

```text
src-tauri/target/windows-runtime/<commit>-<manifest-sha>/
```

生成的 Tauri overlay 位于 `src-tauri/target/windows-runtime/tauri.generated.conf.json`，构建工具路径和安装器输入由同目录的
`runtime-descriptor.json` 描述。ASR 主模型采用首次启用时下载策略，不进入安装资源映射；descriptor 将 VC Runtime 指向
已验证的 stage payload，再由 NSIS installer adapter 按目标原子复制。公共 `tauri.conf.json` 不引用大型运行时资源，因此未初始化 runtime submodule 时仍可执行 `cargo check`。

`PINVOU3_WINDOWS_RUNTIME_ROOT` 只用于本地迁移或故障排查，且目标仓库的 commit 必须与 lock 完全一致；正式 CI 和发布始终使用 submodule 默认路径。
