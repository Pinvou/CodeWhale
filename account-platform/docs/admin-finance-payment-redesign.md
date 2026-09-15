# 管理后台财务与支付重构设计

> 文档状态：财务支付重构第三轮独立复审通过；UI-60/ADM-67/NFR-57 治理与安全增量需求已确认、未实现、未部署
>
> 版本日期：2026-09-14
>
> 实施顺序：文档审核通过后，先完成一期，再完成二期；每个任务独立审核、独立提交，全部完成后统一推送。

## 1. 背景与目标

当前“财务与支付”导航包含“计费运营、支付与资金、定价与商品”三个页面，但页面归属、能力闭环和高数据量适配存在以下问题：

1. 模型定价属于交易前商业规则，却位于计费运营；支付四方对账属于外部支付事实收敛，却位于计费运营。
2. 调账申请和支付恢复已经在审批中心存在真实流程，但财务页面缺少清晰的运营入口和状态下穿；审批中心当前展示合同仍需按 `admin-governance-security-redesign.md` 补为最小列表与 closed、脱敏 presenter，不能继续以原始 JSON 作为目标形态。
3. 计费页面一次加载钱包、应收、调账、价格、Hold、对账六类数据；支付页面一次加载订单、退款、晚到支付和异常四类数据，局部失败和刷新相互牵连。
4. 多数 Billing 列表固定读取前 100 或 200 条，充值商品虽有稳定 cursor 合同但 Portal 未消费。
5. 模型定价仍暴露旧版“直接激活价格”入口，不能表达当前已实现的草稿、预览、发布、未来生效和转换生命周期。
6. 计量单位和支付 Provider/商品分级控制已有领域事实或管理合同，但没有形成一致、可发现的财务页面。
7. 原因、责任人、结论和修复动作大量依赖浏览器 `prompt`，不能清晰呈现约束、影响和错误。

本次重构以“商业规则 → 外部支付事实 → 内部账务事实”为主链，建立三个职责清晰的一级分区，并完成一期现有能力闭环和二期管理能力扩展。

## 2. 分区依据与领域边界

三个分区按管理对象、事实来源、生命周期和风险模型划分，不按前端组件、数据库表或服务进程划分。

| 一级分区 | 回答的问题 | 事实来源 | 核心不变量 |
|---|---|---|---|
| 账户与计费 | 平台内部应给谁记多少权益、扣多少费用、欠多少 | Billing 钱包、权益、账本、应收、Hold | 积分和权益组成守恒；所有变更可由不可变账本解释 |
| 支付与对账 | 外部 CNY 是否真实发生并与内部事实一致 | Provider 回调、主动查询、账单、订单、退款、账本 | 不猜测支付成功；多源证据收敛；未知结果不盲目重放 |
| 定价与商品 | 新交易按什么单位、价格和商品规则发生 | 版本化计量单位、价格、商品目录及准入证据 | 发布事实不可原地改写；历史订单和 Hold 固定使用受理时快照 |

业务主链为：

```text
定价与商品（定义规则）
        ↓
支付与对账（确认外部交易）
        ↓
账户与计费（内部入账、预占、结算和应收）
```

异常工作队列不单独提升为一级分区：退款、晚到支付、支付异常和支付对账均属于支付事实生命周期；钱包、应收、Hold、调账和财务修复均属于内部账务；计量单位、模型价格和充值商品均属于交易前版本化商业规则。

## 3. 目标信息架构

保留稳定 `section`：`billing`、`payments`、`products`，避免破坏现有深链；仅调整用户可见名称和内部子页归属。

| section | 一级页面 | 子页 | 读权限 |
|---|---|---|---|
| `billing` | 账户与计费 | 概览、钱包与账本、应收与清偿、手工调账、用量预占、财务修复入口 | 各资源现有精确读权限；写权限不得替代读权限 |
| `payments` | 支付与对账 | 概览、支付订单、退款记录、晚到支付复核、支付异常、支付对账、支付渠道控制 | 支付事实沿用现有精确权限；对账权限在二期改为支付语义权限前保持服务端真实合同 |
| `products` | 定价与商品 | 概览、计量单位、模型定价、充值商品目录、商品准入验证 | `platform.pricing.read`、`platform.products.read` 分区块独立加载 |

### 3.1 账户与计费

- 钱包是个人、组织或节点内部价值账户。
- 账本是不可变财务事实；钱包列表只提供摘要，账本通过独立详情读取。
- “应收”统一显示为“应收与清偿”，单位为积分/微额度，不得显示为 CNY 或传统发票应收。
- 手工调账由运营人员创建调整记录和审批申请；审批决定及服务身份执行只在审批中心完成。
- Hold 表达模型用量尚未最终结算的内部预占；安全释放不得被复用为财务强制修复。
- 财务修复只提供申请入口、审批状态和目标资源下穿，不在本页直接执行受审批保护的账务命令。

### 3.2 支付与对账

