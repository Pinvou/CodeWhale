# 账号平台未完成需求实施任务

> 状态：当前执行队列
>
> 需求与状态基线：`e5437902aea2558b78b8076f3645834611ea2514`
>
> 代码审计基线：`e23005aaf6745d4b8c1ffa5ac73943e0f8f98ca5`（APT-001 以本文件所在提交增量闭环）
>
> 约束边界：`architecture.md`、`role-system-design.md`、Accepted ADR 与本目录部署文档

## 1. 执行规则

1. 本文为最初 275 个未完成目标及后续需求变更保留可追溯任务。当前 138 个正式任务中，125 个任务为 `DONE`，7 个任务保持 `BLOCKED_EXTERNAL`，5 个任务为 `PENDING`，0 个任务为 `IN_PROGRESS`，1 个任务为 `IN_REVIEW`；APT-124-R1、APT-130-R1、APT-131-R1 是保留原已审核交付历史、同时重开当前需求状态的具名整改批次，不新增主目标所有权，也不计入 138 个正式任务编号。规范矩阵中的每个未完成编号只能有一个主任务。
2. 任务按稳定服务所有权、事务边界、公开契约和可独立验证能力划分，不按文件数量或代码行数机械拆分。
3. 每个任务使用独立提交，提交必须同时包含该任务所需的代码、迁移、OpenAPI/事件契约、三语 UI、自动化测试、需求状态和相关架构/部署文档更新。
4. 开始任务前先检查依赖任务已经完成；实现中发现不能独立交付时，必须先拆分任务并更新本文，不能把无关能力混入当前提交。
5. `CODE` 任务必须在仓库内闭环；`CODE+ENV` 任务除仓库实现外还需要生产等价环境证据，缺少环境时不得把状态改为已实现或已部署。
6. 每个提交至少运行 `node scripts/account-platform.mjs verify`；涉及桌面、Web、部署、安全、恢复或性能时增加相应定向测试和制品/环境验收。
7. 任务完成仅表示其映射目标全部达到验收标准；部分完成时保持 `IN_PROGRESS`，不得提前关闭或拆成多个混合提交。

状态值：`PENDING`、`IN_PROGRESS`、`IN_REVIEW`、`DONE`、`BLOCKED_EXTERNAL`。

## 2. 任务队列

