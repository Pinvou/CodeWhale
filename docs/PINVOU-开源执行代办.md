# Pinvou 开源执行代办（单人版）

> 目标：完成 Pinvou 社区版开源，在 GitHub 建立全球开发者生态、提高知名度，并为企业版引流。
>
> 执行方式：一人维护，优先关闭会阻止公开的 P0，再做影响 GitHub 转化的 P1；不在首发前追求一次做完所有平台和语言。
>
> 完整风险与放行底稿见 [`PINVOU-MIT-开源决策确认清单.md`](./PINVOU-MIT-开源决策确认清单.md)。公司、法务、权属和开源授权已确认，不纳入本执行清单。

## 当前状态快照（2026-07-26）

> 勾选只表示有代码、CI 或 GitHub 现场证据，不用计划代替事实。公开仓库后续发生变化时，需要重新验证对应条目。

| 阶段 | 状态 | 当前结论 |
|---|---|---|
| M0 决策 | 已完成 | 组织、仓库、MIT、Community/Official 边界、单一应用身份和首发版本均已确认 |
| M1 公开隔离 | 代码侧已完成 | 已建立全新根提交的公开仓库并通过公开历史 Gitleaks；历史真实凭证的吊销/轮换仍需负责人确认 |
| M2 社区基线 | 基本完成 | Community 不依赖私有仓库，MCP 契约和无 Key 错误契约通过；空用户目录的完整手工首启仍待验证 |
| M3 CodeWhale | 已完成 | `Pinvou/CodeWhale`、稳定 tag、固定 gitlink、公开可达检查和 recursive clone 已落地 |
| M4 英文产品体验 | 已决定延期 | 英文 README 和开发入口已完成；英文 UI、system prompt、MCP 全旅程不阻断 `v0.7.0` |
| M5 开源治理 | 基本完成 | MIT、DCO、SECURITY、SUPPORT、TRADEMARKS、NOTICE、Issue 模板和安全/依赖门禁已落地；`CODEOWNERS` 和 Release 级 SBOM 仍待处理 |
| M6 GitHub 入口 | 基本完成 | 英文 README、中文 README、三张截图、Quickstart、Description、Topics 和 Discussions 已就绪；演示视频、Social Preview 和同类差异说明待补 |
| M7 Release | 进行中 | clean clone、前端测试/构建、MCP、submodule、Secret Scanning、Push Protection 和主线保护通过；平台安装包、新机旅程、`v0.7.0` tag/Release 待完成 |
| M8 首发传播 | 未开始 | 英文/中文发布文案、Show HN、Reddit、Product Hunt 和首周维护安排待准备 |
| M9 持续维护 | 部分完成 | SUPPORT 和 Issue 模板已明确 best-effort；固定维护节奏和里程碑发布节奏待执行 |

现场基线：

- 公开仓库：`Pinvou/pinvou-agent`，`main@e34024e62f826e65867eb8801a0b370c11e03bda`。
- 公开 CodeWhale gitlink：`070f4413eeb0e0c4e6f2634f1ada13c60fd2e86e`，稳定 tag `pinvou-v0.9.0-r1`。
- 当前公开应用版本：`0.6.4`；目标首发版本：`0.7.0`。
- 当前没有公开 tag 或 GitHub Release。
- 全新匿名克隆已完成 `npm ci`、完整前端测试、UI lint、生产构建、MCP 契约、architecture guard、fork guard、Cargo metadata 和公开依赖来源检查。
- 当前公开 main 的 `PR Check`、`secret-scan` 和 `CodeQL` 均通过；Secret Scanning、Push Protection、Discussions 和主线保护均已开启。

## 已确认方向

- [x] 开源目标：做生态、提高知名度、为企业版引流。
- [x] 社区版是完整可用的产品，不做阉割后的演示壳。
- [x] 当前 Pinvou 除确实不能公开的 H3C/内部资产、凭证和无再分发授权内容外，其他功能和源代码全部开源。
- [x] 未来企业专属的组织管理、安全合规、私有化交付和企业服务可保持私有，但不因此阉割现有通用社区版。
- [x] 不直接将当前私有仓库改为 Public，使用清理后的全新公开仓库。
- [x] GitHub 公开入口英文优先，产品完整支持英文和简体中文。
- [x] 非 H3C 的中国区域能力仍然开源，可作为可选插件或 China integration pack，不占据国际版默认体验。

## M0：锁定首发范围

> 完成标准：首发必须做什么、明确不做什么，后续不再无限扩项。

- [x] 开源许可证确认为 MIT，Pinvou 名称、Logo 和官方发行版身份通过 `TRADEMARKS.md` 单独保护。
- [x] 公开主仓确认为 `Pinvou/pinvou-agent`，以 Pinvou 组织名义开源；现有 `Pinvou/pinvou3` 继续作为私有内部仓库。
- [x] 底座公开 fork 确认为 `Pinvou/CodeWhale`，保留与上游 `Hmbown/CodeWhale` 的来源关系和授权说明。
- [x] 英文产品定位确认为：“Pinvou is an open-source, local-first desktop AI workspace that turns conversations into finished work with files, knowledge, tools, and automations.”
- [x] README 和首发宣传重点展示 7 项核心能力：Agentic desktop workspace、Deliverable-first artifacts、Local knowledge、MCP and Skills ecosystem、Workflows and automations、Bring your own model、Inspectable and controllable execution。其他现有通用功能仍全部开源。
- [x] 列出必须从公开仓库剔除的内容，剩余现有功能默认全部开源。

### 仓库搜索与品牌表达（已确认）

- 公开仓库使用 `Pinvou/pinvou-agent`，由仓库名直接表达 Agent 类别；产品品牌仍为 `Pinvou`，并在 README 标题、GitHub Description 和 Topics 中继续覆盖 `agent`、`ai-agent`、`desktop-ai`、`agentic-ai` 等搜索词。
- 普通文案、界面和产品名称统一使用首字母大写的 `Pinvou`，不写成容易被理解为缩写的 `PINVOU`。
- 产品版本名称统一为 `Pinvou Community`、`Pinvou Official` 和 `Pinvou Enterprise`；环境变量、常量等技术标识仍按相应规范使用全大写，仓库 slug、CLI 和目录使用小写。