- 支付订单、Provider 交易、退款、拒付、晚到支付和支付异常保留各自领域状态，不用订单摘要覆盖交易事实。
- 支付对账核对 Provider 账单、订单/交易、退款/拒付和 Billing 账本，必须从计费页面迁入本页。
- Provider 级和商品级新单控制分别显示；恢复申请必须基于服务端持久 `resume_draft`，并跳转审批中心完成独立审批。页面刷新只能读取持久可信观察，管理员、财务页和审批中心均不得触发、代理或重放 Provider 主动探测。
- 普通退款产品门禁仍默认关闭；“退款记录”只读展示已有退款，并下穿失败、不确定和强制退款异常，不新增普通退款入口。

### 3.3 定价与商品

- 计量单位定义可计费事实及版本；停用不删除或改写历史单位。
- 模型定价使用草稿、预览、发布、未来生效、结束或取消生命周期；旧版直接激活接口不再作为管理入口。
- 模型覆盖价格可以固定定价或显式继承全局价格；零价与缺失价格含义不同。
- 充值商品目录继续使用草稿、准入验证、发布、下架和逻辑删除；商品业务生命周期与 Provider/商品运行时止损保持独立。
- 已发布价格、计量单位和商品快照不可原地修改；变更通过新版本或受控转换完成。

## 4. 通用交互合同

1. 每个子页按需加载，仅刷新当前资源；一个子页失败不得清空同一级页面其他已加载数据。
2. 真实筛选、排序、分页和 cursor 写入 URL；没有服务端合同的能力不得由前端假装实现。
3. 列表标题分别显示当前页条数和服务端总数；服务端未返回总数时不得把当前页条数标为“共 N 条”。
4. 原因、责任人、结论、修复动作、时间和风险确认使用结构化抽屉或对话框；不得新增 `window.prompt`。
5. 责任人默认使用服务端从当前主体派生的“认领给我”；只有同时具备用户目录读取权限时才显示其他责任人选择器。Provider、渠道、模型、计量单位和商品使用权威目录选择器，不要求操作者记忆内部值。
6. 错误优先展示服务端 `code`、安全的 `message` 和 `request_id`；认证、无权、冲突、不可用、结果未知分别处理。
7. 积分/微额度和 CNY 始终分开；积分不使用货币符号，CNY 使用明确币种和金额格式。
8. 写操作按真实端点使用幂等键、ETag 或 expected version；合同缺失时不得由前端模拟并发安全。
9. 页面和动作分别鉴权；无读权限不加载资源，无写权限不渲染对应动作。
10. 简体中文、英文、日文、键盘操作、焦点恢复、`aria-live`、320/768/1440px 和 200% 缩放与功能同任务交付。

## 5. 一期：现有能力重组与闭环

一期不新增领域状态机；只消费已经存在的资源、审批流程、筛选和充值商品 cursor 合同。

### P1-1 信息架构与稳定深链

- 将三个页面改名为“账户与计费、支付与对账、定价与商品”。
- 把模型定价迁入 `products`，把支付对账迁入 `payments`。
- 保留 `section=billing/payments/products`；增加稳定 `finance_tab`，浏览器前进、后退和刷新恢复当前子页。
- 权限不足时选择首个可访问子页，不因相邻子页权限推导可见性。

### P1-2 调账申请和审批下穿

- “手工调账”提供创建申请面板，先调用 Billing 调账记录创建，再按服务端返回事实创建并提交 `WALLET_ADJUSTMENT` 审批申请。
- 调账列表显示审批申请 ID、审批/执行状态和更新时间；申请人可进入审批中心查看，只有 `SUBMITTED` 才可按 `allowed_actions` 撤回，DRAFT 不可撤回。
- 财务页面不提供批准按钮；审批人只能在审批中心按 `allowed_actions` 决定，禁止自审。
- 创建调账记录成功但审批申请失败时保留恢复入口，不重复创建调整记录。

### P1-3 支付渠道控制和恢复申请

- “支付渠道控制”以结构化卡片和表格展示 Provider/商品开关、版本、触发案件、观测缺口和可用恢复草稿。
- Provider 与商品恢复分别创建审批申请；Provider 恢复不得自动恢复商品。
- 没有合格 `resume_draft` 时动作禁用并展示服务端事实原因，不允许手工拼装命令快照。
- 审批决定与执行继续在审批中心，支付页面只展示状态和深链。
- `resume_draft`、现场观察和控制版本均来自 Billing 持久事实；UI 刷新不得调用 Provider。PAY-57 主动探测仅允许专用受信工作负载和一次性意图，浏览器没有触发接口。

### P1-4 子页独立加载与局部失败

- 删除计费六接口和支付四接口的整组刷新。
- 每个子页维护独立的 loading/error/loaded/query/page 状态和独立刷新按钮。
- 页面切换优先复用已加载状态；筛选变化只请求当前子页。

### P1-5 现有筛选与充值商品 cursor

- 应收接入现有 `wallet_id/status/limit`。
- 订单接入现有 `q/status/limit`；晚到支付和支付异常完整接入已定义筛选。
- 充值商品接入 `cursor/limit/lifecycle_status/include_deleted`，支持上一页历史栈和下一页 cursor。
- 其余无 cursor 列表只显示服务端当前返回范围和明确的容量限制提示。

