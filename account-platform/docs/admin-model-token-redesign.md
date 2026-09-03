# 管理后台“模型与 Token”重构专项设计

> 状态：身份 A 修订稿，等待身份 B 独立审核；本文件定义目标需求和设计，不表示代码已实现。
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

标准闭环为：同步目录 → 查看新增/变化/缺失差异 → 编辑草稿 → 校验协议、公式、有效价格及 READY 路由 → 发布 → 监控 → 计划弃用 → 下架或前向回滚。发布、弃用、下架和回滚都先展示价格、路由、Provisioning、进行中请求、新会话/存量会话截止及替代模型影响，提交时重新预检。

模型详情唯一页签结构为“概览 / 能力、协议与公式 / 路由与运行 / 价格与计量 / 生命周期与依赖 / 审计”。“能力、协议与公式”同时呈现当前不可变能力版本、协议版本及安全计量公式，不另设重复协议页签；“生命周期与依赖”统一承载目录版本差异、价格/路由/Provisioning/进行中请求依赖和计划生效，不把同一依赖拆到概览。价格页签消费 Billing 权威投影；没有 `platform.pricing.read` 时不请求、不渲染，编辑入口还要求 `platform.pricing.manage`，并复用财务定价组件和 Billing 命令合同。审计页签额外要求 `platform.audit.read`。

### 2.2 Token 资源

标准闭环为：选择个人、组织或节点 owner → 创建 PLATFORM Token Account → 通过隔离凭据页新增或轮换会接触 Secret 的凭据 → 由 Token 域直接执行脱敏现场校验 → 从模型目录选择稳定 `model_id` 与已发布 `protocol_version_id` 配置路由 → Provisioning → 服务端健康探测 → 启用 → 隔离轮换、Portal 向 Account 申请审批撤销或停用。撤销不打开隔离页。

创建入口同时要求：

- `platform.tokens.manage`：允许创建和修改 Token Account；
- `platform.tokens.owner-candidates.read`：允许解析最小 owner 候选。

只有 `platform.tokens.manage` 而没有候选读取权限时，页面必须显示“缺少所有者候选权限，无法创建”的失败关闭状态，创建按钮禁用；不得退化为 UUID 文本框、复用通用用户/组织目录或允许盲填。候选已授权但不存在、不可见或越出作用域时返回空页，不泄露对象是否存在。

Token Account 详情在二期成为独立主内容页，页签为“概览 / 凭据 / 路由 / Provisioning / 审计”。凭据只显示类型、状态、指纹、更新时间和稳定结论，不显示 Secret、密文、Provider Key ID、Provider 私有地址或原始错误。

路由表单只接受目录选择器产生的稳定模型、协议版本及路由策略；调用方不得提交健康状态。服务端探测状态固定为 `UNKNOWN/PROBING/HEALTHY/DEGRADED/UNHEALTHY/STALE`，HEALTHY 证据 TTL 为 5 分钟，过期或不可用时按不可路由失败关闭。

### 2.3 运行与用量

一期在当前主内容区提供 Provisioning 详情和用量详情子视图，不使用抽屉挤压列表；二期再补趋势和跨资源联动。

Provisioning 支持状态、模型、Token Account、owner 和时间筛选，详情展示目标代次、尝试、脱敏证据、SLA 与修复入口。用量支持状态、模型、Token Account、组织和时间筛选，并提供“核对异常”工作队列。

异常闭环为：发现异常 → 查看请求、Hold、上游与结算证据 → 仅重新读取并归并既有证据 → 确认最终结算 → 保存审计。普通“重新核对”不得重新发起推理、不得冒充 `USAGE_SETTLEMENT_REPAIR`，未知结果继续按同一幂等引用查询。

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

容量/速率错误与超时分离：过宽为 422 `PAGINATION_FILTER_REQUIRED`；快照容量或创建速率耗尽为 429 `PAGINATION_SNAPSHOT_LIMITED`；常规限流为 429 `RATE_LIMITED`；非快照查询容量不足为 503 `QUERY_CAPACITY_EXCEEDED`。任何失败均不得返回不完整 200、半快照或静默首页。

### 4.3 快照容量

单快照最多 1,000,000 个成员；单主体最多 2 个活动快照且同主体同资源最多 1 个；同资源新快照事务替换旧快照，第三资源事务回收最旧快照。全局最多 64 个活动快照、20,000,000 个成员及 8 GiB 成员存储。70% 告警，85% critical 并拒绝新的非替换快照；墓碑至少保留 20 分钟，清理每分钟幂等执行。