### M0 公开边界（已确认）

> 原则：剔除的是内部资产、敏感数据或不能再分发的实现，不因此删掉通用产品能力。可以通过独立重写、用户自带 Key、构建时生成或运行时下载恢复的功能，仍属于社区版。

#### 必须从公开快照移除

- H3C 专属内容：`bundle/eip/`、`bundle/zhidao/`、`h3c-banner.jpg`、H3C 内部提示词/技能/配置/文档，以及内网地址、账号和运维链路。
- 真实凭证：明文 API Key、Token、Cookie、OAuth/SSO 凭证、Webhook、SSH 密钥、签名证书及可恢复的凭证片段。
- 真实运行记录：session/对话数据、原始模型回答、工具调用 trace、调试日志、录音、截图、系统信息和用户文件。`docs/l1-baselines/` 中的完整运行记录不进公开快照，只保留人工脱敏后的测试方法、用例和结论。

#### 不直接放进 Git，但不删功能

- 删除已 tracked 的 `pinvou3-app/src-tauri/resources/web-template/node_modules/`（2,619 个文件），保留 lockfile，用 `npm ci` 可重现生成。
- BGE-M3 模型权重不进 Git，保留知识库源码和按需下载器，并锁定模型 revision、哈希和 MIT 声明。
- `sense-voice-main` 等本地构建二进制不以“来源不明的现成文件”进 Git；保留 ASR 功能、构建/下载脚本、固定上游 commit 和哈希。
- Lark CLI、WeCom CLI、DingTalk DWS 虽允许再分发，仍优先改为从官方版本构建或按需下载；开源的是完整集成代码，不必把大二进制塞进源码仓库。

#### 已确认有再分发基础，补声明后保留

- CodeWhale / DeepSeek-TUI（MIT）、Lark CLI/Skills（MIT）、WeCom CLI/Skills（MIT）、DingTalk DWS（Apache-2.0 + NOTICE）。
- `pinvou3-app/resources/mcp-servers/iwencai/` 对应的 `lorek666/iwencai-mcp`（README 明确声明 MIT）：保留上游链接和 MIT 声明，锁定源码 revision，标注 Pinvou 修改；数据调用遵守同花顺问财服务条款，只支持用户自带 Key，不公开共享 Key。
- `agency-agents.json` 对应的 `jnMetaCode/agency-agents-zh`（MIT）。
- 三套桌宠 `spritesheet.webp`/`cover.webp`、`sansheng-banner.png`、7 张工具商店 `banner-*.jpg`、7 张 `skill-*.jpg` 和 `scheduled/` 下 3 张 JPG 为 Pinvou 自行生成的自有视觉素材，在公开仓库和社区版中保留。
- MegaCube 为 Pinvou 自有能力；保留 `megacube-icon.png`、MegaCube 网页访问入口及相关通用集成代码，不归入 H3C/内部功能裁剪范围。
- 56 个内置头像：50 个 CC BY 4.0、6 个 CC0 1.0；保留 SVG 内已有元数据，并在 `THIRD_PARTY_NOTICES.md` 汇总署名。
- 可证明来自 `lobehub/lobe-icons` 的模型服务商图标（MIT）；改为锁定 npm 包版本或记录确切上游文件，同时保留品牌归属说明。
- BGE-M3（MIT）和 SenseVoice.cpp（MIT）可保留集成；SenseVoice 模型权重另按 FunASR Model License 归因并保留模型名称。

#### 放行前还要逐项出证的内容

- 其余图片、图标、截图、音频、模型和预编译二进制必须进入“来源 + 固定版本/commit + 许可证 + 哈希 + 归因要求”清单；任一项无法出证就不进首个公开快照。
- MCP manifest 中补齐 `homepage`、`repository`、`license`、`source_revision`；远程商业 API 的配置代码可开源，真实 Key、共享额度和生产端点不公开。

## M1：密钥、内部资产与历史隔离（P0）

> 完成标准：新公开快照不包含任何内部凭证、内网信息、生产运维和 H3C 专属资产。

### 密钥处理口径（已确认）

- 当前文件：删除明文 Key、编译时内置共享 Key 及其发布注入链路；高德、问财、企查查在暂无 BYOK 界面时可暂时不可用，后续版本再补配置入口。
- 凭证本身：对任何曾进入代码、构建环境或发行包的真实 Key 执行吊销/轮换；单纯删文件或改 Git 历史不能使已泄露 Key 失效。
- 公开历史：`Pinvou/pinvou-agent` 从清理后的文件快照创建，不导入现有私有仓库的 commit、branch、tag、PR 和 Actions 历史，因此公开 Git 历史中不会出现旧 Key。
- 私有历史：默认不重写现有 `Pinvou/pinvou3` 历史；Key 吊销后已不能使用，而重写会改变全部后续 commit SHA 并影响分支、PR、worktree 和其他 clone。只在合规或内部安全要求必须彻底清除时，再单独执行全 refs 历史重写。
- 已确认首个社区 Release 暂不补高德、问财、企查查的通用 BYOK 界面，也不隐藏/禁用现有工具卡片；删除共享 Key 后允许出现“入口仍显示但实际调用失败”的已知状态，此问题不作为首发阻断，后续版本再修复。

### 第三方授权处理口径（已确认）

- 保留飞书、企微、钉钉、元典等非 H3C 第三方集成的 OAuth、MCP、Skills、配置说明和产品入口。
- 删除真实的 `app_secret`、访问令牌、Webhook secret、verification token 等凭证值，保留环境变量名和示例配置，由用户授权自己的账号或配置自己的企业应用。
- `client_id`、`app_key` 等公开标识不按密钥盲删；如发现 Pinvou 官方应用标识，单独核对平台条款和是否允许社区共用后再决定。
- 元典当前使用标准远程 OAuth 接入，manifest 未内置客户端密钥，可保留。
- H3C 的 SSO、EIP、知道等专属接入仍按既定范围整体移除。