### P1-6 结构化操作交互

- Hold 释放、订单修复、晚到支付排期/Provider blind、支付异常结论、对账案件分派/修复/结案以及商品命令全部移除浏览器 prompt。调账批准/拒绝只由审批中心按 `allowed_actions` 决定，APT-110 仅把审批中心现有决定 prompt 改为结构化对话框，不新增财务页直接拒绝动作。
- 默认“认领给我”由服务端从当前主体派生；任意改派只在操作者同时具备用户目录读取权限时显示用户选择器。枚举结论和修复动作使用服务端允许值；原因显示最小长度与业务影响。
- 表单失败保留输入；结果未知只允许查询目标资源或案件，不自动重放。

### P1-7 旧定价写入口收敛

- 从计费页面删除旧版“直接激活模型价格”表单。
- 二期完成前，“定价与商品”只读展示当前和历史价格，并明确新版生命周期暂不可操作。
- 不调用旧版 `POST /pricing-versions` 冒充新版价格发布。

## 6. 二期：定价生命周期、规模化列表与运营概览

### P2-1 定价与计量命令策略注册

- 在共享 `approvalpolicy` 注册六个 Billing 命令：价格草稿创建/更新、价格发布/转换、计量单位创建/停用。
- 每个命令使用路由现有精确权限并固定 `AUDIT_ONLY`；未知命令仍失败关闭。
- 不改变领域请求正文、权限名或状态迁移。

### P2-2 计量单位与模型定价生命周期 UI

- 提供计量单位版本列表、创建和停用。
- 提供全局/模型价格列表、草稿创建与编辑、逐单位固定/继承配置、发布前解析预览、发布、未来版本取消和活动版本转换。
- 每个动作逐端点遵循现有真实合同：draft create/update 使用 `expected_etag`，publish 使用 `Idempotency-Key + expected_etag`，transition 使用 `Idempotency-Key` 但没有 `expected_etag`，metering create/deactivate 当前均无 ETag 或幂等键；前端不得统一承诺 CAS 或模拟缺失合同。已发布版本只读。
- 草稿、预览、发布和转换按资源状态独立恢复，不把请求超时显示为成功。

### P2-3 Billing 高数据量列表合同

- 为钱包/账本、应收、订单、退款、调账、Hold、晚到支付、支付异常、对账 run/case 提供稳定 cursor、必要筛选和确定排序。
- 首次请求把规范化筛选、资源作用域和排序后的资源 ID 固化到 15 分钟有效的服务端分页快照；opaque cursor 绑定 `snapshot_id + subject_id + resource + scope + filter_digest + position`。后续页只消费同一快照，资源状态可返回最新值，但成员集合与顺序不因并发创建或状态更新发生重复/遗漏。
- 快照只保存资源 ID、排序位置、绑定摘要和过期时间，不复制资金或支付事实；到期清理可重试。伪造、跨主体、跨资源、跨 wallet/run 作用域或筛选摘要不一致返回 400 `PAGINATION_CURSOR_INVALID`，过期返回 410 `PAGINATION_CURSOR_EXPIRED`，不得静默回到第一页。
- `limit` 默认 50、最大 200；响应统一返回 `items`、`next_cursor`、`snapshot_as_of`，不强制伪造 total。请求 cursor 时除 `limit` 外的筛选、排序和父资源参数必须与首请求相同。
- 快照容量有界：单快照最多 1,000,000 个成员，单主体同时最多 2 个活动快照、同主体同资源最多 1 个；创建同资源新快照时替换旧快照，访问第三种资源时以事务回收该主体最旧快照并留下可验证的 410 墓碑，不能因正常跨页签浏览持续返回 429；全局最多 64 个活动快照且全部快照成员总和最多 20,000,000。命中集合超过单快照上限返回 422 `PAGINATION_FILTER_REQUIRED` 并要求收窄筛选，绝不截断后伪装完整结果。
- 新建快照按主体每分钟最多 6 次、全局每分钟最多 60 次；主体/全局并发或成员水位耗尽返回 429 `PAGINATION_SNAPSHOT_LIMITED` 和 `Retry-After`。读取既有快照不重复消耗创建额度，但仍受常规请求限流。
- 首次成员查询和写入使用 10 秒 statement timeout；超时或存储不可用返回 503 `PAGINATION_SNAPSHOT_UNAVAILABLE` 与 `Retry-After: 5`，不留下可消费的半快照。清理任务每分钟回收过期/失败快照，失败可幂等重试。

