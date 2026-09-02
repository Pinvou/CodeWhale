import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const allPermissions = ['platform.billing.wallets.read', 'platform.usage.read', 'platform.pricing.read', 'platform.products.read'];
let permissions = [...allPermissions];
let mode = 'products-partial';
let raceCalls = 0;
let delayedAccountRequestAborted = false;

const overview = (metrics, window = null) => ({ as_of: '2026-09-02T12:34:56Z', window, metrics });
const metric = (key, value, unit = 'COUNT', includedStatuses = ['ACTIVE']) => ({ key, value, unit, included_statuses: includedStatuses });
const accountOverview = (value) => overview([
  metric('wallet_count', value, 'COUNT', ['ACTIVE', 'FROZEN', 'CLOSED']),
  metric('posted_microcredits', 1000000, 'MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']),
  metric('reserved_microcredits', 200000, 'MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']),
  metric('available_microcredits', 700000, 'MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']),
  metric('receivable_microcredits', 100000, 'MICROCREDITS', ['ACTIVE', 'FROZEN', 'CLOSED']),
  metric('pending_adjustment_count', 2, 'COUNT', ['PENDING_APPROVAL']),
  metric('at_risk_hold_count', 3, 'COUNT', ['ACTIVE', 'REVIEW']),
]);
const paymentsOverview = overview([
  metric('orders_created_count', 4, 'COUNT', ['CREATED']), metric('payments_received_fen', 1250, 'CNY_FEN', ['PAID', 'LATE_PAID']),
  metric('late_review_open_count', 1), metric('payment_anomaly_open_count', 2, 'COUNT', ['OPEN']), metric('reconciliation_open_count', 3),
  metric('refund_attention_count', 1), metric('provider_controlled_count', 0), metric('product_controlled_count', 0),
], { from: '2026-09-01T12:34:56Z', to: '2026-09-02T12:34:56Z', timezone: 'UTC' });
const pricingOverview = overview([
  metric('price_draft_count', 1, 'COUNT', ['DRAFT']), metric('price_future_count', 2, 'COUNT', ['PUBLISHED']), metric('price_active_count', 3),
  metric('price_ended_or_cancelled_count', 4), metric('metering_active_count', 5), metric('metering_inactive_count', 6, 'COUNT', ['INACTIVE']),
]);
const productsOverview = overview([
  metric('product_draft_count', 7, 'COUNT', ['DRAFT']), metric('product_on_sale_count', 8, 'COUNT', ['ON_SALE']), metric('product_off_sale_count', 9, 'COUNT', ['OFF_SALE']),
  metric('product_deleted_count', 10, 'COUNT', ['DELETED']), metric('admission_pending_count', 11, 'COUNT', ['PENDING']), metric('admission_blocked_count', 12, 'COUNT', ['MISSING', 'FAILED', 'EXPIRED', 'REVOKED']),
]);

