# 账号平台未完成需求实施任务

> 状态：当前执行队列
>
> 需求与状态基线：`e5437902aea2558b78b8076f3645834611ea2514`
>
> 代码审计基线：`e23005aaf6745d4b8c1ffa5ac73943e0f8f98ca5`（APT-001 以本文件所在提交增量闭环）
>
> 约束边界：`architecture.md`、`role-system-design.md`、Accepted ADR 与本目录部署文档

## 1. 执行规则

1. 本文为最初 275 个未完成目标及后续需求变更保留可追溯任务。当前 57 个任务中，46 个仓库内可闭环任务为 `DONE`，10 个 `CODE+ENV` 任务已完成仓库实现但仍等待业务冻结、生产等价环境或正式制品证据并保持 `BLOCKED_EXTERNAL`，1 个 `CODE` 任务为 `PENDING`；规范矩阵现有 33 个状态为 `部分实现` 或 `未实现` 的目标编号，每个未完成编号只能有一个主任务。
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
| APT-007 | 超级管理员安全引导与异常失联 Break-glass 的生产等价演练 | ADM-01, ADM-02 | APT-006, APT-046 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-008 | 管理命令审批分类、生命周期、失效、授权复核和幂等执行 | ADM-07, ADM-17, ADM-22, ADM-36–ADM-42 | APT-006, APT-046 | CODE | DONE |
| APT-009 | 顶层组织创建、搜索、启停和跨域安全逻辑删除 | ACC-15–ACC-20 | APT-006, APT-008 | CODE | DONE |
| APT-010 | 组织节点创建、移动、重命名和受阻逻辑删除 | ACC-21, ACC-23–ACC-25 | APT-006, APT-009 | CODE | DONE |
| APT-011 | 多方式邀请、成员增删、所有权转让和即时撤权 | ACC-26–ACC-29 | APT-006, APT-009, APT-010 | CODE | DONE |
| APT-012 | 用户级全局活动上下文、跨端版本同步和成员失效回退 | CORE-02, ACC-34, ACC-38, ACC-39, NFR-08, UI-06, UI-07 | APT-006, APT-011 | CODE | DONE |
| APT-013 | Web 个人中心三 Tab 合并、成功订单过滤与普通用户资源边界收敛 | UI-03 | APT-012 | CODE | DONE |
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
| APT-031 | Token Account、凭据和检查动作的平台权限隔离 | SCOPE-03, ADM-25–ADM-27 | APT-006, APT-026, APT-028 | CODE | DONE |
| APT-032 | 桌面动态目录消费、按会话冻结模型、显式替代分支与受信个人中心入口 | SCOPE-05, TOK-06, UI-09 | APT-003, APT-027–APT-030 | CODE | DONE |
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
| APT-045 | 全域审计、幂等指纹、Outbox/Inbox 收敛和死信恢复闭环 | CORE-07, NFR-05–NFR-07 | APT-003–APT-034, APT-046, APT-047 | CODE | DONE |
| APT-046 | 普通组织角色绑定的展示、有效期、授权撤销和缓存失效 | ACC-30–ACC-32, ADM-05, ADM-06 | APT-006 | CODE | DONE |
| APT-047 | 服务账号、后台任务和 Event Relay 的短期工作负载身份与最小 Subject 权限 | ADM-03, NFR-12 | APT-006 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-048 | 正式充值商品 SKU、商城金额事实、权益及 Provider 映射的业务冻结与生产准入 | PAY-05, PAY-10 | APT-022 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-049 | 账单支付证据、完整关单状态、拒付事实与订单派生摘要收敛 | PAY-12–PAY-14 | APT-019, APT-023, APT-024 | CODE | DONE |
| APT-050 | 模型派发不确定、流式结果与支付证据统一收敛边界 | CORE-06 | APT-029, APT-030, APT-049 | CODE | DONE |
| APT-051 | 全域人工案件 P0/P1/P2 统一 SLA、转派时钟与自动止损评估 | NFR-45 | APT-003, APT-019, APT-021, APT-024, APT-025, APT-030 | CODE | DONE |
| APT-052 | 非计费客户端切换 Readiness、短时签名准入证明及发布系统前置能力 | TOK-37 | APT-018, APT-027, APT-028 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-053 | 客户端幂等迁移及发布/安装/运行三层门禁的正式制品验收 | TOK-35, TOK-36, NFR-20 | APT-032, APT-052 | CODE+ENV | BLOCKED_EXTERNAL |
| APT-054 | 紧急模型下架触发在途取消并保存实际取消、生成量、Hold 与结算证据 | TOK-47 | APT-030, APT-032 | CODE | DONE |
| APT-055 | 服务端受控审计导出、重认证、PII临时授权、加密水印和一次性下载配额 | ADM-09, NFR-48 | APT-006, APT-008 | CODE | DONE |
| APT-056 | 普通退款产品级默认关闭门禁、用户/运营入口屏蔽及服务端创建拒绝 | PAY-31, UI-12, ADM-20 | APT-021, APT-022, APT-034 | CODE | PENDING |
| APT-057 | Provider 中立短期回调授权、Profile 化通知适配、统一证据归并与查询补偿；新华三作为首个 Profile | PAY-15–PAY-17 | APT-022, APT-023, APT-036, APT-041 | CODE | DONE |
| APT-058 | 账号中心独立 HTTPS 子域名、`/api/v1` 支付回调公网命名空间及旧官网入口彻底移除 | UI-09, PAY-15–PAY-17, NFR-03, NFR-04 | APT-032, APT-057 | CODE+ENV | BLOCKED_EXTERNAL |

## 3. 独立提交验收模板

每个任务提交前记录：

- `task_id` 与全部 `target_requirement_id`。
- 代码、迁移、契约、事件、UI 和文档改动范围。
- 正常、拒绝、幂等、并发、失败恢复和权限隔离测试。
- `implementation-status.md` 的状态、证据、已知差距和部署版本变化。
- 实际执行的验证命令与结果。
- 尚需生产等价环境验证的项目；没有环境证据时保持 `NOT_DEPLOYED`。

提交信息使用 `type(scope): 中文描述` 并包含有效 `Signed-off-by`。一个提交只关闭一个 `task_id`；任务依赖只能通过既有已提交接口消费，不能在当前提交中顺带实现其他待办。
