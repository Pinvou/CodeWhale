# Tauri 平台配置

`../tauri.conf.json` 是所有平台共享的唯一基础配置。本目录只保存按目标平台叠加的 overlay 和构建锁：

```text
config/
└─ platforms/
   ├─ linux/
   │  ├─ tauri.conf.json
   │  ├─ x86_64/tauri.conf.json
   │  └─ aarch64/tauri.conf.json
   ├─ macos/tauri.conf.json
   └─ windows/
      ├─ tauri.conf.json
      ├─ signing.wosign.conf.json
      └─ runtime/x86_64.lock.json
```

- `tauri.conf.json`：对应平台的安装包目标、资源映射和安装器参数；macOS 额外把主窗口覆盖为原生顶栏（`decorations: true` + `titleBarStyle: "Overlay"` + `hiddenTitle`，系统红绿灯替代前端自绘三键）。
- Linux 的架构 overlay 把统一入口从固定 SenseVoice.cpp commit 构建到
  `target/linux-asr-runtime/<arch>/` 的当前架构 ELF 映射到 `runtime/asr/`；源码树不存放预编译 ELF。
- `signing.*.conf.json`：发布签名 overlay，仅由对应发布流程显式加载。
- `runtime/*.lock.json`：锁定 Windows 独立 runtime 的 submodule commit、manifest、VC++ 最低版本和目标架构；实际资源映射由 staging 后生成的 overlay 提供，不存放制品本身。

`--config` overlay 按 JSON Merge Patch 合并：对象合并、**数组整体替换**。
macOS overlay 中的 `app.windows` 是基础配置主窗口定义的完整拷贝，改动基础配置 `app.windows` 字段时必须同步。Linux 的隐藏启动 overlay 由 `scripts/tauri/startup-window-config.js` 从基础配置动态生成，开发和 packaging 共用，不重复维护窗口宽高等字段。

`scripts/tauri/build.js` 根据当前操作系统在 **build / bundle** 时加载 `platforms/<os>/tauri.conf.json`
（Linux 的 dev 与 packaging 都会额外注入动态隐藏启动 overlay，Windows dev 不注入，见 `tests/tauri_effective_config.test.js`）；macOS
例外：原生顶栏定义在 overlay 里，因此 dev（`npm run dev` 与 `run-dev.sh` 两条入口）都会以 `--config`
带上同一份 overlay，保证 dev 与打包产物顶栏一致。公共配置不得引用平台专属安装器模板、签名工具或直接硬编码大型平台运行时。

Windows 例外由统一构建入口先验证并 staging `private-runtimes/windows`，再加载
`target/windows-runtime/tauri.generated.conf.json`；Node、VC++ Runtime 和 ASR 模型交付策略通过同目录的
`runtime-descriptor.json` 交给构建适配层。未初始化或锁不匹配时明确失败，不会静默生成缺少运行时的安装包。

项目内所有 `build` / `bundle` 命令必须通过该包装器执行；`npm run tauri -- build`、
`npm run build`、`npm run build:msi` 和 NSIS 脚本均遵循同一入口。包装器将自动 overlay
放在显式签名或 staging overlay 之前，并在 `target/tauri-config/<platform>/` 生成：

- `effective-config.json`：按 Tauri JSON Merge Patch 顺序得到的有效配置；
- `installer-resources.manifest.json`：已验证存在、目标路径无冲突的最终资源文件清单。

基础配置的 `beforeBuildCommand` / `beforeBundleCommand` 会拒绝没有包装器标记的构建。
不要直接运行 `npx tauri build` 或 `npx tauri bundle`，否则命令会在打包前明确失败。

## Linux 交叉构建（`--target`）

架构 overlay 跟随构建入口传入的 `--target <三元组>`（`--target X` 与 `--target=X`
两种写法都认，最后一次出现生效），而不是宿主架构；不传 `--target` 时与原先一样按
宿主架构。Linux 宿主交叉构建到 `build.js` 不认识的机器段会直接失败，不会静默回落
宿主架构。一个例外：Linux 宿主自身的架构就不在表里（x86_64/aarch64 之外）时同样
回落，因为这时构建无从判断是不是交叉。有两个侧车只能按**宿主**架构备料，交叉构建
必须显式跳过两者，否则 `build.js` 在任何备料开始前就停止：

- `PINVOU3_SKIP_LINUX_ASR=1`：跳过 SenseVoice 运行时（它只能在目标架构上原生
  构建），并从架构 overlay 里摘掉它的资源声明；
- `PINVOU3_SKIP_KNOWLEDGE_HOST=1`：跳过共享知识库服务（它的 cargo build 不认
  `--target`），并从平台 overlay 里摘掉 `knowledge-host/`。

只有值 `1` 会打开开关。产出的包不带 SenseVoice 引擎和共享知识库 server。Codex ACP
Bridge 按目标架构备料（`PINVOU3_BRIDGE_TARGET_ARCH`），npm 由第二份宿主架构的
Node 运行。
