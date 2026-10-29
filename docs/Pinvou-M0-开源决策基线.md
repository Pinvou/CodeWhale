# Pinvou 3 开源 M0 决策基线

> 状态：Pinvou 组织已确认，执行中
> 决策版本：M0.4
> 首次确认：2026-07-23
> 本次修订：2026-10-25
> 目标版本：`v0.7.0`
> 公开仓库：`Pinvou/pinvou-agent`

## 1. 文档用途

本文用于 Pinvou 组织内部对齐 Pinvou 3 首次开源的产品范围、代码边界、仓库关系、发行方式和长期维护规则。

M0 已完成组织确认，用于固定“开源什么、保留什么、如何长期维护”。它不代表构建、签名和 Release 已完成；实时进度与未完成项以 [`PINVOU-开源执行代办.md`](./PINVOU-开源执行代办.md) 为准。

## 2. 开源目标

Pinvou 开源的主要目标是：

1. 建立开发者生态。
2. 提升 Pinvou 在国内外的产品知名度。
3. 通过完整可用的社区版为 Official 和 Enterprise 引流。
4. 在 GitHub 获得高质量 Stars、用户反馈和外部贡献。

社区版不是演示壳或刻意阉割版。除确实不能公开的客户专属内容、内部资产、凭证和无再分发授权内容外，Pinvou 现有通用能力默认全部开源。

## 3. 产品定位

英文定位：

> Pinvou is an open-source, local-first desktop AI workspace that turns conversations into finished work with files, knowledge, tools, and automations.

首发重点展示：

1. Agentic desktop workspace
2. Deliverable-first artifacts
3. Local knowledge
4. MCP and Skills ecosystem
5. Workflows and automations
6. Bring your own model
7. Inspectable and controllable execution

## 4. 仓库与许可证

### 4.1 仓库

- 社区主仓：`Pinvou/pinvou-agent`，公开。
- 现有内部仓：`Pinvou/pinvou3`，继续保持私有。
- 底座公开 fork：`Pinvou/CodeWhale`。
- CodeWhale 保留与上游 `Hmbown/CodeWhale` 的来源关系和授权说明。

公开仓库从清理后的文件快照创建全新根提交，不导入私有仓库原有的 commit、branch、tag、PR 和 Actions 历史。

### 4.2 仓库搜索与品牌表达

- 公开仓库使用 `Pinvou/pinvou-agent`；仓库名直接表达 Agent 类别，产品品牌仍统一为 `Pinvou`。
- README 当前标题使用 `Pinvou Agent`，首屏副标题直接说明它是 open-source desktop AI agent。
- GitHub Description 当前使用：`Open-source desktop AI agent for tools, files, knowledge, workflows, and real deliverables.`
- GitHub Topics 当前包含：`ai-agent`、`automation`、`desktop-app`、`llm`、`local-first`、`mcp`、`open-source`、`react`、`rust`、`tauri`。
- 品牌在普通文案、界面和产品名称中统一写作 `Pinvou`，避免被理解为缩写；仅环境变量、常量等技术标识按相应命名规范使用全大写。
- 产品版本名称统一为 `Pinvou Community`、`Pinvou Official` 和 `Pinvou Enterprise`；仓库 slug、CLI 和目录使用小写 `pinvou`。

### 4.3 许可证和品牌

- Community 源码和社区二进制采用 MIT。
- MIT 版权声明使用 `Copyright (c) 2026 Pinvou`。
- 公开维护者身份使用 `Pinvou organization`，不公开维护者个人姓名和个人邮箱。
- Pinvou 名称、Logo 和官方发行版身份由 `TRADEMARKS.md` 单独保护。
- Official 私有扩展不开源并保留所有权利，不因使用 MIT 社区核心而转为 MIT。

## 5. Community、Official 和 Enterprise

### Community

- 完整、可用的开源桌面产品。
- 对应公开 tag/commit，可从公开源码构建。
- 使用 GitHub Releases 获取社区安装包、更新和社区模型资产。
- 支持用户自带模型和自建服务。

### Official