| task_id | 独立交付目标 | 主目标编号 | 依赖 | 类型 | 状态 |
|---|---|---|---|---|---|
| APT-001 | 永久内部用户 ID、独立用户编码、本人展示与受控精确查询 | ACC-56–ACC-58, NFR-50 | APT-006 | CODE | DONE |
| APT-002 | Identity 密码边界、原始字符策略、短信 Adapter 与登录反枚举 | ID-03, ID-08, ID-11, ID-17, ID-18, ACC-01, NFR-14 | 无 | CODE | DONE |
| APT-003 | 注册唯一值预约、持久恢复 Saga、异步钱包 Provisioning 与恢复 UI | ID-14, ACC-03, ACC-04, ACC-51–ACC-55, BILL-04, UI-02, UI-15, ADM-35 | APT-001, APT-002 | CODE | DONE |
| APT-004 | Web 用户名/手机号变更 Saga 与重新认证；桌面忘记密码及资料修改边界 | ACC-06–ACC-09, UI-05 | APT-002 | CODE | DONE |
| APT-005 | 供应商中立 Commerce Customer Mapping 与同步生命周期 | CORE-03, ACC-05, PAY-06 | 无 | CODE | DONE |
| APT-006 | Account 平台/组织 RBAC、原子权限码和显式授权作用域 | ACC-13, ACC-35, NFR-15 | 无 | CODE | DONE |
| APT-007 | 超级管理员安全引导与异常失联 Break-glass 的生产等价演练 | ADM-01, ADM-02 | APT-006, APT-046, APT-077 | CODE+ENV | PENDING |
| APT-008 | 管理命令审批分类、生命周期、失效、授权复核和幂等执行 | ADM-07, ADM-17, ADM-22, ADM-36–ADM-42 | APT-006, APT-046 | CODE | DONE |
| APT-009 | 顶层组织创建、搜索、启停和跨域安全逻辑删除 | ACC-15–ACC-20 | APT-006, APT-008 | CODE | DONE |
| APT-010 | 组织节点创建、移动、重命名和受阻逻辑删除 | ACC-21, ACC-23–ACC-25 | APT-006, APT-009 | CODE | DONE |
| APT-011 | 多方式邀请、成员增删、所有权转让和即时撤权 | ACC-26–ACC-29 | APT-006, APT-009, APT-010 | CODE | DONE |
| APT-012 | 用户级全局活动上下文、跨端版本同步和成员失效回退 | CORE-02, ACC-34, ACC-38, ACC-39, NFR-08, UI-06, UI-07 | APT-006, APT-011 | CODE | DONE |
| APT-013 | Web 个人中心三 Tab 合并、全状态订单历史与普通用户资源边界收敛 | UI-03 | APT-012 | CODE | DONE |
| APT-014 | 账号注销资格、持久 Saga、静默期、跨域阻断与修复入口 | ACC-10, ACC-40–ACC-45, UI-14, ADM-34 | APT-003, APT-006, APT-018, APT-021–APT-025, APT-030 | CODE | DONE |
| APT-015 | 注销资料清理、标识墓碑、Commerce/邀请收敛及组织授予隔离回收 | ACC-11, ACC-46, ACC-47, ACC-49, ACC-50, BILL-39, BILL-40, NFR-13 | APT-014, APT-016, APT-017, APT-019, APT-025 | CODE | DONE |
| APT-016 | BASE/BONUS 组成级权益、消费顺序、到期与守恒约束 | CORE-04, BILL-05, BILL-06, BILL-14, BILL-16–BILL-19, BILL-36, UI-08 | 无 | CODE | DONE |
| APT-017 | 组织购买权益后的组成级成员授予、暂停、撤回和并发约束 | BILL-07–BILL-13, BILL-37, BILL-38, UI-11 | APT-006, APT-009–APT-012, APT-016 | CODE | DONE |
| APT-018 | 版本化计量单位、协议公式、动态预占和确定性价格生命周期 | ACC-37, TOK-17–TOK-21, TOK-23, BILL-20, BILL-22–BILL-25, BILL-41–BILL-45, ADM-12, ADM-13, ADM-23, ADM-24 | APT-012, APT-016 | CODE | DONE |
| APT-019 | 应收、拒付冲销、清偿分配和上下文冻结 | BILL-29–BILL-33, ADM-16 | APT-016, APT-023 | CODE | DONE |
| APT-020 | 钱包、组成级账本和 Hold 运营证据查询 | BILL-27, ADM-18 | APT-016, APT-018 | CODE | DONE |
| APT-021 | 普通退款目标锁定、全 BASE 退款、关联 BONUS 撤销与强制退款隔离 | BILL-28, PAY-24–PAY-30, PAY-32 | APT-016, APT-019, APT-023 | CODE | DONE |
| APT-022 | Payment Provider/渠道/商品映射中立化及生产测试隔离 | PAY-01–PAY-04, NFR-17 | APT-005 | CODE | DONE |
| APT-023 | 一单多流水、统一支付证据、逐流水履约和组织付款快照 | PAY-11, PAY-20–PAY-23, ADM-19, NFR-16 | APT-016, APT-022 | CODE | DONE |
| APT-024 | 晚到支付、无回调/账单降级、人工案件和事实盲区处置 | PAY-18, PAY-19, PAY-35, PAY-37–PAY-40, ADM-33 | APT-022, APT-023, APT-057 | CODE | DONE |
| APT-025 | 拒付/异常复核、四方对账和受审计修复 | PAY-33, PAY-34, PAY-36, ADM-21 | APT-019, APT-023, APT-024 | CODE | DONE |
| APT-026 | 供应商中立模型网关协议和用量证据 Adapter | TOK-04, TOK-05, NFR-18 | 无 | CODE | DONE |
| APT-027 | 独立模型目录、稳定模型身份及不可变目录生命周期 | TOK-07, TOK-42–TOK-44, TOK-46 | APT-026 | CODE | DONE |
| APT-028 | 平台托管路由 Provisioning、资格管线、目录回滚依赖校验及运营修复 | TOK-09, TOK-10, TOK-12, TOK-16, TOK-45, ADM-11, ADM-28–ADM-30 | APT-006, APT-026, APT-027 | CODE | DONE |
| APT-029 | 模型请求幂等指纹、持久派发、流事件加密与结果重放保留 | TOK-24–TOK-30, NFR-49 | APT-012, APT-018, APT-026–APT-028 | CODE | DONE |
| APT-030 | 模型取消、执行/交付分离、可信用量核对和取消期 Hold 收敛 | TOK-31, TOK-33, TOK-38–TOK-41, BILL-34, BILL-35, UI-13, ADM-31, ADM-32 | APT-018, APT-029 | CODE | DONE |
| APT-031 | Token Account、凭据和检查动作的平台权限隔离 | ADM-25–ADM-27 | APT-006, APT-026, APT-028 | CODE | DONE |
| APT-032 | 桌面动态目录消费、按会话冻结模型、显式替代分支与受信个人中心入口 | SCOPE-05, TOK-06 | APT-003, APT-027–APT-030 | CODE | DONE |
| APT-033 | 配置驱动系统探针与通用响应判定 Adapter | ADM-10, NFR-19 | APT-006, APT-020, APT-023, APT-031 | CODE | DONE |
| APT-034 | 桌面、个人中心和后台完整三语发布门禁 | UI-16 | APT-003, APT-004, APT-012, APT-013, APT-017, APT-021, APT-030, APT-032, APT-033 | CODE | DONE |
| APT-035 | HTTPS、生产 KMS/Vault、密钥轮换和秘密配置验收 | NFR-01, NFR-03 | 无 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-036 | 敏感信息白名单、统一结构化日志、Request ID、Trace、健康和指标 | NFR-04, NFR-21–NFR-25 | 无 | CODE | DONE |
| APT-037 | 迁移不可修改门禁、分级 RPO/RTO、加密备份、恢复演练和分类清除 | NFR-09, NFR-10, NFR-34–NFR-38 | APT-035, APT-036 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-038 | 正式高可用部署和故障切换验收 | NFR-11 | APT-035–APT-037 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-039 | 生产等价性能、容量、吞吐、积压和公平准入基线 | NFR-26–NFR-33 | APT-016, APT-018, APT-023, APT-029, APT-036, APT-038 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-040 | 登录和短信多维限流及反枚举耗时验证 | NFR-39, NFR-40 | APT-002, APT-036 | CODE | DONE |
| APT-041 | 支付和模型调用多维速率/并发准入 | NFR-41, NFR-42 | APT-021–APT-024, APT-028–APT-030 | CODE | DONE |
| APT-042 | 跨个人、组织、节点、缓存、批量、导出和事件的租户隔离矩阵 | NFR-43 | APT-006, APT-009–APT-013, APT-017, APT-021, APT-029, APT-033 | CODE | DONE |
| APT-043 | Provider/路由熔断、半开探测和统一可信时钟门禁 | NFR-44, NFR-46 | APT-022, APT-026, APT-036 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-044 | 当前及前两桌面版本兼容、弃用通知和 426 升级闭环 | NFR-47 | APT-032, APT-034 | CODE | DONE |
| APT-045 | 全域审计、幂等指纹、Outbox/Inbox 收敛和死信恢复闭环 | CORE-07, NFR-05–NFR-07 | APT-003–APT-006, APT-008–APT-034, APT-046 | CODE | DONE |
| APT-046 | 普通组织角色绑定的展示、有效期、授权撤销和缓存失效 | ACC-30–ACC-32, ADM-05, ADM-06 | APT-006 | CODE | DONE |
| APT-047 | 服务账号、后台任务和 Event Relay 的短期工作负载身份与最小 Subject 权限生产验收 | ADM-03 | APT-006, APT-076 | CODE+ENV | PENDING |
| APT-048 | 生产环境通过后台目录完成正式收款商品准入，并以真实 Provider 下单、查询/回调、金额/商品快照及履约证据验收通用链路；商品具体配置只记录在运行时目录和审计 | PAY-05, PAY-10 | APT-022, APT-083, APT-084 | CODE+ENV | PENDING |
| APT-049 | 账单支付证据、完整关单状态、拒付事实与订单派生摘要收敛 | PAY-12–PAY-14 | APT-019, APT-023, APT-024 | CODE | DONE |
| APT-050 | 模型派发不确定、流式结果与支付证据统一收敛边界 | CORE-06 | APT-029, APT-030, APT-049 | CODE | DONE |
| APT-051 | 全域人工案件 P0/P1/P2 统一 SLA、转派时钟与自动止损评估 | NFR-45 | APT-003, APT-019, APT-021, APT-024, APT-025, APT-030 | CODE | DONE |
| APT-052 | 非计费客户端切换 Readiness、短时签名准入证明及发布系统前置能力 | SCOPE-06, TOK-37 | APT-018, APT-027, APT-028 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-053 | 客户端幂等迁移及发布/安装/运行三层门禁的正式制品验收 | TOK-35, TOK-36, NFR-20 | APT-032, APT-052 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-054 | 紧急模型下架触发在途取消并保存实际取消、生成量、Hold 与结算证据 | TOK-47 | APT-030, APT-032 | CODE | DONE |
| APT-055 | 服务端受控审计导出、重认证、PII临时授权、加密水印和一次性下载配额 | ADM-09, NFR-48 | APT-006, APT-008 | CODE | DONE |
| APT-056 | 普通退款产品级默认关闭门禁、用户/运营入口屏蔽及服务端创建拒绝 | PAY-31, UI-12, ADM-20 | APT-021, APT-022, APT-034 | CODE | DONE |
| APT-057 | Provider 中立短期回调授权、Profile 化通知适配、统一证据归并与查询补偿；新华三作为首个 Profile | PAY-15–PAY-17 | APT-022, APT-023, APT-036 | CODE | DONE |
| APT-058 | 账号中心独立 HTTPS 子域名、`/api/v1` 支付回调公网命名空间及旧官网入口彻底移除 | UI-09 | APT-032, APT-057 | CODE+ENV | DONE |
| APT-059 | 从桌面账号 Popover 移除平台模型展示和选择职责 | UI-17 | APT-032 | CODE | DONE |
| APT-060 | 建立类型化全局/会话模型引用、作用域隔离及兼容迁移 | TOK-48 | APT-032 | CODE | DONE |
| APT-061 | 在“设置 → 模型”增加品悟平台模型组和新会话默认选择 | UI-18 | APT-060 | CODE | DONE |
| APT-062 | 在普通对话输入区直接提供具体平台模型选择并冻结到当前会话 | UI-19 | APT-060, APT-061 | CODE | DONE |
| APT-063 | 补齐模型选择三语、异常状态、迁移与跨入口回归门禁 | NFR-51 | APT-059–APT-062 | CODE | DONE |
| APT-064 | 新华三永久客户标识、原生订单查询/回调合同、十进制金额与异步履约修复 | PAY-41–PAY-43 | APT-001, APT-022, APT-023, APT-057 | CODE | DONE |
| APT-065 | 受控确认 Provider 失败：现场查询、无资金事实门禁、原子结案和不可变证据 | PAY-44, ADM-43 | APT-024, APT-025, APT-064 | CODE | DONE |
| APT-066 | 拆分目录上架与 Provider/商品止损控制，并向个人中心提供可见不可选的暂停态 | PAY-45, PAY-47, UI-20 | APT-022, APT-051, APT-064 | CODE | DONE |
| APT-067 | 支付渠道控制后台及双人恢复审批、快照失效、执行前复核与审计 | PAY-46, ADM-44 | APT-008, APT-065, APT-066 | CODE | DONE |
| APT-068 | Web 统一身份登录页重设计、短信验证码登录及手机验证码注册 | UI-21 | APT-002, APT-003, APT-034 | CODE | DONE |
| APT-069 | PERSONAL 作用域、个人默认角色、系统/组织精确真值表及独立授权版本迁移 | ACC-59–ACC-62, ADM-45 | APT-006 | CODE | DONE |
| APT-070 | 密码与短信登录在会话签发前强一致校验个人登录权限 | ID-19 | APT-068, APT-069 | CODE | DONE |
| APT-071 | Account 本人资料、上下文、注销和邀请接口接入个人原子权限 | ACC-63 | APT-069 | CODE | DONE |
| APT-072 | Billing 个人充值、余额与组织额度接口接入个人/组织原子权限 | BILL-46 | APT-069, APT-071 | CODE | DONE |
| APT-073 | 删除普通 Token Account/路由/凭据管理入口并隔离存量非平台凭据 | SCOPE-03, ADM-47 | APT-031, APT-069 | CODE | DONE |
| APT-074 | 审批列表、详情、决定和执行复核改为按冻结策略权限解析 | ADM-46 | APT-008, APT-069 | CODE | DONE |
| APT-075 | 组织管理脱离活动计费上下文并实现节点对象级授权决定 | ACC-64 | APT-011, APT-017, APT-069 | CODE | DONE |
| APT-076 | 各内部 HTTP、任务和事件入口接入精确工作负载权限并消费受签人类委托 | NFR-12 | APT-006, APT-045, APT-082 | CODE | DONE |
| APT-077 | Break-glass 可信执行身份派生并删除命令行自报操作者 | ADM-48 | APT-006 | CODE | DONE |
| APT-078 | 桌面、个人中心及后台按权威权限和版本驱动入口状态 | UI-22 | APT-070–APT-075, APT-080, APT-081 | CODE | DONE |
| APT-079 | 全 HTTP/任务/事件/回调/CLI 敏感入口授权覆盖发布门禁 | CORE-08, NFR-52 | APT-069–APT-078, APT-080–APT-082 | CODE | DONE |
| APT-080 | 已登录密码修改和匿名忘记密码接入个人安全权限及生命周期矩阵 | ID-20 | APT-069, APT-070 | CODE | DONE |
| APT-081 | Token 模型目录、调用、用量和取消接口接入个人/组织原子权限 | TOK-49 | APT-069, APT-071 | CODE | DONE |
| APT-082 | Account 签发短期不可篡改人类委托并提供独立验证契约 | ADM-49 | APT-045, APT-069 | CODE | DONE |
| APT-083 | 两层目录、异步可信准入验证/追加撤销/续证止损、安全定义生命周期、动态导入及事务外 Provider 调用 | PAY-48–PAY-52 | APT-022, APT-066, APT-076 | CODE | DONE |
| APT-084 | 商品命令并发幂等、原子权限、治理策略、目录/准入验证与撤销 API、OpenAPI 和 Portal 精确代理 | PAY-53, ADM-50–ADM-52 | APT-069, APT-083 | CODE | DONE |
| APT-085 | 权限驱动的后台商品/准入验证与撤销三语交互及 OIDC 近期认证 step-up | UI-23 | APT-034, APT-078, APT-084 | CODE | DONE |
| APT-086 | 商品配置文档去值、基线扫描和任务/状态统计一致性发布门禁 | PAY-54, NFR-53 | APT-083–APT-085 | CODE | DONE |
| APT-087 | Portal 查询参数、Tab、浏览器历史与权限加载状态一致性 | UI-24 | APT-013, APT-078 | CODE | DONE |
| APT-088 | 从普通个人中心移除角色授权展示和本人角色绑定请求 | UI-25 | APT-013, APT-078 | CODE | DONE |
| APT-089 | 权益包摘要、全量列表和组成级详情渐进式交互 | UI-26 | APT-013, APT-016, APT-034 | CODE | DONE |
| APT-090 | 全状态订单响应式摘要表格与完整详情弹窗 | UI-27 | APT-013, APT-034 | CODE | DONE |
| APT-091 | 单订单隔离、案件止损准入矩阵与商品级聚合暂停 | PAY-55 | APT-051, APT-066 | CODE | DONE |
| APT-092 | Provider 调用健康观察、分区聚合持久止损、观察缺口与历史控制迁移 | PAY-56, PAY-57, NFR-54 | APT-022, APT-045, APT-076, APT-091 | CODE | DONE |
| APT-093 | Provider/商品独立双人恢复及分级控制后台证据 | PAY-58, ADM-53 | APT-008, APT-067, APT-092 | CODE | DONE |
| APT-094 | 统一管理后台壳层、分组导航、URL 状态与共享基础组件 | UI-28 | APT-034, APT-078 | CODE | DONE |
| APT-095 | 身份、用户、角色、组织及修复页面 UI/UX 改版 | UI-29 | APT-094 | CODE | DONE |
| APT-096 | 财务、支付、审批、定价及充值商品页面 UI/UX 改版 | UI-30 | APT-085, APT-093, APT-094 | CODE | DONE |
| APT-097 | 模型、Token、系统、审计及只读运行手册页面 UI/UX 改版 | UI-31 | APT-094 | CODE | DONE |
| APT-098 | 管理后台跨页面集成、三语、无障碍、响应式与制品发布验收 | UI-32 | APT-094–APT-097 | CODE | DONE |
| APT-099 | 组织删除就绪度只读合同、结构化阻断与管理端删除闭环 | ACC-65, UI-36 | APT-009, APT-094, APT-095 | CODE | DONE |
| APT-100 | 组织详情独立 URL、浏览器历史与分区信息架构 | UI-34 | APT-087, APT-094, APT-095 | CODE | DONE |
| APT-101 | 组织目录游标/状态筛选、远程用户选择与紧凑创建交互 | UI-33 | APT-001, APT-009, APT-094, APT-095 | CODE | DONE |
| APT-102 | 组织成员服务端分页、搜索筛选与按需权限编辑 | ACC-66, UI-35 | APT-011, APT-046, APT-095 | CODE | DONE |
| APT-103 | 平台组织资料、状态、成员和结构权限拆分与兼容迁移 | ACC-67 | APT-006, APT-069, APT-075 | CODE | DONE |
| APT-104 | 平台组织节点移动管理合同及结构树交互 | ACC-68 | APT-010, APT-075, APT-103 | CODE | DONE |
| APT-105 | 财务支付三分区改名、子页迁移与稳定深链 | UI-37 | APT-094, APT-096 | CODE | DONE |
| APT-106 | 手工调账申请、审批恢复与审批中心深链 | UI-38 | APT-008, APT-105 | CODE | DONE |
| APT-107 | 支付 Provider/商品控制展示与恢复申请深链 | UI-39 | APT-093, APT-105 | CODE | DONE |
| APT-108 | 财务支付子页独立加载、局部失败与独立刷新 | UI-40 | APT-105 | CODE | DONE |
| APT-109 | Billing 既有筛选与充值商品稳定 cursor 适配 | UI-41 | APT-084, APT-108 | CODE | DONE |
| APT-110 | 财务支付商品结构化操作面板并移除浏览器 prompt | UI-42 | APT-106–APT-108 | CODE | DONE |
| APT-111 | 移除旧模型价格写入口并提供新版生命周期只读展示 | UI-43 | APT-105 | CODE | DONE |
| APT-112 | 注册六个 Billing 定价与计量管理命令策略 | ADM-54 | APT-008, APT-018 | CODE | DONE |
| APT-113 | 计量单位和模型定价完整生命周期管理界面 | UI-44 | APT-111, APT-112 | CODE | DONE |
| APT-114 | Billing 高数据量列表 cursor、筛选与稳定排序合同 | NFR-55 | APT-018–APT-025 | CODE | DONE |
| APT-115 | Portal 消费 Billing 全量分页和 URL 状态合同 | UI-45 | APT-109, APT-114 | CODE | DONE |
| APT-116 | 财务支付三个分区运营概览 API 与权限裁剪 | BILL-47 | APT-018–APT-025, APT-083, APT-093 | CODE | DONE |
| APT-117 | 财务支付三个分区运营概览、下穿与局部失败界面 | UI-46 | APT-105, APT-113, APT-115, APT-116 | CODE | DONE |
| APT-118 | 超级管理员完整常驻平台权限迁移、缓存失效与安全边界回归 | ADM-55 | APT-069, APT-073, APT-097 | CODE | DONE |
| APT-119 | owner 候选专用权限、权威目录与授权版本迁移 | ADM-61 | APT-006, APT-069, APT-118 | CODE | DONE |
| APT-120 | Token 高数量分页快照、预算、cursor、入口超时与容量基座 | NFR-56 | APT-036, APT-045 | CODE | DONE |
| APT-121 | 九类 Token 查询合同与模型/Token 资源审计投影 | ADM-56, ADM-63 | APT-119, APT-120 | CODE | DONE |
| APT-122 | Token 协议/公式四命令策略注册与不可变版本门禁 | ADM-60 | APT-008, APT-018 | CODE | DONE |
| APT-123 | 十九项 Token 命令治理基础、策略注册与持久结果恢复 | ADM-65 | APT-008, APT-074, APT-076, APT-118, APT-120, APT-122 | CODE | DONE |
| APT-124 | 模型生命周期、目录回滚及 Token 变更的影响预检 | ADM-57 | APT-028, APT-030, APT-120 | CODE | DONE |
| APT-125 | 路由稳定模型/协议解析与服务端健康探测状态机 | ADM-62 | APT-027, APT-028, APT-121, APT-122, APT-123 | CODE | DONE |
| APT-126 | 模型/运行概览与 Billing 模型价格跨域只读投影 | ADM-59 | APT-113, APT-121, APT-125 | CODE | DONE |
| APT-127 | 模型与 Token 三 section、独立 URL 状态与逐源错误隔离 | UI-47, UI-48 | APT-094, APT-097, APT-121 | CODE | DONE |
| APT-128 | 协议、安全公式、预览、发布及 Billing 同步界面 | UI-49 | APT-122, APT-123, APT-127 | CODE | DONE |
| APT-129 | Token owner/路由模型选择器与健康管理界面 | UI-50 | APT-119, APT-123, APT-125, APT-127 | CODE | DONE |
| APT-130 | Provisioning 详情、尝试历史与用量核对异常闭环 | UI-51 | APT-121, APT-123, APT-127 | CODE | DONE |
| APT-131 | 模型与 Token 结构化高风险交互与未知结果恢复 | UI-52 | APT-123–APT-130 | CODE | DONE |
| APT-132 | 模型独立详情、六页签与资源审计 | UI-53 | APT-121, APT-126–APT-128, APT-131 | CODE | DONE |
| APT-133 | Token Account 独立详情与隔离凭据新增/轮换 | UI-54, ADM-58 | APT-121, APT-123, APT-124, APT-127, APT-129, APT-131 | CODE | DONE |
| APT-134 | 模型生命周期依赖分析、计划生效与会话截止 | UI-55 | APT-123, APT-124, APT-131, APT-132, APT-133 | CODE | DONE |
| APT-135 | 模型上下文价格与计量共享编辑入口 | UI-56 | APT-113, APT-126, APT-132 | CODE | DONE |
| APT-136 | 目录差异、路由/Provisioning 健康与核对异常趋势界面 | UI-57 | APT-124, APT-126, APT-127, APT-130, APT-131 | CODE | IN_REVIEW |
| APT-137 | 模型与 Token 分级 feature gate、N/N-1 兼容、旧链接与回滚发布门禁 | UI-58 | APT-127–APT-136, APT-138 | CODE+ENV | PENDING |
| APT-138 | 十九项 Token 命令一致性总验收与原子激活 | ADM-64 | APT-122–APT-136 | CODE | PENDING |

## 3. APT-119～APT-138 详细交付合同

以下任务严格按依赖顺序开发。每项通过身份 B 独立审核后才可单独提交；任一任务不得顺带实现后续任务的 UI 或 schema。未特别声明的数据库变更均采用前向追加迁移，down 只回滚本任务新增对象并保留历史事实。

ADM-64 的十九个稳定命令不得因共用治理中间件而失去领域所有者。`策略/通用结果` 表示策略注册与 `migrations/token/000024_*` 命令结果持久化的唯一所有者；`API/领域事实` 表示公开/内部路由及最终领域事实的唯一实现任务；全部命令最终由 APT-138 做一致性验收并激活：