| 资源 | 默认确定排序 | cursor 额外作用域 | 规范化筛选最小集合 |
|---|---|---|---|
| 钱包 | `created_at DESC, id DESC` | 无 | `owner_type,status,q` |
| 钱包账本 | `posting.created_at DESC, posting.id DESC` | `wallet_id` | `entry_type,business_reference,created_from,created_to` |
| 应收 | `created_at DESC, id DESC` | 无 | `wallet_id,status,source_type,created_from,created_to` |
| 支付订单 | `created_at DESC, id DESC` | 无 | `q,status,provider_id,channel,created_from,created_to` |
| 退款 | `created_at DESC, id DESC` | 无 | `q,status,provider_id,created_from,created_to` |
| 手工调账 | `created_at DESC, id DESC` | 无 | `wallet_id,status,requested_by,created_from,created_to` |
| 用量 Hold | `created_at DESC, id DESC` | 无 | `wallet_id,status,model_id,created_from,created_to` |
| 晚到支付复核 | `created_at DESC, id DESC` | 无 | `q,status,severity,owner_id,provider_id,created_from,created_to` |
| 支付异常 | `created_at DESC, id DESC` | 无 | `q,status,severity,owner_id,provider_id,created_from,created_to` |
| 对账运行 | `started_at DESC, id DESC` | 无 | `provider_id,channel,status,business_date_from,business_date_to` |
| 对账案件 | `created_at DESC, id DESC` | 可选 `run_id` | `status,severity,owner_id,case_type,created_from,created_to` |

所有时间筛选使用 RFC3339 UTC 半开区间 `[from,to)`；`from >= to`、超出服务端允许窗口或未知枚举返回 400。APT-114 同一提交包含快照迁移、OpenAPI、查询实现、过期清理、跨主体/跨筛选拒绝以及并发创建/更新下无重复遗漏测试；对上表排序/筛选组合建立复合索引，并以百万订单数据验证首个快照查询/写入不超过 10 秒。

容量验收同时暴露低基数指标：活动快照数、成员总数、每分钟创建成功/限流/超时数、清理积压最老年龄和存储字节水位；成员或存储达到 70% 告警、85% 严重告警，清理积压最老年龄超过 5 分钟告警。指标只按资源类型和结果码分组，不带主体、筛选或 cursor。

### P2-4 Portal 全量分页适配

- 消费 P2-3 的 cursor 和过滤合同；分页状态进入 URL。
- 返回列表详情后恢复原筛选和页游标；410 过期明确提示“列表快照已过期”并由用户主动从第一页刷新，不自动跳页或复用旧 cursor。旧无 cursor 响应安全降级。
- 表格、移动卡片和空/错误状态不得因分页丢失操作入口。

### P2-5 分区运营概览 API

APT-116 不复用当前外层固定要求 `platform.billing.wallets.read` 的 `/billing/admin/v1/overview`，新增四个独立端点；“定价与商品”并行消费 pricing 与 products 两个端点。每个端点只返回其既有精确读权限允许的事实，Billing 不读取 Token 模型目录，因此二期不承诺“活动模型价格覆盖率”或跨服务分母。

| 端点 | 精确权限 | 窗口 | metric key | 单位与固定口径 |
|---|---|---|---|---|
| `GET /billing/admin/v1/finance-overviews/accounts` | `platform.billing.wallets.read` | 点时 `as_of` | `wallet_count` | `COUNT`；全部未物理删除钱包 |
| 同上 | 同上 | 点时 | `posted_microcredits` / `reserved_microcredits` / `available_microcredits` / `receivable_microcredits` | `MICROCREDITS`；分别汇总钱包当前投影，积分 UI 按既有换算展示且不使用货币符号 |
| 同上 | 同上 | 点时 | `pending_adjustment_count` | `COUNT`；`manual_adjustments.status=PENDING_APPROVAL` |
| 同上 | 同上 | 点时 | `at_risk_hold_count` | `COUNT`；Hold 为 `REVIEW`，或 `ACTIVE` 且 `expires_at <= as_of` |
| `GET /billing/admin/v1/finance-overviews/payments` | `platform.billing.wallets.read` | 默认 `[now-24h,now)`，可选 RFC3339 UTC `from/to`，最大 31 天 | `orders_created_count` | `COUNT`；`payment_orders.created_at` 落入窗口 |
| 同上 | 同上 | 同上 | `payments_received_fen` | `CNY_FEN`；交易状态 `PAID/LATE_PAID`，以 `provider_paid_at`，缺失时 `verified_at` 落入窗口；同一交易只计一次 |
| 同上 | 同上 | 点时 | `late_review_open_count` / `payment_anomaly_open_count` / `reconciliation_open_count` | `COUNT`；分别按各领域非终态集合统计，响应同时返回用于解释的 `included_statuses` |
| 同上 | 同上 | 点时 | `refund_attention_count` | `COUNT`；退款 `FAILED/MANUAL_REVIEW` |
| 同上 | 同上 | 点时 | `provider_controlled_count` / `product_controlled_count` | `COUNT`；当前仍阻断新单的 Provider 控制和商品控制分别统计，不相互替代 |
| `GET /billing/admin/v1/finance-overviews/pricing` | `platform.pricing.read` | 点时 | `price_draft_count` / `price_future_count` / `price_active_count` / `price_ended_or_cancelled_count` | `COUNT`；按价格派生状态互斥统计；不使用 Token 模型目录作分母 |
| 同上 | 同上 | 点时 | `metering_active_count` / `metering_inactive_count` | `COUNT`；按计量单位版本当前 active 事实统计 |
| `GET /billing/admin/v1/finance-overviews/products` | `platform.products.read` | 点时 | `product_draft_count` / `product_on_sale_count` / `product_off_sale_count` | `COUNT`；默认不计墓碑，`DELETED` 仅单独返回 `product_deleted_count` |
| 同上 | 同上 | 点时 | `admission_pending_count` / `admission_blocked_count` | `COUNT`；pending 为最新验证 `PENDING`；blocked 为非墓碑商品无验证、最新验证 `FAILED`、当前绑定证据 `expires_at <= as_of` 或证据存在 revocation，以及 `ON_SALE` 但无活动绑定；两者互斥，最新验证 `PENDING` 优先于 blocked，当前验证和活动绑定均有效且未撤销的 `VERIFIED` 不计入 |

