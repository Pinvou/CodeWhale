# 账号平台未完成需求实施任务

> 状态：当前执行队列
>
> 需求与状态基线：`e5437902aea2558b78b8076f3645834611ea2514`
>
> 代码审计基线：`e23005aaf6745d4b8c1ffa5ac73943e0f8f98ca5`（APT-001 以本文件所在提交增量闭环）
>
> 约束边界：`architecture.md`、`role-system-design.md`、Accepted ADR 与本目录部署文档

## 1. 执行规则

1. 本文为最初 275 个未完成目标及后续需求变更保留可追溯任务。当前 138 个任务中，110 个任务为 `DONE`，7 个任务保持 `BLOCKED_EXTERNAL`，21 个任务为 `PENDING`；规范矩阵中的每个未完成编号只能有一个主任务。
2. 任务按稳定服务所有权、事务边界、公开契约和可独立验证能力划分，不按文件数量或代码行数机械拆分。
3. 每个任务使用独立提交，提交必须同时包含该任务所需的代码、迁移、OpenAPI/事件契约、三语 UI、自动化测试、需求状态和相关架构/部署文档更新。
4. 开始任务前先检查依赖任务已经完成；实现中发现不能独立交付时，必须先拆分任务并更新本文，不能把无关能力混入当前提交。
5. `CODE` 任务必须在仓库内闭环；`CODE+ENV` 任务除仓库实现外还需要生产等价环境证据，缺少环境时不得把状态改为已实现或已部署。
6. 每个提交至少运行 `node scripts/account-platform.mjs verify`；涉及桌面、Web、部署、安全、恢复或性能时增加相应定向测试和制品/环境验收。
7. 任务完成仅表示其映射目标全部达到验收标准；部分完成时保持 `IN_PROGRESS`，不得提前关闭或拆成多个混合提交。

状态值：`PENDING`、`IN_PROGRESS`、`DONE`、`BLOCKED_EXTERNAL`。

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
| APT-123 | 十九项 Token 命令治理基础、策略注册与持久结果恢复 | ADM-65 | APT-008, APT-074, APT-076, APT-118, APT-120, APT-122, APT-124 | CODE | PENDING |
| APT-124 | 模型生命周期、目录回滚及 Token 变更的影响预检 | ADM-57 | APT-028, APT-030, APT-120 | CODE | DONE |
| APT-125 | 路由稳定模型/协议解析与服务端健康探测状态机 | ADM-62 | APT-027, APT-028, APT-121, APT-122, APT-123 | CODE | PENDING |
| APT-126 | 模型/运行概览与 Billing 模型价格跨域只读投影 | ADM-59 | APT-113, APT-121, APT-125 | CODE | PENDING |
| APT-127 | 模型与 Token 三 section、独立 URL 状态与逐源错误隔离 | UI-47, UI-48 | APT-094, APT-097, APT-121 | CODE | PENDING |
| APT-128 | 协议、安全公式、预览、发布及 Billing 同步界面 | UI-49 | APT-122, APT-123, APT-127 | CODE | PENDING |
| APT-129 | Token owner/路由模型选择器与健康管理界面 | UI-50 | APT-119, APT-123, APT-125, APT-127 | CODE | PENDING |
| APT-130 | Provisioning 详情、尝试历史与用量核对异常闭环 | UI-51 | APT-121, APT-123, APT-127 | CODE | PENDING |
| APT-131 | 模型与 Token 结构化高风险交互与未知结果恢复 | UI-52 | APT-123–APT-130 | CODE | PENDING |
| APT-132 | 模型独立详情、六页签与资源审计 | UI-53 | APT-121, APT-126–APT-128, APT-131 | CODE | PENDING |
| APT-133 | Token Account 独立详情与隔离凭据新增/轮换 | UI-54, ADM-58 | APT-121, APT-123, APT-124, APT-127, APT-129, APT-131 | CODE | PENDING |
| APT-134 | 模型生命周期依赖分析、计划生效与会话截止 | UI-55 | APT-123, APT-124, APT-131, APT-132, APT-133 | CODE | PENDING |
| APT-135 | 模型上下文价格与计量共享编辑入口 | UI-56 | APT-113, APT-126, APT-132 | CODE | PENDING |
| APT-136 | 目录差异、路由/Provisioning 健康与核对异常趋势界面 | UI-57 | APT-124, APT-126, APT-127, APT-130, APT-131 | CODE | PENDING |
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
| `TOKEN_MODEL_PUBLISH` | APT-123 | APT-123 | Token model publish admin API | 既有 immutable catalog version/outbox |
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

