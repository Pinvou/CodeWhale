# 账号平台管理后台改版设计——独立修订稿

> 文档状态：基础改版已获产品批准；财务支付两期重构已于第三轮独立复审通过。
>
> 版本日期：2026-08-27

## 0. 文档效力与来源优先级

《账号平台管理后台·改版设计.pdf》的 38 个画板只作为视觉布局、信息密度和组件风格基线，不是权限、流程或接口事实源。画板与本文冲突时，以本文为准；本文与已批准需求或架构冲突时，以需求和架构为准，并阻止实现，不能用视觉稿覆盖业务约束。

本修订稿依据以下当前事实编制：

1. `account-platform/docs/requirements.md`：业务需求与非功能要求。
2. `account-platform/docs/architecture.md`：服务边界、治理模式和安全边界。
3. `account-platform/docs/role-system-design.md`：三类权限、内置角色和精确权限矩阵。
4. `account-platform/docs/implementation-tasks.md`：交付任务和环境门禁。
5. `account-platform/contracts/openapi/admin.yaml` 及各服务 OpenAPI：已定义的管理接口。
6. 当前路由注册、审批策略注册表和真实代码：已实现行为的核验依据。

除第 1.3 节已经产品批准的 `UI-28`～`UI-32`、`UI-37`～`UI-46`、`ADM-54`、`BILL-47`、`NFR-55` 界面与交互专项需求外，本文不得创造新的业务需求、权限或接口。财务支付重构的领域边界、两期范围和任务顺序以 `admin-finance-payment-redesign.md` 为准。原稿中的“新建用户”“新建角色”、KYC、邮箱恢复、Portal 内 2FA、Portal 代理 bootstrap、break-glass 或 IdP 管理等能力全部删除或明确禁用。

---

## 1. 范围与非目标

### 1.1 范围

本次改版覆盖公网管理 Portal 的信息架构、权限驱动导航、管理员日常只读查询、受控业务写入、审批协作、异步任务、审计导出、错误与状态反馈，以及桌面/中宽/移动端适配。页面覆盖原 PDF 的 S1～S38 画板，但其行为必须受现有 API、权限和治理策略约束。

### 1.2 非目标

- 不提供后台“新建用户”。用户建立只能走现有注册链路；后台仅查询、状态管理和受控修复。
- 不提供“新建/编辑/删除角色”。角色定义为内置事实；后台仅查看角色并管理角色绑定。
- 不提供 KYC、邮箱恢复或无需求支撑的 2FA 设置。
- 不开放退款发起入口；只展示既有退款记录并处理失败、不确定或强制异常案件。
- 不在公网 Portal 执行或代理 bootstrap、break-glass、IdP 管理、密钥轮换 CLI 或任意 shell。
- 超级管理员默认具备全部常驻平台管理权限；Token 高风险操作仍逐接口校验原子权限并保留审批、审计、脱敏和不可回显约束。
- 不在客户端推断权限、审批策略或状态迁移；服务端始终是授权与状态事实源。

### 1.3 本轮已批准的 UI/UX 需求边界

本轮产品批准的是基于现有接口与既有功能需求的管理后台界面改版，不批准任何新的业务能力、权限、状态迁移或服务端接口。`account-platform/docs/requirements.md` 中的 `UI-28`～`UI-32` 是这五项需求的正式编号源；本专项文档定义其详细页面范围、交互合同和验收边界，不能替代或扩张正式需求。五项目标通过独立复审后，必须由后续独立文档步骤把 `UI-28`～`UI-32` 同步进 `implementation-status.md` 的需求状态矩阵，再按矩阵中的未实现状态创建正式任务；本次修订不编辑状态矩阵，状态矩阵尚未同步时不得建任务或进入实现。

| 界面需求 | 目标 | 必须实现 | 明确不改变 |
|---|---|---|---|
| `UI-28` | 统一后台壳层与共享基础组件 | 拥有后台应用壳层、分组导航、路由/URL、当前项、页头、语言、退出登录，以及可复用的加载/空/无权/错误/部分失败容器、状态标签、表格/详情框架、对话框、Toast、焦点与状态播报基础组件；在本项提交内完成这些壳层/组件自身的简中/英文/日文、WCAG 2.1 AA、320px/768px/1440px/200% 响应式和组件状态测试；只显示具有精确读权限的入口 | 不负责各领域页面的数据接线或页面级验收；不新增导航对应的服务端资源；不因视觉分组合并权限 |
| `UI-29` | 身份、用户、角色与组织界面改版 | **依赖 UI-28**；在本项单独提交内完成用户、角色绑定、组织、节点及现有修复页面的真实数据接线，并完成全部受影响页的简中/英文/日文、WCAG 2.1 AA、320px/768px/1440px/200% 响应式，以及加载、空、无权、错误、部分失败、冲突、离线/超时、结果不确定状态 | 不新增用户或角色；不改变用户、角色、组织生命周期及管理动作；质量与状态不得推迟到 UI-32 补做 |
| `UI-30` | 财务、支付、审批、定价与充值商品界面改版 | **依赖 UI-28**；在本项单独提交内完成现有额度/账本、订单、案件、调账、Hold、审批、定价和商品页面，并完成全部受影响页的简中/英文/日文、WCAG 2.1 AA、320px/768px/1440px/200% 响应式，以及加载、空、无权、错误、部分失败、冲突、离线/超时、结果不确定状态；资金与 CNY 明确区分 | 不开放退款入口；不新增探测、恢复、定价或商品命令；不可用能力继续隐藏；质量与状态不得推迟到 UI-32 补做 |
| `UI-31` | 模型、Token、系统与审计界面改版 | **依赖 UI-28**；在本项单独提交内完成现有模型、Token Account、路由、凭据元数据、Provisioning、用量、系统状态、审计与只读运行手册页面，并完成全部受影响页的简中/英文/日文、WCAG 2.1 AA、320px/768px/1440px/200% 响应式，以及加载、空、无权、错误、部分失败、冲突、离线/超时、结果不确定状态；秘密不回显 | 超级管理员默认获得常驻 Token 管理权限，但不能查看秘密或临时敏感数据；不增加公网 bootstrap、break-glass、IdP 或事件重放执行入口；质量与状态不得推迟到 UI-32 补做 |
| `UI-32` | 无产品代码的跨页面集成与发布验收 | **依赖 UI-28、UI-29、UI-30、UI-31 全部完成**；仅新增或调整集成/E2E/视觉/无障碍验收与发布证据，不修改产品代码；按第 9、11、13～15 节验证异步恢复、真实错误、加载、冲突、离线/超时、结果不确定、三语、AA、响应式、字段展示和危险操作，并验证构建制品与源码一致 | 不承担 UI-28～UI-31 漏做的产品实现；不用前端假数据补齐服务端缺口；不把测试通过解释为新增功能已上线 |
| `UI-37` | 财务支付三分区信息架构 | 保留 `billing/payments/products` 稳定 section，将用户可见页面调整为“账户与计费、支付与对账、定价与商品”；模型定价迁入定价与商品，支付四方对账迁入支付与对账；子页 URL、权限和前进后退独立 | 不把分组变更解释为权限合并；不改变账本、支付或定价事实 |
| `UI-38` | 手工调账申请与审批下穿 | 从账户与计费创建或恢复 `WALLET_ADJUSTMENT` 申请，并下穿审批中心查看状态 | 申请、审批、服务身份执行分离；申请人不得自审 |
| `UI-39` | 支付渠道控制与恢复申请 | 展示 Provider/商品分层控制和证据，分别使用服务端恢复草稿发起申请 | 不把 Provider 恢复与商品恢复合并；不在财务页批准或执行 |
| `UI-40` | 财务支付子页独立状态 | 当前子页独立加载、刷新、失败和权限裁剪 | 单个子页失败不清空或阻断其他子页 |
| `UI-41` | 既有筛选与商品稳定分页 | 接入已有筛选和充值商品 cursor，并保持 URL/返回上下文 | 不伪造服务端筛选、总数或 cursor |
| `UI-42` | 财务支付结构化操作 | 用结构化表单、字段校验、确认摘要和错误映射替代浏览器 prompt | 不开放普通退款；危险操作不丢失原因、幂等或版本事实 |
| `UI-43` | 旧价格写入口收敛 | 移除旧 `pricing-versions` 写入口，新生命周期未交付时只读展示 | 不用旧接口假扮草稿、预览、发布或转换 |
| `ADM-54` | 定价与计量命令策略 | 精确注册六个现有管理命令为 `AUDIT_ONLY` | 不扩权、不改变领域状态机，未知命令继续失败关闭 |
| `UI-44` | 计量单位与模型定价生命周期 | 接入计量单位、草稿、预览、发布、未来生效和转换 UI | 不原地编辑已发布事实；逐端点遵循真实 ETag/幂等合同，不模拟缺失的 CAS |
| `NFR-55` | Billing 稳定分页合同 | 高数据量列表补齐稳定 cursor、确定排序和规范化筛选 | 不使用无界 limit 或当前页前端切片冒充分页 |
| `UI-45` | Billing 全量分页 Portal 适配 | Portal 消费 Billing cursor，并以 URL 恢复目录上下文 | 不把当前页条数显示为全量总数 |
| `BILL-47` | 财务支付概览口径 | 三个分区分别返回权限裁剪、单位与时间窗明确的概览块 | 局部失败不伪装为 0，不混合积分和 CNY |
| `UI-46` | 财务支付运营概览界面 | 三个分区独立展示概览、局部失败并可筛选下穿 | 不在前端扫描列表计算平台总量 |

### 1.3.1 设计稿一致性优先级与强制版式

正式实现的视觉和页面编排以 `admin-portal-ui-prototype.html` 为第一优先级；本文负责约束业务、权限和异常状态，原 PDF 只作为视觉风格补充。三者冲突时，业务事实服从真实 API 与权限合同，视觉及交互编排服从 HTML 原型，不得退回旧版管理台配色或仅移动原有 Tab。

- 壳层固定采用白色 `272px` 侧栏、白色 `72px` 顶栏、`#f4f6f9` 页面底色和 `#3159e8` 主色；旧版深色侧栏、青绿色主色及深色总览 Hero 不得出现在源码或构建制品。
- 每个领域页面按“领域眉题与页面标题 → 页面级动作 → 独立状态/错误 → 指标或业务摘要 → 卡片标题 → 查询/筛选工具栏 → 表格/列表 → 分页或辅助说明”组织。业务页面不得只通过改变 Tab 位置宣称完成改版。
- 用户、组织、订单、案件、Token Account、用量请求和审计等已有详情能力统一采用右侧详情抽屉；列表上下文保留，遮罩点击或 `Esc` 可关闭，关闭后焦点返回触发控件。危险写操作继续使用强确认或审批交互，不在详情抽屉中绕过原治理规则。
- 角色绑定、注册修复、审批和商品等历史挂载页面必须进入当前领域卡片流，并继承同一主色、圆角、工具栏、表格、状态与响应式规则；不得以独立旧页面样式悬挂在新壳层之外。
- 1440px 采用四列指标卡和最大 `1480px` 内容区；中宽降为两列，窄屏为单列并使用可横向滚动的数据表；粗指针目标不小于 `44px`，减弱动画偏好必须关闭非必要过渡。

实现必须渐进替换当前管理后台，不改变现有 API 请求路径和请求正文。独立挂载的注册修复、角色绑定、审批、定价和充值商品模块可以在本轮统一样式与页面编排，但不得通过复制实现产生第二套状态事实或重复请求。

### 1.4 产品批准与候选项转换

- 原候选 `UIA-001` 已获批准，并拆分为 `UI-28`～`UI-31` 四个可独立提交的界面需求。
- 原候选 `UIA-018` 仅批准“基于现有 API 的基础改版总验收”，转换为 `UI-32`；其验收范围不自动包含任何未批准候选 API。
- 财务支付重构 `UI-37`～`UI-46`、`ADM-54`、`BILL-47`、`NFR-55` 已由产品明确批准，按 `admin-finance-payment-redesign.md` 的一期、二期及 APT-105～APT-117 顺序交付；其批准不自动扩展到发票、税务、银行结算、提现或定金领域。
- 原 `UIA-013` 已全量转换为 `NFR-55 / APT-114`；原 `UIA-015` 的 Billing Portal 适配已转换为 `UI-45 / APT-115`，仅 Account/Token 其余列表仍保留候选；原 `UIA-020` 的六个 Billing 命令已转换为 `ADM-54 / APT-112`，仅四个 Token 协议/公式命令仍保留候选。其余未获批准候选继续执行 `NOT_AVAILABLE` 或永久隐藏，不创建服务端任务，也不作为本轮任务依赖。
- PDF 与 HTML 原型继续只提供信息层级、布局和组件风格参考；原型中的示例数据、演示身份切换器和没有真实接口的按钮不得进入生产后台。

---

## 2. 设计原则

1. **最小权限**：导航、页面、卡片、字段和动作分别鉴权；能进入页面不等于能看到所有字段或执行所有动作。
2. **精确授权**：只使用已定义的原子权限；不使用模糊的 `admin`、`manage_all` 或前端角色名判断。
3. **作用域先于展示**：系统权限作用于平台；组织权限必须同时满足当前成员身份、组织状态和 `ORGANIZATION/UNIT` 节点范围。
4. **职责分离**：申请、审批、执行三方分离；申请人不能自审，执行只能由审批策略冻结的最小权限服务身份完成。
5. **状态机闭合**：界面只展示服务端允许的动作；终态不可通过刷新、重试或换页面绕过。
6. **并发安全**：只在端点真实支持时使用 `Idempotency-Key`、领域 expected version 或 ETag；缺失时按第 7 节标记 API_GAP，不能由前端模拟。
7. **可追溯**：每个敏感动作记录操作者、权限、作用域、原因、请求 ID、资源版本、审批链和结果。
8. **故障可恢复**：异步任务刷新后可恢复；结果未知时只能查询或对账，不得盲目重复执行。
9. **安全默认**：无权、作用域不明、策略缺失、认证过期、服务端已有探测证据失败/过期/缺失或状态不明时拒绝写入。
10. **三语与无障碍同交付**：简体中文、英文、日文及 WCAG 2.1 AA 是验收内容，不是后续优化。

---

## 3. 信息架构与权限驱动导航

### 3.1 一级导航

| 导航组 | 页面 | 出现条件 |
|---|---|---|
| 工作台 | 平台总览 | 至少具有一个平台页面的精确读权限；卡片仍按各自读权限独立请求和展示 |
| 身份与组织 | 用户管理、角色与权限、组织管理、我的组织 | 子页分别要求现有 `platform.users.*` 修复/读取权限、`platform.roles.read`、`platform.organizations.read` 或现有组织作用域读权限；没有可用接口的“我的组织”隐藏 |
| 财务与支付 | 账户与计费、支付与对账、定价与商品 | `billing/payments/products` section 保持稳定；账户页按钱包/账本/应收/Hold 权限，支付页按订单/案件/对账/控制权限，定价商品页按 `platform.pricing.*` 与 `platform.products.*` 分区块独立显示；一个子页的权限不解锁相邻子页 |
| 模型与 Token | Token Gateway | 至少具有 `platform.models.read`、`platform.tokens.read` 或 `platform.usage.read` 中该页真实数据所需权限；写权限不替代读权限 |
| 治理与安全 | 审批中心、审计与导出、系统与安全、偏好设置 | 审批读取沿用 `platform.billing.wallets.read` 且动作继续精确鉴权；审计和系统分别要求 `platform.audit.read`、`platform.system.status.read`；偏好设置只在已有管理页面可访问且存在真实本地偏好时显示 |

