# pinvou3 安装说明（本地 LLM 版）

> 本包提供 **arm64 (aarch64)** 与 **LoongArch64（dpkg 架构 `loong64`，新世界）** 两个 Linux 架构的安装包：
> - **arm64**：Ubuntu 22.04 及以上（glibc 2.35+，WebKitGTK 2.40+），典型设备如 NVIDIA Jetson、Raspberry Pi 5、Apple Silicon Linux VM 等；
> - **LoongArch64**：UOS V25（2500 版本）或 deepin 23 及以上（glibc 2.38+，WebKitGTK 2.40+）。
>
> 旧世界（old-world）龙芯系统（UOS V20 龙芯版、Loongnix 20、麒麟 V10 时代）**不受支持**：这些系统没有 Tauri 2 依赖的 webkit2gtk-4.1/libsoup3。
> 更老的发行版（如基于 Ubuntu 20.04 的 Jetson L4T）缺少所需 glibc 符号，安装后无法运行。
> 默认连接 **本机 127.0.0.1:8000** 的 vLLM 服务，**不依赖外网**。

---

## 1. 安装

安装包按架构发布（以下示例以 `<版本>` 占位，请替换为实际下载的版本号）：

| 架构 | 发布资产名 | 系统基线 |
|------|------------|----------|
| arm64 (aarch64) | `pinvou3_<版本>_arm64.deb` | Ubuntu 22.04+（glibc 2.35+） |
| LoongArch64 (loong64) | `pinvou3_<版本>_loong64.deb`（由 release-packages.yml 的 build-linux-loongarch64 job 交叉构建；tauri-bundler 无法处理 loongarch64，deb 由 `scripts/package-deb-loongarch64.sh` 按 bundler 布局手工组装，`scripts/release-deb.sh` 不适用于 loong64） | UOS V25 / deepin 23+（glibc 2.38+） |

```bash
# <arch> 按下载的安装包替换为 arm64 或 loong64
sudo dpkg -i pinvou3_<版本>_<arch>.deb
# 若报依赖缺失，自动补装：
sudo apt-get install -f
```

应用内 OTA 覆盖安装仅安装 Pinvou 的必需依赖，不会因缺少 LibreOffice、OCR 等推荐工具而阻断升级。若 Ubuntu 软件源索引过期导致依赖下载出现 404、哈希不一致等仓库错误，安装器会自动刷新索引并重试一次；不会执行系统升级，也不会自动修复与 Pinvou 无关的软件包。

安装包会声明 Tauri UI 运行时依赖，并推荐以下文档处理工具：
- `libwebkit2gtk-4.1-0`、`libgtk-3-0`（Tauri UI 运行时）
- 推荐（非强制）：`poppler-utils`、`tesseract-ocr`、`tesseract-ocr-chi-sim`、`pandoc`、`p7zip-full`、`python3`（文档/图片处理工具）、`libreoffice`、`libemail-outlook-message-perl`

### UOS（统信）注意事项

- UOS V25 已自带本包所需的 WebKitGTK（webkit2gtk-4.1）与 glibc 运行库，安装 deb 前无需额外准备依赖。
- UOS 可能要求应用经过签名：第三方未签名 deb 在默认策略下会被拦截安装。遇到拦截时，需先在系统设置中开启**开发者模式**（Developer Mode），或改用统信开发者证书签名后的安装包 / 应用商店分发渠道。
- LoongArch64 版的已知能力差异（arm64 版不受影响）：
  - 连接器 CLI（钉钉 / 飞书 / 企业微信等）上游暂未发布 loong64 安装制品，本包因此不附带连接器锁定文件：pin 校验与托管安装不可用，已在系统 PATH 中的连接器命令行工具仍可正常使用。
  - 本地向量嵌入在编译期关闭，知识库语义检索自动退化为全文检索：建库、导入、挂载与检索（全文）在 loong64 上完整可用；界面中的本地嵌入模型下载入口会显示「当前平台暂不支持本地向量嵌入模型」。
  - 官方 Codex / Claude / Kimi CLI 原生安装器均仅覆盖 x86_64 与 arm64，loong64 上应用内安装不可用；系统 PATH 中的同名二进制仍可使用。
  - 语音识别（SenseVoice 引擎）已随包提供（loong64 交叉构建，启用 ggml LASX）。

---

## 2. 启动本地 LLM（vLLM）

pinvou3 默认向本机 `http://127.0.0.1:8000/v1` 发送请求。接收方需**先自行启动 vLLM**，模型名建议包含 `_256k` 后缀（底座据此派生 256K 上下文窗口）。

示例启动命令（供参考，请按实际环境调整）：

```bash
# 假设模型路径为 /opt/models/qwen3.6-35b-a3b-fp8
vllm serve /opt/models/qwen3.6-35b-a3b-fp8 \
  --served-model-name qwen36_35b_256k \
  --max-model-len 262144 \
  --tensor-parallel-size 1 \
  --gpu-memory-utilization 0.95 \
  --enable-auto-tool-choice \
  --tool-call-parser qwen3_coder \
  --reasoning-parser qwen3
```

