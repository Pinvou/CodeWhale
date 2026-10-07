import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f02-ui-'));
let vite; let browser; let page; let passed = 0;
const errors = [];
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); page.setDefaultTimeout(10000);
  page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-ui.html`;
  const fresh = async (kind = 'personal') => { errors.length = 0; await page.goto(url); await page.waitForFunction(() => window.f02); if (kind !== 'personal') await page.evaluate(kind => window.f02.render(kind), kind); await page.waitForFunction(() => ![...document.querySelectorAll('[role=status]')].some(item => item.textContent === window.f02.languageCopy().loading)); await page.waitForSelector(kind === 'personal' ? 'form select' : 'tbody tr'); };
  const click = async key => { const clicked = await page.evaluate(key => { const text = window.f02.languageCopy()[key]; const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === text && !item.disabled); if (!button) return false; button.click(); return true; }, key); assert.ok(clicked, `Unavailable button: ${key}`); };
  const settled = async () => page.waitForFunction(() => ![...document.querySelectorAll('[role=status]')].some(item => item.textContent === window.f02.languageCopy().loading));
  const caseRun = async (name, fn) => { await fn(); assert.deepEqual(errors, [], 'Unexpected React errors'); passed++; console.log(`PASS ${name}`); };
  await caseRun('Personal create renders exact money and rejects unsafe provider link and SVG', async () => {
    await fresh(); await click('create'); await page.waitForSelector('article'); await settled();
    assert.ok(await page.evaluate(() => document.querySelector('article').textContent.includes('90,071,992,547,409.93')));
    assert.equal(await page.$('article a, article img'), null);
    assert.equal(await page.evaluate(() => window.f02.calls.filter(call => call.method === 'POST').length), 1);
  });
  await caseRun('Personal unknown outcome retains key and facts across language change', async () => {
    await fresh(); await page.evaluate(() => { window.f02.unknownCreate = true; }); await click('create'); await page.waitForSelector('[role=alert]'); await settled();
    await page.select('[aria-label=fixture-language]', 'ja');
    assert.ok(await page.evaluate(() => document.querySelector('[role=alert]').textContent === window.f02.languageCopy().unknown));
    await page.evaluate(() => { window.f02.unknownCreate = false; }); await click('retry'); await page.waitForSelector('article'); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls.filter(call => call.method === 'POST'); return calls.length === 2 && calls[0].key === calls[1].key && calls[0].body === calls[1].body; }));
  });
  await caseRun('Delayed previous identity create cannot update the new identity', async () => {
    await fresh(); await page.evaluate(() => { window.f02.delayedCreate = true; }); await click('create'); await page.waitForFunction(() => typeof window.f02.releaseCreate === 'function');
    await page.evaluate(() => window.f02.switchIdentity()); await settled(); await page.evaluate(() => window.f02.releaseCreate());
    await page.waitForFunction(() => window.f02.calls.filter(call => call.url.endsWith('/v1/me/entitlement-packages')).length === 2);
    assert.equal(await page.$('article'), null);
  });
  await caseRun('PLAN package has no ordinary refund operation', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.plan = true; }); await click('details'); await page.waitForSelector('article'); await settled();
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('article button')].some(item => item.textContent.trim() === window.f02.languageCopy().refunds)));
    assert.ok(await page.evaluate(() => !window.f02.calls.some(call => call.url.endsWith('/refunds'))));
  });
  await caseRun('Unknown whole refund retains original target and key after leaving its tab', async () => {
    await fresh('admin'); await click('details'); await page.waitForSelector('article textarea'); await page.type('article textarea', 'Controlled whole refund reason');
    await page.evaluate(() => { window.f02.unknownRefund = true; }); await click('refunds'); await page.waitForSelector('[role=alert]'); await settled();
    await click('wallet'); await page.waitForSelector('form textarea'); await settled(); await click('orders'); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(item => item.textContent.trim() === window.f02.languageCopy().retry && !item.disabled));
    await page.evaluate(() => { window.f02.unknownRefund = false; }); await click('retry'); await page.waitForFunction(() => window.f02.calls.filter(call => call.url.endsWith('/refunds')).length === 2); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls.filter(call => call.url.endsWith('/refunds')); const body = JSON.parse(calls[0].body); return calls[0].key === calls[1].key && calls[0].body === calls[1].body && body.payment_transaction_id === window.f02.id && body.entitlement_package_id === window.f02.id && !('amount_fen' in body); }));
  });
  await caseRun('Mismatched refund amount is unknown and retry uses the same invocation', async () => {
    await fresh('admin'); await click('details'); await page.waitForSelector('article textarea'); await page.type('article textarea', 'Controlled whole refund reason');
    await page.evaluate(() => { window.f02.wrongRefund = true; }); await click('refunds'); await page.waitForSelector('[role=alert]'); await settled();
    await page.evaluate(() => { window.f02.wrongRefund = false; }); await click('retry'); await page.waitForFunction(() => window.f02.calls.filter(call => call.url.endsWith('/refunds')).length === 2); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls.filter(call => call.url.endsWith('/refunds')); return calls[0].key === calls[1].key && calls[0].body === calls[1].body; }));
  });
  await caseRun('Owned late review uses its actual review_status and query route', async () => {
    await fresh('admin'); await click('sla'); await page.waitForFunction(() => document.body.textContent.includes('REVIEW_REQUIRED_LATE_PAYMENT')); await settled(); await click('details'); await page.waitForSelector('article'); await settled(); await click('query');
    await page.waitForFunction(() => window.f02.calls.some(call => call.url.endsWith('/query') && call.method === 'POST')); await settled();
  });
  await caseRun('Product display names update only the three localized fields and current version', async () => {
    await fresh('products'); await click('details'); await page.waitForSelector('form textarea'); await page.type('form textarea', 'Controlled display correction'); await click('saveNames');
    await page.waitForFunction(() => window.f02.calls.some(call => call.url.endsWith('/display-names'))); await settled();
    assert.ok(await page.evaluate(() => { const call = window.f02.calls.find(call => call.url.endsWith('/display-names')); const body = JSON.parse(call.body); return call.method === 'POST' && Object.keys(body).sort().join(',') === ['display_name_en', 'display_name_ja', 'display_name_zh_cn', 'expected_version', 'reason'].sort().join(',') && body.expected_version === 1; }));
  });
  await caseRun('Invalid draft integer fails visibly before any product mutation', async () => {
    await fresh('products'); await click('details'); await page.waitForSelector('form textarea'); await page.type('form textarea', 'Controlled draft correction');
    await page.evaluate(() => { const input = [...document.querySelectorAll('label')].find(item => item.firstChild?.textContent === window.f02.languageCopy().validityDays)?.querySelector('input'); const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set; setter.call(input, '1e3'); input.dispatchEvent(new Event('input', { bubbles: true })); });
    await click('saveDraft'); await page.waitForSelector('[role=alert]');
    assert.ok(await page.evaluate(() => !window.f02.calls.some(call => call.method === 'PUT')));
  });
  await caseRun('All three languages expose localized current finance labels', async () => {
    await fresh(); for (const language of ['zh', 'ja', 'en']) { await page.select('[aria-label=fixture-language]', language); await page.waitForFunction(() => document.querySelector('h2')?.textContent === window.f02.languageCopy().title); }
  });
  await caseRun('Wallet approval edit reconstructs exact owner proof and submits the updated version', async () => {
    await fresh('admin'); await click('approvals'); await page.waitForFunction(() => document.body.textContent.includes('WALLET_ADJUSTMENT')); await settled(); await click('details'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Controlled adjustment approval reason'); await click('editSubmit'); await page.waitForFunction(() => window.f02.approvalStatus === 'SUBMITTED'); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls; const edit = calls.find(call => call.method === 'PUT'); const submit = calls.find(call => call.url.endsWith('/actions/submit')); return edit.body.includes('"amount_microcredits":9007199254740993') && edit.body.includes('"expected_version":1') && JSON.parse(submit.body).expected_version === 2 && calls.some(call => call.url.includes('/adjustments?')); }));
  });
  await caseRun('Unknown approval 202 cannot confirm submission and retry preserves expected version', async () => {
    await fresh('admin'); await click('approvals'); await page.waitForFunction(() => document.body.textContent.includes('WALLET_ADJUSTMENT')); await settled(); await click('details'); await page.waitForSelector('article textarea'); await page.evaluate(() => { window.f02.approval202 = true; });
    await click('submit'); await page.waitForSelector('[role=alert]'); await settled(); assert.equal(await page.evaluate(() => window.f02.approvalStatus), 'DRAFT');
    await page.evaluate(() => { window.f02.approval202 = false; }); await click('retry'); await page.waitForFunction(() => window.f02.approvalStatus === 'SUBMITTED'); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls.filter(call => call.url.endsWith('/actions/submit')); return calls.length === 2 && calls[0].body === calls[1].body && JSON.parse(calls[1].body).expected_version === 1; }));
  });
  for (const product of [false, true]) await caseRun(`${product ? 'Product' : 'Provider'} resume submits only the actual owner draft`, async () => {
    await fresh('admin'); await click('providerControls'); await page.waitForSelector('section textarea'); await settled(); await page.type('section textarea', 'Controlled payment resume reason');
    await page.evaluate(product => { const label = window.f02.languageCopy().resume; const buttons = [...document.querySelectorAll('button')].filter(item => item.textContent.trim() === label && !item.disabled); buttons[product ? 1 : 0].click(); }, product);
    await page.waitForFunction(() => window.f02.approvalStatus === 'SUBMITTED'); await settled();
    assert.ok(await page.evaluate(product => { const call = window.f02.calls.find(call => call.url.endsWith('/api/admin/approvals/requests') && call.method === 'POST'); const body = JSON.parse(call.body); return body.command_type === (product ? 'PAYMENT_PRODUCT_RESUME' : 'PAYMENT_PROVIDER_RESUME') && body.normalized_command.provider_control_version === 3 && (!product || body.normalized_command.product_control_version === 4) && window.f02.calls.some(call => call.url.endsWith('/payment-provider-controls')); }, product));
  });
  await caseRun('Approval decision uses the current action control and expected request version', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.approvalStatus = 'SUBMITTED'; }); await click('approvals'); await settled(); await click('details'); await page.waitForSelector('article textarea'); await page.type('article textarea', 'Controlled approval decision reason'); await click('approve'); await page.waitForFunction(() => window.f02.approvalStatus === 'APPROVED'); await settled();
    assert.ok(await page.evaluate(() => { const call = window.f02.calls.find(call => call.url.endsWith('/actions/decide')); const body = JSON.parse(call.body); return body.expected_version === 1 && body.decision === 'APPROVE' && body.reason === 'Controlled approval decision reason'; }));
  });
  await caseRun('Publish-only product operator keeps mapping read-only while saving localized names', async () => {
    await fresh('products'); await page.evaluate(() => { window.f02.publishOnly = true; window.f02.switchIdentity(); }); await settled(); await click('details'); await page.waitForSelector('form textarea'); await settled();
    assert.ok(await page.evaluate(() => [...document.querySelectorAll('form select, form input[type=checkbox]')].every(item => item.disabled)), 'Publish-only operator can edit mapping');
    assert.ok(await page.evaluate(() => !window.f02.calls.some(call => call.url.endsWith('/recharge-product-create-options') && window.f02.calls.indexOf(call) > window.f02.calls.findIndex(call => call.url.endsWith('/admission-verifications')))), 'Publish-only operator requested create metadata');
    await page.type('form textarea', 'Controlled localized display reason'); await click('saveNames'); await page.waitForFunction(() => window.f02.calls.some(call => call.url.endsWith('/display-names'))); await settled();
    assert.ok(await page.evaluate(() => window.f02.calls.filter(call => call.url.endsWith('/display-names')).length === 1 && !window.f02.calls.some(call => call.method === 'PUT')));
  });
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) {
  console.error(`FAIL after ${passed} PASS: ${error.message}`);
  if (page) console.error(JSON.stringify(await page.evaluate(() => ({ text: document.body.textContent.slice(0, 1200), methods: window.f02?.calls.slice(-8).map(call => [call.method, call.url]) })).catch(() => ({}))));
  process.exitCode = 1;
} finally { if (browser) await browser.close(); if (vite) await vite.close(); if (fs.existsSync(profile)) fs.rmSync(profile, { recursive: true, force: true }); }