统一 200 响应为 `{ "as_of": RFC3339, "window": null|{"from","to","timezone":"UTC"}, "metrics": [{"key","value","unit","included_statuses"}] }`。点时指标的 `window=null`；计数没有分母，当前阶段不返回比例。未知时间窗返回 400；无权返回 403；任一查询失败使该端点返回 503 和稳定错误码，不返回部分指标或伪 0，但不阻断页面对另外三个端点的调用。OpenAPI 固定 metric key、unit 和状态集合，合同测试覆盖边界时间、重复交易、墓碑、准入缺失、权限隔离和局部失败。

### P2-6 分区运营概览 UI

- 三个页面分别消费 P2-5；定价与商品页面将 pricing/products 作为两个独立区块，不在前端扫描列表计算平台总量。
- 指标卡只下穿列表真实支持的对应筛选；无等价筛选的指标只进入未过滤目录并显示口径说明。四个端点独立加载、无权隐藏和失败。
- 积分、CNY、数量、比例和累计/时间窗标签清晰区分。
- APT-117 已按上述口径完成：请求代次与取消控制阻止刷新竞态和离页回写，服务端错误只展示稳定码与 `request_id`；三语、键盘、响应式、403 隐藏、局部 503、重试与筛选下穿均由合同和真实 Chromium smoke 覆盖，且 smoke 已进入 account-platform 定向及全量前端门禁。

### 6.1 治理与安全增量边界

UI-60/ADM-67/NFR-57 不改写本文件已经交付的财务信息架构和普通 `AUDIT_ONLY` 命令，只补齐审批保护命令的安全边界：

