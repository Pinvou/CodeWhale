import React from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../../src/features/account-platform/AccountPlatformView.jsx';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { accountPlatformCopy, adminPlatformCopy, financeCopy, accountLayoutCopy, accountCenterCopy, adminLayoutCopy, organizationCopy, adminRolePresentationCopy, platformRoleCopy, modelUsageCopy } from '../../src/shared/i18n.js';
const id = '11111111-1111-4111-8111-111111111111';
const orgID = '33333333-3333-4333-8333-333333333333', unitID = '44444444-4444-4444-8444-444444444444', otherID = '22222222-2222-4222-8222-222222222222';
const root = createRoot(document.querySelector('#root'));
const c = window.restored = { calls: [], effects: new Set(), user: id, language: 'zh', flags: {}, unknown: false, kind: new URLSearchParams(location.search).get('mode') || 'account' };
const json = (value, status = 200) => new Response(status === 204 ? null : JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const roleCatalog = [
  { code: 'PLATFORM_AUDITOR', scope: 'PLATFORM', assignable: true, permissions: ['platform.audit.read', 'platform.audit.export', 'platform.billing.ledger.read', 'platform.billing.wallets.read', 'platform.models.read', 'platform.organizations.read', 'platform.roles.read', 'platform.system.status.read', 'platform.users.read', 'platform.usage.read'] },
  { code: 'PLATFORM_BILLING_OPERATOR', scope: 'PLATFORM', assignable: true, permissions: ['platform.billing.wallets.read', 'platform.billing.ledger.read'] },
  { code: 'PLATFORM_AUDIT_PII_EXPORT', scope: 'PLATFORM', assignable: true, permissions: ['platform.audit.pii.export'] },
  { code: 'PLATFORM_SUPER_ADMIN', scope: 'PLATFORM', assignable: false, permissions: ['platform.roles.read', 'platform.roles.assign'] },
];
c.roleBindings = [{ binding_id: '66666666-6666-4666-8666-666666666666', user_id: otherID, role_code: 'PLATFORM_AUDITOR', scope: 'PLATFORM', grant_source: 'ADMIN', status: 'ACTIVE', starts_at: '2026-10-01T00:00:00Z', expires_at: null }];
c.roleReceipts = new Map();
// The service computes the current projection on each read/replay. A controlled
// server clock makes expiry reachable without changing the accepted intent.
const roleProjection = binding => {
  const now = c.roleClockMs ?? Date.now();
  return { ...binding, status: binding.status === 'REVOKED' ? 'REVOKED' : binding.expires_at && Date.parse(binding.expires_at) <= now ? 'EXPIRED' : Date.parse(binding.starts_at) > now ? 'SCHEDULED' : 'ACTIVE' };
};
const account = () => ({ user_id: c.user, username: 'sample-account', mobile: '+8613800000000', status: 'ACTIVE', personal_authorization_version: 1, platform_authorization_version: 1,
  personal_permissions: c.personal || ['personal.billing.read', 'personal.recharge.create', 'personal.recharge.manage', 'personal.usage.read', 'personal.profile.update'],
  platform_permissions: c.permissions || ['platform.roles.read', 'platform.roles.assign', 'platform.users.read', 'platform.users.status.manage', 'platform.billing.wallets.read', 'platform.billing.ledger.read', 'platform.billing.repairs.request', 'platform.products.read', 'platform.models.read', 'platform.tokens.read', 'platform.pricing.read', 'platform.usage.read', 'platform.registrations.repair', 'platform.profile-changes.repair', 'platform.organizations.read', 'platform.organizations.create', 'platform.organizations.profile.manage', 'platform.organizations.status.manage', 'platform.organizations.units.manage', 'platform.organizations.members.manage', 'platform.organizations.delete'],
  active_context: c.context || { context_type: 'PERSONAL', context_version: 1, permissions: [] } });
const order = (index = 1) => ({ order_id: `00000000-0000-4000-8000-${String(index).padStart(12, '0')}`, product_id: 'sample-points', amount_fen: '990', status: index === 1 ? 'PENDING' : ['PAID', 'EXPIRED', 'MANUAL_REVIEW', 'CLOSED'][index % 4], fulfillment_status: index === 2 ? 'CREDITED' : 'PENDING', close_status: 'OPEN', expires_at: '2099-01-01T00:00:00Z', created_at: '2026-10-10T12:00:00Z', paid_at: index % 4 === 0 ? '2026-10-10T12:15:00Z' : null });
const packages = Array.from({ length: 7 }, (_, i) => ({ package_id: `00000000-0000-4000-8000-${String(i + 20).padStart(12, '0')}`, plan_id: 'sample-points', current_plan_name_zh_cn: '积分权益', current_plan_name_en: 'Points benefit', current_plan_name_ja: 'ポイント特典', source_type: 'PAYMENT', status: 'ACTIVE', points_remaining_microcredits: '9007199254740993', points_used_microcredits: '10000', valid_until: `2099-01-${String(i + 1).padStart(2, '0')}T00:00:00Z`, components: [{ component_type: 'BASE' }] }));
globalThis.fetch = async (input, options = {}) => {
  const url = String(input), method = options.method || 'GET';
  const call = { url, method, body: options.body, key: options.headers?.['Idempotency-Key'] };
  c.calls.push(call);
  // Synthetic DTOs for personal-center interactions; the real BFF and feature
  // controllers still validate ownership, versions, money and write receipts.
  if (c.centerOrganization && url.includes(`/organizations/${orgID}/`)) {
    const member = { organization_id: orgID, membership_id: unitID, user_id: otherID, display_name: 'Sample member', status: 'ACTIVE', version: c.membershipVersion || 1, roles: ['MEMBER'], unit_ids: [unitID] };
    if (url.includes('/quota-members?')) return json({ items: [member], next_cursor: null });
    if (url.includes('/members?')) return json({ items: c.removedMember ? [] : [...(c.includeOwner ? [{ ...member, user_id: id, membership_id: '88888888-8888-4888-8888-888888888888', display_name: 'Sample owner', roles: ['OWNER'], unit_ids: [] }] : []), member], next_cursor: null });
    if (url.endsWith(`/members/${otherID}`) && method === 'DELETE') {
      c.effects.add(call.key); c.removedMember = true;
      return c.unknown ? json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503) : json({ organization_id: orgID, member: { ...member, status: 'REVOKED' } });
    }
    if (url.endsWith('/balance')) return json({ owner_type: 'ORGANIZATION', owner_id: orgID, wallet_id: id, posted_microcredits: c.zeroBalance ? '0' : '1000000', available_microcredits: c.zeroBalance ? '0' : '999999', reserved_microcredits: c.zeroBalance ? '0' : '1' });
    if (url.endsWith('/entitlement-packages')) return json({ items: c.noSources ? [] : [{ ...packages[0], points_remaining_microcredits: '999999', components: [{ component_id: unitID, component_type: 'BASE', status: 'ACTIVE', available_microcredits: '999999', expires_at: '2099-01-01T00:00:00Z' }] }] });
    if (url.endsWith('/quota-grants')) return json({ items: [] });
    if (url.endsWith('/quota-grants:batch') && method === 'POST') {
      c.effects.add(call.key);
      const input = JSON.parse(options.body).items[0];
      return c.unknown ? json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503) : json({ items: [{ index: 0, status: 'SUCCEEDED', grant: { ...input, membership_version: c.replayVersion || input.membership_version, organization_id: orgID, grant_id: id, status: 'ACTIVE', allocation_version: 1, original_microcredits: input.amount_microcredits, available_microcredits: input.amount_microcredits, reserved_microcredits: '0', used_microcredits: '0', expired_microcredits: '0', revoked_microcredits: '0' } }] }, 207);
    }
    if (url.endsWith('/role-bindings')) return json({ items: c.includeOwner ? [{ organization_id: orgID, binding_id: otherID, user_id: otherID, role_code: 'ORG_ADMIN', unit_id: null, status: 'ACTIVE', expires_at: null }] : [], truncated: false });
    if (url.endsWith('/recharge/products')) return json({ items: [{ product_id: 'sample-points', availability_status: 'AVAILABLE', name: 'Sample points', amount_fen: '990', base_points: '100', bonus_points: '10', validity_days: 30, channels: ['ALIPAY'] }] });
    if (url.endsWith('/payment-capabilities')) return json({ channels: ['ALIPAY'], test_only: true, formal_collection_admitted: false });
    if (/\/recharge\/orders(?:\?|$)/.test(url) && method === 'GET') {
      const cursor = new URL(url, location.origin).searchParams.get('cursor') || '';
      const result = () => c.historyFail ? json({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : json({ organization_id: orgID, items: (cursor ? [order(3)] : [order(1), order(2)]).map(row => c.historyPaid && row.order_id === order(1).order_id ? { ...row, status: 'PAID', fulfillment_status: 'CREDITED', paid_at: '2026-11-06T00:00:00Z' } : row), next_cursor: cursor ? null : 'page_two' });
      if (c.holdHistory) return new Promise(resolve => { c.historySignal = options.signal; c.releaseHistory = () => resolve(result()); });
      return result();
    }
    if (/\/recharge\/orders\/[^/?]+$/.test(url) && c.orgOrderReadStatus) return json({ code: 'UPSTREAM_UNAVAILABLE' }, c.orgOrderReadStatus);
    if (/\/recharge\/orders\/[^/?]+$/.test(url)) { const value = { ...order(Number(url.split('-').at(-1))), ...(c.detailPaid ? { status: 'PAID', fulfillment_status: 'CREDITED' } : {}), created_at: undefined, beneficiary_organization_id: orgID, channel: 'ALIPAY', qr_data: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aNbQAAAAASUVORK5CYII=' }; if (c.holdOrderDetail) return new Promise(resolve => { c.releaseOrderDetail = () => resolve(json(value)); }); return json(value); }

    if (url.endsWith('/model-policy')) return json({ organization_id: orgID, version: 1, authorization_version: 1, mode: 'ALLOW_ALL', model_ids: [] });
  }
  if (url.includes('/api/admin/billing/') && method === 'GET') {
    const params = new URL(url, location.origin).searchParams;
    for (const key of ['created_from', 'created_to', 'entered_from', 'entered_to', 'next_review_before']) if (params.get(key) && !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(params.get(key))) return json({ code: 'INVALID_TIME_WINDOW' }, 400);
  }

  if (url === '/api/session') return json({ authenticated: true, csrf_token: 'CONTROL_SYNTHETIC_CSRF', feature_flags: c.flags });
  if (url.endsWith('/account/bootstrap')) return json(account());
  if (url.endsWith('/admin/v1/roles') && method === 'GET') return json({ items: roleCatalog });
  if (url.includes('/admin/v1/role-bindings') && method === 'GET') {
    if (c.roleReadFail && c.roleReceipts.size) return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503);
    const target = new URL(url, location.origin).searchParams.get('user_id');
    const bindings = c.roleBindings.filter(binding => !target || binding.user_id === target);
    return json({ items: bindings.slice(0, 500).map(roleProjection), truncated: bindings.length > 500 });
  }
  if (url.endsWith('/admin/v1/role-bindings') && method === 'POST') {
    const body = JSON.parse(options.body);
    if (!c.roleReceipts.has(call.key)) {
      const binding = { ...body, binding_id: `77777777-7777-4777-8777-${String(c.roleReceipts.size + 1).padStart(12, '0')}`, scope: 'PLATFORM', grant_source: 'ADMIN', created_by: c.user, status: Date.parse(body.starts_at) > Date.now() ? 'SCHEDULED' : 'ACTIVE' };
      c.roleReceipts.set(call.key, binding); c.roleBindings.push(binding);
    }
    if (c.roleUnknown) { c.roleUnknown = false; return json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503); }
    if (c.roleHoldGrant) { c.roleHoldGrant = false; return new Promise(resolve => { c.releaseRoleGrantAck = () => resolve(json(roleProjection(c.roleReceipts.get(call.key)), 201)); }); }
    return json(roleProjection(c.roleReceipts.get(call.key)), 201);
  }
  if (/\/admin\/v1\/role-bindings\/[a-f0-9-]+$/.test(url) && method === 'DELETE') {
    const binding = c.roleBindings.find(item => item.binding_id === url.split('/').pop());
    if (!binding) return json({ code: 'NOT_FOUND' }, 404);
    binding.status = 'REVOKED'; binding.revoked_by = c.user;
    return json(binding);
  }
  if (url.endsWith('/v1/models')) return json({ items: [{ model_id: unitID, display_name: 'Sample model' }] });
  if (url.endsWith('/v1/me/organizations')) return json({ items: c.centerOrganization ? [{ organization_id: orgID, name: c.organizationName || 'Sample organization', status: 'ACTIVE', version: 1, roles: c.organizationRoles || ['OWNER'] }] : [] });
  if (url.endsWith('/v1/me/invitations')) return json({ items: [] });
  if (url.includes('/v1/me/balance?')) return new Response('{"available_microcredits":9223372036854775807,"reserved_microcredits":10001}', { headers: { 'Content-Type': 'application/json' } });
  if (/\/v1\/me\/usage-(requests|turns)/.test(url)) {
    if (c.usagePages) {
      const cursor = new URL(url, location.origin).searchParams.get('cursor');
      const older = Boolean(cursor);
      if (older && c.failUsageOnce) { c.failUsageOnce = false; return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503); }
      const result = () => json(c.usagePages[c.usageCursorPages ? c.usageCursorPages[cursor || ''] : older ? 1 : 0]);
      if (older && c.holdUsage) return new Promise(resolve => { c.heldUsageSignal = options.signal; c.releaseUsage = () => resolve(result()); });
      return result();
    }
    return json({ items: c.usage || [], next_cursor: null });
  }
  if (url.endsWith('/v1/recharge/products') && c.failInitialProducts) return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503);
  if (url.endsWith('/v1/recharge/products')) return json({ items: [{ product_id: 'sample-points', current_plan_name_zh_cn: '标准积分包', current_plan_name_en: 'Standard points', current_plan_name_ja: '標準ポイント', amount_fen: '990', base_points: '100', bonus_points: '10', validity_days: 30, channels: ['ALIPAY'] }] });
  if (url.endsWith('/v1/payment-capabilities')) return json({});
  if (url.endsWith('/v1/me/entitlement-packages')) return json({ items: packages });
  if (url.endsWith('/v1/me/profile-change-requests') && method === 'POST') return json({ code: 'USERNAME_ALREADY_REGISTERED' }, 409);
  if (/\/v1\/recharge\/orders\/[^/?]+$/.test(url) && c.orderReadFail) return json({ code: 'UPSTREAM_UNAVAILABLE' }, c.orderReadStatus || 503);
  if (url.endsWith('/v1/recharge/orders') && method === 'POST') { c.effects.add(call.key); const receipt = () => c.unknown ? json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503) : json(order(), 201); if (c.delayCreate) return new Promise(resolve => { c.releaseCreate = () => resolve(receipt()); }); return receipt(); }
  if (url.endsWith('/v1/recharge/orders')) return json({ items: Array.from({ length: 6 }, (_, i) => order(i + 1)) });
  if (/\/v1\/recharge\/orders\/[^/?]+$/.test(url)) { const value = order(Number(url.split('-').at(-1))); if (c.holdOrderDetail) return new Promise(resolve => { c.releaseOrderDetail = () => resolve(json(value)); }); return json(value); }
  if (url.includes('/admin/v1/user-code-lookups/')) {
    const code = decodeURIComponent(url.split('/').at(-1)).replaceAll('-', '').toUpperCase();
    return code === 'USR0123456789ABCDEFMV' ? json({ user_id: otherID, username: 'coded-user', user_code: 'USR-0123-4567-89AB-CDEF-MV', status: 'ACTIVE' }) : json({ code: 'NOT_FOUND' }, 404);
  }
  if (url.includes('/admin/v1/users?')) {
    const query = new URL(url, location.origin).searchParams.get('q') || '';
    return json({ items: query.replaceAll('-', '').toUpperCase().startsWith('USR') ? [] : [{ user_id: query === 'remote' ? otherID : id, username: query === 'remote' ? 'remote-user' : 'sample-account', user_code: 'SAMPLE-001', status: 'ACTIVE' }], next_cursor: null });
  }
  if (/\/admin\/v1\/users\/[a-f0-9-]+\/memberships$/.test(url) && c.failMemberships) return json({ code: 'ORGANIZATIONS_DISABLED' }, 503);
  if (/\/admin\/v1\/users\/[a-f0-9-]+\/memberships$/.test(url)) return json({ items: [{ organization_id: orgID, organization_name: 'Sample organization', membership_status: 'ACTIVE', roles: ['MEMBER'], unit_admin_unit_ids: [] }] });
  if (/\/admin\/v1\/users\/[a-f0-9-]+$/.test(url)) return json({ user_id: url.split('/').at(-1), username: 'sample-account', user_code: 'SAMPLE-001', status: 'ACTIVE', created_at: '2026-10-01T00:00:00Z' });
  const org = { organization_id: orgID, name: 'Sample organization', status: c.orgStatus || 'ACTIVE', version: 3, member_count: 1 };
  if (url.includes('/admin/v1/organizations?')) return json({ items: [org], next_cursor: null });
  if (url.endsWith(`/admin/v1/organizations/${orgID}`)) return json(org);
  if (url.endsWith('/admin/v1/organizations') && method === 'POST') { c.effects.add(call.key); return c.unknownOrgCreate ? json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503) : json(org, 201); }
  if (url.endsWith(`/organizations/${orgID}/units`)) return json({ items: [{ organization_id: orgID, unit_id: unitID, name: 'Sample unit', parent_id: null, status: 'ACTIVE', version: 1 }], structure_version: 2 });
  if (url.includes(`/organizations/${orgID}/members?`)) return json({ items: [{ organization_id: orgID, membership_id: '55555555-5555-4555-8555-555555555555', user_id: otherID, display_name: new URL(url, location.origin).searchParams.get('q') || 'Sample member', status: 'ACTIVE', version: 1, roles: ['MEMBER'], unit_ids: [unitID] }], next_cursor: null });
  if (url.endsWith(`/organizations/${orgID}/deletion-readiness`)) return json({ organization_id: orgID, current_status: c.orgStatus || 'ACTIVE', account_version: 3, ready: c.orgReady === true, blockers: c.orgReady ? [] : [{ code: 'ACTIVE_MEMBERS', resource_type: 'MEMBERSHIP', resource_id: otherID }] });
  if (url.includes('/admin/v1/finance-overviews/')) {
    const surface = url.split('/').at(-1);
    if (c.failedFinanceOverview === surface) return json({ code: 'FINANCE_OVERVIEW_UNAVAILABLE' }, 503);
    const key = { accounts: 'posted_microcredits', payments: 'orders_created_count', products: 'product_on_sale_count', pricing: 'price_draft_count' }[surface];
    return json({ as_of: '2026-10-29T00:00:00Z', window: surface === 'payments' ? { from: '2026-10-28T00:00:00Z', to: '2026-10-29T00:00:00Z', timezone: 'UTC' } : null, metrics: [{ key, value: surface === 'accounts' ? '9223372036854775807' : '2', unit: surface === 'accounts' ? 'MICROCREDITS' : 'COUNT', included_statuses: { products: ['ON_SALE'], accounts: ['ACTIVE', 'FROZEN', 'CLOSED'], pricing: ['DRAFT'], payments: ['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW'] }[surface] }] });
  }
  if (/\/admin\/v1\/wallets\/[a-f0-9-]+\/ledger\?/.test(url)) return new Response('{"items":[{"posting_id":"11111111-1111-4111-8111-111111111111","journal_entry_id":"22222222-2222-4222-8222-222222222222","ledger_account_code":"USER_LIABILITY","ledger_account_type":"LIABILITY","entry_type":"ADJUSTMENT","business_reference":"controlled-reference","description":"Controlled adjustment","delta_microcredits":9223372036854775807,"journal_balance_microcredits":0,"created_at":"2026-10-29T00:00:00Z"}],"next_cursor":null}', { headers: { 'Content-Type': 'application/json' } });
  if (url.endsWith('/admin/v1/overview')) { if (c.failDomain && url.includes('/api/admin/' + c.failDomain + '/')) return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503); return json({ wallets: 2, routes: 3, reconciliation_pending: 2, reconciliation_stopped: 0, users: 7, active_users: 4, disabled_users: 2, organizations: 3, active_memberships: 5, pending_invitations: 1, token_accounts: 2, active_accounts: 1, active_credentials: 2, healthy_routes: 3, usage_requests: 99, review_usage_requests: 4, pending_payment_orders: 1, review_payment_orders: 9, pending_adjustments: 2, posted_microcredits: '9007199254740993', reserved_microcredits: '10000', receivable_microcredits: '0', payment_capabilities: { normal_refund_creation_enabled: false } }); }
  if (url.includes('/admin/v1/wallets?')) return json({ items: [{ wallet_id: id, posted_microcredits: '9007199254740993', available_microcredits: '9007199254740993', status: 'ACTIVE', financial_status: 'NORMAL' }] });
  if (url.includes('/admin/v1/refunds?')) return json({ items: ['NORMAL', 'FORCED'].map(operation_type => ({ operation_id: id, operation_type, payment_order_id: otherID, payment_transaction_id: otherID, entitlement_package_id: otherID, amount_fen: '9007199254740993', base_microcredits: '10000', bonus_microcredits: '0', status: 'SUCCEEDED', reason: 'CONTROLLED', created_at: '2026-10-01T00:00:00Z' })), next_cursor: null });
  if (url.includes('/admin/v1/receivables?')) return json({ items: [{ receivable_id: id, wallet_id: otherID, original_microcredits: '9007199254740993', settled_microcredits: '10000', outstanding_microcredits: '9007199254730993', status: 'PARTIALLY_SETTLED' }], next_cursor: null });
  if (url.endsWith(`/admin/v1/receivables/${id}/allocations`)) return json({ items: [{ allocation_id: otherID, amount_microcredits: '10000', created_at: '2026-10-01T00:00:00Z' }] });
  if (url.includes('/admin/v1/adjustments?')) return json({ items: [] });
  if (url.endsWith('/admin/v1/adjustments') && method === 'POST') { c.effects.add(call.key); return json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503); }
  if (url.includes('/admin/v1/recharge-products?')) return json({ items: c.useCatalogFixture ? [c.catalogRecord || { catalog_item_id: otherID, product_id: 'controlled-product', display_name_zh_cn: '测试商品', display_name_en: 'Controlled product', display_name_ja: 'テスト商品', display_order: 1, base_points: '100', bonus_points: '0', validity_days: 30, invoice_product_name: 'Service', lifecycle_status: 'ON_SALE', version: c.catalogVersion || 1, mapping: { provider_id: 'controlled', provider_version: '1', sku_bom: 'CONTROLLED_SKU', amount_fen: '100', currency: 'CNY', channels: ['ALIPAY'], admission_class: 'FORMAL' } }] : [], next_cursor: null });
  if (url.endsWith('/admin/v1/recharge-product-create-options')) return json({ adapters: [{ provider_id: 'controlled', provider_version: '1', channels: ['ALIPAY'] }], invoice_product_names: ['Service'] });
  if (url.endsWith(`/admin/v1/recharge-products/${otherID}/admission-verifications`)) return json({ items: [] });
  if (url.endsWith(`/admin/v1/recharge-products/${otherID}/display-names`) && method === 'POST') {
    c.effects.add(call.key); c.catalogVersion = 2;
    const writes = c.calls.filter(value => value.url.endsWith('/display-names') && value.method === 'POST');
    return writes.length === 1 ? json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503) : json({ catalog_item_id: otherID, version: 2 });
  }
  if (url.endsWith('/admin/v1/recharge-products') && method === 'POST') {
    const body = JSON.parse(options.body); c.effects.add(call.key); c.catalogRecord = { ...body, catalog_item_id: otherID, lifecycle_status: 'DRAFT', version: 1 }; return json(c.catalogRecord, 201);
  }
  const adminOrder = { ...order(2), status: 'PAID', fulfillment_status: 'CREDITED', transactions: [], entitlement_packages: [] };
  if (/\/admin\/v1\/payment-orders\/[a-f0-9-]+$/.test(url)) return json(adminOrder);
  if (url.includes('/admin/v1/payment-orders?')) {
    const status = c.filteredOrders ? new URL(url, location.origin).searchParams.get('status') || 'PAID' : adminOrder.status;
    const response = () => json({ items: [{ ...adminOrder, status }], next_cursor: null });
    if (c.delayAdminOrders && status === 'PAID') { c.delayAdminOrders = false; return new Promise(resolve => { c.releaseAdminOrders = () => resolve(response()); }); }
    return response();
  }
  if (c.reviewFixture && /\/admin\/v1\/late-payment-reviews\/[a-f0-9-]+\/schedule$/.test(url) && method === 'POST') {
    c.effects.add(call.key);
    if (c.delayReviewSchedule) { c.delayReviewSchedule = false; return new Promise(resolve => { c.releaseReviewSchedule = () => resolve(json({ code: 'COMMAND_RESULT_UNKNOWN' }, 503)); }); }
    return json({ case_id: url.split('/').at(-2) });
  }
  if (c.reviewFixture && /\/admin\/v1\/(late-payment-reviews|payment-anomalies)/.test(url) && method === 'GET') {
    const review = caseID => ({ case_id: caseID, payment_order_id: caseID, review_status: 'REVIEW_REQUIRED_LATE_PAYMENT', status: 'OPEN', anomaly_type: 'FORCED_REFUND', severity: 'P2', owner_id: id });
    if (url.includes('?')) return json({ items: [review(otherID), review(orgID)], next_cursor: null });
    const selected = url.split('/').at(-1);
    if (c.failReviewID === selected) return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503);
    return json({ case: review(c.reviewAckID || selected), events: [] });
  }
  if (/\/admin\/v[12]\//.test(url) && method === 'GET') return json({ items: [], next_cursor: null });
  return json({ code: 'CONTROLLED_NOT_IMPLEMENTED' }, 404);
};
let client;
function render() {
  root.render(c.kind === 'admin' ? <AdminPlatformView client={client} copy={adminPlatformCopy[c.language]} language={c.language} onLanguageChange={value => { c.language = value; render(); }} /> : <AccountPlatformView client={client} copy={accountPlatformCopy[c.language]} language={c.language} portal onLanguageChange={value => { c.language = value; render(); }} />);
}
c.reload = () => { client = c.kind === 'admin' ? new AdminPlatformClient() : new AccountPlatformBffClient(); c.client = client; render(); };
c.copy = () => ({ account: accountPlatformCopy[c.language], admin: adminPlatformCopy[c.language], finance: financeCopy[c.language], accountLayout: accountLayoutCopy[c.language], center: accountCenterCopy[c.language], usage: modelUsageCopy[c.language], adminLayout: adminLayoutCopy[c.language], organization: organizationCopy[c.language], rolePresentation: adminRolePresentationCopy[c.language], roles: platformRoleCopy[c.language] });
c.reload();
