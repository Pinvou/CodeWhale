# 平台打包目录

这里仅保存可审查的安装器模板、签名工具入口和构建期 staging 脚本；大型或私有运行时不得提交到本目录。

```text
packaging/
├─ linux/
│  └─ deb/                 # desktop、postinst、prerm
├─ macos/                  # 后续放置 entitlements、公证和 DMG 定制
└─ windows/
   ├─ wix/                 # MSI/WiX 模板和 fragments
   ├─ nsis/                # NSIS 模板、hooks、环境脚本和 staging
   ├─ runtime/             # 私有 runtime 校验、展开与 Tauri overlay 生成
   └─ signing/             # 发布签名实现
```

公共构建编排位于 `../../scripts/tauri/`：

- `platform-config.js`：只负责选择当前平台 overlay。
- `windows-runtime.js`：触发私有 runtime staging 并读取统一 descriptor，不感知安装器细节。
- `windows-installer.js`：按 bundle 目标准备 NSIS 专属资源。
- `build.js`：组合平台配置、运行时与安装器 staging，并启动 Tauri CLI。

工具市场不再在构建期注入共享 API Key；Windows WoSign 凭据仍由签名脚本从仓库外的本机配置读取。

平台脚本不得修改其他平台的资源树；所有生成物必须写入 `src-tauri/target/`。Windows
安装包通过 `private-runtimes/windows` 的锁定 gitlink 合入运行时，协议和初始化方式见
[`docs/windows-private-runtime-submodule.md`](../../../docs/windows-private-runtime-submodule.md)。
