import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, readdir } from 'node:fs/promises';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from '../src/features/account-platform/context-guard.js';
import { formatPoints, formatUsagePoints, formatWholePoints, MICROCREDITS_PER_POINT, microcreditsToPoints, parsePointsInputToMicrocredits } from '../src/features/account-platform/credit-units.js';
import { activeEntitlementPackages, entitlementPackageDisplayName, entitlementProgress, remainingEntitlementMicrocredits } from '../src/features/account-platform/entitlement-view.js';
import { compactOrderID, effectivePaymentChannel, hasSelectablePaymentChannels, isPayablePaymentOrder, orderDurationDays, orderHistoryTimestamp, orderSummaryStatus, payablePaymentOrders, paymentOrderHistory, paymentOrderRemainingSeconds, paymentScanPrompt } from '../src/features/account-platform/order-view.js';
import { availablePortalSection, initialPortalSection, portalLoginLocation, portalNavigationSections, portalSectionLocation } from '../src/features/account-platform/portal-sections.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { buildUnitTree } from '../src/features/account-platform/admin-organization-tree.js';
import { adjustmentApprovalAction, buildPaymentControlApprovalDraft, createFinanceTabRequestTracker, financeTabShouldLoad, paymentControlApprovalAction, readFinanceTabQuery, resetFinanceSurfaceLoading, submitPaymentControlApproval, updateFinanceTabState, updatePaymentCapabilityState } from '../src/features/account-platform/admin-finance.js';
import { adminFinanceDetailUpdates, adminFinanceQueryUpdates, adminNavigationGroups, adminSectionURL, clearAdminFinanceDetailUpdates, clearAdminFinanceQueryUpdates, isAdminFinancePaginatedTab, normalizeAdminFinanceDetailQuery, normalizeAdminFinanceQuery, readAdminFinanceDetail, readAdminFinanceQuery, readAdminFinanceTab, readAdminOrganizationDirectory, readAdminOrganizationRoute, readAdminSection, resolveAdminFinanceTab, writeAdminSection } from '../src/features/account-platform/admin-navigation.js';
import { createFinanceDetailRecovery, financeCursorContext, financeDetailRouteIsCurrent, isFinanceSnapshotExpired, normalizeFinanceCollection, previousFinanceCursor, rememberFinanceCursor, resetExpiredFinanceQuery, runFinanceDetailRequest } from '../src/features/account-platform/admin-finance-pagination.js';
import { adminSectionReadOnly, adminVisibleSections, canAccessOrganizationPortal } from '../src/features/account-platform/authorization-ui.js';
import { createAccountSynchronizationQueue, resolveAccountSynchronization } from '../src/features/account-platform/account-synchronization.js';
import { summarizeUsageRecords } from '../src/features/account-platform/usage-summary.js';
import { accountPlatformErrorCodes, localizedAccountPlatformError } from '../src/features/account-platform/error-localization.js';
import { accountDisplayLabel } from '../src/features/account-platform/account-display-label.js';
import { profileCaptchaSubmissionOffset } from '../src/features/account-platform/profile-captcha-state.js';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';

test('account platform errors prefer wrapped domain codes over generic bridge failures', () => {
  const copy = accountPlatformCopy.zh;
  const wrapped = {
    code: 'REQUEST_FAILED',
    message: 'Error: oidc_credentials_invalid: account or password is incorrect',
  };
  assert.deepEqual(accountPlatformErrorCodes(wrapped), [
    'REQUEST_FAILED',
    'Error',
    'oidc_credentials_invalid',
  ]);
  assert.equal(localizedAccountPlatformError(copy, wrapped), copy.errors.oidc_credentials_invalid);
  assert.equal(
    localizedAccountPlatformError(copy, { code: 'MOBILE_INVALID', message: 'CAPTCHA_INVALID: conflicting wrapper message' }, {
      MOBILE_INVALID: copy.mobileInvalid,
      CAPTCHA_INVALID: copy.captchaInvalid,
    }),
    copy.mobileInvalid,
  );
  assert.equal(
    localizedAccountPlatformError(copy, { code: 'REQUEST_FAILED', message: 'mobile_auth_failed: SMS_CODE_INVALID' }, {
      SMS_CODE_INVALID: copy.mobileAuthFailed,
    }),
    copy.mobileAuthFailed,
  );
  assert.equal(
    localizedAccountPlatformError(copy, {
      code: 'FUTURE_AUTH_ERROR',
      message: 'oidc_credentials_invalid: previous nested cause',
    }),
    copy.requestFailed,
  );
  assert.equal(localizedAccountPlatformError(copy, new Error('unclassified transport detail')), copy.requestFailed);

  const startedAt = performance.now();
  assert.deepEqual(accountPlatformErrorCodes('a'.repeat(20_000)), []);
  assert.ok(performance.now() - startedAt < 500, 'long unclassified errors must be parsed in bounded time');
});

test('personal account summaries prefer the current username over a stale display name', async () => {
  assert.equal(accountDisplayLabel({ username: 'renamed-user', display_name: 'old-user' }, 'User'), 'renamed-user');
  assert.equal(accountDisplayLabel({ username: '  ', display_name: 'Display Name' }, 'User'), 'Display Name');
  assert.equal(accountDisplayLabel({}, 'User'), 'User');

  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  assert.match(view, /const userLabel = accountDisplayLabel\(me, copy\.user\)/);
  assert.match(view, /copy\.welcome\(accountDisplayLabel\(me, copy\.user\)\)/);
  assert.match(popover, /const displayName = accountDisplayLabel\(me, copy\.user\)/);
});

test('benefit packages use the current catalog name for the active language and preserve the purchase snapshot as fallback', () => {
  const item = {
    plan_id: 'PLAN_STANDARD',
    plan_name: 'Purchase-time snapshot',
    current_plan_name_zh_cn: '标准版（新）',
    current_plan_name_en: 'Standard (New)',
    current_plan_name_ja: 'スタンダード（新）',
  };
  assert.equal(entitlementPackageDisplayName(item, 'zh'), '标准版（新）');
  assert.equal(entitlementPackageDisplayName(item, 'en-US'), 'Standard (New)');
  assert.equal(entitlementPackageDisplayName(item, 'ja-JP'), 'スタンダード（新）');
  assert.equal(entitlementPackageDisplayName({ plan_name: 'Legacy snapshot', plan_id: 'LEGACY' }, 'en'), 'Legacy snapshot');
  assert.equal(entitlementPackageDisplayName({ current_plan_name_en: ' ', plan_name: 'Legacy snapshot', plan_id: 'LEGACY' }, 'en'), 'Legacy snapshot');
  assert.equal(entitlementPackageDisplayName({ plan_id: 'LEGACY' }, 'en'), 'LEGACY');
});

test('Web account synchronization updates profile fields without reloading unchanged context resources', () => {
  const activeContext = {
    context_version: 7,
    personal_authorization_version: 11,
    platform_authorization_version: 3,
    organization_authorization_version: 0,
    organization_id: '',
    permissions: ['personal.profile.read'],
  };
  const currentAccount = { user_id: 'user-1', username: 'old-name', active_context: activeContext };
  const renamedAccount = { user_id: 'user-1', username: 'new-name', active_context: { ...activeContext } };

  const synchronization = resolveAccountSynchronization(currentAccount, currentAccount, renamedAccount);

  assert.equal(synchronization.applyAccount, true);
  assert.equal(synchronization.reloadContext, false);
  assert.equal(synchronization.account.username, 'new-name');
});

test('Web account synchronization does not replace a newer active context with an in-flight bootstrap response', () => {
  const requestAccount = {
    user_id: 'user-1',
    username: 'old-name',
    active_context: { context_version: 7, organization_id: '' },
  };
  const currentAccount = {
    ...requestAccount,
    active_context: { context_version: 8, organization_id: 'organization-2' },
  };
  const staleBootstrapAccount = {
    ...requestAccount,
    username: 'new-name',
    active_context: { ...requestAccount.active_context },
  };

  const synchronization = resolveAccountSynchronization(requestAccount, currentAccount, staleBootstrapAccount);

  assert.equal(synchronization.applyAccount, false);
  assert.equal(synchronization.reloadContext, false);
  assert.equal(synchronization.account.active_context.organization_id, 'organization-2');

  const mutationSynchronization = resolveAccountSynchronization(requestAccount, requestAccount, staleBootstrapAccount, true);
  assert.equal(mutationSynchronization.applyAccount, false);
  assert.equal(mutationSynchronization.account.active_context.organization_id, '');
});

test('Web account synchronization reloads context resources only when authorization context changes', () => {
  const currentAccount = {
    user_id: 'user-1',
    username: 'old-name',
    active_context: { context_version: 7, personal_authorization_version: 11, organization_id: '' },
  };
  const nextAccount = {
    ...currentAccount,
    username: 'new-name',
    active_context: { ...currentAccount.active_context, personal_authorization_version: 12 },
  };

  const synchronization = resolveAccountSynchronization(currentAccount, currentAccount, nextAccount);

  assert.equal(synchronization.applyAccount, true);
  assert.equal(synchronization.reloadContext, true);
  assert.equal(synchronization.account.username, 'new-name');
});

