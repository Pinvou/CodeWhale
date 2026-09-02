import assert from 'node:assert/strict';
import test from 'node:test';
import { financeOverviewDrilldown, financeOverviewMetricTimeScope, formatFinanceOverviewValue } from '../src/features/account-platform/admin-finance-overview.js';

test('finance overview drills down only with filters that exactly reproduce the metric scope', () => {
  assert.deepEqual(financeOverviewDrilldown('accounts', 'pending_adjustment_count'), {
    tab: 'adjustments', query: { status: 'PENDING_APPROVAL' }, exact: true,
  });
  assert.deepEqual(financeOverviewDrilldown('payments', 'payment_anomaly_open_count'), {
    tab: 'anomalies', query: { status: 'OPEN' }, exact: true,
  });
  assert.deepEqual(financeOverviewDrilldown('products', 'product_deleted_count'), {
    tab: 'catalog', query: { product_lifecycle_status: 'DELETED', product_include_deleted: 'true' }, exact: true,
  });
});

test('compound and point-in-time metrics use an explicitly unfiltered related destination', () => {
  assert.deepEqual(financeOverviewDrilldown('payments', 'orders_created_count'), { tab: 'orders', query: {}, exact: false });
  assert.deepEqual(financeOverviewDrilldown('accounts', 'at_risk_hold_count'), { tab: 'holds', query: {}, exact: false });
  assert.deepEqual(financeOverviewDrilldown('products', 'admission_blocked_count'), { tab: 'catalog', query: {}, exact: false });
  assert.deepEqual(financeOverviewDrilldown('pricing', 'price_active_count'), { tab: 'prices', query: {}, exact: false });
  assert.equal(financeOverviewDrilldown('accounts', 'unknown_metric'), null);
});

test('finance overview values honor the unit supplied by the server contract', () => {
  const formatters = {
    formatCount: (value) => `count:${value}`,
    formatPoints: (value) => `points:${value}`,
    formatMoney: (value) => `money:${value}`,
  };
  assert.equal(formatFinanceOverviewValue({ value: 7, unit: 'COUNT' }, formatters), 'count:7');
  assert.equal(formatFinanceOverviewValue({ value: 800000, unit: 'MICROCREDITS' }, formatters), 'points:800000');
  assert.equal(formatFinanceOverviewValue({ value: 990, unit: 'CNY_FEN' }, formatters), 'money:990');
});

test('only transaction-flow metrics use the payment window; operational queues remain point-in-time', () => {
  assert.equal(financeOverviewMetricTimeScope('payments', 'orders_created_count'), 'window');
  assert.equal(financeOverviewMetricTimeScope('payments', 'payments_received_fen'), 'window');
  assert.equal(financeOverviewMetricTimeScope('payments', 'refund_attention_count'), 'point');
  assert.equal(financeOverviewMetricTimeScope('accounts', 'wallet_count'), 'point');
});
