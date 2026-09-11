# 管理后台“模型与 Token”重构专项设计

> 状态：身份 A 于 2026-09-11 增补分区概览整改，等待身份 B 独立审核；本文件定义目标需求和设计，不表示代码已实现。
>
> 范围：模型资产、协议与安全计量公式、Token Account、凭据、路由、Provisioning、用量与核对异常，以及模型上下文中的 Billing 价格投影。

## 1. 决策与不变量

当前“模型与 Token”业务分区保留，但不再以技术组件名“Token Gateway”承载全部能力。左侧导航拆为三个稳定二级页面：

| section | 页面 | 核心对象 | 最小读取权限 |
|---|---|---|---|
| `models` | 模型管理 | 模型目录、协议、公式、目录差异、生命周期和价格投影 | `platform.models.read` 或 `platform.pricing.read`；区块再按原子权限裁剪 |
| `tokenResources` | Token 资源 | Token Account、凭据元数据、路由与健康 | `platform.tokens.read` |
| `modelOperations` | 运行与用量 | Provisioning、用量请求、核对异常 | `platform.tokens.read` 或 `platform.usage.read`；区块再裁剪 |

必须保持以下边界：

1. Token 是模型目录、协议、公式、Token Account、凭据、路由、Provisioning 和用量请求的事实源；Account/Organization 是 owner 候选的权威来源；Billing 是价格、计量单位和金额的事实源。
2. 模型详情可以展示和编辑 Billing 的同一份价格，但不得复制价格表、由 Token 代写 Billing，或削弱“定价与商品”的跨模型治理入口。
3. 导航、请求、字段和动作只依赖原子权限，不以角色名称绕过权限。平台超级管理员通过真实权限授权获得完整常驻平台能力；秘密不可回显和临时 PII 导出边界不变。
4. 任一数据源失败只影响自己的区块；无权、失败和真实数值 0 是三种不同状态。
5. 所有写命令在服务端重新检查权限、近期认证、资源版本、幂等和依赖；前端结构化确认不等于审批，也不构成执行授权。

## 2. 页面与业务闭环

### 2.1 模型管理

“模型目录”负责同步目录、差异预览、草稿编辑、发布、计划弃用、下架和回滚；“协议与公式”补齐现有后台接口对应的界面；模型详情在二期成为独立主内容页。

标准闭环为：同步目录 → 查看新增/变化/缺失差异 → 编辑草稿 → 校验协议、公式、有效价格及 READY 路由 → 发布 → 监控 → 计划弃用 → 下架或前向回滚。发布、弃用、下架和回滚都先展示价格、路由、Provisioning、进行中请求、新会话/存量会话截止及替代模型影响，提交时重新预检。未来发布仍属于既有 `TOKEN_MODEL_PUBLISH / AUDIT_ONLY`：计划必须冻结与即时发布相同的草稿版本、草稿内容、基础目录、协议、能力、别名、价格与路由证据，通过受治理命令后才进入 `SCHEDULED`，到期 worker 复用即时发布的单一领域效果；只有弃用和下架使用 `TOKEN_MODEL_LIFECYCLE_CHANGE / APPROVAL_REQUIRED`，进入 Account 申请、独立审批、授权事件与执行租约链。

模型详情唯一页签结构为“概览 / 能力、协议与公式 / 路由与运行 / 价格与计量 / 生命周期与依赖 / 审计”。“能力、协议与公式”同时呈现当前不可变能力版本、协议版本及安全计量公式，不另设重复协议页签；“生命周期与依赖”统一承载目录版本差异、价格/路由/Provisioning/进行中请求依赖和计划生效，不把同一依赖拆到概览。价格页签消费 Billing 权威投影；没有 `platform.pricing.read` 时不请求、不渲染，编辑入口还要求 `platform.pricing.manage`，并复用财务定价组件和 Billing 命令合同。审计页签额外要求 `platform.audit.read`。

### 2.2 Token 资源

标准闭环为：选择个人、组织或节点 owner → 创建 PLATFORM Token Account → 通过隔离凭据页新增或轮换会接触 Secret 的凭据 → 由 Token 域直接执行脱敏现场校验 → 从模型目录选择稳定 `model_id` 与已发布 `protocol_version_id` 配置路由 → Provisioning → 服务端健康探测 → 启用 → 隔离轮换、Portal 向 Account 申请审批撤销或停用。撤销不打开隔离页。

创建入口同时要求：

- `platform.tokens.manage`：允许创建和修改 Token Account；
- `platform.tokens.owner-candidates.read`：允许解析最小 owner 候选。

只有 `platform.tokens.manage` 而没有候选读取权限时，页面必须显示“缺少所有者候选权限，无法创建”的失败关闭状态，创建按钮禁用；不得退化为 UUID 文本框、复用通用用户/组织目录或允许盲填。候选已授权但不存在、不可见或越出作用域时返回空页，不泄露对象是否存在。

Token Account 详情在二期成为独立主内容页，页签为“概览 / 凭据 / 路由 / Provisioning / 审计”。凭据只显示类型、状态、指纹、更新时间和稳定结论，不显示 Secret、密文、Provider Key ID、Provider 私有地址或原始错误。

路由表单只接受目录选择器产生的稳定模型、协议版本及路由策略；调用方不得提交健康状态。服务端探测状态固定为 `UNKNOWN/PROBING/HEALTHY/DEGRADED/UNHEALTHY/STALE`，HEALTHY 证据 TTL 为 5 分钟。Token Account 详情必须返回与健康证据同源的数据库权威 `as_of`；Portal 只能以该值判断证据是否来自未来或已过期，不得使用浏览器时钟或自设容差。`as_of` 缺失/非法、观测时间晚于 `as_of`、TTL 非法或证据过期时均按不可路由失败关闭。

### 2.3 运行与用量

一期在当前主内容区提供 Provisioning 详情和用量详情子视图，不使用抽屉挤压列表；二期再补趋势和跨资源联动。

Provisioning 支持状态、模型、Token Account、owner 和时间筛选，详情展示目标代次、尝试、脱敏证据、SLA 与修复入口。用量支持状态、模型、Token Account、组织和时间筛选，并提供“核对异常”工作队列。

异常闭环为：发现异常 → 查看请求、Hold、上游与结算证据 → 仅重新读取并归并既有证据 → 确认最终结算 → 保存审计。普通“重新核对”不得重新发起推理、不得冒充 `USAGE_SETTLEMENT_REPAIR`，未知结果继续按同一幂等引用查询。

### 2.4 分区顶部概览

`UI-59` 与 `ADM-66` 只替换 UI-57/ADM-59 中概览的页面归属、权限组合和读取投影；UI-57 已冻结的指标状态集合、数据库权威时间、趋势桶、目录 observation 与性能口径继续有效。不得再以“目录与运行健康”把三个领域混成一个组件，也不得在模型管理和运行与用量重复渲染同一组指标。

| 所属页面 | 顶部概览 | 固定指标 | 权限与可见性 | 允许的点击结果 |
|---|---|---|---|---|
| `models` 模型管理 | 模型目录概览 | `published_model_count`、`deprecated_model_count`、`model_draft_count` | 整个概览仅要求 `platform.models.read`；进入“协议与公式”或模型详情时不显示 | 下钻仅以 2.4.1 逐指标矩阵为准；保持 `section=models` |
| `tokenResources` Token 资源 | 路由健康概览 | `stable_route_count`、`healthy_route_count`、`route_attention_count`，健康率同时显示分子和 `stable_route_count` 分母 | 仅要求 `platform.tokens.read`；进入 Token Account 详情时不显示 | 下钻仅以 2.4.1 逐指标矩阵为准；保持 `section=tokenResources` |
| `modelOperations` 运行与用量 | 运行与用量概览 | Token 权限组：`provisioning_pending_count`、`provisioning_attention_count`；Usage 权限组：`usage_request_count`、`reconciliation_pending_count`、`reconciliation_stopped_count` 及用量/核对趋势 | `platform.tokens.read` 与 `platform.usage.read` 分组裁剪；至少一个权限存在即显示有权组，两者均无时不请求并进入既有无泄露拒绝路径；只有具备 `platform.usage.read` 时显示时间选择器；进入 Provisioning/用量详情时不显示 | 下钻仅以 2.4.1 逐指标矩阵为准；保持 `section=modelOperations` |

时间选择器 `model_operations_window=24h|7d|30d` 及“用量与核对趋势”只在具备 `platform.usage.read` 的 `modelOperations` 列表态存在，只影响窗口用量与趋势请求；token-only 不渲染选择器并按 2.4.2 清除该参数。模型目录和路由健康都是点时指标，不能携带或解释该窗口。切换 section 时继续按第 3 节清除其他区状态；概览下穿不得改变 section。浏览器返回必须恢复概览前的本区页签、筛选、cursor 和适用窗口，刷新与分享链接得到同一状态。

#### 2.4.1 逐指标下钻矩阵

下钻类型只有三种：`EXACT` 表示概览集合与列表筛选集合严格相等；`RELATED` 表示只能进入相关列表或其真子集，卡片及结果区必须持续显示“相关结果/非等价统计”；`NON_INTERACTIVE` 表示仅展示指标，不能使用按钮、链接、tabindex 或点击样式。不得为了让卡片可点击而把不等价集合标为精确。

| 指标/趋势序列 | 类型 | 当前允许行为 | 不精确原因或精确条件 |
|---|---|---|---|
| `published_model_count` | EXACT | 留在 `models`，原子替换全部 `model_*` 筛选为 `model_lifecycle_status=PUBLISHED` 并清除 `model_cursor` | 当前模型列表可表达同一 PUBLISHED 集合 |
| `deprecated_model_count` | EXACT | 留在 `models`，原子替换全部 `model_*` 筛选为 `model_lifecycle_status=DEPRECATED` 并清除 `model_cursor` | 当前模型列表可表达同一 DEPRECATED 集合 |
| `model_draft_count` | NON_INTERACTIVE | 仅显示 | 指标来自草稿事实，当前模型目录筛选不能证明与草稿集合等价；未新增 additive draft filter |
| `stable_route_count` | NON_INTERACTIVE | 始终显示服务端真实非负整数，值为零时显示 `0`；不显示 N/A，始终不可点击 | 当前主列表对象是 Token Account，且无 route `reference_status` 列表筛选；只有以它为分母的健康率在值为零时显示不可计算 |
| `healthy_route_count` | RELATED | 留在 `tokenResources`，原子替换全部 `token_*` 筛选为 `token_health_status=HEALTHY` 并清除 `token_cursor`，结果标题标“相关 Token Account” | 指标计数路由，列表计数 Token Account，二者不保证一一对应 |
| `route_attention_count` | RELATED | 留在 `tokenResources`，只聚焦 Token Account 列表并清除 `token_cursor`，持续标“相关结果”；不得自动选取某一个异常状态冒充完整集合 | 指标是六类需关注路由的并集，当前列表既按账户聚合又只接受单值 health status |
| `provisioning_pending_count` | EXACT | 留在 `modelOperations`，原子替换全部 `provisioning_*` 筛选为 `provisioning_status=PENDING`、切换 `operations_tab=provisionings` 并清除 `provisioning_cursor` | 当前 Provisioning 列表可表达同一 PENDING 集合 |
| `provisioning_attention_count` | RELATED | 留在 `modelOperations`，切换 `operations_tab=provisionings`、清除 `provisioning_cursor` 并标“相关结果”；不写单一状态筛选 | 指标是 FAILED 与 REPAIR_REQUIRED 并集，当前未新增 additive/多值状态筛选 |
| `usage_request_count` | EXACT | 留在 `modelOperations`，原子替换全部 `usage_*` 筛选为响应 `window.from/to`，切换 `operations_tab=usage` 并清除 `usage_cursor` | 列表 UTC 半开时间条件与服务端窗口集合一致，且不附加其他状态条件 |
| `reconciliation_pending_count` | RELATED | 留在 `modelOperations`，切换 `operations_tab=reconciliation`、清除 `reconciliation_cursor` 并标“相关结果”；不写伪等价状态 | 指标基于待核对用量请求点时集合，列表对象是核对案件，且当前无等价的多状态请求过滤 |
| `reconciliation_stopped_count` | RELATED | 留在 `modelOperations`，切换 `operations_tab=reconciliation`、清除 `reconciliation_cursor` 并标“相关结果”；不写伪等价 stopped 筛选 | 当前核对案件列表没有 `reconciliation_stopped` 等价筛选 |
| 趋势 `usage_request_count` 序列 | EXACT | 点击整条序列或桶时按所选响应窗口/桶边界执行用量列表的同一原子替换规则 | 服务端趋势和列表均按 `started_at` UTC 半开区间表达 |
| 趋势 `reconciliation_attention_count` 序列 | RELATED | 仅切换核对 Tab、清除其 cursor 并标“相关结果”，或设为不可交互 | 趋势按案件 `created_at` 计数，当前核对列表没有对应时间筛选 |

EXACT 下钻必须在一次 history 写入中完成：先删除目标数据源的全部具名筛选和 cursor，再写入矩阵给出的完整筛选集合；不能在旧筛选上增量叠加。RELATED 下钻同样不能保留会进一步缩小且未向用户说明的旧筛选。该次写入使用 `pushState`，浏览器后退精确恢复下钻前的 URL、筛选、cursor、页签和窗口；popstate 不得追加第二条历史或重新解释为当前默认值。

#### 2.4.2 权限变化与缓存失效

概览缓存键至少绑定稳定用户、授权版本、section、响应变体和（仅 Usage 变体）窗口。授权版本变化、任一相关权限撤销、退出登录、gate 关闭或 section 离开时，Portal 必须先中止在途请求并递增请求代次，再清除已撤销组的成功缓存、错误和下钻状态；随后才能按新权限发起请求。旧授权版本或旧代次的迟到响应不得渲染、写缓存、恢复 URL 或触发下钻。

`modelOperations` 若没有 `platform.usage.read`，必须隐藏时间选择器，并在渲染或请求前以同一次规范化 `replaceState` 删除 `model_operations_window`，不得再写回该参数。若服务端共享 SQL 的参数类型要求内部窗口值，Token 只能在完成授权后于服务内部规范化为固定 `24h` 占位；Usage 分支必须保持 `never executed`，该占位不进入响应、URL、日志或缓存键，也不构成 Usage 数据读取。

#### 2.4.3 零分母与不可评估语义

`stable_route_count` 卡片始终显示服务端真实非负整数，值为零时明确显示 `0`。仅当 `stable_route_count=0` 时，健康率不执行 `healthy_route_count/stable_route_count`，不显示 `0%` 或 `100%`，而显示“不可计算 / 无可评估路由”；三语使用同一语义，并通过文本而非仅颜色表达。此时 `healthy_route_count` 仍显示服务端真实值，但健康率状态不得点击为“零条健康路由”的精确结果。

三个概览分别维护 loading、error、retry、last_updated 和成功缓存。403 表示该概览/指标组无权，接口失败表示暂不可用，合法数值 `0` 与趋势全零无数据保持独立语义；概览失败不卸载或遮挡本页列表、详情入口和操作区。请求在离页、进详情、撤权或 gate 关闭时中止，迟到响应不得回写当前页面。

三个标题、指标、口径、错误、“相关结果”说明和空状态必须提供简体中文、英文、日文。指标使用原生按钮或等价语义，键盘可达且焦点可见；不可精确下穿的卡片不得伪装成按钮。触控目标至少 44×44 CSS px，在 320px 宽与 200% 缩放下不横向遮挡关键操作，并尊重 reduced motion。

## 3. URL、深链与权限裁剪

### 3.1 稳定参数

| 页面 | 列表状态 | 详情状态 |
|---|---|---|
| 模型管理 | `model_* + model_cursor`、`protocol_* + protocol_cursor`、`formula_* + formula_cursor` | `model_id=<UUID>&model_detail_tab=`；嵌套 `model_provisioning_* + model_provisioning_cursor`、`model_audit_* + model_audit_cursor` |
| Token 资源 | `token_* + token_cursor`；owner 选择器 `owner_candidate_* + owner_candidate_cursor` | `token_account_id=<UUID>&token_detail_tab=`；嵌套使用 `token_credential_cursor`、`token_route_cursor`、`token_provisioning_cursor`、`token_audit_cursor` |
| 运行与用量 | `operations_tab`；`provisioning_* + provisioning_cursor`、`usage_* + usage_cursor`、`reconciliation_* + reconciliation_cursor` | `provisioning_id=<UUID>` 或 `usage_request_id=<UUID>`；嵌套 `provisioning_attempt_*`、`usage_evidence_*` 各有 cursor |

筛选、页签、cursor 和详情目标都进入 URL；前进、后退、刷新和复制链接必须恢复相同状态。排序固定服务端且不进入 URL，不提供客户端 sort。三个 section 及其每个数据源使用彼此独立的具名 cursor 和筛选参数，禁止复用通用 `cursor/q/status`。全部精确键以第 4.1 节映射为准。进入任一新 section 时，先删除另外两个 section 的全部页签、筛选、详情和 cursor 参数；同 section 进入详情时保留该 section 的列表上下文，返回目录才能恢复。

旧 `section=tokens` 自 GA 起至少兼容 90 天或两个生产版本（取较长者），按“模型管理 → Token 资源 → 运行与用量”选择首个可访问页面。规范化必须先删除旧聚合页的 `tab/token_tab/tokens_tab/q/status/protocol/source/owner_type/health/model_id_filter/owner_id_filter/from/to/cursor`，以及三个新 section 的全部状态，再以一次 `replaceState` 写入目标 section；不猜测旧 tab 所属页面、不携带旧 cursor 或详情。三个页面均无权则清理后进入 403。

### 3.2 无权深链清理顺序

处理任意请求 URL 时必须先执行以下纯客户端规范化，再决定是否请求数据：

1. 读取请求的 `section`，计算该 section 的最小读取权限；不得先读取详情。
2. 若 section 无权，通过一次 `history.replaceState` 删除三个 section 的**全部详情、页签、筛选和 cursor 参数**；然后把 `section` 设为 `forbidden` 并渲染统一 403。
3. 只有 section 有权时才校验详情参数。非法 UUID 删除对应详情和详情页签，回到该 section 列表并显示稳定的“链接无效”提示；不得向服务端发送非法 ID。
4. `model_id` 只允许出现在 `models`，`token_account_id` 只允许出现在 `tokenResources`。`provisioning_id` 与 `usage_request_id` 互斥；同时存在时二者及对应详情状态全部删除，回到运行列表并提示“详情目标冲突”，不得任择其一。
5. 合法 UUID 仍必须在请求前通过 section 权限门禁。无权合法 UUID、非法 UUID、互斥详情均不得进入 DOM、控制台、遥测、错误日志或审计，也不得发起详情请求。

页面内无权页签采用同样顺序：先从 URL 删除页签及其专属详情参数，再 `replaceState` 到首个有权页签；没有任何有权页签时进入已经清理参数的 403。服务端仍独立执行相同权限检查，不能信任客户端裁剪。

### 3.3 强制验收向量

- 合法 `model_id` + 无模型页权限：零详情请求，清理全部详情/页签参数后 403，ID 不进入 DOM/日志。
- 非法 `model_id` + 有模型页权限：零详情请求，清理详情参数后回模型列表并显示稳定提示。
- 同时给出 `provisioning_id` 和 `usage_request_id`：零详情请求，两个 ID 均清理，不选择任一目标。
- 合法 `token_account_id` + 只有用量权限：零 Token Account 详情请求，清理后 403，不改道到运行页掩盖拒绝。
- 无权 `token_detail_tab=audit`：不请求审计投影，删除详情/页签状态后进入可访问状态或 403。
- 从 `tokenResources`（含 `token_cursor` 和详情）切换到 `models`：Token 参数全部删除，模型页不误用 Token cursor；浏览器后退恢复原 Token URL 和上下文。
- 访问带任意旧 `tab/q/status/cursor/detail` 的 `section=tokens`：一次 `replaceState` 只保留首个有权新 section，旧状态全部删除；刷新不再发生第二次迁移。
- 在三个 section 间连续前进/后退：每个历史项只包含当时目标 section 的参数，不出现跨 section 详情请求或 cursor 错配。
- 同一模型页同时设置 `protocol_cursor` 与 `formula_cursor`，让公式请求失败：协议可继续翻页且两个 cursor 不互相覆盖；刷新和后退恢复各自 URL，重试只清除公式错误。
- 运行页依次写入 `provisioning_cursor`、`usage_cursor`、`reconciliation_cursor` 后切换页签：每个数据源只发送自己的 filter/cursor；任一 503/504 不清除另外两者的成功结果或 URL 状态。
- 从模型详情带 `model_provisioning_cursor/model_audit_cursor` 返回目录：只清理详情与嵌套状态并保留 `model_cursor`；Token 详情和 Provisioning/Usage 详情按各自嵌套 cursor 执行同一断言。