以上五组及页面名称以 `admin-portal-ui-prototype.html` 的 `navGroups` 为视觉信息架构，财务三个 section 的领域归属以 `admin-finance-payment-redesign.md` 为业务真相源，不再保留“总览/治理/运行手册/系统运行”六组旧分类。角色、支付、商品、审批、偏好均为独立 URL `section`，不能作为用户或财务页内的二级聚合 Tab。只读运行手册作为“系统与安全”或“审计与导出”的内容区存在，不形成第六导航组；面向具体用户的注册、资料与注销修复归入“用户管理”，Keycloak 平台级受控入口及隧道说明归入“系统与安全”，两者使用各自既有精确权限独立显示。

### 3.2 导航与组件鉴权

- 页面是否显示由服务端返回的精确权限集合决定，不用角色名称硬编码。
- 原型定义页面但当前没有真实接口或适用读权限时必须隐藏；不得以静态样例、假数据或空壳页面补齐导航。
- 页面标题、统计卡片、表格列、详情字段、导出按钮和行内动作分别声明所需权限。
- 对无权字段不请求或由服务端裁剪；不得请求后仅用 CSS 隐藏。
- 无页面读权限时不显示导航；直接访问路由返回统一“无权访问”，不泄露资源是否存在。
- 一个聚合接口返回多领域数据时，每个数据块携带 `visibility` 或按权限裁剪；前端不得以“能看总览”为由显示财务、Token 或 PII。

### 3.3 组织选择器与节点范围

- 系统管理页不复用组织作用域；组织管理页必须显式选择组织。
- 组织选择器只列出当前有效成员关系可访问的组织；组织为 `DISABLED/DELETED` 时按服务端权限只读或拒绝。
- `scope_type=ORGANIZATION` 可覆盖整个组织；`scope_type=UNIT` 仅覆盖目标节点及其子树。
- 当前节点必须显示在页头和每个写入确认框中；切换节点后清空勾选、草稿和分页游标。
- 后端对每次请求重新验证成员关系、组织状态、角色绑定和节点祖先关系，拒绝客户端伪造 `unit_id`。

---

## 4. 内置角色与权限边界

### 4.1 系统内置角色

| 角色 | 主要能力 | 明确不包含 |
|---|---|---|
| `PLATFORM_SUPER_ADMIN` | 全部常驻平台管理能力，包括模型、Token Account、路由、Provisioning 与凭据现场校验 | 不包含临时 PII 导出权限；不能查看秘密、绕过审批或绕过最后超级管理员保护 |
| `PLATFORM_IDENTITY_ADMIN` | 用户查询/状态、注册/资料/注销修复、角色定义与既有绑定只读 | **不含 `platform.roles.assign`，不能 POST/DELETE 角色绑定**；不含财务、Token、审计导出、角色定义创建 |
| `PLATFORM_ORGANIZATION_ADMIN` | 组织查询、创建、状态、删除、所有权及组织结构管理 | 财务审批、Token、角色定义创建 |
| `PLATFORM_BILLING_OPERATOR` | 财务读、同步、修复/调账申请、Hold 释放、支付恢复申请、商品只读、用量只读 | 自己审批、支付恢复批准、Token |
| `PLATFORM_BILLING_APPROVER` | 财务读、修复/调账批准、支付恢复批准、用量只读 | 发起同一案件并自审、Token、商品发布 |
| `PLATFORM_PRICING_ADMIN` | 定价及充值商品目录管理 | Token、财务审批；商品恢复仍须独立审批链 |
| `PLATFORM_MODEL_ADMIN` | 模型、Token Account、Token 路由、凭据验证、用量查询/重试 | 财务审批、组织管理 |
| `PLATFORM_OPERATIONS_ADMIN` | 系统状态；角色矩阵保留 `platform.events.retry`，但当前无 API/UI | Token 凭据、财务批准、PII 导出 |
| `PLATFORM_AUDITOR` | 审计只读及默认脱敏导出 | 写操作、永久 PII 导出能力 |

系统角色只由权限构成。Portal 只查看角色定义、授予和撤销绑定，不允许创建、改名、编辑权限集合或删除内置角色。

最后一个有效超级管理员不得主动停用自身、注销自身账号、删除自身账号或撤销导致平台不再存在有效超级管理员的角色绑定。break-glass 只用于管理员异常失联后的受控恢复，不是该保护规则的绕过通道。

### 4.2 路由—字段—动作—权限—作用域—治理真值表

“治理模式”取值：`READ_ONLY`、`DIRECT_BUSINESS`、`AUDIT_ONLY`、`APPROVAL_REQUIRED`、`OUT_OF_BAND`、`NOT_AVAILABLE`。`DIRECT_BUSINESS` 是普通用户/组织业务动作并继续接受领域门禁；`AUDIT_ONLY` 表示管理服务端授权与审计，不自动证明该端点具备近期认证、原因、幂等或版本字段；这些能力以第 7 节真实合同为准。`APPROVAL_REQUIRED` 不能调用直接执行接口绕过审批；`NOT_AVAILABLE` 不产生 UI 或代理调用。

| 路由/页面 | 字段或动作 | 原子权限 | 作用域 | 治理模式 |
|---|---|---|---|---|
| `/overview` | 用户/组织统计卡 | `platform.users.read` / `platform.organizations.read` | PLATFORM | READ_ONLY，卡片独立裁剪 |
| `/overview` | 财务卡 | `platform.billing.wallets.read` | PLATFORM | READ_ONLY，金额脱敏策略适用 |
| `/overview` | Token/用量卡 | `platform.tokens.read` / `platform.usage.read` | PLATFORM | READ_ONLY；超级管理员默认显示，其他角色仍按原子权限裁剪 |
| `/users` | 列表、状态、`user_code` | `platform.users.read` | PLATFORM | READ_ONLY；手机号脱敏 |
| `/users/:id` | 启用/停用 | `platform.users.status.manage` | PLATFORM | AUDIT_ONLY |
| `/users/:id` | 新建/删除用户 | 无 | 无 | **禁用，不渲染、不提供接口** |
| `/roles` | 角色定义 | `platform.roles.read` | PLATFORM | READ_ONLY |
| `/role-bindings` | 授予/撤销绑定 | `platform.roles.assign` | PLATFORM 或组织绑定所载 scope | AUDIT_ONLY |
| `/roles` | 新建/编辑/删除角色 | 无 | 无 | **禁用，不渲染、不提供接口** |
| `/organizations` | 列表/详情 | `platform.organizations.read` | PLATFORM | READ_ONLY |
| `/organizations` | 创建 | `platform.organizations.create` | PLATFORM | AUDIT_ONLY |
| `/organizations/:id` | 改名/结构/成员/状态 | `platform.organizations.status.manage` | PLATFORM | AUDIT_ONLY |
| `/organizations/:id` | 转移所有权 | `platform.organizations.owner.transfer` | PLATFORM | AUDIT_ONLY，强确认 |
| `/organizations/:id` | 删除 | `platform.organizations.delete` | PLATFORM | AUDIT_ONLY，跨域预检；DELETED 终态 |
| `/repairs/registrations` | 列表、重试、补偿 | `platform.registrations.repair` | PLATFORM | 列表 READ_ONLY；`ACCOUNT_REGISTRATION_RETRY/COMPENSATE` 固定 AUDIT_ONLY |
| `/repairs/profile-changes` | 列表、重试 | `platform.profile-changes.repair` | PLATFORM | 列表 READ_ONLY；`ACCOUNT_PROFILE_CHANGE_RETRY` 固定 AUDIT_ONLY |
| `/repairs/account-closures` | 列表 | `platform.account-closures.read` | PLATFORM | READ_ONLY |
| `/repairs/account-closures` | 重试 | `platform.account-closures.repair` | PLATFORM | `ACCOUNT_CLOSURE_RETRY` 固定 AUDIT_ONLY |
| `/billing/wallets` | 钱包列表 | `platform.billing.wallets.read` | PLATFORM | READ_ONLY |
| `/billing/wallets/:id/ledger` | 账本列表 | `platform.billing.ledger.read` | PLATFORM | READ_ONLY |
| `/billing/receivables*` | 应收与 allocations 读取 | `platform.billing.wallets.read` | PLATFORM | READ_ONLY |
| `/billing/payment-orders*`、`/billing/refunds`、`/billing/late-payment-reviews*`、`/billing/payment-anomalies*`、`/billing/adjustments` | 列表/详情读取 | `platform.billing.wallets.read` | PLATFORM | READ_ONLY |
| `/billing/payment-provider-controls` | Provider/商品控制与已有恢复草稿读取 | `platform.billing.wallets.read` | PLATFORM | READ_ONLY；只消费服务端已有合格证据 |
| 支付订单 sync、晚付 claim/query/schedule/archive/provider-blind、支付异常 claim | 对应单案件写动作 | `platform.billing.orders.sync` | PLATFORM | `BILLING_PAYMENT_ORDER_SYNC`、`BILLING_LATE_PAYMENT_CLAIM`、`BILLING_LATE_PAYMENT_QUERY`、`BILLING_LATE_PAYMENT_SCHEDULE`、`BILLING_LATE_PAYMENT_ARCHIVE`、`BILLING_PROVIDER_BLIND_ESCALATE`、`BILLING_PAYMENT_ANOMALY_CLAIM` 均固定 AUDIT_ONLY |
| 支付订单 evidence repair/关闭的退款端点、支付异常 resolve、调账记录 create | 对应单案件写动作 | `platform.billing.repairs.request` | PLATFORM | `BILLING_PAYMENT_ORDER_EVIDENCE_REPAIR`、`BILLING_NORMAL_REFUND`、`BILLING_PAYMENT_ANOMALY_RESOLVE`、`BILLING_ADJUSTMENT_REQUEST` 均固定 AUDIT_ONLY；退款产品入口仍关闭 |
| `/billing/adjustments/:id/approve` | 直接批准资金执行 | `platform.billing.adjustments.approve` | PLATFORM | `WALLET_ADJUSTMENT` 固定 APPROVAL_REQUIRED；Portal 不直调，使用审批请求 |
| `/billing/adjustments/:id/reject` | 拒绝待处理调账 | `platform.billing.adjustments.approve` | PLATFORM | `BILLING_ADJUSTMENT_REJECT` 固定 AUDIT_ONLY |
| `/billing/usage-holds*`、`/billing/reconciliation-runs*` | Hold、对账 run/case 读取 | `platform.usage.read` | PLATFORM | READ_ONLY |
| `/billing/usage-holds/:id/release` | 安全释放 Hold | `platform.billing.holds.release` | PLATFORM | **`BILLING_USAGE_HOLD_SAFE_RELEASE` 固定 AUDIT_ONLY**；不得在同一 API 动态提升为财务审批 |
| 财务事实强制修复 | 新建独立修复申请 | `platform.billing.repairs.request` | PLATFORM | 使用独立财务修复命令，APPROVAL_REQUIRED；不得借 Hold release 改账 |
| 对账 run/import/case command | 执行或更新对账 | `platform.usage.reconcile.retry` | PLATFORM | `BILLING_RECONCILIATION_RUN`、`BILLING_PROVIDER_BILL_IMPORT`、`BILLING_RECONCILIATION_CASE_UPDATE` 均固定 AUDIT_ONLY |
| `/billing/repairs` | 发起财务修复 | `platform.billing.repairs.request` | PLATFORM | APPROVAL_REQUIRED |
| `/billing/repairs` | 审批财务修复 | `platform.billing.repairs.approve` | PLATFORM | APPROVAL_REQUIRED；禁自审 |
| `/billing/provider-controls` | 申请 Provider 恢复 | `platform.billing.payment-controls.resume.request` | PLATFORM | APPROVAL_REQUIRED；只能消费 GET 返回的已有合格证据与 `resume_draft` |
| `/billing/provider-controls` | 批准 Provider 恢复 | `platform.billing.payment-controls.resume.approve` | PLATFORM | APPROVAL_REQUIRED；独立审批人 |
| `/billing/provider-controls/probe` | “立即探测” | 无现有端点 | PLATFORM | **NOT_AVAILABLE**：`UIA-005` 前不显示按钮、不由 Portal 直连 Provider |
| `/billing/products` | 商品列表/详情 | `platform.products.read` | PLATFORM | READ_ONLY |
| `/billing/products` | 新建草稿 | `platform.products.create` | PLATFORM | AUDIT_ONLY |
| `/billing/products/:id` | 发布、准入验证/重试/撤销 | `platform.products.publish` | PLATFORM | AUDIT_ONLY；准入证据与版本门禁 |
| `/billing/products/:id` | 下架 | `platform.products.unpublish` | PLATFORM | AUDIT_ONLY；业务下架不替代 Provider 止损 |
| `/billing/products/:id` | 删除 | `platform.products.delete` | PLATFORM | AUDIT_ONLY；仅 DRAFT/OFF_SALE |
| `/billing/products/:id` | 申请/批准商品恢复 | `platform.billing.payment-controls.resume.request` / `.approve` | PLATFORM | APPROVAL_REQUIRED；Provider ACTIVE 后另起新申请 |
| `/pricing` | 定价/计量读取 | `platform.pricing.read` | PLATFORM | READ_ONLY |
| `/pricing-versions` | 旧版价格版本历史读取 | `platform.pricing.read` | PLATFORM | READ_ONLY；旧版 `POST`、运行时命令策略与 Portal 代理入口均已移除，历史迁移记录仅作为不可变审计事实保留 |
| `/pricing/drafts`、`/pricing/:id/draft`、`/pricing/:id/publish`、`/pricing/:id/transition`、`/metering-units*` | 新版定价/计量写入 | `platform.pricing.manage` | PLATFORM | 六个 Billing 命令已由 `ADM-54 / APT-112` 按 `AUDIT_ONLY` 精确注册；`UI-44 / APT-113` 已开放权限裁剪后的 React 生命周期工作区。新建价格草稿和计量单位分别在写前重取精确价格空间与完整单位基线；模型、协议公式使用各自独立加载的当前状态，并在提交前复核权限、目录存在性和协议兼容性 |
| `/models` | 读取 | `platform.models.read` | PLATFORM | READ_ONLY |
| `/models:sync` | 同步 | `platform.models.sync` | PLATFORM | `TOKEN_MODEL_CATALOG_SYNC` 固定 AUDIT_ONLY |
| `/models` | 草稿、发布、生命周期、前向回滚 | `platform.models.manage` | PLATFORM | `TOKEN_MODEL_DRAFT_SAVE`、`TOKEN_MODEL_PUBLISH`、`TOKEN_MODEL_LIFECYCLE_CHANGE`、`TOKEN_MODEL_CATALOG_ROLLBACK` 均固定 AUDIT_ONLY |
| `/token/protocols`、`/token/formulas/:id/preview` | 协议/公式读取与预览 | `platform.pricing.read` | PLATFORM | READ_ONLY |
| `/token/protocols`、`/token/formulas` | 草稿/发布 | `platform.pricing.manage` | PLATFORM | **NOT_AVAILABLE**：路由分别声明 `TOKEN_PROTOCOL_DRAFT_CREATE`、`TOKEN_PROTOCOL_PUBLISH`、`TOKEN_FORMULA_DRAFT_CREATE`、`TOKEN_FORMULA_PUBLISH`，但当前 `approvalpolicy` 注册表缺少这些命令，实际失败关闭为 `COMMAND_POLICY_UNAVAILABLE`；见 `UIA-020` |
| `/token/accounts`、`/token/routes`、`/token/provisionings` | 读取 | `platform.tokens.read` | PLATFORM | READ_ONLY |
| 同上 | 创建、路由、轮换、修复 | `platform.tokens.manage` | PLATFORM | AUDIT_ONLY；密钥值不回显 |
| `/token/credentials/validate` | 现场凭据验证 | `platform.tokens.credentials.validate` | PLATFORM | AUDIT_ONLY；只返回结论和证据摘要 |
| `/usage/requests` | 查询 | `platform.usage.read` | PLATFORM | READ_ONLY |
| `/usage/reconciliation` | 重试 | `platform.usage.reconcile.retry` | PLATFORM | AUDIT_ONLY；结果未知先对账 |
| `/system/status` | 状态读取 | `platform.system.status.read` | PLATFORM | READ_ONLY |
| `/system/events/account` | Account Outbox 查询/重放 | `platform.events.retry`（权限码已定义） | PLATFORM；Account 服务自有库 | **NOT_AVAILABLE**：当前 Account OpenAPI 无端点；候选 `UIA-008`。Account 只能查询/重放自己的 Outbox |
| `/system/events/billing` | Billing Outbox 查询/重放 | `platform.events.retry`（权限码已定义） | PLATFORM；Billing 服务自有库 | **NOT_AVAILABLE**：当前 Billing OpenAPI 无端点；候选 `UIA-021`。Billing 只能查询/重放自己的 Outbox |
| `/system/events/token` | Token Outbox 查询/重放 | `platform.events.retry`（权限码已定义） | PLATFORM；Token 服务自有库 | **NOT_AVAILABLE**：当前 Token OpenAPI 无端点；候选 `UIA-022`。Token 只能查询/重放自己的 Outbox |
| `/system/events` | Portal 只读聚合与所属服务路由 | 每个领域卡片独立要求 `platform.events.retry` | PLATFORM；不持有跨域数据写权限 | **NOT_AVAILABLE**：当前无聚合适配；候选 `UIA-023`。Portal 不代理跨库写，重放请求仅路由到事件所属服务 |
| `/audit` | 日志查询 | `platform.audit.read` | PLATFORM | READ_ONLY，默认脱敏 |
| `/audit/exports` | 默认导出 | `platform.audit.export` | PLATFORM | AUDIT_ONLY，异步、近期认证 |
| `/audit/exports` | 含 PII 导出 | `platform.audit.export` + 临时 `platform.audit.pii.export` | PLATFORM | AUDIT_ONLY，临时授权且强化审计 |
| `/approvals/my-requests` | 申请人视图/撤回 | 冻结策略的 `requester_permission` | PLATFORM | APPROVAL_REQUIRED |
| `/approvals/inbox` | 审批人视图/决定 | 冻结策略的 `approver_permission` | PLATFORM | APPROVAL_REQUIRED；禁自审 |
| `/my-org/profile` | 组织资料读取 | `org.profile.read` | ORGANIZATION | READ_ONLY；当前由 `/me/organizations` 摘要提供 |
| `/my-org/profile` | 组织资料更新/注销申请 | `org.profile.update` / `org.delete.request`（权限码已定义） | ORGANIZATION | **NOT_AVAILABLE**：当前 OpenAPI 无端点；Account API 见 `UIA-019`，Portal 适配见 `UIA-012` |
| `/my-org/units` | 节点读取/管理 | `org.units.read` / `.manage` | ORGANIZATION 或 UNIT 子树 | 读 READ_ONLY；写 AUDIT_ONLY |
| `/my-org/members` | 成员读取/管理 | `org.members.read` / `.manage` | ORGANIZATION 或 UNIT 子树 | 读 READ_ONLY；写 AUDIT_ONLY |
| `/my-org/roles` | 角色读取/绑定 | `org.roles.read` / `.assign` | ORGANIZATION 或 UNIT 子树 | 读 READ_ONLY；写 AUDIT_ONLY |
| `/my-org/wallet` | 额度、权益购买 | `org.wallet.read` / `org.entitlements.purchase` | ORGANIZATION | 读 READ_ONLY；购买按支付合同 |
| `/my-org/quota` | 自己/全员额度 | `org.quota.read.self` / `.all` | ORGANIZATION 或 UNIT 子树 | READ_ONLY |
| `/my-org/quota` | 分配/收回 | `org.quota.allocate` / `.revoke` | ORGANIZATION 或 UNIT 子树 | AUDIT_ONLY |
| `/my-org/usage` | 自己的用量 | `org.usage.read.self` | 当前活动组织上下文 | READ_ONLY；使用 `/me/usage-requests` |
| `/my-org/usage` | 全员用量 | `org.usage.read.all`（权限码已定义） | ORGANIZATION 或 UNIT 子树 | **NOT_AVAILABLE**：当前 OpenAPI 无聚合端点；Token API 见 `UIA-016`，独立 Portal 适配见 `UIA-024` |
| `/my-org/models` | 模型读取/策略 | `org.models.read` / `.manage` | ORGANIZATION 或 UNIT 子树 | 读 READ_ONLY；写 AUDIT_ONLY |
| `/my-org/audit` | 查询/导出 | `org.audit.read` / `.export`（权限码已定义） | ORGANIZATION 或 UNIT 子树 | **NOT_AVAILABLE**：当前 OpenAPI 无组织审计端点，Portal 不显示页面或按钮；见 `UIA-009` |
| `/my-org/grants` | 注销账号遗留额度回收 | `org.grants.reclaim_closed` | ORGANIZATION | DIRECT_BUSINESS；仅调用现有单条/批量 reclaim 合同及其幂等键 |
| `/chat/completions` | 组织上下文模型调用 | `org.model.invoke` | 当前活动 ORGANIZATION/UNIT 上下文 | DIRECT_BUSINESS；继续执行路由、额度、Hold 与 Provider 全部门禁 |
| `/runbooks/bootstrap` | 状态与运行手册 | `platform.system.status.read` | PLATFORM | OUT_OF_BAND；Portal 不执行 |
| `/runbooks/break-glass` | 状态与运行手册 | `platform.audit.read` | PLATFORM | OUT_OF_BAND；Portal 不执行 |
| `/runbooks/service-identities` | 非敏感元数据与审计事实 | `platform.audit.read` | PLATFORM | READ_ONLY；当前无凭据管理端点，不展示凭据 |
| `/runbooks/idp` | 边界说明/健康状态 | `platform.system.status.read` | PLATFORM | OUT_OF_BAND；无管理链接/代理 |