| `command_type` | 策略/通用结果 | API/领域事实实现任务 | 权威 API 边界 | 权威领域持久化 |
|---|---|---|---|---|
| `TOKEN_PROTOCOL_DRAFT_CREATE` | APT-122 / APT-123 | APT-122 | Token protocol draft admin API | 既有 protocol draft/head/version 表 |
| `TOKEN_PROTOCOL_PUBLISH` | APT-122 / APT-123 | APT-122 | Token protocol publish admin API | 既有 immutable protocol version/outbox |
| `TOKEN_FORMULA_DRAFT_CREATE` | APT-122 / APT-123 | APT-122 | Token formula draft admin API | 既有 formula draft/head/version 表 |
| `TOKEN_FORMULA_PUBLISH` | APT-122 / APT-123 | APT-122 | Token formula publish admin API | 既有 immutable formula version/Billing sync outbox |
| `TOKEN_MODEL_DRAFT_SAVE` | APT-123 | APT-123 | Token model draft admin API | 既有 model draft/catalog version 表 |
| `TOKEN_MODEL_PUBLISH` | APT-123 | APT-123（`IMMEDIATE` 基线）；APT-134（`SCHEDULED` 扩展） | Token 即时 publish admin API；APT-134 lifecycle plan + governed command/result GET | APT-123 既有 immutable catalog version/outbox；APT-134 `000027_*` plan/version/lease/event + 到期 worker 复用同一 publish 领域效果 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` | APT-123 | APT-134 | Account approval API → Token internal execute | APT-134 `000027_*` lifecycle plan/lease + 既有 catalog version |
| `TOKEN_MODEL_CATALOG_SYNC` | APT-123 | APT-123 | Token catalog sync admin API | 既有 catalog observation/version/outbox |
| `TOKEN_MODEL_CATALOG_ROLLBACK` | APT-123 | APT-123 | Account approval API → Token internal execute | 既有前向 catalog version/outbox，禁止破坏性回退 |
| `TOKEN_ACCOUNT_CREATE` | APT-123 | APT-123 | Token Account admin API | 既有 token account/version 表 |
| `TOKEN_ACCOUNT_STATUS_CHANGE` | APT-123 | APT-123 | Token Account status admin API | 既有 token account/version/outbox |
| `TOKEN_ROUTE_UPSERT` | APT-123 | APT-125 | Token route admin API | APT-125 `000025_*` route/version/health 表 |
| `TOKEN_ROUTE_DELETE` | APT-123 | APT-125 | Token route admin API | APT-125 `000025_*` route tombstone/version 表 |
| `TOKEN_CREDENTIAL_ENROLL` | APT-123 | APT-133 | credential origin launch/session API | APT-133 `000026_*` session/result + 既有 credential metadata/secret contract |
| `TOKEN_CREDENTIAL_ROTATE` | APT-123 | APT-133 | credential origin launch/session API | APT-133 `000026_*` session/result + 既有 credential state machine |
| `TOKEN_CREDENTIAL_REVOKE` | APT-123 | APT-133 | Account approval API → Token internal execute | 既有 credential metadata/state machine；不进入隔离页 |
| `TOKEN_CREDENTIAL_VALIDATE` | APT-123 | APT-133 | Portal → Token direct 脱敏 admin API | 既有 credential metadata + APT-123 脱敏 command result |
| `TOKEN_ROUTE_PROVISIONING_REPAIR` | APT-123 | APT-130 | Token Provisioning repair admin API | 既有 provisioning generation/attempt/outbox |
| `TOKEN_USAGE_RECONCILIATION_RETRY` | APT-123 | APT-130 | Token usage reconciliation admin API | 既有 usage/evidence/reconciliation attempt/outbox |

执行顺序特例固定为 APT-122 → APT-124 → APT-123：先交付四项协议/公式策略与只读预检计算，再交付因 activation record 尚不存在而被解释为预激活关闭态的治理基础；全部领域适配完成后才由 APT-138 创建记录并原子激活，编号不代表可以越过依赖。

### APT-119 owner 候选权限与权威目录

- **实现状态**：`DONE`；身份 B 第五轮独立代码审核通过。仓库实现尚未推送、未部署；本地未配置 `ACCOUNT_DATABASE_URL`，真实 PostgreSQL 集成用例未执行并保留为发布前隔离库门禁。
- **目标/范围**：主目标 `ADM-61`。仅修改 `migrations/account/000043_*`、`internal/account/{authorization.go,directory.go,admin.go,service.go}`、Account 内部 OpenAPI/工作负载策略、可信公网代理头合同与定向测试；提供仅经 `internal.token-owner-candidates.read` service-auth 调用的用户、组织、节点最小权威投影，Token 只消费稳定 ID、类型、标签和状态。APT-119 不提供 Portal/BFF 或公网 admin 候选路由；公开快照分页门面及搜索筛选归 APT-120/121。
- **数据/权限/回滚**：同一 Account 迁移幂等创建 `platform.tokens.owner-candidates.read`，仅原子授予 `PLATFORM_SUPER_ADMIN`/`PLATFORM_MODEL_ADMIN`；权限 scope 修复、正确边新增、错误边撤销先合并角色集合，再按用户与作用域去重，使每个有效授权版本恰递增一次。内部扫描默认 20,000、最大 50,000，与未来公开 UI 的 20/50 完全分离；cursor 使用独立 HMAC 密钥、15 分钟 TTL，绑定 `scan_id`、调用 service/workload subject、人类 issuer/subject、authorization version、resource、filter 与同事务数据库时间高水位，按不可变 `(owner_type,id)` 采集。仅首请求或非法 cursor 消耗 actor 30/min、caller service 60/min 配额，合法续页继承逻辑扫描；每 actor/service 同时最多 3/10 个活跃扫描。down 按实际边变化去重失效。不新增 feature gate。
- **验收**：精确两角色允许、其他平台/组织/个人角色无泄露拒绝；Portal/公网路径不存在；伪造来源头和直连不可获得工作负载身份；逻辑扫描配额、活跃扫描上限跨副本精确；空候选、错误 scope 且两个正确边均预存、错误角色边、重复 up/down、授权变更、cursor 篡改/跨主体/过期均自动测试。PostgreSQL 集成还必须在 10 秒预算内完整遍历 1,500 条以上数据、证明续页仅计一次并证明首屏后按 DB 时间插入不进入同一扫描；以隔离库 `ACCOUNT_DATABASE_URL=... go test ./internal/account -run TestTokenOwnerCandidatesPostgresIntegration -count=1` 执行，本地未配置时明确记录未运行。
- **不包含**：不改 Token Account 创建表单、不提供 UUID 盲填、不实现 Token 分页快照或路由模型选择器。

### APT-120 Token 分页快照、入口预算与容量基座

> 实施状态（2026-09-03）：`DONE`；共享 schema、签名 cursor、容量/清理/授权失效、稳定错误和三类绝对 deadline 链已实现，并通过身份 B 第三轮独立代码审核。当前尚未推送、未部署。本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`，真实 PostgreSQL 重型用例未执行；PR 门禁创建独立数据库、显式运行三项命名用例并逐项要求 PASS、拒绝任何 SKIP，该门禁仍须在候选提交上实际通过。

- **目标/范围**：主目标 `NFR-56`。修改 `migrations/token/000023_*`，新增 `internal/token/admin_pagination*.go`，同步 Token OpenAPI 通用 envelope/error schema、`internal/portal/{static.go,service.go}` 的 BFF 超时、Portal HTTP server 配置以及 `deploy/{compose,helm}` 的 ingress/upstream timeout 模板和合同测试。本任务拥有快照头、成员、墓碑、清理和容量索引 schema，后续任务不重复改该 schema。
- **预算/兼容/回滚**：三类完整链唯一冻结为：首屏快照构建 `Token 10s → BFF 12s → ingress 14s`，已有快照读取 `Token 3s → BFF 5s → ingress 7s`，跨域预检 `Token 5s → BFF 7s → ingress 9s`。入口生成并透传同一个绝对 deadline，BFF/ingress 只能保留传输余量、不得逐层重新计时或放大业务预算；服务端按请求类型只消费一种业务预算。cursor 签名绑定主体/授权/资源/筛选/排序/limit/高水位，实现 400/410/422/429/503/504 稳定错误。feature gate 关闭时不产生新快照；down 前停新建、让活动 cursor 返回 410 后再移除快照表。
- **验收**：默认/最大 limit、全部容量硬边界、70%/85% 水位、第三资源回收、并发翻页无重漏、事务中断无半快照均通过；对 `10→12→14`、`3→5→7`、`5→7→9` 三链分别做真实代理、时钟偏差和故障注入，证明绝对 deadline 未被重置、入口保留足够余量且不先于 Token 稳定错误响应。
- **不包含**：不改九类业务查询、Portal URL、owner Account schema 或任何写命令。

### APT-121 九类查询与资源审计投影