## 4. 查询、分页与独立加载

所有模型与 Token 列表使用统一 envelope：

```json
{
  "items": [],
  "next_cursor": null,
  "has_more": false,
  "limit": 50,
  "as_of": "2026-09-03T00:00:00Z",
  "snapshot_expires_at": "2026-09-03T00:15:00Z",
  "request_id": "..."
}
```

模型、协议、公式、Token Account、Provisioning、用量、核对异常和资源审计默认 50、最大 200；owner 候选默认 20、最大 50。超范围为 400 `INVALID_LIMIT`。服务端没有 total 时，Portal 只显示“本页 N 项 / 还有下一页”，不得推算总数。

cursor 必须签名绑定主体、授权版本、资源、规范化筛选、排序、limit 和快照高水位，TTL 15 分钟；篡改/错配返回 400 `INVALID_CURSOR`，过期、替换或回收返回 410 `CURSOR_EXPIRED`。排序最后包含稳定 ID。

### 4.1 逐资源查询、URL 与 API 合同

所有筛选都由服务端执行；空字符串、重复参数、未知枚举、非法时间窗和无权作用域必须在创建快照前规范化或拒绝，Portal 不以当前页前端过滤冒充查询。**所有排序固定在服务端，Portal 和公开 API 均不提供 `sort/order/order_by` 参数**；下表的权威排序是确定性全序，最后一项稳定 ID 是唯一 tie-breaker。URL 使用资源前缀，API 使用表中无前缀参数；Portal 只做一一映射，不转义为共享 `q/status/cursor`。

| 资源 | Portal URL 参数（含独立 cursor） | Token Admin API 与参数 | 权威确定性全序 | 现有 schema / 目标来源 |
|---|---|---|---|---|
| 模型 | `model_q/model_lifecycle_status/model_protocol_code/model_source_adapter/model_source_missing/model_cursor` | `GET /token/admin/v1/models?q=&lifecycle_status=&protocol_code=&source_adapter=&source_missing=&cursor=&limit=` | `model_catalog_entries.normalized_name ASC, model_catalog_entries.model_id ASC` | 两个排序字段均已存在；`source_adapter` 来自 `model_identities.adapter_version`。`source_missing` 是 `source_available=false` 的 API 规范化别名。 |
| 协议 | `protocol_q/protocol_status/protocol_code/protocol_published/protocol_created_from/protocol_created_to/protocol_cursor` | `GET /token/admin/v1/protocols?q=&status=&protocol_code=&published=&created_from=&created_to=&cursor=&limit=` | `protocol_definitions.protocol_code ASC, protocol_definitions.version DESC, protocol_definitions.id ASC` | 均为现有列；`q` 仅匹配代码或精确 ID，不虚构名称列。 |
| 公式 | `formula_q/formula_status/formula_protocol_id/formula_metering_unit_code/formula_published/formula_created_from/formula_created_to/formula_cursor` | `GET /token/admin/v1/formulas?q=&status=&protocol_definition_id=&metering_unit_code=&published=&created_from=&created_to=&cursor=&limit=` | `protocol_definitions.protocol_code ASC, protocol_formula_versions.version DESC, protocol_formula_versions.id ASC` | 版本、ID、时间均为现有列；`q` 只匹配精确 formula ID 或关联协议代码，`metering_unit_code` 查询现有 `required_unit_versions` JSONB key。新增独立列表 API；兼容期内旧 `/protocols` 聚合响应只读，不用于分页。 |
| Token Account | `token_q/token_owner_type/token_owner_id/token_status/token_health_status/token_credential_status/token_cursor` | `GET /token/admin/v1/token-accounts?q=&owner_type=&owner_id=&status=&health_status=&credential_status=&cursor=&limit=` | `token_accounts.updated_at DESC, token_accounts.id ASC` | 排序列已存在；健康/凭据状态由现有凭据、路由和 Provisioning 只读聚合投影产生，不新增可写事实。服务端始终强制 `management_mode=PLATFORM`。 |
| Provisioning | `provisioning_q/provisioning_status/provisioning_model_id/provisioning_token_account_id/provisioning_scope_type/provisioning_scope_id/provisioning_sla_overdue/provisioning_cursor` | `GET /token/admin/v1/provisionings?q=&status=&model_id=&token_account_id=&scope_type=&scope_id=&sla_overdue=&cursor=&limit=` | `route_provisionings.updated_at DESC, route_provisionings.id DESC` | 均来自现有 `route_provisionings`；`token_account_id` 映射现有 `active_token_account_id`，未激活任务不会误匹配。模型详情和 Token 详情嵌套列表分别使用 `model_provisioning_* + model_provisioning_cursor`、`token_provisioning_* + token_provisioning_cursor`，调用同一 API 并固定 `model_id` 或 `token_account_id`。 |
| 用量请求 | `usage_q/usage_execution_status/usage_settlement_status/usage_model/usage_token_account_id/usage_organization_id/usage_started_from/usage_started_to/usage_cursor` | `GET /token/admin/v1/usage-requests?q=&execution_status=&settlement_status=&model=&token_account_id=&organization_id=&started_from=&started_to=&cursor=&limit=` | `usage_requests.updated_at DESC, usage_requests.id ASC` | 排序及过滤来自现有列；`q` 只支持精确 `usage_request_id` 或 `idempotency_key`，模型筛选使用现有稳定快照字段 `model`，不声称已有 `model_id`。 |
| 核对异常 | `reconciliation_q/reconciliation_status/reconciliation_severity/reconciliation_case_type/reconciliation_owner_id/reconciliation_sla_overdue/reconciliation_cursor` | `GET /token/admin/v1/reconciliation-cases?q=&status=&severity=&case_type=&owner_id=&sla_overdue=&cursor=&limit=` | `manual_review_cases.updated_at DESC, manual_review_cases.id DESC` | 新只读 API 从现有 `manual_review_cases` 固定 `domain=TOKEN` 且限定用量/核对 case type；`q` 仅精确 case ID 或 `resource_id`。模型、Token Account 等扩展条件未进入一期 UI/API，避免不存在的 `detected_at/reason_code` 投影。 |
| 资源审计 | 模型详情：`model_audit_action/model_audit_actor_id/model_audit_occurred_from/model_audit_occurred_to/model_audit_cursor`；Token 详情：`token_audit_action/token_audit_actor_id/token_audit_occurred_from/token_audit_occurred_to/token_audit_cursor` | `GET /token/admin/v1/resource-audit?resource_type=&resource_id=&action=&actor_id=&occurred_from=&occurred_to=&cursor=&limit=` | `token_audit_logs.occurred_at DESC, token_audit_logs.id DESC` | 均为现有列；服务端强制单资源 `resource_type + resource_id`，新增的是脱敏分页投影 API，不新增审计事实。两种详情上下文各自持有 cursor，不能互相复用。 |
| owner 候选 | `owner_candidate_q/owner_candidate_type/owner_candidate_status/owner_candidate_organization_id/owner_candidate_cursor` | `GET /token/admin/v1/owner-candidates?q=&owner_type=&status=&organization_id=&cursor=&limit=` | `normalized_display_label ASC, owner_type ASC, owner_id ASC` | 新 Token 只读门面从 Account 用户和 Organization 组织/节点权威 API 取得最小字段，在快照构建时按 Unicode NFKC + Unicode case-fold 计算不回写源库的 `normalized_display_label`；`owner_type/owner_id` 是稳定投影字段。不得复制目录或新增可写 owner 表。 |

`protocol_published/formula_published` 只是 `published_at IS [NOT] NULL` 的布尔过滤，`*_created_from/to` 采用 UTC 半开区间。当前 UI 未展示但 API 允许的高级条件（模型 `source_missing`、公式 `metering_unit_code`、Provisioning scope/SLA、owner `organization_id`）仍必须使用表中 URL 键保存，后续展开高级筛选不会改变 cursor 绑定；未列出的 API 参数一律 400 `INVALID_FILTER`。

详情中的嵌套列表也不得借用父列表 cursor：Token Account 的凭据/路由分别使用 `token_credential_metadata_status + token_credential_cursor`、`token_route_health_status + token_route_cursor`，Provisioning 尝试历史使用 `provisioning_attempt_status + provisioning_attempt_cursor`，用量证据使用 `usage_evidence_type + usage_evidence_cursor`；它们分别调用 `/token-accounts/{id}/credentials`、`/token-accounts/{id}/routes`、`/provisionings/{id}/attempts`、`/usage-requests/{id}/evidence` 的目标分页 GET，并由服务端按 `version DESC,id DESC`、`priority DESC,id DESC`、`repair_round ASC,attempt_number ASC,id ASC`、`observed_at ASC,source_kind ASC,source_id ASC` 固定排序。凭据现有 `version/id`、路由现有 `priority/id`、尝试现有 `repair_round/attempt_number/id` 均可直接使用。用量证据把两个既有事实源统一为只读 API 投影：`CANCELLATION` 将 `usage_cancellation_evidence.occurred_at` 映射为 `observed_at`、`source_id=id`，`CONVERGENCE` 将 `usage_convergence_evidence.observed_at` 保持为 `observed_at`、`source_id=id`；跨表唯一键固定为 `(source_kind,source_id)`，cursor 冻结三元位置 `(observed_at,source_kind,source_id)` 及筛选/授权版本，不新增可写事实。其他用量摘要不冒充该 evidence 列表数据源。详情关闭时只清理对应嵌套 URL 状态，父目录 filter/cursor 保留。

无 cursor 的请求表示创建新首屏快照：服务端规范化筛选、冻结授权版本和排序高水位，在同一事务写入完整成员集并返回第一页；空结果也创建可验证的空快照，不回退无快照查询。相同主体/资源的新首屏请求按容量规则事务替换旧快照。

带 cursor 的请求表示读取既有快照续页：cursor 中的位置为排他下界，响应不得重复上一页末项。续页不得重新查询事实表、重新排序或扩大成员集；Portal 可在浏览器 URL 保留原筛选以便展示，但调用服务时以 cursor 为主。若同时携带筛选、排序或 limit，规范化后必须与 cursor 绑定值完全一致，否则返回 400 `INVALID_CURSOR`；授权版本变化返回 410 `CURSOR_EXPIRED`。`next_cursor=null` 与 `has_more=false` 必须同时成立。

资源独立 cursor 不绕过第 4.3 节的单主体活动快照上限；第三个资源首屏可能事务回收最旧快照，浏览器后退到被回收 cursor 时只对该数据源返回 410 并显示“快照已过期，按保留筛选重新查询”，不得清理或重建其他数据源快照。

### 4.2 三类唯一预算

下表中的预算互斥且不可累加、嵌套重置或用重试延长；从入口代理收到请求开始计时，包含排队、网络和服务处理。入口代理的 upstream timeout 必须严格大于业务预算，以便领域服务先返回稳定错误，目标值为业务预算加 2 秒；客户端总等待再比入口代理多 2 秒。

| 请求类别 | 唯一业务预算 | 包含工作 | 超时错误 | 入口代理 upstream timeout |
|---|---:|---|---|---:|
| 首屏快照构建 | 10 秒 | 首次成员查询、完整快照写入，以及快照头/成员/高水位/墓碑同事务提交 | 503 `PAGINATION_SNAPSHOT_UNAVAILABLE`，`Retry-After: 5` | 12 秒 |
| 已有快照读取 | 3 秒 | 验签、授权版本校验、快照页读取和 envelope 组装；不允许重建快照 | 504 `QUERY_TIMEOUT` | 5 秒 |
| 跨域影响预检 | 5 秒 | Token 本域依赖快照与 Billing/Account/Organization 的有界并行投影；不创建分页快照 | 504 `PRECHECK_TIMEOUT`，并标记结果不确定、写动作失败关闭 | 7 秒 |

“首次快照构建”的 10 秒是整次构建的唯一墙钟预算，不是 SQL statement timeout 外再加应用预算；数据库 statement timeout 使用同一剩余 deadline。“已有快照读取”不允许因为 cursor 无效或页面为空而隐式创建新快照。“跨域预检”不复用 3 秒普通读取预算，也不在各下游分别获得 5 秒；所有下游共享同一绝对 deadline。

容量/速率错误与超时分离：过宽为 422 `PAGINATION_FILTER_REQUIRED`；快照容量或创建速率耗尽为 429 `PAGINATION_SNAPSHOT_LIMITED`；常规限流为 429 `RATE_LIMITED`；非快照查询容量不足为 503 `QUERY_CAPACITY_EXCEEDED`。任何失败均不得返回不完整 200、半快照或静默首页。创建速率耗尽时服务端按数据库权威分钟窗口返回距下一窗口的真实 `Retry-After`（1～60 秒），Portal 必须原样转发；前端显示倒计时并在冷却结束前禁用该数据源的刷新/重试，保留同一查询上次成功数据，且不得自动重试。完全相同 URL 的并发只读请求在客户端合并为一次网络请求；单个视图离开只取消该调用者等待，不得中断仍有调用者依赖的共享请求。

### 4.3 快照容量

单快照最多 1,000,000 个成员；单主体最多 2 个活动快照且同主体同资源最多 1 个；同资源新快照事务替换旧快照，第三资源事务回收最旧快照。全局最多 64 个活动快照、20,000,000 个成员及 8 GiB 成员存储。70% 告警，85% critical 并拒绝新的非替换快照；墓碑至少保留 20 分钟，清理每分钟幂等执行。

每个页面/页签独立维护 loading、error、retry、last_updated、filter 和 cursor。模型失败不得阻断价格投影，用量失败不得阻断 Provisioning；离页中止请求，代次令牌防止旧响应回写。

### 4.4 目录 observation、页面级概览与运行趋势合同

新增页面级只读投影固定为：

| Token Admin API | 必要权限 | 响应范围 | 请求参数 |
|---|---|---|---|
| `GET /token/admin/v1/overviews/model-catalog` | `platform.models.read` | 仅模型目录三个点时指标 | 不接受查询参数 |
| `GET /token/admin/v1/overviews/token-resources` | `platform.tokens.read` | 仅路由三个点时指标及健康率分母 | 不接受查询参数 |
| `GET /token/admin/v1/overviews/model-operations`（Usage 有权时携带 `?window=24h|7d|30d`） | `platform.tokens.read` 或 `platform.usage.read` | 按授权组裁剪 Provisioning 或用量/核对/趋势；两组均有权时返回同一数据库事务的完整运行投影 | 有 `platform.usage.read` 时要求单值规范 `window`；token-only 请求不得携带窗口 |

三个投影的公共 metric item 固定 `additionalProperties:false`，required keys 恰为 `key,value,unit,aggregation,included_statuses,denominator_key`：`value` 是非负 int64，`unit="COUNT"`；点时指标 `aggregation="POINT_IN_TIME"`，窗口指标 `aggregation="WINDOW"`；`included_statuses` 是固定顺序闭合枚举数组；无分母时 `denominator_key=null`，健康/需关注路由固定为 `"stable_route_count"`。两个点时响应的 exact closed schema 为：

| 端点 | 顶层 required keys（`additionalProperties:false`） | metrics 固定长度、顺序与状态 |
|---|---|---|
| `model-catalog` | `schema_version:"1"`、`as_of`、`metrics` | 恰 3 项：`published_model_count/[PUBLISHED]`、`deprecated_model_count/[DEPRECATED]`、`model_draft_count/[DRAFT]`；三项 denominator 均为 null |
| `token-resources` | `schema_version:"1"`、`as_of`、`metrics` | 恰 3 项：`stable_route_count/[RESOLVED]`、`healthy_route_count/[HEALTHY]`、`route_attention_count/[UNKNOWN,PROBING,DEGRADED,UNHEALTHY,STALE,EXPIRED_HEALTH]`；后两项 denominator 为 `stable_route_count` |

`as_of` 必须是规范 RFC 3339 UTC 时间，两个端点都不接受任何 query；出现任意未知、空名、重复或带值 query 均在仓储调用前返回 400，稳定码分别为 `INVALID_MODEL_CATALOG_OVERVIEW_QUERY`、`INVALID_TOKEN_RESOURCES_OVERVIEW_QUERY`。不得忽略参数、返回宽松 additional properties 或复用运行窗口。

`model-operations` 也使用 `additionalProperties:false`，公共顶层 required keys 为 `schema_version:"1"`、`as_of`、`permission_view`，并使用以下唯一 `oneOf`；服务端只能从权威授权上下文选择变体，客户端不得请求或声明变体：

| 权限组合 | 固定响应字段 | 禁止返回/执行 |
|---|---|---|
| token-only | `schema_version:"1"`、`as_of`、`permission_view:"TOKEN_ONLY"`、`provisioning:{metrics:[provisioning_pending_count,provisioning_attention_count]}` | 不返回 `usage/window/trend`；不执行 Usage、核对案件或趋势 SQL 分支 |
| usage-only | `schema_version:"1"`、`as_of`、`permission_view:"USAGE_ONLY"`、`usage:{window,metrics:[usage_request_count,reconciliation_pending_count,reconciliation_stopped_count],trend}` | 不返回 `provisioning`；不执行 Provisioning SQL 分支 |
| both | `schema_version:"1"`、`as_of`、`permission_view:"TOKEN_AND_USAGE"`、`provisioning`、`usage` | 只执行两组已授权分支；两个组共享该请求的数据库 `as_of` |

数组项数、key、unit、aggregation、included_statuses 和 denominator_key 均闭合且顺序固定。无权组必须完全省略，不能返回 null、空对象、空数组或数值 0；两个权限均无时在仓储调用前返回无泄露 403。实现可使用一条带布尔授权参数的生产 SQL，但数据库执行计划必须证明无权 CTE/子计划 `never executed`、Actual Loops=0 且零 buffer；也可在服务端选取互斥的闭合 SQL 变体，任何方案都不得先查询再从响应删除。

模型、Token 和运行投影独立裁剪、鉴权、限时、监控与失败；“独立”不表示跨服务请求，它们仍由 Token 服务读取同一事实源。字段缺失、类型或状态集合非法、查询超时及仓储失败均使目标投影返回稳定错误，不返回部分 200 或伪 0；但不影响其他投影及页面列表。

三个新投影的后续实现必须扩展现有 `internal/platform/requestdeadline`，新增 `OverviewRead` class（稳定传输值 `overview-read`），并在 `BudgetFor`中固定 `Business=3s/BFF=5s/Ingress=7s`。`Classify` 必须先于通用“无 cursor 即 snapshot-build”规则，把三个精确 `GET|HEAD /admin/v1/overviews/{model-catalog|token-resources|model-operations}` 路由分类为 `OverviewRead`，未知 overview 路由不得被该 class 接受。这是本次待实现差距：当前代码只有 `snapshot-build/snapshot-read/precheck`，无 cursor 的新概览若不扩展会被错分为 10 秒 `snapshot-build`，不能冒称已具备 3 秒 overview 预算。

Portal 必须复用既有链路：在 `tokenAdminIngressDeadline` 删除浏览器提供的 `X-Pinvou-Absolute-Deadline` 和 `X-Pinvou-Deadline-Class`，以入口当前时刻调用 `requestdeadline.Start(now, OverviewRead)`，并由代理通过 `requestdeadline.ApplyToUpstream` 写入同一份 RFC 3339 Nano 绝对业务 deadline 和 class。Token 继续由 `TokenAdminMiddleware` 按路由选择 `OverviewRead` fallback，使用 `ConsumeBusinessDeadline` 消费：有效且更早的上游绝对期限只能缩短 Token 本地 3 秒预算，绝不能延长或重置；缺失、畸形、class 不匹配或较晚值仍按现有语义退回 Token 接收时刻起的本地 3 秒上限，因此必须以 Portal 始终成功 `Start/ApplyToUpstream` 的合同测试证明正常链路共享入口起点。不新增或虚构 `account-portal` workload 认证；只在现有受信代理拓扑、入口头清理与用户 Bearer 授权边界内消费这两个内部头。SQL statement timeout 必须取当前 context 剩余时间，不另加新的 3 秒。

