# 全局价格管理分页

全局工作区使用 `GET /admin/v1/pricing?limit=100`；该初页调用仍有效。
返回 `items`、布尔 `has_more` 和字符串 `next_cursor`。末页的游标为空字符串；
有后页时，将服务端游标原样作为 `cursor` 并保持相同 `limit` 读取后页。
默认 limit 为 50，允许 1–200；非法值、重复参数、空游标、游标与查询空间/单价 ID
混用或列表使用未知筛选参数均拒绝。原 `scope_type` 价格空间与 `price_version_id`
读取保留各自契约，空间读取的 200 条完整性保护不受此分页影响。

游标为版本化、大小有界的 base64url JSON，绑定页大小和 PostgreSQL 微秒精度的
`created_at`、UUID；规范解码后使用参数化 `(created_at,id) < (边界时刻,边界ID)`
条件，沿既有两列倒序读取 `limit+1` 判定后页。游标不是授权证据，也不是持久快照。
每次请求仍由现有读权限保护，在一个 repeatable-read 事务中投影权威 ETag。
较新插入不移动已读边界；更新状态仍以各次读取时刻为准。回退会重新读先前边界，
从最新价格重新开始可查看新插入。不能把一次遍历理解为跨请求的全局时间点快照。

界面提供上一页、下一页、原失败页重试、当前页刷新和从最新价格重新开始；
不自动扫描全部价格。失败时保留成功页及其导航位置，下一次重试使用原请求游标。
编辑、确认操作、未决写入和 UNKNOWN 恢复期间禁用分页与主动刷新，保留编辑与原幂等状态。
请求沿用资源取消/代次校验，权限、用户、会话或模型上下文变化重置分页。
价格修改、发布预检、ETag 与生命周期规则继续由既有写入路径约束。

回归：`node --test account-platform/test/contract/managed-price-pagination.test.mjs`；
Go `TestManagedPriceCursorValidation` 和真实 PostgreSQL `TestPostgresManagedPricePagination`
（沿用 Billing 自有数据库夹具）。后者注册到现有财务 PostgreSQL runner 的必跑叶用例，
并可用 `PINVOU_MANAGED_PRICE_FIXTURE_OUTPUT` 输出受控合成 handler DTO 供消费者测试。

`npm --prefix pinvou3-app run test:account-platform-management-ui` 包含真实 React/AdminClient
分页 smoke；该测试的 HTTP 服务受控，不代表真实 Account/Billing 服务验收。
`node pinvou3-app/tests/account_platform_managed_price_ui_smoke.mjs --serve-only` 仅编译并启动
本机 HTTP 入口，供可用浏览器验证 DOM，不自动启动或下载浏览器。
