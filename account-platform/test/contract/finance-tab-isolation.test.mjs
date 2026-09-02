import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('finance child tabs load, cache, fail, and refresh independently', async () => {
  const [view, finance, built, products] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-finance.js'),
    read('internal/portal/static/assets/admin.js'),
    read('internal/portal/static/assets/product-admin.js'),
  ]);

  assert.match(view, /target === 'billing' \|\| target === 'payments' \|\| target === 'products'[\s\S]*?next = \{\};/);
  assert.match(view, /const loadTab = useCallback/);
  for (const tab of ['wallets', 'receivables', 'adjustments', 'holds', 'orders', 'refunds', 'lateReviews', 'anomalies', 'reconciliations', 'controls', 'prices']) {
    assert.match(view, new RegExp(`targetTab === '${tab}'`));
  }
  assert.match(view, /financeTabStates\?\.\[surface\]\?\.\[tab\]/);
  assert.match(view, /financeTabShouldLoad\(activeTabState, desiredQuery\)/);
  assert.match(view, /requestedQuery: querySnapshot/);
  assert.match(view, /loadTab\(tab, routeAware \? \{ query: normalizeAdminFinanceQuery\(tab, routeQuery\) \} : undefined\)/);
  assert.match(view, /activeTabState\.error/);
  assert.match(view, /activeTabState\.loading && !activeTabState\.loaded/);
  assert.match(view, /activeTabState\.loaded &&/);
  assert.match(view, /financeRequestTrackerRef/);
  assert.match(view, /requestTracker\.invalidatePrefix/);
  assert.match(view, /next\.adjustmentWallets = wallets\.items/);
  assert.doesNotMatch(view, /next\.wallets = wallets\.items/);
  assert.match(view, /requestKey = 'payments:capabilities'/);
  assert.match(view, /paymentCapabilityState\?\.loaded === true/);
  assert.match(view, /paymentCapabilityReady \? data\.paymentCapabilities \|\| \{\} : \{\}/);
  assert.doesNotMatch(view, /billingOverviewUnavailable/);
  assert.doesNotMatch(view, /billingOverview: null/);
  assert.match(view, /onReload=\{\(\) => loadTab\('controls'/);
  assert.doesNotMatch(view, /const \[overview, wallets, receivables, adjustments, holds/);
  assert.doesNotMatch(view, /const \[overview, orders, refunds, lateReviews, anomalies, reconciliations/);
  assert.match(finance, /updateFinanceTabState/);
  assert.match(finance, /readFinanceTabQuery/);
  assert.match(view, /const querySnapshot =/);
  assert.match(view, /query: querySnapshot, page: \{ cursor: null \}/);
  assert.match(finance, /createFinanceTabRequestTracker/);
  assert.match(finance, /resetFinanceSurfaceLoading/);
  assert.match(finance, /updatePaymentCapabilityState/);
  assert.match(built, /financeTabStates/);
  assert.match(built, /FINANCE_TAB_NOT_AUTHORIZED/);
  assert.match(view, /pinvou-admin-extension-visibility/);
  assert.match(view, /authorizationVersion/);
  assert.match(products, /root\.addEventListener\("pinvou-admin-extension-visibility"/);
  assert.match(products, /activationPromise/);
  assert.match(products, /loadGeneration/);
  assert.match(products, /generation!==loadGeneration/);
  assert.match(products, /catch\(error\)\{if\(generation!==loadGeneration\)return false;loadMessage/);
  assert.match(products, /id="product-load-message"/);
  assert.match(products, /loaded=true;loadMessage\(""\);syncQueryUI\(\);return true/);
  assert.match(products, /version!==activeAuthorizationVersion/);
  assert.match(products, /globalThis\.location\.reload\(\)/);
  assert.doesNotMatch(products, /\(async\(\)=>\{try\{const session=/);
});