function json(response, payload, status = 200) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/mode') { mode = url.searchParams.get('value') || mode; raceCalls = 0; return json(response, { mode }); }
  if (url.pathname === '/__test__/permissions') { permissions = url.searchParams.get('pricing') === '0' ? allPermissions.filter((value) => value !== 'platform.pricing.read') : [...allPermissions]; return json(response, { permissions }); }
  if (url.pathname === '/__test__/state') return json(response, { delayedAccountRequestAborted, raceCalls });
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'test-csrf' });
  if (url.pathname === '/api/account/v1/account/bootstrap' || url.pathname === '/api/account/v1/me/authorization-context') return json(response, { user_id: '11111111-1111-4111-8111-111111111111', platform_permissions: permissions });
  if (url.pathname.endsWith('/finance-overviews/accounts')) {
    if (mode === 'billing-race') {
      raceCalls += 1;
      if (raceCalls === 1) return setTimeout(() => json(response, accountOverview(111)), 250);
      return json(response, accountOverview(222));
    }
    if (mode === 'billing-delay') {
      request.on('aborted', () => { delayedAccountRequestAborted = true; });
      response.on('close', () => { if (!response.writableEnded) delayedAccountRequestAborted = true; });
      return setTimeout(() => json(response, accountOverview(333)), 500);
    }
    return json(response, accountOverview(1));
  }
  if (url.pathname.endsWith('/finance-overviews/payments')) return json(response, paymentsOverview);
  if (url.pathname.endsWith('/finance-overviews/pricing')) {
    if (mode === 'products-ok-pricing-forbidden') return json(response, { code: 'FORBIDDEN', message: 'forbidden' }, 403);
    return json(response, pricingOverview);
  }
  if (url.pathname.endsWith('/finance-overviews/products')) {
    if (mode === 'products-partial') return json(response, { code: 'FINANCE_OVERVIEW_UNAVAILABLE', message: 'finance overview is temporarily unavailable', request_id: 'request-products' }, 503);
    return json(response, productsOverview);
  }
  if (url.pathname.endsWith('/admin/v1/overview')) return json(response, { payment_capabilities: {} });
  if (url.pathname.endsWith('/admin/v1/wallets') || url.pathname.endsWith('/admin/v1/payment-orders')) return json(response, { items: [], next_cursor: '', snapshot_as_of: '2026-09-02T12:34:56Z' });
  if (url.pathname.endsWith('/admin/v1/pricing')) return json(response, { items: [] });
  if (url.pathname.endsWith('/admin/v1/metering-units')) return json(response, { items: [] });
  if (url.pathname.endsWith('/admin/v1/protocols')) return json(response, { protocols: [], formulas: [] });
  if (url.pathname.endsWith('/admin/v1/recharge-products')) return json(response, { items: [], next_cursor: '' });
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); return response.end('not found'); }
  const type = path.extname(target) === '.html' ? 'text/html' : path.extname(target) === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8`, 'Cache-Control': 'no-store' });
  response.end(await readFile(target));
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'zh'));

  await page.goto(`http://127.0.0.1:${port}/admin.html?section=products&finance_tab=catalog`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-finance-overview-state.error');
  assert.equal(await page.$$eval('.adm-finance-overview', (nodes) => nodes.length), 2, 'pricing and product blocks render independently');
  assert.match(await page.$eval('.adm-finance-overview-state.error', (node) => node.textContent), /FINANCE_OVERVIEW_UNAVAILABLE/);
  assert.doesNotMatch(await page.$eval('.adm-finance-overview-state.error', (node) => node.textContent), /finance overview is temporarily unavailable/);
  assert.equal(await page.$$eval('.adm-finance-overview-card', (nodes) => nodes.length), 6, 'successful pricing metrics survive product failure');

  await page.evaluate(() => fetch('/__test__/mode?value=products-ok-pricing-forbidden'));
  await page.click('.adm-finance-overview-state.error button');
  await page.waitForFunction(() => document.querySelectorAll('.adm-finance-overview-card').length === 12);
  await page.click('.adm-finance-overview:first-child > header > button');
  await page.waitForFunction(() => document.querySelectorAll('.adm-finance-overview').length === 1);
  assert.match(await page.$eval('.adm-finance-overview h2', (node) => node.textContent), /充值商品/);

  await page.evaluate(() => fetch('/__test__/permissions?pricing=0').then(() => window.dispatchEvent(new Event('focus'))));
  await new Promise((resolve) => setTimeout(resolve, 100));
  await page.evaluate(() => fetch('/__test__/mode?value=products-both-ok'));
  await page.evaluate(() => fetch('/__test__/permissions?pricing=1').then(() => window.dispatchEvent(new Event('focus'))));
  await page.waitForFunction(() => document.querySelectorAll('.adm-finance-overview').length === 2);

  await page.evaluate(() => fetch('/__test__/mode?value=billing-initial'));
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=billing&finance_tab=wallets`, { waitUntil: 'networkidle0' });
  await page.waitForFunction(() => document.querySelector('.adm-finance-overview-card strong')?.textContent.trim() === '1');
  await page.evaluate(() => fetch('/__test__/mode?value=billing-race'));
  await page.click('.adm-finance-overview > header > button');
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.raceCalls === 1));
  await page.click('.adm-finance-overview > header > button');
  await page.waitForFunction(() => document.querySelector('.adm-finance-overview-card strong')?.textContent.trim() === '222');

  await page.evaluate(() => fetch('/__test__/mode?value=billing-delay'));
  await page.click('.adm-finance-overview > header > button');
  await new Promise((resolve) => setTimeout(resolve, 40));
  await page.evaluate(() => [...document.querySelectorAll('.adm-sidebar nav button')].find((button) => button.textContent.includes('支付与对账')).click());
  await page.waitForFunction(() => document.querySelector('.adm-finance-overview h2')?.textContent.includes('支付'));
  await page.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((state) => state.delayedAccountRequestAborted));

  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(overflow <= 1, `mobile finance overview must not overflow horizontally: ${overflow}`);
  assert.deepEqual(pageErrors, []);
  console.log('Admin finance overview UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