### 机器人与 Webhook 处理口径（已确认）

- 保留钉钉、飞书、企微等非 H3C 机器人和 Webhook 的通用代码、Skills、脚本与使用文档。
- 删除真实 Webhook URL、机器人 Code、群聊/会话 ID、签名密钥和验证令牌；示例统一改用明确占位符。
- 不提供 Pinvou 官方共享机器人，用户运行时配置自己的机器人或 Webhook。
- 日志和错误信息不得输出机器人 Code、Webhook URL、签名密钥等敏感参数，已有输出改为隐藏或脱敏。

### 运行服务器与远程控制处理口径（已确认）

- 手机远程控制客户端、Web 页面、中继服务器、测试和通用部署能力全部开源，不裁掉功能。
- 公开源码默认不得连接 Pinvou 生产中继；社区用户通过配置使用本地或自建中继，官方发行版可在构建/发布环境中注入官方服务地址。
- Pinvou 运行中的所有服务器信息均不得进入公开源码和 Git 历史，包括访问地址、域名/IP、端口组合、服务器账号、远程目录、SSH key 路径、代理/隧道拓扑、内部设备配置和管理凭证。
- 公开 GitHub Release 可以同时提供由公开源码构建的社区包，以及包含私有功能和官方服务地址的 Pinvou 官方包；官方包必须明确标注其不能由公开源码完整复现。服务器地址允许存在于官方二进制，但可复用密钥、客户端 Secret、SSH 凭证和管理 Token 仍不得内置。
- 部署、发布、OTA、Nginx、证书、CI/CD 和测试脚本必须逐项检查；真实值改为必填环境变量、命令行参数或明确占位符，并提供安全的 `.env.example` 与自建说明。
- 社区公开快照完全移除 Pinvou 遥测能力；官方私有版维持现状。远程控制中继本身仍开源，只拆除设备统计、上报和统计管理后台。
- Community 首版不提供应用内自动更新，只通过 `Pinvou/pinvou-agent` GitHub Releases 手动下载安装；Official 维持现有更新服务。公开快照不得包含 Pinvou OTA 地址、服务器配置或部署路径；后续如恢复 Community 自动更新，必须使用公开 Release 通道和独立更新签名验签。
- 社区版知识库 BGE-M3 模型包改由 `Pinvou/pinvou-agent` GitHub Releases 分发，保留 SHA-256 校验与可复现的生成说明；SenseVoice 继续使用其官方上游 ModelScope/GitHub 下载源。官方私有版维持现有模型分发服务。

### H3C 与 MegaCube 边界（已确认）

- 社区版完整移除 H3C EIP、知道、Windows 域环境引导、SmartHub OTA，以及相关 Skills、源码、预编译二进制、Rust/前端接线、测试、文档、品牌和内部场景。
- 已删除基于 `/opt/.h3c/...` 预装目录、固定模型路径和启动脚本的一键启用能力；通用本地推理能力保留为用户自行启动服务后，通过设置页发现或手动配置 vLLM、Ollama、LM Studio 端点。
- MegaCube 属于 Pinvou 自有能力：名称、图标、产品入口、本地模型与通用集成全部保留；访问地址由官方构建/运行配置注入，社区源码不包含真实地址。
- 官方私有版可继续保留其他 H3C 自动引导和 MegaCube 配置；预装本地模型自动启动能力不再由 Pinvou 应用提供。
- 社区版删除 `feedback.rs` 中固定设备 SN、`H3cFeedbackUploader` 及相关 H3C 反馈上传接线；反馈入口改为打开 `Pinvou/pinvou-agent` 的 GitHub Issue，不自动上传日志、设备信息或附件。官方私有版反馈行为维持现状。

### 日志、会话与历史测试记录（已确认）

- 社区首版删除现有 `docs/l1-baselines/` 中的历史 transcript、评分报告和专项重评估记录；当前盘点为 109 个文件、约 528 KB，其中已发现内部模型/运行信息和服务器地址副本。
- 保留 L1 harness、评分规则、场景定义和生成脚本；完成开源清理后重新生成不含内部信息的英文合成 baseline。
- 删除任何真实用户对话截图、session 导出、运行日志、replay 结果、验证报告和测试产物；真实运行数据目录 `~/.pinvou3/` 不得进入公开快照。
- 普通单元测试 fixture 仅在确认是人工或合成数据且通过敏感信息扫描后保留。
- 补齐 `.gitignore`，禁止提交日志、session、replay、L1 临时输出和其他运行产物。

### Tracked 依赖与预编译二进制（已确认）

- 删除 `pinvou3-app/src-tauri/resources/web-template/node_modules/`；保留 `package.json` 与 lockfile，通过 `npm ci` 和固定 Node.js 版本构建。
- 社区源码 Git 不直接提交 `dws`、`lark-cli`、`wecom-cli`、SenseVoice 等预编译 ELF/EXE；由 CI 从固定上游版本下载并校验，或从固定 commit 可复现构建。
- GitHub Release 安装包可以包含许可证允许再分发的第三方二进制，但必须同时提供上游仓库、固定版本/commit、许可证/NOTICE、SHA-256 和构建方式清单。
- 补齐 Linux x86_64/ARM64、Windows 等目标平台的依赖组装流程，不能继续把仓库内单个平台二进制当成唯一来源。
- H3C EIP、知道的所有预编译二进制随对应功能彻底删除，不进入社区源码、CI 或 Release。

### 共享依赖改造与回归门槛（已确认）