- 由公开 Community core 加 Pinvou 私有扩展组成。
- 可包含官方服务配置和官方运营能力。
- 私有源码只存在于私有组织仓库。
- 可以将 Official 安装包作为独立资产上传到公开 GitHub Release，但必须明确标注其包含私有组件，不能由公开源码完整复现。

### Enterprise

- 在 Official 基础上提供企业组织管理、安全合规、私有化交付和企业支持。
- 未来企业专属能力可以保持私有，但不能以此为由移除社区版已有的通用能力。

## 6. 必须从公开仓库移除

### 6.1 客户和内部专属内容

- H3C EIP、H3C 知道、Windows 域环境自动引导和 SmartHub OTA。
- 相关 Skills、提示词、配置、源码、预编译二进制、测试、文档、品牌素材和内部业务场景。
- `/opt/.h3c/...` 等客户专属检测、目录、模型路径和启动方式。

MegaCube 产品入口、图标和集成经组织复核确认为 Official 私有能力（2026-10-25），社区版不包含，与 H3C 内容一同不进入公开仓库；此决定取代此前“社区版保留名称、图标、产品入口和通用集成”的口径。

### 6.2 凭证和基础设施

- API Key、Token、Cookie、OAuth/SSO 凭证、Webhook Secret、SSH 密钥和签名凭据。
- Pinvou API、OTA、遥测、中继、模型服务、部署后台等运行服务地址。
- 服务器域名/IP、端口组合、账号、远程目录、SSH 路径、代理和隧道拓扑。
- 真实机器人 Code、Webhook URL、群聊 ID、签名密钥和验证令牌。

`https://pinvou.com/` 是公开源码中唯一允许直接出现的 Pinvou 官方营销域名，可用于 README、About 和 GitHub repository homepage。该规则不放行任何实际运行服务地址。

### 6.3 真实运行数据

- 用户 session、对话、模型原始回答、工具调用 trace 和调试日志。
- 录音、截图、系统信息、设备标识和用户文件。
- 当前 `docs/l1-baselines/` 中的真实或历史运行记录。
- 当前 `资料包/` 和内部归档研发文档。

公开版只保留使用合成数据重新生成的英文测试样例、截图、测试方法和结论。

## 7. 密钥和历史处理

密钥清理分为三个层次：

1. 从准备公开的当前代码和配置中删除明文及内置共享 Key。
2. 对曾经进入代码、构建环境或安装包的真实 Key 执行吊销或轮换。
3. 公开仓库使用全新清洁历史，确保旧私有历史不被带入。

现有私有 `Pinvou/pinvou3` 历史默认不重写。旧 Key 吊销后失效；只有在内部安全或合规明确要求时，才单独评估私有仓库全 refs 历史重写。

首个社区版本暂不补高德、问财和企查查的通用 BYOK 界面。删除共享 Key 后，允许相关入口暂时可见但调用失败，这一已知状态不阻断 `v0.7.0`；后续版本再完善配置入口。

## 8. 遥测、反馈、OTA 和模型

### Community

- 完整移除 Pinvou 遥测客户端、服务端统计、管理页面、配置和测试。
- 反馈入口打开 `Pinvou/pinvou-agent` GitHub Issues，不自动上传日志、设备信息或附件。
- 首个 Community 版本不提供应用内自动更新，通过 `Pinvou/pinvou-agent` GitHub Releases 手动下载安装；后续启用自动更新时只能使用该公开 Release 通道并执行独立更新签名验签。
- BGE-M3 社区模型资产由 GitHub Releases 分发并提供 SHA-256。
- SenseVoice 使用其官方上游 ModelScope/GitHub 来源。

### Official

- 遥测、反馈、OTA 和模型分发维持当前私有实现。
- 官方二进制可以包含官方服务地址，但不能包含可复用 Key、客户端 Secret、SSH 凭证或管理 Token。

## 9. 第三方代码、资产和二进制

### 保留原则

可以保留具有明确授权或属于 Pinvou 自有的内容，但必须记录来源、固定版本或 commit、许可证、哈希和归因要求。

已确认可保留的主要内容包括：