#### 4.2.1 三领域事件候选合同

事件候选合同仅在产品批准后进入正式设计，且三域逐项满足以下同构合同：

1. Account、Billing、Token 分别在本服务管理前缀下提供 `GET /outbox-events`、`GET /outbox-events/{event_id}` 与 `POST /outbox-events/{event_id}/replays`，不得由 Account 代查或代写其他库。
2. GET 只返回 `event_id/event_type/status/attempt_count/occurred_at/last_attempt_at/last_error_category/request_id` 等脱敏摘要，并必须返回 `event_digest`：对不可变事件包络规范序列化结果计算的 64 位小写十六进制 SHA-256。摘要不得包含原始 payload；本域列表定义 `event_type/status/from/to/cursor/limit`。
3. POST 必须同时携带 body `expected_digest`、Header `Idempotency-Key`（16～128 字符）和 body `reason`（去除首尾空白后至少 20 字符）。`expected_digest` 必须等于最近 GET 返回的 `event_digest`。
4. 只有 `published_at IS NULL AND dead_lettered_at IS NOT NULL` 的**未发布死信**可重放；普通待发布事件、已发布事件、非死信事件以及其他领域 event ID 均失败关闭，且跨域失败不泄露事件是否存在。
5. 领域执行器使用 Serializable 单一事务：按本域 `event_id` `FOR UPDATE` 读取事件，重新计算不可变包络摘要，复核 `expected_digest` 与未发布死信状态；随后追加不可变 `event_replay_requests`，再原子清除 `dead_lettered_at`、清零尝试计数并把 `next_attempt_at` 设为当前时间重排队。摘要或状态任一变化均回滚整个事务。
6. 幂等指纹至少覆盖 `event_id + expected_digest + reason + 执行工作负载身份`。同键同请求返回原 `replay_id`；同键异事件、异摘要、异 reason 或异执行身份全部冲突失败，不追加记录、不重排队。
7. HTTP 人类调用者先由所属领域服务校验 `platform.events.retry`，记录请求者、reason、目标域和 request ID；实际事务只允许 `job_type=EVENT_REPLAY` 的短期工作负载身份执行，并在 `event_replay_requests` 及管理审计中记录 service name/job run ID。人类授权审计与工作负载执行审计是两条可关联但不可互相替代的记录。
8. Portal 只并列读取三个领域响应，并把重放动作原样路由到该事件所属端点；不持有 Outbox 写权限、不统一执行事务、不跨库。

### 4.3 Token 权限隔离硬规则

- 只有明确拥有 `platform.tokens.read` 的主体才能进入 Token 页面或看到 Token 聚合数据。
- 创建/修改 Token Account、路由、Provisioning 或凭据元数据要求 `platform.tokens.manage`。
- 现场凭据验证必须额外要求 `platform.tokens.credentials.validate`。
- `PLATFORM_SUPER_ADMIN` 在角色矩阵中显式包含上述常驻 Token 权限；Portal 仍只消费服务端返回的原子权限，不能在客户端按角色名称绕过授权。
- Token 密钥、Authorization Header、完整凭据和可复用验证材料永不显示、导出或写入审计详情。

---

## 5. 审批职责分离与执行合同

### 5.1 视图区分

| 视图 | 用户可见内容 | 允许动作 | 禁止动作 |
|---|---|---|---|
| 我的申请 | 自己创建的申请、冻结快照、当前状态、审批记录、执行结果 | 保存草稿、提交、在允许状态撤回、失败后基于最新资源新建替代申请 | 审批自己的申请、修改已提交快照、直接执行命令 |
| 待我审批 | 当前主体拥有冻结策略 `approver_permission` 且作用域匹配的申请 | 查看差异与风险、批准、拒绝 | 审批自己发起的申请、修改申请参数、代替执行服务执行 |
| 执行与失败 | 只读展示执行尝试、服务身份、幂等键、错误和重试资格 | 仅策略允许的重新执行/失效操作 | 人工编辑执行结果、伪造成功、绕过 CAS |
| 审批策略 | 当前注册策略的命令、版本、申请/审批权限、执行身份、时限 | 只读 | Portal 编辑策略或权限 |

### 5.2 精确审批策略

| 命令族 | 申请权限 | 审批权限 | 执行服务身份 | 特殊约束 |
|---|---|---|---|---|
| 钱包/权益/账本/应收/支付/退款异常/拒付/对账/用量结算等财务修复 | `platform.billing.repairs.request` | `platform.billing.repairs.approve` | `billing-command-executor` | 一名独立审批人；不能自审 |
| 钱包/权益调账（`WALLET_ADJUSTMENT`、`ENTITLEMENT_COMPONENT_ADJUSTMENT`） | `platform.billing.repairs.request` | `platform.billing.repairs.approve` | `billing-command-executor` | 以注册表冻结策略为准；`platform.billing.adjustments.approve` 仅用于已注册的拒绝类审计动作，不得作为资金执行批准权限 |
| `PAYMENT_PROVIDER_RESUME` | `platform.billing.payment-controls.resume.request` | `platform.billing.payment-controls.resume.approve` | `billing-command-executor` | Provider 独立恢复；当前只能消费 controls GET 返回的合格、未过期探测/评估证据 |
| `PAYMENT_PRODUCT_RESUME` | 同上 | 同上 | `billing-command-executor` | Provider 已 ACTIVE 后另起新申请；商品逐个恢复 |

实际页面必须从 `approval_command_policies`/审批策略接口读取冻结的 `requester_permission`、`approver_permission`、策略版本、时限和执行身份，不能维护第二份前端常量表。未知命令或策略缺失一律禁止提交和审批。

### 5.3 冻结、并发与幂等

1. 提交时冻结命令、参数、目标资源、`critical_resource_versions`、策略版本、申请人身份/权限/作用域、认证绑定、风险摘要和到期时间；字段以 ApprovalDraft OpenAPI 为准。
2. 提交后请求正文不可编辑；变更必须撤回或失效后创建新申请，并以 `supersedes_request_id` 关联。
3. 服务端禁止 `requester_actor_id == approver_actor_id`；对支付恢复，审批人还不得是相关异常确认人。
4. 审批决定当前使用审批请求 `expected_version`、reason 与 `observed_versions`；OpenAPI 未接受决定幂等键，不得宣称同键重放保证。若需显式决定幂等，列入 `UIA-011`。
5. 执行前重新校验目标资源 CAS、策略仍有效、授权未撤销、执行窗口未过期。任一失败转 `INVALIDATED` 或 `EXECUTION_FAILED`，不执行副作用。
6. 只有策略冻结的最小权限服务身份可执行；Portal 和人类管理员永远不能模拟该身份。
7. 执行结果不明时按领域进入“结果不确定”展示分类（不是统一 API 状态码），只允许使用已有查询、探测或对账接口，禁止自动重复副作用。

### 5.4 审批生命周期

`DRAFT → SUBMITTED → APPROVED → EXECUTING → EXECUTED`

- `SUBMITTED → REJECTED | WITHDRAWN | EXPIRED | INVALIDATED`
- `APPROVED → INVALIDATED`
- `EXECUTING → EXECUTION_FAILED`
- `EXECUTION_FAILED → EXECUTING | INVALIDATED`，仅策略允许且幂等/CAS 验证通过。
- 终态不可重开；后续操作必须新建替代申请。
- 提交有效期默认 24 小时、上限 72 小时；批准后的执行窗口默认 5 分钟。界面显示绝对时间和剩余时间。

---

## 6. 生命周期与动作矩阵

### 6.1 组织

| 当前状态 | 允许动作 | 目标状态/结果 | 禁止动作 |
|---|---|---|---|
| ACTIVE | 停用 | DISABLED | 直接删除 |
| DISABLED | 恢复、跨域删除预检 | ACTIVE 或保持 DISABLED | 未通过预检删除 |
| DISABLED 且预检通过 | 逻辑删除 | DELETED | 同时恢复 |
| DELETED | 只读审计/历史引用 | DELETED | **任何恢复、编辑、成员/节点变更、重新使用标识** |

组织删除确认必须展示未结余额、待处理订单/审批、成员关系、节点、审计保留和其他跨域阻断。DELETED 是终态，原稿“可恢复”文案全部删除。

### 6.2 充值商品

| 当前状态 | 允许动作 | 目标状态/结果 | 禁止动作 |
|---|---|---|---|
| DRAFT | 准入验证、发布、删除 | 验证状态变化、ON_SALE、DELETED | 直接上架而无有效准入证据 |
| ON_SALE | 下架、续接追加式准入证据 | OFF_SALE 或保持 ON_SALE | **删除、修改已发布事实** |
| OFF_SALE | 新证据验证、重新上架申请、删除 | 验证状态变化、ON_SALE、DELETED | 编辑不可变发布映射 |
| DELETED | 历史只读 | DELETED | 恢复、发布、编辑、复用目录标识 |