- `marked` 13.0.3 与 DOMPurify 3.4.2 首版保留当前运行行为，补齐上游、版本、许可证和 SHA-256；Tailwind 首版保留运行时机制，但改由固定 npm 版本可重复生成，不继续使用无版本 CDN 文件。
- 上述前端依赖、tracked `node_modules` 和可复现二进制组装属于通用改进，私有官方仓与社区仓共同采用；H3C、遥测和官方服务配置只留在私有层。
- 改造不得改变官方版基本功能和发布行为；未通过开发启动、核心 UI/Markdown/安全过滤、网页模板、连接器、Rust/前端测试、生产构建、安装/卸载、升级和 Release 组装验证，不得进入公开快照或正式发布。
- Tailwind 静态 CSS 迁移不纳入首个开源版本；待具备视觉回归覆盖后再单独实施，避免动态 class 丢失造成界面退化。（更新 2026-09：已由 PR #1217 单独实施——Tailwind 3.4.17 迁至 Vite/PostCSS 构建期并删除 Play CDN runtime；动态 class 经全源码字面量核查与 browser smoke 覆盖，Safari 14 的 inset 物理属性契约由 `tests/compat_audit.test.mjs` 与 `npm run audit:compat` 的 dist CSS 审计共同看护。）

### Community 与 Official 单一应用身份（已确认）

- Community 和 Official 是同一产品的不同构建，保留相同的现有产品显示名、应用标识、安装路径、`~/.pinvou3` 和凭据命名空间，不为社区版复制一整套身份配置。
- 首版不支持两个构建并存或同时运行，不增加跨构建文件锁；安装另一种构建视为覆盖切换，共享现有用户数据。
- Community 首版不提供应用内自动更新，通过 GitHub Releases 手动下载安装；Official 保留当前官方更新通道，避免 Community 串用 Official 升级包。
- Release 资产文件名、许可说明和 Official build ID 必须明确区分；首版不承诺 Official 降级到 Community 后私有配置和残留扩展完全无损。

### 公开与私有开发主线（已确认）

- `Pinvou/pinvou-agent` 是社区版及全部通用代码的唯一上游；`Pinvou/pinvou3` 是企业/官方私有扩展的唯一上游，同时承担官方版集成测试和定时发布。
- 代码归属不按“在哪个版本发现、主要由哪个版本使用或测试”判断，而按能力边界判断：无需 Pinvou 内部服务、客户信息、私有凭据或商业授权即可供社区用户使用的代码进入公开仓库；仅服务官方运营、企业客户或特定客户的代码进入私有仓库。
- 混合能力拆成“公开接口、通用实现和可配置能力”与“私有适配器、内部服务和运营配置”；公开仓库不得依赖私有仓库，私有官方版记录并同步其使用的公开 commit。
- 首发阶段采用独立的 community/official 双 worktree，不再增加一层 Pinvou 仓库 submodule；通用改动在公开 worktree 开发，并在私有官方集成分支完成完整回归后合并。
- 通用修复即使首先在官方版发现，也必须进入公开仓库；私有仓库同步公开 commit 后再发布官方版。官方版频繁发布不要求社区版同步创建 Release。
- 两个仓库的 `AGENTS.md` 强制每个开发任务先声明 `代码归属：public/private/split`、目标仓库、判断依据和跨仓同步关系，并通过 `git remote get-url origin` 验证仓库身份；归属明确时直接执行，真正无法判断时才询问。
- 公开 CI 检查密钥、内部地址、客户专属功能、遥测和私有依赖；私有 PR 若触及通用代码，必须关联对应公开 PR/commit。公开 core 更新必须通过社区版测试与官方版集成回归。

### 资料包与历史研发文档（已确认）

- 当前 `资料包/` 不进入社区首版；现有中文单文件指南、产品截图及其生成产物在公开前移除。
- 完成社区清理后使用合成数据重新拍摄英文截图，统一放入 `docs/assets/`，并重写面向国际开发者的简洁英文用户指南。
- 通用截图生成/压缩工具通过敏感信息检查后可保留，移动到 `scripts/docs/`；不得包含真实账号、会话、服务器地址或本机路径。
- `docs/archived/` 不公开；现有 `docs/` 逐篇筛选，保留并英文化架构、构建、贡献、扩展开发等有开发者价值的文档，移除内部运维、服务器、迁移复盘、模型调优记录和过期方案。

### 自动防泄漏门禁（已确认）

- 使用 Gitleaks 检查 API Key、Token、私钥、密码和高熵字符串；私有仓导出前、公开仓创建后全历史、以及公开仓每个 PR 都必须执行。
- 增加公开安全检查：网络地址采用批准的公共第三方域名 allowlist，只允许 `localhost`、`.example`、RFC 保留测试 IP 和明确批准的上游；其他 URL/IP、SSH 目标、远程目录或硬编码凭证阻止合并。
- Pinvou 真实域名/IP 的精确黑名单只保存在私有扫描环境，不写入公开仓库或公开 CI 配置。
- 不使用宽松的整批忽略 baseline；每个命中必须修复，或以最小范围记录明确的误报理由。
- 发布门槛：密钥扫描零未处理命中，公开源码文件树和 Git 历史中不存在 Pinvou 运行服务器访问信息；官方二进制另做凭证扫描，允许官方服务地址但不允许可复用密钥。

### 公开官网与服务地址边界（已确认）

- `https://pinvou.com/` 是公开源码中唯一允许直接出现的 Pinvou 官方营销域名，可用于 README、应用“关于”页面和 GitHub repository homepage。
- 允许公开的是官网营销入口，不因此放行 API、OTA、遥测、中继、模型下载、部署后台或其他 Pinvou 运行服务地址；这些地址仍只能存在于私有配置或官方二进制。
- 官网英文入口、Community/Official/Enterprise 分栏、真实下载页和英文 SEO 已决定延期，不阻断首个公开源码和 GitHub Release。
- 延期期间允许官网保留现有展示状态，但不得提供错误安装包、错误校验值或混淆 Community 与 Official 的授权说明。
- 后续接入真实下载页时，再统一官网指南、更新日志、公开源码和 Release 版本，并补充 GitHub/Open Source、企业咨询、页面 description、Open Graph 和社交分享图。

