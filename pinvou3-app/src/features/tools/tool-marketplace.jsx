import { BookOpen, Briefcase, Building2, CloudSun, Code, FileText, Hexagon, Layout, LineChart, Mail, MessageCircle, Navigation, Package, Palette, Presentation, Search, Send, Server, Video } from '../../components/icons.jsx';
import { dict } from '../../shared/i18n.js';
import { builtinToolShortName, MODE_CONTROLLED_BUILTIN_SKILL_IDS } from './builtin-plugin-logic.js';

const tc = (t) => (t && t.uiToolCommon) || dict.zh.uiToolCommon;

    // ══ 编入新工具/技能指南(2026-08 能力包归一后) ════════════════════════════
    // 一个产品 = 一张卡。禁止「连接器卡 + 技能卡」双卡并存,历史上 PPT/公文/ima
    // 都踩过(重复卡、分类漂移),归一后规则如下:
    // 1. 纯 MCP 工具:在 resources/mcp-servers/<id>/manifest.json 声明,本文件加
    //    连接器条目(category 直接落业务类 id:collab/docs/dev/finance/life)。
    // 2. 组合包(MCP + 配套技能):manifest 加 companion_skills 声明 + 预置技能落
    //    skill-marketplace/<name>/SKILL.md。✦不要✦加连接器条目以外的第二张卡——
    //    商店卡由后端技能数据自动合成(companionSkillCards),连接器条目只作元数据
    //    源(详情/配置/欢迎语),渲染层按 bundleMcpIds 自动过滤,不会出现两张卡。
    // 3. 纯技能:只注册预置技能(preset_manifests),不加 tsSkillsData 静态卡——
    //    静态卡机制已随 s4/s7 删除,合成路径全覆盖。
    // 4. 连接器认领的技能(如 ima-skills 归 ima):加入 ToolStoreView 的
    //    CONNECTOR_CLAIMED_SKILLS,防止单独成卡。
    // 5. 占位卡(未上线)用 backendId: null;builtin 技能(如视觉设计)保留 builtin 标记。
    // 6. 文案三语走 i18n overlay(uiToolDetails/storeData),本文件只留中文原文。
    // 旧实现待更新:7 个 MCP manifest 仍内嵌 routing_rules/tool_table_entries
    // (weather/iwencai/obsidian/patsnap-search/qcc/canva-mcp/yuandian-mcp)——
    // 非组合包无双写问题,新增组合包时勿模仿,知识一律进 SKILL.md。
    // 版本号:feishu/wecom/dingtalk/tmeet 卡片 version 与 connectors.lock.json /
    // npm pin 强一致，由 connector_online_install_contract 测试钉住防腐化；运行时
    // 仍以后端 bundle_readiness 的 bundle.version 为准，卡片值作 overlay 回退。
    // ════════════════════════════════════════════════════════════════════════
    // 输出渲染分发：失败→红字；大输出→receipt；高频工具→定制视图；其余→纯文本
    const tsToolsData = [
      { id: 1, backendId: 'weather', mcpServer: true, title: '高德天气', subtitle: '高德地图实时天气与多日预报', category: 'life', type: 'MCP Server', version: 'v1.0.0', latency: '<50ms', desc: '通过高德地图 Web 服务 API 查询全国城市实时天气与未来多日预报。需要填写你自己的高德 Web 服务 API Key，密钥只写入本机系统凭据。', icon: CloudSun, color: 'bg-gradient-to-b from-sky-400 to-blue-500', installed: false, authRequired: true, configTitle: '高德天气 Key', configDescription: 'Key 只保存在本机凭据，不写入 mcp.json。', configDocUrl: 'https://console.amap.com/dev/key/app', configDocLabel: '去创建 Web 服务 Key', configFields: [{ key: 'AMAP_KEY', label: 'API Key', required: true, target: 'env', secret: true, placeholder: '粘贴高德 Web 服务 Key', helpText: '请选择「Web 服务」类型。' }], welcomeQueries: ['杭州今天天气', '北京这周会下雨吗', '上海明天穿什么'] },
      { id: 2, backendId: 'iwencai', mcpServer: true, title: '同花顺问财', subtitle: 'A股行情、财务、选股、宏观、新闻', category: 'finance', type: 'MCP Server', version: 'v1.0.0', latency: '<500ms', desc: '基于同花顺问财官方 API，提供 12 个金融查询工具。需要填写你自己的问财 API Key，密钥只写入本机系统凭据。', icon: LineChart, color: 'bg-gradient-to-b from-red-400 to-red-600', installed: false, authRequired: true, configTitle: '问财 Key', configDescription: 'Key 只保存在本机凭据，不写入 mcp.json。', configDocUrl: 'https://www.iwencai.com/skillhub', configDocLabel: '打开问财 SkillHub', configFields: [{ key: 'IWENCAI_API_KEY', label: 'API Key', required: true, target: 'env', secret: true, placeholder: '粘贴 IWENCAI_API_KEY', helpText: '进入任一官方 Skill，在「安装方式」中复制。' }], welcomeQueries: ['茅台最新股价', '今天大盘怎么样', '市盈率低于10的银行股', '最近降息新闻'] },
      { id: 3, backendId: null, title: 'QQ邮箱 API', subtitle: '智能邮件收发与线程提炼', category: 'collab', type: 'REST API', version: 'v1.4.2', latency: '<120ms', desc: '提供标准的邮件收发、搜索和整理接口。结合大模型可实现自然语言读取邮件内容、汇总长线程对话、自动归档管理文件夹。', icon: Mail, color: 'bg-gradient-to-b from-amber-400 to-orange-500', installed: false, authRequired: true },
      { id: 4, backendId: 'ima', imaOpenapi: true, title: '腾讯 ima', subtitle: '用 OpenAPI 操作 ima 笔记与知识库', category: 'docs', type: 'OpenAPI Skill', version: 'v1.1.8', latency: '云端', desc: '接入腾讯 ima OpenAPI Skill：通过 Pinvou 内置的受控工具调用 ima.qq.com 官方 OpenAPI，支持笔记搜索/读取/创建/追加，以及知识库搜索、浏览、网页导入和内容添加。需要填写你自己的 Client ID 和 API Key，凭据只写入本机系统凭据，不进入对话、环境变量、仓库或 mcp.json。', icon: BookOpen, color: 'bg-gradient-to-b from-sky-500 to-indigo-600', installed: false, authRequired: true, configTitle: '连接腾讯 ima', configDescription: '凭据只保存在本机，用于启用 IMA OpenAPI Skill。', configDocUrl: 'https://ima.qq.com/agent-interface', configDocLabel: '获取 Client ID / API Key', configFields: [{ key: 'IMA_CLIENT_ID', label: 'Client ID', required: true, target: 'credential', secret: true, placeholder: 'Client ID' }, { key: 'IMA_API_KEY', label: 'API Key', required: true, target: 'credential', secret: true, placeholder: 'API Key' }], welcomeQueries: ['搜索我的 ima 知识库', '列出我有哪些 ima 笔记', '把这段内容新建为 ima 笔记', '在 ima 知识库里查产品方案'] },
      { id: 5, backendId: null, title: '乐享文档连接器', subtitle: '企业知识文档全量检索', category: 'docs', type: 'Webhook/API', version: 'v1.1.0', latency: '<80ms', desc: '支持通过 API 搜索、创建和管理乐享知识库中的文档。支持批量导入 Markdown、按标签整理内容、实时订阅团队文档的更新动态。', icon: Hexagon, color: 'bg-gradient-to-b from-blue-400 to-blue-600', installed: false, authRequired: true },
      { id: 6, backendId: 'tencent-docs', mcpServer: true, title: '腾讯文档 MCP', subtitle: '官方远程 MCP：在线文档/表格/幻灯片读写与协作', category: 'docs', type: 'MCP Server', version: 'v1.0.41', latency: '云端', desc: '接入腾讯文档官方远程 MCP（通用文档 + PPT/Word/Excel 精细编辑四个服务）：以你的账号创建、读取、编辑在线文档、表格、幻灯片、思维导图、智能表格，管理知识库空间与文件，支持网页剪藏与本地文件上云；安装时自动配官方技能（场景路由 + 工具 API 参考）。需要腾讯文档个人 Token（官方页面 QQ/微信扫码获取），密钥只写入本机系统凭据。', icon: FileText, color: 'bg-gradient-to-b from-blue-500 to-indigo-600', installed: false, authRequired: true, configTitle: '连接腾讯文档', configDescription: 'Token 只保存在本机系统凭据，不写入 mcp.json，也不会进入对话。', configDocUrl: 'https://docs.qq.com/scenario/open-claw.html?nlc=1', configDocLabel: '去获取腾讯文档 Token', configFields: [{ key: 'TENCENT_DOCS_TOKEN', label: '个人 Token', required: true, target: 'bearer', secret: true, placeholder: '粘贴腾讯文档个人 Token', helpText: '打开授权页用 QQ 或微信扫码登录后复制 Token；请勿添加 Bearer 前缀。' }], welcomeQueries: ['列出我最近的腾讯文档', '把这份周报写成腾讯智能文档', '新建一个在线表格记录收支', '读取这个腾讯文档表格并分析'] },
      { id: 8, backendId: 'wecom-bot', mcpServer: true, title: '企微群机器人', subtitle: '向企业微信群推送消息与通知', category: 'collab', type: 'MCP Server', version: 'v1.0.0', latency: '云端', desc: '接入企业微信官方群机器人 webhook（消息推送 API）：让 AI 向绑定的群推送文本（可@成员）、Markdown、图文、图片和文件。需要填写你自己的群机器人 Webhook Key，密钥只写入本机系统凭据。数据经企业微信服务器（可选联网功能，opt-in）。', icon: MessageCircle, color: 'bg-gradient-to-b from-cyan-400 to-blue-500', installed: false, authRequired: true, configTitle: '企微群机器人 Key', configDescription: 'Key 只保存在本机凭据，不写入 mcp.json。', configDocUrl: 'https://developer.work.weixin.qq.com/document/path/91770', configDocLabel: '查看官方群机器人文档', configFields: [{ key: 'WECOM_BOT_KEY', label: 'Webhook Key', required: true, target: 'env', secret: true, placeholder: '粘贴群机器人 key 或完整 webhook 地址', helpText: '企业微信群里右上角「…」→ 群机器人 → 添加机器人，复制 webhook 地址即可。' }], welcomeQueries: ['往群里发一条今日天气播报', '把这份周报整理成 Markdown 发到企微群', '@所有人：下午三点例会别忘了', '把这个文件发到企微群'] },
      { id: 9, backendId: 'feishu', feishuCli: true, title: '飞书（Lark）', subtitle: '以你本人身份操作飞书文档/日历/表格/消息', category: 'collab', type: 'CLI + 官方技能', version: 'v1.0.95', latency: '云端', desc: '接入飞书官方 CLI + 官方域技能（MIT）：让 AI 以你本人身份读写云文档、查改日历、操作多维表格（Base）与电子表格、收发消息、管理知识库与任务。点「连接飞书」浏览器一键授权，全程不填 key。数据经飞书云 OpenAPI（可选联网功能，opt-in）。', icon: Send, color: 'bg-gradient-to-b from-teal-400 to-emerald-500', installed: false, authRequired: true, configFields: [], welcomeQueries: ['读飞书文档帮我做一份 PPT', '把飞书文档整理成摘要', '查我今天的飞书日历', '看看我飞书里的待办任务'] },
      { id: 17, backendId: 'eip', eipCli: true, internal: 'H3C', title: 'H3C 员工门户（EIP）', subtitle: '以你本人 SSO 身份查考勤/假期/加班/待办/找人/资讯/消费', category: 'h3c', type: 'CLI + 官方技能', version: 'v1.0.0', latency: '内网', desc: '接入 H3C EIP 员工门户：以你本人 SSO 身份查考勤打卡、假期余额、加班时数、待办审批、搜同事、看集团新闻公告、查食堂消费与报销等。点「连接」浏览器 SSO 一键登录，全程不填 key。需公司内网。', icon: Briefcase, color: 'bg-gradient-to-b from-indigo-400 to-blue-600', installed: false, authRequired: true, configFields: [], welcomeQueries: ['我这个月考勤有没有异常', '我还有几天年假', '有没有待办审批', '帮我查同事的联系方式'] },
      { id: 18, backendId: 'zhidao', zhidaoCli: true, internal: 'H3C', title: 'H3C 知道 · 内部知识库', subtitle: '以你本人 SSO 身份检索 H3C 内部知识与资料', category: 'h3c', type: 'CLI + 官方技能', version: 'v1.0.0', latency: '内网', desc: '接入 H3C「知道」内部知识库：以你本人 SSO 身份搜索知识条目、资料库和文档内容，支持基于企业内部知识进行问答与资料定位。点「连接」扫码或浏览器 SSO 登录，全程不填 key。需公司内网。', icon: BookOpen, color: 'bg-gradient-to-b from-emerald-400 to-teal-600', installed: false, authRequired: true, configFields: [], welcomeQueries: ['在知道里搜一下交换机配置规范', '帮我查内部资料库里的安全方案', '知道里有没有这类故障处理文档', '帮我找这个产品的内部知识'] },
      { id: 22, backendId: 'tmeet', tmeetCli: true, title: '腾讯会议', subtitle: '以你本人身份管理会议/录制/纪要/参会报告', category: 'collab', type: 'CLI + 官方技能', version: 'v1.0.18', latency: '云端', desc: '接入腾讯会议官方 CLI（@tencentcloud/tmeet）+ 官方技能：让 AI 以你本人身份创建、查询、修改和取消腾讯会议，查询受邀人、参会报告、录制、转写与智能纪要，并支持会中呼叫成员入会。点「连接」打开腾讯会议授权页扫码登录，全程不填 key。', icon: Video, color: 'bg-gradient-to-b from-sky-400 to-blue-600', installed: false, authRequired: true, configFields: [], welcomeQueries: ['帮我创建一个腾讯会议', '查一下我的腾讯会议录制', '看看最近会议的智能纪要', '查询这场腾讯会议的参会人'] },
      { id: 99, backendId: 'wecom', wecomCli: true, title: '企业微信', subtitle: '以你本人身份操作企微消息/邮件/文档/会议/日程', category: 'collab', type: 'CLI + 官方技能', version: 'v1.2.1', latency: '云端', desc: '接入企业微信官方 CLI（@wecom/cli，MIT）+ 官方域技能：让 AI 以你本人身份收发消息、收发邮件、读写文档/智能表格/在线表格/智能文档、管理微盘文件、创建/查询会议与日程、管理待办、查询通讯录。点「连接」用企业微信 App 扫码授权，全程不填 key。数据经企业微信云（可选联网功能，opt-in）。', icon: MessageCircle, color: 'bg-gradient-to-b from-cyan-400 to-blue-500', installed: false, authRequired: true, configFields: [], welcomeQueries: ['把这段内容写成企微智能文档', '读一下我企微某篇文档的内容', '在企微智能表格里新建一张子表', '查一下企微智能表格里的数据'] },
      { id: 10, backendId: 'dingtalk', dingtalkCli: true, title: '钉钉', subtitle: '以你本人身份操作钉钉文档/日历/表格/消息', category: 'collab', type: 'CLI + 官方技能', version: 'v1.0.61', latency: '云端', desc: '接入钉钉官方 DingTalk Workspace CLI（dws，Apache-2.0）+ 官方技能：让 AI 以你本人身份读写钉钉文档、查改日历、操作 AI 表格/在线表格、收发群聊消息、处理待办/审批/日志/邮箱等。点「连接」用钉钉 App 扫码授权，全程不填 key。', icon: Navigation, color: 'bg-gradient-to-b from-blue-400 to-indigo-500', installed: false, authRequired: true, configFields: [], welcomeQueries: ['读一下我的钉钉文档', '查我今天的钉钉日程', '在钉钉 AI 表格里查数据', '看看我的钉钉待办'] },
      { id: 11, backendId: null, title: 'TAPD 敏捷研发', subtitle: '缺陷与迭代的自动化追踪', category: 'dev', type: 'Action Skill', version: 'v2.8.0', latency: '<60ms', desc: '研发管理核心工具。允许 AI 查询项目迭代进度、自动拆分需求条目、更新缺陷状态，实现从需求到发布的研发全生命周期数字化。', icon: Layout, color: 'bg-gradient-to-b from-violet-500 to-fuchsia-600', installed: false, authRequired: true },
      { id: 12, backendId: null, title: 'CNB 云原生管线', subtitle: '代码仓库与 CI/CD 调度', category: 'dev', type: 'MCP Server', version: 'v1.0.0', latency: '<40ms', desc: '将云原生开发能力赋予大模型。支持通过自然语言进行代码仓库检索、提交 Issue、审查 PR、触发并监控流水线部署等极客操作。', icon: Code, color: 'bg-gradient-to-b from-orange-400 to-rose-500', installed: false, authRequired: true },
      { id: 13, backendId: 'qcc', oauthMcp: true, oauthServerName: 'qcc-company', title: '企查查', subtitle: '企业工商数据授权查询', category: 'finance', type: 'Remote MCP', version: 'v1.0.0', latency: '云端', desc: '接入企查查智能体数据平台 qcc-company 远程 MCP。点「连接」默认通过企查查账号 OAuth 授权；如有企查查 API Key 也可选填，密钥只写入本机系统凭据。', icon: Building2, color: 'bg-gradient-to-br from-blue-600 to-cyan-500', installed: false, authRequired: true, configTitle: '企查查 API Key（可选）', configDescription: 'API Key 只保存在本机系统凭据，不写入 mcp.json。留空则直接使用 OAuth 授权。', configFields: [{ key: 'QCC_API_KEY', label: '企查查 API Key', required: false, target: 'bearer', secret: true, placeholder: '粘贴企查查 API Key（可留空）', helpText: '可选。默认使用 OAuth 授权；如需以 API Key 方式接入再填写。' }], welcomeQueries: ['查一下华为的工商信息', '腾讯的工商登记信息', '比亚迪有哪些对外投资', '阿里巴巴的股东结构'] },
      { id: 20, backendId: 'patsnap-search', mcpServer: true, title: '智慧芽专利&文献', subtitle: '全球专利与论文融合检索，支持公开号详情获取', category: 'docs', type: 'MCP Server', version: 'v1.0.0', latency: '云端', desc: '接入智慧芽远程 MCP，在全球专利数据库和文献库中进行融合检索，支持自然语言、语义搜索、关键词检索和多维过滤，并可按专利公开号或结果 URL 拉取 Markdown 详情。需要填写智慧芽 API Key，密钥只写入本机系统凭据，mcp.json 仅保存环境变量占位符。', icon: Search, color: 'bg-gradient-to-b from-emerald-500 to-cyan-600', installed: false, authRequired: true, configTitle: '填写智慧芽 API Key', configDescription: 'API Key 仅存储在本机系统凭据中，不会明文写入 mcp.json；连接智慧芽服务时通过 Authorization 请求头发送。', configDocUrl: 'https://open.zhihuiya.com/dashboard/api-keys', configDocLabel: '查看 API Key 获取说明', configFields: [{ key: 'PATSNAP_API_KEY', label: '智慧芽 API Key', required: true, target: 'bearer', secret: true, placeholder: '粘贴你的智慧芽 API Key', helpText: '请从智慧芽开放平台或企业管理员提供的 MCP/API 凭证中获取。' }], welcomeQueries: ['检索固态电池电解质相关专利和论文', '查找近五年 CRISPR 递送系统核心专利和文献', '获取公开号 CN109123456A 的专利详情', '分析宁德时代钠离子电池方向专利布局'] },
      { id: 21, backendId: 'canva-mcp', oauthMcp: true, oauthServerName: 'canva_mcp', title: 'Canva 可画', subtitle: '海报、演示文稿、封面与品牌模板设计', category: 'life', type: 'Remote MCP', version: 'v1.0.0', latency: '云端', desc: '接入 Canva 可画远程 MCP。支持通过自然语言生成和编辑海报、演示文稿、小红书封面、品牌模板等设计内容；点「连接」后会打开浏览器进行 Canva 可画账号授权，全程不填写 API Key。设计指令、素材、文件夹和品牌模板相关内容会发送到 Canva 可画远程 MCP 服务。', icon: Palette, color: 'bg-gradient-to-b from-cyan-500 to-pink-500', installed: false, authRequired: true, configFields: [], welcomeQueries: ['帮我生成一张新品发布海报', '做一份三页产品介绍演示文稿', '设计一张小红书封面', '用品牌模板生成活动宣传图'] },
      { id: 14, backendId: 'obsidian', mcpServer: true, title: 'Obsidian 知识库', subtitle: '检索并管理本机 Obsidian 笔记，读写你的个人知识', category: 'docs', type: 'MCP Server', version: 'v1.1.0', latency: '<30ms', desc: '把你本机的 Obsidian 笔记库（vault）接入大模型。支持全文检索、读取、新建、编辑、改名（自动修双链）与删除——让 AI 基于并维护你自己沉淀的知识。自动识别当前打开的库，无需手动配置；笔记不出本机、模型也在本机，知识与算力全链路不出域。', icon: BookOpen, color: 'bg-gradient-to-b from-violet-500 to-purple-700', installed: false, authRequired: false, welcomeQueries: ['帮我搜一下我的笔记', '帮我新建一篇笔记记录今天的想法', '我的知识库有哪些文档？', '总结一下我的笔记'] },
      { id: 19, backendId: 'yuandian-mcp', oauthMcp: true, oauthServerName: 'yuandian_mcp', title: '华宇元典法律数据', subtitle: '法律法规、案例文书与企业司法风险查询', category: 'docs', type: 'Remote MCP', version: 'v1.0.0', latency: '云端', desc: '接入华宇元典开放平台远程 MCP。支持法律法规、裁判案例、企业司法风险等法律数据检索；点「连接」后会打开浏览器进行元典账号授权，全程不填写 API Key。', icon: BookOpen, color: 'bg-gradient-to-b from-emerald-500 to-cyan-700', installed: false, authRequired: true, configFields: [], welcomeQueries: ['检索一下劳动合同解除相关案例', '查一下公司股权责任相关法规', '帮我分析企业司法风险', '找一下最近的裁判观点'] },
      { id: 16, backendId: 'gongwen', mcpServer: true, title: '公文写作', subtitle: '党政机关公文直出 GB/T 9704 合规 .docx', category: 'docs', type: 'MCP Server', version: 'v1.0.0', latency: '本地', desc: '说“写个通知 / 起草意见”，AI 按文种结构与固定话术写好内容，渲染器套党政机关公文国标格式（方正小标宋标题、仿宋_GB2312 正文、国标页边距、红头与红色分隔线）直出 .docx，全程本地、数据不出机。配合「党政机关公文写作」技能效果最佳。首次安装自动下载 python-docx 依赖（需联网）。', icon: FileText, color: 'bg-gradient-to-b from-red-500 to-rose-700', installed: false, authRequired: false, welcomeQueries: ['起草一份关于印发管理办法的通知', '写一份加强某项工作的实施意见', '拟一份会议通知', '写一份情况报告'] },
    ];

    // 本地能力的元数据(非商店连接器卡):组合包化的 pptx 已无连接器卡,但安装后
    // 「去新对话」引导卡按 backendId 取标题/描述/欢迎问题(三语 overlay 同 localizeTool);
    // category 同时供商店组合包合成卡确定业务分区。
    const tsToolWelcomeData = [
      { backendId: 'pptx', title: 'PPT 生成', category: 'docs', desc: '说“做个 PPT / 汇报”，AI 先列大纲让你确认，再按内容自动选主题（9 套）生成可编辑 .pptx——真·图表、自带封面缩略图，全程本地、数据不出机。首次安装会自动下载 python-pptx 依赖（需联网）。', icon: Presentation, welcomeQueries: ['做个 Q2 季度汇报 PPT', '帮我做一份产品介绍 PPT', '做个项目方案演示', '做个公司介绍 PPT'] },
    ];

    // configFields 字段级展示文案白名单：两处合并（localizeTool 的源数据×词典
    // overlay、mergeConfigFields 的后端功能事实×overlay）共用同一策略——只有这些
    // key 允许被 overlay 覆盖，其余（key/required/target/secret 等功能事实）一律
    // 保留源侧。后端 bundle_readiness 只下发功能事实、从不下发展示文案（见
    // src-tauri marketplace/bundle.rs ConfigFieldSpec），展示文案的唯一来源就是
    // overlay/词典；白名单是纵深防御：后端未来若加入展示字段，这里保证三语
    // overlay 仍然赢。
    const CONFIG_FIELD_DISPLAY_KEYS = ['label', 'helpText', 'placeholder'];

    // overlay 提供 configFields 时按字段 key 深合并，只覆盖白名单内的展示文案，其余源字段保留。
    const localizeTool = (tool, t) => {
      if (!tool) return tool;
      const tools = t?.uiToolDetails?.tools;
      const localized = tools && (tool.backendId ? tools[tool.backendId] : tools['card' + tool.id]);
      if (!localized) return tool;
      const merged = { ...tool, ...localized };
      if (Array.isArray(localized.configFields) && Array.isArray(tool.configFields)) {
        merged.configFields = tool.configFields.map((src) => {
          const ov = localized.configFields.find((f) => f && f.key === src.key);
          if (!ov) return src;
          const out = { ...src };
          for (const k of CONFIG_FIELD_DISPLAY_KEYS) if (ov[k] != null) out[k] = ov[k];
          return out;
        });
      }
      return merged;
    };

    // 后端 config_fields（key/required/secret/target 功能事实，bundle_readiness 的
    // bundle 字段）× 本地 overlay（label/placeholder/helpText 展示文案）按 key 合并；
    // 后端无字段时原样使用 overlay。功能事实（required/target/secret 等）保持后端
    // 胜出，展示文案按白名单一律 overlay 胜出（纵深防御，见上方白名单注释）。
    const mergeConfigFields = (backendFields, overlayFields) => {
      if (!Array.isArray(backendFields) || backendFields.length === 0) return overlayFields;
      const overlayByKey = {};
      (overlayFields || []).forEach((f) => { if (f && f.key) overlayByKey[f.key] = f; });
      return backendFields.map((f) => {
        const ov = overlayByKey[f.key] || {};
        const merged = { ...f };
        for (const k of CONFIG_FIELD_DISPLAY_KEYS) {
          if (ov[k] != null) merged[k] = ov[k];
        }
        return merged;
      });
    };

    // 技能市场预置卡(backendId 必须匹配 Rust SkillManifest.id)。技能=SKILL.md 目录,
    // 装到 bundle/skills/ 进 system prompt;与上方 MCP 工具(tsToolsData)并列两个子页。
    // ⚠ 此表已近退役:预置技能卡一律由后端数据合成(companionSkillCards),不要再往
    // 这里加条目(编入规则见 tsToolsData 头部指南)。s5 是唯一遗留——builtin 内置技能
    // (visual-design 在 bundle/skills/,无需安装),其包模型归属待决策,决策后此表应清空。
    // 注:pptx 不在此——它是「PPT 生成」MCP 的同名 companion 技能,卡片由后端
    // list_marketplace_skills 数据合成(见 ToolStoreView 的 companionSkillCards)。
    const tsSkillsData = [
      { id: 's5', title: '视觉设计', subtitle: '设计系统直出网页 / banner / 海报 / 简历', category: 'other', type: 'Skill', version: '内置', latency: '本地', desc: '内置自动技能:模型按需自动加载,以设计系统级审美直出网页 / banner / 海报 / 简历等。无需安装、随时可用。', icon: Palette, color: 'bg-gradient-to-b from-pink-400 to-fuchsia-600', installed: true, authRequired: false, builtin: true,
      // The audit badge keys mode control off backendId: this legacy entry's
      // real backend skill id (visual-design) must reach the builtin page so
      // its badge agrees with the store card (review round-5 M3).
      backendId: 'visual-design' },
    ];

    // 后端合成技能卡的补充展示数据(按 backendId 取):
    const tsSkillIconByName = { Presentation, FileText, LineChart, BookOpen };

    const tsCategories = [
      { id: 'all', label: '全部' },
      { id: 'h3c', label: 'H3C 内部' },
      { id: 'collab', label: '沟通协作' },
      { id: 'docs', label: '文档知识' },
      { id: 'dev', label: '研发' },
      { id: 'finance', label: '金融数据' },
      { id: 'life', label: '生活实用' },
      { id: 'other', label: '其他' },
    ];

    // ── 列表视图双维度分组(纯函数,ToolStoreView 消费;分支互斥、按优先级短路) ──
    // 类型分组顺序即展示顺序。bundleMcpIds = 含 companion_skills 的 MCP 工具 id 列表(工具包),
    // 由 list_marketplace_tools 反建;缺省视为无工具包。
    const TOOL_TYPE_GROUPS = ['bundle', 'mcp', 'skill', 'cli', 'api', 'upcoming'];
    const getToolTypeGroup = (tool, bundleMcpIds) => {
      if (!tool) return 'upcoming';
      // companion 合成卡优先判 bundle:其 category 可为 'skill'(独立技能)或
      // companion MCP 的业务类,不能靠 category 值判型,须按 companionBundle 标志短路
      if (tool.companionBundle) return 'bundle';
      // mcpServer/oauthMcp 显式标记位须先于 userUploaded/skill 分支（二轮评审：
      // 自定义上传的 MCP 卡同时带 userUploaded + mcpServer，旧顺序被错归为 Skill 组）。
      if (tool.mcpServer || tool.oauthMcp) return 'mcp';
      if (tool.userUploaded || tool.builtin || tool.category === 'skill') return 'skill';
      if (!tool.backendId) return 'upcoming';
      if (tool.feishuCli || tool.wecomCli || tool.dingtalkCli || tool.eipCli || tool.zhidaoCli || tool.tmeetCli) return 'cli';
      if (tool.imaOpenapi) return 'api';
      if (Array.isArray(bundleMcpIds) && bundleMcpIds.includes(tool.backendId)) return 'bundle';
      // mcpServer/oauthMcp 为显式标记位,分组不依赖可本地化的 type 文案;type 正则仅作兜底。
      if (tool.oauthMcp || tool.mcpServer || /mcp/i.test(tool.type || '')) return 'mcp';
      return 'api';
    };
    // 业务分组:直接取条目 category(数据即业务类 id)。无明确业务归属的条目
    // (自定义上传 MCP、用户上传技能、内置视觉设计,以及 category 缺失/无法识别/
    // 仅标 'skill' 的——'skill' 是类型而非业务类)一律落 'other' 单列「其他」,
    // 不再兜底 'life'——未知条目错挂「生活实用」比单列「其他」更难发现。
    // Official 独有的 H3C 工具仍保留独立业务组。
    const TOOL_BUSINESS_GROUPS = ['h3c', 'collab', 'docs', 'dev', 'finance', 'life', 'other'];
    const getToolBusinessGroup = (tool) => {
      if (!tool) return 'other';
      return TOOL_BUSINESS_GROUPS.includes(tool.category) ? tool.category : 'other';
    };

    // eslint-disable-next-line sonarjs/cognitive-complexity -- tool-card action buttons render many branches; legacy view; tracked separately
    const TsActionBtn = ({ tool, busy, onAction, onUpdate, onEditDisplay, size = 'sm', t }) => {
      const T = tc(t);
      const isLg = size === 'lg';
      const actionAttrs = {
        'data-testid': 'tool-store-action',
        'data-tool-id': tool.backendId || '',
        'data-tool-title': tool.title || '',
      };
      if (tool.builtin) {
        return (
          <span className={`${isLg ? 'px-6 py-2.5 text-[15px]' : 'px-4 py-1.5 text-[13px]'} rounded-full font-bold bg-slate-100 dark:bg-[#2C2C2E] text-slate-500 dark:text-slate-400 whitespace-nowrap`}>{T.builtinEnabled}</span>
        );
      }
      if (!tool.backendId) {
        return (
          <button {...actionAttrs} disabled className={`${isLg ? 'px-10 py-2.5 text-[15px]' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold bg-slate-100 dark:bg-[#1C1C1E] border border-slate-200 dark:border-slate-700 text-slate-400 dark:text-slate-500 cursor-not-allowed`}>
            {T.comingSoon}
          </button>
        );
      }
      if (busy) {
        return (
          <button {...actionAttrs} disabled className={`${isLg ? 'px-10 py-2.5 text-[15px]' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold opacity-50 cursor-wait bg-slate-100 dark:bg-[#1C1C1E] border border-slate-200 dark:border-slate-700 text-slate-500`}>
            ...
          </button>
        );
      }
      // Phase 2 第八刀：后端动作下发优先（bundle_readiness 的 actions，§3.3）。
      // 动作 id → 现有按钮样式/行为的映射表；flow 只标记交互形态（oauth 五态弹窗 /
      // CLI 流程卡都在 onAction 路由里分流）。OAuth MCP 本刀不下发 actions（后端
      // ready 尚未纳入 token 态），走下方旧分支；无 actions（取数失败/未知包）
      // 同样回退旧分支 —— 回退分支 Phase 3 清理。
      if (Array.isArray(tool.actions)) {
        const btnCls = {
          primary: `${isLg ? 'px-10 py-2.5 text-[15px] shadow-md shadow-blue-500/20' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold transition-all active:scale-95 bg-blue-600 hover:bg-blue-700 text-white`,
          danger: `${isLg ? 'px-10 py-2.5 text-[15px]' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold transition-all active:scale-95 bg-slate-100 dark:bg-[#2C2C2E] border border-slate-200 dark:border-slate-700 text-[#FF3B30] dark:text-[#FF453A] hover:bg-slate-200 dark:hover:bg-[#3A3A3C]`,
          outline: `${isLg ? 'px-6 py-2.5 text-[15px]' : 'px-4 py-1.5 text-[13px]'} rounded-full font-bold transition-all active:scale-95 bg-blue-50 dark:bg-blue-500/10 border border-blue-200 dark:border-blue-500/30 text-blue-600 dark:text-blue-400 hover:bg-blue-100 dark:hover:bg-blue-500/20 whitespace-nowrap`,
        };
        const specs = {
          install: { label: T.install, cls: btnCls.primary, run: () => onAction(tool.backendId, false) },
          configure: { label: T.configure, cls: btnCls.primary, run: () => onAction(tool.backendId, false) },
          connect: { label: (tool.authStatus && tool.authStatus !== 'not_installed') ? T.reauthorize : T.connect, cls: btnCls.primary, run: () => onAction(tool.backendId, false) },
          disconnect: { label: T.disconnect, cls: btnCls.danger, run: () => onAction(tool.backendId, true) },
          uninstall: { label: T.uninstall, cls: btnCls.danger, run: () => onAction(tool.backendId, true) },
          update: { label: T.update, cls: btnCls.outline, testid: 'tool-store-update', run: () => onUpdate && onUpdate(tool.backendId) },
          // edit_display：上传包展示名/说明编辑（extra 覆盖），点击由 onEditDisplay 弹编辑对话框
          edit_display: { label: T.editDisplay, cls: btnCls.outline, testid: 'tool-store-edit-display', run: () => onEditDisplay && onEditDisplay(tool.backendId) },
          // repair：CLI 包重连（flow=cli_connect）走 onAction；技能包覆盖重装即修复，走 onUpdate
          repair: { label: T.repair, cls: btnCls.outline, run: (a) => (a.flow && a.flow.kind === 'cli_connect' ? onAction(tool.backendId, false) : onUpdate && onUpdate(tool.backendId)) },
        };
        const list = tool.actions.filter(a => specs[a.id]);
        // ima 等认领技能的 update 挂在连接器卡上，后端 actions 不含它——沿用
        // updateAvailable 本地补一个 update 按钮（V5 认领退出后清理）。
        if (tool.updateAvailable && onUpdate && !list.some(a => a.id === 'update')) {
          list.push({ id: 'update', enabled: true });
        }
        return (
          <div className="flex items-center gap-2">
            {list.map((a) => {
              const s = specs[a.id];
              return (
                <button
                  key={a.id}
                  {...actionAttrs}
                  {...(s.testid ? { 'data-testid': s.testid } : {})}
                  disabled={!a.enabled}
                  title={a.reason || undefined}
                  onClick={(e) => { e.stopPropagation(); if (a.enabled) s.run(a); }}
                  className={`${s.cls} ${a.enabled ? '' : 'opacity-50 cursor-not-allowed'}`}
                >
                  {s.label}
                </button>
              );
            })}
          </div>
        );
      }
      // —— 以下为无 actions 时的旧分支（回退；Phase 3 清理）——
      if (tool.installed) {
        const uninstallBtn = (
          <button
            {...actionAttrs}
            onClick={(e) => { e.stopPropagation(); onAction(tool.backendId, true); }}
            className={`${isLg ? 'px-10 py-2.5 text-[15px]' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold transition-all active:scale-95 bg-slate-100 dark:bg-[#2C2C2E] border border-slate-200 dark:border-slate-700 text-[#FF3B30] dark:text-[#FF453A] hover:bg-slate-200 dark:hover:bg-[#3A3A3C]`}
          >
            {(tool.feishuCli || tool.wecomCli || tool.dingtalkCli || tool.eipCli || tool.zhidaoCli || tool.tmeetCli || tool.imaOpenapi || tool.oauthMcp) ? T.disconnect : T.uninstall}
          </button>
        );
        // 预置技能内容落后于商店版本(App 升级带入新版)时,并列给出"更新"入口;
        // 仅技能卡传 onUpdate 且带 updateAvailable,MCP/连接器卡行为不变。
        if (!tool.updateAvailable || !onUpdate) return uninstallBtn;
        return (
          <div className="flex items-center gap-2">
            <button
              data-testid="tool-store-update"
              data-tool-id={tool.backendId || ''}
              data-tool-title={tool.title || ''}
              onClick={(e) => { e.stopPropagation(); onUpdate(tool.backendId); }}
              className={`${isLg ? 'px-6 py-2.5 text-[15px]' : 'px-4 py-1.5 text-[13px]'} rounded-full font-bold transition-all active:scale-95 bg-blue-50 dark:bg-blue-500/10 border border-blue-200 dark:border-blue-500/30 text-blue-600 dark:text-blue-400 hover:bg-blue-100 dark:hover:bg-blue-500/20 whitespace-nowrap`}
            >
              {T.update}
            </button>
            {uninstallBtn}
          </div>
        );
      }
      if (tool.oauthMcp) {
        const retry = tool.authStatus && tool.authStatus !== 'not_installed';
        return (
          <button
            {...actionAttrs}
            onClick={(e) => { e.stopPropagation(); onAction(tool.backendId, false); }}
            className={`${isLg ? 'px-10 py-2.5 text-[15px] shadow-md shadow-blue-500/20' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold transition-all active:scale-95 bg-blue-600 hover:bg-blue-700 text-white`}
          >
            {retry ? T.reauthorize : T.connect}
          </button>
        );
      }
      const hasConfig = tool.configFields && tool.configFields.length > 0;
      return (
        <button
          {...actionAttrs}
          onClick={(e) => { e.stopPropagation(); onAction(tool.backendId, false); }}
          className={`${isLg ? 'px-10 py-2.5 text-[15px] shadow-md shadow-blue-500/20' : 'w-20 py-1.5 text-[14px]'} rounded-full font-bold transition-all active:scale-95 bg-blue-600 hover:bg-blue-700 text-white`}
        >
          {(tool.feishuCli || tool.wecomCli || tool.dingtalkCli || tool.eipCli || tool.zhidaoCli || tool.tmeetCli || tool.imaOpenapi || tool.oauthMcp) ? (hasConfig ? T.configure : T.connect) : (hasConfig ? T.configure : T.install)}
        </button>
      );
    };

    // ── 飞书连接流程卡（内联、非阻塞；取代旧的阻塞式扫码浮层）──


    // Read-only builtin plugin card (docs/builtin-toolset-contract.md §3.1
    // transparency/audit window): shows the tool list, security level, version
    // and data access scopes; no action buttons at all (no uninstall, no
    // toggle). The read-only badge reuses PlatformToolAction's Web read-only
    // degraded styling. All copy goes through uiBuiltinPlugins, falling back to
    // the zh dictionary per the tc precedent when copy is not passed; unknown
    // data-access scope keys render as-is.
    // Builtin skills (e.g. visual design) reuse this card: no tool list /
    // security level / data access (a pure prompt skill does not touch data
    // directly) — they declare facts with a kindLabel (type) row and a
    // versionText ("Built-in") row.
    const BuiltinPluginCard = ({ tool, copy }) => {
      const C = copy || dict.zh.uiBuiltinPlugins;
      const Icon = tool.icon || Package;
      const mcpTools = Array.isArray(tool.mcpTools) ? tool.mcpTools : [];
      const dataAccess = Array.isArray(tool.dataAccess) ? tool.dataAccess : [];
      const levelDesc = (tool.securityLevel && C.levels) ? C.levels[tool.securityLevel] : null;
      const rowCls = 'flex items-start gap-2 text-[12px]';
      const labelCls = 'shrink-0 w-[72px] font-semibold text-slate-400 dark:text-slate-500 leading-5';
      return (
        <div data-testid="builtin-plugin-card" data-tool-id={tool.backendId || ''}
          className="flex items-start gap-4 py-3 px-3 border-b border-slate-100 dark:border-white/5 last:border-0">
          <div className={`h-16 w-16 flex-shrink-0 rounded-[16px] border border-black/5 shadow-sm dark:border-white/5 flex items-center justify-center text-white ${tool.color || 'bg-gradient-to-b from-slate-400 to-slate-600'}`}>
            <Icon size={30} strokeWidth={1.5} />
          </div>
          <div className="flex-1 min-w-0 flex flex-col gap-1.5 py-1">
            <div className="flex items-center gap-2 flex-wrap">
              <h2 className="text-[17px] font-semibold text-slate-900 dark:text-white truncate tracking-tight">{tool.title}</h2>
              <span className="px-3 py-1 text-[12px] rounded-full font-bold bg-emerald-50 dark:bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 whitespace-nowrap">{MODE_CONTROLLED_BUILTIN_SKILL_IDS.includes(tool.backendId) ? C.readonlyModeBadge : C.readonlyBadge}</span>
            </div>
            {tool.subtitle && <p className="text-[13px] text-slate-500 dark:text-slate-400 font-medium">{tool.subtitle}</p>}
            {tool.desc && <p className="text-[12px] text-slate-500 dark:text-slate-400 leading-relaxed">{tool.desc}</p>}
            {tool.kindLabel && (
              <div className={rowCls}>
                <span className={labelCls}>{C.kindLabel}</span>
                <span className="leading-5 text-slate-600 dark:text-slate-300">{tool.kindLabel}</span>
              </div>
            )}
            {mcpTools.length > 0 && (
              <div className={rowCls}>
                <span className={labelCls}>{C.toolsLabel}</span>
                <div className="flex flex-wrap gap-1.5">
                  {mcpTools.map(name => (
                    <span key={name} title={name} className="text-[11px] font-mono px-1.5 py-0.5 rounded bg-slate-100 dark:bg-slate-800 text-slate-600 dark:text-slate-300">{builtinToolShortName(name, tool.backendId)}</span>
                  ))}
                </div>
              </div>
            )}
            {tool.securityLevel && (
              <div className={rowCls}>
                <span className={labelCls}>{C.securityLabel}</span>
                <span className="inline-flex items-center gap-1.5 leading-5">
                  <span className="text-[10px] font-bold px-1.5 py-0.5 rounded uppercase tracking-wide bg-blue-50 dark:bg-blue-500/10 text-blue-600 dark:text-blue-400" title={levelDesc || undefined}>{tool.securityLevel}</span>
                  {levelDesc && <span className="text-slate-500 dark:text-slate-400">{levelDesc}</span>}
                </span>
              </div>
            )}
            {(tool.bundleVersion || tool.versionText) && (
              <div className={rowCls}>
                <span className={labelCls}>{C.versionLabel}</span>
                <span className="leading-5 text-slate-600 dark:text-slate-300">{tool.bundleVersion ? `v${tool.bundleVersion}` : tool.versionText}<span className="ml-1.5 text-slate-400 dark:text-slate-500">{C.versionNote}</span></span>
              </div>
            )}
            {dataAccess.length > 0 && (
              <div className={rowCls}>
                <span className={labelCls}>{C.dataAccessLabel}</span>
                <div className="flex flex-wrap gap-1.5">
                  {dataAccess.map(scope => (
                    <span key={scope} title={scope} className="text-[11px] px-1.5 py-0.5 rounded bg-slate-100 dark:bg-slate-800 text-slate-600 dark:text-slate-300">{(C.dataAccess && C.dataAccess[scope]) || scope}</span>
                  ))}
                </div>
              </div>
            )}
          </div>
        </div>
      );
    };


export { tsToolsData, tsToolWelcomeData, localizeTool, mergeConfigFields, tsSkillsData, tsSkillIconByName, tsCategories, TOOL_TYPE_GROUPS, getToolTypeGroup, TOOL_BUSINESS_GROUPS, getToolBusinessGroup, TsActionBtn, BuiltinPluginCard };