业务下架只控制单商品可售性，不替代支付 Provider 止损。准入证据到期或撤销持续关闭运行时可售性；补充新证据后，如果已发生关闭，仍须发起独立商品恢复审批。Provider 恢复不会自动恢复任何商品。

标识复用边界：内部 `catalog_item_id` 与 `product_id` 永不复用；商品曾进入 ON_SALE 后，其 Provider SKU/BOM 永不复用。仅从未上架的 DRAFT 在删除后，外部 SKU/BOM 才可按现行需求释放；释放不改变内部墓碑和历史审计。

### 6.3 用户、定价、Provisioning

| 对象 | 生命周期 | 关键约束 |
|---|---|---|
| 管理用户状态 | ACTIVE ↔ DISABLED | 后台不创建/删除用户；用户自助注销由独立流程产生 DELETED/ANONYMIZED，后台只处理修复 |
| 定价 | DRAFT → PUBLISHED → ACTIVE → ENDED；PUBLISHED → CANCELLED | 已发布事实不可回滚编辑或删除；修正以新版本生效 |
| Provisioning | PENDING → READY/FAILED/REPAIR_REQUIRED；FAILED → PENDING/REPAIR_REQUIRED；READY → PENDING/FAILED | 修复只创建新的 PENDING 尝试；不直接伪造 READY |

### 6.4 并发验收

- 两个管理员基于同一 `expected_version` 操作商品或审批时，仅首个合法提交成功，后一个得到该端点实际 409；组织节点移动使用真实 `expected_structure_version`。顶层组织状态/改名/删除没有客户端 CAS 字段，不纳入此验收，列入 `UIA-003`。
- **仅对 OpenAPI 明确定义幂等键/幂等语义的端点**：同一幂等键和相同规范化正文重放返回同一资源/结果，同键不同正文按该领域真实冲突码失败。无幂等合同的端点不适用此保证。
- ON_SALE 商品的删除请求无论 UI、API 或重放均返回当前 Billing 领域冲突响应（当前通用码为 `BILLING_CONFLICT`），不产生软删除记录；不得断言存在未定义的状态迁移错误码。
- DELETED 组织的恢复请求无论权限多高均失败；超级管理员与 break-glass 均不得绕过。
- 商品/审批/节点移动/定价等具备真实版本或 ETag 合同的页面遇到并发冲突时保留原因草稿，刷新事实后重新确认；无 CAS 合同的页面只按领域 409 重查，不声称比较了版本。

---

## 7. 敏感写动作的逐端点真实合同

权限和治理分类始终由服务端执行，但“近期认证、原因、幂等、`expected_version`”不是所有现有端点都具备的统一字段。Portal 只能按当前 OpenAPI 发送已经定义的字段；表中 `API_GAP（待产品决策）` 是上线阻断，不得由前端自造 Header/字段后宣称已实现。确认框和危险视觉属于 UI 约束，不等价于服务端安全合同。

| 现有端点/动作 | 当前精确权限与治理 | 当前 OpenAPI 已有合同 | 当前缺口及处理 |
|---|---|---|---|
| `POST /account/admin/v1/role-bindings` | `platform.roles.assign`，AUDIT_ONLY | body 必填 `user_id/role_code/idempotency_key`；临时 PII 授权时 `reason` 必填 | 普通绑定没有通用 reason/CAS/recent-auth；高风险绑定强化为 `API_GAP`，见 `UIA-002` |
| `DELETE /account/admin/v1/role-bindings/{binding_id}` | `platform.roles.assign`，AUDIT_ONLY | 端点语义幂等；无 request body | 无 reason、显式幂等键、版本和 recent-auth；`API_GAP`，见 `UIA-002` |
| `PATCH /account/admin/v1/users/{user_id}/status` | `platform.users.status.manage`，AUDIT_ONLY | body 仅 `status` | 无 reason、幂等、版本和 recent-auth；Portal 不虚构这些字段，强化见 `UIA-003` |
| `POST /account/admin/v1/organizations` | `platform.organizations.create`，AUDIT_ONLY | `Idempotency-Key` Header；body 可作兼容 fallback | 无 reason/CAS/recent-auth；当前按合同实现 |
| 顶层组织改名、状态 | `platform.organizations.status.manage`，`ACCOUNT_ORGANIZATION_RENAME/STATUS_CHANGE` 固定 AUDIT_ONLY | body 仅名称或状态；响应可能为 409 | OpenAPI 无幂等键、reason、`expected_version`；不能声称 CAS，强化见 `UIA-003` |
| 顶层组织删除 | `platform.organizations.delete`，`ACCOUNT_ORGANIZATION_DELETE` 固定 AUDIT_ONLY | DELETE 无 body；响应可能为 409 | OpenAPI 无幂等键、reason、`expected_version`；强化见 `UIA-003` |
| 组织节点创建/重命名/删除、成员添加/删除/角色替换 | `platform.organizations.status.manage`；`ACCOUNT_UNIT_CREATE/RENAME/DELETE`、`ACCOUNT_MEMBER_ADD/REMOVE/ROLES_REPLACE` 均固定 AUDIT_ONLY | 节点 create/rename/delete、成员 add/delete 多数支持 `Idempotency-Key` Header 或 body fallback；成员角色替换未强制幂等 | GET 树返回 version，但这些写入未接受 `expected_version`；成员角色替换也无 reason，强化见 `UIA-003` |
| 顶层组织所有权转移 | `platform.organizations.owner.transfer`，`ACCOUNT_OWNERSHIP_TRANSFER` 固定 AUDIT_ONLY | Header 或 body fallback 幂等键 | 无 reason/expected_version/recent-auth；强化见 `UIA-003` |
| `POST /account/admin/v1/audit-exports` | `platform.audit.export`，AUDIT_ONLY | body 必填 filter/reason/include_pii/idempotency_key；服务端校验 15 分钟内认证 | 当前具备；含 PII 另需临时 `platform.audit.pii.export` |
| 审计 download-ticket | `platform.audit.export`，AUDIT_ONLY | 再次校验 15 分钟内认证；票据 15 分钟、一次成功 | 当前具备；解密密钥缺口另见第 10 节与 `UIA-006` |
| 定价 draft create/update | `platform.pricing.manage`；`BILLING_PRICE_DRAFT_CREATE`、`BILLING_PRICE_DRAFT_UPDATE` 固定 AUDIT_ONLY | OpenAPI 定义 `PriceDraft.expected_etag`，PUT 定义 412 | 策略已由 `APT-112` 注册，UI 由 `APT-113` 接入；只按 draft 的 ETag 合同，不扩展并发保证 |
| 定价 publish | `platform.pricing.manage`；`BILLING_PRICE_PUBLISH` 固定 AUDIT_ONLY | OpenAPI 定义 Header `Idempotency-Key` + body `expected_etag` | 策略已由 `APT-112` 注册，UI 由 `APT-113` 接入；发布无 reason/recent-auth，按真实合同接入 |
| 定价 transition、计量单位 create/deactivate | `platform.pricing.manage`；`BILLING_PRICE_TRANSITION`、`BILLING_METERING_UNIT_CREATE`、`BILLING_METERING_UNIT_DEACTIVATE` 固定 AUDIT_ONLY | OpenAPI 已定义各自请求；transition 只有幂等键，metering 两动作当前均无幂等键/ETag | 策略已由 `APT-112` 注册，UI 由 `APT-113` 接入；不能声称不存在的 CAS 或幂等保证 |
| 协议/公式 draft/publish | `platform.pricing.manage`；当前 `COMMAND_POLICY_UNAVAILABLE` | OpenAPI 已定义各自请求；`TOKEN_PROTOCOL_DRAFT_CREATE`、`TOKEN_PROTOCOL_PUBLISH`、`TOKEN_FORMULA_DRAFT_CREATE`、`TOKEN_FORMULA_PUBLISH` 尚未注册 | `UIA-020` 前按钮不存在；注册后按 Token 领域真实冲突码处理 |
| 商品草稿创建 | `platform.products.create`，`BILLING_RECHARGE_PRODUCT_CREATE` 固定 AUDIT_ONLY | Header 必填 16～128 字符 `Idempotency-Key`；body 必填完整商品事实、`expected_version: 0` 与非空 `reason`；15 分钟内近期认证 | 当前合同完整，不列 API_GAP；缺字段为请求错误，非零版本为 `BILLING_CONFLICT`，同键异正文为 `IDEMPOTENCY_CONFLICT` |
| 商品发布、准入验证/重试/撤销 | `platform.products.publish`；`BILLING_RECHARGE_PRODUCT_PUBLISH`、`BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY`、`BILLING_RECHARGE_PRODUCT_ADMISSION_REVOKE` 均固定 AUDIT_ONLY | Header `Idempotency-Key` + body `expected_version/reason`，发布可带 `evidence_id`；15 分钟内近期认证 | 只处理该端点实际返回的 401/409 code，不抽象为全局错误码 |
| 商品下架 | `platform.products.unpublish`，`BILLING_RECHARGE_PRODUCT_UNPUBLISH` 固定 AUDIT_ONLY | Header `Idempotency-Key` + body `expected_version/reason`；15 分钟内近期认证 | 当前合同完整 |
| 商品删除 | `platform.products.delete`，`BILLING_RECHARGE_PRODUCT_DELETE` 固定 AUDIT_ONLY | Header `Idempotency-Key` + body `expected_version/reason`；15 分钟内近期认证 | 仅 DRAFT/OFF_SALE；ON_SALE 返回 `BILLING_CONFLICT` |
| 调账记录创建 | `platform.billing.repairs.request`，AUDIT_ONLY；资金执行另走审批 | body 必填 wallet/amount/reason/idempotency_key | 无资源版本；后续 approval draft 冻结版本 |
| Hold 释放 | `platform.billing.holds.release`，AUDIT_ONLY | Header `Idempotency-Key` + Decision reason | 无 expected_version/recent-auth；当前按合同实现 |
| 支付异常 claim | `platform.billing.orders.sync`，`BILLING_PAYMENT_ANOMALY_CLAIM` 固定 AUDIT_ONLY | Header `Idempotency-Key` | 无 reason/expected_version |
| 支付异常 resolve | `platform.billing.repairs.request`，`BILLING_PAYMENT_ANOMALY_RESOLVE` 固定 AUDIT_ONLY | Header `Idempotency-Key` + body resolution/reason | 无 expected_version |
| 对账 case command | `platform.usage.reconcile.retry`，`BILLING_RECONCILIATION_CASE_UPDATE` 固定 AUDIT_ONLY | Header `Idempotency-Key` + body command/reason | 无 expected_version |
| 晚付 claim/query/schedule/archive/provider-blind | `platform.billing.orders.sync`；`BILLING_LATE_PAYMENT_CLAIM`、`BILLING_LATE_PAYMENT_QUERY`、`BILLING_LATE_PAYMENT_SCHEDULE`、`BILLING_LATE_PAYMENT_ARCHIVE`、`BILLING_PROVIDER_BLIND_ESCALATE` 均固定 AUDIT_ONLY | 当前无显式幂等字段；schedule 使用 `next_review_at/reason`，provider-blind 使用 Decision `reason`，claim/query/archive 无 body | 不得显示“安全重试”；合同强化归 Billing 领域任务 |
| Token Account 创建/状态、路由写入 | `platform.tokens.manage`，AUDIT_ONLY | 创建/路由无幂等、reason、版本；状态只有 reason | 这些强化项均为 `API_GAP`；见 `UIA-004` |
| Token 凭据验证 | `platform.tokens.credentials.validate`，AUDIT_ONLY | 无 body，只返回统一脱敏结论 | 无“本次验证”幂等/原因；不得展示原始错误或秘密 |
| 用量对账重试 | `platform.usage.reconcile.retry`，AUDIT_ONLY | Header `Idempotency-Key` + reason | 当前具备；不会重新发送模型请求 |
| 模型 draft 保存 | `platform.models.manage`，AUDIT_ONLY | `expected_catalog_version/expected_draft_version` | 无幂等/reason/recent-auth；只把 catalog/draft 409 当该领域版本冲突 |
| 模型发布/生命周期/前向回滚 | `platform.models.manage`，AUDIT_ONLY | body 含领域 expected version、idempotency_key、reason | 当前具备；错误码使用模型领域错误 |
| 审批 draft/更新/submit/withdraw/decide | 冻结策略的 requester/approver 权限，APPROVAL_REQUIRED | draft 有 idempotency/reason/critical versions；更新、迁移、决定有 `expected_version`，决定另有 reason/observed_versions | 只有 draft 具备显式幂等键；submit/withdraw/decide 无幂等字段，强化见 `UIA-011`；只能依赖审批领域 CAS |

### 7.1 支付 Provider 与商品双人恢复

1. `GET /billing/admin/v1/payment-provider-controls` 当前提供分层止损事实、版本、观测缺口及 Provider/逐商品独立 `resume_draft`；Portal 只能用返回草稿建立审批申请。
2. 当前 OpenAPI **没有“立即执行现场只读探测”端点**。在 `UIA-005` 完成前不显示“立即探测”按钮，也不能用前端直连 Provider 代替。恢复草稿缺少合格/未过期探测证据时禁止提交。
3. `PAYMENT_PROVIDER_RESUME` 只恢复 Provider 控制；由有 request 权限者申请、另一名有 approve 权限且非相关异常确认人的管理员批准。
4. Provider ACTIVE 后，每个商品以 `PAYMENT_PRODUCT_RESUME` 新申请独立恢复，重新冻结商品版本、准入证据和服务端提供的探测证据。
5. 商品恢复不得批量“全开”；每个目标的状态、版本和证据单独校验。部分失败只按服务端明确结果展示，不推断未知项成功或失败。

---

## 8. 列表、详情、脱敏与批量策略

列表仅开放 OpenAPI 明确定义的参数。未提供 cursor、排序或筛选的接口不得由 Portal 假装支持；若数据量要求服务端能力，则标记 `API_GAP（待产品决策）`。通用 `Limit` 为 1～200、默认 50，但只有显式引用该参数的端点才可发送。除组织和充值商品的合同明确稳定 cursor 页外，Portal 不承诺未写入 OpenAPI 的排序稳定性。

