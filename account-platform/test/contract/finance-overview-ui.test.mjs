import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (path) => readFile(new URL(path, import.meta.url), 'utf8');

test('finance overview UI consumes the four atomic aggregates without scanning collection pages', async () => {
  const [client, view, helper] = await Promise.all([
    read('../../../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../../../pinvou3-app/src/features/account-platform/admin-finance-overview.js'),
  ]);
  for (const resource of ['accounts', 'payments', 'pricing', 'products']) {
    assert.ok(client.includes(`/admin/v1/finance-overviews/${resource}`));
  }
  assert.match(view, /function FinanceOverviewBlock/);
  assert.match(view, /error\?\.status === 403/);
  assert.match(view, /if \(state\.forbidden\) return null/);
  assert.match(view, /surface="pricing" load=\{pricingLoad\}/);
  assert.match(view, /surface="products" load=\{productsLoad\}/);
  assert.match(view, /client\.financePaymentsOverview\(\{\}, signal\)/);
  assert.doesNotMatch(view, /reduce\([^\n]*(wallets|orders|refunds|prices)/);
  assert.match(helper, /pending_adjustment_count: route\('adjustments', \{ status: 'PENDING_APPROVAL' \}, true\)/);
  assert.match(helper, /payment_anomaly_open_count: route\('anomalies', \{ status: 'OPEN' \}, true\)/);
  assert.match(helper, /admission_blocked_count: route\('catalog'\)/);
});

test('overview presentation exposes unit, scope, time, accessible drilldown, isolated retry, and responsive layout', async () => {
  const [view, copy, styles, builtJS, builtCSS, runtimeSmoke, packageJSON, smokeSelector] = await Promise.all([
    read('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../../../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('../../../pinvou3-app/src/features/account-platform/admin-platform.css'),
    read('../../internal/portal/static/assets/admin.js'),
    read('../../internal/portal/static/assets/admin.css'),
    read('../../../pinvou3-app/tests/admin_finance_overview_ui_smoke.mjs'),
    read('../../../pinvou3-app/package.json'),
    read('../../../scripts/select-frontend-smokes.mjs'),
  ]);
  assert.match(view, /labels\.units\[metric\.unit\]/);
  assert.match(view, /metric\.included_statuses/);
  assert.match(view, /overview\.window/);
  assert.match(view, /overview\.as_of/);
  assert.match(view, /financeOverviewMetricTimeScope\(surface, metric\.key\)/);
  assert.match(view, /labels\.window\(formatUTCDate\(overview\.window\.from\), formatUTCDate\(overview\.window\.to\)/);
  assert.match(view, /<button type="button" className="adm-finance-overview-card"/);
  assert.match(view, /onClick=\{\(\) => void reload\(\)\}/);
  assert.match(view, /state\.data/);
  assert.match(view, /data: null, error: \{/);
  assert.match(view, /candidate && canDrilldown\(candidate\)/);
  assert.match(view, /request_id=\$\{state\.error\.requestID\}/);
  assert.doesNotMatch(view, /state\.error\.message \?/);
  assert.match(view, /labels\.descriptions\[metric\.key\]/);
  assert.equal((copy.match(/pointInTime: \(asOf\)/g) || []).length, 3);
  assert.match(styles, /\.adm-finance-overview-grid\{display:grid;grid-template-columns:repeat\(4/);
  assert.match(styles, /@media\(max-width:760px\)[\s\S]*\.adm-finance-overview-grid\{grid-template-columns:1fr\}/);
  for (const behavior of ['products-partial', 'products-ok-pricing-forbidden', 'billing-race', 'billing-delay', 'delayedAccountRequestAborted']) assert.ok(runtimeSmoke.includes(behavior));
  assert.equal(JSON.parse(packageJSON).scripts['test:admin-finance-overview-ui'], 'node tests/admin_finance_overview_ui_smoke.mjs');
  assert.match(smokeSelector, /command\("npm", "test:admin-finance-overview-ui"\)/);
  assert.match(smokeSelector, /\["account-platform", \[[\s\S]*command\("npm", "test:admin-finance-overview-ui"\)/);
  for (const marker of ['finance-overviews/accounts', 'finance-overviews/payments', 'finance-overviews/pricing', 'finance-overviews/products', 'adm-finance-overview-card']) {
    assert.ok(builtJS.includes(marker) || builtCSS.includes(marker), `built portal contains ${marker}`);
  }
  assert.match(builtJS, /timeZone:`UTC`/);
});