> 实施状态（2026-09-03）：已完成并通过身份 B 第四轮独立代码审核（DONE），尚未提交、推送或部署。已完成九类查询、资源审计和 evidence 投影，并对 APT-119 Account 内部 owner 权威源做了仅增加可选 `organization_id` 过滤的向后兼容集成修正；本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`，真实 PostgreSQL 集成用例未执行，PR 隔离库门禁必须逐项 PASS 且拒绝 SKIP。

- **目标/范围**：主目标 `ADM-56`、`ADM-63`。修改 `internal/token/{admin.go,catalog.go,catalog_http.go,protocol_admin.go,management.go,provisioning.go,provisioning_http.go,reconciliation.go,postgres.go}`，新增资源审计/用量 evidence 只读投影文件，同步 Token/Portal BFF OpenAPI 与合同测试。
- **查询合同**：严格按专项设计 4.1 为模型、协议、公式、Token Account、Provisioning、用量、核对异常、资源审计及 APT-119 owner 候选分别适配独立 filter/cursor 和确定全序，拒绝客户端 sort。evidence 投影唯一键 `(source_kind,source_id)`，时间映射和三元 cursor 不得变体。资源审计要求资源读权与 `platform.audit.read` 交集。
- **兼容/回滚/验收**：旧无 cursor 请求在 gate 关闭时保持旧合同，开启时使用新 envelope；回滚仅停新适配层。验证九类参数映射、无权/不存在无泄露、单源失败、审计不扩权、evidence 同时间稳定翻页。
- **不包含**：不改快照 schema、不实现 Portal 列表/详情、不增加审计导出或新可写事实。

### APT-122 协议/公式命令与策略注册

> 实施状态（2026-09-03）：DONE，已通过身份 B 第三轮独立代码审核，尚未推送或部署。四个稳定命令、Account 000044 追加策略、CAS/ETag、结构化审计与事务 Outbox、Billing 权威计量单位只读校验与数据库实际不可变公式绑定回执/同一 T1 重放、协议发布引用快照复验、15 分钟近期认证及默认关闭 activation gate 已完成；本地未配置 PostgreSQL 集成环境，三个真实迁移链命名用例未执行，由 PR 隔离库门禁强制 PASS 且拒绝 SKIP。

- **目标/范围**：主目标 `ADM-60`。修改 `migrations/account/000044_*`、`internal/platform/approvalpolicy/{registry.go,registry_test.go}`、`internal/token/{protocol_admin.go,protocol_admin_http.go}`（按仓库实际路由文件名落位）、Token OpenAPI及协议/公式持久化合同测试；本任务拥有四个协议/公式 public API 与既有 draft/head/immutable version/outbox 写入适配。
- **数据/回滚**：前向追加注册四个稳定 command type 为精确权限的 `AUDIT_ONLY`，交付策略、handler 和自动化测试，API 冻结专项 7.2 的 reason、CAS、幂等、预检、审计与结果查询输入输出；不改变既有领域表语义和发布版本不可变性。APT-138 activation record 不存在时，公开新增协议/公式写路径必须保持失败关闭且不可达，只有 APT-138 持久状态为 `ACTIVE` 才能选择这些 handler。down 只删本迁移策略版本，被审计引用时失败关闭；通用持久命令结果由后续 APT-123 唯一建表。
- **验收**：四路由从 registry 解析到对应 `platform.pricing.*` 权限并正确写入权威 draft/version/outbox，未注册/错类返回 `COMMAND_POLICY_UNAVAILABLE`；DB↔registry 逐字段一致且旧版本不被更新。增加默认态门禁测试，证明代码、策略和 handler 已安装但 activation record 缺失时公开新增写请求不会进入 handler、不会写领域事实或审计 outbox。超时仅返回待查询的稳定 command reference；持久未知结果收敛由 APT-123 基础实现并由 APT-138 做四命令全链路验收，本任务不得另建临时结果表或自行重试。
- **不包含**：不改协议/公式 UI、不将命令改为强制审批、不修改三项 Token 高风险策略。

### APT-123 十九项 Token 命令治理与三项强制审批

- **实现状态**：`DONE`；已通过身份 B 第五轮独立代码审核。Account `000045`、Token `000024`、十九项策略注册与通用持久结果、三项审批精确权限/策略、actor 绑定结果查询、最小 executor 内部路由与 workload allowlist、中间提交 PRE_ACTIVATION 门禁已实现。十九项领域 adapter 的最终接入与验收仍由下表对应后续任务负责；activation record/epoch/state machine 及审批事件 consumer 仍唯一归 APT-138，均未在本任务提前实现。尚未推送或部署；本地未配置 Account/Token PostgreSQL URL，真实迁移链命名用例未执行，CI 已强制逐项 PASS 且拒绝 SKIP。
- **目标/范围**：主目标 `ADM-65`，仅交付 `ADM-64` 的预激活基础且不得关闭 ADM-64。修改 `migrations/account/000045_*`、`migrations/token/000024_*`、`internal/platform/approvalpolicy/{registry.go,registry_test.go}`、`internal/account/{approvals.go,approval_execution.go,approval_worker.go}`、`internal/token` 十九项命令治理中间件/结果查询/审计 outbox/内部执行路由、`cmd/{account-service,token-gateway}` 及对应 OpenAPI、事件和工作负载权限配置。本任务唯一拥有其余十五项策略注册、三项审批权限/策略以及十九项通用命令结果、幂等指纹和恢复索引 schema；APT-122 已拥有四项协议/公式策略版本且本任务不得重写。十九项领域 adapter 不在本任务接入；本任务也不创建 activation record、epoch、状态列、状态机或审批事件 consumer，这些激活对象与 consumer 唯一归 APT-138/`000029_*`；`000028` 后续由 APT-136 用于运行概览索引。
- **十九项共同合同**：逐项执行下表所有者映射；所有管理命令创建 API 接受必填 reason、专项 7.2 指定 CAS/版本和 16～128 字符 `Idempotency-Key`（APT-133 隔离 origin 的 `/launch` 不是命令创建 API，固定只接收 `launch_id`），持久化 command type、actor/permission、请求摘要、幂等指纹、预检/关键版本、前后状态、审计 request_id 与脱敏结果。提交前重验权限/CAS/预检；唯一键冲突返回同一结果或 `IDEMPOTENCY_KEY_REUSED`；超时只通过结果查询恢复，禁止盲重试、`force`、`ignore_blocker`。`TOKEN_CREDENTIAL_ENROLL`、`TOKEN_CREDENTIAL_ROTATE` 也必须注册为 `AUDIT_ONLY`，结果只保存不透明引用和脱敏元数据。
- **审批/权限/执行**：严格写入专项 7.3 三组 13 字段及 exact ordered key array；lifecycle/rollback 保留 v1 并追加 v2，revoke 新增 v1；六权限仅原子授予超级/模型管理员并递增 authorization version。`executableApprovalCommands` 保留全部既有合法项，新增 Token 子集恰为三项；`token-command-executor` 仅持有 approval begin/complete 和三个 `internal.token.*.execute`。人类 direct 固定 403 是已安装但只有 APT-138 `ACTIVE` 才会被路由选择的新分支，不得在预激活阶段提前覆盖旧 v1 direct。
- **状态/兼容/回滚**：APT-138 的 activation record 不存在即被所有 N/N-1 组件解释为预激活关闭态：旧 v1 direct 保持原行为，新审批入口、executor 消费、新治理 envelope 和新增协议/公式写路径全部关闭；本任务严禁自行持久化 `PRE_ACTIVATION`、epoch 或写 `ACTIVE`。故障回滚只关闭基础 gate，并保留结果、审计及已引用策略；activation 记录缺失语义、最终状态机和原子激活唯一归 APT-138。
- **验收**：逐项验证十九项策略可解析以及通用权限/近期认证/reason/CAS/预检/持久幂等/不可变审计/稳定错误/未知结果恢复中间件合同；真实迁移后验证 DB↔registry 13 字段/数组顺序、allowlist 保留/增量、授权快照、最小执行身份和 enroll/rotate 零 Secret。增加中间提交门禁，证明 APT-122/123 代码、迁移、策略和 handler 已安装但 activation record 缺失时仍保持旧 v1 direct 原行为，且新审批、新 executor 消费、新治理 envelope、新协议/公式写均不可达、无双写。本任务不做十九项领域全链路一致性结论，该结论唯一归 APT-138。
- **不包含**：不实现 APT-124 的预检计算、凭据隔离页、Portal 操作面板、生命周期计划、目录差异或各领域既有事实状态机。

### APT-124 跨域影响预检

- **实现状态**：`DONE`；五类只读预检、同一 Token 快照的固定全序流式关键集合 SHA/组合 ETag、规范化双域预检指纹、Billing 单事务批量价格覆盖、逐 tuple READY/替代资源事实、默认关闭 gate、5 秒绝对 deadline、脱敏错误和真实迁移链集成门禁已实现，并通过身份 B 第五轮独立代码审核。随本任务创建独立提交，尚未推送或部署；本地未配置 Token/Billing PostgreSQL URL，真实 PostgreSQL 用例未执行，CI 隔离库命名门禁必须 PASS 且拒绝 SKIP。
- **后续整改不抹除原结论**：APT-124 的原五类预检交付继续保持 `DONE`。APT-131 第三轮审核发现，结构化高风险交互需要新增的通用权威预检、完整财务快照和执行事件透传不属于 Portal/UI 任务，因此另立 `APT-124-R1 / REOPENED_REMEDIATION`；该批次只追加合同，不把 APT-124 已通过的原范围改回未完成，也不得与 APT-131 混合提交。
- **依赖表示**：APT-123 已完成时依赖的是 APT-124 原交付，历史顺序和证据继续有效；正式任务表无法把同一编号的历史完成版本与当前 R1 分成两条依赖边，因此 APT-123 不再声明对当前 `IN_PROGRESS` 的 APT-124 活边。APT-124-R1 反向依赖已完成的 APT-123 基座，不能据此重开 ADM-65。
- **目标/范围**：主目标 `ADM-57`。修改 `internal/token/{catalog.go,catalog_http.go,management.go,provisioning.go,reconciliation.go,billing.go}` 及 Token/Billing 内部客户端和 OpenAPI，只增加模型生命周期/目录回滚、Token Account 状态、路由、凭据动作的只读预检。
- **合同/回滚**：返回当前版本、价格覆盖、路由/Provisioning/在途请求、会话/替代模型、租约和替代凭据的脱敏摘要；共享 APT-120 唯一 5s deadline，失败关闭。无 schema 所有权，以 feature gate 关闭新 GET 即可回滚。
- **验收**：任一下游超时统一 `PRECHECK_TIMEOUT`；预检后并发变更使正式命令 409/失效；无权/不存在无泄露；不返回 Provider 地址、Secret 或原始错误。
- **不包含**：不改实际命令状态机、不创建审批、不做 UI 影响面板。

### APT-125 路由解析与健康状态机

- **实现状态**：`DONE`；稳定模型/协议路由、版本化 v2 管理合同、服务端健康探测与 TTL、精确 Provisioning 引用、影响证据、迁移兼容及 CI PostgreSQL 门禁已实现，并通过身份 B 第四轮独立代码审核。APT-138 尚未创建或激活持久 activation record：`PRE_ACTIVATION` 继续保留旧 v1 direct Portal 合同，`ACTIVE` 才选择 v2 稳定写合同；两条领域事务均预留 activation lock/epoch 的事务内起始及提交前重检边界，当前生产仍保持预激活状态。当前尚未推送或部署；本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`，真实 PostgreSQL 用例显式 SKIP，CI 隔离库必须逐项 PASS 且拒绝 SKIP。
- **目标/范围**：主目标 `ADM-62`。修改 `migrations/token/000025_*`、`internal/token/{routing.go,management.go,provisioning.go,gateway_adapter.go}`、`cmd/token-gateway`、Token OpenAPI 和健康 worker/指标测试。本任务唯一拥有 route-health schema。
- **合同/回滚**：路由写入只接收稳定 `model_id`、已发布 `protocol_version_id`、策略/版本/幂等字段，含 health/upstream 写入值即拒绝；服务端生成 `UNKNOWN/PROBING/HEALTHY/DEGRADED/UNHEALTHY/STALE`，HEALTHY TTL 5m。回滚先停探测 worker，保留最后证据为 STALE，再回滚读投影。
- **验收**：非发布模型/协议、客户端 health/upstream、过期/缺证健康全部失败关闭；并发探测、TTL、单调状态迁移、失败恢复和指标可观测均通过。
- **不包含**：不做 Portal 选择器、不修改模型目录事实或 Provider 凭据。

### APT-126 运营概览与跨域价格投影

- **目标/范围**：主目标 `ADM-59`。修改 `internal/token` 新增模型/运行概览查询，`internal/billing/{pricing_resolution.go,pricing_admin_v2.go,service.go}` 新增按 `model_id` 价格投影，同步两服务 OpenAPI、Portal BFF 代理白名单和定向测试。
- **边界/回滚**：Token 只聚合目录/路由/Provisioning/用量，Billing 仍是当前/未来价格、计量单位、继承和覆盖事实源；不跨库原子、不复制价格、不扫描分页列表算指标。无新 schema，两个读 gate 可分别关闭。
- **验收**：指标明确时间窗/时区/分母，403 隐藏、失败不伪装 0；Billing 无权/不可用/结果不确定都不返伪价格；并列请求局部失败可恢复。
- **不包含**：不做概览/详情 UI、不改定价写合同或生命周期。
- **实现状态（2026-09-04）**：`DONE`；身份 B 首轮独立代码审核通过。Token 端已实现 DB 权威时间的单语句原子聚合、默认 24 小时/最大 31 天 UTC 半开窗、稳定路由健康分母和 3 秒失败关闭；Billing 端已实现稳定 `model_id` 加精确协议/公式/积分单位的 repeatable-read 当前与下一实际变化投影，显式区分 `AVAILABLE/MISSING/BLOCKED`，并返回逐单位继承来源与价格空间 ETag。两个入口由 `TOKEN_ADMIN_OPERATIONS_OVERVIEW_ENABLED`、`BILLING_ADMIN_MODEL_PRICE_PROJECTION_ENABLED` 独立控制且默认关闭；Portal 仅独立透传，不组合快照或宣称跨域原子。OpenAPI、Ajv 实际响应、授权清单、指标和真实 PostgreSQL CI 禁止 SKIP 门禁已补齐；本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`、`PINVOU_TEST_BILLING_DATABASE_URL`，两个命名集成用例明确 SKIP，CI 隔离库必须逐项 PASS 且拒绝 SKIP。当前未推送、未部署。

### APT-127 三页架构、URL 与错误隔离

- **目标/范围**：主目标 `UI-47`、`UI-48`。修改 `pinvou3-app/src/features/account-platform/{AdminPlatformView.jsx,AdminModelTokenWorkspace.jsx,admin-client.js,admin-model-token-state.js,admin-navigation.js,admin-platform.css,authorization-ui.js}` 与 `pinvou3-app/src/shared/admin-platform-i18n.js`，由构建同步 `internal/portal/static/assets/{admin.js,admin.css,account-platform-i18n.js}`；Portal session gate 使用 `cmd/token-portal/main.go`、`internal/portal/{service.go,service_test.go}`，默认关闭值同步 `deploy/compose`、`deploy/helm`。合同验证使用现有 `test/contract/admin-portal-ui-redesign.test.mjs` 和新增导航/状态测试。不改业务 schema 或领域写合同。
- **交付/兼容**：新增“模型管理/Token 资源/运行与用量”三 section，九数源和详情嵌套数据分别管理 URL filter/cursor/error/loading/refresh；跨 section、legacy `section=tokens`、popstate、非法/无权/互斥详情按已批清理顺序处理，零详情请求/零 DOM/日志泄露。
- **feature gate/回滚/验收**：APT-127 交付独立、默认关闭的一期 IA 功能开关及本地/环境装配；on 时 supersede UI-28/UI-31 Token Gateway canonical，off 时历史入口与调用 URL 不变，先关 gate 即可 UI 回滚。APT-137 只负责跨版本分级发布、生产激活/回滚编排和生产等价门禁，不重复实现本期开关或页面。验证 model/token/usage/pricing-only、无权 403、刷新/分享/前进后退、单源错误/重试不清其他数据、三语、AA、320px/200%。
- **不包含**：只接入 APT-121 真实读 API 与空/错态；不做协议公式写入、选择器、运行详情、高风险动作或二期资产详情。
> 代码审核记录（2026-09-04）：身份 B 第三轮 `PASS`。已完成默认关闭的一期 IA gate、三 section 与九类具名 URL/API 映射；当前仅渲染模型、Token Account、Provisioning、用量四类 APT-121 既有列表，逐源维护加载、错误、重试、数据时间、筛选和 cursor，并在 410 时只重建目标数据源首屏。legacy/gate-off 保持旧 `Token Gateway` 与旧调用 URL；无权、非法、跨 section 和互斥详情会在请求前同步归一并清理，operations 页签切换对候选 URL 使用同一归一逻辑且只写一次 history。生产请求规划和逐源状态转换由可测试模块复用，覆盖 usage-only 零 Provisioning 请求、查询首帧无旧行、迟到响应隔离与单源失败。三语、键盘页签、可见焦点、320px/200% 响应式、Portal session gate、Compose/Helm 默认值、BFF allowlist 和生成制品已同步。APT-128 及后续协议/公式操作、选择器、详情和高风险写入仍不在本任务内；尚未推送或部署。

### APT-128 协议与安全计量公式工作台

> 实施状态（2026-09-04）：`DONE`；身份 B 第五轮独立代码审核通过。已按 UI-49、APT-122/123 合同和 APT-127 信息架构完成协议/公式独立分页筛选、草稿/安全预览/发布、Billing 权威单位校验与同步回执、结构化命令交互、三语和响应式实现。写入携带服务端头版本或不可变草稿 ETag、固定幂等键及原因；成功响应逐动作校验完整资源与不可变快照，未知结果锁定同一对话框，页面不执行或回显任意公式表达式。APT-123 `PRE_ACTIVATION` 关闭态继续由 Token 服务端 503 失败关闭，APT-138 未激活；APT-131 通用高风险恢复、APT-132 模型详情与 APT-135 价格编辑均未提前实现。Node 定向行为测试、Portal/Token Go、OpenAPI、ESLint、Portal build、i18n、architecture guard 和 diff 门禁通过；当前尚未推送或部署。

> 代码审核记录（2026-09-04）：身份 B 首轮 `NEEDS_CHANGES` 的四项已修复，等待复审。四类写命令现在逐动作验证 64 位十六进制引用、稳定命令类型、`EXECUTED`、目标对象/版本/状态及安全 precheck；公式发布还验证 `SYNCHRONIZED` 与 Billing 登记摘要，任何畸形 2xx 均按未知结果锁定且不刷新。写请求发出后的原始 504、非标准响应、网络拒绝和无法证明零写的代理错误统一保持原幂等上下文进入未知锁定；公式草稿只读取真实 `precheck.metering_unit_version_count` 并投影数量，不读取或回显单位 ID 集合。来源标题关联 ID，预览失败对话框已补齐可访问名称、初始焦点、焦点圈闭、Escape 和关闭后焦点恢复。定向 Node 已增至 25 项通过。

> 代码审核记录（2026-09-04）：身份 B 第二轮 `NEEDS_CHANGES` 的两项已修复，等待复审。客户端现在按每类动作完整验证 OpenAPI 命令信封及协议/公式资源的必填字段、精确类型、RFC 3339 时间和不可变内容：草稿逐字段匹配本次请求，发布逐字段匹配提交前列表快照，协议 schema 及公式 expression、evaluator、Billing 权威单位映射任一缺失或变化均进入未知结果锁定。可证明零写只接受 Token/Portal 实际产出的精确 HTTP 状态与稳定错误码组合；504 无条件未知，网络错误、非标准响应、错配组合和无法证明零写的 5xx 同样未知。定向行为测试已覆盖缺字段、错类型、快照漂移、四类真实命令/单位校验错误以及状态码错配。

> 代码审核记录（2026-09-04）：身份 B 第三轮 `NEEDS_CHANGES` 的两项已修复，等待复审。四类动作的 precheck 和公式发布 Billing 回执均改为精确键集合，拒绝任意额外或安全敏感字段；公式草稿逐项验证 Billing 返回的 `unit_code/unit_definition_id/version/status`，要求 ACTIVE、精确类型、唯一且与请求映射一一对应。新建协议/公式及发布目标严格要求非空 RFC UUIDv7 和正确 variant，关联计量单位至少要求非空 RFC UUID；合法 UUIDv7、合法关联 UUID、nil、错 version/variant、嵌套额外字段及映射漂移均已加入负向行为测试。

> 代码审核记录（2026-09-04）：身份 B 第四轮 `NEEDS_CHANGES` 的唯一项已修复，等待复审。公式请求、公式资源和 Billing 单位校验结果三层均按规范化 `unit_definition_id` 强制唯一，大小写不同但语义相同的 UUID 视为重复；单位代码只接受无前后空白的大写规范形式。不同单位代码复用同一 UUID、大小写变体重复、非规范代码均在写入前或成功响应确认前失败，合法多单位即使 Billing 返回顺序不同仍可通过精确映射校验。

> 代码审核记录（2026-09-04）：身份 B 第五轮 `PASS`。确认四类命令的响应、precheck、Billing 回执、UUID 与单位映射均按冻结合同失败关闭；精确错误对、未知结果锁定、权限裁剪、局部错误、键盘焦点、三语及响应式边界通过复审。APT-123 仍为 `PRE_ACTIVATION`，本任务未创建 activation record，且未越界实现 APT-131、APT-132 或 APT-135。

- **目标/范围**：主目标 `UI-49`。修改 `pinvou3-app/src/features/account-platform/{AdminModelTokenWorkspace.jsx,AdminProtocolFormulaWorkspace.jsx,admin-client.js,admin-navigation.js,admin-model-token-state.js,admin-protocol-formula-state.js,admin-platform.css}`、`pinvou3-app/src/shared/admin-platform-i18n.js`、Portal 生成制品、既有精确 BFF 白名单验证及协议/公式 UI 合同测试；接入 APT-122 的真实草稿、预览、发布和 Billing 同步命令。OpenAPI 写入/预览合同仍以 APT-122 已冻结的 `contracts/openapi/admin.yaml` 为单一真相源，本任务只补客户端精确映射，不修改服务端领域合同。
- **权限/状态/回滚**：读取、编辑、预览、发布分别按 `platform.pricing.*` 精确裁剪；协议和公式各自维护 filter/cursor/loading/error/refresh，单源失败不清空另一源。受 APT-127 一期 gate 约束，关闭 gate 即恢复旧入口；无数据库变更。
- **验收**：四类命令、公式确定性预览、版本不可变、Billing 不可用/超时、无权无泄漏、三语、键盘与 320px/200% 均有合同或 E2E 证据；页面不得执行任意表达式或展示未脱敏 Provider 数据。
- **不包含**：不改命令策略 registry、协议/公式服务端写模型、模型详情、价格编辑或高风险操作通用面板。

### APT-129 Token 所有者与路由模型选择器

> 实现状态（2026-09-04）：`DONE`，已通过身份 B 第四轮独立代码审核，尚未推送或部署。Token 资源页已按 `platform.tokens.manage + platform.tokens.owner-candidates.read` 双权限和默认关闭的 `admin_token_resource_forms` 子 gate 裁剪创建入口；用户、组织、组织节点候选均消费 APT-119 权威服务端搜索与 cursor，manage-only 不请求候选且不提供手填 ID 回退。路由表单只从已发布目录级联选择稳定 `model_id/protocol_version_id`，写入仅走 APT-125 v2 合同；APT-138 未激活时失败关闭，绝不回退 v1。健康仅展示服务端探测状态、数据库权威 `as_of`、观测时间、TTL 与诊断代码，无可信时间或证据异常时按 UNKNOWN/STALE 不可路由。实现修改 Portal 精确 v2 PUT 白名单、默认关闭配置、客户端/导航、三语、响应式样式和合同测试；未实现 APT-131 通用高风险框架、APT-133 凭据或任何新领域事实。

> 代码审核记录（2026-09-04）：身份 B 第四轮结论为 `PASS`。此前首轮六项、第二轮两项及第三轮两项问题均已修复：权限或子 gate 撤销会在候选/模型请求及渲染前清理 owner URL 组并卸载路由表单，既有候选/模型请求随组件清理中止；授权刷新同步重建 URL 派生状态，避免恢复权限后复活已清理弹窗。路由提交以完整规范化 payload 固定幂等键并用同步 inflight guard 阻止双发，仅精确可证明零写的 HTTP status/code 组合允许同 payload 原键重试或变更 payload 后换键，网络丢失、504、错配组合及非白名单 5xx 锁定原键；浏览器门禁实际覆盖 `409 + INTERNAL_ERROR`，证明改 payload 后仍不再发请求且保留原键。健康投影只使用 Token 详情的数据库权威 `as_of`，不使用浏览器时钟或自设容差；详情由同一只读 Repeatable Read 事务读取 `as_of`、账户、凭据及路由，列表查询不携带额外投影列。证据缺少权威时间、时间非法/倒置、TTL 超过五分钟或观测晚于 `as_of` 均降为 UNKNOWN，按 `as_of` 已过期则降为 STALE，均不可路由。真实 PostgreSQL 门禁覆盖列表列匹配、详情仓储及 HTTP JSON 同快照投影；本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL` 时显式 SKIP，CI 强制不得 SKIP。真实 Portal Chromium smoke 已穿过 `AdminPlatformView → AdminModelTokenWorkspace → AdminTokenResourcesWorkspace → AdminPlatformClient`，记录并断言单次 `replaceState`、候选/模型请求与 `AbortSignal`、DOM 卸载、双击单写和幂等键冻结/换键；定向 Node、Go、OpenAPI、ESLint、Portal 构建、i18n、架构、任务覆盖与 diff 门禁均通过。Portal 只开放本任务实际使用的 v2 PUT，冻结 admin v2 合同按路径精确允许。

