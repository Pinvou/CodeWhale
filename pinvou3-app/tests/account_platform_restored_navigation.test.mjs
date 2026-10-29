import assert from 'node:assert/strict';
import { test } from 'node:test';
import { execFileSync } from 'node:child_process';
import { availablePortalSection, initialPortalSection, personalPortalSections, portalLoginLocation } from '../src/features/account-platform/portal-sections.js';
import { adminRouteLocation, resolveAdminRoute, visibleAdminSections } from '../src/features/account-platform/admin-sections.js';
import { formatWholeBalance } from '../src/features/account-platform/balance-view.js';
import { maskedAccountMobile } from '../src/features/account-platform/account-display-label.js';

test('old personal URLs resolve to the original canonical tabs', () => {
  assert.equal(initialPortalSection('?account_closure=confirm'), 'accountSettings');
  for (const value of ['credits', 'usage', 'entitlements']) assert.equal(initialPortalSection(`?section=${value}`), 'creditsUsage');
  for (const value of ['orders', 'invoice']) assert.equal(initialPortalSection(`?section=${value}`), 'ordersInvoices');
  for (const value of ['overview', 'profile', 'closure']) assert.equal(initialPortalSection(`?section=${value}`), 'accountSettings');
  assert.equal(availablePortalSection('?section=myOrganization', ['accountSettings']), 'accountSettings');
});
test('organization self-quota, personal and organization permissions remain distinct', () => {
  const personal = { personal_permissions: ['personal.billing.read'], active_context: { context_type: 'PERSONAL', permissions: ['org.profile.read'] } };
  assert.deepEqual(personalPortalSections(personal), ['creditsUsage', 'ordersInvoices', 'accountSettings']);
  assert.deepEqual(personalPortalSections({ ...personal, active_context: { context_type: 'ORGANIZATION', organization_id: 'org', permissions: ['org.quota.read.self'] } }), ['creditsUsage', 'myOrganization', 'accountSettings']);
});
test('canonical admin gates and permissions do not substitute unrelated pages', () => {
  const me = { platform_permissions: ['platform.tokens.read'], active_context: { context_type: 'PERSONAL' } };
  assert.deepEqual(visibleAdminSections(me), ['overview', 'tokens']);
  assert.deepEqual(visibleAdminSections(me, { admin_model_token_ia: true }), ['overview', 'tokenResources', 'modelOperations']);
  assert.equal(resolveAdminRoute('?section=management&token_account_id=sample', { admin_model_token_ia: true }).section, 'tokenResources');
  assert.equal(resolveAdminRoute('?section=finance&finance_tab=lateReviews').section, 'payments');
  assert.equal(resolveAdminRoute('?section=finance&finance_tab=lateReviews').tab, 'late');
  assert.equal(resolveAdminRoute('?section=reconciliation').tab, 'reconciliations');
  assert.equal(resolveAdminRoute('?section=entitlement-correction').tab, 'correction');
  assert.equal(resolveAdminRoute('?section=closures').operationsTab, 'closures');
  assert.equal(resolveAdminRoute('?section=finance&finance_tab=approvals').section, 'approvals');
  for (const [permission, expected] of [['platform.pricing.read', 'models'], ['platform.tokens.read', 'tokenResources'], ['platform.usage.read', 'modelOperations']]) {
    const flags = { admin_model_token_ia: true }, available = visibleAdminSections({ platform_permissions: [permission] }, flags);
    assert.equal(resolveAdminRoute('?section=tokens', flags, available).section, expected);
  }
  assert.equal(resolveAdminRoute('?section=tokens&model_id=sample', { admin_model_token_ia: true }, ['tokenResources']).section, 'models');
});
test('explicit navigation defeats stale approval view and preserves unrelated deep filters', () => {
  const url = new URL(adminRouteLocation('?approval_view=mine&model_q=test&finance_tab=ledger', 'payments', { finance_tab: 'orders' }), 'https://example.test');
  assert.equal(url.searchParams.has('approval_view'), false);
  assert.equal(url.searchParams.get('model_q'), 'test');
  assert.equal(url.searchParams.get('finance_tab'), 'orders');
  assert.match(portalLoginLocation('/admin.html', '?section=payments'), /return_to=%2Fadmin.html%3Fsection%3Dpayments$/);
  assert.equal(portalLoginLocation('//external.test', ''), '/auth/login?return_to=%2F');
});
test('whole points keep int64 precision and floor negative fractions', () => {
  assert.equal(formatWholeBalance('9223372036854775807', 'en'), '922,337,203,685,477');
  assert.equal(formatWholeBalance('-1', 'en'), '-1');
  assert.equal(formatWholeBalance('10001', 'en'), '1');
  assert.equal(formatWholeBalance(9007199254740992, 'en'), '—');
  assert.equal(maskedAccountMobile('+8613800000000'), '+86138••••0000');
});