| 真实 GET 端点/类别 | 当前参数 | cursor/排序事实 | 详情、脱敏与批量 |
|---|---|---|---|
| `/account/admin/v1/users` | `q`, `limit`, `cursor` | 按更新时间和用户 ID 稳定游标分页；`q` 是用户名/展示名模糊搜索或精确 UUID 搜索 | 当前账号生命周期不采集或维护邮箱，用户列表、详情和搜索不展示/使用邮箱；底层可选字段仅保留兼容性。完整 `user_code` 使用独立精确查询；无批量状态变更；状态筛选仍为 `API_GAP`（`UIA-007`） |
| `/account/admin/v1/user-code-lookups/{user_code}` | 完整、校验和有效的 path `user_code` | 单条精确查询；禁止前缀、模糊、自动补全和批量 | 与 users 的 `q` 搜索完全分开；404 使用无枚举文案 |
| `/account/admin/v1/roles` | 无 | 无分页/排序参数 | 内置定义只读；不得创建/编辑角色 |
| `/account/admin/v1/role-bindings` | `user_id`, `organization_id` | 无 cursor/limit/sort | 显示有效期、来源、scope；无批量授予/撤销；大列表分页为 `UIA-007` |
| `/account/admin/v1/organizations` | `q`, `limit`, `cursor`, `status` | 合同声明按更新时间和标识稳定分页 | DELETED 只读；生命周期动作单目标 |
| 管理组织 units/members | 仅 path `organization_id` | 返回完整树/集合；无分页/筛选/排序 | PII 脱敏；树 mutation 返回版本但多数写入未消费版本 |
| `/account/admin/v1/audit-logs` | `limit`, `actor`, `action`, `resource`, `result`, `from`, `to` | 无 cursor/sort | 默认脱敏；详情用 `/{audit_id}`；大结果必须异步导出，在线 cursor 为 `UIA-007` |
| registration repair queue | `limit`, `status`, `stage`, `responsibility`, `older_than_minutes`, `sla` | 无 cursor/sort | 返回强脱敏队列；逐条 retry/compensate |
| profile-change/account-closure queues | 无参数 | 无分页/筛选/排序 | 强脱敏；逐条 retry；筛选与分页为 `UIA-007` |
| wallets / wallet ledger | `limit`；ledger 另有 path wallet | 无 cursor/sort | 积分单位；无批量写 |
| receivables | `limit`, `wallet_id`, `status` | 无 cursor/sort | allocations 详情为独立 path；无批量写销 |
| pricing | `limit`；旧 pricing-versions/metering-units 无参数 | 无 cursor/sort | 已发布事实只读；Billing 筛选/分页由已批准 `NFR-55 / APT-114` 交付，Portal 适配由 `UI-45 / APT-115` 交付 |
| `/billing/admin/v1/recharge-products` | `cursor`, `limit`, `lifecycle_status`, `include_deleted` | 明确 stable cursor page；默认 50、最大 200 | 准入历史为子资源；写入逐项、无批量恢复 |
| payment orders | `limit`, `q`, `status` | 无 cursor/sort；`q` 覆盖手机号、Pinvou/Mall/交易订单号或 SKU | `/{order_id}` 详情；退款发起入口关闭 |
| refunds / adjustments / usage-holds | 各自仅 `limit` | 无 cursor/sort | Hold 有详情；无批量审批/释放 |
| late-payment reviews | `limit`, `status`, `provider_id`, `owner_id`, `entered_from/to`, `next_review_before`, `overdue` | 无 cursor/sort | 单案件详情/认领/查询/排期/归档 |
| payment anomalies | `anomaly_type`, `status`, `severity`, `owner_id`, `limit` | 无 cursor/sort | 单案件详情/认领/解决 |
| reconciliation runs/cases | runs 无参数；cases 为 path `run_id` + `limit` | 无 cursor/sort | case command 单条；未知结果不批量重放 |
| Token accounts / admin usage requests | 各自仅 `limit` | 无 cursor/sort | Token 有单条详情；用量有单条详情；秘密永不返回 |
| protocols/formulas/models | 无列表参数 | 无 cursor/sort | 当前返回版本集合/目录；不虚构筛选 |
| provisionings | `limit`, `status` | 无 cursor/sort | 单条详情含 attempt history；修复逐条 |
| approval requests | `limit`, `command_type`, `status`, `risk_class`, `requester_user_id`, `target`, `submitted_from/to`, `expires_from/to` | 无 cursor/sort | 服务端返回 `allowed_actions`；禁止批量审批；cursor 为 `UIA-007` |
| `/me/organizations`、`/me/role-bindings`、`/me/invitations` | 无参数 | 无分页/排序 | 仅当前主体；不得用于全平台枚举 |
| 组织 units/members/role-bindings | units 可传 `root_unit_id`；members 可传 `unit_id`；role-bindings 无筛选 | 无 cursor/sort | UNIT 必须显式子树；跨节点结果由服务端排除；组织审计列表不存在 |
| 组织额度/权益列表 | 依 token-gateway 现有 organization balance/packages/quota-grants 合同 | 仅端点已定义的分页字段 | 支持的 batch 仅限合同已有 quota allocate/reclaim；不能推广到其他资源 |

所有真实筛选和 cursor 状态写入 URL。无 cursor 的接口刷新时重新执行相同查询并明确“数据可能已变化”，不能生成或复用虚构 cursor。用户目录已完成向后兼容的 Account cursor 与 Portal 翻页适配；Billing 由 `NFR-55 / APT-114` 与 `UI-45 / APT-115` 交付；其余 Account、Token 服务缺口仍分别由 `UIA-007/UIA-014` 承担，Portal 对这两域的其余适配仍由缩减后的候选 `UIA-015` 承担。对应任务完成前只能使用现有上限和服务端返回顺序。

详情、脱敏与批量的统一落地边界：

- 只有存在详情 GET 的 audit log、payment order、late-payment case、payment anomaly、usage hold、Token Account、usage request、Provisioning、approval request 才提供可刷新详情路由。
- 用户完整编码查询使用专用 user-code lookup；角色、顶层组织、修复队列、定价版本、商品目录、协议/公式/模型等没有单对象 GET 时，只能展示列表项已返回字段或已有子资源，不能拼造详情请求。
- 脱敏以服务端响应为准；Portal 再做视觉截断不构成安全脱敏。注册/资料/注销修复、Token 凭据、用量请求、审计与支付证据不得展示 OpenAPI 未返回的原始载荷。
- 管理后台列表不开放批量状态变更、批量角色绑定、批量审批、批量支付/商品恢复、批量修复或批量重试。仅组织额度的 batch allocate/reclaim 可使用 token-gateway 已有合同，且不能类推到其他列表。

---

## 9. 逐领域异步状态与刷新能力

当前系统不存在统一任务服务，因此 Portal 不建立虚构的 `QUEUED/RUNNING/CANCELLED/RESULT_UNKNOWN` 总状态，也不承诺所有任务有心跳、取消、跨设备恢复或批量结果。页面直接展示领域原值，并只在存在查询端点时恢复。

| 领域 | 真实状态/响应 | 当前可刷新恢复 | 重试、取消与过期 |
|---|---|---|---|
| 审计导出 | `PENDING, GENERATING, READY, FAILED, EXPIRED, DOWNLOADED` | `GET /account/admin/v1/audit-exports/{export_id}`；只返回任务状态，不返回密钥 | 无 cancel/retry 端点；FAILED/EXPIRED 需创建新导出并计入配额 |
| 商品准入验证 | `PENDING, VERIFIED, FAILED` | 商品下的 verification history GET；创建返回 `verification_id` | 仅 FAILED 使用显式 `/retry` 创建/返回新尝试；有 `/revoke`，无 cancel |
| Token Provisioning | 资源 `PENDING, READY, FAILED, REPAIR_REQUIRED`；attempt 为 `RUNNING, SUCCEEDED, FAILED, TIMED_OUT` | list/detail GET；详情含有界 attempt history | 仅符合条件的 terminal failed provisioning 可 POST repair；无 cancel |
| 审批 | `DRAFT, SUBMITTED, APPROVED, REJECTED, WITHDRAWN, EXPIRED, INVALIDATED, EXECUTING, EXECUTED, EXECUTION_FAILED` | list/detail GET；服务端返回 `allowed_actions` | 只按 allowed_actions submit/withdraw/decide；无通用 cancel/retry，执行失败按策略处理 |
| 注册修复 | list 返回持久 operation；retry 返回 202 当前 operation 摘要 | 可回到 repair queue 查询，但 OpenAPI 未定义通用 task_id/轮询字段 | 仅逐 operation retry/compensate；不显示取消/心跳 |
| 资料变更修复 | 持久 request；retry 返回 202 摘要 | 可重查 queue，但列表没有过滤/分页参数 | 无 cancel；不虚构 attempt 进度字段 |
| 注销修复 | 持久 closure request；retry 返回 200 当前 stage | 可重查 queue | 仅 Idempotency-Key + reason 的阶段重试；无 cancel |
| 支付订单同步、对账 run、Provider 查询、模型同步 | 当前管理端点返回 200 同步结果 | 刷新相应资源/案件，而非查询“任务中心” | 超时或结果不确定时按领域证据/案件处理；不得盲重放 |
| 用量对账重试 | 返回 200 usage request 摘要 | GET usage request 详情 | 幂等重试只核对证据，不重发模型请求；无 cancel |

刷新恢复规则：只有响应返回稳定资源 ID 且存在匹配的 GET 时，Portal 才把 ID 放入 URL 并恢复。没有 GET/状态字段的操作只保留 request ID 和原页面查询入口；不得声明跨设备恢复。`UIA-010` 只实现这些现有逐领域恢复组件；统一任务中心、心跳、取消、批量进度和跨领域结果状态明确不在本轮范围。

---

## 10. 审计导出（NFR-48）

审计导出是异步敏感操作，不是表格“下载当前页”。

1. 需要 `platform.audit.export`、认证年龄 ≤15 分钟、必填原因和明确时间/资源/作用域。
2. 默认导出不可逆标识和脱敏 PII；申请包含明文 PII 时还需在有效期内的临时 `platform.audit.pii.export`，该权限不永久绑定给任何内置角色。
3. 时间范围最大 31 天、最多 100,000 行或 500 MB；每人最多 2 个并发任务、**每人每日最多 10 个**。当前实现按 UTC 日界统计；Portal 不自行换算或推断重置时刻。
4. 产物为加密且签名的包；服务端生成 15 分钟有效、一次下载票据。成功消费或过期后不可再次下载。
5. 水印包含 export ID、操作者、生成时间、环境和作用域；文件内也必须携带可验证元数据。
6. 创建、开始、完成、失败、票据签发、下载成功/失败/过期均写审计；当前无取消端点，不展示取消动作。
7. 密钥、Token、Authorization Header、模型输入输出正文永不进入导出。
8. 当前 API 只在超限时返回 `AUDIT_EXPORT_QUOTA_EXCEEDED`，没有“今日剩余数、并发占用或 reset_at”字段。Portal 在 `UIA-006` 前只显示固定上限与超限事实，不虚构剩余数或重置时间。
9. `POST /account/admin/v1/audit-exports` 的成功创建响应可包含 `decryption_key` 和 `decryption_key_format`；状态 GET 明确不返回密钥。UI 只在该次响应的受控一次性面板显示，要求操作者复制/保存确认；不得写日志、埋点、错误上报、URL、剪贴板历史提示正文、LocalStorage、SessionStorage、IndexedDB 或长期前端状态，面板关闭/路由离开即清除内存。
10. 刷新、重新登录或换设备均不能恢复解密密钥；丢失密钥只能重新创建新导出，并重新占用每日/并发配额。当前服务端对同一幂等请求的 replay 仍可能再次返回密钥，与“一次性创建响应”目标不一致，属于 `API_GAP（待产品决策）`；仅在候选 `UIA-006` 获批并转为正式任务后修正。

---

## 11. 状态、错误与反馈模型

### 11.1 页面状态

所有页面和局部卡片都必须实现：加载骨架、空状态、无权、可重试错误、不可重试错误、部分失败、领域并发冲突、离线/超时、结果不确定。上述是展示分类，不是 API 错误码。聚合页面中的一个卡片失败不能让其他已成功卡片消失；失败卡片显示服务端实际 `request_id` 和仅在安全时可用的独立重查。

### 11.2 真实错误码与展示映射

Portal 必须优先读取当前 endpoint 返回的 `code/message/request_id`。下表只列代码中已存在的稳定码；HTTP 状态相同不代表错误码相同，也不得把某领域码推广成全局码。

| 真实错误码/领域 | 界面行为 |
|---|---|
| HTTP 401（具体 code 以响应为准） | 登录失效时重新登录；不能假定存在跨服务统一认证错误码 |
| `RECENT_AUTHENTICATION_REQUIRED` | 仅审计导出和商品等实际返回该码的端点进入 OIDC step-up；成功后重新读取目标 |
| `ACCOUNT_FORBIDDEN`、`TOKEN_FORBIDDEN` 或端点实际 403 code | 显示无权，不依赖虚构的通用权限/作用域码；不泄露隐藏字段 |
| `NOT_FOUND`、`USER_NOT_FOUND`、`TOKEN_ACCOUNT_NOT_FOUND` | 使用该端点的无泄露文案；不存在跨服务统一资源不可访问码 |
| `ACCOUNT_CONFLICT`、`BILLING_CONFLICT` | 通用领域冲突；没有结构化新版本时只提示刷新，不声称一定是 CAS |
| `MODEL_CATALOG_VERSION_CONFLICT`、`MODEL_POLICY_VERSION_CONFLICT`、`ORGANIZATION_LIFECYCLE_CONFLICT` | 对对应模型/策略/组织生命周期资源展示事实已变化并重新读取 |
| HTTP 412（定价 draft/publish） | 作为 ETag 条件失败处理；只覆盖定义 412 的定价端点 |
| `IDEMPOTENCY_CONFLICT`、模型请求的 `IDEMPOTENCY_KEY_REUSED` | 仅在实际返回这些码的 Billing/Token 端点提示同键不同输入 |
| `APPROVAL_WORKFLOW_REQUIRED` | 引导创建审批申请，不调用直接写接口 |
| `APPROVAL_SELF_DECISION_FORBIDDEN` | 明确必须由另一名合资格审批人决定 |
| `APPROVAL_REQUEST_INVALIDATED`、`APPROVAL_REQUEST_EXPIRED` | 旧申请只读；基于最新事实新建，不映射成虚构短码 |
| `LAST_ACTIVE_SUPER_ADMIN`、`LAST_OWNER_REQUIRED` | 展示不变量和交接路径，不提供强制绕过 |
| `ORG_UNIT_HIERARCHY_INVALID` | 展示层级约束，刷新节点树后重新选择 |
| `AUDIT_EXPORT_QUOTA_EXCEEDED` | 显示固定配额和“当前超限”；API 未返回 reset 时间，不显示倒计时 |
| `NORMAL_REFUND_CREATION_DISABLED` | 保持退款发起入口关闭 |
| `PAYMENT_RATE_LIMITED`、`MODEL_RATE_LIMITED` | 仅当响应含 Retry-After/详情时显示真实等待时间 |
| `PAYMENT_PROVIDER_CIRCUIT_OPEN`、`PAYMENT_PROVIDER_UNAVAILABLE`、`AUTHORIZATION_UNAVAILABLE`、`APPROVAL_UNAVAILABLE` | 保留输入与 request_id，提供安全重查；不自动重复外部副作用 |
| `ROUTE_PROVISIONING_PENDING`、`ROUTE_PROVISIONING_FAILED`、`ROUTE_REPAIR_REQUIRED` | 跳转真实 Provisioning 详情并按 allowed state 显示修复 |

若服务端只返回 HTTP 状态或未登记 code，Portal 使用“认证、无权、不存在、冲突、限流、不可用、结果不确定”等内部展示分类，但这些分类不得写入验收脚本作为 API code。错误信息展示实际 code 和 request_id；不得只显示“操作失败”。生产界面不展示堆栈、SQL、密钥或原始 Provider 敏感响应。

---

## 12. 页面级规格

### 12.1 总览与身份组织