- **目标/范围**：主目标 `UI-50`。修改 `pinvou3-app/src/features/account-platform/{AdminTokenResourcesWorkspace.jsx,admin-token-resource-state.js,AdminModelTokenWorkspace.jsx,AdminPlatformView.jsx,admin-client.js,admin-navigation.js,admin-platform.css}`、`pinvou3-app/src/shared/admin-platform-i18n.js`、`pinvou3-app/tests/admin_token_resources_ui_smoke.mjs`、前端 smoke 选择器、Token Account 详情 `as_of` 投影及 OpenAPI、Portal gate/BFF 白名单与生成制品、部署默认值、选择器及 Portal 合同测试；消费 APT-119 owner candidates 与 APT-125 稳定模型/协议、健康投影。
- **权限/兼容/回滚**：创建入口必须同时具备 `platform.tokens.manage` 与 `platform.tokens.owner-candidates.read`；仅 manage 时展示可解释的权限死路但不发候选请求，仅 owner-candidates 时不展示创建入口。写入只提交稳定 ID，拒绝手输 UUID、显示名、health 或 upstream；无数据库变更，关闭表单子 gate 回退旧页面只读能力。
- **验收**：用户/组织/节点候选搜索、空结果、续页、撤权 cursor 失效、模型/协议级联、HEALTHY TTL/STALE 展示、无权/不存在不可区分及键盘交互通过；覆盖 manage-only、candidate-only 和双权限矩阵。
- **不包含**：不改 owner 权限迁移、路由健康状态机、凭据、Provisioning 或模型目录事实。

### APT-130 Provisioning、用量与核对异常闭环

> 实现状态（2026-09-04）：`DONE`；身份 B 第三轮独立代码审核 `PASS`。Provisioning 新增版本化安全详情投影且不内联 attempt、worker 或自由文本，旧 v1 合同保持兼容；核对异常快照与 loader 共用 `TOKEN + USAGE_RECONCILIATION_REVIEW + USAGE_REQUEST + usage_requests EXISTS` 权威约束；repair 幂等按 actor/command/digest 隔离并校验 16–128 字符 key；核对原因仅进入审计，运行投影仅允许受控诊断码白名单，其余历史值统一脱敏，reason 按 Unicode 1–500 字符校验。实现严格复用 APT-121、APT-123 与 APT-127，子 gate 默认关闭，不新增数据库事实，也未提前实现 APT-131、APT-132+ 或 APT-138。Portal 真实 smoke 已覆盖正式异常类型、详情与 attempt 网络闭合投影、从不请求 v1 内联详情、attempt/evidence 局部失败与重试、异常下穿、窄屏回流、双击单写、未知结果同键恢复、撤权中止、审计跳转及 Secret/PII 不进入运行 DOM；Go、OpenAPI、Portal/BFF、前端合同、i18n、构建与架构门禁通过。真实 PostgreSQL 门禁覆盖权威异常筛选、pre-v2 明文 key 同 actor 恢复/跨 actor 隔离、同 actor 并发仅一次 repair/command/audit 与审计专用 reason，在本地无测试数据库时按约定 `SKIP`，CI 保持 no-SKIP。当前未推送、未部署。

> 后续整改不抹除原结论：APT-130 的安全详情、分页、异常队列和原 direct repair/reconcile 闭环继续保持 `DONE`。APT-131 第三轮审核发现，把 repair/reconcile 接入通用治理结果所需的闭合 target、CAS 与结果恢复适配属于运行领域，因此另立 `APT-130-R1 / REOPENED_REMEDIATION`；该批次不得改写 APT-130 已审核通过的查询、脱敏和“绝不重发模型请求”结论。

- **目标/范围**：主目标 `UI-51`。修改 `internal/token/{admin.go,admin_queries.go,admin_queries_http.go,service.go}` 的脱敏详情/attempt 分页查询与 repair/retry API 适配，复用既有 generation/attempt/outbox 写入；修改 Portal“运行与用量”页、稳定 URL 的主内容详情子视图、BFF 白名单/i18n/CSS及合同测试；接入 Provisioning 详情与 attempt history、用量详情/evidence、核对异常队列和“仅重新核对”命令。禁止使用抽屉、覆盖弹层或把详情挤入列表同一布局。
- **URL/状态/回滚**：Provisioning、用量、异常队列及用量 evidence 分别持有独立 filter/cursor/loading/error/refresh；仅 `provisioning_id` 与 `usage_request_id` 是详情深链参数且互斥进入全宽主内容子视图。核对异常不设独立详情参数，从异常队列选择记录后沿用其权威 `usage_request_id` 进入同一用量详情，并在该详情内显示核对证据/操作；面包屑和返回保留来源队列及各自主列表状态。受一期 gate 约束且无数据库变更，关闭子 gate 隐藏详情/处置入口但保留只读列表。
- **验收**：九类查询合同中的三类页面翻页、并发局部失败、重试、空态、403/404 无泄漏、evidence 三元 cursor、未知结果恢复和审计跳转通过；文案明确“重新核对”绝不重新请求模型。
- **不包含**：不改 Provisioning/用量事实、分页快照 schema、模型请求重放机制、趋势聚合或 Token 详情。

### APT-131 结构化高风险操作交互

> 实现状态（2026-09-04）：`DONE / REMEDIATION_REVIEW_PASS`。APT-124-R1、APT-130-R1 已分别通过身份 B 第三轮、第二轮审核并独立提交；APT-131-R1 已按 Portal/UI 所有权完成统一结构化对话框、权威命令/审批接线、可信审批分类证明、终态/未知恢复、撤权/关 gate 中止和安全端口浏览器 smoke，并通过身份 B 第二轮独立复审。三批整改全部闭环，仍未推送或部署。

- **目标/范围**：主目标 `UI-52`。APT-131 仅拥有 Portal 通用命令对话框/影响面板/命令客户端、BFF 精确代理、审批分类证明、终态/未知状态机、i18n/CSS 和真实浏览器 smoke；消费 APT-124-R1、APT-130-R1 的权威合同，但不在 Portal 计算预检、财务指纹、领域版本或治理结果。
- **治理/兼容/回滚**：统一采集 reason、近期认证、CAS/version、idempotency key 和适用生效信息；依据服务端策略显示 `AUDIT_ONLY` 或 `APPROVAL_REQUIRED`。关闭动作 gate 时，Token 审批 submit/withdraw/decide 必须在代理前凭本地可信分类证明零 Account 调用失败关闭；非 Token 审批由 Account 创建或详情响应生成的同类证明继续正常代理。未知结果仅按冻结 `command_reference` GET；关闭 gate 或撤权会卸载 UI 并中止在途请求。
- **验收**：通用 UI 覆盖协议/公式、同步/发布、生命周期/回滚、Token Account/路由、Provisioning repair 和用量 reconciliation，但每个动作只能消费已经通过前置整改审核的权威 precheck/command/result 合同。可信 `SUCCEEDED/REJECTED/FAILED` 必须收敛为终态，只有 `PENDING/UNKNOWN`、无权、未找到或网络失败维持未知且零写；清除本分区 `prompt/confirm`，通过三语、焦点圈定、320px/200%、撤权 abort、双击幂等及安全端口浏览器 smoke。凭据领域由 APT-133、计划排程由 APT-134、十九项原子激活由 APT-138 验收。
- **不包含**：不创建或修改审批策略/权限，不实现 Token/Billing 预检计算、领域命令解析、领域 CAS 或执行事件，不写凭据 Secret，也不实现生命周期计划或独立详情 IA。

#### APT-131 第三轮任务所有权整改与提交顺序