- `WALLET_ADJUSTMENT`、`PAYMENT_PROVIDER_RESUME`、`PAYMENT_PRODUCT_RESUME` 通过前向迁移与兼容 activation 投影保持既有 ACTIVE 能力；升级、N/N-1 或回滚不得使这三类命令退化为 policy-only。
- `ENTITLEMENT_COMPONENT_ADJUSTMENT`、`LEDGER_REPAIR`、`RECEIVABLE_WRITE_OFF`、`PAYMENT_FINANCIAL_REPAIR`、`REFUND_EXCEPTION_REPAIR`、`CHARGEBACK_FINANCIAL_REPAIR`、`RECONCILIATION_FINANCIAL_REPAIR`、`USAGE_SETTLEMENT_REPAIR` 当前只有 policy，默认 INACTIVE。每一类须独立补齐领域不变量、补偿账务、外部证据、closed schema/proof/presenter、consumer/executor/recovery、policy version 与真实 E2E 后才可单独激活。
- 财务业务页拥有复杂表单、当前事实重读、`resume_draft`、编辑、续提和 fresh superseding；审批中心只展示 closed 申请、决定和安全恢复安排。业务页可以重读事实后以 expected_version Update 同一已激活 DRAFT，并按新版本/precheck/CAS submit；只有中心快捷 submit 要求 DRAFT 完全未修改。SUBMITTED 后或终态变更意图时，业务页才创建新的命令/快照/request ID/幂等键并以 `supersedes_request_id` 关联，不能继承旧批准或复制审批时间线。
- Provider/商品恢复沿用现有 15 分钟近期认证；钱包调整不在本增量中新增 recent auth。批准 proof 只由 Account 以 2 秒绝对 deadline、请求响应合计64KiB、30秒clock skew、60秒TTL 的受信内部 closed 合同调用 Billing。专项第8.2.1节按当前真实持久结构冻结映射：钱包使用四字段normalized command、`{adjustment_version}` critical和`{wallet_id,delta_microcredits}` financial；Provider/商品的normalized与critical分别为同一8/14字段版本对象，financial为`{new_orders_enabled:false,provider_control_version,product_control_version}`（Provider项最后一值显式null）。Billing按同一closed结构重读权威行；typed RFC8785公共encoder和legacy/new golden必须证明映射，禁止普通json.Marshal、Portal对象或Token专用canonicalizer冒充。Account返回后只CAS本域request/auth checkpoint/policy/activation/proof binding并持久化关联revision，不做跨库current revision CAS；Billing executor在副作用前重读并于本域事务验证revision，漂移零副作用。浏览器不能传proof；proof-required开启后v1/v2 APPROVE都强制该proof，仅v1接受并忽略legacy `observed_versions`，v2拒绝；REJECT零领域调用。Payment Provider主动探测只属于PAY-57专用工作负载。
- Billing三类既有命令在proof与proof-era授权event中的金额、revision及各`*_version`等领域`int64`，必须按专项第8.2节先做typed int64与业务约束校验，再统一编码为规范十进制JSON字符串；小值、0及binary64安全范围内的值也不得改为number，null只允许出现在逐字段矩阵明示位置。`policy_version/snapshot_schema_version`仅作为闭合协议常量保留JSON number。旧Billing表、现有v1 API/审批快照和`events.Event.Marshal/Unmarshal`的legacy numeric wire保持不变，不得用新proof/event encoder重算历史摘要。新授权wire只使用显式`APPROVAL_RFC8785_DECIMAL_V1`判别且由后续GSA-06/07/08按sidecar、逐字节Relay与dual consumer顺序接线，本设计修订不表示已存在写入口或已部署。
- EXECUTION_FAILED 逐命令区分 transient-safe、unknown、permanent、事实/策略变化和窗口结束；unknown 只查证，财务页面与审批中心均不提供通用重试或直接执行。
- 历史授权Outbox、Relay发布、仅Inbox、本地claim和只读precheck都不是执行授权；Account Begin与reconciler对同库request status的CAS是唯一所有权线性化点。APPROVE与Relay Claim都对唯一`ADMIN_APPROVAL` singleton取FOR SHARE，分别绑定approval_epoch与authorization_relay_epoch；QUIESCING/FENCE都对同一行FOR UPDATE并分别递增对应epoch，前者在事务内复核并记录授权subject计数，后者要求可发布pending、active/reclaimable claim和未分类dead-letter全为0。FENCE提交后才外部确认全副本handler=0，不另建Relay控制真相，恢复Relay不得误废同一proof epoch批准。`account.approval-command.authorized.v1`在HTTP/DB/workload/Portal层永久禁止人工/管理面通用replay；同一不可变Outbox行的自动at-least-once投递恢复不属于replay；只有已INVALIDATED且权威确认无Begin/execution/effect的授权DLQ可不可变封存为不可发布cutover exclusion，其`event_approval_epoch`必须等于原Outbox不可变`approval_control_epoch`，封存current control epoch仅作审计且不参与后续FENCE相等判断；其余阻断。QUIESCING前先滚动部署fence/delivery-bytes-aware新Relay，旧Relay claim/handler=0后撤销其claim/publish权限。随后停止新APPROVE，让所有其余既有授权Outbox以原event ID/Msg-Id和同语义bytes恢复至DB确定published/dead-letter/exclusion，等待/回收in-flight claim，并确保授权Outbox可发布pending=0、Relay active/reclaimable claim=0、未分类dead-letter=0、每行DB终态确定、Q前consumer pending/ack-pending/handler=0；合法exclusion永久保留且不计入前三项。持久FENCE授权Relay，读取可信Q并创建未启动的Q+1三个Billing新durable，再停止并删除/永久pause旧共享durable、撤ACL和探针验证不可接收。历史请求不补proof或跨epoch；旧consumer只能在既有5分钟execution_deadline前用旧epoch原event Begin，expiry worker继续以`EXECUTION_WINDOW_EXPIRED`收敛，20分钟只作健康drain/readiness预算。gate前APPROVED/EXECUTING/EXECUTION_FAILED、旧epoch非终态execution和未知effect必须全部为0；CAS后才启动新consumer并恢复Relay。Billing物理`MaxDeliver=-1`，应用层最多12次领域调用并按稳定ID DLQ PubAck、`DLQ_PUBLISHED_ACK_PENDING`、服务器确认DoubleAck收口；不得用Term/单向Ack冒充或再调领域。Token保持无限投递。
- 审批中心采用版本化 v1/v2 线：v1 在兼容窗口保持既有完整 wire 与 `observed_versions`，v2 使用 minimal list 与 closed detail；proof-required 开启后两线 APPROVE 使用同一服务端 proof。Portal 会话不得混线，v2 写失败不得降级到 v1。
- 切换不把Billing三命令置为inactive：Account DB唯一`ADMIN_APPROVAL`持久control同时承载mode/wire/proof与Relay RUNNING/FENCED；不得另建Relay真相。mode/wire/proof变化递增approval_epoch+version，Relay状态变化只递增authorization_relay_epoch+version。合法mode组合仅为LEGACY=(wire1,proof false)、QUIESCING=(wire1,proof false)、PROOF_REQUIRED=(wire2,proof true)、SAFE_ROLLBACK=(wire1,proof true)，合法边仅proof前LEGACY↔QUIESCING、QUIESCING→PROOF_REQUIRED及开启后PROOF_REQUIRED↔SAFE_ROLLBACK，开启proof后不得降为false。QUIESCING只有在Relay仍RUNNING、Q未持久化且旧durable/ACL完整时可回LEGACY；Relay已FENCED、Q已持久化或旧durable已退役时不得回退LEGACY。SAFE关闭create/update/submit/APPROVE/Begin，REJECT/WITHDRAW只对SUBMITTED安全收口，历史GET保留。PROOF_REQUIRED gate提交后、Relay仍FENCED时，consumer启动失败或Relay恢复CAS未提交只能进入零新APPROVED/Outbox的SAFE+FENCED；Relay恢复CAS一旦提交为RUNNING，其后故障才按SAFE+RUNNING处理可能已有或并发的批准。后一分支中，前一proof epoch无Begin/execution/effect的APPROVED及无活动Begin/lease且确认无effect、不可安全恢复的EXECUTION_FAILED在deadline内按`APPROVAL_SAFE_ROLLBACK_INVALIDATED`收敛；deadline已到沿用`EXECUTION_WINDOW_EXPIRED`，已Begin/effect/unknown只查证；Outbox不接受人工replay，同一原行自动恢复按专项合同执行。SAFE按Relay FENCED/RUNNING真实状态先归一为FENCED并验证，清零旧approval epoch非终态/unknown并确认consumer ready，再递增approval_epoch、启动consumer、最后恢复Relay并单独递增authorization_relay_epoch。DRAFT可由业务页权威Update，SUBMITTED在切换后批准时取得新proof；legacy APPROVED只可在deadline内旧epoch Begin，EXECUTING只查证到终态，EXECUTION_FAILED按原execution在deadline内分类收敛且不补proof。只有授权Outbox/Relay/Q前consumer排空、Relay FENCED、Q+1新durable ready、旧durable不可接收、同一快照APPROVED/EXECUTING/EXECUTION_FAILED/旧epoch非终态execution/未知effect全为0且逐副本readiness完成后才原子开启；随后启动新consumer并恢复Relay。开启后legacy N-1 Account不得接任何approval写/Begin，回滚候选必须支持control/proof，其他旧二进制由路由和DB写权限强制只读。混合版本、crash和gate竞态不得产生第二Outbox、execution或财务效果。
- 完整mode×Relay动作冻结为专项第5.4节八组合：LEGACY/PROOF_REQUIRED仅Relay RUNNING时可按各自协议APPROVE；FENCED时detail无APPROVE，操作固定503`APPROVAL_RELAY_FENCED`、`Retry-After: 5`、`Cache-Control: no-store`并保持SUBMITTED，入口已见FENCED时零proof/Outbox，竞态中已发起只读proof必须丢弃且仍零Outbox。QUIESCING/SAFE两种Relay状态均禁止APPROVE，SAFE另关闭create/update/submit/Begin。Relay状态边只允许受控cutover/maintenance/SAFE流程与同一行锁/CAS；非法mode/purpose、精确零值未满足、readiness失败或crash均保持FENCED/原态，财务页和审批中心不得提供绕过入口。
- SAFE+RUNNING只允许受控SAFE_DRAIN Claim选择SAFE CAS前已提交、绑定前一approval epoch且DB尚未确定published/dead-letter/exclusion的原授权Outbox，以原event ID作为相同Msg-Id和sidecar同语义delivery bytes自动at-least-once恢复；禁止新Outbox/APPROVE/proof/Begin，失败进入原dead-letter并仅在合法失效后exclusion。PubAck/DB close不确定时允许相同消息再次publish，即使超过24小时去重窗产生重复/乱序流消息；Inbox、Account Begin唯一CAS、execution与领域幂等保证最多一个execution/effect，人工requeue/replay仍永久禁止。Claim/FENCE两种先锁顺序与提交前、publish前、PubAck后DB close前crash均按同一singleton锁/lease幂等收敛，FENCE须等原行在DB确定published/dead-letter/exclusion。consumer启动失败或Relay恢复CAS未提交发生在Relay FENCED且批准未开放的分支，必须零新APPROVED/Outbox；Relay恢复CAS一旦提交即为RUNNING，其后故障才可能存在SAFE_DRAIN输入。
- Account capability required keys、v2 body/v1 header `action_control_version`及no-store精确沿用专项第5.4节；财务页只按服务端detail动作显示深链，version缺失/畸形/漂移或RUNNING→FENCED→RUNNING时abort、清缓存且不复活APPROVE。
- Account control初值为LEGACY/1/false/RUNNING/1/1/1。既有未发布/dead-letter授权Outbox（含迁移前DLQ）精确复用当前`events.Event.Marshal` backfill legacy epoch1与可投递delivery bytes/digest sidecar；已published历史只保存重构语义摘要/provenance，不能证明原broker bytes、ACK或effect，proof-era新事件才使用显式`APPROVAL_RFC8785_DECIMAL_V1` closed wire并将全部领域int64编码为规范十进制字符串。原event id/payload/发布/dead-letter事实不改。兼容trigger在旧writer同一SERIALIZABLE事务先取control FOR SHARE，仅在schema1/LEGACY/wire1/proof=false/RUNNING/approval epoch1时填缺失epoch；当前version/relay epoch只须为正整数，因此RUNNING→FENCED→RUNNING维护使version由1变3后旧writer仍可成功，approval epoch非1则拒绝。QUIESCING取同一行FOR UPDATE并在同一事务最终扫描/VALIDATE，40001/40P01只允许完整事务context-aware有界重试。新writer显式写，旧writer撤权、最终backfill/VALIDATE先于QUIESCING。不可由现有Marshal重构、null/错epoch或sidecar/digest不符硬阻断且不得exclusion/rewrite/人工replay。

