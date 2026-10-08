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
let vite; let browser; let page; let passed = 0; let failed = 0;
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
  const caseRun = async (name, fn) => { try { await fn(); assert.deepEqual(errors, [], 'Unexpected React errors'); passed++; console.log(`PASS ${name}`); } catch (error) { failed++; console.error(`FAIL ${name}: ${error.message}`); } };
  await caseRun('Expired order snapshot restarts the first page with its search filter', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; });
    await page.type('form input', 'merchant-needle'); await click('search'); await settled();
    await click('next'); await settled(); await page.evaluate(() => { window.f02.expiredCursor = true; });
    await click('search'); await settled();
    // The next-page request receives 410; its automatic retry keeps the filter.
    await click('next'); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls.filter(c => c.url.includes('/payment-orders?')); const last = new URL(calls.at(-1).url, location.origin); return last.searchParams.get('q') === 'merchant-needle' && !last.searchParams.get('cursor'); }));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].find(b => b.textContent === window.f02.languageCopy().previous)?.disabled), true);
  });
  await caseRun('Expired review snapshot preserves status and resets page history', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; }); await click('sla'); await settled();
    await page.select('section form select', 'REVIEW_REQUIRED_LATE_PAYMENT'); await click('refresh'); await settled();
    await page.evaluate(() => { window.f02.expiredCursor = true; }); await click('next'); await settled();
    assert.ok(await page.evaluate(() => { const calls = window.f02.calls.filter(c => c.url.includes('/late-payment-reviews?')); const last = new URL(calls.at(-1).url, location.origin); return last.searchParams.get('status') === 'REVIEW_REQUIRED_LATE_PAYMENT' && !last.searchParams.get('cursor'); }));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].find(b => b.textContent === window.f02.languageCopy().previous)?.disabled), true);
  });
  await caseRun('Pending committed adjustment can resume approval without creating another adjustment', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; }); await click('wallet'); await settled();
    assert.ok(await page.evaluate(() => document.body.textContent.includes('controlled-adjustment-key')));
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled(); await click('submit'); await settled();
    assert.ok(await page.evaluate(() => { const request = window.f02.calls.find(c => c.method === 'POST' && c.url.endsWith('/api/admin/approvals/requests')); return request && JSON.parse(request.body).idempotency_key === 'approval:controlled-adjustment-key' && !window.f02.calls.some(c => c.method === 'POST' && c.url.endsWith('/adjustments')); }));
  });
  await caseRun('Terminal adjustment replacement preserves old request lineage and wallet binding', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.approvalCreated = true; window.f02.approvalStatus = 'REJECTED'; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled();
    await click('newInvocation'); await settled();
    await page.type('form input[pattern]', '123'); await page.type('form textarea', 'Controlled replacement adjustment reason'); await click('draft'); await settled();
    assert.ok(await page.evaluate(() => { const a = window.f02.calls.find(c => c.method === 'POST' && c.url.endsWith('/adjustments')); const p = window.f02.calls.find(c => c.method === 'POST' && c.url.endsWith('/api/admin/approvals/requests')); if (!a || !p) return false; const adjustment = JSON.parse(a.body), draft = JSON.parse(p.body); return adjustment.wallet_id === window.f02.id && adjustment.idempotency_key.startsWith(`replacement:${window.f02.id}:`) && draft.supersedes_request_id === window.f02.id && draft.target_resource_id === window.f02.other; }));
  });
  await caseRun('Existing adjustment draft resumes with the fresh version and no duplicate adjustment', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.approvalCreated = true; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled(); await click('submit'); await settled();
    assert.ok(await page.evaluate(() => window.f02.approvalStatus === 'SUBMITTED' && window.f02.calls.some(c => c.method === 'PUT' && c.url.endsWith(`/requests/${window.f02.id}`)) && !window.f02.calls.some(c => c.method === 'POST' && c.url.endsWith('/adjustments'))));
  });
  await caseRun('Changed pending adjustment facts prevent recovery writes', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled(); await page.evaluate(() => { window.f02.changedAdjustment = true; }); await click('submit'); await settled();
    assert.ok(await page.evaluate(() => !window.f02.calls.some(c => c.method === 'POST' || c.method === 'PUT')));
  });
  await caseRun('Replacement rechecks the requester immediately before creating the new adjustment', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.approvalCreated = true; window.f02.approvalStatus = 'WITHDRAWN'; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled(); await click('newInvocation');
    await page.type('form input[pattern]', '123'); await page.type('form textarea', 'Controlled replacement adjustment reason');
    await page.evaluate(() => { window.f02.replacementRequester = window.f02.other; }); await click('draft'); await settled();
    assert.ok(await page.evaluate(() => !window.f02.calls.some(c => c.method === 'POST' && c.url.endsWith('/adjustments'))));
  });
  await caseRun('Replacement locks the wallet and retains the same financial facts and key after unknown ACK', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.approvalCreated = true; window.f02.approvalStatus = 'INVALIDATED'; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled(); await click('newInvocation');
    assert.equal(await page.evaluate(() => document.querySelector('form input:not([pattern])').readOnly), true);
    await page.type('form input[pattern]', '9007199254740993'); await page.type('form textarea', 'Controlled replacement adjustment reason');
    await page.evaluate(() => { window.f02.unknownAdjustment = true; }); await click('draft'); await settled(); await page.select('[aria-label=fixture-language]', 'ja');
    await page.evaluate(() => { window.f02.unknownAdjustment = false; }); await click('retry'); await settled();
    assert.ok(await page.evaluate(() => { const a = window.f02.calls.filter(c => c.method === 'POST' && c.url.endsWith('/adjustments')); return a.length === 2 && a[0].body === a[1].body && a[0].key === a[1].key && a[0].body.includes('9007199254740993') && window.f02.approvalStatus === 'SUBMITTED'; }));
  });
  await caseRun('Incomplete approval history cannot expose a replacement or resume action', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.incompleteApprovalHistory = true; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled();
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('[data-finance-adjustments] button')].some(b => [window.f02.languageCopy().submit, window.f02.languageCopy().newInvocation].includes(b.textContent)) && !window.f02.calls.some(c => c.method === 'POST' || c.method === 'PUT')));
  });
  await caseRun('A linked adjustment approval opens its current actionable detail', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.approvalCreated = true; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled();
    await page.evaluate(() => [...document.querySelectorAll('[data-finance-adjustments] article button')].find(b => b.textContent === window.f02.languageCopy().details).click()); await settled();
    assert.ok(await page.evaluate(() => document.querySelector('article')?.textContent.includes(window.f02.id) && [...document.querySelectorAll('article button')].some(b => b.textContent === window.f02.languageCopy().editSubmit)));
  });
  await caseRun('Foreign wallet detail cannot enable a replacement', async () => {
    await fresh('admin'); await page.evaluate(() => { window.f02.financeRecovery = true; window.f02.approvalCreated = true; window.f02.approvalStatus = 'EXPIRED'; window.f02.replacementWallet = window.f02.other; }); await click('wallet'); await settled();
    await page.evaluate(() => document.querySelector('[data-finance-adjustments] button').click()); await settled();
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('button')].some(b => b.textContent === window.f02.languageCopy().newInvocation) && !window.f02.calls.some(c => c.method === 'POST')));
  });
  console.log(`RESULT ${passed} PASS ${failed} FAIL 0 SKIP`); if (failed) process.exitCode = 1;
} catch (error) {
  console.error(`FAIL after ${passed} PASS: ${error.message}`);
  if (page) console.error(JSON.stringify(await page.evaluate(() => ({ text: document.body.textContent.slice(0, 1200), methods: window.f02?.calls.slice(-8).map(call => [call.method, call.url]) })).catch(() => ({}))));
  process.exitCode = 1;
} finally { if (browser) await browser.close(); if (vite) await vite.close(); if (fs.existsSync(profile)) fs.rmSync(profile, { recursive: true, force: true }); }