#### 部署与运行地址初步审计（不得直接进入公开快照）

- `scripts/deploy-remote-relay.sh`：包含服务器、公开/直连地址、远程目录、服务名和上传路径默认值；改为必填参数或环境变量。
- `scripts/deploy-remote-relay-test.sh`：包含 relay/proxy 服务器、SSH key、隧道用户、预期地址/IP 和 SSHD 配置；移除真实拓扑，测试改用隔离环境变量和保留测试地址。
- `scripts/release-deb.sh`：包含发行服务器、下载地址、远程目录以及三项内置共享 Key 注入；服务器配置外置，并删除共享 Key 注入链路。
- `scripts/release-kb-model.sh`：包含模型分发服务器、下载地址和远程目录；改为通用上传模板。
- `scripts/release-kb-model.sh` 同时改造成社区 Release 资产生成/发布脚本，不再包含 Pinvou 模型服务器上传逻辑。
- `deploy/nginx-pinvou-domain*.conf`、`deploy/nginx-pinvou3-updates.conf`：包含真实域名、证书/站点/OTA 路径或上游配置；改为 `.example` 模板或由部署环境生成。
- `remote-control-relay/10-hardening.conf`：移除真实代理 IP、访问策略和部署路径后保留通用加固模板；`nginx-telemetry-stats.conf` 随社区遥测整体移除。
- `pinvou3-app/run-dev.sh`：移除生产远控 HTTP/WebSocket 默认地址，开发环境默认使用本地服务或要求显式配置。
- Rust/前端默认值：远控、OTA、知识库模型下载等模块中存在运行地址常量，必须统一改为构建/运行时配置；测试中的同值副本一并替换为 `.example` 域名和 RFC 保留测试 IP。
- 社区遥测删除范围：`pinvou3-app/src-tauri/src/telemetry.rs` 及其初始化/事件接线、`remote-control-relay/telemetry-service.js`、统计管理页面、Nginx 配置、部署步骤和相关测试；同时确认公开文件树中不再出现生产地址、enrollment token、device pepper、管理员密码或硬件标识上报逻辑。
- MegaCube：图标、入口和通用集成保留；真实访问地址从前端源码移除，由官方构建或运行配置注入，社区构建未配置时给出明确提示。
- `.github/workflows/` 继续纳入最终密钥与地址扫描；只允许 GitHub/上游公共地址和 secret 变量引用，不允许出现 Pinvou 服务器信息或真实 secret 值。
- 全仓定向扫描已定位 24 个 Pinvou/MegaCube 运行 URL 候选文件，除上述脚本和源码外还包括远控架构/E2E 文档及多份测试；公开快照必须同步清理文档和测试副本，不能只改生产代码。
- `com.pinvou...` 一类应用包名、反向域名标识和品牌名称不是访问地址，可保留；判定标准是是否能实际定位或连接 Pinvou 运行基础设施。

- [ ] 先轮换或吊销高德、问财、企查查及历史模型/API Key。
- [ ] 检查并处理 SSO、OAuth、Webhook、机器人和部署凭证。
- [x] 从公开快照移除 H3C EIP 和知道连接器。
- [x] 移除内部协议、逆向实现和 H3C 内部二进制。
- [x] 移除 H3C Logo、Banner、员工文案及未授权截图。
- [x] Community 首版移除应用内自动更新和私有发布地址；保留可复现的社区安装包构建/上传脚本，Official 更新行为继续由私有仓库维护。
- [x] 移除全部 Pinvou 运行服务器访问地址，以及内网域名、IP、账号、路径和模型服务地址；源码、脚本、配置、文档与测试副本已纳入公开扫描。
- [x] 删除 tracked `node_modules`、缓存、测试产物和无来源大二进制。
- [x] 对准备公开的文件树执行密钥扫描。
- [x] 从清理后的文件快照创建新仓库，不带入旧 branch、tag、PR 和 Actions 历史。
- [x] 对新公开仓库全历史执行 Gitleaks 扫描。

## M2：建立独立可用的社区版（P0）

> 完成标准：普通开发者不需要内网、企业账号、共享 Key 或私有仓库就能启动主要功能。

- [x] 将内部连接器从 Community 主应用编译与启动链路移除。
- [ ] 国际版默认只启用通用能力和通用工具。
- [x] 非 H3C 的中国区域 MCP/Skills 源码全部进入公开仓库。
- [x] 首发不包含任何共享 Key；高德、问财、企查查缺少 BYOK 界面和安全降级属于已接受的首发例外。
- [ ] 首发后补通用 BYOK 配置界面和缺 Key 安全降级，并消除工具卡片假可用状态。
- [ ] 无任何生产凭证时可完成首次启动。
- [ ] 无 H3C 内网时能完成对话、文件产出、本地知识库和通用 MCP 流程。
- [ ] 为缺失的可选能力提供清楚的英文安装或禁用提示。
- [ ] 完成一次新用户空配置首启测试。

## M3：整理 DeepSeek-TUI / CodeWhale Submodule（P0）

> 完成标准：`git clone --recursive` 能从公开地址拉取固定、可溯源、可构建的底座版本。

### Submodule 公开历史口径（已确认）

- `Pinvou/CodeWhale` 从 `Hmbown/CodeWhale` 的公开历史重建，保留上游作者、commit、MIT LICENSE 和来源链接；不公开现有 `h3c-hexin/DeepSeek-TUI` 私有 fork 历史。
- Pinvou 需要的 6 个长期 patch 重新整理并完成 H3C、凭证、服务器地址和内部测试记录清理后，以新的清晰提交加入公开 fork。
- 为清理后的固定基线创建 `pinvou-v<upstream>-r<N>` tag，主仓 submodule 固定到 tag 对应的确切 commit，不设置浮动 branch。
- 首发继续使用 `DeepSeek-TUI/` 本地目录名，避免扩大构建路径和发布回归范围；README 明确其远端仓库为 `Pinvou/CodeWhale`，目录改名延期到开源版稳定后。