`OverviewRead` 的 5 秒 BFF 和 7 秒 Ingress deadline 继续由现有 `Contract`/context 机制安装，仅作运输兜底，不重置 3 秒业务期限。Token `writeDeadlineError`/`WriteContextDeadlineError` 与 Portal `writeTokenAdminDeadlineError` 必须扩展为同时根据 `OverviewRead` 和规范资源路径映射错误：`model-catalog`、`token-resources`、`model-operations` 分别返回 504 `MODEL_CATALOG_OVERVIEW_TIMEOUT`、`TOKEN_RESOURCES_OVERVIEW_TIMEOUT`、`MODEL_OPERATIONS_OVERVIEW_TIMEOUT`。Token 已响应时 Portal 原样透传状态、稳定码、`request_id` 和安全响应头；BFF context 先耗尽时也必须以同一规范路径映射该专属 504，而非落入现有 class 默认 `QUERY_TIMEOUT`。浏览器和 BFF 不自动重试，用户显式重试才由 Portal `Start` 创建新合同。

稳定错误合同固定为：沿用 NFR-56 常规读取阈值，每个稳定用户按端点各有独立 `60 次/分钟` 桶，由受信入口规范化的来源 IP 在三个新端点间共享一个 `120 次/分钟` 桶，不得相信浏览器自报的转发头；任一桶超限返回 429 `TOKEN_OVERVIEW_RATE_LIMITED` 并携带服务端计算的 1～60 秒 `Retry-After`。不得为三个端点各建 120/IP 桶，也不得把各指标组另算请求。gate/仓储/容量不可用分别返回 503 `MODEL_CATALOG_OVERVIEW_UNAVAILABLE`、`TOKEN_RESOURCES_OVERVIEW_UNAVAILABLE`、`MODEL_OPERATIONS_OVERVIEW_UNAVAILABLE`；业务 deadline 耗尽分别返回 504 `MODEL_CATALOG_OVERVIEW_TIMEOUT`、`TOKEN_RESOURCES_OVERVIEW_TIMEOUT`、`MODEL_OPERATIONS_OVERVIEW_TIMEOUT`。非法、重复或对 token-only 不适用的窗口返回 400 `INVALID_TIME_WINDOW`。Portal 保留该数据源上次成功值并展示过期标识，429 倒计时结束前禁用重试，不把错误改写为 0。

既有 `GET /token/admin/v1/operations-overview` 保留其当前只读响应和权限合同，作为 UI-58 约束的 N/N-1 兼容入口至少延续至旧 `section=tokens` 的兼容窗口结束；新 Portal 的三个分区概览不得调用它。兼容入口不接受新字段或新写语义，弃用和移除必须走既有正式版本通知与回滚门禁。新投影复用现有事实和查询口径，不新增数据库事实、跨服务聚合或第二份指标来源。

性能门禁必须直接执行三个新 handler 使用的生产 SQL（或上述 `model-operations` 闭合 SQL 变体的同一生产常量），不得以旧 `/operations-overview` SQL、列表 count 或测试专用简化查询替代。沿用本节既有 PostgreSQL 17、2 vCPU、4 GiB、冷一暖三和 3 秒上限，并冻结以下附加数据：`model_catalog_versions` 恰 100 行；`model_catalog_entries` 恰 1,000,000 行，每版 10,000 行，head 版恰为 7,000 PUBLISHED、2,000 DEPRECATED、1,000 REMOVED；`model_catalog_drafts` 恰 50,000 行，draft_version 1/2/3 分布为 30,000/15,000/5,000，base_catalog_version 为 head/head-1/更早版本分布 30,000/15,000/5,000。`routing_policies` 恰 1,000,000 行，其中 800,000 RESOLVED、150,000 LEGACY_UNRESOLVED、50,000 DELETED；RESOLVED 中 400,000 HEALTHY 且 `health_expires_at>as_of`、80,000 HEALTHY 但已过期、80,000 UNKNOWN、80,000 PROBING、80,000 DEGRADED、40,000 UNHEALTHY、40,000 STALE；全部路由的 route_version 1/2/3 分布 500,000/300,000/200,000。seed 必须按数据库冻结 `as_of` 构造并逐项断言这些数量、版本、状态及健康过期边界。

计划门禁要求模型 SQL 对 head 状态集合只使用一次有界扫描并命中既有 `model_catalog_entries_status_idx`，版本 head 使用版本主键/唯一索引；草稿集合恰扫描一次，允许对冻结的 50,000 行使用单次 bounded Seq Scan 或主键 index-only scan，但不得重复扫描。路由 SQL 必须以一次扫描同时聚合 stable/healthy/attention，并命中新追加的具名部分索引 `routing_policies_overview_health_idx`（键为 `health_status,health_expires_at,id`，谓词精确 `reference_status='RESOLVED'`）；禁止对 1,000,000 行路由 Seq Scan 或分别重复扫描三次。运行 SQL 继续满足本节原三个逻辑集合及具名索引断言。保留卷重启后，每条生产 SQL 的第一次事实访问直接执行 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，随后连续三次暖执行；每次不超过 3 秒，且 token-only、usage-only、both 证明无权分支 Actual Loops=0/零 buffer。兼容旧接口继续运行原响应 schema、组合权限、指标/趋势口径和 SQL 回归，不能因新投影拆分而改变旧客户端结果。

目录差异预览返回的 observation 是本次 Adapter 发现结果的不可变规范投影。指纹必须覆盖同步实际消费的 Provider、Adapter 版本及每个发现模型的稳定 upstream 引用和规范显示字段，并按固定全序计算。同步写入只能采用以下一种闭合方式：复用该次冻结 observation；或在写前只重新发现一次、逐字段重算并与请求冻结指纹精确相等后，直接以这次已校验结果进入同一同步事务。禁止预检后再次发现第三份结果。任何新增、删除、改名、重复项归一或来源版本变化都返回稳定冲突，并保证目录、通用命令结果、审计与 Outbox 零写；响应不确定时只查询原 `command_reference`，不得重新发现或重发同步。

运行趋势 API 只接受单值规范枚举 `window=24h|7d|30d`，不接受客户端 `from/to`、重复窗口或大小写/空白变体。Token 在同一只读事务中以 `transaction_timestamp()` 冻结 `as_of`，并推导 UTC 半开窗口 `[from,to)`，其中 `to=as_of`、`from=to-window`；响应必须原样返回三者，Portal 展示及下穿筛选只使用响应边界，浏览器时间快慢、时区或 DST 均不得改变请求和结果。

桶合同闭合为：`24h` 使用 3,600 秒并恰有 24 点，`7d` 与 `30d` 使用 86,400 秒并分别恰有 7/30 点；第一桶从 `from` 开始，相邻桶首尾相接，最后一桶精确结束于 `to`，不生成额外部分桶。每点覆盖左闭右开区间，只返回受控非负整数计数；点数、桶宽、边界、顺序或总量校验失败时该来源进入局部错误，不能渲染旧数据。所有计数均为 0 时显示明确“所选窗口暂无数据”，不得用最小 2% 高度等视觉规则伪造非零柱。

APT-136 唯一拥有 Token 追加迁移 `000028_operations_overview_indexes`：建立 `usage_requests(started_at, id)` 全局时间/稳定 tie 索引，以及 `manual_review_cases(created_at, id)`（包含 `resource_id/status`）且谓词精确为 `domain='TOKEN' AND case_type='USAGE_RECONCILIATION_REVIEW' AND resource_type='USAGE_REQUEST'` 的部分索引。down 只删除这两个索引，不改写或删除事实。APT-123-R2 取得 `000029_token_account_owner_cap_policy`，APT-138 的 activation 迁移顺延为 `000031`。

性能门禁固定使用 PostgreSQL 17 Alpine 容器、2 vCPU、4 GiB 内存、`shared_buffers=512MB`、`work_mem=16MB`，并用确定性种子装载 1,000,000 条 `usage_requests` 与 100,000 条 `manual_review_cases`。用量中 700,000 条均匀分布在权威 `as_of` 前 30 日、300,000 条均匀分布在第 31～180 日；状态分布为 85% 终态、10% `SETTLEMENT_PENDING/REVIEW` 且未停止、5% `SETTLEMENT_PENDING/REVIEW` 且已停止。性能库必须完整保留生产迁移的 `domain='TOKEN'` CHECK，不得为造数删除或放宽约束；案件中 60,000 条满足精确 Token 用量核对谓词，另 20,000 条使用生产允许的其他 `case_type`、20,000 条使用生产允许的其他 `resource_type` 作为互斥合法干扰项，精确案件的 70% 位于 30 日窗口。种子阶段完成上述数量/分布断言并执行 `VACUUM (ANALYZE)`；保留数据卷重启受限 PostgreSQL 容器后，measure 进程对事实表的第一次访问必须直接执行生产 30 日单 SQL 的 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，在此之前不得运行 count、抽样或其他预热事实查询；随后紧接三次相同 SQL 作为暖缓存。冷缓存一次和三次暖缓存的每次 `Execution Time` 均不得超过 3 秒。

`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` 必须证明三个不同逻辑集合各自恰有一次有界索引扫描：30 日窗口用量使用 `usage_requests(started_at,id)`；不受窗口限制的 `POINT_IN_TIME` 待核对/停止计数使用既有 `usage_requests_pending_idx`（允许在该一次扫描内按受控状态和 stopped 字段聚合）；30 日精确核对案件使用新增部分时间索引。为验证案件 `resource_id` 对应权威用量事实而进行的主键等值 membership probe 可以出现，但不得重新扫描窗口集合或点时集合。禁止任一逻辑集合的重复 scan node、`Seq Scan`、逐桶相关子查询或客户端分页聚合；门禁同时断言索引名、实际行数分布、buffer 统计与迁移 up/down。

## 5. 权威 owner 候选与权限迁移

新增原子权限 `platform.tokens.owner-candidates.read`，只允许读取创建 Token Account 所需的最小投影：`id/type/display_label/status`。它不等价于 `platform.users.read`、`platform.organizations.read` 或成员目录权限。

该权限必须通过 **Account 追加迁移** 闭环交付，不得只改角色文档或依赖第 42 版“当时已有全部 PLATFORM 权限”的一次性授予：

1. `permissions` 幂等插入 `platform.tokens.owner-candidates.read`，scope=`PLATFORM`。
2. `role_permissions` 原子授予且仅授予 `PLATFORM_SUPER_ADMIN` 与 `PLATFORM_MODEL_ADMIN`，重复执行 `ON CONFLICT DO NOTHING`。
3. 对两个角色全部未撤销有效 `role_assignments` 调用 `bump_role_assignment_scope_version(user_id, role_code, organization_id, unit_id)`；同一迁移事务提交，使授权版本递增、旧缓存和旧 cursor 立即失效。
4. down 迁移先删除两个角色的授予，再对两个角色全部有效绑定递增版本，最后删除权限定义；递增查询按角色绑定本身执行，不依赖已经删除的 `role_permissions` 连接，确保撤权后缓存立即失效且不影响其他权限。
5. 迁移加入 manifest，并提供运行级测试证明授权版本变化、缓存失效和回滚行为。

逐角色验收矩阵：

| 角色/权限组合 | 候选读取 | Token Account 创建入口 |
|---|---|---|
| `PLATFORM_SUPER_ADMIN` | 允许 | 同时有 manage，允许 |
| `PLATFORM_MODEL_ADMIN` | 允许 | 同时有 manage，允许 |
| 其他平台角色、所有组织/个人角色 | 无泄露 403 | 拒绝 |
| 自定义组合：manage-only | 403 | 显示缺权限死路，禁用且不出现 UUID 输入 |
| 自定义组合：owner-candidates-only | 允许最小候选读取 | 没有 manage，隐藏/禁用创建 |

候选查询对主体每分钟 30 次、来源 IP 每分钟 60 次，使用 NFR-56 的候选 limit 和 cursor。Token 提交创建命令时必须重新向权威域解析 owner 状态、作用域和操作者权限，不能信任选择时快照。

## 6. 新增/轮换凭据的隔离写入与浏览器导航

本节仅适用于会接触 Secret 的 `TOKEN_CREDENTIAL_ENROLL` 和 `TOKEN_CREDENTIAL_ROTATE`。`TOKEN_CREDENTIAL_VALIDATE` 由 Portal 直接调用 Token 域脱敏校验命令，不接收或返回 Secret；`TOKEN_CREDENTIAL_REVOKE` 由 Portal 向 Account 创建审批申请，不启动 credential origin 隔离页。

### 6.1 固定 origin 与 CSP

APT-137 G0 按负责人批准统一新域，正式 Portal 固定 `https://account.pinvou.com`，UAT Portal 固定 `https://account-uat.pinvou.com`，分别只对应下表同环境 credential。旧 credential 域未上线，不增加兼容/重定向；handoff 按真实 `location.origin` 选唯一目标，正式 Portal 禁止降级本地 HTTP，原合法开发双 loopback 保留。脚本修改后由现有 `scripts/build-credential-handoff.mjs` 生成 hash 并检查，不手填摘要。除精确域与配对外，权限、会话、MessageChannel、一次 POST 及 CSP 安全合同保持不变。域名代码和 DNS 配置分别验收；后续发布脚本、部署模板、探针与环境门禁尚属独立交付计划，本组不包含其实现。新增域名记录见 [DNS 清单](../deploy/runbooks/model-token-dns.md)，本组不执行 DNS/证书或现场修改。

每个环境只允许一个经部署清单固定、禁止运行时任意覆盖的 credential origin：

| 环境 | credential origin |
|---|---|
| Production | `https://credentials.pinvou.com` |
| Cloud UAT | `https://credentials-uat.pinvou.com` |

Portal 的 CSP 必须由部署环境选择精确值，例如生产：

```http
Content-Security-Policy: default-src 'self'; form-action 'self' https://credentials.pinvou.com; frame-ancestors 'none'; base-uri 'none'
```

不得使用 `https:`、通配子域、调用方传入 origin 或动态重定向。Portal 另提供固定同源 `GET /credential-handoff`；URL 不接受 query、fragment、path 参数或任何 launch/result 引用。它只返回最小静态 HTML 与由专用构建步骤生成、字节冻结的内联 handoff 脚本，不加载 Portal App、第三方代码、字体、像素、遥测或 Service Worker。构建同时生成该脚本的 SHA-256 base64 摘要；CSP 仅允许以精确 `script-src 'sha256-<build-pinned-digest>'` 授权的该内联脚本，并禁止 `script-src 'self'`、`'unsafe-inline'`、`'unsafe-eval'`、动态 nonce 和运行时拼接脚本。响应不读写业务会话、不设置 Cookie，并固定返回 `Cache-Control: no-store`、`Referrer-Policy: origin` 与 `Content-Security-Policy: default-src 'none'; script-src 'sha256-<build-pinned-digest>'; form-action https://credentials.pinvou.com; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'`。handoff 在发送 READY 前及接收 BIND 后均必须验证 `navigator.serviceWorker.controller === null`；其注册 scope 不得覆盖 `/credential-handoff`。构建/部署门禁扫描 Service Worker 注册与 scope，Production/UAT 生产等价探针再从浏览器验证；若既有 root scope 会覆盖 handoff，必须先收窄/迁移 scope，否则发布失败关闭。credential origin 自身使用 `default-src 'self'; connect-src 'self' <exact-oidc-origin>; form-action 'self' <exact-oidc-origin>; frame-ancestors 'none'; base-uri 'none'`，且不加载第三方脚本、像素或字体。

### 6.2 Portal 同源 handoff 与顶层 POST launch

Chromium 对直接 `target=_blank` 的跨 origin 表单 POST 会发送不透明 `Origin:null`，因此该路径正式废止；Portal 不得直接向 credential origin 打开、fetch 或提交 launch。唯一允许的浏览器协议如下，版本冻结为 `credential-handoff/v1`：

1. 用户点击后，Portal 必须在同一可信用户手势栈中先以 `_blank` 打开固定同源 `/credential-handoff`。弹窗返回 `null`、未在 3 秒内进入 READY、任一阶段 `navigator.serviceWorker.controller !== null` 或部署检查发现注册 scope 覆盖 handoff 时关闭窗口并失败，且此时不得创建 launch；若 launch 已因并发响应创建，则必须立即按同一 `result_ref` 撤销。
2. handoff 仅运行 6.1 的最小静态文档。Portal 使用 CSPRNG 生成至少 256 bit handoff nonce 并创建 `MessageChannel`；不得使用 launch/result ID 充当 nonce。
3. handoff 先通过 `window.opener.postMessage` 发送闭合 `{version:1,type:"HANDOFF_READY"}`。Portal 只接受 `event.origin` 精确等于自身 origin 且 `event.source === popup` 的消息，随后以精确 Portal origin 为 `targetOrigin` 发送闭合 `{version:1,type:"HANDOFF_BIND",nonce}` 并只转移一个 MessagePort。
4. handoff 同样校验 `event.origin`、`event.source === window.opener`、nonce 格式和闭合 v1 schema，通过所收端口回复 `{version:1,type:"HANDOFF_BOUND",nonce}`。Portal 仅在 3 秒内收到 nonce 匹配的 ACK 后创建 5 分钟、单次消费、最多 3 次尝试的 launch 与不透明 `result_ref`。
5. Portal 只经绑定端口发送一次闭合 `{version:1,type:"HANDOFF_SUBMIT",nonce,launch_id}`；handoff 只接受 READY→BOUND→SUBMITTED 顺序、同一 nonce 和规范 UUID，并在 3 秒内回复一次 `{version:1,type:"HANDOFF_ACCEPTED",nonce}`。重复、乱序、额外字段、第二端口、来源漂移或超时均关闭端口/窗口；已经创建 launch 时原子撤销并进入稳定失败。
6. launch ID 仅可短暂存在于 Portal 运行内存、MessagePort 消息和 handoff 为提交即时创建的 DOM API 表单值中；不得进入可见 DOM 文本、URL/query/fragment/path、Cookie、local/session storage、IndexedDB、Cache API、window.name、剪贴板、Referer、Performance、遥测、控制台、访问日志或崩溃报告。handoff 不读取 `document.cookie`，不设置 Cookie，提交后不保留节点。
7. handoff 收到 SUBMIT 后先在内存中创建尚未挂载的临时 form 并写入唯一 `launch_id`，再发出 ACCEPTED；随后立即关闭 MessagePort、移除监听、清空 nonce/launch JavaScript 变量并执行 `window.opener = null`，最后才挂载该 form、以 `_self` 向部署白名单中的固定 `https://credentials.pinvou.com/launch` 提交并移除节点。body 只能是单一 `launch_id`。不得退化为直接 `_blank` 跨 origin POST、fetch、GET、query/fragment、iframe、同页 Portal 导航或复制链接。
8. credential origin 的 `/launch` 只接受 `application/x-www-form-urlencoded` POST，并要求浏览器 `Origin` 与当前环境唯一 Portal origin 精确相等；缺失、`null`、伪造、跨环境或带异常端口的 Origin 一律拒绝且不得消费 launch，Referer 永不作为授权证据。
9. Portal 在 READY/BIND 阶段保留 opener 仅用于精确 source/origin 握手；handoff ACCEPTED 后主动置空，credential origin 的首个 303 再以 COOP 强制隔离。除上述闭合握手外，双方不得读取或调用对方 DOM。
10. Portal 只把同一 `result_ref` 作为权威恢复引用。SUBMIT/303/响应丢失或结果未知时只轮询该引用，绝不重新 POST launch、生成新 handoff nonce 或换幂等键盲试；用户取消、权限撤销、gate 关闭、窗口提前关闭、任一阶段 3 秒超时或 10 秒内无首 heartbeat，均以适用稳定原因原子撤销已创建且未终态的 launch。
11. launch 被验证并原子消费后必须返回：

