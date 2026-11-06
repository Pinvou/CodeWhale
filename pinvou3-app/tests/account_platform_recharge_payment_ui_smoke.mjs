import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { financeCopy } from '../src/shared/i18n.js';
import { startRechargePollingFixture } from './account_platform_recharge_polling_server.mjs';

const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; real recharge React validation was not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-recharge-payment-'));
let fixture, browser, passed = 0, failed = 0, executed = 0;
try {
  fixture = await startRechargePollingFixture();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  const base = new URL(fixture.url).origin;
  const control = async facts => { const response = await fetch(`${base}/__fixture__/control`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(facts) }); assert.equal(response.status, 200); return response.json(); };
  const state = async () => (await fetch(`${base}/__fixture__/state`)).json();
  const click = async text => page.evaluate(text => { const button = [...(document.querySelector('[role=dialog]') || document).querySelectorAll('button')].find(node => node.textContent === text) || [...document.querySelectorAll('button')].find(node => node.textContent === text); if (!button || button.disabled) throw new Error(`Button unavailable: ${text}`); button.focus(); button.click(); }, text);
  const noDialog = () => page.waitForFunction(() => !document.querySelector('[role="dialog"]'));
  const pending = '[aria-label="Pending payments"] li';
  const fresh = async () => { await control({ reset: true }); errors.length = 0; await page.goto(fixture.url); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Create payment order' && !button.disabled)); };
  const open = async () => { await click(financeCopy.en.continuePayment); await page.waitForSelector('[role="dialog"] img'); };
  const run = async (name, fn) => {
    if (process.env.PINVOU_RECHARGE_CASE && !name.includes(process.env.PINVOU_RECHARGE_CASE)) return;
    executed++;
    try { await fn(); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); }
    catch (error) { failed++; console.error(`FAIL ${name}: ${error.stack}`); }
  };
  await run('QR loading, slow feedback and retry never create another order', async () => {
    await fresh(); await control({ qrMode: 'remote' });
    const heldImages = []; let imageMode = 'hold';
    const intercept = request => {
      if (request.url() !== 'https://example.com/public-fixture-qr.png') { request.continue(); return; }
      if (imageMode === 'hold') heldImages.push(request);
      else request.respond({ status: 200, contentType: 'image/png', body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aNbQAAAAASUVORK5CYII=', 'base64') });
    };
    await page.setRequestInterception(true); page.on('request', intercept);
    try {
      await click(financeCopy.en.create); await page.waitForSelector('[data-qr-state=loading]');
      assert.ok((await page.$eval('[role=dialog]', node => node.textContent)).includes(financeCopy.en.paymentOrderCreated));
      assert.ok((await page.$eval('[role=dialog]', node => node.textContent)).includes(financeCopy.en.qrLoading));
      assert.equal(await page.$eval('[role=dialog] img', node => node.hidden), true);
      await page.waitForFunction(text => document.querySelector('[role=dialog]')?.textContent.includes(text), { timeout: 20000 }, financeCopy.en.paymentSlow);
      assert.equal(heldImages.length, 1); await heldImages.shift().abort('failed');
      await page.waitForSelector('[data-qr-state=error]');
      assert.ok((await page.$eval('[role=dialog]', node => node.textContent)).includes(financeCopy.en.qrLoadFailed));
      imageMode = 'success'; await click(financeCopy.en.qrRetry); await page.waitForSelector('[data-qr-state=loaded]');
      assert.equal(await page.$eval('[role=dialog] img', node => node.hidden), false);
      assert.equal((await state()).productMutations, 1); assert.equal((await state()).createKeys.length, 1);
      await click(financeCopy.en.dismissPayment); await noDialog();
    } finally {
      for (const request of heldImages) await request.abort('aborted').catch(() => {});
      page.off('request', intercept); await page.setRequestInterception(false);
    }
  });
  await run('missing payment instructions display preparation and still allow close and status query', async () => {
    await fresh(); await control({ qrMode: 'missing' }); await click(financeCopy.en.create);
    await page.waitForSelector('[data-qr-state=preparing]');
    assert.ok((await page.$eval('[role=dialog]', node => node.textContent)).includes(financeCopy.en.paymentPreparing));
    await control({ qrMode: 'inline' }); await click(financeCopy.en.query);
    await page.waitForSelector('[data-qr-state=loaded]'); assert.equal((await state()).productMutations, 1);
    await click(financeCopy.en.dismissPayment); await noDialog();
  });
  await run('page reload only restores the pending list; history remains readonly', async () => {
    await fresh(); assert.equal(await page.$$eval(pending, nodes => nodes.length), 1, 'a separate payable-only list must exist'); assert.equal((await state()).detailReads, 0); assert.equal(await page.$('[role="dialog"]'), null); assert.equal(await page.$('.ap-finance img'), null);
    await click(financeCopy.en.details); await page.waitForSelector('article'); assert.equal((await state()).detailReads, 1); assert.equal(await page.$('[role="dialog"]'), null); assert.equal(await page.$('.ap-finance img'), null);
    await page.reload(); await page.waitForSelector(pending); assert.equal(await page.$('[role="dialog"]'), null); assert.equal((await state()).detailReads, 1);
  });
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: create opens accessible provider-facts modal and Escape leaves order open`, async () => {
    await page.setViewport({ width: language === 'ja' ? 390 : 1100, height: 900 }); await fresh(); await page.select('[aria-label="Fixture language"]', language); const copy = financeCopy[language];
    await click(copy.create); await page.waitForSelector('[data-qr-state=loaded]'); assert.notEqual(await page.$('[role="dialog"] img'), null, 'provider QR must be inside a separate payment modal');
    await page.waitForFunction(text => [...document.querySelectorAll('button')].some(node => node.textContent === text && !node.disabled), {}, copy.create);
    assert.equal(await page.$eval('.ap-order-close-backdrop', node => getComputedStyle(node).position), 'fixed');
    assert.equal(await page.$eval('[role=dialog]', node => node.getBoundingClientRect().width <= innerWidth - 32), true);
    if (process.env.PINVOU_RECHARGE_SCREENSHOT_DIR) { fs.mkdirSync(process.env.PINVOU_RECHARGE_SCREENSHOT_DIR, { recursive: true }); await page.screenshot({ path: path.join(process.env.PINVOU_RECHARGE_SCREENSHOT_DIR, `recharge-${language}.png`) }); }
    const text = await page.$eval('[role="dialog"]', node => node.textContent); assert.ok(text.includes('12.34')); assert.ok(text.includes(copy.channels.ALIPAY)); assert.ok(text.includes(copy.scanToPayWithApps([copy.channels.ALIPAY])), 'collection refresh preserves Provider scan-app facts'); assert.ok(text.includes(copy.countdown(119).split('1:59')[0]));
    assert.equal(await page.$eval('img', node => node.closest('[role="dialog"]') !== null), true);
    assert.equal(await page.evaluate(() => document.activeElement?.textContent), copy.dismissPayment);
    await page.$eval('[role=dialog] a', node => node.focus()); await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift'); assert.equal(await page.evaluate(() => document.activeElement?.textContent), copy.query);
    await page.keyboard.press('Tab'); assert.equal(await page.evaluate(() => document.activeElement?.textContent), copy.pay);
    await page.waitForFunction(text => [...document.querySelectorAll('button')].some(node => node.textContent === text && !node.disabled), {}, copy.create); await page.keyboard.press('Escape'); await noDialog(); assert.equal(await page.$('img'), null); assert.equal((await state()).status, 'PENDING'); assert.equal((await state()).productMutations, 1);
    assert.equal(await page.evaluate(() => document.activeElement?.textContent), copy.create);
  });
  await run('closed window refreshes only list and receives paid callback without selecting a QR', async () => {
    await fresh(); await open(); await click(financeCopy.en.dismissPayment); await noDialog();
    const before = await state(); await control({ status: 'PAID' });
    await page.waitForFunction(() => document.querySelectorAll('[aria-label="Pending payments"] li').length === 0);
    await page.waitForFunction(() => document.querySelector('[data-fixture-paid]')?.textContent === 'Paid callbacks: 1');
    const after = await state(); assert.equal(after.detailReads, before.detailReads); assert.ok(after.listReads > before.listReads); assert.equal(await page.$('[role="dialog"]'), null); assert.equal(await page.$('img'), null);
  });
  for (const status of ['PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW']) await run(`active payment withdraws on ${status}`, async () => {
    await fresh(); await open(); await control({ status }); await noDialog();
    await page.waitForFunction(() => document.querySelectorAll('[aria-label="Pending payments"] li').length === 0); assert.equal(await page.$('img'), null); assert.equal(await page.$('[role="dialog"] a'), null);
  });
  await run('non-OPEN and local deadline withdraw immediately, without a detail GET to resume', async () => {
    await fresh(); await open(); await control({ closeStatus: 'CLOSE_REQUESTED' }); await noDialog();
    await page.waitForFunction(() => document.querySelectorAll('[aria-label="Pending payments"] li').length === 0); assert.equal(await page.$('img'), null);
    await fresh(); await control({ expiresInMS: 2000 }); await open(); await noDialog();
    await page.waitForFunction(() => document.querySelectorAll('[aria-label="Pending payments"] li').length === 0); assert.equal(await page.$('img'), null);
  });
  await run('owned polling PAID begun before expiry refreshes once after window withdrawal', async () => {
    await fresh(); await control({ expiresInMS: 4500 }); await open(); await control({ hold: true });
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await noDialog(); await control({ status: 'PAID', release: true });
    await page.waitForFunction(() => document.querySelector('[data-fixture-paid]')?.textContent === 'Paid callbacks: 1');
    assert.equal(await page.$('img'), null); assert.equal(await page.$('[role=dialog]'), null);
    assert.equal(await page.$$eval(pending, nodes => nodes.length), 0); assert.equal((await state()).productMutations, 0);
  });
  await run('a held query cannot reopen a dismissed window and no order close POST is sent', async () => {
    await fresh(); await open(); await control({ hold: true }); await click(financeCopy.en.query);
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await click(financeCopy.en.dismissPayment); await noDialog(); await control({ release: true, releasePending: true });
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 0);
    assert.equal(await page.$('[role="dialog"]'), null); assert.equal(await page.$('img'), null); assert.equal((await state()).productMutations, 0);
    await open(); assert.ok((await state()).detailReads >= 3);
  });
  await run('list paid proof retires an older held PENDING query without duplicate refresh or reopened list', async () => {
    await fresh(); await open(); await control({ hold: true }); await click(financeCopy.en.query);
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await control({ status: 'PAID' }); await noDialog();
    await page.waitForFunction(() => document.querySelector('[data-fixture-paid]')?.textContent === 'Paid callbacks: 1');
    await control({ release: true, releasePending: true });
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Create payment order' && !button.disabled));
    assert.equal(await page.$$eval(pending, nodes => nodes.length), 0); assert.equal(await page.$('img'), null); assert.equal(await page.evaluate(() => [...document.querySelectorAll('article button')].some(node => node.textContent === 'Close payment order')), false);
    assert.equal(await page.$eval('[data-fixture-paid]', node => node.textContent), 'Paid callbacks: 1');
  });
  await run('pending continue GET cannot open a window after the list has received paid proof', async () => {
    await fresh(); await control({ hold: true }); await click(financeCopy.en.continuePayment);
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await control({ status: 'PAID' }); await page.waitForFunction(() => document.querySelector('[data-fixture-paid]')?.textContent === 'Paid callbacks: 1');
    await control({ release: true, releasePending: true });
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Create payment order' && !button.disabled));
    assert.equal(await page.$('[role=dialog]'), null); assert.equal(await page.$$eval(pending, nodes => nodes.length), 0); assert.equal(await page.$('img'), null);
  });
  await run('late list PENDING cannot restore payment after detail PAID, even when paid refresh joins that list request', async () => {
    await fresh(); await open(); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Create payment order' && !button.disabled));
    await control({ holdList: true }); await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).heldLists === 1);
    await control({ status: 'PAID' }); await click(financeCopy.en.query); await noDialog();
    await control({ releaseList: true }); await page.waitForFunction(() => document.querySelector('[data-fixture-paid]')?.textContent === 'Paid callbacks: 1');
    assert.equal(await page.$$eval(pending, nodes => nodes.length), 0, 'older list PENDING must not replace the known PAID facts'); assert.equal(await page.$('img'), null);
  });
  await run('late create PENDING receipt cannot flash a payment window after list PAID', async () => {
    await fresh(); await control({ holdCreate: true }); await click(financeCopy.en.create);
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).heldCreates === 1);
    await control({ status: 'PAID' }); await page.waitForFunction(() => document.querySelector('[data-fixture-paid]')?.textContent === 'Paid callbacks: 1');
    await page.evaluate(() => { window.paymentDialogInsertions = 0; window.paymentDialogObserver = new MutationObserver(records => { for (const record of records) for (const node of record.addedNodes) if (node.nodeType === 1 && (node.matches('[role=dialog]') || node.querySelector('[role=dialog]'))) window.paymentDialogInsertions++; }); window.paymentDialogObserver.observe(document.body, { childList: true, subtree: true }); });
    await control({ releaseCreate: true }); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Create payment order' && !button.disabled));
    assert.equal(await page.evaluate(() => { window.paymentDialogObserver.disconnect(); return window.paymentDialogInsertions; }), 0, 'a stale create receipt must not open even a transient modal');
    assert.equal(await page.$$eval(pending, nodes => nodes.length), 0); assert.equal(await page.$('[role=dialog]'), null);
  });
  await run('older collection window cannot extend the deadline confirmed by an explicit detail read', async () => {
    await fresh(); await control({ holdList: true }); await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).heldLists === 1);
    await control({ expiresInMS: 2500 }); await click(financeCopy.en.continuePayment); await page.waitForSelector('[role=dialog] img');
    await control({ releaseList: true }); await noDialog();
    await page.waitForFunction(() => document.querySelectorAll('[aria-label="Pending payments"] li').length === 0); assert.equal(await page.$('img'), null);
  });
  for (const action of ['Switch to actor B', 'Retire login epoch', 'Retire client epoch without rendering', 'Revoke read permission', 'Unmount component']) await run(`held old ACK cannot restore payment after ${action}`, async () => {
    await fresh(); await open(); await control({ hold: true }); await click(financeCopy.en.query);
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await click(action); await noDialog(); await control({ release: true, releasePending: true });
    assert.equal(await page.$('[role="dialog"]'), null); assert.equal(await page.$('img'), null);
  });
  for (const status of [401, 403]) await run(`detail ${status} synchronously retires payment and list`, async () => {
    await fresh(); await open(); await control({ denyDetail: status }); await click(financeCopy.en.query); await noDialog(); assert.equal(await page.$('img'), null); assert.equal(await page.$(pending), null);
  });
  await run('confirmed create remains in pending list when its follow-up collection refresh is unavailable', async () => {
    await fresh(); await control({ denyList: 503 }); await click(financeCopy.en.create); await page.waitForSelector('[role="dialog"] img');
    await page.waitForSelector('[role=alert]'); await click(financeCopy.en.dismissPayment); await noDialog();
    assert.equal(await page.$$eval(pending, nodes => nodes.length), 1); await control({ denyList: 0 }); await open();
  });
  await run('create permission denial does not revoke independent existing-order read access', async () => {
    await fresh(); await control({ denyCreate: 403 }); await click(financeCopy.en.create); await page.waitForSelector('[role=alert]');
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Continue payment' && !node.disabled));
    await open(); assert.equal((await state()).createKeys.length, 0); assert.equal((await state()).detailReads, 1);
  });
  await run('UNKNOWN create retries exactly original facts and key then opens confirmed receipt', async () => {
    await fresh(); await control({ unknownCreate: true }); await click(financeCopy.en.create); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === 'Retry the same request' && !button.disabled));
    const before = await state(); assert.equal(before.createKeys.length, 1); await click(financeCopy.en.retry); await page.waitForSelector('[role="dialog"] img');
    const after = await state(); assert.deepEqual(after.createKeys, before.createKeys); assert.equal(after.productMutations, 2);
  });
  await run('organization explicit Query uses modal without wallet-read permission and preserves org scope', async () => {
    await fresh(); await click('Organization surface'); await page.waitForSelector('input'); await click('Remove organization wallet read permission');
    await page.waitForFunction(() => !document.querySelector('input')?.disabled); await page.type('input', '33333333-3333-4333-8333-333333333333'); await click('Query order'); await page.waitForSelector('[role="dialog"] img');
    const before = await state(); assert.ok(before.requests.some(item => item.path.includes('/organizations/44444444-4444-4444-8444-444444444444/recharge/orders/')));
    await click(financeCopy.en.dismissPayment); await noDialog(); assert.equal(await page.$('img'), null); assert.equal((await state()).productMutations, 0);
  });
  await run('an independently denied organization wallet read does not retire the purchase order reader', async () => {
    await fresh(); await control({ denyWallet: 403 }); await click('Organization surface'); await page.waitForSelector('[role=alert]');
    await page.type('input', '33333333-3333-4333-8333-333333333333'); await click('Query order'); await page.waitForSelector('[role=dialog] img');
    assert.equal((await state()).productMutations, 0);
  });
  await run('organization purchase uses same provider modal and withdrawal follows terminal proof', async () => {
    await fresh(); await click('Organization surface'); await click('Remove organization wallet read permission'); await page.waitForFunction(() => document.querySelector('.ap-org-action select')?.options.length > 1); await page.select('.ap-org-action select', 'public-fixture-product'); await page.waitForFunction(() => document.querySelectorAll('.ap-org-action select')[1]?.options.length > 1); await page.$$eval('.ap-org-action select', nodes => { const node = nodes[1]; node.value = 'ALIPAY'; node.dispatchEvent(new Event('change', { bubbles: true })); }); await click('Purchase organization entitlements');
    await page.waitForSelector('[role="dialog"] img'); await control({ status: 'PAID' }); await noDialog(); assert.equal(await page.$('img'), null);
  });
  const freshOrganizationOrder = async () => {
    await fresh(); await click('Organization surface');
    await page.waitForFunction(() => document.querySelector('.ap-org-action select')?.options.length > 1);
    await page.select('.ap-org-action select', 'public-fixture-product');
    await page.waitForFunction(() => document.querySelectorAll('.ap-org-action select')[1]?.options.length > 1);
    await page.$$eval('.ap-org-action select', nodes => { nodes[1].value = 'ALIPAY'; nodes[1].dispatchEvent(new Event('change', { bubbles: true })); });
    await page.type('input', '33333333-3333-4333-8333-333333333333'); await click('Query order');
    await page.waitForSelector('[role=dialog] img'); await click(financeCopy.en.dismissPayment); await noDialog();
  };
  const unknownOrganizationCreate = async () => {
    await control({ unknownCreate: true }); await click('Purchase organization entitlements');
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-org-action button')].some(node => node.textContent === 'Retry the same operation' && !node.disabled));
    const result = await state(); assert.equal(result.createKeys.length, 1); return result.createKeys;
  };
  const organizationOrderText = () => page.evaluate(() => [...document.querySelectorAll('section p')].find(node => node.textContent.startsWith('33333333-3333-4333-8333-333333333333'))?.textContent);
  const observeOrganizationDialogs = () => page.evaluate(() => {
    window.organizationDialogInsertions = 0;
    window.organizationDialogObserver = new MutationObserver(records => { for (const record of records) for (const node of record.addedNodes) if (node.nodeType === 1 && (node.matches('[role=dialog]') || node.querySelector('[role=dialog]'))) window.organizationDialogInsertions++; });
    window.organizationDialogObserver.observe(document.body, { childList: true, subtree: true });
  });
  const assertNoOrganizationDialog = async target => {
    assert.equal(await page.evaluate(() => { window.organizationDialogObserver.disconnect(); return window.organizationDialogInsertions; }), 0, target);
    assert.equal(await page.$('[role=dialog]'), null, target); assert.equal(await page.$('img'), null, target);
    assert.match(await organizationOrderText(), /Paid.*Credited/, 'organization order facts must retain confirmed PAID');
  };
  await run('organization UNKNOWN retry cannot revive QR through a late PENDING Create ACK after Query PAID', async () => {
    await freshOrganizationOrder(); const keys = await unknownOrganizationCreate();
    await control({ hold: true, holdCreate: true }); await click('Query order');
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await click('Retry the same operation');
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).heldCreates === 1);
    await control({ status: 'PAID', release: true });
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && !node.disabled));
    assert.match(await organizationOrderText(), /Paid.*Credited/); await noDialog(); await observeOrganizationDialogs();
    const before = await state(); await control({ releaseCreate: true });
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-org-action button')].some(node => node.textContent === 'Purchase organization entitlements' && !node.disabled));
    await assertNoOrganizationDialog('known organization PAID must fence an earlier PENDING Create ACK');
    const after = await state(); assert.equal(after.productMutations, before.productMutations); assert.deepEqual(after.createKeys, keys); assert.equal(after.status, 'PAID');
  });
  await run('organization old Query cannot revive QR after the original-key Create ACK confirms PAID', async () => {
    await freshOrganizationOrder(); const keys = await unknownOrganizationCreate();
    await control({ hold: true }); await click('Query order');
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).held === 1);
    await control({ status: 'PAID', holdCreate: true }); await click('Retry the same operation');
    await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).heldCreates === 1);
    await control({ releaseCreate: true });
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-org-action button')].some(node => node.textContent === 'Purchase organization entitlements' && !node.disabled));
    assert.match(await organizationOrderText(), /Paid.*Credited/); await noDialog(); await observeOrganizationDialogs();
    const before = await state(); await control({ release: true, releasePending: true });
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && !node.disabled));
    await assertNoOrganizationDialog('known organization PAID must fence an earlier PENDING Query');
    const after = await state(); assert.equal(after.productMutations, before.productMutations); assert.deepEqual(after.createKeys, keys);
  });
  await run('organization nonpayable fence retires with actor, organization context and login epoch', async () => {
    for (const action of ['Switch to actor B', 'Switch to organization B', 'Retire login epoch']) {
      await freshOrganizationOrder(); await control({ status: 'PAID' }); await click('Query order');
      await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && !node.disabled));
      assert.match(await organizationOrderText(), /Paid.*Credited/); await noDialog();
      await control({ status: 'PENDING' }); await click(action);
      await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && node.disabled));
      await page.type('input', '33333333-3333-4333-8333-333333333333');
      await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && !node.disabled)); await click('Query order');
      await page.waitForSelector('[role=dialog] img'); assert.match(await organizationOrderText(), /Pending/);
      assert.equal((await state()).productMutations, 0, action);
    }
  });
  await run('organization PAID refresh wallet denial does not retire its independently allowed purchase reader', async () => {
    await freshOrganizationOrder(); const responses = [];
    const observe = response => { const pathname = new URL(response.url()).pathname; if (/organizations\/.*\/(balance|entitlement-packages|recharge\/products|payment-capabilities)$/.test(pathname)) responses.push({ path: pathname, status: response.status() }); };
    page.on('response', observe);
    try {
      await control({ status: 'PAID', denyWallet: 403 }); await click('Query order'); await page.waitForSelector('[role=alert]');
      await page.waitForFunction(() => JSON.parse(document.querySelector('[data-fixture-metrics]').textContent).walletReads >= 2);
      const observed = { orderText: await organizationOrderText(), responses: [...responses], state: await state(), dialog: !!await page.$('[role=dialog]') };
      if (process.env.PINVOU_RECHARGE_OBSERVATION_FILE) fs.writeFileSync(process.env.PINVOU_RECHARGE_OBSERVATION_FILE, JSON.stringify(observed, null, 2));
      assert.ok(responses.some(row => row.path.endsWith('/balance') && row.status === 403));
      assert.ok(responses.some(row => row.path.endsWith('/recharge/products') && row.status === 200));
      assert.ok(responses.some(row => row.path.endsWith('/payment-capabilities') && row.status === 200));
      assert.match(observed.orderText || '', /Paid.*Credited/, 'wallet refresh rejection must preserve the confirmed organization PAID facts');
      await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && !node.disabled));
      const before = await state(); await click('Query order');
      await page.waitForFunction(() => [...document.querySelectorAll('button')].some(node => node.textContent === 'Query order' && !node.disabled));
      const after = await state(); assert.equal(after.detailReads, before.detailReads + 1, 'wallet-only 403 must not retire the authorized purchase reader');
      assert.match(await organizationOrderText(), /Paid.*Credited/); assert.equal(await page.$('[role=dialog]'), null); assert.equal(after.productMutations, 0);
      const denied = page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/recharge/products') && response.status() === 403);
      await control({ denyPurchase: 403 }); await click('Query order'); await denied;
      await page.waitForFunction(() => ![...document.querySelectorAll('section p')].some(node => node.textContent.startsWith('33333333-3333-4333-8333-333333333333')));
      assert.equal(await page.$('[role=dialog]'), null); assert.equal(await page.$('img'), null);
      const retired = await state();
      await page.evaluate(async () => { const button = [...document.querySelectorAll('button')].find(node => node.textContent === 'Query order'); if (!button.disabled) button.click(); await Promise.resolve(); await Promise.resolve(); });
      assert.equal((await state()).detailReads, retired.detailReads, 'purchase-source 403 must still retire its reader without another HTTP');
      assert.equal(await page.$('[role=dialog]'), null); assert.equal((await state()).productMutations, 0);
    } finally { page.off('response', observe); }
  });
  assert.ok(executed > 0, 'No real recharge browser cases executed'); assert.equal(failed, 0, `${failed} recharge browser cases failed`);
  console.log(`Recharge payment UI: ${passed} PASS, ${failed} FAIL`);
} finally {
  await browser?.close(); await fixture?.close(); assert.equal(path.dirname(profile), path.resolve(os.tmpdir())); fs.rmSync(profile, { recursive: true, force: true });
}
