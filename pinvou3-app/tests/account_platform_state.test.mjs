import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, readdir } from 'node:fs/promises';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from '../src/features/account-platform/context-guard.js';
import { formatPoints, formatUsagePoints, formatWholePoints, MICROCREDITS_PER_POINT, microcreditsToPoints } from '../src/features/account-platform/credit-units.js';
import { activeEntitlementPackages, entitlementProgress, remainingEntitlementMicrocredits } from '../src/features/account-platform/entitlement-view.js';
import { compactOrderID, isPayablePaymentOrder, orderDurationDays, orderHistoryTimestamp, orderSummaryStatus, payablePaymentOrders, paymentOrderHistory, paymentOrderRemainingSeconds } from '../src/features/account-platform/order-view.js';
import { availablePortalSection, initialPortalSection, portalLoginLocation, portalNavigationSections, portalSectionLocation } from '../src/features/account-platform/portal-sections.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { buildUnitTree } from '../src/features/account-platform/admin-organization-tree.js';
import { adminNavigationGroups, adminSectionURL, readAdminSection, writeAdminSection } from '../src/features/account-platform/admin-navigation.js';
import { adminSectionReadOnly, adminVisibleSections, canAccessOrganizationPortal } from '../src/features/account-platform/authorization-ui.js';
import { summarizeUsageRecords } from '../src/features/account-platform/usage-summary.js';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';

test('admin portal navigation groups and URL state remain deterministic', () => {
  assert.deepEqual(adminNavigationGroups, [
    { key: 'workbench', sections: ['overview'] },
    { key: 'identity', sections: ['users', 'roles', 'organizations', 'myorg'] },
    { key: 'finance', sections: ['billing', 'payments', 'products'] },
    { key: 'models', sections: ['tokens'] },
    { key: 'governance', sections: ['approvals', 'audit', 'system', 'settings'] },
  ]);
  const location = { pathname: '/admin.html', search: '?trace=kept&section=users', hash: '#focus' };
  assert.equal(readAdminSection(location), 'users');
  assert.equal(adminSectionURL('billing', location), '/admin.html?trace=kept&section=billing#focus');
  assert.equal(
    adminSectionURL('roles', location, { role: 'ORG_ADMIN', trace: null }),
    '/admin.html?section=roles&role=ORG_ADMIN#focus',
  );
  const calls = [];
  const history = {
    pushState: (...args) => calls.push(['push', ...args]),
    replaceState: (...args) => calls.push(['replace', ...args]),
  };
  writeAdminSection('tokens', 'push', history, location);
  writeAdminSection('audit', 'replace', history, location);
  assert.deepEqual(calls, [
    ['push', { section: 'tokens' }, '', '/admin.html?trace=kept&section=tokens#focus'],
    ['replace', { section: 'audit' }, '', '/admin.html?trace=kept&section=audit#focus'],
  ]);
});

test('Keycloak security access and user operations have independent permission boundaries', () => {
  const snapshot = (permissions) => ({ platform_permissions: permissions });

  assert.deepEqual(adminVisibleSections(snapshot(['platform.users.identity.repair'])), ['system', 'settings']);
  assert.deepEqual(adminVisibleSections(snapshot(['platform.account-closures.read'])), ['users', 'settings']);
  assert.equal(adminSectionReadOnly(snapshot(['platform.system.status.read']), 'system'), true);
  assert.equal(adminSectionReadOnly(snapshot(['platform.users.identity.repair']), 'system'), false);
  assert.equal(adminSectionReadOnly(snapshot(['platform.account-closures.repair']), 'users'), false);
});

test('admin identity pages own role and repair workspaces in React', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const roles = await readFile(new URL('../src/features/account-platform/AdminRolesWorkspace.jsx', import.meta.url), 'utf8');
  const authorization = await readFile(new URL('../src/features/account-platform/authorization-ui.js', import.meta.url), 'utf8');
  assert.match(view, /function AdminExtensionSlot/);
  assert.doesNotMatch(view, /role-bindings-root/);
  assert.match(view, /<AdminRolesWorkspace/);
  assert.match(roles, /function GrantDrawer/);
  assert.match(roles, /client\.grantRoleBinding/);
  assert.match(roles, /client\.revokeRoleBinding/);
  assert.doesNotMatch(view, /registration-repair-root/);
  assert.match(view, /host\.append\(node\)/);
  assert.match(authorization, /users: \['platform\.users\.read', 'platform\.registrations\.repair', 'platform\.profile-changes\.repair', 'platform\.account-closures\.read'\]/);
  assert.match(authorization, /system: \['platform\.system\.status\.read', 'platform\.users\.identity\.repair'\]/);
  assert.match(view, /section === 'users'.*UsersWorkspace/s);
  assert.match(view, /function UserRepairWorkspace/);
  assert.match(view, /function RegistrationRepairQueue/);
  assert.match(view, /function ProfileRepairQueue/);
  assert.match(view, /function AccountClosureOperations/);
  assert.match(view, /section === 'system'.*KeycloakSecurityAccess/s);
  assert.doesNotMatch(view, /function IdentitySecurity/);
  assert.match(authorization, /roles: \['platform\.roles\.read'\]/);
  assert.doesNotMatch(authorization, /identity: \[/);
});