| 整改批次 | 状态 | 依赖 | 唯一代码所有权 | 必须通过的独立验收 | 独立提交顺序 |
|---|---|---|---|---|---|
| `APT-124-R1` | `DONE / REMEDIATION_REVIEW_PASS` | APT-122、APT-123、APT-124 原已审核交付 | `internal/token` 与 `internal/billing` 的权威 precheck/financial projection；Account 拥有 approval snapshot 持久化/响应、submit 前重验、授权事件 payload 和 begin-execution lease 冻结投影。同步 Token/Billing/Account OpenAPI 与事件合同。拥有 13/8 完整 financial 变化证据、提交前 CAS 重验，以及协议/公式、目录同步和模型命令所需跨域预检 | 完整定价集合任一维度等量替换也改变指纹；不得使用 `max(price.version)`；`critical_resource_versions + financial_snapshot` 从 approval create 持久化与响应，经 submit 前重验、授权事件，逐字段原样进入 begin-execution lease，执行领取前的快照变化使申请失效；闭合 OpenAPI、真实 PostgreSQL、Account 持久化/HTTP、授权事件和 lease 合同全部通过。本批只交付快照贯穿与重验，不启动 APT-138 consumer 或领域执行 | 1；身份 B 第三轮审核 `PASS`，本批独立提交 |
| `APT-130-R1` | `DONE / REMEDIATION_REVIEW_PASS` | APT-123、APT-130、APT-124-R1 | 仅 `internal/token` 的 Provisioning repair / usage reconciliation target 解析、领域版本/CAS、precheck 与 governed result adapter；同步 Token OpenAPI/合同，不改 Portal/UI | 两个 command type 使用不同闭合 schema；额外字段、错 target/type/version、`replay_model_request` 缺失或非 `false` 均在持久化前拒绝；precheck 与 command 共用解析器；结果只按 actor/command/digest/reference 恢复，核对绝不重发模型请求 | 2；身份 B 第二轮审核 `PASS`，本批独立提交 |
| `APT-131-R1` | `DONE / REMEDIATION_REVIEW_PASS` | APT-124-R1、APT-130-R1 | 仅 `internal/portal`、Portal BFF/OpenAPI 代理、`pinvou3-app` 的通用对话框/命令客户端、三语/CSS 与 smoke；拥有审批分类证明、可信终态/未知恢复和 gate/撤权 abort，不改 Token/Billing 领域计算 | Account 权威 create/update/detail 响应均可生成 session/actor/approval/type/TTL 绑定证明；gate off 动作请求对 Token 为零 Account 调用、可信非 Token 可代理、缺失/伪造/过期/跨主体失败关闭；终态矩阵、零盲写、协议/公式既有 UI 不回退；smoke 端口由 OS 安全分配并连续 5 次稳定 | 3；身份 B 第二轮审核 `PASS`，本批独立提交 |

> APT-131-R1 待审记录（2026-09-04）：Portal 总动作 gate 在源码、Compose 与 Helm 中均默认关闭；Account 权威 create/update/detail 的闭合 2xx 投影在服务端生成短期分类证明，Token 后续动作在 gate off 时零 Account 调用失败关闭，非 Token 的 create→update/submit/withdraw/decide 保持可用。同步、发布、生命周期、回滚、协议/公式、Token 状态/路由、Provisioning repair 与 usage reconcile 均使用结构化交互并消费 APT-124-R1/APT-130-R1 权威 precheck、版本/CAS、命令或审批结果；repair/reconcile 命令携带精确版本/generation/high-watermark 且显式禁止模型重放。`SUCCEEDED/REJECTED/FAILED` 保留可信终态，只有 `PENDING/UNKNOWN` 或冻结引用 GET 失败保持未知并禁止盲写；关 gate 或撤权卸载对话框并中止在途请求。Portal/Token Go、Node 合同、OpenAPI、ESLint、Portal build、i18n、architecture/task/authorization/diff 门禁及真实 Chromium smoke 已执行；模型动作与 Token 资源 smoke 均以 OS 分配安全端口连续五次通过。全量合同仍只有 browser-login、H3C provider 和 foundation v2 allowlist 三项既有基线失败，与本批 diff 无关。APT-134 排程与 APT-138 执行/激活未启动。

> APT-131-R1 首轮整改记录（2026-09-04）：生命周期与目录回滚 UI 只接受 APT-124-R1 的闭合 13/8 字段 `critical_resource_versions` 和闭合 6 字段 `financial_snapshot`，审批创建、提交响应及冻结引用恢复均逐字段原样核验，旧 `approval_snapshot` / `price_coverage` 形状失败关闭。同步、发布、生命周期、回滚四类在途动作按 `action.type` 与各自原子权限绑定；单项撤权立即卸载对话框并中止预检，权限恢复不会复活动作或产生延迟写。模型动作与 Token 资源 smoke 共用 Chromium 完整 unsafe-port 集合的安全端口绑定器，OS 分配命中 3659 等受限端口时先关闭再重试；两套真实 smoke 均须连续五次通过。该轮提交身份 B 复审后由下述第二轮记录收口。

> APT-131-R1 代码审核记录（2026-09-04）：身份 B 第二轮 `PASS`。确认生命周期/回滚严格消费并原样冻结 APT-124-R1 的 13/8 字段关键版本与 6 字段财务快照，审批创建、提交响应和冻结引用恢复不接受旧形状或漂移；发布、同步、生命周期、回滚按动作类型精确撤权并中止，恢复权限不复活、不产生延迟写；共用 Chromium 安全端口绑定器覆盖完整 unsafe 集合和 3659 重试。定向 Node 47/47、Portal/Token Go、OpenAPI、ESLint、Portal build、三语、架构、任务、授权与 diff 门禁通过，模型动作及 Token 资源真实 smoke 各连续 5/5 通过，运行 smoke 通过。全量合同 333/336，三项 browser-login、H3C provider、foundation v2 allowlist 为既有基线失败且与本批 diff 无关。APT-132+、APT-134 排程和 APT-138 激活/执行未实现。

三批必须逐批执行“身份 A 实现 → 身份 B 独立审核 → A 修复 → B 复审直至 `PASS` → 独立提交”。文档审核未通过前不得继续代码整改；任一前置批次未通过时，后续批次保持失败关闭且不得用 Portal 假数据、旧 direct POST 或宽松 OpenAPI 维持按钮可用。APT-124 与 APT-130 原 `DONE` 提交和审核记录保留，整改提交只追加所有权明确的增量。

> APT-124-R1 实现记录（2026-09-04）：`DONE`；身份 B 第三轮独立代码审核 `PASS`。Billing 已用同一只读 Repeatable Read 快照返回全局财务事实修订号与固定全序 SHA-256，价格、公式绑定、单位、价格区块及 pricing space 的任一相关事实变化（包括非最大维度变化和等量替换）均使快照失效；公式单位校验同时返回数据库权威观测时间与完整单位事实指纹。Token 已为模型生命周期提供精确 13 字段、回滚提供精确 8 字段关键快照，并独立冻结 6 字段 `financial_snapshot`；协议草稿/发布、公式草稿/发布和目录同步使用各自闭合解析与权威跨域预检。模型发布还在同一 Token Repeatable Read 快照内按草稿引用的精确 `protocol_version_id` 冻结 resolved 路由、Provisioning 与 READY 覆盖计数及固定全序集合指纹；缺路由、缺 Provisioning 或 READY 覆盖不足均阻断，提交前状态漂移或等量路由事实变化在创建 command 前返回 409。Account 已将两类快照原子持久化并响应，submit 在串行化事务内重新读取权威源逐字段校验，通过后原样写入授权事件并冻结至 begin-execution lease；漂移失败关闭。仅测试接缝可在包内打开 Token approval create 以真实穿过 Account HTTP handler、强授权、actor 级幂等锁、真实 Token HTTP 权威预检及请求/snapshot/CREATED event 同一事务，生产构造器仍为 nil，`PRE_ACTIVATION` 不变；并发同 actor 同 key 只产生一条请求和一次权威读取，跨 actor 隔离。APT-138 activation record、consumer 和领域执行未启动。定向 Go、Ajv、OpenAPI、迁移清单、架构和 diff 门禁通过；本地未配置 Token/Billing/Account PostgreSQL URL，真实 PostgreSQL 命名用例按约定 `SKIP`，PR 隔离库门禁已逐项要求 `PASS` 且拒绝 `SKIP`。本批独立提交，尚未推送或部署。