```http
HTTP/1.1 303 See Other
Location: /session
Cross-Origin-Opener-Policy: same-origin
Referrer-Policy: no-referrer
Cache-Control: no-store
```

`/session` 是固定地址，不携带任何 ID。首次 303 和后续所有 credential origin 响应都保持 `COOP: same-origin`、`Referrer-Policy: no-referrer`、`Cache-Control: no-store`。因为 POST URL 本身也固定为 `/launch`，重定向后的地址、Referer 和浏览器历史均不含 launch ID；反向代理、WAF、应用访问日志和崩溃报告禁止记录请求 body。浏览器刷新 `/session` 不得重放 POST。

弹窗或新页在 launch 创建前被阻止时必须安全失败且零 launch；创建后任一握手/窗口/heartbeat 阶段失败时原子撤销，显示对应稳定错误（无首 heartbeat 为 `CREDENTIAL_WINDOW_BLOCKED`）和“允许本站打开新窗口后重试”。不得改为 GET、query/fragment、同页降级、iframe、复制链接或把 Secret 收回 Portal。窗口已打开但认证失败时同样只经 `result_ref` 轮询稳定状态。

强制真实 Chromium 验收覆盖上述 11 点，并至少包含：直接 `_blank` 跨 origin POST 被测试明确禁用；弹窗阻止/READY 前失败零 launch；CSP 仅允许精确 SHA-256 hash 授权的内联脚本并禁止 `script-src 'self'`、`'unsafe-inline'`、`'unsafe-eval'`；`navigator.serviceWorker.controller === null` 且所有注册 scope 不覆盖 handoff，root scope 覆盖时门禁失败；双向 origin+source、CSPRNG nonce、闭合 v1 schema 与 MessageChannel ACK；重复/乱序/第二端口/超时在有 launch 时只撤销一次；handoff 无 App/第三方/遥测/Service Worker 且 launch 仅在内存、端口和瞬时表单；HANDOFF_ACCEPTED 后 opener 为空；credential `/launch` 收到精确 Portal Origin，缺失/`null`/伪造/跨环境均零消费；首次 303 `Location: /session` 且从该响应起 COOP/no-referrer/no-store；刷新及未知结果不重发 POST；URL、Referer、History、Performance、Cookie/storage/window.name/代理和应用日志均无 launch/result ID；弹窗阻断、取消、撤权、关 gate 有界撤销且无降级；换浏览器、换主体和重放失败关闭。

### 6.3 隔离会话

隔离页自行执行专用 audience 的 OIDC Authorization Code+PKCE 与 `prompt=login&max_age=900`，强校验 state、nonce、PKCE、auth_time、subject、origin 和 CSRF，并绑定环境、账户、动作、槽位、版本、浏览器和幂等摘要。Secret 只提交给 credential origin 同源端点，在 Token/Gateway 内存受控处理；Portal 只轮询不透明结果。

轮换状态为 CANDIDATE → ACTIVE、旧版本 RETIRING → REVOKED。撤销前必须检查活动路由、租约和在途请求；窃取、换浏览器、换主体、回放、超时或未知结果均失败关闭。

## 7. 影响预检、审计与命令治理

### 7.1 通用规则

模型生命周期、目录回滚、Token Account 状态、路由和凭据动作先读取只读影响预检，返回资源版本、关联路由/Provisioning/进行中请求、阻断和脱敏摘要。跨域预检只并列 Token 与 Billing/Account/Organization 投影，不宣称分布式原子；正式写入在提交时重新检查。

协议/公式草稿与发布、模型草稿与生命周期、目录同步/回滚、Token Account 创建/状态、路由、凭据、Provisioning 修复和用量核对都必须注册稳定 command type，并明确为 `AUDIT_ONLY` 或 `APPROVAL_REQUIRED`。未知命令、`force` 或 `ignore_blocker` 失败关闭。所有命令至少携带 reason、目标资源版本或创建哨兵、16～128 字符 Idempotency-Key；高风险动作执行近期认证及适用的双人审批、自审禁止。当前接口缺少其中任一字段时属于本需求的 API 合同增量，Portal 在对应合同完成前隐藏写入口，不能自行伪造。

模型和 Token Account 详情审计要求资源读取权限与 `platform.audit.read` 的交集。Token 基于本域既有不可变事实提供固定单资源过滤、脱敏分页投影；Portal 不拼接全局 Audit 列表，资源页签不隐式获得导出权限。

### 7.2 ADM-64 逐动作治理矩阵

“近期认证 15m”表示服务端只信 OIDC `auth_time` 且必须在 15 分钟内；“命令结果查询”是按操作者、command type 和幂等摘要读取已保存的脱敏结果，不是再次执行。所有审计均保存 actor、permission、command type、reason、幂等摘要、预检/关键版本、前后状态、结果和 request_id，绝不保存 Secret 或 Provider 私有信息。