test('admin finance capabilities use independent permission-gated routes without unsupported drafts', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const authorization = await readFile(new URL('../src/features/account-platform/authorization-ui.js', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/admin-platform.css', import.meta.url), 'utf8');
  const main = await readFile(new URL('../src/app/admin-main.jsx', import.meta.url), 'utf8');
  const approvals = await readFile(new URL('../../account-platform/internal/portal/static/assets/approval-requests.js', import.meta.url), 'utf8');
  const products = await readFile(new URL('../../account-platform/internal/portal/static/assets/product-admin.js', import.meta.url), 'utf8');

  assert.doesNotMatch(view, /function financeSurfaces/);
  assert.doesNotMatch(view, /pinvou\.admin\.finance-surface/);
  assert.match(view, /platformPermissions\.has\('platform\.products\.read'\)/);
  assert.match(view, /'approval-requests-root': section === 'approvals'/);
  assert.match(view, /'product-admin-root': section === 'products'/);
  assert.match(view, /section === 'billing'.*surface="billing"/s);
  assert.match(view, /section === 'payments'.*surface="payments"/s);
  assert.match(view, /section === 'settings'.*SettingsWorkspace/s);
  assert.match(view, /'pricing-admin-root': false/);
  assert.match(authorization, /billing: \['platform\.billing\.wallets\.read', 'platform\.pricing\.read', 'platform\.usage\.read'\]/);
  assert.match(authorization, /payments: \['platform\.billing\.wallets\.read'\]/);
  assert.match(authorization, /products: \['platform\.products\.read'\]/);
  assert.match(authorization, /approvals: \['platform\.billing\.wallets\.read'\]/);
  assert.match(styles, /\.admin-portal-body > #approval-requests-root[\s\S]*display:none/);
  assert.match(styles, /\.approval-control-snapshot/);
  assert.match(main, /pinvou-admin-language-change/);
  assert.match(approvals, /<details class="approval-control-snapshot">/);
  assert.match(approvals, /pinvou-admin-language-change/);
  assert.match(products, /pinvou-admin-language-change/);
});

test('admin model, system, audit, and runbook surfaces keep technical identifiers usable and secrets out of the DOM', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/admin-platform.css', import.meta.url), 'utf8');

  assert.match(view, /function TechnicalID/);
  assert.match(view, /navigator\.clipboard\.writeText\(text\)/);
  assert.match(view, /aria-label=\{`\$\{copy\.copyIdentifier\}: \$\{text\}`\}/);
  assert.match(view, /<TechnicalID value=\{item\.model_id\} copy=\{copy\}/);
  assert.match(view, /<TechnicalID value=\{item\.provisioning_id\} copy=\{copy\}/);
  assert.match(view, /<TechnicalID value=\{item\.request_id\} copy=\{copy\}/);
  assert.match(view, /function RunbookBoundary/);
  assert.match(view, /platform\.system\.status\.read/);
  assert.match(view, /platform\.audit\.read/);
  assert.match(view, /<Status value="OUT_OF_BAND"/);
  assert.match(view, /copy\.runbooks\.noExecution/);
  assert.match(styles, /\.adm-technical-id/);
  assert.match(styles, /\.adm-runbook-list/);
  assert.equal((i18n.match(/copyIdentifier:/g) || []).length, 3);
  assert.equal((i18n.match(/runbooks: \{/g) || []).length, 3);
  assert.match(i18n, /凭据、Token 和可复用验证材料永不显示/);
  assert.doesNotMatch(view, /Authorization Header|client_secret|access_token/);
});

async function sourceFiles(root) {
  const files = [];
  for (const entry of await readdir(root, { withFileTypes: true })) {
    const child = new URL(`${entry.name}${entry.isDirectory() ? '/' : ''}`, root);
    if (entry.isDirectory()) files.push(...await sourceFiles(child));
    else if (/\.(?:js|jsx|mjs|rs)$/.test(entry.name)) files.push(child);
  }
  return files;
}

test('account balances convert internal microcredits to display points', () => {
  assert.equal(MICROCREDITS_PER_POINT, 10_000);
  assert.equal(microcreditsToPoints(1_000_000), 100);
  assert.equal(formatPoints(1_000_000, 'en'), '100');
  assert.equal(formatPoints(7_969_994, 'en'), '796.9994');
  assert.equal(formatPoints(1, 'en'), '0.0001');
  assert.equal(formatPoints(0, 'en'), '0');
  assert.equal(formatPoints(null, 'en'), '—');
  assert.equal(formatWholePoints(7_969_994, 'en'), '796');
  assert.equal(formatUsagePoints(8_317, 'en'), '0.83');
  assert.equal(formatUsagePoints(10_000, 'en'), '1.00');
});

test('Web personal center groups model calls by logical conversation turn', () => {
  const records = [
    { usage_request_id: 'request-2', logical_turn_id: 'turn-a', model: 'model-a', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', actual_microcredits: 5_000, input_tokens: 20, output_tokens: 4, cached_input_tokens: 1, started_at: '2026-08-28T08:01:00Z', completed_at: '2026-08-28T08:02:00Z' },
    { usage_request_id: 'request-1', logical_turn_id: 'turn-a', model: 'model-a', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', actual_microcredits: 3_317, input_tokens: 10, output_tokens: 2, cached_input_tokens: 0, started_at: '2026-08-28T08:00:00Z', completed_at: '2026-08-28T08:01:30Z' },
    { usage_request_id: 'legacy-request', model: 'model-a', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'COMPLETED', actual_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-08-27T08:00:00Z' },
  ];
  const summaries = summarizeUsageRecords(records);
  assert.equal(summaries.length, 2);
  assert.equal(summaries[0].logical_turn_id, 'turn-a');
  assert.equal(summaries[0].usage_request_count, 2);
  assert.equal(summaries[0].actual_microcredits, 8_317);
  assert.equal(summaries[0].input_tokens, 30);
  assert.equal(summaries[0].output_tokens, 6);
  assert.equal(summaries[0].cached_input_tokens, 1);
  assert.equal(summaries[0].started_at, '2026-08-28T08:00:00Z');
  assert.equal(summaries[0].completed_at, '2026-08-28T08:02:00Z');
  assert.equal(summaries[1].usage_request_count, 1);
  assert.equal(summaries[1].actual_microcredits, 0);
});

test('Web usage summary keeps an unfinished request actionable within a grouped turn', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'request-complete', logical_turn_id: 'turn-active', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 1_000, input_tokens: 10, output_tokens: 2, cached_input_tokens: 0, started_at: '2026-08-28T08:00:00Z', completed_at: '2026-08-28T08:01:00Z' },
    { usage_request_id: 'request-active', logical_turn_id: 'turn-active', display_status: 'RUNNING', execution_status: 'UPSTREAM_DISPATCHING', delivery_status: 'STREAMING', settlement_status: 'PENDING', actual_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-08-28T08:02:00Z' },
  ]);
  assert.equal(summary.cancel_usage_request_id, 'request-active');
  assert.equal(summary.display_status, 'RUNNING');
  assert.equal(summary.execution_status, 'UPSTREAM_DISPATCHING');
  assert.equal(summary.actual_microcredits, null);
  assert.equal(summary.completed_at, '');
});

test('Web personal center uses a permission-gated organization tab and describes all order states', () => {
  assert.deepEqual(portalNavigationSections, ['creditsUsage', 'ordersInvoices', 'myOrganization', 'accountSettings']);
  assert.equal(initialPortalSection(''), 'creditsUsage');
  assert.equal(initialPortalSection('?section=unknown'), 'creditsUsage');
  assert.equal(initialPortalSection('?section=profile'), 'accountSettings');
  assert.equal(initialPortalSection('?section=usage'), 'creditsUsage');
  assert.equal(initialPortalSection('?section=invoice'), 'ordersInvoices');
  assert.equal(initialPortalSection('?section=organization'), 'myOrganization');
  assert.equal(initialPortalSection('?section=myOrganization'), 'myOrganization');
  assert.equal(availablePortalSection('?section=ordersInvoices', ['creditsUsage', 'accountSettings']), 'creditsUsage');
  assert.equal(availablePortalSection('?section=myOrganization', ['creditsUsage', 'accountSettings']), 'creditsUsage');
  assert.equal(availablePortalSection('?section=accountSettings', ['creditsUsage', 'accountSettings']), 'accountSettings');
  assert.equal(portalSectionLocation('ordersInvoices', '?profile=mobile&lang=zh', '/', false), '/?lang=zh&section=ordersInvoices');
  assert.equal(portalSectionLocation('accountSettings', '?profile=mobile', '/', true), '/?profile=mobile&section=accountSettings');
  assert.equal(portalLoginLocation('/', '?section=creditsUsage'), '/auth/login?return_to=%2F%3Fsection%3DcreditsUsage');
  assert.equal(portalLoginLocation('/admin.html', '?section=users'), '/auth/login?return_to=%2Fadmin.html%3Fsection%3Dusers');
  assert.equal(portalLoginLocation('//evil.example', '?section=creditsUsage'), '/auth/login?return_to=%2F%3Fsection%3DcreditsUsage');
  for (const locale of ['zh', 'en', 'ja']) {
    assert.ok(accountPlatformCopy[locale].sections.creditsUsage);
    assert.ok(accountPlatformCopy[locale].sections.ordersInvoices);
    assert.ok(accountPlatformCopy[locale].sections.myOrganization);
    assert.ok(accountPlatformCopy[locale].sections.accountSettings);
    assert.ok(accountPlatformCopy[locale].orderHistoryLead);
  }
});

test('My organization visibility requires an active organization and a read permission', () => {
  const snapshot = (organizationID, permissions) => ({ active_context: { organization_id: organizationID, permissions } });
  assert.equal(canAccessOrganizationPortal(snapshot('org-1', ['org.profile.read'])), true);
  assert.equal(canAccessOrganizationPortal(snapshot('org-1', ['org.wallet.read'])), true);
  assert.equal(canAccessOrganizationPortal(snapshot('org-1', ['org.members.manage'])), false);
  assert.equal(canAccessOrganizationPortal(snapshot('', ['org.profile.read'])), false);
});

test('organization billing uses organization-scoped routes and reports only service failures as unavailable', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const authorization = await readFile(new URL('../src/features/account-platform/authorization-ui.js', import.meta.url), 'utf8');
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const desktopClient = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');

  assert.ok(authorization.includes("billingRead: organizationContext ? active.has('org.wallet.read') :"));
  assert.doesNotMatch(authorization, /billingRead: organizationContext \?[^\n]*org\.quota\.read/);
  for (const source of [bffClient, desktopClient]) {
    assert.match(source, /organizationBalance\(organizationId, signal\)[\s\S]*?`\/v1\/organizations\/\$\{organizationId\}\/balance`/);
    assert.match(source, /organizationEntitlementPackages\(organizationId, signal\)[\s\S]*?`\/v1\/organizations\/\$\{organizationId\}\/entitlement-packages`/);
  }
  assert.ok(view.includes('client.organizationBalance(organizationId, request.signal)'));
  assert.ok(view.includes('client.organizationEntitlementPackages(organizationId, request.signal)'));
  assert.match(view, /function serviceRequestFailed\(result\)[\s\S]*?status === 0 \|\| status >= 500/);
  assert.match(view, /setBillingUnavailable\(billingFailed\)/);
  assert.ok(view.includes('setBillingDetailsUnavailable([7, 8, 9].some((index) => serviceRequestFailed(results[index])))'));
});

test('Web personal center shows whole-point balances and two-decimal model usage', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const portalCredits = view.match(/function PortalCreditsUsage[\s\S]*?function PortalOrdersInvoices/)?.[0] || '';
  const entitlementRow = view.match(/function EntitlementPackageRow[\s\S]*?function EntitlementPackageDetail/)?.[0] || '';
  assert.doesNotMatch(portalCredits, /creditsUsageLead/);
  assert.match(view, /formatWholePoints\(balance\?\.available_microcredits, language\)/);
  assert.match(view, /entitlementSummary\(activePackages\.length, formatWholePoints\(remainingMicrocredits, language\)\)/);
  assert.match(entitlementRow, /data-points-format="integer"/);
  assert.match(entitlementRow, /usedOfTotal\(formatWholePoints\(item\.points_used_microcredits, language\), formatWholePoints\(item\.points_total_microcredits, language\)\)/);
  assert.doesNotMatch(entitlementRow, /usedOfTotal\(formatPoints\(/);
  assert.doesNotMatch(view, /\bformatPoints\(/);
  assert.match(view, /const summaries = summarizeUsageRecords\(records\)/);
  assert.match(view, /formatUsagePoints\(record\.actual_microcredits \|\| 0, language\)/);
});

test('Web personal center keeps the URL, selected tab, and browser history synchronized', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(view, /auth !== 'ready'[\s\S]*availablePortalSection\(globalThis\.location\?\.search \|\| '', navigationSections\)/);
  assert.match(view, /history\?\.replaceState\?\.\(\{\}, '', nextLocation\)/);
  assert.match(view, /addEventListener\?\.\('popstate', onPopState\)/);
  assert.match(view, /history\?\.pushState\?\.\(\{\}, '', nextLocation\)/);
});

test('Web personal center summarizes active benefit packages before opening full details', async () => {
  const now = Date.parse('2026-08-27T00:00:00Z');
  const items = [
    { package_id: 'later', status: 'ACTIVE', valid_until: '2026-09-30T00:00:00Z', points_total_microcredits: 100, points_used_microcredits: 25, points_remaining_microcredits: 75 },
    { package_id: 'used', status: 'CONSUMED', valid_until: '2026-08-29T00:00:00Z', points_total_microcredits: 100, points_used_microcredits: 140, points_remaining_microcredits: 0 },
    { package_id: 'revoked', status: 'REVOKED', valid_until: '2026-09-01T00:00:00Z', points_total_microcredits: 100, points_used_microcredits: 0, points_remaining_microcredits: 100 },
    { package_id: 'expired', status: 'ACTIVE', valid_until: '2026-08-01T00:00:00Z', points_total_microcredits: 0, points_used_microcredits: 0, points_remaining_microcredits: 10 },
  ];
  assert.deepEqual(activeEntitlementPackages(items, now).map((item) => item.package_id), ['used', 'later']);
  assert.equal(entitlementProgress(items[0]), 25);
  assert.equal(entitlementProgress(items[1]), 100);
  assert.equal(entitlementProgress(items[3]), 0);
  assert.equal(remainingEntitlementMicrocredits(activeEntitlementPackages(items, now)), 75);
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  assert.match(view, /activePackages\.slice\(0, 5\)/);
  assert.match(view, /copy\.entitlementSummary\(activePackages\.length/);
  assert.match(view, /mode: 'all'/);
  assert.match(view, /<EntitlementPackageDetail/);
  assert.match(view, /<section className="ap-entitlements">/);
  assert.doesNotMatch(view, /className="ap-card ap-entitlements"/);
  assert.match(styles, /\.ap-entitlements \{ margin-top: 28px; padding-top: 32px; border-top: 1px solid var\(--ap-border-soft\); \}/);
  assert.match(styles, /\.ap-entitlement-summary-list \{ display: grid; gap: 0; \}/);
  assert.doesNotMatch(styles, /\.ap-entitlement-summary-list \{[^}]*(?:border|border-radius|background)/);
  assert.match(styles, /\.ap-entitlement-row \{[^}]*padding: 18px 0;[^}]*background: transparent;/);
});

test('Web personal center renders all order states as responsive summaries with accessible details', async () => {
  const orders = [
    { order_id: 'pending', status: 'PENDING', created_at: '2026-08-28T08:00:00Z', paid_at: null, fulfillment_status: 'PENDING' },
    { order_id: 'paid', status: 'PAID', created_at: '2026-08-26T08:00:00Z', paid_at: '2026-08-26T08:10:00Z', credited_at: '2026-08-26T08:11:00Z', valid_until: '2026-09-25T08:11:00Z', fulfillment_status: 'CREDITED' },
    { order_id: 'late', status: 'PAID_LATE', created_at: '2026-08-27T08:00:00Z', paid_at: '2026-08-29T08:00:00Z', fulfillment_status: 'PENDING' },
    { order_id: 'failed', status: 'FAILED', created_at: '2026-08-25T08:00:00Z', fulfillment_status: 'PENDING' },
  ];
  assert.deepEqual(paymentOrderHistory(orders).map((item) => item.order_id), ['pending', 'late', 'paid', 'failed']);
  assert.equal(orderHistoryTimestamp(orders[0]), '2026-08-28T08:00:00Z');
  assert.equal(orderHistoryTimestamp(orders[1]), '2026-08-26T08:10:00Z');
  assert.equal(orderSummaryStatus(orders[0]), 'PENDING');
  assert.equal(orderSummaryStatus(orders[1]), 'COMPLETED');
  assert.equal(orderSummaryStatus({ status: 'PAID_LATE', fulfillment_status: 'CREDITED' }), 'LATE_COMPLETED');
  assert.equal(orderSummaryStatus(orders[2]), 'PROCESSING');
  assert.equal(orderSummaryStatus(orders[3]), 'FAILED');
  assert.equal(orderSummaryStatus({ status: 'PENDING', close_status: 'CLOSE_REQUESTED' }), 'CLOSING');
  assert.equal(orderSummaryStatus({ status: 'PENDING', close_status: 'LOCALLY_CLOSED_EXTERNAL_PENDING' }), 'EXPIRED');
  assert.equal(orderSummaryStatus({ status: 'PENDING', close_status: 'PROVIDER_CLOSED' }), 'CLOSED');
  assert.equal(orderSummaryStatus({ status: 'EXPIRED', close_status: 'LOCALLY_CLOSED_EXTERNAL_PENDING' }), 'EXPIRED');
  assert.equal(orderDurationDays(orders[1]), 30);
  assert.equal(orderDurationDays(orders[2]), null);
  assert.equal(compactOrderID('1234567890abcdef'), '12345678…');
  const now = Date.parse('2026-08-27T08:00:00Z');
  const payable = [
    { order_id: 'open', status: 'PENDING', close_status: 'OPEN', expires_at: '2026-08-27T08:10:00Z', created_at: '2026-08-27T07:59:00Z' },
    { order_id: 'expired', status: 'PENDING', close_status: 'OPEN', expires_at: '2026-08-27T07:59:59Z', created_at: '2026-08-27T07:58:00Z' },
    { order_id: 'closing', status: 'PENDING', close_status: 'CLOSE_REQUESTED', expires_at: '2026-08-27T08:10:00Z', created_at: '2026-08-27T07:57:00Z' },
    { order_id: 'paid', status: 'PAID', close_status: 'OPEN', expires_at: '2026-08-27T08:10:00Z', created_at: '2026-08-27T07:56:00Z' },
  ];
  assert.equal(isPayablePaymentOrder(payable[0], now), true);
  assert.deepEqual(payablePaymentOrders(payable, now).map((item) => item.order_id), ['open']);
  assert.equal(paymentOrderRemainingSeconds(payable[0], now), 600);
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  assert.match(view, /<table className="ap-order-summary-table">/);
  assert.match(view, /event\.key === 'Enter' \|\| event\.key === ' '/);
  assert.match(view, /<OrderDetails/);
  assert.match(styles, /\.ap-order-summary-cards \{ display: none;/);
  assert.match(styles, /@media \(max-width: 560px\)[\s\S]*\.ap-order-summary-table-wrap \{ display: none;/);
});

test('admin organization client uses protected organization, member, and unit endpoints', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify(options.method === 'POST' ? { organization_id:'org-id' } : { items:[] }), { status:options.method === 'POST' ? 201 : 200, headers:{ 'Content-Type':'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('/portal');
    client.csrfToken = 'c'.repeat(32);
    await client.createOrganization('Example', 'owner-id');
    await client.organizations('研发 & AI');
    await client.organizationMembers('org-id');
    await client.transferOrganizationOwnership('org-id', 'new-owner-id');
    await client.organizationUnits('org-id');
    await client.createOrganizationUnit('org-id', 'Engineering', 'parent-id');
    await client.renameOrganization('org-id', 'Renamed');
    await client.renameOrganizationUnit('org-id', 'unit-id', 'Platform');
    await client.deleteOrganizationUnit('org-id', 'unit-id');
    assert.deepEqual(calls.map(call => [call.url, call.options.method || 'GET']), [
      ['/portal/api/admin/account/admin/v1/organizations', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations?limit=100&q=%E7%A0%94%E5%8F%91%20%26%20AI', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/members', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/ownership', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id', 'PATCH'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units/unit-id', 'PATCH'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/units/unit-id', 'DELETE'],
    ]);
    assert.deepEqual(JSON.parse(calls[0].options.body), { name:'Example', owner_user_id:'owner-id' });
    assert.deepEqual(JSON.parse(calls[3].options.body), { new_owner_user_id:'new-owner-id' });
    assert.deepEqual(JSON.parse(calls[5].options.body), { name:'Engineering', parent_id:'parent-id' });
    assert.deepEqual(JSON.parse(calls[6].options.body), { name:'Renamed' });
    assert.deepEqual(JSON.parse(calls[7].options.body), { name:'Platform' });
  } finally { globalThis.fetch = originalFetch; }
});

test('user code is displayed to its owner and resolved through a dedicated exact admin endpoint', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    calls.push(url);
    return new Response(JSON.stringify({ user_id:'user-id', user_code:'USR-0123-4567-89AB-CDEF-MV', status:'ACTIVE' }), { status:200, headers:{ 'Content-Type':'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('/portal');
    await client.userByCode('usr-0123-4567-89ab-cdef-mv');
    assert.deepEqual(calls, ['/portal/api/admin/account/admin/v1/user-code-lookups/usr-0123-4567-89ab-cdef-mv']);
  } finally { globalThis.fetch = originalFetch; }

  const personal = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const admin = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(personal, /me\?\.user_code/);
  assert.match(popover, /me\?\.user_code/);
  assert.match(admin, /client\.userByCode/);
  assert.match(admin, /key:\s*'user_code'/);
  assert.doesNotMatch(personal, /me\?\.user_id/);
  for (const locale of ['zh', 'en', 'ja']) assert.ok(accountPlatformCopy[locale].userCode);
});

test('admin user workspace uses cursor pages and all three protected repair routes', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push([url, options.method || 'GET']);
    return new Response(JSON.stringify({ items: [], next_cursor: null }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('/portal');
    client.csrfToken = 'c'.repeat(32);
    await client.users('vin pay', undefined, 'opaque-cursor');
    await client.registrationRepairs({ status: 'REPAIR_REQUIRED', older_than_minutes: '30' });
    await client.profileChangeRepairs();
    await client.accountClosures();
    await client.retryRegistration('registration-id');
    await client.compensateRegistration('registration-id');
    await client.retryProfileChange('profile-id');
    assert.deepEqual(calls, [
      ['/portal/api/admin/account/admin/v1/users?limit=25&q=vin+pay&cursor=opaque-cursor', 'GET'],
      ['/portal/api/admin/account/admin/v1/registration-operations?limit=100&status=REPAIR_REQUIRED&older_than_minutes=30', 'GET'],
      ['/portal/api/admin/account/admin/v1/profile-change-requests', 'GET'],
      ['/portal/api/admin/account/admin/v1/account-closure-requests', 'GET'],
      ['/portal/api/admin/account/admin/v1/registration-operations/registration-id/actions/retry', 'POST'],
      ['/portal/api/admin/account/admin/v1/registration-operations/registration-id/actions/compensate', 'POST'],
      ['/portal/api/admin/account/admin/v1/profile-change-requests/profile-id/actions/retry', 'POST'],
    ]);
  } finally { globalThis.fetch = originalFetch; }
});

test('admin organization units form a nested management tree', () => {
  const roots = buildUnitTree([
    { unit_id:'child', parent_id:'root', name:'Child' },
    { unit_id:'root', parent_id:null, name:'Root' },
  ]);
  assert.equal(roots.length, 1);
  assert.equal(roots[0].unit_id, 'root');
  assert.equal(roots[0].children[0].unit_id, 'child');
});

test('context guard invalidates and aborts stale organization loads', () => {
  const guard = createContextRequestGuard();
  const first = guard.begin();
  const second = guard.begin();
  assert.equal(first.signal.aborted, true);
  assert.equal(guard.isCurrent(first.generation), false);
  assert.equal(guard.isCurrent(second.generation), true);
});

test('recharge polling backs off from two to five seconds with bounded jitter', () => {
  assert.equal(rechargePollDelay(0, () => 0.5), 2000);
  assert.equal(rechargePollDelay(3, () => 0.5), 3500);
  assert.equal(rechargePollDelay(99, () => 0.5), 5000);
  assert.ok(rechargePollDelay(0, () => 0) >= 1800);
  assert.ok(rechargePollDelay(99, () => 1) <= 5500);
});

test('organization context never accepts a personal Token candidate', () => {
  const items = [{ token_account_id: 'personal', owner_type: 'USER' }, { token_account_id: 'organization', owner_type: 'ORG' }];
  assert.deepEqual(eligibleForContext(items, 'organization-id').map(item => item.token_account_id), ['organization']);
  assert.deepEqual(eligibleForContext(items, '').map(item => item.token_account_id), ['personal']);
});

test('BFF client keeps CSRF on mutations and never persists OIDC tokens', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (String(url).endsWith('/api/session')) return new Response(JSON.stringify({ authenticated: true, csrf_token: 'c'.repeat(32) }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    await client.session();
    await client.switchContext('PERSONAL', null, 7);
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'c'.repeat(32));
    assert.equal(calls[1].options.credentials, 'same-origin');
    assert.equal(JSON.parse(calls[1].options.body).expected_context_version, 7);
    assert.equal('accessToken' in client, false);
    assert.equal('refreshToken' in client, false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF session distinguishes an expired login from a temporary service failure', async () => {
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response(JSON.stringify({ authenticated: false }), {
      status: 401,
      headers: { 'Content-Type': 'application/json' },
    });
    const client = new AccountPlatformBffClient('https://portal.example');
    assert.deepEqual(await client.session(), { authenticated: false });

    globalThis.fetch = async () => new Response(JSON.stringify({ code: 'SESSION_STORE_UNAVAILABLE', message: 'temporary failure' }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' },
    });
    await assert.rejects(client.session(), (error) => error.status === 503 && error.code === 'SESSION_STORE_UNAVAILABLE');
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF client restores the recent order without browser persistence', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ order: null }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    assert.deepEqual(await client.recentRechargeOrder(), { order: null });
    assert.equal(calls[0].url, '/portal/api/token/v1/recharge/orders/recent');
    assert.equal(calls[0].options.cache, 'no-store');
    assert.equal('recentOrder' in client, false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('personal usage history is read-only and never resends a model request', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ items: [{ model: 'example-model', display_status: 'SETTLED', actual_microcredits: 10_000 }] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    const history = await client.usageHistory();
    assert.equal(history.items[0].display_status, 'SETTLED');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, '/portal/api/token/v1/me/usage-requests?limit=20');
    assert.equal(calls[0].options.method, 'GET');
    assert.equal(calls[0].url.includes('/chat/completions'), false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('desktop client keeps OAuth tokens behind the account-platform Bridge', async () => {
  const calls = [];
  const transport = {
    session: async () => ({ enabled: true, configured: true, authenticated: true }),
    login: async credentials => {
      calls.push({ credentials });
      return { authenticated: true };
    },
    logout: async () => ({ authenticated: false }),
    request: async request => {
      calls.push(request);
      return { status: 200, body: { items: [] } };
    },
  };
  const client = new AccountPlatformDesktopClient(transport);
  assert.equal((await client.session()).authenticated, true);
  await client.login('person@example.com', 'secret-password');
  await client.createRecharge('product-10', 'ALIPAY', 'recharge-id');
  assert.deepEqual(calls[0], { credentials: { account: 'person@example.com', password: 'secret-password' } });
  assert.deepEqual(calls[1], {
    area: 'token',
    resource: '/v1/recharge/orders',
    method: 'POST',
    body: { product_id: 'product-10', channel: 'ALIPAY' },
    idempotencyKey: 'recharge-id',
  });
  assert.equal('accessToken' in client, false);
  assert.equal('refreshToken' in client, false);
});

test('desktop client surfaces backend error codes without exposing auth state', async () => {
  const client = new AccountPlatformDesktopClient({
    request: async () => ({ status: 403, body: { code: 'permission_denied', message: 'denied' } }),
  });
  await assert.rejects(client.organizations(), error => {
    assert.equal(error.code, 'permission_denied');
    assert.equal(error.status, 403);
    return true;
  });
});

test('account clients publish only increasing global context versions', async () => {
  const originalFetch = globalThis.fetch;
  const observed = [];
  let version = 4;
  globalThis.fetch = async () => new Response(JSON.stringify({ items: [] }), {
    status: 200,
    headers: { 'Content-Type': 'application/json', 'X-Context-Version': String(version) },
  });
  try {
    const client = new AccountPlatformBffClient('/portal');
    const unsubscribe = client.onContextVersion(value => observed.push(value));
    await client.organizations();
    version = 3;
    await client.organizations();
    version = 6;
    await client.organizations();
    unsubscribe();
    assert.deepEqual(observed, [4, 6]);
    assert.equal(client.contextVersion, 6);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF client refreshes a stale CSRF token once before replaying a rejected mutation', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  let sessionCalls = 0;
  let mutationCalls = 0;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (String(url).endsWith('/api/session')) {
      sessionCalls += 1;
      const token = sessionCalls === 1 ? 'o'.repeat(32) : 'n'.repeat(32);
      return new Response(JSON.stringify({ authenticated: true, csrf_token: token }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
    mutationCalls += 1;
    if (mutationCalls === 1) {
      return new Response(JSON.stringify({ code: 'CSRF_REJECTED', message: 'stale token' }), { status: 403, headers: { 'Content-Type': 'application/json' } });
    }
    return new Response(JSON.stringify({ active_context: { context_version: 8 } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    await client.session();
    const result = await client.switchContext('PERSONAL', null, 7);
    assert.equal(result.active_context.context_version, 8);
    assert.equal(calls.length, 4);
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'o'.repeat(32));
    assert.equal(calls[3].options.headers['X-CSRF-Token'], 'n'.repeat(32));
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF client never retries a non-CSRF authorization denial', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (String(url).endsWith('/api/session')) return new Response(JSON.stringify({ authenticated: true, csrf_token: 'c'.repeat(32) }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    return new Response(JSON.stringify({ code: 'FORBIDDEN', message: 'permission denied' }), { status: 403, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    await client.session();
    await assert.rejects(client.switchContext('PERSONAL', null, 7), error => error.code === 'FORBIDDEN');
    assert.equal(calls.length, 2);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('personal usage history supports opaque pagination without exposing routing secrets', async () => {
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(bffClient, /cursor=\$\{encodeURIComponent\(cursor\)\}/);
  assert.match(view, /record\.input_tokens/);
  assert.match(view, /record\.output_tokens/);
  assert.match(view, /page\.next_cursor/);
  assert.match(view, /setUsageRecords\(page\.items \|\| \[\]\)/);
  assert.match(view, /usagePageCursors\[targetPage - 1\]/);
  assert.match(view, /copy\.pageNumber\(page\)/);
  assert.match(view, /hasPrevious=\{usagePage > 1\}/);
  assert.equal((i18n.match(/usagePagination:/g) || []).length, 3);
  assert.equal((i18n.match(/previousPage:/g) || []).length, 3);
  assert.equal((i18n.match(/nextPage:/g) || []).length, 3);
  assert.doesNotMatch(view, /record\.(?:token_account_id|credential_id|wallet_id|provider)/);
});

test('desktop account entry is a popover and no longer mounts a full workspace', async () => {
  const source = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  assert.match(source, /data-testid="account-platform-button"/);
  assert.match(source, /<AccountPlatformPopover/);
  assert.doesNotMatch(source, /accountPlatformEnabled && accountPlatformClient/);
  assert.doesNotMatch(source, /data-testid="llmapi-account-button"/);
  assert.doesNotMatch(source, /<AccountPopover/);
  assert.doesNotMatch(source, /navigateFromScheduledRun\('accountPlatform'\)/);
  assert.doesNotMatch(source, /currentView === 'accountPlatform' && \(/);
  assert.doesNotMatch(source, /<AccountPlatformView/);
});

test('desktop account entry uses the same monochrome round icon treatment as sidebar controls', async () => {
  const source = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  const entry = source.match(/data-testid="account-platform-button"[\s\S]*?<AccountPlatformPopover/)?.[0] || '';
  assert.match(entry, /rounded-full/);
  assert.match(entry, /<User size=\{18\}/);
  assert.doesNotMatch(entry, /bg-gradient-to-br/);
});

test('desktop sign-in renders inside the popover and never opens a browser', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const bridge = await readFile(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.match(popover, /type="password"/);
  assert.match(popover, /'password', 'sms', 'register'/);
  assert.match(popover, /copy\.registrationUsername/);
  assert.match(popover, /client\.registerMobile\(account\.trim\(\), mobile\.trim\(\)/);
  assert.match(popover, /client\.precheckMobileRegistration\(account\.trim\(\), mobile\.trim\(\), password\)/);
  assert.match(popover, /client\.createSliderCaptcha\(mobile\.trim\(\)/);
  assert.match(popover, /<SliderCaptcha/);
  assert.match(popover, /setSmsCooldown\(Math\.max\(1, Number\(result\.retry_after_seconds \|\| 60\)\)\)/);
  assert.match(popover, /smsCooldown > 0 \? copy\.resendCountdown\(smsCooldown\)/);
  assert.match(popover, /autoComplete=\{loginMode === 'register' \? 'new-password' : 'current-password'\}/);
  assert.match(popover, /await client\.loginSms\(mobile\.trim\(\), smsChallenge, smsCode\.trim\(\)\)/);
  assert.match(popover, /await client\.registerMobile/);
  assert.match(popover, /try \{\s*await client\.login\(registeredUsername, password\);\s*\} catch \{/);
  assert.match(popover, /setError\(copy\.registrationCompletedLoginFailed\)/);
  assert.match(popover, /setLoginMode\('password'\)/);
  assert.match(bridge, /invoke\("account_platform_login", \{ credentials: credentials \}\)/);
  const loginImplementation = backend.match(/pub async fn login\([sS]*?pub async fn send_sms/)?.[0] || '';
  assert.doesNotMatch(loginImplementation, /open_target|authorization_request|TcpListener/);
});

test('desktop mobile registration confirms matching passwords before network requests', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.match(popover, /const \[passwordConfirmation, setPasswordConfirmation\] = useState\(''\)/);
  assert.match(popover, /if \(password !== passwordConfirmation\) return copy\.passwordMismatch/);
  assert.match(popover, /registrationInputError\(copy, account, mobile, password, passwordConfirmation\)/);
  assert.match(popover, /placeholder=\{copy\.confirmPassword\}[\s\S]*?value=\{passwordConfirmation\}|value=\{passwordConfirmation\}[\s\S]*?placeholder=\{copy\.confirmPassword\}/);
  assert.match(popover, /!passwordConfirmation \|\| password !== passwordConfirmation/);
  assert.equal(accountPlatformCopy.zh.mobileOrAccount, '手机号码或用户名');
  assert.equal(accountPlatformCopy.zh.password, '密码（至少 8 位，含字母和数字）');
  assert.equal(accountPlatformCopy.zh.confirmPassword, '再次输入密码');
  assert.equal(accountPlatformCopy.zh.passwordMismatch, '两次输入的密码不一致。');
});

test('desktop retains forgotten password while profile changes are Web-only', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const webView = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const webClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const portal = await readFile(new URL('../../account-platform/internal/portal/service.go', import.meta.url), 'utf8');
  const accountRoutes = await readFile(new URL('../../account-platform/internal/account/service.go', import.meta.url), 'utf8');
  const accountProfile = await readFile(new URL('../../account-platform/internal/account/profile_changes.go', import.meta.url), 'utf8');
  assert.match(popover, /'password', 'sms', 'register', 'forgot'/);
  assert.match(popover, /await client\.resetPassword\(mobile\.trim\(\), smsChallenge, smsCode\.trim\(\), password\)/);
  assert.doesNotMatch(popover, /profileSettings|profileChange|PROFILE_MOBILE/);
  assert.match(client, /resetPassword\(mobile, challengeId, code, password/);
  assert.doesNotMatch(client, /ProfileChange|profile-change-requests/);
  assert.match(backend, /PasswordReset|reset_password|password-resets|PASSWORD_RESET/);
  assert.doesNotMatch(backend, /profile-change-requests|PROFILE_MOBILE/);
  assert.doesNotMatch(client, /password-resets/);
  assert.match(webView, /section === 'profile'/);
  assert.match(webView, /client\.createProfileChange\('USERNAME'/);
  assert.match(webView, /client\.sendProfileMobileChallenge/);
  assert.match(webClient, /reauthenticate=1/);
  assert.match(webClient, /section=accountSettings/);
  assert.match(webView, /client\.reauthenticate\(`\/\?section=accountSettings&profile=\$\{field\}`\)/);
  assert.doesNotMatch(webView, /client\.reauthenticate\(`?\/account/);
  assert.match(webView, /return_to=%2F%3Fsection%3DaccountSettings/);
  assert.match(webClient, /reauthenticate\(returnTo = '\/\?section=accountSettings'\)/);
  assert.doesNotMatch(webClient, /returnTo = '\/account/);
  assert.match(webClient, /profile-change-requests/);
  assert.match(portal, /query\.Set\("prompt", "login"\)/);
  assert.match(portal, /query\.Set\("max_age", "900"\)/);
  assert.match(accountRoutes, /POST\("\/v1\/auth\/password-resets"/);
  assert.match(accountRoutes, /POST\("\/v1\/me\/profile-change-requests"/);
  assert.match(accountProfile, /case "USERNAME":/);
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(accountPlatformCopy[language].authModes.forgot);
    assert.ok(accountPlatformCopy[language].profileSettings);
    assert.ok(accountPlatformCopy[language].changeUsername);
    assert.ok(accountPlatformCopy[language].changeMobile);
  }
});

test('slider verification overlays the login form without changing its layout height', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const loginForm = popover.match(/<form className=\{`space-y-3[\s\S]*?<\/form>/)?.[0] || '';
  assert.doesNotMatch(loginForm, /<SliderCaptcha/);
  assert.match(popover, /role="dialog" aria-modal="true"[\s\S]*?absolute inset-0 z-20/);
  assert.match(popover, /onClose=\{\(\) => \{ if \(busy\) return; setCaptcha\(null\); setCaptchaOffset\(0\); \}\}/);
  assert.match(popover, /if \(captcha && !busy\)[\s\S]*?setCaptcha\(null\)/);
  assert.equal(accountPlatformCopy.zh.captchaClose, '关闭验证');
});

test('desktop account popover does not expose the internal user id', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.doesNotMatch(popover, /me\?\.user_id/);
});

test('desktop account popover only loads the user and balance summary', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.doesNotMatch(popover, /client\.organizations/);
  assert.doesNotMatch(popover, /client\.managedTokens/);
  assert.doesNotMatch(popover, /client\.switchContext/);
  assert.doesNotMatch(popover, /copy\.activeContext/);
  assert.doesNotMatch(popover, /copy\.accountStatus/);
});

test('desktop recharge completes inside the account popover without opening a web page', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.match(popover, /copy\.balance[\s\S]*copy\.recharge/);
  assert.match(popover, /client\.products\(controller\.signal\)/);
  assert.doesNotMatch(popover, /client\.recentRechargeOrder/);
  assert.match(popover, /const openRecharge = \(\) => \{\s*setOrder\(null\)/);
  assert.match(popover, /client\.createRecharge\(productId, channel, newIdempotencyKey\(\)\)/);
  assert.match(popover, /order\?\.qr_data/);
  assert.doesNotMatch(popover, /import\('qrcode'\)/);
  assert.match(popover, /client\.rechargeOrder\(order\.order_id\)/);
  assert.match(popover, /refreshedBalance = await client\.balance\(\)/);
  assert.match(popover, /copy\.paymentReceived/);
  assert.doesNotMatch(popover, /target=["']_blank["']|window\.open|open_target|href=\{order\.(?:payment_url|qr_content)\}/);
  assert.match(backend, /\("token", &Method::GET, \["v1", "recharge", "products"\], None\)/);
  assert.match(backend, /\("token", &Method::POST, \["v1", "recharge", "orders"\], None\)/);
  assert.match(backend, /\("token", &Method::GET, \["v1", "recharge", "orders", id\], None\)[\s\S]{0,500}?=> safe_id\(id\)/);
});

test('desktop account persists only refresh credentials and clears them on logout or auth failure', async () => {
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const credentialStore = await readFile(new URL('../src-tauri/src/platform/credential_store.rs', import.meta.url), 'utf8');
  assert.match(backend, /SystemCredentialStore/);
  assert.match(backend, /for_account_platform_refresh_token/);
  assert.match(backend, /self\.credentials\s*\.set/);
  assert.match(backend, /self\.credentials\s*\.delete/);
  assert.match(backend, /ensure_restored/);
  assert.match(backend, /self\.clear_authentication\(\)\.await\?/);
  assert.doesNotMatch(backend, /\.set\([^\n]*password/);
  assert.match(backend, /refresh_token: String/);
  assert.match(credentialStore, /pinvou3-account-platform-refresh-token/);
});

test('built-in desktop chat uses the in-memory OIDC session and Token Gateway route', async () => {
  const accountBackend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const runtimeProvider = await readFile(new URL('../src-tauri/src/features/account_platform/runtime_model.rs', import.meta.url), 'utf8');
  assert.match(accountBackend, /chat_runtime_authorization/);
  assert.match(accountBackend, /token_gateway_openai_base/);
  assert.match(accountBackend, /\("token", &Method::POST, \["v1", "chat", "completions"\], None\)/);
  assert.match(runtimeProvider, /chat_runtime_authorization\(\)/);
  assert.match(runtimeProvider, /model\.base_url = authorization\.base_url/);
  assert.match(runtimeProvider, /RuntimeModelCredential::api_key\([\s\S]*authorization\.access_token/);
});

test('personal center and desktop chat refresh the built-in runtime after account or catalog changes', async () => {
  const accountBackend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const commands = await readFile(new URL('../src-tauri/src/app/commands/account_platform.rs', import.meta.url), 'utf8');
  assert.match(accountBackend, /bootstrap_account_identity/);
  assert.match(accountBackend, /account_user_id: Option<String>/);
  assert.match(accountBackend, /oidc-user-\{account_user_id\}-session-\{generation\}/);
  assert.equal(
    (commands.match(/pool\.mark_model_updated\(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID\)/g) || []).length,
    4,
  );
});

test('legacy built-in model data has an idempotent client-only migration', async () => {
  const migration = await readFile(new URL('../src-tauri/src/features/account_platform/client_migration.rs', import.meta.url), 'utf8');
  const prefs = await readFile(new URL('../src-tauri/src/platform/prefs/mod.rs', import.meta.url), 'utf8');
  assert.match(migration, /sessions.*_session_models\.json/s);
  assert.match(migration, /scheduled-runs.*session-profiles\.json/s);
  assert.match(migration, /legacy-llmapi-bindings-v1\.json/);
  assert.match(migration, /MigrationState::CleanupPending/);
  assert.match(prefs, /migrate_account_platform_model_path/);
  assert.match(prefs, /LEGACY_BUILTIN_LLMAPI_MODEL_ID/);
  assert.match(prefs, /BUILTIN_ACCOUNT_PLATFORM_MODEL_ID/);
});

test('new desktop build has no legacy model runtime or compatibility switch', async () => {
  const lib = await readFile(new URL('../src-tauri/src/lib.rs', import.meta.url), 'utf8');
  const features = await readFile(new URL('../src-tauri/src/features/mod.rs', import.meta.url), 'utf8');
  const account = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.doesNotMatch(lib, /commands::llmapi|features::llmapi_hub/);
  assert.doesNotMatch(features, /llmapi_hub/);
  assert.doesNotMatch(account, /PINVOU_ACCOUNT_PLATFORM_ENABLED|account_platform_disabled/);

  const allowedMigrationFiles = new Set([
    '/src-tauri/src/features/account_platform/client_migration.rs',
    '/src-tauri/src/platform/prefs/mod.rs',
    '/src-tauri/src/platform/prefs/model.rs',
  ]);
  for (const file of [
    ...await sourceFiles(new URL('../src/', import.meta.url)),
    ...await sourceFiles(new URL('../src-tauri/src/', import.meta.url)),
  ]) {
    const pathname = file.pathname.replace(/\\/g, '/');
    if ([...allowedMigrationFiles].some(suffix => pathname.endsWith(suffix))) continue;
    const source = await readFile(file, 'utf8');
    assert.doesNotMatch(source, /builtin_llmapi|llmapi_hub|get_llmapi|commands::llmapi|PINVOU_ACCOUNT_PLATFORM_ENABLED/, file.pathname);
  }
});

test('desktop account restoration starts with the app without blocking startup', async () => {
  const source = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  assert.match(source, /accountPlatformClient\.session\(controller\.signal\)\.catch/);
});

test('admin console exposes users, roles, and organizations as independently named HTML prototype pages', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /section === 'organizations'/);
  assert.match(view, /section === 'users'/);
  assert.match(view, /section === 'roles'/);
  assert.doesNotMatch(view, /section === 'accounts'/);
  assert.doesNotMatch(i18n, /账号与组织/);
  assert.match(i18n, /users:\s*'用户管理',\s*roles:\s*'角色与授权',\s*organizations:\s*'组织管理'/);
  assert.match(view, /key:\s*'username',\s*label:\s*copy\.userManagement\.account/);
  assert.doesNotMatch(view, /copy\.accounts\.email/);
  assert.doesNotMatch(view, /function userLabel\(user\) \{[\s\S]*?user\?\.email[\s\S]*?\}/);
  assert.doesNotMatch(view, /item\.display_name \|\| item\.email/);
  assert.match(view, /client\.users\(search\.trim\(\), undefined, nextCursor\)/);
  assert.match(view, /client\.userByCode\(code\)/);
  assert.match(view, /copy\.userManagement\.confirmDisable/);
  assert.match(view, /id === currentUserID/);
  assert.match(i18n, /username:\s*'用户名'/);
  assert.doesNotMatch(i18n, /搜索用户名、名称、邮箱/);
});

test('admin authorization refresh retries transient browser failures without reporting a service outage', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const refreshEffect = view.slice(view.indexOf("if (auth !== 'ready') return undefined;"), view.indexOf("if (auth !== 'ready' || !authorization) return undefined;"));
  assert.match(refreshEffect, /refreshAuthorization = async \(isRetry = false\)/);
  assert.match(refreshEffect, /setTimeout\?\.\(\(\) => \{[\s\S]*?refreshAuthorization\(true\)[\s\S]*?\}, 1500\)/);
  assert.match(refreshEffect, /reason\?\.status === 401\) setAuth\('guest'\)/);
  assert.match(refreshEffect, /reason\?\.status === 403\) setAuth\('denied'\)/);
  assert.doesNotMatch(refreshEffect, /setError\(copy\.failed\)/);
});

test('personal center keeps context on overview and cannot create organizations', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const desktopClient = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(i18n, /title:'个人中心'/);
  assert.match(i18n, /activeContext:'当前身份', switchContext:'切换身份', personal:'个人账号'/);
  assert.doesNotMatch(i18n, /当前上下文|身份上下文/);
  assert.match(view, /function Overview[\s\S]*copy\.activeContext/);
  assert.equal((view.match(/copy\.activeContext/g) || []).length, 1);
  assert.match(view, /copy\.organizationIdentity\(activeOrganization\.name\)/);
  assert.doesNotMatch(view, /client\.createOrganization/);
  assert.doesNotMatch(bffClient, /createOrganization\(/);
  assert.doesNotMatch(desktopClient, /createOrganization\(/);
  assert.match(styles, /\.ap-sidebar-foot select \{[^}]*color: var\(--ap-primary\);[^}]*background: var\(--ap-component\);/);
});

test('Web personal center consolidates navigation while preserving context-sensitive capabilities', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /organizations\.length > 0 && canReadOrganization \? \['organization'\] : \[\]/);
  assert.match(view, /access\.profileRead \|\| access\.billingRead \? \['overview'\]/);
  assert.match(view, /access\.billingRead \? \['credits', 'entitlements'\]/);
  assert.match(view, /access\.usageRead \? \['usage'\]/);
  assert.match(view, /personalContext && access\.billingRead \? \['orders', 'invoice'\] : \[\]/);
  assert.match(view, /portal \? portalNavigationSections/);
  assert.match(view, /section === 'creditsUsage'[\s\S]*<PortalCreditsUsage/);
  assert.match(view, /section === 'ordersInvoices'[\s\S]*<PortalOrdersInvoices/);
  assert.match(view, /section === 'myOrganization'[\s\S]*<PortalMyOrganization/);
  assert.match(view, /section === 'accountSettings'[\s\S]*<PortalAccountSettings/);
  assert.doesNotMatch(view, /section === 'wallet'/);
  assert.match(view, /personalContext \? \([\s\S]*<Wallet[\s\S]*: \([\s\S]*<ContextCredits/);
  assert.match(view, /function PortalMyOrganization[\s\S]*<OrganizationEntitlements[\s\S]*<Organization/);
  const creditsUsage = view.slice(view.indexOf('function PortalCreditsUsage'), view.indexOf('function PortalMyOrganization'));
  const accountSettings = view.slice(view.indexOf('function PortalAccountSettings'), view.indexOf('function Dialog'));
  assert.doesNotMatch(creditsUsage, /OrganizationEntitlements/);
  assert.doesNotMatch(accountSettings, /<Organization\b/);
  assert.match(view, /summary \? paymentOrderHistory\(orders\) : orders/);
  assert.equal((i18n.match(/creditsTitle:/g) || []).length, 3);
  assert.equal((i18n.match(/entitlementsTitle:/g) || []).length, 3);
  assert.match(view, /auth !== 'ready'[\s\S]*availablePortalSection\(globalThis\.location\?\.search \|\| '', navigationSections\)/);
  assert.match(view, /<nav>\s*\{navigationSections\.map/);
});

test('personal center exposes only user-facing usage history instead of Token administration', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const bffClient = await readFile(new URL('../src/features/account-platform/bff-client.js', import.meta.url), 'utf8');
  const desktopClient = await readFile(new URL('../src/features/account-platform/desktop-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/account-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /section === 'usage'/);
  assert.match(view, /client\.usageHistory\('', request\.signal\)/);
  assert.match(i18n, /usage:'用量记录'/);
  assert.doesNotMatch(view, /section === 'tokens'|client\.(managedTokens|eligibleTokens|credentials|rotateCredential|createTokenAccount)/);
  assert.doesNotMatch(i18n, /Token 管理|供应商凭据|模型消费准备状态|消费结算状态|可管理 Token/);
  assert.match(bffClient, /usageHistory\(cursor = '', signal\)/);
  assert.match(desktopClient, /usageHistory\(cursor = '', signal\)/);
  assert.doesNotMatch(`${bffClient}\n${desktopClient}`, /managedTokens|eligibleTokens|rotateCredential|createTokenAccount/);
});

test('account portal uses stable asset names with revalidated caching', async () => {
  const viteConfig = await readFile(new URL('../vite.config.js', import.meta.url), 'utf8');
  const portalService = await readFile(new URL('../../account-platform/internal/portal/service.go', import.meta.url), 'utf8');
  assert.match(viteConfig, /entryFileNames: 'assets\/\[name\]\.js'/);
  assert.match(viteConfig, /chunkFileNames: 'assets\/\[name\]\.js'/);
  assert.match(viteConfig, /assetFileNames: 'assets\/\[name\]\[extname\]'/);
  assert.match(portalService, /Header\(\)\.Set\("Cache-Control", "no-cache"\)/);
});

test('personal center uses the canonical account.pinvou.com origin end to end', async () => {
  const desktop = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const portal = await readFile(new URL('../../account-platform/internal/portal/static.go', import.meta.url), 'utf8');
  const nginx = await readFile(new URL('../../deploy/nginx-pinvou-account-domain.conf', import.meta.url), 'utf8');
  assert.match(desktop, /DEFAULT_PORTAL_URL: &str = "https:\/\/account\.pinvou\.com\/"/);
  assert.doesNotMatch(desktop, /join\("account\.html"\)/);
  assert.match(portal, /switch r\.URL\.Path \{[\s\S]*case "\/":/);
  assert.doesNotMatch(portal, /r\.URL\.Path == "\/account"/);
  assert.match(nginx, /server_name account\.pinvou\.com/);
  assert.match(nginx, /location = \/api\/v1\/payment-callback\/token \{/);
  assert.match(nginx, /location = \/api\/v1\/payment-callbacks \{/);
  assert.match(nginx, /location \/ \{[\s\S]*proxy_pass http:\/\/127\.0\.0\.1:18105/);
  assert.doesNotMatch(nginx, /location \^~ \/assets\//);
});

test('personal center follows the restrained neutral visual system', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  assert.match(view, /className="ap-sidebar-brand"/);
  assert.match(view, /className="ap-user-summary"/);
  assert.match(view, /<NavIcon name=\{key\}/);
  assert.match(view, /<AccountProfileCard/);
  assert.match(view, /<IdentityDialog/);
  assert.match(view, /<ProfileChangeDialog/);
  assert.match(styles, /--ap-page: #f7f8fa/);
  assert.match(styles, /--ap-primary: #202124/);
  assert.match(styles, /--ap-accent: #1e8e3e/);
  assert.match(styles, /grid-template-columns: 250px minmax\(0, 1fr\)/);
  assert.match(styles, /max-width: 1000px/);
  assert.match(styles, /\.ap-profile-card \{[\s\S]*border-radius: 12px/);
  assert.match(styles, /\.ap-dialog-backdrop[\s\S]*rgba\(0, 0, 0, \.45\)/);
  assert.match(styles, /\.ap-product-grid button\.selected \{ color: var\(--ap-primary\);[\s\S]*background: #ececec/);
  assert.match(styles, /\.ap-sidebar nav button \{ color: rgba\(27, 26, 36, \.42\); \}/);
  assert.match(styles, /\.ap-sidebar nav button\.active \{ color: var\(--ap-primary\);/);
});

test('account settings uses row-based account facts without duplicating credit data', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const styles = await readFile(new URL('../src/features/account-platform/account-platform.css', import.meta.url), 'utf8');
  const start = view.indexOf('function PortalAccountSettings');
  const end = view.indexOf('function IdentityDialog', start);
  const accountSettings = view.slice(start, end);

  assert.ok(start >= 0 && end > start, 'account settings source must remain independently inspectable');
  assert.match(accountSettings, /className="ap-profile-details"/);
  assert.match(accountSettings, /copy\.activeContext/);
  assert.match(accountSettings, /copy\.userCode/);
  assert.match(accountSettings, /copy\.organizations/);
  assert.match(accountSettings, /copy\.accountStatuses\?\.\[me\?\.status\]/);
  assert.match(accountSettings, /copy\.currentUsername/);
  assert.match(accountSettings, /copy\.currentMobile/);
  const identityIndex = accountSettings.indexOf('copy.activeContext');
  const usernameIndex = accountSettings.indexOf('copy.currentUsername');
  const mobileIndex = accountSettings.indexOf('copy.currentMobile');
  const userCodeIndex = accountSettings.indexOf('copy.userCode');
  const organizationsIndex = accountSettings.indexOf('copy.organizations');
  assert.ok(identityIndex < usernameIndex && usernameIndex < mobileIndex && mobileIndex < userCodeIndex && userCodeIndex < organizationsIndex);
  assert.doesNotMatch(accountSettings, /copy\.availableCredits/);
  assert.doesNotMatch(accountSettings, /available_microcredits/);
  assert.doesNotMatch(accountSettings, /\bbalance\b/);
  assert.match(view, /className="ap-card ap-danger-card"/);
  assert.match(styles, /\.ap-profile-detail-row \{ display: grid; grid-template-columns: 168px minmax\(0, 1fr\);[^}]*min-height: 56px/);
  assert.match(styles, /\.ap-profile-identity-row \{ min-height: 60px; \}/);
  assert.match(styles, /\.ap-account-state-active \{ color: #1e8e3e; background: #e6f4ea; \}/);
  assert.match(styles, /@media \(max-width: 560px\)[\s\S]*\.ap-profile-detail-row \{ grid-template-columns: 1fr/);
  assert.match(styles, /\.ap-danger-card \.ap-primary \{ color: #d93025/);
});

test('admin organization detail supports ownership transfer, rename, leaf deletion, and list search', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  assert.match(view, /client\.organizationMembers\(organization\.organization_id/);
  assert.match(view, /client\.transferOrganizationOwnership\(organization\.organization_id, target\.user_id\)/);
  assert.match(view, /copy\.accounts\.confirmOwnership/);
  assert.match(view, /copy\.accounts\.transferOwner/);
  assert.match(i18n, /transferOwner:\s*'转让所有权'/);
  assert.match(view, /client\.renameOrganization\(organization\.organization_id, nextName\)/);
  assert.match(view, /client\.renameOrganizationUnit\(organization\.organization_id, unit\.unit_id, trimmed\)/);
  assert.match(view, /client\.deleteOrganizationUnit\(organization\.organization_id, unit\.unit_id\)/);
  assert.match(view, /unit\.children\.length > 0/);
  assert.match(view, /copy\.accounts\.organizationSearch/);
  assert.match(view, /editingOrganizationName \?\s*\(\s*<form/);
  assert.match(view, /setEditingOrganizationName\(true\)/);
});

test('payment capabilities isolate provider limitations and enable reviewed-order repair', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  const provider = await readFile(new URL('../../account-platform/internal/platform/h3cpayment/provider.go', import.meta.url), 'utf8');
  assert.match(provider, /ProviderID: "h3c_mall"/);
  assert.match(provider, /OrderQuerySupported: true/);
  assert.match(provider, /ManualRepairSupported: true/);
  assert.match(view, /refundSupported = paymentCapabilities\.refund_supported === true/);
  assert.match(view, /reconciliationSupported = paymentCapabilities\.bill_supported === true/);
  assert.match(view, /reconciliationImportSupported = paymentCapabilities\.bill_file_import_supported === true/);
  assert.match(view, /repairSupported = paymentCapabilities\.manual_repair_supported === true/);
  assert.match(view, /client\.repairPaymentOrder\(item\.order_id, reason\)/);
  assert.doesNotMatch(view, /paymentCapabilities\.provider === 'h3c_mall'/);
  assert.match(view, /refundSupported && item\.status === 'ACTIVE'/);
  assert.match(view, /disabled=\{busy \|\| !repairSupported/);
  assert.match(view, /paymentCapabilities\.reconciliation_mode/);
  assert.match(i18n, /不支持的退款、主动关单、支付回调或日终账单对账操作已自动禁用/);
});

test('late-payment review operations expose filters, evidence, and audited commands', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  for (const field of ['provider_id','owner_id','entered_from','entered_to','next_review_before','overdue']) assert.match(view, new RegExp(`lateFilter\\.${field}`));
  for (const method of ['claimLatePaymentReview','queryLatePaymentReview','scheduleLatePaymentReview','archiveLatePaymentReview','escalateProviderBlind']) assert.match(view, new RegExp(`client\\.${method}`));
  assert.match(view, /lateReviewDetail\.evidence/);
  assert.match(view, /lateReviewDetail\.events/);
  assert.match(client, /\/admin\/v1\/late-payment-reviews/);
  assert.match(i18n, /晚到支付复核/);
  assert.doesNotMatch(view, /paid_status|markPaid|手工.*已支付/);
});

test('personal plan history shows traceable package and payment identifiers', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  for (const field of ['package_id', 'pinvou_order_id', 'provider_order_reference', 'sku_code', 'credited_at', 'valid_until', 'invoice_status']) {
    assert.match(view, new RegExp(`item\\.${field}`));
  }
  assert.match(view, /points_total_microcredits/);
  assert.match(view, /points_used_microcredits/);
  assert.match(view, /remainingEntitlementMicrocredits/);
});

test('Web payment QR uses a dialog and only user-activated payable orders can reopen it', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.match(view, /setQrData\(isPayablePaymentOrder\(order\)/);
  assert.match(view, /paymentDialogOpen && activePayment && <PaymentQRCodeDialog/);
  assert.match(view, /<PendingPaymentOrders copy=\{copy\} orders=\{pendingOrders\}/);
  assert.match(view, /onActivateRecharge\(orderId\)/);
  assert.match(view, /const history = await client\.rechargeOrders\(\)/);
  assert.match(view, /setInterval\(\(\) => void refresh\(\), 5000\)/);
  assert.doesNotMatch(view, /className="ap-payment"/);
  assert.doesNotMatch(view, /client\.recentRechargeOrder\(request\.signal\)/);
  assert.match(popover, /isPayablePaymentOrder\(order\) \? order\?\.qr_data/);
});

test('organization purchases open the shared payment dialog and refresh organization entitlements after payment', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  const organizationEntitlements = view.slice(view.indexOf('function OrganizationEntitlements'), view.indexOf('function EntitlementSummary'));
  assert.match(view, /<PortalMyOrganization[\s\S]*qrData=\{qrData\}/);
  assert.match(organizationEntitlements, /if \(await onPurchase\(productId, channel\)\) setPaymentDialogOpen\(true\)/);
  assert.match(organizationEntitlements, /paymentDialogOpen && activePayment && <PaymentQRCodeDialog/);
  assert.match(view, /client\.organizationBalance\(activeOrganizationId\)/);
  assert.match(view, /client\.organizationEntitlementPackages\(activeOrganizationId\)/);
  assert.match(view, /client\.organizationRechargeOrder\(activeOrganizationId, order\.order_id\)/);
  assert.match(backend, /\["v1", "organizations", organization_id, "recharge", "orders", order_id\]/);
  assert.match(backend, /safe_id\(organization_id\) && safe_id\(order_id\)/);
});

test('personal center clears a transient background synchronization error after account bootstrap recovers', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(view, /const \[synchronizationError, setSynchronizationError\] = useState\(''\)/);
  assert.match(view, /const account = await client\.bootstrap\(\);[\s\S]*?setSynchronizationError\(''\);[\s\S]*?authorizationVersionChanged/);
  assert.match(view, /catch \(reason\) \{[\s\S]*?setSynchronizationError\(localizedError\(copy, reason\)\)/);
  assert.match(view, /const visibleError = error \|\| synchronizationError/);
});

test('billing operators can search repair candidates without provider-specific UI branches', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  assert.match(view, /orderFilter/);
  assert.match(view, /mobile_masked/);
  assert.match(view, /primary_provider_transaction_id/);
  assert.match(client, /query\.set\('q', filters\.q\)/);
  assert.match(client, /query\.set\('status', filters\.status\)/);
  assert.doesNotMatch(view, /h3c|新华三/i);
});

test('billing order readers tolerate provider fields that are null before the first successful sync', async () => {
  const payments = await readFile(new URL('../../account-platform/internal/billing/payments.go', import.meta.url), 'utf8');
  const refunds = await readFile(new URL('../../account-platform/internal/billing/refunds.go', import.meta.url), 'utf8');
  const reconciliation = await readFile(new URL('../../account-platform/internal/billing/reconciliation.go', import.meta.url), 'utf8');
  assert.match(payments, /COALESCE\(payment_url,''\),COALESCE\(qr_data,''\)/);
  assert.equal((refunds.match(/COALESCE\(payment_url,''\),COALESCE\(qr_data,''\)/g) || []).length, 2);
  assert.match(reconciliation, /COALESCE\(payment_url,''\),primary_provider_transaction_id/);
});

test('desktop account popover does not own platform model selection', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.doesNotMatch(popover, /client\.modelCatalog|client\.selectCatalogModel/);
  assert.doesNotMatch(popover, /copy\.platformModel|copy\.selectPlatformModel/);
  assert.match(popover, /client\.balance/);
  assert.match(popover, /client\.openPortal/);
  for (const language of ['zh', 'en', 'ja']) {
    for (const key of ['platformModel', 'selectPlatformModel', 'modelSelectionSaved', 'modelDeprecationWarning']) {
      assert.equal(accountPlatformCopy[language][key], undefined, `${language}.${key} is retired from the account popover copy`);
    }
  }
});

test('settings owns the platform model default for new conversations', async () => {
  const settings = await readFile(new URL('../src/features/settings/SettingsView.jsx', import.meta.url), 'utf8');
  const main = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/i18n.js', import.meta.url), 'utf8');
  assert.match(main, /defaultModelRef=\{bs && bs\.defaultModelRef\}/);
  assert.match(main, /accountPlatformClient=\{accountPlatformClient\}/);
  assert.match(settings, /accountPlatformClient\.modelCatalog/);
  assert.match(settings, /accountPlatformClient\.selectCatalogModel/);
  assert.match(settings, /defaultModelRef\?\.source === ACCOUNT_PLATFORM_SOURCE/);
  assert.match(settings, /filter\(model => !isBuiltInAccountPlatformModel\(model\)\)/);
  assert.match(settings, /data-testid="settings-platform-models"/);
  assert.equal((i18n.match(/platformModelsDesc:/g) || []).length, 3);
  assert.equal((i18n.match(/appliesToNewSessions:/g) || []).length, 3);
});

test('chat model selection freezes a typed platform reference without changing the global default', async () => {
  const settings = await readFile(new URL('../src/features/settings/SettingsView.jsx', import.meta.url), 'utf8');
  const sessions = await readFile(new URL('../src/platform/tauri/bridge/sessions.js', import.meta.url), 'utf8');
  const bridgeSettings = await readFile(new URL('../src/platform/tauri/bridge/settings.js', import.meta.url), 'utf8');
  const command = await readFile(new URL('../src-tauri/src/app/commands/account_platform.rs', import.meta.url), 'utf8');
  assert.match(settings, /accountPlatformClient\.modelCatalog/);
  assert.match(settings, /source: ACCOUNT_PLATFORM_SOURCE, model_id: item\.model_id/);
  assert.match(settings, /bridge\.models\.switchModelRef\(activeSessionId, modelRef\)/);
  assert.match(settings, /filter\(model => !isBuiltInAccountPlatformModel\(model\)\)/);
  assert.match(bridgeSettings, /state\.pendingDraftModelRef = modelRef/);
  assert.doesNotMatch(bridgeSettings, /switchModelRef[\s\S]{0,800}setActiveModel\(/);
  assert.match(sessions, /var pendingModelRef = state\.pendingDraftModelRef/);
  assert.match(sessions, /account_platform_select_session_model/);
  assert.match(command, /pub async fn account_platform_select_session_model/);
  assert.match(command, /switch_session_model_ref/);
  assert.match(command, /"session:model_changed"/);
});