- **平台总览**：首层采用身份与组织、资金与计费、模型与 Token 三张等宽领域摘要卡，系统健康作为独立横向状态条，不再把六个不同口径的总数拆成 `4+2` 孤立指标卡。卡片按 `platform.users.read` / `platform.organizations.read`、`platform.billing.wallets.read`、`platform.tokens.read` / `platform.usage.read`、`platform.system.status.read` 独立请求、独立隐藏和局部失败；无权或失败不得渲染为 0，也不得导致其他卡片清空。删除 KYC 卡和总览“只读权限”告警。身份卡突出活跃用户并补充组织、有效成员关系和停用账号；资金卡以 `posted_microcredits - reserved_microcredits - receivable_microcredits` 计算可用积分并同时展示已入账、使用中、应收占用和钱包数；模型卡的路由健康率以 `healthy_routes / routes` 计算，不得以 Token 账号数作分母，用量请求明确标注为累计值。待办只展示当前权限内的非零事项，按异常/警告分级，点击进入对应有权工作队列；全部为零时显示明确的无待办状态。
- **用户管理**：`q` 搜索与 `user_code` 完整精确查询使用不同入口；目录每页 25 条并消费 `next_cursor`，页头总量与当前页条数分开；当前无状态筛选，在相应合同完成前不得显示；只有启用/停用等现有动作，停用确认必须说明会话立即失效并禁止当前管理员自停用；删除“新建用户”。
- **角色与权限**：以“角色目录 / 授权管理”双工作区呈现；目录表格与右侧详情按个人、平台、组织三类业务归属展示，节点管理员归入组织类，同时展示完整权限及授予方式。授权管理仍保留平台/组织/组织节点的真实作用域，使用用户/组织/节点选择器和分步确认，只提供现有普通角色的授予与撤销。角色定义只在本页集中展示，用户和组织详情仅保留自身有效访问摘要及目录深链；变更记录进入独立审计页。删除“新建角色”和任何角色定义编辑。
- **组织管理**：显示生命周期、节点、所有者和阻断摘要；DELETED 全只读且不可恢复；删除使用跨域预检。
- **组织目录**：使用状态筛选和稳定游标，不把当前页数量标成全量；创建组织按需展开，所有者通过服务端用户搜索选择。组织名称进入带 `organization_id` 的独立详情 URL，返回目录恢复搜索、筛选和页码。
- **组织详情**：不再使用详情抽屉，按概览、成员与权限、组织结构、生命周期与风险分区。成员使用紧凑分页表格，角色和节点范围仅在按需编辑面板中展开；当前账号生命周期不采集邮箱，因此组织成员和候选用户均不得展示或搜索邮箱。
- **组织删除**：ACTIVE 仅允许先停用；DISABLED 先读取删除就绪度并展示 Account/Billing/Token 阻断及处理入口，提交终态删除时输入组织名称和原因并重新强一致预检。409、503、403、404 分别展示，不得统一降级为通用失败。
- **修复队列**：注册、资料变更和注销在同一异常工作台内分队列展示；空队列折叠为紧凑状态，注册筛选仅在存在队列数据时展开；每次重试展示现有 attempt、诊断字段和结果未知处理，不虚构服务端不存在的进度。

### 12.2 财务、支付与商品

- **领域分区**：账户与计费只管理积分/权益内部账务；支付与对账只管理外部 CNY 交易、案件、对账和渠道控制；定价与商品只管理交易前版本化商业规则。模型定价归入定价与商品，支付对账归入支付与对账。
- **财务总览**：积分与 CNY 分开；卡片按钱包、账本、应收等权限裁剪。
- **订单与支付案件**：成功/失败/未知清晰分层；同步、认领、异常解决使用精确权限和单案件操作。
- **退款记录**：不显示“新建退款”；仅查询既有退款及处理失败/未知/强制异常案件。
- **调账/Hold/修复**：申请和审批视图分离；任何需要审批的动作只创建 approval request。
- **定价**：已发布版本只读；结束或取消按状态矩阵操作，不在原版本直接编辑。新版 draft/publish/transition 与计量单位写入已由 `APT-112` 注册命令并由 `APT-113` 接入 React 工作区；旧版 `POST /pricing-versions` 写入口已移除，不得冒充新版生命周期入口。无幂等合同的单位创建与停用在请求结果未知时持久锁定本次操作，必须通过权威刷新确认结果后才能继续。
- **充值商品**：草稿、准入验证、发布、下架、删除、Provider/商品恢复分别显示。ON_SALE 删除按钮始终不存在；商品恢复不与 Provider 恢复合并。

### 12.3 模型、Token 与用量

- **模型**：模型读、同步、管理三个动作独立鉴权。
- **Token Account/路由/Provisioning**：无 `platform.tokens.read` 时整组隐藏；密钥不可回显；变更动作要求 manage；凭据验证另需 credentials.validate。
- **协议/公式**：只展示已定义的计量事实和版本，不提供任意表达式在线执行；草稿/发布写动作在 `UIA-020` 前隐藏。
- **用量请求/对账**：不显示模型内容；结果未知必须查询/对账；重试使用新 attempt。

### 12.4 治理、运行手册与我的组织

- **审批**：我的申请、待我审批、执行与失败、策略注册表为四个清晰视图，完全由冻结策略权限决定。
- **审计**：在线查询默认脱敏；导出遵循第 10 节异步合同。
- **S34～S37 运行手册**：只读展示状态、前置条件、审计记录和联系渠道。公网 Portal 不执行/代理 bootstrap、break-glass、IdP 管理或服务身份凭据操作。
- **我的组织**：顶部固定组织和节点选择器；已有资料摘要、成员、节点、额度、本人用量、模型卡片分别鉴权；UNIT 绑定仅覆盖目标子树。组织审计 API 等待 `UIA-009`；资料更新/注销 API 等待 `UIA-019`，其 Portal 闭环仅由 `UIA-012` 负责；全员用量 API 等待 `UIA-016`，其 Portal 闭环仅由 `UIA-024` 负责。两类能力独立决策、独立交付，未获批入口永久隐藏。

---

## 13. 响应式、无障碍与三语

### 13.1 响应式

| 断点 | 布局要求 |
|---|---|
| 桌面 ≥1280 px | 固定侧栏 + 主内容；大表格可横向滚动，首列和操作列按需固定 |
| 中宽 768～1279 px | 可折叠侧栏；筛选器两列；详情抽屉不遮挡主要确认信息 |
| 移动 <768 px | 单列卡片/键值列表替代表格；筛选为全屏面板；危险确认全屏；不隐藏必需字段或权限提示 |

任何断点都不得因为空间不足省略作用域、状态、金额/积分单位、审批风险或不可逆警告。

### 13.2 WCAG 2.1 AA 与键盘/读屏

- 正文和控件满足 AA 对比度；状态不能只用颜色表达，必须有文本/图标和可读标签。
- 所有控件可用键盘完成，焦点顺序与视觉顺序一致；弹窗有焦点圈定、Esc 规则和返回焦点。
- 表格提供标题、列头和排序状态；分页、筛选、toast、异步进度用可读语义和 `aria-live`。
- 图标按钮有可翻译名称；复制成功、错误和冲突由读屏可感知。
- 200% 缩放不丢失功能；触控目标不小于 44×44 CSS px。

### 13.3 i18n

- 所有文案同时提供简体中文、英文、日文，不允许以中文回退替代缺失翻译。
- 日期、时区、数字、货币使用 locale 格式化，但审计和技术时间同时显示明确时区。
- 状态和错误码的机器值不翻译，展示标签翻译；搜索和复制使用原始值。
- 文案不得嵌入字符串拼接；复数、占位符和长英文/日文需做布局测试。

---

## 14. 展示规则与危险操作层级

### 14.1 标识、单位与技术字段

- 用户可见不可变编码示例：`USR-R5MF-TAAT-735X-9CD9-7Q`。该示例仅说明展示格式；不允许从编码推断内部数据库主键。
- 积分使用整数和“积分”单位，例如 `2,000 积分`；金额使用 `CNY ¥49.00`。禁止用 `¥` 表示积分。
- UUID、request ID、订单号、Provider ID、幂等键等在表格中中间截断；悬停/聚焦 Tooltip 显示完整值，并提供独立复制按钮。
- 密钥、Token 和完整凭据不适用“复制完整值”，因为它们不得进入 DOM。

### 14.2 危险层级

| 等级 | 示例 | 交互 |
|---|---|---|
| 信息 | 刷新、查看已有探测证据、复制 ID | 普通按钮，不弹确认；“立即探测”在 `UIA-005` 前不存在 |
| 可逆变更 | 停用用户、下架商品、调整非终态设置 | 二级按钮 + 影响摘要 + 原因 |
| 高风险 | 角色绑定、所有权转移、财务修复、支付恢复 | 强确认；近期认证、原因、幂等/CAS 仅在真实端点支持时提交，缺失项按第 7 节阻断/任务处理；需要时进入审批 |
| 不可逆 | 删除组织/商品、一次性 PII 导出下载 | 红色危险区、输入目标编码确认、显示不可逆后果；仍不得绕过状态机 |

危险按钮不与主要只读动作并排使用相同视觉权重；终态禁止动作不显示，而不是显示可点击后再报错。

---

## 15. 需求—路由—动作—权限—API—任务—验收追踪矩阵

下表中的 API 为当前管理合同族；实现必须以 OpenAPI 实际 method/path 为准，不能由页面自行补造。

| 需求/主题 | Portal 路由与动作 | 权限 | API 合同 | 任务 | 核心验收 |
|---|---|---|---|---|---|
| ADM-01 Bootstrap | `/runbooks/bootstrap` 只读 | `platform.system.status.read` | 系统状态/审计读接口；无公网执行 API | APT-007 | BOOTSTRAP_PENDING 无登录会话；Portal 无执行按钮 |
| ADM-02/ADM-08 运维边界 | `/runbooks/break-glass`、`/runbooks/idp` 只读 | 系统/审计读权 | 无公网 break-glass/IdP 管理 API | APT-007、APT-077 | 只能按受控终端/受限网络运行手册执行 |
| 角色绑定 | `/role-bindings` 授予/撤销 | `platform.roles.assign` | `GET/POST /account/admin/v1/role-bindings`；`DELETE .../{binding_id}` | APT-006、APT-046、APT-069 | 不存在角色创建；超级管理员的完整常驻权限来自服务端角色真值表 |
| 用户状态/修复 | `/users`、修复队列 | `platform.users.read`、`platform.users.status.manage`、`platform.registrations.repair`、`platform.profile-changes.repair`、`platform.account-closures.read/repair` | users/status、registration/profile/closure repair APIs | 注册 APT-003；资料 APT-004；注销 APT-014 | 无后台新建/删除；仅按各端点真实重试合同恢复 |
| 组织生命周期 | `/organizations` | `platform.organizations.*` | `/account/admin/v1/organizations` 及 status/delete/ownership | 顶层组织 APT-009；节点 APT-010 | DELETED 不可恢复；删除先停用和跨域预检 |
| 审批框架 | `/approvals/*` | 冻结 policy 精确权限 | approval-policies/approval-requests/actions | APT-008、APT-074、APT-082 | 禁自审、冻结快照、CAS/幂等、服务身份执行 |
| 服务身份 | 审批执行只读 | 无人类执行权限 | 审批执行内部合同 | APT-047 | 只能最小服务身份执行；环境凭据门禁通过 |
| 支付恢复 | Provider/商品控制 | payment-controls resume request/approve | payment-provider-controls GET + approval APIs；现场探测端点缺失 | APT-065、APT-067、APT-093；缺口 UIA-005 | 两类独立审批；Provider 不自动开商品；无探测证据不提交 |
| 充值商品 | `/billing/products` | `platform.products.read/create/publish/unpublish/delete` | recharge-products、publish/unpublish/delete、admission-verifications | APT-083、APT-084、APT-085、APT-093 | ON_SALE 不可删除；证据失效关闭；恢复另审批 |
| 定价/应收/钱包/订单 | 财务各页 | `platform.pricing.read/manage`、`platform.billing.wallets.read`、`platform.billing.ledger.read`、`platform.billing.orders.sync`、`platform.billing.repairs.request/approve` | Billing admin APIs | APT-018、APT-019、APT-020、APT-023、APT-024、APT-025；Billing 命令注册 APT-112；生命周期 UI APT-113 | 单位准确；审批动作不走直接接口；新版定价写入按权限、精确空间 ETag、真实幂等合同和未知结果恢复规则开放 |
| 退款关闭 | `/billing/refunds` 只读及异常案件 | 对应财务读/修复权 | existing refund/admin repair APIs | APT-056 | 无退款发起入口或 API 调用 |
| Token 隔离 | `/token/*` | `platform.tokens.*` | token admin overview/accounts/routes/validate/provisionings | APT-028、APT-031、APT-073 | 无权限页面/字段/卡片均不可见；密钥不回显 |
| 用量对账 | `/usage/*` | `platform.usage.*` | usage requests/reconciliation APIs | APT-030 | 结果未知先查/对账，不盲重试 |
| 系统状态 | `/system/status` | `platform.system.status.read` | `GET /system/status` | APT-033 | 只读探针；APT-033 不承担事件重试 |
| Account 事件查询/重放（预留） | 不显示 | `platform.events.retry` | 当前无 Account OpenAPI | 候选 UIA-008；无正式任务状态 | 产品批准并完成 Account 自有端点前无入口，禁止查询/写入其他服务库 |
| Billing 事件查询/重放（预留） | 不显示 | `platform.events.retry` | 当前无 Billing OpenAPI | 候选 UIA-021；无正式任务状态 | 产品批准并完成 Billing 自有端点前无入口，禁止查询/写入其他服务库 |
| Token 事件查询/重放（预留） | 不显示 | `platform.events.retry` | 当前无 Token OpenAPI | 候选 UIA-022；无正式任务状态 | 产品批准并完成 Token 自有端点前无入口，禁止查询/写入其他服务库 |
| 事件 Portal 聚合（预留） | 不显示 | 每个卡片独立校验 `platform.events.retry` | 当前无 Portal 聚合适配 | 候选 UIA-023；无正式任务状态 | 只读聚合和所属服务路由；不实现跨库写或通用事件执行器 |
| 组织审计（预留） | 不显示 | `org.audit.read/export` | 当前无 OpenAPI | 候选 UIA-009；无正式任务状态 | 产品未批准或正式任务未完成时永久隐藏 |
| 组织资料写入/注销（预留） | 不显示 | `org.profile.update`、`org.delete.request` | 当前无 Account OpenAPI | 候选 Account API `UIA-019`、Portal `UIA-012`；无正式任务状态 | 产品未批准或两个正式任务未完成时仅资料摘要只读 |
| 组织全员用量（预留） | 不显示 | `org.usage.read.all` | 当前无 Token OpenAPI | 候选 Token API `UIA-016`、独立 Portal `UIA-024`；无正式任务状态 | 产品未批准或两个正式任务未完成时仅本人用量；不依赖资料/注销能力 |
| ADM-09/NFR-48 审计导出 | `/audit/exports` | audit.export + 可选临时 pii.export | audit-exports/status/download-tickets/download | APT-055；密钥/配额缺口 UIA-006 | 10/日、2 并发、一次下载、15 分钟、加密签名水印；密钥只显示一次 |
| 38 页后台改版 | S1～S38 | 本文逐页矩阵 | 仅调用已存在 API；缺口隐藏 | 正式 UI 需求 UI-28～UI-31；无产品代码集成验收 UI-32 | UI-28 拥有壳层/共享组件；UI-29/30/31 分域独立交付完整质量；UI-32 只做四项完成后的总验收 |
| 组织管理治理闭环 | `/organizations`、`/organizations/:id` | `platform.organizations.*` 精确权限 | 组织游标、删除就绪度、成员游标、节点移动及既有生命周期 API | APT-099～APT-104 | 文档先行、每个功能项独立提交；不得用超级管理员身份绕过生命周期和最小权限 |

---