- [x] 将公开 fork 重建到 `Pinvou/CodeWhale`。
- [x] 保留上游 MIT LICENSE、原版权和 upstream 链接。
- [x] 全量扫描 fork 内的 Pinvou/H3C 内部信息与凭证。
- [x] 为 Pinvou 使用的 fork 基线创建稳定 tag `pinvou-v0.9.0-r1`。
- [x] 主仓固定到稳定 tag 对应的确切 commit。
- [x] 删除 `.gitmodules` 中容易误导的浮动 `branch` 配置。
- [x] 为 fork 编写英文 README，说明上游基线、保留修改和同步方法。
- [x] 将 `fork-policy` 和 `fork-modifications` 的开发者必读部分提供英文版。
- [x] CI 验证 gitlink commit 在公开 fork 中可达。
- [x] 验证 `git clone --recursive`、gitlink/tag 映射和底座元数据。

## M4：英文产品体验和国际化（P0）

> 完成标准：英文操作系统用户不会在 UI、助手回复、工具描述和错误提示中突然遇到中文。
>
> 当前决策：M4 完整产品国际化延期，不作为 `v0.7.0` 首发阻断项；英文 README、贡献入口和 Release Notes 仍须保持可用。

### 首发国际化范围（已确认）

- 默认 UI、设置、错误提示、首次启动、核心工具/工作流/Skills 和工具市场状态完成英文适配；英文环境下助手默认用英文回答。
- 建立独立的 `instructions.en.md` 与 `instructions.zh-Hans.md`，不使用“中文主 prompt + 英文回复补丁”；UI、system prompt、MCP/manifest 描述使用同一 locale。
- 工具名和 schema 字段保持英文 ASCII，名称、描述、错误与引导按 locale 切换；首次默认语言由 OS locale 决定，用户可手动切换。
- 中文 UI、system prompt、三省六部和现有中文 Skills 完整保留并执行回归，不因国际化降低现有中文体验。
- 飞书、企微、钉钉、问财、企查查等 China integration pack 的大量参考文档，以及 `agency-agents-zh` 全量角色数据，首发不要求全文翻译；英文环境不默认启用，但必须提供英文名称、用途说明和中国区域标签，源码仍完整开源。

- [ ] 系统首启时自动检测 OS locale；非中文环境默认英文。
- [ ] 完成英文 UI 全页扫描，清理写死的中文文案。
- [ ] 拆分 `instructions.en.md` 和 `instructions.zh-Hans.md`。
- [ ] 英文 system prompt 不再依赖“中文主 prompt + 英文回复补丁”。
- [ ] 通用 MCP 工具名和 schema 字段保持英文 ASCII。
- [ ] 为通用 MCP 提供英文描述，并按 locale 返回对应描述。
- [ ] 修复 `present_artifact` 等 schema 中写死“中文标题”的描述。
- [ ] 工具市场 manifest 支持 `name_i18n` 和 `description_i18n`。
- [ ] 英文错误、空状态、权限提示和安装引导完整。
- [ ] 为英文 prompt 跑 system prompt dump、工具调用和真模型用户旅程。
- [ ] 中文模式回归通过，确保国际化不降低现有体验。

## M5：第三方许可与开源治理（P0）

> 完成标准：许可边界清晰，外部开发者知道怎么贡献、报告漏洞和获得支持。

### 外部贡献授权（已确认）

- 采用 MIT + DCO 1.1，不使用 CLA；贡献者通过 `git commit -s` 添加 `Signed-off-by`，确认其有权按项目许可证提交贡献。
- `CONTRIBUTING.md` 提供签署示例，公开 CI 对 PR 执行 DCO 检查；不增加额外账号、纸面合同或企业 CLA 门槛。

### 安全漏洞接收（已确认）

- 在 `Pinvou/pinvou-agent` 开启 GitHub Private Vulnerability Reporting，`SECURITY.md` 将私密 GitHub Security Advisory 作为首选报告入口。
- 使用 `security@pinvou.com` 作为备用安全邮箱，首发阶段转发到维护者个人邮箱；个人邮箱地址不得出现在公开仓库、文档或自动回复中。
- 接收邮箱必须启用 MFA；对外回复使用 `security@pinvou.com` 发件身份，避免泄露个人邮箱地址。上线前从外部邮箱验证收信、垃圾邮件处理和回复链路。
- 普通 Bug 使用公开 Issues；涉及凭证泄漏、远程执行、数据越权或权限绕过的问题不得公开披露。
- 首发不设置漏洞赏金或固定响应 SLA，采用 best-effort coordinated disclosure。

### 社区支持渠道（已确认）

- GitHub Issues 只接收可复现 Bug 和边界明确的功能请求，并通过模板收集版本、平台、复现步骤和必要日志。
- GitHub Discussions 用于使用问题、想法讨论、扩展分享和案例展示；不把一般问答混入 Issue。
- 安全问题只通过 GitHub Private Vulnerability Reporting 或 `security@pinvou.com` 私密提交。
- 首发不创建 Discord、Slack、微信群等额外即时沟通渠道，避免单人维护时分散响应和沉淀。
- `SUPPORT.md` 明确社区支持为 best-effort，不承诺响应时间或 SLA；企业支持通过官网入口单独承接。

### 公开主线治理（已确认）

- `main` 禁止直接 push，全部修改通过 PR 合入；禁止删除或强制推送公开主线。
- 单人维护阶段不设置“必须由另一名审批人批准”，避免维护者无法合并自己的 PR；以必需 CI 状态检查作为合并门槛。
- 必需检查至少包括 DCO、密钥与内部地址扫描、公开/私有边界检查、格式、单测、关键构建、submodule 状态和 fork guard。
- 默认使用 Squash Merge 保持公开历史清晰；外部贡献者不拥有直接合并权限，由维护者检查并合并。
- `v*` Release tag 创建后不得移动、覆盖或复用；修复通过新版本 tag 发布。
- 紧急修复仍创建最小 PR 并通过必要门禁，不以直接 push 或绕过保护作为常规流程。