- **目标/范围**：主目标 `ADM-65`，仅交付 `ADM-64` 的预激活基础且不得关闭 ADM-64。修改 `migrations/account/000045_*`、`migrations/token/000024_*`、`internal/platform/approvalpolicy/{registry.go,registry_test.go}`、`internal/account/{approvals.go,approval_execution.go,approval_worker.go}`、`internal/token` 十九项命令适配器/结果查询/审计 outbox/内部执行路由、`cmd/{account-service,token-gateway}` 及对应 OpenAPI、事件和工作负载权限配置。本任务唯一拥有其余十五项策略注册、三项审批权限/策略以及十九项通用命令结果、幂等指纹和恢复索引 schema；APT-122 已拥有四项协议/公式策略版本且本任务不得重写。本任务不创建 activation record、epoch、状态列或状态机；这些对象唯一归 APT-138/`000028_*`。
- **十九项共同合同**：逐项执行下表所有者映射；所有管理命令创建 API 接受必填 reason、专项 7.2 指定 CAS/版本和 16～128 字符 `Idempotency-Key`（APT-133 隔离 origin 的 `/launch` 不是命令创建 API，固定只接收 `launch_id`），持久化 command type、actor/permission、请求摘要、幂等指纹、预检/关键版本、前后状态、审计 request_id 与脱敏结果。提交前重验权限/CAS/预检；唯一键冲突返回同一结果或 `IDEMPOTENCY_KEY_REUSED`；超时只通过结果查询恢复，禁止盲重试、`force`、`ignore_blocker`。`TOKEN_CREDENTIAL_ENROLL`、`TOKEN_CREDENTIAL_ROTATE` 也必须注册为 `AUDIT_ONLY`，结果只保存不透明引用和脱敏元数据。
- **审批/权限/执行**：严格写入专项 7.3 三组 13 字段及 exact ordered key array；lifecycle/rollback 保留 v1 并追加 v2，revoke 新增 v1；六权限仅原子授予超级/模型管理员并递增 authorization version。`executableApprovalCommands` 保留全部既有合法项，新增 Token 子集恰为三项；`token-command-executor` 仅持有 approval begin/complete 和三个 `internal.token.*.execute`。人类 direct 固定 403 是已安装但只有 APT-138 `ACTIVE` 才会被路由选择的新分支，不得在预激活阶段提前覆盖旧 v1 direct。
- **状态/兼容/回滚**：APT-138 的 activation record 不存在即被所有 N/N-1 组件解释为预激活关闭态：旧 v1 direct 保持原行为，新审批入口、executor 消费、新治理 envelope 和新增协议/公式写路径全部关闭；本任务严禁自行持久化 `PRE_ACTIVATION`、epoch 或写 `ACTIVE`。故障回滚只关闭基础 gate，并保留结果、审计及已引用策略；activation 记录缺失语义、最终状态机和原子激活唯一归 APT-138。
- **验收**：逐项验证十九项策略可解析以及通用权限/近期认证/reason/CAS/预检/持久幂等/不可变审计/稳定错误/未知结果恢复中间件合同；真实迁移后验证 DB↔registry 13 字段/数组顺序、allowlist 保留/增量、授权快照、最小执行身份和 enroll/rotate 零 Secret。增加中间提交门禁，证明 APT-122/123 代码、迁移、策略和 handler 已安装但 activation record 缺失时仍保持旧 v1 direct 原行为，且新审批、新 executor 消费、新治理 envelope、新协议/公式写均不可达、无双写。本任务不做十九项领域全链路一致性结论，该结论唯一归 APT-138。
- **不包含**：不实现 APT-124 的预检计算、凭据隔离页、Portal 操作面板、生命周期计划、目录差异或各领域既有事实状态机。

### APT-124 跨域影响预检