## 16. 38 个画板逐页修改清单

以下清单是对原 PDF 的强制改版说明。保留原画板视觉语言时，也必须执行这些行为修订。

| 画板 | 页面 | 必须修改 |
|---|---|---|
| S1 | 信息架构 | 导航改为权限驱动；新增明确的“我的申请/待我审批”分离；运行手册只读分组；无权组完全隐藏 |
| S2 | 平台总览 | 三张领域摘要卡 + 独立系统健康条；删除 KYC 和只读告警；统计卡片独立鉴权及局部失败；无权/失败不显示 0；可用积分扣除应收占用；路由健康分母为全部路由；累计值显式标注；非零待办可进入有权队列；Token 卡不因超级管理员身份出现 |
| S3 | 用户管理 | 删除“新建用户”；将 `q` 搜索与 `user_code` 完整精确查询分开；手机号脱敏；状态筛选在 `UIA-007/UIA-015` 前不显示 |
| S4 | 角色管理 | 删除“新建角色”和虚构权限；改为内置角色只读 + 绑定授予/撤销；明确超级管理员具备全部常驻平台管理权限，临时 PII 导出继续独立授权 |
| S5 | Token Account | 页面整体要求 `platform.tokens.read`；写动作要求 manage，凭据验证另鉴权；密钥永不回显 |
| S6 | 组织列表 | 删除“DELETED 可恢复”；补 ACTIVE/DISABLED/DELETED 状态动作矩阵和删除预检；顶层写入 CAS 等待 `UIA-003` |
| S7 | 账务总览 | 积分/CNY 分栏；卡片字段独立权限；补空/错/无权/部分失败状态 |
| S8 | 支付复核 | 明确案件只读、认领、解决、Provider/商品恢复的不同权限和治理模式；结果未知不重复写 |
| S9 | 待审批 | 只显示拥有冻结 `approver_permission` 的申请；显示禁自审、到期、快照、版本和作用域 |
| S10 | 注册修复 | 只用现有 status/stage/responsibility/older_than/sla/limit；展示响应已有 operation 字段；禁止虚构 attempt、cursor 或批量重试 |
| S11 | 平台设置 | 删除 2FA、邮箱恢复等无任务能力；语言必须含简中/英文/日文；仅保留有 API/任务的设置 |
| S12 | 路由管理 | 使用 tokens.read/manage 分权；技术字段截断/复制/Tooltip；现有路由写没有近期认证、原因、幂等/CAS，强化等待 `UIA-004` |
| S13 | 系统状态 | 仅使用 `GET /system/status`；卡片独立失败。事件能力未获产品批准时永久隐藏；获批后须由 Account/Billing/Token 各自端点（候选 UIA-008、UIA-021、UIA-022）及 Portal 适配（候选 UIA-023）共同交付，Portal 不跨库重放 |
| S14 | 我的组织 | 增加组织/节点选择器；已有卡片独立鉴权；UNIT 只覆盖目标子树；组织审计 API 等待 `UIA-009`；资料写/注销由 Account API `UIA-019` + Portal `UIA-012` 闭环；全员用量由 Token API `UIA-016` + Portal `UIA-024` 闭环。两条链互不依赖，任一未批准时只永久隐藏本链入口 |
| S15 | 定价与商品 | ON_SALE 删除按钮移除；显示准入证据、发布事实不可变、Provider/商品独立恢复；新版定价与计量工作区已按 APT-112、APT-113 开放，并保持各资源独立加载、局部失败与刷新恢复 |
| S16 | 审计 | 导出额度为“每人每日 10 个”；异步导出、默认脱敏、临时 PII 权限、一次下载/过期/水印/加密签名；密钥一次性面板与 replay 修复依赖 `UIA-006` |
| S17 | 应收 | 当前仅 wallet_id/status/limit；时间/cursor 由 APT-114、APT-115 交付；allocations 详情只读，写销/修复进入精确审批策略 |
| S18 | 手工调账 | 创建记录使用 reason/idempotency_key；资金执行只走审批；`expected_version` 只用于后续审批请求而非调账创建端点 |
| S19 | Hold | 释放按精确权限和真实 Idempotency-Key/reason；详情版本不等于 release CAS；不批量释放 |
| S20 | 订单 | 当前仅 q/status/limit，q 支持合同列出的订单/交易/SKU/手机号；Provider/时间/cursor 由 APT-114、APT-115 交付；详情脱敏 |
| S21 | 退款 | 删除“发起退款”；保留既有记录与失败/不确定/强制异常案件处理入口 |
| S22 | 对账 | 当前 reconciliation run 返回同步 200，不显示异步任务状态；按 run/case 真实资源展示，不自动重放外部副作用 |
| S23 | 账务修复 | 所有财务修复只创建审批申请；使用 requester/approver 精确权限 |
| S24 | 协议 | 已发布协议/版本只读；无 Token 权限不显示关联敏感字段；不提供任意编辑执行；草稿/发布在 `UIA-020` 前隐藏 |
| S25 | 公式 | 展示不可变版本和单位；不在 Portal 执行任意表达式；积分/CNY 类型明确；草稿/发布在 `UIA-020` 前隐藏 |
| S26 | 凭据元数据 | 只显示不可复用元数据；验证需 credentials.validate；禁止复制密钥/Token |
| S27 | Provisioning | 按 PENDING/READY/FAILED/REPAIR_REQUIRED 状态矩阵；修复创建新 PENDING attempt |
| S28 | 用量请求 | 不显示模型正文；当前列表仅 limit，模型/状态/时间筛选等待 `UIA-014/UIA-015`；详情按 usage_request_id 脱敏 |
| S29 | 用量对账 | 重试需 usage.reconcile.retry、Idempotency-Key 和 reason；返回更新后的 usage request，不虚构新 attempt ID；不确定时查询/对账 |
| S30 | 注销修复 | 区分自助注销终态和修复 attempt；后台不恢复已匿名化身份；补幂等/审计 |
| S31 | 审批策略注册表 | 只读展示命令、版本、申请/审批权限、执行身份、时限；Portal 不编辑策略 |
| S32 | 审批执行/失败 | 显示服务身份、CAS、幂等、执行窗口和失败可重试性；人类不得直接执行 |
| S33 | 我的申请 | 仅申请人视图；显示 DRAFT/SUBMITTED 等全状态、撤回条件和替代申请关系 |
| S34 | Bootstrap | 删除“开始激活”；只显示状态、受控终端运行手册、审计和联系人；公网不代理 |
| S35 | Break-glass | 不展示可复制执行命令/秘密；只显示离线受控终端边界、最近演练和审计记录 |
| S36 | 服务身份 | 只显示非敏感元数据和健康；不显示凭据值、不允许 Portal 冒充/轮换，执行由受控环境管理 |
| S37 | IdP | 不提供管理链接、反向代理或凭据；只显示健康与“受限网络/VPN/SSH 隧道”运行手册 |
| S38 | 组织节点树 | 节点权限与 UNIT 子树一致；拖拽移动使用真实 `expected_structure_version`；新增/重命名/删除只按现有合同，CAS 强化等待 `UIA-003`；越界目标不可选 |

---

## 17. 验收矩阵

| 编号 | 验收场景 | 预期结果 |
|---|---|---|
| AC-01 | 仅 `PLATFORM_SUPER_ADMIN` 登录并访问 Token 路由/卡片 | 导航、Token Account、路由和 Provisioning 正常显示并可按原子权限管理；凭据仅返回脱敏元数据，秘密和原始错误不回显 |
| AC-02 | 用户/角色页检查创建能力 | 无“新建用户/角色”控件，OpenAPI 无对应调用 |
| AC-03 | 申请人尝试审批自己提交的财务修复 | 服务端返回 `APPROVAL_SELF_DECISION_FORBIDDEN`；审批记录无变化 |
| AC-04 | 审批提交后资源版本改变 | 执行前 CAS 失败，申请 INVALIDATED；不产生副作用 |
| AC-05 | 两人并发对同一商品执行带相同 `expected_version` 的命令，或并发迁移同一审批 | 仅首个成功；后一个返回实际 409，刷新后重新确认。顶层组织不纳入 CAS 验收 |
| AC-06 | 删除 ON_SALE 商品 | UI 无按钮；API 返回当前 `BILLING_CONFLICT`；历史订单不受影响 |
| AC-07 | Provider 故障恢复 | `UIA-005` 前无“立即探测”按钮；完成后只使用服务端只读探测证据，独立申请与审批后仅 Provider ACTIVE，商品仍关闭 |
| AC-08 | 商品恢复 | Provider ACTIVE 后另起申请；每商品独立审批/版本/证据校验 |
| AC-09 | UNIT 管理员伪造兄弟节点 ID | 服务端返回该端点实际 403；导航、列表和详情均不泄露兄弟节点；不要求统一作用域错误码 |
| AC-10 | 审计导出第 11 次/日 | 返回 `AUDIT_EXPORT_QUOTA_EXCEEDED`；在 API 提供额度元数据前不显示 reset 时间 |
| AC-11 | 默认导出与临时 PII 导出 | 默认不可逆标识/脱敏；无有效临时权限时 PII 请求被拒绝 |
| AC-12 | 下载票据重复使用或 15 分钟后使用 | 第二次及过期下载失败并写审计 |
| AC-13 | 审计导出、准入验证、Provisioning、审批分别刷新 | 只通过各领域现有资源 ID/GET 恢复真实状态；无 GET 的操作不声称恢复，不建立虚构 task ID |
| AC-14 | 对明确支持幂等的商品、审计导出、用量重试等端点重放 | 相同输入返回原结果；不同输入使用该领域实际冲突码；不推广到无幂等合同端点 |
| AC-15 | 商品/审批 `expected_version`、节点移动 `expected_structure_version`、定价 ETag 冲突 | 分别处理实际 409/412，刷新并重新确认；顶层组织、Token 路由等无 CAS 端点不纳入 |
| AC-16 | 组件局部无权/失败 | 其他有权且成功卡片继续显示；无权字段不请求/不渲染 |
| AC-17 | S34～S37 公网检查 | 无 bootstrap/break-glass/IdP/服务身份凭据执行、代理或外链管理入口 |
| AC-18 | 三语切换 | 所有页面、状态、错误、确认框有简中/英文/日文，无单语回退 |
| AC-19 | 键盘和读屏 | 全流程可键盘完成；焦点、表格、错误、toast、进度均可感知 |
| AC-20 | 320px、768px、1440px 与 200% 缩放 | 无功能丢失、遮挡或横向页面溢出；必要表格容器可滚动 |
| AC-36 | 对 ACTIVE 组织点击危险操作 | 不提供可执行删除；先停用后才可查询删除就绪度 |
| AC-37 | DISABLED 组织存在余额、邀请、审批、路由或未终态用量 | 就绪度按领域显示稳定阻断码和处理入口；删除保持未发生 |
| AC-38 | 生命周期依赖不可用 | 显示“生命周期检查暂时不可用”和 request_id，可安全重试；不显示为未知通用失败 |
| AC-39 | 直接访问或刷新带 organization_id 的详情 URL | 恢复同一组织及详情分区；无权或不存在时不泄露对象并可返回目录 |
| AC-40 | 组织数超过一页并往返详情 | 目录稳定翻页，返回时恢复查询、状态和页码；标题只声明本页数量 |
| AC-41 | 用户数超过 25 后创建组织或添加成员 | 可通过服务端用户搜索选择后续用户，不依赖用户目录首屏数据 |
| AC-42 | 组织成员和节点数量同时增长 | 成员列表服务端分页；每行不预渲染全部角色和全部节点复选框，编辑面板按需加载 |
| AC-43 | 仅持有组织状态管理权限 | 只能启停组织，不能改名、增删成员、改角色或修改节点结构；服务端直接调用同样拒绝 |
| AC-44 | 平台结构管理员移动节点 | 使用真实 expected_structure_version；跨组织、自身、后代、停用父节点和旧版本全部失败关闭 |
| AC-21 | user_code、积分、CNY、技术 ID 展示 | 编码不可变且可复制；积分无 ¥；金额带 CNY；技术 ID 可访问 Tooltip/复制 |
| AC-22 | 退款页 | 无退款发起入口；既有记录和允许的异常案件仍可查询/处理 |
| AC-23 | 删除商品后的标识复用 | `catalog_item_id/product_id` 永不复用；曾进入 ON_SALE 的商品，其 Provider SKU/BOM 永不复用；从未上架的 DRAFT 删除后，外部 SKU/BOM 可按现行需求释放 |
| AC-24 | Portal 篡改角色名、权限 UI 或隐藏字段 | 服务端仍按原子权限/作用域拒绝，未授权字段不在响应中 |
| AC-25 | 最后一个有效超级管理员尝试自停用、自注销或自撤权 | 服务端稳定拒绝；break-glass 不能绕过，且审计完整 |
| AC-26 | 仅 `PLATFORM_IDENTITY_ADMIN` 直调角色绑定写接口 | `POST /account/admin/v1/role-bindings` 和 `DELETE .../{binding_id}` 均返回 403；GET 角色/绑定仍可读 |
| AC-27 | 创建审计导出后刷新、丢失密钥、重放创建请求 | UI 不持久化密钥且刷新无法恢复；`UIA-006` 完成后 replay 不再返回密钥；丢失只能新建并计入配额 |
| AC-28 | 检查事件重试、组织审计、组织资料写/注销及全员用量入口 | 未获得产品批准的能力永久无路由、按钮或代理请求；获批能力仅在对应正式任务全部 DONE 后开放；权限码存在不等于能力存在 |
| AC-31 | 三领域事件 GET/POST 合同 | 每个 GET 返回不可变包络 64 位小写 SHA-256 `event_digest`；每个 POST 缺 `expected_digest`、16～128 字符 `Idempotency-Key` 或去空白后不足 20 字符 reason 均失败且无副作用 |
| AC-32 | 事件摘要或状态并发变化 | 摘要变化、普通待发布、非死信、已发布或已被另一请求重排队的事件均在 Serializable 事务复核时失败；`event_replay_requests` 与 Outbox 重排队要么同时提交、要么同时回滚 |
| AC-33 | 事件跨域、身份与幂等隔离 | 跨域 event ID 无存在性泄露且不能写库；人类 `platform.events.retry` 授权审计与 `EVENT_REPLAY` 工作负载执行审计均存在并可关联；同键同请求返回原 replay ID，同键异事件/摘要/reason/执行身份冲突失败；Portal 仅路由所属域 |
| AC-34 | 仅批准组织资料更新/注销 | 只创建 UIA-019、UIA-012 对应的正式任务；资料更新/注销闭环可用，全员用量入口永久隐藏；任务图不引用 UIA-016/UIA-024 对应任务且无缺失依赖 |
| AC-35 | 仅批准组织全员用量 | 只创建 UIA-016、UIA-024 对应的正式任务；全员/节点子树用量闭环可用，资料更新/注销入口永久隐藏；任务图不引用 UIA-019/UIA-012 对应任务且无缺失依赖 |
| AC-29 | 创建充值商品草稿的字段、版本与幂等检查 | 缺任一必填商品字段或 reason 返回请求错误；`expected_version != 0` 返回 `BILLING_CONFLICT`；相同 Header 幂等键同正文重放原结果、异正文返回 `IDEMPOTENCY_CONFLICT` |
| AC-30 | 调用新版定价、计量单位、协议或公式写端点 | Billing 六命令在 APT-112 前、Token 四命令在候选 UIA-020 获批并完成前，真实响应均为 `COMMAND_POLICY_UNAVAILABLE` 且 Portal 无按钮；完成后每个命令从注册表解析为精确权限和 AUDIT_ONLY，并通过路由合同测试 |

