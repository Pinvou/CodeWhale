# 平台打包目录

这里仅保存可审查的安装器模板、签名工具入口和构建期 staging 脚本；大型运行时不得直接提交到本目录。

```text
packaging/
├─ linux/
│  └─ deb/                 # desktop、postinst、prerm
├─ macos/                  # 后续放置 entitlements、公证和 DMG 定制
└─ windows/
   ├─ wix/                 # MSI/WiX 模板和 fragments
   ├─ nsis/                # NSIS 模板、hooks、环境脚本和 staging
   ├─ runtime/             # 独立 runtime 校验、展开与 Tauri overlay 生成
   └─ signing/             # 发布签名实现
```

公共构建编排位于 `../../scripts/tauri/`：

- `platform-config.js`：只负责选择当前平台 overlay。
- `windows-runtime.js`：触发独立 runtime staging 并读取统一 descriptor，不感知安装器细节。
- `windows-installer.js`：按 bundle 目标准备 NSIS 专属资源。
- `codex-bridge.js`：准备应用隔离的 Codex ACP Bridge 运行时。
- `knowledge-host.js`：准备 pinvou-knowledge-host-helper 运行时资源。
- `build.js`：组合平台配置、运行时与安装器 staging，并启动 Tauri CLI。

以上清单以 `scripts/tauri/` 目录为准。

Windows NSIS 构建会把第三方预签名运行时收集到 `target/release/nsis-stage/resources`，
再通过生成的 NSIS hook 直接安装，避免 Tauri 将这些资源当作首方产物重复调用签名工具。
主程序、卸载程序和安装器仍沿用 Tauri 的首方签名流程。

工具市场不在构建期注入共享 API Key。Windows 运行时从公开、锁定的独立仓库取得，不依赖跨仓库凭据；
WoSign 凭据仍由签名脚本从仓库外的本机配置读取。

平台脚本不得修改其他平台的资源树；所有生成物必须写入 `src-tauri/target/`。Windows
安装包通过 `private-runtimes/windows` 的锁定 gitlink 合入运行时，协议和初始化方式见
[`docs/windows-private-runtime-submodule.md`](../../../docs/windows-private-runtime-submodule.md)。
