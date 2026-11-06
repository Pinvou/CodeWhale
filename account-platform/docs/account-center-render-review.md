# Account center rendering follow-up

This follows the deployed account-center UI and the user's accepted 25-item rendering audit, plus three subsequent requests. It retains the existing PR and application state owners. Invoice application remains unavailable. CodeWhale, database migrations, dependencies, payment creation semantics and deployment configuration are unchanged.

## Acceptance map

Paths below are under `pinvou3-app/src/features/account-platform/` unless specified otherwise.

| ID | Accepted behavior | Implementation |
|---|---|---|
| 01 | Usage details open in a dialog without moving table cells | PortalUsage.jsx, PersonalUsageWorkspace.jsx |
| 02 | Order details controls retain readable width; narrow screens use cards | account-portal-layout.css, PersonalFinancePortalSections.jsx |
| 03 | Model policy fields and model selection form a responsive grid | OrganizationModelPolicyWorkspace.jsx |
| 04 | Standard fields/buttons align at 40px; coarse pointers use 44px | account-portal-layout.css |
| 05 | Adjacent independent actions have visible spacing and wrap | OrganizationWorkspace.jsx, OrganizationFinanceWorkspace.jsx |
| 06 | Department name and parent columns align | account-portal-layout.css |
| 07 | Missing department uses a read-only label | OrganizationWorkspace.jsx |
| 08 | Owner removal is disabled and explained; server authorization remains authoritative | OrganizationWorkspace.jsx |
| 09 | Role bindings have headings, scope, status and explicit permanent expiry and role permission explanations | OrganizationWorkspace.jsx |
| 10 | New organization purchases require a valid product and channel | OrganizationActionForm.jsx, OrganizationFinanceWorkspace.jsx |
| 11 | Channel hints distinguish selecting a product from unavailable channels | OrganizationFinanceWorkspace.jsx |
| 12 | Organization orders created by the current payer survive reload and support pagination/detail/payment recovery | OrganizationOrderHistory.jsx and the scoped history API below |
| 13 | Full-ID lookup has aligned controls and local UUID feedback | OrganizationFinanceWorkspace.jsx |
| 14 | Selected organization product shows credited points and validity | OrganizationFinanceWorkspace.jsx |
| 15 | Consistent card layers; ended organization benefits/quotas are initially collapsed | OrganizationFinanceWorkspace.jsx, account-portal-layout.css |
| 16 | Resolved empty allocation/reclaim forms become compact explanations without discarding unknown invocations | OrganizationActionForm.jsx, OrganizationClosedGrantsWorkspace.jsx |
| 17 | Authorized organization buyers can navigate from the credits overview to purchase; zero-credit members without purchase access receive administrator guidance | OrganizationCreditOverview.jsx, AccountPlatformView.jsx |
| 18 | Shared benefits use explicit card styles and contain exact large balances | PortalBenefits.jsx, account-portal-layout.css |
| 19 | Usage refresh belongs to the usage heading | PortalUsage.jsx, PersonalUsageWorkspace.jsx |
| 20 | Period statistics start compact and retain continuation/error recovery | PortalUsage.jsx |
| 21 | Order, benefit-validity and role-expiry timestamps include seconds and a time-zone indicator | account-center-view.js and its account-center consumers |
| 22 | Account closure follows ordinary profile/security operations | AccountPlatformView.jsx |
| 23 | Admin navigation is labeled as an admin entry | AccountPlatformView.jsx |
| 24 | Failed compact copying reveals the complete selectable identifier | CopyIdentifier.jsx |
| 25 | Tab ARIA orientation matches the responsive visual orientation | AccountPortalLayout.jsx |
| 26 | Recharge cards use compact spacing and type while preserving price, points and validity | PersonalFinancePortalSections.jsx, account-portal-layout.css |
| 27 | Account information, context, profile and security have white card surfaces | account-portal-layout.css |
| 28 | Payment preparation/image loading, long waits, image error and retry are visible; order creation is distinguished from payment success | RechargePaymentDialog.jsx, finance-i18n.js |

## Scoped organization history