关键约束：
- Qwen3.6 官方建议使用 `vllm>=0.19.0`；旧版本可能不支持对应模型或无法识别下述 parser 参数，遇到未注册错误时请先升级 vLLM。
- `--served-model-name` **推荐** 设为 `qwen36_35b_256k`（或至少包含 `_256k`），底座据此派生 256K 上下文窗口与 compaction 阈值。
- `--enable-auto-tool-choice --tool-call-parser qwen3_coder` **必须**带上：缺了 vLLM 不会把模型输出解析成标准 `tool_calls`，工具调用会直接失效或漂移。注意 Qwen3.6 的 tool-call parser 名是 `qwen3_coder`；其他模型可能使用 `hermes` 等不同 parser，升级或更换模型时必须按对应模型卡核对。`qwen3` 是 `--reasoning-parser` 的注册名，写给 `--tool-call-parser` 会因未注册而启动失败。
- `--reasoning-parser qwen3` 建议按官方模型卡带上，由服务端把 thinking 内容解析到独立字段；缺省时 `<think>` 内容可能泄漏进正文。思考深度档位（reasoning effort）现由品悟按模型配置透传（SavedModel.reasoning_effort），不再使用 `DEEPSEEK_REASONING_EFFORT` 环境变量。
- 若使用其他模型名（如 `Qwen2.5-72B-Instruct`），底座也能识别 Qwen 系列并派生 128K 窗口；如仍想获得 256K 阈值，请在模型名中附加 `_256k` 后缀。
- 若 vLLM 绑在其他端口（如 `8080`），见下节「自定义后端地址」。

---

## 3. 启动 pinvou3

图形界面启动方式（任选其一）：

```bash
# 命令行
pinvou3

# 或从桌面菜单查找 "PINVOU 智能助手"
```

首次启动会自动在 `~/.pinvou3/` 下创建配置目录并解包内置技能。

---

## 4. 自定义后端地址与模型（可选）

### 方式 A：环境变量（临时 / 开发调试）

```bash
export DEEPSEEK_BASE_URL="http://192.168.1.100:8000/v1"
export DEEPSEEK_API_KEY="local-no-auth"   # 无鉴权时保持此值
export DEEPSEEK_MODEL="qwen36_35b_256k"
pinvou3
```

环境变量优先级最高，适合 run-dev.sh 或临时切换。

### 方式 B：应用设置（持久化，推荐）

打开「设置 → 模型与后端」，新增或编辑模型，填写模型名、API 地址和密钥后设为当前模型。应用会把模型保存到 `advanced.saved_models`，密钥单独存入系统凭证库；不要把明文密钥手写进 `settings.json`。

支持的模型预设：
- `local_vllm` — 默认本地 qwen36_35b_256k（无需改配置）
- `openai_compatible` — OpenAI 官方 / 兼容 API（如 GPT-4o、自托管 proxy、自定义本地 vLLM）
- `deepseek` — DeepSeek 官方 API
- `kimi` — Moonshot / Kimi
- `qwen` — 通义千问
- `doubao` — 豆包（火山方舟）
- `minimax` — MiniMax
- `glm` — 智谱 GLM
- `mimo` — 小米 MiMo

需要用环境变量持久覆盖时，也可将方式 A 的变量写入启动脚本；桌面菜单启动通常不会读取 `~/.bashrc`。

---

## 5. Windows 软件更新（Windows 版）

Windows 与 macOS 共用静态 `latest.json` 更新源，并读取
`platforms["windows-x86_64"]` 中独立的版本、URL、SHA-256、大小和更新说明：

1. 查询更新：获取 `https://pinvou.com/pinvou3/latest.json`，只接受当前平台条目，不回退到顶层 Linux 包。
2. 下载更新：直接将签名服务器生成的 NSIS `.exe` 或 MSI 下载到 `~/.pinvou3/updates/`。
3. 完整性校验：严格校验清单大小和 SHA-256，校验通过后再将临时文件原子提交为正式安装文件。
4. 安装前复验：再次核对路径、扩展名、文件大小和 SHA-256。
5. 启动安装：MSI 使用系统被动安装；NSIS 使用 `/P /UPDATE`。Windows 弹出 UAC 后，当前 Pinvou 进程退出，安装成功后启动新版。

正式 Windows 安装器由独立签名服务器构建并签署，再按
`docs/windows-static-update-release.md` 人工发布。应用不读取 BIOS SN、不依赖专用 OTA
服务，也不上传升级结果。`PINVOU3_HOME` 仍可重定位用户数据和更新暂存目录。

Linux `.deb` 更新链路继续使用顶层清单字段、SHA-256 和
`pkexec apt-get install`；macOS 使用 `platforms["macos-universal"]`。

---

## 6. 卸载

```bash
sudo apt remove pinvou3
```

用户数据（`~/.pinvou3/`）不会自动删除，如需清理：

```bash
rm -rf ~/.pinvou3
```

---

## 7. 外发文件清单

| 文件 | 说明 |
|------|------|
| `pinvou3_<版本>_arm64.deb` | 主安装包（arm64） |
| `pinvou3_<版本>_loong64.deb` | 主安装包（LoongArch64） |
| `INSTALL.md` | 本安装说明 |