test('Web account synchronization converges after a context switch and coalesces event storms', async () => {
  let releaseFirstBootstrap;
  const firstBootstrap = new Promise((resolve) => {
    releaseFirstBootstrap = resolve;
  });
  const personalContext = { context_version: 7, personal_authorization_version: 11, organization_id: '' };
  const organizationContext = { context_version: 8, personal_authorization_version: 11, organization_authorization_version: 4, organization_id: 'organization-2' };
  let currentAccount = { user_id: 'user-1', username: 'old-name', active_context: personalContext };
  let bootstrapCalls = 0;
  let accountUpdates = 0;
  const queue = createAccountSynchronizationQueue();
  const bootstrap = async () => {
    bootstrapCalls += 1;
    if (bootstrapCalls === 1) return firstBootstrap;
    return { user_id: 'user-1', username: 'new-name', active_context: { ...organizationContext } };
  };
  const synchronize = async () => {
    const requestAccount = currentAccount;
    const account = await bootstrap();
    const synchronization = resolveAccountSynchronization(
      requestAccount,
      currentAccount,
      account,
      queue.isContextMutationInProgress(),
    );
    if (!synchronization.applyAccount) {
      queue.retry();
      return;
    }
    currentAccount = synchronization.account;
    accountUpdates += 1;
  };

  queue.request(synchronize);
  await Promise.resolve();
  assert.equal(bootstrapCalls, 1);

  queue.beginContextMutation();
  queue.request(synchronize);
  queue.request(synchronize);
  queue.request(synchronize);
  currentAccount = { ...currentAccount, active_context: { ...organizationContext } };
  releaseFirstBootstrap({ user_id: 'user-1', username: 'new-name', active_context: { ...personalContext } });
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(bootstrapCalls, 1, 'events remain coalesced while the context mutation is active');

  queue.endContextMutation();
  await queue.whenIdle();

  assert.equal(bootstrapCalls, 2, 'the stale response and repeated events produce one convergence bootstrap');
  assert.equal(accountUpdates, 1);
  assert.equal(currentAccount.username, 'new-name');
  assert.equal(currentAccount.active_context.organization_id, 'organization-2');
  assert.equal(currentAccount.active_context.context_version, 8);
  queue.dispose();
});

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
  assert.equal(readAdminFinanceTab('billing', { search: '?finance_tab=holds' }), 'holds');
  assert.equal(readAdminFinanceTab('payments', { search: '?finance_tab=reconciliations' }), 'reconciliations');
  assert.equal(readAdminFinanceTab('payments', { search: '?finance_tab=controls' }), 'controls');
  assert.equal(readAdminFinanceTab('products', { search: '?finance_tab=prices' }), 'prices');
  assert.equal(readAdminFinanceTab('billing', { search: '?finance_tab=prices' }), 'wallets');
  assert.equal(readAdminFinanceTab('payments', { search: '' }), 'orders');
  assert.deepEqual(readAdminFinanceQuery('payments', 'orders', { search: '?payment_q=order%2F42&payment_status=PAID&payment_limit=25&payment_cursor=next%2F1' }), { q: 'order/42', status: 'PAID', provider_id: '', channel: '', created_from: '', created_to: '', limit: '25', cursor: 'next/1' });
  assert.deepEqual(readAdminFinanceQuery('billing', 'receivables', { search: '?receivable_wallet_id=wallet-1&receivable_status=OPEN&receivable_limit=999' }), { wallet_id: 'wallet-1', status: 'OPEN', source_type: '', created_from: '', created_to: '', limit: '50', cursor: '' });
  assert.deepEqual(readAdminFinanceQuery('payments', 'lateReviews', { search: '?late_provider_id=h3c&late_overdue=true&late_limit=200' }), { q: '', status: '', severity: '', provider_id: 'h3c', owner_id: '', created_from: '', created_to: '', entered_from: '', entered_to: '', next_review_before: '', overdue: true, limit: '200', cursor: '' });
  assert.deepEqual(normalizeAdminFinanceQuery('anomalies', { status: '', owner_id: ' actor ', limit: '50' }), { q: '', anomaly_type: '', status: '', severity: '', owner_id: 'actor', provider_id: '', created_from: '', created_to: '', limit: '50', cursor: '' });
  const orderUpdates = adminFinanceQueryUpdates('orders', { q: ' order ', status: '', limit: '50' });
  assert.equal(orderUpdates.payment_q, 'order');
  assert.equal(orderUpdates.payment_status, null);
  assert.equal(orderUpdates.payment_limit, '50');
  assert.equal(orderUpdates.payment_cursor, null);
  assert.equal(isAdminFinancePaginatedTab('reconciliations'), true);
  assert.equal(isAdminFinancePaginatedTab('controls'), false);
  assert.equal(clearAdminFinanceQueryUpdates().late_owner_id, null);
  assert.deepEqual(readAdminFinanceDetail('billing', 'wallets', { search: '?finance_detail=ledger&finance_detail_id=wallet-1&ledger_entry_type=USAGE&ledger_cursor=ledger-next' }), {
    type: 'ledger', id: 'wallet-1', query: { entry_type: 'USAGE', business_reference: '', created_from: '', created_to: '', limit: '50', cursor: 'ledger-next' },
  });
  assert.deepEqual(readAdminFinanceDetail('payments', 'reconciliations', { search: '?finance_detail=reconciliationCases&finance_detail_id=run-1&case_status=OPEN&case_type=PAYMENT_LEDGER_MISSING&case_limit=25' }), {
    type: 'reconciliationCases', id: 'run-1', query: { status: 'OPEN', severity: '', owner_id: '', case_type: 'PAYMENT_LEDGER_MISSING', created_from: '', created_to: '', limit: '25', cursor: '' },
  });
  assert.deepEqual(readAdminFinanceDetail('payments', 'orders', { search: '?finance_detail=reconciliationCases&finance_detail_id=run-1' }), { type: '', id: '', query: {} });
  assert.equal(adminFinanceDetailUpdates('ledger', 'wallet-1', { entry_type: 'USAGE' }).ledger_entry_type, 'USAGE');
  assert.equal(clearAdminFinanceDetailUpdates().case_cursor, null);
  assert.equal(normalizeAdminFinanceDetailQuery('ledger', { entry_type: 'DEBIT' }).entry_type, '');
  assert.deepEqual(resolveAdminFinanceTab('holds', 'wallets', ['wallets', 'receivables']), { tab: 'wallets', replace: true });
  assert.deepEqual(resolveAdminFinanceTab('receivables', 'wallets', ['wallets', 'receivables']), { tab: 'receivables', replace: false });
  assert.deepEqual(resolveAdminFinanceTab('prices', 'catalog', ['catalog']), { tab: 'catalog', replace: true });
  assert.deepEqual(resolveAdminFinanceTab('orders', 'orders', []), { tab: '', replace: false });
  assert.deepEqual(readAdminOrganizationRoute({ search: '?organization_id=org-1&organization_tab=members' }), { organizationID: 'org-1', tab: 'members' });
  assert.deepEqual(readAdminOrganizationRoute({ search: '?organization_id=org-1&organization_tab=unknown' }), { organizationID: 'org-1', tab: 'overview' });
  assert.deepEqual(readAdminOrganizationDirectory({ search: '?organization_q=AI&organization_status=DISABLED&organization_cursor=next%2Fpage' }), { q: 'AI', status: 'DISABLED', cursor: 'next/page' });
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

test('admin finance stable cursor state is context-bound and safely degrades', () => {
  const page = normalizeFinanceCollection({ items: [{ id: 1 }], next_cursor: 'next', snapshot_as_of: '2026-09-02T08:00:00Z' }, { status: 'OPEN', cursor: '' });
  assert.deepEqual(page, { items: [{ id: 1 }], page: { cursor: '', nextCursor: 'next', snapshotAsOf: '2026-09-02T08:00:00Z', legacy: false, expired: false } });
  assert.equal(normalizeFinanceCollection({ items: [] }, {}).page.legacy, true);
  const cursors = new Map();
  rememberFinanceCursor(cursors, 'orders', { status: 'PAID', limit: '50', cursor: '' }, 'opaque-next', 'payments');
  assert.equal(previousFinanceCursor(cursors, 'orders', { status: 'PAID', limit: '50', cursor: 'opaque-next' }, 'payments'), '');
  assert.equal(previousFinanceCursor(cursors, 'orders', { status: 'FAILED', limit: '50', cursor: 'opaque-next' }, 'payments'), null);
  assert.notEqual(financeCursorContext('orders', { status: 'PAID' }, 'payments'), financeCursorContext('orders', { status: 'PAID' }, 'billing'));
  assert.equal(isFinanceSnapshotExpired({ status: 410 }), true);
  assert.equal(isFinanceSnapshotExpired({ code: 'PAGINATION_CURSOR_EXPIRED' }), true);
  assert.deepEqual(resetExpiredFinanceQuery({ status: 'OPEN', cursor: 'old' }), { status: 'OPEN', cursor: '' });
  const detailRequests = createFinanceTabRequestTracker();
  const oldLedgerRequest = detailRequests.begin('billing:ledger-detail');
  const currentLedgerRequest = detailRequests.begin('billing:ledger-detail');
  assert.equal(detailRequests.isCurrent('billing:ledger-detail', oldLedgerRequest), false);
  assert.equal(detailRequests.isCurrent('billing:ledger-detail', currentLedgerRequest), true);
});

test('admin finance detail requests reject stale routes, reversed responses, and expired post-write snapshots', async () => {
  const backTracker = createFinanceTabRequestTracker();
  let routeCurrent = true;
  let finishBackRequest;
  const backRequest = runFinanceDetailRequest({
    requestTracker: backTracker,
    requestKey: 'payments:reconciliation-cases-detail',
    load: () => new Promise((resolve) => { finishBackRequest = resolve; }),
    isRouteCurrent: () => routeCurrent,
  });
  routeCurrent = false;
  finishBackRequest({ items: [{ case_id: 'stale-after-back' }] });
  assert.deepEqual(await backRequest, { status: 'stale' });

  const switchTracker = createFinanceTabRequestTracker();
  let finishRunA;
  let finishRunB;
  const runA = runFinanceDetailRequest({
    requestTracker: switchTracker,
    requestKey: 'payments:reconciliation-cases-detail',
    load: () => new Promise((resolve) => { finishRunA = resolve; }),
  });
  const runB = runFinanceDetailRequest({
    requestTracker: switchTracker,
    requestKey: 'payments:reconciliation-cases-detail',
    load: () => new Promise((resolve) => { finishRunB = resolve; }),
  });
  finishRunB({ items: [{ case_id: 'run-b' }] });
  assert.deepEqual(await runB, { status: 'ok', value: { items: [{ case_id: 'run-b' }] } });
  finishRunA({ items: [{ case_id: 'run-a' }] });
  assert.deepEqual(await runA, { status: 'stale' });

  const expired = await runFinanceDetailRequest({
    requestTracker: createFinanceTabRequestTracker(),
    requestKey: 'payments:reconciliation-cases-detail',
    load: async () => { throw Object.assign(new Error('expired'), { status: 410 }); },
  });
  assert.equal(expired.status, 'expired');
  assert.equal(expired.error.status, 410);

  const attempts = [];
  const restarted = [];
  const recover = createFinanceDetailRecovery({
    query: { status: 'OPEN', cursor: 'expired-cursor' },
    load: async (query) => {
      attempts.push(query);
      if (query.cursor) throw Object.assign(new Error('expired'), { status: 410 });
      return { items: [{ case_id: 'authoritative-first-page' }] };
    },
    onRestart: (query) => restarted.push(query),
  });
  await assert.rejects(recover(), error => error.status === 410);
  assert.deepEqual(await recover(), { items: [{ case_id: 'authoritative-first-page' }] });
  assert.deepEqual(attempts.map(item => item.cursor), ['expired-cursor', '']);
  assert.deepEqual(restarted, [{ status: 'OPEN', cursor: '' }]);

  const route = { type: 'reconciliationCases', id: 'run-1', query: { status: 'OPEN', cursor: '' } };
  const matches = (canRead) => financeDetailRouteIsCurrent({ canRead, current: route, type: 'reconciliationCases', id: 'run-1', query: route.query });
  assert.equal(matches(true), true);
  assert.equal(matches(false), false, 'revoking read permission must invalidate the current detail before another GET or state write');
});