test('readonly collection routes preserve exact composite identities and finite filter fields', async () => {
  const { financeReadIdentity, readFinanceListRoute, financeListRouteUpdates } = await import('../src/features/account-platform/admin-finance-read-route.js');
  const id = '11111111-1111-4111-8111-111111111111';
  assert.notEqual(financeReadIdentity('refunds', { operation_id: id, operation_type: 'NORMAL' }), financeReadIdentity('refunds', { operation_id: id, operation_type: 'FORCED' }));
  assert.throws(() => financeReadIdentity('refunds', { operation_id: id, operation_type: 'UNKNOWN' }));
  assert.equal(readFinanceListRoute('refunds', '?refund_limit=9999&refund_id=FORCED:' + id).query.limit, '50');
  assert.deepEqual(financeListRouteUpdates('receivables', { wallet_id: id, limit: '25' }), { receivable_wallet_id: id, receivable_status: null, receivable_source_type: null, receivable_created_from: null, receivable_created_to: null, receivable_limit: '25', receivable_cursor: null, receivable_id: null });
});

test('financial overview wire keeps bare int64 values and only exact drilldowns claim filtering', async () => {
  const { parseFinanceJSON } = await import('../src/features/account-platform/finance-contract.js');
  const { financeOverviewDrilldown } = await import('../src/features/account-platform/admin-finance-overview.js');
  assert.equal(parseFinanceJSON('{"metrics":[{"value":9223372036854775807}]}', { overviewIntegers: true }).metrics[0].value, '9223372036854775807');
  assert.throws(() => parseFinanceJSON('{"metrics":[{"value":1.5}]}', { overviewIntegers: true }));
  assert.equal(financeOverviewDrilldown('accounts', 'at_risk_hold_count'), null);
  assert.equal(financeOverviewDrilldown('payments', 'payments_received_fen', { from: 'a', to: 'b' }).exact, false);
  assert.deepEqual(financeOverviewDrilldown('payments', 'orders_created_count', { from: 'a', to: 'b' }), { tab: 'orders', query: { created_from: 'a', created_to: 'b' }, exact: true });
});

test('date form filters become RFC3339 before routing to collection clients', async () => {
  const { financeListRouteUpdates } = await import('../src/features/account-platform/admin-finance-read-route.js');
  for (const kind of ['orders', 'ledger', 'late', 'anomalies', 'receivables']) {
    const local = '2026-10-01T08:00:12.345';
    const updates = financeListRouteUpdates(kind, { created_from: local });
    assert.ok(Object.values(updates).includes(new Date(local).toISOString()));
    const exact = '2026-10-01T08:00:12.345678+08:00';
    assert.ok(Object.values(financeListRouteUpdates(kind, { created_from: exact })).includes(exact));
    assert.ok(Object.values(financeListRouteUpdates(kind, { created_from: local }, null, { created_from: new Date(local).toISOString().replace('.345Z', '.345678Z') })).includes(new Date(local).toISOString().replace('.345Z', '.345678Z')));
  }
});

test('unchanged repeated DST hour retains its original offset and submillisecond precision', () => {
  const script = `import { financeListRouteUpdates } from ${JSON.stringify(new URL('../src/features/account-platform/admin-finance-read-route.js', import.meta.url).href)};
    const reference = '2026-11-01T01:30:00.123456-05:00';
    const result = financeListRouteUpdates('orders', { created_from: '2026-11-01T01:30:00.123' }, null, { created_from: reference });
    process.stdout.write(result.order_created_from);`;
  assert.equal(execFileSync(process.execPath, ['--input-type=module', '-e', script], { env: { ...process.env, TZ: 'America/New_York' }, encoding: 'utf8' }), '2026-11-01T01:30:00.123456-05:00');
});