- **实现状态**：`DONE`；五类只读预检、同一 Token 快照的固定全序流式关键集合 SHA/组合 ETag、规范化双域预检指纹、Billing 单事务批量价格覆盖、逐 tuple READY/替代资源事实、默认关闭 gate、5 秒绝对 deadline、脱敏错误和真实迁移链集成门禁已实现，并通过身份 B 第五轮独立代码审核。随本任务创建独立提交，尚未推送或部署；本地未配置 Token/Billing PostgreSQL URL，真实 PostgreSQL 用例未执行，CI 隔离库命名门禁必须 PASS 且拒绝 SKIP。
- **目标/范围**：主目标 `ADM-57`。修改 `internal/token/{catalog.go,catalog_http.go,management.go,provisioning.go,reconciliation.go,billing.go}` 及 Token/Billing 内部客户端和 OpenAPI，只增加模型生命周期/目录回滚、Token Account 状态、路由、凭据动作的只读预检。
- **合同/回滚**：返回当前版本、价格覆盖、路由/Provisioning/在途请求、会话/替代模型、租约和替代凭据的脱敏摘要；共享 APT-120 唯一 5s deadline，失败关闭。无 schema 所有权，以 feature gate 关闭新 GET 即可回滚。
- **验收**：任一下游超时统一 `PRECHECK_TIMEOUT`；预检后并发变更使正式命令 409/失效；无权/不存在无泄露；不返回 Provider 地址、Secret 或原始错误。
- **不包含**：不改实际命令状态机、不创建审批、不做 UI 影响面板。

### APT-125 路由解析与健康状态机

- **目标/范围**：主目标 `ADM-62`。修改 `migrations/token/000025_*`、`internal/token/{routing.go,management.go,provisioning.go,gateway_adapter.go}`、`cmd/token-gateway`、Token OpenAPI 和健康 worker/指标测试。本任务唯一拥有 route-health schema。
- **合同/回滚**：路由写入只接收稳定 `model_id`、已发布 `protocol_version_id`、策略/版本/幂等字段，含 health/upstream 写入值即拒绝；服务端生成 `UNKNOWN/PROBING/HEALTHY/DEGRADED/UNHEALTHY/STALE`，HEALTHY TTL 5m。回滚先停探测 worker，保留最后证据为 STALE，再回滚读投影。
- **验收**：非发布模型/协议、客户端 health/upstream、过期/缺证健康全部失败关闭；并发探测、TTL、单调状态迁移、失败恢复和指标可观测均通过。
- **不包含**：不做 Portal 选择器、不修改模型目录事实或 Provider 凭据。

### APT-126 运营概览与跨域价格投影

- **目标/范围**：主目标 `ADM-59`。修改 `internal/token` 新增模型/运行概览查询，`internal/billing/{pricing_resolution.go,pricing_admin_v2.go,service.go}` 新增按 `model_id` 价格投影，同步两服务 OpenAPI、Portal BFF 代理白名单和定向测试。
- **边界/回滚**：Token 只聚合目录/路由/Provisioning/用量，Billing 仍是当前/未来价格、计量单位、继承和覆盖事实源；不跨库原子、不复制价格、不扫描分页列表算指标。无新 schema，两个读 gate 可分别关闭。
- **验收**：指标明确时间窗/时区/分母，403 隐藏、失败不伪装 0；Billing 无权/不可用/结果不确定都不返伪价格；并列请求局部失败可恢复。
- **不包含**：不做概览/详情 UI、不改定价写合同或生命周期。

### APT-127 三页架构、URL 与错误隔离

- **目标/范围**：主目标 `UI-47`、`UI-48`。仅修改 `internal/portal/static/{admin.html,assets/admin.js,assets/admin.css,assets/account-platform-i18n.js}`、`internal/portal/{static.go,service.go}` 及现有 `test/contract/admin-portal-ui-redesign.test.mjs`/新导航合同测试。
- **交付/兼容**：新增“模型管理/Token 资源/运行与用量”三 section，九数源和详情嵌套数据分别管理 URL filter/cursor/error/loading/refresh；跨 section、legacy `section=tokens`、popstate、非法/无权/互斥详情按已批清理顺序处理，零详情请求/零 DOM/日志泄露。
- **feature gate/回滚/验收**：独立一期 IA gate；on 时 supersede UI-28/UI-31 Token Gateway canonical，off 时历史入口不变。先关 gate 即可 UI 回滚。验证 model/token/usage/pricing-only、无权 403、刷新/分享/前进后退、单源错误/重试不清其他数据、三语、AA、320px/200%。
- **不包含**：只接入 APT-121 真实读 API 与空/错态；不做协议公式写入、选择器、运行详情、高风险动作或二期资产详情。

### APT-128 协议与安全计量公式工作台