test('admin billing client forwards only existing normalized finance filters', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    calls.push(url);
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('/portal');
    await client.wallets({ owner_type: 'ORGANIZATION', status: 'ACTIVE', q: 'owner', limit: '50', cursor: 'wallet-next' });
    await client.walletLedger('wallet/id', { entry_type: 'USAGE', business_reference: 'order-1', limit: '25', cursor: 'ledger-next' });
    await client.receivables({ wallet_id: 'wallet-id', status: 'OPEN', limit: '25' });
    await client.paymentOrders({ q: 'order 42', status: 'PAID', limit: '50' });
    await client.refunds({ q: 'refund-1', status: 'FAILED', provider_id: 'h3c', limit: '50', cursor: 'refund-next' });
    await client.adjustments({ wallet_id: 'wallet-id', status: 'POSTED', requested_by: 'operator', limit: '100', cursor: 'adjustment-next' });
    await client.holds({ wallet_id: 'wallet-id', status: 'REVIEW', model_id: 'model-1', limit: '200', cursor: 'hold-next' });
    await client.latePaymentReviews({ status: 'PROVIDER_BLIND', provider_id: 'h3c', owner_id: 'owner-id', entered_from: '2026-09-01T08:00', entered_to: '2026-09-02T08:00', next_review_before: '2026-09-03T08:00', overdue: true, limit: '200' });
    await client.paymentAnomalies({ anomaly_type: 'CHARGEBACK', status: 'OPEN', severity: 'P0', owner_id: 'owner-id', limit: '25' });
    await client.reconciliationRuns({ provider_id: 'h3c', channel: 'ALIPAY', status: 'FAILED', business_date_from: '2026-09-01', business_date_to: '2026-09-02', limit: '50', cursor: 'run-next' });
    await client.reconciliationCases('run/id', { status: 'OPEN', severity: 'P1', owner_id: 'owner-id', case_type: 'INTERNAL_PAID_PROVIDER_MISSING', limit: '25', cursor: 'case-next' });
    const enteredFrom = encodeURIComponent(new Date('2026-09-01T08:00').toISOString());
    const enteredTo = encodeURIComponent(new Date('2026-09-02T08:00').toISOString());
    const nextReviewBefore = encodeURIComponent(new Date('2026-09-03T08:00').toISOString());
    assert.deepEqual(calls, [
      '/portal/api/admin/billing/admin/v1/wallets?limit=50&cursor=wallet-next&owner_type=ORGANIZATION&status=ACTIVE&q=owner',
      '/portal/api/admin/billing/admin/v1/wallets/wallet%2Fid/ledger?limit=25&cursor=ledger-next&entry_type=USAGE&business_reference=order-1',
      '/portal/api/admin/billing/admin/v1/receivables?limit=25&wallet_id=wallet-id&status=OPEN',
      '/portal/api/admin/billing/admin/v1/payment-orders?limit=50&q=order+42&status=PAID',
      '/portal/api/admin/billing/admin/v1/refunds?limit=50&cursor=refund-next&q=refund-1&status=FAILED&provider_id=h3c',
      '/portal/api/admin/billing/admin/v1/adjustments?limit=100&cursor=adjustment-next&wallet_id=wallet-id&status=POSTED&requested_by=operator',
      '/portal/api/admin/billing/admin/v1/usage-holds?limit=200&cursor=hold-next&wallet_id=wallet-id&status=REVIEW&model_id=model-1',
      `/portal/api/admin/billing/admin/v1/late-payment-reviews?limit=200&status=PROVIDER_BLIND&provider_id=h3c&owner_id=owner-id&entered_from=${enteredFrom}&entered_to=${enteredTo}&next_review_before=${nextReviewBefore}&overdue=true`,
      '/portal/api/admin/billing/admin/v1/payment-anomalies?limit=25&anomaly_type=CHARGEBACK&status=OPEN&severity=P0&owner_id=owner-id',
      '/portal/api/admin/billing/admin/v1/reconciliation-runs?limit=50&cursor=run-next&provider_id=h3c&channel=ALIPAY&status=FAILED&business_date_from=2026-09-01&business_date_to=2026-09-02',
      '/portal/api/admin/billing/admin/v1/reconciliation-runs/run%2Fid/cases?limit=25&cursor=case-next&status=OPEN&severity=P1&owner_id=owner-id&case_type=INTERNAL_PAID_PROVIDER_MISSING',
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
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
  assert.match(view, /'product-admin-root': section === 'products' && financeTab === 'catalog'/);
  assert.match(view, /section === 'billing'.*surface="billing"/s);
  assert.match(view, /section === 'payments'.*surface="payments"/s);
  assert.match(view, /section === 'settings'.*SettingsWorkspace/s);
  assert.match(view, /<PricingLifecycleWorkspace/);
  assert.match(authorization, /billing: \['platform\.billing\.wallets\.read', 'platform\.usage\.read'\]/);
  assert.match(authorization, /payments: \['platform\.billing\.wallets\.read', 'platform\.usage\.read'\]/);
  assert.match(authorization, /products: \['platform\.products\.read', 'platform\.pricing\.read'\]/);
  assert.match(authorization, /approvals: \['platform\.billing\.wallets\.read', 'platform\.billing\.repairs\.request', 'platform\.billing\.repairs\.approve', 'platform\.billing\.payment-controls\.resume\.request', 'platform\.billing\.payment-controls\.resume\.approve',/);
  assert.match(styles, /\.admin-portal-body > #approval-requests-root[\s\S]*display:none/);
  assert.match(main, /pinvou-admin-language-change/);
  assert.doesNotMatch(approvals, /approval-control-snapshot|createControlResume|resume_draft/);
  assert.match(view, /function PaymentControlsPanel/);
  assert.match(approvals, /pinvou-admin-language-change/);
  assert.match(products, /pinvou-admin-language-change/);
});

test('manual adjustments create and recover approval requests only from accounts and billing', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const client = await readFile(new URL('../src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  const finance = await readFile(new URL('../src/features/account-platform/admin-finance.js', import.meta.url), 'utf8');
  const authorization = await readFile(new URL('../src/features/account-platform/authorization-ui.js', import.meta.url), 'utf8');
  const approvals = await readFile(new URL('../../account-platform/internal/portal/static/assets/approval-requests.js', import.meta.url), 'utf8');

  assert.match(client, /createAdjustment\(input\)[\s\S]*?\/admin\/v1\/adjustments/);
  assert.match(client, /createApprovalDraft\(input, signal\)[\s\S]*?\/admin\/v1\/approval-requests/);
  assert.match(client, /submitApprovalRequest\(requestID, expectedVersion, signal\)/);
  assert.doesNotMatch(client, /decideAdjustment\(/);
  assert.match(view, /await client\.createAdjustment[\s\S]*?await ensureAdjustmentApproval/);
  assert.match(view, /command_type: 'WALLET_ADJUSTMENT'/);
  assert.match(finance, /idempotency_key: `approval:\$\{requestKey\}`/);
  assert.match(view, /created \? copy\.billing\.adjustmentApprovalPending/);
  assert.match(view, /recoverAdjustmentApproval/);
  assert.match(view, /approval_command_type: 'WALLET_ADJUSTMENT', approval_target: item\.adjustment_id/);
  assert.doesNotMatch(view, /client\.decideAdjustment/);
  assert.match(authorization, /approvals: \['platform\.billing\.wallets\.read', 'platform\.billing\.repairs\.request', 'platform\.billing\.repairs\.approve', 'platform\.billing\.payment-controls\.resume\.request', 'platform\.billing\.payment-controls\.resume\.approve',/);
  const pending = { status: 'PENDING_APPROVAL', request_idempotency_key: 'adjustment-key' };
  assert.equal(adjustmentApprovalAction(pending, { status: 'DRAFT' }, { canRequest: true }), 'resume');
  assert.equal(adjustmentApprovalAction(pending, { status: 'SUBMITTED' }, { canRequest: true }), 'view');
  assert.equal(adjustmentApprovalAction(pending, null, { canRequest: true }), 'resume');
  assert.equal(adjustmentApprovalAction(pending, { status: 'DRAFT' }, { canApprove: true }), 'view');
  assert.equal(adjustmentApprovalAction(pending, { status: 'DRAFT' }, {}), 'none');
  assert.match(view, /pinvou-admin-route-change/);
  assert.match(approvals, /syncRouteFilters/);
  assert.match(approvals, /addEventListener\('pinvou-admin-route-change',routeChanged\)/);
  assert.match(approvals, /addEventListener\('popstate',routeChanged\)/);
  assert.doesNotMatch(approvals, /class="approval-create"/);
  assert.doesNotMatch(approvals, /function createAdjustment/);
});

test('payment controls preserve server recovery drafts and keep provider and product requests separate', async () => {
  const control = {
    provider_id: 'provider-a',
    provider_control_version: 7,
    resume_draft: { command_type: 'PAYMENT_PROVIDER_RESUME', target_resource_type: 'PAYMENT_PROVIDER_CONTROL', target_resource_id: 'provider-a', normalized_command: { provider_control_version: 7 } },
  };
  const product = {
    product_id: 'product-a',
    version: 11,
    resume_draft: { command_type: 'PAYMENT_PRODUCT_RESUME', target_resource_type: 'PAYMENT_PRODUCT_CONTROL', target_resource_id: 'product-a', normalized_command: { product_control_version: 11 } },
  };
  const providerDraft = buildPaymentControlApprovalDraft(control, null, 'provider recovery reason', 'provider-key');
  const productDraft = buildPaymentControlApprovalDraft(control, product, 'product recovery reason', 'product-key');
  assert.equal(providerDraft.command_type, 'PAYMENT_PROVIDER_RESUME');
  assert.equal(providerDraft.normalized_command.provider_control_version, 7);
  assert.deepEqual(providerDraft.expected_impact, { provider_id: 'provider-a', product_id: null, new_orders_enabled: true });
  assert.equal(providerDraft.financial_snapshot.product_control_version, null);
  assert.equal(productDraft.command_type, 'PAYMENT_PRODUCT_RESUME');
  assert.equal(productDraft.normalized_command.product_control_version, 11);
  assert.deepEqual(productDraft.expected_impact, { provider_id: 'provider-a', product_id: 'product-a', new_orders_enabled: true });
  assert.equal(productDraft.financial_snapshot.product_control_version, 11);
  assert.equal(buildPaymentControlApprovalDraft(control, product, 'short', 'key'), null);
  assert.equal(buildPaymentControlApprovalDraft(control, { ...product, resume_draft: null }, 'valid recovery reason', 'key'), null);

  const resumableDraft = { approval_request_id: 'approval-provider', status: 'DRAFT', version: 3, allowed_actions: ['SUBMIT'] };
  assert.equal(paymentControlApprovalAction(null, { canRequest: true }), 'create');
  assert.equal(paymentControlApprovalAction(null, { canRequest: false }), 'none');
  assert.equal(paymentControlApprovalAction(resumableDraft, { canRequest: true }), 'submit');
  assert.equal(paymentControlApprovalAction({ ...resumableDraft, allowed_actions: [] }, { canRequest: true }), 'submit');
  assert.equal(paymentControlApprovalAction({ ...resumableDraft, status: 'SUBMITTED' }, { canRequest: true }), 'view');
  for (const status of ['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED', 'EXECUTED']) {
    assert.equal(paymentControlApprovalAction({ ...resumableDraft, status }, { canRequest: true }), 'create');
    assert.equal(paymentControlApprovalAction({ ...resumableDraft, status }, { canRequest: false }), 'view');
  }
  assert.equal(paymentControlApprovalAction({ ...resumableDraft, status: 'EXECUTION_FAILED' }, { canRequest: true }), 'view');
  for (const status of ['UNKNOWN', '', undefined]) {
    assert.equal(paymentControlApprovalAction({ ...resumableDraft, status }, { canRequest: true }), 'view');
  }

  const detailFor = (requestID, draftInput, status, version, allowedActions = status === 'DRAFT' ? ['SUBMIT'] : []) => ({
    approval_request_id: requestID,
    command_type: draftInput.command_type,
    target: { resource_type: draftInput.target_resource_type, resource_id: draftInput.target_resource_id },
    status,
    version,
    action_control_version: 1,
    detail_availability: 'AVAILABLE',
    allowed_actions: allowedActions,
  });
  const recoveredCalls = [];
  let recoveredReads = 0;
  const recovered = await submitPaymentControlApproval({
    createGovernedApprovalDraft: async () => 'approval-provider',
    governedApprovalRequest: async () => {
      recoveredReads += 1;
      return detailFor('approval-provider', providerDraft, recoveredReads === 1 ? 'DRAFT' : 'SUBMITTED', recoveredReads === 1 ? 1 : 2);
    },
    submitGovernedApprovalRequest: async (requestID, detail) => recoveredCalls.push(['submit', requestID, detail.version]),
  }, providerDraft);
  assert.equal(recovered.status, 'SUBMITTED');
  assert.equal(recoveredReads, 2);
  assert.deepEqual(recoveredCalls, [['submit', 'approval-provider', 1]]);

  let submitCalls = 0;
  await assert.rejects(() => submitPaymentControlApproval({
    createGovernedApprovalDraft: async () => 'approval-provider',
    governedApprovalRequest: async () => detailFor('approval-provider', providerDraft, 'DRAFT', 1),
    submitGovernedApprovalRequest: async () => { submitCalls += 1; throw new Error('submit response lost'); },
  }, providerDraft), { commandReference: 'approval-provider' });
  assert.equal(submitCalls, 1);

  const createdInputs = [];
  for (const input of [providerDraft, productDraft]) {
    await submitPaymentControlApproval({
      createGovernedApprovalDraft: async (draftInput) => {
        createdInputs.push(draftInput);
        return `approval-${draftInput.command_type}`;
      },
      governedApprovalRequest: async (requestID) => detailFor(requestID, input, 'SUBMITTED', 2),
      submitGovernedApprovalRequest: async () => assert.fail('already submitted details must not be submitted again'),
    }, input);
  }
  assert.strictEqual(createdInputs[0], providerDraft);
  assert.strictEqual(createdInputs[1], productDraft);
  assert.deepEqual(createdInputs.map((item) => item.command_type), ['PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME']);

  const crossRefreshCalls = [];
  const existingProviderApproval = {
    ...resumableDraft,
    command_type: providerDraft.command_type,
    target_resource_type: providerDraft.target_resource_type,
    target_resource_id: providerDraft.target_resource_id,
    action_control_version: 1,
    detail_availability: 'AVAILABLE',
  };
  const crossRefresh = await submitPaymentControlApproval({
    governedApprovalRequest: async (requestID) => detailFor(requestID, providerDraft, crossRefreshCalls.length ? 'SUBMITTED' : 'DRAFT', crossRefreshCalls.length ? 4 : 3),
    submitGovernedApprovalRequest: async (requestID, detail) => {
      crossRefreshCalls.push([requestID, detail.version]);
    },
  }, null, existingProviderApproval);
  assert.equal(crossRefresh.status, 'SUBMITTED');
  assert.deepEqual(crossRefreshCalls, [['approval-provider', 3]]);

  await assert.rejects(() => submitPaymentControlApproval({
    createGovernedApprovalDraft: async () => 'approval-product',
    governedApprovalRequest: async () => detailFor('approval-product', productDraft, 'DRAFT', 1, []),
    submitGovernedApprovalRequest: async () => { throw new Error('must not submit without allowed action'); },
  }, productDraft), { code: 'COMMAND_RESULT_UNKNOWN', commandReference: 'approval-product' });
});

test('finance tab state isolates cached data, errors, and stale requests', () => {
  const current = {
    wallets: [{ wallet_id: 'wallet-a' }],
    orders: [{ order_id: 'order-a' }],
    financeTabStates: {
      billing: { wallets: { loaded: true, error: '' } },
      payments: { orders: { loaded: true, error: '' } },
    },
  };
  const failedHolds = updateFinanceTabState(current, 'billing', 'holds', { attempted: true, loading: false, error: 'failed' });
  assert.deepEqual(failedHolds.wallets, current.wallets);
  assert.deepEqual(failedHolds.orders, current.orders);
  assert.deepEqual(failedHolds.financeTabStates.billing.wallets, { loaded: true, error: '' });
  assert.deepEqual(failedHolds.financeTabStates.payments.orders, { loaded: true, error: '' });
  assert.deepEqual(failedHolds.financeTabStates.billing.holds, { attempted: true, loading: false, error: 'failed' });
  assert.equal(current.financeTabStates.billing.holds, undefined);

  const filteredOrders = updateFinanceTabState(failedHolds, 'payments', 'orders', {
    loaded: true,
    query: { q: 'merchant-42', status: 'PAID' },
    page: { cursor: null },
  });
  assert.deepEqual(readFinanceTabQuery(filteredOrders, 'payments', 'orders', { q: '', status: '' }), { q: 'merchant-42', status: 'PAID' });
  assert.deepEqual(readFinanceTabQuery(filteredOrders, 'payments', 'anomalies', { anomaly_type: '', status: 'OPEN', severity: '' }), { anomaly_type: '', status: 'OPEN', severity: '' });
  const restoredOrderQuery = readFinanceTabQuery(filteredOrders, 'payments', 'orders');
  restoredOrderQuery.q = 'local-edit';
  assert.equal(filteredOrders.financeTabStates.payments.orders.query.q, 'merchant-42');
  const executedQuery = { q: '', status: 'PAID', limit: '100' };
  const unsubmittedDraft = { q: '', status: 'FAILED', limit: '100' };
  assert.equal(financeTabShouldLoad({ attempted: true, loading: false, query: executedQuery }, executedQuery), false);
  assert.equal(financeTabShouldLoad({ attempted: true, loading: false, query: executedQuery, requestedQuery: executedQuery, error: 'failed' }, executedQuery), false);
  assert.equal(financeTabShouldLoad({ attempted: true, loading: false, query: executedQuery, requestedQuery: unsubmittedDraft, error: 'failed' }, executedQuery), true);
  assert.equal(financeTabShouldLoad({ attempted: false, loading: false }, executedQuery), true);
  assert.equal(financeTabShouldLoad({ attempted: false, loading: true }, executedQuery), false);

  const tracker = createFinanceTabRequestTracker();
  const firstOrders = tracker.begin('payments:orders');
  const holds = tracker.begin('billing:holds');
  const secondOrders = tracker.begin('payments:orders');
  assert.equal(tracker.isCurrent('payments:orders', firstOrders), false);
  assert.equal(tracker.isCurrent('payments:orders', secondOrders), true);
  assert.equal(tracker.isCurrent('billing:holds', holds), true);
  tracker.invalidatePrefix('payments:');
  assert.equal(tracker.isCurrent('payments:orders', secondOrders), false);
  assert.equal(tracker.isCurrent('billing:holds', holds), true);
  tracker.invalidateAll();
  assert.equal(tracker.isCurrent('billing:holds', holds), false);

  const interrupted = resetFinanceSurfaceLoading({
    financeTabStates: { payments: { orders: { attempted: true, loading: true, loaded: false }, refunds: { attempted: true, loading: true, loaded: true } } },
    paymentCapabilityState: { attempted: true, loading: true, loaded: false },
  }, 'payments');
  assert.deepEqual(interrupted.financeTabStates.payments.orders, { attempted: false, loading: false, loaded: false });
  assert.deepEqual(interrupted.financeTabStates.payments.refunds, { attempted: true, loading: false, loaded: true });
  assert.deepEqual(interrupted.paymentCapabilityState, { attempted: false, loading: false, loaded: false });
  assert.equal(interrupted.paymentCapabilities, null);

  const afterWalletRefresh = { ...failedHolds, wallets: [{ wallet_id: 'wallet-new' }] };
  const afterOlderAdjustmentResponse = { ...afterWalletRefresh, adjustmentWallets: [{ wallet_id: 'wallet-old' }] };
  assert.deepEqual(afterOlderAdjustmentResponse.wallets, [{ wallet_id: 'wallet-new' }]);
  assert.deepEqual(afterOlderAdjustmentResponse.adjustmentWallets, [{ wallet_id: 'wallet-old' }]);
  const firstCapabilities = tracker.begin('payments:capabilities');
  const secondCapabilities = tracker.begin('payments:capabilities');
  assert.equal(tracker.isCurrent('payments:capabilities', firstCapabilities), false);
  assert.equal(tracker.isCurrent('payments:capabilities', secondCapabilities), true);

  const overview = { total_wallets: 9, payment_capabilities: { refund_supported: true } };
  const overviewState = { billingOverview: overview, overviewAvailability: { finance: 'ready' } };
  const capabilityLoading = updatePaymentCapabilityState(overviewState, { attempted: true, loading: true, loaded: false, error: '' });
  assert.strictEqual(capabilityLoading.billingOverview, overview);
  assert.deepEqual(capabilityLoading.overviewAvailability, { finance: 'ready' });
  assert.equal(capabilityLoading.paymentCapabilities, null);
  const capabilityFailed = updatePaymentCapabilityState(capabilityLoading, { attempted: true, loading: false, loaded: false, error: 'failed' }, null);
  assert.strictEqual(capabilityFailed.billingOverview, overview);
  assert.deepEqual(capabilityFailed.overviewAvailability, { finance: 'ready' });
  assert.equal(capabilityFailed.paymentCapabilities, null);
  const capabilityReady = updatePaymentCapabilityState(capabilityFailed, { attempted: true, loading: false, loaded: true, error: '' }, { refund_supported: false });
  assert.strictEqual(capabilityReady.billingOverview, overview);
  assert.deepEqual(capabilityReady.paymentCapabilities, { refund_supported: false });
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
  assert.equal(parsePointsInputToMicrocredits('1.2345'), 12_345);
  assert.equal(parsePointsInputToMicrocredits('-1.2345'), -12_345);
  assert.equal(parsePointsInputToMicrocredits('0.0001'), 1);
  assert.equal(parsePointsInputToMicrocredits('0'), 0);
  assert.equal(parsePointsInputToMicrocredits('1.23456'), null);
  assert.equal(parsePointsInputToMicrocredits('1e2'), null);
  assert.equal(parsePointsInputToMicrocredits('900719925474.0991'), Number.MAX_SAFE_INTEGER);
  assert.equal(parsePointsInputToMicrocredits('900719925474.0992'), null);
  assert.equal(parsePointsInputToMicrocredits('-900719925474.0991'), Number.MIN_SAFE_INTEGER);
  assert.equal(parsePointsInputToMicrocredits('-900719925474.0992'), null);
  assert.equal(microcreditsToPoints(1_000_000), 100);
  assert.equal(formatPoints(1_000_000, 'en'), '100');
  assert.equal(formatPoints(7_969_994, 'en'), '796.9994');
  assert.equal(formatPoints(1, 'en'), '0.0001');
  assert.equal(formatPoints(0, 'en'), '0');
  assert.equal(formatPoints(null, 'en'), '—');
  assert.equal(formatWholePoints(9_222, 'en'), '0');
  assert.equal(formatWholePoints(19_222, 'en'), '1');
  assert.equal(formatWholePoints(7_969_994, 'en'), '796');
  assert.equal(formatUsagePoints(8_317, 'en'), '0.83');
  assert.equal(formatUsagePoints(10_000, 'en'), '1.00');
});

test('Web personal center groups model calls by logical conversation turn', () => {
  const records = [
    { usage_request_id: 'request-2', logical_turn_id: 'turn-a', model: 'model-a', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 5_000, charged_microcredits: 5_000, input_tokens: 20, output_tokens: 4, cached_input_tokens: 1, started_at: '2026-08-28T08:01:00Z', completed_at: '2026-08-28T08:02:00Z' },
    { usage_request_id: 'request-1', logical_turn_id: 'turn-a', model: 'model-a', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 3_317, charged_microcredits: 3_317, input_tokens: 10, output_tokens: 2, cached_input_tokens: 0, started_at: '2026-08-28T08:00:00Z', completed_at: '2026-08-28T08:01:30Z' },
    { usage_request_id: 'legacy-request', model: 'model-a', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'COMPLETED', settlement_status: 'RELEASED', actual_microcredits: null, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-08-27T08:00:00Z' },
  ];
  const summaries = summarizeUsageRecords(records);
  assert.equal(summaries.length, 2);
  assert.equal(summaries[0].logical_turn_id, 'turn-a');
  assert.equal(summaries[0].usage_request_count, 2);
  assert.equal(summaries[0].actual_microcredits, 8_317);
  assert.equal(summaries[0].charged_microcredits, 8_317);
  assert.equal(summaries[0].execution_status, '');
  assert.equal(summaries[0].settlement_status, '');
  assert.equal(summaries[0].input_tokens, 30);
  assert.equal(summaries[0].output_tokens, 6);
  assert.equal(summaries[0].cached_input_tokens, 1);
  assert.equal(summaries[0].started_at, '2026-08-28T08:00:00Z');
  assert.equal(summaries[0].completed_at, '2026-08-28T08:02:00Z');
  assert.equal(summaries[1].usage_request_count, 1);
  assert.equal(summaries[1].actual_microcredits, null);
  assert.equal(summaries[1].charged_microcredits, 0);
});

test('Web usage summary never merges a legacy request id with an equal logical turn id', () => {
  const collision = '0199-usage-collision';
  const summaries = summarizeUsageRecords([
    { usage_request_id: 'request-a', logical_turn_id: collision, display_status: 'RELEASED', settlement_status: 'RELEASED', charged_microcredits: 0 },
    { usage_request_id: collision, display_status: 'RELEASED', settlement_status: 'RELEASED', charged_microcredits: 0 },
  ]);
  assert.equal(summaries.length, 2);
});

test('Web usage summary keeps an unfinished request actionable within a grouped turn', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'request-complete', logical_turn_id: 'turn-active', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 1_000, charged_microcredits: 1_000, input_tokens: 10, output_tokens: 2, cached_input_tokens: 0, started_at: '2026-08-28T08:00:00Z', completed_at: '2026-08-28T08:01:00Z' },
    { usage_request_id: 'request-active', logical_turn_id: 'turn-active', display_status: 'IN_PROGRESS', execution_status: 'UPSTREAM_DISPATCHING', delivery_status: 'DELIVERING', settlement_status: 'HOLD_ACTIVE', actual_microcredits: null, charged_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-08-28T08:02:00Z' },
  ]);
  assert.equal(summary.cancel_usage_request_id, 'request-active');
  assert.equal(summary.display_status, 'IN_PROGRESS');
  assert.equal(summary.execution_status, '');
  assert.equal(summary.settlement_status, '');
  assert.equal(summary.actual_microcredits, null);
  assert.equal(summary.charged_microcredits, null);
  assert.equal(summary.completed_at, '');
});

test('Web usage summary derives charge status from every request in the logical turn', () => {
  const summaries = summarizeUsageRecords([
    { usage_request_id: 'released-retry', logical_turn_id: 'turn-mixed', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'DELIVERED', settlement_status: 'RELEASED', actual_microcredits: 56_300, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-09-15T11:54:01Z', completed_at: '2026-09-15T12:00:00Z' },
    { usage_request_id: 'settled-call', logical_turn_id: 'turn-mixed', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'DELIVERED', settlement_status: 'SETTLED', actual_microcredits: 36_800, charged_microcredits: 36_800, input_tokens: 14_199, output_tokens: 1_053, cached_input_tokens: 128, started_at: '2026-09-15T11:54:00Z', completed_at: '2026-09-15T11:55:00Z' },
    { usage_request_id: 'released-only', logical_turn_id: 'turn-released', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'DELIVERED', settlement_status: 'RELEASED', actual_microcredits: 49_900, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-09-15T06:51:00Z', completed_at: '2026-09-15T06:52:00Z' },
  ]);
  assert.equal(summaries[0].display_status, 'SETTLED');
  assert.equal(summaries[0].execution_status, '');
  assert.equal(summaries[0].settlement_status, '');
  assert.equal(summaries[0].actual_microcredits, 93_100);
  assert.equal(summaries[0].charged_microcredits, 36_800);
  assert.equal(summaries[0].input_tokens, null);
  assert.equal(summaries[0].output_tokens, null);
  assert.equal(summaries[0].cached_input_tokens, null);
  assert.equal(summaries[0].cancel_usage_request_id, '');
  assert.equal(summaries[1].display_status, 'RELEASED');
  assert.equal(summaries[1].actual_microcredits, 49_900);
  assert.equal(summaries[1].charged_microcredits, 0);
});

test('Web usage summary treats missing Tokens as zero only before dispatch', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'cancelled', logical_turn_id: 'turn-cancelled', display_status: 'CANCELLED_BEFORE_DISPATCH', settlement_status: 'RELEASED', actual_microcredits: null, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null },
    { usage_request_id: 'settled', logical_turn_id: 'turn-cancelled', display_status: 'SETTLED', settlement_status: 'SETTLED', actual_microcredits: 1_000, charged_microcredits: 1_000, input_tokens: 10, output_tokens: 2, cached_input_tokens: 1 },
  ]);
  assert.equal(summary.input_tokens, 10);
  assert.equal(summary.output_tokens, 2);
  assert.equal(summary.cached_input_tokens, 1);
});

test('Web usage summary consumes a bounded server-side turn projection', () => {
  const [summary] = summarizeUsageRecords([{
    usage_request_id: 'head-request', logical_turn_id: 'large-turn', usage_request_count: 10_000,
    cancel_usage_request_id: 'active-request', display_status: 'MANUAL_REVIEW', execution_status: '', delivery_status: '', settlement_status: '',
    actual_microcredits: null, charged_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null,
  }]);
  assert.equal(summary.usage_request_count, 10_000);
  assert.equal(summary.cancel_usage_request_id, 'active-request');
  assert.equal(summary.display_status, 'MANUAL_REVIEW');
});

test('Web usage summary does not offer cancellation after execution enters settlement or review', () => {
  const summaries = summarizeUsageRecords([
    { usage_request_id: 'settlement-pending', logical_turn_id: 'turn-pending', display_status: 'SETTLEMENT_PENDING', execution_status: 'SETTLEMENT_PENDING', delivery_status: 'DELIVERED', settlement_status: 'USAGE_PENDING', actual_microcredits: 30_700, charged_microcredits: null, input_tokens: 14_196, output_tokens: 293, cached_input_tokens: 128, started_at: '2026-09-15T09:40:00Z' },
    { usage_request_id: 'review', logical_turn_id: 'turn-review', display_status: 'MANUAL_REVIEW', execution_status: 'REVIEW', delivery_status: 'DELIVERED', settlement_status: 'REVIEW', actual_microcredits: null, charged_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-09-15T05:45:00Z', completed_at: '2026-09-15T05:46:00Z' },
  ]);
  for (const summary of summaries) {
    assert.equal(summary.charged_microcredits, null);
    assert.equal(summary.cancel_usage_request_id, '');
  }
});

test('Web usage summary keeps manual review visible beside an active request', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'active', logical_turn_id: 'turn-review-active', display_status: 'IN_PROGRESS', execution_status: 'STREAMING', delivery_status: 'DELIVERING', settlement_status: 'HOLD_ACTIVE', actual_microcredits: null, charged_microcredits: null, started_at: '2026-09-15T05:45:01Z' },
    { usage_request_id: 'review', logical_turn_id: 'turn-review-active', display_status: 'MANUAL_REVIEW', execution_status: 'REVIEW', delivery_status: 'DELIVERED', settlement_status: 'REVIEW', actual_microcredits: 12_300, charged_microcredits: null, started_at: '2026-09-15T05:45:00Z' },
  ]);
  assert.equal(summary.display_status, 'MANUAL_REVIEW');
  assert.equal(summary.cancel_usage_request_id, 'active');
  assert.equal(summary.execution_status, '');
  assert.equal(summary.delivery_status, '');
  assert.equal(summary.settlement_status, '');
  assert.equal(summary.charged_microcredits, null);
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

test('personal surfaces show whole-point balances while Web model usage keeps two decimals', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
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
  assert.match(view, /formatUsagePoints\(record\.charged_microcredits, language\)/);
  assert.match(popover, /import \{ formatWholePoints \} from '\.\/credit-units\.js'/);
  assert.equal((popover.match(/formatWholePoints\(/g) || []).length, 4);
  assert.doesNotMatch(popover, /\bformatPoints\(/);
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
    await client.organizations({ q:'研发 & AI', status:'DISABLED', cursor:'next/page' });
    await client.organizationMembers('org-id', { q:'vinpay', role:'AUDITOR', cursor:'member/page' });
    await client.transferOrganizationOwnership('org-id', 'new-owner-id');
    await client.organizationUnits('org-id');
    await client.createOrganizationUnit('org-id', 'Engineering', 'parent-id');
    await client.renameOrganization('org-id', 'Renamed');
    await client.renameOrganizationUnit('org-id', 'unit-id', 'Platform');
    await client.deleteOrganizationUnit('org-id', 'unit-id');
    assert.deepEqual(calls.map(call => [call.url, call.options.method || 'GET']), [
      ['/portal/api/admin/account/admin/v1/organizations', 'POST'],
      ['/portal/api/admin/account/admin/v1/organizations?limit=25&q=%E7%A0%94%E5%8F%91+%26+AI&status=DISABLED&cursor=next%2Fpage', 'GET'],
      ['/portal/api/admin/account/admin/v1/organizations/org-id/members?limit=25&q=vinpay&role=AUDITOR&cursor=member%2Fpage', 'GET'],
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
    await client.users({ q:'new owner', exclude_organization_id:'org-id' });
    await client.registrationRepairs({ status: 'REPAIR_REQUIRED', older_than_minutes: '30' });
    await client.profileChangeRepairs();
    await client.accountClosures();
    await client.retryRegistration('registration-id');
    await client.compensateRegistration('registration-id');
    await client.retryProfileChange('profile-id');
    assert.deepEqual(calls, [
      ['/portal/api/admin/account/admin/v1/users?limit=25&q=vin+pay&cursor=opaque-cursor', 'GET'],
      ['/portal/api/admin/account/admin/v1/users?limit=25&q=new+owner&exclude_organization_id=org-id', 'GET'],
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

test('BFF logout reports server failures and clears CSRF only after success', async () => {
  const originalFetch = globalThis.fetch;
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    client.csrfToken = 'c'.repeat(32);
    globalThis.fetch = async () => new Response(JSON.stringify({ code: 'LOGOUT_UNAVAILABLE', message: 'temporary failure' }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' },
    });
    await assert.rejects(client.logout(), error => error.status === 503 && error.code === 'LOGOUT_UNAVAILABLE');
    assert.equal(client.csrfToken, 'c'.repeat(32));

    globalThis.fetch = async () => new Response(null, { status: 204 });
    assert.equal(await client.logout(), null);
    assert.equal(client.csrfToken, '');
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF logout refreshes a stale CSRF token once before ending the session', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url: String(url), options });
    if (calls.length === 1) return new Response(JSON.stringify({ code: 'CSRF_REJECTED' }), { status: 403, headers: { 'Content-Type': 'application/json' } });
    if (calls.length === 2) return new Response(JSON.stringify({ authenticated: true, csrf_token: 'n'.repeat(32) }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    return new Response(null, { status: 204 });
  };
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    client.csrfToken = 'o'.repeat(32);
    await client.logout();
    assert.equal(calls.length, 3);
    assert.equal(calls[0].options.headers['X-CSRF-Token'], 'o'.repeat(32));
    assert.equal(calls[1].url, 'https://portal.example/api/session');
    assert.equal(calls[2].options.headers['X-CSRF-Token'], 'n'.repeat(32));
    assert.equal(client.csrfToken, '');
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('personal and admin logout treat another tab already ending the session as success', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url) => {
    calls.push(String(url));
    if (String(url).endsWith('/api/logout')) return new Response(JSON.stringify({ code: 'CSRF_REJECTED' }), { status: 403, headers: { 'Content-Type': 'application/json' } });
    return new Response(JSON.stringify({ authenticated: false }), { status: 401, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const personal = new AccountPlatformBffClient('https://portal.example');
    personal.csrfToken = 'p'.repeat(32);
    assert.equal(await personal.logout(), null);
    assert.equal(personal.csrfToken, '');

    const admin = new AdminPlatformClient('https://portal.example');
    admin.csrfToken = 'a'.repeat(32);
    assert.equal(await admin.logout(), null);
    assert.equal(admin.csrfToken, '');
    assert.deepEqual(calls, [
      'https://portal.example/api/logout',
      'https://portal.example/api/session',
      'https://portal.example/api/logout',
      'https://portal.example/api/session',
    ]);
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
    return new Response(JSON.stringify({ view_version: 'logical-turns-v1', items: [{ model: 'example-model', display_status: 'SETTLED', actual_microcredits: 10_000, charged_microcredits: 10_000 }] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    const history = await client.usageHistory();
    assert.equal(history.items[0].display_status, 'SETTLED');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, '/portal/api/token/v1/me/usage-turns?limit=20');
    assert.equal(calls[0].options.method, 'GET');
    assert.equal(calls[0].url.includes('/chat/completions'), false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('new Portal falls back to the N-1 per-request view without treating released Provider cost as a charge', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (url.includes('/usage-turns?')) {
      return new Response(JSON.stringify({ code: 'NOT_FOUND' }), { status: 404, headers: { 'Content-Type': 'application/json' } });
    }
    return new Response(JSON.stringify({
      items: [{ usage_request_id: 'legacy-request', logical_turn_id: 'turn-old-service', model: 'model', display_status: 'RELEASED', settlement_status: 'RELEASED', actual_microcredits: 49_900 }],
      next_cursor: 'legacy-cursor',
    }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    const history = await client.usageHistory();
    const [summary] = summarizeUsageRecords(history.items);
    assert.equal(summary.actual_microcredits, 49_900);
    assert.equal(summary.charged_microcredits, 0);
    assert.deepEqual(calls.map(call => call.url), [
      '/portal/api/token/v1/me/usage-turns?limit=20',
      '/portal/api/token/v1/me/usage-requests?limit=20',
    ]);

    calls.length = 0;
    await client.usageHistory(history.next_cursor);
    assert.deepEqual(calls.map(call => call.url), ['/portal/api/token/v1/me/usage-requests?limit=20&cursor=legacy-cursor']);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('logical-turn cursors stay on the explicitly versioned view', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ view_version: 'logical-turns-v1', items: [], next_cursor: null }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    await client.usageHistory('turn-v1.YWJj');
    assert.deepEqual(calls.map(call => call.url), ['/portal/api/token/v1/me/usage-turns?limit=20&cursor=turn-v1.YWJj']);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('projection backfill falls back safely in both Web and desktop transports', async () => {
  const webCalls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    webCalls.push(url);
    if (url.includes('/usage-turns?')) {
      return new Response(JSON.stringify({ code: 'USAGE_HISTORY_PROJECTION_BUILDING' }), { status: 503, headers: { 'Content-Type': 'application/json' } });
    }
    return new Response(JSON.stringify({ items: [], next_cursor: null }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const webClient = new AccountPlatformBffClient('/portal');
    await webClient.usageHistory();
    assert.deepEqual(webCalls, [
      '/portal/api/token/v1/me/usage-turns?limit=20',
      '/portal/api/token/v1/me/usage-requests?limit=20',
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }

  const desktopCalls = [];
  const desktop = new AccountPlatformDesktopClient({
    request: async request => {
      desktopCalls.push(request.resource);
      if (request.resource.includes('/usage-turns?')) return { status: 404, body: { code: 'NOT_FOUND' } };
      return { status: 200, body: { items: [], next_cursor: null } };
    },
  });
  await desktop.usageHistory();
  assert.deepEqual(desktopCalls, [
    '/v1/me/usage-turns?limit=20',
    '/v1/me/usage-requests?limit=20',
  ]);
});

test('desktop client keeps OAuth tokens behind the account-platform Bridge', async () => {
  const calls = [];
  const transport = {
    session: async () => ({ enabled: true, configured: true, authenticated: true }),
    openPortal: async input => {
      calls.push({ openPortal: input });
    },
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
  await client.openPortal('ja');
  await client.login('person@example.com', 'secret-password');
  await client.createRecharge('product-10', 'ALIPAY', 'recharge-id');
  assert.deepEqual(calls[0], { openPortal: { language: 'ja' } });
  assert.deepEqual(calls[1], { credentials: { account: 'person@example.com', password: 'secret-password' } });
  assert.deepEqual(calls[2], {
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
  const passwordInput = await readFile(new URL('../src/features/account-platform/PasswordInput.jsx', import.meta.url), 'utf8');
  const bridge = await readFile(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  const backend = await readFile(new URL('../src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8');
  assert.match(passwordInput, /type="password"/);
  assert.match(passwordInput, /data-password-reveal-output/);
  assert.match(popover, /'password', 'sms', 'register'/);
  assert.match(popover, /copy\.registrationUsername/);
  assert.match(popover, /client\.registerMobile\(account\.trim\(\), mobile\.trim\(\)/);
  assert.match(popover, /client\.precheckMobileRegistration\(account\.trim\(\), mobile\.trim\(\), password\)/);
  assert.match(popover, /client\.createSliderCaptcha\(mobile\.trim\(\)/);
  assert.match(popover, /<SliderCaptcha/);
  assert.match(popover, /setSmsCooldown\(Math\.max\(1, Number\(result\.retry_after_seconds \|\| 60\)\)\)/);
  assert.match(popover, /smsCooldown > 0 \? copy\.resendCountdown\(smsCooldown\)/);
  assert.match(popover, /autoComplete=\{loginMode === 'password' \? 'current-password' : 'new-password'\}/);
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
  const passwordInput = await readFile(new URL('../src/features/account-platform/PasswordInput.jsx', import.meta.url), 'utf8');
  assert.match(popover, /const \[passwordConfirmation, setPasswordConfirmation\] = useState\(''\)/);
  assert.match(popover, /const \[credentialEpoch, setCredentialEpoch\] = useState\(0\)/);
  assert.match(passwordInput, /type="password"/);
  assert.match(popover, /showLabel=\{copy\.showPassword\}[\s\S]*?hideLabel=\{copy\.hidePassword\}/);
  assert.match(popover, /showLabel=\{copy\.showPasswordConfirmation\}[\s\S]*?hideLabel=\{copy\.hidePasswordConfirmation\}/);
  assert.match(passwordInput, /aria-pressed=\{revealed\}/);
  assert.match(popover, /if \(password !== passwordConfirmation\) return copy\.passwordMismatch/);
  assert.match(popover, /registrationInputError\(copy, account, mobile, password, passwordConfirmation\)/);
  assert.match(popover, /placeholder=\{copy\.confirmPassword\}[\s\S]*?value=\{passwordConfirmation\}|value=\{passwordConfirmation\}[\s\S]*?placeholder=\{copy\.confirmPassword\}/);
  assert.match(popover, /!passwordConfirmation \|\| password !== passwordConfirmation/);
  assert.equal(accountPlatformCopy.zh.mobileOrAccount, '手机号码或用户名');
  assert.equal(accountPlatformCopy.zh.password, '密码（至少 8 位，含字母和数字）');
  assert.equal(accountPlatformCopy.zh.confirmPassword, '再次输入密码');
  assert.equal(accountPlatformCopy.zh.passwordMismatch, '两次输入的密码不一致。');
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(accountPlatformCopy[language].showPassword);
    assert.ok(accountPlatformCopy[language].hidePassword);
    assert.ok(accountPlatformCopy[language].showPasswordConfirmation);
    assert.ok(accountPlatformCopy[language].hidePasswordConfirmation);
  }
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
  assert.match(popover, /onClose=\{\(\) => \{\s*if \(busy\) return;\s*setCaptcha\(null\);\s*setCaptchaOffset\(0\);\s*\}\}/);
  assert.match(popover, /if \(captcha && !busy\)[\s\S]*?setCaptcha\(null\)/);
  assert.equal(accountPlatformCopy.zh.captchaClose, '关闭验证');
});

test('releasing a mobile captcha slider verifies immediately and requests the SMS code', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const webView = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  for (const source of [popover, webView]) {
    assert.match(source, /onPointerDown=\{\(\) => setDragging\(true\)\}/);
    assert.match(source, /onPointerUp=\{releaseSlider\}/);
    assert.match(source, /if \(!dragging \|\| busy\) return;/);
    assert.match(source, /onConfirm\(shownOffset\)/);
  }
  assert.match(popover, /onConfirm=\{\(\) => void sendSms\(\)\}/);
  assert.match(webView, /onConfirm=\{sendMobileCode\}/);
});

test('Web profile captcha submits the visible slider offset and keeps failures inside the verification dialog', async () => {
  assert.equal(profileCaptchaSubmissionOffset(173, 0), 173);
  assert.equal(profileCaptchaSubmissionOffset({ type: 'click' }, 149), 149);
  assert.equal(profileCaptchaSubmissionOffset(undefined, 88), 88);
  assert.equal(profileCaptchaSubmissionOffset(-1, 88), 0);
  assert.equal(profileCaptchaSubmissionOffset(NaN, 88), 0);

  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const openBlocks = [...view.matchAll(/const openMobileCaptcha = \(\) =>[\s\S]*?const sendMobileCode/g)].map((match) => match[0]);
  const sendBlocks = [...view.matchAll(/const sendMobileCode = \(selectedOffset\) =>[\s\S]*?const completeMobile/g)].map((match) => match[0]);
  assert.equal(openBlocks.length, 2);
  assert.equal(sendBlocks.length, 2);
  for (const block of openBlocks) {
    assert.match(block, /createProfileSliderCaptcha\(mobile\.trim\(\)\)/);
    assert.doesNotMatch(block, /createProfileChange/);
  }
  for (const block of sendBlocks) {
    assert.match(block, /createProfileChange\('MOBILE', mobile\.trim\(\)\)/);
    assert.match(block, /sendProfileMobileChallenge\(id, captcha\.captcha_id, profileCaptchaSubmissionOffset\(selectedOffset, captchaOffset\)\)/);
    assert.match(block, /delivery\?\.delivery_status !== 'SENT'/);
    assert.match(block, /smsDeliveryPending[\s\S]*?return;[\s\S]*?setChallengeSent\(true\)/);
  }
  assert.match(view, /function ProfileCaptcha\(\{ challenge, offset, busy, error = ''/);
  assert.match(view, /<div role="alert" className="ap-alert error">\s*\{error\}/);
  assert.match(view, /error=\{captchaError\}/);
  assert.match(view, /SMS_UNAVAILABLE: copy\.smsUnavailable/);
  assert.match(view, /delivery_status !== 'SENT'/);
  assert.equal(typeof accountPlatformCopy.zh.smsDeliveryPending, 'string');
  assert.equal(typeof accountPlatformCopy.en.smsDeliveryPending, 'string');
  assert.equal(typeof accountPlatformCopy.ja.smsDeliveryPending, 'string');
});

test('Web account closure reauthenticates and resumes explicit confirmation instead of reporting platform unavailable', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  const flow = await readFile(new URL('../src/features/account-platform/account-closure-flow.js', import.meta.url), 'utf8');
  const closure = await readFile(new URL('../../account-platform/internal/account/account_closure.go', import.meta.url), 'utf8');
  const service = await readFile(new URL('../../account-platform/internal/account/service.go', import.meta.url), 'utf8');

  assert.match(closure, /validateAccountClosureRequest\(now, principal, idempotencyKey, confirmation\)/);
  assert.match(closure, /now\.Sub\(principal\.AuthenticatedAt\) > accountClosureRecentAuth[\s\S]*?return ErrRecentAuthenticationRequired/);
  assert.match(service, /ErrRecentAuthenticationRequired[\s\S]*?RECENT_AUTHENTICATION_REQUIRED/);
  assert.match(flow, /accountPlatformErrorCodes\(reason\)\.includes\('RECENT_AUTHENTICATION_REQUIRED'\)/);
  assert.match(view, /params\.set\('closure', 'confirm'\)[\s\S]*?submitAccountClosure\(\{/);
  assert.match(view, /accountClosureResumeAction\(\{[\s\S]*?if \(!action\.clearMarker\) return;[\s\S]*?params\.delete\('closure'\)[\s\S]*?if \(action\.prompt\) void onRequestClosure\(\)/);
  assert.match(view, /<AccountClosure[\s\S]*?queryStatus=\{closureQueryState\}[\s\S]*?onRetry=\{retryClosure\}/);
  assert.match(view, /presentation\.state === accountClosureQueryStatus\.loading[\s\S]*?presentation\.state === accountClosureQueryStatus\.error[\s\S]*?!presentation\.canRequest/);
  assert.match(flow, /confirmation !== 'CLOSE_MY_ACCOUNT'[\s\S]*?confirmation-mismatch/);
  for (const language of ['zh', 'en', 'ja']) {
    assert.equal(typeof accountPlatformCopy[language].closure.recentAuthenticationRequired, 'string');
    assert.ok(accountPlatformCopy[language].closure.recentAuthenticationRequired.length > 0);
  }
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
  assert.match(popover, /const openRecharge = \(\) => \{\s*if \(!personalSurfaceAccess\(me\)\.rechargeCreate\) \{\s*setError\(copy\.permissionDenied\);\s*return;\s*\}\s*setOrder\(null\)/);
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

test('recharge only shows a payment-channel selector when the backend offers a real choice', async () => {
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.equal(hasSelectablePaymentChannels(), false);
  assert.equal(hasSelectablePaymentChannels(['ALIPAY']), false);
  assert.equal(hasSelectablePaymentChannels(['alipay', ' ALIPAY ']), false);
  assert.equal(hasSelectablePaymentChannels(['ALIPAY', 'WECHAT']), true);
  assert.equal(effectivePaymentChannel(['WECHAT'], 'ALIPAY'), 'WECHAT');
  assert.equal(effectivePaymentChannel(['ALIPAY', 'WECHAT'], 'wechat'), 'WECHAT');
  assert.equal(effectivePaymentChannel([], 'ALIPAY'), '');
  assert.match(popover, /hasSelectablePaymentChannels\(channels\) &&/);
  assert.match(popover, /onCreate\(productId, effectiveChannel\)/);
  assert.equal((view.match(/hasSelectablePaymentChannels\(availableChannels\) &&/g) || []).length, 2);
  assert.match(view, /onPurchase\(productId, effectiveChannel\)/);
  assert.match(view, /onRecharge\(productId, effectiveChannel\)/);
});

test('QR scan instructions come from the order capability snapshot instead of current products or accounting channels', async () => {
  const provider = await readFile(new URL('../../account-platform/internal/platform/h3cpayment/provider.go', import.meta.url), 'utf8');
  const billing = await readFile(new URL('../../account-platform/internal/billing/payments.go', import.meta.url), 'utf8');
  const orderReader = await readFile(new URL('../../account-platform/internal/billing/refunds.go', import.meta.url), 'utf8');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.equal(paymentScanPrompt(accountPlatformCopy.zh, ['ALIPAY', 'WECHAT']), '支持以下应用扫码：支付宝、微信支付');
  assert.equal(paymentScanPrompt(accountPlatformCopy.en, []), accountPlatformCopy.en.scanToPay);
  assert.match(provider, /QRScanApps:\s*\[\]string\{"ALIPAY", "WECHAT"\}/);
  assert.match(billing, /QRScanApps:\s*append\(\[\]string\(nil\), capabilities\.QRScanApps\.\.\.\)/);
  assert.match(orderReader, /provider_capability_snapshot/);
  assert.match(orderReader, /applyPaymentCapabilitySnapshot/);
  assert.match(popover, /const scanApps = order\?\.qr_scan_apps \|\| \[\]/);
  assert.match(view, /const paymentScanApps = order\?\.qr_scan_apps \|\| \[\]/);
  assert.doesNotMatch(popover, /products\.find\(\(product\) => product\.product_id === order\?\.product_id\)\?\.qr_scan_apps/);
  assert.doesNotMatch(view, /products\.find\(\(product\) => product\.product_id === order\?\.product_id\)\?\.qr_scan_apps/);
  assert.match(popover, /paymentScanPrompt\(copy, scanApps\)/);
  assert.match(view, /paymentScanPrompt\(copy, scanApps\)/);
  assert.doesNotMatch(popover, /h3c_mall|provider_id/);
  assert.doesNotMatch(view, /h3c_mall/);
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
  assert.match(runtimeProvider, /model\.base_url = self\s*\.model_proxy\s*\.register_session\(\s*&request\.session_id,\s*&authorization\.base_url,/);
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
  assert.match(i18n, /users:\s*'用户管理',\s*roles:\s*'角色与权限',\s*organizations:\s*'组织管理'/);
  assert.match(view, /key:\s*'username',\s*label:\s*copy\.userManagement\.account/);
  assert.doesNotMatch(view, /copy\.accounts\.email/);
  assert.doesNotMatch(view, /function userLabel\(user\) \{[\s\S]*?user\?\.email[\s\S]*?\}/);
  assert.doesNotMatch(view, /item\.display_name \|\| item\.email/);
  assert.doesNotMatch(view, /member\.email/);
  assert.match(view, /client\.organizationMembers\(organization\.organization_id, filters\)/);
  assert.match(view, /setSelectedMember\(member\)/);
  assert.match(view, /memberCursorBack\.current/);
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
  assert.match(i18n, /title:\s*'个人中心'/);
  assert.equal(accountPlatformCopy.zh.activeContext, '当前身份');
  assert.equal(accountPlatformCopy.zh.switchContext, '切换身份');
  assert.equal(accountPlatformCopy.zh.personal, '个人账号');
  assert.doesNotMatch(i18n, /当前上下文|身份上下文/);
  assert.match(view, /function Overview[\s\S]*copy\.activeContext/);
  const overview = view.slice(view.indexOf('function Overview'), view.indexOf('function Overview') + 8000);
  assert.equal((overview.match(/copy\.activeContext/g) || []).length, 1);
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
  assert.match(view, /access\.profileRead \|\| access\.billingRead \|\| access\.invitationsRead \? \['overview'\]/);
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
  assert.match(i18n, /usage:\s*'用量记录'/);
  assert.doesNotMatch(view, /section === 'tokens'|client\.(managedTokens|eligibleTokens|credentials|rotateCredential|createTokenAccount)/);
  assert.doesNotMatch(i18n, /Token 管理|供应商凭据|模型消费准备状态|消费结算状态|可管理 Token/);
  assert.match(bffClient, /usageHistory\(cursor = '', signal\)/);
  assert.match(desktopClient, /usageHistory\(cursor = '', signal\)/);
  assert.doesNotMatch(`${bffClient}\n${desktopClient}`, /managedTokens|eligibleTokens|rotateCredential|createTokenAccount/);
});

test('account portal uses stable asset names with revalidated caching', async () => {
  const viteConfig = await readFile(new URL('../vite.config.mjs', import.meta.url), 'utf8');
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
  assert.match(view, /readAdminOrganizationRoute/);
  assert.match(view, /organization_id: item\.organization_id, organization_tab: 'overview'/);
  assert.match(view, /activeTab === 'members'/);
  assert.match(view, /activeTab === 'units'/);
  assert.match(view, /activeTab === 'lifecycle'/);
  assert.doesNotMatch(view, /<DetailDrawer label=\{`\$\{copy\.accounts\.organizationManagement\}/);
  assert.match(view, /organization_status: organizationStatus \|\| null/);
  assert.match(view, /organization_cursor: nextCursor \|\| null/);
  assert.match(view, /client\.users\(ownerSearch\.trim\(\), controller\.signal\)/);
  assert.match(view, /createOpen &&/);
});

test('payment capabilities isolate provider limitations and enable reviewed-order repair', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const i18n = await readFile(new URL('../src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  const provider = await readFile(new URL('../../account-platform/internal/platform/h3cpayment/provider.go', import.meta.url), 'utf8');
  assert.match(provider, /ProviderID: "h3c_mall"/);
  assert.match(provider, /OrderQuerySupported: true/);
  assert.match(provider, /ManualRepairSupported: true/);
  assert.match(view, /normalRefundCreationEnabled =\s*paymentCapabilities\.normal_refund_creation_enabled === true &&\s*paymentCapabilities\.refund_supported === true/);
  assert.match(view, /reconciliationSupported = paymentCapabilities\.bill_supported === true/);
  assert.match(view, /reconciliationImportSupported = paymentCapabilities\.bill_file_import_supported === true/);
  assert.match(view, /repairSupported = paymentCapabilities\.manual_repair_supported === true/);
  assert.match(view, /client\.repairPaymentOrder\(item\.order_id, reason\)/);
  assert.doesNotMatch(view, /paymentCapabilities\.provider === 'h3c_mall'/);
  assert.match(view, /normalRefundCreationEnabled && item\.status === 'ACTIVE'/);
  assert.match(view, /disabled=\{busy \|\| !canRequestRepair \|\| !repairSupported/);
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
  assert.match(organizationEntitlements, /if \(await onPurchase\(productId, effectiveChannel\)\) setPaymentDialogOpen\(true\)/);
  assert.match(organizationEntitlements, /paymentDialogOpen && activePayment && <PaymentQRCodeDialog/);
  assert.match(view, /client\.organizationBalance\(activeOrganizationId\)/);
  assert.match(view, /client\.organizationEntitlementPackages\(activeOrganizationId\)/);
  assert.match(view, /client\.organizationRechargeOrder\(activeOrganizationId, order\.order_id\)/);
  assert.match(backend, /\[\s*"v1",\s*"organizations",\s*organization_id,\s*"recharge",\s*"orders",\s*order_id,?\s*\]/);
  assert.match(backend, /safe_id\(organization_id\) && safe_id\(order_id\)/);
});

test('personal center clears a transient background synchronization error after account bootstrap recovers', async () => {
  const view = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(view, /const \[synchronizationError, setSynchronizationError\] = useState\(''\)/);
  assert.match(view, /const account = await client\.bootstrap\(\);[\s\S]*?setSynchronizationError\(''\);[\s\S]*?resolveAccountSynchronization/);
  assert.match(view, /resolveAccountSynchronization\([\s\S]*?if \(!synchronization\.applyAccount\) \{[\s\S]*?synchronizationQueue\.retry\(\);[\s\S]*?return;[\s\S]*?meRef\.current = synchronization\.account;[\s\S]*?setMe\(synchronization\.account\)/);
  assert.match(view, /const requestSynchronization = \(\) => synchronizationQueue\.request\(synchronize\)/);
  assert.match(view, /synchronizationQueue\?\.beginContextMutation\(\);[\s\S]*?finally \{[\s\S]*?synchronizationQueue\?\.endContextMutation\(\)/);
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
  const bridge = await readFile(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  const command = await readFile(new URL('../src-tauri/src/app/commands/account_platform.rs', import.meta.url), 'utf8');
  assert.doesNotMatch(popover, /client\.modelCatalog|client\.selectCatalogModel/);
  assert.doesNotMatch(popover, /copy\.platformModel|copy\.selectPlatformModel/);
  assert.match(popover, /client\.balance/);
  assert.match(popover, /client\.openPortal\(language\)/);
  assert.match(bridge, /openPortal: function \(input\) \{ return invoke\("account_platform_open_portal", \{ input: input \}\); \}/);
  assert.match(command, /account_platform_open_portal\([\s\S]*?input: PortalOpenRequest,[\s\S]*?state\.open_portal\(input\.language\)\.await/);
  for (const language of ['zh', 'en', 'ja']) {
    for (const key of ['platformModel', 'selectPlatformModel', 'modelSelectionSaved', 'modelDeprecationWarning']) {
      assert.equal(accountPlatformCopy[language][key], undefined, `${language}.${key} is retired from the account popover copy`);
    }
  }
});

test('settings owns the platform model default for new conversations', async () => {
  const settings = await readFile(new URL('../src/features/settings/SettingsView.jsx', import.meta.url), 'utf8');
  const main = await readFile(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
  const i18n = (await Promise.all(['zh', 'en', 'ja'].map(language => readFile(new URL('../src/shared/i18n/' + language + '.js', import.meta.url), 'utf8')))).join('');
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
  const settings = await readFile(new URL('../src/features/settings/composer-shared.jsx', import.meta.url), 'utf8');
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