详细状态机、activation、历史 APPROVED 收敛、Account 审批分页冻结合同、presenter、proof、唯一发布/激活顺序和 P0/P1/P2 验收以 `admin-governance-security-redesign.md` 为准。以上增量需求已确认，尚未实现、尚未部署。

## 7. 独立任务与提交顺序

| 顺序 | task_id | 需求 | 阶段 | 独立交付边界 | 依赖 |
|---|---|---|---|---|---|
| 1 | APT-105 | UI-37 | 一期 | 财务三分区改名、子页迁移、`finance_tab` 深链 | APT-094, APT-096 |
| 2 | APT-106 | UI-38 | 一期 | 调账申请、审批恢复与深链；不改审批状态机 | APT-008, APT-105 |
| 3 | APT-107 | UI-39 | 一期 | 支付 Provider/商品控制结构化展示及恢复申请深链 | APT-093, APT-105 |
| 4 | APT-108 | UI-40 | 一期 | 财务子页独立加载、局部失败与独立刷新 | APT-105 |
| 5 | APT-109 | UI-41 | 一期 | 既有筛选接入与充值商品 cursor | APT-084, APT-108 |
| 6 | APT-110 | UI-42 | 一期 | 财务/支付/商品结构化操作面板，移除 prompt | APT-106, APT-107, APT-108 |
| 7 | APT-111 | UI-43 | 一期 | 删除旧价格写入口并提供新版生命周期只读说明 | APT-105 |
| 8 | APT-112 | ADM-54 | 二期 | 六个 Billing 定价/计量命令策略注册 | APT-008, APT-018 |
| 9 | APT-113 | UI-44 | 二期 | 计量单位和模型定价完整生命周期 UI | APT-111, APT-112 |
| 10 | APT-114 | NFR-55 | 二期 | Billing 高数据量 cursor/筛选/排序合同 | APT-018–APT-025 |
| 11 | APT-115 | UI-45 | 二期 | Portal 消费 Billing 全量分页合同 | APT-109, APT-114 |
| 12 | APT-116 | BILL-47 | 二期 | 三分区运营概览 API 与权限裁剪 | APT-018–APT-025, APT-083, APT-093 |
| 13 | APT-117 | UI-46 | 二期 | 三分区运营概览 UI、下穿和局部失败 | APT-105, APT-113, APT-115, APT-116 |