每个页面/页签独立维护 loading、error、retry、last_updated、filter 和 cursor。模型失败不得阻断价格投影，用量失败不得阻断 Provisioning；离页中止请求，代次令牌防止旧响应回写。

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

每个环境只允许一个经部署清单固定、禁止运行时任意覆盖的 credential origin：

| 环境 | credential origin |
|---|---|
| Production | `https://credentials.account.pinvou.com` |
| Cloud UAT | `https://credentials-uat.account.pinvou.com` |

Portal 的 CSP 必须由部署环境选择精确值，例如生产：

```http
Content-Security-Policy: default-src 'self'; form-action 'self' https://credentials.account.pinvou.com; frame-ancestors 'none'; base-uri 'none'
```

不得使用 `https:`、通配子域、调用方传入 origin 或动态重定向。credential origin 自身使用 `default-src 'self'; connect-src 'self' <exact-oidc-origin>; form-action 'self' <exact-oidc-origin>; frame-ancestors 'none'; base-uri 'none'`，且不加载第三方脚本、像素或字体。

### 6.2 顶层 POST launch

Portal 先从 Token 创建 5 分钟、单次消费、最多 3 次尝试的 launch 记录和不透明 `result_ref`。用户点击后，以 HTML form 的顶层跨 origin POST 打开新浏览上下文：

```html
<form method="post"
      action="https://credentials.account.pinvou.com/launch"
      target="_blank"
      rel="noopener noreferrer">
  <input type="hidden" name="launch_id" value="...">
</form>
```

Production/UAT 的 action 必须来自编译/部署白名单，不接受 API 返回任意 URL。表单只传 `launch_id`，不传账户、owner、Secret 或 Provider 信息；`launch_id` 只存在 POST body，禁止 query、fragment、path、cookie、local/session storage、window.name、Referer、遥测和日志。表单必须由真实用户手势直接提交，并以 `target=_blank` + `rel="noopener noreferrer"`（或经安全测试证明等价的 opener 隔离机制）保证隔离页没有 opener。

credential origin 的 `/launch` 只接受 `application/x-www-form-urlencoded` POST，验证并原子消费 launch 后返回：

```http
HTTP/1.1 303 See Other
Location: /session
Referrer-Policy: no-referrer
Cache-Control: no-store
```

`/session` 是固定地址，不携带任何 ID。因为 POST URL 本身也固定为 `/launch`，重定向后的地址、Referer 和浏览器历史均不含 launch ID；反向代理、WAF、应用访问日志和崩溃报告禁止记录请求 body。浏览器刷新 `/session` 不得重放 POST。

弹窗或新页被阻止时必须安全失败：Portal 在 10 秒内未观察到 Token 保存的隔离页一次性 heartbeat，就原子撤销 launch，显示稳定错误 `CREDENTIAL_WINDOW_BLOCKED` 和“允许本站打开新窗口后重试”；不得改为 GET、query/fragment、同页降级、iframe、复制链接或把 Secret 收回 Portal。窗口已打开但认证失败时同样只经 `result_ref` 轮询稳定状态。

强制浏览器验收：launch ID 仅出现于 POST body；303 后地址栏、`document.referrer`、Performance/Navigation、浏览器历史和代理访问日志均无 ID；`window.opener === null`；刷新不重发 POST；弹窗阻止时 launch 被撤销且网络中没有任何 query 降级。

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
| `TOKEN_MODEL_PUBLISH` | `platform.models.manage` | `AUDIT_ONLY` | 15m | 必填 | `expected_catalog_version` + `expected_draft_version` | 必填 | 协议/公式、有效价格、READY 路由覆盖 | 通用审计 + 新目录版本 | 命令结果查询并读取目标目录版本 |
| `TOKEN_MODEL_LIFECYCLE_CHANGE` | `platform.models.lifecycle.request` | `APPROVAL_REQUIRED` | 15m | 必填 | `expected_catalog_version` + 生命周期计划版本 | 必填 | 价格、路由、Provisioning、在途请求、会话截止、替代模型 | 申请、批准、执行及生命周期审计 | 查询审批/执行和新目录版本；不得重提或 force |
| `TOKEN_MODEL_CATALOG_SYNC` | `platform.models.sync` | `AUDIT_ONLY` | 否 | 必填 | `expected_catalog_version` + Adapter 观察版本 | 必填 | 只读差异预览及连续缺失证据 | 通用审计 + 同步差异摘要 | 命令结果查询；未知时不得启动第二次同步 |
| `TOKEN_MODEL_CATALOG_ROLLBACK` | `platform.models.catalog.rollback.request` | `APPROVAL_REQUIRED` | 15m | 必填 | `source_catalog_version` + `expected_catalog_version` | 必填 | 全量目录差异、价格、路由、Provisioning、在途请求 | 申请、批准、执行及新目录版本审计 | 查询审批/执行和前向回滚版本；不执行破坏性回退 |
| `TOKEN_ACCOUNT_CREATE` | `platform.tokens.manage` + `platform.tokens.owner-candidates.read` | `AUDIT_ONLY` | 15m | 必填 | owner 权威版本 + 创建哨兵 0 | 必填 | owner 状态/作用域、同 owner 重名与配额 | 通用审计；不记录候选搜索词 | 命令结果查询并读取 account；不得盲目再建 |
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