- **目标/范围**：主目标 `UI-49`。仅修改 `internal/portal/static/{admin.html,assets/admin.js,assets/admin.css,assets/account-platform-i18n.js}`、`internal/portal/{static.go,service.go}`、Portal BFF 白名单及协议/公式 UI 合同测试；接入 APT-122 的真实草稿、预览、发布和 Billing 同步命令。
- **权限/状态/回滚**：读取、编辑、预览、发布分别按 `platform.pricing.*` 精确裁剪；协议和公式各自维护 filter/cursor/loading/error/refresh，单源失败不清空另一源。受 APT-127 一期 gate 约束，关闭 gate 即恢复旧入口；无数据库变更。
- **验收**：四类命令、公式确定性预览、版本不可变、Billing 不可用/超时、无权无泄漏、三语、键盘与 320px/200% 均有合同或 E2E 证据；页面不得执行任意表达式或展示未脱敏 Provider 数据。
- **不包含**：不改命令策略 registry、协议/公式服务端写模型、模型详情、价格编辑或高风险操作通用面板。

### APT-129 Token 所有者与路由模型选择器

- **目标/范围**：主目标 `UI-50`。仅修改 Portal Token Account 创建/编辑表单、共享搜索选择器、BFF 参数映射、i18n/CSS 和选择器合同测试；消费 APT-119 owner candidates 与 APT-125 稳定模型/协议、健康投影。
- **权限/兼容/回滚**：创建入口必须同时具备 `platform.tokens.manage` 与 `platform.tokens.owner-candidates.read`；仅 manage 时展示可解释的权限死路但不发候选请求，仅 owner-candidates 时不展示创建入口。写入只提交稳定 ID，拒绝手输 UUID、显示名、health 或 upstream；无数据库变更，关闭表单子 gate 回退旧页面只读能力。
- **验收**：用户/组织/节点候选搜索、空结果、续页、撤权 cursor 失效、模型/协议级联、HEALTHY TTL/STALE 展示、无权/不存在不可区分及键盘交互通过；覆盖 manage-only、candidate-only 和双权限矩阵。
- **不包含**：不改 owner 权限迁移、路由健康状态机、凭据、Provisioning 或模型目录事实。

### APT-130 Provisioning、用量与核对异常闭环

- **目标/范围**：主目标 `UI-51`。修改 `internal/token/{provisioning.go,provisioning_http.go,reconciliation.go}` 的 repair/retry API 适配与既有 generation/attempt/outbox 写入、Portal“运行与用量”页、稳定 URL 的主内容详情子视图、BFF 白名单/i18n/CSS及合同测试；接入 Provisioning 详情与 attempt history、用量详情/evidence、核对异常队列和“仅重新核对”命令。禁止使用抽屉、覆盖弹层或把详情挤入列表同一布局。
- **URL/状态/回滚**：Provisioning、用量、异常队列及用量 evidence 分别持有独立 filter/cursor/loading/error/refresh；仅 `provisioning_id` 与 `usage_request_id` 是详情深链参数且互斥进入全宽主内容子视图。核对异常不设独立详情参数，从异常队列选择记录后沿用其权威 `usage_request_id` 进入同一用量详情，并在该详情内显示核对证据/操作；面包屑和返回保留来源队列及各自主列表状态。受一期 gate 约束且无数据库变更，关闭子 gate 隐藏详情/处置入口但保留只读列表。
- **验收**：九类查询合同中的三类页面翻页、并发局部失败、重试、空态、403/404 无泄漏、evidence 三元 cursor、未知结果恢复和审计跳转通过；文案明确“重新核对”绝不重新请求模型。
- **不包含**：不改 Provisioning/用量事实、分页快照 schema、模型请求重放机制、趋势聚合或 Token 详情。

### APT-131 结构化高风险操作交互

