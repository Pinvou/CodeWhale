import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('portal consumes only existing Billing filters and recharge product stable cursors', async () => {
  const [view, navigation, client, product, productStyles, copy] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('internal/portal/static/assets/product-admin.js'),
    read('internal/portal/static/assets/product-admin.css'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
  ]);

  for (const parameter of [
    'receivable_wallet_id', 'receivable_status', 'receivable_limit',
    'payment_q', 'payment_status', 'payment_limit',
    'late_status', 'late_provider_id', 'late_owner_id', 'late_entered_from', 'late_entered_to', 'late_next_review_before', 'late_overdue', 'late_limit',
    'anomaly_type', 'anomaly_status', 'anomaly_severity', 'anomaly_owner_id', 'anomaly_limit',
  ]) assert.match(navigation, new RegExp(parameter));
  assert.match(navigation, /readAdminFinanceQuery/);
  assert.match(navigation, /adminFinanceQueryUpdates/);
  assert.match(view, /onRouteQueryChange\?\.\('receivables', query\)/);
  assert.match(view, /onRouteQueryChange\?\.\('orders', query\)/);
  assert.match(view, /onRouteQueryChange\?\.\('lateReviews', query\)/);
  assert.match(view, /onRouteQueryChange\?\.\('anomalies', query\)/);
  assert.match(view, /client\.receivables\(querySnapshot\)/);
  assert.match(view, /requestedQuery: querySnapshot/);
  assert.match(view, /const querySnapshot = routeAware \? normalizeAdminFinanceQuery\(targetTab, options\.query \|\| routeQuery\) : null/);
  assert.doesNotMatch(view, /options\.query \|\| (receivableFilter|orderFilter|lateFilter|anomalyFilter)/);
  assert.match(view, /loadTab\(tab, routeAware \? \{ query: normalizeAdminFinanceQuery\(tab, routeQuery\) \} : undefined\)/);
  assert.match(view, /currentPageRange\(items\.length/);
  assert.doesNotMatch(view, /<SectionHeader title=\{surface === 'payments'[\s\S]*?count=\{activeTabState\.loaded \? items\.length/);
  assert.doesNotMatch(view, /total_count/);
  assert.match(client, /receivables\(filters = \{\}, signal\)/);
  for (const method of ['paymentOrders', 'latePaymentReviews', 'paymentAnomalies']) assert.match(client, new RegExp(`${method}\\(filters = \\{\\}, signal\\)`));

  for (const parameter of ['product_cursor', 'product_limit', 'product_lifecycle_status', 'product_include_deleted']) assert.match(product, new RegExp(parameter));
  assert.match(product, /next_cursor/);
  assert.match(product, /pageCursors/);
  assert.match(product, /product-previous/);
  assert.match(product, /product-next/);
  assert.match(product, /window\.addEventListener\("popstate"/);
  assert.doesNotMatch(product, /total_count/);
  assert.match(productStyles, /\.product-pagination/);
  assert.match(copy, /currentPageRange/);
});