---

## 18. 开放问题与实现门禁

### 18.1 已批准转换追踪

| 原候选 | 已批准的正式 UI 需求 | 批准边界 | 建立正式任务前的门禁 |
|---|---|---|---|
| `UIA-001` | `UI-28`、`UI-29`、`UI-30`、`UI-31` | 仅基于现有 API 的后台壳层、共享组件和三组领域页面改版；正式编号与目标以 `requirements.md` 为准，第 1.3 节定义详细验收 | 独立复审通过后先同步需求状态矩阵；随后先创建 UI-28 正式任务，再创建分别依赖 UI-28 的 UI-29/30/31 正式任务 |
| `UIA-018` | `UI-32` | 仅批准 UI-28～31 的无产品代码集成与发布验收，不自动包含任何候选 API 或未批准能力 | 仅当状态矩阵已同步、且 UI-28～31 对应正式任务均已存在时才能建任务；正式依赖仅为 UI-28～31，交付不得修改产品代码 |
| `UIA-013` | `NFR-55 / APT-114` | Billing 高数据量列表合同已全量批准并转换；15 分钟有界服务端 ID 快照、筛选、排序、容量和滥用门禁以财务重构设计为准 | 已进入正式任务；原 UIA-013 不再出现在候选表或候选依赖中 |
| `UIA-015` Billing 子集 | `UI-45 / APT-115` | 仅 Billing 列表的 Portal cursor、URL 和返回恢复已批准；Account/Token 子集仍未批准 | APT-115 只依赖正式 Billing 合同；缩减后的 UIA-015 不得再次包含 Billing |
| `UIA-020` Billing 六命令 | `ADM-54 / APT-112` | 仅六个 Billing 定价/计量命令策略注册已批准；四个 Token 协议/公式命令仍未批准 | APT-112 只注册 Billing 六命令；缩减后的 UIA-020 不得再次包含 Billing |

### 18.2 未批准候选总则

以下总则和候选表只适用于 `UIA-002`～`UIA-012`、`UIA-014`、缩减为 Account/Token 子集的 `UIA-015`、`UIA-016`～`UIA-017`、`UIA-019`、缩减为 Token 四命令的 `UIA-020` 以及 `UIA-021`～`UIA-024`。`UIA-001`、`UIA-013`、`UIA-018` 已全量转换，`UIA-015/UIA-020` 的 Billing 子集已转换，只存在于上方追踪表，不再属于候选项。候选编号不是正式任务 ID，因而没有任务状态，也不构成 `implementation-tasks.md` 中的依赖边。产品决策必须先发生：

1. 产品批准某一建议项后，先在唯一需求源 `requirements.md` 中形成正式编号和目标；本 UI/UX 专项或独立需求变更文档只补充详细交互与验收。随后必须通过独立文档步骤同步 `implementation-status.md` 的需求状态矩阵，矩阵完成前不得在 `implementation-tasks.md` 建任务；建成的新任务初始状态只能是 `PENDING`，随后只使用 `PENDING / IN_PROGRESS / DONE / BLOCKED_EXTERNAL` 四种正式状态。
2. 正式任务只引用届时真实存在的正式任务 ID；本表的 UIA 引用仅表示**候选纳入条件**，转换时必须解析为已创建的正式任务，不得把 UIA 编号直接写入正式依赖。
3. 产品未批准的建议项不创建任务、不进入状态统计、不成为汇总任务依赖；只在需求决策记录中保存“未纳入”结论，并保留对应入口永久隐藏的验收事实。
4. 汇总候选项在正式纳入时动态收集“已经产品批准且已创建”的正式任务；未批准项通过永久隐藏验收收敛，不得创造四种正式任务状态之外的“拒绝/取消任务”状态。

| 候选建议项 | 唯一目标需求 | 候选纳入条件（非正式依赖） | 单一交付边界 | 可独立验收 |
|---|---|---|---|---|
| `UIA-002` | `UIR-002` 角色绑定写合同强化 | APT-006、APT-046、APT-069 | **Account 单提交**：OpenAPI、服务、迁移（如需）、合同/权限测试 | reason/recent-auth/幂等/CAS 取舍固化；Identity Admin 对 binding POST/DELETE 均 403；不改 Portal |
| `UIA-003` | `UIR-003` 用户状态与组织/成员/节点写合同强化 | APT-006、APT-009、APT-010、APT-011、APT-046、APT-069、APT-075 | **Account 单提交**：相关 OpenAPI、服务和并发/审计测试 | 每个端点分别定义 reason/幂等/CAS；旧客户端兼容；无跨节点越权；不改 Portal |
| `UIA-004` | `UIR-004` Token Account/状态/路由写合同强化 | APT-006、APT-028、APT-031、APT-069、APT-073、APT-075 | **Token 单提交**：OpenAPI、服务和秘密/并发测试 | 幂等、reason、版本与必要 step-up 有明确合同；秘密不回显；不改 Portal |
| `UIA-005` | `UIR-005` 支付恢复只读现场探测 API | APT-008、APT-067、APT-092、APT-093 | **Billing 单提交**：OpenAPI、探测服务、证据存储和无收费副作用测试 | 失败/过期/未知证据不能生成可提交草稿；Provider/商品证据隔离；不改 Portal |
| `UIA-006` | `UIR-006` 审计导出一次性密钥与配额可观察合同 | APT-006、APT-008、APT-055 | **Account 单提交**：OpenAPI、导出服务和安全测试 | create 首次响应后 replay/GET 不返密钥；丢失需新建；配额字段若不提供则合同明确；不改 Portal |
| `UIA-007` | `UIR-007` Account 其余高数据量列表合同 | APT-003、APT-004、APT-009、APT-011、APT-014、APT-046、APT-055、APT-074、APT-075 | **Account 单提交**：仅尚未覆盖的 Account 列表 OpenAPI、查询实现和分页测试 | users 的 q 与 user_code 已分离且用户目录 cursor 已实现；继续补齐角色绑定及修复队列等剩余 Account cursor/筛选/稳定排序，不改 Billing、Token、Portal |
| `UIA-008` | `UIR-008` Account Outbox 查询/重放 API | 产品批准；APT-006、APT-045、APT-069、APT-076 | **Account 单提交**：仅 Account OpenAPI、Account 自有 Outbox/`event_replay_requests`、授权委托与测试 | 完整实现 §4.2.1：GET 返回不可变包络 `event_digest`；POST 强制 `expected_digest`、16～128 字符幂等键、至少 20 字符 reason；仅未发布死信在 Serializable 事务中追加请求并重排队；人类授权与 `EVENT_REPLAY` 身份分别审计；摘要变化、非死信、已发布、跨域 ID、同键异请求均失败关闭 |
| `UIA-009` | `UIR-009` 组织审计查询/导出 API | 产品批准；APT-045、APT-069、APT-075；若含导出再依赖 APT-055 | **Account 单提交**：组织审计 OpenAPI、scope 查询、脱敏/导出测试 | UNIT 子树不可越权；导出继承 NFR-48；不改 Portal |
| `UIA-010` | `UIR-010` 现有异步领域的 Portal 刷新恢复 | 正式 UI 需求/任务 UI-28、UI-29、UI-30、UI-31；APT-003、APT-004、APT-014、APT-028、APT-055、APT-074、APT-083、APT-084 | **Portal 单提交**：仅逐领域状态组件、URL 恢复和前端测试 | 只恢复审计导出/准入/Provisioning/审批等已有 GET；无统一任务服务、心跳或取消 |
| `UIA-011` | `UIR-011` 审批 submit/withdraw/decide 显式幂等 | APT-008、APT-074、APT-082 | **Account 单提交**：Approval OpenAPI、服务和重放/并发测试 | 同键同参返回原决定，同键异参冲突；保留 expected_version；不改 Portal |
| `UIA-012` | `UIR-012` 我的组织资料更新/注销 Portal 闭环 | 产品单独批准资料/注销；APT-009、APT-014、APT-069、APT-075；唯一候选任务依赖：UIA-019 | **Portal 单提交**：只接入已完成的 Account 组织资料更新/注销合同 | `org.profile.update` 与 `org.delete.request` 独立鉴权；注销阻断完整；不读取全员用量、不依赖 UIA-016/UIA-024、不改 Account API |
| `UIA-014` | `UIR-014` Token 高数据量列表合同 | APT-028、APT-030、APT-031、APT-081 | **Token 单提交**：仅 Token 列表 OpenAPI、查询实现和分页测试 | Token Account/用量/Provisioning 的 cursor、筛选、排序稳定；不改 Account、Billing、Portal |
| `UIA-015` | `UIR-015` Account/Token 其余列表 Portal 适配 | 正式 UI 需求/任务 UI-28、UI-29、UI-30、UI-31；候选条件仅 UIA-007、UIA-014 | **Portal 单提交**：用户目录之外的 Account/Token 列表 URL、cursor、筛选、排序和前端合同测试 | Billing 部分已转换为 UI-45/APT-115；继续保证 Account/Token 参数不串用，旧无 cursor 响应安全降级；不改服务 |
| `UIA-016` | `UIR-016` 组织全员/节点子树用量聚合 API | 产品批准；APT-030、APT-069、APT-075、APT-081 | **Token 单提交**：组织用量 OpenAPI、查询、UNIT scope 与脱敏测试 | `org.usage.read.all` 才可读；UNIT 仅目标子树；不返回模型正文；不改 Account/Portal |
| `UIA-017` | `UIR-017` 已批准 API gap 的 Portal 适配（不含列表、我的组织和事件专项） | 正式 UI 需求/任务 UI-28、UI-29、UI-30、UI-31；以及 UIA-002、UIA-003、UIA-004、UIA-005、UIA-006、UIA-009、UIA-011、UIA-020 中产品已批准且正式任务已创建的子集 | **Portal 单提交**：只适配正式任务已完成的角色/写合同、探测、审计、审批及命令注册修复 | 未批准能力永久隐藏，已批准但对应正式任务未完成的能力继续隐藏；列表由 UIA-015、资料/注销由 UIA-012、全员用量由 UIA-024、事件由 UIA-023 负责；不改服务/API |
| `UIA-019` | `UIR-019` 组织资料更新与组织注销申请 API | 产品批准；APT-009、APT-014、APT-069、APT-075 | **Account 单提交**：资料/注销 OpenAPI、服务、阻断和 scope 测试 | 无权/UNIT 写入拒绝；注销不绕过跨域阻断；不改 Token/Portal |
| `UIA-020` | `UIR-020` Token 缺失管理命令策略注册 | APT-008、APT-026、APT-045、APT-069 | **共享 approvalpolicy 单提交**：只补 4 个 Token 协议/公式命令的注册与合同测试 | Billing 六命令已转换为 ADM-54/APT-112；此候选仅使四个 Token 命令解析为路由现有精确权限和 AUDIT_ONLY，未知命令仍失败关闭；不改 OpenAPI、领域服务或 Portal |
| `UIA-021` | `UIR-021` Billing Outbox 查询/重放 API | 产品批准；APT-006、APT-045、APT-069、APT-076 | **Billing 单提交**：仅 Billing OpenAPI、Billing 自有 Outbox/`event_replay_requests`、授权委托与测试 | 完整实现 §4.2.1：GET 返回不可变包络 `event_digest`；POST 强制 `expected_digest`、16～128 字符幂等键、至少 20 字符 reason；仅未发布死信在 Serializable 事务中追加请求并重排队；人类授权与 `EVENT_REPLAY` 身份分别审计；摘要变化、非死信、已发布、跨域 ID、同键异请求均失败关闭 |
| `UIA-022` | `UIR-022` Token Outbox 查询/重放 API | 产品批准；APT-006、APT-045、APT-069、APT-076 | **Token 单提交**：仅 Token OpenAPI、Token 自有 Outbox/`event_replay_requests`、授权委托与测试 | 完整实现 §4.2.1：GET 返回不可变包络 `event_digest`；POST 强制 `expected_digest`、16～128 字符幂等键、至少 20 字符 reason；仅未发布死信在 Serializable 事务中追加请求并重排队；人类授权与 `EVENT_REPLAY` 身份分别审计；摘要变化、非死信、已发布、跨域 ID、同键异请求均失败关闭 |
| `UIA-023` | `UIR-023` 三领域事件 Portal 只读聚合与路由适配 | 产品批准；APT-076；正式 UI 需求/任务 UI-28、UI-31；候选条件 UIA-008、UIA-021、UIA-022 中产品批准并已创建正式任务的领域子集；若产品目标定义为“全平台事件”，三项领域候选必须全部批准 | **Portal 单提交**：只读聚合脱敏摘要，并把重放动作路由到事件所属服务；不持有 Outbox 写能力 | 三域卡片独立授权/失败；请求不跨域；某领域未批准时永久隐藏，对应正式任务未完成时暂时隐藏；部分获批时页面不得宣称“全平台覆盖”；不改服务/API |
| `UIA-024` | `UIR-024` 我的组织全员/节点子树用量 Portal 闭环 | 产品单独批准全员用量；APT-030、APT-069、APT-075、APT-081；唯一候选任务依赖：UIA-016 | **Portal 单提交**：只接入已完成的 Token 组织聚合用量合同 | 只有 `org.usage.read.all` 显示；UNIT 仅目标子树；不显示模型正文；不依赖 UIA-019/UIA-012、不改 Token API |

当前实现门禁：

1. `UIA-001`、`UIA-013`、`UIA-018` 以及 `UIA-015/UIA-020` 的 Billing 子集已按 §1.4 和 §18.1 转换为正式需求/任务，不再受候选总则约束；只有 §18.2 明确列出的剩余候选及缩减子集仍无正式状态，涉及其新 API 的画板能力必须隐藏，不能以前端模拟。
2. APT-007 的 bootstrap/break-glass 受控环境验证和 APT-047 的最小服务身份环境验证仍是上线门禁。门禁未通过不授权在 Portal 加临时执行入口。
3. Account/Billing/Token 事件能力须分别做产品决策，并记录目标是三域全平台覆盖还是明确的领域子集；组织审计、组织资料写/注销和全员用量同样先决策。未决或未纳入期间采用 NOT_AVAILABLE，并以永久隐藏验收事实收敛，不创建占位任务。
4. `UI-28` 拥有壳层和共享基础组件；`UI-29`、`UI-30`、`UI-31` 各自依赖 UI-28，并在本任务内完成受影响页的三语、AA、响应式和全部真实状态。`UI-32` 只在 UI-28～31 全部完成后执行第 9、11、13～15 节的无产品代码集成验收。资料/注销链为 Account API `UIA-019` + Portal `UIA-012`；全员用量链为 Token API `UIA-016` + Portal `UIA-024`。两条候选链仍未批准，不形成正式任务或缺失依赖。

上一版身份 A 已完成对权限真值表、审批策略、生命周期、38 个画板改动及验收矩阵的独立复审。本轮角色 B 经两次修订后，角色 A 在第三轮确认 `UI-28`～`UI-32` 的需求来源、候选转换、任务边界与质量归属均无 P0/P1 阻断。`UI-28`～`UI-32` 已登记在唯一需求源 `requirements.md`，本专项文档定义其详细交互与验收；后续先同步需求状态矩阵，再写入正式任务源并进入实现。