- **目标/范围**：主目标 `UI-52`。仅修改 Portal 通用命令对话框/影响面板/命令客户端、BFF 代理、i18n/CSS和动作合同测试；覆盖模型发布/弃用/下架/回滚、Token Account 禁用、路由变更、Provisioning 修复、核对重试和凭据撤销入口。
- **治理/兼容/回滚**：统一采集 reason、近期认证、CAS/version、idempotency key、生效时间并先调用 APT-124 预检；依据 APT-122/123 registry 显示 `AUDIT_ONLY` 或 `APPROVAL_REQUIRED`，未知结果只按幂等键查询恢复。关闭动作子 gate 后全部变为只读；无数据库/权限/策略分类变更。
- **验收**：只验证通用对话框/影响面板/命令客户端，以及截至本任务依赖完成时已接入的协议/公式、模型基础、Token Account、路由、Provisioning 与用量命令交互；覆盖权限裁剪、预检失败、CAS 冲突、双击幂等、适用审批禁止自审、超时未知恢复及审计关联。凭据领域命令由 APT-133 验收，生命周期计划命令由 APT-134 验收，十九项总一致性仅由 APT-138 验收；清除 `prompt/confirm`，三语、焦点锁定与危险动作二次确认通过。
- **不包含**：不创建审批策略/权限、不实现预检计算、不写凭据 Secret，也不实现生命周期计划或独立详情 IA。

### APT-132 模型独立详情页

- **目标/范围**：主目标 `UI-53`。仅修改 Portal 模型详情路由、`admin.html`/JS/CSS/i18n、BFF 投影与详情合同测试；唯一 IA 冻结为“概览 / 能力、协议与公式 / 路由与运行 / 价格与计量 / 生命周期与依赖 / 审计”六页签，其他文档、原型和实现不得再拆分、合并或改名。
- **URL/权限/回滚**：稳定 `model_id` 深链，每个嵌套数据源使用独立 filter/cursor/error；价格页签要求 Billing 读权，审计页签要求资源读权与 `platform.audit.read` 交集，无权页签不请求、不进 DOM/日志。二期详情 gate 可独立关闭并回到 APT-127 列表，返回时保留列表 URL；无数据库变更。
- **验收**：合法 UUID 无权、非法 UUID、互斥详情、无权页签、刷新/分享/popstate、各嵌套源局部失败、三语和可访问性通过；稳定 ID 可复制，敏感值不可回显。
- **不包含**：只交付六页签 IA、路由、读取投影和受控空态；不实现“生命周期与依赖”中的计划写能力（APT-134）、“价格与计量”写组件（APT-135）、凭据或 Token 详情。

### APT-133 Token Account 详情与隔离凭据生命周期

- **目标/范围**：主目标 `UI-54`、`ADM-58`。修改 `migrations/token/000026_*`、`internal/token` 的 credential launch/session/result 与脱敏验证/撤销适配、独立 `cmd/token-credential-portal`（及其最小服务包）、Portal Token 详情五页签“概览、凭据、路由、Provisioning、审计”和安全自动化；本任务唯一拥有凭据 launch/session schema。TLS、DNS、反向代理、环境白名单与生产等价探针证据统一归 APT-137，本任务为 `CODE`。
- **安全合同**：完整实现专项设计 6.1～6.3，不接受缩减替代：每环境唯一固定 credential origin；Portal CSP `form-action` 精确放行；仅新增/轮换使用真实用户手势触发的 `target=_blank` + `rel="noopener noreferrer"` 顶层 POST。launch 仅在 POST body，原子消费后 303 到固定 `/session`，URL、Referer、历史、存储、window.name、遥测和日志均无 ID/Secret；10s 内无一次性 heartbeat 时原子撤销并返回 `CREDENTIAL_WINDOW_BLOCKED`，绝不降级 query/fragment/path/GET/同页/iframe。现场验证为 Portal→Token direct 脱敏命令；撤销为 Portal→Account 的 APT-123 审批命令且不进入隔离页。
- **隔离会话/回滚**：专用 OIDC audience、Authorization Code+PKCE、`prompt=login&max_age=900`，强校验 state、nonce、PKCE、auth_time、subject、CSRF Origin，并绑定环境、账户、动作、槽位、版本、浏览器和幂等摘要；launch 5 分钟有效、单次消费、最多三次尝试。独立 credential gate 或固定 origin 配置缺失时失败关闭；回滚先停新 launch、原子撤销全部未完成 session，再关闭隔离服务；down 仅在无活动/审计引用后删除本迁移对象。
- **验收**：本任务独立完成 `TOKEN_CREDENTIAL_ENROLL`、`TOKEN_CREDENTIAL_ROTATE`、`TOKEN_CREDENTIAL_VALIDATE`、`TOKEN_CREDENTIAL_REVOKE` 四项领域命令验收；覆盖五页签独立 URL/cursor、专用 audience、prompt/max_age、state/nonce/auth_time/subject、PKCE、CSRF Origin、浏览器绑定、5min/三次、原子消费/撤销、10s heartbeat、稳定错误、303 零泄漏、`window.opener === null`、刷新不重放、Referer/历史/Performance/代理及应用日志扫描、弹窗阻断无 query 降级、换浏览器/换主体/重放/超时失败关闭、验证脱敏、轮换状态机与撤销审批。十九项总一致性留给 APT-138；N/N-1 和环境探针由 APT-137 验收。
- **不包含**：不重建 KMS/Secret 底座、不允许 Portal 接触 Secret、不改三项审批策略值、不实现模型详情或生命周期。