- CodeWhale / DeepSeek-TUI：MIT。
- Lark CLI/Skills：MIT。
- WeCom CLI/Skills：MIT。
- DingTalk DWS：Apache-2.0，并保留 NOTICE。
- 问财 MCP：上游声明 MIT，同时明确数据来源于同花顺问财，使用者需遵守其服务条款。
- `agency-agents-zh`：MIT。
- Pinvou 自行生成的桌宠、Banner 和工具/Skill 图片。
- 具有明确 CC BY 4.0、CC0 1.0 或 MIT 来源的头像和模型图标。
- BGE-M3、SenseVoice.cpp 及相应许可允许的集成。

### 不直接进入 Git

- 已 tracked 的 `web-template/node_modules/`。
- BGE-M3 模型权重。
- SenseVoice、DingTalk、Lark、WeCom 等预编译二进制。
- 沃通 Windows 签名工具及其他没有明确再分发授权的构建工具。

这些能力通过固定版本、锁文件、校验哈希、CI 下载或从固定 commit 构建恢复。GitHub Release 安装包只有在许可证允许的情况下才可包含对应二进制，并须附第三方声明。

## 10. CodeWhale Submodule

- 公共 fork 使用 `Pinvou/CodeWhale`。
- 从公开上游 `Hmbown/CodeWhale` 的完整公开历史建立，不公开现有私有 fork 历史。
- 将 Pinvou 仍需保留的六类通用 patch 清理后重新提交为新的公开 commit。
- 删除 H3C、客户信息、内部地址、凭证和 Pinvou 私有发布配置。
- `Pinvou/pinvou-agent` 固定引用经过验证的 `Pinvou/CodeWhale` 稳定 tag/commit，不跟随浮动分支。
- 首发保留本地目录名 `DeepSeek-TUI/`，避免为了重命名引入无价值风险。

## 11. 公开与私有开发规则

代码归属不按“在哪个版本发现、主要由哪个版本使用或测试”判断，而按能力边界判断。

- 无需 Pinvou 内部服务、客户信息、私有凭据和商业授权即可供社区用户使用：进入公开仓库。
- 仅服务官方运营、企业客户或特定客户：进入私有仓库。
- 同时包含通用和私有部分：拆成公开接口/通用实现与私有适配器/运营配置。
- `model_store`（企业模型商店）依赖官方模型分发服务与账号体系，归入私有仓库。
- 桌面双端远程控制（桌面主控/受控、controller 协议、任务权限、文件传输、enrollment/pairing 与控制器中继面）归入私有仓库，开源仅保留手机/QR 远程控制一期（2026-10-25 确认）。

长期关系：

- `Pinvou/pinvou-agent` 是 Community 及通用代码的唯一上游。
- `Pinvou/pinvou3` 是企业/官方私有扩展的唯一上游，也是官方版集成测试和定时发布主线。
- 通用 Bug 即使在 Official 中发现，也在公开仓库修复，再同步到私有仓库。
- 官方版可以频繁发布，不要求 Community 同步创建 Release。
- 公开仓库不能依赖私有仓库；私有仓库必须记录使用的公开 core commit。

首发阶段采用 community/official 两个独立 worktree，不再增加一层 Pinvou 仓库 submodule。

两个仓库的 `AGENTS.md` 将强制每个任务先声明：

```text
代码归属：public / private / split
目标仓库：Pinvou/pinvou-agent 或 Pinvou/pinvou3
判断依据：一句话说明
关联同步：是否需要更新另一个仓库
```

## 12. 国际开发者适配

- README 使用英文主入口，并提供 `README.zh-CN.md`。
- 英文是社区版默认 UI、首次启动、设置、错误提示、核心工具和核心 Skills 语言。
- 中文能力完整保留并执行回归。
- System prompt 独立维护 `instructions.en.md` 和 `instructions.zh-Hans.md`，不使用“中文主 prompt 加英文补丁”。
- 通用 MCP 工具名和 schema 字段使用英文 ASCII。
- MCP 描述根据 locale 返回英文或中文。
- China integration 和大量中文角色保留，但以可选 China pack 形式存在，不作为国际版默认体验。

## 13. 首发版本和平台

首个公开版本：

- Git tag：`v0.7.0`
- Community：`Pinvou Community 0.7.0`
- Official：`Pinvou Official 0.7.0` 加独立 build ID