每个任务必须遵循：实现者完成代码和定向验证 → 独立审核者审查需求完整性、边界、同类问题、异常状态和测试 → 有问题退回原实现者修复 → 同一审核者复审，直至通过 → 独立提交并更新任务状态。后续任务不得把前一任务尚未通过的工作一并提交。

## 8. 非目标与业务边界

- 不开放普通退款创建；历史退款继续收敛。
- 不创建银行账户、提现、资金划拨或平台内“定金”领域；页面名称不使用“定金”。
- 不在本轮实现真实开票、税务申报、商户结算单和银行到账。若 Pinvou 承担这些职责，必须另立需求、领域事实、权限和接口；仅有 `invoice_product_name` 不等于开票管理能力。
- 不复制审批中心的数据或状态机；财务业务页负责复杂表单、当前事实重读、创建/续提/fresh superseding，审批中心只负责闭合展示、决定和安全恢复安排；双向链接不复制时间线。
- 不通过前端聚合代替服务端运营统计，不使用假数据补齐 API 缺口。
- 不改变普通用户个人中心的信息架构、充值商品购买合同或模型调用结算规则。

## 9. 验收门禁

### 9.1 每任务门禁

- 对照 task_id 的全部验收点完成源码自检。
- 新增或修改接口时通过 OpenAPI lint、路由授权和合同测试。
- Portal 变更通过定向状态测试、三语检查、UI lint、构建和受影响浏览器 smoke。
- 运行 `python3 scripts/architecture-guard.py`；不涉及 CodeWhale gitlink时无需新增 fork 行为。
- 验证无普通退款入口、无 prompt、无无权请求、无伪总数和无跨分区单位混用。
- 独立审核者明确给出“通过”，才能提交。

### 9.2 全部任务完成门禁

- 一期、二期 13 个任务均有独立提交和审核结论。
- 需求、设计、任务、状态、OpenAPI、源码、构建制品和测试断言一致。
- 财务三个 section 的深链、浏览器历史、权限变化和局部失败通过集成测试。
- 完成完整前端、Portal、Billing、Account 定向测试和架构守卫。
- 最终自检没有超范围改动、未验证高风险项或文档状态虚报后，统一推送当前分支到 GitHub。