The new Web-only read-only `GET /v1/organizations/{organization_id}/recharge/orders` uses the existing `org.entitlements.purchase` authorization and current organization resolver. The server resolves the wallet and requester. A cursor cannot select either. Each Billing query requires both requester and wallet, orders by `(created_at, id) DESC`, and returns an opaque continuation position. Token limits public pages to 50 entries. Rows never include provider payment instructions.

The label deliberately says “organization orders I created”: membership does not grant access to another payer's orders. Details and continuing payment use the existing scoped order reader, without creating another payment order. Creation time comes from the authorized history row because the existing detail DTO does not promise that field. Late results are fenced by API, actor/context, session epoch and read generation.

The history UI is mounted only by the Web Portal presentation; the desktop transport does not acquire a new endpoint. A refreshed history page owns its returned status, and an older selected detail cannot overwrite it. A confirmed paid read refreshes history through the existing paid-refresh lifetime.

History requires compatible Billing, Token and Portal versions. A future deployment of this change must include those services; the previous portal-only deployment procedure cannot enable this API. Older personal history callers retain their existing response shape. There are no migrations. A failed history read is displayed as unavailable, never as a successful empty history.

## Verification entry points

- `npm --prefix pinvou3-app run build:account-portal` regenerates the embedded account/admin entry points and hashed assets.
- `node pinvou3-app/tests/account_platform_restored_ui_smoke.mjs` exercises the real React/BFF consumers, responsive geometry, usage dialogs, copy denial, allocation recovery, history pagination, details and session retirement with synthetic HTTP data.
- `npm --prefix pinvou3-app run test:account-platform-recharge-ui` exercises controlled QR loading/slow/error/retry and the existing payment lifetime scenarios. Loading retry never changes a signed provider URL or creates an order.
- `npm --prefix pinvou3-app run test:account-platform-organization-ui` covers existing organization consumers.
- `TestOrganizationFinanceAuthorityAndWalletScope`, `TestOrderHistoryCursorRejectsMalformedPositions` and `TestOrganizationProxyFiniteDeliveredRoutes` verify HTTP authority and cursor boundaries.
- `TestRetainedOrganizationOrderHistoryPagination` is selected by the existing isolated financial PostgreSQL runner. It uses the public migrations and existing runtime/migrator roles to verify equal timestamps, gaps/duplicates, foreign payer/wallet exclusion and a new order arriving between pages.
- `node scripts/check-token-management-authorization-coverage.mjs`, its mutation tests, the financial runner tests and `python scripts/architecture-guard.py` cover route/inventory wiring and architectural boundaries.

Browser/HTTP fixtures are not a live provider payment or production deployment. Review conclusions, exact commit identities, executed results, failures and resource receipts are retained in the task's local review evidence; they are not inferred from this checklist.

## 本轮渲染反馈的追加修正

- 管理后台入口统一放在侧栏退出登录上方，复用相同控件样式和原平台权限条件；窄屏可换行排列。
- 充值金额改为 18px、500 字重，选中与未选中商品均保留 1px 边框，仅改变颜色和背景。
- 个人与组织订单详情使用稳定尺寸的弹窗框架，详情读取提示归弹窗持有；进入、返回焦点不滚动背景，弹窗内部键盘导航仍可滚动到目标控件。
- 关闭详情仅清除该次读取的反馈，不清除未知资金操作的事实或幂等键。客户端、身份、授权版本或会话换代立即撤回旧详情；读取被拒绝时撤回组织详情并退休读取入口。
- 组织历史保留已展开页，由历史组件为每次详情读取及重试建立独立代次围栏；仅在同一读取所有者下更新原有行，后续集合读取优先。付款弹窗查询显式绑定其展示订单。

验证包含延迟详情的列表和弹窗几何、窄屏短视口、错误关闭、客户端/会话换代、后页订单失败后重试成功、付款查询与后台输入隔离，以及 401/403 退出。编译产物的组织额度空态检查改为验证已批准的紧凑不可操作状态，保留零写入和 UNKNOWN 原事实/原幂等键恢复断言。