| 稳定 `command_type` | 权限 | 治理 | 近期认证 | reason | CAS / 版本 | 幂等 | 影响预检 | 审计 | 未知结果恢复 |
|---|---|---|---|---|---|---|---|---|---|
| `TOKEN_PROTOCOL_DRAFT_CREATE` | `platform.pricing.manage` | `AUDIT_ONLY` | 否 | 必填 | `expected_protocol_head_version`，首次为 0 | 必填 | 协议代码冲突、已发布引用 | 通用审计 + 草稿版本 | 命令结果查询；不得重复创建 |
| `TOKEN_PROTOCOL_PUBLISH` | `platform.pricing.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_protocol_version` + 草稿 ETag | 必填 | 公式兼容、模型草稿引用 | 通用审计 + 不可变发布版本 | 命令结果查询并读取发布版本 |
| `TOKEN_FORMULA_DRAFT_CREATE` | `platform.pricing.manage` | `AUDIT_ONLY` | 否 | 必填 | `expected_protocol_version` + `expected_formula_head_version`（首次 0） | 必填 | 计量单位版本、协议参数映射 | 通用审计 + 草稿版本 | 命令结果查询；不得重复创建 |
| `TOKEN_FORMULA_PUBLISH` | `platform.pricing.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_formula_version` + 草稿 ETag | 必填 | 确定性预览、单位版本、Billing 同步可用性 | 通用审计 + Token/Billing 同步摘要 | 命令结果查询；同步不确定时按同一命令收敛 |
| `TOKEN_MODEL_DRAFT_SAVE` | `platform.models.manage` | `AUDIT_ONLY` | 否 | 必填 | `expected_catalog_version` + `expected_draft_version` | 必填 | 协议/公式存在及字段有效性 | 通用审计 + 草稿差异 | 命令结果查询或刷新草稿；冲突不自动覆盖 |
| `TOKEN_MODEL_PUBLISH` | `platform.models.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_catalog_version` + `expected_draft_version` | 必填 | 协议/公式、有效价格、READY 路由覆盖 | 通用审计；即时模式产生新目录版本，计划模式原子产生 `SCHEDULED` 计划 | 命令结果查询；即时模式读取目标目录版本，计划模式读取计划且到期前不得宣称目录版本 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` | `platform.models.lifecycle.request` | `APPROVAL_REQUIRED` | 15m | 必填 | `expected_catalog_version` + 生命周期计划版本 | 必填 | 价格、路由、Provisioning、在途请求、会话截止、替代模型 | 申请、批准、执行及生命周期审计 | 查询审批/执行和新目录版本；不得重提或 force |
| `TOKEN_MODEL_CATALOG_SYNC` | `platform.models.sync` | `AUDIT_ONLY` | 否 | 必填 | `expected_catalog_version` + Adapter 观察版本 | 必填 | 只读差异预览及连续缺失证据 | 通用审计 + 同步差异摘要 | 命令结果查询；未知时不得启动第二次同步 |
| `TOKEN_MODEL_CATALOG_ROLLBACK` | `platform.models.catalog.rollback.request` | `APPROVAL_REQUIRED` | 15m | 必填 | `source_catalog_version` + `expected_catalog_version` | 必填 | 全量目录差异、价格、路由、Provisioning、在途请求 | 申请、批准、执行及新目录版本审计 | 查询审批/执行和前向回滚版本；不执行破坏性回退 |
| `TOKEN_ACCOUNT_CREATE` | `platform.tokens.manage` + `platform.tokens.owner-candidates.read` | `AUDIT_ONLY` | 15m | 必填 | owner 权威版本 + 创建哨兵 0 + owner 当前账户数/上限/配额策略版本 | 必填 | owner 状态/作用域、同 owner 重名与配额 | 通用审计；不记录候选搜索词 | 命令结果查询并读取 account；不得盲目再建 |
| `TOKEN_ACCOUNT_STATUS_CHANGE` | `platform.tokens.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_account_version` | 必填 | 路由、Provisioning、在途请求与替代账户 | 通用审计 + 状态前后值 | 命令结果查询并刷新账户；未知时保持只读 |
| `TOKEN_ROUTE_UPSERT` | `platform.tokens.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_account_version` + `expected_route_version`（创建为 0） | 必填 | 模型/协议发布状态、凭据、健康、路由冲突 | 通用审计；upstream 始终脱敏 | 命令结果查询并读取路由版本；不得自动重试写入 |
| `TOKEN_ROUTE_DELETE` | `platform.tokens.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_account_version` + `expected_route_version` | 必填 | Provisioning、在途请求、剩余 READY 覆盖 | 通用审计 + 删除墓碑 | 命令结果查询并读取墓碑；不得因 404 推断未执行 |
| `TOKEN_CREDENTIAL_ENROLL` | `platform.tokens.manage` | `AUDIT_ONLY` | 15m（隔离页重验） | 必填 | `expected_account_version` + credential slot 版本 | launch/结果引用即幂等键 | 活动路由、槽位冲突及 Provider 能力 | Portal launch 与 Token 凭据元数据审计分离 | 只轮询 `result_ref`；不得把 Secret 返回 Portal |
| `TOKEN_CREDENTIAL_ROTATE` | `platform.tokens.manage` | `AUDIT_ONLY` | 15m（隔离页重验） | 必填 | `expected_account_version` + active credential 版本 | launch/结果引用即幂等键 | 路由/租约/在途请求、候选验证和切换条件 | CANDIDATE/ACTIVE/RETIRING 脱敏审计 | 只轮询 `result_ref`，按原状态机收敛；不得重启轮换 |
| `TOKEN_CREDENTIAL_REVOKE` | `platform.tokens.credentials.revoke.request` | `APPROVAL_REQUIRED` | 15m（Portal 申请时重验） | 必填 | `expected_account_version` + credential 版本 | 必填 | 活动路由、Provisioning 租约、在途请求及替代凭据 | 申请、批准、执行及 REVOKED 脱敏审计 | 查询 Account 审批/执行和 Token 凭据元数据；不进入隔离页，未知时仍视为不可用候选 |
| `TOKEN_CREDENTIAL_VALIDATE` | `platform.tokens.credentials.validate` | `AUDIT_ONLY` | 15m | 必填 | `expected_account_version` + credential 版本 | 必填 | 凭据状态和探测配额；不读取明文 | 只记录稳定结论/证据摘要 | 命令结果查询；不得重复探测或显示原始错误 |
| `TOKEN_ROUTE_PROVISIONING_REPAIR` | `platform.tokens.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_provisioning_version` + 目标代次 | 必填 | 仅 `REPAIR_REQUIRED`、依赖账户/凭据/路由状态 | 通用审计 + 新修复轮次 | 查询同一 Provisioning；不得创建并行修复轮次 |
| `TOKEN_USAGE_RECONCILIATION_RETRY` | `platform.usage.reconcile.retry` | `AUDIT_ONLY` | 否 | 必填 | `expected_usage_version` + 证据高水位 | 必填 | 仅待结算/复核状态、客户扣款禁止及当前证据 | 通用审计 + 证据高水位 | 按同一幂等键查询；只核对既有证据，绝不重发模型请求 |

`TOKEN_MODEL_LIFECYCLE_CHANGE`、`TOKEN_MODEL_CATALOG_ROLLBACK` 和 `TOKEN_CREDENTIAL_REVOKE` 是 ADM-38“治理命令通常只审计”的明确高风险例外。普通发布、创建、修复和核对为 `AUDIT_ONLY` 不表示低风险或可跳过预检。组织业务模型策略的 `TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE` 属于组织权限域，不进入本平台真实 Token 资源矩阵。

`TOKEN_MODEL_PUBLISH` 的受治理 canonical command 使用闭合 `publish_mode` discriminator，且不因模式改变治理分类：

- `IMMEDIATE`：`{model_id, publish_mode:"IMMEDIATE"}`；既有公开即时发布请求由 Token 边界规范化成该形式。成功结果只在同一事务完成真实草稿发布后返回新 `catalog_version`。
- `SCHEDULED`：`{model_id, publish_mode:"SCHEDULED", lifecycle_plan_id, lifecycle_plan_version, effective_at}`；`effective_at` 是数据库权威绝对时间且满足最小窗口。`expected_versions` 必须原样冻结 APT-124 发布预检的 `model_id`、`expected_catalog_version`、`expected_draft_version`、`draft_fingerprint`、`protocol_definition_id`、`protocol_version`、稳定路由数量/固定全序指纹、Provisioning 数量/READY 数量/固定全序指纹、`financial_fact_revision` 与 `price_set_fingerprint`；`draft_fingerprint` 覆盖基础目录版本、完整草稿内容、能力与别名，不能只冻结计数。命令持久化、通用结果与 `SCHEDULED` 计划行必须位于同一 Token 事务，成功投影闭合返回 `command_reference`、`command_type`、`lifecycle_plan_id`、`lifecycle_plan_version`、`status:SCHEDULED`、`effective_at`，此时不得返回或暗示新目录版本。
- 计划结果不确定时只能按同一 actor、command type、幂等摘要和 `command_reference` GET 恢复；无引用、404、无权、超时或网络失败均保持 `UNKNOWN` 且禁止用新键重建。明确零写错误才允许按原键恢复或按修改后的 payload 使用新键。取消使用独立幂等/CAS 将尚未执行计划置为 `CANCELLED`；到期 worker 重新逐字段校验冻结草稿与依赖，复用即时发布的目录锁、基础目录/协议/能力/别名校验、前向目录复制、草稿删除、审计/outbox 单一领域效果，成功后才产生并返回 `result_catalog_version`。草稿或依赖漂移置为 `INVALIDATED` 并保持目录零写；worker 租约过期可接管但不能重复发布。

### 7.3 三个强制审批命令的正式策略

现有 `TOKEN_MODEL_LIFECYCLE_CHANGE` 与 `TOKEN_MODEL_CATALOG_ROLLBACK` 的 policy version 1 已登记为 `AUDIT_ONLY`，属于不可变历史，**不得 UPDATE 或删除**。Account 前向追加迁移为两者各插入 policy version 2；`TOKEN_CREDENTIAL_REVOKE` 尚无策略，插入 version 1。策略发布后继续受现有不可原地修改/删除保护。灰度切换必须同时关闭旧 direct API 和启用新策略，不能出现 v1 direct 与 v2 approval 双写窗口。

| command type / 新策略版本 | 申请权限 | 审批权限 | 最低审批人数 / 自审 | 近期认证 | 申请有效期 / 执行窗口 | 执行身份 | 必须冻结的关键版本快照 |
|---|---|---|---|---|---|---|---|
| `TOKEN_MODEL_LIFECYCLE_CHANGE` v2 | `platform.models.lifecycle.request` | `platform.models.lifecycle.approve` | 1 名独立审批人；稳定用户 ID 禁止自审 | 申请、决定均 15m | 24h / 5m | `token-command-executor` | `model_id`、当前/目标 lifecycle、`catalog_version`、计划版本及截止时间、替代模型、Billing price version、路由/Provisioning/在途请求指纹、申请人与审批人授权版本、policy version |
| `TOKEN_MODEL_CATALOG_ROLLBACK` v2 | `platform.models.catalog.rollback.request` | `platform.models.catalog.rollback.approve` | 1 名独立审批人；稳定用户 ID 禁止自审 | 申请、决定均 15m | 24h / 5m | `token-command-executor` | 当前/来源 `catalog_version`、完整差异摘要、Billing price version、路由/Provisioning/在途请求指纹、申请人与审批人授权版本、policy version |
| `TOKEN_CREDENTIAL_REVOKE` v1 | `platform.tokens.credentials.revoke.request` | `platform.tokens.credentials.revoke.approve` | 1 名独立审批人；稳定用户 ID 禁止自审 | Portal 申请、Account 决定均 15m；不启动隔离页 | 24h / 5m | `token-command-executor` | `token_account_id`/account version、credential ID/version/status/fingerprint、活动路由/租约/在途请求指纹、替代凭据版本、申请人与审批人授权版本、policy version；永不含 Secret/密文/Key ID/Provider 地址 |

下列为迁移 SQL 与 `approvalpolicy.Policy` 必须逐字段复制的唯一目标值；`critical_resource_fields` 是 **exact ordered JSON key array**，不得重排、别名、增删或用对象代替：

```json
{
  "command_type": "TOKEN_MODEL_LIFECYCLE_CHANGE",
  "policy_version": 2,
  "approval_mode": "APPROVAL_REQUIRED",
  "requester_permission": "platform.models.lifecycle.request",
  "approver_permission": "platform.models.lifecycle.approve",
  "risk_class": "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
  "approval_count": 1,
  "self_approval_forbidden": true,
  "snapshot_schema_version": 2,
  "critical_resource_fields": ["model_id", "catalog_version", "current_lifecycle", "target_lifecycle", "lifecycle_plan_version", "effective_at", "existing_session_cutoff_at", "replacement_model_id", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"],
  "expires_after": "24 hours",
  "execution_window": "5 minutes",
  "executor_identity": "token-command-executor"
}
```

```json
{
  "command_type": "TOKEN_MODEL_CATALOG_ROLLBACK",
  "policy_version": 2,
  "approval_mode": "APPROVAL_REQUIRED",
  "requester_permission": "platform.models.catalog.rollback.request",
  "approver_permission": "platform.models.catalog.rollback.approve",
  "risk_class": "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
  "approval_count": 1,
  "self_approval_forbidden": true,
  "snapshot_schema_version": 2,
  "critical_resource_fields": ["current_catalog_version", "source_catalog_version", "catalog_diff_fingerprint", "billing_price_version", "route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version"],
  "expires_after": "24 hours",
  "execution_window": "5 minutes",
  "executor_identity": "token-command-executor"
}
```

```json
{
  "command_type": "TOKEN_CREDENTIAL_REVOKE",
  "policy_version": 1,
  "approval_mode": "APPROVAL_REQUIRED",
  "requester_permission": "platform.tokens.credentials.revoke.request",
  "approver_permission": "platform.tokens.credentials.revoke.approve",
  "risk_class": "GOVERNANCE_OR_NORMAL_STATE_MACHINE",
  "approval_count": 1,
  "self_approval_forbidden": true,
  "snapshot_schema_version": 1,
  "critical_resource_fields": ["token_account_id", "token_account_version", "credential_id", "credential_version", "credential_status", "credential_fingerprint", "active_route_set_fingerprint", "provisioning_lease_set_fingerprint", "inflight_request_set_fingerprint", "replacement_credential_version", "policy_version"],
  "expires_after": "24 hours",
  "execution_window": "5 minutes",
  "executor_identity": "token-command-executor"
}
```

六个权限目前均不存在，必须由同一个 Account 前向追加迁移幂等创建，原子授予 `PLATFORM_SUPER_ADMIN` 与 `PLATFORM_MODEL_ADMIN`，并对两个角色全部有效绑定递增 authorization version，使权限缓存、审批队列可见性与旧 cursor 立即失效。迁移同时追加上述三个策略版本；回滚只关闭 feature gate 和 direct 人类入口，不破坏性删除已被申请引用的权限或历史策略。超级管理员因此具备完整申请/审批能力，但同一稳定用户仍不能审批自己的申请。

Portal 对三个动作只向 Account `POST /account/admin/v1/approval-requests` 提交 command type、reason、幂等键和冻结输入；原有 `POST /token/admin/v1/models/{model_id}/lifecycle`、`POST /token/admin/v1/model-catalog:rollback` 以及任何凭据 revoke 管理路由不再接受人类 OIDC/委托，返回稳定 403 `DIRECT_APPROVAL_COMMAND_FORBIDDEN`，也不能通过内部 HTTP 伪造申请人。批准事务仅发布 `account.approval-command.authorized.v1`。

独立 ServiceAccount `token-command-executor` 只拥有 `internal.authorization.check`、`internal.approval-execution.begin/complete` 以及 `internal.token.model-lifecycle.execute`、`internal.token.catalog-rollback.execute`、`internal.token.credential-revoke.execute` 三个精确执行权限，无任何 `platform.*` 申请/审批权限。它消费授权事件后向 Account 强一致领取一次性执行租约，复核命令指纹、策略版本、双方权限/授权版本、有效期和全部关键快照，再通过非公网内部入口调用 Token 领域事务；任一变化使申请 `INVALIDATED`，未知结果只按 `approval_request_id + command_type + command_fingerprint` 查询同一执行，不重新发起。

交付时不能只插入数据库策略：必须同步更新 `internal/platform/approvalpolicy` 代码注册表，将 `TOKEN_MODEL_LIFECYCLE_CHANGE` 指向 v2、`TOKEN_MODEL_CATALOG_ROLLBACK` 指向 v2、`TOKEN_CREDENTIAL_REVOKE` 指向 v1，并将三个 command type 加入 `executableApprovalCommands` allowlist。数据库和代码必须逐字段相等：`command_type`、`policy_version`、`approval_mode`、`requester_permission`、`approver_permission`、`risk_class`、`approval_count`、`self_approval_forbidden`、`snapshot_schema_version`、`critical_resource_fields`、`expires_after`、`execution_window`、`executor_identity`。新增自动门禁要从真实迁移后数据库读取三个版本逐字段与注册表比较。allowlist 门禁必须保留现有 `WALLET_ADJUSTMENT`、`PAYMENT_PROVIDER_RESUME`、`PAYMENT_PRODUCT_RESUME` 及其他既有合法项；仅断言新增的 `TOKEN_*` 可执行集合恰为上述三项，不允许其他 Token 治理命令被意外放行。任一字段、有序数组、版本或 allowlist 差异都阻断发布。

### 7.4 APT-131 第三轮整改的服务所有权与冻结合同

身份 B 第三轮审核确认：结构化交互不能把缺失的 Token/Billing 权威合同实现塞进 Portal。原 APT-124、APT-130 的已审核交付保持 `DONE`，新增缺口按 `APT-124-R1 → APT-130-R1 → APT-131-R1` 三个具名整改批次顺序独立审核、独立提交；在前置批次通过前，相应 Portal 动作保持失败关闭。

**APT-124-R1（Token/Billing 预检所有权）**：

- Token 拥有命令目标解析、Token 本域依赖快照和组合 precheck；Billing 只拥有价格、公式、单位与覆盖事实的只读投影；Account 拥有 approval create 时的 snapshot 持久化与响应、submit 前重验、授权事件 payload 及 begin-execution lease 冻结投影。Portal 只能透传和展示，不得计算数量、版本、阻断、指纹或伪造成功。
- 协议草稿/发布、公式草稿/发布、目录同步、模型发布、模型生命周期和目录回滚所需的跨域预检，必须逐 `command_type` 使用闭合请求/响应 schema、同一个绝对 deadline、规范化输入和稳定失败码。请求或任一嵌套对象出现额外字段、错类型、未知 target 或不匹配资源时，在创建命令/审批前拒绝。
- 模型发布必须在同一个 Token Repeatable Read 快照内按草稿锁定的精确 `protocol_version_id` 读取 resolved 路由和 Provisioning；闭合 `expected_versions` 同时冻结 route/Provisioning/READY 覆盖计数与固定全序全量行指纹。没有稳定路由、没有对应 Provisioning、READY 覆盖不足，或提交前发生即使计数不变的路由/Provisioning 事实变化，均在 command 持久化前失败关闭。
- `billing_price_version` 是 Billing 在同一事务中返回的权威**完整价格事实修订号**：请求涉及的任一 pricing space、formula binding、unit config、block、生命周期或有效期事实发生变化时必须改变；禁止使用各 pricing space 独立递增版本的 `MAX(price.version)` 代替。Billing 同时对全部相关行按固定全序输出流式 SHA-256，覆盖稳定 ID、作用域、模型、协议、公式、单位、价格版本、配置类型/值、生命周期和有效区间；新增、删除、等量替换、非最大维度变化均必须改变 `financial_snapshot.price_set_fingerprint`。
- 生命周期 13 字段和回滚 8 字段的 `critical_resource_versions` 保持第 7.3 节 exact ordered key array，不以计数或当前页摘要代替。`financial_snapshot` 另行使用闭合 schema，至少冻结 Billing 修订号、价格集合指纹、覆盖结论和 Billing `as_of`；Account 在 approval create 时原子持久化并原样响应同一规范化 `critical_resource_versions + financial_snapshot`，submit 前重新读取权威源逐字段重验，通过后原样写入授权事件，并在 begin-execution 时原样冻结到 lease。执行领取前重新计算的任一变化使申请失效，不能只比较 13/8 字段中的局部最大版本。
- APT-124-R1 只交付权威预检、CAS/指纹、Account snapshot 持久/响应/重验及事件/lease 贯穿，不创建 APT-134 的生命周期计划，不启动 APT-138 的 consumer 或任何领域执行。

**APT-130-R1（运行领域命令适配所有权）**：

- `TOKEN_ROUTE_PROVISIONING_REPAIR` 的 target 只接受闭合字段 `provisioning_id`、`expected_provisioning_version`、`target_generation`、`replay_model_request:false`；`TOKEN_USAGE_RECONCILIATION_RETRY` 只接受闭合字段 `usage_request_id`、`expected_usage_version`、`evidence_high_watermark`、`replay_model_request:false`。布尔值必须显式为 `false`，不得省略、置真、字符串化或藏入额外字段。
- precheck、命令创建和执行前重验复用同一个逐命令解析器；必须验证稳定 UUID、目标资源类型、领域状态、版本/CAS、当前代次或证据高水位、actor/权限和请求摘要。错 command/target 组合、额外字段或试图重放模型请求在写 `PENDING` 命令前拒绝。
- governed result 只按冻结的 actor、command type、digest、idempotency key 和 `command_reference` 恢复。成功只代表创建了合规 repair/reconciliation 轮次或核对结果，绝不代表重新发起模型请求；reason 仅进入审计，响应继续使用 APT-130 的脱敏投影。
- APT-130-R1 不改 APT-130 已完成的分页、安全详情和异常队列，也不激活 APT-138。

**APT-131-R1（Portal/UI 所有权）**：

- Portal 在 Account 的权威 approval create、update 或详情 GET 响应通过校验后，将 `session + actor access-token fingerprint + approval_request_id + command_type + short TTL` 写入服务端分类证明；证明不得由客户端提交的明文 `command_type` 推导。直接绕过、伪造、过期、跨 session/actor 或未知分类均失败关闭。
- 总 gate 关闭后，approval submit/withdraw/decide 在动作代理前只查本地证明：`TOKEN_*` 为零次 Account 调用并拒绝；可信非 Token 证明继续代理。非 Token 的正常 create→submit 链必须由 create 响应立即生成证明，不能要求用户先额外 GET 详情；历史未分类申请只能在 gate 开启时通过权威 GET 建立证明。
- UI 只消费前两批的权威 precheck/command/result。可信 GET 返回 `SUCCEEDED`、`REJECTED`、`FAILED` 时分别收敛为成功或稳定终态；只有 `PENDING`、`UNKNOWN`、403/404 或网络失败保持未知。未知状态冻结原始引用、输入和幂等键，禁止无引用重写或换键盲试。
- 权限或 gate 撤销同步卸载对话框并 abort 在途 precheck/command/recovery；权限恢复不能复活旧输入。浏览器 smoke 必须由生产组件和客户端链路执行，绑定端口时显式排除 Chromium unsafe ports（含 3659），并连续运行至少 5 次证明稳定。
- APT-131-R1 不新增 Token/Billing 预检计算、领域 parser/CAS、执行事件或数据库事实；不提前实现 APT-133 Secret、APT-134 排程或 APT-138 激活。

### 7.4.1 APT-138 生产总装补充（2026-09-06，基础方案已获身份 B 审核通过）

本补充只细化已批准的激活合同，不改变十九项领域行为。三个整改批次已独立通过并提交；本批代码及迁移的默认状态仍为 PRE，实际环境激活属于 APT-137 发布编排。

当前实施状态为 `DONE / NOT_DEPLOYED`，已获身份 B 代码及本批验收整批最终独立 PASS；应用默认 PRE，未生产激活或部署。基础方案、首次执行投递和跨服务 epoch 细化已经身份 B 批准；已实现接线按块复审，领域实测发现的缺陷按独立整改提交处理，提交清单及剩余验收见任务文档。gateway 必须监督器异常退出（包括意外正常返回）须先关闭 readiness，再取消与 HTTP、metrics 及其他背景任务共享的进程 context，最终非零退出；正常进程取消不误报致命失败。context-owned HTTP 入口保留既有全部 collectors，不以监督器失败后继续存活的 HTTP 作为健康证明。

2026-09-06 已取得的阶段性实测（均为具名测试 exit 0、无 SKIP，不代表 APT-138 总验收 PASS）：

- CLI 候选 `ebcea29df0b2a35470335e589a343c096e175e17`：`TestTokenGovernanceTransitionPostgresIntegration` 10.64 秒，真实非超级用户 migrator、NOLOGIN 旧会话拒绝、ACTIVE/SAFE epoch 与事务排序通过。
- Account 快照链候选 `353c9401`：`TestApprovalSnapshotLifecyclePostgresIntegration` 4.57 秒，含并发首次领取仅一个 true、真实 Token 生命周期执行、重放和 worker；此项不替代三个 consumer 的独立链验收。
- 整合候选 `a37a27ae899dac78a6a2f26df602cc045ce6288e`：CredentialIsolation 9.15 秒、完整 TokenProtocolFormulaGovernedCommands 2.84 秒、GovernedFormulaMetering 2.07 秒、RevokeAudit 11.74 秒及十九项命令矩阵 19/19 通过。真实 NATS transport 用例 7.33 秒通过，但 Account/Token HTTP 为显式 fixture，只证明订阅、未知结果保留、SAFE 停止与新 epoch 恢复；三条真实领域成功链仍须另行验收。
- 小包 tokengovernance 以及 consumer HTTP 恢复/严格传输、绝对 deadline 和 supervisor 单元已通过阶段性固定快照。候选 `d42ff88f` 五个生产 main 编译全部 PASS；gateway 两个必须监督器单元包 5.687 秒，httpserver 全包 1.941 秒（含真实应用/metrics 双监听关闭及自定义 gauge 保留）均 exit 0。上述阶段性证据不得宣传为已生产激活；后续三条 consumer 证据见下，最终总门禁仍待完成。
- 三审批消费链最终阶段候选 `544e921a896a52f32dea068733f3dc3b7b3ff125`：`TestTokenApprovalConsumersPostgresIntegration` 27.59 秒，Lifecycle 4.19 秒、Rollback 19.34 秒、Credential 4.06 秒，全部 exit 0、无 SKIP。真实 Account/Token PostgreSQL、HTTP/JWT、正式领域 adapter 证明首次通用 audit/Outbox 各一条、重复投递完整十一表事实不变、领域响应丢失查询恢复，以及 Begin 响应丢失和旧 epoch 的零领域写入。报价与 activation 仍为明确 fixture，不宣称此项覆盖 Billing 或持久 CLI；回滚使用正式本地协议能力 adapter，不发送模型请求。候选 `e3ca2f6b` 真实 NATS transport 7.35 秒 PASS，只计传输层。
- 实测发现并修复 consumer 内 `json.RawMessage` 共用底层内存：预检解码不得引用冻结事件的可复用 slice；只有只读已存在结果路径借用冻结投影，新预检独立解码，严格事实比较不变。新格式差异单元在旧生产候选 `f50cb27f` 0.01 秒按预期失败，在修复候选 `e3ca2f6b` 0.00 秒通过；全部六个 consumer HTTP 场景共 0.03 秒通过。失败诊断只输出固定阶段码，保留未确认不 ACK、不盲重 POST，不传播原始错误或敏感字段。
- 身份 B 已给上述 consumer 完成块阶段终审 PASS；此结论不包含待独立整改的计划发布目录基线，也不代表 APT-138 整体 PASS。
- 后续独立 APT-134-R2 已签署 `cf0e04db`；整合候选 `59ce9862b71a8b723e4c3ff2c4f95631d267e559` 的 `TestModelLifecyclePlansPostgresIntegration` 2.77 秒 PASS，exit 0、无 SKIP，覆盖 worker 发布/弃用/下架、漂移拒绝及 expiry/lease。固定候选 `544e921a` 的治理命令 PG 1.66 秒、真实 activation transition 14.97 秒、十九项矩阵 19/19 和 handler manifest 亦通过。
- Consumer 阶段审核曾追加 `REOPEN` 精度项：普通 float64 JSON 比较会合并 2^53 以上相邻整数；本批只为 consumer 增加 UseNumber、单对象 EOF 的精确冻结对象比较，保留格式/键顺序无关与严格字段传输校验，不修改共享领域 helper。两个 supervisor 以单个 generation 对象显式拥有并关闭 context/cancel，状态变化先撤回 ready/取消旧代再建立新代，退出释放当前代，不以循环 defer 堆积或关闭 vet 隐藏所有权。两项均获 B 静态审核 PASS；固定候选 `6c86b421` 全量 go vet、精度及七项顶层 consumer/supervisor/epoch 单元通过，仍须纳入最终固定候选复验和整批终审。
- 固定候选 `6c86b421` 的 Token PG/NATS 批次已通过十九项矩阵、CredentialIsolation 3.07 秒、ProbeLease 10.56 秒、RevokeAudit 12.80 秒、CredentialCompletion 11.40 秒、Transition 29.94 秒、LifecyclePagination 9.62 秒、完整 Lifecycle 2.71 秒、治理 down 9.95 秒、CatalogRollback 11.11 秒、ModelAccount 13.99 秒及 GovernedRoute 13.95 秒；真实 NATS transport 7.34 秒仍仅计传输层证据。这一批次的独立 scheduled-publication 用例未通过：旧 fixture 没有安装新入口选择所需的 activation guard。已按 B 审核补精确 ACTIVE1 guard/context 和 ACTIVE2 完整事实零写负例，未修改生产；不能把本批次整体记作通过。
- Linux race 发现的移动认证测试替身竞态已独立为 APT-138-P2、签署 `de76c56d`。候选 `fd0b59a78f85749e8c540a64aa6c4c98e1567791` 仅 HEAD 加该批 doc/test，移动认证及密码重置权限各 `-race -count=20` 通过（1.131 秒、1.056 秒），Account 整包 `-short -race -count=1` 3.814 秒通过，B 最终 PASS；此项无 PG 环境，不代表 APT-138 全量 race。
- 后续候选 `032cd73b` 的 ScheduledPublicationBaseline 十一个子场景（含 wrong epoch 完整零写）15.00 秒、TokenProtocolFormula 8.74 秒、TokenAdminProtocol 2.07 秒、Runtime 16.03 秒，均 PASS、同批 exit 0、无 SKIP。浏览器候选 `168253477e50d765bed9614a39cce779d7a39ca9` 的 `TestCredentialOriginServicePostgresIntegration` 25.32 秒 PASS、exit 0，启用 `PINVOU_REQUIRE_CREDENTIAL_BROWSER=1`：五条完整 Chromium 流程、真实 DB 每账户一条凭据、撤销后精确 401、既有交换次数 6 不再增长及七表完整事实零写均通过。
- 固定候选 `cb1ab922793b75ca8f9c10ee8fea404f8bd26463` 包含 P2、已审 scheduled-publication fixture 和浏览器测试兼容修订。根代理核对其生产 Go 与上述 `032cd73b` / `168253477e50d765bed9614a39cce779d7a39ca9` 一致，差异为测试适配与文档；受限无网络 Linux 容器中全量 `go test -p 1 -short -race -tags=nomsgpack -count=1 ./...` 及 `go vet -p 1 -tags=nomsgpack ./...` 已通过，容器退出码 0。该快照不含后续 B 终审要求的凭据生产 guard 组合测试，新增测试已在 `635aa54acc6986b201394aa6c165d8d08918978a` 通过九项真实 PG（9.82 秒、exit 0、无 SKIP）及 Token 全 vet，包含 second_check_denied 0.02 秒的八表完整回滚、row_wait 1.61 秒和 Account 超时 0.49 秒；生产代码未变，B 已接受分层证据并给整批最终 PASS；保留各项候选归属，不混合快照宣称同一候选整批通过，实际激活和部署仍归后续 APT-137。全量 Node 最新为 376/378 通过、2 失败、0 SKIP（49.31 秒），仍为 browser-login-theme 与 foundation 既有断言，不宣称全绿，后续由 APT-137 处理。

凭据生产 guard 组合验收补充：既有自定义 guard 的凭据 PG/origin 测试不能证明新增生产组合。新增具名测试直接调用 `InstallProductionGovernance`，经真实 Token 数据库、正式 `TokenGovernanceStore`/行锁、正式 `authorization.Client` 和本地 Account 授权 HTTP 协议端点，验证冻结 actor、authorization version、issuer/subject、精确 manage/validate 权限和同一 epoch。用实际凭据 launch/session 操作证明成功与失败的持久效果；拒绝、版本/actor 漂移、过期 epoch、Account 超时以及先等待既有行锁再耗尽剩余绝对 deadline 均须精确零效果、释放锁。HTTP 权威响应可控 fixture 只用于故障注入，不替代 Account 本身权限计算 PG，也不替代已通过的非超级用户 CLI 状态迁移门禁。数据库沿真实 public 迁移，业务连接使用已登记的非超级用户 runtime；不修改生产安全逻辑。验收仍归同一 APT-138，九项真实 PG 与 B 整批最终审核已通过，当前 DONE/NOT_DEPLOYED。

1. **持久真相与运维边界**：Token `000031` 由受信 `token_migrator` 切换到 `token_schema_owner` 执行；runtime 无 CREATE、无 activation 写权限。单行状态记录与不可变转换历史由 NOLOGIN schema owner 持有，runtime 仅 SELECT；受信运维 CLI 默认只检查，显式切换须提交旧 state/epoch、十九项 handler 摘要、完整策略集合摘要、operator/reason/request_id 并通过同事务 CAS。只允许 PRE/0→ACTIVE/1、ACTIVE/n→SAFE_ROLLBACK/n+1、SAFE_ROLLBACK/n→ACTIVE/n+1；拒绝跳转、重复推进、回 PRE、缺证据和摘要不一致。CLI 使用受信 migrator 身份，不新增人类管理 HTTP 激活入口，也不从环境布尔值自动推进。首次激活/恢复之前检查实际 Account 策略表与 registry 全字段，以及实际安装的十九项 adapter 描述；摘要不是仅对数量或函数名称的自我声明。down 只接受从未激活且无引用的 PRE，保留所有已产生的历史。
2. **逐事务选择**：所有新写入口先读取同一持久 state/epoch/摘要，领域事务在首个事实写入前锁定该行并比对选定值，提交前继续持锁，CAS 与领域效果因而串行。矩阵包含通用 Begin/Complete 的 PENDING/终态、两项运行命令、三个审批效果、四项协议公式、账户 Saga、路由、凭据隔离和生命周期计划的 claim/expire/fail/apply。PRE 的兼容 direct 也携带 PRE/0，在 PRE→ACTIVE 竞态下不能越过事务重验。既有同请求终态 replay/GET 保持只读，不因 SAFE 阻断恢复；这些查询仍验证已知状态与摘要，但不得领取新租约、唤醒新写或重发命令。数据库不可达或未知状态/摘要失败关闭，不退回旧 direct。
3. **两个 worker 与 create ready**：生产 supervisor 只在读到已知 ACTIVE epoch 后启动 lifecycle 与 account-provisioning 两个工作循环；PRE 不启动。每个循环携带该 epoch，数据库租约仍是跨实例唯一执行依据。account worker 完成依赖配置与循环 ready 握手前，本实例 create capability 始终关闭；ready 不能替代每请求持久 ACTIVE 检查。SAFE 或任何读取不确定时先关闭本地 create，再取消循环，持有旧 epoch 的事务提交失败；恢复 ACTIVE 建立新 epoch 循环并重新 ready，不能复用旧 ready/lease。不引入 account provisioning NATS 订阅。
4. **跨服务权威与绝对截止时间**：Account 的 Token 审批 create/update/submit/decide/begin 在当前操作内强读 Token activation，不使用缓存或测试开关；历史幂等结果只读恢复保留。Account 与 Token 不宣称跨数据库 2PC：Account 读后切换的竞态由 Token 领域最终同事务 epoch guard 阻止效果，SAFE 下不再领取新执行。credential guard 在已持有 Token 行锁时调用真实 Account 权威源，核对冻结 actor/issuer/subject、所需权限和 authorization version，并在同一事务检查 gate epoch；一项操作只选一次绝对 deadline，所有下游与重验取剩余预算，超时失败关闭并释放行锁，禁止层层重新获得完整超时预算。
5. **审批消费**：独立 `token-command-executor` 使用既定最小服务身份，生产 NATS 按三个命令建立具名 durable consumer；共享授权主题中的合法非目标命令忽略，不影响其他领域消费者。ACTIVE 时才消费/领取，SAFE 停止新领取；事件必须逐字段校验冻结 command/financial/critical/policy 指纹，先权威预检与 Account begin，再调用对应内部领域入口，最后提交 Account complete。重复/乱序以原 approval+command+fingerprint 收敛；响应不确定只能查询已选执行，不能盲重发领域 POST。消费者和 worker 的生产注册、取消与错误退出均有具名集成证据。

**实现约束补充**：十九项 adapter 使用同一可执行 descriptor 登记驱动实际 dispatch 与摘要生成，不允许另造 ready 清单或仅手写十九个名称自比。上述 credential 绝对 deadline 在请求进入时冻结，行锁等待、Account HTTP 和 Token 事务均共用剩余预算，不能在 guard 内重新起计时。SAFE 中结果 GET 不调用会推进 Saga 的恢复逻辑；目标审批事件在 SAFE、依赖失败或结果仍不确定时不得 ACK 丢弃，只保持原引用等待只读恢复或合法重新领取。仅合法的非目标命令才忽略并 ACK。

**线性化及真实旧版本边界**：上述“旧 epoch 提交失败”精确指 CAS 先提交、领域事务随后取锁的顺序；若领域事务先取得 activation 行锁，它可在 SAFE CAS 之前提交，CAS 必须等待。验收保证绝无 SAFE CAS 提交后的旧 epoch 新效果，不把已提交的效果撤销。允许混跑的 N-1 最低能力必须已包含持久 activation reader/事务 fence，并对未知 state/摘要失败关闭；本批以前完全不认识 `000031` 的旧二进制不符合该最低能力，禁止与 ACTIVE 共存，不能用已支持 epoch 的 mock 伪装旧版本。首次激活及 SAFE 恢复的运维前置检查须真实确认 Token runtime 禁止新登录、所有可继承 runtime 的登录身份均已禁写、存量 writer 连接已清退并取得必要表锁；不满足则不执行 ACTIVE CAS。APT-137 必须先清退不兼容进程、验证固定候选镜像，再恢复登录并仅启动支持 fence 的实例；此为发布安全前置条件，不宣称迁移能够自动改造旧二进制。进入 SAFE 的 CAS 不依赖先清退新 N，直接由行锁串行化关闭写入。

**旧凭据永久 fence**：activation 记录额外冻结 `approved_runtime_roles`（排序、去重的当前候选发布数据库登录身份集合）。`token_gateway` 作为业务权限组永久 NOLOGIN；新发布为每个实际 Token 数据库 writer 使用独立登录身份继承其最小业务权限，不共用已退役凭据。CLI 在 ACTIVE CAS 前证明所有旧 writer 及候选身份均 NOLOGIN、无存量会话、持有完整十九项影响表集合的停写锁；不要求清空 PENDING/MANUAL_REVIEW 历史。切换后 APT-137 仅启用记录中的候选身份，旧身份永久保留 NOLOGIN，恢复也不得复用已退役身份。新 N 的持久 reader 必须验证当前 `session_user` 属于记录中的候选集合。所有真正直连 Token DB 的 writer（包括独立 relay/维护进程，若存在）均纳入盘点；只经 HTTP 的 executor 不获得 DB 写身份。CLI 不创建/输出密码、不自行开放 LOGIN，角色/凭据与镜像的实际变更归 APT-137。

**PRE 身份兼容**：首次 PRE/0 的候选集合为空；已审批部署的现有 runtime 身份可读取 PRE 状态并按既定 v1 兼容选择执行，不要求属于尚未激活的集合，create/新治理/新审批仍关闭。`approved_runtime_roles` 的成员校验从首次 ACTIVE 开始，对 ACTIVE/SAFE 都强制；永久禁用旧登录身份是 ACTIVE CAS 的先决条件，不在安装默认 PRE 迁移时提前切断兼容流量。

**配置效果与观察事实**：既有 default-route 内部工作流对账户状态、凭据 ACTIVE 和路由配置的效果仍须绑定选择出的 PRE/ACTIVE epoch，在同一事务内锁定并重验；SAFE 不产生这些配置变更。PRE 只保留既有账户配置兼容，旧自动创建 capability 继续关闭，不能绕过创建 Saga。纯路由健康探测、目录观测及正常模型调用保留原 workload 职责，不伪装成人类十九项命令，也不因 SAFE 误冻结观察事实。通用命令入口先冻结进入时刻；识别 `TOKEN_CREDENTIAL_VALIDATE` 后使用该时刻加 5 秒的同一绝对截止时间，再做凭据业务授权、预检、provider 和行锁操作，分类前已耗尽预算时零业务调用，其他命令不被强行改成 5 秒预算。

**数据库策略集合精确边界**：activation 的 `policy_set_digest` 固定覆盖十九项 governed Token 策略及既有 `WALLET_ADJUSTMENT`、`PAYMENT_PROVIDER_RESUME`、`PAYMENT_PRODUCT_RESUME` 三项 executable Billing 策略，共 22 项；逐项取当前期望版本并完整比较字段/有序数组，缺失或多出未知较新版本均失败关闭。`approvalpolicy.All()` 中仅供内部 workload 的正常状态机策略没有审批数据库行，不伪造这些行，也不使用“DB 与 registry 交集”跳过缺失项；executable allowlist 仍独立验证保全全部既有合法项。

**审批首次投递与未知结果（接线细化）**：Account execution 投影增加来自冻结申请的 `requester_authorization_version`，以及仅用于区分本次真正首次创建持久 execution 的 `first_claim`。`first_claim=true` 只能由首次 INSERT 证明；已有 EXECUTING/EXECUTED 或 FAILED 行重新领取均为 false，不能以 UPDATE 的 RowsAffected 或申请状态推断。三个 Token consumer 仅在首次领取响应明确且所有冻结证据一致时发送一次领域 POST；Begin 响应丢失、重复事件、领域 POST 响应丢失后均只按原 approval/type/fingerprint 查询结果，404 不视为零写许可。UNKNOWN 保留原 EXECUTING 申请与执行租约，在审批详情及其状态审计中可追踪；消费者持续输出稳定的 `TOKEN_APPROVAL_RESULT_UNCONFIRMED` 告警（含原审批引用，不含命令/密钥），NATS 不因重投次数耗尽 ACK/丢弃。管理员通过既有审批详情定位该引用并排查，不提供绕过原租约的“一键重发”。只有原结果终态确认才完成 Account；SAFE 只读、不新增领取、不发送领域 POST。

三个内部执行入口各增加同权限的 executor-only 权威预检及原结果 GET，严格只允许 `token-command-executor` 和对应命令执行权限；不扩大其既定权限集合，不增加 `platform.*` 或跨命令结果枚举。响应字段同步 OpenAPI，验收覆盖并发首次唯一 true、Begin/领域响应丢失、重复/乱序、SAFE 零 POST、原结果 GET 收敛，并验证既有三项 Billing 消费者不改变行为。此补充不引入新的审批领域状态或改写历史快照。

跨服务领域执行请求必须包含首次 ACTIVE 强读冻结的正整数 `activation_epoch`。Token 入口只接受与本次选择的 ACTIVE epoch 相等的值，并把相同选择传入全部写事务 guard；末次强读与 HTTP 发送之间发生 SAFE→新 ACTIVE 时，旧投递不能借用新 epoch 写入。该传输字段不修改冻结审批指纹或已有领域幂等键；不匹配时保留原审批与租约，后续只查询原结果。验收使用确定性 barrier 覆盖 ACTIVE1 领取后切换 SAFE2→ACTIVE3 再发送旧 epoch 的零领域效果，不依赖 supervisor 定时取消消除竞态。

运维前置：可信 `token_migrator` 在执行 ACTIVE 停写盘点期间必须具有 `pg_read_all_stats` 成员资格，以观察全部 writer 会话；普通 runtime 不得获得该权限。缺失时 CLI 明确失败关闭，不以不可见会话当作零会话。APT-137 部署编排负责审批并授予该可观测权限、完成检查/CAS 后按部署前状态撤销本次新增授权；APT-138 不自行变更生产角色。集成测试临时授予并恢复原 direct membership，不能污染后续真实非超级用户门禁。

### 7.5 APT-138 激活前的领域适配整改边界

2026-09-05 激活前只读审计确认，十九项策略注册、既有领域 handler 与 PRE 失败关闭并不等于十九项均已闭合接入通用命令事务。原 APT-122、APT-123、APT-125 的 `DONE` 与身份 B 审核历史保持不可变；当前缺口使用 `REOPENED_REMEDIATION` 具名批次修复，不把 adapter 缺口转嫁给 APT-138，也不以一次大提交抹平所有权。

| 批次 | 精确命令 | 冻结事务与恢复合同 | 旧入口合同 |
|---|---|---|---|
| `APT-122-R2` | `TOKEN_PROTOCOL_DRAFT_CREATE`、`TOKEN_PROTOCOL_PUBLISH`、`TOKEN_FORMULA_DRAFT_CREATE`、`TOKEN_FORMULA_PUBLISH` | 复用已审核 protocol/formula 领域方法与 Billing 单位校验；canonical envelope、权威 precheck、领域草稿/不可变版本、`token_command_results`、脱敏审计和 Outbox 必须同一 Token 事务提交。actor/command/key/digest 相同只重放首次安全结果，异请求冲突；网络不确定只按原 reference GET，禁止盲重发。公式发布继承同一 canonical command/T1 的 Billing binding receipt existing-first：Billing 成功而 Token 回滚/超时/丢响应时只以原 key/reference 收敛，异内容返回 409；Token 仅在完整 receipt 逐字段复核后同事务写不可变公式版本、结果、审计和 Outbox。 | 无 activation record 的 PRE 继续按已冻结兼容边界失败关闭；持久 reader 安装后，专用写入口必须与通用命令读取同一 state/epoch，`ACTIVE` 只允许通用治理路径，`SAFE_ROLLBACK` 与 epoch 漂移零写。 |
| `APT-123-R2` | `TOKEN_MODEL_DRAFT_SAVE`、`TOKEN_MODEL_PUBLISH` 的 `IMMEDIATE`、`TOKEN_ACCOUNT_CREATE`、`TOKEN_ACCOUNT_STATUS_CHANGE`、`TOKEN_MODEL_CATALOG_ROLLBACK` | 即时模型、状态和回滚复用既有权威 precheck/CAS，在同一 Token 事务提交领域事实、通用结果、脱敏审计和 Outbox；`TOKEN_ACCOUNT_CREATE` 是唯一例外，采用“Token reservation T1 → Billing wallet receipt T2 → Token account finalize T3”的持久可恢复 Saga，绝不把跨域调用宣称为原子。`SCHEDULED` publish 仍归 APT-134。目录回滚 adapter 只准备未来批准执行的单一领域调用和可查询结果，本批不得消费审批事件。并发重放隔离 actor，冻结首次结果，404/超时不得推断零写。 | 旧 draft/publish/status/rollback 写入口只允许保留明确的 PRE 兼容；账户创建因恢复worker未注册而在新N PRE下同时关闭generic/direct并返回稳定503。reader安装且APT138 ACTIVE、worker ready后统一选择Saga；`ACTIVE`不旁路，`SAFE_ROLLBACK`先关create且不恢复direct。 |
| `APT-125-R1` | `TOKEN_ROUTE_UPSERT`、`TOKEN_ROUTE_DELETE` | 复用稳定 `model_id/protocol_version_id`、路由版本、墓碑及精确影响证据；路由事实、通用结果、脱敏审计与 Outbox 同一 Token 事务提交，写前使用同一 activation transaction guard。等量依赖替换、epoch 漂移和 CAS 变化均零写；同键重放原 route/tombstone 结果。 | Portal 不再直写 v2 专用入口；PRE 的 v1 兼容边界不变，`ACTIVE` 只走通用 `/commands`，v2 专用写不形成第二条人类路径，`SAFE_ROLLBACK` 不复活 v1/v2 direct。 |

APT-125-R1 实现与复审说明（2026-09-06，`DONE / REMEDIATION_REVIEW_PASS`）：路由通用 adapter 与兼容仓储共用不自行提交事务的 Tx effect；通用分支只写一份 command result、脱敏审计及 Outbox，旧 wrapper 的 `routing_policy_mutations` 保留原兼容语义。account 级 session lock 在 Serializable 快照前取得；跨账户同 actor/type/key 的唯一键竞态只在真实 23505/40001 且失败事务释放后重读已有结果，精确异载荷返回 409，不把其他 SQL 错误伪装为幂等冲突。稳定 impact reader 在写事务中直接复用，选中的 legacy unresolved 行也按 account/route ID 纳入完整健康证据，不能预览 CREATE 却修改旧行。

UPSERT 的通用目标 ID 为账户 ID，command 仅含 `token_account_id/model_id/protocol_version_id/priority/default_scope`；DELETE 的目标 ID 为 route ID，command 仅含 `token_account_id/route_id`。预检返回 account/route 版本和稳定引用，envelope 额外绑定 precheck/request fingerprint。新建路由产生 UNKNOWN 等待服务端探测，不要求先有 HEALTHY/READY。删除结果冻结 `deleted_route_version`（删除前）及 `route_version/account_version`（删除后）、原 route generation 和同事务墓碑 `deleted_at`，不返回上游信息。无 reference 的响应丢失允许两类路由命令通过既有 GET `commands/by-idempotency-key` 按当前 `platform.tokens.manage`、actor/type/key 查询；账户创建仍使用其原双权分支。404 仍是未知结果，不允许据此重发写入。Portal 改动 priority/default_scope 必须中止旧预检、重新获取绑定新参数的证据，已发命令的结果校验上下文冻结，不随外部刷新变化。

实测发现的旧路径问题限定修复：隔离 PG fixture 的多 SQL seed 显式使用 simple protocol（生产连接协议不变）；两处历史路由 receipt 到期时间参数显式 `timestamptz`；历史 route 删除和 credential metadata 的 joined 查询消除歧义列。未新增迁移、未改变探测/Provisioning 领域状态机，也未安装或激活 APT-138。

三批固定按 `APT-122-R2 → APT-123-R2 → APT-125-R1` 独立实现、身份 B 审核与提交；真实 PostgreSQL、HTTP 和 Portal 回归必须逐命令覆盖成功、权限/CAS/版本漂移、并发幂等、响应丢失、旧入口 PRE/ACTIVE/SAFE 选择及审计/Outbox/结果恰一次；公式发布还必须覆盖 Billing 成功→Token 失败/回滚、同键完整回执恢复、异内容 409 及 Token 响应丢失只 GET。三批只补领域 adapter 和旁路关闭，不创建或安装 activation record/reader/transaction guard/credential authority guard，不向 Account 注入 activation 校验，也不注册审批 consumer或新的NATS subscription。APT-123-R2可实现数据库轮询的account provisioning worker/repository/lease seam并直接测试，但不得在生产main注册；生命周期worker同样不得注册。

本节记录的整改前置现已全部通过；APT-138 当前 `DONE / NOT_DEPLOYED`，已取得 B 整批最终 PASS，以下为持续保留的所有权和运行时约束。三个整改批次全部经身份 B `PASS` 后，APT-138 才唯一负责 `000031` 激活状态机、生产 reader/guard、Account activation 校验、三项审批 consumer、生产 NATS 注册，以及 APT-134 lifecycle worker与APT-123-R2 account provisioning DB-polling worker的生产 main 总装，并在同一候选提交上完成十九项总验收；不存在由整改批次提前切换一部分命令的中间状态。两个worker在PRE不启动，ACTIVE才注册/领取带epoch的租约，SAFE_ROLLBACK停止领取新租约且在手租约提交前重验失败关闭；重复实例只允许一个租约持有者，恢复ACTIVE不得重复领域效果。APT-137只验收灰度、N/N-1与部署等价证据，不承接该功能接线。

`TOKEN_ACCOUNT_CREATE` 的 owner 配额不从候选页数量或进程环境推断。APT-123-R2 的 Token `000029_token_account_owner_cap_policy` 建立“不可变 revision + singleton active pointer”共享权威模型：`token_account_owner_cap_policy_revisions` 只允许 INSERT，字段固定为 `policy_version BIGINT PRIMARY KEY CHECK (policy_version >= 1)`、`max_per_owner INTEGER NOT NULL CHECK (max_per_owner BETWEEN 1 AND 1000)`、`config_digest CHAR(64) NOT NULL`、`changed_by VARCHAR(128) NOT NULL`、`changed_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()`、`reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500)`、`request_id UUID NOT NULL`、`principal_digest CHAR(64) NOT NULL`、`idempotency_key_digest CHAR(64) NOT NULL`、`request_digest CHAR(64) NOT NULL`，唯一键为 `(principal_digest,idempotency_key_digest)`；`token_account_owner_cap_policy_active` 只含一条 `singleton_key BOOLEAN PRIMARY KEY CHECK (singleton_key)` 记录，字段固定为稳定 `policy_id UUID NOT NULL UNIQUE`、nullable `active_policy_version BIGINT REFERENCES ...revisions(policy_version)`、`pointer_version BIGINT NOT NULL CHECK (pointer_version >= 0)`、nullable `updated_by VARCHAR(128)`、nullable `updated_at TIMESTAMPTZ`，触发器保证 `pointer_version=COALESCE(active_policy_version,0)`。up 只创建空 revisions 与一条 `active_policy_version=null/pointer_version=0/updated_by=null/updated_at=null` 的 pointer，空指针明确表示创建能力未配置而不是无限配额。revision 本身是不可变配置历史；每次变更还在同一事务写既有 `outbox_events`，事件固定为 `token.account-owner-cap-policy.changed.v1`，以 pointer 的 `policy_id` 为 aggregate、`policy_version` 为 aggregate version，闭合脱敏 payload 为 `policy_version/max_per_owner/config_digest/changed_by/request_id`，现有 Token relay 直接以 `event_id` 唯一发布，不另建未接线事件表。

`config_digest`、`request_digest/principal_digest/idempotency_key_digest` 都要求 SHA-256 小写 64 位十六进制。配置摘要输入是精确 UTF-8 无空白 JSON `{"max_per_owner":<base10 integer>,"policy_version":<base10 integer>}`，按该字段顺序编码。请求摘要不含幂等键、request_id 或 actor，输入固定为 UTF-8 字节序列 `max_per_owner=<base10>\nexpected_active_policy_version=<null|base10>\nreason=<RFC8259 JSON string>\n`；reason 必须为 NFC、无首尾空白且 JSON string 采用 RFC 8259 最短转义。幂等键独立按原始 UTF-8 字节计算摘要并与从受信数据库会话派生的 principal digest 组成作用域。数据库 CHECK/触发器校验摘要格式与 revision 不可变性，应用/SQL 以公开固定向量交叉复核。

唯一写入口为 `token_cap_policy_deployer NOLOGIN` owner role 持有的 `SECURITY DEFINER` 数据库函数 `apply_token_account_owner_cap_policy(max_per_owner, expected_active_policy_version, idempotency_key, reason, request_id)`；函数声明精确 `SET search_path=pg_catalog`，所有业务表、序列、函数与类型均使用 schema-qualified 名称，扩展函数调用也绑定精确 schema/signature。迁移撤销 `PUBLIC` EXECUTE，只授予独立 `token_policy_operator LOGIN NOINHERIT` 执行；operator 不属于 owner role、对函数 schema 无 CREATE、对策略表无 DML。函数逐字节要求 `session_user='token_policy_operator'`，`changed_by` 和 principal digest 只由该认证主体派生，任意 payload、reason、GUC、`SET ROLE` 或其他登录身份均不能改变审计主体。独立 `token_migrator LOGIN NOINHERIT` 通过 secret 注入 migration URL，并仅作为现有 Token schema owner role 与 `token_cap_policy_deployer` 的成员以显式 `SET ROLE` 完成 up/down；它不得调用 apply。Token runtime `token_gateway` 不是 deployer 成员，只拥有 revisions/pointer SELECT，不得调用函数或 UPDATE/INSERT/DELETE。

函数可在锁前快读 existing result，但锁定 singleton pointer 后必须按 `(principal_digest,idempotency_key_digest)` 再执行一次 existing-first 并逐字节比对 request digest；命中相同 payload 时返回同一 revision/outbox reference/response，命中异 payload 时返回固定 409。未命中才校验 nullable expected active version、分配 `max(existing revision)+1`，并在同一事务 INSERT revision、写 `outbox_events`、CAS pointer。唯一键冲突必须回到原 key 的锁内读取/摘要比较，不得另建 revision；因此同 key/同 payload 并发恰有一条 revision事实与一条outbox事实且响应一致，同 key/异 payload 稳定 409，不同 key/同 expected version 只有一个成功，另一请求返回 `COMMAND_PRECHECK_CHANGED / 409`。首次 production bootstrap 必须从 null 以 expected null 创建 version 1；升、降、恢复旧 cap 值都追加更高 revision，禁止回写旧 version、删除历史或不升 version。提交不确定只以原键查询同一 revision/outbox reference。运行时每次创建从 DB 读取 active revision并复核 canonical digest；schema/active pointer 缺失返回 `TOKEN_ACCOUNT_POLICY_UNAVAILABLE / 503`，digest/FK/唯一 pointer异常返回 `TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH / 503`。普通 liveness/readiness 不因未配置 cap 下线整个 Token 服务；独立 `token_account_create` capability/readiness 只在 schema、单一 active revision、digest 和运行服务两表只读/无写权限均正确时为 ready，所有创建入口在其余状态失败关闭。真实迁移测试必须覆盖 ACL、PUBLIC/overload/schema hijack、伪造 actor、锁内并发三矩阵以及同事务恰一revision/outbox事实；Relay按NFR-07至少一次投递，consumer以`event_id + aggregate_version`幂等，重复投递不得产生重复效果。

发布/升级不宣称旧 N-1 writer 能理解 `000029`。“关闭并排空 create”是全 writer quiescence，不只是网关 HTTP 开关：逐项关闭管理员 generic/direct/PRE 兼容入口，停止 system/default-route/background creator；部署可先启动 create capability 关闭的 N 承接其他合法写命令，再排空并停止全部 N-1 write-capable 实例，或撤销其对 Token 账户事实的 INSERT/UPDATE 与创建函数 EXECUTE 权限。只有确认旧 writer DB 连接为零、创建队列/相关 PENDING/MANUAL_REVIEW 与租约均为零，才执行 `000029` up、由专用 deployment 身份显式 bootstrap，并验证 active revision、digest、全 owner 三状态计数/headroom；create capability继续关闭，直到Billing000053与Token000029就绪、APT138 `000031` ACTIVE且DB-polling worker注册/ready后才只向N打开。N-1 在整个窗口只能以数据库只读身份承担读流量，自动化必须证明其处理其他流量不会间接创建 PLATFORM account；停写后不得恢复create。回滚采用相同 quiescence：先停/排空所有创建主体；N-1 可恢复只读/其他无创建副作用的兼容流量，但 create 必须持续关闭，绝不得恢复旧无限创建。N/N-1 或 pending envelope 的旧 policy version 在写前按当前 policy 重验：已完成命令 existing-first 重放，未写命令返回 `COMMAND_PRECHECK_CHANGED / 409` 并要求刷新，禁止沿用旧无限策略。

发布前必须盘点每个 owner 的三状态 PLATFORM count 与目标 cap/headroom；`count >= cap` 的存量 owner 明确 grandfather，不阻断迁移但只读保持超限状态并阻止新建。down 顺序固定为先关闭/排空 create 并收敛或显式终结所有引用 policy version 的 PENDING 命令，再先执行 APT-138 `000031` down；只要 active pointer 非 null、存在任一 revision/outbox/audit、任一命令 envelope/result 引用 policy version，或仍有 N create writer 依赖该 schema，`000029` down 必须失败关闭。需要回 N-1 时保留两表/历史且 create 继续关闭；只有从未激活、无 revision/outbox/引用且无 writer 依赖的空 schema 才允许删除，不影响读流量。

计数集合固定为同一 `owner_type + owner_id` 下全部 `management_mode=PLATFORM` 且状态为 `ACTIVE/DISABLED/FROZEN` 的账户；三状态永久占用容量，当前无删除或释放容量语义。降低 cap 允许存量 `count >= cap` 继续存在且不得自动禁用/删除，只阻止新建；恢复/状态切换不改变容量。管理员 direct/generic、系统初始化与 default-route 等所有可创建 PLATFORM account 的路径必须取得同一 owner advisory lock，并在同一 Token 事务读取当前 policy/count 后 CAS 写入；不能接入该边界的路径不得创建 PLATFORM account。预检冻结 `platform_authorization_version`、`actor_lifecycle_version`、归一 `actor_status`、`actor_identity_digest`、`owner_lifecycle_version`、归一 `owner_status`、`owner_identity_digest`、`owner_account_count`、`owner_account_cap`、`owner_account_cap_policy_version` 与既有 canonical `creation_version:0`；三类版本与两个非PII摘要只存在 Account→Token 内部 authority envelope 和内部 precheck，公网 NFR-56 pagination envelope、candidate item、URL 与 DOM 均禁止返回。

错误/恢复矩阵固定为：`count >= cap` 返回 `TOKEN_ACCOUNT_CAP_REACHED / 409`，同 payload 不立即重试；count/policy/owner authority 漂移返回 `COMMAND_PRECHECK_CHANGED / 409`，刷新预检后使用新 envelope；policy 未配置返回 `TOKEN_ACCOUNT_POLICY_UNAVAILABLE / 503`，digest/schema/active pointer不一致返回 `TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH / 503`；双权限缺失保持无泄漏 `PERMISSION_DENIED / 403`，owner 不存在/不可见保持 `OWNER_NOT_FOUND / 404`。Token T1 以前的这些路径保持 wallet、account、command、audit、Outbox 全部零写；T1 之后则必须保留 reference 并按以下 Saga 合同恢复，不再宣称跨域后置失败零 wallet。同 actor/command/key 的既有结果必须在任何可变读取前恢复。

`TOKEN_ACCOUNT_CREATE` T1与恢复authority统一调用Account service-auth `POST /internal/v1/token-account-authority/snapshot`，只允许`token-gateway` mTLS+service token+窄权限，transport精确为`authority_schema_version, mode, identity_issuer, identity_subject, identity_subject_sha256, actor_user_id, owner_type, owner_id, required_permissions, command_reference, command_request_digest, expected_platform_authorization_version, expected_actor_version, expected_owner_version, expected_actor_identity_digest, expected_owner_identity_digest, authority_request_sha256`且schema version只接受`"1"`。四mode闭合互斥：`OPERATOR_AUTHORIZE`用于人工恢复第一阶段，identity三字段与URL reference非null，actor/owner/command digest/五项expected证据null；Account把reference仅作echo/digest绑定、不探测目标，同RR映射operator并只查ACTIVE+双权。`T1_SNAPSHOT`用于创建，identity/owner/reference/command digest非null，actor及expected null；Account同RR映射creator并读取owner visibility。`T3_CREATOR_RECHECK`只用于自动worker，actor和platform/actor expected+digest取reservation original creator active checkpoint，owner证据取reservation current owner checkpoint；`T3_OPERATOR_RECHECK`只用于人工owner-bound复核，actor及platform/actor expected+digest取OPERATOR_AUTHORIZE响应仅在当前可信请求内存形成的typed operator checkpoint，owner证据仍取reservation。两种T3 identity三字段null且owner/reference/command digest/expected非null，Account只验请求actor对应的本域事实，不能跨库证明Token reservation。OPERATOR响应只返回非null platform/actor evidence、owner evidence固定null；T1及两种T3返回三组evidence。公网candidate仍四字段且只作提示。raw subject/JWT/service token/auth_time不入日志、审计、Outbox或公网。Token T1随后在SERIALIZABLE事务做existing-first、authority证据、owner/policy锁、计数、expected与activation重验，通过才写内部reservation/PENDING result/audit/outbox；不写公开account或调用Billing。原三组T1证据不可变，active checkpoint仅可信漂移ALLOW后fencing CAS推进。T1 commit不确定返回UNKNOWN+reference，零写才可无ref重试；APT-138 ACTIVE+worker ready前create保持关闭。

authority request摘要固定为排除`authority_request_sha256`与raw `identity_subject`的RFC8785 JCS UTF-8，闭合对象仍恰15字段：`actor_user_id, authority_schema_version, command_reference, command_request_digest, expected_actor_identity_digest, expected_actor_version, expected_owner_identity_digest, expected_owner_version, expected_platform_authorization_version, identity_issuer, identity_subject_sha256, mode, owner_id, owner_type, required_permissions`。schema version固定`"1"`，reference始终lowercase-hyphen UUID，其他UUID/enum/version/digest按mode为规范值或JSON null，permissions固定排序双元素。OPERATOR要求identity非null、actor/owner/command digest/expected null；T1要求identity/owner/command digest非null、actor/expected null；`T3_CREATOR_RECHECK`和`T3_OPERATOR_RECHECK`都要求identity null且actor/owner/command digest/expected非null，mode分别绑定creator或operator证据来源，禁止互换。subject SHA绑定raw UTF-8原字节，Account先重算subject SHA再重建JCS常量时间比较；raw subject/JWT/token/auth_time不进入JCS、日志、审计、Outbox或公网。既有T1固定向量SHA保持`2766a2598657ccd2a69eea7f74816a92e0e056c4e36209868da1a3d541bde491`；同issuer/subject/reference/permissions的OPERATOR向量把command digest/owner置null后SHA为`76dc44d593cec9c4944faab4c94e98d5d67eae12b63ccd569c0cc38003d1e475`。固定T3_CREATOR JCS为`{"actor_user_id":"018f0c7a-6b5d-7e3f-8a21-1234567890ab","authority_schema_version":"1","command_reference":"018f0c7a-6b5d-7e3f-9a21-1234567890ac","command_request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_actor_identity_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","expected_actor_version":"7","expected_owner_identity_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","expected_owner_version":"11","expected_platform_authorization_version":"13","identity_issuer":null,"identity_subject_sha256":null,"mode":"T3_CREATOR_RECHECK","owner_id":"018f0c7a-6b5d-7e3f-aa21-1234567890ad","owner_type":"ORGANIZATION","required_permissions":["platform.tokens.manage","platform.tokens.owner-candidates.read"]}`（UTF-8 794字节，SHA-256=`af5fe3148c2d352552c1b54b62f70b16182ed469bd061aee30e5b573b75454ac`）；固定T3_OPERATOR JCS为`{"actor_user_id":"018f0c7a-6b5d-7e3f-8a21-1234567890ab","authority_schema_version":"1","command_reference":"018f0c7a-6b5d-7e3f-9a21-1234567890ac","command_request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_actor_identity_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","expected_actor_version":"7","expected_owner_identity_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","expected_owner_version":"11","expected_platform_authorization_version":"13","identity_issuer":null,"identity_subject_sha256":null,"mode":"T3_OPERATOR_RECHECK","owner_id":"018f0c7a-6b5d-7e3f-aa21-1234567890ad","owner_type":"ORGANIZATION","required_permissions":["platform.tokens.manage","platform.tokens.owner-candidates.read"]}`（UTF-8 795字节，SHA-256=`5d2038e2bea0487ea4ff23b7c6c1ad8366d9ddaf406078c8626e49f8044abba4`）。跨语言测试覆盖四mode null/来源矩阵、两个expected digest、raw subject联动SHA、creator/operator相同与不同仍不可跨mode误配、未知schema和所有语义变化。

APT-123-R2 同时唯一拥有 Billing `000053_token_account_wallet_provisioning_receipts`、内部 provisioning POST/GET 与 Token 账户 provisioning worker seam，不归 APT-138。Billing 用 command reference + owner identity + canonical request digest existing-first，同 key 同 payload重放原 receipt，异 payload 409；在一个 Billing 事务和同一 owner advisory lock 内执行 wallet/ledger Ensure 与不可变 receipt，响应丢失仅 GET receipt。Token DB-polling worker以reservation lease和至少一次语义调用；Outbox只供现有Relay审计/观测，不创建NATS订阅。Billing瞬时或不确定失败保持PENDING，确定失败在Token同一事务把reservation/result标FAILED、释放容量并写审计/Outbox。成功receipt返回后，Token finalize锁reservation并逐字段复核receipt，再重验当前authorization/owner与activation guard；版本变化走安全再预检，cap降低grandfather已合法reservation，撤权或owner失效终态FAILED，SAFE/epoch漂移不得写账户并保持可恢复。合法时同事务创建唯一ACTIVE account、标reservation COMPLETED、终结result/audit/outbox。Billing已创建的owner共享wallet即使Token最终失败也不删除；它不是可见Token Account，须保留receipt、告警和最终状态证据。

GET `/commands/{reference}` 只返回闭合脱敏 command result，但可 existing-first 幂等唤醒一次到期/无租约的恢复工作；不得重新提交命令。APT-123-R2 不注册 worker，故新 N 在 PRE 下必须让 generic 与旧 direct 创建统一返回 `TOKEN_ACCOUNT_CREATE_NOT_ACTIVE / 503`，Portal只显示维护不可用且不发POST；只有APT-138把`000031`切至ACTIVE、生产main注册worker且ready后才打开create capability。多实例以 lease owner/until、随机fencing token、activation/recovery epoch、reservation version与绝对Saga deadline串行；陈旧完成不得覆盖新租约。8次或30分钟耗尽时reservation进入仍占cap的MANUAL_REVIEW，command result保持PENDING并以安全recovery_state投影UNKNOWN/需人工处理，禁止创建第二个wallet/account。

若首次POST响应在reference到达Portal前丢失，恢复路径固定为 `GET /admin/v1/commands/by-idempotency-key?command_type=TOKEN_ACCOUNT_CREATE` + `Idempotency-Key` header。查询键只在服务端计算为`actor_user_id + command_type + key_digest`，不把原key放URL、响应或日志；当前actor须先重新通过创建双权限，缺权限统一403。授权通过后查询必须直接带当前actor条件，禁止先全局探测key：自己的事实返回200完整result/ref，自己的不存在与他人同key事实返回字节级等价404，只有该404允许同keyPOST；数据库/Account/activation不确定503且保持UNKNOWN零POST。同keyPOST仍existing-first。Portal状态机必须覆盖POST丢失→lookup 200、lookup 404→同keyPOST、lookup403/503→锁定，并以响应字节、缓存头和合理timing边界证明跨actor/不存在不可区分。

Billing `000053_token_account_wallet_provisioning_receipts` 闭合表为 `command_reference UUID PK, owner_type, owner_id, request_digest CHAR(64), status, wallet_id, stable_error_code, attempt_count, created_at, completed_at`。PENDING三终态字段均null；SUCCEEDED只有wallet/completed非null；FAILED/UNKNOWN只有安全stable_error/completed非null；终态不可改。POST body精确为command_reference/owner_type/owner_id/request_digest，不接收人类actor/tenant；新事实201、existing 200、digest/owner错配409、非法或额外字段422、瞬时依赖503。GET按reference为200/404/403/503。两端点只接受同时通过mTLS与service token的`token-gateway` workload，并分别要求`internal.billing.token-account-wallet-provision.execute/read`；403/404不泄漏其他workload的receipt。Billing同一事务锁owner，Ensure wallet/ledger与SUCCEEDED或确定FAILED receipt一起提交，传输丢失只GET。

T3使用120秒DB lease、5秒单次跨服务deadline、5秒起至300秒封顶退避、8次尝试和30分钟绝对`expires_at`。租约冻结随机fencing token、activation epoch、recovery epoch和reservation version，finalize必须逐字段CAS；SAFE停止新lease，在手任务guard失败保持PENDING，晚到receipt/T3只追加脱敏证据且不得自动激活。8次或30分钟任一耗尽都原子进入MANUAL_REVIEW、清lease并继续占cap；通用command result仍为PENDING，GET返回`recovery_state=MANUAL_REVIEW`和`TOKEN_ACCOUNT_PROVISIONING_MANUAL_REVIEW`，Portal呈现UNKNOWN/需人工处理。

MANUAL_REVIEW recovery subaction的可执行顺序固定为五段。①Token在任何目标存在性或下游读取前本地验证JWT issuer/audience/signature/auth_time/15分钟窗口，失败`RECENT_AUTH_REQUIRED/403`且零下游。②成功后调用`OPERATOR_AUTHORIZE`，传配置issuer、raw subject/SHA、固定双权与URL reference；Account同RR映射内部operator并验证ACTIVE+双权，不读owner、不探测reference，DENY时Token目标DB/Billing读仍为零。③ALLOW后Token以resolved operator授权上下文+reference做恒定形reservation lookup，内部取冻结事实但不对外暴露；未命中统一404且不调用Billing。④命中后把OPERATOR响应的platform/actor version/status/digest只保存在当前可信请求内存的typed operator checkpoint，结合reservation current owner checkpoint，由backend构造`T3_OPERATOR_RECHECK`；T3成功与最终Token事务前零action/draft/audit/Outbox写；actor必须是resolved operator而非creator，再校验operator双权/生命周期/owner visibility；owner不存在/不可见与本地未命中的最终404字节级同形且都不调用Billing；Portal不得传actor/owner/expected或authority JSON。⑤只有T3可信ALLOW且Token事务重锁reservation并CAS version、recovery/activation epoch与owner checkpoint全部通过，才同tx写action result、operator expected/observed checkpoint、双actor audit、Outbox并更新reservation，绝不覆盖creator frozen/active。T3拒绝/超时/owner不可见/TOCTOU零action事实，同key重试从OPERATOR开始；最终commit不确定返回action_id+UNKNOWN只GET，权威rollback+lookup404才可同keyPOST。通过后才按既有`POST /commands/{reference}/recovery-actions`的REQUEUE/CANCEL、独立key/reason、action result existing-first、双actor audit/outbox合同处置；原creator证据不被替换。raw subject/JWT不得进入日志、审计、Outbox或公网；action POST丢失继续按ID GET或operator/ref/header-key lookup，cancel必须权威查receipt后才释放。

action结果按已知ID读取`GET /commands/{reference}/recovery-actions/{action_id}`；POST响应在取得ID前丢失时，以原operator/reference和仅在header中的原key调用`GET /commands/{reference}/recovery-actions/by-idempotency-key`。服务端先验双权限/scope，再以operator+ref+action ID或key digest直接查询，禁止global probe；自己的200、无权403、他人/不存在字节级等价404、后端不确定503。无ID时仅权威404允许同key POST，一旦有action_id后任何PENDING/UNKNOWN只GET该ID。commit/response loss、跨operator与不存在等价、恢复零重复动作必须进入真实HTTP/Portal验收。

Account权威性不从HTTP状态推断。actor与owner统一归一为`ACTIVE|INACTIVE|NOT_FOUND`：真实ACTIVE映射ACTIVE；BOOTSTRAP_PENDING/PROVISIONING/REPAIR_REQUIRED/CLOSING及所有存在的其他非ACTIVE状态映射INACTIVE；版本化DELETED tombstone映射NOT_FOUND；物理缺行因无version只能视authority unavailable/PENDING。所有影响真实状态、归一状态或最小identity字段的Account写同tx递增对应`users/organizations/organization_units.version`并保留删除tombstone。actor digest仅哈希actor stable ID/normalized status/version，owner digest仅哈希type/id/normalized status/version，均不含名称、手机号或其他PII。

T3请求只能由Token backend构造并以mode绑定证据来源。自动worker使用`T3_CREATOR_RECHECK`：actor固定reservation original creator，platform/actor expected+digest取creator active checkpoint，owner expected/digest取reservation current owner checkpoint；人工恢复使用`T3_OPERATOR_RECHECK`：actor固定OPERATOR_AUTHORIZE resolved operator，platform/actor expected+digest取当前请求内存typed operator checkpoint，owner取reservation current owner checkpoint。两种mode进入同一15字段authority SHA，creator/operator即使ID/version相同也禁止互换。worker须先claim有效lease，人工须先OPERATOR成功+恒定lookup命中；无lease/伪payload/过期claim均零Account调用。Account只验请求actor对应的本域事实，不证明reservation。响应后Token同tx重锁并CAS reservation version、lease/fencing、recovery/activation epoch和对应checkpoint：CREATOR只可推进creator active checkpoint；OPERATOR证据仅在最终全部CAS通过后同tx写recovery action result/双actor audit/Outbox，不得覆盖creator frozen/active checkpoint；T3失败或TOCTOU零action事实，commit UNKNOWN只GET恢复。creator禁用可终结自动Saga但不阻合格operator人工恢复；operator在两阶段间撤权/变INACTIVE、owner漂移或响应后TOCTOU均零action/finalize。测试覆盖creator/operator相同与不同、两mode固定向量、creator disabled、operator漂移、owner漂移、lease偷取及双actor审计。

部署产物必须包含：使用 migration URL 的 `token_migrator` Job（显式 `SET ROLE` 执行全部 migration）、使用 policy deploy URL 的 `token_policy_operator` bootstrap/apply CLI、普通服务之外的 `token_account_create` capability/readiness，以及覆盖管理员、PRE、system/default-route/background、旧 writer DB连接、PENDING reservation/command和worker lease的 quiescence/drain 检查。开发 Compose 仅从 secret/env 创建测试登录凭据；生产角色与成员关系由外部特权引导预配并轮换，密码不入仓、不回显。真实门禁必须以非 superuser migrator/operator/runtime 分别执行 up、bootstrap、read/capability、down，并覆盖伪造主体、schema hijack、overload、撤权、连接未排空与 down fail-closed。

Billing迁移使用`billing_wallet_receipt_deployer NOLOGIN` owner和独立`billing_migrator LOGIN NOINHERIT` migration URL；migrator仅显式assume现有Billing schema owner/deployer执行up/down，runtime `billing_service`不是owner成员，只拥有receipt SELECT/INSERT及受控PENDING→终态UPDATE。cutover顺序为全部create writer/worker quiesce、Billing000053、只向N Billing路由新端点并验capability、Token000029、policy bootstrap且新N create仍关闭、APT138 000031 ACTIVE并注册/证明worker ready，最后才只向N Token打开create。SAFE先关create再停新lease；旧N停写后不再恢复create。down严格反序；N-1 Billing不接新端点，任何receipt/reservation/result/audit/outbox历史或pending/manual-review引用都使对应down失败关闭并保留只读证据。

APT-123-R2 实测审核补充：Account authority 响应增加 `owner_organization_id`（USER 为 null，ORGANIZATION 为自身 ID，ORG_UNIT 为 Account 权威所属组织）。ORG_UNIT 的 owner digest 绑定节点、所属组织、状态和版本；Token 的 T1、T3 与人工恢复均与命令/预占持久化归属比较，父组织非 ACTIVE 时不得 ALLOW，错配不得创建预占、钱包或账户。固定 request/JCS 向量保持不变。人工恢复 action 同时持久化操作者 expected 与 observed 授权版本、actor version/digest、owner version/digest，幂等范围为 operator + command reference + key。独立 MANUAL_REVIEW 队列提供服务端快照分页和操作者绑定游标，仅返回恢复引用与状态，不返回内部权威检查点或钱包信息。

Token/Billing 的数据库和业务 schema 分别归 `token_schema_owner` / `billing_schema_owner`（NOLOGIN）所有，runtime 不得为这些 owner 的成员；migrator 显式 assume schema owner。DBA 预装扩展，旧迁移可临时获得 schema CREATE，000029/000053 在创建受保护对象后回收 runtime、PUBLIC 和对象 deployer 的 CREATE，运行期不得依靠 database owner 或 schema owner 的隐式 CREATE 权限。策略函数仅调用 pg_catalog 的散列和 UUID 原语，阻止 public 函数替换或 schema 劫持。

APT-123-R2 回滚真实验收修订：影响预检的 `precheck_fingerprint` 仅绑定动作参数、Token 事实版本/依赖集合，以及 Billing 事实 revision、价格集合摘要和价格版本集合；独立快照的 `token_observed_at` / `billing_observed_at` 仅作为观测证据展示，不把重新读取的事务时间当作业务变更。审批后的首次执行对财务快照使用严格闭字段、类型与有效日期校验，并仅在事实等价比较中忽略 `billing_as_of`；原已审批快照和其观测时间仍冻结保留在审计中。生命周期生效时间、价格生效区间、租约/健康期限等业务时间仍参与各自事实摘要、业务校验与 CAS；实际版本、集合或动作改变仍须拒绝。

APT-123-R2 草稿漂移数据库状态机修订：Account `000047_token_approval_draft_invalidation` 仅替换 `protect_submitted_approval_request` 函数，保留 000018 的全部既有状态边和不可变字段检查，增加唯一受限状态边：OLD.command_type 属于 `TOKEN_MODEL_LIFECYCLE_CHANGE / TOKEN_MODEL_CATALOG_ROLLBACK / TOKEN_CREDENTIAL_REVOKE`，OLD.status=DRAFT、NEW.status=INVALIDATED 且 NEW.invalidation_code=`AUTHORITATIVE_SNAPSHOT_CHANGED`。已有 service 的失效处理仍同事务记录状态事件，原审批命令及快照不得改写。选择限定三类命令而非全局放宽，因为当前只有这些 Token 权威 snapshot 提交流程需要该路径；其他审批领域状态迁移不在本任务范围。down 恢复原函数定义且不删除任何请求、快照或事件。验证包含正向漂移、其他命令/原因码拒绝、不可变命令篡改拒绝及 down 历史保留。

本轮真实数据库验证同时修正执行实现细节：预检把纯观测钟当事实变化、审批 advisory lock 把含 NUL 的作用域字符串直接送入 PostgreSQL TEXT、UUID/text 混合 SQL 参数缺显式类型转换。锁作用域仍绑定相同主体/命令/幂等键，只将其 SHA-256 十六进制摘要作为锁参数；SQL 类型修正不改变领域事实或冻结历史。000047 原因码判断采用 null-safe 精确比较，NULL 不能借 SQL 三值逻辑放过非法状态边。

## 8. UI、可访问性与错误语义

- 英文枚举在中文界面转换为“已发布、已弃用、可用、对话”等标签，原枚举可置于 Tooltip。
- 资源名称进入详情，稳定 ID 为次要信息并独立复制；表格主要动作保留一个，其余进入“更多”。
- 高风险操作使用结构化面板，显示目标、影响、原因、生效时间、资源版本、幂等和未知结果恢复；移动端使用全屏面板，触控目标至少 44×44 CSS px。
- 三个列表页首屏按第 2.4 节分别展示模型目录、路由健康、运行与用量概览；不得重复跨域模块或跨 section 下穿。指标由对应页面级投影聚合并明确点时/窗口/时区/分母，403 隐藏、失败显示不可用，不扫描列表计算。
- 错误必须保留 `request_id`，区分 400 输入、403 无权、409 版本冲突、410 cursor 过期、422 筛选过宽、429 限流、503 容量/快照不可用、504 查询/预检超时。
- 任何错误、DOM、日志、遥测和审计都不能包含 Secret、密文、Provider Key ID、私有地址、原始错误或无权深链 ID。

## 9. 分期、兼容与回滚

一期：三个 section 和 URL 状态、逐区块独立加载、全部列表服务端分页筛选、协议/公式、owner 选择器、路由模型选择器、Provisioning/用量详情、核对异常队列及结构化高风险面板。

二期：模型/Token Account 独立详情、隔离凭据新增/轮换、Token 直接脱敏校验、Portal→Account 审批撤销、生命周期依赖与计划生效、模型上下文价格与计量、目录差异/健康/异常趋势。

每个能力有独立 feature gate，服务与 Portal 支持 N/N-1。数据库使用前向兼容追加迁移；回滚先停新写和隔离凭据会话，再回滚 Portal/服务。未消费 launch 立即失效，旧 cursor 返回 410，不得在回滚期间重新启用 query 凭据通道。

2026-09-11 增补的 UI-59/ADM-66 概览整改不属于既有 APT-136 `DONE` 证据，当前仅完成需求与设计修订，尚未创建实施任务、修改代码、提交、推送或部署。后续必须先建立相互独立且可逐项提交的任务，再按服务端投影、Portal 分区接入和验收门禁的依赖顺序实施；旧聚合入口在兼容窗口内保持可回滚。

### 9.1 APT-137 G1 离线计划与 envelope 边界

G0 已固定新域，G1 提供既有十三 gate 的闭合计划 schema 与依赖验证，不添加新业务开关。候选 commit/镜像/配置/迁移/策略/handler 摘要、环境和 APP_ENV、runtime/retired/运维身份声明、INTERNAL→LIMITED→FULL 阶段和正值观察/SLO 参数必须完整；缺失、类型强转、非法角色/旧域/错环境和不满足既有 gate 依赖均失败。真实 PRE 初始摘要为 null；CLI 的空字符串只能在 PRE/0 明确归一，不将缺字段或读取失败解释为 PRE。

证据 envelope 只核结构、候选/环境绑定、基本时间窗口与执行/复核者分离，不能证明原始报告存在、签名来源、当前状态或指标真实。默认 CLI 只读输入、不联网，输出 PLAN_VALID/ENV_PENDING/NOT_DEPLOYED，未提供 apply/GO。精确用法和后续未交付边界见 [发布计划说明](../deploy/runbooks/model-token-release.md)；完整原始字节/签名/观察验收留 G5，部署/运维/探针及 CI 留其他后续组。

### 9.2 APT-137 G2 可选部署与最小网络边界

Compose 显式 overlay/profile 和 Helm 可选 Deployment/Service/ServiceAccount/挂起治理 Job 复用现有二进制，不增加领域执行器或重复 gateway 双 worker。独立 Secret 与 workload 文件只给所需服务，runtime 不得获运维 URL，executor 不得获数据库/KMS身份；Token Relay 复用 gateway 同代数据库 Secret。所有新治理事件客户端使用同一外部 TLS NATS，开发匿名总线不可作为受控配置，服务器 ACL/历史仍需环境证明。

新受限 workload 从既有宽 NetworkPolicy 选择器中排除；checker 检查全部实际命中规则的允许并集、精确组件/端口/有界CIDR，而不是只信策略名。Compose 规范化 IPv4/IPv6/mapped 地址、协议与端口范围后拒绝重叠；治理只允许固定只读入口与空参数，拒绝附加容器/initContainer/shell 的自动执行路径。新配置的 OIDC/new origin/门禁规则失败关闭，默认不启动或放量；公开 values/env example 不可部署。完整使用说明见已交付发布说明的 G2 节；角色准备/正式入口与实际来源证据由后续 G3～G6 交付，不能只凭渲染称生产等价。

### 9.3 APT-137 G3 角色与操作的线性化边界

独立运维工具默认 inventory，只读强核真实数据库/迁移账本；缺31只能 NOT_INSTALLED，不伪造 PRE。prepare/fence/enable 修改前核验全部显式目标并串行持有事务 advisory lock，已安装31时持 activation 行锁到角色修改提交；SAFE先提交则旧epoch不能enable，先持锁的enable可先提交，不能把发出SAFE请求当成已停写。退休登录 NOLOGIN提交后清会话为明确两阶段例外，只终止批准角色，不扫描后任意全局ALTER。

旧卷仅受控迁移 DB/public owner，普通领域owner/历史数据不动；检查所有受影响控制角色/递归成员的跨库依赖与非当前PID会话。候选必须真正继承业务组USAGE而非仅MEMBER，不得继承控制owner。SCRAM verifier支持/有效期只返回布尔，不读取密码；真实认证/Secret仍需现场证明。enable首次同epoch/集合全部NOLOGIN才全量LOGIN；全部已LOGIN重试仅确认，混合/跨epoch拒绝。Account只读操作目的不等于SELECT-only身份，需受信短期SELECT+UPDATE/SHARE锁权限，runtime不得继承。

原CLI包装器以绝对候选制品hash/闭合参数/无shell/最小env执行一次明确操作，DSN查询参数不得重绑定库/身份/主机或注入启动选项，原stderr不传播，UNKNOWN仅原request-id/账本查询。运维主体和运行主体分离，SAFE需匹配旧摘要控制制品，不支持任意N/N-1摘要升级。具体调用见已交付发布说明G3节；无自动激活、部署或新领域行为，后续环境和G4～G6仍分离。

### 9.4 APT-137 G4 代理与只读浏览器的分层边界

固定新双origin结构校验与三段nginx渲染不安装站点。edge仅隐藏上游HSTS并输出唯一固定值，保留应用阶段Referrer/CSP/COOP/Cookie/Location；raw request_uri精确校验，nginx自产错误按原状态返回安全头，proxy_intercept_errors off不截改应用401/403/303。仅loopback/受控加密隧道或校验证书SNI的HTTPS上游，可信Forwarded/Host/Tunnel边界不清理真实Origin/Referer/Fetch元数据，日志不得含秘密或完整请求体/参数。

显式只读HTTPS probe仅固定目标、无Cookie/Auth/POST、严格TLS/无重定向、限body与绝对deadline。Chromium在唯一fresh page导航前禁网页JS并严格拦截请求；正式HTTP客户端先取得并验证全部冻结HTML/CSP/hash、拒Link/Refresh/reporting等被动通道，之后才重放安全白名单响应给浏览器。既有Service Worker不禁用/不清理，controller/scope检查只证明该fresh context，不证明所有用户或完整生产握手。实际脚本完整READY/BIND/SUBMIT由受控VM正负矩阵另证，TLS/浏览器scope/实际POST语义分层，不相互替代。现有完整生产等价握手合同不缩减，现场仍需真实IdP/身份/日志与一次性隔离资源。

调用说明见 [nginx入口与探针](../deploy/nginx/README.md) 和发布说明第7节；G5来源签名与G6持续CI仍为后续交付。

## 10. 发布门禁

实现阶段必须提供以下自动证据：

1. model-only、pricing-only、token-only、usage-only、manage-only、owner-candidates-only、超级管理员和模型管理员权限矩阵。
2. owner 权限追加迁移的创建、精确角色授予、授权版本递增、缓存/cursor 失效和 down 回滚测试。
3. 合法 UUID 无权、非法 UUID、互斥详情、无权页签的零详情请求、零 DOM/日志泄漏测试。
4. 三类唯一预算、入口代理超时顺序、快照事务中断无半结果、容量边界、70%/85% 水位、第三资源回收和并发翻页无重复漏项测试。
5. 固定 Portal `/credential-handoff` 与 credential origin 的真实浏览器测试：完整覆盖 6.2 的 11 点握手、构建期固定脚本 SHA-256 CSP、`navigator.serviceWorker.controller === null` 与注册 scope 不覆盖、一次性 `_self` POST、精确 Origin、opener 生命周期、首个 303 后 COOP/no-referrer/no-store、无引用泄漏及失败有界撤销；root scope 覆盖和直接 `_blank` 跨 origin POST 均必须阻断发布。
6. 逐区块失败隔离、URL 前进后退、旧 `section=tokens` 兼容、移动端与键盘可访问性测试。
7. 从真实迁移后数据库对三个强制审批策略与 `internal/platform/approvalpolicy` 逐字段（包括有序 `critical_resource_fields`）比对；`executableApprovalCommands` 保留所有既有合法项，且新增 `TOKEN_*` 子集恰为 lifecycle v2、rollback v2、revoke v1 三项，无其他 Token 命令被放行；任一偏差阻断发布。
8. 现有管理后台合同测试在后续实施任务中更新：feature gate 开启时断言三页为 canonical，关闭时仍断言历史 Token Gateway；本轮不修改实现测试。
9. UI-59/ADM-66 的后续实现必须验证三个页面仅出现本区概览、任一指标下穿不改变 `section`、详情态不渲染列表概览、时间选择器只在有 usage 权限的运行页存在；model-only、token-only、usage-only 权限分别只请求并显示允许投影，局部 403/503/504 不阻断列表，前进后退恢复筛选/适用窗口；旧聚合入口在兼容窗口内保持原合同，新 Portal 零调用；简中/英文/日文、键盘、320px、200% 缩放和 reduced-motion 全部通过。

任一门禁缺失时不得把需求状态标记为已实现、不得推送生产部署。

### 9.5 APT-137 G5 原始证据与离线判定

三个职责模块分别负责受控文件/签名验证、版本化 kind/case 策略和报告/观察/保留期语义，仅依赖 G1 纯校验器及 Node 内置密码学，不 import 其他发布 gate 的顶层副作用。Ed25519 签名使用报告专属外部信任与固定 domain separation，绑定计划 SHA、来源和 envelope；不借用 TOK-37 客户端 readiness 的 key/claims 或签发权限。bundle 必须在受控根内，trust 在实际根外，逐个工件真实字节有界读取和 SHA 核验；签名来源负责工件与事实的真实性，路径外置本身不等于组织独立可信。

十七 kind 完整且恰一份、十九命令逐项及版本化 case/验证层级不可由报告自行缩减；FAIL/SKIP/非零退出/缺工件/错环境不得通过。原始 JSON 数字先按数学值拒绝舍入/下溢，保留等价十进制表示；实际观察末阶段 end 绑定报告 end 与外部最长 24 小时新鲜度，不允许新签旧窗口刷新年龄。六指标均 <= advance_max 且样本/窗口 >= 下限才可推进，advance_max < 值 <= pause_above 或样本/时长不足 HOLD，任一 > pause_above、未授权效果 > 0 或 FAIL 优先 NO_GO。

真实 GA 未登记就保留旧链接，不以提交时间或预发布版起算；至少 90 天或两个后续正式稳定版本取更长。既有导航纯函数和安全 return URL 回归保留原行为，支付复用既有 false 合同，不改 Go/登录/支付/cursor 语义。调用和文件限制见 [发布说明第8节](../deploy/runbooks/model-token-release.md#8-g5-离线证据与兼容验收)；本地 fixture 签名不能作为正式环境报告，G6 与部署证据继续独立。

### 9.6 APT-137 G6 持续验证与最终代码边界

保留既有 account-platform-test 全部门禁，路径触发覆盖 account-platform、直接 account-platform-* 脚本、scope/实际脚本测试及其前端依赖/锁文件；job 总预算 45 分钟，新增步骤另有有界 timeout。专属 PG 与原预建固定 Token 角色集群分离，三个原 CLI 从同一候选预先构建，两具名 PG 和真实 Chrome 均拒 SKIP；公开 Helm/Compose 真实解析、三个新镜像及 nginx 六 parser/双 runtime/真实 SIGTERM 门禁分别留证。工具只清理本次精确容器/进程，ready 前失败或未收到实际信号不得冒充注入通过；不删原测试、不关闭安全策略或依赖审计。

G0～G5 已独立签署，G6 精确候选 bcb25dcb 全账户434/434、任务/授权/架构均已通过，代码与隔离门禁已验证，最后证据文档交 B 复核；前述各组后续范围仅描述当时提交，不撤销现有实现。当前 UI-58 部分实现/NOT_DEPLOYED，代码交付与远端 CI、正式环境验收分开。官方依赖构建未成功和 TEMP 受限测试镜像变体通过必须分别记录；不将后者写作原 Dockerfile 成功或正式生产等价。全部现场前置与持续观察仍由运行手册第6～9节约束，不改变 activation 状态机或新域配对。
