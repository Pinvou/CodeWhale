import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';

const root = path.resolve(import.meta.dirname, '..');
const chrome = [process.env.CHROME, 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium required; validation cannot be skipped');
const server = await createServer({ root, configFile: false, logLevel: 'error', plugins: [react()], css: { postcss: { plugins: [] } }, server: { host: '127.0.0.1', port: 0 } });
await server.listen();
let browser, passed = 0, skipped = 0;
try {
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(), errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
  const fresh = async (query = '') => {
    errors.length = 0;
    await page.setViewport({ width: 1440, height: 1000 });
    await page.goto(`${origin}/tests/fixtures/account-platform-restored-ui.html${query}`, { waitUntil: 'domcontentloaded', timeout: 60000 });
    await page.waitForSelector(query.includes('mode=admin') ? '.adm-restored-shell .adm-page-head' : '.ap-restored-portal .ap-product-picker');
  };
  const click = async (selector, text) => {
    const handle = await page.evaluateHandle(({ selector, text }) => {
      const button = [...document.querySelectorAll(selector)].find(node => node.textContent.trim() === text && node.getClientRects().length);
      return button && !button.disabled ? button : null;
    }, { selector, text });
    const button = handle.asElement();
    assert.ok(button, `visible enabled button missing: ${selector} ${text}`);
    await button.click(); await handle.dispose();
  };
  const accountTab = section => page.click(`.ap-sidebar [data-section="${section}"]`);
  const adminTab = key => page.click(`.adm-sidebar button[data-section="${key}"]`);
  const activeAccount = section => page.waitForSelector(`#ap-panel-${section}:not([hidden])`);
  const noOverflow = async () => {
    const result = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth, offenders: [...document.querySelectorAll('body *')].filter(node => node.getBoundingClientRect().right > innerWidth + 1 && getComputedStyle(node).position !== 'fixed').slice(0, 8).map(node => node.tagName + '.' + node.className) }));
    assert.ok(result.scroll <= result.width + 1, `page has horizontal overflow: ${JSON.stringify(result)}`);
  };
  const run = async (name, action) => {
    if (process.env.UI_CASE && !name.includes(process.env.UI_CASE)) { skipped++; return; }
    try { await action(); assert.deepEqual(errors, [], 'uncaught UI error'); console.log(`PASS ${name}`); passed++; }
    catch (error) { console.error(JSON.stringify({ case: name, pageErrors: errors, dialog: await page.evaluate(() => document.querySelector('[role=dialog]')?.textContent.slice(0, 1200)) })); throw error; }
  };
  await run('personal original tabs, five benefits, exact balance and history navigation', async () => {
    await fresh('?section=usage');
    await activeAccount('creditsUsage');
    assert.equal(await page.$$eval('.ap-sidebar [role=tab]', nodes => nodes.length), 3);
    assert.equal(await page.$$eval('#ap-panel-creditsUsage .ap-benefit-card', nodes => nodes.length), 5);
    assert.equal(await page.$eval('.ap-balance-hero>strong', node => node.textContent), '922,337,203,685,477');
    await accountTab('ordersInvoices'); await activeAccount('ordersInvoices');
    assert.equal(await page.$$eval('.ap-order-history tbody tr', nodes => nodes.length), 6);
    await page.goBack(); await activeAccount('creditsUsage'); await page.goForward(); await activeAccount('ordersInvoices');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'URL navigation wrote business data');
  });
  await run('personal unknown recharge survives tab changes with the original key and facts', async () => {
    await fresh(); await page.evaluate(() => { window.restored.unknown = true; });
    await click('.ap-recharge-form button', await page.evaluate(() => window.restored.copy().finance.create));
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')) && document.querySelector('.ap-finance-feedback button'));
    await accountTab('ordersInvoices'); await accountTab('accountSettings'); await accountTab('creditsUsage');
    await click('.ap-finance-feedback button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('profile deep link and field dialogs retain keyboard and return focus', async () => {
    await fresh('?section=profile&profile=username');
    await page.waitForSelector('[role=dialog] input[autocomplete=username]');
    assert.equal(new URL(page.url()).searchParams.get('section'), 'accountSettings');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(new URL(page.url()).searchParams.has('profile'), false);
    await click('.ap-profile-rows .ap-profile-row:first-of-type button', await page.evaluate(() => window.restored.copy().accountLayout.edit));
    await page.waitForSelector('[role=dialog] input[autocomplete=username]');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    await page.waitForFunction(() => document.activeElement.closest('.ap-profile-row'));
    assert.ok(await page.evaluate(() => Boolean(document.activeElement.closest('.ap-profile-row'))), `focus did not return to edit trigger: ${await page.evaluate(() => document.activeElement.outerHTML.slice(0, 300))}`);
  });
  await run('admin original groups, canonical aliases, and back/forward', async () => {
    await fresh('?mode=admin&section=users');
    await page.waitForSelector('tbody tr');
    assert.equal(await page.$$eval('.adm-nav-group', nodes => nodes.length), 5);
    await adminTab('payments'); await page.waitForSelector('[data-workspace=orders]:not([hidden])');
    await page.goBack(); await page.waitForSelector('[data-workspace=users]:not([hidden])');
    await page.goForward(); await page.waitForSelector('[data-workspace=finance]:not([hidden])');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('admin unknown adjustment keeps the leaf owner across inner and outer navigation', async () => {
    await fresh('?mode=admin&section=billing&finance_tab=wallets');
    const form = '[data-workspace=wallets] form:not(.adm-filter-row)'; await page.waitForSelector(form);
    await page.type(`${form} input:first-of-type`, '11111111-1111-4111-8111-111111111111');
    const inputs = await page.$$(`${form} input`); await inputs[1].type('10000');
    await page.type(`${form} textarea`, 'Controlled adjustment reason');
    await page.$eval(form, node => node.requestSubmit());
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/adjustments')));
    await adminTab('payments'); await adminTab('users'); await adminTab('billing');
    await click('[data-workspace=finance] .adm-tabs button', await page.evaluate(() => window.restored.copy().finance.wallet));
    await page.waitForFunction(() => [...document.querySelectorAll('[data-workspace=wallets] button')].some(node => node.textContent.trim() === window.restored.copy().finance.retry && !node.disabled));
    await click('[data-workspace=wallets] button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/adjustments')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/adjustments')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('model IA gate, permission denial, and missing data never fabricates counts', async () => {
    await fresh('?mode=admin&section=overview');
    await page.evaluate(() => { window.restored.flags.admin_model_token_ia = true; window.restored.permissions = ['platform.tokens.read']; window.restored.calls.length = 0; window.restored.reload(); });
    await page.waitForFunction(() => document.querySelectorAll('.adm-sidebar nav button').length === 3);
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.url.includes('/admin/account/') || call.url.includes('/admin/billing/'))));
    await page.evaluate(() => { history.pushState({}, '', '?mode=admin&section=users'); dispatchEvent(new PopStateEvent('popstate')); });
    await page.waitForFunction(() => document.querySelector('.adm-page [role=alert]')?.textContent === window.restored.copy().adminLayout.unauthorized);
    assert.ok(await page.evaluate(() => !document.querySelector('[data-workspace=users]') && !window.restored.calls.some(call => call.url.includes('/admin/v1/users'))));
  });
  await run('three overview domains fail independently and keep exact monetary digits', async () => {
    await fresh('?mode=admin&section=overview');
    await page.waitForFunction(() => document.querySelectorAll('[data-overview-domain] dl').length === 3);
    assert.ok(await page.$eval('[data-overview-domain=billing]', node => node.textContent.includes('900,719,925,474.0993')));
    await page.evaluate(() => { window.restored.failDomain = 'account'; window.restored.reload(); });
    await page.waitForSelector('[data-overview-domain=account] [role=alert]');
    assert.equal(await page.$$eval('[data-overview-domain=account] dl', nodes => nodes.length), 0);
    await page.waitForFunction(() => document.querySelectorAll('[data-overview-domain=billing] dl,[data-overview-domain=token] dl').length === 2);
  });
  await run('user detail deep links survive reload and current administrator cannot self-disable', async () => {
    await fresh('?mode=admin&section=users&user_q=sample');
    await page.waitForSelector('[data-workspace=users] tbody tr');
    assert.equal(await page.$eval('[data-workspace=users] tbody tr td:last-child button', node => node.disabled), true);
    await page.click('[data-workspace=users] .adm-name-link');
    await page.waitForSelector('.adm-user-details');
    assert.equal(new URL(page.url()).searchParams.get('user_id'), '11111111-1111-4111-8111-111111111111');
    await page.reload(); await page.waitForSelector('.adm-user-details');
    assert.ok(await page.$eval('.adm-user-details', node => !node.textContent.includes('@')));
    await click('[data-workspace=users] button', await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
    await page.waitForSelector('[data-workspace=users] tbody tr');
    assert.equal(new URL(page.url()).searchParams.get('user_q'), 'sample');
  });
  await run('organization detail has four original URL sections, server member search and directory return', async () => {
    await fresh('?mode=admin&section=organizations&organization_q=sample');
    await page.waitForSelector('.adm-organization-directory .adm-name-link'); await page.click('.adm-organization-directory .adm-name-link');
    await page.waitForSelector('.adm-organization-header');
    assert.equal(await page.$$eval('.adm-organization-directory .adm-tabs button', nodes => nodes.length), 4);
    await page.reload(); await page.waitForSelector('.adm-organization-header');
    await click('.adm-organization-directory .adm-tabs button', await page.evaluate(() => window.restored.copy().adminLayout.organization.members));
    await page.waitForSelector('.adm-organization-directory [data-workspace=members] tbody tr');
    await page.type('.adm-organization-directory [data-workspace=members] form input', 'remote member');
    await page.$eval('.adm-organization-directory [data-workspace=members] form', form => form.requestSubmit());
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/members?') && call.url.includes('q=remote+member')));
    await page.waitForFunction(() => document.querySelector('.adm-organization-directory [data-workspace=members] tbody')?.textContent.includes('remote member'));
    await click('.adm-organization-directory>button', await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
    await page.waitForSelector('.adm-organization-directory .adm-name-link');
    assert.equal(new URL(page.url()).searchParams.get('organization_q'), 'sample');
  });
  await run('organization deletion remains disabled for active or blocked resources and requires the name', async () => {
    const id = '33333333-3333-4333-8333-333333333333';
    await fresh(`?mode=admin&section=organizations&organization_id=${id}&organization_tab=lifecycle`);
    await page.waitForSelector('.adm-organization-header');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.url.endsWith('/deletion-readiness'))));
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === window.restored.copy().organization.delete)));
    await page.evaluate(() => { window.restored.orgStatus = 'DISABLED'; window.restored.reload(); });
    await page.waitForFunction(() => document.querySelector('.adm-organization-directory')?.textContent.includes('ACTIVE_MEMBERS'));
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === window.restored.copy().organization.delete)));
    await page.evaluate(() => { window.restored.orgReady = true; window.restored.reload(); });
    await page.waitForFunction(() => [...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === window.restored.copy().organization.delete));
    await click('.adm-organization-directory button', await page.evaluate(() => window.restored.copy().organization.delete));
    await page.waitForSelector('[role=dialog] form.ap-org-action');
    const fields = await page.$$('[role=dialog] fieldset input');
    await fields[0].type('wrong name'); await fields[1].type('Controlled deletion reason');
    await page.click('[role=dialog] form>button[type=submit]');
    await page.waitForSelector('[role=dialog] form [role=alert]');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.method === 'DELETE')));
    await fields[0].focus(); await page.keyboard.down('Control'); await page.keyboard.press('A'); await page.keyboard.up('Control'); await page.keyboard.press('Backspace'); await fields[0].type('Sample organization');
    assert.deepEqual(await page.$$eval('[role=dialog] fieldset input', nodes => nodes.map(node => node.value)), ['Sample organization', 'Controlled deletion reason']);
    await page.click('[role=dialog] form>button[type=submit]');
    await page.waitForSelector('[role=alertdialog]');
    await page.keyboard.press('Escape');
    await page.waitForSelector('[role=dialog] form.ap-org-action');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.method === 'DELETE')));
    await page.keyboard.press('Escape');
  });
  await run('organization creation searches users and keeps the original unknown invocation in its modal', async () => {
    await fresh('?mode=admin&section=organizations');
    await page.evaluate(() => { window.restored.unknownOrgCreate = true; });
    await click('.adm-organization-directory button', await page.evaluate(() => window.restored.copy().organization.create));
    await page.waitForSelector('[role=dialog] .adm-user-picker');
    await page.type('[role=dialog] fieldset>label:first-child input', 'Controlled organization');
    await page.type('.adm-user-picker input', 'remote');
    await click('.adm-user-picker button', await page.evaluate(() => window.restored.copy().organization.search));
    await page.waitForFunction(() => document.querySelector('.adm-user-picker select option[value="22222222-2222-4222-8222-222222222222"]'));
    await page.select('.adm-user-picker select', '22222222-2222-4222-8222-222222222222');
    await page.click('[role=dialog] form>button[type=submit]');
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')) && [...document.querySelectorAll('[role=dialog] form>button')].some(node => node.textContent.trim() === window.restored.copy().organization.retry && !node.disabled));
    await page.keyboard.press('Escape'); assert.ok(await page.$('[role=dialog]'));
    await page.evaluate(() => document.querySelector('.adm-sidebar [data-section=users]').click());
    assert.equal(new URL(page.url()).searchParams.get('section'), 'organizations');
    await click('[role=dialog] form>button', await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('financial read lists retain composite refund identity, filters and exact amounts without writes', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=refunds');
    await page.waitForSelector('[data-workspace=refunds] tbody tr');
    assert.equal(await page.$$eval('[data-workspace=refunds] tbody tr', nodes => nodes.length), 2);
    assert.ok((await page.$eval('[data-workspace=refunds]', node => node.textContent)).includes('90,071,992,547,409.93'));
    const details = await page.$$('[data-workspace=refunds] tbody button'); await details[1].click();
    await page.waitForFunction(() => new URL(location.href).searchParams.get('refund_id')?.startsWith('FORCED:'));
    await page.reload({ waitUntil: 'domcontentloaded' }); await page.waitForSelector('[data-workspace=refunds] dl');
    assert.ok((await page.$eval('[data-workspace=refunds] dl', node => node.textContent)).includes('强制退款'));
    await page.goBack(); await page.waitForSelector('[data-workspace=refunds] form');
    const query = await page.$('[data-workspace=refunds] form input'); await query.type('provider-trace');
    await page.$eval('[data-workspace=refunds] form', node => node.requestSubmit());
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/refunds?') && call.url.includes('q=provider-trace')));
    assert.equal(new URL(page.url()).searchParams.get('refund_q'), 'provider-trace');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('receivable read detail shows original settled outstanding facts and allocations', async () => {
    await fresh('?mode=admin&section=billing&finance_tab=receivables&receivable_id=11111111-1111-4111-8111-111111111111');
    await page.waitForSelector('[data-workspace=receivables] dl');
    const text = await page.$eval('[data-workspace=receivables]', node => node.textContent);
    assert.ok(text.includes('900,719,925,474.0993')); assert.ok(text.includes('900,719,925,473.0993'));
    assert.ok(text.includes('已结清积分')); assert.equal(await page.$$eval('[data-workspace=receivables] tbody tr', nodes => nodes.length), 1);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('profile rejection is visible inside the field modal', async () => {
    await fresh('?section=accountSettings&profile=username'); await page.waitForSelector('[role=dialog] input[autocomplete=username]');
    const input = await page.$('[role=dialog] input[autocomplete=username]'); await input.click(); await input.evaluate(node => node.select()); await input.type('controlled-new-name');
    await page.$eval('[role=dialog] form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('[role=dialog] [role=alert]')?.textContent === window.restored.copy().account.usernameAlreadyRegistered);
  });
  await run('closure authentication return selects visible account settings without a write', async () => {
    await fresh('?account_closure=confirm'); await activeAccount('accountSettings');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('personal order modal keeps history in its originating tab and exposes read failures', async () => {
    await fresh('?section=creditsUsage'); await accountTab('ordersInvoices');
    await page.waitForSelector('.ap-order-history tbody button'); await page.click('.ap-order-history tbody button');
    await page.waitForSelector('[role=dialog]'); await page.goBack();
    await activeAccount('ordersInvoices'); assert.ok(await page.$('[role=dialog]'));
    await page.evaluate(() => { window.restored.orderReadFail = true; });
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.query));
    await page.waitForSelector('[role=dialog] [role=alert]');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    await accountTab('creditsUsage'); assert.equal(await page.$('[role=dialog]'), null);
  });
  await run('personal-only deployments retain user details when organizations are unavailable', async () => {
    await fresh('?mode=admin&section=users'); await page.evaluate(() => { window.restored.failMemberships = true; });
    await page.click('.adm-table .adm-name-link'); await page.waitForSelector('.adm-user-details');
    await page.waitForSelector('[data-workspace=users] [role=alert]');
    assert.ok((await page.$eval('.adm-user-details', node => node.textContent)).includes('11111111-1111-4111-8111-111111111111'));
    await page.evaluate(() => { window.restored.failMemberships = false; });
    await page.click('[data-workspace=users] [role=alert] button');
    await page.waitForFunction(() => !document.querySelector('[data-workspace=users] [role=alert]') && document.querySelector('[data-workspace=users] ul')?.textContent.includes('Sample organization'));
  });
  await run('original payment filters and selected detail survive URL reload', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders&order_status=PAID&order_provider_id=controlled-provider&order_channel=ALIPAY&order_created_from=2026-10-01T00%3A00%3A12.345Z&order_limit=50&order_id=00000000-0000-4000-8000-000000000002');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/payment-orders?') && call.url.includes('status=PAID') && call.url.includes('provider_id=controlled-provider') && call.url.includes('channel=ALIPAY') && call.url.includes('limit=50') && call.url.includes('created_from=')));
    await page.waitForSelector('[data-workspace=orders] article');
    assert.ok((await page.$eval('[data-workspace=orders] article', node => node.textContent)).includes('00000000-0000-4000-8000-000000000002'));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-workspace=orders] article');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/payment-orders?') && call.url.includes('status=PAID')));
    assert.equal(new URL(page.url()).searchParams.get('order_channel'), 'ALIPAY');
  });
  await run('history during a pending filtered read coalesces the latest URL without stale rows', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders&order_status=PENDING');
    await page.evaluate(() => { window.restored.filteredOrders = true; window.restored.reload(); });
    await page.waitForFunction(() => document.querySelector('[data-workspace=orders] tbody')?.textContent.includes('PENDING'));
    await page.waitForFunction(() => !document.querySelector('[data-workspace=orders] .adm-filter-row button').disabled);
    await page.evaluate(() => { window.restored.delayAdminOrders = true; });
    await page.select('[data-workspace=orders] .adm-filter-row select', 'PAID'); await page.$eval('[data-workspace=orders] .adm-filter-row', form => form.requestSubmit());
    await page.waitForFunction(() => typeof window.restored.releaseAdminOrders === 'function');
    const before = await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/payment-orders?') && new URL(call.url, location.origin).searchParams.get('status') === 'PENDING').length);
    await page.goBack(); await page.waitForFunction(() => new URLSearchParams(location.search).get('order_status') === 'PENDING');
    await page.evaluate(() => window.restored.releaseAdminOrders());
    await page.waitForFunction(before => window.restored.calls.filter(call => call.url.includes('/payment-orders?') && new URL(call.url, location.origin).searchParams.get('status') === 'PENDING').length === before + 1 && document.querySelector('[data-workspace=orders] tbody')?.textContent.includes('PENDING'), {}, before);
    assert.ok(!(await page.$eval('[data-workspace=orders] tbody', node => node.textContent)).includes('PAID'));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('review detail failure and mismatched receipt never expose the previously selected case', async () => {
    const first = '22222222-2222-4222-8222-222222222222', second = '33333333-3333-4333-8333-333333333333';
    for (const [tab, prefix, endpoint] of [['late', 'late', 'late-payment-reviews'], ['anomalies', 'anomaly', 'payment-anomalies']]) {
      await fresh(`?mode=admin&section=payments&finance_tab=${tab}`);
      await page.evaluate(({ tab, prefix, first }) => { window.restored.reviewFixture = true; window.restored.permissions = ['platform.billing.wallets.read', 'platform.billing.orders.sync', 'platform.billing.repairs.request']; history.replaceState({}, '', `?mode=admin&section=payments&finance_tab=${tab}&${prefix}_id=${first}`); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); }, { tab, prefix, first });
      await page.waitForFunction(({ tab, first }) => document.querySelector(`[data-workspace=${tab}] article h4`)?.textContent === first, {}, { tab, first });
      await page.evaluate(({ tab, prefix, second }) => { window.restored.failReviewID = second; history.pushState({}, '', `?mode=admin&section=payments&finance_tab=${tab}&${prefix}_id=${second}`); dispatchEvent(new PopStateEvent('popstate')); }, { tab, prefix, second });
      await page.waitForFunction(({ tab, endpoint, second }) => window.restored.calls.some(call => call.url.endsWith('/' + endpoint + '/' + second)) && document.querySelector(`[data-workspace=${tab}] [role=alert]`) && !document.querySelector(`[data-workspace=${tab}] .adm-filter-row button`).disabled, {}, { tab, endpoint, second });
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      await page.evaluate(first => { window.restored.failReviewID = ''; window.restored.reviewAckID = first; }, first);
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().finance.retry));
      await page.waitForFunction(({ tab, endpoint, second }) => window.restored.calls.filter(call => call.url.endsWith('/' + endpoint + '/' + second)).length === 2 && !document.querySelector(`[data-workspace=${tab}] .adm-filter-row button`).disabled, {}, { tab, endpoint, second });
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      await page.evaluate(() => { window.restored.reviewAckID = ''; });
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().finance.retry));
      await page.waitForFunction(({ tab, second }) => document.querySelector(`[data-workspace=${tab}] article h4`)?.textContent === second, {}, { tab, second });
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
      await page.waitForFunction(prefix => !new URLSearchParams(location.search).has(prefix + '_id'), {}, prefix);
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
    }
  });
  await run('unknown command shows its original target and key after history clears the prior detail', async () => {
    const first = '22222222-2222-4222-8222-222222222222', second = '33333333-3333-4333-8333-333333333333';
    await fresh('?mode=admin&section=payments&finance_tab=late');
    await page.evaluate(first => { window.restored.reviewFixture = true; window.restored.permissions = ['platform.billing.wallets.read', 'platform.billing.orders.sync']; history.replaceState({}, '', '?mode=admin&section=payments&finance_tab=late&late_id=' + first); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); }, first);
    await page.waitForFunction(first => document.querySelector('[data-workspace=late] article h4')?.textContent === first, {}, first);
    await click('[data-workspace=late] nav button', await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
    await page.waitForSelector('[data-workspace=late] tbody tr:nth-child(2) button'); await page.click('[data-workspace=late] tbody tr:nth-child(2) button');
    await page.waitForFunction(second => document.querySelector('[data-workspace=late] article h4')?.textContent === second, {}, second);
    await page.type('[data-workspace=late] article textarea', 'Controlled original schedule reason');
    await page.$eval('[data-workspace=late] article input[type=datetime-local]', input => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, '2026-11-01T09:30'); input.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.evaluate(() => { window.restored.delayReviewSchedule = true; }); page.once('dialog', dialog => dialog.accept());
    await click('[data-workspace=late] article button', await page.evaluate(() => window.restored.copy().finance.schedule));
    await page.waitForFunction(() => typeof window.restored.releaseReviewSchedule === 'function'); await page.goBack();
    await page.waitForFunction(() => !new URLSearchParams(location.search).has('late_id')); await page.goBack();
    await page.waitForFunction(first => new URLSearchParams(location.search).get('late_id') === first, {}, first);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.evaluate(() => window.restored.releaseReviewSchedule());
    await page.waitForFunction(second => document.querySelector('[data-workspace=late] [data-retained-finance-command]')?.textContent.includes(second), {}, second);
    const firstWrite = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/schedule')));
    assert.equal(firstWrite.length, 1); assert.ok((await page.$eval('[data-retained-finance-command]', node => node.textContent)).includes(firstWrite[0].key));
    await page.setViewport({ width: 390, height: 1000 }); await noOverflow(); await page.setViewport({ width: 1440, height: 1000 });
    await click('[data-retained-finance-command] button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(first => document.querySelector('[data-workspace=late] article h4')?.textContent === first && !document.querySelector('[data-retained-finance-command]'), {}, first);
    const writes = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/schedule')));
    assert.equal(writes.length, 2); assert.equal(writes[0].url, writes[1].url); assert.equal(writes[0].key, writes[1].key); assert.equal(writes[0].body, writes[1].body); assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
  });
  await run('wallet ledger restores filters, columns and exact signed amounts', async () => {
    await fresh('?mode=admin&section=billing&finance_tab=wallets&wallet_id=11111111-1111-4111-8111-111111111111&ledger_entry_type=ADJUSTMENT&ledger_business_reference=controlled-reference');
    await page.waitForSelector('.adm-wallet-ledger tbody tr');
    assert.ok((await page.$eval('.adm-wallet-ledger', node => node.textContent)).includes('922,337,203,685,477.5807'));
    assert.ok(await page.evaluate(() => window.restored.calls.some(call => call.url.includes('/ledger?') && call.url.includes('entry_type=ADJUSTMENT') && call.url.includes('business_reference=controlled-reference'))));
  });
  await run('all four financial overviews show real time scope and effective drilldown', async () => {
    await fresh('?mode=admin&section=billing'); await page.waitForSelector('[data-finance-overview=accounts] .adm-finance-overview-card');
    assert.ok((await page.$eval('[data-finance-overview=accounts]', node => node.textContent)).includes('922,337,203,685,477.5807'));
    await page.setViewport({ width: 390, height: 1000 }); await noOverflow(); await page.setViewport({ width: 1440, height: 1000 });
    await adminTab('payments'); await page.waitForSelector('[data-finance-overview=payments] .adm-finance-overview-card');
    assert.ok((await page.$eval('[data-finance-overview=payments]', node => node.textContent)).includes('UTC'));
    await page.click('[data-finance-overview=payments] .adm-finance-overview-card');
    await page.waitForFunction(() => new Date(new URL(location.href).searchParams.get('order_created_from')).toISOString() === '2026-10-28T00:00:00.000Z');
    await adminTab('products'); await page.waitForSelector('[data-finance-overview=products] .adm-finance-overview-card');
    await page.evaluate(() => { history.pushState({}, '', '?mode=admin&section=products&finance_tab=catalog&product_q=stale-search&product_include_deleted=true'); dispatchEvent(new PopStateEvent('popstate')); });
    await page.click('[data-finance-overview=products] .adm-finance-overview-card');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/recharge-products?') && call.url.includes('lifecycle_status=ON_SALE')));
    assert.equal(new URL(page.url()).searchParams.has('product_q'), false); assert.equal(new URL(page.url()).searchParams.has('product_include_deleted'), false);
    const priceLabel = await page.evaluate(() => window.restored.copy().adminLayout.nav.products);
    assert.ok(priceLabel); await page.evaluate(() => { history.pushState({}, '', '?mode=admin&section=products&finance_tab=prices'); dispatchEvent(new PopStateEvent('popstate')); });
    await page.waitForSelector('[data-finance-overview=pricing] .adm-finance-overview-card');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('late accepted recharge stays with its origin tab and retains its one order', async () => {
    await fresh('?section=creditsUsage'); await page.evaluate(() => { window.restored.delayCreate = true; });
    await click('.ap-recharge-form button', await page.evaluate(() => window.restored.copy().finance.create));
    await page.waitForFunction(() => typeof window.restored.releaseCreate === 'function'); await accountTab('accountSettings');
    await page.evaluate(() => window.restored.releaseCreate()); await page.waitForFunction(() => !document.querySelector('.ap-recharge-form button').disabled);
    assert.equal(await page.$('.ap-payment-dialog'), null); await activeAccount('accountSettings');
    await accountTab('creditsUsage'); await page.waitForSelector('.ap-payment-dialog');
    assert.equal(await page.$eval('.ap-payment-dialog [role=status]', node => node.textContent), await page.evaluate(() => window.restored.copy().finance.saved));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')).length), 1);
  });
  await run('submitting displayed date filters sends timezone-bearing RFC3339', async () => {
    for (const [section, tab, prefix, endpoint] of [['payments', 'orders', 'order', 'payment-orders'], ['payments', 'late', 'late', 'late-payment-reviews'], ['payments', 'anomalies', 'anomaly', 'payment-anomalies']]) {
      await fresh(`?mode=admin&section=${section}&finance_tab=${tab}&${prefix}_created_from=2026-10-01T00%3A00%3A12.345Z`);
      await page.waitForSelector(`[data-workspace=${tab}] .adm-filter-row input[type=datetime-local]`);
      await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, `[data-workspace=${tab}] .adm-filter-row button`);
      const before = await page.evaluate(endpoint => window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).length, endpoint);
      await page.$eval(`[data-workspace=${tab}] .adm-filter-row`, form => form.requestSubmit());
      await page.waitForFunction(({ endpoint, before }) => window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).length > before, {}, { endpoint, before });
      const value = await page.evaluate(endpoint => new URL(window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).at(-1).url, location.origin).searchParams.get('created_from'), endpoint);
      assert.match(value, /Z$/); assert.equal(value, '2026-10-01T00:00:12.345Z');
      assert.equal(await page.$(`[data-workspace=${tab}] [role=alert]`), null);
    }
  });
  await run('product unknown modal exposes only explicit recovery of the original command', async () => {
    await fresh('?mode=admin&section=products&finance_tab=catalog');
    await page.evaluate(() => { window.restored.useCatalogFixture = true; window.restored.permissions = ['platform.products.read', 'platform.products.publish']; window.restored.reload(); });
    await page.waitForSelector('[data-workspace=catalog] tbody button'); await page.click('[data-workspace=catalog] tbody button'); await page.waitForSelector('[role=dialog] textarea');
    await page.type('[role=dialog] textarea', 'Controlled display-name correction'); page.once('dialog', dialog => dialog.accept());
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.saveNames));
    await page.waitForFunction(() => [...document.querySelectorAll('[role=dialog] button')].some(node => node.textContent.trim() === window.restored.copy().finance.retry && !node.disabled));
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/display-names')));
    assert.equal(calls.length, 2); assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].body, calls[1].body);
    assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
  });
  await run('product creation uses the restored modal and current typed command', async () => {
    await fresh('?mode=admin&section=products&finance_tab=catalog');
    await page.evaluate(() => { window.restored.useCatalogFixture = true; window.restored.permissions = ['platform.products.read', 'platform.products.create']; window.restored.reload(); });
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.endsWith('/recharge-product-create-options')));
    await click('[data-workspace=catalog] button', await page.evaluate(() => window.restored.copy().finance.createProduct));
    await page.waitForSelector('[role=dialog] form');
    for (const [key, value] of [['productID', 'controlled-new'], ['nameZh', '测试商品'], ['nameEn', 'Controlled new'], ['nameJa', 'テスト商品'], ['base', '100'], ['validityDays', '30'], ['sku', 'CONTROLLED_NEW'], ['amountFen', '100']]) {
      const input = await page.evaluateHandle(key => [...document.querySelectorAll('[role=dialog] label')].find(node => node.firstChild?.textContent === window.restored.copy().finance[key])?.querySelector('input'), key);
      assert.ok(input.asElement(), key); await input.asElement().type(value); await input.dispose();
    }
    await page.select('[role=dialog] select', 'Service');
    const provider = await page.evaluateHandle(() => [...document.querySelectorAll('[role=dialog] label')].find(node => node.firstChild?.textContent === window.restored.copy().finance.provider)?.querySelector('select'));
    await provider.asElement().select('controlled/1'); await provider.dispose();
    await page.click('[role=dialog] input[type=checkbox]'); await page.type('[role=dialog] textarea', 'Controlled create');
    page.once('dialog', dialog => dialog.accept()); await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.createProduct));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && window.restored.catalogRecord?.lifecycle_status === 'DRAFT');
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge-products')));
    assert.equal(calls.length, 1); const body = JSON.parse(calls[0].body); assert.equal(body.product_id, 'controlled-new'); assert.equal(body.mapping.provider_id, 'controlled'); assert.deepEqual(body.mapping.channels, ['ALIPAY']);
    assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
  });
  await run('admin my organization follows the same actor context generation', async () => {
    await fresh('?mode=admin&section=users');
    await page.evaluate(() => { window.restored.context = { context_type: 'ORGANIZATION', organization_id: '33333333-3333-4333-8333-333333333333', context_version: 1, membership_version: 1, organization_authorization_version: 1, permissions: ['org.profile.read'] }; history.replaceState({}, '', '?mode=admin&section=myorg'); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); });
    await page.waitForFunction(() => document.querySelector('[data-workspace=myorg]')?.textContent.includes('33333333-3333-4333-8333-333333333333'));
    const before = await page.evaluate(() => window.restored.calls.filter(call => call.url.endsWith('/account/bootstrap')).length);
    await page.evaluate(() => { window.restored.context = { ...window.restored.context, organization_id: '66666666-6666-4666-8666-666666666666', context_version: 2 }; dispatchEvent(new Event('focus')); });
    await page.waitForFunction(before => window.restored.calls.filter(call => call.url.endsWith('/account/bootstrap')).length >= before + 2 && document.querySelector('[data-workspace=myorg]')?.textContent.includes('66666666-6666-4666-8666-666666666666'), {}, before);
    assert.ok(!(await page.$eval('[data-workspace=myorg]', node => node.textContent)).includes('33333333-3333-4333-8333-333333333333'));
  });
  for (const kind of ['account', 'admin']) for (const language of ['zh', 'en', 'ja']) await run(`${kind} ${language}: responsive original frame, localized navigation`, async () => {
    await fresh(`?mode=${kind}&section=${kind === 'admin' ? 'users' : 'creditsUsage'}`);
    await page.select(kind === 'admin' ? '.adm-sidebar-foot select' : '.ap-sidebar-foot select', language);
    for (const width of [1440, 768, 390]) { await page.setViewport({ width, height: 1000 }); await noOverflow(); }
    if (process.env.UI_ARTIFACT_DIR && language === 'zh') {
      fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `${kind}-mobile.png`), fullPage: true });
      await page.setViewport({ width: 1440, height: 1000 });
      await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `${kind}-desktop.png`), fullPage: true });
    }
  });
  console.log(`RESULT ${passed} PASS 0 FAIL ${skipped} SKIP`);
} catch (error) { console.error(error); process.exitCode = 1; }
finally { if (browser) await browser.close(); await server.close(); }