首发支持：

- Linux x86_64
- Linux ARM64
- Windows x86_64
- macOS Apple Silicon，arm64，macOS 14.0+

Intel Mac 首发不承诺支持。

Community 和 Official 是同一产品的不同构建，保留相同的现有应用名、应用 ID、安装路径、`~/.pinvou3` 数据目录和凭据命名空间。首版不支持两种构建并存或同时运行；安装另一种构建视为覆盖切换，不增加跨构建文件锁。

两种构建只通过 Release 资产文件名、许可说明和 Official build ID 区分：

- Community 对应公开源码，首版不提供应用内自动更新，通过 GitHub Releases 手动下载安装。
- Official 包含私有扩展，保留当前官方更新通道。
- Release 文档明确说明二者互斥安装；首版不承诺 Official 降级到 Community 后私有配置和残留扩展完全无损。

## 14. macOS 和 Windows 签名

### macOS

- 同一个公开 GitHub Release 同时提供 Community 和 Official 两种 macOS 包。
- `Pinvou Community` 对应公开源码，不使用 Developer ID，不提交 Apple notarization，也不 staple；Release 明确标注 `Unsigned Community Build`，并提供 Gatekeeper 首次打开说明和 SHA-256。
- Community 不因缺少 Developer ID 而明文持久化用户凭据；若稳定 Keychain 访问无法保证，首版接受 Keychain 额外提示或暂不持久化相关凭据。
- Community 首版不提供应用内自动更新；后续如启用 GitHub Releases 自动更新，仍须使用独立的 updater 签名和客户端验签，该签名与 Apple Developer ID 无关。
- `Pinvou Official` 使用 Developer ID Application、Hardened Runtime、Apple notarization 和 staple，并恢复稳定的 macOS Keychain 凭据存储。
- Apple Developer Program 与 Developer ID 权限只阻塞 Official 正式签名包，不阻塞 Community unsigned/ad-hoc 资产进入首发 Release。

证书、私钥、notarization 凭证和 updater 私钥不能通过聊天传递或进入源码/Git 历史。

### Windows

- Community 和 Official 均使用 Pinvou 现有代码签名证书。
- 公开 CI 构建可复现的未签名候选包。
- 私有 Windows 签名环境完成签名和验签后上传 GitHub Release。
- 沃通工具、UKey、密码、证书指纹和私有签名环境信息不进入公开仓库。

## 15. GitHub Release

同一个公开 Release 可以包含两类安装包：

### Community 资产

- 对应公开 tag/commit。
- 可从公开源码构建。
- 采用 MIT。
- 提供 SHA-256 和签名状态。
- macOS Community 资产明确标注为 `Unsigned Community Build`，不暗示其经过 Developer ID 签名或 Apple notarization；后续如提供自动更新产物，仍须提供 Tauri updater 签名。

### Official 资产

- 文件名和区域与 Community 明确区分。
- 标注包含私有组件，不能由公开源码完整复现。
- 标注官方 build ID 和对应 Community core commit。
- 按 Pinvou 官方用户协议分发。
- 保留 MIT 社区核心和第三方组件的许可证及 NOTICE。

GitHub 自动生成的 Source code 压缩包只对应 Community 源码，不代表 Official 私有扩展开源。

## 16. 贡献、支持和安全

### 贡献

- MIT + DCO 1.1，不使用 CLA。
- 贡献者通过 `git commit -s` 添加 `Signed-off-by`。
- CI 检查 DCO。

### 主线保护

- `main` 禁止直接 push、删除和强制推送，全部修改通过 PR。
- 单人维护阶段不要求另一名审批者批准，以必需 CI 为合并门槛。
- 默认 Squash Merge。
- `v*` Release tag 创建后不可移动、覆盖或复用。

### 支持

- GitHub Issues：Bug 和明确的功能请求。
- GitHub Discussions：使用问题、想法、扩展和案例。
- 首发不创建 Discord、Slack、微信群等额外即时渠道。
- 社区支持为 best-effort，不承诺 SLA。

### 安全

