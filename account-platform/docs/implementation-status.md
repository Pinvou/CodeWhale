# 账号平台实现状态与差距

## 2026-09-14 治理与安全需求变更

需求/设计已由身份B最终审核PASS并以`e15fe698`提交。当前目标总数在此前437项基础上增至440项；GSA-02复核又把未完整落实15分钟审批近期认证的ADM-64从已实现收紧为部分实现，当前汇总为`已实现403 / 部分实现32 / 未实现3 / 移除1 / 非当前范围1`。APT-141～143分别唯一拥有领域、界面、发布目标；[GSA-01～40独立子任务](admin-governance-security-tasks.md)按A修改/B审核逐项推进，任务拆分已获B复审PASS并以`90b2ea08`提交。GSA-01已获B整批代码审核APPROVED，以`88d37a53`提交；GSA-02[逐命令事实与兼容基线](admin-governance-security-command-baseline.md)及必要文档门禁修复已获B整批审核APPROVED并以`b8216c97`提交。GSA-03已获B整批代码审核APPROVED并以`0dd3d481`提交；GSA-04为CODE_COMPLETE/B_CODE_REVIEW_PASS/ENV_PENDING并以`0c9db1e2`提交，非最终APPROVED；依2026-09-15用户例外可沿依赖链继续后续设计/实现与逐项A/B独立提交，但GSA-04及受其Linux证据影响的下游仍保留ENV_PENDING/DEPENDENCY_ENV_PENDING且不得冒充最终DONE/APPROVED，生产部署、activation与cutover仍禁止。GSA-05只依赖已DONE的GSA-02，协议文档以`16895b97`提交、Provider p2 32位MD5兼容事实修订以`4e06e225`提交；typed encoder/closed adapter/legacy与新授权wire、共享golden及性能基线已获B最终代码APPROVED并以`cad7f48f`提交，未接任何业务写入/消息/consumer。GSA-06的Outbox epoch/sidecar、LEGACY writer/backfill及权限迁移设计已通过，代码、迁移与测试已获B代码审核PASS；本机状态为CODE_COMPLETE/B_CODE_REVIEW_PASS，但继续受DEPENDENCY_ENV_PENDING约束，本次独立Signed-off提交随本变更且具体hash见git历史。ADM-64/ADM-67仍仅部分实现，APT-138-R1 PENDING、APT-141继续IN_PROGRESS，其余子任务PENDING，所有新增目标均未部署。

| 目标编号 | 实现状态 | 任务与审核 | 证据 | 部署状态 | 已知差距 |
|---|---|---|---|---|---|
| ADM-67 | 部分实现 | APT-141-IN_PROGRESS; GSA-01-DONE/B_APPROVED; GSA-02-DESIGN_DONE/B_APPROVED; GSA-03-DONE/B_APPROVED; GSA-04-IN_PROGRESS/CODE_COMPLETE/B_CODE_REVIEW_PASS/ENV_PENDING; GSA-05-DONE/B_APPROVED; GSA-06-IN_PROGRESS/CODE_COMPLETE/B_CODE_REVIEW_PASS/DEPENDENCY_ENV_PENDING | GSA-01提交`88d37a53`；GSA-02提交`b8216c97`且整批B审核/文档门禁PASS；GSA-03提交`0dd3d481`且整批B审核APPROVED；GSA-04代码审核与本机完整验证通过并以`0c9db1e2`提交，三项Linux验收待补，非最终APPROVED；GSA-05协议与兼容修订分别提交`16895b97`/`4e06e225`，代码经Go全包、Go/JS golden、完整verify及五轮benchmark验证并获B最终APPROVED，以`cad7f48f`提交；GSA-06最终完整verify exit 0（562 PASS / 2既有SKIP，Go全包test/vet及其余门禁PASS），真实PG整组exit 0（193.543s，含31项角色漂移、writer 18项/backfill 14项权限矩阵、六命令proof、真实COMMIT重试/unknown查证、高水位/DLQ/并发/down），最终Account vet亦通过；代码、迁移与测试获B代码审核PASS，本次独立Signed-off提交随本变更且具体hash见git历史 | NOT_DEPLOYED | 已实现静态推进门禁、Token动态动作裁剪、安全收口，以及Billing三ACTIVE/八INACTIVE权威current/revision投影与Account精确强读；GSA-04已实现但尚未完成环境验收、尚未接线的Account control持久模型/窄锁接口。GSA-05只交付无业务接线的typed encoder/decoder与legacy/new授权wire adapter；GSA-06已实现Outbox epoch/sidecar、LEGACY writer/backfill、未接线proof-era writer seam与专用权限边界，并通过B代码审核，但依赖环境欠账未清。Relay/consumer/proof endpoint仍属后续任务。APT-141仍有proof、安全presenter、历史收敛、失败恢复和八财务家族增量未交付，见GSA-07～16及21～36。 |
| UI-60 | 未实现 | APT-142-PENDING; TASK_REVIEW_PASS | e15fe698需求审核PASS | NOT_DEPLOYED | v1/v2会话兼容、目录/列表/详情、业务编辑替代与治理分区交互尚未整改，见GSA-17～20及37。 |
| NFR-57 | 未实现 | APT-143-PENDING; TASK_REVIEW_PASS | e15fe698需求审核PASS | NOT_DEPLOYED | 逐项发布工具、生产等价故障/性能/兼容证据及实际授权部署尚未完成，见GSA-38～40；不得以代码测试替代生产证据。 |

本批需求提交前`node scripts/account-platform.mjs verify`已通过：Go test/vet、OpenAPI、550项Node合同通过（另2项SKIP）、三语、observability及architecture。该结果只证明现有代码基线，不证明新增需求已实现；新数据库/消息队列行为验收须按子任务取得真实隔离环境证据。

## 2026-09-07 正式安全基础设施补建

部署负责人已授权补建 KMS 与服务身份签发。SECINF-01 原生 OpenBao/Vault Transit 接入及两入口已通过单元、真实隔离 OpenBao 加解密/轮换/撤销、Go vet 和架构检查；仍 NOT_DEPLOYED。服务身份签发/轮换、正式 OpenBao 初始化/解封托管、部署覆盖与 fresh-install 安全门禁、空库上线尚未完成，详见 [补建设计、串行任务及实测记录](production-security-infrastructure.md)。旧测试数据清除已有明确授权，但尚未执行，其他系统不在清理范围。

> 状态：当前实现快照，不能替代目标需求
>
> 校准日期：2026-09-12
>
> 状态定义：`已实现`、`部分实现`、`降级实现`、`未实现`、`移除`、`非当前范围`。本轮 437 个目标编号均已有明确实现结论，不再保留无结论占位。

> 对应目标需求：`requirements.md`

## 0. 状态证据规则

2026-09-12 APT-140 当前增量：APT-139/140 任务拆分已通过身份 B 审核并以 `9f629d25c4e3502353c754f010f58cb3ee812276` 独立提交；APT-139 已获身份 B 最终独立审核 `PASS` 并以 `4f9adf14fa83b47c98e209e462a7809caeeaf5e7` 独立提交。APT-140 Portal 三页面概览、同 section 下钻、权限/授权版本失效、三语、离线 gate 与生产制品浏览器回归在闭合身份 B 首轮提出的运行页 RELATED 标记/Tab 一致性、760px 特异性及 2.4.1 全矩阵覆盖三项阻断后，已获身份 B 第二轮独立代码审核 `PASS`；APT-139/140 状态均为 `DONE / REVIEW_PASS / NOT_DEPLOYED`。定向 Node、生产 Chromium、Portal Go、Portal build、targeted lint、architecture、i18n 均已通过，整改后完整 `node scripts/account-platform.mjs verify` 退出码为 0（合同测试 552 项：550 通过、2 项因缺少真实 Docker Compose/Helm 环境按合同跳过）。全仓 `check:types` 仍被既有 JavaScript 类型债阻断，真实发布门禁因未提供 `PINVOU_RELEASE_ARTIFACT` 等环境证据未执行，均不冒称通过。UI-59 已实现但未推送、启用或部署；真实 N/N-1、灰度与回滚环境证据继续由 APT-137 收口。本段覆盖后文保留的早先“尚未开工”阶段记录。

本文保留目标重构前的实现判断和差距，供迁移规划使用。本轮已逐项核验 437 个目标编号；代码存在性、静态配置和现有自动化测试只能证明仓库实现状态，不能替代环境部署与生产验收：

| 字段 | 要求 |
|---|---|
| `source_commit` | 本轮连续实施的代码起点为 `e23005aaf6745d4b8c1ffa5ac73943e0f8f98ca5`；APT-001 与 APT-002 结论还包含各自状态文件所在的同一提交。它证明结论对应的代码快照，不表示所有实现均由该提交引入 |
| `tests` | 覆盖正常、权限拒绝、幂等、异常与恢复路径的测试名称或报告 |
| `deployment_version` | 已验证的部署版本、环境和验证时间；未部署写 `NOT_DEPLOYED` |
| `known_gaps` | 与目标需求的剩余差异、Provider 降级和未验证风险 |

当前各矩阵中的“当前实现/边界”是 `known_gaps` 的展开说明。本轮已对 437 个目标编号给出实现结论并使用下列共享证据编号；只有取得明确版本、环境和验证结果的项目才填写部署版本，其余项目继续保持 `NOT_DEPLOYED`。

全量核验汇总：`已实现 380`、`部分实现 29`、`未实现 1`、`降级实现 0`、`移除 1`、`非当前范围 1`，合计 `412` 项。新增 ACC-65～ACC-68、UI-33～UI-36 已获产品批准并拆分为 APT-099～APT-104；APT-099～APT-104 已完成。财务支付重构新增 UI-37～UI-46、ADM-54、BILL-47、NFR-55，已拆分为 APT-105～APT-117；独立文档复审已通过，APT-105～APT-117 已全部完成并逐项通过独立代码复审。超级管理员完整平台权限新增 ADM-55 并由 APT-118 实现。其余项目继续按真实差距保持未实现或部分实现，不能沿用 UI-29 的已完成结论。PAY-31、UI-12、ADM-20 已由独立于 Provider capability、默认关闭的产品门禁闭环。PAY-10、PAY-41–PAY-43 已按当前商城合同完成代码适配，真实支付仍需环境联合验收；具体商品配置不再由本文跟踪。PAY-48–PAY-54、ADM-50–ADM-52、UI-23、NFR-53 已由两层目录、可信准入、动态读取、原子权限、幂等审计 API、三语运营交互、精确代理和文档防回退门禁闭环。PAY-55～PAY-58、ADM-53、NFR-54 已由 APT-091～APT-093 收敛。UI-24～UI-32 已由 APT-087～APT-098 在代码基线全部闭环。版本 `0.8.0@79828dd1` 已于 2026-08-26 部署至 `CLOUD_UAT`：三套迁移任务退出码均为 0，Account/Billing/Identity/Token/Portal 与三套 Relay 均通过健康检查，公网 Portal readiness 返回 ready。管理后台原型一致性纠偏版本 `0.8.0@474a522e` 已于 2026-08-28 单独替换 Portal：容器健康，公网 `admin.html`/`admin.js` 返回 200，CSS/JS 哈希与仓库制品一致。超级管理员权限与价格空间修复版本 `0.8.0@4ccf7404` 已于 2026-09-03 部署至 `CLOUD_UAT`：Account 000042、Billing 000051 迁移通过，Account/Billing/Portal 与公网 readiness 均证明目标 Commit，超级管理员常驻平台权限为 46/46、未脱敏 PII 导出授权为 0，价格空间缺口为 0；真实 Provider 商品准入和支付履约仍由 APT-048 联合验收。

模型与 Token 需求在此前23个正式目标基础上新增`UI-59`、`ADM-66`后，当时阶段汇总为`已实现404`、`部分实现30`、`未实现1`、`降级实现0`、`移除1`、`非当前范围1`，合计`437`项；该历史快照已被本页顶部当前440项目标及ADM-64状态复核覆盖；上段的 412 项汇总是首次模型与 Token 变更前已完成基线。既有 `UI-47`～`UI-58`、`ADM-56`～`ADM-65`、`NFR-56` 的需求、设计和 APT-119～APT-138 任务拆分已通过独立文档审核；本次 UI-59/ADM-66 需求与设计已获身份 B 最终 PASS，并以 `599e6e48c4b9c5684c78e697339a922c6e9c419c` 独立提交；APT-139/140 任务文档已通过身份 B 审核并独立提交；APT-139/140 均已完成且获身份 B 最终代码审核 PASS，UI-59/ADM-66 均已实现但 `NOT_DEPLOYED`。APT-119～APT-136 的原交付均已逐项完成独立代码审核且尚未推送、未部署。APT-124-R1、APT-130-R1、APT-131-R1 已分别按跨域快照、运行治理和 Portal/UI 所有权完成整改；APT-122-R2 已通过身份 B 第五轮审核并闭合协议/公式四命令的通用领域 adapter、跨 Billing T1 仅 GET 恢复、同事务结果/审计/Outbox 和旧写入口旁路。APT-127～APT-133 已交付三段式 IA、协议公式、Token 资源、运行核对、结构化高风险操作及模型/Token Account 详情。APT-134 已通过身份 B 第五轮审核：发布计划使用 `TOKEN_MODEL_PUBLISH / AUDIT_ONLY` 的真实草稿与 15 字段冻结证据，弃用/下架使用 13+6 Account 审批快照；替代服务覆盖、服务端候选分页、精确截止、不可变历史、取消权限与存在性保护、UTC 整秒跨语言指纹、幂等/冲突和确定性 worker 失效均已闭合。APT-135 已通过身份 B 第二轮审核：模型详情与财务入口复用 Billing 唯一价格生命周期组件，当前/未来计量投影、CAS/幂等、UNKNOWN 恢复、父 IA 可见性和撤权中止均已闭合。APT-136 已通过身份 B 第三轮审核：目录差异、数据库权威运行趋势、具名索引与生产规模冷一暖三性能门禁均已闭合；同步命令以唯一重观测、publication lock 内权威重算和事务 activation state/epoch guard 保证漂移零事实写。激活前审计新增 `APT-122-R2 → APT-123-R2 → APT-125-R1` 三个 `REOPENED_REMEDIATION` 批次；首批现已 `DONE/PASS`，UI-49 与 ADM-60 恢复已实现；APT-123-R2 已取得身份 B 最终 PASS，状态为 DONE / REMEDIATION_REVIEW_PASS，ADM-61 恢复已实现；即时模型、账户和回滚五命令及创建 Saga 已闭合；APT-125-R1 已获身份 B 最终 PASS，路由两命令通用事务及 Portal 零 v2 直写闭合，UI-50 与 ADM-62 恢复已实现。相关子 gate 保持安全默认。APT-138 已取得身份 B 整批代码/本批验收最终 PASS，状态 `DONE / NOT_DEPLOYED`：Token 000031、持久 activation/epoch reader/事务 guard、正式凭据 Account 权威重验、Account activation 校验、三项审批 consumer/NATS 及 lifecycle/account provisioning 两 worker 的生产注册和 PRE/ACTIVE/SAFE 总装均已实现。应用默认 PRE，未执行生产激活；create 只在 ACTIVE 且 account worker ready 后开放，SAFE 先关 create 再停止新 lease，旧 epoch 效果失败关闭。APT-137 已按 G0～G5 独立交付发布工具与验证链，G6 持续门禁代码与隔离验证已通过，最后证据文档复核；UI-58 从未实现改为部分实现，真实灰度/N/N-1/激活与部署仍 ENV_PENDING/NOT_DEPLOYED。早期任务在无隔离库时的 SKIP 历史不改写；APT-123-R2 本轮已在本机隔离 PostgreSQL 执行具名三服务、非 superuser 迁移/配额、回滚、Account 审批及生命周期 worker 整链并全部 PASS/no-SKIP，详见下述实测记录。尚未推送/部署，候选提交仍需取得实际 CI 证据。

APT-123-R2 最终状态（2026-09-06）：`DONE / REMEDIATION_REVIEW_PASS`，身份 B 已确认整批最终 PASS。已交付五命令 adapter/旧入口选择、真实 Account/Billing/Token 创建 Saga、四 mode 权威证据、跨操作者幂等恢复及独立分页人工队列、双检查点与理由审计、组织节点归属/父组织状态校验、共享 owner cap、NOLOGIN schema owner 隔离、bootstrap/capability/quiescence/drain 与部署产物。Token 000029、Billing 000053 及 Account 000047 为本批新增迁移；000047 仅允许三类 Token 审批的权威漂移 DRAFT→INVALIDATED，并保留原命令、快照、状态矩阵和 down 历史。真实审批链还修复纯观测时间导致错误 409、含 NUL 锁参数、UUID/text 参数类型歧义；冻结审批快照和历史幂等语义未改。仅交付 worker seam/测试，不注册生产 worker/consumer，不创建 activation record；APT-125-R1、APT-138、APT-137 仍按既定顺序完成。当前未推送、未部署。

| 证据编号 | 审计内容 | 结果 |
|---|---|---|
| `AUDIT_HEAD` | 当前分支代码、数据库迁移、OpenAPI、Compose/Helm 和客户端实现；连续实施起点 `e23005aaf6745d4b8c1ffa5ac73943e0f8f98ca5`，并包含本文件所在提交 | 通过代码审阅；不等同生产部署 |
| `CODE-AUDIT` | 逐项检查路由、领域实现、迁移、配置、客户端调用和测试覆盖；确认目标能力存在、部分存在或缺失 | 437/437 个目标编号均有结论；APT-139/140 均获身份 B 最终 PASS，ADM-66/UI-59 均已实现但未部署 |
| `DOC-COVERAGE` | 对比 `requirements.md` 全部稳定编号与本节规范证据行 | 437 个目标编号一一覆盖；UI-59/ADM-66 需求与设计及 APT-139/140 任务拆分均获身份 B 最终 PASS |
| `MODEL-TOKEN-PAGE-OVERVIEWS` | APT-139 OverviewRead、三个页面级只读投影、精确权限/oneOf、持久 60/120 限流、Portal BFF、000033/OpenAPI/性能与兼容门禁 | `DONE / REVIEW_PASS / NOT_DEPLOYED`；身份 A 已按 B 前两轮意见补齐 SQLSTATE `57014` typed timeout、三路原始路径别名拒绝和正授权版本失败关闭，身份 B 最终独立审核 `PASS`。定向 Go、OpenAPI/Node、迁移清单与授权覆盖通过；隔离 PostgreSQL 具名集成测试多轮 PASS/no-SKIP，覆盖两个独立连接池并发 60/120 限流、分钟边界和连接池等待期限扣减。本机无生产规模性能库，PostgreSQL 17 限制环境下每 SQL 独立重启冷一暖三仍待远端 CI；不作为部署证据。 |
| `MODEL-TOKEN-RELEASE-TOOLING` | APT-137 分组发布工具与持续门禁 | G0～G5 已独立签署，G6 精确候选代码与隔离门禁已验证；Node/专属 PG/CLI/真实配置/VM/Chrome/代理分层证据见末节与发布手册第9节。UI-58 部分实现，正式环境和远端 CI 尚待证据，NOT_DEPLOYED。 |
| `TOKEN-GOVERNANCE-ACTIVATION` | APT-138 十九命令原子治理总装与正式凭据权威组合 | 身份 B 整批最终 PASS；cb1ab922 全量 Linux short-race/vet exit 0，635aa54 正式凭据 guard 九项真实 PG 9.82 秒/no-SKIP 及 Token vet 通过，其他 PG/HTTP/JWT/NATS/Chromium 分层证据见末节及设计 7.4.1。不同候选不冒称同一次执行；生产代码一致，NOT_DEPLOYED，APT-137 未完成。 |
| `ADMIN-FINANCE-PAYMENT-REDESIGN` | 财务支付三分区、现有能力闭环、定价生命周期、Billing 分页和分区运营概览设计 | 产品已批准 `UI-37`～`UI-46`、`ADM-54`、`BILL-47`、`NFR-55`；第三轮独立文档复审已通过，代码任务 APT-105～APT-117 已完成并逐项通过独立代码复审 |
| `ROLE-PERMISSION-REDESIGN` | 三类权限、角色真值表、对象作用域、生命周期、审批、工作负载身份、Break-glass 与迁移门禁的安全及业务独立审核 | 2026-08-26 两位独立审核者均确认设计通过；代码实现按 APT-069–APT-082 分任务进行 |
| `THREE-SCOPE-AUTHORIZATION` | PERSONAL 作用域、默认个人角色、系统/组织角色精确真值表、规范权限别名、独立授权版本、节点绑定保留及发布前迁移更正 | Account/Token Go 测试、OpenAPI lint、177 个合同测试和迁移摘要门禁通过；数据库运行级迁移待部署验证 |
| `SUPER-ADMIN-PLATFORM-PERMISSIONS` | 超级管理员完整常驻平台权限、Token 管理可见性、授权缓存失效及临时 PII 权限隔离 | Account 000042 前向/回滚迁移、角色真值表合同、Token 隔离合同、Portal 三语制品和迁移摘要门禁通过；数据库运行级迁移待部署验证 |
| `ADMIN-MODEL-TOKEN-REDESIGN` | 三页面原子权限、分页、权威 owner/路由健康、资产详情、凭据隔离、逐动作治理与 Billing 价格边界 | APT-119～APT-136 原交付、APT-122-R2/123-R2/125-R1 及后续独立整改均保留已审历史；APT-138 已获身份 B 整批最终 PASS，Token 000031 持久状态/epoch、十九项真实 adapter 与精确 DB/registry 门禁、Account 强读、三审批 consumer/NATS、两个 worker 及正式凭据 authority/deadline 组合已实现。真实 PG/HTTP/JWT/Chromium 与 NATS transport 分层验证、Linux 全量 short-race/vet 通过，具体候选和边界见本文末 APT-138 记录及设计 7.4.1。生产默认 PRE，NOT_DEPLOYED；APT-137 现场验收仍未完成；两旧 Node 断言已由 G5 闭合，b837 精确候选 429/429 全账户 Node 通过，不宣称已生产激活或远端 CI 通过。 |
| `ADMIN-MODEL-TOKEN-OVERVIEW-REALIGNMENT` | UI-59/ADM-66 三个分区顶部概览、同 section 下穿、权限裁剪和页面级只读投影 | 需求/设计与 APT-139/140 任务拆分均获身份 B 最终 PASS 并独立提交。APT-139 已获身份 B 最终代码审核 PASS；APT-140 经首轮整改后获身份 B 第二轮代码审核 PASS，UI-59/ADM-66 均已实现但 NOT_DEPLOYED。 |
| `TOKEN-MODEL-OPERATIONS-PRICE-PROJECTION` | ADM-59 Token 本域模型/运行概览与 Billing 按稳定模型的跨域只读价格投影 | APT-126 已通过身份 B 首轮独立代码审核；Token 使用数据库权威时间的单语句原子聚合，Billing 使用 repeatable-read 解析当前与下一实际价格变化，Portal 仅分别透传。两域不互相读取或代写、不扫描分页列表算指标、不宣称跨域原子；两个默认关闭 gate 可独立回滚。定向 Go/Node、OpenAPI、授权覆盖、架构、迁移清单、gofmt 与 diff 门禁通过；本地两个 PostgreSQL URL 未配置，命名用例明确 SKIP，CI 隔离库强制 PASS 且拒绝 SKIP；当前未推送、未部署 |
| `TOKEN-STABLE-ROUTES-HEALTH` | ADM-62 稳定模型/协议路由、服务端健康探测、Provisioning 精确版本、影响证据、迁移兼容与激活事务边界 | APT-125 已通过身份 B 第四轮独立代码审核；Go、Node 合同、OpenAPI、迁移清单、gofmt 与 diff 门禁通过。本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`，真实 PostgreSQL 用例显式 SKIP；CI 隔离库强制 `TestRouteHealthPostgresIntegration`、`TestStableRoutesPostgresIntegration` PASS 且拒绝 SKIP。APT-138 尚未创建或激活持久 activation record，生产保持 `PRE_ACTIVATION` v1 direct，v2 仅在未来 `ACTIVE` 状态可达；当前未推送、未部署 |
| `TOKEN-IMPACT-PRECHECKS` | 五类只读预检、Token 固定全序流式关键集合 SHA/组合 ETag、规范化双域预检指纹、逐 tuple READY 与替代资源事实、Billing 单事务批量价格覆盖，以及 5 秒单一绝对 deadline 与脱敏失败关闭 | APT-124 已通过身份 B 第五轮独立代码审核；Go、vet、Node 合同、OpenAPI、授权覆盖、迁移清单、架构和 diff 门禁通过。本地 Token/Billing PostgreSQL 集成未执行，CI 命名用例强制 PASS 且拒绝 SKIP；随本任务创建独立提交，尚未推送、未部署 |
| `TOKEN-OWNER-CANDIDATES` | ADM-61 专用权限、Account 最小权威投影、service-auth、签名扫描 cursor、授权版本失效、限流与公网隔离 | 身份 B 第五轮独立代码审核通过；Account/Portal Go 范围测试、20 项定向合同、OpenAPI、迁移清单、架构及 diff 门禁通过。真实 PostgreSQL 集成用例本地未执行，发布前必须在隔离库运行 |
| `TOKEN-ADMIN-PAGINATION-BASE` | NFR-56 Token 快照 schema、签名 cursor、容量与清理、稳定错误、绝对 deadline 链及专用入口 | 身份 B 第三轮独立代码审核通过；Go 全量短测试、Go vet、10 项 Token/Helm 合同、CI 策略、OpenAPI/Helm/workflow 解析、迁移清单、架构及 diff 门禁通过。三个 PostgreSQL 重型用例本地未执行；PR 门禁使用独立数据库逐项要求 PASS 并拒绝 SKIP；当前未部署。 |
| `TOKEN-ADMIN-QUERY-ADAPTERS` | ADM-56、ADM-63 与 NFR-56 九类逐资源查询、owner 权威投影、资源审计权限交集及用量 evidence 三元 cursor | 身份 B 第四轮独立代码审核通过；相关 Go 测试/vet、17 项合同、OpenAPI、授权覆盖、架构、gofmt 及 diff 门禁通过。本地未配置 PostgreSQL URL，真实迁移/高量集成未执行；PR 隔离库门禁强制 PASS、拒绝 SKIP；当前未部署。 |
| `TOKEN-PROTOCOL-FORMULA-GOVERNANCE` | ADM-60 四个稳定协议/公式命令的精确 AUDIT_ONLY 策略、CAS/ETag、审计 Outbox、Billing 同步、近期认证与预激活失败关闭 | APT-122 原交付通过身份 B 第三轮审核；APT-122-R2 通过身份 B 第五轮审核。四命令现已用唯一严格 parser/canonical envelope 接入通用 `/commands`，领域事实、通用结果、脱敏审计和 Outbox 同一 Token 事务提交；公式发布保留 Billing existing-first T1 和仅 GET 恢复。Portal/OpenAPI 严格验证结果状态、RFC 3339 时间与非敏感稳定码。Go 全量、Node/Ajv、OpenAPI、Portal build/lint、Chromium、i18n、observability、迁移清单、任务/架构和 diff 门禁通过；本地 PostgreSQL 无隔离 URL 时具名测试 SKIP，CI 强制 no-SKIP；当前未部署。 |
| `TOKEN-COMMAND-GOVERNANCE-BASE` | ADM-65 十九项策略、通用持久结果/幂等恢复、三项审批精确权限与策略、稳定 actor、最小 executor 路由及 PRE_ACTIVATION 失败关闭 | APT-123 已通过身份 B 第五轮独立代码审核；Go 定向测试、Node 合同、OpenAPI、授权覆盖、迁移清单、gofmt 与 diff 门禁通过。本地未配置 Account/Token PostgreSQL URL，真实迁移链命名用例未执行；PR 隔离库门禁强制逐项 PASS 且拒绝 SKIP。十九项领域 adapter、activation record 与审批事件 consumer 仍属后续边界；当前未推送、未部署。 |
| `ORGANIZATION-OBJECT-AUTHORIZATION` | 显式目标组织、节点子树列表、源/目标节点双向移动门禁及成员全部实际节点归属校验 | Account Go 测试、OpenAPI lint、对象作用域合同和全量静态验证通过；数据库多节点绑定与并发移动待部署验证 |
| `LOGIN-PERMISSION-GATE` | Keycloak 浏览器密码/短信/注册完成路径与 Direct Grant 密码/短信路径在成功前强一致检查 `personal.session.login`，并校验精确 issuer 与最小工作负载路由 | Identity 静态合同、Compose/Helm 配置、Account 生命周期授权测试及全量合同通过；自定义 Keycloak 镜像构建和真实登录待部署验证 |
| `PASSWORD-SECURITY-AUTHORIZATION` | 已登录改密的近期认证与个人权限门禁、匿名恢复的防枚举权限/生命周期校验、密码隔离和会话撤销 | Account Go 单元测试、OpenAPI lint、密码安全合同及全量静态验证通过；真实短信、Keycloak 改密和数据库串行化待部署验证 |
| `PERSONAL-ACCOUNT-AUTHORIZATION` | 本人资料、授权上下文、组织列表、资料变更、上下文切换、注销和邀请 Account 路由逐项绑定个人原子权限，并保持 OIDC 主体唯一目标 | Account Go 测试、OpenAPI 403 契约、路由权限矩阵和生命周期失败关闭合同通过；数据库撤权运行级验证待部署 |
| `BILLING-SCOPE-AUTHORIZATION` | 个人余额、权益包、订单与充值管理按 `personal.*` 权限和个人上下文失败关闭；组织购买、余额、权益及额度授予按显式组织目标与 `org.*` 权限实时决定 | Token/Portal Go 测试、显式组织目标和个人/组织不可互借权限用例、OpenAPI 及全量合同通过；数据库撤权和真实组织钱包联调待部署 |
| `PERMISSION-DRIVEN-UI` | 桌面账号弹层、Web 个人中心和平台后台直接消费 Account 权威权限集合及个人/平台/组织独立授权版本；入口、只读态和操作按钮按精确权限决定，版本变化触发失效刷新，退出及注销进度保持可达 | 权限 UI 合同、Portal/桌面版本头传播、三语文案、Portal Go 测试、前端 lint/构建及架构守卫通过；真实撤权推送时延和浏览器/桌面交互待部署验证 |
| `ADMIN-PORTAL-UI-REVIEW` | 管理后台改版只调整展示、交互与验收，现有功能需求、API、权限、治理和状态机保持不变；UI-28～UI-32 拥有独立交付边界 | 角色 A 与角色 B 交替完成三轮独立复审，最终无 P0/P1；正式实现与运行验证分别由 APT-094～APT-098 收敛 |
| `ADMIN-PORTAL-SHELL` | 管理后台严格采用 HTML 原型的五组导航、独立 URL 状态、共享基础组件、三语、响应式和无障碍壳层 | 工作台、身份与组织、财务与支付、模型与 Token、治理与安全五组及角色/支付/商品/审批/偏好独立 section 已通过源码、构建制品、URL、权限矩阵和三语合同；平台总览已改为三张领域摘要卡、独立系统健康条和权限内可跳转待办，四类数据源独立失败，无权/失败不再伪装为 0；`CLOUD_UAT 0.8.0@bb9e8f3b` 已完成四服务滚动发布与公网制品校验 |
| `ADMIN-PLATFORM-OVERVIEW-UAT-20260901` | 平台总览领域摘要、权限隔离、独立失败、指标口径、待办下钻和公网制品部署验证 | 2026-09-01 从不可变目录 `/opt/pinvou-account-platform-bb9e8f3b-20260901T043224Z` 滚动替换 Account、Billing、Token Gateway、Portal；四个 Readiness 均返回 `0.8.0@bb9e8f3b` 且容器健康，近 10 分钟错误行计数为 0；公网 `admin.html` 返回 200，`admin.js` SHA-256 为 `f0acfd9f…`、`admin.css` 为 `3204be56…`，与目标提交归档一致 |
| `ROLE-AUTHORIZATION-UX-UAT-20260901` | 角色目录唯一入口、权限清单、引导式授权、授权记录与独立审计深链的 Portal 部署验证 | 2026-09-01 从不可变目录 `/opt/pinvou-account-platform-13e22858-20260901T122502Z` 单服务滚动替换 Portal，迁移差异为空；Portal Readiness 返回 `0.8.0@13e22858`，公网 `admin.html` 返回 200，`admin.js` SHA-256 为 `2c94f783…`、`admin.css` 为 `f2686d03…`，与 Git 归档和容器制品一致；角色目录操作列保持单行，目录按个人/平台/组织三类归属展示且节点管理员仍以真实 `UNIT` 作用域授权；部署后 Portal panic/fatal 与 Nginx 5xx 计数均为 0，Account、Billing、Token、Identity 及全部基础容器保持健康，回滚目标为 `ea76c318` |
| `ADMIN-PORTAL-PROTOTYPE-CONFORMANCE` | 以 `admin-portal-ui-prototype.html` 为第一视觉和信息架构基准，核验五组导航及精确页面命名、浅色蓝白壳层、页头、指标卡、业务卡片、工具栏、数据表、右侧详情抽屉和旧扩展页融合；禁止旧六组分类及旧深色/青绿色设计回退 | 2026-08-28 源码/制品五组合同、前端 lint、Portal 构建、215 项合同、Go 单测、任务覆盖和架构守卫通过；`CLOUD_UAT 0.8.0@e8c3c929` Portal healthy，公网 `admin.html`/`admin.js` 返回 200，CSS `d43a713e…`、JS `21bc2d85…` 与服务器发布目录一致 |
| `ADMIN-IDENTITY-UX` | 用户、组织、角色绑定、注册修复与注销修复在统一身份页面内按精确权限展示 | 身份扩展挂载测试、37 个权限/角色/基础合同测试、前端 lint 和 Portal 构建通过；未改变现有 Account API 或管理动作 |
| `AUTHORIZATION-COVERAGE-GATE` | 自动发现生产 Go 源码中的 HTTP 路由、后台任务、事件 Consumer/Relay 及全部 CLI，并要求稳定个人/平台/组织权限、精确 `internal.*` 权限或专用公开协议策略逐项分类 | 当前 285 个 HTTP 路由、25 个任务、3 个事件入口和 13 个 CLI 全部登记；APT-122 Billing 单位验证入口固定为 `internal.billing.pricing.read` 且 Token 工作负载已有最小授权。删除登记、未知权限、作用域错配、陈旧项和新增未分类入口的负向合同通过，统一 verify 与正式客户端发布门禁均执行该检查 |
| `WEB-IDENTITY-LOGIN` | Keycloak 浏览器 Authenticator、Pinvou 三语主题、密码边界、短信滑块登录、手机验证码注册、Portal 未登录直达及 Compose/Helm 运行配置合同 | 仓库代码和静态合同通过；自定义 Keycloak 镜像构建及真实短信/注册/PKCE 联调待部署 |
| `ACCOUNT-DOMAIN-UAT-20260825` | 账号独立域名、Portal 根入口、精确支付回调路由、受信 TLS、HTTP 跳转、服务健康和回调客户端授权 | `account.pinvou.com` 已解析到公网入口；Let's Encrypt 证书有效至 2026-11-23；HTTP 以 `308` 跳转 HTTPS，Portal 根入口返回 `200` 且旧 `/account.html` 返回 `404`，两个未授权回调入口返回 `401`，受控 Basic 凭据换取短期 Bearer Token 返回 `200`；Billing 运行 `1efb79d6`、Portal 运行 `bc5683ee`。真实新华三通知报文、重复/乱序和履约结果仍待商城联合验收 |
| `GO-UNIT` | `node scripts/account-platform.mjs test unit` | 所有 Go 包测试通过（项目内置 Go，`nomsgpack`） |
| `GO-POSTGRES` | Billing PostgreSQL 聚合集成验证 | `TestFinanceOverviewsPostgresIntegration` 已在 PostgreSQL 17 的最小隔离镜像表上执行通过，覆盖概览查询所读取的生产列与关键边界；该证据不替代完整迁移链测试 |
| `CONTRACT-36` | 本轮 RBAC 前基线合同集 | OpenAPI lint 与原 36 个合同测试通过 |
| `RBAC-AUTH` | Account RBAC 迁移、默认拒绝、旧 Realm Role 不可绕过、跨服务原子权限和版本响应定向测试 | Go 单元测试与新增合同测试通过；合同总数增至 37 |
| `USER-CODE` | UUIDv7 永久守卫、百万编码生成/校验、永久登记、本人/运营展示、精确查询防枚举与 OpenAPI 合同 | Go 单元测试、38 个合同测试及桌面三语 40 项定向测试通过；数据库运行级迁移验证待部署 |
| `IDENTITY-SMS` | Identity 密码边界、原始字符策略、短期引用与策略版本、两类短信 Provider、多维限流审计和登录反枚举 | Go 全量测试、40 个合同测试、桌面 ESLint/三语检查和 Rust `cargo check` 通过；数据库运行级迁移与真实短信待部署 |
| `REGISTRATION-SAGA` | 唯一值预约、稳定操作/密码指纹、Identity 关联恢复、异步钱包、状态门禁、桌面恢复和证明门禁补偿 | Go 单元测试、41 个合同测试、桌面 ESLint/三语检查和 Rust `cargo check` 通过；数据库运行级迁移、真实 Billing/Identity 故障注入待部署 |
| `COMMERCE-MAPPING` | 供应商中立 Mapping/任务/证据、版本防迟到覆盖、Adapter 合同边界及支付专用准入查询 | Go 单元测试、42 个 OpenAPI/合同测试、迁移与部署模板检查通过；真实 Provider、数据库运行级迁移和故障注入待部署 |
| `ENTITLEMENT-COMPONENTS` | BASE/BONUS 组成级入账、确定消费顺序、人工赠送、到期释放、迟到用量损失复核、守恒投影及三语展示 | Go Billing/Token 单元测试、OpenAPI lint、44 个合同测试、Portal 构建和桌面 ESLint 通过；数据库运行级迁移、并发和故障注入待部署 |
| `ROLE-BINDING-LIFECYCLE` | 本人/组织/平台绑定视图、起止时间、幂等授权、单条幂等撤销、自动到期、授权版本刷新、审计/Outbox 和三语 Portal | Account/Portal Go 单元测试、OpenAPI lint、角色生命周期合同和静态资源检查通过；数据库运行级迁移、真实并发授权及 worker 故障恢复待部署 |
| `SECURITY-RECOVERY` | `BOOTSTRAP_PENDING` 首管安全激活、256 bit 一次性凭据、5 次锁定、代次失效、最后管理员门禁、ACC-48 撤权原语和 Ed25519 受信运维身份离线 Break-glass | Go 单元测试、签名篡改/过期测试、CLI/迁移合同与全量静态验证通过；生产等价 Keycloak/数据库并发、故障补偿、告警投递和事故演练待执行 |
| `APPROVAL-COMMANDS` | 版本化命令分类、不可逆审批状态机、不可变快照、独立审批、强一致授权复核、Outbox 授权和 Billing 幂等领域执行 | Account/Billing/Portal Go 单元测试、OpenAPI lint、47 个合同测试与全量静态验证通过；正式服务调用已接入 APT-047 短期工作负载身份边界 |
| `WORKLOAD-IDENTITY` | 独立 ServiceAccount、按次轮换 OIDC 令牌、Subject 到精确 `internal.*` 权限默认拒绝映射、逐内部路由权限声明、Relay 本域 Subject 与独立 NATS 凭据、服务任务审计身份 | Go 单元测试、Helm Schema、逐路由精确权限合同及全量静态验证通过；真实 issuer/JWKS、NATS operator 权限和撤销/轮换演练待部署 |
| `AUDIT-IDEMPOTENCY-EVENTS` | 当前跨域 Saga、管理/资金/授权/路由写审计、稳定请求指纹、事务 Outbox/Inbox、摘要绑定且服务身份化的幂等死信重放 | Go 单元测试、三域迁移、CLI/合同与全量静态验证通过；真实数据库迁移、积压和 Broker 故障恢复演练待部署 |
| `ORGANIZATION-LIFECYCLE` | 顶层组织创建幂等、状态/游标搜索、启停投影、组织钱包冻结以及 Account/Billing/Token 删除阻断事实强校验 | Account/Billing/Token Go 单元测试、OpenAPI lint、组织生命周期合同测试与全量静态验证通过；数据库运行级迁移和生产故障注入待部署 |
| `ORGANIZATION-UNIT-LIFECYCLE` | 节点命令指纹幂等、组织结构版本、闭包树移动以及 Account/Billing/Token 限制态预检和逻辑删除 | Account/Billing/Token Go 单元测试、OpenAPI lint、节点生命周期合同测试与全量静态验证通过；数据库运行级迁移、真实并发和跨服务故障注入待部署 |
| `MEMBERSHIP-GOVERNANCE` | 多目标邀请反枚举、成员命令幂等、即时撤权/上下文回退、Billing 限制投影和单 OWNER 数据库约束 | Account/Billing Go 单元测试、OpenAPI lint、成员治理合同测试与全量静态验证通过；数据库运行级迁移、真实并发、通知通道和跨服务故障注入待部署 |
| `GLOBAL-CONTEXT` | 用户级唯一活动上下文、CAS 版本切换、跨端刷新、失效回退及 Account/Token/Portal 版本传播 | Account/Token/Portal Go 单元测试、OpenAPI lint、全局上下文合同、桌面 40 项状态测试和 Portal 构建通过；数据库运行级迁移、真实多端并发和生产缓存传播待部署 |
| `PERSONAL-CENTER-IA` | Web 个人中心“额度与用量 / 订单与发票 / 我的组织 / 账号设置”条件导航、全状态订单历史、旧深链映射，以及个人/组织资源显示边界 | 账号平台状态测试、UI ESLint 和 Portal 构建验证；“我的组织”只在活动组织和适用读取权限同时存在时显示，集中承载购买、额度、成员、节点和模型策略；额度页只保留当前身份只读事实与本人用量，账号设置不再混入组织管理 |
| `ORGANIZATION-MEMBER-QUOTA` | 组织购买、精确 BASE/BONUS 组成授予、逐项批量、成员生命周期、Hold/结算及权限化 UI | Go 单测、OpenAPI、组织额度合同、桌面状态测试及 Portal 构建通过；真实 PostgreSQL 并发竞争和生产迁移仍待部署验证 |
| `METERING-PRICING-LIFECYCLE` | 版本化计量单位/协议公式、确定性估算、动态 Hold 续租与追加预占、逐单位价格匹配及完整生命周期 | Billing/Token/Portal Go 单测、OpenAPI lint、59 个合同测试和全量静态验证通过；数据库运行级迁移、真实并发发布和长流故障注入待部署 |
| `PRICING-LIFECYCLE-UI` | 管理端计量单位、价格草稿、预览、发布、未来生效与转换的 React 生命周期工作区，以及精确价格空间投影和非幂等未知结果恢复 | 定价生命周期 UI 合同、服务端渲染执行、共享同步锁、Billing Go 单测、OpenAPI、三语覆盖、前端 lint 与 Portal 构建通过；真实浏览器交互和部署后并发恢复待统一发布验收 |
| `FINANCE-OVERVIEW-UI` | 三个财务分区的权限化运营概览、精确或说明式下穿、局部失败隔离、请求竞态和响应式交互 | 财务概览纯函数与 UI 合同、三语覆盖、前端 lint、Portal 构建及桌面/移动端真实 Chromium smoke 通过；account-platform 定向选择器和全量前端门禁均已登记该 smoke，构建制品包含四个聚合端点和概览样式；公网制品已在 `0.8.0@b56b71ae CLOUD_UAT` 验证，部署后权限撤销等登录态行为仍待验收 |
| `FINANCE-PAYMENT-UAT-20260902` | 财务支付 APT-105～APT-117 的迁移、受影响服务滚动发布、公网制品与安全边界验证 | `CLOUD_UAT 0.8.0@b56b71ae`：不可变目录 `/opt/pinvou-account-platform-b56b71ae-20260902T141036Z`；Account 000041、Billing 000050 迁移及校验和通过；Account、Billing、Portal readiness 均证明目标 Commit；四个概览代理入口未认证均返回 401；公网 admin.js/admin.css 与发布目录 SHA-256 一致，受影响服务近 10 分钟错误计数为 0；上一回滚目录为 `d90bb308`。Identity 的历史 `version=dev, commit=unknown` 未在本次改动，仍须后续正式发布治理。 |
| `SUPER-ADMIN-PRICING-UAT-20260903` | 超级管理员完整平台权限与价格空间版本修复的迁移、受影响服务滚动发布及安全边界验证 | `CLOUD_UAT 0.8.0@4ccf7404`：不可变目录 `/opt/pinvou-account-platform-4ccf7404-20260902T160335Z`；五类迁移前备份及 SHA-256 校验通过；Account 000042、Billing 000051 迁移后版本与校验和通过；超级管理员常驻平台权限 46/46、Token 权限 3/3、未脱敏 PII 导出 0；价格空间缺口 0；Account、Billing、Portal 与公网 readiness 均证明完整 Commit；公网 admin.js 与发布制品 SHA-256 一致，受影响服务近 20 分钟致命错误计数为 0；上一回滚目录为 `b56b71ae`。 |
| `PAYMENT-PROVIDER-BOUNDARY` | 供应商中立支付端口、版本化能力发现、商品映射与订单快照、动态渠道 UI、能力门禁及生产测试隔离 | Go 单测、OpenAPI lint、合同测试、Portal 构建和部署模板检查通过；具体商品清单只从运行时目录和审计查询，真实 Provider 联调仍由 APT-048 收敛 |
| `PAYMENT-EVIDENCE-LIFECYCLE` | 一单多流水、不可变回调/查询/账单/修复证据、逐流水权益包、付款人与受益方冻结、独立状态机、订单派生摘要及运营明细 | Go 单测、OpenAPI lint、合同测试、桌面 ESLint 和 Portal 构建通过；数据库运行级迁移及真实 Provider 乱序/重复证据故障注入待部署 |
| `PAYMENT-CALLBACK-GATEWAY` | Provider 中立 Basic 客户端授权、Token 绑定 Provider/版本/商户、Profile 注册、严格通知适配、先持久化后 `202`、重复/冲突防重及查询补偿 | H3C Profile 已直接复用 queryOrder 原生订单对象、确定性派生通知 ID、精确校验十进制金额/客户/SKU并修复异步订单投影 SQL；Go 单测、OpenAPI 与回调合同通过。公网旧版本已验证 Token/202，但新版原生报文和最终履约仍待部署联合验收 |
| `PAYMENT-CONVERGENCE` | Provider 账单逐行进入统一不可变证据归并器，关单状态单调推进，支付/退款/拒付事实独立且订单摘要可重建 | Go 单测、OpenAPI lint、关单/账单/摘要合同及全量静态验证通过；真实 Provider 账单、关单竞态和数据库运行级迁移待部署 |
| `RECEIVABLE-LIFECYCLE` | 用量超额双记、独立应收、钱包上下文冻结、现金 BASE 清偿分配、单支付权益包拒付冲销及三语运营查询 | Go 单测、OpenAPI lint、75 个合同测试、桌面 ESLint 和 Portal 构建通过；拒付已接入统一异常案件并由受控结论命令执行定向冲销；数据库运行级迁移、并发结算与真实 Provider 拒付故障注入待部署 |
| `HOLD-OPERATIONS-EVIDENCE` | 钱包 Posting 至权益组成来源、Hold 组成/成员分配、租约历史、异常分类及幂等安全释放证据 | Go 单测、OpenAPI lint、80 个合同测试、桌面 ESLint 和 Portal 构建通过；数据库运行级迁移、真实并发幂等和生产授权隔离待部署 |
| `TARGETED-REFUND` | 支付流水+权益包精确目标、整笔剩余现金退款、BASE/关联 BONUS 冻结撤销、幂等 Outbox Consumer 恢复和渠道强制退款独立证据 | 退款领域实现、迁移和测试保留；新建普通退款由默认关闭的独立产品门禁阻断，既有请求继续按 Provider 能力收敛 |
| `REFUND-PRODUCT-GATE` | 新建普通退款独立产品门禁、稳定 API 拒绝、Provider 能力隔离、管理入口双门禁和部署默认关闭 | APT-056 已实现；Compose、远端 Compose 与 Helm 均显式默认 `false`，合同测试保护历史收敛和强制退款路径 |
| `LATE-PAYMENT-REVIEW` | 72小时边界查询、唯一 P2 案件、每日/每周复核、用户申诉证据、无账单降级、正式收款准入、严格未支付归档与 Provider blind 停单 | Go 单测、OpenAPI lint、90 个合同测试、管理页41项状态测试、桌面 ESLint 和 Portal 构建通过；数据库运行级迁移、真实 Provider 历史查询/终态和告警投递待部署 |
| `PAYMENT-ANOMALY-RECONCILIATION` | 拒付、强制退款、重复/晚到冲突、退款失败与非法迁移统一案件；Provider、订单/流水、退款、账本每日四方对账；不可变证据和幂等处置命令 | Go 单测、OpenAPI lint、92 个合同测试、三语运营页面和 Portal 构建通过；数据库运行级迁移、真实 Provider 账单、跨日时区、并发定时任务及财务故障注入待部署 |
| `GATEWAY-ADAPTER-NEUTRALITY` | 版本化协议能力发现、显式路由协议、Provider/Adapter/上游引用历史证据，以及 New API/通用 OpenAI 兼容 Adapter 隔离 | Go 单测、OpenAPI lint、93 个合同测试、三语运营页面、Portal 构建和 Helm/Compose 配置检查通过；数据库运行级迁移及真实 Adapter 替换/用量证据联调待部署 |
| `MODEL-CATALOG-LIFECYCLE` | Adapter 目录发现、稳定 UUID、不可变能力/目录版本、草稿 CAS、发布/弃用/移除、连续缺失核验、前向回滚、历史墓碑和请求版本快照 | Go 单测、OpenAPI lint、94 个合同测试、三语运营页面和 Helm/Compose 配置检查通过；桌面目录消费、回滚联合依赖校验、数据库运行级迁移和真实 Adapter 同步待后续任务/部署 |
| `ROUTE-PROVISIONING-ELIGIBILITY` | 按作用域/模型/协议/代次唯一的托管路由状态机、租约认领、退避/SLA、组织模型策略、READY 资格门禁、回滚联合依赖校验和运营修复 | Go 单元测试、Rust 桌面代理测试、OpenAPI lint、95 个合同测试和三语 Portal 构建通过；数据库运行级迁移、真实 Adapter/凭据和 Worker 故障注入待部署 |
| `USAGE-EXECUTION-REPLAY` | 稳定用户级键摘要、版本化规范指纹、Hold/派发持久状态、一次性派发租约、加密流事件/结果、Last-Event-ID 重放及24小时/30天/90天分层清理 | Go 全量单元测试、OpenAPI lint、重放结构合同和静态验证通过；数据库运行级崩溃/断线/KMS故障注入待部署，注销调用与备份清除分别由 APT-015/APT-037 接入 |
| `EXTERNAL-CONVERGENCE` | 支付与模型外部事实统一遵循先留不可变证据、再复核/结算/释放且不猜测成功或自动重发高风险请求的边界 | Go 单元测试、支付/模型收敛合同与全量静态验证通过；真实 Provider 乱序结果、断线和数据库运行级迁移待部署 |
| `USAGE-CANCELLATION-CONVERGENCE` | 原稳定用户幂等取消、派发前 Hold 全释放、派发后交付隔离、Adapter 取消能力、可信实际用量、版本化核对期限、损失复核及幂等人工重试 | Go 全量单元测试、OpenAPI lint、取消结构合同、桌面/Portal 构建和静态验证通过；真实 PostgreSQL 竞态、Provider 用量延迟、断线和故障注入待部署 |
| `TOKEN-OPERATIONS-ISOLATION` | 平台 Token Account 创建/启停、PLATFORM 资源过滤、普通管理面移除、存量非平台账号/凭据/路由隔离及用户状态脱敏 | Go 单元测试、OpenAPI lint、普通路由不存在用例、隔离迁移与平台 Token 运营合同通过；数据库运行级迁移及真实 Provider 凭据检查待部署 |
| `TOKEN-MODEL-USAGE-PERMISSIONS` | 个人/组织上下文模型目录、协议、调用及用量读取的原子权限，以及脱离活动组织的稳定用户取消权限 | Token Go 单元测试、OpenAPI lint、原子权限合同及全量静态验证通过；真实 Account 授权故障、组织撤员后取消和并发版本变更待部署验证 |
| `HUMAN-DELEGATION-SIGNING` | Account 强一致复核人类主体原子权限后，以 Ed25519 签发最长 5 分钟且绑定受众、权限、组织/节点、资源、HTTP 方法/路径、请求摘要和授权版本的委托；Billing 额度写命令验签后只从上下文取 actor，并保存工作负载与委托人双主体审计 | 签发/验签、跨服务传播及正文/目标替换 Go 测试、迁移、Compose/Helm Secret 契约与全量静态验证通过；真实密钥轮换和跨服务故障注入待部署验收 |
| `ACCOUNT-CLOSURE-CLEANUP` | 主体生命周期隔离、资料/Commerce/邀请自动收敛、Identity 删除、原标识释放、永久 ID/编码墓碑、三年保留策略，以及组织授予隔离与合规整笔回收 | Account/Billing/Token/Portal Go 单元测试、OpenAPI lint、注销清理合同、桌面 ESLint 和 Portal 构建通过；数据库运行级迁移、真实 Identity/Commerce 故障注入和三年时钟推进待部署 |
| `CLIENT-SWITCH-READINESS` | 服务身份保护的只读 OIDC/Account/目录/READY 路由/价格公式检查、公开兼容范围、制品/环境/版本绑定的 Ed25519 短时证明和不可变签发审计 | Go 单元测试、OpenAPI lint、合同测试、Compose/Helm 静态门禁通过；生产密钥注入、真实环境连续无副作用核对和发布系统联调待外部验收 |
| `DESKTOP-CLIENT-SWITCH` | 动态目录、按会话冻结模型、显式替代分支、受信 Portal、客户端范围守卫及发布/安装/运行三层失败关闭门禁 | Rust check、桌面 lint/build、Bridge 协议、发布门禁签名/篡改测试和全仓 verifier 通过；生产签名身份、正式安装制品原子替换/回滚和真实环境联调待验收 |
| `CONFIGURED-SYSTEM-PROBES` | Portal 通过运行时注册表装配本地、通用 HTTP/JSON 断言与 Relay 指标 Adapter，不在聚合主流程固定组件或厂商 | Portal Go 单测、Compose/Helm 合同和全仓 verifier 通过；真实环境端点、超时与积压告警待部署验收 |
| `I18N-RELEASE-GATE` | 桌面账号弹层、Web 个人中心和管理后台的精确三语词条对称、空值、单语硬编码、原始错误回显及 Portal 构建制品检查 | 缺键/多键/空词条、单语 JSX/属性、后端 message 回退与制品缺词条负向合同通过；发布命令与全量 verify 均串联该门禁 |
| `PRODUCTION-SECURITY-GATE` | 生产 Ingress TLS、ExternalSecret KMS/Vault 来源、远程包络加密、持久 `key_id` 双读、全密文分批重加密、轮换 readiness 与旧 Key 停用验收 | Go 轮换/历史 Key 单测、Helm schema/ExternalSecret 负向合同和 TLS/KMS 环境门禁合同通过；无真实域名证书、SecretStore、KMS Key 轮换或部署版本证据 |
| `OBSERVABILITY-RELEASE-GATE` | 稳定 JSON 日志 schema 与字段白名单、秘密标记丢弃、Request ID 校验/传播、HTTP/NATS/后台 Trace、健康分层、独立低基数指标与 Exemplar | Go 注入/关联/健康/指标单测、负向源码门禁与合同测试通过；真实 OTLP/Prometheus/日志采集和三组件 Trace 查询待部署验收 |
| `RECOVERY-RELEASE-GATE` | 历史迁移规范化 SHA-256 只追加清单、生产迁移前快照和迁移后数据库摘要、分级 RPO/RTO、加密不可变备份、演练时效及分类删除收据 | Go/Node 正负向测试和全量发布门禁通过；真实四库备份、隔离恢复、月季年演练和删除收据待外部环境验收 |
| `HA-RELEASE-GATE` | 双故障域、故障后容量、核心多副本/PDB/滚动策略、外部HA依赖及Pod/节点/区域/主节点故障切换 | Helm schema/合同和证据门禁正负向测试通过；真实集群拓扑、容量与无损切换报告待外部环境验收 |
| `PERFORMANCE-RELEASE-GATE` | 生产等价资源/数据/样本、分段时延、200并发、十万Hold、百万订单/三百万流水及事件积压排空和高水位准入 | 策略与证据门禁正负向合同通过；真实生产等价负载报告待外部环境验收 |
| `AUTH-RISK` | 密码/短信登录与刷新按来源、设备、标识、稳定用户、Session 和 Provider 原子准入；短信发送/核验按手机号、来源、设备、用途和 Provider 独立计数，统一无枚举响应、耗时桶和精确重试时间 | Identity/Account Go 单测、Keycloak 风险维度转发、OpenAPI/Helm/桌面合同和迁移不可修改门禁通过；真实 Redis、短信 Provider 与攻击流量演练待部署 |
| `REQUEST-ADMISSION` | 支付创建/查询/退款按付款人、受益组织、来源、订单、动作和 Provider 原子准入；模型新逻辑请求按用户、组织成员、组织、模型、路由和全局速率/在途数在 Hold 前准入；合法回调先验签持久化再异步归并 | Billing/Token Go 单测、OpenAPI、3 个准入合同测试和迁移不可修改门禁通过；真实 PostgreSQL 高并发、Provider 回调突发与生产容量调参待部署 |
| `TENANT-ISOLATION-MATRIX` | 每次合同发布执行个人、组织、组织节点、角色/作用域、活动上下文、撤权/到期/移除/停用/注销、猜测ID、批量混入、搜索、缓存和伪造事件隔离矩阵；不存在租户导出接口时显式证明无公开导出面 | 15 个拒绝向量、授权行约束、无泄露错误和导出面合同检查通过；真实 PostgreSQL 组合数据与服务间缓存/事件故障注入待部署 |
| `PROVIDER-CIRCUIT-CLOCK` | Provider/能力/路由独立60秒熔断、20样本50%或连续5失败、三次半开探测、指数重开、业务拒绝排除和高风险不重发；生产三源NTP中位偏差在250ms告警、1秒退出就绪并阻止新写操作 | Go 单测、Helm配置/NetworkPolicy和3个合同测试通过；真实Provider失败注入、告警投递、三独立授时源和生产偏差注入待外部验收 |
| `CLIENT-COMPATIBILITY-LIFECYCLE` | Account、Billing、Gateway、Portal 共用显式客户端版本策略；N/N-1/N-2 发布时间线强制后继发布后至少90天支持、正常弃用提前60天，安全例外至少7天并要求审批号；桌面普通 API 和平台模型请求均携带精确版本，过期版本远程写操作稳定返回426和可信升级入口，本地读取与本地模型不受影响 | Go 策略/中间件单测、3个版本合同矩阵、OpenAPI、Compose/Helm 和桌面静态合同通过；实际淘汰日期由每次正式发布更新清单 |
| `AUDIT-EXPORT-PROTECTION` | 服务端审计导出任务、15分钟近期认证、默认脱敏、最长24小时临时PII授权、31天/10万行/500MB及并发/日配额、加密签名水印封装和15分钟一次性下载 | Account/Portal Go 单测、OpenAPI lint、3个审计导出合同测试、三语 Portal 构建和静态门禁通过；真实 PostgreSQL 配额竞争、生产密钥托管和大规模导出待部署验收 |
| `DESKTOP-41` | 在 `pinvou3-app/` 执行账号平台状态、三语覆盖、计划任务定向测试 | 41 个测试通过 |
| `VERIFY-ALL` | `node scripts/account-platform.mjs verify` | Go 单测与 Vet、OpenAPI/158 个合同测试、任务覆盖、迁移/恢复/HA/性能/i18n/可观测发布门禁、架构守卫和差异检查全部通过 |

### 0.1 target_requirement_id 规则

- 功能矩阵第一列是规范的 `target_requirement_id`，必须与 `requirements.md` 同义；不得使用行号、标题或自由文本代替。
- 历史 `CR-*` 是变更记录编号，不是目标需求编号，必须通过 1.1 节的 `target_requirement_id` 列映射到一个或多个目标能力。
- `ID/ACC/TOK/BILL/PAY/UI/ADM/NFR` 状态项与同名目标编号一一对应；目标文档新增或修改编号时，必须在同一变更中完成代码核验并更新证据行，不再写入无结论占位。
- `AUDIT_HEAD` 表示上述代码审计基线；`DOC_CURRENT` 表示本文件所在 Git 提交。两者都不能单独作为生产完成或部署证据。

| target_requirement_id | 当前状态 | source_commit | tests | deployment_version | known_gaps |
|---|---|---|---|---|---|
| REQ-ID-01 | 已实现 | DOC_CURRENT | DOC-COVERAGE | NOT_APPLICABLE | 需求文档共 363 个规范能力编号及 3 个编号治理目标，规范证据矩阵逐项一一覆盖且无缺失或额外编号 |
| REQ-ID-02 | 已实现 | DOC_CURRENT | DOC-COVERAGE | NOT_APPLICABLE | 366 个规范状态行均包含状态、源码基线、验证证据、部署版本和已知差距字段 |
| REQ-ID-03 | 已实现 | DOC_CURRENT | DOC-COVERAGE | NOT_APPLICABLE | 规范性表格和补充规则均复用稳定 target_requirement_id；重复出现作为同一能力的补充验收条件 |
| CORE-01 | 已实现 | AUDIT_HEAD | GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | Keycloak 是持久身份/会话事实源，独立 Identity Adapter 管理密码策略和厂商协议，Account 只保存业务映射；生产部署待验证 |
| CORE-02 | 已实现 | DOC_CURRENT | GO-UNIT; RBAC-AUTH; GLOBAL-CONTEXT | NOT_DEPLOYED | Account 独立拥有用户、平台/组织授权和以 user_id 唯一的活动上下文；OIDC Session 只证明当前登录，会话之间不再分别拥有计费上下文 |
| CORE-03 | 已实现 | AUDIT_HEAD | GO-UNIT; COMMERCE-MAPPING | NOT_DEPLOYED | Commerce Mapping 只维护用户/组织与外部客户的 Provider、Adapter 版本、脱敏引用及状态；授权上下文不读取 Mapping，生产部署待验证 |
| CORE-04 | 已实现 | AUDIT_HEAD | GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 平衡不可变账本及组成级守恒事实已实现；后续应收和支付流水任务必须继续通过同一账本扩展，数据库运行级验证待部署 |
| CORE-05 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Provider 凭据加密持久化且只在 Gateway 使用时解密；生产 KMS/Vault 待部署证明 |
| CORE-06 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CONVERGENCE; USAGE-EXECUTION-REPLAY; EXTERNAL-CONVERGENCE | NOT_DEPLOYED | 支付回调/查询/账单/修复与模型派发、上游引用、完整结果、只读证据查询、取消及结算/释放均先追加不可变摘要证据；冲突结果不覆盖首个完整事实，不确定结果进入复核且禁止自动重发高风险请求 |
| CORE-07 | 已实现 | DOC_CURRENT | GO-UNIT; REGISTRATION-SAGA; TARGETED-REFUND; AUDIT-IDEMPOTENCY-EVENTS | NOT_DEPLOYED | 当前跨服务变更均使用持久幂等 Saga 或事务 Outbox/Inbox；Relay、死信、摘要绑定服务身份重放和不可变修复审计形成闭环，不宣称跨库原子事务。 |
| CORE-08 | 已实现 | DOC_CURRENT | ROLE-PERMISSION-REDESIGN; AUTHORIZATION-COVERAGE-GATE | NOT_DEPLOYED | 生产 HTTP、任务、事件、回调和 CLI 均自动发现并映射到稳定人类权限、精确 `internal.*` 权限或专用协议策略；未知权限、作用域错配、漏登记和授权事实不可用均失败关闭。 |
| SCOPE-01 | 已实现 | AUDIT_HEAD | DESKTOP-39 | NOT_DEPLOYED | 本地独立模型仍受保护，账号平台迁移不删除用户自建模型 |
| SCOPE-02 | 已实现 | AUDIT_HEAD | DESKTOP-39; TOKEN-OPERATIONS-ISOLATION | NOT_DEPLOYED | 个人中心、自助 API 与模型调用请求均不再提供 Token Account、路由或凭据的管理和选择入口。 |
| SCOPE-03 | 已实现 | DOC_CURRENT | GO-UNIT; TOKEN-OPERATIONS-ISOLATION | NOT_DEPLOYED | 普通 `/v1/token-accounts*` 路由及公开合同已删除，用户用量状态不再返回 Token Account/钱包内部 ID；只有 `/admin/v1/*` 可管理 PLATFORM 资源，存量 USER 资源由迁移冻结、退役凭据并停用路由。 |
| SCOPE-04 | 已实现 | AUDIT_HEAD | DESKTOP-39 | NOT_DEPLOYED | 桌面运行时不再引用旧 `llmapi_hub`/旧功能开关；仍缺安装与发布制品证明 |
| SCOPE-05 | 已实现 | DOC_CURRENT | DESKTOP-CLIENT-SWITCH | NOT_DEPLOYED | 客户端迁移提交由基线 SHA 和路径白名单自动限制在品悟客户端、客户端测试/工具及直接相关文档；服务端 readiness 由独立 APT-052 提供，未混入客户端任务 |
| SCOPE-06 | 部分实现 | DOC_CURRENT | DESKTOP-CLIENT-SWITCH | NOT_DEPLOYED | TOK-37 已作为独立平台/发布能力实现仓库边界，但正式制品和生产等价发布证据仍由 APT-052 收敛。 |
| ID-15 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 注册在 Identity 与 Account 提交后返回，Billing 仅在后续 bootstrap 调用；钱包失败不会删除已提交的已验证身份或 Account |
| ID-17 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | 原始密码、冻结策略版本、短期带密钥 HMAC 和最终创建只位于 Identity Adapter；Account/短信 API 只接收 15 分钟不透明引用，持久注册 Saga 另由 ACC-51～ACC-55 追踪 |
| ID-18 | 已实现 | AUDIT_HEAD | GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | Identity 按原始字节执行 8–128 可打印 ASCII、字母和数字规则；允许中间半角空格，首尾空格返回专用错误，客户端精确确认且不 trim 密码 |
| ID-19 | 已实现 | DOC_CURRENT | THREE-SCOPE-AUTHORIZATION; LOGIN-PERMISSION-GATE | NOT_DEPLOYED | Keycloak 浏览器与 Direct Grant 的密码、手机号密码和短信路径均在凭据成功后、`context.success()` 前用工作负载身份向 Account 强一致校验精确 issuer/subject 的 `personal.session.login`；拒绝、未知主体、状态不允许、响应畸形或 Account 不可用均不签发会话。 |
| ID-20 | 已实现 | DOC_CURRENT | GO-UNIT; PASSWORD-SECURITY-AUTHORIZATION | NOT_DEPLOYED | 已登录改密要求近期认证和 `personal.security.manage`；匿名恢复只在强一致权限与生命周期允许时发送短信，未知、无权和授权不可用均同形已受理，完成后撤销全部会话。 |
| ACC-02 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Identity 与 Account 创建成功后，注册接口返回稳定 `user_id` 和 `PROVISIONING`，不调用 Billing；部署证据待补 |
| ACC-03 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | `PROVISIONING`/`REPAIR_REQUIRED` 可登录并管理资料；Token Gateway 与充值按稳定状态原因拒绝，本地独立模型不受影响 |
| ACC-04 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | 钱包任务快照策略版本与 SLA，SLA 内退避重试，超时进入 `REPAIR_REQUIRED` 并支持受审计人工重试 |
| ACC-15 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE; RBAC-AUTH | NOT_DEPLOYED | 创建由原子平台权限保护；操作者+幂等键冻结请求指纹并返回原组织，同键异请求冲突，名称唯一；成功的审计/Outbox 与创建同事务，字段/名称/幂等冲突及权限拒绝另写稳定失败审计；生产迁移待部署 |
| ACC-16 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE; RBAC-AUTH | NOT_DEPLOYED | 支持名称/精确 ID、ACTIVE/DISABLED/DELETED 显式状态和更新时间+ID游标分页；默认不返回已删除组织，服务端平台读取权限过滤入口保持默认拒绝 |
| ACC-17 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE | NOT_DEPLOYED | DISABLED→ACTIVE 前由 Billing 强校验生命周期投影和钱包可恢复性；成功时成员、用户/Session 授权版本、审计和唯一状态事件递增，重复启用无重复事件 |
| ACC-18 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE | NOT_DEPLOYED | ACTIVE→DISABLED 前先在 Billing 冻结组织钱包，因而新购买、额度入账和 Hold 统一失败关闭；Account 同事务暂停成员、回退组织 Session、递增授权版本并写审计/Outbox |
| ACC-20 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE | NOT_DEPLOYED | 仅停用组织可在 Account/Billing 全部预检清零后进入终态 DELETED；成员撤权、Session 回退、资料脱敏、审计/Outbox 与 Billing 钱包关闭均保留历史，重复删除返回成功 |
| ACC-21 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-UNIT-LIFECYCLE; RBAC-AUTH | NOT_DEPLOYED | 显式目标组织及活动父节点校验、闭包树写入、操作者隔离幂等键与版本化请求指纹同事务保存；重放返回原节点，同键异请求冲突并写审计 |
| ACC-23 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-UNIT-LIFECYCLE; DESKTOP-39; RBAC-AUTH | NOT_DEPLOYED | 显式编辑 UI 与原子权限保护的独立接口共同约束重命名；稳定命令键返回原结果，名称变化只递增节点/结构版本，不改变节点 ID、父子关系和历史审计 |
| ACC-24 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-UNIT-LIFECYCLE; RBAC-AUTH | NOT_DEPLOYED | 移动以组织级 advisory lock 和 `expected_structure_version` 串行提交，拒绝跨组织、停用父节点、自身和后代成环；闭包关系与父节点在同一事务切换 |
| ACC-25 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-UNIT-LIFECYCLE | NOT_DEPLOYED | 删除先使 Billing/Token 投影进入 DELETION_PENDING，阻止新授予/治理事实和节点 Token Account，再汇总子节点、成员、角色、审批、授予登记、Token 路由及未终态用量阻断；清零后只写 DELETED/删除时间并永久保留树、命令、审计和关联历史 |
| ACC-27 | 已实现 | DOC_CURRENT | GO-UNIT; MEMBERSHIP-GOVERNANCE; RBAC-AUTH | NOT_DEPLOYED | 邀请接受和平台直接添加都校验组织/用户状态与原子权限；操作者隔离的幂等键冻结目标、角色和节点请求指纹，重复添加返回同一成员关系且不重复递增版本或发送事件 |
| ACC-28 | 已实现 | DOC_CURRENT | GO-UNIT; MEMBERSHIP-GOVERNANCE; GLOBAL-CONTEXT; RBAC-AUTH | NOT_DEPLOYED | 移除在串行事务内幂等写 REVOKED、终止全部当前/未来角色、递增成员/用户/授权版本；若目标组织是用户级活动上下文则原子回退 PERSONAL 并递增 context_version，唯一 Outbox 后同步 Billing REVOKED 投影 |
| ACC-31 | 已实现 | DOC_CURRENT | GO-UNIT; RBAC-AUTH; ROLE-BINDING-LIFECYCLE | NOT_DEPLOYED | 普通组织/节点角色按显式目标作用域校验 `org.roles.assign`，拒绝 OWNER、基础角色和平台角色；支持开始/到期时间及成员级稳定幂等键，重复请求返回原绑定 |
| ACC-32 | 已实现 | DOC_CURRENT | GO-UNIT; ROLE-BINDING-LIFECYCLE | NOT_DEPLOYED | 单绑定撤销可重复调用并返回同一终态，OWNER/平台/基础角色拒绝走普通接口；事务内同步递增成员、用户、Session 与授权版本并发送变更事件 |
| ACC-35 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; RBAC-AUTH | NOT_DEPLOYED | 组织管理路由显式携带 target organization_id，不切换活动上下文，并按目标组织、成员状态、原子权限码及组织/节点作用域默认拒绝 |
| ACC-36 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 平台管理路由仅检查平台管理员身份，不读取或切换聊天计费活动上下文 |
| ACC-37 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | usage_request 与 Hold 冻结活动上下文、成员关系、路由、凭据、模型、协议、公式、估算器和逐单位价格事实；数据库迁移与真实并发待部署验证 |
| ACC-38 | 已实现 | DOC_CURRENT | GO-UNIT; MEMBERSHIP-GOVERNANCE; GLOBAL-CONTEXT | NOT_DEPLOYED | 普通或平台入口移除成员、停用或删除组织时均在同一 Account 事务把命中的用户级活动上下文回退 PERSONAL 并递增 context_version；Session 兼容投影同时收敛 |
| ACC-39 | 已实现 | DOC_CURRENT | GO-UNIT; RBAC-AUTH; GLOBAL-CONTEXT | NOT_DEPLOYED | Account 全部认证路由、Token 用户路由及跨服务平台授权响应使用标准版本头；组织路径返回目标 authorization_version，Portal 与桌面桥接不丢弃版本 |
| ACC-40 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA | NOT_DEPLOYED | 独立注销聚合、命令与不可变审计已持久化；状态机只前进，Worker 使用租约、版本和退避恢复，超过30分钟 SLA 转人工修复 |
| ACC-41 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA | NOT_DEPLOYED | 本地角色交接完成后用户原子进入 CLOSING；Account 授权解析和 Token 新业务稳定拒绝，历史查询及既有订单关闭、申诉和用量取消使用显式只读/修复白名单 |
| ACC-42 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA | NOT_DEPLOYED | Token 评估覆盖预占、派发/流式执行和待核对用量；仅已结算/释放终态或具有禁止客户追扣证据的平台承担终态放行 |
| ACC-43 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA | NOT_DEPLOYED | Billing 评估同时检查未解决拒付、个人钱包余额和未清应收，任一未收敛即保持 FINANCIAL_CLEARANCE 阻断 |
| ACC-44 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA | NOT_DEPLOYED | 开放/复核中的资金性支付对账案件阻断；既有明确纯信息 `UNSUPPORTED_PROVIDER_ENTRY` 不阻断，案件证据和受控结案链保留 |
| ACC-45 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA | NOT_DEPLOYED | 支付处理中、关单请求、外部待确认、失败关单、晚到支付复核及 PROVIDER_BLIND 均进入稳定注销阻断项 |
| ACC-46 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | Commerce 任务保存真实主体生命周期版本；注销拒绝未收敛资金任务，取消资料类任务，完成/重试回写均以当前生命周期和 ACTIVE 主体双重失败关闭，Mapping 最终转为 DETACHED。 |
| ACC-47 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | Identity 尚未更新的资料请求自动取消；已更新的请求先向前提交到同一用户，再进入取消和统一去标识化，预约随终态释放且不会生成第二个用户。 |
| ACC-48 | 已实现 | DOC_CURRENT | GO-UNIT; SECURITY-RECOVERY; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | 所有者和最后有效超级管理员仍须先交接；通过后清理事务撤销全部有效/未来平台及组织绑定、终止成员关系、递增授权与主体版本、清除 Account Session，并由 Identity 删除撤销全部远端认证会话。 |
| ACC-49 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | 当前、待生效标识命中的未接受邀请及由注销用户创建的未接受邀请统一进入 `CANCELLED_ACCOUNT_CLOSURE`，目标摘要、提示和一次性令牌被随机覆盖并保留不可变审计。 |
| ACC-50 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | 只有本地自动清理、Identity 幂等删除和 Billing 授予隔离均留有证明后，才在串行事务中校验主体版本、删除本地身份映射、DETACH Commerce、随机墓碑直接标识并写入 `ANONYMIZED` 与 Outbox；迟到旧版本不可回写。 |
| ACC-51 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | 正式映射与全部活动预约由数据库唯一约束共同保护，Account 提交时在串行事务内重检 |
| ACC-52 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | 发送前5分钟预约、核验后30分钟租约及 Identity 创建后无 TTL 恢复锁均已持久化并有确定释放规则 |
| ACC-53 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | 服务端操作 ID 贯穿短信、Identity、Account 与钱包；请求指纹、Identity 密码 HMAC 绑定和稳定钱包幂等键拒绝同键异请求 |
| ACC-54 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | Identity 关联查询与30分钟向前恢复已实现；24小时后仅在无 Account/映射/钱包/Outbox 事实证明下先停用并确认 Identity，再释放预约 |
| ACC-55 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | 以操作 ID、48小时签名令牌和数据库令牌摘要完成不可枚举查询，仅返回脱敏阶段与重试时间 |
| ACC-56 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; USER-CODE | NOT_DEPLOYED | `users.id` 由 UUIDv7 创建，主键保证全库唯一；数据库触发器拒绝修改和物理删除，注销墓碑保留原值，领域写路径没有复用入口 |
| ACC-57 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; USER-CODE | NOT_DEPLOYED | Account 用密码学安全随机源均匀生成 16 位 Crockford 载荷和 SHA-256 前 10 bit 校验码；具备格式/非空/唯一约束、保存点冲突重试、永久登记及不可变触发器 |
| ACC-58 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; USER-CODE; DESKTOP-39 | NOT_DEPLOYED | 本人资料、桌面/Web 个人入口及运营列表/详情展示格式化编码；授权运营人员可按完整编码精确识别活动或已注销墓碑主体 |
| ACC-59 | 已实现 | DOC_CURRENT | GO-UNIT; THREE-SCOPE-AUTHORIZATION | NOT_DEPLOYED | 数据库和授权解析器区分 `PERSONAL/PLATFORM/ORGANIZATION/UNIT`；人类角色只组合三类稳定权限码，未知权限失败关闭，`internal.*` 不进入人类角色表。 |
| ACC-60 | 已实现 | DOC_CURRENT | GO-UNIT; THREE-SCOPE-AUTHORIZATION | NOT_DEPLOYED | 全部用户创建事务自动绑定不可普通分配或删除的 `PERSONAL_USER`，迁移幂等回填非终态用户；运行时按账号生命周期白名单决定个人权限。 |
| ACC-61 | 已实现 | DOC_CURRENT | GO-UNIT; THREE-SCOPE-AUTHORIZATION | NOT_DEPLOYED | 个人、平台、每个组织及节点维护独立单调版本；角色真值表替换主动使存量绑定版本失效，平台权限与节点权限不再扁平合入个人/组织权限数组，节点授权保留绑定节点、角色和精确权限。 |
| ACC-62 | 已实现 | DOC_CURRENT | GO-UNIT; THREE-SCOPE-AUTHORIZATION | NOT_DEPLOYED | 全部组织内置角色以替换式迁移对齐审定真值表并新增 `ORG_MODEL_ADMIN`；旧权限码只在 Account 解析边界映射到规范 `org.*`，数据库不保留双重授权。 |
| ACC-63 | 已实现 | DOC_CURRENT | GO-UNIT; PERSONAL-ACCOUNT-AUTHORIZATION | NOT_DEPLOYED | 本人资料/授权/组织读取、资料修改、上下文切换、注销申请/查询及邀请读取/接受均绑定对应 `personal.*` 中间件并公开 403 契约；领域目标只取 OIDC 主体，生命周期矩阵在权限通过后继续失败关闭。 |
| ACC-64 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-OBJECT-AUTHORIZATION | NOT_DEPLOYED | 组织管理使用显式组织和可选节点目标，不切换活动计费上下文；节点树、成员、节点移动和成员移除按目标实际归属及完整子树失败关闭。 |
| ACC-65 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE; OPENAPI; UI-BUILD | NOT_DEPLOYED | 新增删除就绪度只读端点，聚合 Account、Billing、Token 阻断且不写生命周期投影；DELETE 保持二次强校验并把可选兼容请求中的原因写入审计。 |
| ACC-66 | 已实现 | DOC_CURRENT | CONTRACT; ACCOUNT-SERVICE | NOT_DEPLOYED | 平台成员端点按加入时间与成员关系 ID 提供稳定游标页，支持显示名、用户编码、完整用户 ID 搜索及角色筛选，并拒绝非法游标和角色。 |
| ACC-67 | 已实现 | DOC_CURRENT | GO-UNIT; CONTRACT; OPENAPI; UI-BUILD | NOT_DEPLOYED | 资料、状态、成员和结构写操作分别使用原子平台权限；迁移为既有 `status.manage` 角色补齐新能力并使活动绑定版本失效，审批策略以 v2 追加，历史策略不被改写。 |
| ACC-68 | 已实现 | DOC_CURRENT | GO-UNIT; CONTRACT; OPENAPI; UI-BUILD | NOT_DEPLOYED | 管理端节点移动使用结构版本 CAS、组织级串行锁、幂等命令记录、闭包表环路校验和平台结构权限；Portal 排除自身及后代目标，并在版本冲突后刷新树。 |
| TOK-04 | 已实现 | DOC_CURRENT | GO-UNIT; GATEWAY-ADAPTER-NEUTRALITY | NOT_DEPLOYED | `/v1/model-protocols` 暴露 Adapter 版本和显式 `CHAT v1` JSON/SSE 能力；路由持久化 `protocol_code`，未声明的 Embedding/Rerank/Image 等协议不会被推断可用 |
| TOK-07 | 已实现 | DOC_CURRENT | GO-UNIT; MODEL-CATALOG-LIFECYCLE | NOT_DEPLOYED | 独立版本化目录只把 PUBLISHED 及处于允许宽限期的 DEPRECATED 模型作为 `CATALOG_VISIBLE` 返回；目录展示不读取上下文资格、钱包或请求级预占结果 |
| TOK-09 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 按作用域、模型、协议和目标代次持久化 PENDING/READY/FAILED/REPAIR_REQUIRED；Worker 使用租约、SKIP LOCKED、指数退避、SLA 和修复轮次收敛，资源代次验证通过后才进入 READY |
| TOK-10 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 请求强制依次执行目录解析、托管 Provisioning、上下文资格、路由选择、定价/预占、Hold 和上游调用；非 READY 使用稳定阶段原因码失败关闭 |
| TOK-12 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | Gateway 每请求解析服务端可信上下文，组织调用同时复核 `model.invoke`、组织模型策略和当前 READY Provisioning，候选查询只接受同代有效账号/凭据/路由 |
| TOK-16 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 模型列表返回当前上下文及授权版本和非权威资格提示；请求仍重新解析目录、策略、Provisioning 和路由，不信任客户端缓存 |
| TOK-17 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | CHAT 协议使用不可变版本公式绑定计量单位；新增协议沿用同一发布契约 |
| TOK-18 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 公式缺省量按 0，发布前执行安全函数和单位引用校验，缺失绑定失败关闭 |
| TOK-20 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 请求在上游前按冻结公式和价格计算最大费用并原子创建组成级 Hold；生产故障注入待部署 |
| TOK-21 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 请求与 Hold 保存上下文、路由、模型、协议、公式、估算器及逐单位最终价格和来源快照 |
| TOK-23 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 长调用定时续租，流式与非流式响应在超出授权成本前按原价格快照追加预占；真实长流中断待部署验证 |
| TOK-25 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | 公开模型入口强制客户端键，只保存摘要并以稳定 user_id + 固定 model-inference 命名空间唯一；初次上下文/路由/计价快照不可变，待生产迁移验证 |
| TOK-26 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | CHAT v1 JSON 使用确定性带版本规范化，覆盖全部请求语义并排除 Session/传输/活动上下文；同键异指纹在 Hold 前稳定返回 `IDEMPOTENCY_KEY_REUSED` |
| TOK-27 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | 非终态保留至收敛，事件/结果/零正文墓碑硬上限为24小时/30天/90天；结果过期且墓碑存在返回410，墓碑清理同时释放键摘要 |
| TOK-28 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | Adapter 规范数据事件先按独立 AAD 加密落库和完整性校验、取得连续 sequence 后才交付；支持 Last-Event-ID 增量重放及聚合结果回退 |
| TOK-29 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | 上游上下文不继承 HTTP 断线取消；同键重连按 STREAMING、终态、核对状态和载荷过期分别重放、返回202或410，不创建第二次上游调用 |
| TOK-30 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | Hold 前加密准备、Billing 按 usage_request_id 唯一 EnsureHold、Hold/正式意图原子提升、一次性派发租约及 CAS 审计已闭环；已取得租约且结果未知的非幂等 Adapter 只转 REVIEW |
| TOK-35 | 部分实现 | DOC_CURRENT | DESKTOP-CLIENT-SWITCH | NOT_DEPLOYED | 盘点、配置备份、幂等 schema/会话/任务迁移、旧身份/缓存/凭据清理重试及只读安装预检已实现；仍缺正式安装制品崩溃注入和跨版本验收 |
| TOK-36 | 部分实现 | DOC_CURRENT | CLIENT-SWITCH-READINESS; DESKTOP-CLIENT-SWITCH | NOT_DEPLOYED | 发布门禁验证公开兼容、OIDC、摘要和短时证明；安装门禁验证证明、系统包签名及只读预检；运行门禁无旧路径回退。平台安装器的二进制/配置原子提交回滚仍缺正式制品环境证明 |
| TOK-37 | 部分实现 | DOC_CURRENT | GO-UNIT; CLIENT-SWITCH-READINESS | NOT_DEPLOYED | 内部服务身份接口只读核验 OIDC、Account 契约、目录、READY 路由及价格/公式，不创建 Hold/用量/推理；发布系统可取得绑定环境、制品摘要、客户端/服务/契约/目录版本的 Ed25519 短时证明，更新器仅获取公开兼容范围和验签公钥。生产密钥注入、真实环境连续无副作用核对及发布系统联调待验收。 |
| TOK-38 | 已实现 | DOC_CURRENT | GO-UNIT; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 取消仅按原稳定 user_id 授权并持久化键摘要/指纹；派发前竞态在用量行锁内阻止意图提升、幂等释放唯一 Hold 并终结为 LOCAL_CONFIRMED/CANCELLED_BEFORE_DISPATCH，终态返回 ALREADY_TERMINAL |
| TOK-39 | 已实现 | DOC_CURRENT | GO-UNIT; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | Adapter 统一声明 EXPLICIT_CANCEL、STREAM_ABORT_CONFIRMED_CANCEL 或 UNSUPPORTED；当前 OpenAI-compatible/New API 明确为 UNSUPPORTED，不把普通 HTTP/SSE 断开伪装成取消成功 |
| TOK-40 | 已实现 | DOC_CURRENT | GO-UNIT; USAGE-EXECUTION-REPLAY; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | execution/cancel/delivery/settlement 独立持久；断线只改 delivery，后台上下文继续读取、持久化和结算，显式停止写 USER_STOPPED 且不覆盖真实执行状态 |
| TOK-41 | 已实现 | DOC_CURRENT | GO-UNIT; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 保存取消、Provider 终态、最终用量时间线、Adapter/上游引用及不可变脱敏证据；不支持取消继续自然收敛，结果不确定按原引用退避核对并在期限转 REVIEW |
| TOK-42 | 已实现 | DOC_CURRENT | GO-UNIT; MODEL-CATALOG-LIFECYCLE | NOT_DEPLOYED | 草稿 CAS、发布、弃用、正常/紧急移除均使用合法迁移、稳定幂等键、不可变新目录版本和审计；已发布历史由数据库触发器禁止更新/删除 |
| TOK-43 | 已实现 | DOC_CURRENT | GO-UNIT; MODEL-CATALOG-LIFECYCLE | NOT_DEPLOYED | 连续两次上游缺失才自动弃用且从不物理删除；目录墓碑和 usage_request 显示/能力快照独立于实时 Model Gateway 保留 |
| TOK-44 | 已实现 | DOC_CURRENT | GO-UNIT; MODEL-CATALOG-LIFECYCLE | NOT_DEPLOYED | 稳定 UUID 身份与版本化名称/别名分离；别名直接指向模型身份，结构上不能成环，发布拒绝重复别名、规范名称冲突和 REMOVED 目标 |
| TOK-45 | 已实现 | DOC_CURRENT | GO-UNIT; MODEL-CATALOG-LIFECYCLE; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 回滚以前向更高版本复制历史内容，保留来源/原因/操作者且不复活 REMOVED 墓碑；发布前逐模型校验当前 Adapter、协议、有效价格及同代 READY Provisioning/账号/凭据/路由，并在事务内重检路由依赖 |
| TOK-46 | 已实现 | DOC_CURRENT | GO-UNIT; METERING-PRICING-LIFECYCLE; GATEWAY-ADAPTER-NEUTRALITY; MODEL-CATALOG-LIFECYCLE | NOT_DEPLOYED | 模型身份、目录、能力、协议、价格/公式和路由代次独立版本化；请求在 Hold/派发前冻结全部引用和生命周期显示快照，显示变化不创建新身份 |
| TOK-47 | 已实现 | AUDIT_HEAD | GO-UNIT; MODEL-CATALOG-LIFECYCLE; MODEL-EMERGENCY-CANCELLATION; DESKTOP-CLIENT-SWITCH; ACCOUNT-PLATFORM-STATE | NOT_DEPLOYED | Gateway 执行新会话/继续截止，桌面选择前校验上下文和目录生命周期并在不可继续时进入显式替代分支；紧急 REMOVED 与取消策略原子提交，Worker 锁定生效前已受理的在途请求并按可信结果收敛 |
| TOK-48 | 已实现 | AUDIT_HEAD | RUST-CHECK; RUST-TEST-COMPILE; MODEL-CATALOG-SELECTION; ACCOUNT-PLATFORM-STATE; BRIDGE-DOMAIN-PROTOCOL; WEB-BRIDGE-DOMAIN; MULTIAGENT-REGRESSION; UI-LANGUAGE-COVERAGE; UI-LINT; UI-BUILD; ARCHITECTURE-GUARD | NOT_DEPLOYED | 类型化全局/会话引用、兼容迁移、失败关闭、设置页新会话默认项和对话区当前会话选择均已实现；空白草稿只寄存选择并在首条消息物化时冻结，不改全局默认；来源冲突、生命周期、退役入口、三语状态和桥接协议已有自动回归门禁 |
| TOK-49 | 已实现 | DOC_CURRENT | GO-UNIT; TOKEN-MODEL-USAGE-PERMISSIONS | NOT_DEPLOYED | 目录/协议、模型调用与用量读取按个人/组织上下文分别强一致校验精确权限；取消始终以 `personal.usage.cancel` 绑定原稳定用户，不借用当前组织。 |
| BILL-04 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA | NOT_DEPLOYED | Account 提交原子写入钱包任务与 Outbox，后台使用稳定键调用 Billing、退避重试并在成功后激活账号；bootstrap 不同步调用 Billing |
| BILL-05 | 已实现 | AUDIT_HEAD | GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 权益组成表只允许 BASE/BONUS，商品入账按冻结商品快照拆分且舍入余数归 BONUS，不存在第三种积分类型 |
| BILL-09 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 组织用量 Hold 同时锁定成员授予与其精确权益包、BASE/BONUS 组成，不跨包或跨组成回退；结算和释放同步维护成员授予及组织组成桶 |
| BILL-10 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 批量授予为每一项派生稳定子幂等键并使用独立事务，返回逐项成功或稳定失败，不回滚其他项 |
| BILL-11 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 授予不提供覆盖编辑；普通撤回只回收无 Hold 占用的当前全部未消费额到原权益包原组成，已消费历史保留，变更额度需撤回可撤回部分后按新键重建 |
| BILL-12 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 普通撤回严格回到原权益包原组成并拒绝 `ACCOUNT_CLOSED`；注销后的专用全额回收属于 BILL-40，不可通过普通接口绕过 |
| BILL-13 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 成员移除以成员版本把 ACTIVE 授予转为 SUSPENDED 且不回池；同一用户仅能以更高成员版本重新加入并在未到期时恢复 |
| BILL-16 | 已实现 | AUDIT_HEAD | GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | Hold、结算和人工扣减按到期时间、BONUS/BASE、入账时间和稳定组件 ID 确定排序 |
| BILL-20 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 六类计量单位按稳定编码、版本、精度、分母、舍入和生命周期管理，历史引用不可删除 |
| BILL-23 | 已实现 | AUDIT_HEAD | CODE-AUDIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 发布和运行时均要求公式引用单位完整配置；全局只允许固定价，模型允许固定价或显式继承 |
| BILL-24 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | Provider 用量规范化为互斥非缓存输入与缓存输入，成本公式只消费规范化结果 |
| BILL-25 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 固定零价有效且阻止回退；模型继承必须显式声明，缺项稳定返回 PRICE_MISSING |
| BILL-29 | 已实现 | DOC_CURRENT | GO-UNIT; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | 实际费用超预留时只消费原 Hold 支撑额，并在同一事务以应收资产分录、独立应收和有符号钱包可用投影记录超额；数据库并发验证待部署 |
| BILL-30 | 已实现 | DOC_CURRENT | GO-UNIT; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | 未清应收只把对应个人或组织钱包置为 RECEIVABLE_OPEN，并拒绝该钱包的新建和追加 Hold；其他钱包上下文不受影响 |
| BILL-31 | 已实现 | DOC_CURRENT | GO-UNIT; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | 现金充值仅以 BASE 按最老未清应收逐笔分配，分配记录和账本不可变；退款追回、批准人工还款与核销被限制为显式受审计来源类型，不提供直接改应收入口 |
| BILL-32 | 已实现 | DOC_CURRENT | GO-UNIT; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | BONUS 不参与清偿；应收未清期间即使存在其他可用 BONUS，该钱包上下文也不能创建新 Hold，清偿后仍沿用原到期日 |
| BILL-33 | 已实现 | DOC_CURRENT | GO-UNIT; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | Provider 已确认拒付证据只定位一笔支付流水和其唯一权益包；撤销该包可撤销 BASE/关联 BONUS，现金本金缺口形成该订单应收，不改写支付成功事实或其他权益包；人工案件队列属于 PAY-33 |
| BILL-34 | 已实现 | DOC_CURRENT | GO-UNIT; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 结算只使用 Adapter 归一化的 Provider 最终/累计可信用量；分别保存可信用量、持久化序号、客户端确认序号和停止时间，禁止按 Hold 或字节估算收费，不可恢复证据缺口进入禁止用户扣款的平台损失复核 |
| BILL-35 | 已实现 | DOC_CURRENT | GO-UNIT; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 派发前取消释放全部 Hold；派发后保持原 Hold 续租并按实际用量收敛，版本化最大执行/最终用量延迟/核对期限后最终查询、释放未结算 Hold并转 REVIEW，晚到证据不得重新扣用户 |
| BILL-36 | 已实现 | AUDIT_HEAD | GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 数据库按权益包和组成强制原始额等于可用、Hold、已用、有效/暂停授予、过期及撤销桶之和，包聚合仅由触发器投影 |
| BILL-37 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 授予事务校验组织钱包、权益包和精确组成归属，只从该组成可用桶转入活动授予桶；数据库同时强制授予自身和组成总额守恒 |
| BILL-38 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 分配、撤回、成员 Hold、结算、释放和到期均锁定精确组成/授予事实；组织级幂等键保存版本化请求指纹，同键异请求稳定冲突，批量子项各自提交 |
| BILL-39 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | 注销以闭包请求和主体版本幂等隔离该用户全部有效/普通暂停授予为 `SUSPENDED/ACCOUNT_CLOSED`；不回池、不能恢复/消费/普通撤回，既有到期路径从暂停桶转入过期桶。 |
| BILL-40 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | 组织权限码保护单条及逐项批量回收；Billing 强校验 Account 已 `ANONYMIZED`、原包/组成、有效期、Hold 与资金冻结，以稳定指纹将全部当前剩余量从暂停桶原子移回原组成并写不可变命令、审计和 Outbox。 |
| BILL-41 | 已实现 | AUDIT_HEAD | CODE-AUDIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 按请求时间唯一选择模型覆盖，无覆盖才选全局；缺失、歧义和不可用使用稳定错误 |
| BILL-42 | 已实现 | AUDIT_HEAD | CODE-AUDIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 逐单位解析 FIXED_PRICE/INHERIT_GLOBAL，固定零价阻止回退并冻结最终来源 |
| BILL-43 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 发布事务按定价空间串行锁定并执行区间冲突、ETag、幂等键和请求指纹校验；真实数据库并发待部署验证 |
| BILL-44 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 支持 DRAFT/PUBLISHED/ACTIVE/ENDED/CANCELLED、生效前取消及 ACTIVE 阻断/全局继承/替代三种结束模式 |
| BILL-45 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | Hold 冻结逐单位价格、来源、分母和舍入；撤销、结束或替代后续租、追加及结算不重算 |
| BILL-46 | 已实现 | DOC_CURRENT | GO-UNIT; BILLING-SCOPE-AUTHORIZATION | NOT_DEPLOYED | 个人余额、权益和订单严格要求个人原子权限且不能在组织上下文借用；组织购买、余额、权益和授予使用 URL 显式目标组织实时鉴权，无需切换活动上下文。 |
| BILL-47 | 已实现 | DOC_CURRENT | GO-UNIT; GO-POSTGRES; OPENAPI; ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 四个精确权限端点为三个分区返回固定 key/unit/status 的点时或 UTC 半开窗口指标；查询失败整端点 503，不返回部分指标或伪 0。 |
| PAY-11 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE | NOT_DEPLOYED | 订单允许多笔 Provider 流水；支付来源权益包唯一绑定一笔成功流水，不同成功流水分别独立履约或进入复核 |
| PAY-12 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE; PAYMENT-CONVERGENCE | NOT_DEPLOYED | 回调、主动查询、Provider 账单和人工修复均先保存 Provider/版本/流水/金额/时间/载荷摘要；成功账单行复用统一归并器并按流水防止重复入账，未匹配或冲突账单仍留证并进入对账复核 |
| PAY-13 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CONVERGENCE; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | 支付流水 `PAID/LATE_PAID` 不被退款、拒付或关单覆盖；退款、拒付、关单分别推进独立状态机，订单支付/退款/拒付/关注摘要由底层事实含应收优先级完整重建 |
| PAY-14 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CONVERGENCE; ACCOUNT-PLATFORM-STATE | NOT_DEPLOYED | Pinvou 订单到期末次查询仍未付款即以 `EXPIRED` 形成用户可见本地终态并清除支付指令；`close_status` 独立跟踪 Provider 关闭观察，商城无法关单不阻塞本地终态，晚到成功仍保留并归并 |
| PAY-19 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 本地 `EXPIRED` 后以不高于每5分钟一次的频率继续捕获晚到支付；72小时边界执行最终查询并幂等转唯一审核案件，绝不恢复普通 `PENDING`；真实 Provider 故障注入待部署 |
| PAY-20 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE | NOT_DEPLOYED | 本地关闭或超时后可创建新订单；不同 Provider 成功流水分别幂等履约或复核，同一流水重复证据只归并一次并保留 LATE_PAID |
| PAY-23 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE; COMMERCE-MAPPING | NOT_DEPLOYED | 组织订单冻结实际付款管理员、受益组织及付款人的 Provider Commerce Mapping 快照；组织级 Mapping 只由 Provider 能力选择 |
| PAY-24 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | 普通退款同时绑定并校验支付流水、权益包和订单归属；订单 ID 不能单独执行退款 |
| PAY-25 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | 任一组成/授予已消费、预占、到期、撤销，或存在 Hold、拒付、强制退款、应收及资金异常时在 Provider 调用前拒绝 |
| PAY-26 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | 不接受金额输入；现金金额派生自目标支付流水并整笔退款，关联 BONUS 零折现且成功时一并撤销，普通退款无部分状态 |
| PAY-27 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | 串行事务锁定目标并原子保存组成/授予快照、冻结和 Outbox；显式幂等能力门禁后的 Worker 使用稳定退款号恢复 REQUESTED 意图 |
| PAY-28 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | 退款意图冻结权益包、组成和活动授予；明确失败恢复，不确定结果继续冻结，成功或受审计复核后才终结 |
| PAY-29 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | 退款沿目标支付流水所属订单调用原 Provider，稳定商户退款号受显式幂等能力保护，不借用其他流水或钱包 |
| PAY-31 | 已实现 | AUDIT_HEAD | GO-UNIT; TARGETED-REFUND; REFUND-PRODUCT-GATE; DESKTOP-39 | NOT_DEPLOYED | 默认关闭的独立产品门禁在内部/后台创建端点和运营动作前生效，Provider 支持退款也不会自动开放；既有退款 Consumer、强制退款事实和只读查询继续可用 |
| PAY-32 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | 渠道强制退款保存不可变独立证据和案件并立即冻结原流水权益包/授予；确认后按退款现金比例向上取整定向撤销 BASE/关联 BONUS，BASE 不足形成应收，不覆盖支付成功或伪装普通退款 |
| PAY-35 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 能力显式区分 API、受控不可变文件导入和 QUERY_DEGRADED；查询快照降级不冒充供应商账单对账，真实文件格式联调待部署 |
| PAY-37 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 72小时最终查询后唯一创建P2案件，未认领进入运营队列并告警，前7天每日、之后每周复核；用户申诉只追加证据，运营查询发现支付后经统一归并器幂等履约 |
| PAY-38 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 无回调无账单的 FROZEN 映射必须同时声明长期历史查询、有限支付/晚到窗口和权威未支付终态，能力冻结到订单；不合格 Provider 拒绝正式新单 |
| PAY-39 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 原子归档门禁要求超过 Provider 窗口+晚到窗口+7天、期限后成功查询和权威未支付终态，且无支付流水、冲突证据或未复核用户申诉；归档保留全部事实并可被强支付证据重开 |
| PAY-40 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 到归档期限仍无成功权威查询会进入 PROVIDER_BLIND、保存原因/责任/下次处置、升级事件并停用该 Provider 版本新订单，不能普通归档 |
| PAY-41 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CALLBACK-GATEWAY | NOT_DEPLOYED | 新华三创建订单必传付款人的永久不可变 `user_id` 作为 `merchantCustomerKey`；主动查询和回调返回值均与订单冻结的付款人标识再次核对，手机号和展示编码不承担资金归属 |
| PAY-42 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CALLBACK-GATEWAY | NOT_DEPLOYED | H3C Profile 严格接收与 `queryOrder.content` 相同的原生订单对象，回调和主动查询复用同一 DTO、状态/流水/时间/金额映射及统一证据归并器，未知状态失败关闭为人工复核 |
| PAY-43 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CALLBACK-GATEWAY | NOT_DEPLOYED | 商城元金额由精确十进制转换为分；PAID 必须具有正实付金额、支付时间和稳定流水，订单号、客户标识、SKU、金额或支付状态冲突均拒绝履约并保留复核证据 |
| PAY-44 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | `CONFIRM_PROVIDER_FAILED` 仅允许本人已认领的非法跃迁案件；Billing 现场查询并原子复核永久客户标识、SKU、金额、无流水/权益/申诉/并发证据后结案，保存不可变确认和 Outbox |
| PAY-45 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PRODUCT-CONTROLS | NOT_DEPLOYED | 目录上架事实保持独立；Provider 与商品级 `new_orders_enabled` 运行时控制共同拦截创建订单，自动止损只更新控制表并保留来源案件和版本 |
| PAY-46 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PROVIDER-RECOVERY | NOT_DEPLOYED | `PAYMENT_PROVIDER_RESUME` 复用 Account 双人审批，冻结 Provider/商品控制版本与探测订单；Billing 执行前现场查询 Provider、复核 P0/P1 案件、映射及人员隔离后原子恢复并写不可变审计 |
| PAY-47 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PRODUCT-CONTROLS | NOT_DEPLOYED | 商品目录继续返回暂停商品，并以 `PROVIDER_PAUSED`/`PRODUCT_PAUSED` 和稳定原因码说明运行时不可售性；服务端创建订单同步失败关闭 |
| PAY-48 | 已实现 | AUDIT_HEAD | GO-UNIT; PRODUCT-CATALOG-RUNTIME | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | Billing 已实现 DRAFT/ON_SALE/OFF_SALE/DELETED 目录聚合及添加、发布、下架、删除领域命令；云端迁移、目录聚合和服务健康已验证。 |
| PAY-49 | 已实现 | AUDIT_HEAD | GO-UNIT; PRODUCT-ADMISSION-LIFECYCLE | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 发布快照、异步验证轮次、完整指纹、追加证据/绑定/撤销、续证与失效止损均已持久化；生产 Provider 准入仍需 APT-048 环境验收。 |
| PAY-50 | 已实现 | AUDIT_HEAD | GO-UNIT; PRODUCT-CATALOG-RUNTIME | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 下架原子结束活动映射和当前准入绑定，新单失败关闭，历史订单继续引用冻结映射快照。 |
| PAY-51 | 已实现 | AUDIT_HEAD | GO-UNIT; PRODUCT-CATALOG-RUNTIME | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 删除使用保留稳定标识与 ever_published 的逻辑墓碑；发布映射、订单和审计事实不删除、不复用。 |
| PAY-52 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PROVIDER-BOUNDARY | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 列表、下单和 H3C 查询均动态解析数据库目录/映射；下单先提交稳定本地意图并释放锁，再调用 Provider，未知结果保留恢复状态。 |
| PAY-53 | 已实现 | AUDIT_HEAD | GO-UNIT; RECHARGE-PRODUCT-ADMIN | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 商品命令校验 expected_version、16–128 字符幂等键、终端 OIDC auth_time 与不可变命令审计；旧版本、异请求同键和陈旧认证失败关闭。 |
| PAY-54 | 已实现 | AUDIT_HEAD | PRODUCT-DOCUMENT-BASELINE | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 需求、架构、任务和状态基线不再记录运行时商品标识、金额、权益值或商品数量；商品变更只进入目录和审计。 |
| PAY-55 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-TIERED-STOP-LOSS | NOT_DEPLOYED | 单案件评估不再写 Provider 控制；只有受信新单异常类型可由逾期 SLA 关闭目标商品，独立分钟评估不受通知节流影响。 |
| PAY-56 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PROVIDER-HEALTH | NOT_DEPLOYED | 新单关键分区按去重连续失败、失败率、路径多样性和跨商品共同故障持久评估；非关键动作与业务拒绝不触发 Provider 新单控制。 |
| PAY-57 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PROVIDER-HEALTH | NOT_DEPLOYED | 调用前意图、事务外调用、调用后观察和 Outbox 已分阶段持久化；尝试、gap、窗口成员、策略及阈值穿越均有不可变证据。 |
| PAY-58 | 已实现 | DOC_CURRENT | PAYMENT-TIERED-RECOVERY; PAYMENT-TIERED-STOP-LOSS-DESIGN | NOT_DEPLOYED | Provider 与商品使用独立审批命令和执行审计；Provider 恢复不改商品，商品恢复不改 Provider/其他商品，且 Provider 暂停、观察缺口、开放阻断案件、探测失败或冻结版本变化均失败关闭。 |
| UI-02 | 已实现 | AUDIT_HEAD | REGISTRATION-SAGA; DESKTOP-39 | NOT_DEPLOYED | 桌面区分注册、自动登录与钱包初始化，展示 `PROVISIONING`/`REPAIR_REQUIRED`，初始化期间不请求余额、充值或平台模型 |
| UI-13 | 已实现 | DOC_CURRENT | DESKTOP-39; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 三语用量页可停止当前客户端交付，并分别展示执行和结算状态；UNSUPPORTED 明示后台继续结束并按可信实际用量结算，不显示为即时免费 |
| UI-14 | 已实现 | DOC_CURRENT | ACCOUNT-CLOSURE-SAGA; UI-LINT; ACCOUNT-PORTAL-BUILD | NOT_DEPLOYED | 个人中心提供明确确认、阶段/SLA、脱敏阻断分类和用户动作；轮询持久状态，不展示 Provider 响应、内部资源 ID 或其他用户信息 |
| UI-15 | 已实现 | AUDIT_HEAD | REGISTRATION-SAGA; DESKTOP-39 | NOT_DEPLOYED | 系统凭据库保存恢复查询凭据，启动后按 `next_retry_at` 轮询并区分 Identity、Account、钱包、修复和补偿状态，处理中禁止重复注册 |
| ADM-11 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 组织端仅以 `org.models.read/manage` 读取或 CAS 修改允许/拒绝模型策略和资格提示；真实 Token Account、路由、凭据及 Provisioning 只由平台超级管理员和平台模型运营权限访问 |
| ADM-12 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 后台可查看、创建版本和停用计量单位；历史引用受不可变约束保护 |
| ADM-13 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 后台支持全局/模型草稿、逐单位配置、预览、发布、撤销和 ACTIVE 显式结束，带 ETag 与幂等冲突控制 |
| ADM-14 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 管理后台可查询钱包状态；生产授权隔离仍需部署验证 |
| ADM-15 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 管理后台可查询 Journal/Posting，数据库保护已入账分录不可变 |
| ADM-16 | 已实现 | DOC_CURRENT | GO-UNIT; RECEIVABLE-LIFECYCLE | NOT_DEPLOYED | 运营 API 和三语账务页展示应收来源、原始/已清/未清金额、钱包冻结状态及不可变清偿分配；没有直接降低应收的管理写接口 |
| ADM-17 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 人工调整冻结目标、钱包、金额和调整版本；独立审批后由 Billing 执行端使用稳定幂等键写一次平衡账本，旧直接批准入口失败关闭 |
| ADM-18 | 已实现 | DOC_CURRENT | GO-UNIT; HOLD-OPERATIONS-EVIDENCE | NOT_DEPLOYED | 运营 API 和三语页面查询 Hold、组成/成员分配、当前租约及不可变租约历史；异常释放只允许 REVIEW 或租约已过期 ACTIVE，要求权限、原因和幂等键并保存操作前后及分配快照 |
| ADM-19 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE | NOT_DEPLOYED | 后台支持订单搜索、同步、修复及单订单明细，展示实际付款人、受益方、全部支付流水、不可变证据和派生摘要 |
| ADM-20 | 已实现 | AUDIT_HEAD | GO-UNIT; TARGETED-REFUND; REFUND-PRODUCT-GATE; DESKTOP-39 | NOT_DEPLOYED | 既有普通/强制退款、证据和异常处置继续只读可见；新建动作只有产品门禁与 Provider capability 同时开放才可能出现，当前所有交付配置均默认关闭且服务端兜底拒绝 |
| ADM-21 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | 三语后台可独立运行、重跑和查看四方对账，保留每次运行及原始差异快照，并以幂等命令认领、分配、登记修复动作、结论和处理状态；真实 Provider 账单待部署验证 |
| ADM-22 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 当前 Billing 修复命令均登记稳定类型、目标、原因、脱敏证据、前后快照和幂等键；改变财务事实的修复统一进入审批执行框架，证据收敛类修复固定为仅审计 |
| ADM-23 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 后台可查看和创建版本化模型协议定义 |
| ADM-24 | 已实现 | AUDIT_HEAD | GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 后台支持公式草稿、校验预览、发布和不可变历史，并同步 Billing 公式单位绑定 |
| ADM-25 | 已实现 | DOC_CURRENT | GO-UNIT; TOKEN-OPERATIONS-ISOLATION; SUPER-ADMIN-PLATFORM-PERMISSIONS | NOT_DEPLOYED | `PLATFORM_SUPER_ADMIN` 与 `PLATFORM_MODEL_ADMIN` 具备 Token 运营原子权限，可通过后台创建个人/组织作用域的 PLATFORM 管理账号并启停；组织角色不隐式继承 |
| ADM-26 | 已实现 | DOC_CURRENT | GO-UNIT; TOKEN-OPERATIONS-ISOLATION; SUPER-ADMIN-PLATFORM-PERMISSIONS | NOT_DEPLOYED | 超级管理员与平台模型运营接口仅返回凭据类型、状态、版本、更新时间、允许模型及指纹，不返回密钥、密文、Key ID 或 Provider 地址 |
| ADM-27 | 已实现 | DOC_CURRENT | GO-UNIT; TOKEN-OPERATIONS-ISOLATION | NOT_DEPLOYED | 凭据检查使用独立原子权限和只审计命令，响应只含 `VALID/INVALID`、稳定脱敏错误码与元数据，原始 Provider 错误不出服务边界 |
| ADM-28 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 路由 CRUD、确定性选择和托管目标/实际代次统一展示；平台候选只有当前 READY 且账号、凭据和路由代次一致时可进入数据面，显式兼容 USER 路由保留原所有权校验 |
| ADM-29 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | 平台模型管理员可查询 Provisioning 作用域、目标/实际代次、状态、阶段、尝试次数、租约、重试/SLA 和脱敏错误，并读取尝试历史 |
| ADM-30 | 已实现 | DOC_CURRENT | GO-UNIT; ROUTE-PROVISIONING-ELIGIBILITY | NOT_DEPLOYED | FAILED/REPAIR_REQUIRED 才接受带原因和幂等键的修复命令；命令开启新受限轮次，只有协调 Worker 重建并验证资源后才写 READY，审计不可变 |
| ADM-31 | 已实现 | DOC_CURRENT | GO-UNIT; CONTRACT-36; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 平台模型运营查询返回请求/上下文、请求指纹、四类状态、上游引用、持久化/客户端确认序号和不可变脱敏取消证据，不返回用户正文或秘密 |
| ADM-32 | 已实现 | DOC_CURRENT | GO-UNIT; CONTRACT-36; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 人工核对重试强制幂等键与请求指纹，只调度原上游证据查询且不重发模型请求；已释放并禁止用户扣款的平台损失案件拒绝重开收费路径 |
| ADM-33 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW; DESKTOP-41 | NOT_DEPLOYED | 三语后台支持按状态、Provider、责任人、进入/复核时间和逾期筛选，展示能力/策略快照及不可变证据/操作时间线，并提供认领、查询、安排、严格归档和 blind 升级；无手工标记已支付入口 |
| ADM-34 | 已实现 | DOC_CURRENT | ACCOUNT-CLOSURE-SAGA; UI-LINT; ACCOUNT-PORTAL-BUILD | NOT_DEPLOYED | 独立权限保护的注销队列展示阶段、SLA、阻断快照和脱敏错误；幂等重试只能重新评估当前阶段，没有强制完成、忽略阻断或直接删除用户入口 |
| ADM-35 | 已实现 | AUDIT_HEAD | GO-UNIT; REGISTRATION-SAGA; RBAC-AUTH | NOT_DEPLOYED | 三语修复页及脱敏队列支持状态、阶段、责任域、年龄和 SLA 过滤；可重试确定阶段，补偿按钮与命令受24小时、无业务事实、先停用 Identity 后释放预约的证明门禁约束 |
| ADM-36 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 策略版本 1 以稳定 `command_type` 显式登记全部当前后台写入口；代码覆盖合同检查路由注解，未知或分类冲突均失败关闭 |
| ADM-37 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 九类直接改变资金事实的命令固定为 `APPROVAL_REQUIRED`、恰好一名独立审批人且禁止自审；当前仅已实现领域语义的钱包调整可申请，其余类型在对应领域执行器交付前失败关闭 |
| ADM-38 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 普通退款、组织删除、治理配置和确定性任务显式登记为 `AUDIT_ONLY`；强制财务修改只能使用独立命令类型，不能通过参数切换分类 |
| ADM-39 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 持久状态机使用串行事务、版本 CAS、默认24小时申请期和5分钟执行窗口；支持撤回、过期、终态不可重开及 `supersedes_request_id` 新申请 |
| ADM-40 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 申请保存规范化命令 SHA-256 指纹、目标、关键版本、财务/策略/授权快照和预期影响；审批与执行不匹配时写结构化失效原因 |
| ADM-41 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 申请、审批、执行分别直接读取 Account 用户、平台绑定、有效期和授权版本；撤权、停用、到期、版本变化或授权事实不可确认时失败关闭 |
| ADM-42 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 批准事务仅发布绑定指纹/策略/快照的 Outbox 授权；Billing 执行端在短窗口内复核并以稳定执行键原子入账，失败可按原命令重试且数据库阻止新调整绕过审批 |
| ADM-43 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | 支付异常接口只接收案件、原因和幂等键，Provider 状态及资金门禁完全由 Billing 现场查询与事务复核决定 |
| ADM-44 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PROVIDER-RECOVERY | NOT_DEPLOYED | 后台控制查询返回 Provider/商品开关、原因、版本和可直接提交 Account 审批的冻结恢复草稿；申请人与审批人独立，版本变化自动失效 |
| ADM-45 | 已实现 | DOC_CURRENT | GO-UNIT; THREE-SCOPE-AUTHORIZATION; SUPER-ADMIN-PLATFORM-PERMISSIONS | NOT_DEPLOYED | 九类系统角色以审定逐码真值表维护；超级管理员覆盖全部常驻平台权限，专业角色仍按职责分离，未脱敏 PII 导出继续独立临时授权，迁移显式递增受影响主体的平台授权版本。 |
| ADM-46 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 审批列表、详情、允许动作、决定及执行前复核均从申请版本冻结的请求/审批权限解析；撤权或策略字段变化失败关闭。 |
| ADM-47 | 已实现 | DOC_CURRENT | GO-UNIT; TOKEN-OPERATIONS-ISOLATION | NOT_DEPLOYED | 普通 Token 管理路由、处理器和 OpenAPI 资源已移除，存量非平台账号/凭据/路由保存可逆隔离证据后冻结；创建和执行候选均强制 PLATFORM 管理模式。 |
| ADM-48 | 已实现 | DOC_CURRENT | GO-UNIT; SECURITY-RECOVERY | NOT_DEPLOYED | Break-glass 操作者与目标来自最长 15 分钟 Ed25519 签名凭据，CLI 不接受身份参数；凭据 ID 唯一防重放，审计 actor 不再伪装成恢复目标。 |
| ADM-49 | 已实现 | DOC_CURRENT | GO-UNIT; ROLE-PERMISSION-REDESIGN; HUMAN-DELEGATION-SIGNING | NOT_DEPLOYED | Account 只在强一致原子权限通过后签发最长 5 分钟 Ed25519 委托；Billing 组织额度写入口绑定受众、权限、组织、资源、方法、路径和原始请求摘要验签，DTO 已删除自报 actor，领域审计同时固化工作负载主体、委托 ID、人类 actor 及授权版本。 |
| ADM-50 | 已实现 | AUDIT_HEAD | AUTHORIZATION-COVERAGE; RECHARGE-PRODUCT-ADMIN | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | Account 新增五个原子商品权限并按审定角色真值表授予；Billing 路由逐项要求对应权限，角色名和定价宽权限不能替代。 |
| ADM-51 | 已实现 | AUDIT_HEAD | COMMAND-POLICY; RECHARGE-PRODUCT-ADMIN | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 添加、验证/撤销、上架、下架和删除均登记 AUDIT_ONLY 策略，命令记录绑定操作者、原因、幂等指纹和前后版本。 |
| ADM-52 | 已实现 | AUDIT_HEAD | OPENAPI-LINT; PORTAL-PROXY; RECHARGE-PRODUCT-ADMIN | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | Billing 提供列表、添加、验证/重试/撤销、上架、下架和删除接口；OpenAPI 与 Portal 只开放精确方法和 UUID 路径。 |
| ADM-53 | 已实现 | DOC_CURRENT | PAYMENT-TIERED-RECOVERY; PAYMENT-TIERED-STOP-LOSS-DESIGN | NOT_DEPLOYED | 后台控制接口返回 Provider/商品层级、策略与评估窗口指标、观察缺口和控制版本，并只在当前层满足条件时分别生成 Provider 或单商品恢复草稿。 |
| ADM-54 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; APPROVAL-COMMANDS; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 六个现有定价与计量管理命令已按路由精确注册为 `AUDIT_ONLY`，统一要求 `platform.pricing.manage`；Account 000041 以追加迁移持久化相同策略，保留旧命令历史事实且未知命令继续失败关闭。 |
| ADM-55 | 已实现 | DOC_CURRENT | SUPER-ADMIN-PLATFORM-PERMISSIONS; THREE-SCOPE-AUTHORIZATION; TOKEN-OPERATIONS-ISOLATION; SUPER-ADMIN-PRICING-UAT-20260903 | 0.8.0@4ccf7404 (CLOUD_UAT, 2026-09-03) | Account 000042 为超级管理员补齐除临时未脱敏 PII 导出外的全部现有常驻平台权限并递增有效绑定的授权版本；Token 高风险命令继续执行既有审批、审计、脱敏和秘密不可回显边界。 |
| ADM-56 | 已实现 | APT-121-DONE | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-ADMIN-QUERY-ADAPTERS | NOT_DEPLOYED | 九类 Token 管理查询已接入逐资源规范化筛选、固定全序、独立签名 cursor、统一 envelope、稳定错误与无 total 合同，并通过身份 B 第四轮独立代码审核。真实 PostgreSQL 集成本地未执行，CI 隔离库强制 PASS 且拒绝 SKIP。 |
| ADM-57 | 已实现 | APT-124-DONE; APT-124-R1-DONE | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-IMPACT-PRECHECKS; OPENAPI-LINT; GO-UNIT; POSTGRES-CI-GATE | NOT_DEPLOYED | APT-124 原五类只读预检、固定全序流式关键集合指纹、规范化双域预检指纹、逐 tuple 动作感知依赖、Billing 权威单事务批量价格覆盖、默认关闭 gate 和统一 5 秒 deadline 已实现，并通过身份 B 第五轮独立代码审核，历史 `DONE` 结论保留。APT-124-R1 已实现协议/公式/同步等闭合权威 precheck、对任一定价事实变化敏感的完整 financial 修订号/全量指纹、模型生命周期精确 13 字段与回滚精确 8 字段快照；模型发布新增精确协议版本的 resolved 路由、Provisioning 与 READY 覆盖计数/全量指纹并在提交前重验。Account 已实现 `critical_resource_versions + financial_snapshot` 的 approval create 原子持久/响应、submit 权威重验、授权事件和 begin-execution lease 全链透传；包内真实 HTTP/强授权/幂等事务测试接缝不改变生产 `PRE_ACTIVATION`。APT-124-R1 已通过身份 B 第三轮独立代码审核；APT-138 consumer/领域执行未启动。 |
| ADM-58 | 已实现 | APT-133-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; POSTGRES-CI-GATE; BROWSER-SMOKE | NOT_DEPLOYED | APT-133 已通过身份 B 第四轮独立代码审核。独立 Credential Origin、一次性 launch/session/result、PKCE/JWT/JWKS、权威 actor/authorization/gate 版本冻结、验证租约、最大版本递增、三阶段轮换及 11+0 撤销执行器已闭环；原始 executor 严格拒绝非法 financial snapshot 且在仓储前零写。生产 handoff 使用固定 SHA-256 CSP、SW 隔离、闭合 MessageChannel、一次内存 body POST 与同引用未知恢复，秘密和 launch/result ID 不进入 Portal、URL、DOM、Performance、storage、日志或审计。本地 PG 测试无 URL 时明确 SKIP，CI 强制 no-SKIP；APT-137 网络探针与 APT-138 激活未提前实现。 |
| ADM-59 | 已实现 | AUDIT_HEAD | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-MODEL-OPERATIONS-PRICE-PROJECTION | NOT_DEPLOYED | Token 仅从本域目录、稳定路由、Provisioning 和用量事实生成 DB 时间原子概览；Billing 按稳定 `model_id` 与精确定价 tuple 独立返回当前/下一实际变化、覆盖状态、计量单位、继承来源和 ETag。Portal 不组合快照，两域各自超时、失败关闭并由独立默认关闭 gate 回滚；本地真实 PostgreSQL 用例明确 SKIP，CI 必须 PASS 且拒绝 SKIP。 |
| ADM-60 | 已实现 | APT-122-DONE; APT-122-R2-DONE | TOKEN-PROTOCOL-FORMULA-GOVERNANCE; ADMIN-MODEL-TOKEN-REDESIGN | NOT_DEPLOYED | APT-122 原策略/CAS/Billing 权威同步交付及 APT-122-R2 通用 adapter 整改已分别通过身份 B 第三轮、第五轮审核。四命令以 canonical envelope 和权威 precheck 把领域 draft/version、通用 result、脱敏 audit、Outbox 收敛在同一 Token 事务；公式发布跨 Billing T1 可按原 reference 仅 GET 恢复，确定冲突终态可查。旧专用写在 PRE 失败关闭且不构成 ACTIVE 旁路，APT-138 尚未激活。 |
| ADM-61 | 已实现 | APT-119-DONE; APT-123-R2-DONE; REMEDIATION_REVIEW_PASS | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-ACCOUNT-SAGA; POSTGRES-THREE-SERVICE; OWNER-CAP-ROLE-GATE; BROWSER-SMOKE | NOT_DEPLOYED | APT-119 已审核候选交付历史保留，APT-123-R2 已取得身份 B 最终 PASS。公开候选四字段/服务端分页不泄露授权证据；T1/OPERATOR/T3_CREATOR/T3_OPERATOR 维持固定 request/JCS 向量与独立 creator/operator 检查点，ORG_UNIT response/digest 绑定 Account 真实组织且父组织非 ACTIVE 失败关闭。Token reservation → Billing immutable receipt → Token finalize Saga、共享配额与跨 writer 并发、SAFE/lease/响应丢失、分页 MANUAL_REVIEW 队列、跨 operator 同 key 隔离、双检查点与理由审计均已实现。Token/Billing 数据库/schema 由 NOLOGIN owner 隔离，真实 migrator/operator/runtime 身份验证迁移、策略和拒绝 CREATE/DML。bootstrap/capability/quiescence/drain 已交付，三服务与角色门禁本地真实 PASS/no-SKIP。生产 create/worker 接线已由 APT-138 完成并获 B 最终 PASS；实际部署与 ACTIVE 激活仍待 APT-137，当前 NOT_DEPLOYED。 |
| ADM-62 | 已实现 | APT-125-DONE; APT-125-R1-DONE | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-STABLE-ROUTES-HEALTH; TOKEN-ROUTE-GOVERNANCE-PG; BROWSER-SMOKE | NOT_DEPLOYED | APT-125 稳定引用/健康历史保留；APT-125-R1 获身份 B 最终 PASS。两类通用 adapter 共享 Tx effect，事实/版本/墓碑/result/audit/outbox 同事务；精确集合/legacy 健康漂移、跨账户幂等、真实 HTTP 响应丢失及 PRE/ACTIVE/SAFE/v2 retired 矩阵全部具名 PG PASS/no-SKIP，生产尚未激活。 |
| ADM-63 | 已实现 | APT-121-DONE | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-ADMIN-QUERY-ADAPTERS | NOT_DEPLOYED | 模型与 Token Account 已从本域不可变事实提供固定单资源、脱敏分页审计投影，读取采用资源读权与 `platform.audit.read` 的同一授权版本原子交集；身份 B 第四轮独立代码审核通过。 |
| ADM-64 | 部分实现 | APT-138-DONE; APT-138-R1-PENDING | TOKEN-GOVERNANCE-ACTIVATION; GO-UNIT; OPENAPI; BROWSER-SMOKE; GSA-02-FACT-REVIEW | NOT_DEPLOYED | APT-138 身份 B 整批最终 PASS 的十九命令 registry/DB/dispatch、000031 activation、生产 guard、Account 强读、三审批 consumer/NATS与双worker事实继续成立。GSA-02确认 Account审批链只要求`AuthenticatedAt`非零，三项Token审批的申请/决定15分钟年龄门禁尚未闭合；代码唯一由ADM-67/GSA-13和GSA-14实现，APT-138-R1只引用两项提交/B审核证据关闭历史缺口，不另改产品代码。PRE/ACTIVE/SAFE及实际激活、部署/N-N-1边界不变。 |
| ADM-65 | 已实现 | APT-123-DONE; APT-123-R2-DONE; REMEDIATION_REVIEW_PASS | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-COMMAND-GOVERNANCE-BASE; POSTGRES-APPROVAL-CHAIN | NOT_DEPLOYED | APT-123 十九策略、通用结果/幂等恢复、三项强制审批与 PRE 基础历史保留。APT-123-R2 已闭合即时模型/账户/回滚五命令与真实 Account→Token 审批/生命周期 worker 测试；稳定事实比较不把独立观测时间当变更，Account 000047 草稿漂移失效及历史保护真实通过。范围外路由 adapter 仍归 APT-125-R1，activation record/生产 worker/consumer 注册仍归 APT-138，ADM-64 未激活。 |
| ADM-66 | 已实现 | APT-139-DONE; REVIEW_PASS | ADMIN-MODEL-TOKEN-OVERVIEW-REALIGNMENT; MODEL-TOKEN-PAGE-OVERVIEWS | NOT_DEPLOYED | APT-139 已新增 OverviewRead 3/5/7 秒期限链、三个页面级只读投影、closed schema、精确权限/oneOf/无权零 SQL、60/120 限流、typed 三路稳定错误、原始路径别名拒绝、冻结数据与生产 SQL 性能门禁，并保留旧 `/operations-overview`；三个投影均以单次授权快照要求正授权版本。身份 B 最终独立审核 PASS；定向检查及隔离 PostgreSQL 具名集成多轮通过，生产规模限制环境性能仍待远端 CI，当前未推送或部署。 |
| NFR-04 | 已实现 | DOC_CURRENT | GO-UNIT; OBSERVABILITY-RELEASE-GATE | NOT_DEPLOYED | 共享日志 Handler 仅接受稳定事件和字段白名单，未知消息折叠，原始 error、密码、Token、Cookie、正文和 Payload 属性不序列化；标准库日志也经固定事件适配，注入秘密标记与绕过扫描进入全量 verify |
| NFR-21 | 已实现 | DOC_CURRENT | GO-UNIT; OBSERVABILITY-RELEASE-GATE | NOT_DEPLOYED | 所有账号平台服务启动即配置统一 JSON schema，必含 time、level、service、environment、version、event、result；HTTP、Relay 和后台任务复用同一 Handler，未知字段失败关闭 |
| NFR-22 | 已实现 | DOC_CURRENT | GO-UNIT; OBSERVABILITY-RELEASE-GATE | NOT_DEPLOYED | 每个业务/Relay HTTP 入口只接受64字符内安全 Request ID，否则生成128-bit ID；响应、日志和共享 HTTP Client 传播同一受信 ID，事件上下文以 event_id 建立独立请求关联 |
| NFR-23 | 已实现 | DOC_CURRENT | GO-UNIT; OBSERVABILITY-RELEASE-GATE | NOT_DEPLOYED | HTTP 使用 W3C Trace Context；Outbox 固化 Trace ID，Relay 发布和 NATS Consumer 从事件恢复远端 Trace 并建立子 Span，后台任务建立任务 Span，日志和 Prometheus Exemplar 可按 Trace ID 关联 |
| NFR-24 | 已实现 | DOC_CURRENT | GO-UNIT; OBSERVABILITY-RELEASE-GATE | NOT_DEPLOYED | 业务服务独立 live/ready，readiness 只检查安全处理请求所需依赖；Identity 检查 Keycloak，Relay 分别检查 PostgreSQL/NATS，liveness 不因依赖断开失败，且不调用计费/切换准入业务 |
| NFR-25 | 已实现 | DOC_CURRENT | GO-UNIT; OBSERVABILITY-RELEASE-GATE | NOT_DEPLOYED | 独立9090监听输出 HTTP 请求/错误状态/延迟/并发/readiness 与 Relay 周期/结果/积压/死信指标；标签仅使用有界 service/method/route/status/source/outcome，Trace ID 只进入 Exemplar |
| NFR-26 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端UAT仅完成健康烟测，单节点development环境不满足生产等价前置条件；未执行30分钟、规定样本量、≤70%声明容量、三类摘要和四段计时 |
| NFR-27 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 关键读写P95/P99及0.1%错误率门禁已实现；本次没有规定样本量的真实接口压测指标 |
| NFR-28 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | Provider端到端3s/8s和平台附加500ms/1.5s已分离校验；本次未冻结健康Provider和资源档位做分段实测 |
| NFR-29 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 流式派发500ms/1.5s、首内容5s/15s及千样本门禁已建立；本次没有真实模型路由负载报告 |
| NFR-30 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; REQUEST-ADMISSION; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 原子准入和200并发门禁已实现；单节点UAT未执行200流式并发、0.5%错误率、公平性和容量调参验收 |
| NFR-31 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | UAT实测仅0个Hold；未达到十万Hold、200稳态/500突发、锁等待P99和守恒/幂等容量证据规模 |
| NFR-32 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | UAT实测仅4个订单、0个支付流水；未达到百万订单、三百万流水、50下单/200归并和零重复资金效果规模 |
| NFR-33 | 部分实现 | DOC_CURRENT | PERFORMANCE-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | UAT实测NATS为1个流、0条消息；未执行每流百万保留、十万积压15分钟排空、50%新流量和高水位停止写入验收 |
| NFR-34 | 部分实现 | DOC_CURRENT | RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 资金60秒、账号配置300秒门禁已实现；本次只有迁移前同机快照，没有四库连续WAL时效或隔离恢复后的实测RPO |
| NFR-35 | 部分实现 | DOC_CURRENT | RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 核心3600秒、非关键14400秒目标已机器化；本次未执行隔离恢复、故障计时和安全降级能力验收 |
| NFR-36 | 部分实现 | DOC_CURRENT | RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 本次五类迁移前备份的SHA-256与最小可读性检查通过，但仍是同机快照；缺连续WAL、日增量、周全量、TLS/AES-256、独立KMS和异地不可变副本 |
| NFR-37 | 部分实现 | DOC_CURRENT | RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 月度35天、季度100天、年度400天门禁与模板已实现；当前云端UAT没有实际月季年演练、失败整改和复测历史 |
| NFR-38 | 部分实现 | DOC_CURRENT | RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 1095天财务证据、90天普通日志及30天清除SLA已机器化；本次没有归档、法律保全、到期扫描与删除收据 |
| NFR-39 | 已实现 | DOC_CURRENT | GO-UNIT; AUTH-RISK | NOT_DEPLOYED | 公网密码/短信 Direct Grant 与 Refresh 精确路由到 Identity Redis 原子门禁，覆盖来源IP分钟/日、设备、标识失败、已学习稳定用户、Session和Provider；失败采用1～30秒渐进延迟、15分钟锁定和750ms统一耗时桶，成功不清空风险计数，未知/已知账号统一为 `invalid_grant` |
| NFR-40 | 已实现 | DOC_CURRENT | GO-UNIT; AUTH-RISK | NOT_DEPLOYED | 短信发送保留60秒/5次小时限制并增加手机号日、IP小时/日、设备、用途和Provider 80%配额；核验使用独立单向摘要计数，图形挑战一次性消费，所有限流返回按最早窗口释放计算的统一 `retry_after` |
| NFR-41 | 已实现 | DOC_CURRENT | GO-UNIT; REQUEST-ADMISSION | NOT_DEPLOYED | 下单按付款人分钟/日、受益组织小时、来源IP分钟和Provider安全容量原子计数；订单查询限12次/分钟并以客户端5秒与Provider最小间隔中的较慢值合并真实查询；退款按付款人3次/小时，同流水/权益包未终结唯一性由目标退款状态机保证；合法签名回调不受用户限流影响，先持久化再异步幂等归并 |
| NFR-42 | 已实现 | DOC_CURRENT | GO-UNIT; REQUEST-ADMISSION | NOT_DEPLOYED | 新逻辑调用在持久请求和Hold之前按用户60次/分钟与5在途、组织成员5在途、组织600次/分钟与100在途，以及模型、路由、全局速率/并发原子准入；幂等重放先命中原请求而不重复计数，超限返回429和精确Retry-After且不调用上游 |
| NFR-43 | 已实现 | DOC_CURRENT | GO-UNIT; TENANT-ISOLATION-MATRIX | NOT_DEPLOYED | 发布合同矩阵覆盖个人间、组织间、节点父/兄弟/作用域外后代、角色互换、活动上下文、撤权/到期/移除/组织停用/注销、猜测ID、批量混入、组织搜索、旧授权缓存和伪造事件；拒绝路径使用同形NotFound/PermissionDenied，允许查询按组织/成员过滤；当前没有租户公开导出接口，新增时必须先扩展矩阵 |
| NFR-44 | 部分实现 | DOC_CURRENT | GO-UNIT; PROVIDER-CIRCUIT-CLOCK; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 熔断器及支付/模型接入已实现；云端UAT没有支付流水和真实Provider错误样本，未覆盖业务拒绝排除、只读半开探针、指数重开、跨副本一致性与告警投递 |
| NFR-45 | 已实现 | DOC_CURRENT | GO-UNIT; REGISTRATION-SAGA; ACCOUNT-CLOSURE-SAGA; LATE-PAYMENT-REVIEW; PAYMENT-ANOMALY-RECONCILIATION; ROUTE-PROVISIONING-ELIGIBILITY; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | Account、Billing、Token 使用统一冻结时钟和不可变案件/转派/止损证据；注册、资料、注销、支付/退款/拒付、对账、路由 Provisioning、用量及高风险命令修复均接入 P0/P1/P2，生产缺值班链即拒绝启动；任一截止逾期实际转派 owner，P0/P1 止损逾期会停用关联 Provider/商品/路由或暂停高风险命令，而非只通知 |
| NFR-54 | 已实现 | AUDIT_HEAD | GO-UNIT; PAYMENT-PROVIDER-HEALTH | NOT_DEPLOYED | 持久观察按 Billing 接收时间与稳定尝试去重，业务拒绝排除，评估冻结成员/高水位并以策略、控制前版本和成员摘要唯一化。 |
| NFR-55 | 已实现 | AUDIT_HEAD | GO-UNIT; ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 11 类 Billing 管理资源已使用 15 分钟资源 ID 快照、确定性 HMAC cursor、规范化筛选和有界 limit；PR 门禁强制运行专用 PostgreSQL 集成测试并拒绝跳过，生产百万级翻页和并发变更证据仍由候选制品绑定的发布门禁验收，未在本地冒充执行。 |
| NFR-56 | 已实现 | APT-120-DONE; APT-121-DONE | ADMIN-MODEL-TOKEN-REDESIGN; TOKEN-ADMIN-PAGINATION-BASE; TOKEN-ADMIN-QUERY-ADAPTERS | NOT_DEPLOYED | APT-120 已实现 Token 000023 快照/成员/墓碑/创建窗口、签名 cursor、TTL/墓碑、授权失效、容量/水位/限流/清理及三类绝对 deadline 基座，并通过身份 B 第三轮审核；APT-121 已实现九类确定性查询、独立 filter/cursor、owner 权威投影、evidence 三元排序及资源审计权限交集，并通过身份 B 第四轮审核。本地因缺少 `PINVOU_TEST_TOKEN_DATABASE_URL` 未执行 PostgreSQL 重型及九类适配用例，PR 隔离库逐项强制 PASS 且拒绝 SKIP；当前 `NOT_DEPLOYED`。 |
| NFR-46 | 部分实现 | DOC_CURRENT | GO-UNIT; PROVIDER-CIRCUIT-CLOCK; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端UAT宿主机Chrony已同步并可见9源，当前样本偏差约-2.3ms至+2.9ms；应用仍为development Compose，缺三源中位配置、100ms连续证明、250ms/1s注入、readiness/写拒绝与告警投递验收 |
| NFR-47 | 已实现 | DOC_CURRENT | GO-UNIT; CLIENT-COMPATIBILITY-LIFECYCLE | NOT_DEPLOYED | 四服务共用版本清单与响应头，三版合同矩阵、90天支持、60天通知、受审批安全例外及稳定426已闭环；每次正式发布必须更新真实发布日期与淘汰时间 |
| NFR-48 | 已实现 | DOC_CURRENT | GO-UNIT; AUDIT-EXPORT-PROTECTION | NOT_DEPLOYED | 服务端任务要求 `platform.audit.export`、15分钟近期认证、明确范围和原因；默认不可逆主体标识及字段脱敏，未脱敏字段另需最长24小时临时 `platform.audit.pii.export`；执行31天/10万行/500MB、每用户2并发/10每日配额，加密签名水印文件只允许15分钟内单次成功下载；缺生产密钥托管、大规模及真实并发验收 |
| UI-16 | 已实现 | DOC_CURRENT | DESKTOP-39; I18N-RELEASE-GATE | NOT_DEPLOYED | 桌面账号弹层、Web 个人中心和管理后台共用精确中/英/日词条形状，未知服务端错误只显示本地化安全兜底；缺键、空词条、回退、单语硬编码或构建制品缺词条会同时阻断 verify 和客户端发布 |
| UI-17 | 已实现 | AUDIT_HEAD | ACCOUNT-PLATFORM-STATE; UI-LINT; ARCHITECTURE-GUARD | NOT_DEPLOYED | 桌面账号 Popover 已移除目录读取、平台模型状态和选择入口；登录、余额、充值、Web 个人中心和退出登录保持不变 |
| UI-18 | 已实现 | AUDIT_HEAD | ACCOUNT-PLATFORM-STATE; UI-LINT; UI-BUILD; ARCHITECTURE-GUARD | NOT_DEPLOYED | 设置页独立显示服务端品悟平台模型目录、生命周期和不可选状态；选择只写入类型化全局默认并明确标注应用于新会话，本地/第三方列表不再显示平台路由占位模型 |
| UI-19 | 已实现 | AUDIT_HEAD | ACCOUNT-PLATFORM-STATE; MULTIAGENT-REGRESSION; UI-LINT; UI-BUILD; ARCHITECTURE-GUARD | NOT_DEPLOYED | 对话选择器按来源展示当前上下文可用的具体平台目录模型及本地/第三方模型；已有会话直接写类型化会话引用，空白草稿延迟到首条消息创建会话后冻结，均不改全局默认 |
| UI-20 | 已实现 | AUDIT_HEAD | PAYMENT-PRODUCT-CONTROLS; UI-LANGUAGE-COVERAGE | NOT_DEPLOYED | Web 个人中心、组织代购和桌面充值 Popover 均保留暂停商品展示，阻止选择/下单并提供中英日维护提示 |
| UI-21 | 已实现 | AUDIT_HEAD | WEB-IDENTITY-LOGIN; AUTH-RISK; REGISTRATION-SAGA | NOT_DEPLOYED | Portal 未登录态直接进入 OIDC PKCE；Keycloak Pinvou 主题提供密码/短信/注册表单，短信登录和手机注册均强制一次性滑块与 6 位验证码，注册复用密码不透明引用及持久预约 Saga；密码不经过 Portal，真实短信、注册和 Cookie SSO 待部署联调 |
| UI-22 | 已实现 | DOC_CURRENT | ROLE-PERMISSION-REDESIGN; PERMISSION-DRIVEN-UI; UI-LANGUAGE-COVERAGE; UI-LINT; UI-BUILD; ARCHITECTURE-GUARD | NOT_DEPLOYED | 桌面账号弹层、Web 个人中心和平台后台均从 Account 权威授权上下文决定入口、只读态和操作可用性，不按角色名推导权限；个人、平台、组织任一授权版本变化均使缓存失效并刷新，退出登录和注销进度保持可达，服务端权限拒绝仍是最终边界。 |
| UI-23 | 已实现 | AUDIT_HEAD | RECHARGE-PRODUCT-ADMIN; PORTAL-STEP-UP | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 后台按五项商品原子权限展示三语列表、添加、验证、上架、下架、删除和撤销入口；认证过期时保存只读草稿并完成 OIDC step-up，返回后不自动重放写请求。 |
| UI-24 | 已实现 | AUDIT_HEAD | ACCOUNT-PLATFORM-STATE; UI-BUILD; PERSONAL-CENTER-IA | NOT_DEPLOYED | 无合法 section 默认进入额度与用量；`myOrganization` 仅在活动组织及适用读取权限同时存在时可用，退出组织或授权回收后替换为首个可访问 Tab；URL、Tab、内容及浏览器历史保持一致。 |
| UI-25 | 已实现 | AUDIT_HEAD | ROLE-BINDING-LIFECYCLE; UI-BUILD | NOT_DEPLOYED | 普通个人中心不再挂载角色授权组件、加载对应制品或请求本人角色绑定；后台角色定义、授权与撤销界面及服务端权限边界保持不变。 |
| UI-26 | 已实现 | AUDIT_HEAD | ACCOUNT-PLATFORM-STATE; UI-LANGUAGE-COVERAGE; UI-BUILD | NOT_DEPLOYED | 默认按到期时间展示最多五个有效包及安全进度和汇总；主页面采用无整体边框、无圆角、只保留条目分隔线的扁平列表；全量列表包含历史状态，详情弹窗保留组成、来源、订单及退款说明并支持键盘关闭。 |
| UI-27 | 已实现 | AUDIT_HEAD | ACCOUNT-PLATFORM-STATE; UI-LANGUAGE-COVERAGE; UI-BUILD | NOT_DEPLOYED | 全状态充值订单按创建时间稳定倒序展示；本地 `EXPIRED` 及兼容的外部待关闭状态统一显示已超时，关闭请求显示处理中且均不恢复二维码；八列桌面摘要、中宽精简、移动卡片和详情弹窗保持可访问。 |
| UI-28 | 已实现 | DOC_CURRENT | ADMIN-PORTAL-UI-REVIEW; ADMIN-PORTAL-SHELL; ADMIN-PORTAL-PROTOTYPE-CONFORMANCE; ADMIN-PLATFORM-OVERVIEW-UAT-20260901; UI-LANGUAGE-COVERAGE; UI-BUILD; ARCHITECTURE-GUARD | CLOUD_UAT `0.8.0@bb9e8f3b` 2026-09-01 | 后台严格采用 HTML 原型的五组信息架构及页面命名；合法 URL 深链、刷新和前进/后退保持当前页，非法或无权 section 失败关闭到首个可访问页；无真实接口或无精确读权限页面隐藏，不使用假数据。平台总览按身份、资金、模型和系统四个数据源独立加载，修正可用积分与路由健康口径，并提供权限内待办下钻。 |
| UI-29 | 已实现 | DOC_CURRENT | ADMIN-PORTAL-UI-REVIEW; ADMIN-PORTAL-SHELL; ADMIN-PORTAL-PROTOTYPE-CONFORMANCE; ADMIN-IDENTITY-UX; ROLE-AUTHORIZATION-UX-UAT-20260901; UI-LANGUAGE-COVERAGE; UI-BUILD | CLOUD_UAT `0.8.0@d0fe55d2` 2026-09-01 | 用户目录已接入 25 条稳定游标分页，用户名/展示名/精确 UUID 搜索与完整用户编码查询分离；当前账号生命周期不采集或维护邮箱，因此用户目录、详情和搜索已移除邮箱，底层可选字段继续兼容保留。停用确认说明会话失效且当前管理员不可自停用；详情补齐既有账号字段，注册/资料/注销修复统一进入权限隔离的紧凑异常工作台，Portal 已补齐资料修复代理白名单；角色与权限页成为唯一完整定义入口，并提供分类权限清单、引导式授权、授权筛选分页和独立审计深链；用户与组织详情仅保留当前访问摘要，Keycloak 边界保持不变。 |
| UI-30 | 已实现 | DOC_CURRENT | ADMIN-PORTAL-UI-REVIEW; ADMIN-PORTAL-PROTOTYPE-CONFORMANCE; ADMIN-FINANCE-UX; UI-LANGUAGE-COVERAGE; UI-BUILD | CLOUD_UAT `0.8.0@e8c3c929` 2026-08-28 | 计费运营、支付与资金、定价与商品、审批中心成为独立 section，删除旧财务聚合 Tab；各页继续复用现有 Billing API，并按精确读/写权限分别展示，未新增业务能力。 |
| UI-31 | 已实现 | DOC_CURRENT | ADMIN-PORTAL-UI-REVIEW; ADMIN-PORTAL-PROTOTYPE-CONFORMANCE; ADMIN-MODEL-SYSTEM-UX; UI-LANGUAGE-COVERAGE; UI-BUILD | CLOUD_UAT `0.8.0@f6266799` 2026-08-31 | Token Gateway、审计与导出、系统与安全采用原型命名；Keycloak 受控入口迁入系统与安全并与系统状态按 `platform.users.identity.repair`、`platform.system.status.read` 独立显示，只读运行手册不形成第六导航组；公网仍无 Bootstrap、Break-glass、IdP 或服务身份执行入口。 |
| UI-32 | 已实现 | DOC_CURRENT | ADMIN-PORTAL-UI-REVIEW; ADMIN-PORTAL-UI-INTEGRATION; ADMIN-PORTAL-PROTOTYPE-CONFORMANCE; UI-LANGUAGE-COVERAGE; UI-BUILD; ARCHITECTURE-GUARD | CLOUD_UAT `0.8.0@e8c3c929` 2026-08-28 | 总验收验证 HTML 原型定义的五组信息架构、独立 URL、精确权限与不可用能力隐藏，并继续覆盖源码/制品设计 Token、旧视觉禁用、详情抽屉、响应式、触控目标与减弱动画。 |
| UI-33 | 已实现 | DOC_CURRENT | ACCOUNT-PLATFORM-STATE; I18N; UI-LINT; UI-BUILD | NOT_DEPLOYED | 组织目录接入 25 条游标分页、状态筛选和 q/status/cursor URL 恢复；创建表单按需展开，初始所有者通过远程用户搜索选择，不再局限于页面首次加载的 25 个用户。 |
| UI-34 | 已实现 | DOC_CURRENT | ACCOUNT-PLATFORM-STATE; I18N; UI-LINT; UI-BUILD | NOT_DEPLOYED | 组织详情改为主内容区独立页面，以 organization_id 和 organization_tab 恢复目标及概览、成员权限、组织结构、生命周期四分区；支持刷新、分享、前进后退并保留目录查询参数。 |
| UI-35 | 已实现 | DOC_CURRENT | ACCOUNT-PLATFORM-STATE; I18N; UI-LINT; UI-BUILD | NOT_DEPLOYED | 成员区改为紧凑分页表格、远程用户候选与按需权限抽屉；列表不再预渲染全部角色/节点控件，也不展示或搜索邮箱。 |
| UI-36 | 已实现 | DOC_CURRENT | CONTRACT; I18N; UI-LINT; UI-BUILD | NOT_DEPLOYED | ACTIVE 组织不再显示删除；DISABLED 组织通过删除检查界面展示跨域阻断，区分 409/503 与 request_id，并要求精确名称和审计理由后才可提交。 |
| UI-37 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; ADMIN-PORTAL-PROTOTYPE-CONFORMANCE; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 已改为“账户与计费 / 支付与对账 / 定价与商品”，模型定价与支付对账归位，并以 `finance_tab` 保持子页刷新、前进后退和权限降级后的稳定深链。 |
| UI-38 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; APPROVAL-COMMANDS; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 账户与计费已提供钱包选择、积分调账申请、Billing 记录与审批申请串联；审批提交失败可按原幂等键恢复，列表展示审批/执行状态并深链审批中心，财务页不再直接批准或拒绝。 |
| UI-39 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; PAYMENT-PROVIDER-RECOVERY; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 支付与对账已结构化展示 Provider/商品分层控制、版本、原因、触发案件和止损证据；恢复申请消费服务端冻结草稿，按 Provider/商品独立提交并深链审批中心，草稿缺失或未知审批状态时失败关闭。 |
| UI-40 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 财务支付各子页按权限独立加载、缓存、刷新和显示局部失败；筛选快照与结果同仓恢复，支付能力、平台总览及商品目录加载状态互不污染。 |
| UI-41 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 应收、支付订单、晚到支付和支付异常消费既有规范化筛选并将筛选与子页写入 URL；充值商品消费稳定 cursor、保留上一页历史并支持前进后退恢复；无 total 的列表仅显示本页数量与容量说明。 |
| UI-42 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 财务、支付、商品与审批决定已使用结构化操作面板，提供字段说明、校验、目标/版本/幂等事实、确认摘要和三语错误；危险写与刷新分阶段，未知结果锁定为目标事实刷新，相关界面不再使用浏览器 prompt/confirm，普通退款仍受默认关闭的独立产品门禁约束。 |
| UI-43 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 旧 `POST /pricing-versions` 已从 Billing 路由、处理器、仓储、运行时策略、OpenAPI、Portal 代理、客户端和 UI 移除；旧 GET 仅返回已发布事实及服务端明确派生状态，迁移期以三语说明只读展示当前/历史价格，新版生命周期端点保持独立。 |
| UI-44 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; METERING-PRICING-LIFECYCLE; PRICING-LIFECYCLE-UI; UI-LINT; UI-BUILD; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | React 管理界面已闭环计量单位、定价草稿、预览、发布、未来生效和转换；按精确价格空间与 `current_space_etag` 读取权威基线，以模型目录选择器、实时权限复核、不可变发布事实和非幂等未知结果恢复锁失败关闭。 |
| UI-45 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; ACCOUNT-PLATFORM-STATE; FINANCE-FULL-PAGINATION; UI-LINT; UI-BUILD; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | Portal 已消费钱包/账本、应收、订单、退款、调账、Hold、晚到支付、支付异常和支付对账的稳定 cursor 与规范筛选；列表和详情状态写入 URL 并支持返回恢复、显式 410 首屏重启和旧接口降级，不伪造总数。 |
| UI-46 | 已实现 | AUDIT_HEAD | ADMIN-FINANCE-PAYMENT-REDESIGN; FINANCE-OVERVIEW-UI; UI-LINT; UI-BUILD; FINANCE-PAYMENT-UAT-20260902 | 0.8.0@b56b71ae (CLOUD_UAT, 2026-09-02) | 三个财务分区分别消费服务端聚合；定价与商品的两个区块独立加载。指标逐项展示单位、服务端状态口径与点时/UTC 窗口，无等价筛选时明确进入未过滤子页；403 隐藏，失败仅影响本区块且不伪装为 0。请求代次和 AbortController 防止刷新竞态及离页回写，桌面/移动端真实浏览器 smoke 已纳入自动前端门禁。 |
| UI-47 | 已实现 | AUDIT_HEAD | ADMIN-MODEL-TOKEN-REDESIGN; UI-LINT; UI-BUILD | NOT_DEPLOYED | APT-127 已通过身份 B 第三轮独立代码审核。默认关闭的一期 IA gate 开启后提供模型管理、Token 资源、运行与用量三个 canonical section，并按 model/token/usage/pricing 原子权限裁剪；旧 `section=tokens` 规范化到首个有权页面，全部无权时先清状态并以单次 replace 进入无泄露 403。gate off 保留 UI-28/UI-31 的旧 Token Gateway 导航、路由和调用 URL；APT-128 及后续业务能力未提前实现。 |
| UI-48 | 已实现 | AUDIT_HEAD | ADMIN-MODEL-TOKEN-REDESIGN; UI-LINT; UI-BUILD | NOT_DEPLOYED | 九类来源使用独立具名筛选、cursor 和详情 URL 状态；候选 URL、初始进入、权限刷新及 popstate 复用同步归一逻辑，operations 页签切换单次写入 history 并清非活动详情。当前四类一期列表按查询键隔离状态，首帧不显示旧查询行，迟到响应不覆盖新查询，单源失败不改变其他成功来源；410 只重建目标首屏。协议、公式、owner 候选、核对、审计和详情业务界面由 APT-128 及后续任务交付。 |
| UI-49 | 已实现 | APT-128-DONE; APT-122-R2-DONE | ADMIN-MODEL-TOKEN-REDESIGN; UI-LINT; UI-BUILD; OPENAPI; BROWSER-SMOKE | NOT_DEPLOYED | APT-128 原界面交付与 APT-122-R2 通用命令接线均通过身份 B 第五轮审核。四动作不再调用旧专用写；Portal 对权威 precheck、Billing 回执、闭合结果状态、严格 RFC 3339 时间和非敏感稳定码失败关闭，畸形 2xx 固定未知锁定并只按安全 reference GET，正常 PENDING/SUCCEEDED/REJECTED/FAILED/UNKNOWN 语义保持一致。APT-138 仍未激活。 |
| UI-50 | 已实现 | APT-129-DONE; APT-125-R1-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | APT-129 权威候选/稳定选择/健康投影保持；APT-125-R1 获身份 B 最终 PASS，Portal 两路由动作统一通用预检/命令，参数改变自动重预检，冻结完整 route/tombstone 结果校验及 GET reference/key 恢复。Chromium 证明权限矩阵、首次新参数提交、响应丢失和零 v2 直写；APT-138 尚未激活。 |
| UI-51 | 已实现 | APT-130-DONE; APT-130-R1-REMEDIATION-REVIEW-PASS | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | APT-130 已通过身份 B 第三轮独立代码审核。运行与用量原交付提供全宽 Provisioning/用量详情、attempt/evidence 独立分页与权威核对异常队列；Portal 只使用不含 worker、凭据或自由错误文本的 v2 安全详情，旧 v1 保持兼容。重新核对只处理既有证据，reason 仅审计，运行投影只返回受控诊断码；repair 幂等按 actor/command/digest 隔离并兼容 pre-v2 同 actor 重放。APT-130-R1 已通过身份 B 第二轮独立代码审核：闭合 parser、显式 `replay_model_request:false`、精确 target、权威版本/CAS、领域排队、通用结果与审计位于同一 Token 事务，漂移返回 409 且零写；结果按 actor/command/digest/reference 隔离恢复，核对绝不重放模型请求。APT-138 仍未激活。 |
| UI-52 | 已实现 | APT-131-REMEDIATION-REVIEW-PASS | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | APT-124-R1、APT-130-R1、APT-131-R1 已分别通过身份 B 第三轮、第二轮、第二轮审核。同步、发布、生命周期、回滚、协议/公式、Token 状态/路由、Provisioning repair 与 usage reconcile 均使用权威预检和结构化交互；审批分类证明在 gate off 前置阻断 Token 后续动作且不影响可信非 Token 链。冻结快照、逐动作撤权、中止、幂等未知恢复及安全端口 smoke 均通过，APT-134 排程与 APT-138 激活/执行未提前实现。 |
| UI-53 | 已实现 | APT-132-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | APT-132 已通过身份 B 第三轮独立代码审核。默认关闭的独立详情 gate、稳定深链、固定六页签、Token 安全当前详情与不可变历史版本分页、Billing 权威只读价格、Provisioning/资源审计分页均已实现；CHAT/JSON Schema 安全摘要、12 项三语依赖、完整价格覆盖、双域观测时间及历史分页完整错误矩阵通过合同和生产 Chromium 验证。无权/gate-off 零请求；本任务未提前实现 APT-133+，当前 APT-133 已由后续独立提交完成，APT-134+ 与 APT-138 仍保持各自边界。 |
| UI-54 | 已实现 | APT-133-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | APT-133 已通过身份 B 第四轮独立代码审核。Token Account 已下穿概览、凭据、路由、Provisioning、审计五页签，使用稳定 account ID、独立分页/错误/刷新/数据时间及权限/gate 撤销清理；新增/轮换仅经同源固定 `/credential-handoff` 与独立 Credential Origin，现场验证仅返回脱敏结论，撤销仅走 Account 审批。ACK/撤销响应丢失固定只撤销一次并 GET 原 `result_ref`，真实生产 handoff 与 Credential Origin/PostgreSQL/IdP 浏览器链连续五账户通过且零 Secret/ID 泄漏。APT-134+、APT-137 生产等价 TLS/代理探针和 APT-138 激活未提前实现。 |
| UI-55 | 已实现 | APT-134-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | 身份 B 第五轮独立代码审核已通过。发布计划复用 `TOKEN_MODEL_PUBLISH / AUDIT_ONLY` 的真实草稿与 15 字段冻结证据，governed result 与 `SCHEDULED` plan 同事务，到期才执行即时发布等价领域效果；弃用/下架以 13+6 快照穿过 Account 审批全链。替代模型完整服务覆盖、固定全序漂移证据、精确截止、历史保留、按目标取消权限与存在性保护、UTC 整秒指纹及确定性 worker 失效均已闭合。生产 worker 未注册，APT-138 未激活；APT-135/136 未提前实现。 |
| UI-56 | 已实现 | APT-135-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE | NOT_DEPLOYED | APT-135 已通过身份 B 第二轮独立代码审核。模型详情与财务治理入口复用同一 Billing 权威价格生命周期组件，展示当前/未来覆盖、计量单位、继承来源、版本及 ETag，缺价/失败不显示伪 0；编辑继续使用既有 CAS、幂等、生命周期和 `AUDIT_ONLY` 合同。默认关闭的模型编辑子 gate、原子权限、父模型 IA 可见性、在途预览/转换候选/UNKNOWN 恢复中止及迟到响应隔离均通过合同与生产 Chromium 验证。Billing 仍是唯一事实源，两域不宣称原子；APT-136/138 未提前实现。 |
| UI-57 | 已实现 | APT-136-DONE | ADMIN-MODEL-TOKEN-REDESIGN; GO-UNIT; OPENAPI; UI-LINT; UI-BUILD; BROWSER-SMOKE; POSTGRES-CI-GATE | NOT_DEPLOYED | APT-136 已通过身份 B 第三轮独立代码审核。Token 服务端以数据库权威时间聚合 24h/7d/30d 运行趋势，Portal 展示闭合桶、全零无数据、独立错误/重试/数据时间和保留筛选的下穿；目录差异不泄漏 Provider 私有事实。同步命令唯一重观测后在 publication lock 内重算目录基线/差异，并在首个事实写前通过 activation state/epoch 事务 guard，Provider、等量目录或激活漂移均零目录/命令/审计/outbox/result 写。`000028` 仅追加两个具名索引且 down 不删事实；性能 CI 完整保留生产 CHECK，以 1,000,000 用量/100,000 案件的 60k+20k+20k 互斥分布执行冷一暖三 `EXPLAIN (ANALYZE, BUFFERS)`，逐项拒绝 SKIP。Go、Node/OpenAPI、迁移/任务/i18n/架构、ESLint/build 与两套 Chromium smoke 连续五次通过；本地缺少隔离 PostgreSQL URL 的具名用例明确 SKIP。APT-137/138 未提前实现。 |
| UI-58 | 部分实现 | APT-137-IN-PROGRESS | MODEL-TOKEN-RELEASE-TOOLING; ADMIN-MODEL-TOKEN-REDESIGN | NOT_DEPLOYED | G0～G5 已签署，固定双域、十三 gate 计划/配置、受控角色/fence/CAS 包装、安全代理/只读探针、来源/原始工件/旧链接保留验证已交付；G6 持续 CI 接线代码与隔离门禁已验证，最后证据文档复核。真实 DNS/TLS/身份/KMS/NATS/备份及 N/N-1 灰度、SAFE/观察报告尚未闭合，不以代码或 fixture 证明部署。 |
| UI-59 | 已实现 | APT-140-REVIEW-PASS | ADMIN-MODEL-TOKEN-OVERVIEW-REALIGNMENT | NOT_DEPLOYED | APT-140 已拆分三个所属页面概览，并实现同 section URL/history 下钻、权限/授权版本失效、三语和生产制品浏览器回归；首轮整改闭合后获身份 B 第二轮代码审核 PASS，尚未推送、启用或部署。 |
| NFR-49 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | 在线事件/结果/墓碑硬上限和清理 Worker、主体即时删除内部入口、派发正文取得租约即销毁、受限案件迁移及解除后最多30天清理已实现；Account 注销编排复用该入口，备份30天生命周期仍须随 APT-037 做生产环境验收 |
| NFR-50 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; USER-CODE | NOT_DEPLOYED | 独立精确查询接口验证完整编码和 `platform.users.read`，统一隐藏无效/不存在/越权差异，按操作者、来源、设备和目标摘要限流并审计所有结果；列表搜索不按编码模糊匹配 |
| NFR-51 | 已实现 | AUDIT_HEAD | MODEL-CATALOG-SELECTION; ACCOUNT-PLATFORM-STATE; BRIDGE-DOMAIN-PROTOCOL; WEB-BRIDGE-DOMAIN; UI-LANGUAGE-COVERAGE; UI-LINT; UI-BUILD; ARCHITECTURE-GUARD | NOT_DEPLOYED | 账号 Popover 退役入口、设置全局默认、对话会话选择、类型化来源、生命周期和失败关闭执行同一门禁；简中、英文、日文词条及桌面/Web 桥接协议由自动测试阻断回归 |
| NFR-52 | 已实现 | DOC_CURRENT | AUTHORIZATION-COVERAGE-GATE | NOT_DEPLOYED | 统一验证与正式客户端发布门禁自动扫描生产源码；新增路由文件、任务、事件或 CLI 未分类即失败，公开登录/注册、支付回调、健康检查、模拟器等例外必须登记稳定专用协议策略。 |
| NFR-53 | 已实现 | AUDIT_HEAD | PRODUCT-DOCUMENT-BASELINE | 0.8.0@79828dd1 (CLOUD_UAT, 2026-08-26) | 统一 verify 扫描四份基线文档，并以负向合同验证商品标识、价格、权益值和商品数量重新落文档时发布失败。 |

### 0.2 既有目标编号证据索引

以下目标编号已与后文功能矩阵、真实路由、领域实现、迁移和现有测试逐项校准。状态以本节规范证据行为准；后文按领域展开当前边界，不再保留历史占位。

| target_requirement_id | 当前状态 | source_commit | tests | deployment_version | known_gaps |
|---|---|---|---|---|---|
| ID-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 8–128 位可打印 ASCII，至少一个字母和一个数字。 |
| ID-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 前端核对两次密码，后端仍独立校验用户名、手机号和密码。 |
| ID-03 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | 客户端即时预检后，Account 在消费图形挑战且发送短信前再次校验用户名/手机号，并通过不透明引用请求 Identity 权威校验；预检不替代最终唯一约束 |
| ID-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 服务端生成一次性挑战，客户端用覆盖登录框的弹层完成；不是第三方 CAPTCHA。 |
| ID-05 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 同手机号至少间隔 60 秒，最多 5 次/小时；前端显示倒计时。供应商明确发送失败不占冷却。 |
| ID-06 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 5 分钟有效，最多 5 次核验，成功后不可复用。 |
| ID-07 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Account 只保存 Provider 挑战标识和状态元数据。 |
| ID-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | `SMSProvider` 由配置/依赖注入选择；阿里云托管模式由厂商生成核验，通用 HTTP 模式由平台安全生成并只交供应商发送；挑战保存 Provider ID、Adapter 版本和不透明校验数据 |
| ID-09 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 桌面端通过 Keycloak Direct Access Grant；不持久化密码。 |
| ID-10 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Keycloak 自定义 Direct Grant 调用 Account 内部核验接口；仅中国大陆手机号。 |
| ID-11 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | 已知/未知手机号都创建挑战并在同一最小耗时桶返回 `202`；已知账号异步投递、未知账号不发送，供应商故障不改变外部响应；手机号、来源和设备摘要共同限流并审计 |
| ID-12 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Refresh Token 仅存系统凭据库，启动时刷新。 |
| ID-13 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 失败路径删除持久化 Refresh Token，不回退历史缓存。 |
| ID-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; REGISTRATION-SAGA; DESKTOP-39 | NOT_DEPLOYED | 注册阶段持久恢复，先按操作关联查询 Identity 再幂等创建；Account 不即时删除已验证身份，成功提交即返回可登录的 `PROVISIONING` |
| ID-16 | 非当前范围 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Keycloak 具备扩展基础，但当前产品闭环未实现；短信找回密码已纳入 ACC-09。 |
| ACC-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | Account 仅依赖服务鉴权的 Identity HTTP 端口及不透明密码引用；Keycloak Token、管理 URL、客户端密钥、用户创建/删除协议均只存在于独立 Identity Adapter |
| ACC-05 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; COMMERCE-MAPPING | NOT_DEPLOYED | 注册和手机号变更异步创建供应商中立 Mapping/版本化同步任务；失败退避或进入修复状态，且只影响依赖 Mapping 的支付 |
| ACC-06 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PROFILE-SAGA; PERSONAL-CENTER-IA | NOT_DEPLOYED | 持久变更请求预约规范化新值并保存独立资料版本，按 Identity 更新、Account CAS、会话撤销顺序向前收敛；Web 个人中心接入该 Saga，桌面客户端不封装资料变更。 |
| ACC-07 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PROFILE-SAGA; PERSONAL-CENTER-IA | NOT_DEPLOYED | Web Portal 通过 `prompt=login`/`max_age=0` 强制重新认证，规范回到根路径用户名修改弹窗，再执行用户名预约、唯一性门禁、CAS 提交和会话撤销；Portal/Account 不接触密码，桌面无用户名修改入口。 |
| ACC-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PROFILE-SAGA; COMMERCE-MAPPING; PERSONAL-CENTER-IA | NOT_DEPLOYED | Web Portal 强制重新认证后规范回到根路径手机号修改弹窗，再提供新手机号滑块/短信验证、资料提交和 Commerce Mapping 版本同步；Portal/Account 不接触密码，桌面无手机号修改入口。 |
| ACC-09 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PROFILE-SAGA; IDENTITY-SMS | NOT_DEPLOYED | 桌面未登录 Popover 保留忘记密码；反枚举短信、短期密码引用、Keycloak 重置和全会话撤销完整接通。 |
| ACC-10 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-SAGA; UI-LINT | NOT_DEPLOYED | 最近5分钟认证、精确确认和幂等键可创建唯一注销请求；所有权/最后有效超级管理员在静默前阻断，进入静默后由账务和 Token 强一致评估推进到自动清理边界，后续资料及标识清理由 ACC-46–ACC-50 独立约束。 |
| ACC-11 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | Identity 删除确认后随机覆盖用户、资料请求、注册/短信和邀请直接标识，永久保留不可变 user_id/user_code；分类保留记录固定三年期限、法律保全例外及到期自动 PURGED 状态。 |
| ACC-12 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 成员关系独立存储；个人中心只显示当前用户关联组织。 |
| ACC-13 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39; RBAC-AUTH | NOT_DEPLOYED | Account 持久化平台/组织角色、原子权限、组织/节点作用域和授权版本；服务端默认拒绝并按接口所需权限判定，客户端显示不作为授权依据 |
| ACC-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 顶层组织创建位于平台管理 API/UI。 |
| ACC-19 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-LIFECYCLE | NOT_DEPLOYED | Account 核验停用状态、邀请和审批治理任务；Token 核验未终态用量；Billing 在串行事务中核验余额/预留、有效权益、Hold、支付、退款、对账差异、用量复核、财务调整及可扩展治理阻断登记，任一未收敛即返回结构化原因并保持停用 |
| ACC-22 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 管理 UI 提供启动编辑动作，避免误改。 |
| ACC-26 | 已实现 | DOC_CURRENT | GO-UNIT; MEMBERSHIP-GOVERNANCE | NOT_DEPLOYED | PHONE、USERNAME、LINK 使用统一持久实体并保留 EMAIL 兼容入口；目标只存摘要/提示，创建不探测用户存在，本人列表只按认证身份反向恢复；Token 或目标邀请 ID 均一次消费，接受时再次绑定当前稳定用户 ID，过期/撤销/接受历史永久保留 |
| ACC-29 | 已实现 | DOC_CURRENT | GO-UNIT; MEMBERSHIP-GOVERNANCE; RBAC-AUTH | NOT_DEPLOYED | 用户入口强制 `org.owner.transfer`、平台入口强制专用平台权限；稳定命令指纹串行撤销旧 OWNER 并保留历史、建立新 OWNER、刷新双方版本/事件，延迟数据库约束保证每个有效组织提交时恰有一个当前所有者 |
| ACC-30 | 已实现 | DOC_CURRENT | GO-UNIT; ROLE-BINDING-LIFECYCLE | NOT_DEPLOYED | 本人接口只按已认证主体返回自己的平台/组织绑定；组织与运营视图展示角色、作用域、起止时间、授权来源和生命周期状态，响应禁止缓存 |
| ACC-33 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; PERSONAL-CENTER-IA | NOT_DEPLOYED | 无有效组织成员关系时，“账号设置”不渲染组织管理区；Web 不再设置独立组织一级 Tab。 |
| ACC-34 | 已实现 | DOC_CURRENT | GO-UNIT; GLOBAL-CONTEXT | NOT_DEPLOYED | `user_active_contexts` 以 user_id 唯一保存 PERSONAL/ORGANIZATION 与单调 context_version；切换以期望版本串行 CAS，最近有效 Session 仅用于迁移初值，所有端读取同一聚合 |
| TOK-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 用户在桌面直接配置的本地模型或第三方云模型由本地模型系统管理，不携带账号平台身份、不使用平台钱包和 Token Gateway。 |
| TOK-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 启用账号平台后，Rust 运行时以当前 Access Token 调用 Gateway。 |
| TOK-03 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | `POST /v1/chat/completions` 支持 JSON 和 SSE；桌面原生流式路径使用该入口。 |
| TOK-05 | 已实现 | DOC_CURRENT | GO-UNIT; GATEWAY-ADAPTER-NEUTRALITY | NOT_DEPLOYED | 请求构造、响应头引用提取和 New API 日志/用量归一化封装于版本化 Adapter；通用 Handler 与核对器只消费协议能力和 `UsageEvidenceProvider` |
| TOK-06 | 已实现 | DOC_CURRENT | GO-UNIT; MODEL-CATALOG-LIFECYCLE; DESKTOP-CLIENT-SWITCH | NOT_DEPLOYED | Gateway 通过 Adapter 同步独立 `CATALOG_VISIBLE` 目录，桌面实时消费当前目录并按上下文资格和生命周期过滤，不把请求级预占写回目录 |
| TOK-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 平台托管、幂等创建，普通用户不可见不可改。 |
| TOK-11 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Gateway 从 Account 获取服务端可信上下文，不接受客户端伪造 payer。 |
| TOK-13 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 按作用域、默认级别和优先级选择，歧义拒绝。 |
| TOK-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | API 只返回类型、状态、指纹、更新时间等元数据。 |
| TOK-15 | 移除 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 不提供个人供应商 Key、Token Account、共享和路由管理；现有相关个人入口及公开能力从目标范围移除。平台/组织运营管理与本地独立模型配置不受此条影响。 |
| TOK-19 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; METERING-PRICING-LIFECYCLE; DESKTOP-39 | NOT_DEPLOYED | 请求使用版本化 UTF8_BYTES_DIV4_CEIL 估算器、协议公式和模型输出边界动态预占；Provider 专用精确 Tokenizer 可作为后续估算器版本扩展 |
| TOK-22 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 每请求独立 Hold；可用余额能够覆盖即可并发。 |
| TOK-24 | 已实现 | DOC_CURRENT | USAGE-EXECUTION-REPLAY | NOT_DEPLOYED | 相同键和指纹恢复既有加密流事件、完整/聚合结果或当前执行/核对状态；重复路径不续租新 Hold、不重新选路且不发起第二次上游调用 |
| TOK-31 | 已实现 | DOC_CURRENT | GO-UNIT; CONTRACT-36; DESKTOP-39; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | 正常完成、失败和取消均只按 Adapter 归一化的可信 Provider 最终或累计用量结算；缺少可信用量保持 USAGE_PENDING 并走证据核对，不按 Hold 或客户端交付估算 |
| TOK-32 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Gateway 对上游结果不确定或网络失败不自动重发模型请求，避免重复消费 |
| TOK-33 | 已实现 | DOC_CURRENT | GO-UNIT; CONTRACT-36; DESKTOP-39; USAGE-CANCELLATION-CONVERGENCE | NOT_DEPLOYED | REVIEW/SETTLEMENT_PENDING 按历史 Provider/Adapter/上游引用退避查询证据；人工重试幂等且不重发模型调用，期限后释放未结算 Hold 并进入终态复核 |
| TOK-34 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 提供个人用量请求列表和详情，不暴露内部凭据。 |
| BILL-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 钱包按 Owner 类型和 ID 唯一；组织请求不能隐式扣个人钱包。 |
| BILL-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Journal Entry 与 Posting 平衡，不能直接覆盖余额。 |
| BILL-03 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | `10,000 microcredits = 1 展示积分`；金额使用分。 |
| BILL-06 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 充值按支付订单和冻结商品快照创建独立权益包及 BASE/BONUS 组成，保存起止时间、原始额和各状态桶；支付流水细化由 PAY-11～PAY-14 继续扩展 |
| BILL-07 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 购买命令明确保存付款申请人和受益组织；履约再次校验组织钱包归属并只向该钱包及组织权益包入账，个人钱包不参与。 |
| BILL-08 | 已实现 | DOC_CURRENT | GO-UNIT; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 只有具有组织原子权限的管理员可从组织有效权益的精确组成授予成员；无成员自领接口，授予、撤回、暂停/恢复、Hold、释放和消费均保留不可变审计。 |
| BILL-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 权益 API 按组成返回总额、可用、预占、已用、过期、撤销和到期时间，并提供总可用及 BASE/BONUS 汇总 |
| BILL-15 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Hold 分配与结算关联具体权益包。 |
| BILL-17 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 每次正向人工调整创建独立 BONUS 权益包和组成，自入账起固定 30 天到期 |
| BILL-18 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 到期 Worker 锁定到期包，先收敛相关 Hold，再按组成写平衡到期分录并保留包及组件历史 |
| BILL-19 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 到期事务释放涉及该组成的不确定 Hold 且不恢复过期额度；迟到确认写入独立平台损失复核，不扣其他权益或形成负余额 |
| BILL-21 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前与历史价格并存，结算/核对使用对应时点价格。 |
| BILL-22 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; METERING-PRICING-LIFECYCLE | NOT_DEPLOYED | 全局默认和模型覆盖均支持立即或未来时间区间，发布拒绝重叠并可预览请求时间的逐单位结果。 |
| BILL-26 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 申请、批准、拒绝和账本入账可审计。 |
| BILL-27 | 已实现 | DOC_CURRENT | CODE-AUDIT; GO-UNIT; HOLD-OPERATIONS-EVIDENCE | NOT_DEPLOYED | 受权运营查询覆盖钱包投影、Journal/Posting、账本科目、lot/权益包/BASE-BONUS 组成来源、Hold、调整、应收和对账；接口只读且不提供余额覆盖或未受审计导出 |
| BILL-28 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | Provider 确认后同一事务撤销目标支付流水权益包全部 BASE 与关联 BONUS、减少来源 Credit Lot 和钱包负债并写平衡退款分录；BONUS 不计现金 |
| PAY-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PAYMENT-PROVIDER-BOUNDARY | NOT_DEPLOYED | Billing 只依赖供应商中立 `payment.Provider`；H3C 状态映射和开发 Simulator HTTP 协议分别位于独立平台 Adapter |
| PAY-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PAYMENT-PROVIDER-BOUNDARY | NOT_DEPLOYED | 版本化能力声明覆盖渠道、查询/保留/间隔、支付与晚到窗口、权威终态、关闭、退款、通知、账单和修复；API、UI、后台轮询与领域动作统一消费该声明，订单冻结能力快照 |
| PAY-03 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PAYMENT-PROVIDER-BOUNDARY | NOT_DEPLOYED | 可售渠道取通用商品、Provider 映射及当前能力交集；测试 Provider 标记 `test_only`，生产启动拒绝且 Helm 不部署，不能扩张正式能力 |
| PAY-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PAYMENT-PROVIDER-BOUNDARY | NOT_DEPLOYED | 通用商品不再拥有外部 SKU；独立不可变映射保存 Provider/版本、外部 SKU、金额、币种、渠道、状态和有效期，订单冻结映射 ID、映射与能力快照 |
| PAY-05 | 未实现 | AUDIT_HEAD | CODE-AUDIT; PAYMENT-PROVIDER-BOUNDARY | NOT_DEPLOYED | 目录与映射已版本化，APT-048 尚须通过后台目录完成真实 Provider 的正式准入及支付/履约联合验收；具体商品配置不在本文记录。 |
| PAY-06 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; COMMERCE-MAPPING | NOT_DEPLOYED | 个人充值从独立 Commerce Mapping 端点读取当前 Provider 的 READY 状态；Mapping 不进入认证/授权上下文，未完成只拒绝依赖它的下单 |
| PAY-07 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 充值选择、待支付列表、二维码弹窗和结果留在品悟应用/Portal 内 |
| PAY-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; ACCOUNT-PLATFORM-STATE | NOT_DEPLOYED | Web 不自动恢复旧二维码；创建订单或用户主动点击仍可付款订单时打开二维码弹窗，已支付、超时及其他不可付款订单不进入待支付列表 |
| PAY-09 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 服务端按 `product_id` 派生 SKU、金额和权益，拒绝客户端覆盖 |
| PAY-10 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PAYMENT-CALLBACK-GATEWAY | NOT_DEPLOYED | Provider 二维码、外部订单引用和金额均读取查询/回调事实，成功支付必须取得并精确校验实付金额；真实支付、商品/发票与上架仍需环境联合验收。 |
| PAY-15 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE; PAYMENT-CALLBACK-GATEWAY; ACCOUNT-DOMAIN-UAT-20260825 | 0.8.0@1efb79d6 (CLOUD_UAT) | 通用回调凭据与 Token 绑定 Provider、版本和商户，Billing 按 Profile 严格解析并先持久化再返回 `202`，与主动查询共用归并器；H3C 是首个 Profile 且固定声明 `notification_supported=true`。`https://account.pinvou.com/api/v1` 下两个通用精确路径已发布并完成受控客户端 Token 签发；真实新华三通知报文、重复/乱序与履约结果仍待联合验收 |
| PAY-16 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Billing 后台查询不依赖客户端二维码页面持续打开 |
| PAY-17 | 已实现 | DOC_CURRENT | GO-UNIT; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 普通 PENDING 在订单期限后继续按退避查询，72小时最终查询仍无终态则转换为明确人工审核，不会永久停留普通 PENDING |
| PAY-18 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-CONVERGENCE; LATE-PAYMENT-REVIEW | NOT_DEPLOYED | 冻结最后付款期限到达后先末次查询，仍未付款即提交本地 `EXPIRED`、清除二维码并退出待支付集合；商城关单为可选外部证据步骤，不影响本地终态，正常未付款不计入异常止损 |
| PAY-21 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE | NOT_DEPLOYED | 每笔确认成功支付流水至多幂等创建一个独立权益包和一组平衡账本入账；重复回调、查询或修复不重复增加额度 |
| PAY-22 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-EVIDENCE-LIFECYCLE; ORGANIZATION-MEMBER-QUOTA | NOT_DEPLOYED | 具备 `org.entitlements.purchase` 权限的管理员可在线购买组织权益；订单冻结付款用户和受益组织，到账只进入受益组织钱包 |
| PAY-30 | 已实现 | DOC_CURRENT | GO-UNIT; TARGETED-REFUND | NOT_DEPLOYED | Provider 成功证据校验后同一事务消费退款预约、撤销目标全部 BASE/关联 BONUS、更新权益/退款终态并写平衡账本；其他来源不受影响 |
| PAY-33 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | 拒付、渠道强制退款、重复支付、晚到冲突、支付事实冲突、退款失败和非法迁移进入统一案件；领域事实保持独立，证据快照和事件不可变，责任人、结论及财务动作状态可审计；确认拒付/强制退款仅定向原流水权益包，BASE 不足形成应收。 |
| PAY-34 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | 按上海自然日由 Worker 每日获取 Provider 账单，并核对内部订单/支付流水、普通及强制退款/拒付和 Billing 账本；API、文件导入及手工重跑均保留独立运行。 |
| PAY-36 | 已实现 | DOC_CURRENT | GO-UNIT; PAYMENT-ANOMALY-RECONCILIATION | NOT_DEPLOYED | 差异冻结分类、严重度及 Provider/内部/退款/账本快照，状态、责任人、修复动作和复核结果只经带请求指纹的幂等命令推进，命令与时间线不可变且禁止直接改写证据。 |
| UI-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 不跳外部浏览器；个人信息也在弹层中展示。 |
| UI-03 | 已实现 | DOC_CURRENT | PERSONAL-CENTER-IA; GLOBAL-CONTEXT | NOT_DEPLOYED | Web 一级导航新增按活动组织及精确读取权限显示的“我的组织”；额度页只保留当前身份余额、只读权益与本人用量，账号设置只保留个人资料、身份切换和注销，组织购买、额度、成员、节点和模型策略集中迁入独立页面；原有精度、全状态订单、紧凑账号资料和旧深链规则保持不变。 |
| UI-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 模型准备、内部结算状态、供应商凭据等从普通用户视图移除。 |
| UI-05 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PERSONAL-CENTER-IA; IDENTITY-SMS; I18N-RELEASE-GATE | NOT_DEPLOYED | Web 个人中心的用户名/手机号修改通过 Identity 托管页近期认证，Portal/Account 不接收密码；所有发起端统一使用根路径回到对应修改弹窗，已删除的 `/account` 页面地址不再生成，桌面移除资料修改区域但保留未登录忘记密码。 |
| UI-06 | 已实现 | DOC_CURRENT | GLOBAL-CONTEXT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 个人中心所有余额、权益、订单和用量请求只按用户级活动上下文加载；个人上下文展示个人权益，组织上下文不混入个人权益明细。 |
| UI-07 | 已实现 | DOC_CURRENT | GLOBAL-CONTEXT | NOT_DEPLOYED | 桌面与 Web 共用服务端活动上下文；客户端只接受递增版本，收到更高版本、窗口聚焦或切换成功即中止旧请求、清空上下文资源并重新读取。 |
| UI-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; ENTITLEMENT-COMPONENTS | NOT_DEPLOYED | 桌面与 Portal 三语展示总可用、BASE/BONUS、来源、各组成余额、到期提醒及确定消费顺序，并明确 BONUS 零现金退款和普通退款关联撤销规则 |
| UI-09 | 已实现 | DOC_CURRENT | DESKTOP-CLIENT-SWITCH; ACCOUNT-DOMAIN-UAT-20260825 | NOT_DEPLOYED | 登录后的账号 Popover 提供三语 Web 个人中心入口；专用 Tauri 命令只打开配置的 HTTPS/loopback Portal 根地址，不接受 WebView URL 或在 URL 中携带 Token。`https://account.pinvou.com/` 已发布，但桌面签名制品安装仍待验收 |
| UI-10 | 已实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36; ACCOUNT-PLATFORM-STATE; UI-BUILD | NOT_DEPLOYED | Web 个人中心以模态弹窗展示新建或主动恢复订单的二维码，套餐区只列出 `CREATED/PENDING + OPEN + 未超期` 订单并在状态失效时立即移除。 |
| UI-11 | 已实现 | DOC_CURRENT | DESKTOP-39; ORGANIZATION-MEMBER-QUOTA; PERSONAL-CENTER-IA | NOT_DEPLOYED | Web 切换到目标组织后，仅在具备适用读取权限时显示“我的组织”，并按原子权限分别提供购买、额度分配/撤回、成员、节点和模型策略；额度与用量、账号设置不再承载组织管理动作。 |
| UI-12 | 已实现 | AUDIT_HEAD | TARGETED-REFUND; REFUND-PRODUCT-GATE; DESKTOP-39 | NOT_DEPLOYED | 用户端不含新建普通退款入口；运营端显式同时消费 `normal_refund_creation_enabled` 与 `refund_supported`，Provider capability 单独变化不能解除产品屏蔽 |
| ADM-01 | 部分实现 | DOC_CURRENT | GO-UNIT; SECURITY-RECOVERY; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端UAT运行库实测为0个有效超级管理员且无待激活用户；离线安全引导代码已部署，但正式密码必须由用户控制的受控终端设置，尚未执行激活、并发兑换、Keycloak/数据库故障补偿和告警投递验收 |
| ADM-02 | 部分实现 | DOC_CURRENT | GO-UNIT; SECURITY-RECOVERY; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 运行库实测为0条Break-glass操作和0条安全告警；首次初始化不得伪装成管理员失联事故，需先正常激活管理员，再验收事故授权、CRITICAL告警接收、5–60分钟到期与正常交接 |
| ADM-03 | 部分实现 | DOC_CURRENT | GO-UNIT; WORKLOAD-IDENTITY; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端UAT已验证真实OIDC discovery/JWKS及三个独立Relay容器；内部HTTP仍共用一个静态令牌，缺逐工作负载投射身份、轮换/撤销和跨身份故障注入证据 |
| ADM-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 支持列表、搜索、详情和生命周期操作。 |
| ADM-05 | 已实现 | DOC_CURRENT | GO-UNIT; RBAC-AUTH; ROLE-BINDING-LIFECYCLE | NOT_DEPLOYED | 运营端可查看角色定义和全部绑定，按用户/组织筛选，并对普通平台/组织/节点角色执行带有效期的幂等授予和单条撤销；SUPER_ADMIN 与 OWNER 保留专用安全流程 |
| ADM-06 | 已实现 | DOC_CURRENT | GO-UNIT; ROLE-BINDING-LIFECYCLE | NOT_DEPLOYED | Account worker 扫描组织写模型及直接平台授权，到期时原子失效绑定、更新授权/上下文缓存版本并写审计和 Outbox；鉴权查询在 worker 延迟时也按可信数据库时间即时拒绝过期授权 |
| ADM-07 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS | NOT_DEPLOYED | 角色可兼任且不同用户可交叉审批；服务端按稳定用户 ID 禁止自审，所有强制审批命令固定只需一名独立审批人。 |
| ADM-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 不嵌入普通后台；通过受限网络/隧道访问。 |
| ADM-09 | 已实现 | DOC_CURRENT | GO-UNIT; AUDIT-EXPORT-PROTECTION; DESKTOP-39 | NOT_DEPLOYED | 筛选和详情保留只读查询；导出改为服务端异步任务，浏览器不再拼接当前结果，权限、近期认证、脱敏/临时PII授权、水印、加密、配额和一次性下载均按 NFR-48 执行。 |
| ADM-10 | 已实现 | DOC_CURRENT | CONFIGURED-SYSTEM-PROBES | NOT_DEPLOYED | `SYSTEM_PROBES_JSON` 配置实际部署组件、通用 HTTP/JSON 断言或 Relay Adapter；页面继续展示健康、积压、核对任务和最近错误，响应不暴露端点或凭据 |
| NFR-01 | 部分实现 | DOC_CURRENT | PRODUCTION-SECURITY-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端 UAT 的 Portal、Account/Identity 边界和 OIDC discovery 已通过 HTTPS、受信证书及不少于14天有效期验收；当前仍非 production-new，缺正式入口续期和生产等价发布门禁证据 |
| NFR-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Compose/Helm/反向代理模板将数据库、Redis、NATS、Relay、内部 API、New API 管理面和 Keycloak Admin 保持非公开；部署证据仍为 NOT_DEPLOYED |
| NFR-03 | 部分实现 | DOC_CURRENT | GO-UNIT; PRODUCTION-SECURITY-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | production-new 的 ExternalSecret、KMS/Vault 双读重加密和 readiness 门禁已实现；云端 UAT 实测仍为本地 KEK，缺真实 SecretStore、新 Key 加解密、旧密文归零及旧 Key 停用证据 |
| NFR-05 | 已实现 | DOC_CURRENT | GO-UNIT; APPROVAL-COMMANDS; AUDIT-IDEMPOTENCY-EVENTS | NOT_DEPLOYED | 当前管理、资金、授权、Token 路由、Break-glass、Saga 修复及死信重放均保存不可变或追加式审计；后台身份不能伪装成人类。 |
| NFR-06 | 已实现 | DOC_CURRENT | GO-UNIT; AUDIT-IDEMPOTENCY-EVENTS | NOT_DEPLOYED | 注册、资料、钱包、路由、支付、退款、模型请求、结算、事件消费及人工重放均使用稳定命名空间、请求指纹或事件 ID；同键异请求失败关闭。 |
| NFR-07 | 已实现 | DOC_CURRENT | GO-UNIT; AUDIT-IDEMPOTENCY-EVENTS | NOT_DEPLOYED | 三服务使用事务 Outbox/Inbox、版本间隙门禁、至少一次 Relay 和死信；人工重放校验不可变摘要并原子登记审计，不使用分布式事务。 |
| NFR-08 | 已实现 | DOC_CURRENT | GO-UNIT; GLOBAL-CONTEXT | NOT_DEPLOYED | 用户级 context_version 数据库单调保护，切换要求 expected_context_version；响应头跨 Account、Token、Portal 与桌面桥接传播，客户端只接受更高版本并刷新缓存 |
| NFR-09 | 部分实现 | DOC_CURRENT | GO-UNIT; RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端UAT已在迁移前创建并校验五类快照，按Account、Billing、Token顺序迁移并复核运行库版本32/34/21；仍缺production-new快照凭证和生产迁移摘要 |
| NFR-10 | 部分实现 | DOC_CURRENT | GO-UNIT; RECOVERY-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 独立版本目录、稳定链接和实际备份已运行；本次未从备份恢复到隔离环境，也没有周期演练报告 |
| NFR-11 | 部分实现 | DOC_CURRENT | HA-RELEASE-GATE; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 云端UAT实测为单Docker节点，核心服务、PostgreSQL、Redis、NATS和Keycloak均为单实例；未执行节点/区域/依赖主节点切换，缺真实多故障域容量与无损故障报告 |
| NFR-12 | 已实现 | DOC_CURRENT | GO-UNIT; WORKLOAD-IDENTITY; HUMAN-DELEGATION-SIGNING; CLOUD-UAT-20260824 | 0.8.0@9271c307 (CLOUD_UAT) | 仓库已将 Account、Identity、Billing、Token 和开发模拟器的内部路由逐条绑定精确 `internal.*` 权限，Helm 只向各 Subject 分配显式权限清单且额度写路径消费签名委托；代码目标完成，运行环境仍共用静态 HTTP 令牌，NATS 无 Operator 凭据，投射令牌与委托密钥轮换/撤销演练由 APT-047 收敛。 |
| NFR-13 | 已实现 | DOC_CURRENT | GO-UNIT; ACCOUNT-CLOSURE-CLEANUP | NOT_DEPLOYED | 注销在 Identity 不可认证证明后立即随机去标识化直接资料并释放用户名/手机号，法律记录只按永久内部主体引用保留；三年分类记录支持法律保全并由 worker 到期标记清除完成。 |
| NFR-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; IDENTITY-SMS | NOT_DEPLOYED | 启动工厂按配置注入 `aliyun-managed` 或 `http-generated`；核心挑战状态机没有厂商条件，历史挑战保存 Provider ID 与 Adapter 版本 |
| NFR-15 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39; RBAC-AUTH | NOT_DEPLOYED | OIDC 仅证明 issuer、subject 和会话；Account 是平台/组织业务授权事实源，三业务服务均不再读取 Realm Role 决定后台权限 |
| NFR-16 | 已实现 | DOC_CURRENT | GO-UNIT; COMMERCE-MAPPING; PAYMENT-PROVIDER-BOUNDARY; PAYMENT-EVIDENCE-LIFECYCLE | NOT_DEPLOYED | Commerce Mapping、商品映射、订单/流水/证据及公开 API 使用供应商中立字段并冻结 Provider/Adapter 版本；厂商 SKU、签名、状态和协议仅存在于 Adapter |
| NFR-17 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; PAYMENT-PROVIDER-BOUNDARY; REFUND-PRODUCT-GATE; DESKTOP-39 | NOT_DEPLOYED | `/v1/payment-capabilities` 暴露版本化中立 Provider 能力；商品渠道、查询 Worker、终态、关单、通知、账单和修复按能力降级，普通退款新建另受独立产品门禁约束，订单保存 Provider 能力快照 |
| NFR-18 | 已实现 | DOC_CURRENT | GO-UNIT; GATEWAY-ADAPTER-NEUTRALITY | NOT_DEPLOYED | 通用数据库、API 和三语 UI 只保存 Provider ID、Adapter 版本、上游引用及摘要；New API 专属请求头、日志接口和字段留在 Adapter，并可配置切换通用 OpenAI 兼容 Adapter |
| NFR-19 | 已实现 | DOC_CURRENT | CONFIGURED-SYSTEM-PROBES | NOT_DEPLOYED | 聚合器只迭代配置的探针注册表；通用 HTTP、可配置 JSON 布尔断言和 Relay 指标解析按 Adapter 类型分派，新增或替换组件无需修改 Portal 主流程 |
| NFR-20 | 部分实现 | DOC_CURRENT | CLIENT-SWITCH-READINESS; DESKTOP-CLIENT-SWITCH | NOT_DEPLOYED | 旧 `llmapi_hub` 运行时已删除，幂等迁移、非计费 readiness 和三层失败关闭门禁已有仓库实现；正式签名制品的安装原子性、回滚及生产联调仍待外部验收 |

## 1. 产品目标

账号平台需要让个人用户以统一身份在品悟桌面端完成注册、登录、自动登录、模型对话、用量查看和充值；让组织用户在显式组织上下文中共享授权资源和组织钱包；让平台管理员能够管理账号、组织、角色、钱包、定价、支付异常、Token 路由、用量核对、审计和服务状态。

系统必须满足五个核心不变量：

1. 当前配置的 Identity Provider 是认证、密码和登录会话的唯一事实源；Account Service 是业务用户、用户名/手机号映射、组织关系和权限的唯一事实源；Commerce Provider Mapping 只维护外部商城客户映射，不参与通用认证和授权。
2. 余额只有 Billing 的不可变平衡账本一个资金事实源。
3. 模型供应商密钥只在 Token Gateway 的受控边界内解密。
4. 外部支付、模型网关或网络结果不确定时进入可核对状态，不猜测成功，也不重复执行高风险请求。
5. 任何敏感操作都由权威服务端按稳定人类权限或精确工作负载/公开协议策略默认拒绝；角色名、已登录状态、活动计费上下文和请求体自报身份都不能替代授权。

### 1.1 已确认的目标变更

以下需求于 2026-08-22 确认，是后续实现和验收的新基线。状态描述的是当前代码与目标之间的差距，不能把“需求已确认”误写成“功能已实现”。

| 变更编号 | target_requirement_id | 已确认需求 | 当前实现判断 | 目标验收摘要 |
|---|---|---|---|---|
| CR-01 | BILL-08–BILL-13 | 组织额度由企业管理员手动分配给成员，员工不可自主领取 | 已实现 | 组织钱包为总额度池；分配只形成绑定原包原组成的组织内成员额度，不转入个人钱包；组织调用同时占用成员授予和组织钱包组成余额 |
| CR-02 | BILL-21–BILL-25 | 系统管理后台设置模型价格，支持统一价格或按时间段分段计价 | 已实现 | 已支持全局默认、模型覆盖、未来生效、显式继承、冲突预览、完整生命周期和 Hold 价格快照 |
| CR-03 | TOK-19, TOK-21, TOK-23, BILL-30 | 预占按照具体模型上下文动态计算 | 已实现 | 当前 CHAT 协议按请求输入估算、请求/模型输出边界、公式和价格快照计算，并支持续租及追加预占 |
| CR-04 | ACC-34, UI-06 | 只能使用当前活动个人/组织上下文授予的额度，个人中心只展示该上下文额度 | 部分实现 | Gateway 已按活动上下文找钱包；需让所有额度/权益 API 和 UI 严格按活动上下文返回，组织场景只显示该成员被授予额度 |
| CR-05 | ACC-06–ACC-09, UI-05 | Web 个人中心修改用户名/手机号，桌面保留忘记密码 | 已实现 | Portal 通过 OIDC 强制重新认证后调用 Account Saga；桌面仅保留未登录密码恢复，不封装资料修改。 |
| CR-06 | ACC-19 | 组织删除为逻辑删除；有余额、有效权益包或未结算 Hold 时禁止删除 | 已实现 | 组织必须先停用并冻结钱包；Account/Billing 预检任一阻断事实存在即返回结构化冲突，清零后仅写 DELETED、关闭钱包且保留账本、订单和审计 |
| CR-07 | TOK-06, TOK-07, TOK-12, TOK-16, TOK-20 | 符合可用条件的模型都可供用户选择 | 已由 CR-35 细化状态语义 | 原记录把目录展示、上下文资格和请求预占混为一个可用条件；模型目录同步要求继续有效，状态划分以 CR-35 为准 |
| CR-08 | BILL-14, BILL-17, BILL-16, UI-08 | 分别展示基础积分、赠送积分、到期顺序、即将到期提醒和多个权益包，同时展示可用总积分 | 已实现 | 组成级 API 与三语 UI 已展示 BASE/BONUS、总可用、来源、确定消费顺序和到期提醒；人工赠送为独立 30 天 BONUS |
| CR-09 | SCOPE-04–SCOPE-06, TOK-15, TOK-35–TOK-37, NFR-20 | 删除旧模型路径及兼容代码，全量迁移到账号平台 + Token Gateway | 部分实现 | 客户端范围内的旧路径删除、数据/凭据迁移、动态目录、非计费 readiness 和三层门禁已实现；正式签名制品升级矩阵、原子回滚和生产联调待外部验收 |
| CR-10 | ACC-13, ACC-59–ACC-64, BILL-46, TOK-49, UI-22, ADM-05–ADM-07, ADM-45–ADM-49 | 基于当前全部能力重新设计角色系统 | 已实现 | 采用个人、系统、组织三类人类权限，角色只由精确权限集合构成；对象作用域、独立版本、策略审批、受信恢复、工作负载委托、权限驱动界面及全敏感入口发布门禁均默认拒绝并已实现。 |
| CR-11 | UI-09 | 品悟应用内个人中心提供 Web 个人中心入口 | 已实现 | 桌面账号弹层通过专用命令打开受信配置的 Portal 根地址；WebView 不能注入任意 URL，OIDC Token 不进入 URL |
| CR-12 | UI-10, PAY-07, PAY-08 | Web 个人中心提供充值入口 | 已实现、需保留 | 当前个人活动上下文的“额度”页已经支持套餐选择、待支付订单列表、二维码弹窗和到账查询；后续 UI/角色重构不得删除 |
| CR-13 | ID-08, NFR-14 | 短信业务流程与供应商实现隔离 | 已实现 | 启动工厂按配置注入供应商托管或平台生成/通用 HTTP 发送 Adapter；业务状态机无厂商条件并持久化 Provider ID、Adapter 版本和不透明校验数据，见 `sms-verification-provider-design.md` |
| CR-14 | TOK-05, NFR-18 | 模型网关与 New API 具体实现隔离 | 已实现 | Token Gateway 领域、数据库、公开 API 和管理 UI 统一使用 `provider_id`、`adapter_version`、`upstream_request_id` 和 `UsageEvidenceProvider`；New API 请求头、日志接口及用量字段只存在于对应 Adapter，配置可切换通用 OpenAI 兼容 Adapter |
| CR-15 | ACC-05, PAY-06, PAY-01, NFR-16 | 外部商城账号和订单语义与新华三实现隔离 | 已实现 | Account/Gateway 以 Commerce Mapping 和通用状态隔离客户语义，Billing 通过供应商中立商品映射、订单引用、支付流水和证据模型隔离 H3C 状态与协议；厂商字段只留在 Adapter |
| CR-16 | CORE-01, CORE-02, ACC-01, NFR-15 | 身份认证 Provider 与业务授权隔离 | 已实现 | Keycloak 用户创建/删除及密码策略迁入独立 Identity Adapter；Account 只消费服务端口和不透明引用；OIDC 只提供标准主体和会话身份，平台及组织权限由 Account 权限系统判定 |
| CR-17 | PAY-01–PAY-04, NFR-17 | 生产/非生产支付 Provider 与支付渠道能力隔离 | 已实现 | H3C 与开发 Simulator 位于独立 Adapter；通用 Billing 只消费版本化能力和不可变商品映射，前端动态渲染渠道；远程 Compose 隔离测试进程、迁移和数据库，生产启动拒绝测试 Provider 且 Helm 无测试支付工作负载或模拟器配置 |
| CR-18 | ADM-10, NFR-19 | 系统运行探针配置驱动 | 已实现 | 系统状态页通过 `SYSTEM_PROBES_JSON` 注册表声明组件、URL 和通用判定 Adapter；Portal 聚合主流程不包含固定组件清单或厂商分支 |
| CR-19 | PAY-12, PAY-14–PAY-15, PAY-18–PAY-20, PAY-23, PAY-25, PAY-28–PAY-31, PAY-33–PAY-36 | 支付按完整生命周期设计并允许 Provider 能力降级 | 已实现、退款范围已由 CR-41 取代 | 原记录中的订单级部分退款已移除；现行支付证据、单调关单、履约隔离及定向整笔退款分别由 PAY-12～PAY-14 与 CR-41 收敛，Provider 缺能力时失败关闭或进入明确降级复核 |
| CR-20 | BILL-07, PAY-22, PAY-23, UI-11 | 组织资金通过企业管理员在线购买组织权益入账 | 已实现 | 权限化组织购买冻结实际付款管理员、其 Commerce Mapping 快照和受益组织；每笔确认支付流水只向受益组织钱包创建独立权益包 |
| CR-21 | SCOPE-01, SCOPE-02, TOK-15, TOK-01 | 不实现个人中心自有供应商 Key/自管 Token Account | 移除 | 本地应用直接配置的云端或本地模型完全绕过账号平台，继续由桌面模型配置管理；个人中心不提供供应商 Key、个人 Token Account、共享策略或路由管理 |
| CR-22 | ACC-29 | 每个有效组织恰好一个所有者 | 已实现 | 所有权只能通过带稳定幂等指纹的专用原子转让命令变更；旧绑定只撤销不删除，延迟数据库约束在提交时强制每个有效组织恰有一个 OWNER，普通角色接口不能增删所有者 |
| CR-23 | TOK-19, TOK-21, TOK-23, BILL-30 | 动态预占采用已确认的边界策略 | 已实现（现行口径以 CR-36 为准） | 无 Tokenizer 时使用版本化确定性估算器且安全裕量为 0；Hold 续租和追加预占已实现，历史固定缺省输出规则不再使用 |
| CR-24 | TOK-06, TOK-07, TOK-12, TOK-16, TOK-20 | 只展示符合可用条件的模型 | 已由 CR-35 取代 | 原记录要求把上下文和路由资格作为目录展示前提；现行规则允许 CATALOG_VISIBLE 独立展示，并分别计算 CONTEXT_ELIGIBLE 与请求级 RESERVABLE |
| CR-25 | BILL-16–BILL-17, BILL-19, BILL-28, PAY-24–PAY-31 | 权益退款、消费顺序和过期 Hold 采用确定规则 | 已实现 | 已完成确定消费顺序、30 天人工 BONUS、过期 Hold 释放、迟到用量平台损失复核，以及目标支付流水/权益包锁定和整包 BASE/关联 BONUS 撤销 |
| CR-26 | BILL-05, BILL-08–BILL-13 | 组织额度按权益包分配并保留成员占用 | 已实现 | 组织授予绑定精确权益包及 BASE/BONUS 组成，支持独立批量结果、成员移除暂停/高版本恢复、普通全额撤回重建和组织调用占用；注销专用隔离与回收继续由 BILL-39/BILL-40 独立追踪 |
| CR-27 | ACC-34, ACC-35–ACC-39, UI-07 | 桌面与 Web 共享一个用户级活动上下文 | 已实现 | 用户级唯一聚合和 CAS 切换已替代 Session 所有权；跨端按版本/聚焦刷新，失效自动回个人，认证响应传播上下文及授权版本；进行中调用继续按既有请求开始快照结算 |
| CR-28 | ADM-01–ADM-03, ADM-06 | 超级管理员初始化、恢复和授权生命周期 | 未实现、ADM-01 初始化凭据由 CR-58 取代 | 原记录使用一次性8位随机初始密码；现行首个管理员初始化改用交互式直接设密或高熵一次性引导凭据；Break-glass 仅处理全部管理员异常失联，不绕过最后一个有效超级管理员主动注销保护，临时授权和自动回收部分保持有效 |
| CR-29 | ADM-07 | 审批允许角色兼任和交叉审批 | 已实现 | 同一用户可同时拥有计费运营和审批角色；服务端按稳定用户 ID 禁止自审，不同用户可交叉审批，强制审批命令只需一名独立审批人 |
| CR-30 | ACC-26 | 组织邀请支持手机号、用户名和一次性链接 | 已实现 | 三种方式使用统一不可物理删除实体、过期与一次性消费规则；链接只保存 Token 摘要，手机号/用户名只保存目标摘要和提示，创建不探测目标，本人恢复查询不接收任意目标输入，接受时绑定当前已认证稳定用户 ID |
| CR-31 | ACC-10, ACC-11 | 用户可主动注销并保留三年法定记录 | 未实现、由 CR-52 细化 | 有个人余额、未清应收、本人未完成订单、未结算 Hold 或尚未转让的组织所有权时禁止完成注销，其他资金/外部事实及自动清理门禁按 CR-52；未消费组织授予不阻止注销并按 CR-51 处理；注销后标识可复用，法定记录以不可反查业务身份保存 3 年 |
| CR-32 | ACC-07, ACC-08, ACC-06 | 用户资料修改采用可恢复 Saga | 已实现 | `profile_change_requests` 保存旧值、新值、资料版本和各阶段时间；Identity 成功后只向前重试，Account CAS 后异步重置 Commerce 任务，失败退避或进入修复状态。 |
| CR-34 | BILL-29–BILL-33, PAY-33 | 用量负余额与拒付应收分场景记账并按钱包上下文隔离冻结 | 已实现 | 用量超额双记独立应收；拒付统一案件确认后只冲销目标支付权益包，现金 BASE 缺口形成订单应收，后续现金 BASE 优先清偿且按钱包隔离冻结，证据、责任人和结论完整保留 |
| CR-35 | TOK-07, TOK-12, TOK-16, TOK-20 | 模型状态拆分为目录展示、上下文资格和请求级实时预占 | 部分实现 | CATALOG_VISIBLE 已由独立目录实现且不混入资格/余额；RESERVABLE 仍在请求级创建 Hold 时实时判断；完整 CONTEXT_ELIGIBLE 和 READY Provisioning 由 APT-028 收口 |
| CR-36 | TOK-04, TOK-17–TOK-20, BILL-20–BILL-21, BILL-23–BILL-25 | 模型协议和计价抽象为版本化计量单位及协议预占公式 | 已实现（当前 CHAT 协议） | 版本化能力发现、路由协议、单位、公式、缺省量为 0、输入互斥计量和零价语义均已落地；新增协议须由 Adapter 与模型目录共同声明 |
| CR-37 | ID-14, ID-15, ACC-02–ACC-04, BILL-04, UI-02 | 注册采用持久 PROVISIONING，钱包初始化异步重试 | 已实现 | Identity 与 Account 成功即账号存在；钱包完成前允许登录和资料管理但禁止平台模型及充值；超过快照化 SLA 才进入 REPAIR_REQUIRED，Billing 暂时不可用不删除已验证身份 |
| CR-38 | SCOPE-01, SCOPE-04–SCOPE-06, TOK-35–TOK-37, NFR-20 | 补齐品悟客户端旧模型路径的数据迁移和切换验收 | 部分实现、由 CR-44 细化门禁 | 客户端独立模型受保护，一次性迁移、凭据清理、范围守卫和失败关闭已实现；跨版本正式安装制品与回滚矩阵待环境验收，TOK-37 仍保持独立提交 |
| CR-39 | PAY-33, PAY-14, PAY-12, PAY-32, PAY-13, PAY-26 | 删除主动部分退款状态并拆分支付领域状态机 | 已实现 | 支付流水成功事实、退款、拒付、关单和异常案件相互独立；普通部分退款已移除，账单逐行复用支付归并器，渠道强制退款独立留证、冻结和受审计结案 |
| CR-40 | TOK-07–TOK-10, TOK-12, TOK-16, TOK-20, ADM-25, ADM-28–ADM-30 | 闭合默认托管路由创建与模型资格判断顺序 | 已实现 | 目录可见、持久 Provisioning、READY/上下文资格、请求级预占、Hold 和上游调用顺序已闭环；平台账号创建、启停、状态/修复查询均按平台模型运营权限隔离 |
| CR-41 | BILL-28, PAY-11, PAY-20, PAY-24–PAY-31, PAY-33 | 支付履约隔离并按实际流水和权益包原子创建退款意图 | 已实现 | 多流水独立履约、目标普通退款和异常退款闭环：消费预检、原子冻结/预约、Outbox Consumer、全 BASE/关联 BONUS 撤销，以及拒付/强制退款受审计定向处置均已实现 |
| CR-42 | BILL-20, BILL-23–BILL-25, ADM-12–ADM-13 | 闭合公式单位、输入缓存计量和零价语义 | 已实现 | 公式引用单位必须完整配置，输入与缓存输入规范化互斥；固定零价、显式继承和缺价已有独立数据与运行时语义 |
| CR-43 | SCOPE-03, ADM-11, ADM-25–ADM-30, ADM-47 | 隔离组织模型业务策略与平台 Token Gateway 运营权限 | 已实现 | `/admin/v1/*` 由 `platform.tokens.*` 独占，普通 Token 管理面和合同已删除，存量非平台凭据隔离；组织模型策略只接受业务模型范围。 |
| CR-44 | SCOPE-04–SCOPE-06, TOK-35–TOK-37, NFR-20 | 将客户端切换拆分为发布、安装和运行三层门禁并移除计费准入请求 | 部分实现 | 发布、安装和运行门禁均已失败关闭且使用无 Hold readiness；正式平台安装器的二进制/配置原子提交与回滚仍待签名制品环境验收 |
| CR-46 | TOK-24–TOK-30 | 补齐模型请求幂等、流式重放、断线恢复和 Hold 后派发恢复契约 | 已实现 | 稳定用户级作用域、分层保留、同键异请求冲突、加密流事件重放、断线状态恢复、唯一 Hold 和一次性派发租约均已实现；上游派发不确定时禁止盲重试 |
| CR-47 | REQ-ID-01–REQ-ID-03, CORE-01–CORE-07, SCOPE-01–SCOPE-05, ID-01–ID-16, ACC-01–ACC-39, TOK-01–TOK-37, BILL-01–BILL-33, PAY-01–PAY-36, UI-01–UI-12, ADM-01–ADM-32, NFR-01–NFR-25 | 删除退休需求并按依赖关系重排、重新编号全部目标能力 | 文档完成、实现状态保持原判断 | 删除编号稳定与禁止重排规则；移除目标基线中的退休能力占位；按基础能力、依赖顺序和领域相关性形成连续新编号，并同步全部文档和实现状态引用 |
| CR-48 | TOK-29, TOK-38–TOK-41, BILL-34–BILL-35, UI-13 | 补齐用户取消、连接断开与模型计费收敛契约 | 已实现 | 取消意图、服务端执行、客户端交付、可信用量和结算分别保存；当前 Adapter 明确不支持取消时后台继续，按实际用量结算并在版本化期限安全续租或释放 Hold |
| CR-49 | PAY-17, PAY-19, PAY-35, PAY-37–PAY-40, ADM-33 | 72小时未收敛订单转人工审核并闭合无回调无账单 Provider 的晚到支付处理 | 已实现 | 前72小时退避查询，边界执行最终查询后创建唯一 P2 案件并告警；前7天每日、此后每周复核，用户申诉只追加证据。正式收款要求有限晚到窗口、长期历史查询和权威未支付终态；归档要求期限后成功权威查询且不存在支付、冲突或未复核申诉，否则升级 Provider blind 并停用该版本新单 |
| CR-50 | BILL-36–BILL-38 | 明确组织权益组成资金守恒、禁止超额授权及并发分配边界 | 已实现 | 数据库守恒桶、组织/钱包/包/组成归属校验、精确组成与授予行锁、版本化请求指纹、同键异请求冲突及逐项子事务共同禁止超额和重复分配；生产等价并发压力仍待部署验证 |
| CR-51 | ACC-10, ACC-11, BILL-12, BILL-13, BILL-36, BILL-39 | 注销用户的未消费组织授予终局占用至原到期日 | 未实现、由 CR-54 调整 | 原规则禁止任何回收；现行规则仍要求 `ACCOUNT_CLOSED` 授予不自动回池、不可恢复、消费或普通撤回，但允许按 CR-54 的独立专用命令主动回收 |
| CR-52 | ACC-10–ACC-11, ACC-40–ACC-50, BILL-39, UI-14, ADM-34 | 用持久注销 Saga 区分资金阻断、系统自动清理和标识释放门禁 | 未实现 | 待核对用量、拒付、资金性差异、外部待确认关单及资金相关 Commerce 任务阻断注销；组织所有权及最后一个有效超级管理员角色须先交接，资料 Saga、其他角色、临时授权和邀请不要求用户处理但必须自动收敛；全部门禁通过且 Identity 不可认证后才能释放标识，迟到任务不得作用于新账号 |
| CR-53 | ID-03, ID-14–ID-15, ID-17–ID-18, ACC-02, ACC-51–ACC-55, UI-15, ADM-35, NFR-04, NFR-06 | 闭合注册预检并发、唯一值预约、幂等、密码边界和孤儿 Identity 补偿 | 已实现 | 数据库唯一预约、稳定操作/密码指纹、脱敏查询、Identity 关联向前恢复及24小时证明门禁补偿已闭环；全域日志字段白名单和自动秘密扫描已完成，剩余全域幂等治理由 APT-045 追踪 |
| CR-54 | BILL-12, BILL-36, BILL-38–BILL-40 | 允许授权管理员选择性回收注销账号的未消费组织授予 | 未实现 | 注销授予不自动回池；账号完成 `ANONYMIZED` 且不存在消费、Hold、待核对用量或资金冻结时，授权管理员可用专用命令一次性全额回收至原权益包原组成，禁止部分回收、跨组成转换、现金退款或直接授予新账号 |
| CR-55 | TOK-42–TOK-45 | 补齐模型目录管理生命周期、历史展示、重命名/别名和版本回滚 | 部分实现 | 生命周期、历史墓碑、稳定模型 ID、版本化名称/别名和前向回滚已实现；回滚仍待 APT-028 用当前价格、Provisioning 路由和凭据完成联合发布校验 |
| CR-56 | TOK-46–TOK-47 | 明确模型关联版本生成边界及下架后的新旧会话和在途请求策略 | 已实现 | 独立版本、请求快照、截止门禁和桌面显式替代分支已实现；紧急 `force_cancel_inflight` 与目录变更原子提交，Worker 收敛在途请求并保存 Adapter 能力、本地中止、实际生成量、Hold 和结算证据 |
| CR-57 | BILL-21–BILL-25, BILL-41–BILL-45, ADM-13 | 明确定价匹配优先级、显式单位继承、时间冲突、生命周期及 Hold 不变性 | 已实现 | 模型覆盖优先、显式继承、固定零价、区间冲突、完整生命周期及既有 Hold 不重算均由数据约束和服务契约实现 |
| CR-58 | ADM-01, ID-18, NFR-04 | 用交互式设密或高熵一次性引导替代8位超级管理员初始密码 | 已实现 | admin 激活前保持 BOOTSTRAP_PENDING；直接设密使用高权限密码策略；引导凭据默认256 bit、最少128 bit、Base64URL、15分钟有效、最多5次失败、一次读取销毁且成功兑换原子失效；输出和统一日志不记录凭据原文 |
| CR-59 | ADM-07, ADM-17, ADM-22, ADM-36–ADM-42 | 补齐管理命令审批对象、生命周期、失效和执行契约 | 已实现 | 后台写命令已版本化分类；审批支持独立决定、过期、撤回、新申请重提、快照失效、实时授权复核、Outbox 授权与 Billing 幂等执行 |
| CR-60 | NFR-26–NFR-48, ADM-09 | 建立性能、容量、灾备、保留、限流、隔离、Provider韧性、时钟、客户端兼容和审计导出的量化验收基线 | 部分实现 | 受控审计导出、量化策略和仓库门禁已闭环；仍需在生产等价环境取得性能/容量、分级RPO/RTO、加密备份与演练、分类保留、多维限流、租户隔离、Provider韧性、授时偏差及客户端兼容的真实运行证据 |
| CR-61 | ACC-02, ACC-11, ACC-56–ACC-58, NFR-50 | 建立永久内部用户 ID 和独立可展示用户编码 | 部分实现 | ACC-56–ACC-58 与 NFR-50 已完成；ACC-11 的完整注销 Saga、资料清理和三年分类保留仍待实现 |
| CR-62 | PAY-31, UI-12, ADM-20 | 普通退款暂不开放，保留既有实现但屏蔽所有新建入口 | 已实现 | 独立产品门禁默认关闭并进入公开 capability，用户入口不存在、运营动作采用双门禁、服务端稳定拒绝新建请求；既有退款收敛、只读查询与被动异常处置保持可用 |
| CR-63 | PAY-10, PAY-12, PAY-15–PAY-17, NFR-03, NFR-04, NFR-41 | 提供 Provider 中立授权与支付通知接口，新华三作为首个 Profile 且不增加额外能力字段 | 旧版云端接口已部署；本轮合同待部署联调 | 凭据和 Token 绑定 Provider、版本及商户，请求体不能自报身份。H3C 固定声明 `notification_supported=true`，最新版原生订单回调与主动查询已复用同一映射并先入 Inbox；异步履约查询中的 `status` 歧义也已修复。真实商城原生通知、重复/乱序和到账结果仍需部署后联合验收 |
| CR-64 | UI-09, PAY-15–PAY-17, NFR-03, NFR-04 | Web 个人中心与公开支付回调统一迁移到账号独立 HTTPS 子域名和 `/api/v1` 命名空间 | 云端 UAT 已部署 | 桌面默认入口、Portal 根路径、Billing 路由、OpenAPI、Helm、Compose、Nginx 与文档已统一为 `https://account.pinvou.com/`；受信 TLS、HTTP `308`、Portal `200`、回调未授权 `401` 与受控 Token 签发 `200` 均已验证。旧官网个人中心代理与旧回调契约未保留兼容代码；真实商城回调仍由 CR-63 跟踪 |
| CR-65 | TOK-48, UI-17–UI-19, NFR-51 | 平台模型入口从账号 Popover 迁移到设置和对话区，并隔离全局默认与当前会话选择 | 已实现 | APT-059–APT-063 已完成 Popover 收敛、类型化引用、设置默认项、对话当前会话选择及三语、异常和跨入口自动回归门禁 |
| CR-66 | PAY-41–PAY-43 | 按最新版新华三合同统一永久客户标识、订单查询/回调和精确金额事实 | 仓库实现完成，待部署联调 | 创建订单传不可变 `user_id`，分页查询强制按同一客户标识，回调直接采用 `queryOrder.content` 原生对象并复用查询映射；金额精确换算且异常失败关闭，剩余真实回调和履约证据待部署验收。具体商品配置由运行时目录管理。 |

这些需求之间的强制关系：

- 活动上下文决定“谁在使用”，角色决定“允许做什么”，额度授予决定“最多能花多少”，模型目录/策略决定“可以选择什么模型”；四者必须独立判断。
- 组织额度分配不是钱包转账。员工切回个人上下文后，不能看到或使用组织授予额度。
- 组织总额度池通过企业管理员在线购买组织权益形成；成员分配仍只是限额授权，不得把组织权益转入个人钱包。平台人工调整只用于受审计修复，不是正常采购入口。
- 模型目录展示、托管路由准备、上下文资格和请求级预占必须按 TOK-10 独立且有序判断：`CATALOG_VISIBLE` 决定目录展示，Provisioning READY 提供可命中托管路由，`CONTEXT_ELIGIBLE` 判断当前上下文权限/策略/路由，`RESERVABLE` 紧邻具体请求的 Hold 原子创建实时计算。
- 模型目录管理生命周期与模型调用资格相互独立：TOK-42–TOK-47 决定目录修订如何发布、弃用、移除、展示历史、回滚、生成关联版本以及处理下架会话；每次新请求仍从当前已发布目录版本重新执行 TOK-10，历史会话展示只读取请求时快照。
- 定价先按 BILL-41 选择模型或全局作用域，再按 BILL-42 逐单位解析固定价/显式继承；BILL-43–BILL-44 保证同作用域时间唯一和生命周期合法，BILL-45 保证任何后续撤销、结束、替代或回滚都不重算已有 Hold。
- 首个超级管理员初始化不再创建可直接登录的短密码：ADM-01 只允许受控交互式直接设密或高熵一次性引导，admin 在原子完成正式密码设置前保持 BOOTSTRAP_PENDING；凭据失败、读取、兑换、过期和重新签发都必须可审计且不可恢复明文。
- 删除旧路径采用客户端一次性幂等数据迁移和明确切换，不保留长期运行时兼容分支；实现改动不得越过品悟客户端及其直接相关文件边界。
- Web 个人充值只增加个人钱包额度；具备组织采购权限的企业管理员可以在组织上下文显式购买组织权益。付款人、订单所有者和权益受益组织必须明确保存，不能把个人充值静默记入组织钱包。
- 桌面打开 Web 个人中心时只传递目标页面，不在 URL、查询参数、Fragment 或浏览器命令中传递 Access/Refresh Token；浏览器使用自己的 Portal OIDC 会话，未登录时进入正常 Web 登录流程。
- 注册、登录、改手机号和找回密码只依赖统一验证码挑战服务，不识别阿里云 Action、签名、模板 CODE 或响应字段。更换短信方案不得修改这些业务流程、公开 API 或 Keycloak 手机登录协议。
- “删除旧模型路径”不仅是关闭功能开关：桌面持久化模型中不得继续保存 New API 用户 ID、Token ID、后台密码或 API Key，也不得在账号平台不可用时回退到旧 New API 用户开通和设备账号流程。
- 模型调用领域只认识上游模型网关、上游请求关联和用量证据；`X-Oneapi-Request-Id`、`New-Api-User`、New API 日志查询参数及其错误文本必须由 New API Adapter 转换为统一模型。
- 手机注册成功后的商城账号开通属于外部 Commerce Customer Provisioning，不属于用户授权上下文本身。商城未开通只能影响需要该商城客户身份的支付动作，不能成为模型调用、组织授权或普通账号功能的通用前置条件。
- 身份 Provider 负责“用户是谁”，Account 权限系统负责“允许做什么”。Keycloak Realm Role 只允许作为迁移期引导或受控运维入口，不能继续散落在 Account、Billing、Token Gateway 的业务路由上。
- Provider 特殊规则可以存在于对应 Adapter 内，例如新华三只支持支付宝并支持支付回调，但没有退款、主动关单或账单接口；通用 Billing 状态机和前端只能消费能力声明及统一错误，不得按 Provider 名称分支。
- 本地应用中由用户直接配置、完全不经过账号平台的云端或本地模型不属于 Token Account 管理，也不属于需要删除的旧内置模型路径；个人中心不新增用户供应商 Key 功能。
- 退款、对账和支付异常以完整产品闭环为目标。Provider 缺少某项能力时只能按 capabilities 执行明确降级，不能反向删除通用领域状态、审计和人工复核要求。
- 注销资格中的资金阻断与系统自动清理必须分离：待核对用量、拒付、资金性差异、外部待确认关单和资金相关 Commerce 任务未收敛时不能释放身份；组织所有权和最后一个有效超级管理员角色必须先交接，资料 Saga、其他角色、临时授权和邀请不要求用户手工处理，但自动清理未完成同样不能越过标识释放门禁。
- 注册短信前预检只用于减少无效短信，数据库正式映射与活动预约才是并发唯一性事实源。密码策略、原始密码及同键异密码判断只存在于 Identity 边界；Identity 已创建后预约不能按 TTL 释放，必须由 Account 提交或经过证明的补偿收敛。

### 1.2 平台实现隔离的统一验收规则

本节适用于短信、身份认证、外部商城、支付、模型网关和运维探针。隔离完成必须同时满足：

1. 领域服务、核心状态机、数据库核心字段、公开 OpenAPI 和普通用户 UI 使用供应商中立名称，不以 `aliyun`、`keycloak`、`h3c`、`mall`、`newapi`、`oneapi` 或 `simulator` 定义业务语义。
2. 厂商 Action、请求头、签名、Endpoint、状态码、响应结构、错误文本和重试约束仅存在于该厂商 Adapter；Adapter 必须把它们映射为统一能力、状态和错误分类。
3. `cmd/*`、Compose、Helm、Secret 和运维文档可以出现实际厂商名称，因为它们承担 Provider 选择和依赖装配；这些配置不得反向渗入业务 Handler、领域实体或前端条件分支。
4. 替换同类 Provider 时，注册/登录、充值、模型调用等业务 Handler、公开 API、桌面业务交互和核心账本结构不需要修改；只允许新增/替换 Adapter、配置、Provider 专属迁移或商品映射。
5. 历史记录必须保存创建时使用的 Provider ID、Adapter 版本和脱敏外部引用，保证 Provider 切换期间仍能查询、核验和收敛；不得用当前 Provider 解释旧订单、旧验证码或旧用量请求。
6. Provider 不支持的能力通过 capabilities 明确返回并在 UI 禁用，不能用本地状态伪造外部成功，也不能因切换 Provider 自动重复高风险请求。
7. 每个 Adapter 使用同一套合同测试验证正常、拒绝、超时不确定、重复请求、状态映射和敏感信息保护；替换验收必须证明业务层没有新增厂商条件分支。

允许保留的实现细节包括：`internal/platform/**` 或明确 Adapter 包中的厂商客户端，`cmd/*` 中的 Provider Factory，Compose/Helm 的实际镜像和环境变量，以及系统状态页对真实部署组件的展示名称。PostgreSQL、Redis 和 NATS 当前已经分别通过 Repository、Store 和事件端口形成基本边界，本轮不要求为了消除名称而机械替换。

## 2. 用户与角色

| 参与者 | 能力边界 |
|---|---|
| 未登录用户 | 密码登录、短信登录、手机注册、滑块与短信验证。 |
| 个人用户 | 个人上下文、余额/权益包、充值、个人用量、平台托管模型路由。不能创建顶层组织。 |
| 组织成员 | 在已加入组织内按角色和节点范围使用组织资源；不能越过当前会话上下文。 |
| 组织所有者/管理员 | 管理组织节点、成员和可分配角色；所有者转让走专用流程。 |
| 平台管理员 | 通过 Account 权限系统授予的平台作用域权限进入运营后台，管理跨租户业务与异常；Keycloak 只证明身份。 |
| 运维人员 | 通过受限网络管理 Keycloak、数据库、NATS、New API 和部署，不等同于平台管理员。 |

## 3. 功能需求矩阵

### 3.1 注册、认证和会话

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| ID-01 | 密码支持常用英文符号 | 已实现 | 8–128 位可打印 ASCII，至少一个字母和一个数字。 |
| ID-02 | 手机注册必须填写唯一用户名、手机号和两次一致的密码 | 已实现 | 前端核对两次密码，后端仍独立校验用户名、手机号和密码。 |
| ID-03 | 用户名、手机号和密码在发短信前预检 | 已实现 | 图形挑战消费后、短信发送前由 Account 校验用户名/手机号，并以不透明引用调用 Identity 权威预检密码；最终唯一约束仍独立执行。 |
| ID-04 | 获取短信前完成滑块挑战 | 已实现 | 服务端生成一次性挑战，客户端用覆盖登录框的弹层完成；不是第三方 CAPTCHA。 |
| ID-05 | 短信请求限流 | 已实现 | 同手机号至少间隔 60 秒，最多 5 次/小时；前端显示倒计时。供应商明确发送失败不占冷却。 |
| ID-06 | 短信挑战有过期、尝试次数和一次性消费约束 | 已实现 | 5 分钟有效，最多 5 次核验，成功后不可复用。 |
| ID-07 | 阿里云发送并核验验证码，平台不保存明文验证码 | 已实现 | Account 只保存 Provider 挑战标识和状态元数据。 |
| ID-08 | 可替换短信验证后端 | 已实现 | 配置与依赖注入支持厂商托管验证码和平台安全生成、供应商仅发送两种模式；业务状态机只保存供应商中立的描述和校验数据。 |
| ID-09 | 账号密码登录支持手机号或用户名 | 已实现 | 桌面端通过 Keycloak Direct Access Grant；不持久化密码。 |
| ID-10 | 手机号 + 短信验证码登录 | 已实现 | Keycloak 自定义 Direct Grant 调用 Account 内部核验接口；仅中国大陆手机号。 |
| ID-11 | 短信反枚举 | 已实现 | 已知/未知手机号使用相同 `202` 响应和最小耗时桶；登录短信异步投递使供应商状态不泄露账号存在性，并按手机号、来源、设备摘要限流审计。 |
| ID-12 | 登录成功后下次启动自动登录 | 已实现 | Refresh Token 仅存系统凭据库，启动时刷新。 |
| ID-13 | 登出、刷新失败或凭据错误后停止自动登录 | 已实现 | 失败路径删除持久化 Refresh Token，不回退历史缓存。 |
| ID-14 | 注册不留下不可登录的半成品账号 | 已实现 | 服务端注册操作持久记录阶段，按外部关联恢复 Identity 并幂等提交 Account；成功即返回可登录的 `PROVISIONING`，钱包故障不删除身份。 |
| ID-16 | 除密码/短信外的 MFA 和账号申诉 | 非当前范围 | Keycloak 具备扩展基础，但当前产品闭环未实现；短信找回密码已纳入 ACC-09。 |
| ID-17 | 密码只在 Identity 边界处理 | 已实现 | 独立 Identity Adapter 持有原始密码、冻结策略版本和短期带密钥 HMAC；Account/短信接口仅接收不透明引用，最终创建后清除原文、到期删除记录。 |
| ID-18 | 密码原始字符策略 | 已实现 | 客户端与 Identity 均按原始序列校验；中间半角空格可用，首尾空格返回 `PASSWORD_EDGE_SPACE_NOT_ALLOWED`，Tab、换行、非 ASCII 和超长输入明确拒绝。 |
| ID-19 | 登录会话签发权限 | 已实现 | Keycloak 浏览器与 Direct Grant 的密码/短信凭据路径在成功前强一致检查 `personal.session.login`；拒绝或 Account 不可用均失败关闭。 |
| ID-20 | 密码安全操作权限 | 已实现 | 已登录与匿名路径共用 `personal.security.manage` 和 `ACTIVE/PROVISIONING/REPAIR_REQUIRED` 允许矩阵；其他状态不发送恢复短信且不变更密码。 |

验收重点：Identity 与 Account 已成功时必须返回“账号已创建”的 PROVISIONING/ACTIVE 结果；钱包短暂失败不得返回注册失败、密码错误或删除手机号身份。短信前预检不替代数据库唯一约束；Identity 创建后的预约不得按 TTL 释放。密码只进入 Identity 边界，首尾空格明确拒绝且任何层不得 trim；密码、验证码、Access Token 和 Refresh Token 不得出现在日志或前端业务 JSON 中。

### 3.2 用户、组织和角色

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| ACC-01 | 身份 Provider 管理适配 | 已实现 | Account 仅调用 Identity HTTP 端口并传递不透明引用；Keycloak 管理协议、Token 和客户端密钥全部位于独立 Adapter。 |
| ACC-05 | 外部 Commerce Customer 开通 | 已实现 | Mapping、任务和证据使用供应商中立模型，冻结 Provider/Adapter 版本并异步退避收敛；外部原始错误和客户标识不进入公开接口。 |
| ACC-06 | 资料变更 Saga | 已实现 | 唯一请求、字段预约、资料 CAS、Identity 更新、会话撤销、退避恢复和人工修复状态均已落地。 |
| ACC-07 | Web 用户名修改 | 已实现 | Portal OIDC 重新认证、规范化预约、Identity Adapter 和 Account CAS 已落地；桌面入口已移除。 |
| ACC-08 | Web 手机号修改 | 已实现 | Web 新手机号滑块/短信验证、重新认证、Identity/Account 提交和 Commerce 同步任务重置已落地；桌面入口已移除。 |
| ACC-09 | 桌面忘记密码 | 已实现 | 未登录手机号短信验证后由 Identity Adapter 重置密码并撤销会话；Account 不接触密码原文。 |
| ACC-10 | 用户主动注销 | 已实现 | 最近5分钟认证、`CLOSE_MY_ACCOUNT` 明确确认和幂等键创建唯一持久请求；本地角色门禁、静默期和跨 Billing/Token 阻断已接通，资料/标识/组织授予的后续自动清理由 ACC-46–ACC-50 与 BILL-39/BILL-40 继续约束。 |
| ACC-11 | 注销记录保留 | 已实现 | 外部 Identity 删除后，Account 随机覆盖全部直接标识并释放用户名/手机号；user_id、user_code 和内部主体引用永久保留，三年分类保留及法律保全状态由独立记录和到期 worker 管理。 |
| ACC-12 | 一个用户可属于多个组织 | 已实现 | 成员关系独立存储；个人中心只显示当前用户关联组织。 |
| ACC-13 | 角色系统按权限码和作用域管理 | 已实现 | Account 持久化并解析平台/组织角色、原子权限及组织/节点作用域，所有服务端授权默认拒绝。 |
| ACC-14 | 个人用户不能创建顶层组织 | 已实现 | 顶层组织创建位于平台管理 API/UI。 |
| ACC-15 | 创建顶层组织 | 已实现 | 平台权限、名称约束、稳定幂等请求指纹、审计和 Outbox 已形成原子创建闭环。 |
| ACC-16 | 搜索组织 | 已实现 | 支持名称/精确 ID、显式状态、稳定复合游标和默认隐藏已删除组织。 |
| ACC-17 | 启用组织 | 已实现 | Billing 恢复门禁、组织/成员/授权版本和幂等审计事件已闭环。 |
| ACC-18 | 停用组织 | 已实现 | Billing 钱包先冻结，Account 再暂停成员并回退活动 Session；新购买、入账与 Hold 失败关闭。 |
| ACC-19 | 组织安全逻辑删除 | 已实现 | Account 治理事实与 Billing 资金、权益、Hold、支付/退款/差异/用量事实执行跨域预检并返回结构化阻断原因。 |
| ACC-20 | 逻辑删除组织 | 已实现 | 预检清零后写终态 DELETED、关闭组织钱包并保留财务/审计历史；重复调用幂等成功。 |
| ACC-21 | 创建组织子节点 | 已实现 | 操作者、组织、命令类型和稳定幂等键冻结请求指纹；活动父节点、同组织闭包、结构版本和审计在同一串行事务提交，重放返回原结果。 |
| ACC-22 | 组织名称和节点名称显式进入编辑态后修改 | 已实现 | 管理 UI 提供启动编辑动作，避免误改。 |
| ACC-23 | 重命名组织节点 | 已实现 | 显式编辑 UI、原子目标权限、名称规则和幂等命令共同约束；成功只更新名称及节点/结构版本，节点 ID、父子关系和历史审计不变。 |
| ACC-24 | 调整组织节点父节点 | 已实现 | `expected_structure_version`、组织串行锁和稳定请求指纹保证同组织原子移动；跨组织、停用父节点、自身及后代父级均拒绝。 |
| ACC-25 | 删除组织节点 | 已实现 | Account/Billing/Token 先进入限制态并汇总成员、角色、子节点、授予登记、路由、用量和任务阻断；全部清零后仅写 DELETED，保留闭包树及历史，重试返回原结果。 |
| ACC-26 | 组织邀请 | 已实现 | 手机号、用户名和一次性链接共用不可物理删除的邀请实体；创建不泄露目标存在，认证身份可反向恢复目标邀请，接受时绑定稳定用户且所有终态幂等保留。 |
| ACC-27 | 添加组织成员 | 已实现 | 邀请接受与平台直接添加均校验目标和权限；稳定请求指纹使重复命令返回同一成员关系，只有真实状态变化才递增版本并发送事件。 |
| ACC-28 | 移除组织成员 | 已实现 | 幂等撤销全部有效/未来角色，递增授权与 Session 版本并让所有相关上下文立即回退个人；Billing 成员投影同步进入 REVOKED，供授予暂停强制使用。 |
| ACC-29 | 所有者安全转让 | 已实现 | `org.owner.transfer`/平台专用权限和稳定幂等键保护专用事务；旧 OWNER 只撤销不删除，新 OWNER 原子建立，延迟数据库约束禁止有效组织零个或多个所有者。 |
| ACC-30 | 查看角色定义和用户关联角色 | 已实现 | 本人只看自身绑定；组织和运营视图完整展示角色作用域、开始/到期、来源和状态。 |
| ACC-31 | 授予普通组织角色 | 已实现 | 普通组织/节点角色按授权作用域、起止时间和稳定幂等键创建，拒绝 OWNER、平台和基础角色。 |
| ACC-32 | 撤销普通组织角色 | 已实现 | 支持单绑定幂等撤销，事务内刷新成员、Session 和授权版本并发送变更事件。 |
| ACC-33 | 无组织用户不显示组织入口 | 已实现 | 无有效组织成员关系时，“账号设置”不显示组织管理区或其他组织入口。 |
| ACC-34 | 当前上下文按用户全局保存并跨端同步 | 已实现 | 每个 user_id 只有一条活动上下文；期望版本 CAS 防止旧端覆盖新端，桌面与 Web 按响应版本和聚焦事件同步。 |
| ACC-40 | 持久化注销 Saga | 已实现 | `account_closure_requests/commands/audit` 保存唯一请求、命令指纹、阶段版本、租约、重试、SLA 和不可变审计；数据库触发器拒绝阶段回退。 |
| ACC-41 | 注销静默期业务冻结 | 已实现 | 角色交接未完成时停在静默前且用户仍可自救；通过后原子置为 CLOSING，Account/Token 拒绝新业务，只开放历史读取与既有订单/用量修复动作。 |
| ACC-42 | 待核对模型用量阻断注销 | 已实现 | Token 内部评估覆盖未结算 Hold、派发/流式执行、取消/核对未终态；平台承担且明确禁止以后追扣客户的证据终态不阻断。 |
| ACC-43 | 未解决拒付阻断注销 | 已实现 | 未解决拒付、非零个人余额和未清应收共同阻断财务清算阶段，历史案件及资金证据不随请求删除。 |
| ACC-44 | 资金性对账差异阻断注销 | 已实现 | 开放或复核中的支付/退款/拒付/账本差异默认阻断；明确纯信息且不关联内部支付事实的 Provider 不支持行可保留而不阻断。 |
| ACC-45 | 外部待确认关单阻断注销 | 已实现 | 订单支付处理中、`CLOSE_REQUESTED`、`LOCALLY_CLOSED_EXTERNAL_PENDING`、失败关单、晚到复核和 `PROVIDER_BLIND` 全部进入跨域阻断快照。 |
| ACC-46 | 注销处理 Commerce Mapping 任务 | 已实现 | 真实主体生命周期版本进入每个任务；资料类任务自动取消，资金任务必须收敛，旧版本完成/失败证据只能标记 STALE，Mapping 在匿名化事务中 DETACHED。 |
| ACC-47 | 注销自动收敛资料修改 Saga | 已实现 | Identity 更新前的请求直接取消；更新后的请求先提交到同一用户再取消，随后旧值、新值、预约证据统一随机去标识化。 |
| ACC-48 | 注销自动撤销角色与临时授权 | 已实现 | 通过所有者和最后管理员门禁后，清理事务撤销全部当前/未来角色与授权、终止成员关系、递增版本、清除本地 Session，随后幂等删除 Identity 主体及 Refresh Token。 |
| ACC-49 | 注销自动失效未接受邀请 | 已实现 | 当前/待生效标识命中或由注销主体创建的 PENDING 邀请统一取消，摘要、提示和令牌随机覆盖；终态与审计永久保留且新账号不能接受。 |
| ACC-50 | 注销标识释放门禁 | 已实现 | 资料/邀请/Commerce/角色/成员清理、Identity 删除和 BILL-39 隔离均提交后，Account 才按主体版本原子写随机墓碑、DETACHED、DELETED、ANONYMIZED 与 Outbox。 |
| ACC-51 | 注册最终唯一约束 | 已实现 | 正式用户名/手机号映射与全部活动预约均有数据库唯一约束，Account 提交在串行事务内重检最终事实。 |
| ACC-52 | 注册唯一值分阶段预约 | 已实现 | 发送短信前5分钟预约、短信验证后30分钟租约和 Identity 创建后无 TTL 恢复锁均已落库，终态释放规则明确。 |
| ACC-53 | 注册操作稳定幂等 | 已实现 | 服务端操作 ID 贯穿短信、Identity、Account 和钱包；非秘密请求指纹、Identity 密码 HMAC 及稳定下游键拒绝同键异请求。 |
| ACC-54 | Identity 孤儿恢复与补偿 | 已实现 | 先按外部关联查询防重复创建，30分钟内自动向前恢复；满24小时且证明无业务事实后先停用 Identity、撤销会话并确认，再复核释放预约。 |
| ACC-55 | 注册操作安全查询 | 已实现 | 操作 ID 与48小时签名令牌双重校验，服务端只存摘要，响应仅暴露阶段、用户公开结果和下次重试时间。 |
| ACC-56 | 内部用户 ID 永久稳定 | 已实现 | UUIDv7 主键、数据库不可变/禁止物理删除触发器和逻辑删除墓碑共同保证已创建 ID 永久稳定且不复用。 |
| ACC-57 | 独立用户编码 | 已实现 | 已实现安全随机 Crockford 载荷、SHA-256 校验码、格式/唯一约束、冲突重试、永久登记和不可变保护。 |
| ACC-58 | 用户编码展示与精确查询 | 已实现 | 本人资料、桌面/Web 与运营后台展示编码；独立接口按完整规范化编码精确查询并能受控识别已注销主体。 |
| ACC-59 | 三类权限作用域 | 已实现 | 权限和角色具备 PERSONAL/PLATFORM/ORGANIZATION/UNIT 作用域；未知权限失败关闭，工作负载权限不混入人类角色。 |
| ACC-60 | 普通用户默认个人角色 | 已实现 | 创建事务和历史迁移保证非终态用户具有 `PERSONAL_USER`，个人能力再受生命周期矩阵限制。 |
| ACC-61 | 独立授权版本与对象决定 | 已实现 | 个人、平台、组织/节点版本独立；节点授权以精确绑定返回且不扁平扩权，真值表迁移使旧缓存失效。 |
| ACC-62 | 组织角色精确真值表 | 已实现 | `ORG_MODEL_ADMIN`、规范 `org.*` 权限码和全部组织角色替换式真值表已落地。 |
| ACC-63 | Account 本人入口原子授权 | 已实现 | 本人资料、上下文、注销和邀请路由逐项使用独立个人权限，目标只从认证主体解析，生命周期白名单不扩大普通能力。 |
| ACC-64 | 组织对象级授权 | 已实现 | 组织管理不依赖活动计费上下文；节点管理员必须提交明确节点目标，列表限于子树，移动同时校验源与新父节点，跨节点成员不向单子树管理者暴露或开放移除。 |
| ACC-65 | 组织删除就绪度查询 | 已实现 | 只读端点返回当前状态、版本、ready 和结构化跨域 blockers；实际删除再次强一致预检，不信任前端检查结果。 |
| ACC-66 | 平台组织成员稳定分页 | 已实现 | 管理端成员查询提供 q、role、cursor、limit，按加入时间与成员关系 ID 稳定翻页，并返回用户编码作为非邮箱识别事实。 |
| ACC-67 | 平台组织治理最小权限 | 已实现 | 改名、状态、成员和节点管理由四个原子权限独立门禁；兼容迁移保留既有角色能力并更新授权版本。 |
| ACC-68 | 平台组织节点移动 | 已实现 | 管理端 API 与结构树均接入结构版本 CAS；非法层级失败关闭，版本冲突刷新后要求操作者重新选择。 |

### 3.3 Token Account、路由和模型调用

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| TOK-01 | 本地独立模型不进入账号平台 | 已实现 | 用户在桌面直接配置的本地模型或第三方云模型由本地模型系统管理，不携带账号平台身份、不使用平台钱包和 Token Gateway。 |
| TOK-02 | 桌面聊天携带与个人中心一致的 OIDC 身份 | 已实现 | 启用账号平台后，Rust 运行时以当前 Access Token 调用 Gateway。 |
| TOK-03 | Token Gateway 提供 OpenAI 兼容聊天入口 | 已实现 | `POST /v1/chat/completions` 支持 JSON 和 SSE；桌面原生流式路径使用该入口。 |
| TOK-05 | 模型网关协议适配 | 已实现 | 通用领域和核对器只消费版本化 Adapter/`UsageEvidenceProvider`；New API 请求头、日志查询与用量字段不再泄漏到数据库、API 或运营 UI。 |
| TOK-06 | 模型目录自动提供模型 | 已实现 | Gateway 通过 Adapter 自动/手动同步独立目录，桌面动态消费当前可见且上下文可用的模型。 |
| TOK-08 | 首次调用自动创建默认 Token Account 和路由 | 已实现 | 平台托管、幂等创建，普通用户不可见不可改。 |
| TOK-09 | 托管路由 Provisioning 生命周期 | 已实现 | 已交付按作用域、模型、协议和期望代唯一的 Provisioning 聚合，以及 PENDING/READY/FAILED/REPAIR_REQUIRED、租约认领、指数退避、SLA 转修复和受审计人工重试。 |
| TOK-10 | 托管模型调用资格顺序闭环 | 已实现 | 已强制执行目录可见、Provisioning READY、上下文资格、请求级预占、Hold、上游调用顺序；非 READY 不进入路由候选或计费链路。 |
| TOK-11 | 请求前解析个人/组织上下文和角色 | 已实现 | Gateway 从 Account 获取服务端可信上下文，不接受客户端伪造 payer。 |
| TOK-13 | 模型和 Provider 路由确定且可审计 | 已实现 | 按作用域、默认级别和优先级选择，歧义拒绝。 |
| TOK-14 | 凭据加密且 UI 不返回明文 | 已实现 | API 只返回类型、状态、指纹、更新时间等元数据。 |
| TOK-15 | 个人中心用户自管 Token Account | 移除 | 不提供个人供应商 Key、Token Account、共享和路由管理；现有相关个人入口及公开能力从目标范围移除。平台/组织运营管理与本地独立模型配置不受此条影响。 |
| TOK-19 | 调用前按具体请求和协议动态预占 | 已实现 | 使用版本化确定性估算器、协议公式、请求输入和输出边界预占，并支持原快照续租及追加预占。 |
| TOK-22 | 支持并发对话且防超扣 | 已实现 | 每请求独立 Hold；可用余额能够覆盖即可并发。 |
| TOK-24 | 模型请求幂等 | 已实现 | 同键同指纹恢复既有流、结果或202核对状态，所有重复路径都复用原 usage_request_id。 |
| TOK-25 | 模型请求幂等键作用域 | 已实现 | 强制客户端逻辑请求键，仅保存 SHA-256 摘要并以稳定用户和固定 model-inference 命名空间唯一；Session、上下文、模型和协议不拆分作用域。 |
| TOK-26 | 同键异请求冲突检测 | 已实现 | 带版本的确定性 CHAT 请求指纹覆盖规范 JSON 语义，同键异指纹在选路/Hold/上游前返回409。 |
| TOK-27 | 模型请求幂等记录保留 | 已实现 | 非终态保留至收敛，事件24小时、结果30天、零正文墓碑90天，过期返回410且墓碑到期后释放摘要唯一占用。 |
| TOK-28 | 流式响应持久化与重放 | 已实现 | 每个规范事件在客户端确认前使用独立 AAD 加密落库、完整性校验和连续编号，支持 Last-Event-ID 重放与聚合结果。 |
| TOK-29 | 模型请求断线重连 | 已实现 | HTTP/SSE 断开只改变交付状态，上游继续；重连按活动流、终态、核对态和过期态恢复且不再次派发。 |
| TOK-30 | Hold 后上游派发恢复 | 已实现 | 加密准备快照覆盖 Hold 前崩溃，Billing 幂等 EnsureHold 后原子提升正式意图；首次 Worker 租约唯一，派发不确定且上游不保证幂等时只进 REVIEW。 |
| TOK-31 | 实际用量结算并释放差额 | 已实现 | 成功、失败和取消均只使用归一化可信 Provider 最终或累计用量；无可信用量保持待核对，不按 Hold、字节或客户端已显示量结算。 |
| TOK-32 | Gateway 自动重试模型请求 | 已实现 | 上游是否执行可能不确定，自动重试会造成重复消费；当前 Gateway 不自动重发模型请求。 |
| TOK-33 | 不确定结果自动核对和人工重试 | 已实现 | `REVIEW`/`SETTLEMENT_PENDING` 后按历史 Adapter 和上游引用退避查询；管理员重试具备独立幂等指纹且不重发模型，期限后释放未结算 Hold 并进入复核。 |
| TOK-34 | 普通用户查看用量历史 | 已实现 | 提供个人用量请求列表和详情，不暴露内部凭据。 |
| TOK-37 | 非计费客户端切换 Readiness | 部分实现 | 已提供服务身份保护的无推理 readiness、公开兼容元数据及短时 Ed25519 准入证明；证明绑定环境、制品、客户端/服务/契约/目录版本并不可变留痕。生产密钥、真实环境无副作用证据和发布系统联调仍待外部验收。 |
| TOK-38 | 用户取消模型请求 | 已实现 | 仅原稳定用户可执行持久幂等取消；派发前通过行锁阻止意图提升、释放唯一 Hold 并写 LOCAL_CONFIRMED，完成事实优先且重复取消不重复调用。 |
| TOK-39 | Provider 取消能力适配 | 已实现 | Adapter 统一声明三类取消能力；当前两个 Adapter 明确 UNSUPPORTED，因此停止交付但继续受控读取原调用并核对用量，不从断流推断取消。 |
| TOK-40 | 传输连接与模型执行状态隔离 | 已实现 | execution/cancel/delivery/settlement 独立持久，断线和 USER_STOPPED 不取消、不判失败、不释放 Hold，上游在受限后台上下文继续且原请求可重放。 |
| TOK-41 | 取消结果和实际用量核对 | 已实现 | 取消时间线、Adapter/上游引用、终态、最终用量和脱敏证据分别保存；不确定结果退避核对，期限后转 REVIEW 并进入禁止晚扣的平台损失边界。 |
| TOK-42 | 模型目录管理生命周期 | 已实现 | DRAFT/PUBLISHED/DEPRECATED/REMOVED、CAS/ETag、幂等命令、合法迁移、不可变版本和审计已经闭环。 |
| TOK-43 | 已移除模型的历史展示 | 已实现 | 连续缺失核验、目录墓碑和请求显示/能力快照均持久保存，不依赖实时 Model Gateway 读取历史名称。 |
| TOK-44 | 模型重命名和别名 | 已实现 | 稳定逻辑模型 UUID 与版本化名称/别名分离，直接身份映射结构性排除循环，并拒绝别名/规范名称冲突及 REMOVED 目标。 |
| TOK-45 | 模型目录版本回滚 | 已实现 | 以前向更高版本复制历史内容并保留来源、审计和 REMOVED 墓碑；提交前联合校验当前价格、READY Provisioning、路由代次和有效凭据。 |
| TOK-46 | 模型关联对象版本生成边界 | 已实现 | 目录、协议、能力、价格/公式和路由代次独立演进，请求冻结全部版本与显示/生命周期快照。 |
| TOK-47 | 模型下架后的会话处理 | 已实现 | 弃用/移除截止和桌面显式替代分支均保持原会话不变；紧急强制下架会锁定生效前在途请求，保存真实取消能力与尝试，并沿原 Hold、可信用量和结算状态收敛。 |
| TOK-48 | 客户端模型选择引用与作用域 | 已实现 | 类型化引用、兼容迁移、失败关闭、设置页新会话默认项及对话当前会话选择已实现；来源冲突、生命周期、退役入口、三语状态和桥接协议已有自动回归门禁。 |
| TOK-49 | Token 个人与组织原子授权 | 已实现 | 每个模型与用量入口都在进入目录、路由、Hold 或状态机前调用 Account 原子权限决策，授权不可用失败关闭。 |

### 3.4 钱包、积分、定价和权益

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| BILL-01 | 个人和组织钱包独立 | 已实现 | 钱包按 Owner 类型和 ID 唯一；组织请求不能隐式扣个人钱包。 |
| BILL-02 | 余额以不可变平衡账本为准 | 已实现 | Journal Entry 与 Posting 平衡，不能直接覆盖余额。 |
| BILL-03 | 积分使用整数最小单位 | 已实现 | `10,000 microcredits = 1 展示积分`；金额使用分。 |
| BILL-06 | 充值形成有有效期的权益包 | 已实现 | 充值按支付订单和冻结商品快照创建独立权益包及 BASE/BONUS 组成，保存起止时间和组成余额。 |
| BILL-07 | 组织权益在线购买入账 | 已实现 | 订单保存付款申请人和受益组织，支付履约再次校验组织钱包归属并创建组织权益包及组成分录，个人钱包不参与。 |
| BILL-08 | 组织成员额度手动分配 | 已实现 | 组织管理员按原子权限从精确包/组成给成员分配；无自领接口，分配、撤回、暂停/恢复、占用和消费均审计。 |
| BILL-14 | 基础积分与赠送积分分项核算展示 | 已实现 | API 与三语 UI 分项返回并展示组成总额、可用、预占、已用、过期、撤销和到期时间。 |
| BILL-15 | 消费按最早到期权益包优先 | 已实现 | Hold 分配与结算关联具体权益包。 |
| BILL-17 | 人工赠送积分有效期 | 已实现 | 每次正向人工调整创建独立 BONUS 组成，自入账起 30 天到期。 |
| BILL-18 | 权益到期处理 | 已实现 | 可审计到期 Worker 先收敛相关 Hold，再按组成写到期账本分录并保留历史。 |
| BILL-19 | 过期不确定 Hold | 已实现 | 到期释放不确定 Hold；迟到真实用量进入平台损失复核，不扣其他权益或形成负余额。 |
| BILL-21 | 定价按模型和版本管理 | 已实现 | 当前与历史价格并存，结算/核对使用对应时点价格。 |
| BILL-22 | 全局统一价格和分时段计价 | 已实现 | 全局默认和模型覆盖支持立即/未来区间、发布冲突拒绝和逐单位完整预览。 |
| BILL-23 | 价格只配置协议公式引用的计量单位 | 已实现 | 全局单位必须固定价，模型单位可固定价或显式继承；发布和运行时均按不可变公式绑定验证完整集合。 |
| BILL-24 | 输入与缓存输入互斥计量 | 已实现 | 当前 Provider Adapter 将总输入规范化为非缓存输入与缓存输入的互斥集合；跨 Provider 通用化由 APT-026 延续相同契约。 |
| BILL-25 | 零价、显式继承与缺失分离 | 已实现 | 固定零价被保存并阻止回退，模型继承显式配置，缺失稳定返回 PRICE_MISSING。 |
| BILL-26 | 人工积分调整需审批 | 已实现 | 申请、批准、拒绝和账本入账可审计。 |
| BILL-27 | 管理员查看钱包账本详情 | 已实现 | 钱包账本查询展示 Posting、科目、业务引用、lot、权益包及 BASE/BONUS 组成，并返回每个 Journal 的零和校验；钱包、Hold、调整、应收和对账均由独立权限保护且无直接余额覆盖。 |
| BILL-28 | 退款权益与账本冲销 | 已实现 | Provider 确认后同一事务冲销目标支付流水与权益包组合的全部 BASE、该支付产生的全部关联 BONUS 及对应账本负债；BONUS 零折现，人工赠送和其他支付来源不受影响。 |
| BILL-33 | 订单级拒付冲销隔离 | 已实现 | 已确认拒付证据只冲销原支付流水唯一权益包的可撤销 BASE 和关联 BONUS；不足现金本金形成订单级应收，不冲销其他权益包，应收未清时冻结对应钱包上下文。 |
| BILL-34 | 模型计费使用可信实际生成量 | 已实现 | Provider 可信用量、Gateway 最后持久化序号、各客户端确认序号和用户停止时间独立保存；缺可信用量不估算结算，不可恢复缺口进入禁止用户扣款的平台损失复核。 |
| BILL-35 | 取消期间 Hold 生命周期 | 已实现 | 派发前取消全释放，派发后继续续租并自然收敛；最大执行、最终用量延迟和核对期限按请求版本冻结，期限后释放并 REVIEW，晚到证据不得重开用户扣款。 |
| BILL-36 | 权益组成资金守恒 | 已实现 | 数据库按权益包/组成强制原始额在可用、Hold、已用、有效/暂停授予、过期和撤销桶间守恒，包汇总由触发器投影。 |
| BILL-37 | 权益组成禁止超额授权 | 已实现 | 授予只从同组织钱包、同包、同 BASE/BONUS 组成的可用桶转出；授予与组成均由数据库守恒约束保护。 |
| BILL-38 | 并发分配锁与幂等边界 | 已实现 | 分配、撤回、成员 Hold、结算、释放和到期共用精确组成/授予行锁；同键异请求稳定冲突，批量逐项派生稳定子键并独立提交。 |
| BILL-39 | 注销授予隔离至到期 | 已实现 | `ACCOUNT_CLOSED` 与普通 `MEMBER_REMOVED` 分离；隔离保持原用户、包、组成、余额和到期日，占用暂停桶且不能恢复、消费或普通撤回，到期只转过期桶。 |
| BILL-40 | 注销授予可选全额回收 | 已实现 | 授权组织管理员在 Account 强一致证明 `ANONYMIZED` 后，可对无 Hold/冻结且未过期的关闭授予执行一次性全剩余回收；单条及批量逐项具备稳定指纹、原组成守恒、不可变审计和 Outbox。 |
| BILL-41 | 模型覆盖价优先匹配 | 已实现 | 按请求时间唯一选择模型覆盖，无覆盖才选择全局，并统一 PRICE_MISSING/PRICE_AMBIGUOUS/PRICE_UNAVAILABLE。 |
| BILL-42 | 计量单位显式继承解析 | 已实现 | 逐单位 FIXED_PRICE/INHERIT_GLOBAL、同时间全局来源、固定零价和最终来源快照均已实现。 |
| BILL-43 | 未来价格区间冲突拒绝 | 已实现 | 同定价空间发布使用事务锁、ETag、幂等指纹和区间冲突校验；真实 PostgreSQL 并发执行待部署验证。 |
| BILL-44 | 定价版本生命周期与撤销 | 已实现 | 已形成 DRAFT/PUBLISHED/ACTIVE/ENDED/CANCELLED，并支持生效前取消及 ACTIVE 阻断、全局接续和替代接续。 |
| BILL-45 | 已创建 Hold 价格不可变 | 已实现 | Hold 完整冻结逐单位最终价格、继承来源、分母和舍入，续租、追加预占及核对均复用原快照。 |
| BILL-46 | Billing 个人与组织原子授权 | 已实现 | 个人充值/余额/订单按 `personal.billing.read`、`personal.recharge.create/manage` 失败关闭；组织购买、钱包与授予按显式目标组织校验 `org.entitlements.purchase`、`org.wallet.read`、`org.quota.*` 和 `org.grants.reclaim_closed`。 |
| BILL-47 | 财务支付分区运营概览口径 | 已实现 | accounts/payments/pricing/products 四端点按精确权限独立返回；支付默认 24 小时、最大 31 天 UTC 半开窗口，单位和状态集合固定，失败原子 503；测试覆盖重复支付证据、价格互斥、墓碑、准入与活动绑定缺失。 |

### 3.5 充值、支付 Provider 与新华三商城

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| PAY-01 | 支付实现位于独立 Adapter | 已实现 | Billing 仅依赖中立支付端口，H3C 和开发 Simulator 分别在平台 Adapter 内解释厂商协议与状态。 |
| PAY-02 | 渠道和动作由能力声明驱动 | 已实现 | 版本化能力统一驱动公开发现接口、商品渠道、前端、轮询间隔、权威终态、关单、退款、通知、账单和修复，并冻结到订单。 |
| PAY-03 | 多支付渠道 | 已实现 | 可售渠道取商品、映射和当前 Provider 能力交集；开发测试渠道不进入生产能力或生产部署。 |
| PAY-04 | 多 Provider 商品映射 | 已实现 | 外部 SKU 已从通用商品分离到不可变版本映射，订单固定创建时映射 ID、映射事实和 Provider 能力。 |
| PAY-05 | 充值商品正式冻结 | 未实现 | v0.3 定价与权益已进入待验目录，但映射仍为 `TEST_ONLY` 且生产失败关闭；APT-048 继续等待商城实际商品与金额联调验收后发布 `FROZEN` 版本。 |
| PAY-06 | 个人用户与当前 Commerce Provider 客户一一对应 | 已实现 | 当前 Provider 使用用户级唯一 Mapping；充值单独查询 READY 状态，未完成不影响登录、模型调用或组织授权。 |
| PAY-07 | 充值全程留在品悟应用内 | 已实现 | 商品选择、待支付列表、二维码弹窗和结果均留在品悟界面，不跳商城后台网页。 |
| PAY-08 | 商品选择与待支付订单恢复 | 已实现 | Web 不自动恢复旧二维码；只允许用户点击仍可付款且未超期的订单后重新查询并打开二维码弹窗。 |
| PAY-09 | 服务端按 product_id 派生 SKU、金额和积分 | 已实现 | 客户端不能覆盖金额、SKU 或权益。 |
| PAY-10 | 二维码和支付金额完全采用商城返回事实 | 部分实现 | Provider 不本地生成二维码；查询和原生回调共同读取外部二维码、订单引用与金额，PAID 缺少真实实付金额会失败关闭；仍需部署后的真实支付和商品事实联合验收。 |
| PAY-11 | 支付流水与权益包履约隔离 | 已实现 | payment_order 1:N payment_transaction，支付来源权益包唯一绑定 source_payment_transaction_id；不同成功流水分别独立履约或进入重复/晚到支付复核。 |
| PAY-13 | 支付领域独立状态机与订单派生摘要 | 已实现 | 支付流水、退款、拒付和关单事实分离；订单四类摘要从底层事实重建，并保持应收与人工复核优先级。 |
| PAY-15 | 支付回调与主动查询 | 已实现 | H3C 以专用 Basic 凭据换取短期 Bearer Token 后投递回调，通知先持久化再返回 `202`；`account.pinvou.com/api/v1` 下两个精确入口已在 Nginx 模板代理到 Billing，回调和主动查询使用同一不可变证据归并器，服务端查询继续补偿漏回调。 |
| PAY-16 | 客户端关闭后订单仍继续收敛 | 已实现 | 查询任务运行在 Billing 服务端。 |
| PAY-17 | 超时订单不永久停留普通 PENDING | 已实现 | 订单过期后继续服务端退避查询；72小时仍无终态时转入唯一人工审核案件，订单不再表现为普通 PENDING。 |
| PAY-18 | 主动关闭未支付订单 | 已实现 | Provider 返回的精确最后付款期限（新华三 `payEndTime`）规范化后冻结到订单，缺失才使用能力窗口；期限到达后先末次查询，仍未付款即提交本地 `EXPIRED`、清除支付指令并退出待付款，Provider 外部关单仅作为独立证据继续收敛，不阻塞本地终态。 |
| PAY-19 | 主动查询退避与72小时边界 | 已实现 | 创建订单冻结最小查询间隔、支付窗口和72小时边界；本地到期后查询间隔至少5分钟，边界任务先完成最后一次查询，再清除高频调度并幂等创建案件。 |
| PAY-21 | 支付后自动更新额度 | 已实现 | 每个确认支付流水至多创建一个独立权益包、BASE/BONUS 组成和账本入账，重复证据不重复更新额度。 |
| PAY-22 | 企业管理员在线购买组织权益 | 已实现 | 有 `org.entitlements.purchase` 权限的企业管理员可在组织上下文购买；订单保存付款用户与受益组织，到账后只增加组织权益。 |
| PAY-24 | 退款目标绑定支付流水和权益包 | 已实现 | 请求和执行均保存并校验 payment_transaction_id、entitlement_package_id 与订单归属；payment_order_id 只作归属和审计。 |
| PAY-26 | 退还目标全部剩余可退金额 | 已实现 | 不接受退款金额输入；现金退款按目标流水整笔金额执行，关联 BONUS 零折现但成功时全部撤销，主动退款状态机无 PARTIALLY_REFUNDED。 |
| PAY-27 | 退款意图原子事务 | 已实现 | 串行事务完成锁定、资格复查、组成/授予冻结、金额与快照预约、请求和 Outbox；提交后由仅允许显式幂等 Provider 的 Consumer 使用稳定退款号执行并按 JetStream 退避恢复。 |
| PAY-30 | 退款同步冲销 | 已实现 | Provider 确认后同一 Billing 事务冲销目标 BASE/关联 BONUS、来源批次与钱包负债，写平衡账本并更新退款和权益终态；其他来源不受影响。 |
| PAY-32 | 渠道部分强制退款异常 | 已实现 | 已发生外部强制退款先保存独立证据和统一案件、绑定原流水/订单/权益包并冻结；受审计确认后按现金比例定向撤销该包 BASE/关联 BONUS，BASE 缺口形成应收，不经过普通退款预检且不覆盖支付成功。 |
| PAY-33 | 支付异常人工复核 | 已实现 | 七类支付异常统一进入不可变证据案件，保留各领域独立状态；受控结论可定向执行拒付/强制退款冲销或进入财务修复计划，并完整保存责任人、结论和审计。 |
| PAY-34 | 每日四方账单对账 | 已实现 | 每日按上海自然日获取 Provider 账单，核对订单/支付流水、退款/拒付和 Billing 账本；手工、定时及文件导入运行相互独立且可重跑。 |
| PAY-35 | 账单能力降级 | 已实现 | 能力返回 API、FILE_IMPORT 或 QUERY_DEGRADED；文件逐行受控导入并不可变保留，完全无账单时只称查询快照降级核对，同时执行正式收款准入与72小时人工审核。 |
| PAY-36 | 对账差异闭环 | 已实现 | 每条差异保存不可变分类、严重度和四方快照；认领、分配、修复计划、解决、忽略与重开均通过幂等命令写入不可变审计，不提供直接改表入口。 |
| PAY-37 | 72小时未收敛订单转人工审核 | 已实现 | 边界查询后只创建一条P2案件并告警待认领队列，前7天每日、之后每周复核；每次查询结果留证，支付成功经同一流水归并器一次履约。 |
| PAY-38 | 无回调无账单 Provider 正式收款准入 | 已实现 | FROZEN 商品创建订单前检查历史查询、保留期、有限支付/晚到窗口和权威未支付终态；能力版本冻结到订单，测试映射不被描述为正式能力。 |
| PAY-39 | 未支付订单最终归档 | 已实现 | `final_archive_eligible_at` 由窗口与宽限期确定；期限后成功权威未支付查询、无冲突/支付/未复核申诉才可原子归档，订单和证据永久保留。 |
| PAY-40 | Provider 事实盲区处理 | 已实现 | 到归档期限仍无成功权威查询自动升级 `PROVIDER_BLIND` 并停用新单；人工也可留原因升级，案件继续显示且不能按普通未支付归档。 |
| PAY-41 | 商城客户使用永久用户标识 | 已实现 | 创建订单和按用户分页查询传付款人的不可变 `user_id` 作为 `merchantCustomerKey`；商城返回值与订单冻结付款人再次核对。 |
| PAY-42 | 新华三查询与回调共用原生订单合同 | 已实现 | H3C 回调严格接收 `queryOrder.content` 原生对象，主动查询和回调复用 DTO、映射和证据归并器；新增未知状态进入人工复核。 |
| PAY-43 | 商城十进制金额精确校验 | 已实现 | `needPayAmt`、`paidAmt`、`skuAmt` 不经浮点精确转分；成功状态缺少正实付额、时间、流水或与本地快照冲突时拒绝履约。 |
| PAY-44 | 受控确认 Provider 失败 | 已实现 | 本人已认领案件经现场查询和无资金事实门禁后原子结案，保存不可变确认、命令和事件；晚到成功事实仍可继续归并。 |
| PAY-45 | 商品目录与支付止损分离 | 已实现 | 自动止损写独立 Provider/商品控制事实，不再篡改目录上架状态；创建订单按两层控制失败关闭。 |
| PAY-46 | 支付能力双人恢复 | 已实现 | Account 双人审批与 Billing 执行门禁共同校验版本、Provider 现场查询、案件、映射和人员隔离，成功后原子恢复并保留不可变证据。 |
| PAY-47 | 支付暂停对用户可见 | 已实现 | 商品列表保留暂停项并返回稳定暂停层级与原因码。 |
| PAY-55 | 订单与商品分级止损 | 已实现 | 单案件保持 L1；仅 PAYMENT_FACT_CONFLICT/ILLEGAL_TRANSITION 受信规则可在止损 SLA 后关闭目标商品，任何单案件均不写 Provider 控制。 |
| PAY-56 | Provider 持久聚合止损 | 已实现 | 仅新单关键分区的多订单连续失败、失败率、跨独立路径或跨商品共同故障写持久 Provider 控制。 |
| PAY-57 | 支付健康观察与确定性评估 | 已实现 | 所有 Provider 调用通过前置意图和后置观察持久化，重复逻辑尝试、业务拒绝和观察缺口具有独立语义。 |
| PAY-58 | 分层恢复与历史控制迁移 | 部分实现 | 现有恢复命令联动打开两层控制，尚未完成独立恢复和历史控制复核迁移。 |

### 3.6 个人中心与运营后台

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| UI-01 | 桌面用户入口常显，未登录在弹层内登录 | 已实现 | 不跳外部浏览器；个人信息也在弹层中展示。 |
| UI-03 | Web 个人中心条件式四 Tab 信息架构 | 已实现 | 个人上下文保留“额度与用量 / 订单与发票 / 账号设置”，活动组织且具备读取权限时增加“我的组织”；组织购买、额度、成员、节点和模型策略从额度页及账号设置迁入该页，其他账号、订单和展示精度规则保持不变。 |
| UI-04 | 普通用户界面不暴露内部结算/供应商运维信息 | 已实现 | 模型准备、内部结算状态、供应商凭据等从普通用户视图移除。 |
| UI-05 | Web 资料修改与桌面密码恢复边界 | 已实现 | Web Portal 提供用户名/手机号修改并由 Identity 强制重新认证，不接收密码；认证后使用根路径回到对应修改弹窗，所有发起端不再生成已删除的 `/account` 页面地址。桌面 Popover 无资料修改区域，但保留忘记密码模式及专用 Bridge/Tauri 密码重置命令。 |
| UI-06 | 个人中心只展示活动上下文额度 | 已实现 | 上下文切换和跨端刷新会先清空旧余额、权益、订单与用量；个人/组织数据按新的服务端活动上下文重新加载。 |
| UI-07 | 跨端活动上下文同步 | 已实现 | Web 响应头和桌面 Tauri 响应都传递 context_version；更高版本、聚焦及切换成功都会终止旧请求并刷新。 |
| UI-08 | 权益包、到期与退款说明 | 已实现 | 桌面与 Portal 三语展示总可用、BASE/BONUS 分项、来源、消费顺序和到期提醒，并明确 BONUS 零现金退款及普通退款关联撤销。 |
| UI-09 | 桌面跳转 Web 个人中心 | 已实现 | 登录后的账号 Popover 通过专用命令打开固定 `https://account.pinvou.com/`，不接受任意外链且不携带 Token；旧官网路径未保留。 |
| UI-10 | Web 个人充值入口 | 已实现 | 新订单二维码使用弹窗；套餐区列出仍可付款订单，支付完成、超时或其他不可付款状态立即移除。 |
| UI-11 | Web 我的组织管理入口 | 已实现 | 活动组织的购买、额度、成员、节点和模型策略集中在“我的组织”，各区块按既有原子权限独立显示和加载；额度页只读，账号设置保持个人账号职责。 |
| UI-12 | 退款入口屏蔽与支付能力降级展示 | 已实现 | 用户端无新建入口；运营动作同时要求独立产品门禁和 Provider 退款能力，所有交付配置默认关闭，未来 Provider 声明能力也不会自动展示。 |
| UI-13 | 模型停止与结算状态反馈 | 已实现 | 三语用量页提供显式停止并同时展示 execution/settlement；不支持取消时明确后台继续和按可信实际用量结算，重开后读取同一请求真实状态。 |
| UI-14 | 注销进度与阻断原因展示 | 已实现 | 桌面与 Web 个人中心三语展示持久阶段、状态、SLA、稳定阻断原因和用户动作；响应删除内部资源 ID 并自动刷新，未完成前不显示完成。 |
| UI-15 | 未完成注册恢复状态展示 | 已实现 | 桌面优先在系统凭据库保存操作查询凭据，重启后按 `next_retry_at` 轮询，区分 Identity、Account、钱包、修复与补偿状态并阻止重复注册。 |
| UI-16 | 桌面与 Web 三语完整性 | 已实现 | 桌面账号弹层、Portal 个人中心和管理后台的简中/英文/日文词条执行双向精确键集、类型和非空检查；稳定错误映射不再回显 Provider/后端原文，源码单语硬编码和 Portal 构建制品缺失均阻断发布。 |
| UI-17 | 账号 Popover 职责收敛 | 已实现 | 已移除平台模型目录请求、下拉和默认值写入，同时保留账号、额度、充值、Web 个人中心和退出登录。 |
| UI-18 | 设置中的平台模型默认项 | 已实现 | 已按服务端目录显示具体平台模型、生命周期和不可选状态，选择只改变空白新会话默认值，并与本地/第三方配置分组隔离。 |
| UI-19 | 对话内直接选择平台模型 | 已实现 | 对话选择器直接列出上下文可用的平台目录模型并与本地/第三方分组；选择只写当前会话，草稿态延迟冻结且不改全局默认。 |
| UI-20 | 充值商品暂停态 | 已实现 | Web、组织购买和桌面入口展示暂停态及三语维护提示且不可提交。 |
| UI-21 | Web 统一身份登录页 | 已实现 | Portal 未登录后直接进入 Keycloak 三语 Pinvou 登录主题；密码登录、短信滑块验证码登录及手机滑块验证码注册均在 Identity 边界闭环，Portal 不接收密码或浏览器可读 Token。 |
| UI-22 | 权限驱动入口状态 | 已实现 | 桌面、个人中心和后台直接消费权威权限集合，按个人/平台/组织独立授权版本刷新入口、只读态及操作按钮；退出登录和注销进度始终可达。 |
| UI-24 | 个人中心 URL 与 Tab 状态一致性 | 已实现 | 无合法 section 默认进入额度与用量；`myOrganization` 按活动组织及读取权限失败关闭，深链、授权变化、Tab 点击和浏览器前进/后退均保持 URL、高亮与内容一致。 |
| UI-25 | 普通个人中心角色信息收敛 | 已实现 | 普通个人中心已删除角色授权挂载点、制品和请求，不展示原始角色信息；后台角色治理保持可用。 |
| UI-26 | 权益包渐进式展示 | 已实现 | 默认最多五条有效包摘要并汇总数量和余额，主页面去除重复资料卡及列表整体边框，只以横向分隔线区分条目；完整列表、历史状态、BASE/BONUS 组成和支付关联进入可访问详情弹窗。 |
| UI-27 | 全状态订单摘要与详情 | 已实现 | 全部充值订单按创建时间稳定倒序展示响应式摘要；本地超时与兼容关闭观察统一显示已超时且不恢复付款入口，桌面八列、中宽精简、移动卡片及详情弹窗保持可访问。 |
| UI-28 | 统一管理后台壳层与共享基础组件 | 已实现 | 已在 CLOUD_UAT 部署 HTML 原型的工作台、身份与组织、财务与支付、模型与 Token、治理与安全五组导航和页面命名；URL 深链、浏览器历史、共享状态、三语、键盘和响应式边界保持有效。 |
| UI-29 | 身份、用户、角色与组织后台界面改版 | 已实现 | 用户管理补齐游标分页、用户名/展示名/精确 UUID 搜索、独立用户编码查询、停用安全确认、当前账号保护、核心详情及注册/资料/注销异常工作台；因当前账号生命周期不采集或维护邮箱，用户目录、详情和搜索不展示或使用邮箱；角色与权限页集中展示唯一角色目录、权限清单和引导式授权，目录将节点管理员归入组织类但授权仍保留节点作用域，用户与组织详情不再重复角色定义，Keycloak 平台级入口不与用户目录混合，“我的组织”因无真实后台接口而隐藏；已于 2026-09-01 部署至 CLOUD_UAT `0.8.0@13e22858`。 |
| UI-30 | 财务、支付、审批、定价与充值商品后台界面改版 | 已实现 | 计费运营、支付与资金、定价与商品、审批中心成为独立页面，继续复用原 API；旧财务聚合 Tab 已删除。 |
| UI-31 | 模型、Token、系统与审计后台界面改版 | 已实现 | Token Gateway、审计与导出、系统与安全按原型命名；Keycloak 受控入口归入系统与安全并与系统状态独立按权限显示，只读运行手册作为页面内容保留，不新增导航组或公网执行能力。 |
| UI-32 | 管理后台跨页面集成与发布验收 | 已实现 | 验收覆盖 HTML 原型五组信息架构、独立 URL、源码/制品设计 Token、旧六组与旧配色禁用、精确权限、不可用能力隐藏、三语、读屏和响应式。 |
| UI-33 | 组织目录可扩展交互 | 已实现 | 列表按 25 条游标分页并支持状态筛选和 URL 恢复；创建入口按需展开，初始所有者候选随用户名、显示名、用户编码或完整 ID 远程搜索。 |
| UI-34 | 独立组织详情深链 | 已实现 | 详情占用主内容区并由 organization_id/organization_tab 深链恢复，浏览器前进后退和返回目录均保持 URL 状态；组织详情不再使用模态抽屉。 |
| UI-35 | 紧凑成员权限工作区 | 已实现 | 成员以紧凑表格分页展示；添加成员使用远程用户搜索，角色与节点范围仅在选中单个成员后打开编辑抽屉。 |
| UI-36 | 组织删除交互闭环 | 已实现 | ACTIVE 不出现删除入口；DISABLED 的删除面板展示就绪度与结构化阻断，要求名称和原因双确认，并保留可重试错误与 request_id。 |
| UI-37 | 财务支付三分区信息架构 | 已实现 | 三分区名称、子页归属、权限边界和 `finance_tab` 稳定深链已在源码、三语目录、原型及内置 Portal 制品中一致实现。 |
| UI-38 | 手工调账申请与审批下穿 | 已实现 | 调账创建、审批提交、部分失败恢复、审批状态/申请 ID 展示和目标过滤深链已闭环；批准、拒绝、撤回及执行仍由审批中心和服务身份负责。 |
| UI-39 | 支付渠道控制与恢复申请 | 已实现 | Provider/商品分层控制证据、独立恢复申请、幂等重试、DRAFT 续提、进行中审批状态和审批中心下穿已闭环；恢复决策不在财务页执行，未知审批状态保持只读。 |
| UI-40 | 财务支付子页独立状态 | 已实现 | 各子页拥有独立 loading/error/loaded/updatedAt、数据缓存和刷新入口；切页复用成功结果，筛选快照随结果恢复，旧响应与单页失败不会覆盖其他子页，商品目录按可见性延迟加载。 |
| UI-41 | 既有筛选与充值商品稳定分页 | 已实现 | 应收、支付订单、晚到支付和支付异常筛选及选中子页可由 URL 恢复；充值商品筛选、cursor、上一页历史与浏览器前进后退已闭环，旧响应不会覆盖新页，界面不伪造 total。 |
| UI-42 | 财务支付结构化操作交互 | 已实现 | 财务、支付、商品和审批决定均使用结构化面板；幂等写、未知结果刷新恢复、三语错误及无障碍状态已闭环，并移除相关浏览器 prompt/confirm。 |
| UI-43 | 旧模型价格写入口收敛 | 已实现 | 旧直接激活链路已完整移除；迁移视图只读展示已发布价格事实和明确生命周期状态，不混入草稿，也不替代新版草稿、预览、发布与转换。 |
| UI-44 | 管理端计量单位与模型定价生命周期 | 已实现 | React 管理界面已接入完整生命周期；精确价格空间投影、模型目录选择、权限复核、不可变发布事实及非幂等未知结果恢复均有合同覆盖。 |
| UI-45 | Billing 全量分页 Portal 适配 | 已实现 | 钱包/账本、应收、订单、退款、调账、Hold、晚到支付、支付异常和支付对账已接入稳定 cursor、完整筛选和 URL 恢复；详情返回保留目录上下文，410 需显式从首屏重新核验，旧服务响应安全降级且不显示伪造总数。 |
| UI-46 | 财务支付分区运营概览界面 | 已实现 | 三个分区分别消费服务端聚合，定价与商品区块独立加载；指标展示单位、状态口径与点时/UTC 时间窗，只在筛选等价时精确下穿。403 隐藏、局部失败不伪装为 0，请求竞态、离页取消、三语、键盘及响应式交互均有自动化回归。 |
| UI-47～UI-52 | 模型与 Token 一期重构 | 已实现 | UI-47～UI-52 均已实现；UI-49 的 APT-122-R2 和 UI-50 的 APT-125-R1 整改均通过身份 B 最终审核，协议/公式及路由命令通用事务、旧入口旁路关闭和 Portal 路径回归已闭合。生产激活仍归 APT-138，不将已实现等同已部署。 |
| UI-53～UI-58 | 模型与 Token 二期运营治理与灰度 | 部分实现 | UI-53 模型独立详情、UI-54 Token Account 详情/隔离凭据、UI-55 生命周期依赖、UI-56 Billing 共享价格入口及 UI-57 目录差异/运行趋势已分别由 APT-132～APT-136 完成独立代码审核；UI-58 发布工具链已分组交付、持续 CI 代码与隔离门禁已验证，实际 N/N-1 灰度仍待环境验收，当前部分实现。二期全部改动均未推送、未部署。 |
| UI-59 | 模型与 Token 分区归属概览 | 已实现 | 需求/设计与 APT-140 任务合同已获 B 最终 PASS；依赖的 APT-139 真实投影已完成并获 B 最终 PASS。APT-140 Portal/UI 经首轮整改后已获 B 第二轮代码审核 PASS，当前尚未推送或部署。 |
| ADM-01 | 首个超级管理员安全初始化 | 部分实现 | 云端 UAT 已部署离线安全引导代码，但运行库当前为0个有效/待激活管理员；正式密码须由用户控制终端设置，生产等价并发及故障演练仍缺失。 |
| ADM-02 | Break-glass 恢复 | 部分实现 | 云端 UAT 当前无 Break-glass 或告警记录；首次初始化不得使用恢复命令，需先正常激活管理员，再做真实事故、告警投递、到期回收和交接演练。 |
| ADM-03 | 服务身份权限 | 部分实现 | 云端 UAT 已验证真实 OIDC/JWKS 和独立 Relay 容器，但仍共用静态 HTTP 令牌；正式集群投射身份、轮换、撤销与权限故障演练未完成。 |
| ADM-04 | 用户与组织分为独立页签 | 已实现 | 支持列表、搜索、详情和生命周期操作。 |
| ADM-05 | 角色与成员权限管理 | 已实现 | 运营端具备绑定查询、有效期、来源、普通平台/组织角色授予与单条撤销；超级管理员和所有者仍走专用安全流程。 |
| ADM-06 | 角色授权自动回收 | 已实现 | 临时授权按数据库时间即时停止参与鉴权，worker 原子固化到期、刷新缓存版本并记录审计/Outbox。 |
| ADM-07 | 审批角色兼任和交叉审批 | 已实现 | 角色可兼任且不同用户可交叉审批；服务端按稳定用户 ID 禁止自审，强制审批命令只需一名独立审批人。 |
| ADM-08 | Keycloak 安全管理入口 | 已实现 | 不嵌入普通后台；通过受限网络/隧道访问。 |
| ADM-09 | 审计筛选、详情和导出 | 已实现 | 筛选和详情继续服务端查询；导出由服务端任务生成，执行近期认证、默认脱敏、PII临时授权、水印、加密、一次性下载和任务配额。 |
| ADM-10 | 系统运行状态 | 已实现 | 运行时注册表配置实际服务和探针 Adapter，页面展示健康、Relay 积压、核对任务和最近错误。 |
| ADM-11 | 组织模型业务策略管理边界 | 已实现 | 组织后台仅管理显式目标组织的允许/拒绝模型策略和目录资格提示，不返回或接受真实 Token Account、路由、凭据或 Provisioning 字段；Gateway 将策略与平台 READY 路由共同计算资格。 |
| ADM-12 | 计量单位定义管理 | 已实现 | 支持版本化定义、停用和不可变历史引用保护。 |
| ADM-13 | 价格版本管理 | 已实现 | 支持全局/模型、固定价/显式继承、逐单位预览、生命周期、并发发布冲突和既有 Hold 保护。 |
| ADM-14 | 钱包状态查询 | 已实现 | 已有钱包详情；生产授权过滤和部署行为仍未验证。 |
| ADM-15 | 钱包账本详情查询 | 已实现 | 已有 Journal/Posting 查询，数据库约束保护已入账分录不可变。 |
| ADM-16 | 应收管理 | 已实现 | 运营人员可查询独立应收、清偿分配和钱包上下文冻结状态；清偿只能经不可变账本事务分配。 |
| ADM-17 | 人工积分调整 | 已实现 | 调整目标、钱包、金额和版本被冻结；独立审批后由 Billing 以稳定执行键写一次平衡账本，直接批准路径失败关闭。 |
| ADM-18 | Hold 运营 | 已实现 | 可查询 Hold 的价格/请求/用量快照、组成与成员授予分配、当前租约和不可变租约历史；异常释放限 REVIEW 或已过期 ACTIVE，要求原因和幂等键并保存操作证据。 |
| ADM-19 | 支付订单运营 | 已实现 | 支持订单查询、同步、受审计修复和明细查看；明细包含付款人、受益方、多笔支付流水、不可变证据及订单派生摘要。 |
| ADM-20 | 退款只读与异常运营 | 已实现 | 后台保留普通/强制退款、证据和目标订单明细查询；独立产品门禁在 UI 与 API 双层阻断新建普通退款，不影响既有请求收敛和被动异常处置。 |
| ADM-21 | Billing 对账管理 | 已实现 | 三语运营页可运行、导入、重跑并查看四方对账，认领或分配差异责任人，登记修复动作及复核结论；不支持账单的 Provider 继续按 capabilities 明确降级。 |
| ADM-22 | Billing 修复执行 | 已实现 | 当前修复动作均绑定稳定命令类型、异常目标、原因、脱敏证据、快照和幂等键；财务事实修复进入审批，证据收敛修复只审计。 |
| ADM-23 | 模型协议定义管理 | 已实现 | 后台提供独立版本化协议定义管理。 |
| ADM-24 | 协议预占公式版本管理 | 已实现 | 支持安全公式校验、预览、发布、Billing 绑定同步和不可变历史。 |
| ADM-25 | Token Account 生命周期管理 | 已实现 | 平台模型运营员可创建、查看和启停 PLATFORM 管理账号，平台资源过滤与角色独占权限已闭环。 |
| ADM-26 | 凭据元数据查询 | 已实现 | 仅按平台超级管理员或模型运营原子权限返回最小凭据元数据，不返回秘密、密文、Key ID 或 Provider 地址。 |
| ADM-27 | 凭据有效性检查 | 已实现 | 独立权限和审计命令触发检查，只返回统一状态、稳定脱敏错误码及凭据元数据。 |
| ADM-28 | 模型路由管理 | 已实现 | 路由 CRUD、确定性选择和模型目录/Provisioning 代次已联结，失配资源不能进入数据面。 |
| ADM-29 | Provisioning 状态查询 | 已实现 | 平台模型管理员可查看代次、尝试、退避、SLA、租约和脱敏错误的独立列表及详情。 |
| ADM-30 | Provisioning 修复 | 已实现 | 受审计、幂等的修复命令只从失败终态开启新轮次，由协调器重建并验证后推进 READY。 |
| ADM-31 | 用量请求与证据查询 | 已实现 | 可按请求读取上下文、请求指纹、Adapter/上游引用、四类状态、持久化/确认序号和脱敏取消证据，不返回正文或秘密。 |
| ADM-32 | 用量核对人工重试 | 已实现 | 人工重试要求幂等键并冻结原因指纹，只查询既有上游证据；已进入禁止用户扣款的平台损失案件不能重开收费路径。 |
| ADM-33 | 晚到支付人工审核管理 | 已实现 | 三语页面支持完整筛选、认领、受审计 Provider 查询、复核安排、严格归档和事实盲区升级，并展示能力/策略快照、证据及操作者时间线；不能手工录入已支付。 |
| ADM-34 | 注销 Saga 修复 | 已实现 | 管理台三语注销队列和受独立权限、审批策略、幂等命令保护的当前阶段重试已实现；API 不提供强制完成、忽略阻断或管理员直删用户。 |
| ADM-35 | 注册 Saga 修复 | 已实现 | 三语修复页通过受限 BFF 访问权限保护的脱敏队列，可按状态、阶段、责任域、年龄和 SLA 过滤并重试确定阶段；补偿按钮及命令受24小时和无 Account/映射/钱包/Outbox 事实证明约束。 |
| ADM-36 | 管理命令审批分类注册 | 已实现 | 版本化策略按稳定命令类型覆盖当前后台写入口；未知命令、未知版本和分类冲突失败关闭。 |
| ADM-37 | 强制审批命令清单 | 已实现 | 九类人工财务事实变更固定要求一名独立审批人并禁止自审；仅已有领域执行器的类型可申请，其余失败关闭。 |
| ADM-38 | 仅审计命令清单 | 已实现 | 普通退款、组织删除、治理配置和确定任务集中登记为仅审计，异常改账必须使用独立强制审批命令。 |
| ADM-39 | 审批请求生命周期 | 已实现 | 持久不可逆状态机支持24小时默认期限、5分钟执行窗口、撤回、过期、CAS竞争、新 ID 重提与 supersedes 关联。 |
| ADM-40 | 审批快照与资源变更失效 | 已实现 | 命令指纹、关键版本、财务/策略/授权快照不可变，审批或执行发现变化时结构化失效。 |
| ADM-41 | 执行前权限重新校验 | 已实现 | 执行前绕过缓存复核双方用户、绑定、权限、有效期和授权版本；事实不可确认时失败关闭。 |
| ADM-42 | 已批准命令安全执行 | 已实现 | 批准只写 Outbox 授权；Billing 受限执行端使用稳定幂等键产生一次领域效果，原命令失败可重试，任何命令变化必须新申请。 |
| ADM-43 | Provider 失败确认 | 已实现 | 后台命令只提交案件、原因和幂等键，现场 Provider 查询和资金门禁由 Billing 服务端控制。 |
| ADM-44 | 支付渠道控制与双人恢复 | 已实现 | 后台提供控制快照和冻结恢复草稿，既有审批队列负责申请、独立审批、失效与执行状态。 |
| ADM-45 | 系统角色精确真值表 | 已实现 | 九类系统角色按审定权限集合全量替换，存量有效绑定同步递增授权版本。 |
| ADM-46 | 策略驱动审批权限 | 已实现 | 可见范围、允许动作、决定和执行前复核逐申请使用冻结策略权限，专用审批权限不再被通用财务权限替代。 |
| ADM-47 | Token 管理平台隔离 | 已实现 | 普通 Token Account/路由/凭据接口未注册且不在公开合同中；平台资源只由 `/admin/v1/*` 管理，存量非平台凭据由可审计迁移冻结隔离。 |
| ADM-48 | Break-glass 可信操作者 | 已实现 | CLI 只从受控描述符读取签名运维凭据，服务端校验受众、主体、目标、时效和唯一凭据 ID；自报身份参数已删除。 |
| ADM-49 | 短期人类委托 | 已实现 | Account 签发受众/权限/目标/时效/请求指纹及授权版本绑定的 Ed25519 委托；Billing 验签消费并以服务主体、委托人、委托 ID 和授权版本记录不可变额度审计。 |
| ADM-53 | 支付分级止损后台治理 | 部分实现 | 后台已有控制读取与 Provider 恢复草稿，待展示分层评估/gap 并拆分商品和 Provider 恢复。 |
| ADM-54 | 定价与计量管理命令策略注册 | 已实现 | 六个现有 Billing 管理命令已按路由一一注册为 `AUDIT_ONLY`，请求权限固定为 `platform.pricing.manage`，并由追加迁移持久化相同不可变策略。 |
| ADM-55 | 超级管理员完整平台管理权限 | 已实现 | 超级管理员默认具备全部常驻平台管理权限；未脱敏 PII 导出继续使用独立临时角色，高风险操作不绕过审批、审计、脱敏与不可回显约束。 |
| ADM-56～ADM-65 | 模型与 Token 管理合同增量 | 部分实现 | ADM-56～ADM-63、ADM-65已分别完成原交付与具名整改审核；Token/Billing读投影仍独立且非跨域原子。ADM-64/APT-138已通过的持久activation/reader/事务guard、Account校验、三审批consumer/NATS与双worker生产总装证据保留，但Account审批链15分钟年龄门禁由GSA-02确认尚未闭合；代码由GSA-13/14唯一实现，APT-138-R1只负责证据关闭。当前NOT_DEPLOYED，未生产激活；APT-137真实灰度、N/N-1与部署边界不变。 |
| ADM-66 | 模型与 Token 页面级只读概览投影 | 已实现 | APT-139 已完成 OverviewRead、三个页面级投影、无权组零 SQL、稳定错误/限流、生产 SQL 性能门禁和旧入口回归，并获身份 B 最终独立审核 PASS；隔离 PostgreSQL 具名集成多轮 PASS/no-SKIP，生产规模限制环境性能待远端 CI。当前未推送或部署。 |

## 4. 非功能需求

本节展开仓库当前实现和剩余边界；规范状态、证据与部署结论以第 0 节为准。

| target_requirement_id | 需求 | 当前实现/边界 |
|---|---|---|
| NFR-01 | 安全传输 | 云端 UAT 已验证三个公开边界的 HTTPS、受信证书、状态及14天续期窗口；正式 production-new 入口和续期演练仍未完成。 |
| NFR-02 | 最小暴露 | Compose/Helm/反向代理模板将数据库、Redis、NATS、Relay、内部 API、New API 管理面和 Keycloak Admin 保持非公开；部署证据仍为 `NOT_DEPLOYED`。 |
| NFR-03 | 密钥管理 | production-new 使用 ExternalSecret + 远程 KMS/Vault 包络加密；本次云端 UAT 仍为本地 KEK，真实 SecretStore、全表重加密、旧密文归零和旧 Key 停用仍待外部环境验收。 |
| NFR-04 | 日志敏感信息保护 | 共享结构化 Handler 使用稳定事件/字段白名单，未知消息折叠并丢弃原始错误与受保护字段；注入秘密标记和绕过源码扫描已进入全量发布门禁。 |
| NFR-05 | 审计 | 已实现：管理、资金、授权、路由、恢复和服务任务写入不可变或追加式审计，死信重放也记录服务身份、任务运行和原因。 |
| NFR-06 | 幂等 | 已实现：当前目标写路径均使用稳定命名空间、规范请求指纹、事件 ID 或领域唯一键，同键异请求拒绝。 |
| NFR-07 | 最终一致性 | 已实现：事务 Outbox/Inbox、版本门禁、至少一次 Relay、死信与摘要绑定重放闭环，不宣称跨库原子事务。 |
| NFR-08 | 全局上下文一致性 | 已实现用户级唯一上下文、数据库单调版本、期望版本 CAS、标准响应头和桌面/Web 缓存失效；生产多端并发仍待部署验证。 |
| NFR-09 | 数据迁移 | 云端 UAT 已在五类快照校验后按域迁移并复核运行库版本；production-new 快照凭证和生产迁移摘要仍待验收。 |
| NFR-10 | 可恢复性 | 云端 UAT 已使用独立版本目录、稳定链接和受限备份；隔离恢复、RPO/RTO 计时与周期演练仍为外部阻塞。 |
| NFR-11 | 高可用 | 生产 Chart 已强制独立故障域、多副本、PDB、零不可用滚动和外部 HA 依赖；本次云端 UAT 为全组件单实例，真实集群容量与无损切换仍为外部阻塞。 |
| NFR-12 | 工作负载身份 | 已实现（待环境验收）：正式模板以 Subject 到精确 `internal.*` 权限清单替代宽路径前缀，内部路由均声明所需权限；云端 UAT 的 issuer/JWKS 可达，但运行时仍为共享静态令牌且 NATS 无 Operator 权限，投射令牌、委托公钥与 NATS 凭据轮换撤销证据由 APT-047 在正式集群补齐。 |
| NFR-13 | 个人信息去标识化与保留 | 已实现 Identity 删除证明后的随机墓碑和标识释放；用户 ID/编码与法定内部引用保留，三年分类记录、法律保全开关及到期自动清除状态独立持久化。 |
| NFR-14 | 短信供应商可替换性 | `SMSProvider` 由启动工厂按配置注入，核心状态机无厂商分支；支持 `aliyun-managed` 与 `http-generated`，并保存 Provider ID 和 Adapter 版本。 |
| NFR-15 | 身份与授权分离 | OIDC 只证明主体与会话；Account 决定平台和组织业务权限，业务路由不再依赖 Identity Provider 角色 Claim。 |
| NFR-16 | Commerce Provider 可替换性 | 已实现：Commerce Mapping、商品映射、订单/流水/证据及公开 API 均使用供应商中立语义并冻结 Provider/Adapter 版本，厂商字段只存在于 Adapter。 |
| NFR-17 | 支付能力可发现 | `/v1/payment-capabilities` 提供版本化中立声明，API、UI、任务和订单快照统一使用渠道、查询、时窗、终态及动作能力。 |
| NFR-18 | 模型网关可替换性 | 已实现：通用记录冻结 Provider ID、Adapter 版本、上游引用和摘要；部署配置可选择 New API 或通用 OpenAI 兼容 Adapter，厂商协议不进入通用 API/UI。 |
| NFR-19 | 运维探针可扩展 | 已实现：Portal 聚合器消费配置注册表和通用/Relay Adapter，不硬编码组件或厂商。 |
| NFR-20 | 无旧模型技术债 | 旧运行时路径、幂等迁移、非计费 readiness 与三层门禁已有仓库实现；正式安装制品原子回滚及生产联调未验收。 |
| NFR-21 | 结构化日志 | 全服务统一 JSON schema 必含时间、级别、服务、环境、版本、事件和结果，HTTP/Relay/后台任务无旁路 Logger。 |
| NFR-22 | Request ID 关联 | HTTP/Relay 入口校验或替换外部值，响应、日志与同步下游传播受信 Request ID，事件处理建立独立关联。 |
| NFR-23 | 分布式 Trace | HTTP、Outbox、Relay、NATS Consumer 和后台任务形成 W3C/等价 Trace 上下文，日志与指标 Exemplar 可关联。 |
| NFR-24 | 健康检查 | 服务与 Relay 均区分进程 liveness 和关键依赖 readiness；普通健康不执行计费或客户端切换准入。 |
| NFR-25 | 独立指标 | 独立受限监听输出 HTTP 和 Relay 请求/错误/延迟/任务/领域积压指标，标签有界且 Trace 仅进入 Exemplar。 |
| NFR-26 | 性能与容量验收基线 | 资源/数据/样本/负载比例/分段计时证据格式已机器化；云端 UAT 仅做健康烟测，不构成生产等价报告。 |
| NFR-27 | 关键内部 API 延迟 | 读写P95/P99和错误率门禁已建立；云端 UAT 未按规定样本量实测。 |
| NFR-28 | 外部依赖接口延迟 | Provider端到端与平台附加耗时独立门禁已建立；云端 UAT 未冻结健康 Provider 做分段实测。 |
| NFR-29 | 流式首个内容事件 | 派发与首内容目标及样本门禁已建立；云端 UAT 没有真实路由负载报告。 |
| NFR-30 | 模型并发容量 | 200并发证据门禁与全局/模型/路由原子准入已经实现；单节点 UAT 未执行容量、错误率和公平性验收。 |
| NFR-31 | Hold 容量 | 十万Hold、稳态/突发、锁等待与守恒门禁已建立；云端 UAT 只有0个Hold，缺环境报告。 |
| NFR-32 | 订单容量 | 百万订单/三百万流水与吞吐门禁已建立；云端 UAT 只有4个订单、0个支付流水，缺环境报告。 |
| NFR-33 | 事件积压容量 | 百万保留、十万排空和高水位证据门禁已建立；云端 UAT 为1个流、0条消息，缺环境报告。 |
| NFR-34 | 分级 RPO | 云端 UAT 已完成迁移快照，但没有连续 WAL 或隔离恢复实测；60秒资金域/300秒配置域生产事实仍缺失。 |
| NFR-35 | 分级 RTO | 核心60分钟/非关键4小时计时及安全降级门禁已完成；云端 UAT 未执行实际恢复计时。 |
| NFR-36 | 备份频率与加密 | 云端 UAT 的同机迁移快照校验通过；持续WAL、日增量、周全量、加密、KMS分离和异地不可变副本仍未验收。 |
| NFR-37 | 恢复演练 | 月/季/年时效、整改和复测模板及门禁已完成；当前没有实际连续演练记录。 |
| NFR-38 | 分类保留与清除 | 统一机器策略和逐类删除收据门禁已完成；当前没有真实三年归档、90天日志、法律保全与清除收据。 |
| NFR-39 | 登录多维限流 | 已实现：公网密码/短信 Direct Grant 和 Refresh 统一经 Redis 原子准入，覆盖来源IP分钟/日、设备、标识、已学习稳定用户、Session和Provider；失败渐进延迟并临时锁定，成功不立即清零，已知/未知账号同形响应和统一耗时桶。 |
| NFR-40 | 短信多维限流 | 已实现：发送与核验分别按手机号、来源IP、设备、用途和Provider计数，Provider固定在安全容量80%，服务端返回窗口实际剩余 `retry_after`，一次性图形挑战与服务端判断不可由前端绕过。 |
| NFR-41 | 支付多维限流 | 已实现：付款人、受益组织、来源IP、订单查询、退款动作和Provider均采用原子准入；真实查询按较慢能力间隔合并，合法回调先持久化后异步处理且不受用户限流影响。 |
| NFR-42 | 模型调用多维限流 | 已实现：用户、组织成员、组织、模型、路由和全局速率/在途容量在Hold前原子校验；幂等重放不重复计数，超限返回429与Retry-After且不触达上游。 |
| NFR-43 | 租户隔离验证矩阵 | 已实现：每次合同发布自动执行15个拒绝向量，并检查直接对象、批量、搜索、缓存、异步事件和无泄露错误；当前无租户公开导出面，未来新增导出路由会先触发门禁失败。 |
| NFR-44 | Provider 熔断 | 通用精确阈值熔断器已接入支付和模型 Provider；云端 UAT 没有真实错误样本，仍缺业务分类、只读探针、跨副本和告警验收。 |
| NFR-45 | 人工队列 SLA | 已实现：三领域统一从源案件创建时间冻结 P0/P1 7×24 与 P2 上海工作时 SLA，转派不重置；注册/资料/注销、支付/退款/拒付/对账、路由 Provisioning、用量和高风险命令队列均保存责任人及不可变时间线，逐截止自动升级至配置的下一值班负责人，P0/P1 止损逾期执行 Provider、支付商品、路由或高风险命令暂停评估。 |
| NFR-54 | 支付健康评估可靠性 | 已实现：观察、窗口成员、策略和评估不可变；稳定尝试、控制版本和成员摘要使并发阈值穿越幂等。 |
| NFR-55 | Billing 高数据量稳定分页 | 已实现：11 类资源使用 15 分钟资源 ID 快照、确定性 HMAC cursor、确定排序、规范化筛选和默认 50/最大 200 的有界 limit；主体/全局速率、快照/成员/8GiB 容量、10 秒超时、过期墓碑、清理指标与 70%/85% 告警均失败关闭。PR 门禁使用专用 PostgreSQL 强制执行事务、并发、全局边界、11 类 SQL、过期及超时回滚测试并拒绝 skip；百万订单完整翻页、并发变更轨迹和候选制品摘要绑定仍作为生产发布证据强制校验，不冒充本地已执行。 |
| NFR-56 | Token 管理高数据量稳定分页 | 已实现：APT-120、APT-121 已分别通过身份 B 第三轮、第四轮审核，交付统一 envelope/schema、签名 cursor、稳定快照/墓碑、全部容量水位与限流、事务回收/清理、三类不可重置 deadline，以及九类逐资源规范化筛选、确定全序、独立 cursor、owner 权威投影、资源审计与 evidence 三元 cursor。本地未运行 PG 重型及九类适配集成用例，CI 隔离库门禁强制 PASS 且拒绝 SKIP；当前未推送、未部署。 |
| NFR-46 | 统一可信时钟 | 云端 UAT 宿主机 Chrony 已同步9源且当前偏差为毫秒级；生产三源中位配置、100ms连续证明、250ms/1s注入和告警/readiness/写拒绝证据仍缺失。 |
| NFR-47 | 桌面 API 兼容与淘汰 | 已实现：N/N-1/N-2共用合同矩阵；清单启动校验90天支持和60天通知，四服务返回契约/最小版本/Sunset头，桌面直连与平台模型请求携带精确版本，过期远程写操作稳定返回426及HTTPS升级入口，不影响本地模型和本地读取。 |
| NFR-48 | 审计导出保护 | 已实现：服务端任务要求专用权限、15分钟近期认证、明确范围和原因，执行默认脱敏/临时PII授权、加密签名水印、31天/10万行/500MB、2并发/10每日及15分钟一次性下载。 |
| NFR-49 | 模型结果重放载荷最小化 | 已实现：在线24小时/30天/90天硬上限、自动清除、主体正文即时删除内部入口、派发正文最小化及独立受限案件迁移/解除后30天清除已建立；生产备份生命周期证据随 APT-037 验收。 |
| NFR-50 | 用户编码查询保护 | 已实现：编码不是认证/授权凭据；查询要求 `platform.users.read`，只做完整等值匹配，使用统一无泄露响应、多维摘要限流和全结果审计，不提供公开、前缀、模糊、自动补全或批量入口。 |
| NFR-51 | 模型选择跨入口回归门禁 | 已实现：账号 Popover 退役入口、设置全局默认、对话会话选择、类型化来源、生命周期和失败关闭执行同一门禁；简中、英文、日文词条及桌面/Web 桥接协议由自动测试阻断回归。 |
| NFR-52 | 敏感入口授权覆盖门禁 | 未实现：全 HTTP、任务、事件、回调和 CLI 的入口登记、默认拒绝与发布检查由 APT-079 待实现。 |

## 5. 已确认领域规则

### 5.1 身份、业务账号与外部客户映射

- Identity Provider 负责认证、密码、登录会话、Refresh Token 和会话撤销，不保存组织角色和资金权限事实。
- Account Service 负责稳定用户 ID、当前用户名/手机号、组织成员关系、角色权限、全局活动上下文及其版本。
- Commerce Provider Mapping 只保存用户或组织与外部客户之间的 Provider、Adapter 版本、脱敏外部引用和同步状态。
- Commerce 映射未完成只能阻止依赖该映射的支付操作，不能阻止登录、模型调用、组织管理或其他账号能力。

### 5.2 组织额度分配

1. 管理员分配时必须选择具体组织权益包及其积分组成，不能只提交一个无法追溯来源的总额。
2. 批量分配按成员逐项执行独立事务，允许部分成功；响应必须返回每一项的成功、失败原因和幂等结果。
3. 成员被移除时，未消费授予转为 `SUSPENDED`，继续占用原组织权益且不回到未分配池；成员在原到期日前重新加入同一组织时恢复，原到期时间不变。
4. 有 `org.quota.allocate` 权限的管理员可以给自己分配，仍受组织总额度、具体权益包和审计约束。
5. 已创建授予不能直接改额；调整必须先撤回未消费且未被 Hold 占用的部分，再创建新授予。
6. 显式撤回成功的未消费额度返回原权益包可分配余额；成员移除导致的暂停不是撤回。

### 5.3 动态预占与权益过期

```text
参数值 = 请求或模型元数据明确值；缺省时为 0
预占单位数量 = 版本化协议预占公式(参数, 模型元数据)
单位费用 = 计量单位数量 × 该单位版本化单价 ÷ 单价分母，并按单位规则舍入
初始 Hold = 所有计量单位费用之和
```

- 计量单位定义至少覆盖 `INPUT_TOKEN`、`CACHED_INPUT_TOKEN`、`OUTPUT_TOKEN`、`IMAGE`、`REQUEST`、`DOCUMENT`，并允许供应商中立扩展；一个价格版本只能包含其绑定协议公式实际引用的单位。
- Chat、Embedding、Rerank、图像等协议分别发布不可变公式版本；公式引用单位与价格单位集合不一致时拒绝发布或创建 Hold。显式值为 `0` 的价格行表示免费，价格行不存在表示配置缺失，二者不得合并。
- `INPUT_TOKEN` 只记录非缓存输入，`CACHED_INPUT_TOKEN` 只记录缓存输入；Provider 的含缓存总输入量必须先扣除缓存量再写入 `INPUT_TOKEN`，同一 Token 不得重复进入两个单位。

- 长调用在到期前续租 Hold；续租失败时不得静默继续产生无限费用，应终止可控调用或进入不确定核对。
- 实际费用超过 Hold 时先申请追加预占；追加成功后结算，追加失败时停止后续可控输出并进入人工复核，同时记录对应钱包负余额投影和独立应收，不能把其他上下文余额作为兜底。
- 权益到期时直接释放并移除仍不确定的 Hold。释放不会恢复已经过期的可用积分；之后确认的真实用量记入人工损失复核，不重新扣减用户其他权益。

### 5.4 权益、退款与消费顺序

- 普通退款现金价值只来自目标支付流水所绑定权益包的全部未消费 BASE；任一 BASE 或该支付产生的关联 BONUS 曾经消费（包括之后冲正）即拒绝普通退款。退款成功时必须冲销该 `payment_transaction_id + entitlement_package_id` 全部未消费 BASE、该支付产生的全部未消费关联 BONUS 及相应账本负债；BONUS 不折算为现金退款，人工赠送和同一订单其他支付流水不受影响。
- 无法冲销足够基础积分、退款状态不确定或 Provider 退款失败时进入人工复核，不允许制造负余额掩盖差异。
- 只有现金充值产生的 BASE、退款追回或受审计人工还款可以清偿独立应收；BONUS 不得清偿应收。
- 拒付冲销绑定原支付流水和原权益包，先撤销该包可撤销的 BASE 与因该支付发放的关联 BONUS；不足现金本金形成订单级应收，不冲销或改写其他权益包。
- 拒付应收未清时冻结对应钱包上下文的新 Hold 和平台模型调用；其他订单的 BONUS 不改账，清偿后仍只按原有效期恢复可用。
- 人工赠送积分自入账起 30 天到期。
- 先按到期时间排序；同日到期时依次消费赠送积分、基础积分；仍相同则按入账时间和稳定 ID 排序。

### 5.5 全局活动上下文

- 每个用户全局只有一个活动上下文，桌面与 Web 使用同一条版本化记录，不再按 OIDC Session 分别保存。
- 个人钱包负数只冻结个人聊天和计费上下文，不影响用户使用正常组织授予；组织钱包负数只冻结该组织上下文，不影响个人或其他组织上下文。
- 切换成功后递增 `context_version`；所有业务 API 响应返回该版本，客户端发现更高版本时必须丢弃旧额度、权限、路由和组织缓存。
- 切换前已经开始的模型调用保存上下文快照和版本，始终按该快照结算。
- 成员被移除或组织停用时，服务端把受影响用户的活动上下文切回个人并递增版本。
- 平台后台授权独立于活动上下文，不能因为用户切换到个人或组织而获得或丢失平台权限。

### 5.6 平台角色、服务身份与恢复

- 部署幂等创建用户名 `admin` 的第一个超级管理员并保持 `BOOTSTRAP_PENDING`，部署者选择受控终端无回显直接设置正式密码，或使用默认256 bit、最少128 bit熵、无填充 Base64URL 的一次性引导凭据。引导凭据默认15分钟有效、最多5次失败，只在受控通道读取一次；交付明文首次读取后销毁，成功兑换原子设置正式密码并删除服务端校验值，激活前不能登录后台或执行管理动作。
- Break-glass 只在凭据丢失、意外停用或身份故障等异常导致全部超级管理员无法有效登录时使用离线运维命令和主机/集群高权限身份，创建短时紧急授权或恢复超级管理员；不得提供公网恢复 API，不得绕过最后一个有效超级管理员主动注销保护或替代计划内角色交接，执行立即告警并写不可删除审计。
- 同一用户可以同时持有计费运营和审批角色，但不能审批自己创建的申请；允许不同用户互相审批各自申请。ADM-37 的强制审批命令无论金额大小都只需一名独立审批人；ADM-38 中的普通退款和组织删除只审计、不进入财务审批流。
- 所有角色绑定可设置开始和到期时间，临时或紧急授权到期后自动失效并使授权缓存失效。
- 服务账号不是人类用户，不进入组织成员和平台角色表。每个服务获得独立工作负载身份及最小内部权限；后台任务继承所属服务身份，Event Relay 只允许读取/确认本服务 Outbox 和发布允许的事件 Subject，不能调用资金审批接口。

### 5.7 邀请、注销与资料变更

- 组织邀请支持手机号、用户名和一次性邀请链接。一次性 Token 只保存摘要，三种邀请均绑定目标组织、预授予角色、有效期和邀请人。
- 注销目标采用 `CLOSURE_REQUESTED → QUIESCING → FINANCIAL_CLEARANCE → IDENTITY_CLEANUP → ANONYMIZED → COMPLETED` 持久 Saga。进入静默期后禁止新模型/Hold、订单、资料、成员和授权事实，只允许清偿、既有订单处理、证据复核、所有权转让和状态查询等明确修复动作；阶段失败幂等重试，超过 SLA 进入 ADM-34。
- 注销完成的资金门禁除个人余额、应收、本人支付/退款订单和 Hold 外，还包括待核对模型用量、未解决拒付、未分类或有资金影响的对账差异、外部待确认关单/晚到审核/PROVIDER_BLIND，以及资金相关 Commerce Mapping 任务。纯信息差异不阻断；资料同步任务可以自动取消。用户是任一有效组织唯一所有者时必须先转让所有权，是最后一个有效超级管理员时必须先向另一名有效用户完成角色交接。
- 资料修改 Saga、平台角色、普通组织角色、临时或紧急授权和未接受邀请不要求用户手工处理，也不作为资金资格阻断；组织所有者绑定仍必须先转让。注销协调器必须分别取消或向前收敛资料 Saga、撤销授权并清缓存、使邀请失效。自动清理失败属于标识释放技术门禁，进入重试或 REPAIR_REQUIRED，不能被忽略。
- 组织余额和未消费组织授予不阻断普通成员注销。注销终止全部成员关系，未消费授予以 `SUSPENDED/ACCOUNT_CLOSED` 继续计入原权益组成的暂停授予未消费额，不能通过普通接口撤回、恢复、转移或直接重新分配，也不能消费或创建新 Hold。
- 账号进入 `ANONYMIZED` 后，具有专用权限的管理员可在不存在消费、Hold、待核对用量或资金冻结时，一次性全额回收单条授予当前全部可回收剩余量；额度只回到原权益包原 BASE/BONUS 组成且原到期日不变。未回收部分到达原到期日后直接失效且不回池；回收不会把旧授予关联给相同标识注册的新账号。
- 标识释放前必须确认 Identity 已删除或永久停用且不可认证、Account 自动清理完成、Commerce Mapping 已 `DETACHED`，并用主体生命周期版本拒绝迟到命令/消息/回调。随后 Account 原子写随机墓碑值、释放用户名/手机号唯一占用并进入 `ANONYMIZED`；同一标识重新注册创建新用户 ID，不关联旧权益、邀请、授予或账本。
- 账本、订单、退款、对账和审计使用不可逆内部主体 ID 保留 3 年，不保留可直接登录或联系用户的信息；法律保全到期前除外。
- 用户名和手机号修改使用可恢复 Saga：创建变更请求并预约新值，完成重新认证/验证码验证，更新 Identity Provider，再提交 Account；手机号提交后撤销其他会话并异步更新 Commerce Mapping。
- Identity Provider 已更新但 Account 未提交时优先向前重试，不自动反向覆盖用户凭据；Account 已提交但 Commerce 同步失败时允许登录和模型调用，仅暂停新支付并进入人工修复。

### 5.8 注册并发、密码边界与补偿

- 注册可用性预检是图形挑战后的非绑定提示。发送短信前必须原子取得用户名和手机号的 `SMS_PENDING` 预约；短信验证成功升级为 `REGISTRATION_IN_PROGRESS`，Identity 创建后升级为不可由普通 TTL 释放的 `IDENTITY_CREATED` 恢复锁。
- 服务端 `registration_operation_id` 及派生幂等键贯穿短信发送/核验、Identity 创建、Account 提交和钱包初始化。同键重试返回原结果；同键改变用户名、手机号、挑战或密码返回冲突，不产生第二个 Identity 或 Account。
- 密码预检和最终创建只由 Identity Adapter 执行。Account 仅接收非秘密注册事实；原密码及其无密钥摘要不得进入数据库业务表、消息、日志、审计或 Trace。密码为8–128位可打印ASCII，允许中间半角空格，首尾空格明确拒绝，任何层不得trim、规范化、改大小写或截断。
- Identity 创建后优先向前提交 Account：30分钟未收敛进入 REPAIR_REQUIRED，预约继续占用；满24小时后只有证明 Account 从未提交且没有任何钱包、订单、成员、授予、用量或在途提交事实，才能先停用并确认 Identity 不可认证、再释放预约。结果不确定时保持修复状态。
- 客户端通过操作 ID 和签名查询令牌恢复未完成注册，运营通过 ADM-35 重试确定阶段。用户查询和运营页面都不能显示密码、验证码、密码指纹或 Provider 原文，也不能绕过证明门禁直接补偿。

## 6. 充值商品目录状态边界

正式接口只信任商品 ID。当前在售 SKU/BOM、金额、权益、商品数量和生命周期是环境运行数据，只能通过受权限控制的后台目录、审计或受控导出查询；本文只报告运营目录能力及部署版本，不维护商品清单。历史迁移作为既有部署事实保留，但新的日常商品变化不得再通过代码或迁移发布。

## 7. 正式上线门禁

以下条件未满足前，不应把当前 UAT 配置视为正式支付上线：

1. 后台上架命令保存与当前环境、Provider/Adapter、商品映射、金额、币种、渠道和发票事实一致且未失效的准入证据。
2. Billing 商品目录通过生产正式准入，生产不允许测试证据扩张收款能力。
3. 使用非测试商城账号完成至少一笔受控支付，验证订单金额、商城订单号、二维码、支付查询、单次入账和权益到期时间；商品值只记入验收证据和审计。
4. 验证重复创建、重复查询、服务重启、客户端退出、网络超时和晚到支付不会重复入账。
5. 验证当前 Provider 的关单、回调、退款和账单 capabilities；H3C 必须验证专用客户端换取短期 Token、重复/冲突回调、乱序状态、持久化后应答、回调丢失时查询补偿及突发限流，且 `notification_supported=true`；普通退款无论 capability 如何都确认用户/运营入口隐藏且服务端创建失败关闭，既有请求与被动异常仍可查询和收敛；账单四方对账和差异责任人/修复/审计闭环通过验收。
6. 生产 Provider 使用 HTTPS、主机白名单和 Secret 管理；真实密钥不在 Compose 文件、Shell 历史或日志中。
7. 数据库备份、恢复演练、监控告警、磁盘容量和日志保留策略已由运维环境落实。
8. 当前 Model Gateway Adapter 能按上游请求引用提供稳定用量证据，Gateway 自动核对、Hold 续租/追加和人工重试通过验收。

## 8. 明确不应实现的行为

- 不允许客户端提交或覆盖充值金额、SKU、积分或支付成功状态。
- 不允许本地随机生成新华三支付二维码。
- 不允许在商城不支持时仅修改本地状态来模拟关单、退款或对账成功。
- 不允许普通用户查看真实供应商密钥、New API 管理凭据或内部服务 Token。
- 不允许在个人中心新增用户供应商 Key、自管 Token Account、共享策略或路由功能；本地独立模型继续留在桌面本地配置域。
- 不允许 Gateway 因网络失败自动重发模型请求。
- 不允许有效组织没有所有者或同时存在多个所有者；所有权变更必须使用唯一所有者转让流程。
- 不允许把个人上下文和组织上下文的余额、Token 路由或审计混在一起。
- 不允许将任何开发/测试 Payment Provider、测试渠道或 Mock New API 的能力描述为生产接口能力。
### APT-123-R2 最终独立审核与实测记录（2026-09-06）

- 状态：`DONE / REMEDIATION_REVIEW_PASS`；身份 B 整批最终 `PASS`，由父执行独立签署提交。当前 `NOT_DEPLOYED`，未推送；不提前关闭 APT-125-R1、ADM-62/UI-50、APT-138 或 APT-137。
- 本地隔离 PostgreSQL 真实具名、无 SKIP：三服务 Account/Billing/Token 创建 Saga `TestTokenAccountProvisioningThreeServicePostgresIntegration` 10.32s；Token 真实 operator/runtime/migrator 配额门禁 `TestTokenAccountOwnerCapPolicyPostgresIntegration` 5.11s；Billing 真实角色迁移门禁 `TestTokenAccountWalletReceiptMigrationRolesPostgresIntegration` 5.859s；目录回滚具名 PG 3.73s；Account 审批/生命周期真实整链 3.38s（草稿、提交、批准、执行与 worker APPLIED，000047 正反矩阵及 down 历史保留）。此前中断或失败的试跑不计通过证据。
- 实测覆盖：T1/T2/T3 响应丢失、撤权/actor 与 owner 漂移、跨 writer 配额竞争、SAFE/lease fencing、人工队列分页/刷新/撤权、跨 operator 同 key 隔离、expected/observed 双检查点与理由审计、真实 SQL P0001 回滚后恢复；真实角色连接均检查数据库及 session/current 身份，runtime 无 schema CREATE 或受保护表写权。
- 审批链修复：独立观测时间不再伪造事实漂移；严格模型财务事实复核保留冻结 snapshot/audit；NUL 锁作用域改 SHA-256 十六进制参数；UUID/text SQL 参数显式定型；新增 Account 000047，仅限定三 Token 命令及权威漂移原因的 DRAFT→INVALIDATED，NULL/其他命令/其他原因/命令篡改拒绝，down 不删历史。
- `go test -p 2 -tags=nomsgpack ./...` 无 PG 全量通过；本批定向 Go、严格财务/Unit digest 固定向量、Node/Ajv 合同、Portal build/lint 与 Chromium smoke 通过。迁移不可变清单 **264 文件**通过；授权入口清单覆盖 **332 HTTP、26 tasks、3 events、15 CLI**；任务、i18n 和 diff 检查通过。
- 不宣称全量 Node 全绿：此前全量合同的 browser-login 与 provider-specific 两项为已核对 HEAD 既有陈旧断言，交由 APT-137 总门禁处理；本批受影响合同已修并定向通过。历史无库 SKIP 记录保留，不能替代本轮具名 PG 或候选提交的 CI 证据。

### APT-125-R1 最终独立审核与实测记录（2026-09-06）

- `DONE / REMEDIATION_REVIEW_PASS`；身份 B 整批最终 PASS，ADM-62/UI-50 恢复已实现，由父独立签署提交。未推送、未部署；APT-138 为 PENDING/READY_TO_IMPLEMENT，未注册生产 reader/guard/consumer/worker 或激活。
- 最新真实 PostgreSQL `TestRouteGovernedCommandsPostgresIntegration` **3.12s PASS/no-SKIP**：同键并发、跨账户同 actor/type/key 201/409、精确凭据替换、legacy 选中行及同版本健康漂移、初始 UNKNOWN、READY/覆盖缺口/lease/inflight 阻断、事务末 epoch guard 回滚、真实 HTTP 提交后断连接与 GET key/reference 恢复、删除前/后版本墓碑、by-key 撤权 403/跨 actor 404、v2 PUT/DELETE 的 PRE/ACTIVE/SAFE 六组合零旁路；事实、通用结果、审计与 Outbox 恰一次，通用路径旧 receipt 为零。
- 同批真实 PG 回归：`TestStableRoutesPostgresIntegration` **11.80s PASS**，`TestRouteHealthPostgresIntegration` **11.04s PASS**；两项新增 parser/envelope 单元 PASS。早期 fixture 失败、无库 SKIP 不计通过证据。
- `go test -p 2 -tags=nomsgpack ./...` 无 PG 环境全量 PASS；其后最小 by-key 新接线由最新 Token PG 编译/实跑覆盖。受影响 Node/Ajv 合同、最终 Portal build/lint、Chromium smoke、授权入口 **332 HTTP/26 tasks/3 events/15 CLI**、i18n、architecture 与 diff 检查通过；迁移没有新增。Chromium 包含 priority 改变自动重预检、首次新参数提交、GET key/reference 恢复、严格墓碑结果、零旧直写及原权限/队列矩阵。
- 保留 APT-137 的两项 HEAD 既有 Node 陈旧断言待总门禁处理；不宣称全量 Node 或生产部署通过。APT-138 仅在本次独立提交后按既定计划推进。

### APT-138 最终独立审核与实测记录（2026-09-06）

- 该次审核记录为 `DONE / NOT_DEPLOYED`，身份 B 代码及本批验收最终独立 `PASS`；当时 ADM-64 由未实现改为已实现，435 项汇总对应为已实现 402、未实现 2，其余分类不变。该段保留 APT-138 当时的审核历史，不再代表当前完整目标状态；GSA-02 后续确认 Account 审批链缺少15分钟年龄校验，故当前明细已把 ADM-64 收紧为部分实现，代码由GSA-13/14唯一实现，APT-138-R1只引用审核证据关闭。应用默认 PRE，未执行生产激活、推送或部署；APT-137 仍待实施。
- `cb1ab922793b75ca8f9c10ee8fea404f8bd26463` 在限制 1 CPU/2 GB、无网络 Linux 容器中，全量 `go test -p 1 -short -race -tags=nomsgpack -count=1 ./...` 和 `go vet -p 1 -tags=nomsgpack ./...` 通过，退出码 0。后续候选只新增验收测试、CI 与状态文档，生产代码未变；此无 PG 短测不替代下列真实集成证据。
- `635aa54acc6986b201394aa6c165d8d08918978a` 的 `TestProductionCredentialAuthorityPostgresIntegration` 九子项真实 PG 全 PASS（9.82 秒、exit 0、无 SKIP），同候选 Token 全 vet 通过。直接安装正式 governance/store/authorization.Client，使用非超级用户 runtime、真实 Token 行锁和可控本地 Account HTTP 授权协议 fixture；覆盖冻结 actor/version/permission/epoch、Account 超时、行锁等待后原绝对 deadline 剩余耗尽（1.61 秒）、提交前第二次拒绝的八表回滚（0.02 秒）与 NOWAIT 锁释放。HTTP 响应 fixture 不冒充 Account 权限计算或 CLI 证据。
- 三审批 consumer 的真实 Account/Token PG+HTTP/JWT 链在 `544e921a896a52f32dea068733f3dc3b7b3ff125` 27.59 秒全部通过，首次/重复十一表、响应丢失只 GET、旧 epoch 零效果已验收；报价及 activation fixture 边界保留。真实 NATS transport 在 `6c86b421` 7.34 秒通过，只计消息层。CLI 非超级用户、状态 CAS/历史/down、十九项精确矩阵及 handler manifest、双 worker、各领域 PG 的候选和时长见设计 7.4.1，不将多候选汇总写成同一次全通过。
- `032cd73b` scheduled-publication 十一子场景含错误 epoch 零写（15.00 秒）、协议公式（8.74 秒）、TokenAdminProtocol（2.07 秒）、Runtime（16.03 秒）均真实 PG PASS/no-SKIP。`168253477e50d765bed9614a39cce779d7a39ca9` 凭据 origin 真实 PostgreSQL+五完整 Chromium 流程 25.32 秒通过，强制浏览器 gate；每账户一条凭据、撤销后精确 401、交换次数 6 不增加及七表零写通过。五 main 编译和应用/metrics 共同退出与 collector 保留已分项验证。
- 根代理最终十五项受影响 Node、授权覆盖 **339 HTTP/27 tasks/6 events/17 CLI**、迁移不可变清单 **272 文件**、i18n、architecture、任务覆盖检查均 PASS。全量 Node 仍为 **376/378 PASS、2 FAIL、0 SKIP**（49.31 秒），browser-login-theme 与 foundation 既有断言归 APT-137 后续门禁，不能宣称全 Node 通过。
- 独立前置修复均已先审核、隔离候选验证和签署，包括策略精确字段、凭据完成/租约/撤销审计、迁移账本重入、公式单位键、生命周期分页/目录基线及测试替身 race；不将这些领域修复重算为本批新变更。B 最终接受上述分层证据，部署/灰度/N/N-1 环境验收不在本次 DONE 内。

### APT-137 分组代码交付与环境缺项（当前记录）

当前 APT-137 为 `IN_PROGRESS / ENV_PENDING / NOT_DEPLOYED`，UI-58 为部分实现；APT-139/ADM-66 与 APT-140/UI-59 均已完成且获身份 B 最终 PASS，状态 `DONE / REVIEW_PASS / NOT_DEPLOYED`；当时437项阶段汇总为已实现404、部分实现30、未实现1、移除1、非当前范围1，其他状态为0；该历史计数已被本页顶部当前440项汇总覆盖。既有 APT-138 和更早候选的测试/未注册/两旧断言描述保留当时事实，不把历史报告改为后来候选。G0～G5 独立签署：`8dcaa87c`、`33209285`、`e63224e9`、`07e29c55`、`0470d83a`、`43d5acc2`；新域固定为 account.pinvou.com→credentials.pinvou.com、account-uat.pinvou.com→credentials-uat.pinvou.com，不兼容旧凭据域。

G5 精确代码候选 `b837fc087518594a8005a487235c0630e02eb57b` 全账户 Node 429/429、0 SKIP、45.093 秒/exit 0，含两项旧断言纠正；OpenAPI 有效（9项既有warning）、architecture 与授权339/27/6/18通过。原首轮归档漏文件，补导同树后完整重跑，未改源码。G6 精确五代码候选 `e3bf4dc40b7b4995614d02b4ac40c3e7bebcd2bc` 与 `e371d69e58fe9e5831bf0e19a478f88bf80235e7` 对应输入无差异；十五文件冻结候选 `bcb25dcb9a31b824d1210009c7d78f15437e8560` 全账户 Node 434/434 PASS、0 SKIP（47.714 秒、exit 0）；任务覆盖 138 tasks/3 remediations/31 incomplete/352 completed、授权 339 HTTP/27 tasks/6 events/18 CLI 和 architecture 均 PASS。代码及隔离门禁已验证，B 已静态 PASS，最终证据文档复核后签署；不是远端 CI 或现场部署通过。相同源码与预构建三个原 CLI 的两项专属 PG 实测 16.86/64.31 秒，合计 82.777 秒、0 SKIP、exit 0，固定角色/fixture 库残留 0/0；真实 Helm 4.2.3 四 profile lint/render checker、Compose config/checker 和三个新增服务镜像构建通过。

nginx 测试镜像须分开记录：仓库保留已审的官方 Alpine 同 host/version/path HTTPS 源及 APK 签名/TLS；官方源构建因网络下载缓慢由根停止，未取得成功结果。根仅在隔离 TEMP 使用已复核的局部依赖镜像源适配取得 nginx 1.18.0/Python 3.8.10 测试制品（镜像 `sha256:5fc7372e9df24e918f30886b893a2e3f0d4f53b377aabbaee6fb9e8a1b94876c`，TEMP 源 SHA-256 `bf05125b9690004ebb0a998c4ac734d504d294f8758a7b64b27f9a9bef1c4f5b`）；未把此适配写回仓库，也不称官方 Dockerfile 原样构建成功。该测试制品以 UID 1000、network none、只读根、cap-drop ALL 运行六 parser、两个 runtime、ready 后 failure 与实际 OS SIGTERM 矩阵，精确 markers/子进程清理均 PASS、driver exit 0、本次 label 容器残留 0。这仅闭合受限容器机制验收，不是官方源可用性、GitHub Actions、正式 TLS 或生产部署通过。

正式环境仍须核验成对 DNS/有效证书/独立Host及代理全链日志，专属OIDC与短期workload/轮换、同一NATS集群ACL及历史、所有旧密文用途的key_id/格式/AAD可解密和可恢复备份、真实HBA/Secret/角色、显式cap/CAS与N/N-1/SAFE/分级观察。缺项不阻塞已验证代码提交，亦不授权放量或伪造现场报告；当前未推送/部署，后续只在适用批准与完整现场前置下执行。详见 [发布手册](../deploy/runbooks/model-token-release.md)。