### APT-134 模型生命周期计划与依赖分析

- **目标/范围**：主目标 `UI-55`。修改 `migrations/token/000027_*`、`internal/token/{catalog.go,catalog_http.go}` 及 lifecycle plan/worker、Token OpenAPI、Portal 模型详情既有“生命周期与依赖”页签和测试；本任务唯一拥有计划版本、计划状态与执行租约 schema，只向 APT-132 已冻结页签增量交付内容，不新建/改名/拆分页签。
- **治理/兼容/回滚**：展示 APT-124 价格/路由/Provisioning/在途请求/会话/替代模型依赖，支持计划发布、弃用、下架以及新会话/存量会话截止时间；提交 APT-123 lifecycle v2 审批并把 `lifecycle_plan_version` 纳入关键快照，worker 仅消费获批事件。gate 关闭先停止新计划与 worker 领租，保留既有计划只读和审计，禁止用 down 反向恢复已执行生命周期事实。
- **验收**：本任务独立完成 `TOKEN_MODEL_LIFECYCLE_CHANGE` 领域命令验收；时区/DST、开始与截止边界、依赖变化导致审批失效、禁止自审、重复事件幂等、租约接管、未知结果恢复、取消未执行计划、已执行不可逆和无替代模型失败关闭均通过。十九项总一致性仅由 APT-138 验收。
- **不包含**：不实现目录差异 UI、价格编辑、路由健康算法、会话迁移执行或修改 lifecycle v2 策略字段。

### APT-135 模型上下文价格与计量入口

- **目标/范围**：主目标 `UI-56`。仅修改 Portal 可复用价格/计量组件、模型详情“价格与计量”页签、财务“定价与商品”复用入口、BFF 与合同测试；读写均消费 Billing 现有 API 与 APT-126 投影。
- **领域/权限/回滚**：Billing 仍是金额、积分费率、计量单位、继承/覆盖和价格生命周期的唯一事实源；模型页只提供当前模型上下文，不复制 schema/缓存或创建第二套价格。严格复用现有 `platform.pricing.read` / `platform.pricing.manage`、价格生命周期、CAS、不可变审计与 `AUDIT_ONLY` 命令，不虚构价格审批权限或审批流程；关闭二期价格入口 gate 后模型详情回到只读摘要，财务入口不受影响。
- **验收**：当前/未来价格、继承来源、覆盖缺口、计量单位、无价/不可用/403、跨入口同一 version/ETag、CAS 冲突、生命周期与 `AUDIT_ONLY` 审计关联、三语和错误隔离通过；不得用 0 伪装未知价格。
- **不包含**：不改 Billing schema、定价算法/权限/治理模式、协议公式事实或财务信息架构，不新增任何价格审批跳转。

### APT-136 目录差异与运行健康趋势

- **目标/范围**：主目标 `UI-57`。仅修改 Portal 模型/运行概览、目录同步/回滚差异预览、趋势卡片/图表、BFF 映射、i18n/CSS和可视化合同测试；只消费 APT-126 聚合与 APT-124 预检。
- **状态/回滚**：展示已发布/草稿模型、目录版本差异、异常路由、失败 Provisioning、待核对用量及明确时间窗的趋势；各卡片独立 loading/error/refresh，不扫描分页列表计算指标。同步/回滚经 APT-131 结构化动作，回滚命令走 APT-123 rollback v2。关闭二期 overview gate 即隐藏增强卡片；无数据库变更。
- **验收**：差异新增/修改/删除、版本选择、空差异、聚合延迟/数据时间、0 与未知区分、局部失败、审批链、回滚未知恢复、单位/时区和 320px/200% 通过。
- **不包含**：不新建指标事实/仓库、不改目录同步算法、健康探测状态机、核对处置或审批策略。

### APT-138 十九项命令一致性总验收与原子激活