> APT-130-R1 实现记录（2026-09-04）：`DONE / REMEDIATION_REVIEW_PASS`；身份 B 第二轮独立代码审核 `PASS`。Provisioning repair 与 usage reconciliation 已共用同一闭合命令解析器，分别只接受与 `target_resource_id` 完全一致的 `provisioning_id` 或 `usage_request_id`；额外字段、错资源类型/目标、缺失或非布尔/非 `false` 的 `replay_model_request` 均在通用命令持久化前拒绝。HTTP 入口只生成一次 canonical envelope 并把同一 command type、去除外围空白后的 reason、请求/幂等摘要贯穿 existing-first、权威 precheck、持久化、并发恢复和领域 adapter；大小写或外围空白输入不会造成通用结果与领域审计分叉。两类命令在同一 Token 数据库事务中依次执行 existing-first、资源 `FOR UPDATE`、权威重算、最终 CAS、通用结果、领域排队与双层审计；Provisioning 冻结 `updated_at + desired_generation`，Usage 冻结 `updated_at + evidence_high_watermark`，CAS 漂移会整体回滚并返回 409，不留下 `token_command_results`、领域 command、领域写或审计。结果按 actor、command type、幂等摘要和 command reference 隔离恢复，返回闭合脱敏结果及审计 request_id。Provisioning 仅重新排队既有 generation，Usage 仅调度既有证据核对且不会创建 usage attempt、调用 Provider 或重放模型请求；APT-138 activation/consumer 仍未启动，生产保持 `PRE_ACTIVATION`。Go 全包、Node/Ajv 合同、OpenAPI、迁移清单、架构、任务覆盖和 diff 门禁通过；本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`，真实 PostgreSQL Handler/CAS/非法输入/确定性锁竞态漂移/同 actor 并发幂等/跨 actor 隔离/规范化输入用例按约定 `SKIP`，PR 隔离库已增加命名 no-SKIP 门禁。本批独立提交，尚未推送或部署。

### APT-132 模型独立详情页

> 实现状态（2026-09-04）：`DONE`；身份 B 第三轮独立代码审核 `PASS`。已按唯一六页签结构实现稳定 `model_id` 独立主内容页；默认关闭的详情 gate、非法/无权/无 gate URL 归一、目录状态保留返回、刷新/分享/浏览器历史、逐原子权限页签裁剪和撤权请求中止已接入生产路由。Token 新增同一只读 Repeatable Read 快照的当前目录、不可变能力/协议及安全公式详情投影，不返回 Provider/Adapter/upstream、原始协议 schema 或公式 expression；另新增只返回旧目录/能力安全摘要的不可变历史版本快照分页，当前、草稿与历史明确分离。CHAT `parameters` 数组和受支持的 JSON Schema `properties` 均按真实字段数统计，畸形或歧义 schema 失败关闭。Provisioning 和资源审计复用既有服务端分页，Billing 价格页直读 APT-126 权威投影并明确两域非原子；生命周期页固定展示 12 项三语依赖、完整价格覆盖及 Token/Billing 独立权威观测时间。详情不提供任何写动作。定向 Ajv/导航 19 项、Token/Portal Go、OpenAPI、授权、i18n、ESLint、Portal build、architecture、task 与 Chromium 生产制品 smoke 已通过；smoke 覆盖 320px/200%、六页签、历史 cursor 深链/刷新、完整依赖与价格覆盖、局部审计失败/重试、价格/运行按需请求、权限撤销中止、无权与 gate-off 零详情请求和 ID 零 DOM。本地未配置 `PINVOU_TEST_TOKEN_DATABASE_URL`，命名 PostgreSQL 用例按约定 `SKIP`，PR 隔离库已登记 no-SKIP 门禁。APT-134 排程、APT-135 价格编辑、APT-136 趋势、APT-138 激活及 Token Account 详情均未实现。

> 代码审核整改记录（2026-09-04）：身份 B 首轮 `NEEDS_CHANGES` 指出资源审计不能替代版本历史、CHAT 基线 `request_schema.parameters` 被错误按 `properties` 计数，以及生命周期依赖未完整展示价格覆盖/双域观测时间且部分内部键直接进入界面。身份 A 已逐项整改：历史版本改为模型绑定的签名快照 cursor 分页和闭合脱敏 schema；schema 摘要支持两种明确形状且畸形失败关闭，真实 PostgreSQL 用例断言 CHAT 为 4；生命周期按固定键序和三语标签展示 12 项依赖、价格覆盖版本及双域 `observed_at`，生产 Chromium 断言内部 snake_case 键不进入 DOM。当前重新提交身份 B 复审，审核通过前不标 `DONE`。

> 第二轮整改记录（2026-09-04）：身份 B 确认首轮三项整改通过，仅退回历史分页 OpenAPI 错误矩阵。身份 A 已按共享分页 Handler 的真实结果补齐 422 `PAGINATION_FILTER_REQUIRED` 与 504 `QUERY_TIMEOUT`，并把 429 收紧为同时覆盖 `RATE_LIMITED` 和 `PAGINATION_SNAPSHOT_LIMITED` 的闭合 `TokenAdminQueryLimited`；合同测试逐项断言 200/400/403/410/422/429/503/504 和两种 429 稳定码，当前再次提交身份 B 复审。

> 代码审核记录（2026-09-04）：身份 B 第三轮 `PASS`。确认首轮历史版本、安全 schema 摘要和生命周期完整投影三项整改均闭合；第二轮历史分页响应矩阵与真实共享 Handler 一致，429 闭合覆盖速率限制和快照容量两种稳定码，未知码及额外字段负例通过。APT-132 未越界实现 APT-133 Token Account 详情、APT-134 生命周期计划、APT-135 价格编辑、APT-136 趋势或 APT-138 激活/执行。

- **目标/范围**：主目标 `UI-53`。修改 Token 现有目录事实的安全只读详情投影、Admin OpenAPI、Portal 模型详情路由、`admin.html`/JS/CSS/i18n、BFF 精确代理、生产浏览器 smoke 注册与详情合同测试；不新增迁移、领域事实或写合同。唯一 IA 冻结为“概览 / 能力、协议与公式 / 路由与运行 / 价格与计量 / 生命周期与依赖 / 审计”六页签，其他文档、原型和实现不得再拆分、合并或改名。
- **URL/权限/回滚**：稳定 `model_id` 深链，每个嵌套数据源使用独立 filter/cursor/error；价格页签要求 Billing 读权，审计页签要求资源读权与 `platform.audit.read` 交集，无权页签不请求、不进 DOM/日志。二期详情 gate 可独立关闭并回到 APT-127 列表，返回时保留列表 URL；无数据库变更。
- **验收**：合法 UUID 无权、非法 UUID、互斥详情、无权页签、刷新/分享/popstate、各嵌套源局部失败、三语和可访问性通过；稳定 ID 可复制，敏感值不可回显。
- **不包含**：只交付六页签 IA、路由、读取投影和受控空态；不实现“生命周期与依赖”中的计划写能力（APT-134）、“价格与计量”写组件（APT-135）、凭据或 Token 详情。

### APT-133 Token Account 详情与隔离凭据生命周期

> 实现状态（2026-09-05）：`DONE`。已完成五页签独立详情、安全分页投影、一次性新增/轮换、受治理现场验证、撤销审批与生产 revoke adapter；撤销执行前逐字段重验 Token 权威 11 字段快照与空 financial snapshot，Account 在创建、提交重验、授权事件及执行租约中保持同一冻结证据。旧 v1 内联详情继续仅作兼容，新页面只消费 v2 安全摘要及独立分页资源。身份 B 首轮提出的 OIDC 同源信任、10 秒 heartbeat 竞态、验证租约恢复、历史最大版本递增、三阶段轮换脱敏审计和有界撤销均已整改。经两轮身份 B 文档审核冻结并通过 Portal 同源固定 `/credential-handoff`：构建期固定 SHA-256 CSP、运行期 controller/scope 双检查、闭合 v1 MessageChannel、严格非 null Portal Origin、固定 303 `/session` 及 credential origin 同源秘密提交均已落地；Portal、URL、DOM、storage、日志与审计不接触 Secret。生产接线 Chromium 已连续 5 次通过完整 handoff 流程及弹窗受阻、乱序、超时、撤权、关 gate、跨浏览器和刷新不重放负例；另有具名真实 PostgreSQL 集成门禁穿过 Credential Origin HTTP handler、持久仓库、PKCE、签名 JWT/JWKS 与受控 IdP，并在 CI 中接真实 Chromium 连续执行 5 个账户。本地未配置 PostgreSQL 时两项具名用例明确 `SKIP`，CI 强制 no-SKIP。身份 B 第四轮独立代码审核已通过；PRE_ACTIVATION 未切换，APT-134+、APT-137 生产等价 TLS/代理探针及 APT-138 激活不在本任务范围，尚未推送或部署。

> 第二轮整改记录（2026-09-05）：launch 与 session 已持久化 actor issuer/subject、Account authorization version 和治理 gate epoch；创建、消费、首 heartbeat、OIDC 回调前后及最终 credential 事务均经同一事务型 authority guard 重验，验证期间漂移会在最终提交前失败关闭且不产生 credential。10 秒仅约束首 heartbeat，完成首 heartbeat 后 OIDC 可在 5 分钟 session TTL 内继续。handoff 在 MessagePort 同步拒绝时认定“确定未提交”，一旦发送 SUBMIT 后丢 ACK/网络结果则进入 UNKNOWN，只轮询既有 result reference；撤销响应不确定也先撤销一次后查询同一结果，绝不重建或重 POST。`TOKEN_CREDENTIAL_REVOKE` 的 `financial_snapshot` 只接受精确非 null 空对象 `{}`，缺失、null、数组或额外字段均在任何权威读取和持久化前返回稳定 400。生产继续保持 PRE_ACTIVATION；本任务只提供 APT-138 可注入的事务 authority guard 接缝，不在本批启动 gate consumer 或十九项执行。

> 第三轮整改记录（2026-09-05）：Token 内部 credential revoke executor 的原始 HTTP JSON 边界已独立执行 `financial_snapshot` 存在、非 null、object 且零字段检查，四类非法输入在 repository/precheck 前稳定 400；真实 PostgreSQL 用例同时断言 command、audit、outbox 总量不变。接力窗口在 ACK 丢失后自动固定一次 `CREDENTIAL_LAUNCH_CANCELLED` 撤销，撤销响应丢失仍查询同一 result reference；同步未提交与立即取消使用后端当前状态合法的稳定原因，失败 Promise 会释放但既有 revoke attempt 永不重复，后续只 GET 并最终收敛。真实 Chromium CI 已废止跨 origin 直提交流程，改由生产 Portal `GET /credential-handoff` 构建产物与 MessageChannel 在内存传递 launch ID，浏览器 URL/DOM/Performance 均无该 ID，再穿过真实 Credential Origin、PostgreSQL、签名 JWT/JWKS 与受控 IdP，连续 5 个独立账户验收。

> 代码审核记录（2026-09-05）：身份 B 第四轮 `PASS`。确认 Token executor 在 repository/precheck 前严格拒绝缺失、null、数组或非空 `financial_snapshot` 且零 command/audit/outbox 写入；ACK/撤销响应丢失固定只撤销一次并仅查询原 `result_ref`，不重建或重 POST；生产 Portal `/credential-handoff`、冻结脚本、MessageChannel、真实 Credential Origin/PostgreSQL/PKCE/JWT/JWKS/受控 IdP 与连续五账户 Chromium 路径均闭合，launch ID 和 Secret 未进入 URL、DOM、Performance、日志或审计。APT-138 未来注入权威 guard 时必须以严格绝对超时约束外部 Account 查询，避免长期占用 Token 行锁；本任务不激活 consumer、十九项命令或生产 gate。

- **目标/范围**：主目标 `UI-54`、`ADM-58`。修改 `migrations/token/000026_*`、`internal/token` 的 credential launch/session/result 与脱敏验证/撤销适配、独立 `cmd/token-credential-portal`（及其最小服务包）、Portal 同源固定 `/credential-handoff` 最小静态路由、专用脚本构建与 SHA-256 CSP 生成/校验、Portal Token 详情五页签“概览、凭据、路由、Provisioning、审计”和安全自动化；本任务唯一拥有 handoff v1 消息 schema、凭据 launch/session schema、运行时 SW controller 检查与代码级失败撤销。TLS、DNS、反向代理、环境白名单、Service Worker 注册 scope 的生产等价复核与生产探针证据统一归 APT-137，本任务为 `CODE`。
- **安全合同**：完整实现专项设计 6.1～6.3 的 11 点合同，不接受直接跨 origin POST 等缩减替代。① Portal 同源 `GET /credential-handoff` 固定无 ID/query/fragment，响应不建 session/Cookie，使用 no-store、Referrer-Policy origin 与最小 CSP；专用构建冻结内联脚本字节并生成固定 SHA-256，CSP 只允许以该精确 hash 授权的内联脚本，并禁止 `script-src 'self'`、`'unsafe-inline'`、`'unsafe-eval'`。handoff 在 READY 前与 BIND 后均要求 `navigator.serviceWorker.controller===null`，代码/部署保证注册 scope 不覆盖。② 用户手势先开 handoff；弹窗、READY/BIND 失败不创建 launch，BOUND 后才创建，之后失败撤销同一 `result_ref`。③ 双向精确 origin+`event.source`、至少 256-bit CSPRNG nonce、闭合 v1 schema、专属 `MessageChannel`。④ READY→BOUND→SUBMIT→ACCEPTED 单次有序 ACK，重复/乱序/跨窗口/3 秒阶段超时失败关闭。⑤ launch ID 仅存在运行时内存、MessagePort 与瞬时不可见 DOM form，不进可见 DOM、URL、Cookie、storage/IndexedDB/Cache、window.name、Clipboard、Referer/History/Performance、遥测或日志。⑥ handoff 先建立未挂载的瞬时 form，再发 ACCEPTED；随后关闭 port、清监听和 JavaScript 变量、置 `opener=null`，最后才挂载 form 并以 `_self` POST 单个 `launch_id` 到固定 credential `/launch`；禁止直接 `_blank` 跨 origin POST、fetch/XHR、GET/query/fragment/path、iframe/同页降级。⑦ `/launch` 精确要求当前 Portal 的非 null Origin，拒绝零消费。⑧ 首个 303 固定 `/session` 且从该响应起 COOP same-origin/no-referrer/no-store。⑨ 未知只查询同一 `result_ref`，不得新 nonce/key 或重 POST。⑩ 关窗/取消/撤权/关 gate/10 秒 heartbeat 超时分阶段有界撤销。⑪ 现场验证为 Portal→Token direct 脱敏命令，撤销为 Portal→Account 的 APT-123 审批命令且均不进入隔离页。
- **隔离会话/回滚**：专用 OIDC audience、Authorization Code+PKCE、`prompt=login&max_age=900`，强校验 state、nonce、PKCE、auth_time、subject、CSRF Origin，并绑定环境、账户、动作、槽位、版本、浏览器和幂等摘要；launch 5 分钟有效、单次消费、最多三次尝试。独立 credential gate 或固定 origin 配置缺失时失败关闭；回滚先停新 launch、原子撤销全部未完成 session，再关闭隔离服务；down 仅在无活动/审计引用后删除本迁移对象。
- **验收**：本任务独立完成 `TOKEN_CREDENTIAL_ENROLL`、`TOKEN_CREDENTIAL_ROTATE`、`TOKEN_CREDENTIAL_VALIDATE`、`TOKEN_CREDENTIAL_REVOKE` 四项领域命令验收；覆盖五页签独立 URL/cursor、专用 audience、prompt/max_age、state/nonce/auth_time/subject、PKCE、CSRF Origin、浏览器绑定、5min/三次、原子消费/撤销、10s heartbeat、稳定错误、验证脱敏、轮换状态机与撤销审批。真实双 origin Chromium 必须覆盖固定无 ID handoff URL与最小静态头/CSP、CSP 只允许精确 SHA-256 hash 授权的内联脚本并禁止 `script-src 'self'`/`'unsafe-inline'`/`'unsafe-eval'`、两阶段 controller 为 null、已注册 scope 不覆盖、用户手势先开窗、完整 v1 MessageChannel 握手、nonce/origin/source、一次 `_self` body POST、精确非 null Origin、固定 303、opener 生命周期及网络/全部 DOM API/storage/log 零泄漏；还要覆盖 hash 漂移、root scope/controller 覆盖、弹窗受阻、重复/乱序/阶段超时、缺失/null/伪造 Origin、取消/关窗/撤权/关 gate、未知查询不重 POST、刷新不重放、跨浏览器/主体/回放失败关闭。十九项总一致性留给 APT-138；N/N-1、TLS/代理及生产等价双 origin/heartbeat/SW scope 探针由 APT-137 验收。
- **不包含**：不重建 KMS/Secret 底座、不允许 Portal 接触 Secret、不改三项审批策略值、不实现模型详情或生命周期。

### APT-134 模型生命周期计划与依赖分析

> 实现状态（2026-09-05）：`DONE`。模型发布、弃用与下架均已形成不可变计划、权威依赖/价格/路由/Provisioning/在途请求证据、版本与 CAS、取消/过期/冲突及审计闭环；`REMOVED` 的新会话与存量会话截止统一冻结为精确生效时间。`DRAFT → PUBLISHED` 复用 `TOKEN_MODEL_PUBLISH / AUDIT_ONLY`，在同一 Token 事务持久化 governed result 与 `SCHEDULED` 计划，到期才按即时发布等价语义锁定、重验、生成目录版本并删除草稿；弃用/下架继续走 Account 独立审批、授权事件、执行租约和 Token executor。生产 worker 仍未在 main 注册，APT-138 激活边界不变；当前未推送、未部署。

> APT-134 代码审核记录（2026-09-05）：身份 B 经五轮独立复审后 `PASS`。前四轮依次闭合替代模型完整可服务覆盖与固定全序漂移证据、精确截止/down 历史保护、真实 Account→Token 审批链、服务端候选分页、计划发布真实草稿语义、合法迁移对、幂等键轮换、按目标权限取消、权威 request fingerprint、legacy `IMMEDIATE` 兼容、15 字段冻结证据及即时/计划结果互斥；第五轮确认 UTC 整秒 JS/Go 固定指纹向量、错误权限族对现有/随机计划不可区分且零写，以及草稿删除/协议失效等确定性 worker 漂移原子进入 `INVALIDATED` 且不再领取。Token/Account Go、闭合 Ajv/合同、OpenAPI、ESLint、Portal build、i18n、架构、任务、授权、迁移清单与 diff 门禁通过，模型操作和模型详情生产 Chromium smoke 均连续 5/5 通过。本地未配置 PostgreSQL URL 时具名用例明确 `SKIP`，PR 隔离库已登记 no-SKIP。APT-135/136 未实现，APT-138 consumer/worker 激活未启动。

- **目标/范围**：主目标 `UI-55`。修改 `migrations/token/000027_*`、`internal/token/{catalog.go,catalog_http.go}` 及 lifecycle plan/worker、Token OpenAPI、Portal 模型详情既有“生命周期与依赖”页签和测试；本任务唯一拥有计划版本、计划状态与执行租约 schema，只向 APT-132 已冻结页签增量交付内容，不新建/改名/拆分页签。
- **治理/兼容/回滚**：展示 APT-124 价格/路由/Provisioning/在途请求/会话/替代模型依赖，支持计划发布、弃用、下架以及新会话/存量会话截止时间。计划发布冻结 `expected_catalog_version`、`expected_draft_version`、完整草稿内容及即时发布的协议/能力/别名/价格/路由证据，通过既有 `TOKEN_MODEL_PUBLISH / AUDIT_ONLY` governed command 后进入 `SCHEDULED`；弃用/下架才提交 APT-123 lifecycle v2 审批，把 `lifecycle_plan_version` 纳入 13+6 快照并由 worker 消费获批事件。gate 关闭先停止新计划与 worker 领租，保留既有计划只读和审计，禁止用 down 反向恢复已执行生命周期事实。
- **验收**：本任务独立完成计划型 `TOKEN_MODEL_PUBLISH` 与 `TOKEN_MODEL_LIFECYCLE_CHANGE` 两条领域命令验收；发布覆盖真实 Save draft、完整冻结发布证据、governed schedule、草稿漂移零写及 worker 复用即时发布效果，且不产生 Account 审批。弃用/下架覆盖时区/DST、开始与截止边界、依赖变化导致审批失效、禁止自审、重复事件幂等、租约接管、未知结果恢复、取消未执行计划、已执行不可逆和无替代模型失败关闭。十九项总一致性仅由 APT-138 验收。
- **不包含**：不实现目录差异 UI、价格编辑、路由健康算法、会话迁移执行或修改 lifecycle v2 策略字段。

### APT-135 模型上下文价格与计量入口

> 实现状态（2026-09-05）：`DONE`；身份 B 第二轮独立代码审核 `PASS`。模型详情已按精确稳定 `model_id`、协议与公式维度复用财务侧同一个 `PricingLifecycleWorkspace`，只调用 Billing 现有价格空间、计量单位、CAS、幂等、生命周期与 `AUDIT_ONLY` 合同；只读投影显示当前/未来覆盖、解析价格、计量单位、继承来源、价格版本与 ETag，缺价和失败不会渲染伪 0。财务价格列表可下穿模型价格上下文并返回原价格页；两入口不复制价格定义，也不宣称 Token/Billing 跨域原子。新增模型详情编辑子 gate 默认关闭，关 gate 或撤销 `platform.pricing.manage` 会同步清理 URL、卸载编辑器，并通过统一 AbortController、代次与 mounted 守卫中止列表、预览、转换候选和未知结果恢复读取；财务治理入口不受影响。父模型 IA、详情 gate 或模型读取能力关闭时，财务价格列表只显示非交互稳定 ID，不产生隐藏导航或模型请求。此前“价格生命周期加载失败”已核对为 Billing 000051 对价格空间基线的既有修复，本任务继续复用其幂等修复与 no-SKIP PostgreSQL 门禁，没有另建旁路或复制状态。闭合 OpenAPI/Ajv、共享 PostgreSQL 权威事实一致性、Portal session、导航、i18n、响应式和生产 Chromium 两入口 smoke 已通过；尚未推送或部署。

> 代码审核记录（2026-09-05）：身份 B 第二轮 `PASS`。首轮提出的瞬态 Billing 读取中止和父 IA 下穿可见性两项均已闭合；预览、ACTIVE 转换候选与 UNKNOWN 恢复在关 gate、撤权或卸载后会中止，迟到响应不会复活 UI 或触发写入。财务入口仅在模型 IA、详情 gate、模型可见性和 `platform.models.read` 同时满足时提供下穿。身份 B 复跑共享合同 3/3、价格生命周期 8/8、导航 17/17、生产 Chromium smoke 与 diff 门禁均通过。本机未配置 `PINVOU_TEST_BILLING_DATABASE_URL` 时具名 PostgreSQL 用例明确 `SKIP`，PR 隔离数据库门禁保持 no-SKIP；APT-136 趋势与 APT-138 激活未实现。

- **目标/范围**：主目标 `UI-56`。仅修改 Portal 可复用价格/计量组件、模型详情“价格与计量”页签、财务“定价与商品”复用入口、BFF 与合同测试；读写均消费 Billing 现有 API 与 APT-126 投影。
- **领域/权限/回滚**：Billing 仍是金额、积分费率、计量单位、继承/覆盖和价格生命周期的唯一事实源；模型页只提供当前模型上下文，不复制 schema/缓存或创建第二套价格。严格复用现有 `platform.pricing.read` / `platform.pricing.manage`、价格生命周期、CAS、不可变审计与 `AUDIT_ONLY` 命令，不虚构价格审批权限或审批流程；关闭二期价格入口 gate 后模型详情回到只读摘要，财务入口不受影响。
- **验收**：当前/未来价格、继承来源、覆盖缺口、计量单位、无价/不可用/403、跨入口同一 version/ETag、CAS 冲突、生命周期与 `AUDIT_ONLY` 审计关联、三语和错误隔离通过；不得用 0 伪装未知价格。
- **不包含**：不改 Billing schema、定价算法/权限/治理模式、协议公式事实或财务信息架构，不新增任何价格审批跳转。

### APT-136 目录差异与运行健康趋势

- **目标/范围**：主目标 `UI-57`。修改 Token 模型/运行聚合、目录同步 observation 绑定、具名索引迁移 `000028_operations_overview_indexes`、OpenAPI，Portal 模型/运行概览、目录同步/回滚差异预览、趋势卡片/图表、BFF 映射、i18n/CSS 和可视化/性能合同测试；复用 APT-126 聚合、APT-124 预检与 APT-131 结构化动作，不新建第二份指标事实。
- **状态/回滚**：`REOPENED_REMEDIATION / IN_REVIEW`。展示已发布/草稿模型、目录版本差异、异常路由、失败 Provisioning、待核对用量及明确时间窗的趋势；各卡片独立 loading/error/refresh，不扫描分页列表计算指标。同步/回滚经 APT-131 结构化动作，回滚命令走 APT-123 rollback v2。关闭二期 overview gate 即隐藏增强卡片。up 仅增加 `usage_requests(started_at,id)` 和谓词精确为 `TOKEN / USAGE_RECONCILIATION_REVIEW / USAGE_REQUEST` 的 `manual_review_cases(created_at,id)` 部分索引（包含核对所需稳定投影列）；down 只删索引、不删事实。APT-138 activation 迁移因此顺延为 `000029`。
- **窗口/趋势验收**：客户端只传规范枚举 `window=24h|7d|30d`；Token 用同一只读事务的 `transaction_timestamp()` 派生并响应权威 UTC `[from,to)`，Portal 展示与下穿只复用该响应，不使用浏览器时钟。`24h` 必须为 24 个 3,600 秒桶，`7d/30d` 必须为 7/30 个 86,400 秒桶；首尾、相邻边界、点数、非负计数和总量均闭合，末桶精确结束于 `to`。全零显示无数据，不画最小高度伪柱。覆盖浏览器严重 clock skew、非法窗口、局部失败、旧响应隔离、单位/时区与 320px/200%。
- **同步验收**：差异覆盖新增/修改/删除、版本选择和空差异。同步写入复用预览的同一 observation，或在写前只重取一次并对 Provider/Adapter/实际消费模型字段的固定全序 fingerprint 做精确匹配后直接消费该集合；两次源结果变化必须在任何目录/命令/审计/Outbox 持久化前失败关闭。提交响应未知仅查询原引用，禁止再次发现或重发。
- **性能验收**：CI 使用 PostgreSQL 17 Alpine、2 vCPU、4 GiB、`shared_buffers=512MB`、`work_mem=16MB` 和确定性种子：1,000,000 条用量（30 日内/第31～180日为 700,000/300,000，85%终态、10%待核对未停止、5%待核对已停止）及 100,000 条案件（60,000 精确 Token 用量核对、40,000 干扰，精确案件70%在30日内）。`VACUUM (ANALYZE)` 后，保留卷重启容器的首次冷缓存与紧接三次暖缓存均执行生产 30 日单 SQL 的 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，每次 `Execution Time <= 3s`。计划必须为窗口用量恰一次新增 started_at 索引扫描、点时待核对恰一次既有 `usage_requests_pending_idx` 扫描、窗口精确案件恰一次新增部分索引扫描；案件权威存在性仅允许主键等值 membership probe。禁止重复扫描同一逻辑集合、`Seq Scan`、逐桶相关子查询或客户端分页聚合。CI 命名用例必须 no-SKIP，并验证固定数据分布、buffer、索引名、迁移 up/down 和索引谓词/稳定 tie。
- **不包含**：不新建指标事实/仓库，不改变健康探测状态机、核对处置或审批策略，不激活 APT-138；同步算法只增加 observation 的冻结/写前一致性边界，不改变目录合并业务语义。

> APT-136 首轮整改记录（2026-09-05）：身份 B 代码复审为 `NEEDS_CHANGES`。当前实现已提供目录差异、运行概览与趋势壳，但同步预览/写入 observation 未绑定、30 日查询缺少专用索引及生产量级执行计划证据、窗口仍受浏览器时钟影响、趋势点数/桶宽和全零图形未闭合。本文先冻结修复所有权与验收，代码仍处 `IN_REVIEW`，不得提前标 `DONE` 或写入通过证据。

### APT-138 十九项命令一致性总验收与原子激活

- **目标/范围**：主目标 `ADM-64`。在 APT-122～APT-136 的领域/API 接入全部通过独立审核后，修改 `migrations/token/000029_*`、`internal/token` 治理激活仓储/路由门禁、Account 审批入口激活校验、`internal/platform/approvalpolicy` 总合同测试及发布前集成测试。本任务唯一拥有持久治理激活记录（状态、epoch、策略集合摘要、十九项处理器摘要、操作者、CAS、时间与审计 request_id），不改任何领域事实 schema；`000028` 已由 APT-136 唯一用于运行概览索引。
- **状态机/原子边界**：初始只能是 `PRE_ACTIVATION`：旧 v1 direct 保持当前兼容行为，新治理/新审批写入口关闭；全部一致性门禁成功后，只允许一次单事务 CAS 将 activation record 从 `PRE_ACTIVATION → ACTIVE`，同一持久 epoch 的同一次原子路由切换关闭三项旧 direct、接受新审批执行并开放其余十六项新治理 envelope，不允许逐命令、逐实例或重复激活，不存在双写窗口。`ACTIVE → SAFE_ROLLBACK` 只停止十九项新写、审批领取和执行消费，绝不重开旧 direct；修复并重跑全部门禁后只允许 `SAFE_ROLLBACK → ACTIVE`。禁止回到 `PRE_ACTIVATION`、跳过状态、客户端指定状态或仅切一项命令。
- **N/N-1/故障恢复**：Account 创建审批前强一致校验 Token activation epoch；Token 处理 direct/执行/结果查询时校验同一持久 epoch。N-1 不认识 `ACTIVE/SAFE_ROLLBACK` 或策略集合摘要时失败关闭，新旧实例混跑不得自行降级 direct。切换进程崩溃、数据库提交不确定、事件重复/乱序、Account/Token 网络分区只按 activation CAS 与 command result 查询恢复，不重放写命令。
- **总验收**：十九项逐一核对策略/权限/近期认证/reason/CAS/持久幂等/影响预检/审计/未知恢复、API handler 和领域存储所有者；三项审批逐字段比对 DB↔registry、allowlist、最小执行身份和 direct 403，十六项 `AUDIT_ONLY` 验证不可绕过中间件。故障注入覆盖切换前、CAS 提交中、提交后响应丢失、混合 N/N-1、重复事件、进入/退出 `SAFE_ROLLBACK`，证明状态迁移合法、零双写、零 direct 复活和审计连续。APT-133 提供的 credential authority guard 接缝在本任务接入真实 Account 权威源时必须使用贯穿事务的严格绝对 deadline；超时或不可确认时失败关闭并释放 Token 行锁，不允许外部查询无限延长持锁时间。
- **回滚/不包含**：应用回滚只能 CAS 进入 `SAFE_ROLLBACK` 并保留 activation/command/audit 历史；`000029` down 在任何非 `PRE_ACTIVATION` 或有引用时失败关闭。不实现页面、领域命令、预检计算、凭据隔离、TLS/代理或生产部署；发现差异必须退回唯一所属 APT 修复，不能在本任务夹带修补。

### APT-137 分级发布、兼容与总回滚门禁

- **目标/范围**：主目标 `UI-58`。仅修改 Account/Token/Portal 分级发布配置、`deploy/compose`、`deploy/helm`、OpenAPI/Portal 契约门禁、运维 runbook 和发布验证脚本；复用 APT-127 已交付的一期 IA 基础开关，汇总 APT-127～APT-136，并唯一负责跨版本分级发布与生产激活/回滚编排，以及 credential origin 的 DNS/TLS、Portal/credential CSP、固定环境白名单、反向代理/WAF 日志抑制和生产等价健康探针证据，但不夹带功能修复。
- **发布/兼容/回滚**：冻结一期 IA/列表/协议公式/选择器/运行闭环/结构化操作与二期详情/凭据/生命周期/价格/概览的独立 gate 和依赖顺序；服务端、Portal、cursor envelope、审批事件和隔离凭据入口支持 N/N-1。`section=tokens` 至少保留 90 天且跨两个稳定版本规范化；UI-47 开启后 supersede UI-28/UI-31 canonical。回滚顺序为先停写/worker/launch，再关二期、一期和代理路由；活动 cursor 返回 410，进行中审批/计划/session 按各任务合同收敛。
- **验收**：逐 gate on/off、混合 N/N-1、滚动升级/回滚、旧链接/书签/popstate、九源 cursor 独立、CSP/origin、DNS/TLS/代理/WAF、10s heartbeat 探针、DB↔registry 逐字段一致、`executableApprovalCommands` 保留既有项且 Token 增量恰三项、容量及 `10→12→14`/`3→5→7`/`5→7→9` 绝对 deadline 链/告警、三语与安全扫描全部成为发布门禁，并记录生产等价环境证据。credential 生产探针必须在真实 Portal/credential 双 origin 与生产等价响应头/代理下覆盖 APT-133 全部 handoff 合同：固定无 ID URL、用户手势先开、CSP 只允许精确 SHA-256 hash 授权的内联脚本并禁止 `script-src 'self'`/`'unsafe-inline'`/`'unsafe-eval'`、`navigator.serviceWorker.controller===null`、所有注册 scope 不覆盖 handoff（既有 root scope 必须先收窄，否则发布失败）、v1 MessageChannel origin/source/nonce/ACK、一次 `_self` body POST、精确非 null Origin、固定 303、安全头与 opener 生命周期、无网络/DOM API/storage/log 泄漏，以及受阻/乱序/超时/撤权/关 gate/未知/刷新/跨浏览器等失败路径；不得用直接跨 origin POST、单 origin mock 或绕过浏览器的 HTTP 测试替代。
- **不包含**：不新增业务 API/schema/权限/页面，不改审批分类或数据事实；任何验收失败回到其唯一所属 APT 修复后重新审核。

## 4. 独立提交验收模板

每个任务提交前记录：

- `task_id` 与全部 `target_requirement_id`。
- 代码、迁移、契约、事件、UI 和文档改动范围。
- 正常、拒绝、幂等、并发、失败恢复和权限隔离测试。
- `implementation-status.md` 的状态、证据、已知差距和部署版本变化。
- 实际执行的验证命令与结果。
- 尚需生产等价环境验证的项目；没有环境证据时保持 `NOT_DEPLOYED`。

提交信息使用 `type(scope): 中文描述` 并包含有效 `Signed-off-by`。一个提交只关闭一个 `task_id`；任务依赖只能通过既有已提交接口消费，不能在当前提交中顺带实现其他待办。