### 公开版权与维护者身份（已确认）

- MIT 版权声明统一使用 `Copyright (c) 2026 Pinvou`，公开维护者身份使用 `Pinvou organization`。
- README、GitHub 仓库信息、Release Notes 和社区支持入口不公开维护者个人姓名或个人邮箱。
- `TRADEMARKS.md` 保护 Pinvou 名称、Logo 和官方发行版身份；如公司要求披露法定权利主体，只在适当的法律/商标文档和官方用户协议中使用公司法定全称。

- [x] 添加顶层 `LICENSE`。
- [x] 生成 `THIRD_PARTY_NOTICES.md`。
- [x] 为第三方代码、技能、二进制、图片和字体建立来源/版本/许可证清单。
- [x] 移除或替换无明确授权的素材。
- [x] 提供 GitHub dependency graph 的 SPDX SBOM 导出说明；Release 级 SBOM 仍在 M7 验收。
- [x] 添加 `SECURITY.md`。
- [x] 添加 `TRADEMARKS.md`，保护 Pinvou 名称、Logo 和官方发行版身份。
- [x] 添加英文 `CONTRIBUTING.md`。
- [x] 添加 `CODE_OF_CONDUCT.md`。
- [x] 添加 `SUPPORT.md`，明确单人维护、best-effort support，不承诺 SLA。
- [ ] 添加 `CODEOWNERS`。
- [x] 添加 Bug、Feature Issue 模板，并使用 Private Vulnerability Reporting 接收安全问题。
- [x] 选用轻量的 MIT + DCO 贡献授权机制。
- [x] CI 增加 Gitleaks、dependency review 和 CodeQL；更细的第三方再分发许可核对在 Release 前人工签收。

## M6：为 GitHub 访客做好产品入口（P1）

> 完成标准：新访客 30 秒内看懂 Pinvou，5 分钟内能下载体验或开始本地开发。

- [x] `README.md` 使用英文主入口。
- [x] 添加 `README.zh-CN.md`，两个版本顶部互相链接。
- [ ] README 首屏放一句定位、主视觉、演示 GIF、下载和 Quickstart。
- [ ] 制作 30–60 秒英文无声演示 GIF/视频。
- [x] README 提供三张产品界面截图。
- [ ] 用三个具体场景说明产品价值，不罗列内部模块名。
- [x] 编写源码运行 Quickstart。
- [ ] 提供一条开发启动命令，自动完成必要检查。
- [x] 在 README 列出支持的操作系统、首发 macOS 范围、模型提供方和数据外发边界。
- [ ] 写一个简短、诚实的同类产品差异说明。
- [ ] GitHub repository description 和 Topics 已完成；Social Preview 仍待人工设置。
- [x] 开启 GitHub Discussions。

## M7：构建、发行与公开放行（P0）

> 完成标准：新仓库可构建，公开安装包可下载，全部放行检查通过。

### 社区首发平台范围（已确认）

- 首发正式支持 Linux x86_64、Linux ARM64、Windows x86_64，以及 macOS Apple Silicon（arm64，macOS 14.0+）；macOS 不延期到 roadmap，Intel Mac 首发不承诺支持。
- macOS 适配以现有 `origin/feat/macos` 为基础，已包含平台抽象、DMG、OTA、SenseVoice、PrivacyInfo、entitlements、构建/发布脚本和 CI；合入最新主线并完成开源清理后重新执行全量回归。
- 同一个公开 GitHub Release 同时提供 Community 和 Official 两种 macOS 包。
- macOS Community 包对应公开源码，不使用 Developer ID，不做 Apple notarization 或 staple；明确标注 `Unsigned Community Build`，提供 Gatekeeper 首次打开说明和 SHA-256。
- Community 不因缺少 Developer ID 而明文持久化用户凭据；若稳定 Keychain 访问无法保证，首版接受 Keychain 额外提示或暂不持久化相关凭据。
- Community 首版不提供应用内自动更新；后续如启用 GitHub Releases 自动更新，仍须使用独立 updater 签名与客户端验签，不能只依赖 SHA-256、Bundle ID 或 HTTPS。
- macOS Official 包使用 Developer ID Application、Hardened Runtime、Apple notarization 和 staple，并恢复稳定的 Keychain 凭据存储。
- Apple Developer Program 与 Developer ID 权限只阻塞 Official 正式签名包，不阻塞 Community unsigned/ad-hoc 首发资产。证书、私钥、notarization 凭证和 updater 私钥不得通过聊天传递或进入源码/Git 历史。

### 首个公开版本（已确认）

- 首个公开 tag 使用 `v0.7.0`，不从 `0.1.0` 重置，也暂不宣称 `1.0.0` 稳定版。
- Community 和 Official 保持相同应用显示身份；Release 资产分别标注 `community` 与 `official-<build-id>`，Official manifest 记录对应的公开 community core commit。
- Git tag `v0.7.0` 指向公开源码；官网、使用指南、更新日志、应用内版本、安装包名称和 GitHub Release Notes 全部统一到 `0.7.0`。

### Windows 代码签名（已确认）

- 社区版和官方版 Windows 安装包沿用相同应用身份并互斥覆盖安装；两种资产通过文件名、许可说明和 Official build ID 区分，社区版首版不启用应用内更新。
- 公开 CI 生成可复现的未签名候选包；私有 Windows 签名环境完成正式签名和验签后，将签名产物上传到公开 GitHub Release。
- 沃通签名工具、UKey、证书凭据、密码、证书指纹和私有签名环境信息不得进入公开源码或 Git 历史；公开仓库只保留不含私密信息的构建、产物交接和公开验签接口。
- 当前私有分支内的 `wosigncode.exe`、`wosigncodecmd.exe` 未发现可再分发授权，默认不进入开放仓库；只有取得明确书面再分发授权后才可重新评估。
- Release 同时发布 SHA-256、签名状态和签名者说明；签名后的社区包仍须能追溯到对应公开 tag/commit。