## 8. UI、可访问性与错误语义

- 英文枚举在中文界面转换为“已发布、已弃用、可用、对话”等标签，原枚举可置于 Tooltip。
- 资源名称进入详情，稳定 ID 为次要信息并独立复制；表格主要动作保留一个，其余进入“更多”。
- 高风险操作使用结构化面板，显示目标、影响、原因、生效时间、资源版本、幂等和未知结果恢复；移动端使用全屏面板，触控目标至少 44×44 CSS px。
- 首屏可展示已发布模型、待发布草稿、异常路由、失败 Provisioning 和待核对用量，但指标由服务端聚合、明确窗口/时区/分母，403 隐藏、失败显示不可用，不扫描列表计算。
- 错误必须保留 `request_id`，区分 400 输入、403 无权、409 版本冲突、410 cursor 过期、422 筛选过宽、429 限流、503 容量/快照不可用、504 查询/预检超时。
- 任何错误、DOM、日志、遥测和审计都不能包含 Secret、密文、Provider Key ID、私有地址、原始错误或无权深链 ID。

## 9. 分期、兼容与回滚

一期：三个 section 和 URL 状态、逐区块独立加载、全部列表服务端分页筛选、协议/公式、owner 选择器、路由模型选择器、Provisioning/用量详情、核对异常队列及结构化高风险面板。

二期：模型/Token Account 独立详情、隔离凭据新增/轮换、Token 直接脱敏校验、Portal→Account 审批撤销、生命周期依赖与计划生效、模型上下文价格与计量、目录差异/健康/异常趋势。

每个能力有独立 feature gate，服务与 Portal 支持 N/N-1。数据库使用前向兼容追加迁移；回滚先停新写和隔离凭据会话，再回滚 Portal/服务。未消费 launch 立即失效，旧 cursor 返回 410，不得在回滚期间重新启用 query 凭据通道。

## 10. 发布门禁

实现阶段必须提供以下自动证据：

1. model-only、pricing-only、token-only、usage-only、manage-only、owner-candidates-only、超级管理员和模型管理员权限矩阵。
2. owner 权限追加迁移的创建、精确角色授予、授权版本递增、缓存/cursor 失效和 down 回滚测试。
3. 合法 UUID 无权、非法 UUID、互斥详情、无权页签的零详情请求、零 DOM/日志泄漏测试。
4. 三类唯一预算、入口代理超时顺序、快照事务中断无半结果、容量边界、70%/85% 水位、第三资源回收和并发翻页无重复漏项测试。
5. 固定 credential origin/CSP、顶层 POST、303 无 ID、no-opener/no-referrer、弹窗阻止撤销且无 query 降级的真实浏览器测试。
6. 逐区块失败隔离、URL 前进后退、旧 `section=tokens` 兼容、移动端与键盘可访问性测试。
7. 从真实迁移后数据库对三个强制审批策略与 `internal/platform/approvalpolicy` 逐字段（包括有序 `critical_resource_fields`）比对；`executableApprovalCommands` 保留所有既有合法项，且新增 `TOKEN_*` 子集恰为 lifecycle v2、rollback v2、revoke v1 三项，无其他 Token 命令被放行；任一偏差阻断发布。
8. 现有管理后台合同测试在后续实施任务中更新：feature gate 开启时断言三页为 canonical，关闭时仍断言历史 Token Gateway；本轮不修改实现测试。

任一门禁缺失时不得把需求状态标记为已实现、不得推送生产部署。