- **目标/范围**：主目标 `ADM-64`。在 APT-122～APT-136 的领域/API 接入全部通过独立审核后，修改 `migrations/token/000028_*`、`internal/token` 治理激活仓储/路由门禁、Account 审批入口激活校验、`internal/platform/approvalpolicy` 总合同测试及发布前集成测试。本任务唯一拥有持久治理激活记录（状态、epoch、策略集合摘要、十九项处理器摘要、操作者、CAS、时间与审计 request_id），不改任何领域事实 schema。
- **状态机/原子边界**：初始只能是 `PRE_ACTIVATION`：旧 v1 direct 保持当前兼容行为，新治理/新审批写入口关闭；全部一致性门禁成功后，只允许一次单事务 CAS 将 activation record 从 `PRE_ACTIVATION → ACTIVE`，同一持久 epoch 的同一次原子路由切换关闭三项旧 direct、接受新审批执行并开放其余十六项新治理 envelope，不允许逐命令、逐实例或重复激活，不存在双写窗口。`ACTIVE → SAFE_ROLLBACK` 只停止十九项新写、审批领取和执行消费，绝不重开旧 direct；修复并重跑全部门禁后只允许 `SAFE_ROLLBACK → ACTIVE`。禁止回到 `PRE_ACTIVATION`、跳过状态、客户端指定状态或仅切一项命令。
- **N/N-1/故障恢复**：Account 创建审批前强一致校验 Token activation epoch；Token 处理 direct/执行/结果查询时校验同一持久 epoch。N-1 不认识 `ACTIVE/SAFE_ROLLBACK` 或策略集合摘要时失败关闭，新旧实例混跑不得自行降级 direct。切换进程崩溃、数据库提交不确定、事件重复/乱序、Account/Token 网络分区只按 activation CAS 与 command result 查询恢复，不重放写命令。
- **总验收**：十九项逐一核对策略/权限/近期认证/reason/CAS/持久幂等/影响预检/审计/未知恢复、API handler 和领域存储所有者；三项审批逐字段比对 DB↔registry、allowlist、最小执行身份和 direct 403，十六项 `AUDIT_ONLY` 验证不可绕过中间件。故障注入覆盖切换前、CAS 提交中、提交后响应丢失、混合 N/N-1、重复事件、进入/退出 `SAFE_ROLLBACK`，证明状态迁移合法、零双写、零 direct 复活和审计连续。
- **回滚/不包含**：应用回滚只能 CAS 进入 `SAFE_ROLLBACK` 并保留 activation/command/audit 历史；`000028` down 在任何非 `PRE_ACTIVATION` 或有引用时失败关闭。不实现页面、领域命令、预检计算、凭据隔离、TLS/代理或生产部署；发现差异必须退回唯一所属 APT 修复，不能在本任务夹带修补。

### APT-137 分级发布、兼容与总回滚门禁

- **目标/范围**：主目标 `UI-58`。仅修改 Account/Token/Portal 配置说明与 feature-gate 装配、`deploy/compose`、`deploy/helm`、OpenAPI/Portal 契约门禁、运维 runbook 和发布验证脚本；汇总 APT-127～APT-136，并唯一负责 credential origin 的 DNS/TLS、Portal/credential CSP、固定环境白名单、反向代理/WAF 日志抑制和生产等价健康探针证据，但不夹带功能修复。
- **发布/兼容/回滚**：冻结一期 IA/列表/协议公式/选择器/运行闭环/结构化操作与二期详情/凭据/生命周期/价格/概览的独立 gate 和依赖顺序；服务端、Portal、cursor envelope、审批事件和隔离凭据入口支持 N/N-1。`section=tokens` 至少保留 90 天且跨两个稳定版本规范化；UI-47 开启后 supersede UI-28/UI-31 canonical。回滚顺序为先停写/worker/launch，再关二期、一期和代理路由；活动 cursor 返回 410，进行中审批/计划/session 按各任务合同收敛。
- **验收**：逐 gate on/off、混合 N/N-1、滚动升级/回滚、旧链接/书签/popstate、九源 cursor 独立、CSP/origin、DNS/TLS/代理/WAF、10s heartbeat 探针、DB↔registry 逐字段一致、`executableApprovalCommands` 保留既有项且 Token 增量恰三项、容量及 `10→12→14`/`3→5→7`/`5→7→9` 绝对 deadline 链/告警、三语与安全扫描全部成为发布门禁，并记录生产等价环境证据。
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