### Community 与 Official 许可标识（已确认）

- `Pinvou/pinvou-agent` 公开源码和 `Pinvou Community` 二进制按 MIT 发布；GitHub 自动生成的 Source code 压缩包只对应 Community 源码。
- `Pinvou Official` 的私有源码不公开，私有扩展保留所有权利，不因其使用 MIT 社区核心而转为 MIT。
- 公开 GitHub Release 中的 Official 安装包按 Pinvou 官方用户协议分发，并明确说明其中包含不属于 MIT 公开仓库的 proprietary components。
- Official 包继续保留其所使用的 MIT 社区核心及全部第三方组件的许可证、NOTICE 和归因，不把整个 Official 包笼统标记为“MIT 开源版”。
- Official Release 资产必须使用独立名称，并标注官方 build ID、对应公开 core commit 和不可由公开源码完整复现；此标识不要求公开 Official 源码或私有扩展实现。

- [x] 在禁用 credential helper、禁止交互认证的全新环境执行 public recursive clone。
- [ ] Linux x86_64 构建通过。
- [ ] Linux arm64 构建通过。
- [ ] Windows 构建通过。
- [ ] macOS Apple Silicon Community unsigned/ad-hoc 构建、Gatekeeper 指引、安全凭据处理和 Tauri 更新验签通过。
- [ ] macOS Apple Silicon Official 构建、Developer ID 签名、notarization、staple、Keychain 和更新验签通过。
- [x] 公开 CI 覆盖格式、单测、构建、submodule 和 fork guard。
- [x] 启用 Secret Scanning 和 Push Protection。
- [x] 启用主分支保护，并要求 DCO、required-gate、Gitleaks 和 dependency-review。
- [ ] 准备第一个公开版本号和英文 Release Notes。
- [ ] 发布至少 Linux 和 Windows 安装包；社区包与包含私有扩展的官方包使用明确不同的文件名、Official build ID 和许可说明，社区包不得串用官方更新通道。
- [ ] 社区包必须对应公开 tag/commit 并可复现；官方包标注 private extensions、官方 build ID 和对应 community core commit，不宣称可由公开源码完整复现。
- [ ] 官方包允许包含官方服务地址，但必须执行独立凭证扫描，确认不含可复用 Key、客户端 Secret、SSH 凭证或管理 Token。
- [ ] 发布安装包 SHA-256 校验值。
- [ ] 验证新机下载、安装、首启、配置模型和完成首个任务。
- [ ] 再执行密钥扫描、第三方许可扫描和 SBOM 检查。
- [ ] 按本文“最终公开闸门”逐项签收。

## M8：首发和获取第一批 Stars（P1）

> 完成标准：首发材料、反馈入口和首周维护时间准备完成。

- [ ] 准备简短英文发布文案，围绕真实场景，不堆技术名词。
- [ ] 准备 Hacker News `Show HN`、Reddit 和 Product Hunt 素材。
- [ ] 准备中文社区的同步发布文案。
- [ ] 预留首发后 7 天处理安装问题和高价值 Issue。
- [ ] 建立简单的 issue label：`bug`、`feature`、`good first issue`、`help wanted`、`platform`、`i18n`。
- [ ] 列出 5–10 个真实可做的 `good first issue`。
- [ ] 在收到反馈后优先修复安装、启动、配置和首个任务的阻断问题。

## M9：单人可持续维护（P1）

> 完成标准：社区知道项目是单人维护，维护者不被无限支持需求拖垮。

- [x] 在 `SUPPORT.md` 明确社区支持为 best-effort，不提供免费企业 SLA。
- [ ] 每周固定 1–2 个时段集中处理 Issue/PR，不承诺即时响应。
- [x] Bug 模板要求版本、平台、复现步骤和脱敏日志。
- [x] 使用 GitHub Issue/PR 作为开发协作入口，Discussions 承接一般问答和想法。
- [ ] 路线图只放已决定的事，不公开承诺未排期功能。
- [ ] 每月或每个稳定里程碑发布一次，不追求高频发版。
- [ ] 将通用贡献尽量推向 CodeWhale 上游，控制 fork 维护成本。

## 最终公开闸门

只有以下全部满足才对外发布：

- [x] 公开源码和 Git 历史不含已知 H3C 内部代码、资产、凭证和运维链路；Official Release 资产边界仍需在上传前单独签收。
- [x] 当前文件和公开仓库全历史密钥扫描通过。
- [x] Community 公开依赖来源不包含私有仓库、内网或共享 Key。
- [x] Submodule 公开可达，`git clone --recursive` 通过。
- [ ] 英文 UI、system prompt、通用 MCP 描述和错误信息真实用户旅程已决定延期，不阻断 `v0.7.0`。
- [ ] 中文核心体验回归通过。
- [x] `LICENSE`、`THIRD_PARTY_NOTICES.md`、`SECURITY.md`、`TRADEMARKS.md` 和 `CONTRIBUTING.md` 齐全。
- [ ] Linux 和 Windows 安装包已在新机验证。
- [ ] 英文 README、演示 GIF、Quickstart 和首个 GitHub Release 齐全。
- [x] GitHub Secret Scanning、Push Protection、CI 和主分支保护已启用。
- [ ] 首发后一周的维护时间已预留。

## 当前执行顺序

1. 将本 M0 基线和状态清单纳入私有仓库，后续以证据更新，不再重复做已完成事项。
2. 在公开仓库通过 PR 将版本从 `0.6.4` 统一升级到 `0.7.0`，准备英文 Release Notes。
3. 由各平台负责人完成 Community/Official 安装包、签名、公证和新机旅程验证。
4. 对最终 Release 资产执行凭证、许可证、SBOM、SHA-256 和 Community core commit 签收。
5. 创建不可移动的 `v0.7.0` tag 和公开 GitHub Release，上传两类明确区分的资产。
6. 按 M8 集中首发，随后执行 M9 单人维护节奏；M4、官网真实下载页和通用 BYOK 作为后续迭代。