- 开启 GitHub Private Vulnerability Reporting。
- `security@pinvou.com` 作为备用安全邮箱，首发转发到维护者个人邮箱。
- 个人邮箱地址不公开，接收邮箱启用 MFA，对外回复使用 `security@pinvou.com` 身份。
- 首发不设置漏洞赏金或固定响应 SLA。

## 17. 自动防泄漏门禁

- Gitleaks 检查 Key、Token、私钥、密码和高熵字符串。
- 自定义 URL/IP allowlist 检查运行服务地址、服务器信息和远程路径。
- Pinvou 真实基础设施精确黑名单只存在于私有扫描环境，不写入公开仓库。
- 不使用整批忽略 baseline；每个命中必须修复或给出最小范围误报说明。
- 公开前扫描准备发布的文件树。
- 创建全新公开仓库后再扫描全部公开历史。
- 每个公开 PR 持续执行扫描。
- 发布门槛为零个未处理的密钥或内部地址命中。

## 18. 官网和公开入口

- `https://pinvou.com/` 作为公开官网和企业引流入口。
- 官网英文入口、Community/Official/Enterprise 分栏、真实下载页、英文 SEO 和社交分享信息已决定延期，不阻断首个公开源码和 GitHub Release。
- 延期期间允许官网保留现有展示状态，但不得放置错误安装包、错误校验值或把 Community 与 Official 混写为同一授权版本。
- 后续接入下载页时，分别显示 Community 和 Official 的版本、平台、SHA-256、签名状态及 Release Notes，并与 GitHub Release 保持一致。

## 19. 已接受的首发限制

- 高德、问财、企查查删除共享 Key 后可能暂时不可用，首版暂不增加 BYOK 界面。
- 相关入口首版允许继续显示并在调用时失败，后续版本修复。
- 完整英文产品体验和国际化 M4 已决定延期；首发先保证英文 GitHub 入口和开发文档，不把英文 UI/system prompt 全旅程作为 `v0.7.0` Release 阻断项。
- China integration 中的大量中文内容首发不全部翻译，但必须提供英文名称、说明和 China 标签，并且不作为英文默认体验。
- Intel Mac 首发不承诺支持。
- macOS Community 首发包不使用 Developer ID 或 Apple notarization，用户可能需要按文档完成 Gatekeeper 首次打开；Official macOS 包提供正式签名和公证体验。
- 社区支持由单人 best-effort 维护。

这些是明确接受的产品限制，不得与密钥、签名、内部地址、发行资产标识和更新通道等安全放行条件混为一谈；后者未通过时仍不得发布。

## 20. 组织复核清单

请组织成员重点确认：

- [x] 认可“除客户专属、内部资产、凭证和无授权内容外，现有通用能力全部开源”。
- [x] 认可 Community、Official 和 Enterprise 的功能与许可边界。
- [x] 认可公开通用代码上游、私有官方集成发布主线的长期开发方式。
- [x] 认可 Official 二进制可以作为独立资产出现在公开 GitHub Release。
- [x] 认可 H3C 内容完全不进入公开源码、历史、CI 和 Community Release。
- [x] 已完成已知客户专属内容、内部服务、真实数据和无再分发授权资产的范围确认；后续发现新增候选时继续按相同边界处理。
- [x] 认可首发平台、`v0.7.0` 和 Community/Official 单一应用身份、互斥覆盖安装。
- [x] 认可 macOS Community 为 unsigned/ad-hoc 开源包、macOS Official 为 Developer ID 签名与公证包。
- [x] 认可英文优先、完整保留中文能力；完整英文产品体验 M4 延期，不阻断 `v0.7.0`。
- [x] 认可单人维护下的贡献、支持、安全报告和主线保护方式。

如有异议，请按“章节 + 建议修改 + 原因 + 是否阻断首发”的格式反馈，避免只给笼统结论。

## 21. M0 完成标准

Pinvou 组织已确认本文，M0 已完成。后续阶段按执行代办推进：

1. 只读扫描密钥、运行服务器地址、H3C、遥测、日志、部署脚本和历史测试数据。
2. 输出逐文件处理清单。
3. 轮换或吊销真实凭证。
4. 在不破坏现有私有版功能和发布流程的前提下开始清理。
5. 为全新公开仓库生成可验证的清洁文件快照。
