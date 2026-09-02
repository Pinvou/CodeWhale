import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('Portal consumes every Billing stable collection without inventing totals', async () => {
  const [view, navigation, pagination, client, copy, built, servicePagination, postgresPagination] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
    read('../pinvou3-app/src/features/account-platform/admin-finance-pagination.js'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('internal/portal/static/assets/admin.js'),
    read('internal/billing/admin_pagination.go'),
    read('internal/billing/admin_pagination_integration_test.go'),
  ]);

  for (const tab of ['wallets', 'receivables', 'adjustments', 'holds', 'orders', 'refunds', 'lateReviews', 'anomalies', 'reconciliations']) {
    assert.match(navigation, new RegExp(`${tab}: Object\\.freeze`), `${tab} must publish URL filters and cursor`);
    const method = tab === 'lateReviews' ? 'latePaymentReviews' : tab === 'anomalies' ? 'paymentAnomalies' : tab === 'orders' ? 'paymentOrders' : tab === 'reconciliations' ? 'reconciliationRuns' : tab;
    assert.match(view, new RegExp(`client\\.${method}\\(querySnapshot\\)`));
  }
  assert.match(view, /normalizeFinanceCollection/);
  for (const parameter of ['wallet_cursor', 'receivable_cursor', 'adjustment_cursor', 'hold_cursor', 'payment_cursor', 'refund_cursor', 'late_cursor', 'anomaly_cursor', 'reconciliation_cursor']) assert.match(navigation, new RegExp(parameter));
  for (const method of ['wallets', 'walletLedger', 'receivables', 'paymentOrders', 'refunds', 'adjustments', 'holds', 'reconciliationRuns', 'reconciliationCases', 'latePaymentReviews', 'paymentAnomalies']) assert.match(client, new RegExp(`${method}\\([^)]*filters = \\{\\}`));

  assert.match(pagination, /next_cursor/);
  assert.match(pagination, /snapshot_as_of/);
  assert.match(pagination, /legacy: !hasSnapshotContract/);
  assert.match(pagination, /PAGINATION_CURSOR_EXPIRED/);
  assert.match(view, /rememberFinanceCursor/);
  assert.match(view, /previousFinanceCursor/);
  assert.match(view, /restartExpiredFinancePage/);
  assert.match(view, /copy\.billing\.snapshotExpired/);
  assert.match(view, /client\.walletLedger\(wallet\.wallet_id, query\)/);
  assert.match(view, /client\.reconciliationCases\(run\.reconciliation_run_id, query\)/);
  for (const parameter of ['finance_detail', 'finance_detail_id', 'ledger_entry_type', 'ledger_business_reference', 'ledger_created_from', 'ledger_cursor', 'case_status', 'case_severity', 'case_owner_id', 'case_type', 'case_cursor']) assert.match(navigation, new RegExp(parameter));
  assert.match(view, /const canReadLedger = permissionSet\.has\('platform\.billing\.ledger\.read'\)/);
  assert.match(view, /\.\.\.\(canReadLedger \? \[\{/);
  assert.doesNotMatch(view, /client\.wallets\(\{ limit: '200' \}\)/);
  assert.match(view, /routeDetail\.type === 'ledger' && canReadLedger/);
  assert.match(view, /routeDetail\.type === 'reconciliationCases' && canReadUsage/);
  assert.match(view, /ledger\.page\?\.legacy \|\| ledger\.page\?\.expired/);
  assert.match(view, /cases\.page\?\.legacy \|\| cases\.page\?\.expired/);
  assert.match(view, /page\.expired \|\| !page\.nextCursor/);
  assert.match(view, /ledger-detail/);
  assert.match(view, /reconciliation-cases-detail/);
  assert.match(view, /runFinanceDetailRequest/);
  assert.match(view, /createFinanceDetailRecovery/);
  assert.match(view, /const canReadUsageRef = useRef\(canReadUsage\)/);
  assert.match(view, /canRead: canReadUsageRef\.current/);
  assert.match(view, /if \(!routeMatches\(\)\) return null/);
  assert.match(view, /operation\.restartPagination \? copy\.billing\.restartPagination/);
  assert.match(view, /if \(!canReconcileRef\.current \|\| !cases \|\| cases\.loading \|\| cases\.page\?\.expired\) return/);
  assert.match(view, /action === 'ASSIGN' && !canReadUserDirectoryRef\.current/);
  assert.match(view, /\.\.\.\(canReconcile \? \[\{/);
  assert.match(view, /after: refreshCurrentCases/);
  assert.doesNotMatch(view, /normalizeFinanceCollection\(await client\.reconciliationCases/);
  assert.match(view, /tab === 'reconciliations' && canReadUsage/);
  assert.match(servicePagination, /expireAdminPaginationSnapshot/);
  assert.match(servicePagination, /ORDER BY created_at,id LIMIT 1/);
  assert.match(postgresPagination, /third resource should recycle the oldest subject snapshot/);
  assert.match(postgresPagination, /ErrPaginationCursorExpired/);
  assert.match(view, /FinancePagination/);
  assert.doesNotMatch(view, /total_count/);
  assert.doesNotMatch(view, /items\.length\s*[),]\s*.*total/i);
  for (const key of ['legacyPagination', 'snapshotExpired', 'restartPagination', 'snapshotAsOf']) assert.equal((copy.match(new RegExp(`${key}:`, 'g')) || []).length, 3);
  assert.match(built, /PAGINATION_CURSOR_EXPIRED/);
  assert.match(built, /snapshot_as_of/);
});

test('Portal preserves list context and requires explicit recovery from expired snapshots', async () => {
  const [view, navigation] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
  ]);
  assert.match(view, /onRouteQueryChange\?\.\(tab, nextQuery\)/);
  assert.match(view, /resetExpiredFinanceQuery\(activeTabState\.query \|\| routeQuery\)/);
  assert.match(view, /onRouteQueryChange\?\.\(tab, query, 'replace'\)/);
  assert.match(view, /activeTabState\.page\?\.expired \? restartExpiredFinancePage/);
  assert.match(view, /onRouteDetailChange\?\.\('ledger'/);
  assert.match(view, /onRouteDetailChange\?\.\('reconciliationCases'/);
  assert.match(view, /count=\{page \? undefined : items\.length\}/);
  assert.match(navigation, /key === 'overdue' \? \(query\[key\] \? 'true' : null\) : query\[key\] \|\| null/);
  assert.match(navigation, /clearAdminFinanceQueryUpdates/);
});
